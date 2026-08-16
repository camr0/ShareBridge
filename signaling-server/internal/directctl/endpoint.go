package directctl

import (
	"context"
)

// HandleReportEndpoint marks the current epoch's DDNS leg complete once DDNS
// succeeds, then re-evaluates readiness.
//
// NOTE: this is a MINIMAL implementation for Task 10 (it only needs to drive
// the DDNS leg of the readiness gate). Task 11 fleshes out endpoint_ip/port
// persistence, the port/status invariant validation, last_report_at, and
// "same-IP already provisioned" short-circuiting.
func (c *Controller) HandleReportEndpoint(ctx context.Context, apiKeyID, ip string, port int, status string) {
	if ip == "" {
		return
	}
	rec, _, err := LoadOrCreateAgent(c.app, apiKeyID)
	if err != nil {
		return
	}
	ns := rec.GetString("namespace")
	if _, err := c.ddnsFn(ctx, "*."+ns+"."+c.cfg.BaseDomain, ip, 60); err != nil {
		// DDNS failed -> leave the epoch's ddnsReady false; the next report retries.
		return
	}
	c.epochMu.Lock()
	if e := c.epochs[apiKeyID]; e != nil {
		e.ddnsReady = true
		c.maybeReadyLocked(e.conn, apiKeyID, e)
	}
	c.epochMu.Unlock()
}
