package migrations_test

import (
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/stretchr/testify/require"
	"sharebridge/control/migrations"
)

// TestBoundAgentDiagnosticTextMigration covers the T5-m1 forward migration
// (remediation R4 item 2): the two §12 diagnostic TextFields that
// AddAgentsRelaySTUN left unbounded are tightened to maxima that match the
// writers (closed reason enum ≤ 64; textual IP ≤ 45), the bound is enforced
// at the record-save layer, every value the real writers persist is still
// accepted, and re-runs are a clean no-op.
func TestBoundAgentDiagnosticTextMigration(t *testing.T) {
	testApp, err := tests.NewTestApp(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { testApp.Cleanup() })

	// Bootstrap with the full registered chain (system + app migrations 1,
	// ..., 9, 9b — PocketBase sorts by filename, so the "9b_" prefix is what
	// keeps this migration AFTER AddAgentsRelaySTUN on a fresh install).
	// Its clean application here proves the whole sorted chain composes.
	require.NoError(t, testApp.Bootstrap())
	require.NoError(t, testApp.RunSystemMigrations())

	// Reset to the PRE-migration shape (a DB at migration-9 state: both §12
	// diagnostic TextFields unbounded, Max 0 — the audited Task 5 gap) so the
	// transition the migration performs is exercised explicitly.
	agentsCol, err := testApp.FindCollectionByNameOrId("agents")
	require.NoError(t, err)
	agentsCol.Fields.GetByName("direct_status_reason").(*core.TextField).Max = 0
	agentsCol.Fields.GetByName("stun_observed_ip").(*core.TextField).Max = 0
	require.NoError(t, testApp.Save(agentsCol))

	// Run the migration; both maxima match the writers' domains.
	require.NoError(t, migrations.BoundAgentDiagnosticText(testApp))
	agentsCol, err = testApp.FindCollectionByNameOrId("agents")
	require.NoError(t, err)
	reasonField := agentsCol.Fields.GetByName("direct_status_reason").(*core.TextField)
	ipField := agentsCol.Fields.GetByName("stun_observed_ip").(*core.TextField)
	require.Equal(t, 64, reasonField.Max)
	require.Equal(t, 45, ipField.Max)

	// Every value the §12 writer (directpredicate.go) can persist is within
	// the bound and still saves.
	for _, reason := range []string{
		"", // DirectReasonNone
		"no_mapper", "stun_timeout", "stun_mismatch", "stun_not_public",
		"probe_failed", "agent_offline", // incl. the Task 20 selection enum
	} {
		rec := seedRelayTestAgent(t, testApp, "reason-"+strings.ReplaceAll(reason, "_", "-"))
		rec.Set("direct_status_reason", reason)
		require.NoError(t, testApp.Save(rec), "reason %q must save", reason)
	}
	for _, ip := range []string{
		"203.0.113.7",     // maximal IPv4 textual form (15 chars)
		"255.255.255.255", // every IPv4 literal
		"2001:db8::1",     // IPv6 observation (recorded, classified non-public)
		"0123:4567:89ab:cdef:0123:4567:89ab:cdef", // maximal 45-char IPv6 form
	} {
		rec := seedRelayTestAgent(t, testApp, "ip-"+strings.ReplaceAll(strings.ToLower(ip), ":", "-"))
		rec.Set("stun_observed_ip", ip)
		require.NoError(t, testApp.Save(rec), "ip %q must save", ip)
	}

	// Beyond the bound fails closed at the save layer.
	longReason := strings.Repeat("a", 65)
	rec := seedRelayTestAgent(t, testApp, "reason-too-long")
	rec.Set("direct_status_reason", longReason)
	require.Error(t, testApp.Save(rec), "reason beyond Max 64 must be rejected")

	longIP := strings.Repeat("a", 46)
	rec = seedRelayTestAgent(t, testApp, "ip-too-long")
	rec.Set("stun_observed_ip", longIP)
	require.Error(t, testApp.Save(rec), "ip beyond Max 45 must be rejected")

	// Idempotent: re-running is a clean no-op (no error, maxima unchanged).
	require.NoError(t, migrations.BoundAgentDiagnosticText(testApp))
	agentsCol, err = testApp.FindCollectionByNameOrId("agents")
	require.NoError(t, err)
	require.Equal(t, 64, agentsCol.Fields.GetByName("direct_status_reason").(*core.TextField).Max)
	require.Equal(t, 45, agentsCol.Fields.GetByName("stun_observed_ip").(*core.TextField).Max)
}
