package directctl

import (
	"errors"
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

// TestAllocateOriginRetriesOnCollision forces the unique-constraint collision
// branch of AllocateOrigin by stubbing generateOriginLabel to return a fixed
// label on the first call (which collides with a pre-seeded session's origin)
// and a fresh label afterward. It asserts the transaction recovers and returns
// the second label, and that the pre-seeded session's origin is unchanged.
func TestAllocateOriginRetriesOnCollision(t *testing.T) {
	app, _ := newTestController(t)
	key := mustAPIKey(t, app, "allocate-collision")

	agent, _, err := LoadOrCreateAgent(app, key.Id)
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	ns := agent.GetString("namespace")

	const firstLabel = "000000000000"
	const secondLabel = "111111111111"

	// Pre-seed a session whose origin is firstLabel.<ns>.example.com, so the
	// first AllocateOrigin save attempt collides against the unique origin index.
	sessions, _ := app.FindCollectionByNameOrId("sessions")
	seeded := core.NewRecord(sessions)
	seeded.Set("code", "collision-seed")
	seeded.Set("api_key_id", key.Id)
	seeded.Set("origin", firstLabel+"."+ns+".example.com")
	seeded.Set("is_active", true)
	if err := app.Save(seeded); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// Stub generateOriginLabel: first call returns the colliding label, then
	// subsequent calls return a fresh one.
	original := generateOriginLabel
	t.Cleanup(func() { generateOriginLabel = original })
	calls := 0
	generateOriginLabel = func() string {
		calls++
		if calls == 1 {
			return firstLabel
		}
		return secondLabel
	}

	sess := core.NewRecord(sessions)
	sess.Set("code", "collision-new")
	sess.Set("api_key_id", key.Id)

	origin, err := AllocateOrigin(app, ns, "example.com", sess)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if origin != secondLabel+"."+ns+".example.com" {
		t.Fatalf("origin = %q, want %q", origin, secondLabel+"."+ns+".example.com")
	}
	if calls < 2 {
		t.Fatalf("generateOriginLabel called %d times, want >= 2 (collision retried)", calls)
	}

	// The pre-seeded session's origin must be unchanged.
	seededReload, err := app.FindRecordById("sessions", seeded.Id)
	if err != nil {
		t.Fatalf("reload seeded: %v", err)
	}
	if got := seededReload.GetString("origin"); got != firstLabel+"."+ns+".example.com" {
		t.Fatalf("seeded origin changed: got %q want %q", got, firstLabel+"."+ns+".example.com")
	}
}

// TestIsUniqueViolation pins the fragile string matching in isUniqueViolation
// against the actual modernc.org/sqlite unique-constraint error text.
func TestIsUniqueViolation(t *testing.T) {
	// Raw modernc.org/sqlite unique-constraint error text.
	if !isUniqueViolation(errors.New("constraint failed: UNIQUE constraint failed: sessions.origin (2067)")) {
		t.Fatalf("expected raw sqlite unique-constraint error to match")
	}
	// The normalized form PocketBase's Save surfaces for the same failure.
	if !isUniqueViolation(errors.New("origin: Value must be unique")) {
		t.Fatalf("expected normalized unique-constraint error to match")
	}
	if isUniqueViolation(errors.New("validation error")) {
		t.Fatalf("generic error must not match")
	}
	if isUniqueViolation(nil) {
		t.Fatalf("nil error must not match")
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
