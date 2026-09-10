package direct

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// defaultEndpointSendTimeout bounds ONE reporter send. It is a transport bound,
// not a state-loop bound: the drain goroutine performs the send, so no caller
// (and no port lock) ever waits on the network. The open-path report is
// additionally bounded by the caller's context (see ReportOpen), which is the
// confirmation deadline that gates the OK open_ack.
const defaultEndpointSendTimeout = 5 * time.Second

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
		send: send,
		ch:   make(chan endpointEvent, 64),
		done: make(chan struct{}),
	}
	go r.drain()
	return r
}

// drain is the reporter's single send goroutine. It serializes advisory and
// confirmed sends in enqueue order, so a confirmed open report can never be
// overtaken by a later close report within the same connection.
func (r *Reporter) drain() {
	for {
		select {
		case e := <-r.ch:
			r.deliver(e)
		case <-r.done:
			return
		}
	}
}

// deliver performs one send under the per-send transport bound, then signals
// the event's confirmation channel (if any). A nil event context (advisory
// transitions) is bounded only by defaultEndpointSendTimeout.
func (r *Reporter) deliver(e endpointEvent) {
	ctx := e.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	sendCtx, cancel := context.WithTimeout(ctx, defaultEndpointSendTimeout)
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

	select {
	case r.ch <- e:
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
