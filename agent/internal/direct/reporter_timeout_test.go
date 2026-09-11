package direct

// M4 closeout batch 1, finding 4 (availability): the reporter's single drain
// serialized advisory close reports in front of the confirmed open report, and
// one advisory send could occupy the drain for the full 5s transport bound
// while the daemon's open confirmation expires at 3s — an unnecessary
// fail-closed. The fix both prioritizes a queued confirmed-open event over
// queued advisories AND bounds an advisory send well below the open deadline.
//
// This test proves BOTH mechanisms from the outside: one advisory is in flight
// and three more are queued when the open is requested. With only the advisory
// bound (no priority) the open would still wait behind the three queued
// advisories and miss its deadline; with only priority (no bound) the in-flight
// advisory's 5s sends it past the deadline. The open must complete inside its
// own deadline despite both.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestReporterOpenConfirmationOvertakesStalledAdvisories(t *testing.T) {
	// Advisory sends (port 0 "closed" and nonzero "close_failed") block until
	// their own (bounded) ctx expires — exactly a wedged transport that still
	// honors cancellation. The open report send is always immediate.
	var advisories atomic.Int32
	r := NewReporter(func(ctx context.Context, ip string, port int, status string) error {
		if status == "close_failed" || port == 0 {
			advisories.Add(1)
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	})
	defer r.Close()
	r.SetIP("203.0.113.7")

	// One advisory in flight, three queued behind it.
	for i := 0; i < 4; i++ {
		r.OnTransition(StateOpen, StateClosed, 0)
	}

	// The open confirmation deadline mirrors the daemon's production window and
	// is deliberately ABOVE the advisory bound but far BELOW the 5s transport
	// bound: the open must not be delayed behind advisories.
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()

	if err := r.ReportOpen(ctx, 52017); err != nil {
		t.Fatalf("the confirmed open report was delayed behind advisory sends: %v", err)
	}
	if advisories.Load() == 0 {
		t.Fatalf("the fixture never delivered an advisory send; the test did not exercise the queue")
	}
}
