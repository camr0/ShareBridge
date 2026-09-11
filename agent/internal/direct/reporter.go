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
// per-endpoint coalescing, which caps the queued backlog at one advisory.
const advisorySendTimeout = time.Second

// reporterQueueCap bounds the number of pending events. A single FIFO carries
// both advisories and confirmed opens so per-endpoint transition order is
// preserved; advisories are coalesced (at most one pending per endpoint, the
// newest), so only concurrent open reports can grow the queue and the cap is a
// defensive safety net.
const reporterQueueCap = 64

// ErrReporterClosed is returned by ReportOpen once the reporter has been
// closed: a closed reporter cannot confirm an open report, so the open must be
// treated as unsuccessful by the caller.
var ErrReporterClosed = errors.New("direct: endpoint reporter is closed")

// errReporterQueueFull is returned by ReportOpen when the FIFO is at
// reporterQueueCap (only reachable with many outstanding open reports; the
// advisory side is coalesced). The open must be treated as unsuccessful.
var errReporterQueueFull = errors.New("direct: endpoint reporter queue is full")

// Reporter maps on-demand port state transitions to report_endpoint messages.
//
// ALL transitions share ONE FIFO (queue) so an endpoint's reports reach control
// in the order the port state machine produced them: a confirmed open report
// can never overtake an advisory that was enqueued before it (audit A2 — the
// previous dedicated priority channel let a stale close_failed/closed land
// after a healthy open and mark the endpoint port-zero). Advisory transitions
// (close and close-failed) are coalesced per endpoint — the newest queued
// advisory supersedes any earlier queued one — which bounds the queued backlog
// to a single advisory so a stalled advisory cannot delay a confirmed open past
// its deadline (finding 4). A dropped advisory is always superseded by a newer
// one for the same endpoint, never reordered relative to it.
//
// The OPEN transition is delivered exclusively by ReportOpen, which confirms
// delivery before returning and reports a full queue, a send error, or a
// timeout as an error instead of silently dropping.
type Reporter struct {
	mu     sync.Mutex
	ip     string
	send   func(ctx context.Context, ip string, port int, status string) error
	queue  []endpointEvent // the single FIFO: advisories and confirmed opens
	notify chan struct{}   // buffered (cap 1) wake-up for the drain
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

// isAdvisory reports whether the event is an advisory transition (rather than a
// confirmed open report). Confirmed opens are the only events that carry a
// per-caller confirmation channel.
func (e endpointEvent) isAdvisory() bool { return e.done == nil }

func NewReporter(send func(ctx context.Context, ip string, port int, status string) error) *Reporter {
	r := &Reporter{
		send:   send,
		notify: make(chan struct{}, 1),
	}
	go r.drain()
	return r
}

// signalLocked wakes the drain without blocking. Caller must hold r.mu.
func (r *Reporter) signalLocked() {
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

// drain is the reporter's single send goroutine. It pops events from the FIFO
// in order, so a confirmed open report is never sent before an advisory (or
// another open) that was enqueued earlier, and a later transition never
// overtakes an earlier one.
func (r *Reporter) drain() {
	for {
		r.mu.Lock()
		if len(r.queue) > 0 {
			e := r.queue[0]
			r.queue = r.queue[1:]
			r.mu.Unlock()
			r.deliver(e)
			continue
		}
		if r.closed {
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()
		<-r.notify
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
// events; a confirmed open still waiting on its result is answered with
// ErrReporterClosed so it cannot hang on a torn-down reporter.
func (r *Reporter) Close() {
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		for _, e := range r.queue {
			if e.done != nil {
				e.done <- ErrReporterClosed
			}
		}
		r.queue = nil
		r.signalLocked()
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
// the OK open_ack rather than racing it through this queue.
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
	// Coalesce per endpoint: a queued advisory immediately superseded by this
	// newer one is dropped, so the queued advisory backlog is at most one and
	// cannot delay a later confirmed open past its deadline. Only the TAIL is
	// coalesced, so an advisory is never dropped across an intervening open
	// report (which would reorder an endpoint's transitions); each dropped
	// advisory is strictly older than the one that replaced it.
	if n := len(r.queue); n > 0 && r.queue[n-1].isAdvisory() {
		r.queue[n-1] = e
	} else if len(r.queue) < reporterQueueCap {
		r.queue = append(r.queue, e)
	}
	// A full queue drops the advisory (advisory reports may be lost; the next
	// transition re-reports) and OnTransition never blocks the state loop.
	r.signalLocked()
	r.mu.Unlock()
}

// ReportOpen sends the open transition's endpoint report for grantedPort — the
// external port the router actually granted — and confirms delivery before
// returning. It is the open path's dedicated synchronous report: unlike the
// advisory transitions it is never silently dropped. A full queue, a send
// failure, or a confirmation that misses ctx's deadline is returned as an
// error, and the caller MUST treat the open as unsuccessful.
//
// ReportOpen is called after OnDemandPort.OpenForIf has returned, so it holds
// no state-loop lock, no ackMu, and no router I/O while it waits. The event is
// appended to the SAME FIFO advisories use, so it cannot overtake any advisory
// enqueued before it; the only block is the bounded confirmation select on ctx.
func (r *Reporter) ReportOpen(ctx context.Context, grantedPort int) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrReporterClosed
	}
	if len(r.queue) >= reporterQueueCap {
		r.mu.Unlock()
		return fmt.Errorf("endpoint open report enqueue: %w", errReporterQueueFull)
	}
	done := make(chan error, 1)
	e := endpointEvent{ip: r.ip, port: grantedPort, ctx: ctx, done: done}
	r.queue = append(r.queue, e)
	r.signalLocked()
	r.mu.Unlock()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("endpoint open report confirm: %w", ctx.Err())
	}
}
