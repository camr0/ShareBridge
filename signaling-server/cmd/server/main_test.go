package main

import (
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/pocketbase/pocketbase/tools/types"
	"github.com/stretchr/testify/require"
	"sharebridge/server/migrations"
)

func setupServerTestApp(t *testing.T) (core.App, func()) {
	t.Helper()

	testApp, err := tests.NewTestApp(t.TempDir())
	require.NoError(t, err)

	require.NoError(t, testApp.Bootstrap())
	require.NoError(t, testApp.RunSystemMigrations())
	require.NoError(t, migrations.CreateCollections(testApp))

	return testApp, func() { testApp.Cleanup() }
}

func createServerTestUser(t *testing.T, app core.App, email string) *core.Record {
	t.Helper()

	usersCol, err := app.FindCollectionByNameOrId("users")
	require.NoError(t, err)

	user := core.NewRecord(usersCol)
	user.SetEmail(email)
	user.SetPassword("testpassword123")
	require.NoError(t, app.Save(user))

	return user
}

func createServerTestAPIKey(t *testing.T, app core.App, userID string) *core.Record {
	t.Helper()

	apiKeysCol, err := app.FindCollectionByNameOrId("api_keys")
	require.NoError(t, err)

	record := core.NewRecord(apiKeysCol)
	record.Set("account_id", userID)
	record.Set("is_active", true)
	record.Set("label", "test key")
	require.NoError(t, app.Save(record))

	return record
}

func createServerTestSession(t *testing.T, app core.App, apiKeyID, code string, expiresAt *time.Time) *core.Record {
	t.Helper()

	sessionsCol, err := app.FindCollectionByNameOrId("sessions")
	require.NoError(t, err)

	record := core.NewRecord(sessionsCol)
	record.Set("code", code)
	record.Set("api_key_id", apiKeyID)
	record.Set("agent_id", "test-agent")
	if expiresAt != nil {
		dt, err := types.ParseDateTime(*expiresAt)
		require.NoError(t, err)
		record.Set("expires_at", dt)
	}
	require.NoError(t, app.Save(record))

	return record
}

func testSessionExists(t *testing.T, app core.App, code string) bool {
	t.Helper()

	records, err := app.FindRecordsByFilter(
		"sessions",
		"code = {:code}",
		"",
		1,
		0,
		map[string]any{"code": code},
	)
	require.NoError(t, err)
	return len(records) == 1
}

func TestDeleteExpiredSessions_PreservesSessionsWithoutExpiry(t *testing.T) {
	app, cleanup := setupServerTestApp(t)
	defer cleanup()

	user := createServerTestUser(t, app, "noexpiry@example.com")
	apiKey := createServerTestAPIKey(t, app, user.Id)
	createServerTestSession(t, app, apiKey.Id, "NOEXPIRY1", nil)

	require.NoError(t, deleteExpiredSessions(app))
	require.True(t, testSessionExists(t, app, "NOEXPIRY1"))
}

func TestDeleteExpiredSessions_DeletesOnlyExpiredSessions(t *testing.T) {
	app, cleanup := setupServerTestApp(t)
	defer cleanup()

	user := createServerTestUser(t, app, "expired@example.com")
	apiKey := createServerTestAPIKey(t, app, user.Id)

	expiredAt := time.Now().UTC().Add(-1 * time.Hour)
	futureAt := time.Now().UTC().Add(1 * time.Hour)

	createServerTestSession(t, app, apiKey.Id, "EXPIRED1", &expiredAt)
	createServerTestSession(t, app, apiKey.Id, "FUTURE01", &futureAt)

	require.NoError(t, deleteExpiredSessions(app))
	require.False(t, testSessionExists(t, app, "EXPIRED1"))
	require.True(t, testSessionExists(t, app, "FUTURE01"))
}
