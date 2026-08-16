package directctl

import (
	"context"
	"testing"
	"time"
)

func TestHandleTLSReadyUnknownFingerprint(t *testing.T) {
	app, ctrl := newTestController(t)
	apiKeyID := mustAPIKey(t, app, "key-unknown-fp").Id

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
	sent3 := ctrl.captureSend(func() { ctrl.HandleReportEndpoint(context.Background(), apiKeyID, "1.2.3.4", 0, "") })
	if sent3["type"] != "enrollment_ready" {
		t.Fatalf("expected enrollment_ready after DDNS")
	}

	// Persisted ready must not authorize a fresh epoch alone.
	ctrl.HandleHello(context.Background(), nil, apiKeyID, "acct-1", "agent-1")
	if ctrl.epochReady(apiKeyID) {
		t.Fatalf("new epoch must not inherit readiness")
	}
}
