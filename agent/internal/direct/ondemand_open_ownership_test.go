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

// TestDiscardOpenMappingForCreatorAfterJoinIsNoOp is the M5 remediation R1
// fix-round finding: "created the mapping" is necessary but not sufficient for
// a rollback. A recipient that joined the SAME mapping instance after the
// creation is another owner, so the creator is no longer the sole owner and
// its failed open must not clear the mapping, its logical-open state, its
// session or its holds. This is the concurrent interleaving the daemon cannot
// order: with the mapping already created, another recipient's open commits on
// the state loop while the creator is still blocked in report confirmation.
func TestDiscardOpenMappingForCreatorAfterJoinIsNoOp(t *testing.T) {
	fc := newFakeClock(time.Now())
	p := newTestPort(fc, &recordingMapper{}, time.Minute)
	defer p.Close()

	creator, err := p.OpenForIfTracked("share", time.Minute, 0)
	if err != nil {
		t.Fatalf("creator open: %v", err)
	}
	if !creator.Created() {
		t.Fatalf("the first open must be the creator")
	}

	// A second recipient joins the SAME instance while the creator is still in
	// report confirmation: its open commits, its report is confirmed (the
	// OK-ack commit succeeds), and it starts a stream.
	joiner, err := p.OpenForIfTracked("share", 30*time.Second, 0)
	if err != nil {
		t.Fatalf("join open: %v", err)
	}
	if joiner.Created() {
		t.Fatalf("the second open must not report created=true")
	}
	if joiner.Instance() != creator.Instance() {
		t.Fatalf("the join must be on the creator's instance %d, got %d", creator.Instance(), joiner.Instance())
	}
	if _, ok := p.CommitOpenAck(0); !ok {
		t.Fatalf("the joining recipient's OK-ack commit must be authorized")
	}
	sess, err := p.BeginSession("share")
	if err != nil {
		t.Fatalf("BeginSession: %v", err)
	}
	hold := p.Begin()
	if hold == 0 {
		t.Fatalf("Begin returned the zero token on an open port")
	}

	// The creator's report now fails and it rolls back. It is no longer the sole
	// owner, so the rollback must be a no-op: the joining recipient (and its
	// stream) must survive.
	if err := p.DiscardOpenMappingFor(creator); err != nil {
		t.Fatalf("creator rollback after a join must be a nil no-op, got %v", err)
	}
	if !p.Open() {
		t.Fatalf("the creator's failure discarded a mapping another recipient joined")
	}
	if !p.SessionActive(sess) {
		t.Fatalf("the creator's failure cleared the joining recipient's session")
	}
	if got := p.Begin(); got != hold {
		t.Fatalf("the creator's failure cleared the joining recipient's holds (epoch %d, want %d)", got, hold)
	}
}

// TestDiscardOpenMappingForCreatorAfterRecipientRenewalIsNoOp pins the
// ownership counter to the state loop's own join/renewal decisions: a
// recipient whose longer lease extends the mapping's lease is an owner too, so
// the creator's later failure must not remove the instance it renewed.
func TestDiscardOpenMappingForCreatorAfterRecipientRenewalIsNoOp(t *testing.T) {
	fc := newFakeClock(time.Now())
	p := newTestPort(fc, &recordingMapper{}, time.Minute)
	defer p.Close()

	creator, err := p.OpenForIfTracked("share", time.Minute, 0)
	if err != nil {
		t.Fatalf("creator open: %v", err)
	}
	// A longer lease takes the renewal branch, writing the mapping again.
	if _, err := p.OpenForIfTracked("share", 5*time.Minute, 0); err != nil {
		t.Fatalf("recipient renewal: %v", err)
	}
	if err := p.DiscardOpenMappingFor(creator); err != nil {
		t.Fatalf("creator rollback after a renewal must be a nil no-op, got %v", err)
	}
	if !p.Open() {
		t.Fatalf("the creator's failure discarded a mapping a recipient renewed")
	}
}

// TestJoinCounterResetsWithANewMappingInstance proves the ownership counter is
// per-instance, not a monotonic port-global: a join on a discarded instance
// must not make the NEXT instance's creator look non-sole, otherwise the
// genuine created-open rollback would be disabled forever.
func TestJoinCounterResetsWithANewMappingInstance(t *testing.T) {
	fc := newFakeClock(time.Now())
	p := newTestPort(fc, &recordingMapper{}, time.Minute)
	defer p.Close()

	first, err := p.OpenForIfTracked("share", time.Minute, 0)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := p.OpenForIfTracked("share", time.Minute, 0); err != nil {
		t.Fatalf("join: %v", err)
	}
	if err := p.DiscardOpenMappingFor(first); err != nil {
		t.Fatalf("joined creator rollback: %v", err)
	}

	// A lockdown close removes the instance; the next creation is a fresh
	// instance whose creator is the sole owner again.
	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	second, err := p.OpenForIfTracked("share", time.Minute, 0)
	if err != nil {
		t.Fatalf("second creation: %v", err)
	}
	if !second.Created() {
		t.Fatalf("the post-close open must be a creation")
	}
	if second.Instance() == first.Instance() {
		t.Fatalf("a replacement mapping must be a distinct instance")
	}
	if err := p.DiscardOpenMappingFor(second); err != nil {
		t.Fatalf("the new instance's creator rollback must be allowed: %v", err)
	}
	if p.Open() {
		t.Fatalf("the new instance's creator rollback must discard it")
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
