package directctl

import (
	"fmt"
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

// sessionRef is a concrete view of a live session resolved by code. It is
// constructed only from a persisted session that passed the active lookup
// filter; `active` is additionally gated on the session's expiry so a
// non-expiring row cannot outlive its lease.
type sessionRef struct {
	apiKeyID string
	origin   string
	active   bool
}

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
// classification stands: relay_only is unsupported (410). Password-protected
// and non-Immich shares stay unsupported in both states (Task 27 owns their
// restoration). Status classification does not depend on live transport
// readiness (epoch, agent connectivity, open-signal, probe, presence) —
// those are §9.1 selection terms evaluated later, per navigation.
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

// sessionByCode resolves a session by its share code using a concrete lookup:
// the filter already requires is_active = true (so soft-deleted rows never
// match), and the live expiry is re-checked here so an expired session is not
// treated as active. Ownership is the api_key_id stored on the session row.
func (c *Controller) sessionByCode(code string) (*sessionRef, error) {
	recs, err := c.app.FindRecordsByFilter("sessions", "code = {:code} && is_active = true", "", 1, 0, map[string]any{"code": code})
	if err != nil || len(recs) == 0 {
		return nil, err
	}
	rec := recs[0]
	s := &sessionRef{apiKeyID: rec.GetString("api_key_id"), origin: rec.GetString("origin"), active: true}
	if exp := rec.GetDateTime("expires_at"); !exp.IsZero() && exp.Time().Before(time.Now()) {
		s.active = false
	}
	return s, nil
}

// Redirect is the LEGACY Phase 3 direct-connect flow: open_signal → open_ack
// → reachability probe → 302 to the agent's direct origin. It is retired from
// the canonical routes as of Task 20 (route selection, §9.3, owns GET
// /s/{code} and /share/{code}; the legacy server-side direct 302 is never
// restored in any flag state). It is retained ONLY because the Phase 3
// end-to-end test (internal/handler/e2e_test.go) still exercises this exact
// flow against its own route wiring; Task 21's bounded prepare-route flow
// subsumes the open-signal/probe machinery with a four-second budget, after
// which this method (and its probe lease constant) should be deleted.
func (c *Controller) Redirect(w http.ResponseWriter, r *http.Request, code string) error {
	sess, err := c.sessionByCode(code)
	if err != nil || sess == nil || !sess.active {
		return c.unavailable(w)
	}
	apiKeyID, origin := sess.apiKeyID, sess.origin
	if origin == "" {
		return c.unavailable(w)
	}
	if !c.epochReady(apiKeyID) || !c.hub.AgentConnected(apiKeyID) {
		return c.unavailable(w)
	}
	ack, err := c.emitOpenFn(r.Context(), apiKeyID, code, origin, 120*time.Second)
	if err != nil || ack.Status != "ok" {
		return c.unavailable(w)
	}
	if ack.GrantedPort < 1 || ack.GrantedPort > 65535 {
		return c.unavailable(w)
	}
	if err := c.probeFn(r.Context(), origin, code, apiKeyID, ack); err != nil {
		return c.unavailable(w)
	}
	loc := "https://" + origin
	if ack.GrantedPort != 443 {
		loc += fmt.Sprintf(":%d", ack.GrantedPort)
	}
	loc += "/s/" + code
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, loc, http.StatusFound)
	return nil
}

func (c *Controller) unavailable(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store") // §9.3: selection responses are never cacheable
	w.WriteHeader(http.StatusServiceUnavailable)
	w.Write([]byte(`{"error":"direct unavailable"}`))
	return nil
}
