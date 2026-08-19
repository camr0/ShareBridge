package migrations_test

import (
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/stretchr/testify/require"
	"sharebridge/control/migrations"
)

func TestMigration8_AddSessionsInactiveReason(t *testing.T) {
	testApp, err := tests.NewTestApp(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { testApp.Cleanup() })

	require.NoError(t, testApp.Bootstrap())
	require.NoError(t, testApp.RunSystemMigrations())
	require.NoError(t, migrations.CreateCollections(testApp))
	require.NoError(t, migrations.AddRelayOnly(testApp))
	require.NoError(t, migrations.AddSessionRelayStaticPub(testApp))
	require.NoError(t, migrations.AddImmichSessionFields(testApp))
	require.NoError(t, migrations.CreateAgents(testApp))

	// Seed sessions representing each pre-migration lifecycle state.
	seed := func(code, shareType string, relayOnly, protected, active bool, expiresAt time.Time) {
		key := mustMigAPIKey(t, testApp, code)
		col, _ := testApp.FindCollectionByNameOrId("sessions")
		rec := core.NewRecord(col)
		rec.Set("code", code)
		rec.Set("api_key_id", key.Id)
		rec.Set("agent_id", "agent-1")
		rec.Set("share_type", shareType)
		rec.Set("relay_only", relayOnly)
		rec.Set("is_password_protected", protected)
		rec.Set("is_active", active)
		if !expiresAt.IsZero() {
			rec.Set("expires_at", expiresAt)
		}
		require.NoError(t, testApp.Save(rec))
	}

	// Active gallery (immich, direct, unprotected) — must survive active.
	seed("gal", "immich", false, false, true, time.Now().Add(time.Hour))
	// Active relay-only — unsupported tombstone (checked before expiry).
	seed("relay", "immich", true, false, true, time.Now().Add(-time.Hour))
	// Active WebDAV — unsupported tombstone.
	seed("webdav", "opencloud", false, false, true, time.Time{})
	// Active protected — unsupported tombstone.
	seed("prot", "immich", false, true, true, time.Now().Add(time.Hour))
	// Inactive immich past expiry — expired.
	seed("exp", "immich", false, false, false, time.Now().Add(-time.Hour))
	// Inactive immich future expiry — revoked.
	seed("rev", "immich", false, false, false, time.Now().Add(time.Hour))

	require.NoError(t, migrations.AddSessionsInactiveReason(testApp))

	col, _ := testApp.FindCollectionByNameOrId("sessions")
	require.NotNil(t, col.Fields.GetByName("inactive_reason"), "sessions collection missing inactive_reason")

	get := func(code string) *core.Record {
		recs, err := testApp.FindRecordsByFilter("sessions", "code = {:code}", "", 1, 0, map[string]any{"code": code})
		require.NoError(t, err)
		require.Len(t, recs, 1, "session %q", code)
		return recs[0]
	}

	// Active gallery remains active with an empty discriminator.
	g := get("gal")
	require.True(t, g.GetBool("is_active"), "active gallery must stay active")
	require.Equal(t, "", g.GetString("inactive_reason"))

	// Unsupported is assigned first (a relay-only row with a past expiry is
	// "unsupported", not "expired").
	require.False(t, get("relay").GetBool("is_active"))
	require.Equal(t, "unsupported", get("relay").GetString("inactive_reason"))
	require.False(t, get("webdav").GetBool("is_active"))
	require.Equal(t, "unsupported", get("webdav").GetString("inactive_reason"))
	require.False(t, get("prot").GetBool("is_active"))
	require.Equal(t, "unsupported", get("prot").GetString("inactive_reason"))

	// Already-inactive rows: past expiry → expired, future expiry → revoked.
	require.False(t, get("exp").GetBool("is_active"))
	require.Equal(t, "expired", get("exp").GetString("inactive_reason"))
	require.False(t, get("rev").GetBool("is_active"))
	require.Equal(t, "revoked", get("rev").GetString("inactive_reason"))

	// Idempotent: running again must not error.
	require.NoError(t, migrations.AddSessionsInactiveReason(testApp))
}

func mustMigAPIKey(t *testing.T, app core.App, hash string) *core.Record {
	t.Helper()
	users, _ := app.FindCollectionByNameOrId("users")
	user := core.NewRecord(users)
	user.Set("email", hash+"@example.com")
	user.Set("password", "1234567890")
	require.NoError(t, app.Save(user))

	apiKeys, _ := app.FindCollectionByNameOrId("api_keys")
	key := core.NewRecord(apiKeys)
	key.Set("account_id", user.Id)
	key.Set("key_hash", hash)
	require.NoError(t, app.Save(key))
	return key
}
