package directctl

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

// seedSession seeds a live, active session code "abc" → apiKey "key-1" with
// origin "demo.sb1.example.com" and a future expiry. It does NOT install an
// epoch or register a hub agent, so callers can control each gate separately.
func seedSession(t *testing.T, app core.App) string {
	t.Helper()
	apiKeyID := mustAPIKey(t, app, "key-1").Id

	sessions, _ := app.FindCollectionByNameOrId("sessions")
	sess := core.NewRecord(sessions)
	sess.Set("code", "abc")
	sess.Set("api_key_id", apiKeyID)
	sess.Set("origin", "demo.sb1.example.com")
	sess.Set("is_active", true)
	sess.Set("expires_at", time.Now().Add(time.Hour))
	if err := app.Save(sess); err != nil {
		t.Fatalf("save session: %v", err)
	}
	return apiKeyID
}

// seedSessionAndEpoch seeds a session code "abc" → apiKey "key-1" with origin
// "demo.sb1.example.com", and installs a ready epoch + a connected hub agent so
// Redirect passes every gate. The api_key_id relation requires a real API key
// record, so it uses mustAPIKey's generated ID.
func seedSessionAndEpoch(t *testing.T, app core.App, ctrl *Controller) string {
	t.Helper()
	apiKeyID := seedSession(t, app)

	// Ready epoch (epochReady) + connected agent (hub.AgentConnected).
	ctrl.epochMu.Lock()
	ctrl.epochs[apiKeyID] = &epochState{agentID: "agent-1", namespace: "sbdeadbeef", ready: true}
	ctrl.epochMu.Unlock()
	ctrl.hub.RegisterAgent(apiKeyID, nil)

	return apiKeyID
}

func TestRedirect302ToOriginNoStore(t *testing.T) {
	app, ctrl := newTestController(t)
	seedSessionAndEpoch(t, app, ctrl) // code "abc" → apiKey "key-1", origin "demo.sb1.example.com", epoch ready
	ctrl.emitOpenFn = func(ctx context.Context, apiKeyID, shareID, origin string, lease time.Duration) (OpenAck, error) {
		return OpenAck{ShareID: shareID, GrantedPort: 443, PublicIP: "1.2.3.4", Status: "ok", Nonce: "n", Seq: 1}, nil
	}
	ctrl.probeFn = func(ctx context.Context, origin, code, apiKeyID string, ack OpenAck) error { return nil }

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/s/abc", nil)
	_ = ctrl.Redirect(rec, req, "abc")
	if rec.Code != http.StatusFound {
		t.Fatalf("code = %d", rec.Code)
	}
	if rec.Header().Get("Location") != "https://demo.sb1.example.com/s/abc" {
		t.Fatalf("loc = %q", rec.Header().Get("Location"))
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("missing no-store")
	}
}

func TestRedirectUnavailableWhenNotReady(t *testing.T) {
	_, ctrl := newTestController(t)
	// no session → unavailable
	rec := httptest.NewRecorder()
	_ = ctrl.Redirect(rec, httptest.NewRequest("GET", "/s/abc", nil), "abc")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d", rec.Code)
	}
}

// TestRedirectUnavailableLiveSessionNoReadyEpoch seeds a LIVE session and a
// connected hub agent, but does NOT install a ready epoch. This exercises the
// epochReady gate specifically (readiness is connection-epoch-local, not just
// persisted cert_status), distinct from the no-session case above.
func TestRedirectUnavailableLiveSessionNoReadyEpoch(t *testing.T) {
	app, ctrl := newTestController(t)
	apiKeyID := seedSession(t, app) // live session, but NO epoch
	ctrl.hub.RegisterAgent(apiKeyID, nil)

	rec := httptest.NewRecorder()
	_ = ctrl.Redirect(rec, httptest.NewRequest("GET", "/s/abc", nil), "abc")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

// TestRedirectUnavailableLiveSessionAgentDisconnected seeds a LIVE session and
// a ready epoch, but does NOT register a connected hub agent, exercising the
// AgentConnected gate independently of epoch readiness.
func TestRedirectUnavailableLiveSessionAgentDisconnected(t *testing.T) {
	app, ctrl := newTestController(t)
	apiKeyID := seedSession(t, app) // live session
	ctrl.epochMu.Lock()
	ctrl.epochs[apiKeyID] = &epochState{agentID: "agent-1", namespace: "sbdeadbeef", ready: true}
	ctrl.epochMu.Unlock()
	// no hub.RegisterAgent → AgentConnected is false

	rec := httptest.NewRecorder()
	_ = ctrl.Redirect(rec, httptest.NewRequest("GET", "/s/abc", nil), "abc")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

// seedResolveSession seeds a session with code and the given mutation applied on
// top of a default active, direct, gallery (immich) session. The mutation may
// flip is_active/share_type/relay_only/is_password_protected/expires_at or set
// inactive_reason. This is the ResolveForRedirect test harness, so it does NOT
// install an epoch or a connected hub agent (status logic must not depend on
// live transport readiness).
func seedResolveSession(t *testing.T, app core.App, code string, mutate func(*core.Record)) {
	t.Helper()
	apiKeyID := mustAPIKey(t, app, "key-"+code).Id

	sessions, _ := app.FindCollectionByNameOrId("sessions")
	sess := core.NewRecord(sessions)
	sess.Set("code", code)
	sess.Set("api_key_id", apiKeyID)
	sess.Set("agent_id", "agent-1")
	sess.Set("share_type", "immich")
	sess.Set("is_active", true)
	if mutate != nil {
		mutate(sess)
	}
	if err := app.Save(sess); err != nil {
		t.Fatalf("save session %s: %v", code, err)
	}
}

// TestResolveForRedirect is the canonical-route status matrix (§8): active
// gallery → 302; relay-only/WebDAV/protected/expired → 410; revoked/unknown →
// 404; inactive rows with a missing/unrecognized inactive_reason fail safe as
// 404.
func TestResolveForRedirect(t *testing.T) {
	app, ctrl := newTestController(t)

	cases := []struct {
		name   string
		code   string
		seed   bool
		mutate func(*core.Record)
		status int
	}{
		{name: "active gallery", code: "active", seed: true, status: http.StatusFound},
		{name: "relay-only active", code: "relay", seed: true, mutate: func(r *core.Record) { r.Set("relay_only", true) }, status: http.StatusGone},
		{name: "webdav active", code: "webdav", seed: true, mutate: func(r *core.Record) { r.Set("share_type", "opencloud") }, status: http.StatusGone},
		{name: "protected active", code: "prot", seed: true, mutate: func(r *core.Record) { r.Set("is_password_protected", true) }, status: http.StatusGone},
		{name: "expired active", code: "expactive", seed: true, mutate: func(r *core.Record) { r.Set("expires_at", time.Now().Add(-time.Hour)) }, status: http.StatusGone},
		{name: "revoked tombstone", code: "revoked", seed: true, mutate: func(r *core.Record) { r.Set("is_active", false); r.Set("inactive_reason", "revoked") }, status: http.StatusNotFound},
		{name: "expired tombstone", code: "exptomb", seed: true, mutate: func(r *core.Record) { r.Set("is_active", false); r.Set("inactive_reason", "expired") }, status: http.StatusGone},
		{name: "unsupported tombstone", code: "unsup", seed: true, mutate: func(r *core.Record) { r.Set("is_active", false); r.Set("inactive_reason", "unsupported") }, status: http.StatusGone},
		{name: "missing reason", code: "missing", seed: true, mutate: func(r *core.Record) { r.Set("is_active", false) }, status: http.StatusNotFound},
		{name: "invalid reason", code: "invalid", seed: true, mutate: func(r *core.Record) { r.Set("is_active", false); r.Set("inactive_reason", "bogus") }, status: http.StatusNotFound},
		{name: "unknown code", code: "unknown", status: http.StatusNotFound},
	}

	for _, tc := range cases {
		if tc.seed {
			seedResolveSession(t, app, tc.code, tc.mutate)
		}
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, status := ctrl.ResolveForRedirect(tc.code)
			if status != tc.status {
				t.Fatalf("status = %d, want %d", status, tc.status)
			}
			if tc.status == http.StatusFound && rec == nil {
				t.Fatalf("active gallery must return its session record")
			}
		})
	}
}

// TestRedirectUnavailableGrantedPortZero seeds a fully-ready session/epoch/agent
// but stubs emitOpenFn to return GrantedPort 0 with Status "ok", asserting the
// port-range rejection (port 0 must never redirect).
func TestRedirectUnavailableGrantedPortZero(t *testing.T) {
	app, ctrl := newTestController(t)
	seedSessionAndEpoch(t, app, ctrl)
	ctrl.emitOpenFn = func(ctx context.Context, apiKeyID, shareID, origin string, lease time.Duration) (OpenAck, error) {
		return OpenAck{ShareID: shareID, GrantedPort: 0, PublicIP: "1.2.3.4", Status: "ok", Nonce: "n", Seq: 1}, nil
	}
	ctrl.probeFn = func(ctx context.Context, origin, code, apiKeyID string, ack OpenAck) error { return nil }

	rec := httptest.NewRecorder()
	_ = ctrl.Redirect(rec, httptest.NewRequest("GET", "/s/abc", nil), "abc")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}
