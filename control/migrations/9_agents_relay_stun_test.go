package migrations_test

import (
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/stretchr/testify/require"
	"sharebridge/control/migrations"
)

// TestAgentsRelaySTUNMigration covers migration 9 (§12): the seven diagnostic
// agent fields, the direct_status enum, preservation of existing agent rows,
// idempotent re-runs, and the partial unique index on nonzero relay_port.
func TestAgentsRelaySTUNMigration(t *testing.T) {
	testApp, err := tests.NewTestApp(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { testApp.Cleanup() })

	require.NoError(t, testApp.Bootstrap())
	require.NoError(t, testApp.RunSystemMigrations())
	require.NoError(t, migrations.CreateCollections(testApp))
	require.NoError(t, migrations.CreateAgents(testApp))

	// Seed a pre-existing agent row (simulates a DB at migration 8 state).
	existing := seedRelayTestAgent(t, testApp, "seed-ns")
	existingNamespace := existing.GetString("namespace")
	existingAPIKey := existing.GetString("api_key_id")

	require.NoError(t, migrations.AddAgentsRelaySTUN(testApp))

	agentsCol, err := testApp.FindCollectionByNameOrId("agents")
	require.NoError(t, err)

	// All seven diagnostic fields exist with the specified types.
	for _, name := range []string{
		"relay_port", "relay_generation", "relay_last_seen_at",
		"stun_observed_ip", "stun_observed_at", "direct_status", "direct_status_reason",
	} {
		require.NotNil(t, agentsCol.Fields.GetByName(name), "agents collection missing field %q", name)
	}

	require.IsType(t, &core.NumberField{}, agentsCol.Fields.GetByName("relay_port"))
	require.IsType(t, &core.NumberField{}, agentsCol.Fields.GetByName("relay_generation"))
	require.IsType(t, &core.DateField{}, agentsCol.Fields.GetByName("relay_last_seen_at"))
	require.IsType(t, &core.TextField{}, agentsCol.Fields.GetByName("stun_observed_ip"))
	require.IsType(t, &core.DateField{}, agentsCol.Fields.GetByName("stun_observed_at"))
	require.IsType(t, &core.SelectField{}, agentsCol.Fields.GetByName("direct_status"))
	require.IsType(t, &core.TextField{}, agentsCol.Fields.GetByName("direct_status_reason"))

	relayPortField := agentsCol.Fields.GetByName("relay_port").(*core.NumberField)
	require.True(t, relayPortField.OnlyInt, "relay_port must be an integer")
	require.False(t, relayPortField.Required, "relay_port must allow zero values")

	relayGenField := agentsCol.Fields.GetByName("relay_generation").(*core.NumberField)
	require.True(t, relayGenField.OnlyInt, "relay_generation must be an integer")
	require.False(t, relayGenField.Required, "relay_generation must allow zero values")

	for _, name := range []string{"relay_last_seen_at", "stun_observed_at"} {
		dateField := agentsCol.Fields.GetByName(name).(*core.DateField)
		require.False(t, dateField.Required, "%s must allow empty values", name)
	}

	stunIPField := agentsCol.Fields.GetByName("stun_observed_ip").(*core.TextField)
	require.False(t, stunIPField.Required, "stun_observed_ip must allow empty values")

	reasonField := agentsCol.Fields.GetByName("direct_status_reason").(*core.TextField)
	require.False(t, reasonField.Required, "direct_status_reason must allow empty values")

	// direct_status is a diagnostic enum limited to the three §12 values.
	directStatusField := agentsCol.Fields.GetByName("direct_status").(*core.SelectField)
	require.Equal(t, 1, directStatusField.MaxSelect)
	require.False(t, directStatusField.Required, "direct_status must allow empty values")
	require.Equal(t, []string{"unknown", "eligible", "relay_fallback"}, directStatusField.Values)

	// Existing agent record preserved through migration, with all new
	// diagnostic fields at their zero/empty defaults.
	preserved, err := testApp.FindRecordById("agents", existing.Id)
	require.NoError(t, err)
	require.Equal(t, existingNamespace, preserved.GetString("namespace"))
	require.Equal(t, existingAPIKey, preserved.GetString("api_key_id"))
	require.Equal(t, 0, preserved.GetInt("relay_port"))
	require.Equal(t, 0, preserved.GetInt("relay_generation"))
	require.True(t, preserved.GetDateTime("relay_last_seen_at").IsZero())
	require.Equal(t, "", preserved.GetString("stun_observed_ip"))
	require.True(t, preserved.GetDateTime("stun_observed_at").IsZero())
	require.Equal(t, "", preserved.GetString("direct_status"))
	require.Equal(t, "", preserved.GetString("direct_status_reason"))

	// A fresh agent saved with every new field zero/empty is accepted.
	require.NoError(t, testApp.Save(seedRelayTestAgent(t, testApp, "all-empty")))

	// Each allowed direct_status value passes validation.
	for _, status := range []string{"unknown", "eligible", "relay_fallback"} {
		rec := seedRelayTestAgent(t, testApp, "status-"+status)
		rec.Set("direct_status", status)
		require.NoError(t, testApp.Save(rec), "direct_status %q must be allowed", status)
	}

	// Values outside the enum are rejected.
	badEnum := seedRelayTestAgent(t, testApp, "bad-enum")
	badEnum.Set("direct_status", "routable")
	require.Error(t, testApp.Save(badEnum), "direct_status must reject values outside the enum")

	// The partial unique index enforces uniqueness only for nonzero
	// relay_port: zero values coexist...
	zeroA := seedRelayTestAgent(t, testApp, "zero-a")
	zeroB := seedRelayTestAgent(t, testApp, "zero-b")
	zeroB.Set("relay_port", 0)
	require.NoError(t, testApp.Save(zeroA))
	require.NoError(t, testApp.Save(zeroB))

	// ...while duplicate nonzero ports conflict...
	dup := seedRelayTestAgent(t, testApp, "dup")
	dup.Set("relay_port", 40101)
	require.NoError(t, testApp.Save(dup))
	clash := seedRelayTestAgent(t, testApp, "clash")
	clash.Set("relay_port", 40101)
	require.Error(t, testApp.Save(clash), "duplicate nonzero relay_port must be rejected")

	// ...a distinct nonzero port is accepted...
	other := seedRelayTestAgent(t, testApp, "other")
	other.Set("relay_port", 40102)
	require.NoError(t, testApp.Save(other))

	// ...and a zero port does not clash with the nonzero ports above.
	zeroAfter := seedRelayTestAgent(t, testApp, "zero-after")
	zeroAfter.Set("relay_port", 0)
	require.NoError(t, testApp.Save(zeroAfter))

	// Existing API-key/namespace unique indexes remain alongside the new
	// partial unique index.
	require.NotEmpty(t, agentsCol.GetIndex("idx_agents_api_key_id"))
	require.NotEmpty(t, agentsCol.GetIndex("idx_agents_namespace"))

	relayPortIndex := agentsCol.GetIndex("idx_agents_relay_port")
	require.NotEmpty(t, relayPortIndex, "agents collection missing idx_agents_relay_port")
	require.True(t, strings.Contains(strings.ToUpper(relayPortIndex), "UNIQUE"), "relay_port index must be unique")
	require.True(t, strings.Contains(relayPortIndex, "relay_port"), "index must target relay_port")
	require.True(t, strings.Contains(relayPortIndex, "0"), "relay_port index must be partial (nonzero only)")

	// Idempotent: re-running is a clean no-op — no error, no duplicated
	// fields or indexes.
	require.NoError(t, migrations.AddAgentsRelaySTUN(testApp))
	afterCol, err := testApp.FindCollectionByNameOrId("agents")
	require.NoError(t, err)
	require.Equal(t, len(agentsCol.Fields), len(afterCol.Fields), "field count must not change on re-run")
	relayPortIndexCount := 0
	for _, idx := range afterCol.Indexes {
		if strings.Contains(idx, "idx_agents_relay_port") {
			relayPortIndexCount++
		}
	}
	require.Equal(t, 1, relayPortIndexCount, "relay_port index must not be duplicated on re-run")
}

// seedRelayTestAgent inserts an agents row (with its own api_key, since
// agents.api_key_id is unique) and only sets relay_port when the collection
// already has the field (i.e. migration 9 has run).
func seedRelayTestAgent(t *testing.T, app core.App, namespace string) *core.Record {
	t.Helper()
	key := mustMigAPIKey(t, app, "relay9-"+namespace)
	agentsCol, err := app.FindCollectionByNameOrId("agents")
	require.NoError(t, err)
	rec := core.NewRecord(agentsCol)
	rec.Set("api_key_id", key.Id)
	rec.Set("namespace", namespace)
	if agentsCol.Fields.GetByName("relay_port") != nil {
		rec.Set("relay_port", 0)
	}
	require.NoError(t, app.Save(rec))
	return rec
}
