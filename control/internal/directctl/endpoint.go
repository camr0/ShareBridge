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

	// Empty IP: no endpoint yet — do NOT save.
	if ip == "" {
		return
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
