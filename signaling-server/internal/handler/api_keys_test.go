package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sharebridge/server/internal/hub"
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
