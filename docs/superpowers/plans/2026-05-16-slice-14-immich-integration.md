# Slice 14 Immich Integration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add Immich shared-link support as a relay-only photo/video gallery backend, with zero-touch agent polling, Immich password validation, thumbnail gallery loading, and full-asset downloads through the existing secure relay transfer path.

**Architecture:** Keep the signaling server untrusted and storage-agnostic: it stores Immich keys as normal session codes, gates protected Immich sessions before peer creation, and relays password validation messages to the agent. The agent owns all Immich API calls through a new `agent/internal/immich` package and adapts Immich assets to the existing transfer manager through a broader storage backend interface. The browser selects file-tree or gallery mode from `/s/` versus `/i/`, then uses a typed binary envelope so thumbnail bytes cannot collide with file chunks.

**Tech Stack:** Go, PocketBase migrations, coder/websocket, Pion WebRTC, existing secure relay channel, plain browser ES modules, `node --test`, vendored lightGallery browser assets.

---

## File Structure

**New files:**
- `agent/internal/immich/client.go` — Immich shared-link client, polling client, SSRF host validation, asset metadata normalization, password validation.
- `agent/internal/immich/client_test.go` — `httptest.Server` coverage for URL validation, shared-link polling, password login cookie handling, thumbnail streaming, asset downloads, and base64 SHA-1 decoding.
- `signaling-server/migrations/6_add_immich_session_fields.go` — adds `share_type` and `is_password_protected` to `sessions`.
- `signaling-server/web/src/gallery.js` — gallery-mode state machine and DOM rendering for `thumbnail_list`, `thumbnail_data`, and `asset_request`.
- `signaling-server/web/src/gallery.test.js` — gallery-mode unit tests.
- `signaling-server/web/src/binaryEnvelope.js` — typed binary envelope for DataChannel/relay payloads.
- `signaling-server/web/src/binaryEnvelope.test.js` — browser-side envelope tests.
- `signaling-server/web/src/vendor/lightgallery/` — vendored lightGallery JS/CSS/assets copied from pinned npm package.

**Modified files:**
- `signaling-server/internal/handler/agent_ws.go` — external-code validation, `share_type`, `is_password_protected`, `unregister_share`, `auth_ok`, `auth_fail`, and Immich registration persistence.
- `signaling-server/internal/handler/agent_ws_test.go` — registration and unregister protocol tests.
- `signaling-server/internal/handler/browser_ws.go` — `/i/` session metadata gate, `password_required`, `password_submit`, 5-strike Immich auth limit, and protected Immich peer creation delay.
- `signaling-server/internal/handler/browser_ws_test.go` — Immich browser flow tests.
- `signaling-server/internal/hub/hub.go` — code unregister helper and per-connection auth failure helpers used by handlers.
- `signaling-server/internal/hub/hub_test.go` — unregister and browser-close coverage.
- `signaling-server/cmd/server/main.go` — serve `/i/{key}` with the existing browser app.
- `signaling-server/migrations/migrations_test.go` — migration 6 assertions and idempotence.
- `agent/internal/signaling/client.go` — registration options (`share_type`, external `code`, `is_password_protected`, `relay_only`) and `UnregisterShare`.
- `agent/internal/signaling/client_test.go` — registration payload and unregister payload tests.
- `agent/internal/store/store.go` — persist `IsPasswordProtected`.
- `agent/internal/store/store_test.go` — persistence round trip for the new field.
- `agent/internal/config/config.go` — `IMMICH_URL`, `IMMICH_ALLOWED_HOST`, `IMMICH_API_KEY`, `IMMICH_POLL_INTERVAL`.
- `agent/internal/config/config_test.go` — env/config defaults for Immich settings.
- `agent/internal/daemon/daemon.go` — Immich startup polling, periodic sync, protected auth handling, unprotected nonce join skip-HMAC, relay-only session creation, removed-share termination.
- `agent/internal/daemon/daemon_test.go` — Immich auth and auto-registration tests.
- `agent/internal/transfer/manager.go` — rename `openCloudClient` to `StorageBackend`, add gallery metadata methods, send thumbnails through typed binary envelope, handle `asset_request`.
- `agent/internal/transfer/manager_test.go` — gallery and envelope tests.
- `agent/internal/cloudwebdav/client.go` — doc/comment alignment only for the renamed backend interface.
- `agent/internal/web/api.go` — accept `immich` as a share type only if manual API creation is still desired; reject when Immich config is missing.
- `agent/internal/web/api_v1.go` — same validation for JSON API.
- `agent/internal/web/api_test.go` and `agent/internal/web/api_v1_test.go` — validation tests.
- `signaling-server/web/index.html` — add gallery root, password-required copy compatible with Immich, and vendored lightGallery styles.
- `signaling-server/web/src/app.js` — path-mode detection, Immich password submit, gallery routing, typed binary receive path.
- `signaling-server/web/src/app.test.js` — path-mode and password-submit tests.
- `signaling-server/web/package.json` and lockfile if present — add pinned `lightgallery` dev dependency used only for vendoring.

## External API Notes

- Current Immich releases use `GET /api/shared-links/me?key={key}` for shared-link metadata. Password validation is `POST /api/shared-links/login?key={key}` with a JSON password body; the agent stores the returned `immich_shared_link_token` cookie in memory for subsequent metadata requests.
- For album shared links, newer Immich may return album metadata without expanded top-level `assets`; the agent falls back to `GET /api/timeline/buckets?key={key}&albumId={albumId}` and `GET /api/timeline/bucket?key={key}&albumId={albumId}&timeBucket={bucket}` to enumerate public album assets.
- Current docs/archive pages show `downloadAsset` accepts asset `id` as a path parameter and `key` as a query parameter.
- The original archived docs for `getMySharedLink` used `/api/shared-links/my-share` and `password` query parameters. That path returns `403 Forbidden` on newer Immich because it is treated as a protected share-id route.

References used while writing this plan:
- `https://github.com/immich-app/immich/blob/main/server/src/controllers/shared-link.controller.ts`
- `https://v1.105.1.archive.immich.app/docs/api/get-all-shared-links/`
- `https://pr-15149.preview.immich.app/docs/api/download-asset/`

## Implementation Notes

- Keep `/s/` behavior unchanged. Existing OpenCloud/Nextcloud sessions must not see Immich password flow or gallery mode.
- Immich sessions must be `relay_only: true`; do not create direct P2P WebRTC peers for Immich.
- Use `auth_fail` for Immich password failures and keep existing `auth_failed` behavior for OC/NC HMAC failures until a deliberate cleanup renames both.
- Do not persist Immich recipient passwords in `sessions.json`, PocketBase, logs, or browser storage.
- The server-generated code path remains 8 lowercase base36 chars. Only externally supplied codes use the 8-128 URL-safe validation.
- Keep the transfer manager single-active-transfer behavior. Gallery thumbnails can stream at open, but full asset requests should still serialize through the active transfer guard.
- Vendored lightGallery files must be tracked under `signaling-server/web/src/vendor/lightgallery/`; this app is served as plain static files without a bundler.

## Task 1: Add Session Metadata and External Code Protocol on the Signaling Server

**Files:**
- Create: `signaling-server/migrations/6_add_immich_session_fields.go`
- Modify: `signaling-server/migrations/migrations_test.go`
- Modify: `signaling-server/internal/handler/agent_ws.go`
- Modify: `signaling-server/internal/handler/agent_ws_test.go`
- Modify: `signaling-server/internal/hub/hub.go`
- Modify: `signaling-server/internal/hub/hub_test.go`

- [ ] **Step 1: Write failing migration coverage**

Add this test to `signaling-server/migrations/migrations_test.go`:

```go
func TestMigration6_AddImmichSessionFields(t *testing.T) {
	testApp, err := tests.NewTestApp(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { testApp.Cleanup() })

	require.NoError(t, testApp.Bootstrap())
	require.NoError(t, testApp.RunSystemMigrations())
	require.NoError(t, migrations.CreateCollections(testApp))
	require.NoError(t, migrations.AddRelayOnly(testApp))
	require.NoError(t, migrations.AddSessionRelayStaticPub(testApp))
	require.NoError(t, migrations.AddImmichSessionFields(testApp))

	sessionsCol, err := testApp.FindCollectionByNameOrId("sessions")
	require.NoError(t, err)
	require.NotNil(t, sessionsCol.Fields.GetByName("share_type"))
	require.NotNil(t, sessionsCol.Fields.GetByName("is_password_protected"))

	require.NoError(t, migrations.AddImmichSessionFields(testApp))
}
```

Run: `cd signaling-server && go test ./migrations -run TestMigration6_AddImmichSessionFields -count=1`

Expected: FAIL with `undefined: migrations.AddImmichSessionFields`.

- [ ] **Step 2: Implement migration 6**

Create `signaling-server/migrations/6_add_immich_session_fields.go`:

```go
package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(AddImmichSessionFields, nil)
}

func AddImmichSessionFields(app core.App) error {
	sessionsCol, err := app.FindCollectionByNameOrId("sessions")
	if err != nil {
		return err
	}

	changed := false
	if sessionsCol.Fields.GetByName("share_type") == nil {
		sessionsCol.Fields.Add(&core.TextField{Name: "share_type", Required: false})
		changed = true
	}
	if sessionsCol.Fields.GetByName("is_password_protected") == nil {
		sessionsCol.Fields.Add(&core.BoolField{Name: "is_password_protected", Required: false})
		changed = true
	}
	if !changed {
		return nil
	}
	return app.Save(sessionsCol)
}
```

Run: `cd signaling-server && go test ./migrations -run TestMigration6_AddImmichSessionFields -count=1`

Expected: PASS.

- [ ] **Step 3: Write failing registration tests for long Immich keys and metadata**

Add tests to `signaling-server/internal/handler/agent_ws_test.go` using the existing WebSocket test helpers in that file:

```go
func TestAgentWS_RegisterShare_AcceptsImmichExternalCodeMetadata(t *testing.T) {
	testApp, serverURL, cleanup := setupAgentWSTest(t)
	defer cleanup()

	apiKey := createTestAPIKey(t, testApp)
	wsURL := strings.Replace(serverURL, "http://", "ws://", 1) + "/ws/agent?api_key=" + apiKey
	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(t, err)
	defer conn.CloseNow()

	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","agent_id":"agent-immich"}`)))
	_, _, err = conn.Read(ctx)
	require.NoError(t, err)

	code := "ffSw63qnIYMt_aBcDeFgHiJkLmNoPqRsTuVwXyZ-1234567890"
	payload := fmt.Sprintf(`{"type":"register_share","code":%q,"share_type":"immich","is_password_protected":true,"relay_only":true,"relay_static_pub":"04abcd"}`, code)
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(payload)))

	_, raw, err := conn.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"type":"share_registered"`)
	require.Contains(t, string(raw), code)

	session := findSessionByCode(t, testApp, code)
	require.Equal(t, "immich", session.GetString("share_type"))
	require.True(t, session.GetBool("is_password_protected"))
	require.True(t, session.GetBool("relay_only"))
}

func TestAgentWS_RegisterShare_RejectsExternalCodeOver128Chars(t *testing.T) {
	testApp, serverURL, cleanup := setupAgentWSTest(t)
	defer cleanup()

	apiKey := createTestAPIKey(t, testApp)
	wsURL := strings.Replace(serverURL, "http://", "ws://", 1) + "/ws/agent?api_key=" + apiKey
	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(t, err)
	defer conn.CloseNow()

	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","agent_id":"agent-immich"}`)))
	_, _, err = conn.Read(ctx)
	require.NoError(t, err)

	code := strings.Repeat("a", 129)
	payload := fmt.Sprintf(`{"type":"register_share","code":%q,"share_type":"immich"}`, code)
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(payload)))

	_, raw, err := conn.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"type":"error"`)
	require.Contains(t, string(raw), "invalid external code format")
}
```

Run: `cd signaling-server && go test ./internal/handler -run 'TestAgentWS_RegisterShare_(AcceptsImmichExternalCodeMetadata|RejectsExternalCodeOver128Chars)' -count=1`

Expected: FAIL because `share_type` / `is_password_protected` are not parsed or stored, and long external keys are rejected by the current regex.

- [ ] **Step 4: Implement metadata fields and external code validation**

In `signaling-server/internal/handler/agent_ws.go`, extend `agentMsg`:

```go
ShareType           string `json:"share_type,omitempty"`
IsPasswordProtected bool   `json:"is_password_protected,omitempty"`
```

Replace `codeRegex` with two validators:

```go
var generatedCodeRegex = regexp.MustCompile(`^[a-z0-9]{8}$`)
var externalCodeRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]{8,128}$`)
```

In the custom-code branch of `handleRegisterShare`, validate with `externalCodeRegex` and return:

```go
"invalid external code format (8-128 chars, alphanumeric + hyphen + underscore)"
```

Change `createSession` and `claimSessionCode` signatures to accept `shareType string, isPasswordProtected bool`, and set:

```go
record.Set("share_type", shareType)
record.Set("is_password_protected", isPasswordProtected)
```

When reclaiming an existing custom-code session, update those fields too:

```go
record.Set("share_type", shareType)
record.Set("is_password_protected", isPasswordProtected)
record.Set("relay_only", relayOnly)
```

Keep generated-code sessions valid by passing empty `shareType` and `false` for `isPasswordProtected` when those fields are not present.

Run: `cd signaling-server && go test ./internal/handler -run 'TestAgentWS_RegisterShare_(AcceptsImmichExternalCodeMetadata|RejectsExternalCodeOver128Chars)' -count=1`

Expected: PASS.

- [ ] **Step 5: Write failing unregister protocol tests**

Add this hub test to `signaling-server/internal/hub/hub_test.go`:

```go
func TestHubUnregisterCodeClosesPairedBrowser(t *testing.T) {
	h := New()
	ctx := context.Background()
	agentConn, agentServer := newTestWebSocketPair(t)
	browserConn, browserServer := newTestWebSocketPair(t)
	defer agentConn.CloseNow()
	defer agentServer.CloseNow()
	defer browserConn.CloseNow()
	defer browserServer.CloseNow()

	h.RegisterAgent("api-key-1", agentConn)
	h.RegisterCode("IMMICHKEY1", "api-key-1")
	require.NoError(t, h.PairSession("IMMICHKEY1", browserConn))

	h.UnregisterCode(ctx, "IMMICHKEY1", "api-key-1", "share has been removed")

	_, ok := h.GetAgentConn("IMMICHKEY1")
	require.False(t, ok)

	_, raw, err := browserServer.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, string(raw), "share has been removed")
}
```

Add this handler test to `signaling-server/internal/handler/agent_ws_test.go`:

```go
func TestAgentWS_UnregisterShareDeletesOwnedSession(t *testing.T) {
	testApp, serverURL, cleanup := setupAgentWSTest(t)
	defer cleanup()

	apiKey := createTestAPIKey(t, testApp)
	conn := dialAgentAndHello(t, serverURL, apiKey, "agent-unregister")
	defer conn.CloseNow()

	ctx := context.Background()
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","code":"IMMICHDEL1","share_type":"immich"}`)))
	_, _, err := conn.Read(ctx)
	require.NoError(t, err)

	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"unregister_share","code":"IMMICHDEL1"}`)))
	_, raw, err := conn.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"type":"share_unregistered"`)

	session := findSessionByCode(t, testApp, "IMMICHDEL1")
	require.Nil(t, session)
}
```

Run:

```bash
cd signaling-server
go test ./internal/hub -run TestHubUnregisterCodeClosesPairedBrowser -count=1
go test ./internal/handler -run TestAgentWS_UnregisterShareDeletesOwnedSession -count=1
```

Expected: FAIL because `UnregisterCode` and `unregister_share` do not exist.

- [ ] **Step 6: Implement unregister**

Add to `signaling-server/internal/hub/hub.go`:

```go
func (h *Hub) UnregisterCode(ctx context.Context, code, apiKey, reason string) {
	h.mu.Lock()
	owner, ok := h.codes[code]
	if !ok || owner != apiKey {
		h.mu.Unlock()
		return
	}
	delete(h.codes, code)
	sessionPair := h.pairs[code]
	delete(h.pairs, code)
	h.mu.Unlock()

	if sessionPair != nil && sessionPair.browserConn != nil {
		send(ctx, h, sessionPair.browserConn, map[string]string{"type": "error", "message": reason})
		sessionPair.browserConn.Close(websocket.StatusNormalClosure, reason)
	}
}
```

In `AgentWS`, add case:

```go
case "unregister_share":
	if agentID == "" {
		continue
	}
	handleUnregisterShare(ctx, conn, h, app, apiKeyID, msg.Code)
```

Implement:

```go
func handleUnregisterShare(ctx context.Context, conn *websocket.Conn, h *hub.Hub, app core.App, apiKeyID, code string) {
	if code == "" {
		hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "code required"})
		return
	}
	session, err := getSessionByCode(app, code)
	if err != nil {
		hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "database error"})
		return
	}
	if session == nil {
		hub.SendDirect(ctx, conn, map[string]string{"type": "share_unregistered", "code": code})
		return
	}
	if session.GetString("api_key_id") != apiKeyID {
		hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "session not owned by this api key"})
		return
	}
	if err := app.Delete(session); err != nil {
		hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "database error"})
		return
	}
	h.UnregisterCode(ctx, code, apiKeyID, "share has been removed")
	hub.SendDirect(ctx, conn, map[string]string{"type": "share_unregistered", "code": code})
}
```

Run:

```bash
cd signaling-server
go test ./migrations -count=1
go test ./internal/hub -count=1
go test ./internal/handler -run 'TestAgentWS_(RegisterShare|UnregisterShare)' -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add signaling-server/migrations signaling-server/internal/handler/agent_ws.go signaling-server/internal/handler/agent_ws_test.go signaling-server/internal/hub/hub.go signaling-server/internal/hub/hub_test.go
git commit -m "feat(server): support Immich session metadata"
```

## Task 2: Add Immich Browser Auth Gate on the Signaling Server

**Files:**
- Modify: `signaling-server/internal/handler/browser_ws.go`
- Modify: `signaling-server/internal/handler/browser_ws_test.go`
- Modify: `signaling-server/internal/handler/agent_ws.go`
- Modify: `signaling-server/internal/hub/hub.go`

- [ ] **Step 1: Write failing protected Immich browser-flow tests**

Add to `signaling-server/internal/handler/browser_ws_test.go`:

```go
func TestBrowserWS_ProtectedImmichSendsPasswordRequiredBeforeIceConfig(t *testing.T) {
	testApp, serverURL, cleanup := setupBrowserWSTest(t)
	defer cleanup()
	apiKey := createTestAPIKey(t, testApp)
	agentConn := dialAgentAndHello(t, serverURL, apiKey, "agent-immich-auth")
	defer agentConn.CloseNow()

	registerPayload := `{"type":"register_share","code":"IMMICHAUTH1","share_type":"immich","is_password_protected":true,"relay_only":true,"relay_static_pub":"04abcd"}`
	require.NoError(t, agentConn.Write(context.Background(), websocket.MessageText, []byte(registerPayload)))
	_, _, err := agentConn.Read(context.Background())
	require.NoError(t, err)

	browserConn := dialBrowser(t, serverURL, "IMMICHAUTH1")
	defer browserConn.CloseNow()

	_, raw, err := browserConn.Read(context.Background())
	require.NoError(t, err)
	require.JSONEq(t, `{"type":"password_required"}`, string(raw))

	readCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, err = agentConn.Read(readCtx)
	require.Error(t, err, "protected Immich must not knock before password_submit")
}

func TestBrowserWS_ImmichPasswordSubmitReturnsRelayPolicyAfterAuthOK(t *testing.T) {
	testApp, serverURL, cleanup := setupBrowserWSTest(t)
	defer cleanup()
	apiKey := createTestAPIKey(t, testApp)
	agentConn := dialAgentAndHello(t, serverURL, apiKey, "agent-immich-auth-ok")
	defer agentConn.CloseNow()

	require.NoError(t, agentConn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"register_share","code":"IMMICHAUTH2","share_type":"immich","is_password_protected":true,"relay_only":true,"relay_static_pub":"04abcd"}`)))
	_, _, err := agentConn.Read(context.Background())
	require.NoError(t, err)

	browserConn := dialBrowser(t, serverURL, "IMMICHAUTH2")
	defer browserConn.CloseNow()
	_, _, err = browserConn.Read(context.Background())
	require.NoError(t, err)

	require.NoError(t, browserConn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"password_submit","code":"IMMICHAUTH2","password":"secret"}`)))
	_, rawSubmit, err := agentConn.Read(context.Background())
	require.NoError(t, err)
	require.Contains(t, string(rawSubmit), `"type":"password_submit"`)
	require.Contains(t, string(rawSubmit), `"code":"IMMICHAUTH2"`)
	require.Contains(t, string(rawSubmit), `"password":"secret"`)

	connID := extractJSONField(t, rawSubmit, "conn_id")
	require.NotEmpty(t, connID)
	require.NoError(t, agentConn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"auth_ok","code":"IMMICHAUTH2","conn_id":"`+connID+`"}`)))

	_, rawPolicy, err := browserConn.Read(context.Background())
	require.NoError(t, err)
	require.Contains(t, string(rawPolicy), `"type":"relay_policy"`)
	require.Contains(t, string(rawPolicy), `"relay_only":true`)
}
```

Run: `cd signaling-server && go test ./internal/handler -run 'TestBrowserWS_.*Immich' -count=1`

Expected: FAIL because browser messages lack `password_submit`, protected sessions always receive `ice_config`, and the existing `auth_ok` relay-policy path is not yet explicitly covered for protected Immich auth.

- [ ] **Step 2: Extend browser messages and protected session gate**

In `browser_ws.go`, extend `browserMsg`:

```go
Code     string `json:"code,omitempty"`
Password string `json:"password,omitempty"`
```

After session lookup, compute:

```go
shareType := sessionRecord.GetString("share_type")
isImmich := shareType == "immich"
isPasswordProtected := sessionRecord.GetBool("is_password_protected")
```

After pairing/registering `connID`, before quota/ICE send:

```go
if isImmich && isPasswordProtected {
	sessionHub.SendDirect(requestCtx, browserConn, map[string]string{"type": "password_required"})
} else {
	sendBrowserIceConfigAndMaybeKnock(requestCtx, sessionHub, browserConn, apiKeyID, sessionCode, connID, relayOnly, quotaExceeded, periodEnd, cfg)
}
```

Extract the existing ICE-config + relay-only initial knock code into:

```go
func sendBrowserIceConfigAndMaybeKnock(ctx context.Context, sessionHub *hub.Hub, browserConn *websocket.Conn, apiKeyID, sessionCode, connID string, relayOnly, quotaExceeded bool, periodEnd time.Time, cfg *config.Config) {
	iceServers := turn.BuildICEConfig(&turn.ICEConfigRequest{STUNURL: cfg.STUNURL})
	msg := map[string]any{"type": "ice_config", "ice_servers": iceServers, "relay_only": relayOnly}
	if quotaExceeded {
		msg["relay_quota_exceeded"] = true
		msg["quota_period_end"] = periodEnd.Format(time.RFC3339)
	}
	sessionHub.SendDirect(ctx, browserConn, msg)
	if relayOnly {
		_ = sessionHub.SendToAgent(ctx, apiKeyID, map[string]any{"type": "knock", "conn_id": connID, "code": sessionCode})
	}
}
```

- [ ] **Step 3: Implement `password_submit` relay and code validation**

In the browser message switch:

```go
case "password_submit":
	if !isImmich || !isPasswordProtected {
		sessionHub.SendDirect(requestCtx, browserConn, map[string]string{"type": "auth_fail", "message": "password auth not required"})
		continue
	}
	if msg.Code != sessionCode {
		sessionHub.SendDirect(requestCtx, browserConn, map[string]string{"type": "auth_fail", "message": "session code mismatch"})
		continue
	}
	sessionHub.SendToAgent(requestCtx, apiKeyID, map[string]any{
		"type":     "password_submit",
		"conn_id":  connID,
		"code":     sessionCode,
		"password": msg.Password,
	})
```

In `agent_ws.go`, add `Password string` to `agentMsg`, add `case "auth_fail":` separate from existing `auth_failed`, and route:

```go
case "auth_fail":
	if agentID == "" {
		continue
	}
	failures := h.IncrementAuthFailure(msg.ConnID)
	if failures >= 5 {
		h.CloseBrowserConnWithError(ctx, msg.ConnID, "too many incorrect password attempts")
	} else {
		h.ForwardToBrowserByConnID(ctx, msg.ConnID, map[string]any{
			"type": "auth_fail",
			"attempts_remaining": 5 - failures,
		})
	}
```

Keep existing `auth_failed` at 3 attempts for OC/NC HMAC.

Preserve the existing `case "auth_ok"` behavior in `agent_ws.go` and make sure it works for Immich sessions: look up the session by `msg.Code`, create `relay_prepare` for the agent when relay is allowed, then send `relay_policy` to `msg.ConnID` with `relay_only: true`. Do not send a new `knock` after protected Immich `auth_ok`; the Immich password validation is the auth proof, and the browser proceeds by opening the secure relay transfer channel from `relay_policy`.

Run: `cd signaling-server && go test ./internal/handler -run 'TestBrowserWS_.*Immich' -count=1`

Expected: PASS.

- [ ] **Step 4: Add 5-strike auth-fail coverage**

Add to `browser_ws_test.go`:

```go
func TestBrowserWS_ImmichAuthFailClosesAfterFiveAttempts(t *testing.T) {
	testApp, serverURL, cleanup := setupBrowserWSTest(t)
	defer cleanup()
	apiKey := createTestAPIKey(t, testApp)
	agentConn := dialAgentAndHello(t, serverURL, apiKey, "agent-immich-fail")
	defer agentConn.CloseNow()

	require.NoError(t, agentConn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"register_share","code":"IMMICHFAIL1","share_type":"immich","is_password_protected":true,"relay_only":true,"relay_static_pub":"04abcd"}`)))
	_, _, err := agentConn.Read(context.Background())
	require.NoError(t, err)

	browserConn := dialBrowser(t, serverURL, "IMMICHFAIL1")
	defer browserConn.CloseNow()
	_, _, err = browserConn.Read(context.Background())
	require.NoError(t, err)

	for i := 0; i < 5; i++ {
		require.NoError(t, browserConn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"password_submit","code":"IMMICHFAIL1","password":"bad"}`)))
		_, rawSubmit, err := agentConn.Read(context.Background())
		require.NoError(t, err)
		connID := extractJSONField(t, rawSubmit, "conn_id")
		require.NoError(t, agentConn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"auth_fail","conn_id":"`+connID+`","code":"IMMICHFAIL1"}`)))
		_, rawBrowser, err := browserConn.Read(context.Background())
		require.NoError(t, err)
		if i < 4 {
			require.Contains(t, string(rawBrowser), `"type":"auth_fail"`)
		} else {
			require.Contains(t, string(rawBrowser), "too many incorrect password attempts")
		}
	}
}
```

Run: `cd signaling-server && go test ./internal/handler -run TestBrowserWS_ImmichAuthFailClosesAfterFiveAttempts -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add signaling-server/internal/handler/browser_ws.go signaling-server/internal/handler/browser_ws_test.go signaling-server/internal/handler/agent_ws.go signaling-server/internal/hub/hub.go
git commit -m "feat(server): gate Immich password auth"
```

## Task 3: Add `/i/{key}` Routing and Browser Path Mode Detection

**Files:**
- Modify: `signaling-server/cmd/server/main.go`
- Modify: `signaling-server/cmd/server/main_test.go`
- Modify: `signaling-server/web/src/app.js`
- Modify: `signaling-server/web/src/app.test.js`

- [ ] **Step 1: Write failing route and path-mode tests**

Add to `signaling-server/cmd/server/main_test.go`:

```go
func TestServerRoutesImmichLinksToBrowserApp(t *testing.T) {
	router := setupRouterForTest(t)
	req := httptest.NewRequest(http.MethodGet, "/i/ffSw63qnIYMt_aBcDeFgHiJkLmNoPqRsTuVwXyZ", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "ShareBridge")
}
```

Add to `app.test.js`:

```js
test('detectPathMode returns gallery mode for /i links and file mode for /s links', () => {
  assert.deepEqual(__test.detectPathMode('/i/ffSw63qn'), { mode: 'gallery', code: 'ffSw63qn' })
  assert.deepEqual(__test.detectPathMode('/s/a3f9k2xp'), { mode: 'files', code: 'a3f9k2xp' })
})
```

Run:

```bash
cd signaling-server && go test ./cmd/server -run TestServerRoutesImmichLinksToBrowserApp -count=1
cd signaling-server/web && node --test src/app.test.js -v
```

Expected: FAIL because `/i/` is not served and `detectPathMode` does not exist.

- [ ] **Step 2: Add route and path-mode helper**

In `signaling-server/cmd/server/main.go`, beside the existing `/s/{code}` or `/join` browser routes, add:

```go
router.GET("/i/{key}", handler.ServeFileNoCache("./web/index.html"))
```

In `app.js`, add:

```js
export function detectPathMode(pathname = window.location.pathname) {
  const [, prefix, ...rest] = pathname.split('/')
  const code = decodeURIComponent(rest.join('/'))
  if (prefix === 'i' && code) return { mode: 'gallery', code }
  if (prefix === 's' && code) return { mode: 'files', code }
  return { mode: 'files', code: '' }
}
```

Expose it through the existing `__test` export by adding `detectPathMode` as a property in the object literal.

In initialization, replace current path parsing with:

```js
const pathMode = detectPathMode()
sessionCode = pathMode.code
galleryMode = pathMode.mode === 'gallery'
```

Run:

```bash
cd signaling-server && go test ./cmd/server -run TestServerRoutesImmichLinksToBrowserApp -count=1
cd signaling-server/web && node --test src/app.test.js -v
```

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add signaling-server/cmd/server/main.go signaling-server/cmd/server/main_test.go signaling-server/web/src/app.js signaling-server/web/src/app.test.js
git commit -m "feat(web): route Immich share links"
```

## Task 4: Extend Agent Registration, Store, and Config for Immich

**Files:**
- Modify: `agent/internal/signaling/client.go`
- Modify: `agent/internal/signaling/client_test.go`
- Modify: `agent/internal/store/store.go`
- Modify: `agent/internal/store/store_test.go`
- Modify: `agent/internal/config/config.go`
- Modify: `agent/internal/config/config_test.go`

- [ ] **Step 1: Write failing signaling registration tests**

Add to `agent/internal/signaling/client_test.go`:

```go
func TestRegisterShare_IncludesImmichMetadata(t *testing.T) {
	server := newRegisterShareTestServer(t, func(t *testing.T, raw []byte) []byte {
		var got map[string]any
		require.NoError(t, json.Unmarshal(raw, &got))
		require.Equal(t, "register_share", got["type"])
		require.Equal(t, "immich", got["share_type"])
		require.Equal(t, "ffSw63qnIYMt_aBcDeFgHiJkLmNoPqRsTuVwXyZ", got["code"])
		require.Equal(t, true, got["is_password_protected"])
		require.Equal(t, true, got["relay_only"])
		return []byte(`{"type":"share_registered","code":"ffSw63qnIYMt_aBcDeFgHiJkLmNoPqRsTuVwXyZ"}`)
	})
	defer server.Close()

	client := New(strings.Replace(server.URL, "http://", "ws://", 1), "api", "agent")
	require.NoError(t, client.Connect(context.Background()))
	go client.Listen(context.Background())

	opts := RegisterShareOptions{
		ShareURL:            "immich://ffSw63qnIYMt_aBcDeFgHiJkLmNoPqRsTuVwXyZ",
		PreferredCode:       "ffSw63qnIYMt_aBcDeFgHiJkLmNoPqRsTuVwXyZ",
		ShareType:           "immich",
		IsPasswordProtected: true,
		RelayOnly:           true,
		RelayStaticPub:      "04abcd",
	}
	code, _, err := client.RegisterShareWithOptions(context.Background(), opts)
	require.NoError(t, err)
	require.Equal(t, opts.PreferredCode, code)
}

func TestUnregisterShare_SendsMessage(t *testing.T) {
	server := newRegisterShareTestServer(t, func(t *testing.T, raw []byte) []byte {
		require.JSONEq(t, `{"type":"unregister_share","code":"IMMICHDEL1"}`, string(raw))
		return []byte(`{"type":"share_unregistered","code":"IMMICHDEL1"}`)
	})
	defer server.Close()

	client := New(strings.Replace(server.URL, "http://", "ws://", 1), "api", "agent")
	require.NoError(t, client.Connect(context.Background()))
	require.NoError(t, client.UnregisterShare(context.Background(), "IMMICHDEL1"))
}
```

Run: `cd agent && go test ./internal/signaling -run 'Test(RegisterShare_IncludesImmichMetadata|UnregisterShare_SendsMessage)' -count=1`

Expected: FAIL because the options API and unregister method do not exist.

- [ ] **Step 2: Implement registration options while preserving old callers**

In `agent/internal/signaling/client.go`, add:

```go
type RegisterShareOptions struct {
	ShareURL            string
	PreferredCode       string
	ShareType           string
	IsPasswordProtected bool
	RelayOnly           bool
	RelayStaticPub      string
}
```

Keep the old method as a wrapper:

```go
func (c *Client) RegisterShare(ctx context.Context, shareURL, preferredCode string, relayOnly bool, relayStaticPub string) (string, bool, error) {
	return c.RegisterShareWithOptions(ctx, RegisterShareOptions{
		ShareURL:       shareURL,
		PreferredCode:  preferredCode,
		RelayOnly:      relayOnly,
		RelayStaticPub: relayStaticPub,
	})
}
```

Move the current implementation body into:

```go
func (c *Client) RegisterShareWithOptions(ctx context.Context, opts RegisterShareOptions) (string, bool, error) {
	responseCh := make(chan Message, 1)
	c.pendingRegMu.Lock()
	c.pendingReg = responseCh
	c.pendingRegMu.Unlock()
	defer func() {
		c.pendingRegMu.Lock()
		c.pendingReg = nil
		c.pendingRegMu.Unlock()
	}()

	msg := map[string]any{
		"type":       "register_share",
		"share_url":  opts.ShareURL,
		"relay_only": opts.RelayOnly,
	}
	if opts.PreferredCode != "" {
		msg["code"] = opts.PreferredCode
	}
	if opts.ShareType != "" {
		msg["share_type"] = opts.ShareType
	}
	if opts.IsPasswordProtected {
		msg["is_password_protected"] = true
	}
	if opts.RelayStaticPub != "" {
		msg["relay_static_pub"] = opts.RelayStaticPub
	}
	if err := c.Send(ctx, msg); err != nil {
		return "", false, fmt.Errorf("send register_share: %w", err)
	}

	select {
	case resp := <-responseCh:
		if resp.Type == "error" {
			return "", false, fmt.Errorf("server error: %s", resp.Err)
		}
		return resp.Code, resp.Reconnected, nil
	case <-ctx.Done():
		return "", false, ctx.Err()
	}
}
```

Add:

```go
func (c *Client) UnregisterShare(ctx context.Context, code string) error {
	return c.Send(ctx, map[string]string{"type": "unregister_share", "code": code})
}
```

Update `SignalingClientInterface` in `daemon.go` after Task 5 to include `RegisterShareWithOptions` only if tests need it; otherwise keep daemon using a small helper that type-asserts the concrete interface.

Run: `cd agent && go test ./internal/signaling -count=1`

Expected: PASS.

- [ ] **Step 3: Write failing store/config tests**

Add to `agent/internal/store/store_test.go`:

```go
func TestSaveSession_RoundTripIsPasswordProtected(t *testing.T) {
	st := newTempStore(t)
	session := SessionEntry{
		Code:                "IMMICHKEY1",
		ShareURL:            "immich://IMMICHKEY1",
		ShareType:           "immich",
		IsPasswordProtected: true,
		RelayOnly:           true,
		CreatedAt:           time.Now(),
	}
	require.NoError(t, st.SaveSession(session))

	got := st.GetSession("IMMICHKEY1")
	require.NotNil(t, got)
	require.True(t, got.IsPasswordProtected)
}
```

Add to `agent/internal/config/config_test.go`:

```go
func TestConfigLoadsImmichEnv(t *testing.T) {
	t.Setenv("IMMICH_URL", "http://immich.lan:2283")
	t.Setenv("IMMICH_ALLOWED_HOST", "immich.lan")
	t.Setenv("IMMICH_API_KEY", "immich-api-key")
	t.Setenv("IMMICH_POLL_INTERVAL", "45")
	mgr := newTempConfigManager(t)

	cfg := mgr.Get()
	require.Equal(t, "http://immich.lan:2283", cfg.ImmichURL)
	require.Equal(t, "immich.lan", cfg.ImmichAllowedHost)
	require.Equal(t, "immich-api-key", cfg.ImmichAPIKey)
	require.Equal(t, 45, cfg.ImmichPollInterval)
}
```

Run: `cd agent && go test ./internal/store ./internal/config -run 'Immich|IsPasswordProtected' -count=1`

Expected: FAIL because fields do not exist.

- [ ] **Step 4: Implement store/config fields**

In `store.SessionEntry`, add:

```go
IsPasswordProtected bool `json:"is_password_protected,omitempty"`
```

In `config.Config`, add:

```go
ImmichURL          string `json:"immich_url,omitempty"`
ImmichAllowedHost  string `json:"immich_allowed_host,omitempty"`
ImmichAPIKey       string `json:"immich_api_key,omitempty"`
ImmichPollInterval int    `json:"immich_poll_interval,omitempty"`
```

In `Manager.load`, apply env:

```go
if v := os.Getenv("IMMICH_URL"); v != "" {
	cfg.ImmichURL = v
}
if v := os.Getenv("IMMICH_ALLOWED_HOST"); v != "" {
	cfg.ImmichAllowedHost = v
}
if v := os.Getenv("IMMICH_API_KEY"); v != "" {
	cfg.ImmichAPIKey = v
}
if v := os.Getenv("IMMICH_POLL_INTERVAL"); v != "" {
	cfg.ImmichPollInterval = getEnvInt("IMMICH_POLL_INTERVAL", cfg.ImmichPollInterval)
}
if cfg.ImmichPollInterval == 0 {
	cfg.ImmichPollInterval = 30
}
```

Run: `cd agent && go test ./internal/store ./internal/config -run 'Immich|IsPasswordProtected' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/signaling agent/internal/store agent/internal/config
git commit -m "feat(agent): add Immich registration options"
```

## Task 5: Build the Immich API Client

**Files:**
- Create: `agent/internal/immich/client.go`
- Create: `agent/internal/immich/client_test.go`

- [ ] **Step 1: Write failing Immich client tests**

Create `agent/internal/immich/client_test.go`:

```go
package immich

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewRejectsHostOutsideAllowList(t *testing.T) {
	_, err := New(Config{
		BaseURL:     "http://evil.example:2283",
		AllowedHost: "immich.lan",
		ShareKey:    "sharekey",
	})
	require.ErrorContains(t, err, `host "evil.example:2283" not allowed`)
}

func TestPollSharesUsesAPIKeyAndNormalizesProtection(t *testing.T) {
	var gotKey string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/shared-links", r.URL.Path)
		gotKey = r.Header.Get("x-api-key")
		_ = json.NewEncoder(w).Encode([]SharedLink{
			{Key: "plainkey", Type: "ALBUM"},
			{Key: "protectedkey", Password: "********", Type: "ALBUM"},
		})
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), APIKey: "api-key"})
	require.NoError(t, err)
	shares, err := client.PollShares(t.Context())
	require.NoError(t, err)
	require.Equal(t, "api-key", gotKey)
	require.False(t, shares[0].IsPasswordProtected())
	require.True(t, shares[1].IsPasswordProtected())
}

func TestValidatePasswordPostsLoginAndStoresCookie(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/shared-links/login", r.URL.Path)
		require.Equal(t, "sharekey", r.URL.Query().Get("key"))
		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "secret", body["password"])
		http.SetCookie(w, &http.Cookie{Name: "immich_shared_link_token", Value: "auth-token"})
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"key":"sharekey","assets":[]}`))
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)
	ok, err := client.ValidatePassword(t.Context(), "secret")
	require.NoError(t, err)
	require.True(t, ok)
}

func TestListFilesConvertsImmichAssetsToGalleryItems(t *testing.T) {
	sha := bytes.Repeat([]byte{0xab}, 20)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/shared-links/me", r.URL.Path)
		_ = json.NewEncoder(w).Encode(SharedLink{
			Key: "sharekey",
			Album: &Album{Name: "Summer", Description: "Beach"},
			Assets: []Asset{{
				ID: "asset-1", OriginalFileName: "photo.jpg", OriginalMimeType: "image/jpeg",
				FileSizeInByte: 1234, Type: "IMAGE", Checksum: base64.StdEncoding.EncodeToString(sha),
				ExifInfo: &ExifInfo{ExifImageWidth: 4000, ExifImageHeight: 3000},
			}, {
				ID: "asset-2", OriginalFileName: "clip.mp4", OriginalMimeType: "video/mp4",
				FileSizeInByte: 4567, Type: "VIDEO", Duration: "00:01:34.500",
			}},
		})
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)
	gallery, err := client.ListGallery(t.Context())
	require.NoError(t, err)
	require.Equal(t, "Summer", gallery.AlbumName)
	require.Equal(t, "Beach", gallery.AlbumDescription)
	require.Equal(t, "asset-1", gallery.Items[0].ID)
	require.Equal(t, "abababababababababababababababababababab", gallery.Items[0].SHA1)
	require.NotNil(t, gallery.Items[1].Duration)
	require.Equal(t, 94.5, *gallery.Items[1].Duration)
}
```

Run: `cd agent && go test ./internal/immich -count=1`

Expected: FAIL because package does not exist.

- [ ] **Step 2: Implement `agent/internal/immich/client.go`**

Create the package with these exported shapes:

```go
package immich

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	BaseURL     string
	AllowedHost string
	APIKey      string
	ShareKey    string
	Password    string
}

type Client struct {
	baseURL    *url.URL
	allowed    string
	apiKey     string
	shareKey   string
	password   string
	httpClient *http.Client
}

type SharedLink struct {
	Key      string  `json:"key"`
	Password string `json:"password"`
	Type     string `json:"type"`
	Album    *Album  `json:"album"`
	Assets   []Asset `json:"assets"`
}

type Album struct {
	Name        string `json:"albumName"`
	Description string `json:"description"`
}

type Asset struct {
	ID               string    `json:"id"`
	OriginalFileName string    `json:"originalFileName"`
	OriginalMimeType string    `json:"originalMimeType"`
	FileSizeInByte   int64     `json:"fileSizeInByte"`
	Type             string    `json:"type"`
	Duration         string    `json:"duration"`
	Checksum         string    `json:"checksum"`
	ExifInfo         *ExifInfo `json:"exifInfo"`
}

type ExifInfo struct {
	ExifImageWidth  int `json:"exifImageWidth"`
	ExifImageHeight int `json:"exifImageHeight"`
}

func (s SharedLink) IsPasswordProtected() bool {
	return s.Password != ""
}

type Gallery struct {
	AlbumName        string
	AlbumDescription string
	Items            []GalleryItem
}

type GalleryItem struct {
	ID       string
	Name     string
	MimeType string
	Width    int
	Height   int
	Size     int64
	Duration *float64
	SHA1     string
}
```

Implement `New`, host validation, `PollShares`, `ValidatePassword`, `ListGallery`, `GetThumbnail`, and `GetFile` with these paths:

```go
func (c *Client) sharedLinkURL() string {
	u := c.baseURL.ResolveReference(&url.URL{Path: "/api/shared-links/me"})
	q := u.Query()
	q.Set("key", c.shareKey)
	u.RawQuery = q.Encode()
	return u.String()
}

func (c *Client) sharedLinkLoginURL() string {
	u := c.baseURL.ResolveReference(&url.URL{Path: "/api/shared-links/login"})
	q := u.Query()
	q.Set("key", c.shareKey)
	u.RawQuery = q.Encode()
	return u.String()
}

func (c *Client) assetURL(id, suffix string) string {
	u := c.baseURL.ResolveReference(&url.URL{Path: "/api/assets/" + url.PathEscape(id) + suffix})
	q := u.Query()
	q.Set("key", c.shareKey)
	u.RawQuery = q.Encode()
	return u.String()
}
```

Use `GET /api/shared-links` for `PollShares` and set `x-api-key`.

For SHA-1:

```go
func decodeBase64SHA1(input string) string {
	raw, err := base64.StdEncoding.DecodeString(input)
	if err != nil || len(raw) != 20 {
		return ""
	}
	return hex.EncodeToString(raw)
}
```

For video durations, parse Immich's duration string into seconds:

```go
func parseDurationSeconds(input string) *float64 {
	if input == "" {
		return nil
	}
	parts := strings.Split(input, ":")
	if len(parts) != 3 {
		return nil
	}
	hours, err := strconv.Atoi(parts[0])
	if err != nil {
		return nil
	}
	minutes, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil
	}
	seconds, err := strconv.ParseFloat(parts[2], 64)
	if err != nil {
		return nil
	}
	total := float64(hours*3600+minutes*60) + seconds
	return &total
}
```

Run: `cd agent && go test ./internal/immich -count=1`

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add agent/internal/immich
git commit -m "feat(agent): add Immich API client"
```

## Task 6: Wire Immich Sessions, Polling, and Password Validation into the Agent

**Files:**
- Modify: `agent/internal/daemon/daemon.go`
- Modify: `agent/internal/daemon/daemon_test.go`
- Modify: `agent/internal/signaling/client.go`
- Modify: `agent/internal/store/store.go`

- [ ] **Step 1: Write failing daemon tests for unprotected join and password submit**

Add to `agent/internal/daemon/daemon_test.go`:

```go
func TestHandleJoin_ImmichUnprotectedRequiresNonceButSkipsHMAC(t *testing.T) {
	d, sig := newTestDaemon(t)
	d.sessions["IMMICHOPEN1"] = &Session{
		Code: "IMMICHOPEN1", ShareType: "immich", RelayOnly: true,
		peers: map[string]*peer.Peer{}, relayChannels: map[string]relayTransferChannel{},
	}
	d.nonces["conn1"] = nonceEntry{nonce: "abc123", expiresAt: time.Now().Add(time.Minute)}

	d.handleJoin("conn1", "IMMICHOPEN1", "")

	require.True(t, sig.sentContains(`"type":"auth_ok"`))
	require.True(t, sig.sentContains(`"code":"IMMICHOPEN1"`))
}

func TestHandlePasswordSubmit_ImmichValidatesWithClientBeforeAuthOK(t *testing.T) {
	d, sig := newTestDaemon(t)
	fake := &fakeImmichAuth{ok: true}
	d.sessions["IMMICHPASS1"] = &Session{
		Code: "IMMICHPASS1", ShareType: "immich", RelayOnly: true,
		immichAuth: fake,
		peers: map[string]*peer.Peer{}, relayChannels: map[string]relayTransferChannel{},
	}

	d.handlePasswordSubmit("conn1", "IMMICHPASS1", "secret")

	require.Equal(t, "secret", fake.gotPassword)
	require.True(t, sig.sentContains(`"type":"auth_ok"`))
}
```

Run: `cd agent && go test ./internal/daemon -run 'TestHandle(Join_Immich|PasswordSubmit_Immich)' -count=1`

Expected: FAIL because `immichAuth`, password-submit handling, and Immich HMAC skip do not exist.

- [ ] **Step 2: Add minimal Immich interfaces and message fields**

In `daemon.go`, extend `signaling.Message` handling by adding `Password string` to `agent/internal/signaling/client.go` `Message`:

```go
Password string `json:"password,omitempty"`
```

Add local daemon interfaces:

```go
type immichAuthenticator interface {
	ValidatePassword(ctx context.Context, password string) (bool, error)
}

type immichGalleryBackend interface {
	immichAuthenticator
}
```

Extend `Session`:

```go
IsPasswordProtected bool
immichClient         immichGalleryBackend
```

In `handleSignalingMessage`:

```go
case "password_submit":
	go d.handlePasswordSubmit(msg.ConnID, msg.Code, msg.Password)
```

Implement:

```go
func (d *Daemon) handlePasswordSubmit(connID, sessionCode, password string) {
	d.mu.RLock()
	session := d.sessions[sessionCode]
	d.mu.RUnlock()
	if session == nil || session.ShareType != "immich" || session.immichClient == nil {
		_ = d.signaling.Send(context.Background(), map[string]any{"type": "auth_fail", "conn_id": connID, "code": sessionCode})
		return
	}
	ok, err := session.immichClient.ValidatePassword(context.Background(), password)
	if err != nil || !ok {
		_ = d.signaling.Send(context.Background(), map[string]any{"type": "auth_fail", "conn_id": connID, "code": sessionCode})
		return
	}
	_ = d.signaling.Send(context.Background(), map[string]any{"type": "auth_ok", "conn_id": connID, "code": sessionCode})
}
```

In `handleJoin`, change HMAC condition:

```go
if session.Password != "" && session.ShareType != "immich" {
	// existing HMAC verification
}
```

Run: `cd agent && go test ./internal/daemon -run 'TestHandle(Join_Immich|PasswordSubmit_Immich)' -count=1`

Expected: PASS.

- [ ] **Step 3: Write failing auto-registration sync tests**

Add to `daemon_test.go`:

```go
func TestSyncImmichSharesRegistersNewAndUnregistersRemoved(t *testing.T) {
	d, sig := newTestDaemon(t)
	d.config.ImmichURL = "http://immich.lan:2283"
	d.config.ImmichAllowedHost = "immich.lan"
	d.config.ImmichAPIKey = "api"
	d.newImmichPoller = func() (immichPoller, error) {
		return &fakeImmichPoller{shares: []immich.SharedLink{
			{Key: "IMMICHNEW1", Password: "********"},
		}}, nil
	}
	d.sessions["IMMICHOLD1"] = &Session{Code: "IMMICHOLD1", ShareType: "immich", RelayOnly: true}

	require.NoError(t, d.syncImmichShares(context.Background()))

	require.True(t, sig.registeredCode("IMMICHNEW1"))
	require.True(t, sig.unregisteredCode("IMMICHOLD1"))
}
```

Run: `cd agent && go test ./internal/daemon -run TestSyncImmichSharesRegistersNewAndUnregistersRemoved -count=1`

Expected: FAIL because polling sync does not exist.

- [ ] **Step 4: Implement Immich sync loop**

Add daemon fields:

```go
newImmichPoller func() (immichPoller, error)
```

Add:

```go
type immichPoller interface {
	PollShares(ctx context.Context) ([]immich.SharedLink, error)
}
```

In `Start`, after `loadSessionsFromStore(ctx)`:

```go
if d.config.ImmichURL != "" && d.config.ImmichAPIKey != "" {
	go d.runImmichPoller(ctx)
}
```

Implement:

```go
func (d *Daemon) runImmichPoller(ctx context.Context) {
	_ = d.syncImmichShares(ctx)
	interval := time.Duration(d.config.ImmichPollInterval) * time.Second
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.syncImmichShares(ctx); err != nil {
				log.Printf("immich sync: %v", err)
			}
		}
	}
}
```

`syncImmichShares` must:
- Build or use injected poller.
- Poll `[]immich.SharedLink`.
- Register missing shares with `RegisterShareWithOptions` if the signaling client supports it.
- Save sessions with `ShareType: "immich"`, `ShareURL: "immich://"+key`, `IsPasswordProtected`, `RelayOnly: true`.
- Unregister and remove in-memory/store sessions for Immich codes no longer present.
- Close peers/relay channels for removed shares before `UnregisterShare`.

Registration call shape:

```go
opts := signaling.RegisterShareOptions{
	ShareURL:            "immich://" + link.Key,
	PreferredCode:       link.Key,
	ShareType:           "immich",
	IsPasswordProtected: link.IsPasswordProtected(),
	RelayOnly:           true,
	RelayStaticPub:      relayStaticPub,
}
code, _, err := reg.RegisterShareWithOptions(ctx, opts)
```

Run: `cd agent && go test ./internal/daemon -run TestSyncImmichSharesRegistersNewAndUnregistersRemoved -count=1`

Expected: PASS.

- [ ] **Step 5: Wire real Immich clients for new and persisted sessions**

When creating an Immich session, set:

```go
client, err := immich.New(immich.Config{
	BaseURL:     d.config.ImmichURL,
	AllowedHost: d.config.ImmichAllowedHost,
	APIKey:      d.config.ImmichAPIKey,
	ShareKey:    code,
})
```

When loading persisted sessions, branch:

```go
if entry.ShareType == "immich" {
	client, err := immich.New(immich.Config{BaseURL: cfg.ImmichURL, AllowedHost: cfg.ImmichAllowedHost, APIKey: cfg.ImmichAPIKey, ShareKey: entry.Code})
	// register with RegisterShareWithOptions using entry.IsPasswordProtected and RelayOnly true
	// session.webdavClient remains nil; session.immichClient is set
	continue
}
```

Run: `cd agent && go test ./internal/daemon -count=1`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add agent/internal/daemon/daemon.go agent/internal/daemon/daemon_test.go agent/internal/signaling/client.go agent/internal/store/store.go
git commit -m "feat(agent): sync Immich shared links"
```

## Task 7: Add Storage Backend Interface, Typed Binary Envelope, and Gallery Transfer Messages

**Files:**
- Modify: `agent/internal/transfer/manager.go`
- Modify: `agent/internal/transfer/manager_test.go`
- Modify: `agent/internal/cloudwebdav/client.go`
- Modify: `agent/internal/immich/client.go`
- Create: `signaling-server/web/src/binaryEnvelope.js`
- Create: `signaling-server/web/src/binaryEnvelope.test.js`

- [ ] **Step 1: Write failing Go transfer tests for gallery messages**

Add to `agent/internal/transfer/manager_test.go`:

```go
func TestHandleOpen_ImmichGallerySendsThumbnailListAndData(t *testing.T) {
	dc := &mockDC{}
	client := &mockGalleryClient{
		gallery: Gallery{
			AlbumName: "Summer",
			Items: []GalleryItem{{ID: "asset-1", Name: "photo.jpg", MimeType: "image/jpeg", Size: 12}},
		},
		thumbnail: []byte{0xff, 0xd8, 0xff},
	}
	mgr := NewManager(dc, client, 0)
	mgr.HandleOpen()
	time.Sleep(50 * time.Millisecond)

	require.True(t, dc.hasTextType("thumbnail_list"))
	require.Len(t, dc.binaryData, 1)
	require.Equal(t, byte(0x11), dc.binaryData[0][0], "thumbnail frames use typed binary envelope")
}

func TestHandleAssetRequestStreamsByAssetID(t *testing.T) {
	dc := &mockDC{}
	client := &mockGalleryClient{
		gallery: Gallery{Items: []GalleryItem{{ID: "asset-1", Name: "photo.jpg", MimeType: "image/jpeg", Size: 3}}},
		file: []byte("abc"),
	}
	mgr := NewManager(dc, client, 0)

	req, _ := json.Marshal(map[string]string{"type": "asset_request", "id": "asset-1", "quality": "original"})
	mgr.HandleMessage(req)
	time.Sleep(50 * time.Millisecond)

	require.True(t, dc.hasTextType("file_header"))
	require.NotEmpty(t, dc.binaryData)
	require.True(t, dc.hasTextType("chunk_end"))
}
```

Run: `cd agent && go test ./internal/transfer -run 'TestHandle(Open_Immich|AssetRequest)' -count=1`

Expected: FAIL because gallery interface and `asset_request` do not exist.

- [ ] **Step 2: Introduce backend interfaces and envelope constants**

In `manager.go`, replace `openCloudClient` with:

```go
type StorageBackend interface {
	ListFiles(subpath string) ([]cloudwebdav.FileInfo, error)
	GetFile(filePath string, w io.Writer) (int64, error)
	GetSHA1(subpath string) string
}

type GalleryBackend interface {
	ListGallery(ctx context.Context) (Gallery, error)
	GetThumbnail(ctx context.Context, id string, w io.Writer) (int64, error)
	GetAsset(ctx context.Context, id string, quality string, w io.Writer) (int64, error)
}

type Gallery struct {
	AlbumName        string        `json:"albumName"`
	AlbumDescription string        `json:"albumDescription"`
	Items            []GalleryItem `json:"items"`
}

type GalleryItem struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	MimeType  string   `json:"mimeType"`
	Width     int      `json:"width"`
	Height    int      `json:"height"`
	Size      int64    `json:"size"`
	Duration  *float64 `json:"duration"`
	SHA1      string   `json:"sha1,omitempty"`
}
```

Update `Manager.client` to `StorageBackend` and add `gallery GalleryBackend`.

Add envelope constants:

```go
const (
	binaryFrameFileChunk = byte(0x10)
	binaryFrameThumbnail = byte(0x11)
)
```

Encode thumbnail payloads as:

```go
func encodeThumbnailFrame(index uint16, jpg []byte) []byte {
	out := make([]byte, 3+len(jpg))
	out[0] = binaryFrameThumbnail
	out[1] = byte(index >> 8)
	out[2] = byte(index)
	copy(out[3:], jpg)
	return out
}
```

For new file chunks, wrap as `0x10 + bytes`. Browser Task 8 will decode both old raw chunks and new envelope so a new browser can still receive raw chunks from an older agent. Deploy the agent and web assets atomically when enabling this protocol change: an old browser receiving `0x10`-prefixed chunks would treat the prefix as file data and corrupt downloads.

- [ ] **Step 3: Implement gallery transfer flow**

Add constructor:

```go
func NewGalleryManager(dc DataChannel, backend GalleryBackend, maxDownloads int) *Manager {
	return &Manager{dc: dc, gallery: backend, maxDownloads: maxDownloads}
}
```

In `HandleOpen`:

```go
if m.gallery != nil {
	go m.sendGallery()
	return
}
```

Implement:

```go
func (m *Manager) sendGallery() {
	gallery, err := m.gallery.ListGallery(context.Background())
	if err != nil {
		m.sendError("share unavailable: " + err.Error())
		return
	}
	data, _ := json.Marshal(struct {
		Type string `json:"type"`
		Gallery
	}{Type: "thumbnail_list", Gallery: gallery})
	_ = m.dc.SendText(string(data))

	for i, item := range gallery.Items {
		if i > 65535 {
			break
		}
		var buf bytes.Buffer
		if _, err := m.gallery.GetThumbnail(context.Background(), item.ID, &buf); err != nil {
			continue
		}
		_ = m.sendWithBackpressure(encodeThumbnailFrame(uint16(i), buf.Bytes()))
	}
}
```

Extend `HandleMessage` parsing with `ID` and `Quality`, and add:

```go
case "asset_request":
	m.handleAssetRequest(msg.ID, msg.Quality)
```

Implement `handleAssetRequest` by looking up the ID in `ListGallery`, sending `file_header`, streaming `GetAsset`, sending enveloped chunks, then `chunk_end`.

Run: `cd agent && go test ./internal/transfer -count=1`

Expected: PASS.

- [ ] **Step 4: Adapt Immich client to `transfer.GalleryBackend` without import cycles**

Do not import `transfer` into `immich`. Keep Immich API types in `agent/internal/immich` and create this adapter in `daemon.go`:

```go
type immichTransferAdapter struct {
	client *immich.Client
}

func (a immichTransferAdapter) ListGallery(ctx context.Context) (transfer.Gallery, error) {
	g, err := a.client.ListGallery(ctx)
	if err != nil {
		return transfer.Gallery{}, err
	}
	items := make([]transfer.GalleryItem, len(g.Items))
	for i, item := range g.Items {
		items[i] = transfer.GalleryItem{
			ID: item.ID, Name: item.Name, MimeType: item.MimeType, Width: item.Width,
			Height: item.Height, Size: item.Size, Duration: item.Duration, SHA1: item.SHA1,
		}
	}
	return transfer.Gallery{AlbumName: g.AlbumName, AlbumDescription: g.AlbumDescription, Items: items}, nil
}

func (a immichTransferAdapter) GetThumbnail(ctx context.Context, id string, w io.Writer) (int64, error) {
	return a.client.GetThumbnail(ctx, id, w)
}

func (a immichTransferAdapter) GetAsset(ctx context.Context, id string, quality string, w io.Writer) (int64, error) {
	return a.client.GetFile(ctx, id, w)
}
```

Run: `cd agent && go test ./internal/immich ./internal/transfer ./internal/daemon -count=1`

Expected: PASS.

- [ ] **Step 5: Write and implement browser envelope helpers**

Create `signaling-server/web/src/binaryEnvelope.test.js`:

```js
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { decodeBinaryEnvelope, encodeThumbnailEnvelope, FRAME_FILE_CHUNK, FRAME_THUMBNAIL } from './binaryEnvelope.js'

test('decodes typed thumbnail envelope', () => {
  const encoded = encodeThumbnailEnvelope(258, new Uint8Array([1, 2, 3]))
  const decoded = decodeBinaryEnvelope(encoded)
  assert.equal(decoded.type, FRAME_THUMBNAIL)
  assert.equal(decoded.index, 258)
  assert.deepEqual([...decoded.payload], [1, 2, 3])
})

test('treats legacy raw binary as file chunks', () => {
  const decoded = decodeBinaryEnvelope(new Uint8Array([9, 8, 7]))
  assert.equal(decoded.type, FRAME_FILE_CHUNK)
  assert.deepEqual([...decoded.payload], [9, 8, 7])
})
```

Create `binaryEnvelope.js`:

```js
export const FRAME_FILE_CHUNK = 'file_chunk'
export const FRAME_THUMBNAIL = 'thumbnail'
const KIND_FILE_CHUNK = 0x10
const KIND_THUMBNAIL = 0x11

export function encodeThumbnailEnvelope(index, payload) {
  const out = new Uint8Array(3 + payload.length)
  out[0] = KIND_THUMBNAIL
  out[1] = (index >> 8) & 0xff
  out[2] = index & 0xff
  out.set(payload, 3)
  return out
}

export function decodeBinaryEnvelope(input) {
  const data = input instanceof Uint8Array ? input : new Uint8Array(input)
  if (data[0] === KIND_THUMBNAIL) {
    return { type: FRAME_THUMBNAIL, index: (data[1] << 8) | data[2], payload: data.slice(3) }
  }
  if (data[0] === KIND_FILE_CHUNK) {
    return { type: FRAME_FILE_CHUNK, payload: data.slice(1) }
  }
  return { type: FRAME_FILE_CHUNK, payload: data }
}
```

Run: `cd signaling-server/web && node --test src/binaryEnvelope.test.js -v`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add agent/internal/transfer agent/internal/cloudwebdav agent/internal/immich agent/internal/daemon signaling-server/web/src/binaryEnvelope.js signaling-server/web/src/binaryEnvelope.test.js
git commit -m "feat(transfer): add Immich gallery protocol"
```

## Task 8: Build Browser Gallery Mode and Immich Password Submit

**Files:**
- Create: `signaling-server/web/src/gallery.js`
- Create: `signaling-server/web/src/gallery.test.js`
- Modify: `signaling-server/web/src/app.js`
- Modify: `signaling-server/web/src/app.test.js`
- Modify: `signaling-server/web/index.html`

- [ ] **Step 1: Write failing gallery tests**

Create `signaling-server/web/src/gallery.test.js`:

```js
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { createGalleryController } from './gallery.js'

function fakeRoot() {
  return {
    innerHTML: '',
    children: [],
    appendChild(node) { this.children.push(node) },
    querySelector() { return null },
  }
}

test('gallery renders thumbnail shells from thumbnail_list', () => {
  const root = fakeRoot()
  const controller = createGalleryController({
    root,
    createObjectURL: () => 'blob:thumb',
    revokeObjectURL: () => {},
    sendAssetRequest: () => {},
  })

  controller.handleThumbnailList({
    albumName: 'Summer',
    albumDescription: 'Beach',
    items: [{ id: 'asset-1', name: 'photo.jpg', mimeType: 'image/jpeg', width: 4000, height: 3000, size: 12, duration: null }],
  })

  assert.match(root.innerHTML, /Summer/)
  assert.match(root.innerHTML, /photo.jpg/)
})

test('gallery sends asset_request when an item is opened', () => {
  const sent = []
  const controller = createGalleryController({
    root: fakeRoot(),
    createObjectURL: () => 'blob:thumb',
    revokeObjectURL: () => {},
    sendAssetRequest: (id) => sent.push(id),
  })

  controller.handleThumbnailList({ albumName: 'Summer', items: [{ id: 'asset-1', name: 'photo.jpg', mimeType: 'image/jpeg' }] })
  controller.openItem('asset-1')

  assert.deepEqual(sent, ['asset-1'])
})
```

Run: `cd signaling-server/web && node --test src/gallery.test.js -v`

Expected: FAIL because gallery module does not exist.

- [ ] **Step 2: Implement `gallery.js`**

Create a DOM-light controller with these exports:

```js
export function createGalleryController({ root, createObjectURL = URL.createObjectURL, revokeObjectURL = URL.revokeObjectURL, sendAssetRequest }) {
  const state = { items: [], thumbs: new Map(), urls: new Map() }

  function handleThumbnailList(msg) {
    state.items = msg.items || []
    state.thumbs.clear()
    root.innerHTML = renderGalleryShell(msg.albumName || 'Shared album', msg.albumDescription || '', state.items)
  }

  function handleThumbnailData(index, payload) {
    const item = state.items[index]
    if (!item) return
    const blob = new Blob([payload], { type: 'image/jpeg' })
    const url = createObjectURL(blob)
    state.urls.set(item.id, url)
    const img = root.querySelector?.(`[data-thumb-id="${cssEscape(item.id)}"]`)
    if (img) img.src = url
  }

  function openItem(id) {
    if (sendAssetRequest) sendAssetRequest(id)
  }

  function destroy() {
    for (const url of state.urls.values()) revokeObjectURL(url)
    state.urls.clear()
  }

  return { handleThumbnailList, handleThumbnailData, openItem, destroy, state }
}

export function renderGalleryShell(albumName, albumDescription, items) {
  const count = items.length
  return `
    <section class="gallery-header">
      <div>
        <h2>${escapeHTML(albumName)}</h2>
        ${albumDescription ? `<p>${escapeHTML(albumDescription)}</p>` : ''}
      </div>
      <span>${count} item${count === 1 ? '' : 's'}</span>
    </section>
    <section class="gallery-grid">
      ${items.map(renderItem).join('')}
    </section>
  `
}
```

Add small local `escapeHTML`, `cssEscape`, `renderItem`, `formatDuration` helpers. Use buttons for thumbnails so keyboard users can open them. Video assets get a duration badge.

Run: `cd signaling-server/web && node --test src/gallery.test.js -v`

Expected: PASS.

- [ ] **Step 3: Wire app mode, password_required, and auth_fail handling**

In `app.js`, import:

```js
import { createGalleryController } from './gallery.js'
import { decodeBinaryEnvelope, FRAME_FILE_CHUNK, FRAME_THUMBNAIL } from './binaryEnvelope.js'
```

Add state:

```js
let galleryMode = false
let galleryController = null
```

On init:

```js
const mode = detectPathMode(window.location.pathname)
galleryMode = mode.mode === 'gallery'
sessionCode = mode.code
if (galleryMode) {
  galleryController = createGalleryController({
    root: document.getElementById('gallery-root'),
    sendAssetRequest: (id) => transferChannel?.send(JSON.stringify({ type: 'asset_request', id, quality: 'original' })),
  })
}
```

In signaling message handler:

```js
case 'password_required':
  hideSection('join-section')
  showSection('password-section')
  updateStatus('This Immich share is password protected.')
  break
case 'auth_fail':
  showSection('password-section')
  updateStatus(`Incorrect password. ${msg.attempts_remaining ?? 0} attempts remaining.`)
  break
```

Change `submitPassword()`:

```js
function submitPassword() {
  sessionPassword = document.getElementById('password-input').value
  if (galleryMode) {
    socket.send(JSON.stringify({ type: 'password_submit', code: sessionCode, password: sessionPassword }))
    return
  }
  if (pendingNonce) {
    sendJoin()
  }
}
```

When transfer channel opens:

```js
if (galleryMode) {
  updateStatus('Loading gallery...')
} else {
  requestFileList(transferChannel, currentPath.join('/'))
}
```

In transfer text handling:

```js
case 'thumbnail_list':
  galleryController?.handleThumbnailList(msg)
  break
```

In binary handling:

```js
const decoded = decodeBinaryEnvelope(event.data)
if (decoded.type === FRAME_THUMBNAIL) {
  galleryController?.handleThumbnailData(decoded.index, decoded.payload)
  return
}
if (decoded.type === FRAME_FILE_CHUNK) {
  downloadPipeline?.write(decoded.payload)
}
```

Run: `cd signaling-server/web && node --test src/app.test.js src/gallery.test.js src/binaryEnvelope.test.js -v`

Expected: PASS.

- [ ] **Step 4: Update HTML/CSS structure**

In `index.html`, add:

```html
<section id="gallery-section" class="hidden">
  <div id="gallery-root"></div>
</section>
```

Add compact CSS:

```html
<style>
  .gallery-header { display: flex; align-items: end; justify-content: space-between; gap: 1rem; margin: 1rem 0; }
  .gallery-header h2 { margin: 0; font-size: 1.25rem; }
  .gallery-header p { margin: .25rem 0 0; color: var(--pico-muted-color); }
  .gallery-grid { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: .75rem; }
  .gallery-item { position: relative; aspect-ratio: 1; border: 0; padding: 0; overflow: hidden; background: var(--pico-muted-border-color); border-radius: 8px; }
  .gallery-item img { width: 100%; height: 100%; object-fit: cover; display: block; }
  .gallery-duration { position: absolute; right: .4rem; bottom: .4rem; padding: .1rem .35rem; border-radius: 4px; background: rgba(0,0,0,.72); color: white; font-size: .75rem; }
  @media (max-width: 640px) { .gallery-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
</style>
```

Run: `cd signaling-server/web && npm test`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add signaling-server/web/index.html signaling-server/web/src/app.js signaling-server/web/src/app.test.js signaling-server/web/src/gallery.js signaling-server/web/src/gallery.test.js
git commit -m "feat(web): add Immich gallery mode"
```

## Task 9: Vendor lightGallery and Add Lightbox Asset Loading

**Files:**
- Modify: `signaling-server/web/package.json`
- Create/Modify: `signaling-server/web/package-lock.json` if npm creates it.
- Create: `signaling-server/web/src/vendor/lightgallery/`
- Modify: `signaling-server/web/src/gallery.js`
- Modify: `signaling-server/web/src/gallery.test.js`
- Modify: `signaling-server/web/index.html`

- [ ] **Step 1: Add pinned dependency for vendoring**

Run:

```bash
cd signaling-server/web
npm install --save-dev lightgallery@2.9.0
mkdir -p src/vendor/lightgallery
cp node_modules/lightgallery/lightgallery.es5.min.js src/vendor/lightgallery/lightgallery.es5.min.js
cp node_modules/lightgallery/css/lightgallery.css src/vendor/lightgallery/lightgallery.css
cp -R node_modules/lightgallery/fonts src/vendor/lightgallery/fonts
```

Expected: `package.json`, lockfile, and vendored files are updated.

- [ ] **Step 2: Load vendored CSS/JS**

In `index.html`, add:

```html
<link rel="stylesheet" href="/src/vendor/lightgallery/lightgallery.css">
<script src="/src/vendor/lightgallery/lightgallery.es5.min.js"></script>
```

In `gallery.js`, accept an optional `lightGallery` initializer:

```js
export function createGalleryController({ root, lightGallery = globalThis.lightGallery, createObjectURL = URL.createObjectURL, revokeObjectURL = URL.revokeObjectURL, sendAssetRequest }) {
  // existing state
  function initLightbox() {
    if (!lightGallery || !root.querySelector) return
    const grid = root.querySelector('.gallery-grid')
    if (!grid) return
    state.lightbox = lightGallery(grid, { selector: '.gallery-item', download: true })
  }
  // call initLightbox after thumbnail_list render
}
```

Keep ShareBridge in charge of actual full asset fetching: `openItem(id)` still sends `asset_request`; lightGallery provides navigation/keyboard/zoom scaffolding and item affordances.

- [ ] **Step 3: Add test for optional lightGallery init**

Add to `gallery.test.js`:

```js
test('gallery initializes lightGallery when available', () => {
  let initialized = false
  const root = fakeRoot()
  root.querySelector = () => ({})
  const controller = createGalleryController({
    root,
    lightGallery: () => { initialized = true; return { destroy() {} } },
    createObjectURL: () => 'blob:thumb',
    revokeObjectURL: () => {},
    sendAssetRequest: () => {},
  })

  controller.handleThumbnailList({ albumName: 'Summer', items: [] })
  assert.equal(initialized, true)
})
```

Run: `cd signaling-server/web && node --test src/gallery.test.js -v`

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add signaling-server/web/package.json signaling-server/web/package-lock.json signaling-server/web/index.html signaling-server/web/src/gallery.js signaling-server/web/src/gallery.test.js signaling-server/web/src/vendor/lightgallery
git commit -m "feat(web): vendor gallery lightbox"
```

## Task 10: Accept Immich in Agent APIs Without Making Manual Creation the Primary Path

**Files:**
- Modify: `agent/internal/web/api.go`
- Modify: `agent/internal/web/api_v1.go`
- Modify: `agent/internal/web/api_test.go`
- Modify: `agent/internal/web/api_v1_test.go`
- Modify: `agent/internal/web/templates/share-form.html`

- [ ] **Step 1: Write failing validation tests**

Add to `agent/internal/web/api_v1_test.go`:

```go
func TestV1CreateShareAcceptsImmichTypeWhenConfigured(t *testing.T) {
	ws, daemon := newTestWebServerWithDaemon(t)
	daemon.config.ImmichURL = "http://immich.lan:2283"
	daemon.config.ImmichAllowedHost = "immich.lan"
	daemon.config.ImmichAPIKey = "api"

	body := strings.NewReader(`{"share_url":"immich://KEY","share_type":"immich"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/shares", body)
	rec := httptest.NewRecorder()
	ws.v1CreateShareHandler(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code)
}
```

Run: `cd agent && go test ./internal/web -run TestV1CreateShareAcceptsImmichTypeWhenConfigured -count=1`

Expected: FAIL because `immich` is rejected.

- [ ] **Step 2: Add share-type validator helper**

In `api.go` or a small shared file in `agent/internal/web`, add:

```go
func validateShareType(shareType string, cfg *config.Config) error {
	switch shareType {
	case "opencloud", "nextcloud":
		return nil
	case "immich":
		if cfg == nil || cfg.ImmichURL == "" || cfg.ImmichAllowedHost == "" || cfg.ImmichAPIKey == "" {
			return fmt.Errorf("immich is not configured")
		}
		return nil
	default:
		return fmt.Errorf("share_type must be 'opencloud', 'nextcloud', or 'immich'")
	}
}
```

Use it in both form and JSON handlers. Manual Immich creation is a convenience path for tests and admin users; auto-discovery remains the primary product flow. `Daemon.CreateSession` must branch on `shareType == "immich"` and register a relay-only external-code session after extracting `KEY` from `immich://KEY`.

The manual branch should reject non-`immich://` URLs with `share_url must be immich://KEY for manual Immich shares`, set `RelayOnly: true`, set `ShareURL: "immich://"+key`, create the Immich client for that key, and call `RegisterShareWithOptions` with `PreferredCode: key`, `ShareType: "immich"`, and `IsPasswordProtected` from Immich shared-link metadata.

Run: `cd agent && go test ./internal/web -count=1`

Expected: PASS.

- [ ] **Step 3: Update share form copy without making a landing page**

In `share-form.html`, add an Immich option only if the form already has share type controls:

```html
<option value="immich">Immich</option>
```

Add a compact helper line near the share URL input:

```html
<small>Immich links are normally discovered automatically from your configured Immich server.</small>
```

Run: `cd agent && go test ./internal/web -count=1`

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add agent/internal/web
git commit -m "feat(agent): recognize Immich share type"
```

## Task 11: End-to-End Verification and Docs Updates

**Files:**
- Modify: `signaling-server/README.md`
- Modify: `OpenCloudShare_plan.md` only if it has stale roadmap status for Slice 14.
- Modify: `docs/superpowers/specs/2026-05-16-immich-integration-design.md` only if implementation discoveries differ from the spec.

- [ ] **Step 1: Run full automated verification**

Run:

```bash
cd signaling-server && go test ./...
cd signaling-server/web && npm test
cd agent && go test ./...
```

Expected: all commands PASS.

- [ ] **Step 2: Run a local browser smoke test**

Start the signaling server:

```bash
cd signaling-server
go run ./cmd/server
```

In another terminal, start an agent configured against a local Immich test instance or mocked development instance:

```bash
cd agent
IMMICH_URL=http://localhost:2283 IMMICH_ALLOWED_HOST=localhost:2283 IMMICH_API_KEY=sb_immich_read_key_example go run ./cmd/agent daemon
```

Manual checks:
- Create an unprotected Immich shared album.
- Visit `http://localhost:<server-port>/i/<key>`.
- Confirm the browser receives `ice_config`/relay policy, loads gallery thumbnails, and can download one asset.
- Create a protected Immich shared album.
- Visit `http://localhost:<server-port>/i/<key>`.
- Confirm `password_required` appears before peer creation.
- Submit a wrong password five times and confirm the WebSocket closes with a clear error.
- Submit the correct password in a fresh session and confirm gallery loads.
- Delete or expire the Immich share, wait one poll interval, and confirm the agent unregisters it and active recipients receive `share has been removed`.

Expected: all manual checks behave as described.

- [ ] **Step 3: Update docs with exact implemented API paths**

If implementation verified paths differ from the spec, update `docs/superpowers/specs/2026-05-16-immich-integration-design.md` with:

```markdown
Implementation verified against the target Immich version used for testing:
- Shared link info: `GET /api/shared-links/me?key={key}`
- Album asset IDs: `GET /api/timeline/buckets?key={key}&albumId={albumId}` and `GET /api/timeline/bucket?key={key}&albumId={albumId}&timeBucket={bucket}` when `/shared-links/me` has no expanded `assets`
- Password validation: `POST /api/shared-links/login?key={key}` with JSON body `{"password":"..."}`; store returned `immich_shared_link_token` cookie in memory
- Share polling: `GET /api/shared-links` with `x-api-key`
- Thumbnail: `GET /api/assets/{id}/thumbnail?key={key}`
- Asset download: `GET /api/assets/{id}/original?key={key}` or the verified download endpoint
```

Add README configuration:

````markdown
### Immich Backend

Set these on the agent:

```bash
IMMICH_URL=http://immich.lan:2283
IMMICH_ALLOWED_HOST=immich.lan:2283
IMMICH_API_KEY=sb_immich_read_key_example
IMMICH_POLL_INTERVAL=30
```

Immich shares are relay-only and are served at `/i/<immich-share-key>`.
````

Run:

```bash
rg -n "Immich|IMMICH_|/i/" signaling-server/README.md docs/superpowers/specs/2026-05-16-immich-integration-design.md OpenCloudShare_plan.md
```

Expected: docs mention the implemented behavior and do not claim server-side HMAC password privacy for Immich.

- [ ] **Step 4: Commit**

```bash
git add signaling-server/README.md OpenCloudShare_plan.md docs/superpowers/specs/2026-05-16-immich-integration-design.md
git commit -m "docs: document Immich integration"
```

## Final Verification

- [ ] Run server tests: `cd signaling-server && go test ./...`
- [ ] Run browser tests: `cd signaling-server/web && npm test`
- [ ] Run agent tests: `cd agent && go test ./...`
- [ ] Run whitespace check: `git diff --check`
- [ ] Review staged changes: `git status --short && git log --oneline -5`

Expected final state:
- All automated tests pass.
- `/s/` file-share behavior is unchanged.
- `/i/` routes serve gallery mode.
- Immich shares register with external keys up to 128 URL-safe characters.
- Protected Immich shares do not create relay/direct transfer channels before successful Immich password validation.
- Immich shares are always relay-only.
- Thumbnails and file chunks are separated by typed binary framing.
- No Immich recipient password is persisted.
