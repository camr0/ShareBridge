package directctl

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

// GenerateNamespace returns a random, lowercase-hex namespace prefixed with
// "sb" (e.g. "sb1a2b3c4d"). It is the label used in direct-mode DNS origins
// and cert SANs.
func GenerateNamespace() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "sb" + hex.EncodeToString(b)
}

// LoadOrCreateAgent returns the existing agent record for apiKeyID, or creates
// one (with a fresh namespace and cert_status "pending") if none exists.
//
// The lookup-then-insert is retried a few times so that a lost race against a
// concurrent insert on the unique api_key_id index resolves by re-reading the
// now-existing row instead of surfacing a spurious error.
func LoadOrCreateAgent(app core.App, apiKeyID string) (*core.Record, bool, error) {
	for i := 0; i < 3; i++ {
		recs, err := app.FindRecordsByFilter("agents", "api_key_id = {:k}", "", 1, 0, map[string]any{"k": apiKeyID})
		if err != nil {
			return nil, false, err
		}
		if len(recs) > 0 {
			return recs[0], false, nil
		}

		col, _ := app.FindCollectionByNameOrId("agents")
		rec := core.NewRecord(col)
		rec.Set("api_key_id", apiKeyID)
		rec.Set("namespace", GenerateNamespace())
		rec.Set("cert_status", "pending")
		rec.Set("endpoint_port", 0)
		if err := app.Save(rec); err != nil {
			if i == 2 {
				return nil, false, err
			}
			continue // lost a unique-constraint race -> retry the lookup
		}
		return rec, true, nil
	}
	return nil, false, fmt.Errorf("agent create failed after retries")
}

var defaultOriginLabel = func() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// generateOriginLabel returns a fresh 48-bit random origin label as a
// lowercase-hex string (12 chars). It is a package-level variable so tests can
// stub it to force the unique-constraint collision path in AllocateOrigin.
var generateOriginLabel = defaultOriginLabel

// SetGenerateOriginLabel overrides the origin-label generator (test-only). A
// nil fn restores the default random generator.
func SetGenerateOriginLabel(fn func() string) {
	if fn == nil {
		generateOriginLabel = defaultOriginLabel
		return
	}
	generateOriginLabel = fn
}

// SaveCertReady marks an agent record as having a successfully installed
// certificate and records the leaf fingerprint + expiry for later renewal.
func SaveCertReady(app core.App, rec *core.Record, fingerprint string, notAfter time.Time) error {
	rec.Set("cert_status", "ready")
	rec.Set("cert_fingerprint", fingerprint)
	rec.Set("cert_expires_at", notAfter)
	return app.Save(rec)
}

// AllocateOrigin returns a free origin and, given a session record, saves it
// transactionally (retrying unique-constraint collisions) so the origin and
// session are committed atomically.
func AllocateOrigin(app core.App, namespace, baseDomain string, session *core.Record) (string, error) {
	var origin string
	err := app.RunInTransaction(func(txApp core.App) error {
		o, err := allocateOriginTx(txApp, namespace, baseDomain, session)
		if err != nil {
			return err
		}
		origin = o
		return nil
	})
	return origin, err
}

// allocateOriginTx allocates a free origin within an already-open transaction.
func allocateOriginTx(txApp core.App, namespace, baseDomain string, session *core.Record) (string, error) {
	for i := 0; i < 8; i++ {
		candidate := generateOriginLabel() + "." + namespace + "." + baseDomain
		session.Set("origin", candidate)
		session.Set("is_active", true)
		if err := txApp.Save(session); err != nil {
			if isUniqueViolation(err) {
				continue // collision -> retry with a new label
			}
			return "", err // real validation/DB error - do not mask it
		}
		return candidate, nil
	}
	return "", fmt.Errorf("could not allocate a free origin")
}

// AllocateOriginFor returns the session's already-allocated origin (re-activating
// the session), or allocates a fresh control-managed origin via the agent's
// namespace. The origin is never agent-supplied.
func (c *Controller) AllocateOriginFor(app core.App, apiKeyID string, session *core.Record) (string, error) {
	var origin string
	err := app.RunInTransaction(func(txApp core.App) error {
		o, err := c.AllocateOriginForTx(txApp, apiKeyID, session)
		if err != nil {
			return err
		}
		origin = o
		return nil
	})
	return origin, err
}

// AllocateOriginForTx is AllocateOriginFor but runs inside an already-open
// transaction (txApp), so session creation/claim and origin allocation commit
// atomically (I7).
//
// A pre-existing origin is reused only when its namespace still matches the
// agent's current namespace. After standalone key revocation deletes the agent
// row, a re-enroll gets a FRESH namespace; reclaiming the session would
// otherwise reuse the stale origin (bound under the deleted namespace) and the
// new cert/Binder would reject it. On mismatch we allocate a fresh origin under
// the current namespace, leaving the stale origin behind (never reused).
func (c *Controller) AllocateOriginForTx(txApp core.App, apiKeyID string, session *core.Record) (string, error) {
	rec, _, err := LoadOrCreateAgent(txApp, apiKeyID)
	if err != nil {
		return "", err
	}
	namespace := rec.GetString("namespace")

	if origin := session.GetString("origin"); origin != "" && originNamespace(origin) == namespace {
		session.Set("is_active", true)
		if err := txApp.Save(session); err != nil {
			return "", err
		}
		return origin, nil
	}
	return allocateOriginTx(txApp, namespace, c.cfg.BaseDomain, session)
}

// originNamespace extracts the namespace component from a direct-mode origin of
// the form "<label>.<namespace>.<base-domain>". The label is lowercase hex (no
// dots) and the namespace is "sb"+hex (no dots), so the namespace is always the
// second dot-separated component.
func originNamespace(origin string) string {
	parts := strings.Split(origin, ".")
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

// IsUniqueViolation reports whether err is a SQLite UNIQUE constraint failure
// (exported for the handler package's collision-retry path).
func IsUniqueViolation(err error) bool {
	return isUniqueViolation(err)
}

// isUniqueViolation reports whether err is a SQLite UNIQUE constraint failure.
//
// It matches both forms the error can reach this code in:
//   - the raw modernc.org/sqlite text ("UNIQUE constraint failed: <col>"), and
//   - the normalized validation error PocketBase's Save surfaces from that
//     constraint failure ("<field>: Value must be unique").
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint failed") ||
		strings.Contains(msg, "value must be unique")
}
