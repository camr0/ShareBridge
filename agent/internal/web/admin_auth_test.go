package web

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sharebridge/agent/internal/config"
)

const adminTestPassword = "s3cret-admin"

// newAdminServer builds a WebServer through the production constructor (so the
// layout template and the fail-closed validation are exercised) with a mock
// daemon that also implements the lockdown capability.
func newAdminServer(t *testing.T, addr, password string, cfg *config.Config) (*WebServer, *lockdownDaemonMock) {
	t.Helper()
	if cfg == nil {
		cfg = &config.Config{SignalingURL: "wss://share.example.com", AllowedHost: "opencloud.example.com", AgentAPIKey: "sb_agent_key", APIKey: "ocs_control_key", UIPort: 7878}
	}
	mock := &lockdownDaemonMock{mockDaemonV1: newMockDaemonV1(cfg)}
	ws, err := NewWebServer(mock, addr, 0, password)
	if err != nil {
		t.Fatalf("NewWebServer(%q, password=%q): %v", addr, password, err)
	}
	return ws, mock
}

func TestNewWebServerRefusesNonLoopbackWithoutPassword(t *testing.T) {
	ws, err := NewWebServer(nil, "0.0.0.0", 7878, "")
	if err == nil {
		t.Fatalf("NewWebServer(0.0.0.0, no password) = nil error, want refusal; server=%v", ws)
	}
	if ws != nil {
		t.Fatalf("NewWebServer must not return a server on refusal, got %+v", ws)
	}
	for _, want := range []string{"UI_ADDR", "UI_PASSWORD"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal error %q must mention %s", err, want)
		}
	}
}

func TestNewWebServerAcceptsNonLoopbackWithPassword(t *testing.T) {
	ws, err := NewWebServer(nil, "0.0.0.0", 7878, adminTestPassword)
	if err != nil {
		t.Fatalf("NewWebServer(0.0.0.0, password) = %v, want nil", err)
	}
	if ws == nil {
		t.Fatal("NewWebServer returned nil server for a valid non-loopback bind with password")
	}
}

// TestNewWebServerDefaultsEmptyAddrToLoopback pins the secure normalization:
// an unset address must never become "all interfaces" (":port").
func TestNewWebServerDefaultsEmptyAddrToLoopback(t *testing.T) {
	ws, err := NewWebServer(nil, "", 7878, "")
	if err != nil {
		t.Fatalf("NewWebServer(\"\", no password) = %v, want nil (empty addr is the loopback default)", err)
	}
	if ws.addr != "127.0.0.1" {
		t.Fatalf("empty addr normalized to %q, want 127.0.0.1", ws.addr)
	}
}

// TestStartRefusesNonLoopbackWithoutPasswordBeforeListening pins that the
// refusal happens before any listener is created, even when a WebServer is
// constructed directly (bypassing the constructor).
func TestStartRefusesNonLoopbackWithoutPasswordBeforeListening(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("close probe listener: %v", err)
	}

	ws := &WebServer{daemon: newMockDaemonV1(&config.Config{}), addr: "0.0.0.0", port: port, password: ""}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	startErr := make(chan error, 1)
	go func() { startErr <- ws.Start(ctx) }()

	select {
	case err := <-startErr:
		if err == nil {
			t.Fatal("Start on non-loopback without password returned nil, want refusal error")
		}
		for _, want := range []string{"UI_ADDR", "UI_PASSWORD"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("startup error %q must mention %s", err, want)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not refuse immediately: a listener may have been started")
	}

	// Nothing may be accepting connections on the port after the refusal.
	conn, dialErr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
	if dialErr == nil {
		conn.Close()
		t.Fatalf("a listener accepted a connection on port %d despite the validation refusal", port)
	}
}

// adminEndpoints is every dynamic admin surface reachable from the browser UI
// (pages, HTML fragments, JSON APIs and mutations).
var adminEndpoints = []struct {
	method string
	path   string
	body   string
}{
	{http.MethodGet, "/", ""},
	{http.MethodGet, "/settings", ""},
	{http.MethodGet, "/history", ""},
	{http.MethodGet, "/api/shares", ""},
	{http.MethodPost, "/api/shares", "share_type=opencloud&share_url=https%3A%2F%2Fexample.com%2Fshare"},
	{http.MethodDelete, "/api/shares/abc", ""},
	{http.MethodGet, "/api/share-form", ""},
	{http.MethodGet, "/api/status", ""},
	{http.MethodGet, "/api/turn-status", ""},
	{http.MethodPut, "/api/settings", "signaling_url=wss%3A%2F%2Fevil.example"},
	{http.MethodPost, "/api/lockdown", ""},
	{http.MethodPost, "/api/unlock", ""},
	{http.MethodGet, "/api/lockdown-status", ""},
	{http.MethodGet, "/api/relay-quota", ""},
	{http.MethodGet, "/api/quota-widget", ""},
	{http.MethodGet, "/api/quota-inline", ""},
	{http.MethodGet, "/api/settings/secrets", ""},
	{http.MethodGet, "/api/v1/shares", ""},
	// Wrong/alternate methods must not slip past the auth layer either.
	{http.MethodPatch, "/api/shares", ""},
	{http.MethodHead, "/api/settings", ""},
}

// TestAdminSurfaceRequiresAuthWhenPasswordConfigured proves every admin
// endpoint rejects an unauthenticated caller when a password is configured,
// even when the caller forges the CSRF browser headers, and that no state
// changes as a result.
func TestAdminSurfaceRequiresAuthWhenPasswordConfigured(t *testing.T) {
	ws, mock := newAdminServer(t, "127.0.0.1", adminTestPassword, nil)
	handler := ws.handler()

	beforeSignaling := mock.GetConfig().SignalingURL

	for _, ep := range adminEndpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			req := httptest.NewRequest(ep.method, ep.path, strings.NewReader(ep.body))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			// Forged browser headers: a forgeable CSRF header must not authenticate.
			req.Header.Set("HX-Request", "true")
			req.Header.Set("X-Requested-With", "XMLHttpRequest")
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated %s %s = %d, want 401: %s", ep.method, ep.path, rec.Code, rec.Body.String())
			}
		})
	}

	if len(mock.sessions) != 0 {
		t.Fatalf("unauthenticated requests created %d sessions, want 0", len(mock.sessions))
	}
	if mock.lockdownCalls != 0 || mock.unlockCalls != 0 {
		t.Fatalf("unauthenticated requests reached lockdown: lock=%d unlock=%d", mock.lockdownCalls, mock.unlockCalls)
	}
	if mock.GetConfig().SignalingURL != beforeSignaling {
		t.Fatalf("unauthenticated request mutated settings: SignalingURL=%q, want %q",
			mock.GetConfig().SignalingURL, beforeSignaling)
	}
}

// TestAdminSurfaceRejectsWrongBasicCredentials pins that a wrong (or empty)
// credential is rejected, not merely the absence of one.
func TestAdminSurfaceRejectsWrongBasicCredentials(t *testing.T) {
	ws, _ := newAdminServer(t, "127.0.0.1", adminTestPassword, nil)
	handler := ws.handler()

	for _, creds := range [][2]string{{"admin", "wrong"}, {"", ""}, {"admin", adminTestPassword + "x"}} {
		req := httptest.NewRequest(http.MethodGet, "/settings", nil)
		req.SetBasicAuth(creds[0], creds[1])
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("GET /settings with creds %q/%q = %d, want 401", creds[0], creds[1], rec.Code)
		}
	}
}

// TestAdminSurfaceAllowsAuthenticatedRequests proves the credential path still
// works end to end: pages render and mutations reach the daemon.
func TestAdminSurfaceAllowsAuthenticatedRequests(t *testing.T) {
	ws, mock := newAdminServer(t, "127.0.0.1", adminTestPassword, nil)
	handler := ws.handler()

	// Page.
	rec := doAdminReq(t, handler, http.MethodGet, "/settings", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated GET /settings = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// Share creation.
	rec = doAdminReq(t, handler, http.MethodPost, "/api/shares",
		"share_type=opencloud&share_url=https%3A%2F%2Fexample.com%2Fshare", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated POST /api/shares = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(mock.sessions) != 1 {
		t.Fatalf("authenticated POST /api/shares created %d sessions, want 1", len(mock.sessions))
	}

	// Settings mutation.
	rec = doAdminReq(t, handler, http.MethodPut, "/api/settings", "signaling_url=wss%3A%2F%2Fnew.example.com", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated PUT /api/settings = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if mock.GetConfig().SignalingURL != "wss://new.example.com" {
		t.Fatalf("authenticated PUT /api/settings did not persist: %q", mock.GetConfig().SignalingURL)
	}

	// Lockdown mutation.
	rec = doAdminReq(t, handler, http.MethodPost, "/api/lockdown", "", true)
	if rec.Code != http.StatusOK || !mock.locked {
		t.Fatalf("authenticated POST /api/lockdown = %d locked=%v, want 200 true", rec.Code, mock.locked)
	}
}

// TestLoopbackWithoutPasswordKeepsLocalAdminUX proves local trust is preserved:
// with the loopback default and no password, pages and mutations work.
func TestLoopbackWithoutPasswordKeepsLocalAdminUX(t *testing.T) {
	ws, mock := newAdminServer(t, "127.0.0.1", "", nil)
	handler := ws.handler()

	rec := doAdminReq(t, handler, http.MethodGet, "/settings", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("loopback GET /settings = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	rec = doAdminReq(t, handler, http.MethodPost, "/api/shares",
		"share_type=opencloud&share_url=https%3A%2F%2Fexample.com%2Fshare&expiry_hours=24&max_downloads=5", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("loopback POST /api/shares = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(mock.sessions) != 1 {
		t.Fatalf("loopback POST /api/shares created %d sessions, want 1", len(mock.sessions))
	}

	rec = doAdminReq(t, handler, http.MethodPut, "/api/settings", "signaling_url=wss%3A%2F%2Flocal.example.com", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("loopback PUT /api/settings = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if mock.GetConfig().SignalingURL != "wss://local.example.com" {
		t.Fatalf("loopback PUT /api/settings did not persist: %q", mock.GetConfig().SignalingURL)
	}
}

// TestLoopbackWithoutPasswordRejectsCrossOriginMutations pins the same-origin
// enforcement that replaces sole reliance on the forgeable HX-Request header.
func TestLoopbackWithoutPasswordRejectsCrossOriginMutations(t *testing.T) {
	ws, mock := newAdminServer(t, "127.0.0.1", "", nil)
	handler := ws.handler()

	beforeSignaling := mock.GetConfig().SignalingURL

	crossOrigin := []string{
		"https://evil.example",
		"http://attacker.test",
		"null",
	}
	for _, origin := range crossOrigin {
		req := httptest.NewRequest(http.MethodPut, "http://127.0.0.1:7878/api/settings",
			strings.NewReader("signaling_url=wss%3A%2F%2Fevil.example"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("cross-origin Origin %q PUT /api/settings = %d, want 403", origin, rec.Code)
		}
	}

	// Cross-origin share creation must be blocked with no state change.
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7878/api/shares",
		strings.NewReader("share_type=opencloud&share_url=https%3A%2F%2Fexample.com%2Fshare"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST /api/shares = %d, want 403", rec.Code)
	}

	if len(mock.sessions) != 0 {
		t.Fatalf("cross-origin mutation created %d sessions, want 0", len(mock.sessions))
	}
	if mock.GetConfig().SignalingURL != beforeSignaling {
		t.Fatalf("cross-origin mutation changed settings: %q", mock.GetConfig().SignalingURL)
	}
}

// TestLoopbackSameOriginMutationAllowed proves a same-origin browser mutation
// (Origin header equals the request Host) still works.
func TestLoopbackSameOriginMutationAllowed(t *testing.T) {
	ws, mock := newAdminServer(t, "127.0.0.1", "", nil)
	handler := ws.handler()

	req := httptest.NewRequest(http.MethodPut, "http://127.0.0.1:7878/api/settings",
		strings.NewReader("signaling_url=wss%3A%2F%2Fsame-origin.example.com"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("Origin", "http://127.0.0.1:7878")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("same-origin PUT /api/settings = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if mock.GetConfig().SignalingURL != "wss://same-origin.example.com" {
		t.Fatalf("same-origin PUT /api/settings did not persist: %q", mock.GetConfig().SignalingURL)
	}
}

// TestNonLoopbackWithoutPasswordFailsClosedAtRequestPath pins defense in depth:
// even a directly-constructed server (bypassing the constructor check) must
// refuse admin requests rather than serve them unauthenticated.
func TestNonLoopbackWithoutPasswordFailsClosedAtRequestPath(t *testing.T) {
	mock := &lockdownDaemonMock{mockDaemonV1: newMockDaemonV1(&config.Config{SignalingURL: "wss://share.example.com"})}
	ws := &WebServer{daemon: mock, addr: "0.0.0.0", password: ""}
	handler := ws.handler()

	rec := doAdminReq(t, handler, http.MethodGet, "/settings", "", false)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-loopback GET /settings without password = %d, want 403", rec.Code)
	}

	rec = doAdminReq(t, handler, http.MethodPost, "/api/shares",
		"share_type=opencloud&share_url=https%3A%2F%2Fexample.com%2Fshare", false)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-loopback POST /api/shares without password = %d, want 403", rec.Code)
	}
	if len(mock.sessions) != 0 {
		t.Fatalf("non-loopback unauthenticated mutation created %d sessions, want 0", len(mock.sessions))
	}
}

// TestSettingsPageNeverLeaksSecrets is the end-to-end proof that the settings
// page does not disclose the control API key or the agent API key to an
// unauthenticated caller, and does not embed raw secrets in the HTML even for
// an authenticated caller (they are revealed on demand).
func TestSettingsPageNeverLeaksSecrets(t *testing.T) {
	const (
		controlKey = "ocs_control_supersecret_value"
		agentKey   = "sb_agent_supersecret_value"
	)
	cfg := &config.Config{
		SignalingURL: "wss://share.example.com",
		APIKey:       controlKey,
		AgentAPIKey:  agentKey,
		UIPort:       7878,
	}
	ws, _ := newAdminServer(t, "127.0.0.1", adminTestPassword, cfg)
	handler := ws.handler()

	assertNoSecrets := func(t *testing.T, body string) {
		t.Helper()
		if strings.Contains(body, controlKey) {
			t.Errorf("response leaked the control API key")
		}
		if strings.Contains(body, agentKey) {
			t.Errorf("response leaked the agent API key")
		}
	}

	// Unauthenticated page request: rejected and no secret in the body.
	rec := doAdminReq(t, handler, http.MethodGet, "/settings", "", false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /settings = %d, want 401", rec.Code)
	}
	assertNoSecrets(t, rec.Body.String())

	// Authenticated page request: renders, but masks the secrets.
	rec = doAdminReq(t, handler, http.MethodGet, "/settings", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated GET /settings = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	assertNoSecrets(t, rec.Body.String())
	if !strings.Contains(rec.Body.String(), maskedSecret) {
		t.Errorf("authenticated settings page must render the masked placeholder %q", maskedSecret)
	}

	// The reveal endpoint is itself an admin surface: unauthenticated callers
	// get nothing.
	rec = doAdminReq(t, handler, http.MethodGet, "/api/settings/secrets", "", false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /api/settings/secrets = %d, want 401", rec.Code)
	}
	assertNoSecrets(t, rec.Body.String())

	// Authenticated reveal returns the values (explicit user action only).
	rec = doAdminReq(t, handler, http.MethodGet, "/api/settings/secrets", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated GET /api/settings/secrets = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), controlKey) || !strings.Contains(rec.Body.String(), agentKey) {
		t.Fatalf("authenticated reveal must return both keys, got %s", rec.Body.String())
	}
}

// TestSettingsSavePreservesMaskedKey pins that submitting the masked
// placeholder (the untouched form) does not overwrite the stored key.
func TestSettingsSavePreservesMaskedKey(t *testing.T) {
	cfg := &config.Config{SignalingURL: "wss://share.example.com", APIKey: "ocs_original_key"}
	ws, mock := newAdminServer(t, "127.0.0.1", adminTestPassword, cfg)
	handler := ws.handler()

	body := "signaling_url=wss%3A%2F%2Fshare.example.com&api_key=" + maskedSecret
	rec := doAdminReq(t, handler, http.MethodPut, "/api/settings", body, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/settings = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := mock.GetConfig().APIKey; got != "ocs_original_key" {
		t.Fatalf("submitting the mask changed the stored API key: %q", got)
	}

	// An explicit replacement value still applies.
	rec = doAdminReq(t, handler, http.MethodPut, "/api/settings",
		"signaling_url=wss%3A%2F%2Fshare.example.com&api_key=ocs_rotated_key", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/settings (rotate) = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := mock.GetConfig().APIKey; got != "ocs_rotated_key" {
		t.Fatalf("explicit API key rotation not applied: %q", got)
	}
}

// doAdminReq issues a request through the full handler chain, optionally with
// the admin credential. Browser CSRF headers are always set so each test
// isolates the auth/origin behavior.
func doAdminReq(t *testing.T, handler http.Handler, method, path, body string, authenticated bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("HX-Request", "true")
	if authenticated {
		req.SetBasicAuth("admin", adminTestPassword)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}
