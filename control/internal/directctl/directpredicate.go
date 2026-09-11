package directctl

// Direct-eligibility predicate and bounded diagnostics (plan Task 19, spec
// §§7.1, 10.3, 11.2, 12, 15.7).
//
// Policy (§10.3): a fresh current-epoch STUN observation that is classified
// public IPv4 and EXACTLY equals (IPv4) every required surface — the
// report_endpoint IP and/or the open_ack public IP — is the ONLY thing that
// authorizes a direct DDNS update or a reachability probe. Mismatch, missing
// or stale observation, and a private/reserved/CGNAT observation all stop the
// flow BEFORE the DDNS update and BEFORE the probe and select relay fallback
// until a later observation succeeds. This closes the Phase 2 probe gap
// (SSRF/scanner) while a relay tunnel can never manufacture direct endpoint
// state (§11.2: it does not report a fake public endpoint or port).
//
// Diagnostic discipline (§12, §15.7 — same pattern as Task 15's
// relay_last_seen_at): every evaluation persists `direct_status` (closed enum
// unknown|eligible|relay_fallback) and `direct_status_reason` (closed enum
// below) plus the driving observation (`stun_observed_ip`/`stun_observed_at`)
// to the agents row. These fields are write-only audit/UI diagnostics: the
// code below NEVER reads them back, and route selection (Task 20) always
// recomputes the live predicate — a persisted value is never authority across
// a request or restart. Reason codes are a fixed enum, never free-form text;
// no secret material is ever included.
//
// Gating is active exactly when STUN challenge scheduling is wired
// (EnableSTUN — the Phase 4a control topology always wires it); with no
// listener there is no observation to match, and the Phase 3 behavior is
// preserved unchanged (the denylist in probe.go still applies there).

import (
	"net/netip"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

// DirectStatus is the closed §12 diagnostic enum for agents.direct_status.
// The agent-level fallback value is deliberately named relay_fallback so it
// can never collide with the per-share sessions.relay_only policy flag.
type DirectStatus string

const (
	// DirectStatusUnknown is the absent-evaluation default (never persisted
	// by this package; it is the migration's zero value).
	DirectStatusUnknown DirectStatus = "unknown"
	// DirectStatusEligible records that the last evaluation found the agent
	// direct-eligible. It is a diagnostic snapshot only and never authorizes
	// anything on its own (§12: selection recomputes the live predicate).
	DirectStatusEligible DirectStatus = "eligible"
	// DirectStatusRelayFallback records that the last evaluation sent the
	// agent to relay fallback.
	DirectStatusRelayFallback DirectStatus = "relay_fallback"
)

// DirectStatusReason is the fixed, closed enum of §12 diagnostic reason codes
// persisted to agents.direct_status_reason. Never free-form text; extend only
// with new bounded codes.
type DirectStatusReason string

const (
	// DirectReasonNone accompanies DirectStatusEligible (nothing failed).
	DirectReasonNone DirectStatusReason = ""
	// DirectReasonNoMapper: no direct endpoint exists at all (the agent has
	// no port mapper and never reported a public IP).
	DirectReasonNoMapper DirectStatusReason = "no_mapper"
	// DirectReasonSTUNTimeout: no fresh current-epoch observation (missing,
	// stale, or the challenge never answered).
	DirectReasonSTUNTimeout DirectStatusReason = "stun_timeout"
	// DirectReasonSTUNMismatch: a fresh observation exists but disagrees
	// (exact IPv4 comparison) with a required surface.
	DirectReasonSTUNMismatch DirectStatusReason = "stun_mismatch"
	// DirectReasonSTUNNotPublic: the fresh observation is a
	// private/reserved/CGNAT (or non-IPv4) address, which §10.3 never
	// qualifies for direct regardless of agreement.
	DirectReasonSTUNNotPublic DirectStatusReason = "stun_not_public"
	// DirectReasonProbeFailed: the STUN-gated reachability probe itself
	// failed (non-200 or wrong nonce).
	DirectReasonProbeFailed DirectStatusReason = "probe_failed"
	// DirectReasonEndpointCloseFailed: the agent reported (report_endpoint status
	// "close_failed") that it exhausted its on-demand mapping-deletion retries,
	// so an owned mapping may still be live on the router. Selection fails
	// closed to relay until a later endpoint report clears the escalation. It is
	// a hard-ineligible (not preparable-unknown) reason.
	DirectReasonEndpointCloseFailed DirectStatusReason = "endpoint_close_failed"
)

// DirectMatch is one §10.3 policy evaluation: whether the observation
// authorizes direct DDNS/probing for the required surfaces, plus the
// diagnostic status/reason to persist.
type DirectMatch struct {
	Matched bool
	Status  DirectStatus
	Reason  DirectStatusReason
}

// exactIPv4Match reports whether reported is a valid dotted-quad IPv4 literal
// exactly equal to the observed address (both compared as 4-byte addresses;
// IPv6 and IPv4-mapped forms never match — §10.3: the direct path is IPv4).
// Parsing is strict: surrounding whitespace or CIDR notation is a mismatch.
func exactIPv4Match(reported string, observed netip.Addr) bool {
	parsed, err := netip.ParseAddr(reported)
	if err != nil {
		return false
	}
	if !parsed.Is4() {
		return false // covers IPv6 and IPv4-mapped forms
	}
	return observed.IsValid() && observed.Unmap() == parsed
}

// evaluateDirectSTUNMatch applies the §10.3 match policy as a pure function:
// the observation must be fresh (current-epoch freshness checked by the
// caller against one clock reading), public-classified, and exactly equal
// (IPv4) to every required address — the report_endpoint IP and the
// open_ack public IP, wherever a surface is being gated. The returned outcome
// is also the diagnostic to persist.
func evaluateDirectSTUNMatch(observation STUNObservation, observationFresh bool, requiredIPs ...string) DirectMatch {
	switch {
	case !observationFresh:
		return DirectMatch{Matched: false, Status: DirectStatusRelayFallback, Reason: DirectReasonSTUNTimeout}
	case !observation.PublicIPv4:
		return DirectMatch{Matched: false, Status: DirectStatusRelayFallback, Reason: DirectReasonSTUNNotPublic}
	}
	for _, required := range requiredIPs {
		if !exactIPv4Match(required, observation.IP) {
			return DirectMatch{Matched: false, Status: DirectStatusRelayFallback, Reason: DirectReasonSTUNMismatch}
		}
	}
	return DirectMatch{Matched: true, Status: DirectStatusEligible, Reason: DirectReasonNone}
}

// recordDirectDiagnostics persists one evaluation's §12 diagnostics onto the
// agent record and saves it. Write-only by contract (§15.7): nothing in this
// package reads direct_status/direct_status_reason back, and the stun
// observation fields are recorded for freshness/audit UI only. The save is
// best-effort at the caller's discretion (this variant is used when a record
// is already loaded; diagnostics failures never block the gated flow).
func (c *Controller) recordDirectDiagnostics(rec *core.Record, observation STUNObservation, observationFresh bool, outcome DirectMatch, now time.Time) {
	rec.Set("direct_status", string(outcome.Status))
	rec.Set("direct_status_reason", string(outcome.Reason))
	if observationFresh {
		rec.Set("stun_observed_ip", observation.IP.Unmap().String())
		rec.Set("stun_observed_at", now)
	}
	_ = c.app.Save(rec)
}

// persistDirectDiagnostics is recordDirectDiagnostics for callers that hold
// no agent record (the probe path): it loads (or creates) the row and records
// the outcome. Diagnostics are best-effort — a load failure silently skips
// the write and never blocks the gated flow.
func (c *Controller) persistDirectDiagnostics(apiKeyID string, observation STUNObservation, observationFresh bool, outcome DirectMatch, now time.Time) {
	rec, _, err := LoadOrCreateAgent(c.app, apiKeyID)
	if err != nil {
		return
	}
	c.recordDirectDiagnostics(rec, observation, observationFresh, outcome, now)
}
