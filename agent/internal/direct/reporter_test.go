package direct

import (
	"sync"
	"testing"
	"time"
)

type endpointReport struct{ ip string; port int; status string }

func TestReporterMapsTransitions(t *testing.T) {
	var mu sync.Mutex
	var got []endpointReport
	done := make(chan struct{})
	r := NewReporter(func(ip string, port int, status string) {
		mu.Lock()
		got = append(got, endpointReport{ip, port, status})
		n := len(got)
		mu.Unlock()
		if n == 3 {
			close(done)
		}
	})
	r.SetIP("1.2.3.4")
	r.OnTransition(StateClosed, StateOpen, 443)          // open report
	r.OnTransition(StateOpen, StateCloseFailed, 443)     // close-failed: nonzero port
	r.OnTransition(StateCloseFailed, StateClosed, 0)     // closed: port 0
	<-done
	mu.Lock()
	defer mu.Unlock()
	if got[0].port != 443 || got[0].status != "" { t.Fatalf("open report: %+v", got[0]) }
	if got[1].status != "close_failed" || got[1].port != 443 { t.Fatalf("close-failed report: %+v", got[1]) }
	if got[2].port != 0 || got[2].status != "" { t.Fatalf("closed report: %+v", got[2]) }
}

func TestReporterOnTransitionNeverBlocksWhenFull(t *testing.T) {
	release := make(chan struct{})
	r := NewReporter(func(ip string, port int, status string) { <-release })
	defer func() {
		close(release)
		r.Close()
	}()

	// The drain goroutine stalls on the first send (blocked on release), so the
	// 64-slot buffer fills: 1 event is consumed by drain, the remaining 64 sit
	// in the buffer.
	for i := 0; i < 65; i++ {
		r.OnTransition(StateClosed, StateOpen, 443)
	}

	// The buffer is now full. A plain `ch <-` send would block the caller here;
	// the drop-on-full enqueue must return promptly instead.
	done := make(chan struct{})
	go func() {
		r.OnTransition(StateClosed, StateOpen, 443)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("OnTransition blocked when the event buffer was full")
	}
}
