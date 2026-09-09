package directctl

import (
	"net/http"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

// Inactive-reason discriminator values (§10). `inactive_reason` is the
// authoritative source for the canonical route's revoked-vs-expired-vs-
// unsupported status; a missing/unrecognized value fails safe as 404.
const (
	inactiveReasonExpired     = "expired"
	inactiveReasonRevoked     = "revoked"
	inactiveReasonUnsupported = "unsupported"
)

// ResolveForRedirect is the single owner of the canonical-route status logic
// (§8, §9.3). It resolves a share code — including inactive tombstones — to
// its persisted session record and the HTTP status the caller must return:
//   - http.StatusFound (302): active servable share; the caller then performs
//     §9.1 route selection (SelectRoute, routes.go) — interstitial for direct
//     candidates, relay 302 or 503 otherwise. The LEGACY meaning of this
//     status ("302 straight to the direct origin") is retired on the
//     canonical path; the caller must never map Found to Redirect.
//   - http.StatusGone (410): expired, or unsupported.
//   - http.StatusNotFound (404): revoked, unknown, or an inactive row whose
//     inactive_reason is missing/unrecognized (fails safe).
//
// Lifecycle classification is flag-gated in exactly one place: with
// RelaySelectionEnabled (Task 20), an active public unprotected Immich
// relay_only share is servable (over relay only — selection routes it to the
// relay URL or 503, never direct, §6.1/§13.3). Without the flag the Phase 3
// classification stands: relay_only is unsupported (410) — relay-dependent
// resolution is unavailable in rollback mode. Password-protected and
// non-Immich shares stay unsupported in both states: §13.3's restoration is
// scoped to supported public Immich gallery shares only, and tombstoned rows
// are never reactivated here. Status classification does not depend on live
// transport readiness (epoch, agent connectivity, open-signal, probe,
// presence) — those are §9.1 selection terms evaluated later, per navigation.
func (c *Controller) ResolveForRedirect(code string) (*core.Record, int) {
	if code == "" {
		return nil, http.StatusNotFound
	}

	recs, err := c.app.FindRecordsByFilter("sessions", "code = {:code}", "", 1, 0, map[string]any{"code": code})
	if err != nil || len(recs) == 0 {
		return nil, http.StatusNotFound
	}
	rec := recs[0]

	if rec.GetBool("is_active") {
		// Live expiry re-check: an active row past its lease is expired.
		if exp := rec.GetDateTime("expires_at"); !exp.IsZero() && exp.Time().Before(time.Now()) {
			return rec, http.StatusGone
		}
		// Active but of an unsupported type → gone. With relay selection
		// enabled, the public unprotected Immich relay_only share is the one
		// promoted exception: it is servable over the relay canonical route.
		if !isSupportedGallerySession(rec) {
			if !(c.cfg.RelaySelectionEnabled && isRelayOnlyPublicImmichSession(rec)) {
				return rec, http.StatusGone
			}
		}
		return rec, http.StatusFound
	}

	// Inactive tombstone: the discriminator is authoritative.
	switch rec.GetString("inactive_reason") {
	case inactiveReasonExpired, inactiveReasonUnsupported:
		return rec, http.StatusGone
	case inactiveReasonRevoked:
		return rec, http.StatusNotFound
	default:
		// Missing or unrecognized reason fails safe as 404.
		return rec, http.StatusNotFound
	}
}

// isSupportedGallerySession reports whether an active session is a servable
// gallery share: an Immich album that is neither relay-only nor
// password-protected (direct-servable under every configuration).
func isSupportedGallerySession(rec *core.Record) bool {
	return rec.GetString("share_type") == "immich" &&
		!rec.GetBool("relay_only") &&
		!rec.GetBool("is_password_protected")
}

// isRelayOnlyPublicImmichSession reports whether an active session is the
// Task 20 promoted type: a public (unprotected) Immich relay_only share,
// servable over relay only (§6.1: ShareBridge never selects, prepares,
// probes, or navigates to the direct origin for it). Evaluated only when
// RelaySelectionEnabled is on; see ResolveForRedirect.
func isRelayOnlyPublicImmichSession(rec *core.Record) bool {
	return rec.GetString("share_type") == "immich" &&
		rec.GetBool("relay_only") &&
		!rec.GetBool("is_password_protected")
}

func (c *Controller) unavailable(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store") // §9.3: selection responses are never cacheable
	w.WriteHeader(http.StatusServiceUnavailable)
	w.Write([]byte(`{"error":"direct unavailable"}`))
	return nil
}
