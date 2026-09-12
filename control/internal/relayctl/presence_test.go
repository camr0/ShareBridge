package relayctl

// Tests for the Task 15 presence view (plan Task 15, spec §§4.2, 7.1, 7.3,
// 12, 15.7): control's ephemeral, in-memory leased view of gateway-authoritative
// tunnel presence. The view is the ONLY presence state route selection may
// read; the persisted agents.relay_last_seen_at row is diagnostics only.
//
// Freshness policy under test (owned by this task):
//   - a lease is valid until min(gateway-stated expiry, receipt + 60 s);
//   - a gateway-stated expiry beyond receipt + 60 s is future-dated and the
//     batch is rejected (fail closed, gateway reconciles via snapshot);
//   - an expired lease makes the relay unavailable, lazily, at Available time;
//   - facts from an older/unknown boot and stale revisions are discarded;
//   - a revision gap is rejected without mutation (never guessed across).
//
// Single-writer structure under test: availability changes only through the
// two PresenceSink methods, which the Task 11 mTLS sync server is the only
// production caller of. Agent `relay_client_state` never reaches this view.

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

// presenceTestBoot is the gateway boot ID the tests' snapshots come from.
const presenceTestBoot = "gateway-boot-ctrl15"

// presenceViewBase is the fixed fake-clock base.
var presenceViewBase = time.Unix(1800000000, 0).UTC()

// fakeClock is a mutex-guarded fake clock the tests advance explicitly.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(now time.Time) *fakeClock { return &fakeClock{now: now} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fixedRoutes is a stub RouteRevisionSource with a fixed current revision.
type fixedRoutes struct{ revision uint64 }

func (f fixedRoutes) CurrentRevision() uint64 { return f.revision }

// presenceOnline builds one online presence fact.
func presenceOnline(boot string, revision uint64, agentID string, port int, generation uint64, expiresAt time.Time) PresenceEvent {
	return PresenceEvent{
		GatewayBootID:  boot,
		Revision:       revision,
		AgentRecordID:  agentID,
		RelayPort:      port,
		Generation:     generation,
		State:          PresenceStateOnline,
		LeaseExpiresAt: expiresAt.Format(time.RFC3339),
	}
}

// presenceOffline builds one offline presence fact.
func presenceOffline(boot string, revision uint64, agentID string, port int, generation uint64) PresenceEvent {
	return PresenceEvent{
		GatewayBootID: boot,
		Revision:      revision,
		AgentRecordID: agentID,
		RelayPort:     port,
		Generation:    generation,
		State:         PresenceStateOffline,
	}
}

// presenceSnapshot wraps events in a snapshot envelope (one boot, one revision).
func presenceSnapshot(boot string, revision uint64, events []PresenceEvent) PresenceEnvelope {
	return PresenceEnvelope{Version: ProtocolVersion, Events: events}
}

// newPresenceTestView builds a view with the fake clock and PocketBase app
// injected (the app backs the relay_last_seen_at diagnostics persistence).
func newPresenceTestView(t *testing.T, app core.App, routes RouteRevisionSource, now func() time.Time) *PresenceView {
	t.Helper()
	view, err := NewPresenceView(PresenceViewConfig{
		App:    app,
		Routes: routes,
		Now:    now,
	})
	if err != nil {
		t.Fatalf("new presence view: %v", err)
	}
	return view
}

// setAgentLastSeen writes agents.relay_last_seen_at directly, simulating a
// freshest-possible diagnostic row (as anything but a gateway lease could).
func setAgentLastSeen(t *testing.T, app core.App, agentRecordID string, at time.Time) {
	t.Helper()
	rec, err := app.FindRecordById("agents", agentRecordID)
	if err != nil {
		t.Fatalf("load agent %s: %v", agentRecordID, err)
	}
	rec.Set("relay_last_seen_at", at)
	if err := app.Save(rec); err != nil {
		t.Fatalf("save agent last seen: %v", err)
	}
}

// agentLastSeen reads the persisted diagnostic value.
func agentLastSeen(t *testing.T, app core.App, agentRecordID string) time.Time {
	t.Helper()
	rec, err := app.FindRecordById("agents", agentRecordID)
	if err != nil {
		t.Fatalf("load agent %s: %v", agentRecordID, err)
	}
	return rec.GetDateTime("relay_last_seen_at").Time()
}

// clockAdvanceTo moves the fake clock to an absolute instant.
func clockAdvanceTo(t *testing.T, clock *fakeClock, at time.Time) {
	t.Helper()
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = at
}

// TestPresenceSnapshotRestartEmptyClearsLeases pins the §15.1 empty-snapshot
// case: "on gateway restart the snapshot is empty until FRP clients reconnect
// and re-register". An empty snapshot is a wholesale lease clear (never a
// panic), and — carrying no boot ID or revision — leaves boot/revision
// tracking untouched so stale facts stay discardable as before.
func TestPresenceSnapshotRestartEmptyClearsLeases(t *testing.T) {
	app := newPublisherTestApp(t)
	_, agentA := createPublisherAgent(t, app, "p15-a", "sb0a1b2c3d", 10001, 3)
	clock := newFakeClock(presenceViewBase)
	view := newPresenceTestView(t, app, fixedRoutes{revision: 5}, clock.Now)

	boot := presenceSnapshot(presenceTestBoot, 10, []PresenceEvent{
		presenceOnline(presenceTestBoot, 10, agentA, 10001, 3, presenceViewBase.Add(45*time.Second)),
	})
	if err := view.ApplyPresenceSnapshot(boot); err != nil {
		t.Fatalf("apply boot snapshot: %v", err)
	}
	if !view.Available(agentA, 10001, 3, 5, clock.Now()) {
		t.Fatalf("lease not established")
	}

	if err := view.ApplyPresenceSnapshot(PresenceEnvelope{Version: ProtocolVersion}); err != nil {
		t.Fatalf("apply empty snapshot: %v", err)
	}
	if view.Available(agentA, 10001, 3, 5, clock.Now()) {
		t.Fatalf("empty snapshot did not clear the lease")
	}
	if view.bootID != presenceTestBoot || view.lastRevision != 10 {
		t.Fatalf("empty snapshot disturbed boot/revision tracking: boot %q revision %d", view.bootID, view.lastRevision)
	}
	// Stale-fact discipline still holds after the clear.
	replay := PresenceEnvelope{Version: ProtocolVersion, Events: []PresenceEvent{
		presenceOnline(presenceTestBoot, 10, agentA, 10001, 3, clock.Now().Add(45*time.Second)),
	}}
	if err := view.ApplyPresenceEvents(replay); err != nil {
		t.Fatalf("apply replay after clear: %v", err)
	}
	if view.Available(agentA, 10001, 3, 5, clock.Now()) {
		t.Fatalf("stale revision resurrected the lease after the clear")
	}
}

// TestControlLoadsFreshPresenceSnapshotOnRestart pins §4.2: on control restart
// the in-memory view is empty and the fresh gateway snapshot is the new truth —
// wholesale, atomic replacement, including across a gateway boot change. A
// later reconcile snapshot replaces the state again: a lease absent from the
// newest snapshot is gone, never served from memory.
func TestControlLoadsFreshPresenceSnapshotOnRestart(t *testing.T) {
	app := newPublisherTestApp(t)
	_, agentA := createPublisherAgent(t, app, "p15-a", "sb0a1b2c3d", 10001, 3)
	_, agentB := createPublisherAgent(t, app, "p15-b", "sb0d4c5b6a", 10002, 1)
	clock := newFakeClock(presenceViewBase)
	routes := fixedRoutes{revision: 500}
	view := newPresenceTestView(t, app, routes, clock.Now)

	// Control restart: view starts empty; the gateway posts its snapshot
	// (boot gateway-boot-ctrl15 — the gateway announces itself by snapshot).
	snapshot := presenceSnapshot(presenceTestBoot, 7, []PresenceEvent{
		presenceOnline(presenceTestBoot, 7, agentA, 10001, 3, presenceViewBase.Add(45*time.Second)),
		presenceOnline(presenceTestBoot, 7, agentB, 10002, 1, presenceViewBase.Add(45*time.Second)),
	})
	if err := view.ApplyPresenceSnapshot(snapshot); err != nil {
		t.Fatalf("apply snapshot: %v", err)
	}
	if view.bootID != presenceTestBoot || view.lastRevision != 7 {
		t.Fatalf("snapshot not adopted: boot %q revision %d", view.bootID, view.lastRevision)
	}
	if !view.Available(agentA, 10001, 3, 500, clock.Now()) {
		t.Fatalf("agent A lease from snapshot not available")
	}
	if !view.Available(agentB, 10002, 1, 500, clock.Now()) {
		t.Fatalf("agent B lease from snapshot not available")
	}
	// Exact (agent, port, generation) join: a superseded generation or wrong
	// port never serves.
	if view.Available(agentA, 10001, 2, 500, clock.Now()) {
		t.Fatalf("superseded generation joined a live lease")
	}
	if view.Available(agentA, 19999, 3, 500, clock.Now()) {
		t.Fatalf("wrong port joined a live lease")
	}
	// Stale route revision: the caller's route read is no longer current.
	if view.Available(agentA, 10001, 3, 499, clock.Now()) {
		t.Fatalf("stale route revision served against live presence")
	}

	// A later reconcile snapshot replaces the state wholesale: agent A is no
	// longer in the gateway's truth and must become unavailable immediately.
	reconcile := presenceSnapshot(presenceTestBoot, 9, []PresenceEvent{
		presenceOnline(presenceTestBoot, 9, agentB, 10002, 1, presenceViewBase.Add(45*time.Second)),
	})
	if err := view.ApplyPresenceSnapshot(reconcile); err != nil {
		t.Fatalf("apply reconcile snapshot: %v", err)
	}
	if view.lastRevision != 9 {
		t.Fatalf("reconcile snapshot revision not adopted: %d", view.lastRevision)
	}
	if view.Available(agentA, 10001, 3, 500, clock.Now()) {
		t.Fatalf("agent A still available after its lease left the snapshot")
	}
	if !view.Available(agentB, 10002, 1, 500, clock.Now()) {
		t.Fatalf("agent B lost by the reconcile snapshot")
	}
}

// TestControlDiscardsOlderBootAndRevision pins §7.3: control discards facts
// from an older/unknown boot and stale revisions, and rejects a revision gap
// without mutating anything (§11.3: neither side guesses across a gap).
func TestControlDiscardsOlderBootAndRevision(t *testing.T) {
	app := newPublisherTestApp(t)
	_, agentA := createPublisherAgent(t, app, "p15-a", "sb0a1b2c3d", 10001, 3)
	_, agentX := createPublisherAgent(t, app, "p15-x", "sb0f1e2d3c", 10003, 1)
	clock := newFakeClock(presenceViewBase)
	view := newPresenceTestView(t, app, fixedRoutes{revision: 5}, clock.Now)

	boot := presenceSnapshot(presenceTestBoot, 10, []PresenceEvent{
		presenceOnline(presenceTestBoot, 10, agentA, 10001, 3, presenceViewBase.Add(45*time.Second)),
	})
	if err := view.ApplyPresenceSnapshot(boot); err != nil {
		t.Fatalf("apply boot snapshot: %v", err)
	}

	// Events from an unknown/older boot are discarded: a batch is never
	// applied for a boot whose snapshot control has not adopted. No error —
	// the gateway's next snapshot post is the reconcile path.
	olderBoot := PresenceEnvelope{Version: ProtocolVersion, Events: []PresenceEvent{
		presenceOnline("gateway-boot-older", 11, agentX, 10003, 1, presenceViewBase.Add(45*time.Second)),
	}}
	if err := view.ApplyPresenceEvents(olderBoot); err != nil {
		t.Fatalf("older-boot batch must be discarded without error: %v", err)
	}
	if view.bootID != presenceTestBoot || view.lastRevision != 10 {
		t.Fatalf("older-boot batch mutated the view: boot %q revision %d", view.bootID, view.lastRevision)
	}
	if view.Available(agentX, 10003, 1, 5, clock.Now()) {
		t.Fatalf("older-boot fact created availability")
	}

	// Stale revisions on the current boot are skipped as replays.
	replay := PresenceEnvelope{Version: ProtocolVersion, Events: []PresenceEvent{
		presenceOffline(presenceTestBoot, 9, agentA, 10001, 3),
		presenceOffline(presenceTestBoot, 10, agentA, 10001, 3),
	}}
	if err := view.ApplyPresenceEvents(replay); err != nil {
		t.Fatalf("replay batch must be discarded without error: %v", err)
	}
	if view.lastRevision != 10 || !view.Available(agentA, 10001, 3, 5, clock.Now()) {
		t.Fatalf("replayed stale revisions mutated the view")
	}

	// A revision gap is rejected and mutates nothing: rev 11 followed by 13
	// skips 12, which control never guesses across.
	gap := PresenceEnvelope{Version: ProtocolVersion, Events: []PresenceEvent{
		presenceOnline(presenceTestBoot, 11, agentX, 10003, 1, presenceViewBase.Add(45*time.Second)),
		presenceOnline(presenceTestBoot, 13, agentX, 10003, 1, presenceViewBase.Add(45*time.Second)),
	}}
	if err := view.ApplyPresenceEvents(gap); !errors.Is(err, ErrBadRevision) {
		t.Fatalf("gap batch error = %v, want ErrBadRevision", err)
	}
	if view.lastRevision != 10 {
		t.Fatalf("gap batch advanced the revision to %d", view.lastRevision)
	}
	if view.Available(agentX, 10003, 1, 5, clock.Now()) {
		t.Fatalf("gap batch created availability")
	}
	if !view.Available(agentA, 10001, 3, 5, clock.Now()) {
		t.Fatalf("gap batch disturbed existing state")
	}

	// The contiguous continuation applies normally.
	cont := PresenceEnvelope{Version: ProtocolVersion, Events: []PresenceEvent{
		presenceOffline(presenceTestBoot, 11, agentA, 10001, 3),
	}}
	if err := view.ApplyPresenceEvents(cont); err != nil {
		t.Fatalf("apply contiguous batch: %v", err)
	}
	if view.lastRevision != 11 || view.Available(agentA, 10001, 3, 5, clock.Now()) {
		t.Fatalf("contiguous offline not applied")
	}
}

// TestPresenceLeaseExpiryMakesRelayUnavailable pins the lease freshness
// policy: the lease mirrors the gateway's 45-second presence lease, expires
// lazily at Available time, expired-at-receipt online facts grant nothing,
// and a future-dated lease is rejected without mutation (fail closed).
func TestPresenceLeaseExpiryMakesRelayUnavailable(t *testing.T) {
	app := newPublisherTestApp(t)
	_, agentA := createPublisherAgent(t, app, "p15-a", "sb0a1b2c3d", 10001, 3)
	clock := newFakeClock(presenceViewBase)
	view := newPresenceTestView(t, app, fixedRoutes{revision: 5}, clock.Now)

	snapshot := presenceSnapshot(presenceTestBoot, 1, []PresenceEvent{
		// The gateway's 45-second lease (§7.3) as the stated expiry.
		presenceOnline(presenceTestBoot, 1, agentA, 10001, 3, presenceViewBase.Add(45*time.Second)),
	})
	if err := view.ApplyPresenceSnapshot(snapshot); err != nil {
		t.Fatalf("apply snapshot: %v", err)
	}

	if !view.Available(agentA, 10001, 3, 5, presenceViewBase.Add(44*time.Second)) {
		t.Fatalf("lease must be available inside its validity")
	}
	if view.Available(agentA, 10001, 3, 5, presenceViewBase.Add(45*time.Second)) {
		t.Fatalf("lease must expire at (not after) its stated expiry")
	}

	// Renewal cannot come from replays: only a newer gateway fact returns
	// availability (the gateway re-confirms via its probe-confirmed path).
	reconfirm := PresenceEnvelope{Version: ProtocolVersion, Events: []PresenceEvent{
		presenceOnline(presenceTestBoot, 2, agentA, 10001, 3, presenceViewBase.Add(50*time.Second).Add(45*time.Second)),
	}}
	clock.Advance(50 * time.Second)
	if err := view.ApplyPresenceEvents(reconfirm); err != nil {
		t.Fatalf("apply re-confirm: %v", err)
	}
	if !view.Available(agentA, 10001, 3, 5, clock.Now()) {
		t.Fatalf("re-confirmed lease not available")
	}
	clock.Advance(46 * time.Second)
	if view.Available(agentA, 10001, 3, 5, clock.Now()) {
		t.Fatalf("re-confirmed lease did not expire")
	}

	// A future-dated lease (beyond the 60-second control bound) is rejected
	// and mutates nothing.
	future := PresenceEnvelope{Version: ProtocolVersion, Events: []PresenceEvent{
		presenceOnline(presenceTestBoot, 3, agentA, 10001, 3, clock.Now().Add(61*time.Second)),
	}}
	if err := view.ApplyPresenceEvents(future); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("future-dated lease error = %v, want ErrInvalidPayload", err)
	}
	if view.lastRevision != 2 {
		t.Fatalf("future-dated batch advanced the revision to %d", view.lastRevision)
	}
	if view.Available(agentA, 10001, 3, 5, clock.Now()) {
		t.Fatalf("future-dated lease created availability")
	}

	// An online fact already expired at receipt grants nothing (expired ⇒
	// unavailable) but still advances the stream position.
	expired := PresenceEnvelope{Version: ProtocolVersion, Events: []PresenceEvent{
		presenceOnline(presenceTestBoot, 3, agentA, 10001, 3, presenceViewBase.Add(45*time.Second)),
	}}
	if err := view.ApplyPresenceEvents(expired); err != nil {
		t.Fatalf("expired-at-receipt batch must apply as absent: %v", err)
	}
	if view.lastRevision != 3 {
		t.Fatalf("expired-at-receipt batch did not advance the revision: %d", view.lastRevision)
	}
	if view.Available(agentA, 10001, 3, 5, clock.Now()) {
		t.Fatalf("expired-at-receipt lease became available")
	}
}

// TestRelayLastSeenIsDiagnosticOnly pins §12: agents.relay_last_seen_at is
// persisted from gateway observations for diagnostics and is never an input
// to availability — route selection (the Available predicate) reads only the
// in-memory leased view.
func TestRelayLastSeenIsDiagnosticOnly(t *testing.T) {
	app := newPublisherTestApp(t)
	_, agentA := createPublisherAgent(t, app, "p15-a", "sb0a1b2c3d", 10001, 3)
	clock := newFakeClock(presenceViewBase)
	view := newPresenceTestView(t, app, fixedRoutes{revision: 5}, clock.Now)

	snapshot := presenceSnapshot(presenceTestBoot, 1, []PresenceEvent{
		presenceOnline(presenceTestBoot, 1, agentA, 10001, 3, presenceViewBase.Add(45*time.Second)),
	})
	if err := view.ApplyPresenceSnapshot(snapshot); err != nil {
		t.Fatalf("apply snapshot: %v", err)
	}

	// The observation was persisted for diagnostics.
	got := agentLastSeen(t, app, agentA)
	if got.IsZero() || got.Sub(presenceViewBase) > time.Minute {
		t.Fatalf("relay_last_seen_at not persisted from the gateway observation: %v", got)
	}

	// Direction 1: a fresh diagnostic row cannot keep an expired lease
	// available.
	clock.Advance(60 * time.Second)
	setAgentLastSeen(t, app, agentA, clock.Now()) // freshest possible diagnostic
	if view.Available(agentA, 10001, 3, 5, clock.Now()) {
		t.Fatalf("availability served from the relay_last_seen_at diagnostic")
	}

	// Direction 2: a live lease does not need the diagnostic — zeroing the
	// row changes nothing about selection.
	clockAdvanceTo(t, clock, presenceViewBase)
	rec, err := app.FindRecordById("agents", agentA)
	if err != nil {
		t.Fatalf("load agent: %v", err)
	}
	rec.Set("relay_last_seen_at", time.Time{})
	if err := app.Save(rec); err != nil {
		t.Fatalf("zero last seen: %v", err)
	}
	if !agentLastSeen(t, app, agentA).IsZero() {
		t.Fatalf("diagnostic row was not zeroed")
	}
	if !view.Available(agentA, 10001, 3, 5, clock.Now()) {
		t.Fatalf("live lease lost by zeroing the diagnostic row")
	}
}

// TestAgentTelemetryCannotSetAvailability pins §7.4/§4.2: the agent's
// relay_client_state is telemetry, and nothing agent-originated can create
// availability. The view's only writers are the two PresenceSink methods fed
// by the Task 11 mTLS sync server; control's persisted agent-side diagnostic
// (relay_last_seen_at) is not an availability input, and after lease expiry
// only a fresh gateway fact can restore availability.
func TestAgentTelemetryCannotSetAvailability(t *testing.T) {
	app := newPublisherTestApp(t)
	_, agentA := createPublisherAgent(t, app, "p15-a", "sb0a1b2c3d", 10001, 3)
	clock := newFakeClock(presenceViewBase)
	view := newPresenceTestView(t, app, fixedRoutes{revision: 5}, clock.Now)

	// Agent-side optimism: the freshest possible persisted telemetry, with
	// NO gateway lease, must never read as available.
	setAgentLastSeen(t, app, agentA, clock.Now())
	if view.Available(agentA, 10001, 3, 5, clock.Now()) {
		t.Fatalf("availability exists without any gateway lease")
	}

	// A gateway lease is the only thing that makes the relay available.
	snapshot := presenceSnapshot(presenceTestBoot, 1, []PresenceEvent{
		presenceOnline(presenceTestBoot, 1, agentA, 10001, 3, presenceViewBase.Add(45*time.Second)),
	})
	if err := view.ApplyPresenceSnapshot(snapshot); err != nil {
		t.Fatalf("apply snapshot: %v", err)
	}
	if !view.Available(agentA, 10001, 3, 5, clock.Now()) {
		t.Fatalf("gateway lease not available")
	}

	// After expiry, refreshed agent-side telemetry still cannot restore
	// availability: only a fresh gateway fact can.
	clock.Advance(60 * time.Second)
	setAgentLastSeen(t, app, agentA, clock.Now())
	if view.Available(agentA, 10001, 3, 5, clock.Now()) {
		t.Fatalf("expired lease revived by refreshed telemetry")
	}
	reconfirm := PresenceEnvelope{Version: ProtocolVersion, Events: []PresenceEvent{
		presenceOnline(presenceTestBoot, 2, agentA, 10001, 3, clock.Now().Add(45*time.Second)),
	}}
	if err := view.ApplyPresenceEvents(reconfirm); err != nil {
		t.Fatalf("apply re-confirm: %v", err)
	}
	if !view.Available(agentA, 10001, 3, 5, clock.Now()) {
		t.Fatalf("fresh gateway fact did not restore availability")
	}
}

// ackingRoutes is a RouteRevisionSource that also satisfies RouteSyncStatus,
// modeling the Task 12 publisher's gateway-acknowledgement watermark and
// sync health (R2 ruling 4, Sol Important-5).
type ackingRoutes struct {
	revision uint64
	acked    uint64
	healthy  bool
}

func (s *ackingRoutes) CurrentRevision() uint64      { return s.revision }
func (s *ackingRoutes) AcknowledgedRevision() uint64 { return s.acked }
func (s *ackingRoutes) Healthy() bool                { return s.healthy }

// TestAvailableProvesGatewayAcknowledgedRevisionWhileSyncHealthy pins R2
// ruling 4: when the route source exposes the gateway acknowledgement
// watermark and sync health, the Available join is proved gateway-side — a
// revision control published but the gateway has not acked is never
// selectable, and sync being down (a control restart before the gateway
// re-converges and re-acks) fails every relay term closed.
func TestAvailableProvesGatewayAcknowledgedRevisionWhileSyncHealthy(t *testing.T) {
	app := newPublisherTestApp(t)
	_, agentA := createPublisherAgent(t, app, "ack5-a", "sb0a1b2c3d", 10001, 3)
	clock := newFakeClock(presenceViewBase)

	source := &ackingRoutes{revision: 42, acked: 41, healthy: true}
	view := newPresenceTestView(t, app, source, clock.Now)
	env := presenceSnapshot(presenceTestBoot, 1, []PresenceEvent{
		presenceOnline(presenceTestBoot, 1, agentA, 10001, 3, presenceViewBase.Add(45*time.Second)),
	})
	if err := view.ApplyPresenceSnapshot(env); err != nil {
		t.Fatalf("apply presence snapshot: %v", err)
	}

	// The gateway acked 41; control has since published 42. Only the acked
	// revision is provably held by the gateway.
	if !view.Available(agentA, 10001, 3, 41, presenceViewBase) {
		t.Fatalf("Available at the gateway-acked revision 41 = false, want true")
	}
	if view.Available(agentA, 10001, 3, 42, presenceViewBase) {
		t.Fatalf("Available at the published-but-unacked revision 42 = true, want false")
	}

	// Sync unhealthy (R2 belt-and-braces): relay unavailable even at the
	// acked revision — a control restart must fail relay selection closed
	// until the gateway re-converges and re-acks.
	source.healthy = false
	if view.Available(agentA, 10001, 3, 41, presenceViewBase) {
		t.Fatalf("Available while sync is unhealthy = true, want false")
	}
	source.healthy = true

	// No acknowledgement recorded at all: nothing is provably held.
	source.acked = 0
	if view.Available(agentA, 10001, 3, 41, presenceViewBase) {
		t.Fatalf("Available with an empty ack watermark = true, want false")
	}

	// Legacy stub sources (RouteRevisionSource only) keep the historical
	// publisher-watermark join, so existing test seams are unaffected.
	legacy := newPresenceTestView(t, app, fixedRoutes{revision: 42}, clock.Now)
	if err := legacy.ApplyPresenceSnapshot(env); err != nil {
		t.Fatalf("apply presence snapshot (legacy source): %v", err)
	}
	if !legacy.Available(agentA, 10001, 3, 42, presenceViewBase) {
		t.Fatalf("legacy watermark join broken: Available at the current revision = false, want true")
	}
	if legacy.Available(agentA, 10001, 3, 43, presenceViewBase) {
		t.Fatalf("legacy watermark join broken: Available past the current revision = true, want false")
	}
}

// TestContradictoryPresenceEventsFailClosed pins §15.7's contradictory-state
// discipline on the control view: presence facts that contradict the live
// lease are discarded fail-closed and never regress or extend availability.
// Specifically — a same-generation online for a DIFFERENT relay port (the
// gateway never does this; port reassignment bumps the generation), an online
// for a SUPERSEDED lower generation, and an offline for a superseded
// generation are all ignored, while a newer-generation offline does fence the
// older lease.
func TestContradictoryPresenceEventsFailClosed(t *testing.T) {
	app := newPublisherTestApp(t)
	_, agentA := createPublisherAgent(t, app, "p15-contradiction", "sb0a1b2c3d", 10001, 3)
	clock := newFakeClock(presenceViewBase)
	view := newPresenceTestView(t, app, fixedRoutes{revision: 5}, clock.Now)

	boot := presenceSnapshot(presenceTestBoot, 1, []PresenceEvent{
		presenceOnline(presenceTestBoot, 1, agentA, 10001, 3, presenceViewBase.Add(45*time.Second)),
	})
	if err := view.ApplyPresenceSnapshot(boot); err != nil {
		t.Fatalf("apply boot snapshot: %v", err)
	}
	if !view.Available(agentA, 10001, 3, 5, clock.Now()) {
		t.Fatal("baseline lease not available")
	}

	// Revision 2: same generation, different port. Contradictory state is
	// discarded; the live lease and its port are untouched.
	contradiction := PresenceEnvelope{Version: ProtocolVersion, Events: []PresenceEvent{
		presenceOnline(presenceTestBoot, 2, agentA, 10002, 3, presenceViewBase.Add(45*time.Second)),
	}}
	if err := view.ApplyPresenceEvents(contradiction); err != nil {
		t.Fatalf("apply same-generation port contradiction: %v", err)
	}
	if view.lastRevision != 2 {
		t.Fatalf("contradiction did not advance the stream position: %d", view.lastRevision)
	}
	if !view.Available(agentA, 10001, 3, 5, clock.Now()) {
		t.Fatal("same-generation port contradiction discarded the live lease")
	}
	if view.Available(agentA, 10002, 3, 5, clock.Now()) {
		t.Fatal("contradictory port became available")
	}

	// Revision 3: an online for a superseded lower generation is discarded.
	superseded := PresenceEnvelope{Version: ProtocolVersion, Events: []PresenceEvent{
		presenceOnline(presenceTestBoot, 3, agentA, 10001, 2, presenceViewBase.Add(45*time.Second)),
	}}
	if err := view.ApplyPresenceEvents(superseded); err != nil {
		t.Fatalf("apply superseded online: %v", err)
	}
	if !view.Available(agentA, 10001, 3, 5, clock.Now()) {
		t.Fatal("superseded online disturbed the live lease")
	}
	if view.Available(agentA, 10001, 2, 5, clock.Now()) {
		t.Fatal("superseded generation became available")
	}

	// Revision 4: an offline for a superseded generation must not clear the
	// newer live lease.
	staleOffline := PresenceEnvelope{Version: ProtocolVersion, Events: []PresenceEvent{
		presenceOffline(presenceTestBoot, 4, agentA, 10001, 2),
	}}
	if err := view.ApplyPresenceEvents(staleOffline); err != nil {
		t.Fatalf("apply stale offline: %v", err)
	}
	if !view.Available(agentA, 10001, 3, 5, clock.Now()) {
		t.Fatal("stale-generation offline cleared the live lease")
	}

	// Revision 5: a NEWER-generation offline fences the older lease.
	fence := PresenceEnvelope{Version: ProtocolVersion, Events: []PresenceEvent{
		presenceOffline(presenceTestBoot, 5, agentA, 10001, 4),
	}}
	if err := view.ApplyPresenceEvents(fence); err != nil {
		t.Fatalf("apply newer-generation offline: %v", err)
	}
	if view.Available(agentA, 10001, 3, 5, clock.Now()) {
		t.Fatal("newer-generation offline did not fence the older lease")
	}
}

// --- task #16: empty-snapshot boot adoption and renewal republish ---

// emptyPresenceSnapshot builds the top-level empty boot snapshot the gateway
// posts at startup: no events, but a boot identity and revision.
func emptyPresenceSnapshot(boot string, revision uint64) PresenceEnvelope {
	return PresenceEnvelope{Version: ProtocolVersion, GatewayBootID: boot, Revision: revision, Events: []PresenceEvent{}}
}

// TestEmptyBootSnapshotAdoptsFreshGatewayBoot pins the task #16 fix: an empty
// snapshot carrying the top-level boot identity is adoptable, so the restarted
// gateway's first real event batch applies. Before the fix the empty snapshot
// left the previous boot recorded and the new boot's events were discarded as
// a superseded boot's replay.
func TestEmptyBootSnapshotAdoptsFreshGatewayBoot(t *testing.T) {
	app := newPublisherTestApp(t)
	_, agentA := createPublisherAgent(t, app, "t16-a", "sb0a1b2c3d", 10001, 3)
	clock := newFakeClock(presenceViewBase)
	view := newPresenceTestView(t, app, fixedRoutes{revision: 5}, clock.Now)

	// Boot A announces itself by snapshot with one lease.
	bootA := presenceSnapshot(presenceTestBoot, 10, []PresenceEvent{
		presenceOnline(presenceTestBoot, 10, agentA, 10001, 3, clock.Now().Add(45*time.Second)),
	})
	if err := view.ApplyPresenceSnapshot(bootA); err != nil {
		t.Fatalf("apply boot A snapshot: %v", err)
	}
	if !view.Available(agentA, 10001, 3, 5, clock.Now()) {
		t.Fatal("boot A lease not established")
	}

	// Gateway restarts (boot B): its presence is empty until FRP clients
	// reconnect, and the empty snapshot names the new boot.
	const bootB = "gateway-boot-ctrl16"
	if err := view.ApplyPresenceSnapshot(emptyPresenceSnapshot(bootB, 0)); err != nil {
		t.Fatalf("apply empty boot B snapshot: %v", err)
	}
	if view.Available(agentA, 10001, 3, 5, clock.Now()) {
		t.Fatal("empty boot snapshot did not clear the previous boot's lease")
	}
	if view.bootID != bootB || !view.haveBoot || view.lastRevision != 0 {
		t.Fatalf("empty boot snapshot was not adopted: boot %q haveBoot %v revision %d", view.bootID, view.haveBoot, view.lastRevision)
	}

	// Boot B's first confirmed tunnel emits an ordered event batch; it must
	// apply because control recorded the boot.
	first := PresenceEnvelope{Version: ProtocolVersion, Events: []PresenceEvent{
		presenceOnline(bootB, 1, agentA, 10001, 3, clock.Now().Add(45*time.Second)),
	}}
	if err := view.ApplyPresenceEvents(first); err != nil {
		t.Fatalf("apply boot B first event: %v", err)
	}
	if !view.Available(agentA, 10001, 3, 5, clock.Now()) {
		t.Fatal("fresh boot's first event was not adopted")
	}
}

// TestLegacyEmptySnapshotStillClearsWithoutDisturbingBootTracking keeps the
// pre-fix empty snapshot (no top-level boot) wire-compatible: it clears every
// lease and leaves boot/revision tracking untouched.
func TestLegacyEmptySnapshotStillClearsWithoutDisturbingBootTracking(t *testing.T) {
	app := newPublisherTestApp(t)
	_, agentA := createPublisherAgent(t, app, "t16-legacy", "sb0a1b2c3d", 10001, 3)
	clock := newFakeClock(presenceViewBase)
	view := newPresenceTestView(t, app, fixedRoutes{revision: 5}, clock.Now)

	boot := presenceSnapshot(presenceTestBoot, 10, []PresenceEvent{
		presenceOnline(presenceTestBoot, 10, agentA, 10001, 3, clock.Now().Add(45*time.Second)),
	})
	if err := view.ApplyPresenceSnapshot(boot); err != nil {
		t.Fatalf("apply boot snapshot: %v", err)
	}
	if err := view.ApplyPresenceSnapshot(PresenceEnvelope{Version: ProtocolVersion, Events: []PresenceEvent{}}); err != nil {
		t.Fatalf("apply legacy empty snapshot: %v", err)
	}
	if view.Available(agentA, 10001, 3, 5, clock.Now()) {
		t.Fatal("legacy empty snapshot did not clear the lease")
	}
	if view.bootID != presenceTestBoot || view.lastRevision != 10 {
		t.Fatalf("legacy empty snapshot disturbed boot/revision tracking: boot %q revision %d", view.bootID, view.lastRevision)
	}
}

// TestEmptyBootSnapshotValidation proves the top-level boot identity is
// validated and must agree with the events when both are present.
func TestEmptyBootSnapshotValidation(t *testing.T) {
	valid := emptyPresenceSnapshot("gateway-boot-x", 3)
	if err := ValidatePresenceSnapshotEnvelope(valid, MaxPresenceEventsPerEnvelope); err != nil {
		t.Fatalf("valid empty boot snapshot rejected: %v", err)
	}
	// Identifiers carry no control/whitespace bytes.
	badBoot := emptyPresenceSnapshot("gateway boot", 3)
	if err := ValidatePresenceSnapshotEnvelope(badBoot, MaxPresenceEventsPerEnvelope); err == nil {
		t.Fatal("empty boot snapshot with an invalid boot identity was accepted")
	}
	// A top-level boot that disagrees with the events is a producer bug.
	mismatch := PresenceEnvelope{Version: ProtocolVersion, GatewayBootID: "boot-one", Revision: 4, Events: []PresenceEvent{
		presenceOnline("boot-two", 4, "agent", 10001, 3, presenceViewBase.Add(45*time.Second)),
	}}
	if err := ValidatePresenceSnapshotEnvelope(mismatch, MaxPresenceEventsPerEnvelope); err == nil {
		t.Fatal("snapshot with mismatched top-level and event boot IDs was accepted")
	}
	// A top-level revision that disagrees with the event revision is refused.
	revisionMismatch := PresenceEnvelope{Version: ProtocolVersion, GatewayBootID: "boot-one", Revision: 9, Events: []PresenceEvent{
		presenceOnline("boot-one", 4, "agent", 10001, 3, presenceViewBase.Add(45*time.Second)),
	}}
	if err := ValidatePresenceSnapshotEnvelope(revisionMismatch, MaxPresenceEventsPerEnvelope); err == nil {
		t.Fatal("snapshot with mismatched top-level and event revisions was accepted")
	}
}

// TestRenewedSnapshotRestoresAvailabilityAfterTheOriginalLeaseExpires mirrors
// the periodic renew republish on the control side: the original lease lapses
// (fail closed), and a republished snapshot carrying the renewed expiry
// restores availability.
func TestRenewedSnapshotRestoresAvailabilityAfterTheOriginalLeaseExpires(t *testing.T) {
	app := newPublisherTestApp(t)
	_, agentA := createPublisherAgent(t, app, "t16-renew", "sb0a1b2c3d", 10001, 3)
	clock := newFakeClock(presenceViewBase)
	view := newPresenceTestView(t, app, fixedRoutes{revision: 5}, clock.Now)

	originalLease := clock.Now().Add(45 * time.Second)
	if err := view.ApplyPresenceSnapshot(presenceSnapshot(presenceTestBoot, 1, []PresenceEvent{
		presenceOnline(presenceTestBoot, 1, agentA, 10001, 3, originalLease),
	})); err != nil {
		t.Fatalf("apply original snapshot: %v", err)
	}
	if !view.Available(agentA, 10001, 3, 5, clock.Now()) {
		t.Fatal("original lease not available")
	}

	clock.Advance(46 * time.Second)
	if view.Available(agentA, 10001, 3, 5, clock.Now()) {
		t.Fatal("lease still available after the original expiry")
	}

	// The gateway's periodic republish carries the renewed lease at a new
	// revision; the wholesale replacement restores the lease.
	renewedLease := clock.Now().Add(45 * time.Second)
	if err := view.ApplyPresenceSnapshot(presenceSnapshot(presenceTestBoot, 2, []PresenceEvent{
		presenceOnline(presenceTestBoot, 2, agentA, 10001, 3, renewedLease),
	})); err != nil {
		t.Fatalf("apply renewed snapshot: %v", err)
	}
	if !view.Available(agentA, 10001, 3, 5, clock.Now()) {
		t.Fatal("renewed republish did not restore availability")
	}
}
