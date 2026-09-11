package direct

// M4 closeout batch 1b, finding A2 (F4 regression): the prioritized openCh made
// a confirmed open report overtake advisory transitions that were queued BEFORE
// it, so control could apply a stale close_failed/closed after the healthy open
// and mark the endpoint port-zero/suppressed. The reporter must deliver events
// in the order the port state machine produced them: a single FIFO, with
// per-endpoint coalescing of superseded advisory states so a stalled advisory
// backlog still cannot delay a confirmed open past its deadline.
//
// These tests are channel-gated (the advisory send is held in flight), so the
// ordering assertion is deterministic rather than scheduling-dependent.

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestReporterPreservesTransitionOrderAcrossOpen reproduces the exact A2 stale
// sequence deterministically: a close_failed advisory is in flight, the next
// open deletes the lingering mapping (enqueuing the closed advisory) and then
// asks for the confirmed open report. The reporter must deliver the earlier
// closed advisory BEFORE the open confirmation — control applies reports in
// arrival order, so an overtaking open leaves control's final state port-zero
// (direct suppressed) for a healthy endpoint.
//
// The advisory send is held past its own bounded ctx so the closed advisory and
// the open report are both pending while close_failed is still in flight; the
// open is enqueued well inside that window. At the base revision the
// prioritized openCh delivers the open before the queued closed advisory, so
// this test fails there on both the delivery order and the final state.
func TestReporterPreservesTransitionOrderAcrossOpen(t *testing.T) {
	var mu sync.Mutex
	var got []endpointReport
	advisorySends := 0
	advisoriesBeforeOpen := -1

	closeFailedEntered := make(chan struct{})
	allDelivered := make(chan struct{})

	r := NewReporter(func(ctx context.Context, ip string, port int, status string) error {
		isAdvisory := status == "close_failed" || port == 0
		mu.Lock()
		if isAdvisory {
			advisorySends++
		} else {
			advisoriesBeforeOpen = advisorySends
		}
		got = append(got, endpointReport{ip, port, status})
		n := len(got)
		mu.Unlock()
		if !isAdvisory {
			if n == 3 {
				close(allDelivered)
			}
			return nil
		}
		if status == "close_failed" {
			close(closeFailedEntered)
		}
		// Hold each advisory past its own bounded ctx so the closed advisory and
		// the open report are both pending while close_failed is in flight.
		<-ctx.Done()
		if n == 3 {
			close(allDelivered)
		}
		return ctx.Err()
	})
	defer r.Close()
	r.SetIP("203.0.113.7")

	// 1. A close failure leaves a lingering mapping: the advisory is queued and
	//    the drain begins sending it (the test holds it in flight).
	r.OnTransition(StateOpen, StateCloseFailed, 443)
	awaitSignal(t, closeFailedEntered, "close_failed advisory entering the drain")

	// 2. The next open deletes that lingering mapping -> the port publishes
	//    StateClosed (an advisory), then maps and the daemon issues the
	//    confirmed open report. Both are enqueued while close_failed is in
	//    flight, exactly reproducing the production stale sequence.
	r.OnTransition(StateCloseFailed, StateClosed, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.ReportOpen(ctx, 52017); err != nil {
		t.Fatalf("ReportOpen: %v", err)
	}
	awaitSignal(t, allDelivered, "all three reports delivered")

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("delivered reports = %d (%+v), want 3", len(got), got)
	}
	// The load-bearing assertion: the open confirmation must NOT overtake the
	// earlier closed advisory.
	if advisoriesBeforeOpen != 2 {
		t.Fatalf("advisories delivered before the open = %d, want 2 (close_failed + closed); the open overtook the queued advisory", advisoriesBeforeOpen)
	}
	// Control applies reports in arrival order, so the LAST delivered report
	// determines its final state: it must be the healthy open, not the stale
	// closed (port 0) or close_failed suppression.
	if got[2].port != 52017 || got[2].status != "" {
		t.Fatalf("last delivered report = %+v, want the healthy open (port 52017); control's final state is stale", got[2])
	}
	if got[1].port != 0 || got[1].status != "" {
		t.Fatalf("second delivered report = %+v, want the earlier closed advisory (port 0)", got[1])
	}
}

// TestReporterOpenConfirmationCompletesUnderStalledAdvisoryBacklog is the
// deterministic rework of the finding-4 availability test. One advisory is in
// flight and three more are queued when the open is requested; the open must
// still complete inside its own (short) deadline. The ordering half asserts the
// open did not overtake the queued backlog (the A2 regression), and the
// coalescing half bounds the backlog: without per-endpoint coalescing the three
// queued advisories each consume the 1s advisory bound and the open misses the
// 2.5s deadline.
func TestReporterOpenConfirmationCompletesUnderStalledAdvisoryBacklog(t *testing.T) {
	var mu sync.Mutex
	advisorySends := 0
	advisoriesBeforeOpen := -1

	var enteredOnce sync.Once
	advisoryEntered := make(chan struct{})

	r := NewReporter(func(ctx context.Context, ip string, port int, status string) error {
		isAdvisory := status == "close_failed" || port == 0
		mu.Lock()
		if isAdvisory {
			advisorySends++
		} else {
			advisoriesBeforeOpen = advisorySends
		}
		mu.Unlock()
		if !isAdvisory {
			return nil
		}
		enteredOnce.Do(func() { close(advisoryEntered) })
		// Block past the advisory send bound exactly like a wedged transport
		// that still honors cancellation.
		<-ctx.Done()
		return ctx.Err()
	})
	defer r.Close()
	r.SetIP("203.0.113.7")

	// One advisory in flight, three queued behind it.
	r.OnTransition(StateOpen, StateClosed, 0)
	awaitSignal(t, advisoryEntered, "first advisory entering the drain")
	// The first advisory is now in flight (dequeued); the three queued behind it
	// coalesce per endpoint to a single pending advisory.
	for i := 0; i < 3; i++ {
		r.OnTransition(StateOpen, StateClosed, 0)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	if err := r.ReportOpen(ctx, 52017); err != nil {
		t.Fatalf("the confirmed open report was delayed behind the advisory backlog: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	// The in-flight advisory plus exactly one coalesced backlog advisory must be
	// delivered before the open: the open neither overtakes the queued backlog
	// (as the priority channel did) nor replays it uncoalesced.
	if advisoriesBeforeOpen != 2 {
		t.Fatalf("advisories delivered before the open = %d, want 2 (in-flight + one coalesced backlog); the open reordered or the backlog was not coalesced", advisoriesBeforeOpen)
	}
}
