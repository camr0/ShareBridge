package direct

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// defaultEndpointSendTimeout bounds ONE reporter send on the confirmed-open
// path: the send is additionally bounded by the caller's context (the open
// report's confirmation deadline), so this is the upper transport bound.
const defaultEndpointSendTimeout = 5 * time.Second

// advisorySendTimeout bounds ONE advisory (closed / close-failed) send. The
// drain serializes sends, so an advisory already in flight delays a confirmed
// open report queued behind it — and the daemon's open confirmation deadline
// (defaultOpenReportTimeout, 3s) is SHORTER than the 5s transport bound, so an
// unmodified advisory could occupy the drain past the open's deadline and fail
// the open closed for no reason (remediation finding 4). Bounding advisories
// well below that deadline keeps the residual in-flight delay harmless. It is
// only a backstop against the in-flight case; queued advisories are handled by
// the drain's open-priority selection, which is what makes an arbitrary
// advisory backlog harmless.
const advisorySendTimeout = time.Second

// ErrReporterClosed is returned by ReportOpen once the reporter has been
// closed: a closed reporter cannot confirm an open report, so the open must be
// treated as unsuccessful by the caller.
var ErrReporterClosed = errors.New("direct: endpoint reporter is closed")

// Reporter maps on-demand port state transitions to report_endpoint messages.
//
// Advisory transitions (close and close-failed) are queued and may be dropped
// when the queue is full: they never gate a control-plane decision, and the
// next transition re-reports. The OPEN transition is different — the control
// needs the mapped external port before it can provision DDNS and hand a
// direct origin to a recipient — so it is delivered exclusively by ReportOpen,
// which confirms delivery before returning and reports a full queue, a send
// error, or a timeout as an error instead of silently dropping.
type Reporter struct {
	mu     sync.Mutex
	ip     string
	send   func(ctx context.Context, ip string, port int, status string) error
	ch     chan endpointEvent // queued advisory transitions: never blocks the state loop
	openCh chan endpointEvent // confirmed open reports: prioritized by the drain
	done   chan struct{}      // closed by Close to stop the drain goroutine
	closed bool
}

type endpointEvent struct {
	ip     string
	port   int
	status string

	// ctx is the confirmation deadline the open report must complete inside.
	// It is nil for advisory transitions, whose send is bounded only by
	// defaultEndpointSendTimeout.
	ctx context.Context

	// done receives the send result and is non-nil only for the confirmed open
	// report. The channel is buffered so the drain never blocks signalling it,
	// even when the caller already gave up on the deadline.
	done chan error
}

func NewReporter(send func(ctx context.Context, ip string, port int, status string) error) *Reporter {
	r := &Reporter{
		send:   send,
		ch:     make(chan endpointEvent, 64),
		openCh: make(chan endpointEvent, 1),
		done:   make(chan struct{}),
	}
	go r.drain()
	return r
}

// drain is the reporter's single send goroutine. It serializes advisory and
// confirmed sends, so a confirmed open report can never be overtaken by a later
// close report within the same connection.
//
// Confirmed open events are PRIORITIZED over queued advisory events: the drain
// first drains openCh non-blockingly, then falls into a blocking select that
// prefers openCh again. A backlog of advisory transitions (which the state loop
// enqueues drop-on-full) therefore cannot delay an open confirmation behind
// it. Combined with the advisory send bound in deliver, the residual is at most
// one advisory already in flight when the open is enqueued.
func (r *Reporter) drain() {
	for {
		select {
		case e := <-r.openCh:
			r.deliver(e)
			continue
		default:
		}
		select {
		case e := <-r.openCh:
			r.deliver(e)
		case e := <-r.ch:
			r.deliver(e)
		case <-r.done:
			return
		}
	}
}

// deliver performs one send under a bound, then signals the event's
// confirmation channel (if any). A confirmed open event carries the caller's
// deadline (bounded further by the transport bound); an advisory event has no
// caller and is bounded by advisorySendTimeout so it cannot occupy the drain
// past the open confirmation window.
func (r *Reporter) deliver(e endpointEvent) {
	ctx := e.ctx
	bound := advisorySendTimeout
	if ctx == nil {
		ctx = context.Background()
	} else {
		bound = defaultEndpointSendTimeout
	}
	sendCtx, cancel := context.WithTimeout(ctx, bound)
	err := r.send(sendCtx, e.ip, e.port, e.status)
	cancel()
	if e.done != nil {
		e.done <- err
	}
}

// Close stops the reporter's drain goroutine. It is idempotent and safe to call
// multiple times. After Close, both OnTransition and ReportOpen drop/refuse
// events, so nothing is ever sent on a torn-down reporter.
func (r *Reporter) Close() {
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		close(r.done)
	}
	r.mu.Unlock()
}

func (r *Reporter) SetIP(ip string) {
	r.mu.Lock()
	r.ip = ip
	r.mu.Unlock()
}

// OnTransition publishes the ADVISORY transition reports: closing/close-failed
// and closed. StateOpen is deliberately not reported here — the open report is
// control-critical and is delivered by ReportOpen so it can be confirmed before
// the OK open_ack rather than racing it through this drop-on-full queue.
func (r *Reporter) OnTransition(old, new PortState, grantedPort int) {
	var e endpointEvent
	switch new {
	case StateClosed:
		e = endpointEvent{port: 0}
	case StateCloseFailed:
		e = endpointEvent{port: grantedPort, status: "close_failed"}
	default:
		// StateClosing, and StateOpen (see the method comment).
		return
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	e.ip = r.ip
	// Non-blocking enqueue: advisory reports may be dropped when the buffer is
	// full. This guarantees OnTransition never blocks the state loop even if
	// the send consumer stalls.
	select {
	case r.ch <- e:
	default:
	}
	r.mu.Unlock()
}

// ReportOpen sends the open transition's endpoint report for grantedPort — the
// external port the router actually granted — and confirms delivery before
// returning. It is the open path's dedicated synchronous report: unlike the
// advisory transitions it is never silently dropped. A full queue (the enqueue
// cannot complete inside ctx), a send failure, or a confirmation that misses
// ctx's deadline is returned as an error, and the caller MUST treat the open as
// unsuccessful.
//
// ReportOpen is called after OnDemandPort.OpenForIf has returned, so it holds
// no state-loop lock, no ackMu, and no router I/O while it waits. The only
// block is the bounded enqueue/confirmation select on ctx.
func (r *Reporter) ReportOpen(ctx context.Context, grantedPort int) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrReporterClosed
	}
	ip := r.ip
	r.mu.Unlock()

	done := make(chan error, 1)
	e := endpointEvent{ip: ip, port: grantedPort, ctx: ctx, done: done}

	// Confirmed open reports ride their own prioritized queue: a backlog of
	// advisory events can never sit in front of them in the drain.
	select {
	case r.openCh <- e:
	case <-ctx.Done():
		return fmt.Errorf("endpoint open report enqueue: %w", ctx.Err())
	}

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("endpoint open report confirm: %w", ctx.Err())
	}
}
