package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These tests pin the split between the two agent HTTP surfaces when
// UI_PASSWORD is set (the recommended remote posture):
//
//   - the admin surface (pages, HTML fragments, /api/shares*, /api/settings*,
//     /api/status, /api/lockdown*, quota widgets, ...) stays behind HTTP Basic;
//   - the /api/v1 JSON API used cross-origin by the OpenCloud/Nextcloud
//     extensions is gated ONLY by X-API-Key, and its CORS preflight (which
//     browsers send without credentials) is answered without Basic auth.
//
// The exemption must be exact: no prefix, case or percent-encoding trick may
// route an admin path through the v1 surface or a v1 path through the admin
// gate.

const (
	v1TestAPIKey = "sb_agent_key"
	v1TestOrigin = "https://opencloud.example.com"
)

// v1HandlerServer builds the production handler for a non-loopback bind with a
// password (the remote posture) and the default test config.
func v1HandlerServer(t *testing.T) (http.Handler, *lockdownDaemonMock) {
	t.Helper()
	ws, mock := newAdminServer(t, "0.0.0.0", adminTestPassword, nil)
	return ws.handler(), mock
}

// TestV1APIAcceptsAPIKeyWithoutAdminBasic is requirement (a): a v1 request
// carrying a valid X-API-Key but no Basic credential must succeed exactly as
// it did before UI_PASSWORD became the recommended posture.
func TestV1APIAcceptsAPIKeyWithoutAdminBasic(t *testing.T) {
	handler, mock := v1HandlerServer(t)
	mock.addSession("abc", "https://oc.example.com/s/abc", "file-1")

	// GET a list.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/shares", nil)
	req.Header.Set("X-API-Key", v1TestAPIKey)
	req.Header.Set("Origin", v1TestOrigin)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/shares with API key, no Basic = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != v1TestOrigin {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, v1TestOrigin)
	}
	if rec.Header().Get("WWW-Authenticate") != "" {
		t.Errorf("v1 response must not challenge for Basic auth, got WWW-Authenticate=%q", rec.Header().Get("WWW-Authenticate"))
	}

	// GET the extension settings.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	req.Header.Set("X-API-Key", v1TestAPIKey)
	req.Header.Set("Origin", v1TestOrigin)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/settings with API key, no Basic = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// POST a share (no CSRF headers: the v1 API is not the browser admin UI).
	req = httptest.NewRequest(http.MethodPost, "/api/v1/shares",
		strings.NewReader(`{"share_url":"https://oc.example.com/s/xyz","share_type":"opencloud","expiry_hours":24}`))
	req.Header.Set("X-API-Key", v1TestAPIKey)
	req.Header.Set("Origin", v1TestOrigin)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/v1/shares with API key, no Basic = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	// DELETE a share.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/shares/abc", nil)
	req.Header.Set("X-API-Key", v1TestAPIKey)
	req.Header.Set("Origin", v1TestOrigin)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE /api/v1/shares/abc with API key, no Basic = %d, want 204: %s", rec.Code, rec.Body.String())
	}
}

// TestV1APIStillFailsClosedOnBadAPIKey is requirement (b): the v1 surface must
// reject missing and invalid API keys with its own 401, not fall through to the
// admin surface and not require Basic auth as a substitute.
func TestV1APIStillFailsClosedOnBadAPIKey(t *testing.T) {
	handler, mock := v1HandlerServer(t)

	keys := []string{"", "wrong-key", "sb_agent_ke", v1TestAPIKey + "x"}
	for _, key := range keys {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/shares", nil)
		if key != "" {
			req.Header.Set("X-API-Key", key)
		}
		req.Header.Set("Origin", v1TestOrigin)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("GET /api/v1/shares key=%q = %d, want 401", key, rec.Code)
		}
		if rec.Header().Get("WWW-Authenticate") != "" {
			t.Errorf("key=%q: rejection must come from the API-key gate, not a Basic challenge (WWW-Authenticate=%q)",
				key, rec.Header().Get("WWW-Authenticate"))
		}
		if !strings.Contains(rec.Body.String(), `"code":"UNAUTHORIZED"`) {
			t.Errorf("key=%q: body = %q, want the v1 UNAUTHORIZED error", key, rec.Body.String())
		}
		// CORS headers must still be present so the browser surfaces the error.
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != v1TestOrigin {
			t.Errorf("key=%q: Access-Control-Allow-Origin = %q on 401, want %q", key, got, v1TestOrigin)
		}
	}

	// A state-changing v1 request with no key must not reach the daemon.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/shares",
		strings.NewReader(`{"share_url":"https://oc.example.com/s/xyz","share_type":"opencloud"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /api/v1/shares without key = %d, want 401", rec.Code)
	}
	if len(mock.sessions) != 0 {
		t.Fatalf("unauthenticated v1 POST created %d sessions, want 0", len(mock.sessions))
	}
}

// TestV1CORSPreflightRequiresNoBasic is requirement (c): the browser preflight
// for a v1 route must reach corsMiddleware and be answered 204 with the
// Access-Control-* headers, without any credential.
func TestV1CORSPreflightRequiresNoBasic(t *testing.T) {
	handler, _ := v1HandlerServer(t)

	for _, path := range []string{"/api/v1/shares", "/api/v1/settings", "/api/v1/shares/abc"} {
		req := httptest.NewRequest(http.MethodOptions, path, nil)
		req.Header.Set("Origin", v1TestOrigin)
		req.Header.Set("Access-Control-Request-Method", http.MethodPost)
		req.Header.Set("Access-Control-Request-Headers", "content-type, x-api-key")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Fatalf("OPTIONS %s = %d, want 204: %s", path, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != v1TestOrigin {
			t.Errorf("OPTIONS %s: Access-Control-Allow-Origin = %q, want %q", path, got, v1TestOrigin)
		}
		if got := rec.Header().Get("Access-Control-Allow-Methods"); got == "" {
			t.Errorf("OPTIONS %s: missing Access-Control-Allow-Methods", path)
		}
		if got := rec.Header().Get("Access-Control-Allow-Headers"); got == "" {
			t.Errorf("OPTIONS %s: missing Access-Control-Allow-Headers", path)
		}
		if rec.Header().Get("WWW-Authenticate") != "" {
			t.Errorf("OPTIONS %s: preflight must not be challenged for Basic auth", path)
		}
	}
}

// TestAdminPathsRejectUnauthenticatedWithNoStateChange is requirement (d)
// driven through the real handler: every admin path still answers 401 without
// Basic and nothing mutates.
func TestAdminPathsRejectUnauthenticatedWithNoStateChange(t *testing.T) {
	handler, mock := v1HandlerServer(t)
	before := mock.GetConfig().SignalingURL

	for _, ep := range adminEndpoints {
		// The v1 route is exercised here too: unauthenticated it must still be
		// rejected (by the API-key gate), just never by an admin handler.
		req := httptest.NewRequest(ep.method, ep.path, strings.NewReader(ep.body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s %s = %d, want 401: %s", ep.method, ep.path, rec.Code, rec.Body.String())
		}
	}

	if len(mock.sessions) != 0 {
		t.Fatalf("unauthenticated admin requests created %d sessions, want 0", len(mock.sessions))
	}
	if mock.lockdownCalls != 0 || mock.unlockCalls != 0 {
		t.Fatalf("unauthenticated admin requests reached lockdown: lock=%d unlock=%d", mock.lockdownCalls, mock.unlockCalls)
	}
	if got := mock.GetConfig().SignalingURL; got != before {
		t.Fatalf("unauthenticated admin request mutated settings: %q, want %q", got, before)
	}
}

// TestAdminCORSPreflightIsNotExempt pins the deliberate decision that the v1
// exemption does NOT extend to the admin surface: its OPTIONS preflight is
// still rejected without Basic. The admin UI is same-origin (no preflight),
// and the admin surface sets no CORS headers, so letting it through would only
// remove a credential requirement without enabling any legitimate flow.
func TestAdminCORSPreflightIsNotExempt(t *testing.T) {
	handler, _ := v1HandlerServer(t)

	for _, path := range []string{"/api/settings", "/api/shares", "/api/lockdown"} {
		req := httptest.NewRequest(http.MethodOptions, path, nil)
		req.Header.Set("Origin", v1TestOrigin)
		req.Header.Set("Access-Control-Request-Method", http.MethodPost)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("OPTIONS %s without Basic = %d, want 401", path, rec.Code)
		}
		if rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("OPTIONS %s must not return CORS headers for the admin surface", path)
		}
	}
}

// TestLockdownStillFailsClosedWithoutCredential is requirement (e): the
// lockdown/unlock gate is independent of the v1 exemption and still denies
// both a password-protected deployment without Basic and the default
// no-password deployment.
func TestLockdownStillFailsClosedWithoutCredential(t *testing.T) {
	// Remote posture: password set, caller has none.
	handler, mock := v1HandlerServer(t)
	for _, path := range []string{"/api/lockdown", "/api/unlock"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("HX-Request", "true")
		req.Header.Set("X-API-Key", v1TestAPIKey) // a valid v1 key must not help
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("POST %s without Basic = %d, want 401: %s", path, rec.Code, rec.Body.String())
		}
	}
	if mock.locked || mock.lockdownCalls != 0 || mock.unlockCalls != 0 {
		t.Fatalf("unauthenticated lockdown/unlock changed state: locked=%v lock=%d unlock=%d",
			mock.locked, mock.lockdownCalls, mock.unlockCalls)
	}

	// Default posture: loopback, no password. requireAdminCredential must still
	// fail closed.
	ws, mock2 := newAdminServer(t, "127.0.0.1", "", nil)
	localHandler := ws.handler()
	for _, path := range []string{"/api/lockdown", "/api/unlock"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("HX-Request", "true")
		rec := httptest.NewRecorder()
		localHandler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("loopback POST %s without password = %d, want 403", path, rec.Code)
		}
	}
	if mock2.locked || mock2.lockdownCalls != 0 || mock2.unlockCalls != 0 {
		t.Fatalf("loopback lockdown/unlock changed state: locked=%v lock=%d unlock=%d",
			mock2.locked, mock2.lockdownCalls, mock2.unlockCalls)
	}
}

// adminTrickPaths are paths that a prefix-, case- or encoding-naive exemption
// could wrongly hand to the v1 surface while they actually address an admin
// resource. Every one of them must be handled by the admin gate and rejected
// 401 without Basic. httptest.NewRequest decodes %2F etc. into URL.Path and
// preserves the raw form in URL.RawPath, exactly like the real server.
var adminTrickPaths = []string{
	"/api/v1/../settings",        // dot-segment
	"/api/v1/../api/settings",    // dot-segment to another admin path
	"/api/v1/../api/lockdown",    // dot-segment to a mutation
	"/api/v1/./settings",         // single dot
	"/api/v1//settings",          // empty segment (double slash)
	"/api/v1/shares/../settings", // dot-segment mid-path
	"/api/v1/settings/../../../settings",
	"/api/v1/settings/../../settings",
	"/api/v1/../v1/settings",       // cleans to a v1 path, must still be gated
	"/api/v1%2f..%2fsettings",      // encoded slash
	"/api/v1%2F..%2Fsettings",      // encoded slash (uppercase)
	"/api/v1%2fshares",             // encoded slash before a v1 route
	"/API/V1/settings",             // uppercase path
	"/api/V1/settings",             // mixed-case path
	"/Api/v1/settings",             // mixed-case path
	"/api//v1/settings",            // empty segment before v1
	"//api/v1/settings",            // leading double slash
	"/%2e%2e/api/settings",         // encoded leading dot-segment
	"/%2fapi%2fv1%2f..%2fsettings", // fully encoded separators
	"/api/v1/shares/../..%2f..%2fsettings",
}

// TestAdminTrickPathsStayFailClosed is requirement (f): adversarial path
// shapes must not slip past the admin gate via the v1 exemption. A valid API
// key is supplied (a caller who legitimately holds one must still not reach
// admin handlers) and no Basic credential.
func TestAdminTrickPathsStayFailClosed(t *testing.T) {
	handler, mock := v1HandlerServer(t)
	before := mock.GetConfig().SignalingURL

	methods := []struct {
		method string
		body   string
	}{
		{http.MethodGet, ""},
		{http.MethodPut, "signaling_url=wss%3A%2F%2Fevil.example"},
		{http.MethodPost, "share_type=opencloud&share_url=https%3A%2F%2Fexample.com%2Fshare"},
		{http.MethodDelete, ""},
	}

	for _, path := range adminTrickPaths {
		for _, m := range methods {
			req := httptest.NewRequest(m.method, path, strings.NewReader(m.body))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("HX-Request", "true")
			req.Header.Set("X-Requested-With", "XMLHttpRequest")
			req.Header.Set("X-API-Key", v1TestAPIKey)
			req.Header.Set("Origin", v1TestOrigin)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s = %d, want 401 (admin gate): %s", m.method, path, rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "<!DOCTYPE html>") {
				t.Fatalf("%s %s served admin HTML without Basic auth", m.method, path)
			}
		}
	}

	if len(mock.sessions) != 0 {
		t.Fatalf("trick paths created %d sessions, want 0", len(mock.sessions))
	}
	if mock.lockdownCalls != 0 || mock.unlockCalls != 0 {
		t.Fatalf("trick paths reached lockdown: lock=%d unlock=%d", mock.lockdownCalls, mock.unlockCalls)
	}
	if got := mock.GetConfig().SignalingURL; got != before {
		t.Fatalf("trick paths mutated settings: %q, want %q", got, before)
	}
}

// encodedV1Paths are well-formed /api/v1 paths whose route is not an admin
// resource. They may legitimately reach the v1 surface (where the API-key gate
// applies); the invariant under test is only that they can never execute an
// admin handler or mutate admin state.
var encodedV1Paths = []string{
	"/api/v1/..%2fsettings",
	"/api/v1/%2e%2e/settings",
	"/api/v1/shares%2f..%2f..%2fsettings",
	"/api/v1/settings%2f..%2f..%2f",
	"/api/v1/settings%00",
	"/api/v1/settings;",
	"/api/v1/..;/settings",
	"/api/v1/settings/..%2f..%2fsettings",
	"/api/v1/shares/",
	"/api/v1/",
	"/api/v1/nonexistent",
}

// TestEncodedV1PathsNeverReachAdminHandlers is the other half of requirement
// (f): paths the dispatch classifies as v1 must be answered by the v1 surface
// (API-key gate / 404 / 405), never by an admin handler.
func TestEncodedV1PathsNeverReachAdminHandlers(t *testing.T) {
	handler, mock := v1HandlerServer(t)
	before := mock.GetConfig().SignalingURL

	for _, path := range encodedV1Paths {
		// No API key on purpose: nothing here may fall back to an admin handler.
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		switch rec.Code {
		case http.StatusUnauthorized, http.StatusNotFound, http.StatusMethodNotAllowed:
		default:
			t.Fatalf("GET %s = %d, want 401/404/405 from the v1 surface: %s", path, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "<!DOCTYPE html>") {
			t.Fatalf("GET %s served admin HTML from the v1 surface", path)
		}
		if rec.Header().Get("WWW-Authenticate") != "" {
			// A Basic challenge would mean the request reached the admin gate.
			t.Fatalf("GET %s was routed to the admin gate (Basic challenge)", path)
		}
	}

	if got := mock.GetConfig().SignalingURL; got != before {
		t.Fatalf("v1 paths mutated admin settings: %q, want %q", got, before)
	}
}
