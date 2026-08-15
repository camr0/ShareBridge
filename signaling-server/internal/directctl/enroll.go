package directctl

import (
	"context"

	"github.com/coder/websocket"
)

// HandleHello enrolls an agent connection: it ensures the agent record exists,
// installs a fresh epoch for this connection, and replies "enrolled" with the
// agent's namespace. The epoch is connection-local, so a replacement socket
// starts un-ready.
func (c *Controller) HandleHello(ctx context.Context, conn *websocket.Conn, apiKeyID, accountID, agentID string) {
	rec, _, err := LoadOrCreateAgent(c.app, apiKeyID)
	if err != nil {
		c.sendFn(ctx, conn, map[string]string{"type": "error", "message": "enrollment failed"})
		return
	}
	c.epochMu.Lock()
	c.epochs[apiKeyID] = &epochState{agentID: agentID, namespace: rec.GetString("namespace"), conn: conn}
	c.epochMu.Unlock()
	c.sendFn(ctx, conn, map[string]string{"type": "enrolled", "namespace": rec.GetString("namespace")})
}

// HandleCSRSubmit issues a certificate for the agent's namespace and returns
// the chain as cert_issue.
func (c *Controller) HandleCSRSubmit(ctx context.Context, conn *websocket.Conn, apiKeyID, csrPEM string) {
	rec, _, err := LoadOrCreateAgent(c.app, apiKeyID)
	if err != nil {
		c.sendFn(ctx, conn, map[string]string{"type": "cert_error", "reason": "lookup failed"})
		return
	}
	namespace := rec.GetString("namespace")
	// Bound the request size (spec §4f).
	if len(csrPEM) > 64*1024 {
		c.sendFn(ctx, conn, map[string]string{"type": "cert_error", "reason": "csr too large"})
		return
	}
	chain, err := c.coord.Issue(ctx, []byte(csrPEM), namespace, apiKeyID)
	if err != nil {
		c.sendFn(ctx, conn, map[string]string{"type": "cert_error", "reason": "issuance failed"})
		return
	}
	c.sendFn(ctx, conn, map[string]string{"type": "cert_issue", "chain_pem": string(chain)})
}

// HandleTLSReady records a successfully installed leaf certificate. It only
// advances readiness for the CURRENT connection epoch; a stale TLS report from
// an old socket is ignored. notAfter is derived from OUR cached chain, never
// the agent's string.
func (c *Controller) HandleTLSReady(ctx context.Context, conn *websocket.Conn, apiKeyID, fingerprint, notAfter string) {
	chain, na, ok := c.coord.ChainByLeaf(apiKeyID, fingerprint)
	if !ok {
		c.sendFn(ctx, conn, map[string]string{"type": "cert_error", "reason": "unknown fingerprint"})
		return
	}
	rec, _, err := LoadOrCreateAgent(c.app, apiKeyID)
	if err != nil {
		return
	}
	if err := SaveCertReady(c.app, rec, fingerprint, na); err != nil {
		c.sendFn(ctx, conn, map[string]string{"type": "cert_error", "reason": "persist failed"})
		return
	}
	// If the agent is behind the latest chain, re-deliver the latest.
	latest, latestFP, _, ok2 := c.coord.LatestChain(apiKeyID)
	if ok2 && fingerprint != latestFP {
		c.sendFn(ctx, conn, map[string]string{"type": "cert_issue", "chain_pem": string(latest)})
	}
	_ = chain

	c.epochMu.Lock()
	e := c.epochs[apiKeyID]
	if e != nil && e.conn == conn {
		e.tlsReady = true
		c.maybeReadyLocked(conn, apiKeyID, e)
	}
	c.epochMu.Unlock()
}

// HandleTLSError is a no-op beyond logging at this layer; the agent retries
// with backoff.
func (c *Controller) HandleTLSError(ctx context.Context, apiKeyID, reason string) {
	// No-op beyond logging at this layer; the agent retries with backoff.
}

// maybeReadyLocked emits enrollment_ready once both TLS and DDNS succeeded in
// the CURRENT epoch. Caller holds epochMu.
func (c *Controller) maybeReadyLocked(conn *websocket.Conn, apiKeyID string, e *epochState) {
	if e.tlsReady && e.ddnsReady && !e.ready {
		e.ready = true
		c.sendFn(context.Background(), conn, map[string]string{"type": "enrollment_ready"})
	}
}
