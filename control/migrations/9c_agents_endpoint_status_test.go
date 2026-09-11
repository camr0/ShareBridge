package migrations_test

import (
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/stretchr/testify/require"
	"sharebridge/control/migrations"
)

// TestAgentEndpointStatusMigration covers migration 10 (M4 closeout finding 3):
// the two durable endpoint-escalation fields are added, bounded, optional, and
// the migration is idempotent.
func TestAgentEndpointStatusMigration(t *testing.T) {
	testApp, err := tests.NewTestApp(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { testApp.Cleanup() })

	require.NoError(t, testApp.Bootstrap())
	require.NoError(t, testApp.RunSystemMigrations())
	require.NoError(t, migrations.CreateCollections(testApp))
	require.NoError(t, migrations.CreateAgents(testApp))
	require.NoError(t, migrations.AddAgentsRelaySTUN(testApp))

	require.NoError(t, migrations.AddAgentEndpointStatus(testApp))

	agentsCol, err := testApp.FindCollectionByNameOrId("agents")
	require.NoError(t, err)

	statusField, ok := agentsCol.Fields.GetByName("endpoint_status").(*core.TextField)
	require.True(t, ok, "agents.endpoint_status must be a text field")
	require.Equal(t, 32, statusField.Max, "endpoint_status must be bounded")
	require.False(t, statusField.Required, "endpoint_status must be optional")

	atField := agentsCol.Fields.GetByName("endpoint_status_at")
	require.IsType(t, &core.DateField{}, atField)
	require.False(t, atField.(*core.DateField).Required, "endpoint_status_at must be optional")

	// Idempotent: a re-run is a no-op and must not error.
	require.NoError(t, migrations.AddAgentEndpointStatus(testApp))
}
