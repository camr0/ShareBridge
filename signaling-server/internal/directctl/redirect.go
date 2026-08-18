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
// (§8). It resolves a share code — including inactive tombstones — to its
// persisted session record and the HTTP status the caller must return:
//   - http.StatusFound (302): active gallery share; the caller performs the
//     direct redirect to the agent's origin.
//   - http.StatusGone (410): expired, or unsupported (relay-only/WebDAV/
//     protected).
//   - http.StatusNotFound (404): revoked, unknown, or an inactive row whose
//     inactive_reason is missing/unrecognized (fails safe).
//
// Status classification does not depend on live transport readiness (epoch,
// agent connectivity, open-signal, probe) — those gates still govern the direct
// redirect and can yield 503, per the Phase 2 runtime-flow rules.
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
		// Active but of an unsupported type → gone.
		if !isSupportedGallerySession(rec) {
			return rec, http.StatusGone
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
// password-protected.
func isSupportedGallerySession(rec *core.Record) bool {
	return rec.GetString("share_type") == "immich" &&
		!rec.GetBool("relay_only") &&
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

// Redirect handles GET /s/<code> by 302-ing the caller to the agent's direct
// HTTPS origin, gated on a live session + epoch readiness + agent connectivity
// + a successful open-signal + reachability probe. The response is never
// cached so a stale redirect cannot be replayed after the session lapses.
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
	w.WriteHeader(http.StatusServiceUnavailable)
	w.Write([]byte(`{"error":"direct unavailable"}`))
	return nil
}
