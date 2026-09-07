package directctl

import (
	"context"
	"time"

	"github.com/coder/websocket"
)

// HandleReportEndpoint tracks the agent's public endpoint (IP + port) and
// provisions the direct wildcard DNS record when the IP changes. Direct
// endpoint/DDNS is an OPTIONAL live capability (§7.1): it feeds direct-route
// preparation and no longer participates in baseline enrollment readiness —
// a DDNS failure downgrades direct availability only, never enrollment or
// share registration. endpoint_ip is saved ONLY after DDNS succeeds, so a
// failed update is retried on the next report. conn is verified against the
// current epoch so a fenced socket cannot drive DDNS.
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
	if status == "close_failed" && port == 0 {
		return
	}
	if status != "" && status != "close_failed" {
		return // protocol error
	}

	rec, _, err := LoadOrCreateAgent(c.app, apiKeyID)
	if err != nil {
		return
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

	rec.Set("endpoint_ip", ip)
	rec.Set("endpoint_port", port)
	rec.Set("last_report_at", time.Now())
	_ = c.app.Save(rec)
}
