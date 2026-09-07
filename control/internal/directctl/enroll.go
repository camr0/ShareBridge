package directctl

import (
	"context"
	"log"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/pocketbase/pocketbase/core"
	"sharebridge/control/internal/relayctl"
	"sharebridge/control/internal/stun"
)

// relayWildcardTTL is the A-record TTL for the static relay content wildcard
// (zone A records use a 60-second TTL).
const relayWildcardTTL = 60

// relayNamespacePattern matches the namespaces GenerateNamespace issues
// ("sb" + 8 lowercase hex). The relay wildcard name composes only from a
// well-formed namespace of the authenticated agent row, so a drifted or
// agent-influenced value can never reach the DNS provider (§17.2: DNS targets
// come only from the authenticated agent row and operator config).
var relayNamespacePattern = regexp.MustCompile(`^sb[0-9a-f]{8}$`)

// relayWildcardName derives the §6 relay content wildcard for the agent's
// namespace: "*.relay.<namespace>.<base-domain>" — the name baked into the
// agent certificate's second SAN and the suffix of every derived relay origin.
func (c *Controller) relayWildcardName(namespace string) string {
	return "*.relay." + namespace + "." + c.cfg.BaseDomain
}

// validRelayGatewayIPv4 strictly parses the operator-supplied gateway address
// as a plain IPv4 literal. Absent, malformed, or IPv6-mapped values are
// rejected so a broken record can never be provisioned.
func validRelayGatewayIPv4(value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || strings.Contains(trimmed, ":") {
		return "", false
	}
	parsed := net.ParseIP(trimmed)
	if parsed == nil || parsed.To4() == nil {
		return "", false
	}
	return parsed.String(), true
}

// ensureRelayDNS idempotently provisions the §6 relay content wildcard for
// the agent's namespace and marks the CURRENT epoch relay-DNS-ready, which
// may complete baseline readiness (TLS + relay DNS) and emit enrollment_ready
// (§7.1). It never blocks enrollment: a failure only keeps readiness off, is
// logged, and is retried on the next hello/tls_ready of the epoch. The name
// composes exclusively from the authenticated agent record's namespace and
// the operator's configuration — never from agent messages.
func (c *Controller) ensureRelayDNS(ctx context.Context, apiKeyID string, rec *core.Record, e *epochState) {
	gatewayIPv4, valid := validRelayGatewayIPv4(c.cfg.RelayGatewayIPv4)
	if !valid {
		log.Printf("relay dns not provisioned for %s: RELAY_GATEWAY_IPV4 absent or invalid", apiKeyID)
		return
	}
	namespace := rec.GetString("namespace")
	if !relayNamespacePattern.MatchString(namespace) {
		log.Printf("relay dns not provisioned for %s: agent namespace %q malformed", apiKeyID, namespace)
		return
	}
	if !c.provisionRelayWildcard(ctx, namespace, gatewayIPv4) {
		return
	}
	c.epochMu.Lock()
	var shouldSend bool
	if cur := c.epochs[apiKeyID]; cur == e {
		e.relayDNSReady = true
		shouldSend = c.markReadyLocked(e)
	}
	c.epochMu.Unlock()
	if shouldSend {
		c.sendEnrollmentReady(apiKeyID, e.conn, rec, e)
	}
}

// provisionRelayWildcard ensures the namespace's wildcard A record points at
// gatewayIPv4, using a process-local cache so repeated enrollments of the same
// namespace are provider-free. Reports whether the record is (now) correct.
func (c *Controller) provisionRelayWildcard(ctx context.Context, namespace, gatewayIPv4 string) bool {
	c.relayDNSMu.Lock()
	cached, cachedOK := c.relayDNSProvisioned[namespace]
	c.relayDNSMu.Unlock()
	if cachedOK && cached == gatewayIPv4 {
		return true
	}
	wildcardName := c.relayWildcardName(namespace)
	if _, err := c.relayDNSFn(ctx, wildcardName, gatewayIPv4, relayWildcardTTL); err != nil {
		log.Printf("relay wildcard provisioning failed for %q: %v", wildcardName, err)
		return false
	}
	c.relayDNSMu.Lock()
	c.relayDNSProvisioned[namespace] = gatewayIPv4
	c.relayDNSMu.Unlock()
	return true
}

// relayEmitter carries the §4.5 tunnel policy and credential signer. It is
// installed on the controller with EnableRelay; nil (the default) disables
// relay_config emission so deployments without relay policy stay relay-free.
type relayEmitter struct {
	settings relayctl.Settings
	signer   *relayctl.Signer
}

// EnableRelay attaches the relay tunnel policy and credential signer. It must
// be called before the controller serves traffic; a zero settings or nil
// signer leaves relay disabled.
func (c *Controller) EnableRelay(settings relayctl.Settings, signer *relayctl.Signer) {
	if signer == nil {
		return
	}
	if err := settings.Validate(); err != nil {
		log.Printf("relay disabled: invalid policy: %v", err)
		return
	}
	c.relay = &relayEmitter{settings: settings, signer: signer}
}

// sendRelayConfig allocates (or reads the stable) relay assignment, mints a
// fresh epoch-bound credential, and sends the §11.1 relay_config message to
// the CURRENT epoch's connection. It runs only after baseline readiness (it
// is called right behind enrollment_ready) and silently skips: unconfigured
// relay, stale sockets, allocation/issuance failure, and it never logs
// credential material (§7.2).
func (c *Controller) sendRelayConfig(apiKeyID string, conn *websocket.Conn, rec *core.Record) {
	if c.relay == nil {
		return
	}
	if !c.isCurrentEpoch(apiKeyID, conn) {
		return // a fenced socket never receives tunnel credentials
	}
	assignment, err := relayctl.EnsureAssignment(c.app, rec, c.relay.settings.PortRange)
	if err != nil {
		log.Printf("relay assignment failed for %s: %v", apiKeyID, err)
		return
	}
	msg, err := relayctl.BuildRelayConfig(assignment, apiKeyID, c.relay.settings.GatewayAddr, c.relay.settings.GatewayPort, c.relay.signer)
	if err != nil {
		log.Printf("relay credential issue failed for %s: %v", apiKeyID, err)
		return
	}
	if err := c.sendFn(context.Background(), conn, msg); err != nil {
		log.Printf("relay_config send failed for %s: %v", apiKeyID, err)
	}
}

// HandleHello enrolls an agent connection: it ensures the agent record exists,
// installs a fresh epoch for this connection (a new monotonic epoch number;
// any prior epoch's STUN challenge state is torn down — reconnect invalidates
// the old observation, §10.2), idempotently provisions the §6 relay content
// wildcard for the agent's namespace (baseline readiness half), and replies
// "enrolled" with the agent's namespace. The epoch is connection-local, so a
// replacement socket starts un-ready.
func (c *Controller) HandleHello(ctx context.Context, conn *websocket.Conn, apiKeyID, accountID, agentID string) {
	rec, _, err := LoadOrCreateAgent(c.app, apiKeyID)
	if err != nil {
		c.sendFn(ctx, conn, map[string]string{"type": "error", "message": "enrollment failed"})
		return
	}
	c.epochMu.Lock()
	if old := c.epochs[apiKeyID]; old != nil {
		c.stunMu.Lock()
		c.stunTeardownLocked(old)
		c.stunMu.Unlock()
	}
	c.epochSeq++
	c.epochs[apiKeyID] = &epochState{agentID: agentID, namespace: rec.GetString("namespace"), conn: conn, epoch: stun.Epoch(c.epochSeq)}
	epoch := c.epochs[apiKeyID]
	c.epochMu.Unlock()
	// Provision relay DNS before the enrolled reply: the record is half of
	// baseline readiness (§7.1) and must exist even if TLS fails later. A
	// failure is retried at tls_ready and on the next enrollment.
	c.ensureRelayDNS(ctx, apiKeyID, rec, epoch)
	c.sendFn(ctx, conn, map[string]string{"type": "enrolled", "namespace": rec.GetString("namespace")})
}

// HandleCSRSubmit issues a certificate for the agent's namespace and returns
// the chain as cert_issue.
func (c *Controller) HandleCSRSubmit(ctx context.Context, conn *websocket.Conn, apiKeyID, csrPEM string) {
	if !c.isCurrentEpoch(apiKeyID, conn) {
		return
	}
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
// the agent's string. When the in-memory leaf index is empty (control restart
// or API-key rotation) the persisted agents.cert_fingerprint + cert_expires_at
// row authorizes the report instead.
func (c *Controller) HandleTLSReady(ctx context.Context, conn *websocket.Conn, apiKeyID, fingerprint, notAfter string) {
	if !c.isCurrentEpoch(apiKeyID, conn) {
		return
	}
	rec, _, err := LoadOrCreateAgent(c.app, apiKeyID)
	if err != nil {
		return
	}
	chain, na, ok := c.coord.ChainByLeaf(apiKeyID, fingerprint)
	if !ok {
		// Fall back to the persisted cert row: the agent record survives a
		// control restart and API-key rotation, so a valid installed cert is
		// still authoritative when the in-memory index is empty.
		persistedFP := rec.GetString("cert_fingerprint")
		persistedExp := rec.GetDateTime("cert_expires_at")
		if fingerprint == "" || fingerprint != persistedFP || persistedExp.IsZero() || !persistedExp.Time().After(time.Now()) {
			c.sendFn(ctx, conn, map[string]string{"type": "cert_error", "reason": "unknown fingerprint"})
			return
		}
		na = persistedExp.Time()
		// Re-seed the leaf index so this fingerprint is accepted for the rest
		// of the epoch without further fallback.
		c.coord.SeedLeaf(apiKeyID, fingerprint, nil, na)
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
	isCurrent := e != nil && e.conn == conn
	if isCurrent {
		e.tlsReady = true
	}
	c.epochMu.Unlock()
	if !isCurrent {
		return
	}
	// Baseline readiness = current-epoch TLS + relay DNS (§7.1): complete any
	// missing relay DNS provisioning from hello (no-op when already done) and
	// emit enrollment_ready when both halves hold. Direct endpoint/DDNS state
	// is an optional live capability and never gates this.
	c.ensureRelayDNS(ctx, apiKeyID, rec, e)
}

// HandleTLSError is a no-op beyond logging at this layer; the agent retries
// with backoff.
func (c *Controller) HandleTLSError(ctx context.Context, apiKeyID, reason string) {
	// No-op beyond logging at this layer; the agent retries with backoff.
}

// markReadyLocked flips e.ready to true when both TLS and relay DNS are ready
// and returns whether the epoch JUST became ready. Caller holds epochMu. The
// enrollment_ready message must be sent AFTER releasing epochMu (see
// sendEnrollmentReady); sending under the lock would hold it across a
// WebSocket write.
func (c *Controller) markReadyLocked(e *epochState) bool {
	if e.tlsReady && e.relayDNSReady && !e.ready {
		e.ready = true
		return true
	}
	return false
}

// sendEnrollmentReady sends the enrollment_ready message outside epochMu. On a
// send failure it clears readiness only if the SAME epoch still owns the conn
// (a newer epoch must not have its readiness clobbered by a stale failure).
// When the send succeeds, relay_config follows immediately — baseline
// readiness is exactly the point where §7.2 tunnel credentials are issued, on
// the authenticated current epoch, never before enrollment — and the §10.2
// immediate STUN challenge is issued last: the current-epoch enrollment
// handshake has now completed, so observation acquisition starts right away
// (including after every reconnect) rather than waiting for a recipient.
func (c *Controller) sendEnrollmentReady(apiKeyID string, conn *websocket.Conn, rec *core.Record, e *epochState) {
	if err := c.sendFn(context.Background(), conn, map[string]string{"type": "enrollment_ready"}); err != nil {
		c.epochMu.Lock()
		if cur := c.epochs[apiKeyID]; cur == e && cur.conn == conn {
			cur.ready = false
		}
		c.epochMu.Unlock()
		return
	}
	c.sendRelayConfig(apiKeyID, conn, rec)
	c.stunChallengeAfterEnrollment(apiKeyID, conn, e)
}
