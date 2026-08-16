package directctl

import (
	"fmt"
	"net/http"
	"time"
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
