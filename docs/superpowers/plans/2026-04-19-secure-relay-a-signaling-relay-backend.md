# Secure Relay Plan A: Signaling + Relay Backend Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the signaling-server relay backend foundation: relay session preparation, signed browser/agent JWTs, in-memory relay session state, relay WebSocket forwarding, and server-authoritative relay byte accounting without breaking the current direct-mode flow.

**Architecture:** Add a new `internal/relay` package that owns relay JWT claims, pending/active relay session state, `jti` replay protection, pending browser wait windows, and quota/accounting helpers. Expose one new `/ws/relay` endpoint plus small additive changes to the existing signaling handlers so they can prepare relay state after browser auth succeeds while current direct-mode WebRTC continues to work unchanged.

**Tech Stack:** Go, PocketBase, coder/websocket, golang-jwt/jwt/v5, in-memory state, PocketBase persistence

---

## File Structure

### New files

- `signaling-server/internal/relay/tokens.go`
  - HS256 JWT claim structs and sign/verify helpers for browser policy JWTs and agent relay JWTs.
- `signaling-server/internal/relay/tokens_test.go`
  - Token round-trip, expiry, purpose, and signature validation tests.
- `signaling-server/internal/relay/registry.go`
  - In-memory relay session registry: pending sessions, pending wait window, spent `jti`s, browser/agent socket binding, byte counters, cleanup.
- `signaling-server/internal/relay/registry_test.go`
  - Registry lifecycle tests: browser-before-agent wait, replay rejection, direct-success cleanup, expiry cleanup.
- `signaling-server/internal/relay/accounting.go`
  - Relay byte accounting helpers that update PocketBase quota fields from authoritative relay bytes.
- `signaling-server/internal/relay/accounting_test.go`
  - Tests for byte accumulation, quota-period rollover, and archival into `bandwidth_usage`.
- `signaling-server/internal/handler/relay_ws.go`
  - Dedicated relay WebSocket endpoint that authenticates browser/agent JWTs, pairs sockets by `sid`, forwards opaque bytes, updates accounting, and closes on protocol violations.
- `signaling-server/internal/handler/relay_ws_test.go`
  - End-to-end backend relay tests covering JWT auth, pending wait window, replay rejection, forwarding, and accounting flush.
- `signaling-server/migrations/5_add_session_relay_static_pub.go`
  - Adds `relay_static_pub` to `sessions` so the server can mint browser relay policy with the expected agent Noise static key.

### Modified files

- `signaling-server/internal/handler/agent_ws.go`
  - Accept `relay_static_pub` during `register_share`.
  - Handle new `auth_ok` message from the agent.
  - Create relay session preparation state and send additive `relay_prepare` / `relay_policy` messages.
- `signaling-server/internal/handler/agent_ws_test.go`
  - Cover `relay_static_pub` persistence and `auth_ok`-driven relay preparation messages.
- `signaling-server/internal/handler/browser_ws.go`
  - Inject relay-preparation dependencies only if needed for browser-side session validation helpers; keep current direct-mode behavior intact.
- `signaling-server/internal/handler/browser_ws_test.go`
  - Verify direct-mode behavior stays intact while relay-preparation data remains additive.
- `signaling-server/internal/hub/hub.go`
  - Add any minimal helper needed to target the currently connected agent/browser by `connID`/session code without coupling relay socket state to the old WebRTC pair map.
- `signaling-server/internal/hub/hub_test.go`
  - Cover any new hub helper added for relay preparation.
- `signaling-server/cmd/server/main.go`
  - Construct the relay registry/accounting services and wire the new `/ws/relay` endpoint.
- `signaling-server/internal/config/config.go`
  - Add relay-specific config for the HS256 JWT secret and pending wait window.
- `signaling-server/go.mod`
  - Make `github.com/golang-jwt/jwt/v5` a direct dependency if it is still indirect.
- `signaling-server/migrations/migrations_test.go`
  - Assert the new `relay_static_pub` field exists after migrations.

## Task 1: Add Session Schema Support For The Agent Relay Static Key

**Files:**
- Create: `signaling-server/migrations/5_add_session_relay_static_pub.go`
- Modify: `signaling-server/internal/handler/agent_ws.go`
- Modify: `signaling-server/internal/handler/agent_ws_test.go`
- Modify: `signaling-server/migrations/migrations_test.go`

- [ ] **Step 1: Write the failing migration and registration tests**

```go
func TestMigration5_AddSessionRelayStaticPub(t *testing.T) {
	testApp, err := tests.NewTestApp(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { testApp.Cleanup() })

	require.NoError(t, testApp.Bootstrap())
	require.NoError(t, testApp.RunSystemMigrations())
	require.NoError(t, migrations.CreateCollections(testApp))
	require.NoError(t, migrations.AddRelayOnly(testApp))
	require.NoError(t, migrations.AddSessionRelayStaticPub(testApp))

	sessionsCol, err := testApp.FindCollectionByNameOrId("sessions")
	require.NoError(t, err)
	require.NotNil(t, sessionsCol.Fields.GetByName("relay_static_pub"))
}

func TestAgentWS_RegisterShare_PersistsRelayStaticPub(t *testing.T) {
	app, cleanup := setupAgentTestApp(t)
	defer cleanup()

	user, err := createTestUser(app, "relay@example.com")
	require.NoError(t, err)
	apiKey, err := createTestAPIKey(app, user.Id, "relaysecret")
	require.NoError(t, err)

	h := hub.New()
	cfg := config.Load()
	reg := relay.NewRegistry(2 * time.Second)

	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, reg, cfg)
	mux := http.NewServeMux()
	mux.Handle("/ws/agent", authMiddleware(http.HandlerFunc(agentHandler)))

	server := httptest.NewServer(mux)
	defer server.Close()

	ctx := context.Background()
	fullKey := apiKey.Id + ".relaysecret"
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent?api_key="+fullKey, nil)
	require.NoError(t, err)
	defer conn.CloseNow()

	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","agent_id":"agent-1"}`)))
	_, _, err = conn.Read(ctx)
	require.NoError(t, err)

	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","code":"RELAYKEY1","relay_static_pub":"04abcd"}`)))
	_, _, err = conn.Read(ctx)
	require.NoError(t, err)

	session, err := getSessionByCode(app, "RELAYKEY1")
	require.NoError(t, err)
	require.Equal(t, "04abcd", session.GetString("relay_static_pub"))
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
cd signaling-server && go test ./migrations ./internal/handler -run 'TestMigration5_AddSessionRelayStaticPub|TestAgentWS_RegisterShare_PersistsRelayStaticPub' -v
```

Expected:

- FAIL because `AddSessionRelayStaticPub` does not exist
- FAIL because `relay_static_pub` is neither parsed nor stored

- [ ] **Step 3: Write the minimal schema and registration implementation**

```go
// signaling-server/migrations/5_add_session_relay_static_pub.go
package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(AddSessionRelayStaticPub, nil)
}

func AddSessionRelayStaticPub(app core.App) error {
	sessionsCol, err := app.FindCollectionByNameOrId("sessions")
	if err != nil {
		return err
	}

	if sessionsCol.Fields.GetByName("relay_static_pub") != nil {
		return nil
	}

	sessionsCol.Fields.Add(&core.TextField{
		Name:     "relay_static_pub",
		Required: false,
	})

	return app.Save(sessionsCol)
}
```

```go
// signaling-server/internal/handler/agent_ws.go
// in agent_ws_test.go, setupAgentTestApp must run the custom migration chain through AddSessionRelayStaticPub:
// CreateCollections -> AddAPIKeyTimestamps -> AddQuotaFields -> AddRelayOnly -> AddSessionRelayStaticPub

type agentMsg struct {
	Type           string          `json:"type"`
	AgentID        string          `json:"agent_id,omitempty"`
	Code           string          `json:"code,omitempty"`
	ShareURL       string          `json:"share_url,omitempty"`
	ExpiresAt      *time.Time      `json:"expires_at,omitempty"`
	SessionID      string          `json:"session_id,omitempty"`
	SDP            string          `json:"sdp,omitempty"`
	Candidate      json.RawMessage `json:"candidate,omitempty"`
	ConnID         string          `json:"conn_id,omitempty"`
	Value          string          `json:"value,omitempty"`
	HasPassword    bool            `json:"has_password,omitempty"`
	RelayOnly      bool            `json:"relay_only,omitempty"`
	RelayStaticPub string          `json:"relay_static_pub,omitempty"`
}

func createSession(app core.App, code, apiKeyID, agentID string, expiresAt *time.Time, relayOnly bool, relayStaticPub string) error {
	col, err := app.FindCollectionByNameOrId("sessions")
	if err != nil {
		return err
	}

	record := core.NewRecord(col)
	record.Set("code", code)
	record.Set("api_key_id", apiKeyID)
	record.Set("agent_id", agentID)
	record.Set("relay_only", relayOnly)
	record.Set("relay_static_pub", relayStaticPub)
	if expiresAt != nil {
		dt, _ := types.ParseDateTime(*expiresAt)
		record.Set("expires_at", dt)
	}
	return app.Save(record)
}

func claimSessionCode(app core.App, code, apiKeyID, accountID, agentID string, expiresAt *time.Time, relayOnly bool, relayStaticPub string) (*core.Record, bool, error)

// inside handleRegisterShare, both call sites must pass msg.RelayStaticPub:
err = createSession(app, code, apiKeyID, agentID, msg.ExpiresAt, msg.RelayOnly, msg.RelayStaticPub)
session, reclaimed, err := claimSessionCode(app, code, apiKeyID, accountID, agentID, msg.ExpiresAt, msg.RelayOnly, msg.RelayStaticPub)
```

- [ ] **Step 4: Run tests to verify they pass**

Run:

```bash
cd signaling-server && go test ./migrations ./internal/handler -run 'TestMigration5_AddSessionRelayStaticPub|TestAgentWS_RegisterShare_PersistsRelayStaticPub' -v
```

Expected:

- PASS for both tests

- [ ] **Step 5: Commit**

```bash
git add signaling-server/migrations/5_add_session_relay_static_pub.go signaling-server/internal/handler/agent_ws.go signaling-server/internal/handler/agent_ws_test.go signaling-server/migrations/migrations_test.go
git commit -m "feat(relay): persist agent relay static key in sessions"
```

## Task 2: Implement HS256 Relay JWT Helpers

**Files:**
- Create: `signaling-server/internal/relay/tokens.go`
- Create: `signaling-server/internal/relay/tokens_test.go`
- Modify: `signaling-server/internal/config/config.go`
- Modify: `signaling-server/go.mod`

- [ ] **Step 1: Write the failing token tests**

```go
func TestSignAndVerifyBrowserPolicyJWT(t *testing.T) {
	claims := BrowserPolicyClaims{
		SID:               "sid-123",
		SessionCode:       "SHARE123",
		RelayAllowed:      true,
		RelayOnly:         false,
		ExpectedStaticPub: "04abcd",
		RegisteredClaims:  jwt.RegisteredClaims{ID: "jti-123"},
	}

	token, err := SignBrowserPolicyJWT("test-secret", claims, time.Unix(1_800_000_000, 0))
	require.NoError(t, err)

	parsed, err := VerifyBrowserPolicyJWT("test-secret", token, time.Unix(1_800_000_010, 0))
	require.NoError(t, err)
	require.Equal(t, claims.SID, parsed.SID)
	require.Equal(t, "browser_policy", parsed.Purpose)
}

func TestVerifyBrowserPolicyJWT_RejectsExpiredToken(t *testing.T) {
	claims := BrowserPolicyClaims{
		SID:              "sid-123",
		RegisteredClaims: jwt.RegisteredClaims{ID: "jti-123"},
	}
	token, err := SignBrowserPolicyJWT("test-secret", claims, time.Unix(1_800_000_000, 0))
	require.NoError(t, err)

	_, err = VerifyBrowserPolicyJWT("test-secret", token, time.Unix(1_800_000_200, 0))
	require.Error(t, err)
}

func TestSignAndVerifyAgentRelayJWT(t *testing.T) {
	claims := AgentRelayClaims{SID: "sid-123", AgentID: "agent-1"}
	token, err := SignAgentRelayJWT("test-secret", claims, time.Unix(1_800_000_000, 0))
	require.NoError(t, err)

	parsed, err := VerifyAgentRelayJWT("test-secret", token, time.Unix(1_800_000_010, 0))
	require.NoError(t, err)
	require.Equal(t, "agent_relay", parsed.Purpose)
	require.Equal(t, "agent-1", parsed.AgentID)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
cd signaling-server && go test ./internal/relay -run 'TestSignAndVerifyBrowserPolicyJWT|TestVerifyBrowserPolicyJWT_RejectsExpiredToken|TestSignAndVerifyAgentRelayJWT' -v
```

Expected:

- FAIL because `internal/relay` and the signing helpers do not exist

- [ ] **Step 3: Write the minimal token helper implementation**

```go
// signaling-server/internal/relay/tokens.go
package relay

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const TokenLifetime = 120 * time.Second

type BrowserPolicyClaims struct {
	SID               string `json:"sid"`
	SessionCode       string `json:"session_code"`
	RelayAllowed      bool   `json:"relay_allowed"`
	RelayOnly         bool   `json:"relay_only"`
	ExpectedStaticPub string `json:"expected_agent_static_pub"`
	Purpose           string `json:"purpose"`
	jwt.RegisteredClaims
}

type AgentRelayClaims struct {
	SID     string `json:"sid"`
	AgentID string `json:"agent_id"`
	Purpose string `json:"purpose"`
	jwt.RegisteredClaims
}

func SignBrowserPolicyJWT(secret string, claims BrowserPolicyClaims, issuedAt time.Time) (string, error) {
	claims.Purpose = "browser_policy"
	claims.RegisteredClaims = jwt.RegisteredClaims{
		ID:        claims.RegisteredClaims.ID,
		IssuedAt:  jwt.NewNumericDate(issuedAt),
		ExpiresAt: jwt.NewNumericDate(issuedAt.Add(TokenLifetime)),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
}

func VerifyBrowserPolicyJWT(secret, token string, now time.Time) (*BrowserPolicyClaims, error) {
	parser := jwt.NewParser(jwt.WithTimeFunc(func() time.Time { return now }))
	claims := &BrowserPolicyClaims{}
	_, err := parser.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		return []byte(secret), nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil {
		return nil, err
	}
	if claims.Purpose != "browser_policy" {
		return nil, errors.New("relay: invalid token purpose")
	}
	return claims, nil
}

func SignAgentRelayJWT(secret string, claims AgentRelayClaims, issuedAt time.Time) (string, error) {
	claims.Purpose = "agent_relay"
	claims.RegisteredClaims = jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(issuedAt),
		ExpiresAt: jwt.NewNumericDate(issuedAt.Add(TokenLifetime)),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
}

func VerifyAgentRelayJWT(secret, token string, now time.Time) (*AgentRelayClaims, error) {
	parser := jwt.NewParser(jwt.WithTimeFunc(func() time.Time { return now }))
	claims := &AgentRelayClaims{}
	_, err := parser.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		return []byte(secret), nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil {
		return nil, err
	}
	if claims.Purpose != "agent_relay" {
		return nil, errors.New("relay: invalid token purpose")
	}
	return claims, nil
}

func NewSID() string { return newTokenID("sid") }

func NewJTI() string { return newTokenID("jti") }

func newTokenID(prefix string) string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(buf)
}
```

```go
// signaling-server/internal/config/config.go
type Config struct {
	Port                   string
	STUNURL                string
	DataDir                string
	RelayJWTSecret         string
	RelayPendingWaitWindow time.Duration
	// existing TURN/SMTP/quota fields stay below
}

func Load() *Config {
	return &Config{
		Port:                   getEnv("PORT", "8080"),
		STUNURL:                getEnv("STUN_URL", "stun:stun.cloudflare.com:3478"),
		DataDir:                getEnv("DATA_DIR", "./pb_data"),
		RelayJWTSecret:         getEnv("RELAY_JWT_SECRET", ""),
		RelayPendingWaitWindow: getEnvDuration("RELAY_PENDING_WAIT_WINDOW", 2*time.Second),
		// existing TURN/SMTP/quota fields stay below
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run:

```bash
cd signaling-server && go test ./internal/relay -run 'TestSignAndVerifyBrowserPolicyJWT|TestVerifyBrowserPolicyJWT_RejectsExpiredToken|TestSignAndVerifyAgentRelayJWT' -v
```

Expected:

- PASS for all token tests

- [ ] **Step 5: Commit**

```bash
git add signaling-server/internal/relay/tokens.go signaling-server/internal/relay/tokens_test.go signaling-server/internal/config/config.go signaling-server/go.mod signaling-server/go.sum
git commit -m "feat(relay): add signed relay jwt helpers"
```

## Task 3: Build The In-Memory Relay Session Registry

**Files:**
- Create: `signaling-server/internal/relay/registry.go`
- Create: `signaling-server/internal/relay/registry_test.go`

- [ ] **Step 1: Write the failing registry tests**

```go
func TestRegistry_BrowserWaitsForAgentWithinPendingWindow(t *testing.T) {
	reg := NewRegistry(2 * time.Second)
	now := time.Unix(1_800_000_000, 0)

	require.NoError(t, reg.CreatePendingSession(PendingSession{
		SID:          "sid-1",
		AccountID:    "acct-1",
		SessionCode:  "SHARE123",
		RelayAllowed: true,
		JTI:          "jti-1",
		ExpiresAt:    now.Add(2 * time.Minute),
	}, now))

	browserStatePeer, browserState, err := reg.BindBrowserSocket("sid-1", "jti-1", nil, now)
	require.NoError(t, err)
	require.Nil(t, browserStatePeer)
	require.Equal(t, StatePendingBrowser, browserState)

	waitDone := make(chan *websocket.Conn, 1)
	go func() {
		peer, waitErr := reg.WaitForAgent("sid-1", now)
		require.NoError(t, waitErr)
		waitDone <- peer
	}()

	agentStatePeer, agentState, err := reg.BindAgentSocket("sid-1", "agent-1", nil, now.Add(500*time.Millisecond))
	require.NoError(t, err)
	require.Nil(t, agentStatePeer)
	require.Equal(t, StateActive, agentState)
	require.Nil(t, <-waitDone)
}

func TestRegistry_AgentWaitsForBrowserWithinPendingWindow(t *testing.T) {
	reg := NewRegistry(2 * time.Second)
	now := time.Unix(1_800_000_000, 0)

	require.NoError(t, reg.CreatePendingSession(PendingSession{
		SID:          "sid-2",
		AccountID:    "acct-1",
		SessionCode:  "SHARE456",
		AgentID:      "agent-1",
		RelayAllowed: true,
		JTI:          "jti-2",
		ExpiresAt:    now.Add(2 * time.Minute),
	}, now))

	agentStatePeer, agentState, err := reg.BindAgentSocket("sid-2", "agent-1", nil, now)
	require.NoError(t, err)
	require.Nil(t, agentStatePeer)
	require.Equal(t, StatePendingAgent, agentState)

	waitDone := make(chan *websocket.Conn, 1)
	go func() {
		peer, waitErr := reg.WaitForBrowser("sid-2", now)
		require.NoError(t, waitErr)
		waitDone <- peer
	}()

	browserStatePeer, browserState, err := reg.BindBrowserSocket("sid-2", "jti-2", nil, now.Add(500*time.Millisecond))
	require.NoError(t, err)
	require.Nil(t, browserStatePeer)
	require.Equal(t, StateActive, browserState)
	require.Nil(t, <-waitDone)
}

func TestRegistry_RejectsSpentJTIReplay(t *testing.T) {
	reg := NewRegistry(2 * time.Second)
	now := time.Unix(1_800_000_000, 0)

	require.NoError(t, reg.CreatePendingSession(PendingSession{SID: "sid-1", JTI: "jti-1", RelayAllowed: true, ExpiresAt: now.Add(2 * time.Minute)}, now))
	_, _, err := reg.BindBrowserSocket("sid-1", "jti-1", nil, now)
	require.NoError(t, err)

	_, _, err = reg.BindBrowserSocket("sid-1", "jti-1", nil, now.Add(100*time.Millisecond))
	require.ErrorIs(t, err, ErrReplay)
}

func TestRegistry_CleanupDirectSuccessRemovesPendingSession(t *testing.T) {
	reg := NewRegistry(2 * time.Second)
	now := time.Unix(1_800_000_000, 0)

	require.NoError(t, reg.CreatePendingSession(PendingSession{SID: "sid-1", JTI: "jti-1", RelayAllowed: true, ExpiresAt: now.Add(2 * time.Minute)}, now))
	reg.MarkDirectSuccess("sid-1")

	_, err := reg.Get("sid-1")
	require.ErrorIs(t, err, ErrUnknownSID)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
cd signaling-server && go test ./internal/relay -run 'TestRegistry_BrowserWaitsForAgentWithinPendingWindow|TestRegistry_AgentWaitsForBrowserWithinPendingWindow|TestRegistry_RejectsSpentJTIReplay|TestRegistry_CleanupDirectSuccessRemovesPendingSession' -v
```

Expected:

- FAIL because registry state and errors do not exist

- [ ] **Step 3: Write the minimal registry implementation**

```go
// signaling-server/internal/relay/registry.go
package relay

import (
	"errors"
	"sync"
	"time"

	"github.com/coder/websocket"
)

var (
	ErrUnknownSID = errors.New("relay: unknown sid")
	ErrReplay     = errors.New("relay: replayed jti")
)

type SessionState string

const (
	StatePendingBrowser SessionState = "pending_browser"
	StatePendingAgent   SessionState = "pending_agent"
	StateActive         SessionState = "active"
)

type PendingSession struct {
	SID               string
	AccountID         string
	SessionCode       string
	AgentID           string
	RelayAllowed      bool
	RelayOnly         bool
	ExpectedStaticPub string
	JTI               string
	ExpiresAt         time.Time
}

type Registry struct {
	mu             sync.Mutex
	pendingWindow  time.Duration
	sessions       map[string]*sessionEntry
	spentJTI       map[string]time.Time
}

type sessionEntry struct {
	session        PendingSession
	agentSocket    *websocket.Conn
	browserSocket  *websocket.Conn
	forwardedBytes int64
	agentReady     chan struct{}
	browserReady   chan struct{}
	done           chan struct{}
}

func NewRegistry(pendingWindow time.Duration) *Registry {
	return &Registry{
		pendingWindow:  pendingWindow,
		sessions:       make(map[string]*sessionEntry),
		spentJTI:       make(map[string]time.Time),
	}
}

func (r *Registry) CreatePendingSession(session PendingSession, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[session.SID] = &sessionEntry{
		session:      session,
		agentReady:   make(chan struct{}),
		browserReady: make(chan struct{}),
		done:         make(chan struct{}),
	}
	return nil
}

func (r *Registry) BindBrowserSocket(sid, jti string, conn *websocket.Conn, now time.Time) (*websocket.Conn, SessionState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, spent := r.spentJTI[jti]; spent {
		return nil, "", ErrReplay
	}
	entry, ok := r.sessions[sid]
	if !ok || now.After(entry.session.ExpiresAt) {
		return nil, "", ErrUnknownSID
	}
	r.spentJTI[jti] = now
	entry.browserSocket = conn
	select {
	case <-entry.browserReady:
	default:
		close(entry.browserReady)
	}
	select {
	case <-entry.agentReady:
		return entry.agentSocket, StateActive, nil
	default:
	}
	return nil, StatePendingBrowser, nil
}

func (r *Registry) WaitForAgent(sid string, now time.Time) (*websocket.Conn, error) {
	r.mu.Lock()
	entry, ok := r.sessions[sid]
	if !ok {
		r.mu.Unlock()
		return nil, ErrUnknownSID
	}
	deadline := now.Add(r.pendingWindow)
	if entry.session.ExpiresAt.Before(deadline) {
		deadline = entry.session.ExpiresAt
	}
	ready := entry.agentReady
	r.mu.Unlock()

	select {
	case <-ready:
	case <-time.After(time.Until(deadline)):
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.sessions, sid)
		return nil, errors.New("relay: pending wait window exceeded")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok = r.sessions[sid]
	if !ok {
		return nil, ErrUnknownSID
	}
	return entry.agentSocket, nil
}

func (r *Registry) BindAgentSocket(sid, agentID string, conn *websocket.Conn, now time.Time) (*websocket.Conn, SessionState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.sessions[sid]
	if !ok || now.After(entry.session.ExpiresAt) {
		return nil, "", ErrUnknownSID
	}
	if entry.session.AgentID != "" && entry.session.AgentID != agentID {
		return nil, "", ErrUnknownSID
	}
	entry.agentSocket = conn
	select {
	case <-entry.agentReady:
	default:
		close(entry.agentReady)
	}
	select {
	case <-entry.browserReady:
		return entry.browserSocket, StateActive, nil
	default:
	}
	return nil, StatePendingAgent, nil
}

func (r *Registry) WaitForBrowser(sid string, now time.Time) (*websocket.Conn, error) {
	r.mu.Lock()
	entry, ok := r.sessions[sid]
	if !ok {
		r.mu.Unlock()
		return nil, ErrUnknownSID
	}
	deadline := now.Add(r.pendingWindow)
	if entry.session.ExpiresAt.Before(deadline) {
		deadline = entry.session.ExpiresAt
	}
	ready := entry.browserReady
	r.mu.Unlock()

	select {
	case <-ready:
	case <-time.After(time.Until(deadline)):
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.sessions, sid)
		return nil, errors.New("relay: pending wait window exceeded")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok = r.sessions[sid]
	if !ok {
		return nil, ErrUnknownSID
	}
	return entry.browserSocket, nil
}

func (r *Registry) WatchSession(sid string) (<-chan struct{}, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.sessions[sid]
	if !ok {
		return nil, ErrUnknownSID
	}
	return entry.done, nil
}

func (r *Registry) AddForwardedBytes(sid string, n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry, ok := r.sessions[sid]; ok {
		entry.forwardedBytes += n
	}
}

func (r *Registry) Get(sid string) (PendingSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.sessions[sid]
	if !ok {
		return PendingSession{}, ErrUnknownSID
	}
	return entry.session, nil
}

func (r *Registry) MarkDirectSuccess(sid string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, sid)
}

func (r *Registry) CloseSession(sid string) (string, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.sessions[sid]
	if !ok {
		return "", 0, ErrUnknownSID
	}
	accountID := entry.session.AccountID
	bytes := entry.forwardedBytes
	select {
	case <-entry.done:
	default:
		close(entry.done)
	}
	delete(r.sessions, sid)
	return accountID, bytes, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run:

```bash
cd signaling-server && go test ./internal/relay -run 'TestRegistry_BrowserWaitsForAgentWithinPendingWindow|TestRegistry_AgentWaitsForBrowserWithinPendingWindow|TestRegistry_RejectsSpentJTIReplay|TestRegistry_CleanupDirectSuccessRemovesPendingSession' -v
```

Expected:

- PASS for the registry lifecycle tests

- [ ] **Step 5: Commit**

```bash
git add signaling-server/internal/relay/registry.go signaling-server/internal/relay/registry_test.go
git commit -m "feat(relay): add pending relay session registry"
```

## Task 4: Implement Relay-Native Byte Accounting

**Files:**
- Create: `signaling-server/internal/relay/accounting.go`
- Create: `signaling-server/internal/relay/accounting_test.go`

- [ ] **Step 1: Write the failing accounting tests**

```go
func setupRelayTestApp(t *testing.T) (core.App, func()) {
	testApp, err := tests.NewTestApp(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, testApp.Bootstrap())
	require.NoError(t, testApp.RunSystemMigrations())
	require.NoError(t, migrations.CreateCollections(testApp))
	require.NoError(t, migrations.AddAPIKeyTimestamps(testApp))
	require.NoError(t, migrations.AddQuotaFields(testApp))
	cleanup := func() { testApp.Cleanup() }
	return testApp, cleanup
}

func createRelayTestAccountWithQuota(app core.App, email string, quotaGB float64, usageGB float64) (*core.Record, error) {
	usersCol, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		return nil, err
	}
	user := core.NewRecord(usersCol)
	user.SetEmail(email)
	user.SetPassword("testpassword123")
	user.Set("relay_quota_gb", quotaGB)
	user.Set("current_period_usage_gb", usageGB)
	user.Set("quota_period_start", time.Now().UTC())
	user.Set("quota_period_end", time.Now().UTC().Add(30*24*time.Hour))
	user.Set("turn_baseline_bytes", 0.0)
	if err := app.Save(user); err != nil {
		return nil, err
	}
	return user, nil
}

func TestApplyRelayBytes_IncrementsCurrentPeriodUsage(t *testing.T) {
	app, cleanup := setupRelayTestApp(t)
	defer cleanup()

	user, err := createRelayTestAccountWithQuota(app, "acct@example.com", 50.0, 0.0)
	require.NoError(t, err)

	now := time.Now().UTC()
	require.NoError(t, ApplyRelayBytes(app, user.Id, 5_000_000, now))

	updated, err := app.FindRecordById("users", user.Id)
	require.NoError(t, err)
	require.InDelta(t, 0.005, updated.GetFloat("current_period_usage_gb"), 0.000001)
}

func TestApplyRelayBytes_RollsExpiredPeriodAndArchivesOldUsage(t *testing.T) {
	app, cleanup := setupRelayTestApp(t)
	defer cleanup()

	user, err := createRelayTestAccountWithQuota(app, "expired@example.com", 50.0, 3.25)
	require.NoError(t, err)
	user.Set("quota_period_start", time.Now().UTC().Add(-40*24*time.Hour))
	user.Set("quota_period_end", time.Now().UTC().Add(-24*time.Hour))
	require.NoError(t, app.Save(user))

	require.NoError(t, ApplyRelayBytes(app, user.Id, 1_000_000_000, time.Now().UTC()))

	bw, err := app.FindRecordsByFilter("bandwidth_usage", "account_id = {:id}", "", 10, 0, map[string]any{"id": user.Id})
	require.NoError(t, err)
	require.Len(t, bw, 1)
	require.Equal(t, int64(3_250_000_000), int64(bw[0].GetFloat("bytes_transferred")))
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
cd signaling-server && go test ./internal/relay -run 'TestApplyRelayBytes_IncrementsCurrentPeriodUsage|TestApplyRelayBytes_RollsExpiredPeriodAndArchivesOldUsage' -v
```

Expected:

- FAIL because `ApplyRelayBytes` does not exist

- [ ] **Step 3: Write the minimal accounting implementation**

```go
// signaling-server/internal/relay/accounting.go
package relay

import (
	"fmt"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

func ApplyRelayBytes(app core.App, accountID string, forwardedBytes int64, now time.Time) error {
	record, err := app.FindRecordById("users", accountID)
	if err != nil {
		return err
	}

	periodEnd := record.GetDateTime("quota_period_end").Time()
	if !periodEnd.IsZero() && now.After(periodEnd) {
		if err := rollQuotaPeriod(app, record, now); err != nil {
			return err
		}
		record, err = app.FindRecordById("users", accountID)
		if err != nil {
			return err
		}
	}

	current := record.GetFloat("current_period_usage_gb")
	record.Set("current_period_usage_gb", current+(float64(forwardedBytes)/1e9))
	return app.Save(record)
}

func rollQuotaPeriod(app core.App, record *core.Record, now time.Time) error {
	bwCol, err := app.FindCollectionByNameOrId("bandwidth_usage")
	if err != nil {
		return err
	}
	archive := core.NewRecord(bwCol)
	archive.Set("account_id", record.Id)
	archive.Set("period_start", record.GetDateTime("quota_period_start").Time())
	archive.Set("period_end", record.GetDateTime("quota_period_end").Time())
	archive.Set("bytes_transferred", int64(record.GetFloat("current_period_usage_gb")*1e9))
	if err := app.Save(archive); err != nil {
		return fmt.Errorf("archive quota period: %w", err)
	}
	record.Set("current_period_usage_gb", 0.0)
	record.Set("quota_period_start", now.UTC())
	record.Set("quota_period_end", now.UTC().Add(30*24*time.Hour))
	return app.Save(record)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run:

```bash
cd signaling-server && go test ./internal/relay -run 'TestApplyRelayBytes_IncrementsCurrentPeriodUsage|TestApplyRelayBytes_RollsExpiredPeriodAndArchivesOldUsage' -v
```

Expected:

- PASS for both accounting tests

- [ ] **Step 5: Commit**

```bash
git add signaling-server/internal/relay/accounting.go signaling-server/internal/relay/accounting_test.go
git commit -m "feat(relay): add server-side relay byte accounting"
```

## Task 5: Add The `/ws/relay` Handler And Opaque Forwarding

**Files:**
- Create: `signaling-server/internal/handler/relay_ws.go`
- Create: `signaling-server/internal/handler/relay_ws_test.go`
- Modify: `signaling-server/cmd/server/main.go`

- [ ] **Step 1: Write the failing relay handler tests**

```go
func TestRelayWS_AgentAndBrowserPairAndForwardOpaqueBytes(t *testing.T) {
	app, cleanup := setupBrowserTestApp(t)
	defer cleanup()

	reg := relay.NewRegistry(2 * time.Second)
	cfg := config.Load()
	cfg.RelayJWTSecret = "secret"

	mux := http.NewServeMux()
	mux.Handle("/ws/relay", http.HandlerFunc(RelayWS(app, reg, cfg)))
	server := httptest.NewServer(mux)
	defer server.Close()

	now := time.Unix(1_800_000_000, 0)
	require.NoError(t, reg.CreatePendingSession(relay.PendingSession{
		SID:          "sid-1",
		AccountID:    "acct-1",
		AgentID:      "agent-1",
		RelayAllowed: true,
		JTI:          "jti-1",
		ExpiresAt:    now.Add(relay.TokenLifetime),
	}, now))
	agentToken, _ := relay.SignAgentRelayJWT("secret", relay.AgentRelayClaims{SID: "sid-1", AgentID: "agent-1"}, now)
	browserToken, _ := relay.SignBrowserPolicyJWT("secret", relay.BrowserPolicyClaims{
		SID:              "sid-1",
		RelayAllowed:     true,
		RegisteredClaims: jwt.RegisteredClaims{ID: "jti-1"},
	}, now)

	ctx := context.Background()
	agentConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/relay", nil)
	require.NoError(t, err)
	defer agentConn.CloseNow()
	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"token":"`+agentToken+`"}`)))

	browserConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/relay", nil)
	require.NoError(t, err)
	defer browserConn.CloseNow()
	require.NoError(t, browserConn.Write(ctx, websocket.MessageText, []byte(`{"token":"`+browserToken+`"}`)))

	require.NoError(t, browserConn.Write(ctx, websocket.MessageBinary, []byte("ciphertext")))
	typ, data, err := agentConn.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, websocket.MessageBinary, typ)
	require.Equal(t, []byte("ciphertext"), data)
}

func TestRelayWS_RejectsReplayedBrowserJTI(t *testing.T) {
	app, cleanup := setupBrowserTestApp(t)
	defer cleanup()

	reg := relay.NewRegistry(2 * time.Second)
	cfg := config.Load()
	cfg.RelayJWTSecret = "secret"

	require.NoError(t, reg.CreatePendingSession(relay.PendingSession{
		SID:          "sid-2",
		AccountID:    "acct-1",
		AgentID:      "agent-1",
		RelayAllowed: true,
		JTI:          "jti-2",
		ExpiresAt:    time.Unix(1_800_000_000, 0).Add(relay.TokenLifetime),
	}, time.Unix(1_800_000_000, 0)))

	mux := http.NewServeMux()
	mux.Handle("/ws/relay", http.HandlerFunc(RelayWS(app, reg, cfg)))
	server := httptest.NewServer(mux)
	defer server.Close()

	token, _ := relay.SignBrowserPolicyJWT("secret", relay.BrowserPolicyClaims{
		SID:              "sid-2",
		RelayAllowed:     true,
		RegisteredClaims: jwt.RegisteredClaims{ID: "jti-2"},
	}, time.Unix(1_800_000_000, 0))
	ctx := context.Background()

	firstConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/relay", nil)
	require.NoError(t, err)
	defer firstConn.CloseNow()
	require.NoError(t, firstConn.Write(ctx, websocket.MessageText, []byte(`{"token":"`+token+`"}`)))

	secondConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/relay", nil)
	require.NoError(t, err)
	defer secondConn.CloseNow()
	require.NoError(t, secondConn.Write(ctx, websocket.MessageText, []byte(`{"token":"`+token+`"}`)))

	_, _, err = secondConn.Read(ctx)
	require.Error(t, err)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
cd signaling-server && go test ./internal/handler -run 'TestRelayWS_AgentAndBrowserPairAndForwardOpaqueBytes|TestRelayWS_RejectsReplayedBrowserJTI' -v
```

Expected:

- FAIL because `RelayWS` does not exist

- [ ] **Step 3: Write the minimal relay handler implementation**

```go
// signaling-server/internal/handler/relay_ws.go
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/pocketbase/pocketbase/core"
	"sharebridge/server/internal/config"
	"sharebridge/server/internal/relay"
)

type relayHello struct {
	Token string `json:"token"`
}

func RelayWS(app core.App, reg *relay.Registry, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()

		ctx := r.Context()
		_, payload, err := conn.Read(ctx)
		if err != nil {
			return
		}

		var hello relayHello
		if err := json.Unmarshal(payload, &hello); err != nil {
			conn.Close(websocket.StatusPolicyViolation, "invalid relay hello")
			return
		}

		if claims, err := relay.VerifyAgentRelayJWT(cfg.RelayJWTSecret, hello.Token, time.Now()); err == nil {
			peer, state, bindErr := reg.BindAgentSocket(claims.SID, claims.AgentID, conn, time.Now())
			if bindErr != nil {
				conn.Close(websocket.StatusPolicyViolation, bindErr.Error())
				return
			}
			if state == relay.StatePendingAgent {
				peer, bindErr = reg.WaitForBrowser(claims.SID, time.Now())
				if bindErr != nil {
					conn.Close(websocket.StatusPolicyViolation, bindErr.Error())
					return
				}
				if peer != nil {
					proxyRelayPair(ctx, app, reg, claims.SID, conn, peer)
				}
				return
			}
			if state == relay.StateActive {
				done, watchErr := reg.WatchSession(claims.SID)
				if watchErr == nil {
					<-done
				}
				return
			}
			return
		}
		if claims, err := relay.VerifyBrowserPolicyJWT(cfg.RelayJWTSecret, hello.Token, time.Now()); err == nil {
			peer, state, bindErr := reg.BindBrowserSocket(claims.SID, claims.RegisteredClaims.ID, conn, time.Now())
			if bindErr != nil {
				conn.Close(websocket.StatusPolicyViolation, bindErr.Error())
				return
			}
			if state == relay.StatePendingBrowser {
				peer, bindErr = reg.WaitForAgent(claims.SID, time.Now())
				if bindErr != nil {
					conn.Close(websocket.StatusPolicyViolation, bindErr.Error())
					return
				}
				if peer != nil {
					proxyRelayPair(ctx, app, reg, claims.SID, conn, peer)
				}
				return
			}
			if state == relay.StateActive {
				done, watchErr := reg.WatchSession(claims.SID)
				if watchErr == nil {
					<-done
				}
				return
			}
			return
		}

		conn.Close(websocket.StatusPolicyViolation, "invalid relay token")
	}
}

func proxyRelayPair(ctx context.Context, app core.App, reg *relay.Registry, sid string, left, right *websocket.Conn) {
	var once sync.Once
	closeAndFlush := func() {
		once.Do(func() {
			_ = left.Close(websocket.StatusNormalClosure, "relay session closed")
			_ = right.Close(websocket.StatusNormalClosure, "relay session closed")
			accountID, forwardedBytes, err := reg.CloseSession(sid)
			if err == nil && forwardedBytes > 0 {
				_ = relay.ApplyRelayBytes(app, accountID, forwardedBytes, time.Now().UTC())
			}
		})
	}

	go forwardRelayFrames(ctx, reg, sid, right, left, closeAndFlush)
	forwardRelayFrames(ctx, reg, sid, left, right, closeAndFlush)
}

func forwardRelayFrames(ctx context.Context, reg *relay.Registry, sid string, src, dst *websocket.Conn, onClose func()) {
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			onClose()
			return
		}
		reg.AddForwardedBytes(sid, int64(len(data)))
		if err := dst.Write(ctx, typ, data); err != nil {
			onClose()
			return
		}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run:

```bash
cd signaling-server && go test ./internal/handler -run 'TestRelayWS_AgentAndBrowserPairAndForwardOpaqueBytes|TestRelayWS_RejectsReplayedBrowserJTI' -v
```

Expected:

- PASS for relay endpoint auth and forwarding tests

- [ ] **Step 5: Commit**

```bash
git add signaling-server/internal/handler/relay_ws.go signaling-server/internal/handler/relay_ws_test.go signaling-server/cmd/server/main.go
git commit -m "feat(relay): add relay websocket endpoint"
```

## Task 6: Integrate Relay Preparation Into The Existing Signaling Flow

**Files:**
- Modify: `signaling-server/internal/handler/agent_ws.go`
- Modify: `signaling-server/internal/handler/agent_ws_test.go`
- Modify: `signaling-server/internal/handler/browser_ws.go`
- Modify: `signaling-server/internal/handler/browser_ws_test.go`
- Modify: `signaling-server/internal/hub/hub.go`
- Modify: `signaling-server/internal/hub/hub_test.go`
- Modify: `signaling-server/cmd/server/main.go`

- [ ] **Step 1: Write the failing signaling integration tests**

```go
func TestAgentWS_AuthOK_SendsRelayPrepareToAgentAndRelayPolicyToBrowser(t *testing.T) {
	app, cleanup := setupBrowserTestApp(t)
	defer cleanup()

	user, err := createTestAccountWithQuota(app, "relayplan@example.com", 50.0, 0.0)
	require.NoError(t, err)
	apiKey, err := createTestAPIKeyForUser(app, user.Id, "sigsecret")
	require.NoError(t, err)
	_, err = createTestSessionWithAPIKey(app, apiKey.Id, "agent-1", "PLANA001")
	require.NoError(t, err)

	h := hub.New()
	cfg := config.Load()
	reg := relay.NewRegistry(2 * time.Second)
	cfg.RelayJWTSecret = "secret"

	fullKey := apiKey.Id + ".sigsecret"
	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, reg, cfg)
	browserHandler := BrowserWS(app, h, cfg)
	mux := http.NewServeMux()
	mux.Handle("/ws/agent", authMiddleware(http.HandlerFunc(agentHandler)))
	mux.Handle("/ws/client", http.HandlerFunc(browserHandler))

	server := httptest.NewServer(mux)
	defer server.Close()

	ctx := context.Background()
	agentConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent?api_key="+fullKey, nil)
	require.NoError(t, err)
	defer agentConn.CloseNow()
	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","agent_id":"agent-1"}`)))
	_, _, err = agentConn.Read(ctx)
	require.NoError(t, err)
	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","code":"PLANA001","relay_static_pub":"04abcd"}`)))
	_, _, err = agentConn.Read(ctx)
	require.NoError(t, err)

	browserConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/client?session=PLANA001", nil)
	require.NoError(t, err)
	defer browserConn.CloseNow()
	_, _, err = browserConn.Read(ctx) // ice_config
	require.NoError(t, err)
	require.NoError(t, browserConn.Write(ctx, websocket.MessageText, []byte(`{"type":"knock"}`)))
	_, nonceMsg, err := browserConn.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, string(nonceMsg), `"type":"nonce"`)
	var noncePayload struct {
		ConnID string `json:"conn_id"`
	}
	require.NoError(t, json.Unmarshal(nonceMsg, &noncePayload))

	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"auth_ok","conn_id":"`+noncePayload.ConnID+`","code":"PLANA001"}`)))

	_, agentRelayPrepare, err := agentConn.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, string(agentRelayPrepare), `"type":"relay_prepare"`)

	_, browserRelayPolicy, err := browserConn.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, string(browserRelayPolicy), `"type":"relay_policy"`)
	require.Contains(t, string(browserRelayPolicy), `"relay_allowed":true`)
}

func TestBrowserWS_DirectFlowStillSendsICEConfigFirst(t *testing.T) {
	app, cleanup := setupBrowserTestApp(t)
	defer cleanup()

	user, err := createTestAccountWithQuota(app, "direct@example.com", 50.0, 0.0)
	require.NoError(t, err)
	apiKey, err := createTestAPIKeyForUser(app, user.Id, "directsecret")
	require.NoError(t, err)
	_, err = createTestSessionWithAPIKey(app, apiKey.Id, "agent-direct", "DIRECT001")
	require.NoError(t, err)

	h := hub.New()
	cfg := config.Load()
	reg := relay.NewRegistry(2 * time.Second)
	fullKey := apiKey.Id + ".directsecret"

	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, reg, cfg)
	mux := http.NewServeMux()
	mux.Handle("/ws/agent", authMiddleware(http.HandlerFunc(agentHandler)))
	mux.Handle("/ws/client", http.HandlerFunc(BrowserWS(app, h, cfg)))

	server := httptest.NewServer(mux)
	defer server.Close()

	ctx := context.Background()
	agentConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent?api_key="+fullKey, nil)
	require.NoError(t, err)
	defer agentConn.CloseNow()
	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","agent_id":"agent-direct"}`)))
	_, _, err = agentConn.Read(ctx)
	require.NoError(t, err)
	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","code":"DIRECT001"}`)))
	_, _, err = agentConn.Read(ctx)
	require.NoError(t, err)

	browserConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/client?session=DIRECT001", nil)
	require.NoError(t, err)
	defer browserConn.CloseNow()

	_, firstMsg, err := browserConn.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, string(firstMsg), `"type":"ice_config"`)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
cd signaling-server && go test ./internal/handler ./internal/hub -run 'TestAgentWS_AuthOK_SendsRelayPrepareToAgentAndRelayPolicyToBrowser|TestBrowserWS_DirectFlowStillSendsICEConfigFirst' -v
```

Expected:

- FAIL because `auth_ok`, `relay_prepare`, and `relay_policy` do not exist yet

- [ ] **Step 3: Write the minimal additive signaling implementation**

```go
// signaling-server/internal/handler/agent_ws.go
// signature change:
// func AgentWS(app core.App, h *hub.Hub, reg *relay.Registry, cfg *config.Config) http.HandlerFunc

case "auth_ok":
	if agentID == "" {
		continue
	}

	session, err := getSessionByCode(app, msg.Code)
	if err != nil || session == nil {
		continue
	}

	sid := relay.NewSID()
	now := time.Now().UTC()

	browserClaims := relay.BrowserPolicyClaims{
		SID:               sid,
		SessionCode:       msg.Code,
		RelayAllowed:      true,
		RelayOnly:         session.GetBool("relay_only"),
		ExpectedStaticPub: session.GetString("relay_static_pub"),
		RegisteredClaims:  jwt.RegisteredClaims{ID: relay.NewJTI()},
	}
	browserJWT, _ := relay.SignBrowserPolicyJWT(cfg.RelayJWTSecret, browserClaims, now)
	agentJWT, _ := relay.SignAgentRelayJWT(cfg.RelayJWTSecret, relay.AgentRelayClaims{SID: sid, AgentID: agentID}, now)

	_ = reg.CreatePendingSession(relay.PendingSession{
		SID:               sid,
		AccountID:         accountID,
		SessionCode:       msg.Code,
		AgentID:           agentID,
		RelayAllowed:      browserClaims.RelayAllowed,
		RelayOnly:         browserClaims.RelayOnly,
		ExpectedStaticPub: browserClaims.ExpectedStaticPub,
		JTI:               browserClaims.RegisteredClaims.ID,
		ExpiresAt:         now.Add(relay.TokenLifetime),
	}, now)

	hub.SendDirect(ctx, conn, map[string]any{
		"type":       "relay_prepare",
		"sid":        sid,
		"expires_at": now.Add(relay.TokenLifetime).Format(time.RFC3339),
		"relay_jwt":  agentJWT,
	})

	h.ForwardToBrowserByConnID(ctx, msg.ConnID, map[string]any{
		"type":          "relay_policy",
		"token":         browserJWT,
		"relay_allowed": browserClaims.RelayAllowed,
		"relay_only":    browserClaims.RelayOnly,
	})

// signaling-server/cmd/server/main.go
reg := relay.NewRegistry(cfg.RelayPendingWaitWindow)
if cfg.RelayJWTSecret == "" {
	log.Fatal("RELAY_JWT_SECRET must be set when relay endpoints are enabled")
}
handlerFunc := handler.AgentWS(app, h, reg, cfg)
```

- [ ] **Step 4: Run focused tests, then the backend suite**

Run:

```bash
cd signaling-server && go test ./internal/handler ./internal/hub ./internal/relay ./migrations -v
```

Expected:

- PASS for the new relay tests
- PASS for existing browser/agent/hub/migration tests
- direct-mode tests still green

- [ ] **Step 5: Commit**

```bash
git add signaling-server/internal/handler/agent_ws.go signaling-server/internal/handler/agent_ws_test.go signaling-server/internal/handler/browser_ws.go signaling-server/internal/handler/browser_ws_test.go signaling-server/internal/hub/hub.go signaling-server/internal/hub/hub_test.go signaling-server/cmd/server/main.go
git commit -m "feat(relay): prepare relay sessions after auth success"
```

## Self-Review

### Spec coverage

- Relay JWT format, expiry, and purpose checks: covered by Task 2.
- Pending relay session state, `jti` replay, pending wait window, direct-success cleanup: covered by Task 3.
- Server-authoritative relay byte accounting: covered by Task 4.
- Dedicated relay WebSocket endpoint and opaque forwarding: covered by Task 5.
- Agent pre-registration notification, browser policy issuance after auth, expected agent static key propagation: covered by Tasks 1 and 6.
- Direct-mode compatibility and additive rollout: covered by Task 6 tests.

### Placeholder scan

- No `TODO`, `TBD`, or “similar to” references remain.
- Every task includes exact file paths, test code, and commands.

### Type consistency

- `TransferChannel` is not implemented in Plan A and therefore not referenced as a code type here.
- Relay JWT helpers use `BrowserPolicyClaims`, `AgentRelayClaims`, and `TokenLifetime` consistently across Tasks 2, 5, and 6.
- The persisted session field name is consistently `relay_static_pub`.

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-04-19-secure-relay-a-signaling-relay-backend.md`. Two execution options:

**1. Subagent-Driven (recommended)** - I dispatch a fresh subagent per task, review between tasks, fast iteration

**2. Inline Execution** - Execute tasks in this session using executing-plans, batch execution with checkpoints

**Which approach?**
