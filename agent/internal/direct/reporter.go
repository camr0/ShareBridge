package direct

import "sync"

type Reporter struct {
	mu   sync.Mutex
	ip   string
	send func(ip string, port int, status string)
	ch   chan endpointEvent // queued sends: never blocks the state loop
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

func (r *Reporter) SetIP(ip string) {
	r.mu.Lock()
	r.ip = ip
	r.mu.Unlock()
}

func (r *Reporter) OnTransition(old, new PortState, grantedPort int) {
	r.mu.Lock()
	ip := r.ip
	r.mu.Unlock()
	switch new {
	case StateClosed:
		r.ch <- endpointEvent{ip, 0, ""}
	case StateOpen:
		r.ch <- endpointEvent{ip, grantedPort, ""}
	case StateCloseFailed:
		r.ch <- endpointEvent{ip, grantedPort, "close_failed"}
	}
	// StateClosing: no report (intermediate).
}
