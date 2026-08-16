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
