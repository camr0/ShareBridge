# Slice 11a+11b — Agent JSON API Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add API key auth, CORS, and JSON endpoints to the agent so the OpenCloud extension can create, list, and revoke shares; track OpenCloud `oc:fileid` per share session for file-specific filtering.

**Architecture:** 11a auto-generates a per-agent API key in config, adds CORS + API key middleware for new `/api/v1/` routes, and shows the key in the settings page. 11b adds `FileID` to `SessionEntry` and `Session`, requests `oc:fileid` in PROPFIND, extracts it via a new `GetRootFileID()` method, and implements four JSON handlers backed by the existing daemon.

**Tech Stack:** Go 1.22, standard library (`net/http`, `encoding/xml`, `crypto/rand`, `encoding/hex`). No new dependencies.

---

## File Map

| Action | File | Purpose |
|--------|------|---------|
| Modify | `agent/internal/config/config.go` | Add `AgentAPIKey` field, auto-generation |
| Modify | `agent/internal/config/config_test.go` | Tests for key generation and persistence |
| Modify | `agent/internal/web/server.go` | Extract `daemonProvider` interface, add CORS + API key middleware, register `/api/v1/` routes |
| Create | `agent/internal/web/middleware_test.go` | Tests for CORS and API key middleware |
| Create | `agent/internal/web/api_v1.go` | JSON response types, `writeJSON` helper, four handlers |
| Create | `agent/internal/web/api_v1_test.go` | Tests for all four v1 handlers |
| Modify | `agent/internal/web/handlers.go` | Add `AgentAPIKey` to `configData` struct and `settingsHandler` |
| Modify | `agent/internal/web/templates/settings.html` | Extension Configuration section |
| Modify | `agent/internal/store/store.go` | Add `FileID` field to `SessionEntry` |
| Modify | `agent/internal/store/store_test.go` | Test FileID persistence |
| Modify | `agent/internal/opencloud/client.go` | Add `<oc:fileid/>` to PROPFIND body, `FileID` to XML struct, `GetRootFileID()` method |
| Modify | `agent/internal/opencloud/client_test.go` | Tests for `GetRootFileID()` |
| Modify | `agent/internal/daemon/daemon.go` | Add `FileID` to `Session`, extract in `CreateSession`, populate in `loadSessionsFromStore` |
| Modify | `agent/internal/daemon/daemon_test.go` | Test FileID is preserved through store load |

---

### Task 1: Add AgentAPIKey to config with auto-generation

**Files:**
- Modify: `agent/internal/config/config.go`
- Modify: `agent/internal/config/config_test.go`

- [ ] **Step 1: Write failing tests** — add to `config_test.go` (add `"strings"` to its imports):

```go
func TestNewManager_GeneratesAgentAPIKey(t *testing.T) {
	tmpDir := t.TempDir()
	homeDir := filepath.Join(tmpDir, "home")
	if err := os.MkdirAll(homeDir, 0755); err != nil {
		t.Fatal(err)
	}
	os.Setenv("HOME", homeDir)
	defer os.Unsetenv("HOME")

	m, err := NewManager()
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	key := m.Get().AgentAPIKey
	if !strings.HasPrefix(key, "sb_agent_") {
		t.Errorf("AgentAPIKey %q does not start with sb_agent_", key)
	}
	// "sb_agent_" (9) + 32 hex chars from 16 random bytes
	if len(key) != 41 {
		t.Errorf("AgentAPIKey %q: expected length 41, got %d", key, len(key))
	}
}

func TestNewManager_PersistsAgentAPIKey(t *testing.T) {
	tmpDir := t.TempDir()
	homeDir := filepath.Join(tmpDir, "home")
	if err := os.MkdirAll(homeDir, 0755); err != nil {
		t.Fatal(err)
	}
	os.Setenv("HOME", homeDir)
	defer os.Unsetenv("HOME")

	m1, err := NewManager()
	if err != nil {
		t.Fatalf("first NewManager() error = %v", err)
	}
	key1 := m1.Get().AgentAPIKey

	m2, err := NewManager()
	if err != nil {
		t.Fatalf("second NewManager() error = %v", err)
	}
	if m2.Get().AgentAPIKey != key1 {
		t.Errorf("AgentAPIKey changed across restarts: %q → %q", key1, m2.Get().AgentAPIKey)
	}
}

func TestNewManager_EnvOverridesAgentAPIKey(t *testing.T) {
	tmpDir := t.TempDir()
	homeDir := filepath.Join(tmpDir, "home")
	if err := os.MkdirAll(homeDir, 0755); err != nil {
		t.Fatal(err)
	}
	os.Setenv("HOME", homeDir)
	defer os.Unsetenv("HOME")
	os.Setenv("SHAREBRIDGE_AGENT_API_KEY", "my-custom-key")
	defer os.Unsetenv("SHAREBRIDGE_AGENT_API_KEY")

	m, err := NewManager()
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	if m.Get().AgentAPIKey != "my-custom-key" {
		t.Errorf("AgentAPIKey = %q, want my-custom-key", m.Get().AgentAPIKey)
	}
}
```

- [ ] **Step 2: Run tests to confirm they fail**

```bash
cd /Users/ali/Git/ShareBridge/agent
go test ./internal/config/... -run "TestNewManager_GeneratesAgentAPIKey|TestNewManager_PersistsAgentAPIKey|TestNewManager_EnvOverridesAgentAPIKey" -v
```

Expected: FAIL — `AgentAPIKey` field does not exist yet.

- [ ] **Step 3: Update `TestNewManager_CreatesConfigDir`** — auto-generation now saves config on first run, creating the directory during `NewManager()`. Replace the section that checks the directory state:

Find this block in `TestNewManager_CreatesConfigDir`:
```go
	// Directory is created on save, not on load
	// Verify it doesn't exist yet
	configDir := filepath.Join(homeDir, ".sharebridge")
	if _, err := os.Stat(configDir); !os.IsNotExist(err) {
		t.Error("config directory should not exist before save")
	}

	// After save, directory should exist
	if err := m.Save(cfg); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if _, err := os.Stat(configDir); os.IsNotExist(err) {
		t.Error("config directory was not created after save")
	}
```

Replace with:
```go
	// AgentAPIKey auto-generation saves config during NewManager(), creating the directory.
	configDir := filepath.Join(homeDir, ".sharebridge")
	if _, err := os.Stat(configDir); os.IsNotExist(err) {
		t.Error("config directory should be created by AgentAPIKey auto-generation during NewManager()")
	}
```

- [ ] **Step 4: Implement in `config.go`**

Add `"crypto/rand"`, `"encoding/hex"`, and `"log"` to imports.

Add `AgentAPIKey` field to `Config` struct after `AllowedHost`:
```go
AgentAPIKey string `json:"agent_api_key,omitempty"` // auth key for /api/v1/ JSON endpoints
```

Add env var override in `load()` after the `ALLOWED_SHAREBRIDGE_HOST` block:
```go
	if v := os.Getenv("SHAREBRIDGE_AGENT_API_KEY"); v != "" {
		cfg.AgentAPIKey = v
	}
```

In `NewManager()`, add after `m.config = cfg` and before `return m, nil`:
```go
	if m.config.AgentAPIKey == "" {
		key, err := generateAgentAPIKey()
		if err != nil {
			return nil, fmt.Errorf("generate agent API key: %w", err)
		}
		m.config.AgentAPIKey = key
		if err := m.save(); err != nil {
			log.Printf("warning: could not persist auto-generated agent API key: %v", err)
		}
	}
```

Add new function at the bottom of `config.go`:
```go
// generateAgentAPIKey produces a "sb_agent_" prefixed 32-hex-character key
// using 16 cryptographically random bytes.
func generateAgentAPIKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand read: %w", err)
	}
	return "sb_agent_" + hex.EncodeToString(b), nil
}
```

- [ ] **Step 5: Run all config tests**

```bash
go test ./internal/config/... -v
```

Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add agent/internal/config/config.go agent/internal/config/config_test.go
git commit -m "feat(11a): auto-generate AgentAPIKey in config on first run"
```

---

### Task 2: Extract daemonProvider interface, add CORS + API key middleware, register /api/v1/ routes

**Files:**
- Modify: `agent/internal/web/server.go`
- Create: `agent/internal/web/middleware_test.go`
- Create: `agent/internal/web/api_v1.go` (stub handlers only in this task)

- [ ] **Step 1: Write failing middleware tests** — create `agent/internal/web/middleware_test.go`:

```go
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
func (m *mockDaemonMiddleware) CreateSession(_ context.Context, _, _ string, _ time.Duration, _ int, _ bool) (string, error) {
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
```

- [ ] **Step 2: Run tests to confirm they fail**

```bash
go test ./internal/web/... -run "TestAPIKeyMiddleware|TestCORSMiddleware" -v
```

Expected: FAIL — `daemonProvider` undefined, methods undefined.

- [ ] **Step 3: Add `daemonProvider` interface and update `WebServer` struct in `server.go`**

Add these imports: `"context"`, `"crypto/subtle"`, `"time"` (if not already present).

Add before the `WebServer` struct:
```go
// daemonProvider is the set of Daemon methods used by web handlers.
// Using an interface allows web package tests to inject a mock.
type daemonProvider interface {
	ListSessions() []*daemon.Session
	GetSession(code string) *daemon.Session
	GetConfig() *config.Config
	CreateSession(ctx context.Context, shareURL, password string, expiry time.Duration, maxDownloads int, relayOnly bool) (string, error)
	RevokeSession(code string) error
	HasTURN() bool
	IsConnected() bool
	GetUptime() time.Duration
	GetConfigPath() string
	SaveConfig(cfg *config.Config) error
}
```

Change `daemon *daemon.Daemon` field to `daemon daemonProvider` in the `WebServer` struct:
```go
type WebServer struct {
	daemon     daemonProvider
	port       int
	password   string
	server     *http.Server
	layoutTmpl *template.Template
	staticFS   http.FileSystem
}
```

Change `NewWebServer` parameter from `d *daemon.Daemon` to `d daemonProvider`:
```go
func NewWebServer(d daemonProvider, port int, password string) (*WebServer, error) {
```

Keep `SetDaemon` with `*daemon.Daemon` to satisfy the `daemon.WebServer` interface:
```go
func (ws *WebServer) SetDaemon(d *daemon.Daemon) {
	ws.daemon = d
}
```

- [ ] **Step 4: Add `corsMiddleware` and `apiKeyMiddleware` to `server.go`** — add after `authMiddleware`:

```go
// corsMiddleware sets CORS headers for /api/v1/ endpoints.
// The allowed origin is "https://" + AllowedHost from config.
// Returns 503 if daemon is not ready or AllowedHost is not configured —
// the extension cannot function without a known origin to restrict CORS to.
// Handles OPTIONS preflight by returning 204 without calling next.
func (ws *WebServer) corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ws.daemon == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":"Service unavailable","code":"UNAVAILABLE"}`))
			return
		}
		cfg := ws.daemon.GetConfig()
		if cfg.AllowedHost == "" {
			// Without AllowedHost we cannot set a safe CORS origin.
			// Reject rather than use a wildcard.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":"AllowedHost not configured","code":"MISCONFIGURED"}`))
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "https://"+cfg.AllowedHost)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

// apiKeyMiddleware checks X-API-Key against AgentAPIKey in config.
// Returns 401 JSON on mismatch; 503 JSON if daemon is not ready.
func (ws *WebServer) apiKeyMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ws.daemon == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":"Service unavailable","code":"UNAVAILABLE"}`))
			return
		}
		cfg := ws.daemon.GetConfig()
		provided := r.Header.Get("X-API-Key")
		if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(cfg.AgentAPIKey)) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"Invalid API key","code":"UNAUTHORIZED"}`))
			return
		}
		next(w, r)
	}
}
```

- [ ] **Step 5: Register `/api/v1/` routes in `registerRoutes`** — add at the end of the function:

```go
	// v1 JSON API — CORS headers + API key auth on every request
	v1 := func(h http.HandlerFunc) http.HandlerFunc {
		return ws.corsMiddleware(ws.apiKeyMiddleware(h))
	}
	mux.HandleFunc("GET /api/v1/shares", v1(ws.v1ListSharesHandler))
	mux.HandleFunc("POST /api/v1/shares", v1(ws.v1CreateShareHandler))
	mux.HandleFunc("DELETE /api/v1/shares/{code}", v1(ws.v1RevokeShareHandler))
	mux.HandleFunc("GET /api/v1/settings", v1(ws.v1SettingsHandler))
	// OPTIONS preflight — CORS only, no auth (browsers don't send auth on preflight)
	mux.HandleFunc("OPTIONS /api/v1/{path...}", ws.corsMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
```

- [ ] **Step 6: Create stub `api_v1.go`** — create `agent/internal/web/api_v1.go`:

```go
package web

import (
	"encoding/json"
	"net/http"
	"time"
)

// v1ShareResponse is the JSON representation of a ShareBridge session for the extension API.
type v1ShareResponse struct {
	Code         string    `json:"code"`
	PublicURL    string    `json:"public_url"`
	ShareURL     string    `json:"share_url"`
	FileID       string    `json:"file_id"`
	Downloads    int       `json:"downloads"`
	MaxDownloads int       `json:"max_downloads"`
	RelayOnly    bool      `json:"relay_only"`
	ExpiresAt    time.Time `json:"expires_at"`
	CreatedAt    time.Time `json:"created_at"`
}

type v1CreateShareRequest struct {
	ShareURL     string `json:"share_url"`
	Password     string `json:"password"`
	ExpiryHours  int    `json:"expiry_hours"`
	MaxDownloads int    `json:"max_downloads"`
	RelayOnly    bool   `json:"relay_only"`
}

type v1CreateShareResponse struct {
	Code      string    `json:"code"`
	PublicURL string    `json:"public_url"`
	ExpiresAt time.Time `json:"expires_at"`
}

type v1SettingsResponse struct {
	DefaultExpiryHours  int  `json:"default_expiry_hours"`
	DefaultMaxDownloads int  `json:"default_max_downloads"`
	DefaultRelayOnly    bool `json:"default_relay_only"`
	TURNAvailable       bool `json:"turn_available"`
}

type v1ErrorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// writeJSON writes v as JSON with the given HTTP status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (ws *WebServer) v1ListSharesHandler(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

func (ws *WebServer) v1CreateShareHandler(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

func (ws *WebServer) v1RevokeShareHandler(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

func (ws *WebServer) v1SettingsHandler(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}
```

- [ ] **Step 7: Run middleware tests**

```bash
go test ./internal/web/... -run "TestAPIKeyMiddleware|TestCORSMiddleware" -v
```

Expected: all PASS.

- [ ] **Step 8: Run all agent tests to check for regressions**

```bash
go test ./... -count=1
```

Expected: all PASS.

- [ ] **Step 9: Commit**

```bash
git add agent/internal/web/server.go agent/internal/web/api_v1.go agent/internal/web/middleware_test.go
git commit -m "feat(11a): extract daemonProvider interface, add CORS + API key middleware, register /api/v1/ routes"
```

---

### Task 3: Show AgentAPIKey in settings page

**Files:**
- Modify: `agent/internal/web/handlers.go`
- Modify: `agent/internal/web/templates/settings.html`

- [ ] **Step 1: Add `AgentAPIKey` to `configData` in `handlers.go`**

In the `configData` struct, add `AgentAPIKey string` after `AllowedHost`:
```go
type configData struct {
	SignalingURL         string
	APIKey               string
	AllowedHost          string
	AgentAPIKey          string
	DefaultExpiry        int
	DefaultMaxDownloads  int
	DefaultRelayOnly     bool
	UIPort               int
}
```

In `settingsHandler`, populate it — add `AgentAPIKey: c.AgentAPIKey` to the `configData` literal:
```go
		cfg = configData{
			SignalingURL:        c.SignalingURL,
			APIKey:              c.APIKey,
			AllowedHost:         c.AllowedHost,
			AgentAPIKey:         c.AgentAPIKey,
			DefaultExpiry:       c.DefaultExpiry,
			DefaultMaxDownloads: c.DefaultMaxDownloads,
			DefaultRelayOnly:    c.DefaultRelayOnly,
			UIPort:              c.UIPort,
		}
```

- [ ] **Step 2: Add Extension Configuration section to `settings.html`**

Add after the closing `</article>` of the About section, before `{{end}}`:

```html
<article>
    <header>
        <hgroup>
            <h2>Extension Configuration</h2>
            <p>Use these values to configure the ShareBridge OpenCloud extension</p>
        </hgroup>
    </header>
    <table>
        <tbody>
            <tr>
                <th>Agent URL</th>
                <td><code>http://localhost:{{.Config.UIPort}}</code></td>
            </tr>
            <tr>
                <th>API Key</th>
                <td>
                    <span id="ext-api-key">{{.Config.AgentAPIKey}}</span>
                    <button type="button"
                        onclick="navigator.clipboard.writeText(document.getElementById('ext-api-key').textContent).then(() => { this.textContent='Copied!'; setTimeout(() => this.textContent='Copy', 2000); })">
                        Copy
                    </button>
                </td>
            </tr>
        </tbody>
    </table>
</article>
```

- [ ] **Step 3: Build and verify it compiles**

```bash
cd /Users/ali/Git/ShareBridge/agent && go build ./...
```

Expected: no errors.

- [ ] **Step 4: Commit**

```bash
git add agent/internal/web/handlers.go agent/internal/web/templates/settings.html
git commit -m "feat(11a): show extension API key and agent URL in settings page"
```

---

### Task 4: Add FileID field to SessionEntry

**Files:**
- Modify: `agent/internal/store/store.go`
- Modify: `agent/internal/store/store_test.go`

- [ ] **Step 1: Write failing test** — add to `store_test.go`:

```go
func TestSaveSession_PersistsFileID(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SHAREBRIDGE_DATA_DIR", tmpDir)

	st, err := New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	session := SessionEntry{
		Code:         "test-code",
		ShareURL:     "https://opencloud.example.com/s/abc",
		FileID:       "storage-users-1$abc!def",
		ExpiresAt:    time.Now().Add(24 * time.Hour),
		MaxDownloads: 5,
		CreatedAt:    time.Now(),
	}
	if err := st.SaveSession(session); err != nil {
		t.Fatalf("SaveSession() error: %v", err)
	}

	loaded := st.GetSession("test-code")
	if loaded == nil {
		t.Fatal("GetSession() returned nil")
	}
	if loaded.FileID != "storage-users-1$abc!def" {
		t.Errorf("FileID = %q, want storage-users-1$abc!def", loaded.FileID)
	}
}
```

- [ ] **Step 2: Run test to confirm it fails**

```bash
go test ./internal/store/... -run TestSaveSession_PersistsFileID -v
```

Expected: FAIL — `SessionEntry.FileID undefined`.

- [ ] **Step 3: Add `FileID` to `SessionEntry` in `store.go`**

Add `FileID string` after `ShareURL` in the `SessionEntry` struct:
```go
type SessionEntry struct {
	Code         string    `json:"code"`
	ShareURL     string    `json:"share_url"`
	FileID       string    `json:"file_id,omitempty"`
	Password     string    `json:"password,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	MaxDownloads int       `json:"max_downloads,omitempty"`
	Downloads    int       `json:"downloads"`
	RelayOnly    bool      `json:"relay_only"`
	CreatedAt    time.Time `json:"created_at"`
}
```

- [ ] **Step 4: Run all store tests**

```bash
go test ./internal/store/... -v
```

Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/store/store.go agent/internal/store/store_test.go
git commit -m "feat(11b): add FileID field to SessionEntry for per-file share tracking"
```

---

### Task 5: Add oc:fileid to PROPFIND and GetRootFileID()

**Files:**
- Modify: `agent/internal/opencloud/client.go`
- Modify: `agent/internal/opencloud/client_test.go`

- [ ] **Step 1: Write failing tests** — add to `client_test.go` (add `"net/http"`, `"net/http/httptest"`, `"strings"` to imports):

```go
func TestGetRootFileID_ReturnsFileID(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" {
			t.Errorf("expected PROPFIND, got %s", r.Method)
		}
		if r.Header.Get("Depth") != "0" {
			t.Errorf("expected Depth: 0, got %q", r.Header.Get("Depth"))
		}
		w.WriteHeader(http.StatusMultiStatus)
		w.Write([]byte(`<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">
  <d:response>
    <d:href>/remote.php/dav/public-files/testtoken/</d:href>
    <d:propstat>
      <d:prop>
        <d:resourcetype><d:collection/></d:resourcetype>
        <oc:fileid>storage-users-1$abc!def</oc:fileid>
      </d:prop>
    </d:propstat>
  </d:response>
</d:multistatus>`))
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "https://")
	c, err := New(srv.URL+"/s/testtoken", host, "")
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	c.httpClient = srv.Client()

	fileID, err := c.GetRootFileID()
	if err != nil {
		t.Fatalf("GetRootFileID() error: %v", err)
	}
	if fileID != "storage-users-1$abc!def" {
		t.Errorf("FileID = %q, want storage-users-1$abc!def", fileID)
	}
}

func TestGetRootFileID_EmptyWhenMissing(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		w.Write([]byte(`<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:">
  <d:response>
    <d:href>/remote.php/dav/public-files/testtoken/</d:href>
    <d:propstat>
      <d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop>
    </d:propstat>
  </d:response>
</d:multistatus>`))
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "https://")
	c, err := New(srv.URL+"/s/testtoken", host, "")
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	c.httpClient = srv.Client()

	fileID, err := c.GetRootFileID()
	if err != nil {
		t.Fatalf("GetRootFileID() should not error when oc:fileid is absent: %v", err)
	}
	if fileID != "" {
		t.Errorf("expected empty fileID for missing oc:fileid, got %q", fileID)
	}
}
```

- [ ] **Step 2: Run tests to confirm they fail**

```bash
go test ./internal/opencloud/... -run "TestGetRootFileID" -v
```

Expected: FAIL — `GetRootFileID` undefined.

- [ ] **Step 3: Update `propfindBody` const in `client.go`** — add `<oc:fileid/>`:

```go
const propfindBody = `<?xml version="1.0" encoding="UTF-8"?>
<D:propfind xmlns:D="DAV:" xmlns:oc="http://owncloud.org/ns">
  <D:prop>
    <D:getcontentlength/>
    <D:getcontenttype/>
    <D:resourcetype/>
    <oc:checksums/>
    <oc:fileid/>
  </D:prop>
</D:propfind>`
```

- [ ] **Step 4: Add `FileID` to the XML `response` struct in `client.go`**

Find the anonymous `response` struct and add `FileID string` to the `Prop` section:
```go
type response struct {
	Href     string `xml:"href"`
	Propstat struct {
		Prop struct {
			ContentLength string `xml:"getcontentlength"`
			ContentType   string `xml:"getcontenttype"`
			ResourceType  struct {
				Collection *struct{} `xml:"collection"`
			} `xml:"resourcetype"`
			Checksums struct {
				Checksum string `xml:"checksum"`
			} `xml:"checksums"`
			FileID string `xml:"fileid"` // oc:fileid, matched by local name
		} `xml:"prop"`
	} `xml:"propstat"`
}
```

- [ ] **Step 5: Add `GetRootFileID()` method to `Client`** — add after `GetFile()`:

```go
// GetRootFileID returns the oc:fileid of the share root via a Depth:0 PROPFIND.
// Returns empty string without error if oc:fileid is absent (graceful degradation
// for older OpenCloud versions or non-OpenCloud WebDAV servers).
func (c *Client) GetRootFileID() (string, error) {
	req, err := http.NewRequest("PROPFIND", c.baseURL, strings.NewReader(propfindBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Depth", "0")
	req.Header.Set("Authorization", c.authHeader())
	req.Header.Set("Content-Type", "application/xml")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("PROPFIND request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMultiStatus {
		return "", fmt.Errorf("PROPFIND returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	var ms multistatus
	if err := xml.Unmarshal(body, &ms); err != nil {
		return "", fmt.Errorf("parse XML: %w", err)
	}

	if len(ms.Response) == 0 {
		return "", nil
	}
	return ms.Response[0].Propstat.Prop.FileID, nil
}
```

- [ ] **Step 6: Run all opencloud tests**

```bash
go test ./internal/opencloud/... -v
```

Expected: all PASS.

- [ ] **Step 7: Commit**

```bash
git add agent/internal/opencloud/client.go agent/internal/opencloud/client_test.go
git commit -m "feat(11b): add oc:fileid to PROPFIND, implement GetRootFileID()"
```

---

### Task 6: Add FileID to daemon.Session, extract in CreateSession

**Files:**
- Modify: `agent/internal/daemon/daemon.go`
- Modify: `agent/internal/daemon/daemon_test.go`

- [ ] **Step 1: Write failing test** — add to `daemon_test.go`:

```go
func TestLoadSessionsFromStore_PreservesFileID(t *testing.T) {
	cfg := &config.Config{
		SignalingURL: "ws://localhost:8080",
		APIKey:       "test-api-key",
		AllowedHost:  "opencloud.example.com",
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()

	st.SaveSession(store.SessionEntry{
		Code:         "file-code",
		ShareURL:     "https://opencloud.example.com/s/abc123",
		FileID:       "storage-1$foo!bar",
		ExpiresAt:    time.Now().Add(24 * time.Hour),
		MaxDownloads: 10,
		CreatedAt:    time.Now().Add(-1 * time.Hour),
	})

	sigClient := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())
	sigClient.registerShare = func(ctx context.Context, shareURL, preferredCode string, relayOnly bool) (string, bool, error) {
		return preferredCode, true, nil
	}

	d, err := NewWithSignaling(cfgMgr, st, sigClient)
	if err != nil {
		t.Fatalf("NewWithSignaling() error: %v", err)
	}

	d.loadSessionsFromStore(context.Background())

	session := d.GetSession("file-code")
	if session == nil {
		t.Fatal("session not found after loadSessionsFromStore")
	}
	if session.FileID != "storage-1$foo!bar" {
		t.Errorf("FileID = %q, want storage-1$foo!bar", session.FileID)
	}
}
```

- [ ] **Step 2: Run test to confirm it fails**

```bash
go test ./internal/daemon/... -run TestLoadSessionsFromStore_PreservesFileID -v
```

Expected: FAIL — `Session.FileID undefined`.

- [ ] **Step 3: Add `FileID` to `daemon.Session` struct**

In `daemon.go`, add `FileID string` after `ShareURL`:
```go
type Session struct {
	Code         string
	ShareURL     string
	FileID       string // oc:fileid extracted via WebDAV PROPFIND on share root
	Password     string
	ExpiresAt    time.Time
	MaxDownloads int
	Downloads    int
	RelayOnly    bool
	CreatedAt    time.Time

	webdavClient *opencloud.Client
	peers        map[string]*peer.Peer
	mu           sync.Mutex
}
```

- [ ] **Step 4: Extract FileID in `CreateSession`**

In `CreateSession`, after creating `webdavClient` and before `RegisterShare`, add:
```go
	// Extract oc:fileid from share root — best-effort; empty string on failure.
	fileID, err := webdavClient.GetRootFileID()
	if err != nil {
		log.Printf("warning: could not extract fileID for %s: %v", shareURL, err)
		fileID = ""
	}
```

Add `FileID: fileID` to the `session` struct literal:
```go
	session := &Session{
		Code:         code,
		ShareURL:     shareURL,
		FileID:       fileID,
		Password:     password,
		ExpiresAt:    now.Add(expiryDuration),
		MaxDownloads: maxDownloads,
		Downloads:    0,
		RelayOnly:    relayOnly,
		CreatedAt:    now,
		webdavClient: webdavClient,
		peers:        make(map[string]*peer.Peer),
	}
```

Add `FileID: fileID` to the `store.SaveSession` call:
```go
	if err := d.store.SaveSession(store.SessionEntry{
		Code:         code,
		ShareURL:     shareURL,
		FileID:       fileID,
		Password:     password,
		ExpiresAt:    session.ExpiresAt,
		MaxDownloads: maxDownloads,
		Downloads:    0,
		RelayOnly:    relayOnly,
		CreatedAt:    now,
	}); err != nil {
		log.Printf("warning: could not persist session: %v", err)
	}
```

- [ ] **Step 5: Populate FileID in `loadSessionsFromStore`**

Add `FileID: entry.FileID` to the `Session` struct literal in `loadSessionsFromStore`:
```go
		session := &Session{
			Code:         code,
			ShareURL:     entry.ShareURL,
			FileID:       entry.FileID,
			Password:     entry.Password,
			ExpiresAt:    entry.ExpiresAt,
			MaxDownloads: entry.MaxDownloads,
			Downloads:    entry.Downloads,
			RelayOnly:    entry.RelayOnly,
			CreatedAt:    entry.CreatedAt,
			webdavClient: webdavClient,
			peers:        make(map[string]*peer.Peer),
		}
```

- [ ] **Step 6: Run all daemon tests**

```bash
go test ./internal/daemon/... -v
```

Expected: all PASS. (Note: `TestCreateSession` triggers `GetRootFileID()` on a DNS-unresolvable host — it fails fast and logs a warning; the test still passes because fileID failure is graceful.)

- [ ] **Step 7: Run all agent tests**

```bash
go test ./... -count=1
```

Expected: all PASS.

- [ ] **Step 8: Commit**

```bash
git add agent/internal/daemon/daemon.go agent/internal/daemon/daemon_test.go
git commit -m "feat(11b): extract oc:fileid in CreateSession, propagate through Session struct"
```

---

### Task 7: Implement v1 JSON handlers

**Note:** The handlers call `derivePublicURL(signalingURL, code)` — this function already exists in `agent/internal/web/handlers.go:177`. It is in the same `web` package, so it can be called directly from `api_v1.go` without any import.

**Files:**
- Modify: `agent/internal/web/api_v1.go`
- Create: `agent/internal/web/api_v1_test.go`

- [ ] **Step 1: Write failing tests** — create `agent/internal/web/api_v1_test.go`:

```go
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

	body := `{"share_url":"https://oc.example.com/s/xyz","expiry_hours":48,"max_downloads":5}`
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
```

- [ ] **Step 2: Run tests to confirm they fail**

```bash
go test ./internal/web/... -run "TestV1" -v
```

Expected: FAIL — handlers return 501 Not Implemented.

- [ ] **Step 3: Replace the four stub handlers in `api_v1.go` with full implementations**

```go
// v1ListSharesHandler returns all active sessions as JSON.
// Optional ?file_id= query param filters by oc:fileid.
func (ws *WebServer) v1ListSharesHandler(w http.ResponseWriter, r *http.Request) {
	if ws.daemon == nil {
		writeJSON(w, http.StatusServiceUnavailable, v1ErrorResponse{"Service unavailable", "UNAVAILABLE"})
		return
	}

	fileID := r.URL.Query().Get("file_id")
	cfg := ws.daemon.GetConfig()

	shares := []v1ShareResponse{}
	for _, session := range ws.daemon.ListSessions() {
		if fileID != "" && session.FileID != fileID {
			continue
		}
		shares = append(shares, v1ShareResponse{
			Code:         session.Code,
			PublicURL:    derivePublicURL(cfg.SignalingURL, session.Code),
			ShareURL:     session.ShareURL,
			FileID:       session.FileID,
			Downloads:    session.Downloads,
			MaxDownloads: session.MaxDownloads,
			RelayOnly:    session.RelayOnly,
			ExpiresAt:    session.ExpiresAt,
			CreatedAt:    session.CreatedAt,
		})
	}

	writeJSON(w, http.StatusOK, shares)
}

// v1CreateShareHandler creates a new ShareBridge share from a JSON request body.
func (ws *WebServer) v1CreateShareHandler(w http.ResponseWriter, r *http.Request) {
	if ws.daemon == nil {
		writeJSON(w, http.StatusServiceUnavailable, v1ErrorResponse{"Service unavailable", "UNAVAILABLE"})
		return
	}

	var req v1CreateShareRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, v1ErrorResponse{"Invalid request body", "BAD_REQUEST"})
		return
	}
	if req.ShareURL == "" {
		writeJSON(w, http.StatusBadRequest, v1ErrorResponse{"share_url is required", "BAD_REQUEST"})
		return
	}

	expiryHours := req.ExpiryHours
	if expiryHours <= 0 {
		expiryHours = ws.daemon.GetConfig().DefaultExpiry
	}

	code, err := ws.daemon.CreateSession(
		r.Context(),
		req.ShareURL,
		req.Password,
		time.Duration(expiryHours)*time.Hour,
		req.MaxDownloads,
		req.RelayOnly,
	)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, v1ErrorResponse{err.Error(), "INTERNAL_ERROR"})
		return
	}

	session := ws.daemon.GetSession(code)
	if session == nil {
		writeJSON(w, http.StatusInternalServerError, v1ErrorResponse{"Session not found after creation", "INTERNAL_ERROR"})
		return
	}

	writeJSON(w, http.StatusCreated, v1CreateShareResponse{
		Code:      session.Code,
		PublicURL: derivePublicURL(ws.daemon.GetConfig().SignalingURL, session.Code),
		ExpiresAt: session.ExpiresAt,
	})
}

// v1RevokeShareHandler revokes a share by code, returning 204 on success.
func (ws *WebServer) v1RevokeShareHandler(w http.ResponseWriter, r *http.Request) {
	if ws.daemon == nil {
		writeJSON(w, http.StatusServiceUnavailable, v1ErrorResponse{"Service unavailable", "UNAVAILABLE"})
		return
	}

	code := r.PathValue("code")
	if code == "" {
		writeJSON(w, http.StatusBadRequest, v1ErrorResponse{"Missing share code", "BAD_REQUEST"})
		return
	}

	if err := ws.daemon.RevokeSession(code); err != nil {
		writeJSON(w, http.StatusNotFound, v1ErrorResponse{"Share not found", "NOT_FOUND"})
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// v1SettingsHandler returns the agent's default share configuration for the extension form.
func (ws *WebServer) v1SettingsHandler(w http.ResponseWriter, r *http.Request) {
	if ws.daemon == nil {
		writeJSON(w, http.StatusServiceUnavailable, v1ErrorResponse{"Service unavailable", "UNAVAILABLE"})
		return
	}

	cfg := ws.daemon.GetConfig()
	writeJSON(w, http.StatusOK, v1SettingsResponse{
		DefaultExpiryHours:  cfg.DefaultExpiry,
		DefaultMaxDownloads: cfg.DefaultMaxDownloads,
		DefaultRelayOnly:    cfg.DefaultRelayOnly,
		TURNAvailable:       ws.daemon.HasTURN(),
	})
}
```

- [ ] **Step 4: Run v1 handler tests**

```bash
go test ./internal/web/... -run "TestV1" -v
```

Expected: all PASS.

- [ ] **Step 5: Run all agent tests**

```bash
go test ./... -count=1
```

Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add agent/internal/web/api_v1.go agent/internal/web/api_v1_test.go
git commit -m "feat(11b): implement /api/v1/ JSON handlers — list, create, revoke, settings"
```
