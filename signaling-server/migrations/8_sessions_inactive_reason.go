package migrations

import (
	"time"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() { m.Register(AddSessionsInactiveReason, nil) }

// Inactive-reason discriminator values (§10). `inactive_reason` is the
// authoritative source for the canonical route's revoked-vs-expired-vs-
// unsupported status; a missing/unrecognized value fails safe as 404.
const (
	inactiveReasonExpired     = "expired"
	inactiveReasonRevoked     = "revoked"
	inactiveReasonUnsupported = "unsupported"
)

// AddSessionsInactiveReason adds the `inactive_reason` discriminator column and
// backfills existing rows. Backfill ordering matters: unsupported
// (relay-only/WebDAV/protected) rows are classified first — so a relay-only row
// whose lease has also lapsed is "unsupported", not "expired" — and only then
// are the remaining already-inactive rows assigned expired (past expires_at) or
// revoked (everything else).
func AddSessionsInactiveReason(app core.App) error {
	sessionsCol, err := app.FindCollectionByNameOrId("sessions")
	if err != nil {
		return err
	}

	if sessionsCol.Fields.GetByName("inactive_reason") == nil {
		sessionsCol.Fields.Add(&core.TextField{Name: "inactive_reason"})
		if err := app.Save(sessionsCol); err != nil {
			return err
		}
	}

	records, err := app.FindAllRecords("sessions")
	if err != nil {
		return err
	}

	now := time.Now().UTC()

	// Pass 1: unsupported tombstones first.
	for _, rec := range records {
		if !isUnsupportedSession(rec) {
			continue
		}
		rec.Set("is_active", false)
		rec.Set("inactive_reason", inactiveReasonUnsupported)
		if err := app.Save(rec); err != nil {
			return err
		}
	}

	// Pass 2: remaining already-inactive rows — expired vs revoked.
	for _, rec := range records {
		if rec.GetBool("is_active") {
			// Active gallery rows stay active with an empty discriminator.
			continue
		}
		if rec.GetString("inactive_reason") != "" {
			// Already classified as unsupported in pass 1.
			continue
		}
		reason := inactiveReasonRevoked
		if exp := rec.GetDateTime("expires_at"); !exp.IsZero() && exp.Time().Before(now) {
			reason = inactiveReasonExpired
		}
		rec.Set("inactive_reason", reason)
		if err := app.Save(rec); err != nil {
			return err
		}
	}

	return nil
}

// isUnsupportedSession reports whether a session is of an unsupported type for
// Phase 3 direct serving: relay-only, WebDAV/file (any share_type other than
// "immich", including the legacy empty value), or password-protected.
func isUnsupportedSession(rec *core.Record) bool {
	if rec.GetBool("relay_only") {
		return true
	}
	if rec.GetBool("is_password_protected") {
		return true
	}
	return rec.GetString("share_type") != "immich"
}
