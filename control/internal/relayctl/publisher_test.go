package relayctl

// Tests for the Task 12 route publisher (plan Task 12, spec §§6, 8, 11.3,
// 15.7): the snapshot is built ONLY from active supported public Immich
// PocketBase rows joined with the persisted relay assignment, deltas are
// ordered and retried until the gateway explicitly acknowledges them, the
// retained delta history is bounded, and limit values only ever tighten
// within a lease epoch. All origin/agent/session fields derive from
// PocketBase rows; nothing arrives from agent or HTTP parameters.
//
// "Supported" here follows Phase 4a §13.3: public unprotected Immich shares
// INCLUDING relay_only ones — relay_only exclusion is selection-side policy
// (§6.1, §9.1), and §12 makes sessions.relay_only routable, so its exact
// relay route must be published like any other supported share's.

import (
	"reflect"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"sharebridge/control/migrations"
)

const (
	publisherTestBootID = "gateway-boot-test-1"
	// Fixed revision seed so delta revisions are deterministic assertions.
	publisherTestSeed = uint64(1000)
)

// publisherTestBase is the fixed fake-clock base: 2023-11-15T12:00:00Z.
var publisherTestBase = time.Unix(1700050000, 0).UTC()

// newPublisherTestApp boots a hermetic PocketBase app with the full session +
// agents schema, including migration 9's relay assignment fields, so the
// publisher exercises the real persisted rows.
func newPublisherTestApp(t *testing.T) core.App {
	t.Helper()

	app := core.NewBaseApp(core.BaseAppConfig{
		DataDir:       t.TempDir(),
		EncryptionEnv: "pb_publisher_test_env",
	})
	if err := app.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	for _, fn := range []func(core.App) error{
		migrations.CreateCollections,
		migrations.AddRelayOnly,
		migrations.AddSessionRelayStaticPub,
		migrations.AddImmichSessionFields,
		migrations.CreateAgents,
		migrations.AddSessionsInactiveReason,
		migrations.AddAgentsRelaySTUN,
	} {
		if err := fn(app); err != nil {
			t.Fatalf("apply migration: %v", err)
		}
	}
	return app
}

// createPublisherAgent inserts a users + api_keys + agents chain and returns
// (apiKeyID, agentRecordID). relayPort 0 models an unassigned agent.
func createPublisherAgent(t *testing.T, app core.App, label, namespace string, relayPort, relayGeneration int) (string, string) {
	t.Helper()

	usersCol, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatalf("users collection: %v", err)
	}
	user := core.NewRecord(usersCol)
	user.SetEmail(label + "@publisher-test.example.com")
	user.SetPassword("publisher-test-password")
	user.Set("relay_quota_gb", 50.0)
	if err := app.Save(user); err != nil {
		t.Fatalf("save user: %v", err)
	}

	keysCol, err := app.FindCollectionByNameOrId("api_keys")
	if err != nil {
		t.Fatalf("api_keys collection: %v", err)
	}
	key := core.NewRecord(keysCol)
	key.Set("account_id", user.Id)
	key.Set("is_active", true)
	key.Set("label", "publisher test key")
	if err := app.Save(key); err != nil {
		t.Fatalf("save api key: %v", err)
	}

	agentsCol, err := app.FindCollectionByNameOrId("agents")
	if err != nil {
		t.Fatalf("agents collection: %v", err)
	}
	agent := core.NewRecord(agentsCol)
	agent.Set("api_key_id", key.Id)
	agent.Set("namespace", namespace)
	agent.Set("cert_status", "ready")
	agent.Set("endpoint_port", 0)
	agent.Set("relay_port", relayPort)
	agent.Set("relay_generation", relayGeneration)
	if err := app.Save(agent); err != nil {
		t.Fatalf("save agent: %v", err)
	}
	return key.Id, agent.Id
}

// createPublisherSession inserts a session row and returns its record ID.
// Defaults model a supported active public Immich share; mutate adjusts
// individual fields for exclusion cases.
func createPublisherSession(t *testing.T, app core.App, apiKeyID, code, origin string, mutate func(*core.Record)) string {
	t.Helper()

	sessionsCol, err := app.FindCollectionByNameOrId("sessions")
	if err != nil {
		t.Fatalf("sessions collection: %v", err)
	}
	record := core.NewRecord(sessionsCol)
	record.Set("code", code)
	record.Set("api_key_id", apiKeyID)
	record.Set("agent_id", "agent-"+code)
	record.Set("origin", origin)
	record.Set("is_active", true)
	record.Set("share_type", "immich")
	record.Set("is_password_protected", false)
	record.Set("relay_only", false)
	if mutate != nil {
		mutate(record)
	}
	if err := app.Save(record); err != nil {
		t.Fatalf("save session %s: %v", code, err)
	}
	return record.Id
}

// newTestPublisher builds a publisher with deterministic limits, revision
// seed, and (unless the test overrides it) a fixed clock.
func newTestPublisher(t *testing.T, app core.App, config PublisherConfig) *Publisher {
	t.Helper()
	if config.Limits == (Limits{}) {
		config.Limits = Limits{MaxStreamsPerOrigin: 32, MaxStreamsPerAgent: 64, MaxStreamsGlobal: 8192}
	}
	if config.Now == nil {
		config.Now = func() time.Time { return publisherTestBase }
	}
	publisher, err := NewPublisher(app, config)
	if err != nil {
		t.Fatalf("new publisher: %v", err)
	}
	return publisher
}

// TestPublisherSnapshotContainsOnlyActiveSupportedExactRelayRoutes pins the
// §8 snapshot source of truth: only active, supported (public unprotected
// Immich, including relay_only — §12/§13.3), unexpired sessions whose agent
// carries a live relay assignment become exact routes. Every route field
// derives from the PocketBase rows: the relay origin from the persisted
// direct origin, the agent/port/generation from the agents row. Missing
// origin or missing assignment means the route is absent — never partially
// routable (§15.7).
func TestPublisherSnapshotContainsOnlyActiveSupportedExactRelayRoutes(t *testing.T) {
	app := newPublisherTestApp(t)
	apiKey1, agent1 := createPublisherAgent(t, app, "assigned", "sb1a2b3c4d", 11007, 3)
	apiKey2, _ := createPublisherAgent(t, app, "unassigned", "sb99999999", 0, 0)

	included := createPublisherSession(t, app, apiKey1, "snapshot1", "album12.sb1a2b3c4d.example.com", nil)
	createPublisherSession(t, app, apiKey1, "inactiver", "inactive.sb1a2b3c4d.example.com", func(rec *core.Record) {
		rec.Set("is_active", false)
		rec.Set("inactive_reason", "revoked")
	})
	createPublisherSession(t, app, apiKey1, "password", "protected.sb1a2b3c4d.example.com", func(rec *core.Record) {
		rec.Set("is_password_protected", true)
	})
	relayOnlySession := createPublisherSession(t, app, apiKey1, "relayonly", "relayonly.sb1a2b3c4d.example.com", func(rec *core.Record) {
		rec.Set("relay_only", true)
	})
	createPublisherSession(t, app, apiKey1, "othertype", "webdav.sb1a2b3c4d.example.com", func(rec *core.Record) {
		rec.Set("share_type", "webdav")
	})
	createPublisherSession(t, app, apiKey1, "noorigin", "", nil)
	createPublisherSession(t, app, apiKey2, "noassign", "orphan.sb99999999.example.com", nil)
	createPublisherSession(t, app, apiKey1, "expiredrow", "expired.sb1a2b3c4d.example.com", func(rec *core.Record) {
		rec.Set("expires_at", publisherTestBase.Add(-time.Hour))
	})

	publisher := newTestPublisher(t, app, PublisherConfig{RevisionSeed: publisherTestSeed})

	snapshot, err := publisher.RouteSnapshot()
	if err != nil {
		t.Fatalf("route snapshot: %v", err)
	}
	if err := ValidateSnapshot(snapshot, MaxRoutesPerSnapshot); err != nil {
		t.Fatalf("snapshot fails protocol validation: %v", err)
	}
	if snapshot.Version != ProtocolVersion {
		t.Fatalf("snapshot version = %d, want %d", snapshot.Version, ProtocolVersion)
	}
	if len(snapshot.Routes) != 2 {
		t.Fatalf("snapshot carries %d routes, want exactly 2 (the direct-eligible row plus the relay_only row): %+v", len(snapshot.Routes), snapshot.Routes)
	}
	// Hostname-sorted: album12... precedes relayonly....
	wantRoutes := []Route{
		{
			Hostname:      "album12.relay.sb1a2b3c4d.example.com",
			AgentRecordID: agent1,
			RelayPort:     11007,
			Generation:    3,
			SessionID:     included,
			Active:        true,
			Limits:        Limits{MaxStreamsPerOrigin: 32, MaxStreamsPerAgent: 64, MaxStreamsGlobal: 8192},
		},
		{
			// relay_only shares stay routable over relay (§12, §13.3): the
			// exclusion lives in route selection (§6.1, §9.1), never in the
			// published route set.
			Hostname:      "relayonly.relay.sb1a2b3c4d.example.com",
			AgentRecordID: agent1,
			RelayPort:     11007,
			Generation:    3,
			SessionID:     relayOnlySession,
			Active:        true,
			Limits:        Limits{MaxStreamsPerOrigin: 32, MaxStreamsPerAgent: 64, MaxStreamsGlobal: 8192},
		},
	}
	for index, wantRoute := range wantRoutes {
		route := snapshot.Routes[index]
		if route.Hostname != wantRoute.Hostname ||
			route.AgentRecordID != wantRoute.AgentRecordID ||
			route.RelayPort != wantRoute.RelayPort ||
			route.Generation != wantRoute.Generation ||
			route.SessionID != wantRoute.SessionID ||
			route.Active != wantRoute.Active ||
			route.Limits != wantRoute.Limits {
			t.Fatalf("route %d mismatch:\n got %+v\nwant %+v", index, route, wantRoute)
		}
		if route.Revision > snapshot.Revision {
			t.Fatalf("route revision %d exceeds snapshot revision %d", route.Revision, snapshot.Revision)
		}
	}

	// Deterministic rebuild: the same DB state yields the identical snapshot.
	second, err := publisher.RouteSnapshot()
	if err != nil {
		t.Fatalf("second route snapshot: %v", err)
	}
	if !reflect.DeepEqual(snapshot, second) {
		t.Fatalf("snapshot is not deterministic:\n first %+v\n second %+v", snapshot, second)
	}
}

// TestPublisherOrdersAddRevokeLimitDeltas pins the §11.3 delta discipline:
// add after registration, limit touches at new revisions for the 30-second
// lease refresh, revoke with an inactive zero-limit route — all strictly
// revision-ordered — and the review-mandated tighten-only rule: a route's
// limits may never widen within a lease epoch; widening requires a fresh
// registration (revoke + add).
func TestPublisherOrdersAddRevokeLimitDeltas(t *testing.T) {
	app := newPublisherTestApp(t)
	apiKey, agent := createPublisherAgent(t, app, "delta", "sb5t4g3h2", 10042, 7)
	sessionID := createPublisherSession(t, app, apiKey, "deltashare", "docs.sb5t4g3h2.example.com", nil)
	hostname := "docs.relay.sb5t4g3h2.example.com"

	publisher := newTestPublisher(t, app, PublisherConfig{RevisionSeed: publisherTestSeed})
	identity := routeIdentity{
		sessionID:     sessionID,
		hostname:      hostname,
		agentRecordID: agent,
		relayPort:     10042,
		generation:    7,
	}

	// Add after successful registration.
	if err := publisher.PublishAdd(sessionID); err != nil {
		t.Fatalf("publish add: %v", err)
	}
	if _, ok := publisher.entries[hostname]; !ok {
		t.Fatalf("route %q not tracked as active after add", hostname)
	}
	page, err := publisher.RouteDeltas(publisherTestSeed)
	if err != nil {
		t.Fatalf("deltas since seed: %v", err)
	}
	if err := ValidateDeltaPage(page, MaxDeltasPerPage); err != nil {
		t.Fatalf("add page fails protocol validation: %v", err)
	}
	if len(page.Deltas) != 1 || page.Deltas[0].Operation != RouteOperationAdd {
		t.Fatalf("expected exactly one route_add delta, got %+v", page.Deltas)
	}
	addDelta := page.Deltas[0]
	if addDelta.Revision != publisherTestSeed+1 || addDelta.Route.Revision != addDelta.Revision {
		t.Fatalf("add delta revisions = %d/%d, want %d", addDelta.Revision, addDelta.Route.Revision, publisherTestSeed+1)
	}
	if !addDelta.Route.Active || addDelta.Route.SessionID != sessionID || addDelta.Route.AgentRecordID != agent ||
		addDelta.Route.RelayPort != 10042 || addDelta.Route.Generation != 7 {
		t.Fatalf("add delta route fields wrong: %+v", addDelta.Route)
	}

	// Healthy sync (explicit ack) enables the lease-refresh limit touch.
	publisher.Acknowledge(StatusAck{Version: ProtocolVersion, GatewayBootID: publisherTestBootID, ControlEpoch: publisher.Epoch(), LastAppliedRevision: addDelta.Revision})
	if !publisher.Healthy() {
		t.Fatalf("publisher not healthy after explicit ack of %d", addDelta.Revision)
	}
	touched, err := publisher.RefreshLeases()
	if err != nil {
		t.Fatalf("refresh leases: %v", err)
	}
	if touched != 1 {
		t.Fatalf("refresh touched %d routes, want 1", touched)
	}
	limitPage, err := publisher.RouteDeltas(addDelta.Revision)
	if err != nil {
		t.Fatalf("deltas after refresh: %v", err)
	}
	if len(limitPage.Deltas) != 1 || limitPage.Deltas[0].Operation != RouteOperationLimit {
		t.Fatalf("expected exactly one route_limit delta, got %+v", limitPage.Deltas)
	}
	limitDelta := limitPage.Deltas[0]
	if !limitDelta.Route.Active {
		t.Fatalf("limit delta carries an inactive route: %+v", limitDelta.Route)
	}
	// The refresh must touch with the SAME limits — never widen (§14 review
	// mandate: within a lease epoch limits only tighten).
	if limitDelta.Route.Limits != (Limits{MaxStreamsPerOrigin: 32, MaxStreamsPerAgent: 64, MaxStreamsGlobal: 8192}) {
		t.Fatalf("limit delta widened limits: %+v", limitDelta.Route.Limits)
	}

	// Tighten-only: a wider limit set is refused outright and emits nothing.
	wider := Limits{MaxStreamsPerOrigin: 64, MaxStreamsPerAgent: 128, MaxStreamsGlobal: 8192}
	if err := publisher.publishLimit(identity, wider); err == nil {
		t.Fatalf("widening limits within a lease epoch was accepted")
	}
	afterRefusal, err := publisher.RouteDeltas(limitDelta.Revision)
	if err != nil {
		t.Fatalf("deltas after refused widen: %v", err)
	}
	if len(afterRefusal.Deltas) != 0 || afterRefusal.LatestRevision != limitDelta.Revision {
		t.Fatalf("refused widen emitted a delta: %+v", afterRefusal)
	}
	if tracked := publisher.entries[hostname]; tracked.route.Limits != (Limits{MaxStreamsPerOrigin: 32, MaxStreamsPerAgent: 64, MaxStreamsGlobal: 8192}) {
		t.Fatalf("tracked limits changed after refused widen: %+v", tracked.route.Limits)
	}

	// A genuine tighten is allowed and published.
	tighter := Limits{MaxStreamsPerOrigin: 16, MaxStreamsPerAgent: 32, MaxStreamsGlobal: 4096}
	if err := publisher.publishLimit(identity, tighter); err != nil {
		t.Fatalf("legitimate tighten refused: %v", err)
	}
	tightenPage, err := publisher.RouteDeltas(limitDelta.Revision)
	if err != nil {
		t.Fatalf("deltas after tighten: %v", err)
	}
	if len(tightenPage.Deltas) != 1 || tightenPage.Deltas[0].Route.Limits != tighter {
		t.Fatalf("tighten delta wrong: %+v", tightenPage.Deltas)
	}
	tightenRevision := tightenPage.Deltas[0].Revision

	// Revoke before local lifecycle removal: inactive route, zeroed limits.
	if err := publisher.PublishRevoke(sessionID); err != nil {
		t.Fatalf("publish revoke: %v", err)
	}
	if _, ok := publisher.entries[hostname]; ok {
		t.Fatalf("route %q still tracked after revoke", hostname)
	}
	full, err := publisher.RouteDeltas(publisherTestSeed)
	if err != nil {
		t.Fatalf("full delta history: %v", err)
	}
	if err := ValidateDeltaPage(full, MaxDeltasPerPage); err != nil {
		t.Fatalf("full history fails protocol validation: %v", err)
	}
	if len(full.Deltas) != 4 {
		t.Fatalf("expected add, limit, tighten, revoke = 4 deltas, got %d: %+v", len(full.Deltas), full.Deltas)
	}
	wantOps := []string{RouteOperationAdd, RouteOperationLimit, RouteOperationLimit, RouteOperationRevoke}
	for index, delta := range full.Deltas {
		if delta.Operation != wantOps[index] {
			t.Fatalf("delta %d operation = %q, want %q", index, delta.Operation, wantOps[index])
		}
		if index > 0 && delta.Revision <= full.Deltas[index-1].Revision {
			t.Fatalf("delta %d revision %d does not supersede %d", index, delta.Revision, full.Deltas[index-1].Revision)
		}
	}
	revokeDelta := full.Deltas[3]
	if revokeDelta.Route.Active || revokeDelta.Route.Limits != (Limits{}) {
		t.Fatalf("revoke must carry an inactive route with zeroed limits: %+v", revokeDelta.Route)
	}

	// A fresh registration (new lease epoch) may attach limits anew.
	if err := publisher.PublishAdd(sessionID); err != nil {
		t.Fatalf("re-publish add: %v", err)
	}
	readd, err := publisher.RouteDeltas(revokeDelta.Revision)
	if err != nil {
		t.Fatalf("deltas after re-add: %v", err)
	}
	if len(readd.Deltas) != 1 || readd.Deltas[0].Operation != RouteOperationAdd {
		t.Fatalf("expected one route_add after re-registration, got %+v", readd.Deltas)
	}
	if readd.Deltas[0].Route.Limits != (Limits{MaxStreamsPerOrigin: 32, MaxStreamsPerAgent: 64, MaxStreamsGlobal: 8192}) {
		t.Fatalf("fresh registration did not attach configured limits: %+v", readd.Deltas[0].Route.Limits)
	}
	_ = tightenRevision
}

// TestPublisherRetriesUntilExplicitAck pins the acknowledgement contract:
// delta pages are retried verbatim (never consumed by a fetch) until the
// gateway explicitly acknowledges the last-applied revision; the 30-second
// lease refresh runs only while sync is healthy; a new gateway boot resets
// the acknowledgement state. Retention is time-bounded, not ack-pruned, so
// history stays fetchable for retries within the window.
func TestPublisherRetriesUntilExplicitAck(t *testing.T) {
	app := newPublisherTestApp(t)
	apiKey, _ := createPublisherAgent(t, app, "ack", "sb0a1b2c3d", 10011, 2)
	sessionID := createPublisherSession(t, app, apiKey, "ackshare", "album.sb0a1b2c3d.example.com", nil)

	publisher := newTestPublisher(t, app, PublisherConfig{RevisionSeed: publisherTestSeed})
	if err := publisher.PublishAdd(sessionID); err != nil {
		t.Fatalf("publish add: %v", err)
	}

	first, err := publisher.RouteDeltas(publisherTestSeed)
	if err != nil {
		t.Fatalf("first delta fetch: %v", err)
	}
	second, err := publisher.RouteDeltas(publisherTestSeed)
	if err != nil {
		t.Fatalf("second delta fetch: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("delta fetch is not retry-safe:\n first %+v\n second %+v", first, second)
	}

	// Unhealthy: no ack yet — the lease refresh must not run.
	if publisher.Healthy() {
		t.Fatalf("publisher healthy without any acknowledgement")
	}
	if got := publisher.AcknowledgedRevision(); got != 0 {
		t.Fatalf("AcknowledgedRevision before any ack = %d, want 0", got)
	}
	touched, err := publisher.RefreshLeases()
	if err != nil {
		t.Fatalf("refresh while unhealthy: %v", err)
	}
	if touched != 0 {
		t.Fatalf("refresh ran while unhealthy and touched %d routes", touched)
	}
	publisher.mu.Lock()
	revision := publisher.revision
	publisher.mu.Unlock()
	if revision != publisherTestSeed+1 {
		t.Fatalf("revision moved while unhealthy: %d, want %d", revision, publisherTestSeed+1)
	}

	// A stale acknowledgement (below the published revision) keeps sync
	// unhealthy.
	if _, err := publisher.Acknowledge(StatusAck{Version: ProtocolVersion, GatewayBootID: publisherTestBootID, ControlEpoch: publisher.Epoch(), LastAppliedRevision: publisherTestSeed}); err != nil {
		t.Fatalf("stale ack: %v", err)
	}
	if publisher.Healthy() {
		t.Fatalf("publisher healthy on stale ack %d < %d", publisherTestSeed, publisherTestSeed+1)
	}

	// The explicit ack of the published revision makes sync healthy.
	if _, err := publisher.Acknowledge(StatusAck{Version: ProtocolVersion, GatewayBootID: publisherTestBootID, ControlEpoch: publisher.Epoch(), LastAppliedRevision: publisherTestSeed + 1}); err != nil {
		t.Fatalf("explicit ack: %v", err)
	}
	if !publisher.Healthy() {
		t.Fatalf("publisher unhealthy after explicit ack of %d", publisherTestSeed+1)
	}
	if got := publisher.AcknowledgedRevision(); got != publisherTestSeed+1 {
		t.Fatalf("AcknowledgedRevision = %d, want %d", got, publisherTestSeed+1)
	}

	// An ack from a foreign control epoch (in-flight across a control
	// restart) is ignored wholesale: it must not reset the recorded state
	// nor disturb the current epoch's health watermark (R2).
	foreignOutcome, err := publisher.Acknowledge(StatusAck{Version: ProtocolVersion, GatewayBootID: "gateway-boot-foreign", ControlEpoch: publisher.Epoch() + 1, LastAppliedRevision: 0})
	if err != nil {
		t.Fatalf("foreign-epoch ack: %v", err)
	}
	if foreignOutcome.Accepted {
		t.Fatalf("foreign-epoch ack must be reported NOT accepted, got %+v", foreignOutcome)
	}
	if foreignOutcome.Reason != AckReasonForeignEpoch {
		t.Fatalf("foreign-epoch refusal reason = %q, want %q", foreignOutcome.Reason, AckReasonForeignEpoch)
	}
	if !publisher.Healthy() {
		t.Fatalf("foreign-epoch ack must be ignored, not reset health")
	}
	if got := publisher.AcknowledgedRevision(); got != publisherTestSeed+1 {
		t.Fatalf("AcknowledgedRevision after foreign-epoch ack = %d, want unchanged %d", got, publisherTestSeed+1)
	}
	touched, err = publisher.RefreshLeases()
	if err != nil {
		t.Fatalf("refresh after ack: %v", err)
	}
	if touched != 1 {
		t.Fatalf("refresh touched %d routes after ack, want 1", touched)
	}

	// A gateway restart (new boot ID) resets acknowledgement state wholesale.
	if _, err := publisher.Acknowledge(StatusAck{Version: ProtocolVersion, GatewayBootID: "gateway-boot-test-2", ControlEpoch: publisher.Epoch(), LastAppliedRevision: 0}); err != nil {
		t.Fatalf("new-boot ack: %v", err)
	}
	if publisher.Healthy() {
		t.Fatalf("publisher healthy after new gateway boot reported revision 0")
	}

	// History survives acknowledgements within the retention window.
	history, err := publisher.RouteDeltas(publisherTestSeed)
	if err != nil {
		t.Fatalf("history after acks: %v", err)
	}
	if len(history.Deltas) < 2 {
		t.Fatalf("retained history shrank after acks: %+v", history)
	}
}

// TestPublisherGapForcesSnapshot pins the review-mandated bounded retention:
// the delta history is kept only inside an explicit window (and count cap);
// any `since` below the retained horizon yields a gap page — never stale
// deltas — forcing the gateway to reconcile from a full snapshot (§11.3).
func TestPublisherGapForcesSnapshot(t *testing.T) {
	app := newPublisherTestApp(t)
	apiKey, _ := createPublisherAgent(t, app, "gap", "sbg4p3h1j2", 10077, 4)
	session1 := createPublisherSession(t, app, apiKey, "gapshare1", "one.sbg4p3h1j2.example.com", nil)
	session2 := createPublisherSession(t, app, apiKey, "gapshare2", "two.sbg4p3h1j2.example.com", nil)

	now := publisherTestBase
	publisher := newTestPublisher(t, app, PublisherConfig{
		RevisionSeed:         publisherTestSeed,
		DeltaRetentionWindow: 5 * time.Minute,
		Now:                  func() time.Time { return now },
	})

	if err := publisher.PublishAdd(session1); err != nil {
		t.Fatalf("publish add 1: %v", err)
	}
	// Advance the fake clock past the retention window, then publish again:
	// the first delta ages out of the bounded ring.
	now = publisherTestBase.Add(6 * time.Minute)
	if err := publisher.PublishAdd(session2); err != nil {
		t.Fatalf("publish add 2: %v", err)
	}

	page, err := publisher.RouteDeltas(publisherTestSeed)
	if err != nil {
		t.Fatalf("deltas over window: %v", err)
	}
	// The gateway at the seed revision needs delta seed+1, which aged out of
	// the bounded window: the deltas in (seed, oldest] are lost, so the page
	// must be a gap — never a stale partial answer.
	if page.Version != ProtocolVersion || page.Status != DeltaStatusGap {
		t.Fatalf("over-window since must yield a gap page, got status %q", page.Status)
	}
	if len(page.Deltas) != 0 {
		t.Fatalf("gap page carried stale deltas: %+v", page.Deltas)
	}
	if page.Since != publisherTestSeed || page.LatestRevision != publisherTestSeed+2 {
		t.Fatalf("gap page since/latest = %d/%d, want %d/%d", page.Since, page.LatestRevision, publisherTestSeed, publisherTestSeed+2)
	}
	if err := ValidateDeltaPage(page, MaxDeltasPerPage); err != nil {
		t.Fatalf("gap page fails protocol validation: %v", err)
	}

	// A gateway already at seed+1 is still bridgeable: the retained suffix
	// starts exactly at its successor, so it gets the complete page.
	bridged, err := publisher.RouteDeltas(publisherTestSeed + 1)
	if err != nil {
		t.Fatalf("bridgeable deltas: %v", err)
	}
	if bridged.Status != DeltaStatusOK || len(bridged.Deltas) != 1 || bridged.Deltas[0].Revision != publisherTestSeed+2 {
		t.Fatalf("bridgeable since must return the complete page, got %q %+v", bridged.Status, bridged.Deltas)
	}

	// A since from before any retained history gaps as well.
	fromZero, err := publisher.RouteDeltas(0)
	if err != nil {
		t.Fatalf("deltas since 0: %v", err)
	}
	if fromZero.Status != DeltaStatusGap {
		t.Fatalf("since 0 must gap when nothing that old is retained, got %q", fromZero.Status)
	}

	// The current revision still answers with an in-window page.
	current, err := publisher.RouteDeltas(publisherTestSeed + 2)
	if err != nil {
		t.Fatalf("deltas at latest: %v", err)
	}
	if current.Status != DeltaStatusOK {
		t.Fatalf("current since must answer OK, got %q", current.Status)
	}

	// The count cap bounds the ring independently of the clock: with room
	// for only two deltas, the oldest of three is dropped. A gateway at the
	// seed revision needs the dropped delta and must gap; a gateway at the
	// dropped delta's own revision is still bridgeable.
	capped := newTestPublisher(t, app, PublisherConfig{
		RevisionSeed:         publisherTestSeed,
		MaxRetainedDeltas:    2,
		DeltaRetentionWindow: time.Hour,
	})
	for _, session := range []string{session1, session2, session1} {
		if err := capped.PublishAdd(session); err != nil {
			t.Fatalf("capped publish add: %v", err)
		}
	}
	overCount, err := capped.RouteDeltas(publisherTestSeed)
	if err != nil {
		t.Fatalf("capped deltas: %v", err)
	}
	if overCount.Status != DeltaStatusGap {
		t.Fatalf("over-cap since must gap, got %q with deltas %+v", overCount.Status, overCount.Deltas)
	}
	inCount, err := capped.RouteDeltas(publisherTestSeed + 1)
	if err != nil {
		t.Fatalf("in-cap deltas: %v", err)
	}
	if inCount.Status != DeltaStatusOK || len(inCount.Deltas) != 2 {
		t.Fatalf("bridgeable since must return the two retained deltas, got %q %+v", inCount.Status, inCount.Deltas)
	}
}

// TestPublisherStampsControlEpochOnSnapshotsAndDeltas pins R2 ruling 1: the
// publisher's control epoch — generated per process start or injected — is
// stamped on EVERY served payload, and an EMPTY snapshot (no publishable
// routes: the fresh-control-boot state) still carries it, because the epoch
// is the only authority that makes such a snapshot adoptable by a gateway
// holding older-epoch state (the T15-m1 redelivery concern, Sol Important-4's
// new-boot adoption gap).
func TestPublisherStampsControlEpochOnSnapshotsAndDeltas(t *testing.T) {
	app := newPublisherTestApp(t)
	apiKey, _ := createPublisherAgent(t, app, "epoch", "sbeb3f1a2c", 10093, 5)

	publisher := newTestPublisher(t, app, PublisherConfig{RevisionSeed: publisherTestSeed, Epoch: 7777})
	if publisher.Epoch() != 7777 {
		t.Fatalf("Epoch() = %d, want the configured 7777", publisher.Epoch())
	}

	// Empty snapshot (no published routes yet): the epoch is ALWAYS present.
	snapshot, err := publisher.RouteSnapshot()
	if err != nil {
		t.Fatalf("empty snapshot: %v", err)
	}
	if len(snapshot.Routes) != 0 {
		t.Fatalf("expected an empty snapshot, got %+v", snapshot.Routes)
	}
	if snapshot.Epoch != 7777 {
		t.Fatalf("empty snapshot epoch = %d, want 7777 (always present, even empty)", snapshot.Epoch)
	}
	if err := ValidateSnapshot(snapshot, MaxRoutesPerSnapshot); err != nil {
		t.Fatalf("empty snapshot with epoch rejected: %v", err)
	}

	sessionID := createPublisherSession(t, app, apiKey, "epochshare", "album.sbeb3f1a2c.example.com", nil)
	if err := publisher.PublishAdd(sessionID); err != nil {
		t.Fatalf("publish add: %v", err)
	}

	page, err := publisher.RouteDeltas(publisherTestSeed)
	if err != nil {
		t.Fatalf("delta fetch: %v", err)
	}
	if page.Epoch != 7777 {
		t.Fatalf("delta page epoch = %d, want 7777", page.Epoch)
	}
	if err := ValidateDeltaPage(page, MaxDeltasPerPage); err != nil {
		t.Fatalf("delta page with epoch rejected: %v", err)
	}

	gap, err := publisher.RouteDeltas(publisherTestSeed - 1)
	if err != nil {
		t.Fatalf("gap fetch: %v", err)
	}
	if gap.Status != DeltaStatusGap || gap.Epoch != 7777 {
		t.Fatalf("gap page = status %q epoch %d, want gap at epoch 7777", gap.Status, gap.Epoch)
	}

	// A zero configured epoch generates a non-zero per-boot identifier.
	generated := newTestPublisher(t, app, PublisherConfig{RevisionSeed: publisherTestSeed})
	if generated.Epoch() == 0 {
		t.Fatalf("generated epoch = 0, want a non-zero per-boot identifier")
	}
}
