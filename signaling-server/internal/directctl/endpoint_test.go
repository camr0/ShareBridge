package directctl

import (
	"context"
	"errors"
	"testing"
)

func TestReportEndpointRetriesFailedDDNS(t *testing.T) {
	app, ctrl := newTestController(t)
	// agents.api_key_id is a required relation, so enrollment needs a real API
	// key record; use its generated ID as apiKeyID (the plan's literal "key-1"
	// would fail the relation constraint inside LoadOrCreateAgent).
	apiKeyID := mustAPIKey(t, app, "key-1").Id
	ctrl.HandleHello(context.Background(), nil, apiKeyID, "acct-1", "agent-1")

	fail := true
	ctrl.ddnsFn = func(ctx context.Context, name, ip string, ttl int) (string, error) {
		if fail {
			return "", errors.New("dns down")
		}
		return "", nil
	}
	ctrl.HandleReportEndpoint(context.Background(), apiKeyID, "1.2.3.4", 0, "")
	rec, _, _ := LoadOrCreateAgent(app, apiKeyID)
	if rec.GetString("endpoint_ip") == "1.2.3.4" {
		t.Fatalf("endpoint_ip must not be saved before DDNS succeeds")
	}

	fail = false
	ctrl.HandleReportEndpoint(context.Background(), apiKeyID, "1.2.3.4", 0, "")
	rec, _, _ = LoadOrCreateAgent(app, apiKeyID)
	if rec.GetString("endpoint_ip") != "1.2.3.4" {
		t.Fatalf("endpoint_ip saved after DDNS success")
	}
}

func TestReportEndpointRejectsBadStatus(t *testing.T) {
	app, ctrl := newTestController(t)
	apiKeyID := mustAPIKey(t, app, "bad-status").Id
	ctrl.HandleHello(context.Background(), nil, apiKeyID, "acct-1", "agent-1")

	called := false
	ctrl.ddnsFn = func(ctx context.Context, name, ip string, ttl int) (string, error) {
		called = true
		return "", nil
	}

	// status "close_failed" with port 0 is invalid → ignored (no DDNS, no save).
	ctrl.HandleReportEndpoint(context.Background(), apiKeyID, "1.2.3.4", 0, "close_failed")
	if called {
		t.Fatalf("ddns must not run for invalid close_failed + port 0")
	}
	rec, _, _ := LoadOrCreateAgent(app, apiKeyID)
	if rec.GetString("endpoint_ip") != "" {
		t.Fatalf("endpoint_ip must not be saved for invalid close_failed + port 0")
	}

	// Unknown status → also rejected (no DDNS, no save).
	ctrl.HandleReportEndpoint(context.Background(), apiKeyID, "1.2.3.4", 9000, "bogus")
	if called {
		t.Fatalf("ddns must not run for unknown status")
	}
	if rec.GetString("endpoint_ip") != "" {
		t.Fatalf("endpoint_ip must not be saved for unknown status")
	}
}

func TestReportEndpointSameIPSkipsDDNS(t *testing.T) {
	app, ctrl := newTestController(t)
	apiKeyID := mustAPIKey(t, app, "same-ip").Id
	ctrl.HandleHello(context.Background(), nil, apiKeyID, "acct-1", "agent-1")

	ctrl.ddnsFn = func(ctx context.Context, name, ip string, ttl int) (string, error) { return "", nil }
	ctrl.HandleReportEndpoint(context.Background(), apiKeyID, "1.2.3.4", 0, "")

	calls := 0
	ctrl.ddnsFn = func(ctx context.Context, name, ip string, ttl int) (string, error) {
		calls++
		return "", errors.New("should not be re-provisioned")
	}
	ctrl.HandleReportEndpoint(context.Background(), apiKeyID, "1.2.3.4", 0, "")
	if calls != 0 {
		t.Fatalf("ddns must not be re-provisioned for unchanged IP")
	}
	rec, _, _ := LoadOrCreateAgent(app, apiKeyID)
	if rec.GetString("endpoint_ip") != "1.2.3.4" {
		t.Fatalf("endpoint_ip must remain saved for unchanged IP")
	}
}
