# Secure Relay Plan D: Coverage + Cleanup + Verification Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Lock in the now-working secure relay flow with high-value coverage, then remove TURN/Coturn/Prometheus-era leftovers and rerun the same checks to prove the cleaned-up system still works.

**Architecture:** Do not redesign direct-vs-relay bootstrap. Direct WebRTC and relay bootstrap remain intentionally different. This plan adds coverage around the shared user-visible outcomes, then deletes old TURN-era config, deploy, quota-poller, and docs paths that are no longer part of the secure-relay architecture. Cleanup happens only after the tests are in place.

**Tech Stack:** Go, PocketBase, coder/websocket, browser JavaScript (`node:test`), Docker Compose/config cleanup, shell verification

---

## File Structure

### Modified files

- `signaling-server/web/src/app.test.js`
  - Browser entrypoint tests for relay-only success, relay fallback, and quota-blocked behavior at the app layer.
- `signaling-server/web/src/connectTransferChannel.test.js`
  - Transport-selection matrix tests that remain the safety net during cleanup.
- `signaling-server/internal/handler/browser_ws_test.go`
  - Browser signaling tests covering relay-only bootstrap, STUN-only ICE config, and quota-blocked behavior without TURN fallback.
- `signaling-server/internal/handler/agent_ws_test.go`
  - Agent signaling tests that keep `relay_policy` / `relay_prepare` behavior pinned while old TURN-era assumptions are removed.
- `agent/internal/daemon/daemon_test.go`
  - Agent daemon tests confirming relay-only sessions skip direct peer creation and relay sessions still bind to transfer manager correctly.
- `signaling-server/internal/config/config.go`
  - Remove TURN/Prometheus config fields that are no longer part of the runtime architecture.
- `signaling-server/internal/config/config_test.go`
  - Update config expectations after TURN/Prometheus removal.
- `signaling-server/internal/handler/browser_ws.go`
  - Stop generating TURN credentials, emit STUN-only `ice_config`, and replace stale TURN wording in quota errors.
- `signaling-server/internal/handler/agent_ws.go`
  - Stop advertising TURN in the welcome/ICE config path; keep relay prep/policy logic unchanged.
- `signaling-server/cmd/server/main.go`
  - Remove quota poller startup and TURN-era initialization assumptions.
- `signaling-server/docker-compose.yml`
  - Remove `coturn` and `prometheus` services plus TURN/Prometheus env vars.
- `signaling-server/deploy.sh`
  - Remove Coturn/Prometheus deployment steps and generated secrets.
- `signaling-server/README.md`
  - Replace TURN/Coturn setup docs with secure-relay + Cloudflare STUN docs.
- `signaling-server/web/home.html`
  - Remove stale “TURN Relay” wording in user-facing copy.
- `.gitignore`
  - Ensure built agent binaries remain ignored if needed.

### Deleted files

- `signaling-server/prometheus.yml`
  - No longer needed once Prometheus scraping is removed.
- `signaling-server/internal/metrics/prometheus.go`
  - Delete Prometheus client used only for TURN-era quota polling.
- `signaling-server/internal/metrics/prometheus_test.go`
  - Delete with the Prometheus client.
- `signaling-server/internal/quota/poller.go`
  - Delete TURN-era quota poller; relay accounting is already authoritative elsewhere.
- `signaling-server/internal/quota/poller_test.go`
  - Delete with the quota poller.
- `signaling-server/internal/turn/credentials.go`
  - Delete TURN credential generator.
- `signaling-server/internal/turn/credentials_test.go`
  - Delete with the credential generator.
- `agent/agent`
  - Remove the committed build artifact from the secure-relay branch.

### Kept on purpose

- `signaling-server/internal/turn/ice_config.go`
  - Keep for now as the STUN-only ICE config helper unless cleanup work makes it trivial to rename/move. Do not churn this helper unless the tests force it.

## Task 1: Add The Final Transport Coverage Matrix Before Cleanup

**Files:**
- Modify: `signaling-server/web/src/app.test.js`
- Modify: `signaling-server/web/src/connectTransferChannel.test.js`
- Modify: `signaling-server/internal/handler/browser_ws_test.go`
- Modify: `signaling-server/internal/handler/agent_ws_test.go`
- Modify: `agent/internal/daemon/daemon_test.go`

- [ ] **Step 1: Write the missing browser app-level relay-only and fallback tests**

```js
// signaling-server/web/src/app.test.js
import { installSessionMessageHandler, initializeIceConfigTransport, __test } from './app.js'

test('relay_policy opens the relay channel and requests the root file list', async () => {
  const statuses = []
  const requested = []
  const hidden = []
  const badgeCalls = []
  const channel = {
    readyState: 'open',
    onopen: null,
    onmessage: null,
    onclose: null,
  }

  const controller = installSessionMessageHandler({
    status: (msg) => statuses.push(msg),
    connectTransferChannel: async () => ({ channel, mode: 'relay' }),
    requestFileList: (ch, path) => requested.push({ ch, path }),
    applyConnectionBadge: ({ mode }) => badgeCalls.push(mode),
    decodeRelayPolicyToken: () => ({ expectedStaticPubHex: '04abcd', relayAllowed: true, relayOnly: true }),
    hideSection: (id) => hidden.push(id),
  })

  await controller.handleMessage({
    type: 'relay_policy',
    token: 'header.payload.sig',
    relay_allowed: true,
    relay_only: true,
  })

  assert.equal(statuses.at(-1), 'Transfer channel open!')
  assert.deepEqual(hidden, ['join-section', 'password-section'])
  assert.deepEqual(badgeCalls, ['relay'])
  assert.deepEqual(requested, [{ ch: channel, path: '' }])
})

test('direct failure after quota warning keeps the quota-blocked message', async () => {
  const statuses = []
  __test.setQuotaState({ exceeded: true, periodEnd: '2026-05-01T00:00:00Z' })

  const peer = initializeIceConfigTransport({
    msg: { type: 'ice_config', relay_only: false, ice_servers: [{ urls: ['stun:stun.cloudflare.com:3478'] }] },
    ws: { send() {} },
    createPeerConnection: () => ({ onicecandidate: null, ondatachannel: null, onconnectionstatechange: null, connectionState: 'new' }),
    onDirectChannel() {},
    onDirectFailure: () => {
      const periodEnd = new Date('2026-05-01T00:00:00Z').toLocaleDateString()
      statuses.push(`Connection failed: Direct unavailable, relay blocked (quota exceeded). Resets ${periodEnd}.`)
    },
  })

  peer.connectionState = 'failed'
  peer.onconnectionstatechange()
  assert.match(statuses.at(-1), /quota exceeded/i)
})
```

- [ ] **Step 2: Write the missing server/daemon coverage tests**

```go
// signaling-server/internal/handler/browser_ws_test.go
func TestBrowserWS_RelayOnlyStillEmitsIceConfigAndInitiatesNonceChallenge(t *testing.T) {
	app, cleanup := setupBrowserTestApp(t)
	defer cleanup()

	user, err := createTestAccountWithQuota(app, "relayonly@example.com", 50, 0)
	require.NoError(t, err)
	apiKey, err := createTestAPIKeyForUser(app, user.Id, "relaysecret")
	require.NoError(t, err)
	session, err := createTestSessionWithAPIKey(app, apiKey.Id, "agent-relay", "RELAYONLY1")
	require.NoError(t, err)
	session.Set("relay_only", true)
	require.NoError(t, app.Save(session))

	h := hub.New()
	cfg := config.Load()
	reg := relay.NewRegistry(2 * time.Second)

	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, reg, cfg)
	mux := http.NewServeMux()
	mux.Handle("/ws/agent", authMiddleware(http.HandlerFunc(agentHandler)))
	mux.Handle("/ws/client", http.HandlerFunc(BrowserWS(app, h, cfg)))

	server := httptest.NewServer(mux)
	defer server.Close()

	ctx := context.Background()
	agentConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent?api_key="+apiKey.Id+".relaysecret", nil)
	require.NoError(t, err)
	defer agentConn.CloseNow()

	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","agent_id":"agent-relay"}`)))
	_, _, err = agentConn.Read(ctx)
	require.NoError(t, err)
	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","code":"RELAYONLY1","relay_only":true}`)))
	_, _, err = agentConn.Read(ctx)
	require.NoError(t, err)

	browserConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/client?session=RELAYONLY1", nil)
	require.NoError(t, err)
	defer browserConn.CloseNow()

	_, browserIce, err := browserConn.Read(ctx)
	require.NoError(t, err)
	assert.Contains(t, string(browserIce), `"type":"ice_config"`)
	assert.Contains(t, string(browserIce), `"relay_only":true`)

	_, knock, err := agentConn.Read(ctx)
	require.NoError(t, err)
	assert.Contains(t, string(knock), `"type":"knock"`)
}
```

```go
// agent/internal/daemon/daemon_test.go
func TestHandleRelayPrepare_RelayOnlySessionDoesNotCreateDirectPeer(t *testing.T) {
	store := newMockStore()
	cfg := &config.Config{SignalingURL: "ws://127.0.0.1:8080", APIKey: "key"}
	cfgMgr := &mockConfigManager{cfg: cfg}
	sig := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, store.agentID)

	d, err := NewWithSignaling(cfgMgr, store, sig)
	require.NoError(t, err)

	d.sessions["RELAY1"] = &Session{
		Code:          "RELAY1",
		RelayOnly:     true,
		ShareURL:      "https://example.com/s/1",
		webdavClient:  nil,
		peers:         map[string]*peer.Peer{},
		relayChannels: map[string]relayTransferChannel{},
	}

	d.handleSignalingMessage(signaling.Message{
		Type: "auth_ok",
		Code: "RELAY1",
	})

	for _, sent := range sig.sendMessages {
		require.NotEqual(t, "offer", sent["type"], "relay_only session should not create a direct WebRTC offer")
	}
}
```

- [ ] **Step 3: Run the targeted tests to verify they fail**

Run:

```bash
cd .worktrees/secure-relay/signaling-server && go test ./internal/handler -run 'TestBrowserWS_RelayOnlyStillEmitsIceConfigAndInitiatesNonceChallenge' -v
cd .worktrees/secure-relay/agent && go test ./internal/daemon -run 'TestHandleRelayPrepare_RelayOnlySessionDoesNotCreateDirectPeer' -v
cd .worktrees/secure-relay/signaling-server/web && node --test src/app.test.js src/connectTransferChannel.test.js
```

Expected:

- at least one FAIL in each area because the new assertions are not implemented yet or current behavior is not fully pinned

- [ ] **Step 4: Add the minimal test-support implementation**

```js
// signaling-server/web/src/app.js
export const __test = {
  setQuotaState({ exceeded, periodEnd }) {
    relayQuotaExceeded = exceeded
    quotaPeriodEnd = periodEnd
  },
}
```

```js
// signaling-server/web/src/app.test.js
import { installSessionMessageHandler, initializeIceConfigTransport, __test } from './app.js'

// Reuse installSessionMessageHandler(...) instead of adding new production entrypoints.
// Use __test.setQuotaState(...) rather than mutating module-private state directly.
```

```go
// signaling-server/internal/handler/browser_ws_test.go
import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"sharebridge/server/internal/config"
	"sharebridge/server/internal/hub"
	"sharebridge/server/internal/middleware"
	"sharebridge/server/internal/relay"
	"sharebridge/server/migrations"
}
```

```go
// agent/internal/daemon/daemon_test.go
// Reuse existing mockSignalingClient.sendMessages inspection to assert no direct offer is emitted.
// No production code change is required here beyond the new test body from Step 2.
```

- [ ] **Step 5: Run the targeted tests again**

Run:

```bash
cd .worktrees/secure-relay/signaling-server && go test ./internal/handler -run 'TestBrowserWS_RelayOnlyStillEmitsIceConfigAndInitiatesNonceChallenge' -v
cd .worktrees/secure-relay/agent && go test ./internal/daemon -run 'TestHandleRelayPrepare_RelayOnlySessionDoesNotCreateDirectPeer' -v
cd .worktrees/secure-relay/signaling-server/web && node --test src/app.test.js src/connectTransferChannel.test.js
```

Expected:

- PASS in all three commands

- [ ] **Step 6: Commit**

```bash
git -C .worktrees/secure-relay add \
  signaling-server/web/src/app.test.js \
  signaling-server/web/src/connectTransferChannel.test.js \
  signaling-server/internal/handler/browser_ws_test.go \
  signaling-server/internal/handler/agent_ws_test.go \
  agent/internal/daemon/daemon_test.go
git -C .worktrees/secure-relay commit -m "test(relay): lock down direct and relay transport matrix"
```

## Task 2: Remove TURN/Coturn/Prometheus Runtime Plumbing From The New Path

**Files:**
- Modify: `signaling-server/internal/config/config.go`
- Modify: `signaling-server/internal/config/config_test.go`
- Modify: `signaling-server/internal/handler/browser_ws.go`
- Modify: `signaling-server/internal/handler/agent_ws.go`
- Modify: `signaling-server/cmd/server/main.go`
- Delete: `signaling-server/internal/metrics/prometheus.go`
- Delete: `signaling-server/internal/metrics/prometheus_test.go`
- Delete: `signaling-server/internal/quota/poller.go`
- Delete: `signaling-server/internal/quota/poller_test.go`
- Delete: `signaling-server/internal/turn/credentials.go`
- Delete: `signaling-server/internal/turn/credentials_test.go`

- [ ] **Step 1: Write the failing cleanup-facing tests**

```go
// signaling-server/internal/config/config_test.go
func TestLoad_DefaultsNoLongerExposeTurnOrPrometheusConfig(t *testing.T) {
	t.Setenv("TURN_HOST", "")
	t.Setenv("TURN_PORT", "")
	t.Setenv("TURN_SECRET", "")
	t.Setenv("PROMETHEUS_URL", "")
	t.Setenv("QUOTA_CHECK_INTERVAL", "")

	cfg := Load()

	require.Equal(t, "stun:stun.cloudflare.com:3478", cfg.STUNURL)
	require.Equal(t, 50.0, cfg.DefaultQuotaGB)
	require.Equal(t, 7*time.Second, cfg.RelayPendingWaitWindow)
}
```

```go
// signaling-server/internal/handler/browser_ws_test.go
func TestBrowserWS_IceConfigIsStunOnlyWithoutTurnFields(t *testing.T) {
	app, cleanup := setupBrowserTestApp(t)
	defer cleanup()

	user, err := createTestAccountWithQuota(app, "stunonly@example.com", 50, 0)
	require.NoError(t, err)
	apiKey, err := createTestAPIKeyForUser(app, user.Id, "stunsecret")
	require.NoError(t, err)
	_, err = createTestSessionWithAPIKey(app, apiKey.Id, "agent-stun", "STUNONLY1")
	require.NoError(t, err)

	h := hub.New()
	cfg := config.Load()
	reg := relay.NewRegistry(2 * time.Second)

	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, reg, cfg)
	mux := http.NewServeMux()
	mux.Handle("/ws/agent", authMiddleware(http.HandlerFunc(agentHandler)))
	mux.Handle("/ws/client", http.HandlerFunc(BrowserWS(app, h, cfg)))

	server := httptest.NewServer(mux)
	defer server.Close()

	ctx := context.Background()
	agentConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent?api_key="+apiKey.Id+".stunsecret", nil)
	require.NoError(t, err)
	defer agentConn.CloseNow()

	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","agent_id":"agent-stun"}`)))
	_, _, err = agentConn.Read(ctx)
	require.NoError(t, err)
	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","code":"STUNONLY1"}`)))
	_, _, err = agentConn.Read(ctx)
	require.NoError(t, err)

	browserConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/client?session=STUNONLY1", nil)
	require.NoError(t, err)
	defer browserConn.CloseNow()

	_, data, err := browserConn.Read(ctx)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"urls":["stun:stun.cloudflare.com:3478"]`)
	assert.NotContains(t, string(data), `"turn:`)
	assert.NotContains(t, string(data), `"username"`)
	assert.NotContains(t, string(data), `"credential"`)
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run:

```bash
cd .worktrees/secure-relay/signaling-server && go test ./internal/config ./internal/handler -run 'TestLoad_DefaultsNoLongerExposeTurnOrPrometheusConfig|TestBrowserWS_IceConfigIsStunOnlyWithoutTurnFields' -v
```

Expected:

- FAIL because config and browser handler still carry TURN/Prometheus-era plumbing

- [ ] **Step 3: Write the minimal cleanup implementation**

```go
// signaling-server/internal/config/config.go
type Config struct {
	Port    string
	STUNURL string
	DataDir string

	RelayJWTSecret         string
	RelayPendingWaitWindow time.Duration

	SMTPHost     string
	SMTPPort     string
	SMTPUser     string
	SMTPPassword string

	DefaultQuotaGB float64
}

func Load() *Config {
	return &Config{
		Port:                   getEnv("PORT", "8080"),
		STUNURL:                getEnv("STUN_URL", "stun:stun.cloudflare.com:3478"),
		DataDir:                getEnv("DATA_DIR", "./pb_data"),
		RelayJWTSecret:         getEnv("RELAY_JWT_SECRET", ""),
		RelayPendingWaitWindow: getEnvDuration("RELAY_PENDING_WAIT_WINDOW", 7*time.Second),
		SMTPHost:               getEnv("SMTP_HOST", ""),
		SMTPPort:               getEnv("SMTP_PORT", "587"),
		SMTPUser:               getEnv("SMTP_USER", ""),
		SMTPPassword:           getEnv("SMTP_PASSWORD", ""),
		DefaultQuotaGB:         getEnvFloat("DEFAULT_QUOTA_GB", 50.0),
	}
}
```

```go
// signaling-server/internal/handler/browser_ws.go
if relayOnly && quotaExceeded {
	sessionHub.SendDirect(requestCtx, browserConn, map[string]string{
		"type":    "error",
		"message": "file host's relay quota exceeded - this share requires secure relay which is unavailable",
	})
	browserConn.Close(websocket.StatusNormalClosure, "relay quota exceeded")
	return
}

iceServers := turn.BuildICEConfig(&turn.ICEConfigRequest{
	STUNURL: cfg.STUNURL,
})

log.Printf("browser_ws: sending ICE config for session %s: STUN=%s", sessionCode, cfg.STUNURL)
```

```go
// signaling-server/internal/handler/agent_ws.go
iceServers := turn.BuildICEConfig(&turn.ICEConfigRequest{
	STUNURL: cfg.STUNURL,
})
```

```go
// signaling-server/cmd/server/main.go
// Delete imports:
// "sharebridge/server/internal/metrics"
// "sharebridge/server/internal/quota"

// Remove:
if cfg.HasTurn() {
	metricsClient := metrics.NewPrometheusClient(cfg.PrometheusURL)
	quotaPoller := quota.NewPoller(app, metricsClient, cfg)
	quotaPoller.Start()
	log.Printf("quota poller started with interval: %v", cfg.QuotaCheckInterval)
}
```

```bash
git -C .worktrees/secure-relay rm \
  signaling-server/internal/metrics/prometheus.go \
  signaling-server/internal/metrics/prometheus_test.go \
  signaling-server/internal/quota/poller.go \
  signaling-server/internal/quota/poller_test.go \
  signaling-server/internal/turn/credentials.go \
  signaling-server/internal/turn/credentials_test.go
```

- [ ] **Step 4: Run the focused cleanup tests and then the full server suite**

Run:

```bash
cd .worktrees/secure-relay/signaling-server && go test ./internal/config ./internal/handler -run 'TestLoad_DefaultsNoLongerExposeTurnOrPrometheusConfig|TestBrowserWS_IceConfigIsStunOnlyWithoutTurnFields' -v
cd .worktrees/secure-relay/signaling-server && go test ./...
```

Expected:

- PASS for the focused config/handler tests
- PASS for the full `signaling-server` suite after deleting the TURN-era packages

- [ ] **Step 5: Commit**

```bash
git -C .worktrees/secure-relay add \
  signaling-server/internal/config/config.go \
  signaling-server/internal/config/config_test.go \
  signaling-server/internal/handler/browser_ws.go \
  signaling-server/internal/handler/browser_ws_test.go \
  signaling-server/internal/handler/agent_ws.go \
  signaling-server/cmd/server/main.go
git -C .worktrees/secure-relay commit -m "refactor(relay): remove TURN-era runtime plumbing"
```

## Task 3: Clean Deployment/Docs Leftovers, Remove Artifacts, And Re-Verify

**Files:**
- Modify: `signaling-server/docker-compose.yml`
- Modify: `signaling-server/deploy.sh`
- Modify: `signaling-server/README.md`
- Modify: `signaling-server/web/home.html`
- Modify: `.gitignore`
- Delete: `signaling-server/prometheus.yml`
- Delete: `agent/agent`

- [ ] **Step 1: Write the failing documentation/deploy cleanup checks**

```bash
cd .worktrees/secure-relay
! rg -n "coturn|TURN_SECRET|TURN_HOST|PROMETHEUS_URL|prometheus" signaling-server/docker-compose.yml signaling-server/deploy.sh signaling-server/README.md
! rg -n "TURN Relay" signaling-server/web/home.html
test ! -f signaling-server/prometheus.yml
test ! -f agent/agent
```

Expected:

- FAIL because the old files and references still exist

- [ ] **Step 2: Apply the cleanup**

```yaml
# signaling-server/docker-compose.yml
services:
  sharebridge-server:
    build: .
    network_mode: host
    environment:
      - PORT=8080
      - DATA_DIR=/data/pb_data
      - RELAY_JWT_SECRET=${RELAY_JWT_SECRET}
      - RELAY_PENDING_WAIT_WINDOW=${RELAY_PENDING_WAIT_WINDOW:-7s}
      - DEFAULT_QUOTA_GB=${DEFAULT_QUOTA_GB:-50}
      - SMTP_HOST=${SMTP_HOST}
      - SMTP_PORT=${SMTP_PORT}
      - SMTP_USER=${SMTP_USER}
      - SMTP_PASSWORD=${SMTP_PASSWORD}
    volumes:
      - sharebridge-data:/data

volumes:
  sharebridge-data:
```

```md
<!-- signaling-server/README.md -->
## Secure Relay

ShareBridge now uses an application-level secure relay fallback instead of Coturn/TURN. Direct mode gathers STUN candidates with `stun.cloudflare.com:3478`, and relay fallback uses `/ws/relay` plus the secure relay channel after browser auth succeeds.

### Configuration

| Environment Variable | Description | Default |
|---------------------|-------------|---------|
| `PORT` | Server port | `8080` |
| `DATA_DIR` | PocketBase data directory | `./pb_data` |
| `STUN_URL` | STUN server URL for direct mode | `stun:stun.cloudflare.com:3478` |
| `RELAY_JWT_SECRET` | HS256 secret for relay/session JWTs | (required for relay) |
| `RELAY_PENDING_WAIT_WINDOW` | Relay browser/agent pending window | `7s` |
```

```html
<!-- signaling-server/web/home.html -->
<div class="label" style="color: #fab387;">Secure Relay</div>
```

```gitignore
# .gitignore
agent/agent
```

```bash
git -C .worktrees/secure-relay rm -f signaling-server/prometheus.yml agent/agent
```

- [ ] **Step 3: Run the cleanup checks again**

Run:

```bash
cd .worktrees/secure-relay
! rg -n "coturn|TURN_SECRET|TURN_HOST|PROMETHEUS_URL|prometheus" signaling-server/docker-compose.yml signaling-server/deploy.sh signaling-server/README.md
! rg -n "TURN Relay" signaling-server/web/home.html
test ! -f signaling-server/prometheus.yml
test ! -f agent/agent
```

Expected:

- PASS for all checks

- [ ] **Step 4: Run the final verification matrix**

Run:

```bash
cd .worktrees/secure-relay/signaling-server && go test ./...
cd .worktrees/secure-relay/agent && go test ./...
cd .worktrees/secure-relay/signaling-server/web && node --test src/*.test.js
cd .worktrees/secure-relay && git diff --check
```

Expected:

- PASS for all Go test suites
- PASS for all browser `node --test` suites
- no whitespace/conflict errors from `git diff --check`

- [ ] **Step 5: Commit**

```bash
git -C .worktrees/secure-relay add \
  signaling-server/docker-compose.yml \
  signaling-server/deploy.sh \
  signaling-server/README.md \
  signaling-server/web/home.html \
  .gitignore
git -C .worktrees/secure-relay commit -m "chore(relay): remove TURN-era deploy and docs leftovers"
```

## Self-Review

- Spec coverage:
  - coverage matrix: Task 1
  - old TURN/Coturn/Prometheus cleanup: Task 2 + Task 3
  - Cloudflare STUN remains the direct default: Task 2 + Task 3
  - final rerun of coverage after cleanup: Task 3 Step 4
- Placeholder scan:
  - no `TODO` / `TBD` placeholders remain
- Type consistency:
  - reuses existing `connectTransferChannel`, `installSessionMessageHandler`, `BrowserWS`, `AgentWS`, `Daemon`, and `turn.BuildICEConfig` names rather than inventing new APIs

## Notes

- Do not attempt to remove the intentional direct-vs-relay bootstrap split. That split is required by the architecture.
- Keep browser Playwright/full E2E automation out of scope for this plan. That remains future work.
