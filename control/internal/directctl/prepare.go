package directctl

// Task 21 bounded prepare-route endpoint (plan Task 21; spec §§4.4, 9.3, 6.1,
// 9.2, 12).
//
// POST /api/shares/<code>/prepare-route is the interstitial's (Task 22)
// preparation call, made by the unauthenticated recipient who already holds
// the native share code (the existing bearer capability). Per request it:
//
//   - re-resolves lifecycle through ResolveForRedirect (the single 404/410/
//     Found lifecycle owner — revoked/unknown 404, expired/unsupported 410);
//   - re-evaluates the live direct predicate (the exact Task 20 selection
//     terms — never a persisted status snapshot, §7.1/§12);
//   - runs ONE four-second context (§4.4) encompassing the inline STUN
//     refresh, the open_signal/open_ack round trip (the mapping), and the
//     verified-tuple probe. The mapping and the STUN refresh overlap; the
//     public probe is strictly sequenced AFTER the STUN match (the Task 19
//     gate inside Probe re-checks the fresh match under one clock reading
//     before any packet leaves). Existing three-second component timeouts
//     (open-ack wait, probe client) remain but are cut short by this context
//     so they can never exceed the overall budget;
//   - answers with bounded no-store JSON whose URLs are constructed ONLY
//     from the persisted session row (§6 origin forms): the direct URL on
//     verified success (with the optional relay URL when selection is enabled
//     and the presence lease backs it), the relay URL alone on failure or
//     budget miss when relay is selectable, or the generic unavailable
//     response otherwise. No topology detail and no internal agent identifier
//     is ever exposed.
//
// A budget miss is §9.1 cold-open behavior: it selects the available relay
// for that navigation and persists NOTHING — no sticky direct-ineligible
// state, no diagnostics write, no probe. Request input (query, headers,
// body fields) is never consulted: there are no redirect/origin parameters,
// and both origins are control-constructed from persisted state. No timing
// measurement influences any decision (§9.1 measure-and-prefer is forbidden):
// the probe is a reachability verification, and its duration is never read.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	// prepareRouteBudget is the §4.4 ONE overall control-preparation budget:
	// a single four-second context covers the inline STUN refresh,
	// open_signal/open_ack, and the verified-tuple probe.
	prepareRouteBudget = 4 * time.Second

	// prepareDirectTimeoutMs is the directTimeoutMs value returned to the
	// interstitial (§9.3): the recipient-path budget the browser enforces on
	// its own GET /s/<code>/connect check.
	prepareDirectTimeoutMs = 4000

	// prepareMaxBodyBytes bounds the request body: empty or one small JSON
	// object is accepted; anything larger is rejected before parsing.
	prepareMaxBodyBytes = 8 << 10

	// directOpenLease is the open-signal lease the agent grants for the
	// preparation window (the same duration the retired Phase 3 flow used;
	// it is an agent-side lease length, not a control-side timeout).
	directOpenLease = 120 * time.Second
)

// directURL is the single §6 direct-URL builder: the PERSISTED session origin
// plus the agent-granted port, with port 443 (the TLS default) omitted. Every
// direct-URL construction uses this one helper (the §9.3 prepare response;
// formerly also the legacy Phase 3 302) so the 443-omission rule can never
// drift between call sites.
func directURL(origin string, port int, code string) string {
	if port == 443 {
		return "https://" + origin + "/s/" + code
	}
	return fmt.Sprintf("https://%s:%d/s/%s", origin, port, code)
}

// prepareResponse is the entire bounded response body (§9.3): a coarse
// status, control-derived URLs from the persisted session only, and the
// interstitial's direct-check budget. It never carries agent identifiers,
// endpoint IPs, STUN/selection internals, or any request-provided value.
type prepareResponse struct {
	Status          string `json:"status"`
	DirectURL       string `json:"direct_url,omitempty"`
	RelayURL        string `json:"relay_url,omitempty"`
	DirectTimeoutMs int    `json:"direct_timeout_ms,omitempty"`
}

// PrepareRoute answers the interstitial's bounded preparation request for
// code. See the package-file contract above.
func (c *Controller) PrepareRoute(w http.ResponseWriter, r *http.Request, code string) error {
	// POST only (§9.3): the preparation is a state-advancing action, not a
	// cacheable resource fetch.
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		return writePrepareError(w, http.StatusMethodNotAllowed, "method not allowed")
	}

	// Lifecycle re-resolution per request (§9.3): ResolveForRedirect stays
	// the single 404/410/Found lifecycle owner — revoked/unknown 404,
	// expired/unsupported 410 — and its Found classification already gates
	// the supported share types (relayOnly included only when selection is
	// enabled).
	sess, status := c.ResolveForRedirect(code)
	switch status {
	case http.StatusFound:
	case http.StatusGone:
		return writePrepareError(w, http.StatusGone, "share expired or unsupported")
	default:
		return writePrepareError(w, http.StatusNotFound, "session not found")
	}
	apiKeyID := sess.GetString("api_key_id")
	origin := sess.GetString("origin")

	// Bounded body: empty or one JSON object. Field values are deliberately
	// NEVER consulted — no redirect/origin input is accepted.
	if err := consumePrepareBody(w, r); err != nil {
		return err
	}

	// §6.1: a relayOnly share is never selected, prepared, probed, or
	// navigated to its direct origin. Answer from relay presence alone.
	if sess.GetBool("relay_only") {
		return c.prepareRelayFallback(w, apiKeyID, origin, code)
	}
	if origin == "" {
		return c.unavailable(w)
	}

	// Live direct-predicate re-check (§9.3): the same Task 20 selection terms
	// the interstitial arm used, recomputed now. Only a matched candidate or
	// a recoverable unknown (STUN freshness, a mapping that may still happen)
	// reaches the bounded preparation; hard contradictions (mismatch,
	// non-public egress, offline agent) go straight to relay-or-unavailable.
	now := c.nowFn()
	facts, _ := c.readRouteFacts(apiKeyID)
	if !directPreparable(c.directSelectionTerm(facts, now)) {
		return c.prepareRelayFallback(w, apiKeyID, origin, code)
	}

	// ONE four-second context (§4.4): a single deadline fixed here covers the
	// inline STUN refresh, the open/ack round trip, and the probe. Every
	// stage shares this one reading of the budget — none of them can push the
	// total past four seconds.
	ctx, cancel := context.WithTimeout(r.Context(), prepareRouteBudget)
	defer cancel()

	type openOutcome struct {
		ack OpenAck
		err error
	}
	// Mapping overlaps STUN (§4.4): the open_signal/open_ack round trip runs
	// concurrently with the inline observation refresh. The channel is
	// buffered, so the goroutine cannot leak even if this request returns
	// early.
	openCh := make(chan openOutcome, 1)
	go func() {
		ack, err := c.emitOpenFn(ctx, apiKeyID, code, origin, directOpenLease)
		openCh <- openOutcome{ack: ack, err: err}
	}()
	// Await ONE inline refresh inside the parent deadline (Task 18's
	// inline-once semantics). The returned observation is deliberately not
	// used as authority here: the probe's §10.3 gate re-evaluates the fresh
	// current-epoch match against the ack's public IP under its own single
	// clock reading (Task 19), so a race between refresh and probe cannot
	// authorize on stale facts.
	c.AwaitFreshObservation(ctx, apiKeyID)

	opened := <-openCh
	if ctx.Err() != nil {
		// Budget miss (§9.1): answer from relay presence for THIS navigation
		// and persist nothing — no probe (whose gate would write bounded
		// diagnostics), no verified tuple, no direct-ineligible state.
		return c.prepareRelayFallback(w, apiKeyID, origin, code)
	}
	if opened.err != nil || opened.ack.Status != "ok" ||
		opened.ack.GrantedPort < 1 || opened.ack.GrantedPort > 65535 {
		// The mapping failed or the agent refused the open.
		return c.prepareRelayFallback(w, apiKeyID, origin, code)
	}

	// Verified-tuple probe, strictly sequenced AFTER the STUN refresh has
	// concluded (§4.4: the public probe cannot start until the STUN match
	// succeeds — enforced by the Task 19 gate inside Probe).
	if err := c.probeFn(ctx, origin, code, apiKeyID, opened.ack); err != nil {
		return c.prepareRelayFallback(w, apiKeyID, origin, code)
	}

	// Success: the direct URL is constructed from the PERSISTED session
	// origin and the just-verified granted port (§6 form via the shared
	// directURL builder; port 443 is the default and omitted).
	resp := prepareResponse{
		Status:          "direct",
		DirectTimeoutMs: prepareDirectTimeoutMs,
		DirectURL:       directURL(origin, opened.ack.GrantedPort, code),
	}
	// The optional relay URL rides along only when selection is enabled and
	// the presence lease currently backs it (§9.3 "optional relay URL").
	if c.cfg.RelaySelectionEnabled {
		if relayLoc, err := relayURL(origin, code); err == nil && c.relaySelectableFor(apiKeyID) {
			resp.RelayURL = relayLoc
		}
	}
	return writePrepareJSON(w, http.StatusOK, resp)
}

// relaySelectableFor re-reads the agent's relay assignment facts and applies
// the §9.2 presence-lease term with a fresh single clock reading. The prepare
// paths use it (instead of the entry-time facts) because the earlier read may
// be stale by up to the whole preparation budget; the re-read is also the
// documented self-heal for a moved route revision (§15.7).
func (c *Controller) relaySelectableFor(apiKeyID string) bool {
	facts, ok := c.readRouteFacts(apiKeyID)
	if !ok {
		return false
	}
	return c.relaySelectable(facts, c.nowFn())
}

// prepareRelayFallback answers every non-success arm: the derived relay URL
// when selection is enabled and the presence lease backs it, the generic
// unavailable response otherwise. It exposes no topology detail and never
// persists anything.
func (c *Controller) prepareRelayFallback(w http.ResponseWriter, apiKeyID, origin, code string) error {
	if c.cfg.RelaySelectionEnabled {
		if loc, err := relayURL(origin, code); err == nil && c.relaySelectableFor(apiKeyID) {
			return writePrepareJSON(w, http.StatusOK, prepareResponse{Status: "relay", RelayURL: loc})
		}
	}
	return c.unavailable(w)
}

// consumePrepareBody enforces the bounded-body contract: at most
// prepareMaxBodyBytes, and only an empty body or a single JSON object. Body
// field values are never returned (there is nothing to consult — both origins
// are constructed from the persisted session row).
func consumePrepareBody(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, prepareMaxBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return writePrepareError(w, http.StatusRequestEntityTooLarge, "request body too large")
		}
		return writePrepareError(w, http.StatusBadRequest, "unreadable request body")
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil // empty body is a valid preparation request
	}
	var obj map[string]any
	if err := json.Unmarshal(trimmed, &obj); err != nil || obj == nil {
		// Malformed JSON, or JSON that is not an object (arrays, strings,
		// numbers, null).
		return writePrepareError(w, http.StatusBadRequest, "malformed request body")
	}
	return nil
}

// writePrepareJSON writes one bounded no-store JSON response (§9.3: the
// prepare response is never cacheable).
func writePrepareJSON(w http.ResponseWriter, status int, v prepareResponse) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
	return nil
}

// writePrepareError writes one bounded no-store JSON error with a static
// message (no internals, no request reflection).
func writePrepareError(w http.ResponseWriter, status int, msg string) error {
	body, _ := json.Marshal(map[string]string{"error": msg})
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
	return nil
}
