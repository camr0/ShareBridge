package direct

import "sync"

type Reporter struct {
	mu     sync.Mutex
	ip     string
	send   func(ip string, port int, status string)
	ch     chan endpointEvent // queued sends: never blocks the state loop
	closed bool
}

type endpointEvent struct{ ip string; port int; status string }

func NewReporter(send func(ip string, port int, status string)) *Reporter {
	r := &Reporter{send: send, ch: make(chan endpointEvent, 64)}
	go r.drain()
	return r
}

func (r *Reporter) drain() {
	for e := range r.ch {
		r.send(e.ip, e.port, e.status)
	}
}

// Close terminates the drain goroutine by closing the event channel. It is
// idempotent and safe to call multiple times. After Close, OnTransition drops
// events without sending, so nothing is ever sent on the closed channel.
func (r *Reporter) Close() {
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		close(r.ch)
	}
	r.mu.Unlock()
}

func (r *Reporter) SetIP(ip string) {
	r.mu.Lock()
	r.ip = ip
	r.mu.Unlock()
}

func (r *Reporter) OnTransition(old, new PortState, grantedPort int) {
	var e endpointEvent
	switch new {
	case StateClosed:
		e = endpointEvent{port: 0}
	case StateOpen:
		e = endpointEvent{port: grantedPort}
	case StateCloseFailed:
		e = endpointEvent{port: grantedPort, status: "close_failed"}
	default:
		return // StateClosing (and anything else): no report.
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	e.ip = r.ip
	// Non-blocking enqueue: reports are advisory, so dropping an event when the
	// buffer is full is acceptable. This guarantees OnTransition never blocks the
	// state loop even if the `send` consumer stalls — the 64-slot buffer absorbs
	// bursts, and the next transition re-reports if an event is dropped.
	select {
	case r.ch <- e:
	default:
	}
	r.mu.Unlock()
}
