package directctl

// Task 20 live route selection (plan Task 20; spec §§6.1, 9.1–9.4, 19).
//
// SelectRoute is the §9.3 canonical-resolution step for lifecycle-active
// sessions (ResolveForRedirect stays the 404/410/Found lifecycle owner). It
// implements the §9.1 algorithm without measure-and-prefer: every decision is
// a boolean safety/availability predicate over live control-plane state —
// never a speed score, never a timing measurement, never a persisted status
// snapshot. The only performance-sensitive primitive in the system (the
// direct reachability probe) is NEVER invoked here (§9.2), and relay
// selection never signals the agent or mutates direct state.
//
//	Matrix (§9.1, for lifecycle-active sessions reaching SelectRoute):
//
//	                     ┌─────────────────┬──────────────────────────────┐
//	                     │ relay present?  │ RelaySelectionEnabled        │
//	relayOnly            │ yes → 302 relay │ flag off ⇒ lifecycle already │
//	  (never direct)     │ no  → 503       │ 410'd it (never reaches here)│
//	──────────────────────────────────────────────────────────────────────
//	direct preparable    │ 200 no-store interstitial (either flag value;   │
//	  (eligible, or only │  Task 22 owns the page; Task 21 owns prepare)   │
//	  STUN missing/stale)│                                                 │
//	──────────────────────────────────────────────────────────────────────
//	hard direct-         │ flag on + relay → 302 relay                     │
//	  ineligible         │ otherwise → 503 offline (never the legacy       │
//	  (mismatch, not-    │ direct 302 — it is retired in every flag state) │
//	  public, WS down)   │                                                 │
//
// Off-mode end-state (Task 19 ruling, carry-forward d): STUN "off" IS
// rollback mode. With the §10.1 listener unwired there is never an
// observation, so CurrentDirectMatch fails closed (stun_timeout) and direct
// candidates fall into the "only STUN freshness missing" interstitial
// category; the futile inline DDNS/probe work in HandleReportEndpoint/Probe
// cannot arise (their §10.3 gates never authorize without an observation),
// so no suppression is needed here — noted for Task 44 anyway.
//
// Origins (§9.3): both canonical origins are constructed ONLY from the
// persisted session row (sessions.origin via relayctl.RelayOriginFromDirect)
// — never from agent input, never from request input. A persisted origin
// that is not a derivable direct origin fails closed to 503.

import (
	"io"
	"math"
	"net/http"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"sharebridge/control/internal/relayctl"
)

// DirectReasonAgentOffline extends the §12 bounded diagnostic enum (declared
// in directpredicate.go) with the selection-time reason for "the agent has no
// live control WebSocket / ready connection epoch": direct preparation is
// categorically unpreparable (no open_signal carrier), which is more than
// STUN freshness missing. Selection computes it in memory only — canonical
// navigation is a hot read path and never persists diagnostics (§12 fields
// stay write-only audit of the gated flows).
const DirectReasonAgentOffline DirectStatusReason = "agent_offline"

// routeFacts is one coherent read of everything selection needs to know
// about the session's agent: the DDNS-verified direct endpoint fact, the
// relay assignment join key, and the route publisher's revision at read
// time. Produced ONLY by readRouteFacts.
type routeFacts struct {
	apiKeyID      string
	agentRecordID string
	endpointIP    string
	relayPort     int
	generation    uint64
	routeRevision uint64
}

// assignable reports whether the agent carries a relay assignment that could
// ever join a route (mirrors the publisher's identityFromJoin rules: port in
// range, generation a valid non-negative int64).
func (f routeFacts) assignable() bool {
	return f.agentRecordID != "" &&
		f.relayPort >= 1 && f.relayPort <= 65535 &&
		f.generation <= math.MaxInt64
}

// readRouteFacts performs the Task 15 read-then-check contract's single
// coherent read: ONE database read of the agents row (endpoint, relay port,
// generation) followed by ONE atomic read of the route publisher's current
// revision — in that order, so the recorded revision is at least as new as
// the row read. It never creates an agent row (selection is a read path;
// LoadOrCreateAgent would mutate) and holds no locks. The second return is
// false when there is no agent row (nothing is routable).
func (c *Controller) readRouteFacts(apiKeyID string) (routeFacts, bool) {
	facts := routeFacts{apiKeyID: apiKeyID}
	if apiKeyID == "" {
		return facts, false
	}
	recs, err := c.app.FindRecordsByFilter("agents", "api_key_id = {:k}", "", 1, 0, map[string]any{"k": apiKeyID})
	if err != nil || len(recs) == 0 {
		return facts, false
	}
	rec := recs[0]
	facts.agentRecordID = rec.Id
	facts.endpointIP = rec.GetString("endpoint_ip")
	facts.relayPort = rec.GetInt("relay_port")
	if gen := rec.GetInt("relay_generation"); gen >= 0 {
		facts.generation = uint64(gen)
	}
	// Revision AFTER the row read (see contract above): any published delta
	// after this read makes the Available revision join fail, which the
	// caller answers with exactly one re-read + retry.
	if c.routes != nil {
		facts.routeRevision = c.routes.CurrentRevision()
	}
	return facts, true
}

// relaySelectable is the §9.2 relay term: "check the gateway presence lease".
// Read-then-check-then-retry contract (Task 15 carry-forward, bind here):
//
//	lease, revision := readRouteFacts()        // ONE coherent read
//	if RelayAvailable(agent, port, gen, revision, now) → true
//	lease, revision := readRouteFacts()        // re-read EXACTLY ONCE
//	if RelayAvailable(agent, port, gen, revision, now) → true
//	else → false
//
// Available can be false because the route revision moved between our row
// read and the predicate (re-registration, claim, revoke, reassignment) —
// the predicate is deliberately fail-closed there and the caller's single
// re-read + retry is the documented self-heal (§15.7). No locks are held
// across either predicate call. now is the caller's single clock reading
// for the first attempt; the retry takes a fresh reading (the re-read may
// land after lease expiry).
func (c *Controller) relaySelectable(facts routeFacts, now time.Time) bool {
	if !facts.assignable() {
		return false
	}
	if c.RelayAvailable(facts.agentRecordID, facts.relayPort, facts.generation, facts.routeRevision, now) {
		return true
	}
	retried, ok := c.readRouteFacts(facts.apiKeyID)
	if !ok || !retried.assignable() {
		return false
	}
	return c.RelayAvailable(retried.agentRecordID, retried.relayPort, retried.generation, retried.routeRevision, c.nowFn())
}

// directSelectionTerm evaluates the §9.1 live direct predicate at selection
// time — a boolean safety/availability term, never a speed score — against
// the single clock reading `now` and the already-read route facts. Outcome
// mapping (§10.3 policy, Task 19 machinery):
//
//   - no live WebSocket / ready epoch → agent_offline: direct preparation
//     has no open_signal carrier, which is categorically more than STUN
//     freshness missing. (Carry-forward e ruling: the enum usage is extended
//     here, at the selection site, because this is where the case actually
//     surfaces; HandleReportEndpoint's empty-IP early return deliberately
//     persists nothing.)
//   - no fresh observation + no published endpoint → no_mapper: the
//     "agent has no direct endpoint at all" case surfaces HERE at selection
//     (it never surfaces in HandleReportEndpoint, whose empty-IP path
//     returns before diagnostics). Treated as preparable-unknown: Task 21's
//     bounded preparation re-evaluates every live input and can still map a
//     port inline, so the share gets the interstitial rather than a relay
//     bounce, and lands on relay within the four-second budget if mapping
//     is impossible.
//   - no fresh observation + published endpoint → stun_timeout: the §9.1
//     "only STUN freshness is missing/stale" interstitial category.
//   - fresh but not-public observation → stun_not_public: §10.3 never
//     qualifies CGNAT/private/reserved egress for direct regardless of any
//     published endpoint; a later observation succeeding is the only path
//     back (§10.3) — hard-ineligible now.
//   - fresh public observation vs the published endpoint: exact IPv4 match
//     → eligible (interstitial); mismatch → hard-ineligible (the published
//     direct record is contradicted by the observed egress — exactly the
//     §10.3 mismatched-egress case, so the browser must not be sent at the
//     possibly-stale direct origin).
//
// The persisted direct_status/direct_status_reason diagnostics are NEVER
// consulted (§12, §15.7): this is a pure live recomputation, and it never
// persists anything either.
func (c *Controller) directSelectionTerm(facts routeFacts, now time.Time) DirectMatch {
	off := DirectMatch{Matched: false, Status: DirectStatusRelayFallback}
	// Order matters and encodes the rulings: the WS fact is the strongest
	// (agent_offline outranks everything), then the no-published-endpoint
	// no_mapper case (carry-forward e: it surfaces here, at selection), then
	// the §10.3 STUN policy against the published endpoint.
	if !c.epochReady(facts.apiKeyID) || !c.hub.AgentConnected(facts.apiKeyID) {
		off.Reason = DirectReasonAgentOffline
		return off
	}
	if facts.endpointIP == "" {
		off.Reason = DirectReasonNoMapper
		return off
	}
	observation, fresh := c.CurrentSTUNObservation(facts.apiKeyID, now)
	if !fresh {
		off.Reason = DirectReasonSTUNTimeout
		return off
	}
	if !observation.PublicIPv4 {
		off.Reason = DirectReasonSTUNNotPublic
		return off
	}
	return evaluateDirectSTUNMatch(observation, true, facts.endpointIP)
}

// directPreparable reports whether the §9.1 flow serves the interstitial for
// a direct candidate: confirmed eligible, or the ONLY unknown is recoverable
// inline by Task 21's bounded preparation (STUN freshness, or a mapping that
// may still succeed). Confirmed contradictions (mismatch, non-public egress,
// offline agent) are not preparable — the §9.1 relay-or-503 arm applies.
func directPreparable(term DirectMatch) bool {
	return term.Matched ||
		term.Reason == DirectReasonSTUNTimeout ||
		term.Reason == DirectReasonNoMapper
}

// SelectRoute is the §9.3 canonical-resolution step for a lifecycle-active
// session (the caller invokes it exactly when ResolveForRedirect returned
// StatusFound, passing that session record). It never answers 404/410 —
// lifecycle is ResolveForRedirect's alone — and it never performs the legacy
// Phase 3 direct 302. sess must be the persisted session record; only its
// persisted api_key_id/origin/relay_only fields are read.
func (c *Controller) SelectRoute(w http.ResponseWriter, r *http.Request, sess *core.Record, code string) error {
	if sess == nil || code == "" {
		return c.unavailable(w)
	}
	apiKeyID := sess.GetString("api_key_id")
	origin := sess.GetString("origin")
	now := c.nowFn() // single clock reading for this navigation decision

	facts, haveFacts := c.readRouteFacts(apiKeyID)

	// §6.1/§9.1 relayOnly branch: relay presence is the ONLY term (§9.2 —
	// gateway-authoritative; the agent WebSocket is irrelevant to relay
	// serving, §15.4). No direct term is even computed, so nothing can
	// select, prepare, probe, or navigate to the direct origin.
	if c.cfg.RelaySelectionEnabled && sess.GetBool("relay_only") {
		if haveFacts && c.relaySelectable(facts, now) {
			return c.serveRelayRedirect(w, r, origin, code)
		}
		return c.unavailable(w)
	}

	term := c.directSelectionTerm(facts, now)
	if directPreparable(term) {
		// Interstitial for non-relayOnly direct candidates (both flag
		// states — rollback mode keeps the new direct flow, §20 rollout).
		return c.serveInterstitial(w, r)
	}

	// Hard direct-ineligible → relay if selection is enabled and the
	// presence lease backs it, else the offline page. Never a direct 302.
	if c.cfg.RelaySelectionEnabled && haveFacts && c.relaySelectable(facts, now) {
		return c.serveRelayRedirect(w, r, origin, code)
	}
	return c.unavailable(w)
}

// relayURL constructs the §6 relay origin deterministically from the
// PERSISTED session origin (never agent or request input): the relay label
// is inserted before the namespace, the browser terminates TLS at the
// gateway's public 443 (passthrough — the gateway has no TLS acceptor,
// §18.4), and the path is the canonical share path. A persisted origin that
// is not a direct origin is an error: the caller fails closed.
func relayURL(origin, code string) (string, error) {
	relayOrigin, err := relayctl.RelayOriginFromDirect(origin)
	if err != nil {
		return "", err
	}
	return "https://" + relayOrigin + "/s/" + code, nil
}

// serveRelayRedirect answers 302 to the control-constructed relay URL,
// never cached (a stale relay redirect must not be replayable after the
// session or lease lapses). A non-derivable origin fails closed to 503.
func (c *Controller) serveRelayRedirect(w http.ResponseWriter, r *http.Request, origin, code string) error {
	loc, err := relayURL(origin, code)
	if err != nil {
		// Not a direct origin ⇒ no derivable relay origin. Never echo the
		// malformed value; 503 (§9.3 "no usable transport").
		return c.unavailable(w)
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, loc, http.StatusFound)
	return nil
}

// serveInterstitial answers the §9.3 200 no-store interstitial. Task 20
// fixes ONLY the response contract (200, no-store, no request reflection,
// no content embedding); Task 22 replaces the body with the real
// progress/relay/noscript page and its CSP. Nothing about this response is
// cacheable and nothing reflects request input.
func (c *Controller) serveInterstitial(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "<!doctype html><html><head><title>ShareBridge</title></head>"+
		"<body><p>Preparing your share route…</p></body></html>\n")
	return nil
}
