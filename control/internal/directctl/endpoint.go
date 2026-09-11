package directctl

import (
	"context"
	"log"
	"time"

	"github.com/coder/websocket"
)

// endpointStatusCloseFailed is the wire status the agent reports when it
// exhausted its on-demand mapping-deletion retries (§13.4 escalation). It is
// persisted to agents.endpoint_status and consumed by route selection, which
// fails closed to relay while it is set.
const endpointStatusCloseFailed = "close_failed"

// HandleReportEndpoint tracks the agent's public endpoint (IP + port) and
// provisions the direct wildcard DNS record when the IP changes. Direct
// endpoint/DDNS is an OPTIONAL live capability (§7.1): it feeds direct-route
// preparation and no longer participates in baseline enrollment readiness —
// a DDNS failure downgrades direct availability only, never enrollment or
// share registration. endpoint_ip is saved ONLY after DDNS succeeds, so a
// failed update is retried on the next report. conn is verified against the
// current epoch so a fenced socket cannot drive DDNS.
//
// A status "close_failed" report is the agent's §13.4 escalation: it exhausted
// its on-demand mapping-deletion retries, so an owned mapping may still be live
// on the router. It is persisted durably (agents.endpoint_status +
// endpoint_status_at) and consumed by route selection, which fails closed to
// relay until a later report clears it — the escalation is never dropped on the
// floor.
//
// §10.3 gate (plan Task 19): when STUN challenge scheduling is wired, the
// direct DDNS update runs ONLY after a fresh current-epoch observation that
// is public-classified and EXACTLY equals (IPv4) the reported IP. A
// mismatch, a non-public (private/reserved/CGNAT) observation, or a missing
// /stale observation stops BEFORE the DDNS update (§11.2: a relay tunnel
// never manufactures a direct endpoint this way) and persists its bounded
// diagnostic outcome (§12) — which nothing reads back as routing input
// (§15.7).
func (c *Controller) HandleReportEndpoint(ctx context.Context, conn *websocket.Conn, apiKeyID, ip string, port int, status string) {
	if !c.isCurrentEpoch(apiKeyID, conn) {
		return
	}
	// Wire invariant: status "close_failed" requires a nonzero port.
	if status == endpointStatusCloseFailed && port == 0 {
		return
	}
	if status != "" && status != endpointStatusCloseFailed {
		return // protocol error
	}

	rec, _, err := LoadOrCreateAgent(c.app, apiKeyID)
	if err != nil {
		return
	}

	// Durable escalation consumer (M4 closeout batch 1, finding 3): a
	// close_failed report means the agent could not release an owned on-demand
	// mapping, so a mapping may still be live on the router. Persist the status
	// and its timestamp BEFORE the endpoint/DDNS work (so it survives a failed
	// DDNS update), log it for operators, and let direct selection fail closed
	// until a later report clears it.
	if status == endpointStatusCloseFailed {
		log.Printf("directctl: agent %s reports close_failed on endpoint port %d: an owned mapping may still be live; direct selection is suppressed until a later report", apiKeyID, port)
		rec.Set("endpoint_status", endpointStatusCloseFailed)
		rec.Set("endpoint_status_at", time.Now())
		_ = c.app.Save(rec)
	}

	prev := rec.GetString("endpoint_ip")

	// Empty IP: no endpoint yet — do NOT save (and nothing to evaluate).
	if ip == "" {
		return
	}

	// §10.3 STUN gate before any DDNS work. Single clock reading for the
	// whole evaluation (freshness + diagnostic stamp).
	if c.stunEnabled() {
		now := c.nowFn()
		observation, fresh := c.CurrentSTUNObservation(apiKeyID, now)
		outcome := evaluateDirectSTUNMatch(observation, fresh, ip)
		c.recordDirectDiagnostics(rec, observation, fresh, outcome, now)
		if !outcome.Matched {
			return // stop BEFORE the DDNS update (spy-proven in tests)
		}
	}

	// DDNS is only attempted on a changed IP; endpoint_ip is saved ONLY after
	// DDNS succeeds, so a failed update is retried on the next report.
	if ip != prev {
		ns := rec.GetString("namespace")
		if _, err := c.ddnsFn(ctx, "*."+ns+"."+c.cfg.BaseDomain, ip, 60); err != nil {
			// leave endpoint_ip unchanged → next report retries
			return
		}
	}

	if status != endpointStatusCloseFailed {
		// A successful report clears any close_failed escalation: the agent's
		// mapped state is once again known-good.
		rec.Set("endpoint_status", "")
	}
	rec.Set("endpoint_ip", ip)
	rec.Set("endpoint_port", port)
	rec.Set("last_report_at", time.Now())
	_ = c.app.Save(rec)
}
