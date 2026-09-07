package directctl

// Tests for plan Task 19 (spec §§7.1, 10.3, 11.2, 12, 15.7): direct DDNS and
// the public reachability probe are gated on a fresh current-epoch STUN
// observation that EXACTLY matches (IPv4) both the report_endpoint IP and the
// open_ack public IP; every §10.3 evaluation persists bounded diagnostic
// reason codes and relay_fallback markers that are NEVER read back as routing
// input; and a relay-only tunnel can never manufacture direct endpoint state
// on either side of the wire.
//
// Observation-injection helper: the §10.3 policy layer (this file) consumes
// the Task 18 observation structure. The UDP/listener classification itself
// is pinned by stun_test.go and the Task 16 listener tests, so policy tests
// install observations directly to control the address/classification axes.

import (
	"context"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pocketbase/pocketbase/core"
)

// installObservation injects one fresh observation into the CURRENT epoch of
// apiKeyID (AcceptedAt = the controller's clock reading, so it is fresh for
// stunObservationTTL of fake-clock time). The classification (public) is
// supplied by the test because the real listener derives it from the UDP
// source; stun_test.go covers that derivation.
func installObservation(t *testing.T, ctrl *Controller, apiKeyID string, ip netip.Addr, public bool) {
	t.Helper()
	now := ctrl.nowFn()
	ctrl.epochMu.Lock()
	defer ctrl.epochMu.Unlock()
	e := ctrl.epochs[apiKeyID]
	if e == nil {
		t.Fatalf("no epoch installed for %s", apiKeyID)
	}
	ctrl.stunMu.Lock()
	defer ctrl.stunMu.Unlock()
	es := ctrl.stunStateLocked(e)
	es.obs = &STUNObservation{IP: ip, PublicIPv4: public, AcceptedAt: now, Epoch: e.epoch}
}

// probeTransportCounter is a spy RoundTripper: it counts every outbound probe
// request ("zero calls on mismatch" proof) and answers with a canned 200 +
// the configured nonce body so a permitted probe completes without network.
type probeTransportCounter struct {
	mu    sync.Mutex
	calls int
	nonce string
}

func (p *probeTransportCounter) RoundTrip(*http.Request) (*http.Response, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(p.nonce)),
		Header:     make(http.Header),
	}, nil
}

func (p *probeTransportCounter) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// newPredicateEnv wires a controller with the real STUN listener enabled
// (§10.3 gating is active only when challenge scheduling is wired), plus the
// shared fake clock. Returns the app, controller, clock and a DDNS spy.
func newPredicateEnv(t *testing.T) (core.App, *Controller, *stunTestClock, *int) {
	t.Helper()
	app, ctrl, clock := newSTUNEnv(t)
	attachSTUNListener(t, ctrl, clock)
	calls := 0
	ctrl.ddnsFn = func(ctx context.Context, name, ip string, ttl int) (string, error) {
		calls++
		return "", nil
	}
	return app, ctrl, clock, &calls
}

// agentRow re-reads the persisted agent record for diagnostics assertions.
func agentRow(t *testing.T, app core.App, apiKeyID string) *core.Record {
	t.Helper()
	rec, _, err := LoadOrCreateAgent(app, apiKeyID)
	if err != nil {
		t.Fatalf("load agent: %v", err)
	}
	return rec
}

// TestDirectPredicateRequiresLiveWSDDNSEndpointAndFreshSTUNMatch pins the
// §7.1/§10.3 live predicate: the fresh current-epoch observation must be
// public-classified and exactly equal (IPv4) to every required surface, the
// observation must belong to the LIVE WebSocket epoch, and the endpoint must
// be reported by the CURRENT epoch's socket. Stale, missing, non-public,
// mismatched and malformed inputs all fall back.
func TestDirectPredicateRequiresLiveWSDDNSEndpointAndFreshSTUNMatch(t *testing.T) {
	freshPublic := STUNObservation{IP: netip.MustParseAddr("203.0.113.7"), PublicIPv4: true}
	cgnat := STUNObservation{IP: netip.MustParseAddr("100.64.12.4"), PublicIPv4: false}

	t.Run("fresh public exact match is eligible", func(t *testing.T) {
		out := evaluateDirectSTUNMatch(freshPublic, true, "203.0.113.7")
		if !out.Matched || out.Status != DirectStatusEligible {
			t.Fatalf("exact fresh public match must be eligible, got %+v", out)
		}
	})

	t.Run("both surfaces must match simultaneously", func(t *testing.T) {
		if out := evaluateDirectSTUNMatch(freshPublic, true, "203.0.113.7", "203.0.113.7"); !out.Matched {
			t.Fatalf("report + open_ack equal to observation must match, got %+v", out)
		}
		if out := evaluateDirectSTUNMatch(freshPublic, true, "203.0.113.7", "203.0.113.9"); out.Matched ||
			out.Reason != DirectReasonSTUNMismatch {
			t.Fatalf("one mismatched surface must refuse with stun_mismatch, got %+v", out)
		}
	})

	t.Run("stale observation falls back with stun_timeout", func(t *testing.T) {
		out := evaluateDirectSTUNMatch(freshPublic, false, "203.0.113.7")
		if out.Matched || out.Status != DirectStatusRelayFallback || out.Reason != DirectReasonSTUNTimeout {
			t.Fatalf("stale observation must be relay_fallback/stun_timeout, got %+v", out)
		}
	})

	t.Run("private, reserved and CGNAT observations fall back", func(t *testing.T) {
		out := evaluateDirectSTUNMatch(cgnat, true, "100.64.12.4")
		if out.Matched || out.Reason != DirectReasonSTUNNotPublic {
			t.Fatalf("CGNAT observation must refuse with stun_not_public even when equal, got %+v", out)
		}
	})

	t.Run("malformed or non-IPv4 required addresses refuse closed", func(t *testing.T) {
		for _, bad := range []string{"", "999.1.1.1", "::1", "::ffff:203.0.113.7", "203.0.113.7 ", "203.0.113.7/32"} {
			if out := evaluateDirectSTUNMatch(freshPublic, true, bad); out.Matched {
				t.Fatalf("required address %q must never match, got %+v", bad, out)
			}
		}
	})

	t.Run("observation dies with the WebSocket epoch", func(t *testing.T) {
		app, ctrl, _, ddnsCalls := newPredicateEnv(t)
		key := mustAPIKey(t, app, "key-pred-live-ws").Id
		enrollReadyAgent(t, ctrl, key)
		installObservation(t, ctrl, key, netip.MustParseAddr("203.0.113.7"), true)

		if out := ctrl.CurrentDirectMatch(key, "203.0.113.7"); !out.Matched {
			t.Fatalf("fresh current-epoch match expected, got %+v", out)
		}

		// A reconnect tears the epoch (and its observation) down: the new
		// epoch has no observation, so the predicate falls back even though
		// the OLD observation was fresh seconds ago (§10.2 epoch binding).
		ctrl.AgentDisconnected(key, nil)
		ctrl.HandleHello(context.Background(), nil, key, "acct-1", "agent-1")
		if out := ctrl.CurrentDirectMatch(key, "203.0.113.7"); out.Matched ||
			out.Reason != DirectReasonSTUNTimeout {
			t.Fatalf("observation must not survive an epoch change, got %+v", out)
		}

		// The endpoint surface must come from the CURRENT socket: a fenced
		// (superseded) conn's report_endpoint must not drive DDNS even with a
		// perfectly matching fresh observation.
		currentConn := &websocket.Conn{}
		ctrl.HandleHello(context.Background(), currentConn, key, "acct-1", "agent-1")
		installObservation(t, ctrl, key, netip.MustParseAddr("203.0.113.7"), true)
		staleConn := &websocket.Conn{}
		ctrl.HandleReportEndpoint(context.Background(), staleConn, key, "203.0.113.7", 443, "")
		if *ddnsCalls != 0 {
			t.Fatalf("fenced socket must not drive DDNS, got %d calls", *ddnsCalls)
		}
		ctrl.HandleReportEndpoint(context.Background(), currentConn, key, "203.0.113.7", 443, "")
		if *ddnsCalls != 1 {
			t.Fatalf("live-socket report with fresh exact match must provision DDNS once, got %d", *ddnsCalls)
		}
	})
}

// TestSTUNMustMatchReportEndpointAndOpenAck pins the dual exact-match rule
// end to end: the DDNS surface (report_endpoint) and the probe surface
// (open_ack.public_ip) EACH require exact IPv4 equality with the fresh
// current-epoch observation.
func TestSTUNMustMatchReportEndpointAndOpenAck(t *testing.T) {
	app, ctrl, _, ddnsCalls := newPredicateEnv(t)
	key := mustAPIKey(t, app, "key-stun-both-surfaces").Id
	enrollReadyAgent(t, ctrl, key)
	installObservation(t, ctrl, key, netip.MustParseAddr("203.0.113.7"), true)

	transport := &probeTransportCounter{nonce: "ack-nonce-1"}
	ctrl.probeClient = &http.Client{Transport: transport}

	// Report surface: the matching report provisions DDNS (once).
	ctrl.HandleReportEndpoint(context.Background(), nil, key, "203.0.113.7", 443, "")
	if *ddnsCalls != 1 {
		t.Fatalf("matching report must provision DDNS once, got %d", *ddnsCalls)
	}
	rec := agentRow(t, app, key)
	if rec.GetString("endpoint_ip") != "203.0.113.7" {
		t.Fatalf("endpoint_ip must be saved after the match, got %q", rec.GetString("endpoint_ip"))
	}

	// Ack surface: open_ack.public_ip differing from the observation refuses
	// the probe BEFORE any network activity (zero transport calls).
	err := ctrl.Probe(context.Background(), "demo.example.com", "sharecode", key,
		OpenAck{PublicIP: "198.51.100.9", GrantedPort: 443, Nonce: "ack-nonce-1"})
	if err == nil {
		t.Fatalf("probe with open_ack IP != observation must be refused")
	}
	if transport.count() != 0 {
		t.Fatalf("mismatched open_ack must stop before the probe, got %d HTTP calls", transport.count())
	}

	// Ack surface: exact agreement with the same observation authorizes the
	// probe, which then completes against the canned transport.
	err = ctrl.Probe(context.Background(), "demo.example.com", "sharecode", key,
		OpenAck{PublicIP: "203.0.113.7", GrantedPort: 443, Nonce: "ack-nonce-1"})
	if err != nil {
		t.Fatalf("probe with matching open_ack must run: %v", err)
	}
	if transport.count() != 1 {
		t.Fatalf("matching open_ack must produce exactly one probe call, got %d", transport.count())
	}
}

// TestMismatchStopsBeforeDDNSUpdateAndProbe is the §10.3/§15.7 stop gate: a
// fresh observation that disagrees with the reported IP stops the flow BEFORE
// the direct DDNS update and BEFORE the reachability probe (spy-proven zero
// calls on both).
func TestMismatchStopsBeforeDDNSUpdateAndProbe(t *testing.T) {
	app, ctrl, _, ddnsCalls := newPredicateEnv(t)
	key := mustAPIKey(t, app, "key-mismatch-stop").Id
	enrollReadyAgent(t, ctrl, key)
	installObservation(t, ctrl, key, netip.MustParseAddr("203.0.113.7"), true)

	transport := &probeTransportCounter{nonce: "n"}
	ctrl.probeClient = &http.Client{Transport: transport}

	// Fresh STUN says 203.0.113.7; the agent reports 203.0.113.9: no DDNS,
	// no endpoint record.
	ctrl.HandleReportEndpoint(context.Background(), nil, key, "203.0.113.9", 443, "")
	if *ddnsCalls != 0 {
		t.Fatalf("mismatch must stop before the DDNS update, got %d calls", *ddnsCalls)
	}
	rec := agentRow(t, app, key)
	if rec.GetString("endpoint_ip") != "" {
		t.Fatalf("mismatched endpoint_ip must never be saved, got %q", rec.GetString("endpoint_ip"))
	}
	if rec.GetString("direct_status") != string(DirectStatusRelayFallback) ||
		rec.GetString("direct_status_reason") != string(DirectReasonSTUNMismatch) {
		t.Fatalf("mismatch must persist relay_fallback/stun_mismatch diagnostics, got %q/%q",
			rec.GetString("direct_status"), rec.GetString("direct_status_reason"))
	}
	if rec.GetString("stun_observed_ip") != "203.0.113.7" {
		t.Fatalf("the driving observation must be recorded for diagnostics, got %q", rec.GetString("stun_observed_ip"))
	}

	// The probe surface refuses the mismatched ack before any HTTP call too.
	err := ctrl.Probe(context.Background(), "demo.example.com", "sharecode", key,
		OpenAck{PublicIP: "203.0.113.9", GrantedPort: 443, Nonce: "n"})
	if err == nil {
		t.Fatalf("probe against a mismatched ack must be refused")
	}
	if transport.count() != 0 {
		t.Fatalf("mismatch must stop before the probe, got %d HTTP calls", transport.count())
	}
}

// TestPrivateReservedCGNATObservationFallsBack pins §10.3: an observation of
// a private/reserved/CGNAT address never qualifies for direct, even when it
// exactly equals the reported IP, and the flow stops before DDNS and probe.
func TestPrivateReservedCGNATObservationFallsBack(t *testing.T) {
	app, ctrl, _, ddnsCalls := newPredicateEnv(t)
	key := mustAPIKey(t, app, "key-cgnat-fallback").Id
	enrollReadyAgent(t, ctrl, key)
	installObservation(t, ctrl, key, netip.MustParseAddr("100.64.12.4"), false)

	transport := &probeTransportCounter{nonce: "n"}
	ctrl.probeClient = &http.Client{Transport: transport}

	ctrl.HandleReportEndpoint(context.Background(), nil, key, "100.64.12.4", 443, "")
	if *ddnsCalls != 0 {
		t.Fatalf("CGNAT observation must stop before the DDNS update, got %d calls", *ddnsCalls)
	}
	rec := agentRow(t, app, key)
	if rec.GetString("endpoint_ip") != "" {
		t.Fatalf("CGNAT-backed endpoint must never be recorded, got %q", rec.GetString("endpoint_ip"))
	}
	if rec.GetString("direct_status") != string(DirectStatusRelayFallback) ||
		rec.GetString("direct_status_reason") != string(DirectReasonSTUNNotPublic) {
		t.Fatalf("CGNAT observation must persist relay_fallback/stun_not_public, got %q/%q",
			rec.GetString("direct_status"), rec.GetString("direct_status_reason"))
	}

	if err := ctrl.Probe(context.Background(), "demo.example.com", "sharecode", key,
		OpenAck{PublicIP: "100.64.12.4", GrantedPort: 443, Nonce: "n"}); err == nil {
		t.Fatalf("probe against a CGNAT ack must be refused")
	}
	if transport.count() != 0 {
		t.Fatalf("CGNAT case must stop before the probe, got %d HTTP calls", transport.count())
	}

	// The classification is §10.3 policy, not the test-only SSRF switch:
	// allowPrivateProbes must not bypass it.
	ctrl.allowPrivate = true
	if err := ctrl.Probe(context.Background(), "demo.example.com", "sharecode", key,
		OpenAck{PublicIP: "100.64.12.4", GrantedPort: 8443, Nonce: "n"}); err == nil {
		t.Fatalf("allowPrivate must not bypass the non-public observation classification")
	}
}

// TestDirectStatusDiagnosticsNeverAuthorizeRoute pins §12/§15.7 diagnostic
// discipline (the Task 15 relay_last_seen_at pattern): the persisted
// direct_status/direct_status_reason are write-only audit/UI diagnostics.
// Authorization always recomputes from the LIVE observation: poisoning the
// row cannot authorize a probe, and a stale observation cannot be rescued by
// a persisted eligible.
func TestDirectStatusDiagnosticsNeverAuthorizeRoute(t *testing.T) {
	app, ctrl, clock, _ := newPredicateEnv(t)
	key := mustAPIKey(t, app, "key-diagnostics-not-authority").Id
	enrollReadyAgent(t, ctrl, key)
	installObservation(t, ctrl, key, netip.MustParseAddr("203.0.113.7"), true)

	transport := &probeTransportCounter{nonce: "n"}
	ctrl.probeClient = &http.Client{Transport: transport}

	// A real match evaluates and records eligible...
	ctrl.HandleReportEndpoint(context.Background(), nil, key, "203.0.113.7", 443, "")
	if rec := agentRow(t, app, key); rec.GetString("direct_status") != string(DirectStatusEligible) {
		t.Fatalf("a match must record eligible diagnostics, got %q", rec.GetString("direct_status"))
	}

	// ...but poisoning the row to relay_fallback cannot DE-authorize a live
	// match: the probe decision reads the live observation, not the record.
	if err := ctrl.Probe(context.Background(), "demo.example.com", "sharecode", key,
		OpenAck{PublicIP: "203.0.113.7", GrantedPort: 4443, Nonce: "n"}); err != nil {
		t.Fatalf("persisted fallback diagnostics must not de-authorize a live match: %v", err)
	}

	// Poison the row to eligible, then let the observation go stale: the
	// persisted eligible must not authorize the probe (live re-evaluation).
	rec := agentRow(t, app, key)
	rec.Set("direct_status", string(DirectStatusEligible))
	rec.Set("direct_status_reason", "")
	if err := app.Save(rec); err != nil {
		t.Fatalf("poison diagnostics: %v", err)
	}
	clock.Advance(stunObservationTTL + time.Second)
	if err := ctrl.Probe(context.Background(), "demo.example.com", "sharecode", key,
		OpenAck{PublicIP: "203.0.113.7", GrantedPort: 5543, Nonce: "n"}); err == nil {
		t.Fatalf("a stale observation must not be authorized by persisted eligible diagnostics")
	}
	if transport.count() != 1 {
		t.Fatalf("only the live-match probe may reach the transport, got %d calls", transport.count())
	}
	// The failed live evaluation overwrites the poisoned row (last evaluation
	// wins for diagnostics).
	if rec := agentRow(t, app, key); rec.GetString("direct_status") != string(DirectStatusRelayFallback) ||
		rec.GetString("direct_status_reason") != string(DirectReasonSTUNTimeout) {
		t.Fatalf("stale probe evaluation must persist relay_fallback/stun_timeout, got %q/%q",
			rec.GetString("direct_status"), rec.GetString("direct_status_reason"))
	}
}

// TestRelayTunnelOnlyAgentDoesNotReportOrAuthorizeFakePublicEndpoint is the
// control half of the §11.2 relay-only isolation: an agent whose ONLY
// established transport is the FRP tunnel reports no endpoint, and control
// records/authorizes no endpoint from that state. relay_client_state
// telemetry never reaches directctl at all (the agent-WS handler treats it as
// telemetry and drops it before any directctl call — structural isolation),
// so this test proves the direct-state surfaces cannot be moved by relay
// operation: a manufactured report_endpoint (§11.2: "a relay tunnel does not
// report a fake public endpoint or port") is refused without a corroborating
// fresh STUN match, and the probe authorizes nothing for the fake IP.
func TestRelayTunnelOnlyAgentDoesNotReportOrAuthorizeFakePublicEndpoint(t *testing.T) {
	app, ctrl, _, ddnsCalls := newPredicateEnv(t)
	key := mustAPIKey(t, app, "key-relay-only-fake").Id
	enrollReadyAgent(t, ctrl, key)

	transport := &probeTransportCounter{nonce: "n"}
	ctrl.probeClient = &http.Client{Transport: transport}

	// Relay-only operation: the tunnel is up, the agent sends relay_client_state
	// (dropped as telemetry upstream) and NO real report_endpoint. Nothing in
	// directctl records endpoint state from that: the row stays endpointless.
	if rec := agentRow(t, app, key); rec.GetString("endpoint_ip") != "" {
		t.Fatalf("relay-only operation must not record an endpoint, got %q", rec.GetString("endpoint_ip"))
	}

	// §11.2 attack surface: the relay-only agent manufactures a public-shaped
	// endpoint report. With no observation at all it must not pass (stun_timeout).
	const fakeIP = "198.51.100.77"
	ctrl.HandleReportEndpoint(context.Background(), nil, key, fakeIP, 443, "")
	if *ddnsCalls != 0 {
		t.Fatalf("fake endpoint must never reach DDNS without a STUN match, got %d calls", *ddnsCalls)
	}
	rec := agentRow(t, app, key)
	if rec.GetString("endpoint_ip") != "" {
		t.Fatalf("fake endpoint must never be recorded, got %q", rec.GetString("endpoint_ip"))
	}
	if rec.GetString("direct_status") != string(DirectStatusRelayFallback) ||
		rec.GetString("direct_status_reason") != string(DirectReasonSTUNTimeout) {
		t.Fatalf("unobserved report must persist relay_fallback/stun_timeout, got %q/%q",
			rec.GetString("direct_status"), rec.GetString("direct_status_reason"))
	}

	// Even with a fresh observation of the agent's REAL shared address, the
	// fake IP still disagrees: mismatch, still zero DDNS.
	installObservation(t, ctrl, key, netip.MustParseAddr("203.0.113.7"), true)
	ctrl.HandleReportEndpoint(context.Background(), nil, key, fakeIP, 443, "")
	if *ddnsCalls != 0 {
		t.Fatalf("fake endpoint must never reach DDNS on mismatch, got %d calls", *ddnsCalls)
	}

	// And the probe authorizes nothing for the fake endpoint: refused before
	// any network activity.
	if err := ctrl.Probe(context.Background(), "demo.example.com", "sharecode", key,
		OpenAck{PublicIP: fakeIP, GrantedPort: 443, Nonce: "n"}); err == nil {
		t.Fatalf("probe against a fake endpoint must be refused")
	}
	if transport.count() != 0 {
		t.Fatalf("fake endpoint must stop before the probe, got %d HTTP calls", transport.count())
	}
	if rec := agentRow(t, app, key); rec.GetString("direct_status") == string(DirectStatusEligible) {
		t.Fatalf("relay-only operation must never persist eligible direct diagnostics")
	}
}
