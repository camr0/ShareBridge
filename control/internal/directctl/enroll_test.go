package directctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pocketbase/pocketbase/core"
	"sharebridge/control/internal/relayctl"
)

// testRelayGatewayIPv4 is the configured public relay gateway address the
// enrollment DNS provisioning points the per-namespace relay wildcard at
// (TEST-NET-3; never a real routable address).
const testRelayGatewayIPv4 = "203.0.113.10"

// relayDNSRecord captures one provisioning call made through relayDNSFn.
type relayDNSRecord struct {
	name string
	ip   string
	ttl  int
}

// collectSends swaps sendFn to record EVERY message sent during fn (maps and
// structs alike, normalized to generic maps) and restores it afterwards.
func (c *Controller) collectSends(fn func()) []map[string]any {
	var got []map[string]any
	old := c.sendFn
	c.sendFn = func(ctx context.Context, conn *websocket.Conn, msg any) error {
		if m, ok := msg.(map[string]any); ok {
			got = append(got, m)
			return nil
		}
		if raw, err := json.Marshal(msg); err == nil {
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			got = append(got, m)
		}
		return nil
	}
	fn()
	c.sendFn = old
	return got
}

// issueTestLeaf issues a chain through the stub coordinator and returns its
// leaf fingerprint, ready for a tls_ready report.
func issueTestLeaf(t *testing.T, ctrl *Controller, apiKeyID string) string {
	t.Helper()
	chain, err := ctrl.coord.Issue(context.Background(), []byte("csr"), "sbdeadbeef", apiKeyID)
	if err != nil {
		t.Fatalf("issue test chain: %v", err)
	}
	return leafFPOf(chain)
}

func TestHandleTLSReadyUnknownFingerprint(t *testing.T) {
	app, ctrl := newTestController(t)
	apiKeyID := mustAPIKey(t, app, "key-unknown-fp").Id

	// Install a current epoch (conn=nil) so the handler's epoch-fencing check
	// passes; the fingerprint itself is still unknown.
	ctrl.HandleHello(context.Background(), nil, apiKeyID, "acct-1", "agent-1")

	// No chain has been issued for this key, so any fingerprint is unknown.
	sent := ctrl.captureSend(func() {
		ctrl.HandleTLSReady(context.Background(), nil, apiKeyID, "deadbeef", time.Now().Add(90*24*time.Hour).Format(time.RFC3339))
	})
	if sent == nil || sent["type"] != "cert_error" {
		t.Fatalf("expected cert_error, got %v", sent)
	}
	if sent["reason"] != "unknown fingerprint" {
		t.Fatalf("expected reason 'unknown fingerprint', got %v", sent["reason"])
	}
	if ctrl.epochReady(apiKeyID) {
		t.Fatalf("must not be ready on unknown fingerprint")
	}
}

// TestBaselineReadyRequiresTLSAndRelayDNSNotDirectDDNS pins the §7.1 baseline
// invariant: enrollment readiness is exactly current-epoch TLS ready + relay
// DNS provisioned. Direct DDNS is an optional live capability — its failure
// must never block baseline readiness — while missing relay DNS must.
func TestBaselineReadyRequiresTLSAndRelayDNSNotDirectDDNS(t *testing.T) {
	app, ctrl := newTestController(t)
	apiKeyID := mustAPIKey(t, app, "key-baseline").Id

	ctrl.cfg.RelayGatewayIPv4 = testRelayGatewayIPv4
	ctrl.relayDNSFn = func(ctx context.Context, name, ip string, ttl int) (string, error) {
		return "", nil
	}
	directDNSCalls := 0
	ctrl.ddnsFn = func(ctx context.Context, name, ip string, ttl int) (string, error) {
		directDNSCalls++
		return "", errors.New("cloudflare unavailable") // direct DDNS is down
	}

	sent := ctrl.captureSend(func() { ctrl.HandleHello(context.Background(), nil, apiKeyID, "acct-1", "agent-1") })
	if sent["type"] != "enrolled" {
		t.Fatalf("expected enrolled, got %v", sent)
	}
	if ctrl.epochReady(apiKeyID) {
		t.Fatalf("relay DNS alone must not complete baseline readiness")
	}

	// A failed direct-DDNS report_endpoint must not ready the epoch (and must
	// not emit anything) — but the report path still attempts it.
	sentReport := ctrl.captureSend(func() {
		ctrl.HandleReportEndpoint(context.Background(), nil, apiKeyID, "1.2.3.4", 0, "")
	})
	if sentReport != nil {
		t.Fatalf("failed direct DDNS must not emit a message, got %v", sentReport)
	}
	if directDNSCalls != 1 {
		t.Fatalf("report_endpoint must still drive direct DDNS, got %d calls", directDNSCalls)
	}
	if ctrl.epochReady(apiKeyID) {
		t.Fatalf("direct endpoint/DDNS must not be an enrollment gate")
	}

	// Current-epoch TLS completes baseline readiness despite dead direct DDNS:
	// the gate is TLS + relay DNS only.
	fp := issueTestLeaf(t, ctrl, apiKeyID)
	sentReady := ctrl.captureSend(func() {
		ctrl.HandleTLSReady(context.Background(), nil, apiKeyID, fp, time.Now().Add(90*24*time.Hour).Format(time.RFC3339))
	})
	if sentReady == nil || sentReady["type"] != "enrollment_ready" {
		t.Fatalf("expected enrollment_ready after TLS + relay DNS with direct DDNS failing, got %v", sentReady)
	}
	if !ctrl.epochReady(apiKeyID) {
		t.Fatalf("epoch must be ready after TLS + relay DNS")
	}

	// Readiness is epoch-local: a replacement epoch starts un-ready.
	ctrl.HandleHello(context.Background(), nil, apiKeyID, "acct-1", "agent-1")
	if ctrl.epochReady(apiKeyID) {
		t.Fatalf("new epoch must not inherit readiness")
	}
}

// TestNoMapperCanReachEnrollmentReady pins the §7.1 deadlock fix: an agent
// without a port mapper never sends report_endpoint, so enrollment_ready must
// arrive from hello (relay DNS) + tls_ready alone, with zero direct-DDNS
// activity and no persisted endpoint.
func TestNoMapperCanReachEnrollmentReady(t *testing.T) {
	app, ctrl := newTestController(t)
	apiKeyID := mustAPIKey(t, app, "key-nomapper").Id

	ctrl.cfg.RelayGatewayIPv4 = testRelayGatewayIPv4
	relayDNSCalls := 0
	ctrl.relayDNSFn = func(ctx context.Context, name, ip string, ttl int) (string, error) {
		relayDNSCalls++
		return "", nil
	}
	directDNSCalls := 0
	ctrl.ddnsFn = func(ctx context.Context, name, ip string, ttl int) (string, error) {
		directDNSCalls++
		return "", nil
	}

	sent := ctrl.captureSend(func() { ctrl.HandleHello(context.Background(), nil, apiKeyID, "acct-1", "agent-1") })
	if sent["type"] != "enrolled" {
		t.Fatalf("expected enrolled, got %v", sent)
	}
	if relayDNSCalls != 1 {
		t.Fatalf("relay wildcard must be provisioned during enrollment, got %d calls", relayDNSCalls)
	}

	fp := issueTestLeaf(t, ctrl, apiKeyID)
	sentReady := ctrl.captureSend(func() {
		ctrl.HandleTLSReady(context.Background(), nil, apiKeyID, fp, time.Now().Add(90*24*time.Hour).Format(time.RFC3339))
	})
	if sentReady == nil || sentReady["type"] != "enrollment_ready" {
		t.Fatalf("a no-mapper agent must reach enrollment_ready without report_endpoint, got %v", sentReady)
	}
	if !ctrl.epochReady(apiKeyID) {
		t.Fatalf("epoch must be ready for a no-mapper agent")
	}
	if directDNSCalls != 0 {
		t.Fatalf("direct DDNS must never run for a no-mapper agent, got %d calls", directDNSCalls)
	}
	rec, _, err := LoadOrCreateAgent(app, apiKeyID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.GetString("endpoint_ip") != "" {
		t.Fatalf("no endpoint must be persisted without a report, got %q", rec.GetString("endpoint_ip"))
	}
}

// TestRelayWildcardProvisionedAtEnrollment pins the §6 provisioning contract:
// enrollment idempotently points *.relay.<namespace>.<base-domain> at the
// configured gateway IPv4 with the 60-second A-record TTL. Absent or invalid
// gateway IPv4 and malformed namespaces are rejected without touching DNS,
// and a recovery after reconfiguration provisions on the next enrollment.
func TestRelayWildcardProvisionedAtEnrollment(t *testing.T) {
	app, ctrl := newTestController(t)
	apiKeyID := mustAPIKey(t, app, "key-relaydns").Id
	ctrl.cfg.RelayGatewayIPv4 = testRelayGatewayIPv4

	var relayDNSCalls []relayDNSRecord
	ctrl.relayDNSFn = func(ctx context.Context, name, ip string, ttl int) (string, error) {
		relayDNSCalls = append(relayDNSCalls, relayDNSRecord{name: name, ip: ip, ttl: ttl})
		return "record-id", nil
	}

	ctrl.HandleHello(context.Background(), nil, apiKeyID, "acct-1", "agent-1")
	agentRec, _, err := LoadOrCreateAgent(app, apiKeyID)
	if err != nil {
		t.Fatal(err)
	}
	namespace := agentRec.GetString("namespace")
	wantName := fmt.Sprintf("*.relay.%s.example.com", namespace)
	if len(relayDNSCalls) != 1 {
		t.Fatalf("expected exactly one provisioning call, got %#v", relayDNSCalls)
	}
	if relayDNSCalls[0].name != wantName {
		t.Fatalf("provisioned name = %q, want %q", relayDNSCalls[0].name, wantName)
	}
	if relayDNSCalls[0].ip != testRelayGatewayIPv4 {
		t.Fatalf("provisioned content = %q, want gateway IPv4 %q", relayDNSCalls[0].ip, testRelayGatewayIPv4)
	}
	if relayDNSCalls[0].ttl != 60 {
		t.Fatalf("provisioned TTL = %d, want the 60s A-record TTL", relayDNSCalls[0].ttl)
	}

	// A replacement epoch re-enrolls the same agent: the record is static for
	// the gateway assignment lifetime, so no second provider call may happen.
	ctrl.HandleHello(context.Background(), nil, apiKeyID, "acct-1", "agent-1")
	if len(relayDNSCalls) != 1 {
		t.Fatalf("idempotent enrollment must not re-provision, got %#v", relayDNSCalls)
	}

	// Absent relay gateway IPv4: rejected, zero DNS activity, never ready.
	absentKey := mustAPIKey(t, app, "key-relaydns-absent").Id
	ctrl.cfg.RelayGatewayIPv4 = ""
	ctrl.HandleHello(context.Background(), nil, absentKey, "acct-1", "agent-2")
	if len(relayDNSCalls) != 1 {
		t.Fatalf("absent relay IPv4 must not touch DNS, got %#v", relayDNSCalls)
	}
	if ctrl.epochReady(absentKey) {
		t.Fatalf("absent relay IPv4 must never complete baseline readiness")
	}

	// Malformed relay gateway IPv4: same rejection.
	malformedKey := mustAPIKey(t, app, "key-relaydns-badip").Id
	ctrl.cfg.RelayGatewayIPv4 = "not-an-ip"
	ctrl.HandleHello(context.Background(), nil, malformedKey, "acct-1", "agent-3")
	if len(relayDNSCalls) != 1 {
		t.Fatalf("invalid relay IPv4 must not touch DNS, got %#v", relayDNSCalls)
	}
	if ctrl.epochReady(malformedKey) {
		t.Fatalf("invalid relay IPv4 must never complete baseline readiness")
	}

	// A namespace that could not have come from GenerateNamespace is refused:
	// the DNS target composes only from the authenticated agent row's
	// well-formed namespace, never from agent-influenced strings.
	agentRec.Set("namespace", "evil.label.example.com")
	if err := app.Save(agentRec); err != nil {
		t.Fatalf("mutate namespace for rejection test: %v", err)
	}
	ctrl.HandleHello(context.Background(), nil, apiKeyID, "acct-1", "agent-1")
	if len(relayDNSCalls) != 1 {
		t.Fatalf("malformed namespace must not touch DNS, got %#v", relayDNSCalls)
	}

	// Restoring a valid address heals the next enrollment: provisioning runs
	// once, and TLS completes readiness for that epoch.
	ctrl.cfg.RelayGatewayIPv4 = testRelayGatewayIPv4
	ctrl.HandleHello(context.Background(), nil, absentKey, "acct-1", "agent-2")
	if len(relayDNSCalls) != 2 {
		t.Fatalf("valid reconfiguration must provision on next enrollment, got %#v", relayDNSCalls)
	}
	if relayDNSCalls[1].name != fmt.Sprintf("*.relay.%s.example.com", mustNamespace(t, app, absentKey)) {
		t.Fatalf("recovery provisioned %q", relayDNSCalls[1].name)
	}
	fp := issueTestLeaf(t, ctrl, absentKey)
	sentReady := ctrl.captureSend(func() {
		ctrl.HandleTLSReady(context.Background(), nil, absentKey, fp, time.Now().Add(90*24*time.Hour).Format(time.RFC3339))
	})
	if sentReady == nil || sentReady["type"] != "enrollment_ready" {
		t.Fatalf("expected enrollment_ready after recovery, got %v", sentReady)
	}
}

// mustNamespace returns the persisted namespace for an agent API key.
func mustNamespace(t *testing.T, app core.App, apiKeyID string) string {
	t.Helper()
	rec, _, err := LoadOrCreateAgent(app, apiKeyID)
	if err != nil {
		t.Fatalf("load agent: %v", err)
	}
	return rec.GetString("namespace")
}

// TestTunnelHostnameIsNotContentWildcard pins the §6 record separation: the
// FRP transport hostname (<relay-tunnel-host>, relay_config.gateway_addr) is
// an operator-provisioned transport record — enrollment must never provision
// it, must never conflate it with the *.relay.<ns> content wildcard it does
// provision, and derived relay origins must live under that wildcard while
// the tunnel host stays outside the content namespace.
func TestTunnelHostnameIsNotContentWildcard(t *testing.T) {
	app, ctrl := newTestController(t)
	apiKeyID := mustAPIKey(t, app, "key-tunnel-sep").Id

	// Relay enabled with a tunnel hostname distinct from any content name.
	signer, err := relayctl.GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	portRange, err := relayctl.NewPortRange(11000, 11019)
	if err != nil {
		t.Fatal(err)
	}
	settings := relayctl.Settings{GatewayAddr: "tunnel.example.com", GatewayPort: 7000, PortRange: portRange}
	ctrl.EnableRelay(settings, signer)
	ctrl.cfg.RelayGatewayIPv4 = testRelayGatewayIPv4

	var provisionedNames []string
	ctrl.relayDNSFn = func(ctx context.Context, name, ip string, ttl int) (string, error) {
		provisionedNames = append(provisionedNames, name)
		return "", nil
	}
	ctrl.ddnsFn = func(ctx context.Context, name, ip string, ttl int) (string, error) { return "", nil }

	ctrl.HandleHello(context.Background(), nil, apiKeyID, "acct-1", "agent-1")
	namespace := mustNamespace(t, app, apiKeyID)
	relayWildcard := fmt.Sprintf("*.relay.%s.example.com", namespace)

	// Enrollment provisions EXACTLY the §6 relay content wildcard — never the
	// direct content wildcard, never the FRP transport hostname.
	if len(provisionedNames) != 1 || provisionedNames[0] != relayWildcard {
		t.Fatalf("enrollment must provision exactly %q, got %#v", relayWildcard, provisionedNames)
	}
	for _, forbidden := range []string{
		fmt.Sprintf("*.%s.example.com", namespace), // direct content wildcard
		settings.GatewayAddr,                       // operator-provisioned transport record
	} {
		for _, provisioned := range provisionedNames {
			if provisioned == forbidden {
				t.Fatalf("enrollment must never provision %q", forbidden)
			}
		}
	}

	// Baseline readiness emits enrollment_ready and then relay_config, whose
	// transport identity stays the tunnel hostname — never the wildcard.
	fp := issueTestLeaf(t, ctrl, apiKeyID)
	frames := ctrl.collectSends(func() {
		ctrl.HandleTLSReady(context.Background(), nil, apiKeyID, fp, time.Now().Add(90*24*time.Hour).Format(time.RFC3339))
	})
	readySeen, relayMsg := false, map[string]any(nil)
	for _, frame := range frames {
		switch frame["type"] {
		case "enrollment_ready":
			readySeen = true
		case "relay_config":
			relayMsg = frame
		}
	}
	if !readySeen {
		t.Fatalf("expected enrollment_ready, got %#v", frames)
	}
	if relayMsg == nil {
		t.Fatalf("expected relay_config after baseline readiness, got %#v", frames)
	}
	if relayMsg["gateway_addr"] != settings.GatewayAddr {
		t.Fatalf("relay_config gateway_addr = %v, want the tunnel host %q", relayMsg["gateway_addr"], settings.GatewayAddr)
	}
	if relayMsg["gateway_addr"] == relayWildcard {
		t.Fatalf("the tunnel transport identity must never be the content wildcard")
	}

	// The §6 relay origin for a share is a child of the provisioned wildcard,
	// while the tunnel host is not part of the content namespace at all.
	directOrigin := fmt.Sprintf("ab12cd34.%s.example.com", namespace)
	relayOrigin, err := relayctl.RelayOriginFromDirect(directOrigin)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(relayOrigin, relayWildcard[1:]) { // strip the leading "*"
		t.Fatalf("relay origin %q must live under the provisioned wildcard %q", relayOrigin, relayWildcard)
	}
	if strings.HasSuffix(settings.GatewayAddr, relayWildcard[1:]) || relayWildcard == settings.GatewayAddr {
		t.Fatalf("tunnel host %q must stay outside the relay content wildcard", settings.GatewayAddr)
	}
}

// TestHandleTLSReadyAcceptsPersistedFingerprintAfterRestart simulates a control
// restart (fresh coordinator, empty leaf index) where the agent record still
// holds the installed cert fingerprint + expiry. The tls_ready must be accepted
// via the persisted row and re-seed the coordinator's leaf index (C3).
func TestHandleTLSReadyAcceptsPersistedFingerprintAfterRestart(t *testing.T) {
	app, ctrl := newTestController(t)
	apiKeyID := mustAPIKey(t, app, "key-restart").Id
	ctrl.HandleHello(context.Background(), nil, apiKeyID, "acct-1", "agent-1")

	// Persist a cert row WITHOUT going through Issue (simulates the coordinator
	// having restarted and lost its in-memory leaf index).
	rec, _, err := LoadOrCreateAgent(app, apiKeyID)
	if err != nil {
		t.Fatal(err)
	}
	const fp = "abc123persisted"
	exp := time.Now().Add(90 * 24 * time.Hour)
	if err := SaveCertReady(app, rec, fp, exp); err != nil {
		t.Fatal(err)
	}

	sent := ctrl.captureSend(func() {
		ctrl.HandleTLSReady(context.Background(), nil, apiKeyID, fp, exp.Format(time.RFC3339))
	})
	if sent != nil && sent["type"] == "cert_error" {
		t.Fatalf("persisted fingerprint must be accepted, got %v", sent)
	}
	if !ctrl.coord.HasLeafFingerprint(apiKeyID, fp) {
		t.Fatalf("leaf index must be re-seeded from the persisted row")
	}
}

// TestFencedConnCannotDriveEpochState verifies a superseded connection cannot
// drive epoch-sensitive state: after a replacement hello installs a new epoch,
// a stale conn's report_endpoint (and by extension tls_ready/csr_submit/
// open_ack) is ignored (I2).
func TestFencedConnCannotDriveEpochState(t *testing.T) {
	app, ctrl := newTestController(t)
	apiKeyID := mustAPIKey(t, app, "key-fence").Id

	stale := new(websocket.Conn)
	fresh := new(websocket.Conn)
	ctrl.HandleHello(context.Background(), stale, apiKeyID, "acct-1", "agent-1")
	ctrl.HandleHello(context.Background(), fresh, apiKeyID, "acct-1", "agent-1")

	ddnsCalls := 0
	ctrl.ddnsFn = func(ctx context.Context, name, ip string, ttl int) (string, error) {
		ddnsCalls++
		return "", nil
	}

	// A stale conn's report_endpoint must be ignored: no DDNS, no readiness.
	ctrl.HandleReportEndpoint(context.Background(), stale, apiKeyID, "1.2.3.4", 0, "")
	if ddnsCalls != 0 {
		t.Fatalf("stale conn must not trigger DDNS, got %d calls", ddnsCalls)
	}
	if ctrl.epochReady(apiKeyID) {
		t.Fatalf("stale conn must not advance readiness")
	}

	// The fresh conn does trigger DDNS.
	ctrl.HandleReportEndpoint(context.Background(), fresh, apiKeyID, "1.2.3.4", 0, "")
	if ddnsCalls != 1 {
		t.Fatalf("fresh conn must trigger DDNS, got %d calls", ddnsCalls)
	}
}
