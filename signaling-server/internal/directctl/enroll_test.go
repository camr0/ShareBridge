package directctl

import (
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestHandleTLSReadyUnknownFingerprint(t *testing.T) {
	app, ctrl := newTestController(t)
	apiKeyID := mustAPIKey(t, app, "key-unknown-fp").Id

	// Install a current epoch (conn=nil) so the handler's epoch-fencing check
	// passes; the fingerprint itself is still unknown.
	ctrl.HandleHello(context.Background(), nil, apiKeyID, "acct-1", "agent-1")

	// No chain has been issued for this key, so any fingerprint is unknown.
	sent := ctrl.captureSend(func() {
		ctrl.HandleTLSReady(context.Background(), nil, apiKeyID, "deadbeef", time.Now().Add(90*24*time.Hour).Format(time.RFC3339))
	})
	if sent == nil || sent["type"] != "cert_error" {
		t.Fatalf("expected cert_error, got %v", sent)
	}
	if sent["reason"] != "unknown fingerprint" {
		t.Fatalf("expected reason 'unknown fingerprint', got %v", sent["reason"])
	}
	if ctrl.epochReady(apiKeyID) {
		t.Fatalf("must not be ready on unknown fingerprint")
	}
}

func TestEpochReadinessGatedOnDDNSAndTLS(t *testing.T) {
	app, ctrl := newTestController(t)
	// agents.api_key_id is a required relation, so the enrollment path needs a
	// real API key record (the plan's literal "key-1" would fail the relation
	// constraint inside LoadOrCreateAgent). Use its generated ID as apiKeyID.
	apiKeyID := mustAPIKey(t, app, "key-1").Id

	ctrl.ddnsFn = func(ctx context.Context, name, ip string, ttl int) (string, error) { return "", nil }

	sent := ctrl.captureSend(func() { ctrl.HandleHello(context.Background(), nil, apiKeyID, "acct-1", "agent-1") })
	if sent["type"] != "enrolled" {
		t.Fatalf("expected enrolled")
	}
	if sent["namespace"] == "" {
		t.Fatalf("namespace empty")
	}

	// tls_ready before DDNS → no enrollment_ready yet. Seed a real chain so
	// ChainByLeaf accepts the reported fingerprint.
	chain, err := ctrl.coord.Issue(context.Background(), []byte("csr"), "sbdeadbeef", apiKeyID)
	if err != nil {
		t.Fatal(err)
	}
	fp := leafFPOf(chain)
	sent2 := ctrl.captureSend(func() {
		ctrl.HandleTLSReady(context.Background(), nil, apiKeyID, fp, time.Now().Add(90*24*time.Hour).Format(time.RFC3339))
	})
	if sent2 != nil && sent2["type"] == "enrollment_ready" {
		t.Fatalf("must not be ready before DDNS")
	}

	// DDNS success (via report_endpoint) → enrollment_ready.
	sent3 := ctrl.captureSend(func() { ctrl.HandleReportEndpoint(context.Background(), nil, apiKeyID, "1.2.3.4", 0, "") })
	if sent3["type"] != "enrollment_ready" {
		t.Fatalf("expected enrollment_ready after DDNS")
	}

	// Persisted ready must not authorize a fresh epoch alone.
	ctrl.HandleHello(context.Background(), nil, apiKeyID, "acct-1", "agent-1")
	if ctrl.epochReady(apiKeyID) {
		t.Fatalf("new epoch must not inherit readiness")
	}
}

// TestHandleTLSReadyAcceptsPersistedFingerprintAfterRestart simulates a control
// restart (fresh coordinator, empty leaf index) where the agent record still
// holds the installed cert fingerprint + expiry. The tls_ready must be accepted
// via the persisted row and re-seed the coordinator's leaf index (C3).
func TestHandleTLSReadyAcceptsPersistedFingerprintAfterRestart(t *testing.T) {
	app, ctrl := newTestController(t)
	apiKeyID := mustAPIKey(t, app, "key-restart").Id
	ctrl.HandleHello(context.Background(), nil, apiKeyID, "acct-1", "agent-1")

	// Persist a cert row WITHOUT going through Issue (simulates the coordinator
	// having restarted and lost its in-memory leaf index).
	rec, _, err := LoadOrCreateAgent(app, apiKeyID)
	if err != nil {
		t.Fatal(err)
	}
	const fp = "abc123persisted"
	exp := time.Now().Add(90 * 24 * time.Hour)
	if err := SaveCertReady(app, rec, fp, exp); err != nil {
		t.Fatal(err)
	}

	sent := ctrl.captureSend(func() {
		ctrl.HandleTLSReady(context.Background(), nil, apiKeyID, fp, exp.Format(time.RFC3339))
	})
	if sent != nil && sent["type"] == "cert_error" {
		t.Fatalf("persisted fingerprint must be accepted, got %v", sent)
	}
	if !ctrl.coord.HasLeafFingerprint(apiKeyID, fp) {
		t.Fatalf("leaf index must be re-seeded from the persisted row")
	}
}

// TestFencedConnCannotDriveEpochState verifies a superseded connection cannot
// drive epoch-sensitive state: after a replacement hello installs a new epoch,
// a stale conn's report_endpoint (and by extension tls_ready/csr_submit/
// open_ack) is ignored (I2).
func TestFencedConnCannotDriveEpochState(t *testing.T) {
	app, ctrl := newTestController(t)
	apiKeyID := mustAPIKey(t, app, "key-fence").Id

	stale := new(websocket.Conn)
	fresh := new(websocket.Conn)
	ctrl.HandleHello(context.Background(), stale, apiKeyID, "acct-1", "agent-1")
	ctrl.HandleHello(context.Background(), fresh, apiKeyID, "acct-1", "agent-1")

	ddnsCalls := 0
	ctrl.ddnsFn = func(ctx context.Context, name, ip string, ttl int) (string, error) {
		ddnsCalls++
		return "", nil
	}

	// A stale conn's report_endpoint must be ignored: no DDNS, no readiness.
	ctrl.HandleReportEndpoint(context.Background(), stale, apiKeyID, "1.2.3.4", 0, "")
	if ddnsCalls != 0 {
		t.Fatalf("stale conn must not trigger DDNS, got %d calls", ddnsCalls)
	}
	if ctrl.epochReady(apiKeyID) {
		t.Fatalf("stale conn must not advance readiness")
	}

	// The fresh conn does trigger DDNS.
	ctrl.HandleReportEndpoint(context.Background(), fresh, apiKeyID, "1.2.3.4", 0, "")
	if ddnsCalls != 1 {
		t.Fatalf("fresh conn must trigger DDNS, got %d calls", ddnsCalls)
	}
}
