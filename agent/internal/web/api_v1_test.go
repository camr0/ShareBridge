package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"sharebridge/agent/internal/config"
	"sharebridge/agent/internal/daemon"
)

// mockDaemonV1 implements daemonProvider for v1 handler tests.
type mockDaemonV1 struct {
	mu       sync.Mutex
	cfg      *config.Config
	sessions map[string]*daemon.Session
	hasTURN  bool
}

func newMockDaemonV1(cfg *config.Config) *mockDaemonV1 {
	return &mockDaemonV1{cfg: cfg, sessions: make(map[string]*daemon.Session)}
}

func (m *mockDaemonV1) addSession(code, shareURL, fileID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[code] = &daemon.Session{
		Code:      code,
		ShareURL:  shareURL,
		FileID:    fileID,
		ExpiresAt: time.Now().Add(24 * time.Hour),
		CreatedAt: time.Now(),
	}
}

func (m *mockDaemonV1) ListSessions() []*daemon.Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]*daemon.Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		result = append(result, s)
	}
	return result
}

func (m *mockDaemonV1) GetSession(code string) *daemon.Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[code]
}

func (m *mockDaemonV1) GetConfig() *config.Config { return m.cfg }

func (m *mockDaemonV1) CreateSession(_ context.Context, shareURL, _ string, expiry time.Duration, maxDownloads int, relayOnly bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	code := fmt.Sprintf("code-%d", len(m.sessions)+1)
	m.sessions[code] = &daemon.Session{
		Code:         code,
		ShareURL:     shareURL,
		FileID:       "storage-1$extracted!id",
		Downloads:    0,
		MaxDownloads: maxDownloads,
		RelayOnly:    relayOnly,
		ExpiresAt:    time.Now().Add(expiry),
		CreatedAt:    time.Now(),
	}
	return code, nil
}

func (m *mockDaemonV1) RevokeSession(code string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[code]; !ok {
		return fmt.Errorf("session %q not found", code)
	}
	delete(m.sessions, code)
	return nil
}

func (m *mockDaemonV1) HasTURN() bool                           { return m.hasTURN }
func (m *mockDaemonV1) IsConnected() bool                       { return true }
func (m *mockDaemonV1) GetUptime() time.Duration                { return time.Hour }
func (m *mockDaemonV1) GetConfigPath() string                   { return "" }
func (m *mockDaemonV1) SaveConfig(cfg *config.Config) error     { m.cfg = cfg; return nil }

func newV1TestServer(cfg *config.Config) (*WebServer, *mockDaemonV1) {
	mock := newMockDaemonV1(cfg)
	ws := &WebServer{daemon: mock}
	return ws, mock
}

func TestV1ListShares_ReturnsAllSessions(t *testing.T) {
	cfg := &config.Config{AgentAPIKey: "key", SignalingURL: "wss://share.example.com"}
	ws, mock := newV1TestServer(cfg)
	mock.addSession("abc", "https://oc.example.com/s/abc", "file-id-1")
	mock.addSession("def", "https://oc.example.com/s/def", "file-id-2")

	req := httptest.NewRequest("GET", "/api/v1/shares", nil)
	rec := httptest.NewRecorder()
	ws.v1ListSharesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var shares []v1ShareResponse
	if err := json.NewDecoder(rec.Body).Decode(&shares); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(shares) != 2 {
		t.Errorf("expected 2 shares, got %d", len(shares))
	}
}

func TestV1ListShares_FiltersByFileID(t *testing.T) {
	cfg := &config.Config{AgentAPIKey: "key", SignalingURL: "wss://share.example.com"}
	ws, mock := newV1TestServer(cfg)
	mock.addSession("abc", "https://oc.example.com/s/abc", "file-id-1")
	mock.addSession("def", "https://oc.example.com/s/def", "file-id-2")

	req := httptest.NewRequest("GET", "/api/v1/shares?file_id=file-id-1", nil)
	rec := httptest.NewRecorder()
	ws.v1ListSharesHandler(rec, req)

	var shares []v1ShareResponse
	json.NewDecoder(rec.Body).Decode(&shares)
	if len(shares) != 1 {
		t.Errorf("expected 1 share after filtering, got %d", len(shares))
	}
	if shares[0].FileID != "file-id-1" {
		t.Errorf("FileID = %q, want file-id-1", shares[0].FileID)
	}
}

func TestV1ListShares_ReturnsEmptyArray(t *testing.T) {
	cfg := &config.Config{AgentAPIKey: "key", SignalingURL: "wss://share.example.com"}
	ws, _ := newV1TestServer(cfg)

	req := httptest.NewRequest("GET", "/api/v1/shares", nil)
	rec := httptest.NewRecorder()
	ws.v1ListSharesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	// Must return [] not null
	if body == "null\n" {
		t.Errorf("expected [] for empty list, got null")
	}
}

func TestV1CreateShare_CreatesSession(t *testing.T) {
	cfg := &config.Config{
		AgentAPIKey:         "key",
		SignalingURL:        "wss://share.example.com",
		DefaultExpiry:       24,
		DefaultMaxDownloads: 10,
	}
	ws, _ := newV1TestServer(cfg)

	body := `{"share_url":"https://oc.example.com/s/xyz","share_type":"opencloud","expiry_hours":48,"max_downloads":5}`
	req := httptest.NewRequest("POST", "/api/v1/shares", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	ws.v1CreateShareHandler(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp v1CreateShareResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Code == "" {
		t.Errorf("expected non-empty Code")
	}
	if resp.PublicURL == "" {
		t.Errorf("expected non-empty PublicURL")
	}
}

func TestV1CreateShare_AcceptsNextcloudShareType(t *testing.T) {
	cfg := &config.Config{
		AgentAPIKey:         "key",
		SignalingURL:        "wss://share.example.com",
		DefaultExpiry:       24,
		DefaultMaxDownloads: 10,
	}
	ws, _ := newV1TestServer(cfg)

	body := `{"share_url":"https://nc.example.com/s/xyz","share_type":"nextcloud","expiry_hours":48}`
	req := httptest.NewRequest("POST", "/api/v1/shares", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	ws.v1CreateShareHandler(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestV1CreateShare_RequiresShareURL(t *testing.T) {
	cfg := &config.Config{AgentAPIKey: "key", DefaultExpiry: 24}
	ws, _ := newV1TestServer(cfg)

	req := httptest.NewRequest("POST", "/api/v1/shares", bytes.NewBufferString(`{}`))
	rec := httptest.NewRecorder()
	ws.v1CreateShareHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestV1CreateShare_RequiresShareType(t *testing.T) {
	cfg := &config.Config{AgentAPIKey: "key", DefaultExpiry: 24}
	ws, _ := newV1TestServer(cfg)

	body := `{"share_url":"https://oc.example.com/s/xyz"}`
	req := httptest.NewRequest("POST", "/api/v1/shares", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	ws.v1CreateShareHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}

	var resp v1ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Code != "BAD_REQUEST" {
		t.Errorf("error code = %q, want BAD_REQUEST", resp.Code)
	}
}

func TestV1CreateShare_RejectsInvalidShareType(t *testing.T) {
	cfg := &config.Config{AgentAPIKey: "key", DefaultExpiry: 24}
	ws, _ := newV1TestServer(cfg)

	body := `{"share_url":"https://oc.example.com/s/xyz","share_type":"invalid"}`
	req := httptest.NewRequest("POST", "/api/v1/shares", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	ws.v1CreateShareHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}

	var resp v1ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Code != "BAD_REQUEST" {
		t.Errorf("error code = %q, want BAD_REQUEST", resp.Code)
	}
}

func TestV1CreateShare_UsesDefaultExpiryWhenZero(t *testing.T) {
	cfg := &config.Config{
		AgentAPIKey:   "key",
		SignalingURL:  "wss://share.example.com",
		DefaultExpiry: 72, // 72 hours default
	}
	ws, mock := newV1TestServer(cfg)

	// Send expiry_hours: 0, should fall back to DefaultExpiry (72h)
	body := `{"share_url":"https://oc.example.com/s/xyz","share_type":"opencloud","expiry_hours":0}`
	req := httptest.NewRequest("POST", "/api/v1/shares", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	ws.v1CreateShareHandler(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify the session was created with 72h expiry (within tolerance)
	session := mock.GetSession("code-1")
	if session == nil {
		t.Fatal("session not created")
	}
	// Allow 1 minute tolerance for test execution time
	expectedExpiry := time.Now().Add(72 * time.Hour)
	if session.ExpiresAt.Before(expectedExpiry.Add(-time.Minute)) || session.ExpiresAt.After(expectedExpiry.Add(time.Minute)) {
		t.Errorf("ExpiresAt = %v, want ~%v (72h from now)", session.ExpiresAt, expectedExpiry)
	}
}

func TestV1RevokeShare_DeletesSession(t *testing.T) {
	cfg := &config.Config{AgentAPIKey: "key"}
	ws, mock := newV1TestServer(cfg)
	mock.addSession("to-revoke", "https://oc.example.com/s/x", "")

	req := httptest.NewRequest("DELETE", "/api/v1/shares/to-revoke", nil)
	req.SetPathValue("code", "to-revoke")
	rec := httptest.NewRecorder()
	ws.v1RevokeShareHandler(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d", rec.Code)
	}
	if mock.GetSession("to-revoke") != nil {
		t.Errorf("session still exists after revoke")
	}
}

func TestV1RevokeShare_NotFound(t *testing.T) {
	cfg := &config.Config{AgentAPIKey: "key"}
	ws, _ := newV1TestServer(cfg)

	req := httptest.NewRequest("DELETE", "/api/v1/shares/nonexistent", nil)
	req.SetPathValue("code", "nonexistent")
	rec := httptest.NewRecorder()
	ws.v1RevokeShareHandler(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestV1Settings_ReturnsDefaults(t *testing.T) {
	cfg := &config.Config{
		AgentAPIKey:         "key",
		DefaultExpiry:       48,
		DefaultMaxDownloads: 20,
		DefaultRelayOnly:    true,
	}
	ws, _ := newV1TestServer(cfg)

	req := httptest.NewRequest("GET", "/api/v1/settings", nil)
	rec := httptest.NewRecorder()
	ws.v1SettingsHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp v1SettingsResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.DefaultExpiryHours != 48 {
		t.Errorf("DefaultExpiryHours = %d, want 48", resp.DefaultExpiryHours)
	}
	if resp.DefaultMaxDownloads != 20 {
		t.Errorf("DefaultMaxDownloads = %d, want 20", resp.DefaultMaxDownloads)
	}
	if !resp.DefaultRelayOnly {
		t.Errorf("DefaultRelayOnly should be true")
	}
}