package migrations

import (
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

func TestCreateAgentsBackfillsActive(t *testing.T) {
	// Hermetic app instance: temp data dir, system migrations only (no app
	// migrations), so CreateCollections/CreateAgents can be exercised in
	// isolation like a fresh v1 -> v2 upgrade.
	app := core.NewBaseApp(core.BaseAppConfig{
		DataDir:       t.TempDir(),
		EncryptionEnv: "pb_test_env",
	})
	if err := app.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := CreateCollections(app); err != nil {
		t.Fatalf("base: %v", err)
	}

	// The api_keys.account_id relation is required, so create a user first.
	users, _ := app.FindCollectionByNameOrId("users")
	user := core.NewRecord(users)
	user.Set("email", "test@example.com")
	user.Set("password", "1234567890")
	if err := app.Save(user); err != nil {
		t.Fatalf("save user: %v", err)
	}

	// Insert a pre-existing api_key (sessions.api_key_id is required).
	apiKeys, _ := app.FindCollectionByNameOrId("api_keys")
	key := core.NewRecord(apiKeys)
	key.Set("account_id", user.Id)
	key.Set("key_hash", "x")
	if err := app.Save(key); err != nil {
		t.Fatalf("save key: %v", err)
	}

	// Insert a pre-existing session (simulates a DB migrated from v1).
	sessions, _ := app.FindCollectionByNameOrId("sessions")
	existing := core.NewRecord(sessions)
	existing.Set("code", "oldcode1")
	existing.Set("api_key_id", key.Id)
	existing.Set("agent_id", "")
	if err := app.Save(existing); err != nil {
		t.Fatalf("save existing: %v", err)
	}

	if err := CreateAgents(app); err != nil {
		t.Fatalf("create agents: %v", err)
	}

	// Existing session must be backfilled active.
	rec, err := app.FindRecordById("sessions", existing.Id)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if rec.GetBool("is_active") != true {
		t.Fatalf("existing session not backfilled active")
	}

	// Unique index on sessions.origin present.
	s2, _ := app.FindCollectionByNameOrId("sessions")
	found := false
	for _, idx := range s2.Indexes {
		if strings.Contains(idx, "origin") && strings.Contains(strings.ToUpper(idx), "UNIQUE") {
			found = true
		}
	}
	if !found {
		t.Fatalf("sessions.origin missing unique index")
	}
}
