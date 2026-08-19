package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/coder/websocket"
	"github.com/pocketbase/pocketbase/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sharebridge/control/internal/hub"
)

func TestListAPIKeys_ReturnsOnlyAuthenticatedUsersKeys(t *testing.T) {
	app, cleanup := setupAgentTestApp(t)
	defer cleanup()

	alice, err := createTestUser(app, "alice-list@example.com")
	require.NoError(t, err)
	bob, err := createTestUser(app, "bob-list@example.com")
	require.NoError(t, err)

	aliceKey, err := createTestAPIKey(app, alice.Id, "alice-secret")
	require.NoError(t, err)
	_, err = createTestAPIKey(app, bob.Id, "bob-secret")
	require.NoError(t, err)

	request := httptest.NewRequest(http.MethodGet, "/api/keys", nil)
	recorder := httptest.NewRecorder()

	requestEvent := new(core.RequestEvent)
	requestEvent.App = app
	requestEvent.Request = request
	requestEvent.Response = recorder
	requestEvent.Auth = alice

	err = ListAPIKeys(app)(requestEvent)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	var response []APIKeyListItem
	err = json.Unmarshal(recorder.Body.Bytes(), &response)
	require.NoError(t, err)
	require.Len(t, response, 1)
	require.Equal(t, aliceKey.Id, response[0].ID)
	require.Equal(t, "test key", response[0].Label)
	require.True(t, response[0].IsActive)
	require.False(t, response[0].CreatedAt.IsZero())
}

func TestRotateAPIKey_TransfersSessionsAndRevokesOldKey(t *testing.T) {
	app, cleanup := setupAgentTestApp(t)
	defer cleanup()

	alice, err := createTestUser(app, "alice-rotate@example.com")
	require.NoError(t, err)

	oldKey, err := createTestAPIKey(app, alice.Id, "old-secret")
	require.NoError(t, err)
	session, err := createTestSession(app, oldKey.Id, "agent-1", "ROTATE01")
	require.NoError(t, err)

	request := httptest.NewRequest(http.MethodPost, "/api/keys/"+oldKey.Id+"/rotate", bytes.NewBufferString(`{"label":"replacement key"}`))
	request.SetPathValue("id", oldKey.Id)
	recorder := httptest.NewRecorder()

	requestEvent := new(core.RequestEvent)
	requestEvent.App = app
	requestEvent.Request = request
	requestEvent.Response = recorder
	requestEvent.Auth = alice

	sessionHub := hub.New()
	err = RotateAPIKey(app, sessionHub)(requestEvent)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())

	var response CreateAPIKeyResponse
	err = json.Unmarshal(recorder.Body.Bytes(), &response)
	require.NoError(t, err)
	require.NotEmpty(t, response.ID)
	require.NotEmpty(t, response.Key)
	require.Equal(t, "test key", response.Label)

	oldKeyRecord, err := app.FindRecordById("api_keys", oldKey.Id)
	require.NoError(t, err)
	assert.False(t, oldKeyRecord.GetBool("is_active"))

	newKeyRecord, err := app.FindRecordById("api_keys", response.ID)
	require.NoError(t, err)
	assert.True(t, newKeyRecord.GetBool("is_active"))
	assert.Equal(t, "test key", newKeyRecord.GetString("label"))

	sessionRecord, err := app.FindRecordById("sessions", session.Id)
	require.NoError(t, err)
	assert.Equal(t, response.ID, sessionRecord.GetString("api_key_id"))
}

// createTestAgent inserts an agent record bound to apiKeyID with the given
// namespace and cert status. It exercises the real agents schema (required
// api_key_id relation + namespace) exactly as the directctl enrollment path does.
func createTestAgent(t *testing.T, app core.App, apiKeyID, namespace, certStatus string) *core.Record {
	t.Helper()
	agentsCol, err := app.FindCollectionByNameOrId("agents")
	require.NoError(t, err)
	rec := core.NewRecord(agentsCol)
	rec.Set("api_key_id", apiKeyID)
	rec.Set("namespace", namespace)
	rec.Set("cert_status", certStatus)
	require.NoError(t, app.Save(rec))
	return rec
}

func TestRotatePreservesAgent(t *testing.T) {
	app, cleanup := setupAgentTestApp(t)
	defer cleanup()

	alice, err := createTestUser(app, "alice-rotate-agent@example.com")
	require.NoError(t, err)

	oldKey, err := createTestAPIKey(app, alice.Id, "old-secret")
	require.NoError(t, err)

	agent := createTestAgent(t, app, oldKey.Id, "sbdeadbeef", "ready")
	agent.Set("cert_fingerprint", "abc123")
	require.NoError(t, app.Save(agent))

	request := httptest.NewRequest(http.MethodPost, "/api/keys/"+oldKey.Id+"/rotate", bytes.NewBufferString(`{}`))
	request.SetPathValue("id", oldKey.Id)
	recorder := httptest.NewRecorder()

	requestEvent := new(core.RequestEvent)
	requestEvent.App = app
	requestEvent.Request = request
	requestEvent.Response = recorder
	requestEvent.Auth = alice

	err = RotateAPIKey(app, hub.New())(requestEvent)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())

	var response CreateAPIKeyResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))

	// Old key is revoked.
	oldKeyRecord, err := app.FindRecordById("api_keys", oldKey.Id)
	require.NoError(t, err)
	assert.False(t, oldKeyRecord.GetBool("is_active"))

	// No agent still points at the old key.
	oldAgents, err := app.FindRecordsByFilter("agents", "api_key_id = {:k}", "", 1, 0, map[string]any{"k": oldKey.Id})
	require.NoError(t, err)
	assert.Len(t, oldAgents, 0)

	// The same agent row now points at the new key, preserving namespace + cert.
	newAgents, err := app.FindRecordsByFilter("agents", "api_key_id = {:k}", "", 1, 0, map[string]any{"k": response.ID})
	require.NoError(t, err)
	require.Len(t, newAgents, 1)
	assert.Equal(t, "sbdeadbeef", newAgents[0].GetString("namespace"))
	assert.Equal(t, "ready", newAgents[0].GetString("cert_status"))
	assert.Equal(t, "abc123", newAgents[0].GetString("cert_fingerprint"))
}

func TestRevokeDeletesAgent(t *testing.T) {
	app, cleanup := setupAgentTestApp(t)
	defer cleanup()

	alice, err := createTestUser(app, "alice-revoke-agent@example.com")
	require.NoError(t, err)

	key, err := createTestAPIKey(app, alice.Id, "revoke-secret")
	require.NoError(t, err)

	createTestAgent(t, app, key.Id, "sbrevoke01", "pending")

	request := httptest.NewRequest(http.MethodDelete, "/api/keys/"+key.Id, nil)
	request.SetPathValue("id", key.Id)
	recorder := httptest.NewRecorder()

	requestEvent := new(core.RequestEvent)
	requestEvent.App = app
	requestEvent.Request = request
	requestEvent.Response = recorder
	requestEvent.Auth = alice

	err = RevokeAPIKey(app, hub.New())(requestEvent)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	// The agent row is deleted so a fresh namespace is issued on re-enroll.
	agents, err := app.FindRecordsByFilter("agents", "api_key_id = {:k}", "", 1, 0, map[string]any{"k": key.Id})
	require.NoError(t, err)
	assert.Len(t, agents, 0)
}

// TestRevokeAPIKeySoftDeletesSessionsAndReenrollGetsFreshOrigin covers the
// standalone-revocation recovery end-to-end: revoke soft-deletes the agent's
// active sessions (same transaction as the key+agent), so a re-enroll with a
// fresh key gets a fresh namespace AND a fresh origin — the stale origin bound
// under the deleted agent's namespace is never reclaimed.
func TestRevokeAPIKeySoftDeletesSessionsAndReenrollGetsFreshOrigin(t *testing.T) {
	app, serverURL, h, cleanup := setupAgentWSWithControllerAndHub(t)
	defer cleanup()

	user, err := createTestUser(app, "revoke-origin@example.com")
	require.NoError(t, err)

	// Old key + enroll → agent row with namespace N1.
	oldKey, err := createTestAPIKey(app, user.Id, "revoke-origin-secret")
	require.NoError(t, err)
	oldFullKey := oldKey.Id + ".revoke-origin-secret"

	oldConn := dialAgentAndEnroll(t, serverURL, oldFullKey, "agent-revoke-origin")
	defer oldConn.CloseNow()
	ctx := context.Background()

	// Register a share → control allocates an origin under N1.
	require.NoError(t, oldConn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","code":"REVOKERECLAIM1"}`)))
	_, raw, err := oldConn.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"type":"share_registered"`)
	var first struct{ Origin string `json:"origin"` }
	require.NoError(t, json.Unmarshal(raw, &first))
	require.NotEmpty(t, first.Origin)

	// Standalone revoke: key inactive + session soft-deleted + agent deleted.
	revokeReq := httptest.NewRequest(http.MethodDelete, "/api/keys/"+oldKey.Id, nil)
	revokeReq.SetPathValue("id", oldKey.Id)
	revokeRec := httptest.NewRecorder()
	revokeEvent := new(core.RequestEvent)
	revokeEvent.App = app
	revokeEvent.Request = revokeReq
	revokeEvent.Response = revokeRec
	revokeEvent.Auth = user
	require.NoError(t, RevokeAPIKey(app, h)(revokeEvent))
	require.Equal(t, http.StatusOK, revokeRec.Code, revokeRec.Body.String())

	records, err := app.FindRecordsByFilter("sessions", "code = {:code}", "", 1, 0, map[string]any{"code": "REVOKERECLAIM1"})
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.False(t, records[0].GetBool("is_active"))

	oldAgents, err := app.FindRecordsByFilter("agents", "api_key_id = {:k}", "", 1, 0, map[string]any{"k": oldKey.Id})
	require.NoError(t, err)
	require.Len(t, oldAgents, 0)

	// Re-enroll with a fresh key → fresh namespace N2.
	newKey, err := createTestAPIKey(app, user.Id, "revoke-origin-new")
	require.NoError(t, err)
	newFullKey := newKey.Id + ".revoke-origin-new"

	newConn := dialAgentAndEnroll(t, serverURL, newFullKey, "agent-revoke-origin")
	defer newConn.CloseNow()

	// Re-register the same code → a FRESH origin under N2, never the stale N1 origin.
	require.NoError(t, newConn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","code":"REVOKERECLAIM1"}`)))
	_, raw2, err := newConn.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, string(raw2), `"type":"share_registered"`)
	var second struct{ Origin string `json:"origin"` }
	require.NoError(t, json.Unmarshal(raw2, &second))
	require.NotEmpty(t, second.Origin)
	require.NotEqual(t, first.Origin, second.Origin)

	session := findSessionByCode(t, app, "REVOKERECLAIM1")
	require.NotNil(t, session)
	require.Equal(t, newKey.Id, session.GetString("api_key_id"))
	require.Equal(t, second.Origin, session.GetString("origin"))
}
