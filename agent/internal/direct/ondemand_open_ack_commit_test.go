package direct

// M4 remediation round C, fix round: the post-open success/rollback primitives
// the daemon's single OK-ack writer depends on.
//
// OpenForIf's commitOpenSuccess choke point authorizes the open *reply*. The
// direct-open flow then performs a bounded endpoint-report wait before it may
// ack, so it needs a SECOND generation re-validation after that wait
// (CommitOpenAck) plus a generation-guarded teardown for the report-failure arm
// (DiscardOpenMapping). These tests pin both: they authorize only the current
// generation's own mapping, they refuse (and discard) a superseded one, and
// they never touch a mapping a newer generation owns.

import (
	"testing"
	"time"
)

// TestCommitOpenAckAuthorizesCurrentGeneration: with no transition, the commit
// token is produced and carries the granted port; a repeat is idempotent.
func TestCommitOpenAckAuthorizesCurrentGeneration(t *testing.T) {
	fc := newFakeClock(time.Now())
	p := newTestPort(fc, &recordingMapper{}, time.Minute)
	defer p.Close()

	if err := p.OpenForIf("share", time.Minute, 0); err != nil {
		t.Fatalf("cold open: %v", err)
	}
	commit, ok := p.CommitOpenAck(0)
	if !ok {
		t.Fatalf("a current-generation open must be authorized")
	}
	if got := commit.GrantedPort(); got != 8443 {
		t.Fatalf("commit granted port = %d, want 8443", got)
	}
	if _, ok := p.CommitOpenAck(0); !ok {
		t.Fatalf("the commit re-validation must be idempotent (no state mutation)")
	}
	if !p.Open() {
		t.Fatalf("the authorized open must stay open")
	}
}

// TestCommitOpenAckRefusesAndDiscardsSupersededGeneration: a §13.4 transition
// published after the open was authorized refuses the post-report commit AND
// tears the stale mapping down (the discovered-identity discard), so a
// superseded open can neither ack OK nor leave a mapping behind.
func TestCommitOpenAckRefusesAndDiscardsSupersededGeneration(t *testing.T) {
	fc := newFakeClock(time.Now())
	base := &recordingMapper{}
	p := newTestPort(fc, base, time.Minute)
	defer p.Close()

	if err := p.OpenForIf("share", time.Minute, 0); err != nil {
		t.Fatalf("cold open: %v", err)
	}
	if !p.Open() {
		t.Fatalf("cold open did not open the port")
	}
	_, _, closedBefore, _ := base.snapshot()

	// Land the lockdown generation while the caller is in its report wait.
	p.SetGeneration(1, true)

	commit, ok := p.CommitOpenAck(0)
	if ok {
		t.Fatalf("a superseded open was authorized (granted port %d); that token becomes an OK open_ack", commit.GrantedPort())
	}
	if p.Open() {
		t.Fatalf("the superseded mapping must be discarded by the refused commit")
	}
	if got := p.State(); got != StateClosed {
		t.Fatalf("state = %v, want StateClosed after the discarded commit", got)
	}
	if _, _, closedAfter, _ := base.snapshot(); closedAfter == closedBefore {
		t.Fatalf("the superseded generation's mapping was not removed")
	}
}

// TestCommitOpenAckNeverTouchesNewerGenerationMapping is the safety half: a
// stale open (generation 0) whose commit is refused after an unlock advanced
// the generation must NOT tear down the mapping the newer generation opened.
func TestCommitOpenAckNeverTouchesNewerGenerationMapping(t *testing.T) {
	fc := newFakeClock(time.Now())
	p := newTestPort(fc, &recordingMapper{}, time.Minute)
	defer p.Close()

	// The daemon's post-lockdown/post-unlock stamp: epoch 2, unlocked.
	p.SetGeneration(2, false)
	if err := p.OpenForIf("share", time.Minute, 2); err != nil {
		t.Fatalf("post-unlock open: %v", err)
	}
	if !p.Open() {
		t.Fatalf("the post-unlock open must open the port")
	}

	commit, ok := p.CommitOpenAck(0) // the stale pre-lockdown generation
	if ok {
		t.Fatalf("a stale generation was authorized: granted port %d", commit.GrantedPort())
	}
	if !p.Open() {
		t.Fatalf("a refused stale commit must not close the newer generation's mapping")
	}
	if got := p.State(); got != StateOpen {
		t.Fatalf("state = %v, want StateOpen", got)
	}
	if _, ok := p.CommitOpenAck(2); !ok {
		t.Fatalf("the current generation's mapping must still be committable")
	}
}

// TestDiscardOpenMappingOnlyOwnGeneration pins the report-failure teardown: it
// closes the mapping while gen still owns it and is a no-op for a stale gen, so
// a failed-report open can never close a post-unlock reopen.
func TestDiscardOpenMappingOnlyOwnGeneration(t *testing.T) {
	fc := newFakeClock(time.Now())
	p := newTestPort(fc, &recordingMapper{}, time.Minute)
	defer p.Close()

	p.SetGeneration(2, false)
	if err := p.OpenForIf("share", time.Minute, 2); err != nil {
		t.Fatalf("post-unlock open: %v", err)
	}
	if err := p.DiscardOpenMapping(0); err != nil {
		t.Fatalf("discard with a stale generation: %v", err)
	}
	if !p.Open() {
		t.Fatalf("a stale discard must not close the newer generation's mapping")
	}
	if err := p.DiscardOpenMapping(2); err != nil {
		t.Fatalf("discard with the owning generation: %v", err)
	}
	if p.Open() {
		t.Fatalf("the owning generation's discard must close the mapping")
	}
	if got := p.State(); got != StateClosed {
		t.Fatalf("state = %v, want StateClosed", got)
	}
}
