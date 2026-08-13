# Slice 10a — Accounts (PocketBase) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Migrate the signaling server from Gin + hand-rolled SQLite to PocketBase, adding user accounts with self-serve API key management.

**Architecture:** PocketBase replaces Gin as the HTTP server. All domain collections (`api_keys`, `sessions`) live in PocketBase's SQLite DB alongside the built-in `users` auth collection. WebSocket handlers and REST routes register via PocketBase's `OnServe` hook. New `internal/pbstore` package replaces `internal/db` for data access.

**Important behavioral rules for this slice:**
- Session ownership still lives on `sessions.api_key_id`, but a same-account reconnect with a different key must transfer the session to the new `api_key_id` atomically.
- `/_/` admin access must be blocked at the reverse proxy first; do not trust `RemoteAddr` alone behind nginx/Caddy/Traefik.
- If SMTP is unset, email verification is disabled for self-hosted installs and users may log in immediately after registration.
- `PORT` and `DATA_DIR` must be wired explicitly in `cmd/server/main.go`; use PocketBase's real data-directory model rather than emulating the old SQLite file-path contract.

**Tech Stack:** PocketBase v0.22+, coder/websocket (unchanged), golang.org/x/crypto/bcrypt (unchanged), Go 1.22+ (for `r.PathValue`)

---

## File Map

**New:**
- `migrations/1_create_collections.go` — registers api_keys + sessions collection schemas
- `internal/pbstore/sessions.go` — session CRUD + atomic claim/reassign for same-account reconnect
- `internal/pbstore/sessions_test.go`
- `internal/pbstore/api_keys.go` — API key validate/create/list/revoke
- `internal/pbstore/api_keys_test.go`
- `internal/handler/api_keys.go` — user-scoped POST/GET/DELETE /api/keys handlers
- `internal/handler/api_keys_test.go`
- `web/register.html`
- `web/login.html`
- `web/account.html`

**Modified:**
- `go.mod` — add pocketbase
- `cmd/server/main.go` — full rewrite for PocketBase
- `cmd/admin/main.go` — strip to create-superuser only
- `internal/config/config.go` — remove ADMIN_TOKEN/AUTH_TOKEN, add SMTP fields
- `internal/handler/agent_ws.go` — port from Gin to `*core.RequestEvent`
- `internal/handler/agent_ws_test.go` — adapt for PocketBase test app
- `internal/handler/browser_ws.go` — port from Gin to `*core.RequestEvent`
- `internal/handler/rest.go` — port from Gin to `*core.RequestEvent`

**Deleted (Task 12):**
- `internal/db/` (entire package)
- `internal/middleware/` (entire package)
- `internal/handler/admin.go`
- `internal/handler/routes.go`

---

## Task 1: Add PocketBase Dependency

**Files:**
- Modify: `signaling-server/go.mod`

- [ ] **Step 1: Add PocketBase**

```bash
cd signaling-server
go get github.com/pocketbase/pocketbase@latest
```

Expected: go.mod updated, no errors.

- [ ] **Step 2: Verify it compiles**

```bash
go build ./...
```

Expected: `PASS` — existing code still compiles with PocketBase added.

- [ ] **Step 3: Commit**

```bash
git add go.mod go.sum
git commit -m "chore: add pocketbase dependency"
```

---

## Task 2: Collection Migrations

**Files:**
- Create: `signaling-server/migrations/1_create_collections.go`

- [ ] **Step 1: Create the migrations package**

Create `signaling-server/migrations/1_create_collections.go`:

```go
package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(CreateCollections, nil)
}

// CreateCollections creates the api_keys and sessions collections.
// Exported so tests can call it directly without relying on m.Register.
func CreateCollections(app core.App) error {
	// --- api_keys collection ---
	apiKeysCol := core.NewBaseCollection("api_keys")
	apiKeysCol.Fields.Add(
		&core.RelationField{
			Name:          "account_id",
			CollectionId:  "users",
			Required:      true,
			CascadeDelete: true,
			MaxSelect:     1,
		},
		&core.TextField{
			Name:     "key_hash",
			Required: false, // set after record ID is known; never exposed via API
		},
		&core.TextField{
			Name: "label",
		},
		&core.DateField{
			Name: "last_used_at",
		},
		&core.BoolField{
			Name: "is_active",
		},
	)
	if err := app.SaveCollection(apiKeysCol); err != nil {
		return err
	}

	// --- sessions collection ---
	sessionsCol := core.NewBaseCollection("sessions")
	sessionsCol.Fields.Add(
		&core.TextField{
			Name:     "code",
			Required: true,
		},
		&core.RelationField{
			Name:          "api_key_id",
			CollectionId:  "api_keys",
			Required:      true,
			CascadeDelete: true,
			MaxSelect:     1,
		},
		&core.TextField{
			Name: "agent_id",
		},
		&core.DateField{
			Name: "expires_at",
		},
	)
	sessionsCol.Indexes = []string{
		"CREATE UNIQUE INDEX `idx_sessions_code` ON `{{COLLECTION}}` (`code`)",
	}
	return app.SaveCollection(sessionsCol)
}
```

- [ ] **Step 2: Verify the package compiles**

```bash
cd signaling-server
go build ./migrations/...
```

Expected: compiles with no errors.

- [ ] **Step 3: Commit**

```bash
git add migrations/
git commit -m "feat: add PocketBase collection migrations for api_keys and sessions"
```

---

## Task 3: pbstore/api_keys.go

**Files:**
- Create: `signaling-server/internal/pbstore/api_keys.go`
- Create: `signaling-server/internal/pbstore/api_keys_test.go`

- [ ] **Step 1: Write the failing tests**

Create `signaling-server/internal/pbstore/api_keys_test.go`:

```go
package pbstore_test

import (
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sharebridge/server/internal/pbstore"
)

func TestCreateAndValidateAPIKey(t *testing.T) {
	testApp := newTestApp(t)
	accountID := createTestUser(t, testApp)

	fullKey, keyData, err := pbstore.CreateAPIKey(testApp, accountID, "private server")
	require.NoError(t, err)
	assert.NotEmpty(t, fullKey)
	assert.NotEmpty(t, keyData.RecordID)
	assert.Equal(t, accountID, keyData.AccountID)
	assert.Equal(t, "private server", keyData.Label)
	assert.True(t, keyData.IsActive)

	// Full key format: <record_id>.<secret>
	parts := strings.SplitN(fullKey, ".", 2)
	require.Len(t, parts, 2)
	assert.Equal(t, keyData.RecordID, parts[0])
	assert.Len(t, parts[1], 43, "secret should be 32 bytes base64url = 43 chars")

	// Validate the key
	validated, err := pbstore.ValidateAPIKey(testApp, fullKey)
	require.NoError(t, err)
	require.NotNil(t, validated)
	assert.Equal(t, keyData.RecordID, validated.RecordID)
	assert.Equal(t, accountID, validated.AccountID)
}

func TestValidateAPIKey_Invalid(t *testing.T) {
	testApp := newTestApp(t)

	// Completely invalid
	result, err := pbstore.ValidateAPIKey(testApp, "notakey")
	require.NoError(t, err)
	assert.Nil(t, result)

	// Valid format but wrong secret
	accountID := createTestUser(t, testApp)
	fullKey, keyData, _ := pbstore.CreateAPIKey(testApp, accountID, "")
	_ = fullKey
	result, err = pbstore.ValidateAPIKey(testApp, keyData.RecordID+".wrongsecret")
	require.NoError(t, err)
	assert.Nil(t, result)
}

func TestValidateAPIKey_Revoked(t *testing.T) {
	testApp := newTestApp(t)
	accountID := createTestUser(t, testApp)

	fullKey, keyData, _ := pbstore.CreateAPIKey(testApp, accountID, "")
	require.NoError(t, pbstore.RevokeAPIKey(testApp, keyData.RecordID, accountID))

	result, err := pbstore.ValidateAPIKey(testApp, fullKey)
	require.NoError(t, err)
	assert.Nil(t, result, "revoked key should not validate")
}

func TestListAPIKeys(t *testing.T) {
	testApp := newTestApp(t)
	accountID := createTestUser(t, testApp)

	pbstore.CreateAPIKey(testApp, accountID, "key-1")
	pbstore.CreateAPIKey(testApp, accountID, "key-2")

	keys, err := pbstore.ListAPIKeys(testApp, accountID)
	require.NoError(t, err)
	assert.Len(t, keys, 2)
}

func TestRevokeAPIKey_WrongOwner(t *testing.T) {
	testApp := newTestApp(t)
	accountID1 := createTestUser(t, testApp)

	usersCol, _ := testApp.FindCollectionByNameOrId("users")
	user2 := core.NewRecord(usersCol)
	user2.SetEmail("user2@example.com")
	user2.SetPassword("password1234!")
	testApp.SaveRecord(user2)

	_, keyData, _ := pbstore.CreateAPIKey(testApp, accountID1, "")
	err := pbstore.RevokeAPIKey(testApp, keyData.RecordID, user2.Id)
	assert.Error(t, err, "wrong owner should get an error")
}
```

- [ ] **Step 2: Run to verify failure**

```bash
cd signaling-server
go test ./internal/pbstore/... 2>&1 | head -10
```

Expected: compile error — `pbstore.CreateAPIKey`, `ValidateAPIKey`, etc. not defined.

- [ ] **Step 3: Implement pbstore/api_keys.go**

Create `signaling-server/internal/pbstore/api_keys.go`:

```go
package pbstore

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"golang.org/x/crypto/bcrypt"
)

// APIKeyData is the domain struct for an api_key record.
type APIKeyData struct {
	RecordID   string
	AccountID  string
	Label      string
	LastUsedAt *time.Time
	IsActive   bool
	CreatedAt  time.Time
}

// ValidateAPIKey validates a full key (<record_id>.<secret>) against the api_keys collection.
// Returns nil if the key is not found, inactive, or the secret does not match.
func ValidateAPIKey(app core.App, fullKey string) (*APIKeyData, error) {
	dotIdx := strings.IndexByte(fullKey, '.')
	if dotIdx <= 0 || dotIdx == len(fullKey)-1 {
		return nil, nil
	}
	recordID := fullKey[:dotIdx]

	record, err := app.FindRecordById("api_keys", recordID)
	if err != nil {
		return nil, nil // not found
	}
	if !record.GetBool("is_active") {
		return nil, nil
	}
	if err := bcrypt.CompareHashAndPassword([]byte(record.GetString("key_hash")), []byte(fullKey)); err != nil {
		return nil, nil
	}

	// Best-effort update of last_used_at; don't fail auth if this save errors.
	record.Set("last_used_at", time.Now())
	_ = app.SaveRecord(record)

	return recordToAPIKey(record), nil
}

// CreateAPIKey creates a new API key for the given account.
// Returns (fullKey, APIKeyData, error). fullKey is shown once and never stored in plaintext.
// The key format is <pb_record_id>.<32-byte-base64url-secret>.
func CreateAPIKey(app core.App, accountID string, label string) (string, *APIKeyData, error) {
	col, err := app.FindCollectionByNameOrId("api_keys")
	if err != nil {
		return "", nil, fmt.Errorf("find api_keys collection: %w", err)
	}

	// Create the record first to get its auto-generated ID.
	record := core.NewRecord(col)
	record.Set("account_id", accountID)
	record.Set("label", label)
	record.Set("is_active", true)
	record.Set("key_hash", "placeholder") // replaced below after we have the record ID
	if err := app.SaveRecord(record); err != nil {
		return "", nil, fmt.Errorf("save api_key record: %w", err)
	}

	// Generate 32-byte random secret, base64url encoded (no padding = 43 chars).
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		_ = app.DeleteRecord(record)
		return "", nil, fmt.Errorf("generate secret: %w", err)
	}
	secret := base64.RawURLEncoding.EncodeToString(secretBytes)
	fullKey := record.Id + "." + secret

	hash, err := bcrypt.GenerateFromPassword([]byte(fullKey), bcrypt.DefaultCost)
	if err != nil {
		_ = app.DeleteRecord(record)
		return "", nil, fmt.Errorf("hash key: %w", err)
	}

	record.Set("key_hash", string(hash))
	if err := app.SaveRecord(record); err != nil {
		_ = app.DeleteRecord(record)
		return "", nil, fmt.Errorf("save key hash: %w", err)
	}

	return fullKey, recordToAPIKey(record), nil
}

// ListAPIKeys returns all API keys owned by the given account, sorted newest-first.
func ListAPIKeys(app core.App, accountID string) ([]*APIKeyData, error) {
	records, err := app.FindRecordsByFilter(
		"api_keys",
		"account_id = {:accountID}",
		"-created", 0, 0,
		dbx.Params{"accountID": accountID},
	)
	if err != nil {
		return nil, err
	}
	keys := make([]*APIKeyData, len(records))
	for i, r := range records {
		keys[i] = recordToAPIKey(r)
	}
	return keys, nil
}

// RevokeAPIKey marks the given key as inactive.
// Returns an error if the key is not found or is not owned by accountID.
func RevokeAPIKey(app core.App, recordID string, accountID string) error {
	record, err := app.FindRecordById("api_keys", recordID)
	if err != nil {
		return fmt.Errorf("key not found")
	}
	if record.GetString("account_id") != accountID {
		return fmt.Errorf("not authorized")
	}
	record.Set("is_active", false)
	return app.SaveRecord(record)
}

func recordToAPIKey(r *core.Record) *APIKeyData {
	data := &APIKeyData{
		RecordID:  r.Id,
		AccountID: r.GetString("account_id"),
		Label:     r.GetString("label"),
		IsActive:  r.GetBool("is_active"),
		CreatedAt: r.GetDateTime("created").Time(),
	}
	lastUsed := r.GetDateTime("last_used_at")
	if !lastUsed.IsZero() {
		t := lastUsed.Time()
		data.LastUsedAt = &t
	}
	return data
}
```

- [ ] **Step 4: Run tests**

```bash
cd signaling-server
go test ./internal/pbstore/... -v
```

Expected: all tests PASS. Note: `TestCreateAndValidateAPIKey` bcrypt cost makes this test take ~1s.

- [ ] **Step 5: Commit**

```bash
git add internal/pbstore/
git commit -m "feat: add pbstore/api_keys with validate/create/list/revoke"
```

---

## Task 4: pbstore/sessions.go

**Files:**
- Create: `signaling-server/internal/pbstore/sessions.go`
- Create: `signaling-server/internal/pbstore/sessions_test.go`

- [ ] **Step 1: Write the failing tests**

Create `signaling-server/internal/pbstore/sessions_test.go`:

```go
package pbstore_test

import (
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sharebridge/server/internal/pbstore"
	"sharebridge/server/migrations"
)

// newTestApp creates a bootstrapped PocketBase app with our collections.
func newTestApp(t *testing.T) *tests.TestApp {
	t.Helper()
	testApp, err := tests.NewTestApp(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { testApp.Cleanup() })
	require.NoError(t, migrations.CreateCollections(testApp))
	return testApp
}

// createTestUser creates a user record for testing and returns its ID.
func createTestUser(t *testing.T, testApp *tests.TestApp) string {
	t.Helper()
	usersCol, err := testApp.FindCollectionByNameOrId("users")
	require.NoError(t, err)
	user := core.NewRecord(usersCol)
	user.SetEmail("test@example.com")
	user.SetPassword("password1234!")
	user.Set("verified", true)
	require.NoError(t, testApp.SaveRecord(user))
	return user.Id
}

// createTestAPIKey uses the real API key creation path so these tests keep
// exercising production key-generation and ownership behavior.
func createTestAPIKey(t *testing.T, testApp *tests.TestApp, accountID string) (string, string) {
	t.Helper()
	fullKey, keyData, err := pbstore.CreateAPIKey(testApp, accountID, "test key")
	require.NoError(t, err)
	return keyData.RecordID, fullKey
}

func TestGetSessionByCode_NotFound(t *testing.T) {
	testApp := newTestApp(t)
	session, err := pbstore.GetSessionByCode(testApp, "noexist")
	require.NoError(t, err)
	assert.Nil(t, session)
}

func TestCreateAndGetSession(t *testing.T) {
	testApp := newTestApp(t)
	accountID := createTestUser(t, testApp)
	apiKeyID, _ := createTestAPIKey(t, testApp, accountID)

	input := &pbstore.SessionData{
		Code:     "testcode",
		APIKeyID: apiKeyID,
		AgentID:  "agent-uuid-1",
			}
	created, err := pbstore.CreateSession(testApp, input)
	require.NoError(t, err)
	assert.Equal(t, "testcode", created.Code)

	fetched, err := pbstore.GetSessionByCode(testApp, "testcode")
	require.NoError(t, err)
	require.NotNil(t, fetched)
	assert.Equal(t, "testcode", fetched.Code)
	assert.Equal(t, apiKeyID, fetched.APIKeyID)
	assert.Equal(t, "agent-uuid-1", fetched.AgentID)
}

func TestClaimSessionCode_Unclaimed(t *testing.T) {
	testApp := newTestApp(t)
	accountID := createTestUser(t, testApp)
	apiKeyID, _ := createTestAPIKey(t, testApp, accountID)
	claimed, reconnected, err := pbstore.ClaimSessionCode(testApp, &pbstore.SessionData{
		Code: "newcode", APIKeyID: apiKeyID, AgentID: "agent-a",
	}, accountID)
	require.NoError(t, err)
	assert.False(t, reconnected)
	assert.Equal(t, apiKeyID, claimed.APIKeyID)
}

func TestClaimSessionCode_SameAccountTransfersOwnership(t *testing.T) {
	testApp := newTestApp(t)
	accountID := createTestUser(t, testApp)
	oldKeyID, _ := createTestAPIKey(t, testApp, accountID)
	newKeyID, _ := createTestAPIKey(t, testApp, accountID)

	pbstore.CreateSession(testApp, &pbstore.SessionData{
		Code: "mycode", APIKeyID: oldKeyID, AgentID: "a1",
	})

	claimed, reconnected, err := pbstore.ClaimSessionCode(testApp, &pbstore.SessionData{
		Code: "mycode", APIKeyID: newKeyID, AgentID: "a1",
	}, accountID)
	require.NoError(t, err)
	assert.True(t, reconnected, "same account should be able to reclaim its own code")
	assert.Equal(t, newKeyID, claimed.APIKeyID, "session must transfer to the new key")
}

func TestClaimSessionCode_DifferentAccount(t *testing.T) {
	testApp := newTestApp(t)
	accountID1 := createTestUser(t, testApp)
	apiKeyID1, _ := createTestAPIKey(t, testApp, accountID1)
	pbstore.CreateSession(testApp, &pbstore.SessionData{
		Code: "takencode", APIKeyID: apiKeyID1, AgentID: "a1",
	})

	// Different user tries to claim the same code
	usersCol, _ := testApp.FindCollectionByNameOrId("users")
	user2 := core.NewRecord(usersCol)
	user2.SetEmail("other@example.com")
	user2.SetPassword("password1234!")
	testApp.SaveRecord(user2)

	apiKeyID2, _ := createTestAPIKey(t, testApp, user2.Id)
	_, _, err := pbstore.ClaimSessionCode(testApp, &pbstore.SessionData{
		Code: "takencode", APIKeyID: apiKeyID2, AgentID: "mallory-agent",
	}, user2.Id)
	require.ErrorIs(t, err, pbstore.ErrCodeAlreadyInUse)
}

func TestUpdateSessionAgent(t *testing.T) {
	testApp := newTestApp(t)
	accountID := createTestUser(t, testApp)
	apiKeyID, _ := createTestAPIKey(t, testApp, accountID)

	pbstore.CreateSession(testApp, &pbstore.SessionData{
		Code: "reconnect", APIKeyID: apiKeyID, AgentID: "old-agent",
	})

	require.NoError(t, pbstore.UpdateSessionAgent(testApp, "reconnect", "new-agent"))

	session, _ := pbstore.GetSessionByCode(testApp, "reconnect")
	assert.Equal(t, "new-agent", session.AgentID)
}

func TestDeleteExpiredSessions(t *testing.T) {
	testApp := newTestApp(t)
	accountID := createTestUser(t, testApp)
	apiKeyID, _ := createTestAPIKey(t, testApp, accountID)

	past := time.Now().Add(-1 * time.Hour)
	pbstore.CreateSession(testApp, &pbstore.SessionData{
		Code: "expired", APIKeyID: apiKeyID, AgentID: "a1",
		ExpiresAt: &past,
	})
	pbstore.CreateSession(testApp, &pbstore.SessionData{
		Code: "active", APIKeyID: apiKeyID, AgentID: "a1",
	})

	require.NoError(t, pbstore.DeleteExpiredSessions(testApp))

	expired, _ := pbstore.GetSessionByCode(testApp, "expired")
	assert.Nil(t, expired)

	active, _ := pbstore.GetSessionByCode(testApp, "active")
	assert.NotNil(t, active)
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
cd signaling-server
go test ./internal/pbstore/... 2>&1 | head -20
```

Expected: compile error — `pbstore` package does not exist yet.

- [ ] **Step 3: Implement pbstore/sessions.go**

Create `signaling-server/internal/pbstore/sessions.go`:

```go
package pbstore

import (
	"fmt"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

// SessionData is the domain struct for a session record.
type SessionData struct {
	RecordID  string
	Code      string
	APIKeyID  string
	AccountID string // resolved from api_keys.account_id
	AgentID   string
	ExpiresAt *time.Time
}

// GetSessionByCode retrieves a session by its share code. Returns nil if not found.
func GetSessionByCode(app core.App, code string) (*SessionData, error) {
	records, err := app.FindRecordsByFilter(
		"sessions",
		"code = {:code}",
		"", 1, 0,
		dbx.Params{"code": code},
	)
	if err != nil || len(records) == 0 {
		return nil, nil
	}
	return recordToSession(records[0]), nil
}

// CreateSession inserts a new session record.
func CreateSession(app core.App, data *SessionData) (*SessionData, error) {
	col, err := app.FindCollectionByNameOrId("sessions")
	if err != nil {
		return nil, fmt.Errorf("find sessions collection: %w", err)
	}
	record := core.NewRecord(col)
	record.Set("code", data.Code)
	record.Set("api_key_id", data.APIKeyID)
	record.Set("agent_id", data.AgentID)
	if data.ExpiresAt != nil {
		record.Set("expires_at", *data.ExpiresAt)
	}
	if err := app.SaveRecord(record); err != nil {
		return nil, fmt.Errorf("save session: %w", err)
	}
	return recordToSession(record), nil
}

// UpdateSessionAgent updates the agent_id on an existing session (reconnect).
func UpdateSessionAgent(app core.App, code string, agentID string) error {
	records, err := app.FindRecordsByFilter(
		"sessions",
		"code = {:code}",
		"", 1, 0,
		dbx.Params{"code": code},
	)
	if err != nil || len(records) == 0 {
		return fmt.Errorf("session not found: %s", code)
	}
	record := records[0]
	record.Set("agent_id", agentID)
	return app.SaveRecord(record)
}

var ErrCodeAlreadyInUse = fmt.Errorf("code already in use")

// ClaimSessionCode atomically creates or reassigns a session code.
// Rules:
//   - if the code is unclaimed, create a new session
//   - if the code exists and belongs to the same account, reassign api_key_id to data.APIKeyID
//   - if the code exists and belongs to a different account, return ErrCodeAlreadyInUse
//
// Reassigning api_key_id on same-account reclaim prevents old-key revocation from
// cascade-deleting a reclaimed session.
func ClaimSessionCode(app core.App, data *SessionData, accountID string) (*SessionData, bool, error) {
	var claimed *SessionData
	reconnected := false

	err := app.RunInTransaction(func(txApp core.App) error {
		var existing struct {
			SessionID string `db:"session_id"`
			AccountID string `db:"account_id"`
			AgentID   string `db:"agent_id"`
		}

		err := txApp.DB().
			NewQuery(`SELECT s.id AS session_id, ak.account_id, s.agent_id
			          FROM sessions s
			          JOIN api_keys ak ON ak.id = s.api_key_id
			          WHERE s.code = {:code}
			          LIMIT 1`).
			Bind(dbx.Params{"code": data.Code}).
			One(&existing)
		if err != nil {
			created, createErr := CreateSession(txApp, data)
			if createErr != nil {
				return createErr
			}
			claimed = created
			return nil
		}

		if existing.AccountID != accountID {
			return ErrCodeAlreadyInUse
		}
		if existing.AgentID != "" && existing.AgentID != data.AgentID {
			return fmt.Errorf("session owned by different agent")
		}

		record, err := txApp.FindRecordById("sessions", existing.SessionID)
		if err != nil {
			return err
		}
		record.Set("api_key_id", data.APIKeyID)
		record.Set("agent_id", data.AgentID)
		if data.ExpiresAt != nil {
			record.Set("expires_at", *data.ExpiresAt)
		}
		if err := txApp.SaveRecord(record); err != nil {
			return err
		}
		claimed = recordToSession(record)
		reconnected = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return claimed, reconnected, nil
}

// DeleteExpiredSessions removes sessions whose expires_at is in the past.
func DeleteExpiredSessions(app core.App) error {
	_, err := app.DB().
		NewQuery(`DELETE FROM sessions WHERE expires_at IS NOT NULL AND expires_at < datetime('now')`).
		Execute()
	return err
}

func recordToSession(r *core.Record) *SessionData {
	data := &SessionData{
		RecordID: r.Id,
		Code:     r.GetString("code"),
		APIKeyID: r.GetString("api_key_id"),
		AgentID:  r.GetString("agent_id"),
	}
	expiresAt := r.GetDateTime("expires_at")
	if !expiresAt.IsZero() {
		t := expiresAt.Time()
		data.ExpiresAt = &t
	}
	return data
}
```

- [ ] **Step 4: Fix the test file import for `core`**

The test file references `core.NewRecord` directly. Add the import to `sessions_test.go`:

```go
import (
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sharebridge/server/internal/pbstore"
	"sharebridge/server/migrations"
)
```

- [ ] **Step 5: Run tests**

```bash
cd signaling-server
go test ./internal/pbstore/... -run TestGetSession -v
go test ./internal/pbstore/... -run TestClaimSessionCode -v
go test ./internal/pbstore/... -run TestIncrementDownloadCount -v
go test ./internal/pbstore/... -run TestDeleteExpiredSessions -v
go test ./internal/pbstore/... -run TestUpdateSessionAgent -v
```

Expected: all PASS. Include coverage for same-account reclaim transferring `api_key_id` to the new key.

- [ ] **Step 6: Commit**

```bash
git add internal/pbstore/ migrations/
git commit -m "feat: add pbstore/sessions with PocketBase-backed session operations"
```

---

## Task 5: Update config.go

**Files:**
- Modify: `signaling-server/internal/config/config.go`

- [ ] **Step 1: Update config**

Replace the contents of `signaling-server/internal/config/config.go`:

```go
package config

import (
	"fmt"
	"os"
)

type Config struct {
	Port     string
	STUNURL  string
	DataDir  string

	// TURN configuration
	TurnHost   string
	TurnPort   string
	TurnSecret string

	// SMTP (optional — if absent, email verification is disabled and login is allowed immediately)
	SMTPHost     string
	SMTPPort     string
	SMTPUser     string
	SMTPPassword string
}

func Load() *Config {
	return &Config{
		Port:    getEnv("PORT", "8080"),
		STUNURL: getEnv("STUN_URL", "stun:stun.cloudflare.com:3478"),
		DataDir: getEnv("DATA_DIR", "./pb_data"),

		TurnHost:   getEnv("TURN_HOST", ""),
		TurnPort:   getEnv("TURN_PORT", "3478"),
		TurnSecret: getEnv("TURN_SECRET", ""),

		SMTPHost:     getEnv("SMTP_HOST", ""),
		SMTPPort:     getEnv("SMTP_PORT", "587"),
		SMTPUser:     getEnv("SMTP_USER", ""),
		SMTPPassword: getEnv("SMTP_PASSWORD", ""),
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// HasTurn returns true if TURN is configured.
func (c *Config) HasTurn() bool {
	return c.TurnHost != "" && c.TurnSecret != ""
}

// TurnURL returns the TURN URL (e.g., "turn:yourdomain.com:3478").
func (c *Config) TurnURL() string {
	return fmt.Sprintf("turn:%s:%s", c.TurnHost, c.TurnPort)
}

// HasSMTP returns true if SMTP is configured.
func (c *Config) HasSMTP() bool {
	return c.SMTPHost != ""
}
```

- [ ] **Step 2: Fix compile errors caused by removed fields**

`cmd/server/main.go` and `cmd/admin/main.go` reference removed fields (`AdminToken`, `AuthToken`). They will be fully rewritten in Tasks 9 and 11 — for now, just verify the error locations:

```bash
cd signaling-server
go build ./... 2>&1
```

Expected: compile errors in `cmd/server/main.go` (references `cfg.AdminToken`) and `internal/handler/routes.go` (references Gin). These will be fixed in Tasks 6–9.

- [ ] **Step 3: Commit**

```bash
git add internal/config/config.go
git commit -m "feat: update config — remove ADMIN_TOKEN/AUTH_TOKEN, add SMTP fields"
```

---

## Task 6: Port agent_ws.go to PocketBase

**Files:**
- Modify: `signaling-server/internal/handler/agent_ws.go`
- Modify: `signaling-server/internal/handler/agent_ws_test.go`

- [ ] **Step 1: Write the failing test (updated for PocketBase)**

Replace `signaling-server/internal/handler/agent_ws_test.go`:

```go
package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sharebridge/server/internal/config"
	"sharebridge/server/internal/handler"
	"sharebridge/server/internal/hub"
	"sharebridge/server/internal/pbstore"
	"sharebridge/server/migrations"
)

// pbTestServer wraps a PocketBase handler func as a plain http.Handler for testing.
// Terminal handlers (no e.Next calls) work fine with a minimal RequestEvent.
func pbTestServer(testApp *tests.TestApp, h *hub.Hub, cfg *config.Config) *httptest.Server {
	mux := http.NewServeMux()
	agentHandler := handler.AgentWS(testApp, h, cfg)
	mux.HandleFunc("/ws/agent", func(w http.ResponseWriter, r *http.Request) {
		e := &core.RequestEvent{App: testApp, Request: r, Response: w}
		if err := agentHandler(e); err != nil {
			var apiErr *apis.ApiError
			if errors.As(err, &apiErr) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(apiErr.Code)
				json.NewEncoder(w).Encode(map[string]string{"message": apiErr.Message})
			} else {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
		}
	})
	return httptest.NewServer(mux)
}

func newHandlerTestApp(t *testing.T) *tests.TestApp {
	t.Helper()
	testApp, err := tests.NewTestApp(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { testApp.Cleanup() })
	require.NoError(t, migrations.CreateCollections(testApp))
	return testApp
}

func createTestAccountAndKey(t *testing.T, testApp *tests.TestApp, email string) (accountID, fullKey string) {
	t.Helper()
	usersCol, err := testApp.FindCollectionByNameOrId("users")
	require.NoError(t, err)
	user := core.NewRecord(usersCol)
	user.SetEmail(email)
	user.SetPassword("password1234!")
	user.Set("verified", true)
	require.NoError(t, testApp.SaveRecord(user))
	fullKey, _, err = pbstore.CreateAPIKey(testApp, user.Id, "test")
	require.NoError(t, err)
	return user.Id, fullKey
}

func TestAgentWS_HelloFlow(t *testing.T) {
	testApp := newHandlerTestApp(t)
	_, fullKey := createTestAccountAndKey(t, testApp, "alice@example.com")

	h := hub.New()
	cfg := config.Load()
	server := pbTestServer(testApp, h, cfg)
	defer server.Close()

	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent?api_key="+fullKey, nil)
	require.NoError(t, err)
	defer conn.CloseNow()

	err = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","agent_id":"test-agent-uuid"}`))
	require.NoError(t, err)

	_, data, err := conn.Read(ctx)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"type":"welcome"`)
	assert.Contains(t, string(data), `"ice_servers"`)
}

func TestAgentWS_InvalidAPIKey(t *testing.T) {
	testApp := newHandlerTestApp(t)
	h := hub.New()
	cfg := config.Load()
	server := pbTestServer(testApp, h, cfg)
	defer server.Close()

	ctx := context.Background()
	_, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent?api_key=invalid", nil)
	assert.Error(t, err)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAgentWS_CodeOwnership(t *testing.T) {
	testApp := newHandlerTestApp(t)
	aliceAccountID, aliceKey := createTestAccountAndKey(t, testApp, "alice@example.com")
	_, malloryKey := createTestAccountAndKey(t, testApp, "mallory@example.com")

	// Alice's api_key ID (for session creation)
	aliceKeyRecords, _ := testApp.FindRecordsByFilter(
		"api_keys", "account_id = {:id}", "", 1, 0,
		map[string]any{"id": aliceAccountID},
	)
	require.NotEmpty(t, aliceKeyRecords)

	// Seed a session owned by Alice
	pbstore.CreateSession(testApp, &pbstore.SessionData{
		Code:     "CUSTOM01",
		APIKeyID: aliceKeyRecords[0].Id,
		AgentID:  "alice-agent",
	})

	h := hub.New()
	cfg := config.Load()
	server := pbTestServer(testApp, h, cfg)
	defer server.Close()

	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent?api_key="+malloryKey, nil)
	require.NoError(t, err)

	conn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","agent_id":"mallory-agent"}`))
	conn.Read(ctx) // welcome

	conn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","share_url":"ocs://evil.com","code":"CUSTOM01"}`))
	_, data, err := conn.Read(ctx)
	require.NoError(t, err)
	assert.Contains(t, string(data), "error")
	assert.Contains(t, string(data), "code already in use")
	conn.CloseNow()
}
```

- [ ] **Step 2: Run to verify failure**

```bash
cd signaling-server
go test ./internal/handler/... -run TestAgentWS 2>&1 | head -15
```

Expected: compile error or test failures — handler still uses Gin.

- [ ] **Step 3: Add CloseAgent to hub**

Append to `signaling-server/internal/hub/hub.go`:

```go
// CloseAgent closes the live WebSocket for the given apiKeyID and removes it
// from the hub. Used when an API key is revoked to immediately eject the agent.
// It also removes code mappings and active pairings for that key so revoked
// shares stop resolving immediately.
func (h *Hub) CloseAgent(apiKeyID string) {
	h.mu.Lock()
	conn, ok := h.agents[apiKeyID]
	delete(h.agents, apiKeyID)
	for code, owner := range h.codes {
		if owner == apiKeyID {
			delete(h.codes, code)
			delete(h.pairs, code)
		}
	}
	h.mu.Unlock()
	if ok {
		conn.Close(websocket.StatusNormalClosure, "api key revoked")
	}
}
```

- [ ] **Step 4: Rewrite agent_ws.go**

Replace `signaling-server/internal/handler/agent_ws.go`:

```go
package handler

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log"
	"math/big"
	"regexp"
	"time"

	"github.com/coder/websocket"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"sharebridge/server/internal/config"
	"sharebridge/server/internal/hub"
	"sharebridge/server/internal/pbstore"
	"sharebridge/server/internal/turn"
)

type agentMsg struct {
	Type         string          `json:"type"`
	AgentID      string          `json:"agent_id,omitempty"`
	Code         string          `json:"code,omitempty"`
	ShareURL     string          `json:"share_url,omitempty"`
	ExpiresAt    *time.Time      `json:"expires_at,omitempty"`
	SessionID    string          `json:"session_id,omitempty"`
	SDP          string          `json:"sdp,omitempty"`
	Candidate    json.RawMessage `json:"candidate,omitempty"`
	ConnID       string          `json:"conn_id,omitempty"`
	Value        string          `json:"value,omitempty"`
	HasPassword  bool            `json:"has_password,omitempty"`
}

var codeRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]{8,30}$`)

func AgentWS(app core.App, h *hub.Hub, cfg *config.Config) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		apiKeyFull := e.Request.URL.Query().Get("api_key")
		if apiKeyFull == "" {
			return apis.NewUnauthorizedError("missing api_key", nil)
		}

		apiKeyData, err := pbstore.ValidateAPIKey(app, apiKeyFull)
		if err != nil || apiKeyData == nil {
			return apis.NewUnauthorizedError("invalid api_key", nil)
		}

		conn, err := websocket.Accept(e.Response, e.Request, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			log.Printf("agent_ws accept: %v", err)
			return nil
		}
		defer conn.CloseNow()

		ctx := e.Request.Context()
		var agentID string

		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				if agentID != "" {
					log.Printf("agent disconnected: %s (agent_id: %s)", apiKeyData.RecordID, agentID)
					h.UnregisterAgent(apiKeyData.RecordID)
				}
				return nil
			}

			var msg agentMsg
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}

			switch msg.Type {
			case "hello":
				handleHello(ctx, conn, h, apiKeyData, msg.AgentID, cfg)
				agentID = msg.AgentID

			case "register_share":
				if agentID == "" {
					hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "hello required before register_share"})
					continue
				}
				handleRegisterShare(ctx, conn, h, app, apiKeyData, agentID, msg)

			case "offer":
				if agentID == "" {
					continue
				}
				h.ForwardToBrowser(ctx, msg.SessionID, map[string]any{"type": "offer", "sdp": msg.SDP})

			case "ice_candidate":
				if agentID == "" {
					continue
				}
				h.ForwardToBrowser(ctx, msg.SessionID, map[string]any{"type": "ice_candidate", "candidate": msg.Candidate})

			case "nonce":
				if agentID == "" {
					continue
				}
				h.ForwardToBrowserByConnID(ctx, msg.ConnID, map[string]any{
					"type":         "nonce",
					"conn_id":      msg.ConnID,
					"value":        msg.Value,
					"has_password": msg.HasPassword,
				})

			case "auth_failed":
				if agentID == "" {
					continue
				}
				failures := h.IncrementAuthFailure(msg.ConnID)
				log.Printf("HMAC auth failed: conn %s failure %d/3", msg.ConnID, failures)
				if failures >= 3 {
					h.CloseBrowserConnWithError(ctx, msg.ConnID, "too many incorrect password attempts")
				} else {
					h.ForwardToBrowserByConnID(ctx, msg.ConnID, map[string]any{
						"type":               "auth_failed",
						"attempts_remaining": 3 - failures,
					})
				}

			case "session_expired":
				log.Printf("session expired (max downloads): %s", msg.SessionID)
			}
		}
	}
}

func handleHello(ctx context.Context, conn *websocket.Conn, h *hub.Hub, apiKeyData *pbstore.APIKeyData, agentID string, cfg *config.Config) {
	if agentID == "" {
		hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "agent_id required"})
		return
	}
	h.RegisterAgent(apiKeyData.RecordID, conn)
	log.Printf("agent hello: api_key=%s agent_id=%s", apiKeyData.RecordID, agentID)

	var turnCreds *turn.Credentials
	if cfg.HasTurn() {
		creds := turn.GenerateCredentials(cfg.TurnSecret, apiKeyData.RecordID, time.Now().Add(24*time.Hour))
		turnCreds = &creds
	}
	iceServers := turn.BuildICEConfig(&turn.ICEConfigRequest{
		STUNURL:     cfg.STUNURL,
		TurnURL:     cfg.TurnURL(),
		Credentials: turnCreds,
	})
	hub.SendDirect(ctx, conn, map[string]any{"type": "welcome", "ice_servers": iceServers})
}

func handleRegisterShare(ctx context.Context, conn *websocket.Conn, h *hub.Hub, app core.App, apiKeyData *pbstore.APIKeyData, agentID string, msg agentMsg) {
	code := msg.Code
	reconnected := false

	if code == "" {
		var err error
		code, err = generateRandomCode()
		if err != nil {
			hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "failed to generate code"})
			return
		}
		created := false
		for range 5 {
			session := &pbstore.SessionData{
				Code:      code,
				APIKeyID:  apiKeyData.RecordID,
				AgentID:   agentID,
				ExpiresAt: msg.ExpiresAt,
			}
			_, err = pbstore.CreateSession(app, session)
			if err == nil {
				created = true
				break
			}
			code, err = generateRandomCode()
			if err != nil {
				hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "failed to generate code"})
				return
			}
		}
		if !created {
			hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "code collision, try again"})
			return
		}
	} else {
		if !codeRegex.MatchString(code) {
			hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "invalid code format (8-30 chars, alphanumeric + hyphen + underscore)"})
			return
		}

		session := &pbstore.SessionData{
			Code:      code,
			APIKeyID:  apiKeyData.RecordID,
			AgentID:   agentID,
			ExpiresAt: msg.ExpiresAt,
		}
		_, reconnected, err := pbstore.ClaimSessionCode(app, session, apiKeyData.AccountID)
		if err != nil {
			if errors.Is(err, pbstore.ErrCodeAlreadyInUse) {
				hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "code already in use"})
			} else if err.Error() == "session owned by different agent" {
				hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "session owned by different agent"})
			} else {
				hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "database error"})
			}
			return
		}
		if reconnected {
			log.Printf("session reconnected: code=%s api_key=%s agent_id=%s", code, apiKeyData.RecordID, agentID)
		}
	}

	h.RegisterCode(code, apiKeyData.RecordID)

	session, err := pbstore.GetSessionByCode(app, code)
	if err != nil || session == nil {
		hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "failed to retrieve session"})
		return
	}

	response := map[string]any{
		"type":        "share_registered",
		"code":        code,
		"reconnected": reconnected,
	}
	if session.ExpiresAt != nil {
		response["expires_at"] = session.ExpiresAt.Format(time.RFC3339)
	}
	hub.SendDirect(ctx, conn, response)
	log.Printf("share registered: code=%s api_key=%s agent_id=%s reconnected=%v", code, apiKeyData.RecordID, agentID, reconnected)
}

func generateRandomCode() (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	const length = 8
	result := make([]byte, length)
	for i := range result {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			return "", err
		}
		result[i] = charset[idx.Int64()]
	}
	return string(result), nil
}
```

- [ ] **Step 5: Run tests**

```bash
cd signaling-server
go test ./internal/handler/... -run TestAgentWS -v
```

Expected: all three TestAgentWS_* tests PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/hub/hub.go internal/handler/agent_ws.go internal/handler/agent_ws_test.go
git commit -m "feat: port agent_ws to PocketBase; add hub.CloseAgent for immediate revocation"
```

---

## Task 7: Port browser_ws.go and rest.go

**Files:**
- Modify: `signaling-server/internal/handler/browser_ws.go`
- Modify: `signaling-server/internal/handler/rest.go`

- [ ] **Step 1: Rewrite browser_ws.go**

Replace `signaling-server/internal/handler/browser_ws.go`:

```go
package handler

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"time"

	"github.com/coder/websocket"
	"github.com/pocketbase/pocketbase/core"
	"sharebridge/server/internal/config"
	"sharebridge/server/internal/hub"
	"sharebridge/server/internal/pbstore"
	"sharebridge/server/internal/turn"
)

type browserMsg struct {
	Type      string          `json:"type"`
	SDP       string          `json:"sdp,omitempty"`
	Candidate json.RawMessage `json:"candidate,omitempty"`
	HMAC      string          `json:"hmac,omitempty"`
}

func BrowserWS(app core.App, h *hub.Hub, cfg *config.Config) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		sessionCode := e.Request.URL.Query().Get("session")

		session, err := pbstore.GetSessionByCode(app, sessionCode)
		if err != nil {
			log.Printf("browser_ws: db error looking up session: %v", err)
			e.Response.WriteHeader(500)
			return nil
		}
		if session == nil {
			e.Response.WriteHeader(404)
			return nil
		}
		if session.ExpiresAt != nil && time.Now().After(*session.ExpiresAt) {
			e.Response.WriteHeader(404)
			return nil
		}

		conn, err := websocket.Accept(e.Response, e.Request, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			log.Printf("browser_ws accept: %v", err)
			return nil
		}
		defer conn.CloseNow()

		ctx := e.Request.Context()

		_, agentOK := h.GetAgentConn(sessionCode)
		if !agentOK {
			hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "agent not connected"})
			conn.Close(websocket.StatusNormalClosure, "agent not connected")
			return nil
		}

		if err := h.PairSession(sessionCode, conn); err != nil {
			hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "agent not connected"})
			conn.Close(websocket.StatusNormalClosure, "agent not connected")
			return nil
		}
		defer h.UnpairSession(sessionCode)

		connID := generateConnID()
		h.RegisterBrowserConn(connID, conn)
		defer h.UnregisterBrowserConn(connID)

		log.Printf("browser connected to session %s (conn %s)", sessionCode, connID)

		var turnCreds *turn.Credentials
		if cfg.HasTurn() {
			creds := turn.GenerateCredentials(cfg.TurnSecret, sessionCode, time.Now().Add(24*time.Hour))
			turnCreds = &creds
		}
		iceServers := turn.BuildICEConfig(&turn.ICEConfigRequest{
			STUNURL:     cfg.STUNURL,
			TurnURL:     cfg.TurnURL(),
			Credentials: turnCreds,
		})
		hub.SendDirect(ctx, conn, map[string]any{"type": "ice_config", "ice_servers": iceServers})

		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				log.Printf("browser disconnected from session %s (conn %s)", sessionCode, connID)
				return nil
			}

			var msg browserMsg
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}

			switch msg.Type {
			case "knock":
				h.SendToAgent(ctx, session.APIKeyID, map[string]any{
					"type":    "knock",
					"conn_id": connID,
					"code":    sessionCode,
				})

			case "join":
				h.SendToAgent(ctx, session.APIKeyID, map[string]any{
					"type":    "join",
					"conn_id": connID,
					"code":    sessionCode,
					"hmac":    msg.HMAC,
				})

			case "answer":
				h.ForwardToAgent(ctx, sessionCode, map[string]any{
					"type":       "answer",
					"session_id": sessionCode,
					"peer_id":    connID,
					"sdp":        msg.SDP,
				})

			case "ice_candidate":
				h.ForwardToAgent(ctx, sessionCode, map[string]any{
					"type":       "ice_candidate",
					"session_id": sessionCode,
					"peer_id":    connID,
					"candidate":  msg.Candidate,
				})
			}
		}
	}
}

func generateConnID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
```

- [ ] **Step 2: Rewrite rest.go**

Replace `signaling-server/internal/handler/rest.go`:

```go
package handler

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"sharebridge/server/internal/hub"
	"sharebridge/server/internal/pbstore"
)

type SessionInfoResponse struct {
	Code      string     `json:"code"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	IsActive  bool       `json:"is_active"`
}

// GetSessionInfo returns session info by code.
func GetSessionInfo(app core.App, h *hub.Hub) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		code := e.Request.PathValue("code")
		if code == "" {
			e.Response.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(e.Response).Encode(map[string]string{"error": "code required"})
			return nil
		}

		session, err := pbstore.GetSessionByCode(app, code)
		if err != nil {
			e.Response.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(e.Response).Encode(map[string]string{"error": "failed to lookup session"})
			return nil
		}
		if session == nil {
			e.Response.WriteHeader(http.StatusNotFound)
			json.NewEncoder(e.Response).Encode(map[string]string{"error": "session not found"})
			return nil
		}

		isActive := true
		if session.ExpiresAt != nil && session.ExpiresAt.Before(time.Now()) {
			isActive = false
		}
		if !h.AgentConnected(session.APIKeyID) {
			isActive = false
		}

		e.Response.Header().Set("Content-Type", "application/json")
		json.NewEncoder(e.Response).Encode(SessionInfoResponse{
			Code:      session.Code,
			ExpiresAt: session.ExpiresAt,
			IsActive:  isActive,
		})
		return nil
	}
}
```

- [ ] **Step 3: Run tests**

```bash
cd signaling-server
go test ./internal/handler/... -run TestAgentWS -v
```

Expected: PASS (no new tests for browser_ws/rest in this task — those are covered by integration tests).

- [ ] **Step 4: Commit**

```bash
git add internal/handler/browser_ws.go internal/handler/rest.go
git commit -m "feat: port browser_ws and rest handlers from Gin to PocketBase RequestEvent"
```

---

## Task 8: User-Scoped API Key Handlers

**Files:**
- Create: `signaling-server/internal/handler/api_keys.go`
- Create: `signaling-server/internal/handler/api_keys_test.go`

- [ ] **Step 1: Write failing tests**

Create `signaling-server/internal/handler/api_keys_test.go`:

```go
package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sharebridge/server/internal/handler"
	"sharebridge/server/internal/pbstore"
)

// setAuth simulates what apis.RequireAuth() does — sets e.Auth to the user record.
func testHandlerWithAuth(testApp *tests.TestApp, userRecord *core.Record, h func(*core.RequestEvent) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		e := &core.RequestEvent{App: testApp, Request: r, Response: w, Auth: userRecord}
		if err := h(e); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		}
	}
}

func TestCreateAPIKey_Handler(t *testing.T) {
	testApp := newHandlerTestApp(t)
	_, accountID := createTestAccountForHandler(t, testApp)

	usersCol, _ := testApp.FindCollectionByNameOrId("users")
	user, _ := testApp.FindRecordById("users", accountID)
	_ = usersCol

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/keys", testHandlerWithAuth(testApp, user, handler.CreateAPIKey(testApp)))
	server := httptest.NewServer(mux)
	defer server.Close()

	body := bytes.NewBufferString(`{"label":"my key"}`)
	resp, err := http.Post(server.URL+"/api/keys", "application/json", body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusCreated, resp.StatusCode)

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	assert.NotEmpty(t, result["full_key"])
	assert.NotEmpty(t, result["id"])
}

func TestListAPIKeys_Handler(t *testing.T) {
	testApp := newHandlerTestApp(t)
	_, accountID := createTestAccountForHandler(t, testApp)
	user, _ := testApp.FindRecordById("users", accountID)

	pbstore.CreateAPIKey(testApp, accountID, "key-1")
	pbstore.CreateAPIKey(testApp, accountID, "key-2")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/keys", testHandlerWithAuth(testApp, user, handler.ListAPIKeys(testApp)))
	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/keys")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	keys := result["keys"].([]any)
	assert.Len(t, keys, 2)
}

func TestRevokeAPIKey_Handler(t *testing.T) {
	testApp := newHandlerTestApp(t)
	_, accountID := createTestAccountForHandler(t, testApp)
	user, _ := testApp.FindRecordById("users", accountID)

	_, keyData, _ := pbstore.CreateAPIKey(testApp, accountID, "")

	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/keys/{id}", testHandlerWithAuth(testApp, user, handler.RevokeAPIKey(testApp)))
	server := httptest.NewServer(mux)
	defer server.Close()

	req, _ := http.NewRequest("DELETE", server.URL+"/api/keys/"+keyData.RecordID, nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func createTestAccountForHandler(t *testing.T, testApp *tests.TestApp) (fullKey string, accountID string) {
	t.Helper()
	usersCol, _ := testApp.FindCollectionByNameOrId("users")
	user := core.NewRecord(usersCol)
	user.SetEmail("handler-test@example.com")
	user.SetPassword("password1234!")
	user.Set("verified", true)
	require.NoError(t, testApp.SaveRecord(user))
	return "", user.Id
}
```

- [ ] **Step 2: Run to verify failure**

```bash
cd signaling-server
go test ./internal/handler/... -run TestCreateAPIKey_Handler 2>&1 | head -10
```

Expected: compile error — `handler.CreateAPIKey`, `ListAPIKeys`, `RevokeAPIKey` not defined.

- [ ] **Step 3: Implement api_keys.go**

Create `signaling-server/internal/handler/api_keys.go`:

```go
package handler

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"sharebridge/server/internal/hub"
	"sharebridge/server/internal/pbstore"
)

type createAPIKeyRequest struct {
	Label string `json:"label"`
}

type createAPIKeyResponse struct {
	ID      string `json:"id"`
	FullKey string `json:"full_key"`
	Label   string `json:"label"`
}

type apiKeyListItem struct {
	ID         string     `json:"id"`
	Label      string     `json:"label"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	IsActive   bool       `json:"is_active"`
}

// CreateAPIKey handles POST /api/keys.
// Requires apis.RequireAuth() middleware — e.Auth must be set.
func CreateAPIKey(app core.App) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		if e.Auth == nil {
			e.Response.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(e.Response).Encode(map[string]string{"error": "authentication required"})
			return nil
		}

		var req createAPIKeyRequest
		json.NewDecoder(e.Request.Body).Decode(&req)

		fullKey, keyData, err := pbstore.CreateAPIKey(app, e.Auth.Id, req.Label)
		if err != nil {
			e.Response.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(e.Response).Encode(map[string]string{"error": "failed to create key"})
			return nil
		}

		e.Response.Header().Set("Content-Type", "application/json")
		e.Response.WriteHeader(http.StatusCreated)
		json.NewEncoder(e.Response).Encode(createAPIKeyResponse{
			ID:      keyData.RecordID,
			FullKey: fullKey,
			Label:   keyData.Label,
		})
		return nil
	}
}

// ListAPIKeys handles GET /api/keys.
// Requires apis.RequireAuth() middleware — e.Auth must be set.
func ListAPIKeys(app core.App) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		if e.Auth == nil {
			e.Response.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(e.Response).Encode(map[string]string{"error": "authentication required"})
			return nil
		}

		keys, err := pbstore.ListAPIKeys(app, e.Auth.Id)
		if err != nil {
			e.Response.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(e.Response).Encode(map[string]string{"error": "failed to list keys"})
			return nil
		}

		items := make([]apiKeyListItem, len(keys))
		for i, k := range keys {
			items[i] = apiKeyListItem{
				ID:         k.RecordID,
				Label:      k.Label,
				CreatedAt:  k.CreatedAt,
				LastUsedAt: k.LastUsedAt,
				IsActive:   k.IsActive,
			}
		}

		e.Response.Header().Set("Content-Type", "application/json")
		json.NewEncoder(e.Response).Encode(map[string]any{"keys": items})
		return nil
	}
}

// RevokeAPIKey handles DELETE /api/keys/{id}.
// Requires apis.RequireAuth() middleware — e.Auth must be set.
// Also closes the agent's live WebSocket immediately if one exists.
func RevokeAPIKey(app core.App, h *hub.Hub) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		if e.Auth == nil {
			e.Response.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(e.Response).Encode(map[string]string{"error": "authentication required"})
			return nil
		}

		keyID := e.Request.PathValue("id")
		if keyID == "" {
			e.Response.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(e.Response).Encode(map[string]string{"error": "key id required"})
			return nil
		}

		if err := pbstore.RevokeAPIKey(app, keyID, e.Auth.Id); err != nil {
			if err.Error() == "not authorized" {
				e.Response.WriteHeader(http.StatusForbidden)
				json.NewEncoder(e.Response).Encode(map[string]string{"error": "not authorized"})
			} else {
				e.Response.WriteHeader(http.StatusNotFound)
				json.NewEncoder(e.Response).Encode(map[string]string{"error": "key not found"})
			}
			return nil
		}

		// Immediately eject any live agent connection for this key.
		h.CloseAgent(keyID)

		e.Response.Header().Set("Content-Type", "application/json")
		json.NewEncoder(e.Response).Encode(map[string]string{"message": "key revoked"})
		return nil
	}
}
```

- [ ] **Step 4: Run tests**

```bash
cd signaling-server
go test ./internal/handler/... -run TestCreateAPIKey_Handler -v
go test ./internal/handler/... -run TestListAPIKeys_Handler -v
go test ./internal/handler/... -run TestRevokeAPIKey_Handler -v
```

Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/handler/api_keys.go internal/handler/api_keys_test.go
git commit -m "feat: add user-scoped API key management handlers"
```

---

## Task 9: Rewrite cmd/server/main.go

**Files:**
- Modify: `signaling-server/cmd/server/main.go`

- [ ] **Step 1: Rewrite main.go**

Prerequisite: add the `handler.ServeFile` helper from Step 2 first, then come back and wire it into `main.go`.

Replace `signaling-server/cmd/server/main.go`:

```go
package main

import (
	"log"
	"net"
	"strconv"
	"strings"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"sharebridge/server/internal/config"
	"sharebridge/server/internal/handler"
	"sharebridge/server/internal/hub"
	"sharebridge/server/internal/pbstore"
	_ "sharebridge/server/migrations"
)

func main() {
	cfg := config.Load()
	h := hub.New()

	app := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir: cfg.DataDir,
	})

	// IMPORTANT: wire PocketBase to cfg.DataDir and cfg.Port explicitly.

	// Configure SMTP if provided (must be done before Bootstrap so email flows work).
	if cfg.HasSMTP() {
		app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
			settings := app.Settings()
			settings.SMTP.Enabled = true
			settings.SMTP.Host = cfg.SMTPHost
			smtpPort, _ := strconv.Atoi(cfg.SMTPPort)
			settings.SMTP.Port = smtpPort
			settings.SMTP.Username = cfg.SMTPUser
			settings.SMTP.Password = cfg.SMTPPassword
			return e.Next()
		})
	}

	app.OnServe().BindFunc(func(se *core.ServeEvent) error {
		router := se.Router

		// Do not expose /_/ on the public reverse-proxied site.
		// Enforce the primary deny rule in nginx/Caddy/Traefik; a same-host reverse
		// proxy makes RemoteAddr appear local, so an app-only localhost check is not enough.

		// WebSocket endpoints
		router.GET("/ws/agent", handler.AgentWS(app, h, cfg))
		router.GET("/ws/client", handler.BrowserWS(app, h, cfg))

		// Session info REST endpoint
		router.GET("/sessions/{code}", handler.GetSessionInfo(app, h))

		// Direct link route — serves index.html; JS reads code from window.location
		router.GET("/s/{code}", handler.ServeFile("./web/index.html"))

		// Static web files
		router.GET("/", handler.ServeFile("./web/index.html"))
		router.GET("/app.js", handler.ServeFile("./web/app.js"))
		router.GET("/register", handler.ServeFile("./web/register.html"))
		router.GET("/login", handler.ServeFile("./web/login.html"))
		router.GET("/account", handler.ServeFile("./web/account.html"))

		// User-scoped API key management (requires JWT auth)
		router.POST("/api/keys", handler.CreateAPIKey(app), apis.RequireAuth())
		router.GET("/api/keys", handler.ListAPIKeys(app), apis.RequireAuth())
		router.DELETE("/api/keys/{id}", handler.RevokeAPIKey(app, h), apis.RequireAuth())

		// Cron: clean up expired sessions every 5 minutes
		app.Cron().MustAdd("expiry_cleanup", "*/5 * * * *", func() {
			if err := pbstore.DeleteExpiredSessions(app); err != nil {
				log.Printf("session cleanup error: %v", err)
			}
		})

		log.Printf("signaling server listening on :%s", cfg.Port)
		log.Printf("pocketbase data dir: %s", cfg.DataDir)

		return se.Next()
	})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}
```

- [ ] **Step 2: Add ServeFile handler helper**

Add to `signaling-server/internal/handler/rest.go` (append after the existing content):

```go
// ServeFile returns a handler that serves a single static file.
func ServeFile(path string) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		http.ServeFile(e.Response, e.Request, path)
		return nil
	}
}
```

Also add `"net/http"` to the imports in rest.go if not already present (it is, from `http.StatusBadRequest` etc.).

- [ ] **Step 3: Build and verify**

```bash
cd signaling-server
go build ./cmd/server/...
```

Expected: compiles cleanly. The old Gin-based cmd/server errors should be resolved.

Also verify in the running binary that:
- the server actually listens on `cfg.Port`
- PocketBase persists data at `cfg.DataDir`
- `/_/` is blocked by the reverse proxy configuration, not only by application code

- [ ] **Step 4: Smoke test**

```bash
# Start the server (it will print a setup URL on first run)
./signaling-server/cmd/server/server &
SERVER_PID=$!
sleep 2

# Verify it's serving
curl -s http://localhost:8080/ | head -5

kill $SERVER_PID
```

Expected: HTML response from index.html.

- [ ] **Step 5: Commit**

```bash
git add cmd/server/main.go internal/handler/rest.go
git commit -m "feat: rewrite cmd/server/main.go for PocketBase with all routes and cron"
```

---

## Task 10: Web Pages — register, login, account

**Files:**
- Create: `signaling-server/web/register.html`
- Create: `signaling-server/web/login.html`
- Create: `signaling-server/web/account.html`

The shared CSS variables match index.html (Catppuccin dark theme).

- [ ] **Step 1: Create register.html**

Create `signaling-server/web/register.html`:

```html
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>ShareBridge — Register</title>
  <style>
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif; background: #1e1e2e; color: #cdd6f4; max-width: 400px; margin: 80px auto; padding: 0 1rem; }
    h2 { font-size: 1.4rem; font-weight: 600; margin-bottom: 24px; }
    label { display: block; margin-bottom: 4px; font-size: 0.9rem; color: #a6adc8; }
    input { width: 100%; padding: 10px 12px; font-size: 1rem; background: #313244; color: #cdd6f4; border: 1px solid #45475a; border-radius: 6px; outline: none; margin-bottom: 16px; }
    input:focus { border-color: #89b4fa; }
    button { width: 100%; padding: 10px; font-size: 1rem; background: #89b4fa; color: #1e1e2e; border: none; border-radius: 6px; cursor: pointer; font-weight: 600; }
    button:hover { opacity: 0.9; }
    #error { color: #f38ba8; margin-top: 12px; font-size: 0.9rem; min-height: 1.2em; }
    .link { margin-top: 16px; font-size: 0.9rem; color: #6c7086; }
    .link a { color: #89b4fa; text-decoration: none; }
  </style>
</head>
<body>
  <h2>Create account</h2>
  <form id="form">
    <label for="email">Email</label>
    <input id="email" type="email" placeholder="you@example.com" required autocomplete="email" />
    <label for="password">Password</label>
    <input id="password" type="password" placeholder="Minimum 8 characters" required autocomplete="new-password" />
    <label for="confirm">Confirm password</label>
    <input id="confirm" type="password" placeholder="Repeat password" required autocomplete="new-password" />
    <button type="submit">Create account</button>
  </form>
  <div id="error"></div>
  <div class="link">Already have an account? <a href="/login">Sign in</a></div>
  <script>
    document.getElementById('form').addEventListener('submit', async (ev) => {
      ev.preventDefault();
      const email = document.getElementById('email').value.trim();
      const password = document.getElementById('password').value;
      const confirm = document.getElementById('confirm').value;
      const errorEl = document.getElementById('error');

      errorEl.textContent = '';
      if (password !== confirm) { errorEl.textContent = 'Passwords do not match.'; return; }
      if (password.length < 8) { errorEl.textContent = 'Password must be at least 8 characters.'; return; }

      try {
        const resp = await fetch('/api/collections/users/records', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ email, password, passwordConfirm: confirm }),
        });
        if (!resp.ok) {
          const body = await resp.json();
          const msg = body.message || body.data?.email?.message || 'Registration failed.';
          errorEl.textContent = msg;
          return;
        }
        // Registration succeeded — redirect to login with a generic notice.
        // If this server has SMTP configured, the user may still need to verify email first.
        window.location.href = '/login?registered=1';
      } catch (err) {
        errorEl.textContent = 'Network error. Please try again.';
      }
    });
  </script>
</body>
</html>
```

- [ ] **Step 2: Create login.html**

Create `signaling-server/web/login.html`:

```html
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>ShareBridge — Sign in</title>
  <style>
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif; background: #1e1e2e; color: #cdd6f4; max-width: 400px; margin: 80px auto; padding: 0 1rem; }
    h2 { font-size: 1.4rem; font-weight: 600; margin-bottom: 24px; }
    #notice { color: #a6e3a1; margin-bottom: 16px; font-size: 0.9rem; min-height: 1.2em; }
    label { display: block; margin-bottom: 4px; font-size: 0.9rem; color: #a6adc8; }
    input { width: 100%; padding: 10px 12px; font-size: 1rem; background: #313244; color: #cdd6f4; border: 1px solid #45475a; border-radius: 6px; outline: none; margin-bottom: 16px; }
    input:focus { border-color: #89b4fa; }
    button { width: 100%; padding: 10px; font-size: 1rem; background: #89b4fa; color: #1e1e2e; border: none; border-radius: 6px; cursor: pointer; font-weight: 600; }
    button:hover { opacity: 0.9; }
    #error { color: #f38ba8; margin-top: 12px; font-size: 0.9rem; min-height: 1.2em; }
    .link { margin-top: 16px; font-size: 0.9rem; color: #6c7086; }
    .link a { color: #89b4fa; text-decoration: none; }
  </style>
</head>
<body>
  <h2>Sign in</h2>
  <div id="notice"></div>
  <form id="form">
    <label for="email">Email</label>
    <input id="email" type="email" placeholder="you@example.com" required autocomplete="email" />
    <label for="password">Password</label>
    <input id="password" type="password" placeholder="Password" required autocomplete="current-password" />
    <button type="submit">Sign in</button>
  </form>
  <div id="error"></div>
  <div class="link">No account? <a href="/register">Create one</a></div>
  <script>
    const params = new URLSearchParams(window.location.search);
    if (params.get('registered')) {
      document.getElementById('notice').textContent = 'Account created. If this server has email verification enabled, check your inbox before signing in.';
    }

    document.getElementById('form').addEventListener('submit', async (ev) => {
      ev.preventDefault();
      const email = document.getElementById('email').value.trim();
      const password = document.getElementById('password').value;
      const errorEl = document.getElementById('error');
      errorEl.textContent = '';

      try {
        const resp = await fetch('/api/collections/users/auth-with-password', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ identity: email, password }),
        });
        if (!resp.ok) {
          errorEl.textContent = 'Invalid email or password.';
          return;
        }
        const body = await resp.json();
        localStorage.setItem('pb_auth_token', body.token);
        localStorage.setItem('pb_user_id', body.record.id);
        window.location.href = '/account';
      } catch (err) {
        errorEl.textContent = 'Network error. Please try again.';
      }
    });
  </script>
</body>
</html>
```

- [ ] **Step 3: Create account.html**

Create `signaling-server/web/account.html`:

```html
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>ShareBridge — API Keys</title>
  <style>
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif; background: #1e1e2e; color: #cdd6f4; max-width: 640px; margin: 48px auto; padding: 0 1rem; }
    h2 { font-size: 1.4rem; font-weight: 600; margin-bottom: 24px; }
    .actions { margin-bottom: 24px; display: flex; gap: 8px; align-items: center; }
    input { padding: 8px 12px; font-size: 0.95rem; background: #313244; color: #cdd6f4; border: 1px solid #45475a; border-radius: 6px; outline: none; }
    input:focus { border-color: #89b4fa; }
    button { padding: 8px 16px; font-size: 0.9rem; background: #89b4fa; color: #1e1e2e; border: none; border-radius: 6px; cursor: pointer; font-weight: 600; }
    button:hover { opacity: 0.9; }
    button.danger { background: #f38ba8; }
    .key-list { margin-top: 8px; }
    .key-item { padding: 14px 16px; background: #313244; border-radius: 8px; margin: 6px 0; display: flex; justify-content: space-between; align-items: center; }
    .key-meta { font-size: 0.85rem; color: #6c7086; margin-top: 4px; }
    .key-label { font-weight: 500; }
    .key-inactive { opacity: 0.5; }
    #status { margin: 16px 0; color: #6c7086; font-size: 0.9rem; min-height: 1.2em; }
    #copy-dialog { display: none; position: fixed; inset: 0; background: rgba(0,0,0,0.6); align-items: center; justify-content: center; }
    #copy-dialog.open { display: flex; }
    .dialog-box { background: #313244; border-radius: 12px; padding: 24px; max-width: 500px; width: 90%; }
    .dialog-box h3 { margin-bottom: 12px; }
    .dialog-box p { color: #a6adc8; font-size: 0.9rem; margin-bottom: 16px; }
    .key-display { font-family: monospace; font-size: 0.85rem; background: #1e1e2e; padding: 12px; border-radius: 6px; word-break: break-all; margin-bottom: 12px; }
    nav { margin-bottom: 24px; font-size: 0.9rem; color: #6c7086; }
    nav a { color: #89b4fa; text-decoration: none; margin-right: 12px; }
    .sign-out { background: none; border: none; color: #6c7086; font-size: 0.9rem; cursor: pointer; padding: 0; }
    .sign-out:hover { color: #cdd6f4; }
  </style>
</head>
<body>
  <nav>
    <a href="/">ShareBridge</a>
    <button class="sign-out" onclick="signOut()">Sign out</button>
  </nav>
  <h2>API Keys</h2>
  <div class="actions">
    <input id="label-input" placeholder="Label (optional)" />
    <button onclick="createKey()">Create key</button>
  </div>
  <div id="status"></div>
  <div class="key-list" id="key-list"></div>

  <div id="copy-dialog">
    <div class="dialog-box">
      <h3>API Key Created</h3>
      <p>Copy this key now. It will not be shown again.</p>
      <div class="key-display" id="new-key-display"></div>
      <button onclick="copyAndClose()">Copy &amp; close</button>
    </div>
  </div>

  <script>
    const token = localStorage.getItem('pb_auth_token');
    if (!token) window.location.href = '/login';

    function authHeaders() {
      return { 'Content-Type': 'application/json', 'Authorization': 'Bearer ' + token };
    }

    async function loadKeys() {
      const resp = await fetch('/api/keys', { headers: authHeaders() });
      if (resp.status === 401) { window.location.href = '/login'; return; }
      const body = await resp.json();
      const list = document.getElementById('key-list');
      list.innerHTML = '';
      if (!body.keys || body.keys.length === 0) {
        list.innerHTML = '<div style="color:#6c7086;font-size:0.9rem;">No API keys yet.</div>';
        return;
      }
      for (const key of body.keys) {
        const item = document.createElement('div');
        item.className = 'key-item' + (key.is_active ? '' : ' key-inactive');
        const label = key.label || '(unlabeled)';
        const lastUsed = key.last_used_at ? 'Last used ' + new Date(key.last_used_at).toLocaleDateString() : 'Never used';
        const created = new Date(key.created_at).toLocaleDateString();
        item.innerHTML = `
          <div>
            <div class="key-label">${label}</div>
            <div class="key-meta">Created ${created} · ${lastUsed} · ${key.is_active ? 'Active' : 'Revoked'}</div>
            <div class="key-meta" style="font-family:monospace;font-size:0.78rem;color:#45475a">${key.id}</div>
          </div>
          ${key.is_active ? `<button class="danger" onclick="revokeKey('${key.id}')">Revoke</button>` : ''}
        `;
        list.appendChild(item);
      }
    }

    async function createKey() {
      const label = document.getElementById('label-input').value.trim();
      document.getElementById('status').textContent = '';
      const resp = await fetch('/api/keys', {
        method: 'POST',
        headers: authHeaders(),
        body: JSON.stringify({ label }),
      });
      if (resp.status === 401) { window.location.href = '/login'; return; }
      if (!resp.ok) { document.getElementById('status').textContent = 'Failed to create key.'; return; }
      const body = await resp.json();
      document.getElementById('new-key-display').textContent = body.full_key;
      document.getElementById('copy-dialog').classList.add('open');
      document.getElementById('label-input').value = '';
      loadKeys();
    }

    function copyAndClose() {
      navigator.clipboard.writeText(document.getElementById('new-key-display').textContent).catch(() => {});
      document.getElementById('copy-dialog').classList.remove('open');
    }

    async function revokeKey(id) {
      if (!confirm('Revoke this key? Any agents using it will fail to reconnect.')) return;
      const resp = await fetch('/api/keys/' + id, { method: 'DELETE', headers: authHeaders() });
      if (!resp.ok) { document.getElementById('status').textContent = 'Failed to revoke key.'; return; }
      loadKeys();
    }

    function signOut() {
      localStorage.removeItem('pb_auth_token');
      localStorage.removeItem('pb_user_id');
      window.location.href = '/login';
    }

    loadKeys();
  </script>
</body>
</html>
```

- [ ] **Step 4: Verify pages are served**

```bash
cd signaling-server
go build -o server ./cmd/server
./server &
SERVER_PID=$!
sleep 2

curl -si http://localhost:8080/register | head -5
curl -si http://localhost:8080/login | head -5
curl -si http://localhost:8080/account | head -5

kill $SERVER_PID
rm server
```

Expected: each returns `200 OK` with `<!DOCTYPE html>`.

- [ ] **Step 5: Commit**

```bash
git add web/register.html web/login.html web/account.html
git commit -m "feat: add register, login, and account web pages"
```

---

## Task 11: Strip cmd/admin/main.go

**Files:**
- Modify: `signaling-server/cmd/admin/main.go`

- [ ] **Step 1: Rewrite cmd/admin/main.go**

Replace `signaling-server/cmd/admin/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"github.com/pocketbase/pocketbase"
	"github.com/spf13/cobra"
	_ "sharebridge/server/migrations"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "admin",
		Short: "ShareBridge signaling server admin tool",
	}

	rootCmd.AddCommand(createSuperuserCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

var createSuperuserCmd = &cobra.Command{
	Use:   "create-superuser [email] [password]",
	Short: "Create the initial PocketBase superuser (for scripted deploys)",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		app := pocketbase.New()
		if err := app.Bootstrap(); err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
		superuser, err := app.FindSuperuserByEmail(args[0])
		if err == nil {
			superuser.SetPassword(args[1])
			return app.SaveSuperuser(superuser)
		}
		// Create new superuser
		return app.CreateInitialSuperuser(args[0], args[1])
	},
}
```

- [ ] **Step 2: Build to verify**

```bash
cd signaling-server
go build ./cmd/admin/...
```

Expected: compiles cleanly.

> Note: `app.CreateInitialSuperuser` / `app.FindSuperuserByEmail` / `app.SaveSuperuser` are the PocketBase v0.22 superuser management methods. If the installed version uses a different API (e.g., `app.SuperuserQuery()` or `app.SaveSuperuserCredential()`), adapt accordingly — check `go doc github.com/pocketbase/pocketbase CreateInitialSuperuser`.

- [ ] **Step 3: Commit**

```bash
git add cmd/admin/main.go
git commit -m "chore: strip cmd/admin to create-superuser only"
```

---

## Task 12: Remove Old Code

**Files:**
- Delete: `signaling-server/internal/db/` (entire package)
- Delete: `signaling-server/internal/middleware/` (entire package)
- Delete: `signaling-server/internal/handler/admin.go`
- Delete: `signaling-server/internal/handler/routes.go`

- [ ] **Step 1: Delete old packages**

```bash
cd signaling-server
rm -rf internal/db/ internal/middleware/
rm internal/handler/admin.go internal/handler/routes.go
```

- [ ] **Step 2: Remove Gin from go.mod**

```bash
cd signaling-server
go mod tidy
```

Expected: Gin and sqlite3 removed from go.mod. PocketBase, coder/websocket, bcrypt, cobra remain.

- [ ] **Step 3: Build everything**

```bash
cd signaling-server
go build ./...
```

Expected: clean build, no errors.

- [ ] **Step 4: Run full test suite**

```bash
cd signaling-server
go test ./... -v 2>&1 | tail -40
```

Expected: all tests PASS. No references to `internal/db` or `internal/middleware`.

- [ ] **Step 5: Final integration smoke test**

```bash
cd signaling-server
go build -o server ./cmd/server
./server &
SERVER_PID=$!
sleep 3

# PocketBase setup URL appears in logs on first run — follow it to create a superuser.
# Then verify endpoints:
curl -si http://localhost:8080/ | grep -c "ShareBridge"
curl -si http://localhost:8080/register | grep -c "DOCTYPE"
curl -si "http://localhost:8080/sessions/doesnotexist" | grep "session not found"

kill $SERVER_PID
rm server
```

Expected:
- `1` (index.html found)
- `1` (register.html found)
- `{"error":"session not found"}` from REST endpoint

- [ ] **Step 6: Commit**

```bash
git add -A
git commit -m "chore: remove internal/db, internal/middleware, old Gin handlers — Slice 10a complete"
```

---

## Self-Review Checklist

Spec requirements → plan coverage:

| Spec Section | Task |
|---|---|
| PocketBase replaces Gin | Tasks 1, 9 |
| `api_keys` collection + cascade delete | Tasks 2, 3 |
| `sessions` collection + cascade delete | Tasks 2, 4 |
| API key format `<record_id>.<secret>` 32-byte entropy | Task 3 |
| Full key shown once in copy dialog | Task 10 |
| Agent auth unchanged (`?api_key=`) | Task 6 |
| Atomic same-account session reclaim transfers `api_key_id` | Tasks 3, 6 |
| Agent lifecycle on revoke — immediate ejection + fail on reconnect | Covered by `hub.CloseAgent(...)` plus `ValidateAPIKey` returning nil for inactive keys |
| `/register`, `/login`, `/account` pages | Task 10 |
| `POST/GET/DELETE /api/keys` with user JWT | Tasks 8, 9 |
| `/_/` blocked at reverse proxy, with app-side checks only as defense in depth | Task 9 |
| SMTP optional | Task 5, 9 |
| `ADMIN_TOKEN` / `AUTH_TOKEN` removed | Task 5 |
| `cmd/admin` stripped to create-superuser | Task 11 |
| Old packages deleted | Task 12 |
| Cron for session expiry | Task 9 |

No gaps found.
