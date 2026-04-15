package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"sharebridge/agent/internal/config"
	"sharebridge/agent/internal/daemon"
)

// mockDaemonMiddleware satisfies daemonProvider for middleware tests.
type mockDaemonMiddleware struct {
	cfg *config.Config
}

func (m *mockDaemonMiddleware) ListSessions() []*daemon.Session              { return nil }
func (m *mockDaemonMiddleware) GetSession(string) *daemon.Session            { return nil }
func (m *mockDaemonMiddleware) GetConfig() *config.Config                    { return m.cfg }
func (m *mockDaemonMiddleware) CreateSession(_ context.Context, _, _, _ string, _ time.Duration, _ int, _ bool) (string, error) {
	return "", nil
}
func (m *mockDaemonMiddleware) RevokeSession(string) error              { return nil }
func (m *mockDaemonMiddleware) HasTURN() bool                           { return false }
func (m *mockDaemonMiddleware) IsConnected() bool                       { return false }
func (m *mockDaemonMiddleware) GetUptime() time.Duration                { return 0 }
func (m *mockDaemonMiddleware) GetConfigPath() string                   { return "" }
func (m *mockDaemonMiddleware) SaveConfig(*config.Config) error         { return nil }

func newMiddlewareTestServer(cfg *config.Config) *WebServer {
	return &WebServer{daemon: &mockDaemonMiddleware{cfg: cfg}}
}

func TestAPIKeyMiddleware_RejectsMissingKey(t *testing.T) {
	cfg := &config.Config{AgentAPIKey: "sb_agent_testkey"}
	ws := newMiddlewareTestServer(cfg)

	handler := ws.apiKeyMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/api/v1/shares", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestAPIKeyMiddleware_RejectsWrongKey(t *testing.T) {
	cfg := &config.Config{AgentAPIKey: "sb_agent_correctkey"}
	ws := newMiddlewareTestServer(cfg)

	handler := ws.apiKeyMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/api/v1/shares", nil)
	req.Header.Set("X-API-Key", "wrong-key")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestAPIKeyMiddleware_AcceptsCorrectKey(t *testing.T) {
	cfg := &config.Config{AgentAPIKey: "sb_agent_correctkey"}
	ws := newMiddlewareTestServer(cfg)

	handler := ws.apiKeyMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/api/v1/shares", nil)
	req.Header.Set("X-API-Key", "sb_agent_correctkey")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestCORSMiddleware_SetsOriginHeader(t *testing.T) {
	cfg := &config.Config{AllowedHost: "opencloud.example.com", AgentAPIKey: "key"}
	ws := newMiddlewareTestServer(cfg)

	handler := ws.corsMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/api/v1/shares", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	origin := rec.Header().Get("Access-Control-Allow-Origin")
	if origin != "https://opencloud.example.com" {
		t.Errorf("CORS origin = %q, want https://opencloud.example.com", origin)
	}
}

func TestCORSMiddleware_HandlesOptionsPreflight(t *testing.T) {
	cfg := &config.Config{AllowedHost: "opencloud.example.com", AgentAPIKey: "key"}
	ws := newMiddlewareTestServer(cfg)

	handler := ws.corsMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // should NOT be reached
	})

	req := httptest.NewRequest("OPTIONS", "/api/v1/shares", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("OPTIONS preflight: expected 204, got %d", rec.Code)
	}
}

func TestCORSMiddleware_Returns503WhenAllowedHostEmpty(t *testing.T) {
	cfg := &config.Config{AllowedHost: "", AgentAPIKey: "key"}
	ws := newMiddlewareTestServer(cfg)

	handler := ws.corsMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/api/v1/shares", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when AllowedHost empty, got %d", rec.Code)
	}
}

func TestV1Chain_CORSHeadersPresentOn401(t *testing.T) {
	// Verifies CORS headers are set even when auth fails.
	// Browsers inspect CORS headers on all responses including error ones.
	cfg := &config.Config{AllowedHost: "opencloud.example.com", AgentAPIKey: "sb_agent_correct"}
	ws := newMiddlewareTestServer(cfg)

	chain := ws.corsMiddleware(ws.apiKeyMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/v1/shares", nil)
	req.Header.Set("X-API-Key", "wrong-key")
	rec := httptest.NewRecorder()
	chain(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
	origin := rec.Header().Get("Access-Control-Allow-Origin")
	if origin != "https://opencloud.example.com" {
		t.Errorf("CORS origin missing on 401 response: got %q", origin)
	}
}