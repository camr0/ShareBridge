# Immich Agent Reinstall Sync Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Immich share-code reservations account-owned, safely reclaimable after an agent reinstall or key replacement, and auditable when another account attempts to claim them.

**Architecture:** Keep each session as both the durable account reservation (derived through its API key) and the current signaling binding. Centralize claim decisions in the server transaction, use live API-key connection state as the takeover guard, persist cross-account attempts outside the rejected transaction, and expose only the expected live-owner conflict as a structured error that Immich reconciliation may skip.

**Tech Stack:** Go, PocketBase, coder/websocket, `errors.Is`/`errors.As`, testify, standard Go logging.

---

## File map

- Create `signaling-server/migrations/7_create_security_events.go` for the internal audit collection.
- Create `signaling-server/internal/handler/security_events.go` for stable security logs and persistence.
- Modify `signaling-server/internal/handler/agent_ws.go` for account-owned reclaim decisions.
- Modify server migration and handler tests for schema, ownership, audit, and rotation coverage.
- Modify `agent/internal/signaling/client.go` and tests for the exported sentinel.
- Modify `agent/internal/daemon/daemon.go` and tests for narrow reconciliation skipping.

### Task 1: Add the internal security-events collection

**Files:**
- Create: `signaling-server/migrations/7_create_security_events.go`
- Modify: `signaling-server/migrations/migrations_test.go`

- [ ] **Step 1: Write the failing migration test**

Append:

```go
func TestMigration7_CreateSecurityEvents(t *testing.T) {
	app, err := tests.NewTestApp(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { app.Cleanup() })
	require.NoError(t, app.Bootstrap())
	require.NoError(t, app.RunSystemMigrations())
	require.NoError(t, migrations.CreateCollections(app))
	require.NoError(t, migrations.CreateSecurityEvents(app))

	collection, err := app.FindCollectionByNameOrId("security_events")
	require.NoError(t, err)
	for _, name := range []string{
		"event_type", "code", "attempting_account_id", "attempting_api_key_id",
		"attempting_agent_id", "owning_account_id", "source_address", "created",
	} {
		require.NotNil(t, collection.Fields.GetByName(name), "missing field %q", name)
	}
	require.Nil(t, collection.ListRule)
	require.Nil(t, collection.ViewRule)
	require.Nil(t, collection.CreateRule)
	require.Nil(t, collection.UpdateRule)
	require.Nil(t, collection.DeleteRule)
	require.IsType(t, &core.TextField{}, collection.Fields.GetByName("attempting_account_id"))
	require.IsType(t, &core.TextField{}, collection.Fields.GetByName("attempting_api_key_id"))
	require.IsType(t, &core.TextField{}, collection.Fields.GetByName("owning_account_id"))
	require.NoError(t, migrations.CreateSecurityEvents(app))
}
```

Add the `core` import required by the type assertions.

- [ ] **Step 2: Run it and verify failure**

```bash
cd signaling-server
go test ./migrations -run TestMigration7_CreateSecurityEvents -v
```

Expected: build failure because `CreateSecurityEvents` is undefined.

- [ ] **Step 3: Implement the migration**

Create:

```go
package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() { m.Register(CreateSecurityEvents, nil) }

func CreateSecurityEvents(app core.App) error {
	if _, err := app.FindCollectionByNameOrId("security_events"); err == nil {
		return nil
	}
	collection := core.NewBaseCollection("security_events")
	collection.Fields.Add(
		&core.TextField{Name: "event_type", Required: true},
		&core.TextField{Name: "code", Required: true},
		&core.TextField{Name: "attempting_account_id", Required: true},
		&core.TextField{Name: "attempting_api_key_id", Required: true},
		&core.TextField{Name: "attempting_agent_id", Required: true},
		&core.TextField{Name: "owning_account_id", Required: true},
		&core.TextField{Name: "source_address", Required: true},
		&core.AutodateField{Name: "created", System: true, OnCreate: true},
	)
	collection.ListRule = nil
	collection.ViewRule = nil
	collection.CreateRule = nil
	collection.UpdateRule = nil
	collection.DeleteRule = nil
	return app.Save(collection)
}
```

- [ ] **Step 4: Run all migration tests**

```bash
cd signaling-server
go test ./migrations -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add signaling-server/migrations/7_create_security_events.go signaling-server/migrations/migrations_test.go
git commit -m "feat(server): add security events collection"
```

### Task 2: Make reservation reclaim account-owned

**Files:**
- Modify: `signaling-server/internal/handler/agent_ws.go`
- Modify: `signaling-server/internal/handler/agent_ws_test.go`

- [ ] **Step 1: Enable the migration in handler test setup**

After `AddImmichSessionFields` in `setupAgentTestApp`:

```go
err = migrations.CreateSecurityEvents(testApp)
require.NoError(t, err)
```

- [ ] **Step 2: Write failing reclaim tests**

Add:

```go
func TestClaimSessionCodeSameKeyRebindsNewAgentID(t *testing.T) {
	app, cleanup := setupAgentTestApp(t)
	defer cleanup()
	user, err := createTestUser(app, "same-key@example.com")
	require.NoError(t, err)
	key, err := createTestAPIKey(app, user.Id, "secret")
	require.NoError(t, err)
	_, err = createTestSession(app, key.Id, "old-agent", "REBINDSAME1")
	require.NoError(t, err)

	record, reconnected, err := claimSessionCode(
		app, "REBINDSAME1", key.Id, user.Id, "new-agent",
		nil, nil, "", "immich", false, func(string) bool { return true },
	)
	require.NoError(t, err)
	require.True(t, reconnected)
	require.Equal(t, key.Id, record.GetString("api_key_id"))
	require.Equal(t, "new-agent", record.GetString("agent_id"))
}

func TestClaimSessionCodeReplacementKeyHonorsLiveOwner(t *testing.T) {
	app, cleanup := setupAgentTestApp(t)
	defer cleanup()
	user, err := createTestUser(app, "replacement@example.com")
	require.NoError(t, err)
	oldKey, err := createTestAPIKey(app, user.Id, "old-secret")
	require.NoError(t, err)
	newKey, err := createTestAPIKey(app, user.Id, "new-secret")
	require.NoError(t, err)
	_, err = createTestSession(app, oldKey.Id, "old-agent", "REBINDKEY01")
	require.NoError(t, err)

	_, _, err = claimSessionCode(
		app, "REBINDKEY01", newKey.Id, user.Id, "new-agent",
		nil, nil, "", "immich", false,
		func(keyID string) bool { return keyID == oldKey.Id },
	)
	require.ErrorIs(t, err, errSessionOwnedByActiveAgent)

	record, reconnected, err := claimSessionCode(
		app, "REBINDKEY01", newKey.Id, user.Id, "new-agent",
		nil, nil, "", "immich", false, func(string) bool { return false },
	)
	require.NoError(t, err)
	require.True(t, reconnected)
	require.Equal(t, newKey.Id, record.GetString("api_key_id"))
	require.Equal(t, "new-agent", record.GetString("agent_id"))
}
```

Add the expiry case:

```go
func TestClaimSessionCodeExpiredReservationIsReleased(t *testing.T) {
	app, cleanup := setupAgentTestApp(t)
	defer cleanup()
	owner, err := createTestUser(app, "expired-owner@example.com")
	require.NoError(t, err)
	ownerKey, err := createTestAPIKey(app, owner.Id, "owner-secret")
	require.NoError(t, err)
	claimant, err := createTestUser(app, "expired-claimant@example.com")
	require.NoError(t, err)
	claimantKey, err := createTestAPIKey(app, claimant.Id, "claimant-secret")
	require.NoError(t, err)
	session, err := createTestSession(app, ownerKey.Id, "old-agent", "EXPIREDCLM1")
	require.NoError(t, err)
	session.Set("expires_at", time.Now().Add(-time.Minute))
	require.NoError(t, app.Save(session))

	record, reconnected, err := claimSessionCode(
		app, "EXPIREDCLM1", claimantKey.Id, claimant.Id, "new-agent",
		nil, nil, "", "immich", false, func(string) bool { return true },
	)
	require.NoError(t, err)
	require.False(t, reconnected)
	require.Equal(t, claimantKey.Id, record.GetString("api_key_id"))
	require.Equal(t, "new-agent", record.GetString("agent_id"))
}
```

- [ ] **Step 3: Run focused tests and verify failure**

```bash
cd signaling-server
go test ./internal/handler -run 'TestClaimSessionCode' -v
```

Expected: build failure because the callback and sentinel do not exist.

- [ ] **Step 4: Add explicit claim outcomes**

Define:

```go
var errCodeAlreadyInUse = errors.New("code already in use")
var errSessionOwnedByActiveAgent = errors.New("session owned by active agent")

type sessionHijackAttemptError struct{ owningAccountID string }

func (e *sessionHijackAttemptError) Error() string {
	return "session code owned by different account"
}
```

Extend `claimSessionCode` with final argument `isAPIKeyConnected func(string) bool`. Project `s.api_key_id AS api_key_id` into:

```go
APIKeyID string `db:"api_key_id"`
```

After loading the record, replace UUID authorization with:

```go
expiresAtValue := record.GetDateTime("expires_at")
if !expiresAtValue.IsZero() && expiresAtValue.Time().Before(time.Now()) {
	if err := txApp.Delete(record); err != nil {
		return err
	}
	createRelayOnly := false
	if relayOnly != nil {
		createRelayOnly = *relayOnly
	}
	if err := createSession(txApp, code, apiKeyID, agentID, expiresAt, createRelayOnly, relayStaticPub, shareType, isPasswordProtected); err != nil {
		return err
	}
	claimed, err = getSessionByCode(txApp, code)
	return err
}
if existing.AccountID != accountID {
	return &sessionHijackAttemptError{owningAccountID: existing.AccountID}
}
if existing.APIKeyID != apiKeyID && isAPIKeyConnected(existing.APIKeyID) {
	return errSessionOwnedByActiveAgent
}
record.Set("api_key_id", apiKeyID)
record.Set("agent_id", agentID)
```

Keep metadata updates and save in the same transaction. Remove the old agent-ID rejection. Pass `h.AgentConnected` from the handler.

- [ ] **Step 5: Run claim tests**

```bash
cd signaling-server
go test ./internal/handler -run 'TestClaimSessionCode' -v
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add signaling-server/internal/handler/agent_ws.go signaling-server/internal/handler/agent_ws_test.go
git commit -m "feat(server): make session reclaim account-owned"
```

### Task 3: Audit cross-account hijacking attempts

**Files:**
- Create: `signaling-server/internal/handler/security_events.go`
- Modify: `signaling-server/internal/handler/agent_ws.go`
- Modify: `signaling-server/internal/handler/agent_ws_test.go`

- [ ] **Step 1: Write the failing WebSocket audit test**

Create two accounts and keys, reserve `CROSSWS001` for the owner, connect the attacker, attempt registration, and assert:

```go
var response map[string]any
require.NoError(t, json.Unmarshal(raw, &response))
require.Equal(t, "error", response["type"])
require.Equal(t, "code already in use", response["message"])
require.NotContains(t, response, "error_code")
```

Capture logs with `log.SetOutput(&logs)` and restore the prior writer. Assert the stable prefix, raw code, both account IDs, attempting key ID, and attempting agent ID. Query:

```go
events, err := app.FindRecordsByFilter(
	"security_events", "event_type = 'session_hijack_attempt'", "", 10, 0,
)
require.NoError(t, err)
require.Len(t, events, 1)
event := events[0]
require.Equal(t, "CROSSWS001", event.GetString("code"))
require.Equal(t, attacker.Id, event.GetString("attempting_account_id"))
require.Equal(t, attackerKey.Id, event.GetString("attempting_api_key_id"))
require.Equal(t, "attacker-agent", event.GetString("attempting_agent_id"))
require.Equal(t, owner.Id, event.GetString("owning_account_id"))
require.NotEmpty(t, event.GetString("source_address"))
```

Re-read the session and assert no ownership or metadata changed.

- [ ] **Step 2: Write the telemetry-failure test**

Delete the `security_events` collection before the same cross-account attempt. Assert the generic rejection remains, the session is unchanged, and logs contain `SECURITY session_hijack_attempt_persistence_failed`.

Also add a same-account live-owner WebSocket test. Keep the old key connected, register from a second key on the same account, and assert:

```go
var response map[string]any
require.NoError(t, json.Unmarshal(raw, &response))
require.Equal(t, "error", response["type"])
require.Equal(t, "session_owned_by_active_agent", response["error_code"])
require.Equal(t, "session owned by different agent", response["message"])
events, err := app.FindRecordsByFilter("security_events", "", "", 10, 0)
require.NoError(t, err)
require.Empty(t, events)
```

- [ ] **Step 3: Run tests and verify failure**

```bash
cd signaling-server
go test ./internal/handler -run 'TestAgentWS_RegisterShare_CrossAccount' -v
```

Expected: FAIL because no event or stable log exists.

- [ ] **Step 4: Create the helper**

```go
package handler

import (
	"fmt"
	"log"
	"github.com/pocketbase/pocketbase/core"
)

const sessionHijackAttemptEvent = "session_hijack_attempt"

type securityEvent struct {
	EventType, Code, AttemptingAccountID, AttemptingAPIKeyID string
	AttemptingAgentID, OwningAccountID, SourceAddress         string
}

func recordSecurityEvent(app core.App, event securityEvent) error {
	collection, err := app.FindCollectionByNameOrId("security_events")
	if err != nil {
		return fmt.Errorf("find security_events collection: %w", err)
	}
	record := core.NewRecord(collection)
	record.Set("event_type", event.EventType)
	record.Set("code", event.Code)
	record.Set("attempting_account_id", event.AttemptingAccountID)
	record.Set("attempting_api_key_id", event.AttemptingAPIKeyID)
	record.Set("attempting_agent_id", event.AttemptingAgentID)
	record.Set("owning_account_id", event.OwningAccountID)
	record.Set("source_address", event.SourceAddress)
	if err := app.Save(record); err != nil {
		return fmt.Errorf("save security event: %w", err)
	}
	return nil
}

func reportSessionHijackAttempt(app core.App, event securityEvent) {
	log.Printf("SECURITY session_hijack_attempt code=%q attempting_account_id=%q attempting_api_key_id=%q attempting_agent_id=%q owning_account_id=%q source_address=%q",
		event.Code, event.AttemptingAccountID, event.AttemptingAPIKeyID,
		event.AttemptingAgentID, event.OwningAccountID, event.SourceAddress)
	if err := recordSecurityEvent(app, event); err != nil {
		log.Printf("SECURITY session_hijack_attempt_persistence_failed code=%q attempting_account_id=%q attempting_api_key_id=%q error=%q",
			event.Code, event.AttemptingAccountID, event.AttemptingAPIKeyID, err)
	}
}
```

- [ ] **Step 5: Wire source address and errors**

Capture `r.RemoteAddr` in `AgentWS` and pass it through `handleRegisterShare`. Map:

```go
if errors.Is(err, errSessionOwnedByActiveAgent) {
	hub.SendDirect(ctx, conn, map[string]string{
		"type": "error", "error_code": "session_owned_by_active_agent",
		"message": "session owned by different agent",
	})
	return
}
var hijack *sessionHijackAttemptError
if errors.As(err, &hijack) {
	reportSessionHijackAttempt(app, securityEvent{
		EventType: sessionHijackAttemptEvent, Code: code,
		AttemptingAccountID: accountID, AttemptingAPIKeyID: apiKeyID,
		AttemptingAgentID: agentID, OwningAccountID: hijack.owningAccountID,
		SourceAddress: sourceAddress,
	})
	hub.SendDirect(ctx, conn, map[string]string{
		"type": "error", "message": "code already in use",
	})
	return
}
```

Keep ordinary collision and database-error branches afterward. The event is recorded only after the rejected claim transaction returns.

- [ ] **Step 6: Run handler tests**

```bash
cd signaling-server
go test ./internal/handler -v
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add signaling-server/internal/handler/security_events.go signaling-server/internal/handler/agent_ws.go signaling-server/internal/handler/agent_ws_test.go
git commit -m "feat(server): audit session hijacking attempts"
```

### Task 4: Verify rotation recovery

**Files:**
- Modify: `signaling-server/internal/handler/api_keys_test.go`

- [ ] **Step 1: Extend the rotation test**

After existing transfer assertions:

```go
require.Equal(t, "agent-1", sessionRecord.GetString("agent_id"))
record, reconnected, err := claimSessionCode(
	app, "ROTATE01", response.ID, alice.Id, "agent-after-reinstall",
	nil, nil, "", "immich", false, sessionHub.AgentConnected,
)
require.NoError(t, err)
require.True(t, reconnected)
require.Equal(t, response.ID, record.GetString("api_key_id"))
require.Equal(t, "agent-after-reinstall", record.GetString("agent_id"))
require.False(t, sessionHub.AgentConnected(oldKey.Id))
events, err := app.FindRecordsByFilter("security_events", "", "", 10, 0)
require.NoError(t, err)
require.Empty(t, events)
```

- [ ] **Step 2: Run the rotation test**

```bash
cd signaling-server
go test ./internal/handler -run TestRotateAPIKey_TransfersSessionsAndRevokesOldKey -v
```

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add signaling-server/internal/handler/api_keys_test.go
git commit -m "test(server): cover rotation recovery after reinstall"
```

### Task 5: Map ownership responses to an agent sentinel

**Files:**
- Modify: `agent/internal/signaling/client.go`
- Modify: `agent/internal/signaling/client_test.go`

- [ ] **Step 1: Write the failing table test**

Add `testify/require` and:

```go
func TestRegisterShareWithOptionsMapsActiveOwnerErrors(t *testing.T) {
	tests := []struct {
		name, response string
		wantOwner      bool
	}{
		{"structured", "{\"type\":\"error\",\"error_code\":\"session_owned_by_active_agent\",\"message\":\"changed wording\"}", true},
		{"legacy", "{\"type\":\"error\",\"message\":\"session owned by different agent\"}", true},
		{"unrelated", "{\"type\":\"error\",\"message\":\"database error\"}", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newRegisterShareTestServer(t, func(t *testing.T, raw []byte) []byte {
				return []byte(tt.response)
			})
			defer server.Close()
			client := New(strings.Replace(server.URL, "http://", "ws://", 1), "api", "agent")
			require.NoError(t, client.Connect(context.Background()))
			go client.Listen(context.Background())
			_, _, err := client.RegisterShareWithOptions(context.Background(), RegisterShareOptions{
				ShareURL: "immich://IMMICHERR1", PreferredCode: "IMMICHERR1", ShareType: "immich",
			})
			require.Error(t, err)
			require.Equal(t, tt.wantOwner, errors.Is(err, ErrSessionOwnedByActiveAgent))
		})
	}
}
```

- [ ] **Step 2: Run it and verify failure**

```bash
cd agent
go test ./internal/signaling -run TestRegisterShareWithOptionsMapsActiveOwnerErrors -v
```

Expected: build failure because the sentinel is undefined.

- [ ] **Step 3: Implement parsing**

Import `errors` and define:

```go
var ErrSessionOwnedByActiveAgent = errors.New("session owned by active agent")
```

Add to `Message`:

```go
ErrorCode string `json:"error_code,omitempty"`
```

Replace registration error handling:

```go
if resp.Type == "error" {
	if resp.ErrorCode == "session_owned_by_active_agent" ||
		(resp.ErrorCode == "" && resp.Err == "session owned by different agent") {
		return "", false, fmt.Errorf("server error: %w", ErrSessionOwnedByActiveAgent)
	}
	return "", false, fmt.Errorf("server error: %s", resp.Err)
}
```

- [ ] **Step 4: Run signaling tests**

```bash
cd agent
go test ./internal/signaling -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/signaling/client.go agent/internal/signaling/client_test.go
git commit -m "feat(agent): identify active session ownership conflicts"
```

### Task 6: Continue Immich reconciliation after an active-owner conflict

**Files:**
- Modify: `agent/internal/daemon/daemon.go`
- Modify: `agent/internal/daemon/daemon_test.go`

- [ ] **Step 1: Write the failing reconciliation test**

Create `TestSyncImmichSharesSkipsActiveOwnerAndCompletesReconciliation` with this setup:

```go
d, sig := newTestDaemon(t)
d.config.ImmichURL = "http://immich.lan:2283"
d.config.ImmichAllowedHost = "immich.lan:2283"
d.config.ImmichAPIKey = "api"
d.newImmichPoller = func() (immichPoller, error) {
	return &fakeImmichPoller{shares: []immich.SharedLink{
		{Key: "IMMICHOWNED1", Type: "ALBUM"},
		{Key: "IMMICHVALID1", Type: "ALBUM"},
	}}, nil
}
d.sessions["IMMICHSTALE1"] = &Session{
	Code: "IMMICHSTALE1", ShareType: "immich", RelayOnly: true,
	peers: map[string]*peer.Peer{}, relayChannels: map[string]relayTransferChannel{},
}
require.NoError(t, d.store.SaveSession(store.SessionEntry{
	Code: "IMMICHSTALE1", ShareURL: "immich://IMMICHSTALE1",
	ShareType: "immich", RelayOnly: true,
}))
var logs bytes.Buffer
previous := log.Writer()
log.SetOutput(&logs)
defer log.SetOutput(previous)
```

Configure the mock to return the sentinel only for `IMMICHOWNED1`:

```go
sig.registerShare = func(ctx context.Context, shareURL, preferredCode string, relayOnly bool, relayStaticPub string) (string, bool, error) {
	if preferredCode == "IMMICHOWNED1" {
		return "", false, signaling.ErrSessionOwnedByActiveAgent
	}
	return preferredCode, false, nil
}
```

Capture logs, run sync, and assert:

```go
require.NoError(t, d.syncImmichShares(context.Background()))
require.Contains(t, logs.String(), "immich sync: skipping shared link IMMICHOWNED1: owned by another active agent installation on this account")
require.Nil(t, d.GetSession("IMMICHOWNED1"))
require.Nil(t, d.store.GetSession("IMMICHOWNED1"))
require.NotNil(t, d.GetSession("IMMICHVALID1"))
require.NotNil(t, d.store.GetSession("IMMICHVALID1"))
require.Nil(t, d.GetSession("IMMICHSTALE1"))
require.Nil(t, d.store.GetSession("IMMICHSTALE1"))
require.True(t, sig.unregisteredCode("IMMICHSTALE1"))
```

- [ ] **Step 2: Write the unrelated-error test**

Create `TestSyncImmichSharesStopsOnUnrelatedRegistrationError` with the same Immich configuration. Poll `IMMICHFAIL1` then `IMMICHAFTER1`, retain `IMMICHSTALE1` in memory, and configure:

```go
sig.registerShare = func(ctx context.Context, shareURL, preferredCode string, relayOnly bool, relayStaticPub string) (string, bool, error) {
	if preferredCode == "IMMICHFAIL1" {
		return "", false, errors.New("database error")
	}
	return preferredCode, false, nil
}
err := d.syncImmichShares(context.Background())
```

Assert:

```go
require.ErrorContains(t, err, "database error")
require.False(t, sig.registeredCode("IMMICHAFTER1"))
require.NotNil(t, d.GetSession("IMMICHSTALE1"))
```

- [ ] **Step 3: Run focused tests and verify failure**

```bash
cd agent
go test ./internal/daemon -run 'TestSyncImmichShares(SkipsActiveOwner|StopsOnUnrelated)' -v
```

Expected: active-owner test fails because the sentinel aborts sync.

- [ ] **Step 4: Implement the narrow skip**

Replace the registration error block:

```go
session, err := d.registerImmichShare(ctx, link, relayStaticPub)
if err != nil {
	if errors.Is(err, signaling.ErrSessionOwnedByActiveAgent) {
		log.Printf("immich sync: skipping shared link %s: owned by another active agent installation on this account", link.Key)
		continue
	}
	return err
}
```

Keep the existing ordering that adds the link to `seen` before registration.

- [ ] **Step 5: Run daemon tests**

```bash
cd agent
go test ./internal/daemon -v
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add agent/internal/daemon/daemon.go agent/internal/daemon/daemon_test.go
git commit -m "fix(agent): skip active Immich ownership conflicts"
```

### Task 7: Full verification and conformance

**Files:**
- Verify all files changed in Tasks 1-6.

- [ ] **Step 1: Format changed Go files**

```bash
gofmt -w signaling-server/migrations/7_create_security_events.go signaling-server/migrations/migrations_test.go signaling-server/internal/handler/security_events.go signaling-server/internal/handler/agent_ws.go signaling-server/internal/handler/agent_ws_test.go signaling-server/internal/handler/api_keys_test.go agent/internal/signaling/client.go agent/internal/signaling/client_test.go agent/internal/daemon/daemon.go agent/internal/daemon/daemon_test.go
```

- [ ] **Step 2: Run full server suite**

```bash
cd signaling-server
go test ./...
```

Expected: PASS for every package.

- [ ] **Step 3: Run full agent suite**

```bash
cd agent
go test ./...
```

Expected: PASS for every package.

- [ ] **Step 4: Run repository checks**

```bash
git diff --check
git status --short
```

Expected: no whitespace errors and only intentional changes, or a clean tree after task commits.

- [ ] **Step 5: Review against the approved design**

```bash
git diff 8b4f625..HEAD -- signaling-server agent
git log --oneline 8b4f625..HEAD
```

Confirm UUID is not an authorization boundary; different same-account keys are blocked only while the bound key is live; cross-account claims are generic, serious, durable, and non-mutating; rotation creates no event; only active-owner errors are skipped; and reconciliation still performs later additions and removals.

- [ ] **Step 6: Commit formatting corrections only if needed**

```bash
git add signaling-server agent
git commit -m "style: format Immich reservation recovery changes"
```

Do not create an empty commit when the tree is clean.
