package direct

// M5 remediation round 1 (post-M5 Sol audit, availability finding): the
// report/OK-ack failure rollback must target only the mapping THIS open
// CREATED, not every mapping whose §13.4 generation happens to match.
//
// OpenForIf now returns the open's identity (generation + mapping instance +
// whether this operation created the mapping), and DiscardOpenMappingFor uses
// it. These tests pin the discriminator:
//
//   - a created mapping is still rolled back;
//   - a JOINED open (the mapping already existed) is a nil no-op, so a later
//     recipient's failure cannot clear the owner's logical-open state;
//   - a superseded generation's mapping is never touched;
//   - a same-generation REPLACEMENT mapping (a newer mapping instance) is never
//     touched — the case the shared openGen cannot distinguish.
//
// This file references the post-fix API directly, so it is compile-only
// against the pre-fix base (see the round report).

import (
	"testing"
	"time"
)

// TestOpenForIfTrackedReportsCreationAndInstance pins the identity facts every
// caller depends on: a cold open created the mapping at a fresh instance; a
// join did not create it and reports the same instance.
func TestOpenForIfTrackedReportsCreationAndInstance(t *testing.T) {
	fc := newFakeClock(time.Now())
	p := newTestPort(fc, &recordingMapper{}, time.Minute)
	defer p.Close()

	first, err := p.OpenForIfTracked("share", time.Minute, 0)
	if err != nil {
		t.Fatalf("cold open: %v", err)
	}
	if !first.Created() {
		t.Fatalf("a cold open must report created=true")
	}
	if first.Generation() != 0 {
		t.Fatalf("outcome generation = %d, want 0", first.Generation())
	}
	if first.Instance() == 0 {
		t.Fatalf("a cold open must report a non-zero mapping instance")
	}

	second, err := p.OpenForIfTracked("share", time.Minute, 0)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if second.Created() {
		t.Fatalf("an open onto an existing mapping must report created=false")
	}
	if second.Instance() != first.Instance() {
		t.Fatalf("a join must report the existing mapping's instance %d, got %d", first.Instance(), second.Instance())
	}
}

// TestDiscardOpenMappingForCreatedRollsBack preserves the genuine rollback: a
// mapping this open created is discarded by its own identity.
func TestDiscardOpenMappingForCreatedRollsBack(t *testing.T) {
	fc := newFakeClock(time.Now())
	p := newTestPort(fc, &recordingMapper{}, time.Minute)
	defer p.Close()

	outcome, err := p.OpenForIfTracked("share", time.Minute, 0)
	if err != nil {
		t.Fatalf("cold open: %v", err)
	}
	if err := p.DiscardOpenMappingFor(outcome); err != nil {
		t.Fatalf("discard of the created mapping: %v", err)
	}
	if p.Open() {
		t.Fatalf("the created mapping's own discard must close it")
	}
	if got := p.State(); got != StateClosed {
		t.Fatalf("state = %v, want StateClosed", got)
	}
	// Idempotent: a repeat is a nil no-op.
	if err := p.DiscardOpenMappingFor(outcome); err != nil {
		t.Fatalf("repeat discard must be a nil no-op, got %v", err)
	}
}

// TestDiscardOpenMappingForJoinedIsNoOp is the audit's port-level root cause: a
// later open that joined an already-open mapping must NOT clear the owner's
// logical-open state (or its sessions) when it rolls back.
func TestDiscardOpenMappingForJoinedIsNoOp(t *testing.T) {
	fc := newFakeClock(time.Now())
	p := newTestPort(fc, &recordingMapper{}, time.Minute)
	defer p.Close()

	owner, err := p.OpenForIfTracked("share", time.Minute, 0)
	if err != nil {
		t.Fatalf("owner open: %v", err)
	}
	if !owner.Created() {
		t.Fatalf("the first open must be the creator")
	}
	sess, err := p.BeginSession("share")
	if err != nil {
		t.Fatalf("BeginSession: %v", err)
	}
	hold := p.Begin()
	if hold == 0 {
		t.Fatalf("Begin returned the zero token on an open port")
	}

	join, err := p.OpenForIfTracked("share", 5*time.Minute, 0)
	if err != nil {
		t.Fatalf("join open: %v", err)
	}
	if join.Created() {
		t.Fatalf("a join must not report created=true")
	}
	if err := p.DiscardOpenMappingFor(join); err != nil {
		t.Fatalf("discard of a joined open must be a nil no-op, got %v", err)
	}
	if !p.Open() {
		t.Fatalf("a joined open's rollback must not close the owner's mapping")
	}
	if !p.SessionActive(sess) {
		t.Fatalf("a joined open's rollback must not clear the owner's session")
	}
	if got := p.Begin(); got != hold {
		t.Fatalf("a joined open's rollback must not clear the owner's holds (epoch %d, want %d)", got, hold)
	}
}

// TestDiscardOpenMappingForSupersededGenerationNoOp pins the preserved
// generation safety: a stale outcome never touches a newer generation's
// mapping.
func TestDiscardOpenMappingForSupersededGenerationNoOp(t *testing.T) {
	fc := newFakeClock(time.Now())
	p := newTestPort(fc, &recordingMapper{}, time.Minute)
	defer p.Close()

	p.SetGeneration(2, false)
	stale, err := p.OpenForIfTracked("share", time.Minute, 2)
	if err != nil {
		t.Fatalf("generation-2 open: %v", err)
	}
	// A post-unlock gen-3 open REBINDS the mapping to generation 3 (a longer
	// lease takes the renewal path), so it is now the newer generation's
	// mapping.
	p.SetGeneration(3, false)
	if _, err := p.OpenForIfTracked("share", 5*time.Minute, 3); err != nil {
		t.Fatalf("generation-3 open: %v", err)
	}
	if err := p.DiscardOpenMappingFor(stale); err != nil {
		t.Fatalf("stale-generation discard: %v", err)
	}
	if !p.Open() {
		t.Fatalf("a stale generation's discard must not close the newer generation's mapping")
	}
	if _, ok := p.CommitOpenAck(3); !ok {
		t.Fatalf("the current generation's mapping must remain committable")
	}
}

// TestDiscardOpenMappingForSameEpochNewerInstanceUntouched is the discriminator
// proof: within ONE generation, a mapping that was closed and REPLACED by a
// newer creation must never be torn down by the earlier open's stale rollback.
// The generation alone (openGen) cannot distinguish the two; the mapping
// instance does.
func TestDiscardOpenMappingForSameEpochNewerInstanceUntouched(t *testing.T) {
	fc := newFakeClock(time.Now())
	p := newTestPort(fc, &recordingMapper{}, time.Minute)
	defer p.Close()

	p.SetGeneration(2, false)
	first, err := p.OpenForIfTracked("share", time.Minute, 2)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	// The first mapping goes away (its own rollback: it created it).
	if err := p.DiscardOpenMappingFor(first); err != nil {
		t.Fatalf("first rollback: %v", err)
	}
	// A SAME-GENERATION replacement map instance is created by another open.
	second, err := p.OpenForIfTracked("share", time.Minute, 2)
	if err != nil {
		t.Fatalf("replacement open: %v", err)
	}
	if second.Generation() != first.Generation() {
		t.Fatalf("the replacement must be in the same generation")
	}
	if second.Instance() == first.Instance() {
		t.Fatalf("the replacement must be a distinct mapping instance")
	}
	// The stale outcome's rollback must be a no-op against the replacement.
	if err := p.DiscardOpenMappingFor(first); err != nil {
		t.Fatalf("stale-instance discard: %v", err)
	}
	if !p.Open() {
		t.Fatalf("a stale instance's discard tore down the same-epoch replacement mapping")
	}
	if _, ok := p.CommitOpenAck(2); !ok {
		t.Fatalf("the replacement mapping must remain committable")
	}
}
