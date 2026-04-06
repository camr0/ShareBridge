package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/pocketbase/pocketbase/tools/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"sharebridge/server/migrations"
)

func setupTestApp(t *testing.T) (core.App, func()) {
	testApp, err := tests.NewTestApp(t.TempDir())
	require.NoError(t, err)

	// Bootstrap the app and run system migrations
	err = testApp.Bootstrap()
	require.NoError(t, err)
	err = testApp.RunSystemMigrations()
	require.NoError(t, err)

	// Run our custom migrations
	err = migrations.CreateCollections(testApp)
	require.NoError(t, err)

	cleanup := func() { testApp.Cleanup() }
	return testApp, cleanup
}

func createTestUser(app core.App, email string) (*core.Record, error) {
	usersCol, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		return nil, err
	}

	user := core.NewRecord(usersCol)
	user.SetEmail(email)
	user.SetPassword("testpassword123")
	if err := app.Save(user); err != nil {
		return nil, err
	}
	return user, nil
}

func createTestAPIKey(app core.App, userID string, secret string) (*core.Record, error) {
	apiKeysCol, err := app.FindCollectionByNameOrId("api_keys")
	if err != nil {
		return nil, err
	}

	// Save first to get the record ID, then hash fullKey = record.Id + "." + secret.
	// This matches the production CreateAPIKey handler.
	record := core.NewRecord(apiKeysCol)
	record.Set("account_id", userID)
	record.Set("is_active", true)
	record.Set("label", "test key")
	if err := app.Save(record); err != nil {
		return nil, err
	}

	fullKey := record.Id + "." + secret
	hash, err := bcrypt.GenerateFromPassword([]byte(fullKey), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	record.Set("key_hash", string(hash))
	if err := app.Save(record); err != nil {
		return nil, err
	}
	return record, nil
}

func TestAPIKeyAuth_ValidKey(t *testing.T) {
	app, cleanup := setupTestApp(t)
	defer cleanup()

	// Create a test user and API key
	user, err := createTestUser(app, "test@example.com")
	require.NoError(t, err)

	secret := "testsecret123"
	apiKey, err := createTestAPIKey(app, user.Id, secret)
	require.NoError(t, err)

	// Build the full API key: record_id.secret
	fullKey := apiKey.Id + "." + secret

	// Create handler that checks context values
	var capturedAPIKeyID, capturedAccountID string
	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAPIKeyID = GetAPIKeyID(r.Context())
		capturedAccountID = GetAccountID(r.Context())
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	})

	// Create router with middleware
	authMiddleware := APIKeyAuth(app)
	router := authMiddleware(testHandler)

	req := httptest.NewRequest("GET", "/test?api_key="+fullKey, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, apiKey.Id, capturedAPIKeyID)
	assert.Equal(t, user.Id, capturedAccountID)
}

func TestAPIKeyAuth_InvalidKey(t *testing.T) {
	app, cleanup := setupTestApp(t)
	defer cleanup()

	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	})

	authMiddleware := APIKeyAuth(app)
	router := authMiddleware(testHandler)

	req := httptest.NewRequest("GET", "/test?api_key=invalid", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAPIKeyAuth_MissingKey(t *testing.T) {
	app, cleanup := setupTestApp(t)
	defer cleanup()

	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	})

	authMiddleware := APIKeyAuth(app)
	router := authMiddleware(testHandler)

	req := httptest.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAPIKeyAuth_InactiveKey(t *testing.T) {
	app, cleanup := setupTestApp(t)
	defer cleanup()

	// Create a test user and API key
	user, err := createTestUser(app, "test@example.com")
	require.NoError(t, err)

	secret := "testsecret123"
	apiKey, err := createTestAPIKey(app, user.Id, secret)
	require.NoError(t, err)

	// Deactivate the key
	apiKey.Set("is_active", false)
	err = app.Save(apiKey)
	require.NoError(t, err)

	fullKey := apiKey.Id + "." + secret

	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	})

	authMiddleware := APIKeyAuth(app)
	router := authMiddleware(testHandler)

	req := httptest.NewRequest("GET", "/test?api_key="+fullKey, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAPIKeyAuth_WrongSecret(t *testing.T) {
	app, cleanup := setupTestApp(t)
	defer cleanup()

	// Create a test user and API key
	user, err := createTestUser(app, "test@example.com")
	require.NoError(t, err)

	secret := "testsecret123"
	apiKey, err := createTestAPIKey(app, user.Id, secret)
	require.NoError(t, err)

	// Use wrong secret
	fullKey := apiKey.Id + "." + "wrongsecret"

	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	})

	authMiddleware := APIKeyAuth(app)
	router := authMiddleware(testHandler)

	req := httptest.NewRequest("GET", "/test?api_key="+fullKey, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAPIKeyAuth_UpdatesLastUsedAt(t *testing.T) {
	app, cleanup := setupTestApp(t)
	defer cleanup()

	// Create a test user and API key
	user, err := createTestUser(app, "test@example.com")
	require.NoError(t, err)

	secret := "testsecret123"
	apiKey, err := createTestAPIKey(app, user.Id, secret)
	require.NoError(t, err)

	// Set last_used_at to a past time
	pastTime := types.NowDateTime().Add(-time.Hour)
	apiKey.Set("last_used_at", pastTime)
	err = app.Save(apiKey)
	require.NoError(t, err)

	fullKey := apiKey.Id + "." + secret

	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	})

	authMiddleware := APIKeyAuth(app)
	router := authMiddleware(testHandler)

	req := httptest.NewRequest("GET", "/test?api_key="+fullKey, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	// Reload the API key record and check last_used_at was updated
	updatedKey, err := app.FindRecordById("api_keys", apiKey.Id)
	require.NoError(t, err)

	lastUsed := updatedKey.GetDateTime("last_used_at")
	assert.False(t, lastUsed.IsZero())
	assert.True(t, lastUsed.Time().After(pastTime.Time()))
}
