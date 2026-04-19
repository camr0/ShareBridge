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
	require.NotNil(t, apiKeysCol.Fields.GetByName("created"))
	require.NotNil(t, apiKeysCol.Fields.GetByName("updated"))

	// Check sessions has expected fields
	require.NotNil(t, sessionsCol.Fields.GetByName("code"))
	require.NotNil(t, sessionsCol.Fields.GetByName("api_key_id"))
	require.NotNil(t, sessionsCol.Fields.GetByName("agent_id"))
	require.NotNil(t, sessionsCol.Fields.GetByName("expires_at"))

	// Verify unique index on sessions.code
	idx := sessionsCol.GetIndex("idx_sessions_code")
	require.NotEmpty(t, idx)
}

func TestMigration3_AddQuotaFields(t *testing.T) {
	testApp, err := tests.NewTestApp(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { testApp.Cleanup() })

	// Bootstrap the app and run system migrations to create default collections (including users)
	err = testApp.Bootstrap()
	require.NoError(t, err)
	err = testApp.RunSystemMigrations()
	require.NoError(t, err)

	// Run migrations 1 and 2 first
	err = migrations.CreateCollections(testApp)
	require.NoError(t, err)
	err = migrations.AddAPIKeyTimestamps(testApp)
	require.NoError(t, err)

	// Run migration 3
	err = migrations.AddQuotaFields(testApp)
	require.NoError(t, err)

	// users collection should have quota fields
	usersCol, err := testApp.FindCollectionByNameOrId("users")
	require.NoError(t, err)
	require.NotNil(t, usersCol)

	for _, fieldName := range []string{"relay_quota_gb", "current_period_usage_gb", "quota_period_start", "quota_period_end", "turn_baseline_bytes"} {
		require.NotNil(t, usersCol.Fields.GetByName(fieldName), "users collection missing field %q", fieldName)
	}

	// bandwidth_usage collection should exist
	bwCol, err := testApp.FindCollectionByNameOrId("bandwidth_usage")
	require.NoError(t, err)
	require.NotNil(t, bwCol)
	require.True(t, bwCol.IsBase())

	for _, fieldName := range []string{"account_id", "period_start", "period_end", "bytes_transferred"} {
		require.NotNil(t, bwCol.Fields.GetByName(fieldName), "bandwidth_usage collection missing field %q", fieldName)
	}

	// Idempotent: running again should not error
	err = migrations.AddQuotaFields(testApp)
	require.NoError(t, err)
}

func TestMigration4_AddRelayOnly(t *testing.T) {
	testApp, err := tests.NewTestApp(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { testApp.Cleanup() })

	require.NoError(t, testApp.Bootstrap())
	require.NoError(t, testApp.RunSystemMigrations())
	require.NoError(t, migrations.CreateCollections(testApp))
	require.NoError(t, migrations.AddRelayOnly(testApp))

	sessionsCol, err := testApp.FindCollectionByNameOrId("sessions")
	require.NoError(t, err)
	require.NotNil(t, sessionsCol.Fields.GetByName("relay_only"))
}

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
