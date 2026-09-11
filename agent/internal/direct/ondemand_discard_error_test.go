package direct

// M4 closeout batch 1, finding 3 (agent half): opDiscardOpen used to reply
// without the deletion error, so the daemon's rollback path could not tell a
// completed discard from a failed one — the "visible escalation" was only
// internally/test-visible. DiscardOpenMapping must surface the IMMEDIATE
// deletion failure so the caller can log it (and so the port's retry/escalation
// path is entered knowingly); the operation stays idempotent, and a failure
// must not make a second discard (or another generation's discard) error.

import (
	"testing"
	"time"
)

// TestDiscardOpenMappingPropagatesDeleteError pins the propagation: when the
// router refuses the delete, the caller must receive a non-nil error and the
// port must retain the escalation state (non-nil CloseError, StateClosing) so
// the retry timer keeps trying and a later escalation to StateCloseFailed +
// the close_failed report is possible.
func TestDiscardOpenMappingPropagatesDeleteError(t *testing.T) {
	fc := newFakeClock(time.Now())
	m := &recordingMapper{alwaysFailDel: true}
	p := newTestPort(fc, m, time.Minute)
	defer p.Close()

	if err := p.OpenForIf("share", time.Minute, 0); err != nil {
		t.Fatalf("cold open: %v", err)
	}
	if !p.Open() {
		t.Fatalf("the cold open must have created the mapping")
	}

	err := p.DiscardOpenMapping(0)
	if err == nil {
		t.Fatalf("DiscardOpenMapping must surface the initial deletion failure instead of swallowing it")
	}
	if p.Open() {
		t.Fatalf("the logical open must be cleared even when the router delete failed")
	}
	if got := p.State(); got != StateClosing {
		t.Fatalf("state = %v, want StateClosing (the delete is handed to the retry timer)", got)
	}
	if p.CloseError() == nil {
		t.Fatalf("the deletion failure must stay visible via CloseError")
	}
	if _, _, calls, _ := m.snapshot(); calls == 0 {
		t.Fatalf("no DeletePortMapping was attempted")
	}
}

// TestDiscardOpenMappingIdempotentAfterFailure: the discard is a one-shot
// logical rollback. A repeat (the guard already discarded, or a retried
// caller) must return nil and must not error, while the router delete stays
// with the port's retry timer.
func TestDiscardOpenMappingIdempotentAfterFailure(t *testing.T) {
	fc := newFakeClock(time.Now())
	m := &recordingMapper{alwaysFailDel: true}
	p := newTestPort(fc, m, time.Minute)
	defer p.Close()

	if err := p.OpenForIf("share", time.Minute, 0); err != nil {
		t.Fatalf("cold open: %v", err)
	}
	if err := p.DiscardOpenMapping(0); err == nil {
		t.Fatalf("first discard must surface the delete failure")
	}
	if err := p.DiscardOpenMapping(0); err != nil {
		t.Fatalf("a repeat discard of an already-discarded open must be a nil no-op, got %v", err)
	}
	// A stale generation's discard is likewise a nil no-op and never touches
	// state it does not own.
	if err := p.DiscardOpenMapping(7); err != nil {
		t.Fatalf("a stale-generation discard must be a nil no-op, got %v", err)
	}
}
