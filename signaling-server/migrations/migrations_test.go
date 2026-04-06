package migrations_test

import (
	"testing"

	"github.com/pocketbase/pocketbase/tests"
	"github.com/stretchr/testify/require"
	"sharebridge/server/migrations"
)

func TestCreateCollections(t *testing.T) {
	testApp, err := tests.NewTestApp(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { testApp.Cleanup() })

	// Bootstrap the app and run system migrations to create default collections (including users)
	err = testApp.Bootstrap()
	require.NoError(t, err)
	err = testApp.RunSystemMigrations()
	require.NoError(t, err)

	// Run the migration
	err = migrations.CreateCollections(testApp)
	require.NoError(t, err)

	// Verify api_keys collection exists
	apiKeysCol, err := testApp.FindCollectionByNameOrId("api_keys")
	require.NoError(t, err)
	require.NotNil(t, apiKeysCol)
	require.True(t, apiKeysCol.IsBase())

	// Verify sessions collection exists
	sessionsCol, err := testApp.FindCollectionByNameOrId("sessions")
	require.NoError(t, err)
	require.NotNil(t, sessionsCol)
	require.True(t, sessionsCol.IsBase())

	// Check api_keys has expected fields
	require.NotNil(t, apiKeysCol.Fields.GetByName("account_id"))
	require.NotNil(t, apiKeysCol.Fields.GetByName("key_hash"))
	require.NotNil(t, apiKeysCol.Fields.GetByName("label"))
	require.NotNil(t, apiKeysCol.Fields.GetByName("last_used_at"))
	require.NotNil(t, apiKeysCol.Fields.GetByName("is_active"))

	// Check sessions has expected fields
	require.NotNil(t, sessionsCol.Fields.GetByName("code"))
	require.NotNil(t, sessionsCol.Fields.GetByName("api_key_id"))
	require.NotNil(t, sessionsCol.Fields.GetByName("agent_id"))
	require.NotNil(t, sessionsCol.Fields.GetByName("expires_at"))

	// Verify unique index on sessions.code
	idx := sessionsCol.GetIndex("idx_sessions_code")
	require.NotEmpty(t, idx)
}
