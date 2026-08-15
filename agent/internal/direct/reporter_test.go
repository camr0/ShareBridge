package direct

import (
	"sync"
	"testing"
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
