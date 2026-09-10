package web

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"sharebridge/agent/internal/config"
)

// TestShareFormUsesAlwaysUseRelayCopy pins the §6.1 wording: the relay mode
// is presented with the exact non-anonymity claim “Always use relay for this
// share” in the share form and in the default-mode settings page, and the old
// “Relay (recommended)” / “hides your IP” anonymity-promising copy is gone
// wherever the share/default mode is presented.
func TestShareFormUsesAlwaysUseRelayCopy(t *testing.T) {
	cfg := &config.Config{DefaultRelayOnly: true}
	ws, _ := newV1TestServer(cfg)

	req := httptest.NewRequest(http.MethodGet, "/api/share-form", nil)
	rec := httptest.NewRecorder()
	ws.shareFormHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	form := rec.Body.String()
	if !strings.Contains(form, "Always use relay for this share") {
		t.Errorf("share form must present the exact §6.1 copy %q", "Always use relay for this share")
	}
	for _, banned := range []string{"Relay (recommended)", "hides your IP"} {
		if strings.Contains(form, banned) {
			t.Errorf("share form must not contain %q (must not promise anonymity, §6.1)", banned)
		}
	}

	// The settings page presents the same exact copy for the default mode.
	layout, err := template.ParseFS(embeddedFS, "templates/layout.html")
	if err != nil {
		t.Fatalf("parse layout: %v", err)
	}
	settingsWS, _ := newV1TestServer(cfg)
	settingsWS.layoutTmpl = layout

	settingsRec := httptest.NewRecorder()
	settingsWS.settingsHandler(settingsRec, httptest.NewRequest(http.MethodGet, "/settings", nil))

	if settingsRec.Code != http.StatusOK {
		t.Fatalf("settings status = %d: %s", settingsRec.Code, settingsRec.Body.String())
	}
	settings := settingsRec.Body.String()
	if !strings.Contains(settings, "Always use relay for this share") {
		t.Errorf("settings page must present the exact §6.1 copy for the default mode")
	}
	if strings.Contains(settings, "Relay (recommended)") {
		t.Errorf("settings page must not contain %q (must not promise anonymity, §6.1)", "Relay (recommended)")
	}
}

// TestShareForm_RelayModeSelectedByDefault pins the share-form mode radio
// defaults: relay selected (and listed first) when DefaultRelayOnly is set,
// with the §6.1 non-anonymity copy and the direct-mode warning intact.
func TestShareForm_RelayModeSelectedByDefault(t *testing.T) {
	cfg := &config.Config{DefaultRelayOnly: true}
	ws, _ := newV1TestServer(cfg)

	req := httptest.NewRequest(http.MethodGet, "/api/share-form", nil)
	rec := httptest.NewRecorder()

	ws.shareFormHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	relayIndex := strings.Index(body, "Always use relay for this share")
	directIndex := strings.Index(body, `>Direct<`)
	if relayIndex == -1 || directIndex == -1 || relayIndex >= directIndex {
		t.Errorf("relay mode must appear before Direct")
	}
	assertRadioChecked(t, body, "mode-relay", "true", true)
	assertRadioChecked(t, body, "mode-direct", "false", false)
	assertRadioLabelledBy(t, body, "mode-relay", "mode-relay-title", "Always use relay for this share")
	assertRadioLabelledBy(t, body, "mode-direct", "mode-direct-title", "Direct")

	for _, want := range []string{
		"End-to-end encrypted, and provides consistent performance.",
		"Peer-to-peer, quota-free.",
		"Direct transfers expose your IP address and may be slower due to browser protocol limitations. Use Relay for more consistent performance.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	for _, old := range []string{"TURN", "Fast, free", "Requires TURN server", "Relay (recommended)", "hides your IP"} {
		if strings.Contains(body, old) {
			t.Errorf("body contains obsolete copy %q", old)
		}
	}
}

func TestShareForm_PreservesSavedDirectDefault(t *testing.T) {
	cfg := &config.Config{DefaultRelayOnly: false}
	ws, _ := newV1TestServer(cfg)

	req := httptest.NewRequest(http.MethodGet, "/api/share-form", nil)
	rec := httptest.NewRecorder()

	ws.shareFormHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	assertRadioChecked(t, rec.Body.String(), "mode-direct", "false", true)
	assertRadioChecked(t, rec.Body.String(), "mode-relay", "true", false)
}

func assertRadioChecked(t *testing.T, body, id, value string, wantChecked bool) {
	t.Helper()

	input := findRadioInput(t, body, id, value)
	if got := strings.Contains(input, "checked"); got != wantChecked {
		t.Errorf("radio id=%q checked = %t, want %t: %s", id, got, wantChecked, input)
	}
}

func assertRadioLabelledBy(t *testing.T, body, id, titleID, title string) {
	t.Helper()

	input := findRadioInput(t, body, id, "")
	if !strings.Contains(input, `aria-labelledby="`+titleID+`"`) {
		t.Errorf("radio id=%q missing aria-labelledby=%q: %s", id, titleID, input)
	}
	titlePattern := regexp.MustCompile(`<[^>]+id="` + regexp.QuoteMeta(titleID) + `"[^>]*>\s*` + regexp.QuoteMeta(title) + `\s*</[^>]+>`)
	if !titlePattern.MatchString(body) {
		t.Errorf("title id=%q with text %q not found", titleID, title)
	}
}

func findRadioInput(t *testing.T, body, id, value string) string {
	t.Helper()

	pattern := `<input\s+[^>]*id="` + regexp.QuoteMeta(id) + `"[^>]*`
	if value != "" {
		pattern += `value="` + regexp.QuoteMeta(value) + `"[^>]*`
	}
	input := regexp.MustCompile(pattern + `>`).FindString(body)
	if input == "" {
		t.Fatalf("radio id=%q not found", id)
	}
	return input
}

func TestCreateShareForm_RequiresShareType(t *testing.T) {
	cfg := &config.Config{AgentAPIKey: "key", DefaultExpiry: 24}
	ws, _ := newV1TestServer(cfg)

	formData := url.Values{}
	formData.Set("share_url", "https://oc.example.com/s/xyz")
	// No share_type provided

	req := httptest.NewRequest("POST", "/api/shares", strings.NewReader(formData.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	ws.createShareHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "share_type is required") {
		t.Errorf("body = %q, want 'share_type is required' message", rec.Body.String())
	}
}

func TestCreateShareForm_RejectsInvalidShareType(t *testing.T) {
	cfg := &config.Config{AgentAPIKey: "key", DefaultExpiry: 24}
	ws, _ := newV1TestServer(cfg)

	formData := url.Values{}
	formData.Set("share_url", "https://oc.example.com/s/xyz")
	formData.Set("share_type", "invalid")

	req := httptest.NewRequest("POST", "/api/shares", strings.NewReader(formData.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	ws.createShareHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "share_type must be") {
		t.Errorf("body = %q, want 'share_type must be' message", rec.Body.String())
	}
}

func TestCreateShareForm_AcceptsOpencloudShareType(t *testing.T) {
	cfg := &config.Config{
		AgentAPIKey:   "key",
		SignalingURL:  "wss://share.example.com",
		DefaultExpiry: 24,
	}
	ws, mock := newV1TestServer(cfg)

	formData := url.Values{}
	formData.Set("share_url", "https://oc.example.com/s/xyz")
	formData.Set("share_type", "opencloud")

	req := httptest.NewRequest("POST", "/api/shares", strings.NewReader(formData.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	ws.createShareHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// Verify session was created
	session := mock.GetSession("code-1")
	if session == nil {
		t.Fatal("session not created")
	}
}

func TestCreateShareForm_AcceptsNextcloudShareType(t *testing.T) {
	cfg := &config.Config{
		AgentAPIKey:   "key",
		SignalingURL:  "wss://share.example.com",
		DefaultExpiry: 24,
	}
	ws, mock := newV1TestServer(cfg)

	formData := url.Values{}
	formData.Set("share_url", "https://nc.example.com/s/xyz")
	formData.Set("share_type", "nextcloud")

	req := httptest.NewRequest("POST", "/api/shares", strings.NewReader(formData.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	ws.createShareHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// Verify session was created
	session := mock.GetSession("code-1")
	if session == nil {
		t.Fatal("session not created")
	}
}

func TestCreateShareForm_AcceptsImmichShareTypeWhenConfigured(t *testing.T) {
	cfg := &config.Config{
		AgentAPIKey:       "key",
		SignalingURL:      "wss://share.example.com",
		DefaultExpiry:     24,
		ImmichURL:         "http://immich.lan:2283",
		ImmichAllowedHost: "immich.lan:2283",
		ImmichAPIKey:      "api",
	}
	ws, mock := newV1TestServer(cfg)

	formData := url.Values{}
	formData.Set("share_url", "immich://KEY")
	formData.Set("share_type", "immich")

	req := httptest.NewRequest("POST", "/api/shares", strings.NewReader(formData.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	ws.createShareHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	session := mock.GetSession("code-1")
	if session == nil {
		t.Fatal("session not created")
	}
}

func TestCreateShareForm_RejectsImmichShareTypeWhenMissingConfig(t *testing.T) {
	cfg := &config.Config{
		AgentAPIKey:   "key",
		SignalingURL:  "wss://share.example.com",
		DefaultExpiry: 24,
	}
	ws, _ := newV1TestServer(cfg)

	formData := url.Values{}
	formData.Set("share_url", "immich://KEY")
	formData.Set("share_type", "immich")

	req := httptest.NewRequest("POST", "/api/shares", strings.NewReader(formData.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	ws.createShareHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "immich is not configured") {
		t.Errorf("body = %q, want 'immich is not configured' message", rec.Body.String())
	}
}

func TestCreateShareForm_ReturnsBadRequestForManualImmichURLValidationError(t *testing.T) {
	cfg := &config.Config{
		AgentAPIKey:       "key",
		SignalingURL:      "wss://share.example.com",
		DefaultExpiry:     24,
		ImmichURL:         "http://immich.lan:2283",
		ImmichAllowedHost: "immich.lan:2283",
		ImmichAPIKey:      "api",
	}
	ws, mock := newV1TestServer(cfg)
	mock.createSessionErr = testClientValidationError{message: "share_url must be immich://KEY for manual Immich shares"}

	formData := url.Values{}
	formData.Set("share_url", "https://example.com/share")
	formData.Set("share_type", "immich")

	req := httptest.NewRequest("POST", "/api/shares", strings.NewReader(formData.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	ws.createShareHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "share_url must be immich://KEY for manual Immich shares") {
		t.Errorf("body = %q, want manual Immich URL validation message", rec.Body.String())
	}
}

// lockdownDaemonMock adds the optional §13.4 lockdown capability on top of the
// shared admin-API mock.
type lockdownDaemonMock struct {
	*mockDaemonV1
	locked        bool
	lockdownCalls int
	unlockCalls   int
	lockdownErr   error
	unlockErr     error
}

func (m *lockdownDaemonMock) Lockdown() error {
	m.lockdownCalls++
	if m.lockdownErr != nil {
		return m.lockdownErr
	}
	m.locked = true
	return nil
}

func (m *lockdownDaemonMock) Unlock() error {
	m.unlockCalls++
	if m.unlockErr != nil {
		return m.unlockErr
	}
	m.locked = false
	return nil
}

func (m *lockdownDaemonMock) IsLocked() bool { return m.locked }

// TestLockdownEndpointsAreRegisteredAuthenticatedAndReversible pins the §13.4
// admin-API surface: the routes exist on the real mux, the state-changing
// POSTs require the CSRF header, lock/unlock toggle the daemon state, and a
// daemon without the capability fails closed instead of silently succeeding.
func TestLockdownEndpointsAreRegisteredAuthenticatedAndReversible(t *testing.T) {
	cfg := &config.Config{}
	mock := &lockdownDaemonMock{mockDaemonV1: newMockDaemonV1(cfg)}
	ws := &WebServer{daemon: mock}

	// The endpoints are reachable through the real route table.
	mux := http.NewServeMux()
	ws.registerRoutes(mux)
	statusReq := httptest.NewRequest(http.MethodGet, "/api/lockdown-status", nil)
	statusRec := httptest.NewRecorder()
	mux.ServeHTTP(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("GET /api/lockdown-status status = %d, want 200: %s", statusRec.Code, statusRec.Body.String())
	}
	if !strings.Contains(statusRec.Body.String(), `"locked":false`) {
		t.Fatalf("status body = %q, want {\"locked\":false}", statusRec.Body.String())
	}

	// A POST without the CSRF header is rejected before the daemon is touched.
	noCSRF := httptest.NewRequest(http.MethodPost, "/api/lockdown", nil)
	noCSRFRec := httptest.NewRecorder()
	ws.csrfMiddleware(ws.lockdownHandler)(noCSRFRec, noCSRF)
	if noCSRFRec.Code != http.StatusForbidden {
		t.Fatalf("lockdown without CSRF = %d, want 403", noCSRFRec.Code)
	}
	if mock.lockdownCalls != 0 {
		t.Fatalf("CSRF-rejected lockdown must not reach the daemon")
	}

	// A CSRF-bearing POST locks the daemon and reports the new state.
	lockReq := httptest.NewRequest(http.MethodPost, "/api/lockdown", nil)
	lockReq.Header.Set("HX-Request", "true")
	lockRec := httptest.NewRecorder()
	ws.csrfMiddleware(ws.lockdownHandler)(lockRec, lockReq)
	if lockRec.Code != http.StatusOK || !mock.locked {
		t.Fatalf("lockdown = %d locked=%v, want 200 locked=true: %s", lockRec.Code, mock.locked, lockRec.Body.String())
	}
	if !strings.Contains(lockRec.Body.String(), `"locked":true`) {
		t.Fatalf("lockdown body = %q, want {\"locked\":true}", lockRec.Body.String())
	}

	// Unlock reverses it through the same authenticated path.
	unlockReq := httptest.NewRequest(http.MethodPost, "/api/unlock", nil)
	unlockReq.Header.Set("X-Requested-With", "XMLHttpRequest")
	unlockRec := httptest.NewRecorder()
	ws.csrfMiddleware(ws.unlockHandler)(unlockRec, unlockReq)
	if unlockRec.Code != http.StatusOK || mock.locked {
		t.Fatalf("unlock = %d locked=%v, want 200 locked=false: %s", unlockRec.Code, mock.locked, unlockRec.Body.String())
	}
	if mock.lockdownCalls != 1 || mock.unlockCalls != 1 {
		t.Fatalf("daemon calls = lock %d unlock %d, want 1/1", mock.lockdownCalls, mock.unlockCalls)
	}

	// A daemon without the capability fails closed (501), never a silent 200.
	plain := &WebServer{daemon: newMockDaemonV1(cfg)}
	plainReq := httptest.NewRequest(http.MethodPost, "/api/lockdown", nil)
	plainReq.Header.Set("HX-Request", "true")
	plainRec := httptest.NewRecorder()
	plain.csrfMiddleware(plain.lockdownHandler)(plainRec, plainReq)
	if plainRec.Code != http.StatusNotImplemented {
		t.Fatalf("unsupported lockdown = %d, want 501", plainRec.Code)
	}
}
