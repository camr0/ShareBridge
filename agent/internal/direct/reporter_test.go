package direct

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type endpointReport struct {
	ip     string
	port   int
	status string
}

func TestReporterMapsTransitions(t *testing.T) {
	var mu sync.Mutex
	var got []endpointReport
	openDelivered := make(chan struct{})
	closeFailedDelivered := make(chan struct{})
	closedDelivered := make(chan struct{})
	r := NewReporter(func(ctx context.Context, ip string, port int, status string) error {
		mu.Lock()
		got = append(got, endpointReport{ip, port, status})
		mu.Unlock()
		switch {
		case status == "close_failed":
			close(closeFailedDelivered)
		case port == 0:
			close(closedDelivered)
		default:
			close(openDelivered)
		}
		return nil
	})
	defer r.Close()
	r.SetIP("1.2.3.4")

	// The open report is the confirmed synchronous path (ReportOpen), not an
	// advisory transition: the reporter must deliver it exactly once with the
	// granted external port.
	if err := r.ReportOpen(context.Background(), 443); err != nil {
		t.Fatalf("ReportOpen: %v", err)
	}
	awaitSignal(t, openDelivered, "open report delivery")
	// Advisory close transitions still flow through OnTransition. Each delivery
	// is awaited before the next is enqueued so the per-endpoint coalescing
	// (which only drops an advisory superseded by a newer queued one) cannot
	// collapse the distinct states this test pins.
	r.OnTransition(StateOpen, StateCloseFailed, 443) // close-failed: nonzero port
	awaitSignal(t, closeFailedDelivered, "close_failed report delivery")
	r.OnTransition(StateCloseFailed, StateClosed, 0) // closed: port 0
	awaitSignal(t, closedDelivered, "closed report delivery")

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("delivered reports = %d (%+v), want 3", len(got), got)
	}
	if got[0].port != 443 || got[0].status != "" {
		t.Fatalf("open report: %+v", got[0])
	}
	if got[1].status != "close_failed" || got[1].port != 443 {
		t.Fatalf("close-failed report: %+v", got[1])
	}
	if got[2].port != 0 || got[2].status != "" {
		t.Fatalf("closed report: %+v", got[2])
	}
}

// TestReporterOnTransitionIgnoresOpen pins the split: the open transition is
// NOT reported by the advisory queue (the daemon's confirmed ReportOpen owns
// it), so a plain OnTransition(→StateOpen) delivers nothing.
func TestReporterOnTransitionIgnoresOpen(t *testing.T) {
	var mu sync.Mutex
	var got []endpointReport
	r := NewReporter(func(ctx context.Context, ip string, port int, status string) error {
		mu.Lock()
		got = append(got, endpointReport{ip, port, status})
		mu.Unlock()
		return nil
	})
	defer r.Close()
	r.SetIP("1.2.3.4")
	r.OnTransition(StateClosed, StateOpen, 443)
	r.OnTransition(StateClosed, StateClosing, 0)
	// A close report is the ordered barrier: once it lands, the ignored open
	// transition provably produced nothing.
	r.OnTransition(StateOpen, StateClosed, 0)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].port != 0 || got[0].status != "" {
		t.Fatalf("OnTransition must report only the close transition, got %#v", got)
	}
}

func TestReporterOnTransitionNeverBlocksWhenFull(t *testing.T) {
	release := make(chan struct{})
	r := NewReporter(func(ctx context.Context, ip string, port int, status string) error {
		<-release
		return nil
	})
	defer func() {
		close(release)
		r.Close()
	}()

	// The drain goroutine stalls on the first send (blocked on release). The
	// identical close advisories coalesce per endpoint into a single pending
	// advisory, so the queue never grows; OnTransition must still never block
	// the caller when the drain is wedged.
	for i := 0; i < 65; i++ {
		r.OnTransition(StateOpen, StateClosed, 0)
	}

	// A plain blocking enqueue would hang the caller while the drain is wedged;
	// the non-blocking enqueue must return promptly instead.
	done := make(chan struct{})
	go func() {
		r.OnTransition(StateOpen, StateClosed, 0)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("OnTransition blocked when the event buffer was full")
	}
}

// TestReporterReportOpenConfirmsBeforeReturning proves ReportOpen does not
// return until the send has completed: the send is held open by the test, and
// ReportOpen cannot have returned while it is blocked (its own deadline is far
// away).
func TestReporterReportOpenConfirmsBeforeReturning(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var deliveredPort int
	r := NewReporter(func(ctx context.Context, ip string, port int, status string) error {
		entered <- struct{}{}
		<-release
		deliveredPort = port
		return nil
	})
	defer r.Close()
	r.SetIP("203.0.113.7")

	result := make(chan error, 1)
	go func() { result <- r.ReportOpen(context.Background(), 52017) }()
	<-entered

	select {
	case err := <-result:
		t.Fatalf("ReportOpen returned (%v) before the send completed", err)
	default:
	}

	close(release)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("ReportOpen: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReportOpen did not return after the send completed")
	}
	if deliveredPort != 52017 {
		t.Fatalf("delivered port = %d, want the granted 52017", deliveredPort)
	}
}

// TestReporterReportOpenSendError surfaces a transport failure as an error.
func TestReporterReportOpenSendError(t *testing.T) {
	want := errors.New("signaling write failed")
	r := NewReporter(func(ctx context.Context, ip string, port int, status string) error {
		return want
	})
	defer r.Close()
	r.SetIP("203.0.113.7")

	if err := r.ReportOpen(context.Background(), 443); !errors.Is(err, want) {
		t.Fatalf("ReportOpen error = %v, want %v", err, want)
	}
}

// TestReporterReportOpenQueueFullIsBounded proves a stalled drain blocks the
// open report only until its deadline and then errors, rather than dropping the
// report silently. Per-endpoint coalescing keeps the advisory side of the FIFO
// at (at most) one pending advisory, so this exercises the bounded open
// confirmation rather than a literally full advisory queue.
func TestReporterReportOpenQueueFullIsBounded(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	r := NewReporter(func(ctx context.Context, ip string, port int, status string) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return nil
	})
	defer r.Close()
	r.SetIP("203.0.113.7")

	// Wedge the drain; the repeated advisories coalesce to one pending advisory.
	r.OnTransition(StateOpen, StateClosed, 0)
	<-entered
	for i := 0; i < 64; i++ {
		r.OnTransition(StateOpen, StateClosed, 0)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := r.ReportOpen(ctx, 443)
	if err == nil {
		t.Fatal("ReportOpen on a full queue returned nil, want a bounded error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("ReportOpen on a full queue took %s, want it bounded near the 100ms deadline", elapsed)
	}
}

// TestReporterReportOpenConfirmationTimeout proves that a send which never
// completes inside the caller's deadline is reported as an error.
func TestReporterReportOpenConfirmationTimeout(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	r := NewReporter(func(ctx context.Context, ip string, port int, status string) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release // deliberately ignore ctx: the caller's bound must still fire
		return nil
	})
	defer r.Close()
	r.SetIP("203.0.113.7")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := r.ReportOpen(ctx, 443)
	if err == nil {
		t.Fatal("ReportOpen with a stalled send returned nil, want a deadline error")
	}
	select {
	case <-entered:
	default:
		t.Fatal("the open report was never handed to the drain")
	}
}
