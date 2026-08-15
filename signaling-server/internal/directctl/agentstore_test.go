package directctl

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

func TestGenerateNamespace(t *testing.T) {
	re := regexp.MustCompile(`^sb[0-9a-f]{8}$`)
	for i := 0; i < 500; i++ {
		if !re.MatchString(GenerateNamespace()) {
			t.Fatalf("bad namespace")
		}
	}
}

func TestLoadOrCreateAgent(t *testing.T) {
	app, _ := newTestController(t)
	key := mustAPIKey(t, app, "load-or-create")

	rec, created, err := LoadOrCreateAgent(app, key.Id)
	if err != nil || !created {
		t.Fatalf("expected created, got err=%v created=%v", err, created)
	}
	if rec.GetString("cert_status") != "pending" {
		t.Fatalf("status")
	}

	rec2, created2, _ := LoadOrCreateAgent(app, key.Id)
	if created2 || rec2.Id != rec.Id {
		t.Fatalf("expected loaded")
	}
}

func TestAllocateOriginIsUniqueAndTransactionallySaved(t *testing.T) {
	app, _ := newTestController(t)
	key := mustAPIKey(t, app, "allocate-origin")

	agent, _, err := LoadOrCreateAgent(app, key.Id)
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	ns := agent.GetString("namespace")

	sessions, _ := app.FindCollectionByNameOrId("sessions")
	shape := regexp.MustCompile(`^[0-9a-f]{12}\.` + regexp.QuoteMeta(ns) + `\.example\.com$`)

	const n = 64
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		sess := core.NewRecord(sessions)
		sess.Set("code", fmt.Sprintf("code-%d", i))
		sess.Set("api_key_id", key.Id)

		origin, err := AllocateOrigin(app, ns, "example.com", sess)
		if err != nil {
			t.Fatalf("allocate %d: %v", i, err)
		}
		if !shape.MatchString(origin) {
			t.Fatalf("origin %q does not match expected shape", origin)
		}
		if seen[origin] {
			t.Fatalf("duplicate origin %q", origin)
		}
		seen[origin] = true

		// The origin + is_active must be committed atomically onto the session
		// row by the time AllocateOrigin returns.
		persisted, err := app.FindRecordById("sessions", sess.Id)
		if err != nil {
			t.Fatalf("reload session %d: %v", i, err)
		}
		if persisted.GetString("origin") != origin {
			t.Fatalf("origin not persisted: got %q want %q", persisted.GetString("origin"), origin)
		}
		if !persisted.GetBool("is_active") {
			t.Fatalf("is_active not persisted")
		}
	}
}

// mustAPIKey creates a user (api_keys.account_id is required) and an api_key
// record for it, returning the saved key.
func mustAPIKey(t *testing.T, app core.App, hash string) *core.Record {
	t.Helper()
	users, _ := app.FindCollectionByNameOrId("users")
	user := core.NewRecord(users)
	user.Set("email", hash+"@example.com")
	user.Set("password", "1234567890")
	if err := app.Save(user); err != nil {
		t.Fatalf("save user: %v", err)
	}

	apiKeys, _ := app.FindCollectionByNameOrId("api_keys")
	key := core.NewRecord(apiKeys)
	key.Set("account_id", user.Id)
	key.Set("key_hash", hash)
	if err := app.Save(key); err != nil {
		t.Fatalf("save key: %v", err)
	}
	return key
}
