package integration

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// startPrimaryStack builds the hermetic stack, starts the primary tunnel,
// publishes its exact relay route, and returns once the real presence
// registry reports it online and the gateway resolves it.
func startPrimaryStack(t *testing.T) (*relayStack, tunnelSpec) {
	t.Helper()
	s := newRelayStack(t)
	spec := tunnelSpec{
		label:      "primary",
		agentID:    "agent-primary",
		namespace:  fixtureNamespace,
		generation: 1,
		proxyPort:  s.allocPort(t),
		localPort:  portOf(t, s.agentAddr),
	}
	s.startTunnel(spec)
	s.applyRoute(s.relayHost, spec, 1)
	s.waitOnline(spec, setupTimeout)
	s.waitRouteReady(s.relayHost, setupTimeout)
	return s, spec
}

// TestRealFRPRelayEndToEndTLS12HTTP11 proves the full stack — gateway →
// pinned frps → pinned frpc → agent HTTPS — completes a real TLS 1.2 /
// HTTP/1.1 session with a ClientHello fragmented across two TLS RECORDS, and
// that the browser-side and post-FRP streams are byte-identical (§23.3).
// Record-level fragmentation only: TCP segment boundaries are not claimed.
func TestRealFRPRelayEndToEndTLS12HTTP11(t *testing.T) {
	defer beginGateCase(t)()
	requireIntegration(t)
	s, _ := startPrimaryStack(t)

	rc := s.newRelayClient(tls.VersionTLS12, tls.VersionTLS12, []string{"http/1.1"}, 2)
	resp := doGet(t, rc.client, s.relayURL("/s/"+fixtureCode), nil)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("page status = %d, want 200", resp.StatusCode)
	}
	if resp.Proto != "HTTP/1.1" {
		resp.Body.Close()
		t.Fatalf("protocol = %q, want HTTP/1.1", resp.Proto)
	}
	if got := rc.negotiatedVersion(); got != tls.VersionTLS12 {
		resp.Body.Close()
		t.Fatalf("negotiated TLS version = %#x, want TLS 1.2", got)
	}
	body := readResponseBytes(t, resp)
	if !strings.Contains(string(body), `id="gallery-root"`) {
		t.Fatalf("page body missing gallery root: %q", body)
	}
	rc.client.CloseIdleConnections()

	digest := assertByteParity(t, rc.wire, s.agentTap)
	if err := checkFragmentation(digest.Records, 2); err != nil {
		t.Fatalf("TLS 1.2 / HTTP/1.1 case: %v", err)
	}
}

// TestRealFRPRelayEndToEndTLS13HTTP2 is the TLS 1.3 / HTTP/2 counterpart of
// the TLS 1.2 case: real h2 negotiation over the L4 relay with a two-record
// ClientHello and exact post-FRP byte preservation. Record-level
// fragmentation only: TCP segment boundaries are not claimed.
func TestRealFRPRelayEndToEndTLS13HTTP2(t *testing.T) {
	defer beginGateCase(t)()
	requireIntegration(t)
	s, _ := startPrimaryStack(t)

	rc := s.newRelayClient(tls.VersionTLS13, tls.VersionTLS13, []string{"h2"}, 2)
	resp := doGet(t, rc.client, s.relayURL("/s/"+fixtureCode+"/items"), nil)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("items status = %d, want 200", resp.StatusCode)
	}
	if resp.Proto != "HTTP/2.0" {
		resp.Body.Close()
		t.Fatalf("protocol = %q, want HTTP/2.0", resp.Proto)
	}
	if got := rc.negotiatedVersion(); got != tls.VersionTLS13 {
		resp.Body.Close()
		t.Fatalf("negotiated TLS version = %#x, want TLS 1.3", got)
	}
	if alpn := rc.negotiatedALPN(); alpn != "h2" {
		resp.Body.Close()
		t.Fatalf("negotiated ALPN = %q, want h2", alpn)
	}
	if body := readResponseBytes(t, resp); !bytes.Contains(body, []byte(`"albumName":"Parity Album"`)) {
		t.Fatalf("items body unexpected: %q", body)
	}
	rc.client.CloseIdleConnections()

	digest := assertByteParity(t, rc.wire, s.agentTap)
	if err := checkFragmentation(digest.Records, 2); err != nil {
		t.Fatalf("TLS 1.3 / HTTP/2 case: %v", err)
	}
}

// TestRealFRPFragmentedClientHelloReplay drives a ClientHello split across
// EXACTLY five TLS RECORDS through the gateway/FRP path and requires the exact
// same bytes — headers and ClientHello fragments included — to arrive at the
// agent after FRP decapsulation (§16.1, §23.3).
//
// Scope: this is TLS record-level fragmentation. Separate TCP write() calls do
// not prove distinct kernel TCP segments, so no TCP-segmentation claim is made
// here or in the evidence (see GATE-EVIDENCE.md §23.3).
func TestRealFRPFragmentedClientHelloReplay(t *testing.T) {
	defer beginGateCase(t)()
	requireIntegration(t)
	s, _ := startPrimaryStack(t)

	rc := s.newRelayClient(tls.VersionTLS12, tls.VersionTLS13, []string{"http/1.1"}, 5)
	resp := doGet(t, rc.client, s.relayURL("/s/"+fixtureCode+"/thumb/img-1"), nil)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("thumb status = %d, want 200", resp.StatusCode)
	}
	readResponseBytes(t, resp)
	rc.client.CloseIdleConnections()

	digest := assertByteParity(t, rc.wire, s.agentTap)
	if err := checkFragmentation(digest.Records, 5); err != nil {
		t.Fatalf("fragmented replay: %v", err)
	}
}

// TestRealFRPExactRouting proves the gateway routes only an exact active
// origin to its owning agent: a valid route reaches exactly one tunnel,
// while random, bare, unknown, and tombstoned SNI reach no agent at all.
func TestRealFRPExactRouting(t *testing.T) {
	defer beginGateCase(t)()
	requireIntegration(t)
	s := newRelayStack(t)

	specA := tunnelSpec{
		label: "agent-a", agentID: "agent-a", namespace: fixtureNamespace,
		generation: 1, proxyPort: s.allocPort(t), localPort: portOf(t, s.agentAddr),
	}
	specB := tunnelSpec{
		label: "agent-b", agentID: "agent-b", namespace: fixtureNamespaceB,
		generation: 1, proxyPort: s.allocPort(t), localPort: portOf(t, s.rawTargetLn.Addr().String()),
	}
	s.startTunnel(specA)
	s.applyRoute(s.relayHost, specA, 1)
	s.waitOnline(specA, setupTimeout)
	s.waitRouteReady(s.relayHost, setupTimeout)

	s.startTunnel(specB)
	s.applyRoute(s.relayHostB, specB, 1)
	s.waitOnline(specB, setupTimeout)
	s.waitRouteReady(s.relayHostB, setupTimeout)

	agentBase := s.agentTap.Len()
	rawBase := s.rawTarget.Len()

	// Valid exact route to agent A: only A's content listener sees bytes.
	sendTLSClientHello(t, s.gatewayAddr, s.relayHost)
	waitForBytes(t, s.agentTap, agentBase+1, 5*time.Second)
	waitForStable(t, s.agentTap, 300*time.Millisecond, 5*time.Second)
	if s.rawTarget.Len() != rawBase {
		t.Fatalf("route A leaked bytes to agent B's target (%d → %d)", rawBase, s.rawTarget.Len())
	}

	// Valid exact route to agent B: only B's raw target sees bytes.
	agentAfterA := s.agentTap.Len()
	sendTLSClientHello(t, s.gatewayAddr, s.relayHostB)
	waitForBytes(t, s.rawTarget, rawBase+1, 5*time.Second)
	waitForStable(t, s.rawTarget, 300*time.Millisecond, 5*time.Second)
	if s.agentTap.Len() != agentAfterA {
		t.Fatalf("route B leaked bytes to agent A's listener (%d → %d)", agentAfterA, s.agentTap.Len())
	}

	// Unknown / random SNI: nothing reaches either agent.
	agentAfterB, rawAfterB := s.agentTap.Len(), s.rawTarget.Len()
	sendTLSClientHello(t, s.gatewayAddr, "random.relay."+fixtureNamespace+"."+fixtureBase)
	// Bare hostname with no route.
	sendTLSClientHello(t, s.gatewayAddr, fixtureBase)
	time.Sleep(750 * time.Millisecond)
	waitForStable(t, s.agentTap, 300*time.Millisecond, 5*time.Second)
	waitForStable(t, s.rawTarget, 300*time.Millisecond, 5*time.Second)
	if s.agentTap.Len() != agentAfterB || s.rawTarget.Len() != rawAfterB {
		t.Fatalf("unknown/bare SNI reached an agent (agent %d→%d, raw %d→%d)",
			agentAfterB, s.agentTap.Len(), rawAfterB, s.rawTarget.Len())
	}

	// Tombstoned route: revoked before any connection, still reaches nobody.
	tombstoneHost := "share09.relay." + fixtureNamespace + "." + fixtureBase
	s.applyRoute(tombstoneHost, specA, 1)
	if err := s.routesTable.Revoke(tombstoneHost, 2); err != nil {
		t.Fatalf("revoke tombstone route: %v", err)
	}
	agentAfterTomb, rawAfterTomb := s.agentTap.Len(), s.rawTarget.Len()
	sendTLSClientHello(t, s.gatewayAddr, tombstoneHost)
	time.Sleep(750 * time.Millisecond)
	waitForStable(t, s.agentTap, 300*time.Millisecond, 5*time.Second)
	waitForStable(t, s.rawTarget, 300*time.Millisecond, 5*time.Second)
	if s.agentTap.Len() != agentAfterTomb || s.rawTarget.Len() != rawAfterTomb {
		t.Fatalf("tombstoned SNI reached an agent (agent %d→%d, raw %d→%d)",
			agentAfterTomb, s.agentTap.Len(), rawAfterTomb, s.rawTarget.Len())
	}
}

// TestRealFRPContentParity runs the Phase 3 content surface — page, items,
// thumb, preview, original, playback with multiple 206 seeks, archive,
// cancellation, accounting, concurrency, revocation and tunnel restart —
// through the real FRP relay and compares it byte-for-byte against the same
// fake backend served directly by the agent (§18.2, acceptance #11).
func TestRealFRPContentParity(t *testing.T) {
	defer beginGateCase(t)()
	requireIntegration(t)
	s, _ := startPrimaryStack(t)

	direct := s.newDirectClient()
	relay := s.newRelayClient(tls.VersionTLS12, tls.VersionTLS13, []string{"h2", "http/1.1"}, 2).client

	t.Run("content", func(t *testing.T) {
		paths := []string{
			"/s/" + fixtureCode,
			"/s/" + fixtureCode + "/items",
			"/s/" + fixtureCode + "/thumb/img-1",
			"/s/" + fixtureCode + "/preview/img-1",
			"/s/" + fixtureCode + "/asset/img-1",
			"/s/" + fixtureCode + "/archive",
			"/s/" + fixtureCode + "/archive/" + archiveToken + "/0",
		}
		for _, path := range paths {
			directResp := doGet(t, direct, s.directURL(path), nil)
			relayResp := doGet(t, relay, s.relayURL(path), nil)
			assertSameResponse(t, path, directResp, relayResp)
		}
	})

	t.Run("range-seeks", func(t *testing.T) {
		seeks := []string{"bytes=0-99", "bytes=100-199", "bytes=500-599", "bytes=1023-", "bytes=900-"}
		for _, seek := range seeks {
			headers := map[string]string{"Range": seek}
			directResp := doGet(t, direct, s.directURL("/s/"+fixtureCode+"/asset/vid-1/playback"), headers)
			relayResp := doGet(t, relay, s.relayURL("/s/"+fixtureCode+"/asset/vid-1/playback"), headers)
			if directResp.StatusCode != http.StatusPartialContent || relayResp.StatusCode != http.StatusPartialContent {
				directResp.Body.Close()
				relayResp.Body.Close()
				t.Fatalf("seek %s direct=%d relay=%d, want 206", seek, directResp.StatusCode, relayResp.StatusCode)
			}
			body := assertSameResponse(t, seek, directResp, relayResp)
			start, end, _ := parseByteRange(seek, contentVideoLen)
			if !bytes.Equal(body, contentVideoBytes[start:end+1]) {
				t.Fatalf("seek %s returned incorrect body bytes", seek)
			}
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.relayURL("/s/"+fixtureCode+"/slow"), nil)
		if err != nil {
			t.Fatalf("build slow request: %v", err)
		}
		resp, err := relay.Do(req)
		if err != nil {
			t.Fatalf("slow request failed before cancellation: %v", err)
		}
		resp.Body.Close()
		cancel()
		select {
		case <-s.content.slowCancelled:
		case <-time.After(10 * time.Second):
			t.Fatal("agent never observed cancellation of the relayed slow stream")
		}
	})

	t.Run("accounting", func(t *testing.T) {
		for _, endpoint := range []string{"page", "items", "thumb", "preview", "original", "playback", "archive-manifest", "archive-part", "slow"} {
			if s.counts.get(endpoint) == 0 {
				t.Fatalf("endpoint %q was never accounted", endpoint)
			}
		}
		if got := s.content.connectCount(); got != 0 {
			t.Fatalf("relay content path hit /connect %d time(s); relay must never open the direct probe path", got)
		}
	})

	t.Run("concurrency", func(t *testing.T) {
		const workers = 8
		var wg sync.WaitGroup
		errs := make(chan error, workers)
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp, err := relay.Get(s.relayURL("/s/" + fixtureCode + "/items"))
				if err != nil {
					errs <- err
					return
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					errs <- fmt.Errorf("status %d", resp.StatusCode)
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("concurrent relay request failed: %v", err)
		}
	})

	t.Run("revocation", func(t *testing.T) {
		revoked := newRelayStack(t)
		spec := tunnelSpec{
			label: "revoked", agentID: "agent-revoked", namespace: fixtureNamespace,
			generation: 1, proxyPort: revoked.allocPort(t), localPort: portOf(t, revoked.agentAddr),
		}
		revoked.startTunnel(spec)
		revoked.applyRoute(revoked.relayHost, spec, 1)
		revoked.waitOnline(spec, setupTimeout)
		revoked.waitRouteReady(revoked.relayHost, setupTimeout)

		// Hold a live relayed TLS connection open so the gateway stream is
		// registered at revoke time. A raw connection (not an http.Client)
		// keeps the stream open without transport keep-alive semantics.
		live := openRelayTLS(t, revoked, revoked.relayHost)
		defer live.Close()
		waitForStreams(t, revoked, 1, 5*time.Second)
		waitForStable(t, revoked.agentTap, 300*time.Millisecond, 5*time.Second)

		// Mutate the table first, then drain established streams (the ordering
		// invariant documented on gateway.Streams).
		if err := revoked.routesTable.Revoke(revoked.relayHost, 2); err != nil {
			t.Fatalf("revoke route: %v", err)
		}
		if closed := revoked.streams.CloseRoute(revoked.relayHost); closed != 1 {
			t.Fatalf("CloseRoute closed %d streams, want 1", closed)
		}
		if revoked.streams.Len() != 0 {
			t.Fatalf("stream registry still holds %d streams after revoke", revoked.streams.Len())
		}
		// Let the closed session's final records drain before the leak baseline.
		waitForStable(t, revoked.agentTap, 500*time.Millisecond, 5*time.Second)

		agentBase := revoked.agentTap.Len()
		sendTLSClientHello(t, revoked.gatewayAddr, revoked.relayHost)
		time.Sleep(750 * time.Millisecond)
		waitForStable(t, revoked.agentTap, 500*time.Millisecond, 5*time.Second)
		if revoked.agentTap.Len() != agentBase {
			t.Fatalf("revoked route still reached the agent (%d → %d)", agentBase, revoked.agentTap.Len())
		}
	})

	t.Run("tunnel-restart", func(t *testing.T) {
		restarted := newRelayStack(t)
		spec := tunnelSpec{
			label: "restart", agentID: "agent-restart", namespace: fixtureNamespace,
			generation: 1, proxyPort: restarted.allocPort(t), localPort: portOf(t, restarted.agentAddr),
		}
		restarted.startTunnel(spec)
		restarted.applyRoute(restarted.relayHost, spec, 1)
		restarted.waitOnline(spec, setupTimeout)
		restarted.waitRouteReady(restarted.relayHost, setupTimeout)

		rc := restarted.newRelayClient(tls.VersionTLS12, tls.VersionTLS13, []string{"http/1.1"}, 0)
		resp := doGet(t, rc.client, restarted.relayURL("/s/"+fixtureCode+"/items"), nil)
		resp.Body.Close()

		// Graceful stop delivers CloseProxy so presence drops promptly.
		restarted.stopTunnelGraceful(spec.label)
		waitOffline(t, restarted, spec, 20*time.Second)

		newSpec := spec
		restarted.startTunnel(newSpec)
		restarted.waitOnline(newSpec, setupTimeout)
		restarted.waitRouteReady(restarted.relayHost, setupTimeout)

		rc2 := restarted.newRelayClient(tls.VersionTLS12, tls.VersionTLS13, []string{"http/1.1"}, 0)
		resp2 := doGet(t, rc2.client, restarted.relayURL("/s/"+fixtureCode+"/thumb/img-2"), nil)
		if resp2.StatusCode != http.StatusOK {
			resp2.Body.Close()
			t.Fatalf("post-restart status = %d, want 200", resp2.StatusCode)
		}
		resp2.Body.Close()
	})
}

// TestRelayPathNeverEmitsOpenSignal asserts the relay data plane never takes
// the direct-path activation artifacts observable from this module
// (acceptance #7):
//
//   - the agent's direct-path probe endpoint (/connect) is never hit;
//   - every gateway dial — recorded through the REAL gateway dial seam
//     (gateway.WithDialer) — targets exactly the resolved route's loopback FRP
//     port, and at least one dial happened so the negative is not vacuous;
//   - the gateway route table resolves the relay origin (positive control)
//     but exposes no route for the direct origin; and
//   - a real TLS ClientHello carrying the direct origin's SNI reaches no agent
//     through the gateway.
//
// Scope: the agent-side SignalGate and port mapper live in the agent module
// (sharebridge/agent) and cannot be instantiated from this stdlib-only relay
// test package; their open-signal semantics are proven by
// agent/internal/direct/opensignal_test.go. This test does not claim to
// exercise them.
func TestRelayPathNeverEmitsOpenSignal(t *testing.T) {
	defer beginGateCase(t)()
	requireIntegration(t)
	s, spec := startPrimaryStack(t)

	relay := s.newRelayClient(tls.VersionTLS12, tls.VersionTLS13, []string{"http/1.1"}, 2).client
	for _, path := range []string{"/s/" + fixtureCode, "/s/" + fixtureCode + "/items", "/s/" + fixtureCode + "/asset/img-1"} {
		resp := doGet(t, relay, s.relayURL(path), nil)
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			t.Fatalf("relay GET %s status = %d, want 200", path, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// Real, wired collaborator evidence: the gateway dial seam recorded every
	// dial the relay data plane made.
	targets := s.dialTargetsSnapshot()
	if err := checkDialTargets(targets, spec.proxyPort); err != nil {
		t.Fatalf("relay path dial audit failed: %v (recorded targets: %v)", err, targets)
	}
	if got := s.content.connectCount(); got != 0 {
		t.Fatalf("relay content path emitted %d /connect open-signal probe(s), want 0", got)
	}

	// Positive control: the relay origin resolves, so the direct-origin
	// negative below is meaningful rather than vacuous.
	if _, err := s.routesTable.Lookup(s.relayHost); err != nil {
		t.Fatalf("relay origin %s did not resolve; the direct-origin negative would be vacuous: %v", s.relayHost, err)
	}
	if _, err := s.routesTable.Lookup(s.directHost); err == nil {
		t.Fatal("gateway route table resolved a direct origin; relay routing must not expose direct routes")
	}

	// Data-path negative: a real browser hello for the direct origin reaches no
	// agent through the gateway. Quiesce the agent tap first so the baseline is
	// not racily short of the earlier relayed traffic's final records.
	waitForStable(t, s.agentTap, 500*time.Millisecond, 5*time.Second)
	agentBase := s.agentTap.Len()
	sendTLSClientHello(t, s.gatewayAddr, s.directHost)
	time.Sleep(750 * time.Millisecond)
	waitForStable(t, s.agentTap, 500*time.Millisecond, 5*time.Second)
	if s.agentTap.Len() != agentBase {
		t.Fatalf("direct-origin SNI reached an agent through the gateway (%d -> %d)", agentBase, s.agentTap.Len())
	}
}

// TestRealFRPRelayPresenceHeartbeatDelayAndExpiry covers the §18.2/§23.7
// heartbeat requirement at the presence layer: one delayed Ping (a frozen
// frpc past a full 10-second interval) must not flap the 45-second presence
// lease, while a sustained absence must transition offline only at true
// lease expiry. The FRP-level cadence/expiry proof lives in
// relay/internal/frptest/heartbeat_gate_test.go (Task 8, §23.7 GO).
func TestRealFRPRelayPresenceHeartbeatDelayAndExpiry(t *testing.T) {
	defer beginGateCase(t)()
	requireIntegration(t)
	if testing.Short() {
		t.Skip("heartbeat lease timing test skipped in -short mode")
	}
	s, spec := startPrimaryStack(t)

	if !s.presence.Online(spec.agentID, spec.proxyPort, uint64(spec.generation)) {
		t.Fatal("tunnel not online before heartbeat test")
	}

	// Freeze the real frpc past one full heartbeat interval. No facts reach
	// the registry; the lease anchored at the last authenticated Ping (within
	// 10s before the freeze) must still hold.
	s.signalTunnel(t, spec.label, syscall.SIGSTOP)
	t.Cleanup(func() { s.signalTunnelQuiet(spec.label, syscall.SIGCONT) })
	time.Sleep(14 * time.Second)
	if !s.presence.Online(spec.agentID, spec.proxyPort, uint64(spec.generation)) {
		t.Fatal("presence flapped offline after one delayed Ping; want the 45s lease to tolerate a missed beat")
	}

	// Thaw: the cadence resumes and the tunnel is still online.
	s.signalTunnel(t, spec.label, syscall.SIGCONT)
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if s.presence.Online(spec.agentID, spec.proxyPort, uint64(spec.generation)) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Freeze again and require true lease expiry: the tunnel must remain
	// online for the next several seconds (not flap early) and then go offline
	// within the 45-second lease plus margin.
	s.signalTunnel(t, spec.label, syscall.SIGSTOP)
	time.Sleep(5 * time.Second)
	if !s.presence.Online(spec.agentID, spec.proxyPort, uint64(spec.generation)) {
		t.Fatal("presence expired far before the 45s lease")
	}
	waitOffline(t, s, spec, 75*time.Second)
}

// signalTunnel sends a signal to a running tunnel's frpc process.
func (s *relayStack) signalTunnel(t *testing.T, label string, signal syscall.Signal) {
	t.Helper()
	s.mu.Lock()
	tun := s.tunnels[label]
	s.mu.Unlock()
	if tun == nil || tun.cmd == nil || tun.cmd.Process == nil {
		t.Fatalf("cannot signal unknown tunnel %q", label)
	}
	if err := tun.cmd.Process.Signal(signal); err != nil {
		t.Fatalf("signal tunnel %s: %v", label, err)
	}
}

// signalTunnelQuiet sends a best-effort signal during cleanup.
func (s *relayStack) signalTunnelQuiet(label string, signal syscall.Signal) {
	s.mu.Lock()
	tun := s.tunnels[label]
	s.mu.Unlock()
	if tun == nil || tun.cmd == nil || tun.cmd.Process == nil {
		return
	}
	_ = tun.cmd.Process.Signal(signal)
}

// stopTunnelGraceful stops one frpc the way a supervised agent restart does:
// the admin stop command plus SIGINT, so frps observes CloseProxy and the
// presence registry transitions offline promptly.
func (s *relayStack) stopTunnelGraceful(label string) {
	s.mu.Lock()
	tun := s.tunnels[label]
	delete(s.tunnels, label)
	s.mu.Unlock()
	if tun == nil || tun.cmd == nil || tun.cmd.Process == nil {
		return
	}
	_ = exec.Command(s.frpcPath, "stop", "-c", tun.configPath).Run()
	_ = tun.cmd.Process.Signal(os.Interrupt)
	_, _ = tun.cmd.Process.Wait()
	tun.cmd = nil
}

func waitOffline(t *testing.T, s *relayStack, spec tunnelSpec, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !s.presence.Online(spec.agentID, spec.proxyPort, uint64(spec.generation)) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("tunnel %s never went offline after graceful stop", spec.label)
}

// sendTLSClientHello opens a TCP connection to addr and writes a real
// crypto/tls ClientHello for sni, then drops the connection. The handshake
// outcome is irrelevant: only the routing/forwarding of the hello bytes is
// under test.
func sendTLSClientHello(t *testing.T, addr, sni string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial gateway %s: %v", addr, err)
	}
	defer conn.Close()
	cfg := &tls.Config{
		ServerName:         sni,
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
		MaxVersion:         tls.VersionTLS13,
	}
	tlsConn := tls.Client(conn, cfg)
	_ = tlsConn.SetDeadline(time.Now().Add(5 * time.Second))
	_ = tlsConn.Handshake()
}

// openRelayTLS opens a real TLS session to the gateway for host and keeps it
// open, registering a gateway stream for the duration of the connection.
func openRelayTLS(t *testing.T, s *relayStack, host string) net.Conn {
	t.Helper()
	raw, err := net.DialTimeout("tcp", s.gatewayAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	tlsConn := tls.Client(raw, &tls.Config{
		ServerName: host,
		RootCAs:    s.pki.pool,
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		t.Fatalf("relay TLS handshake: %v", err)
	}
	return tlsConn
}

func waitForStreams(t *testing.T, s *relayStack, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.streams.Len() == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("gateway stream registry holds %d streams, want %d", s.streams.Len(), want)
}

func waitForBytes(t *testing.T, rec *byteRecorder, atLeast int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if rec.Len() >= atLeast {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("recorder still holds %d bytes, want at least %d", rec.Len(), atLeast)
}

// waitForStable blocks until rec's byte count has been unchanged for quiet
// (bounded by timeout), so asynchronous post-handshake reads do not race a
// leak assertion.
func waitForStable(t *testing.T, rec *byteRecorder, quiet, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := rec.Len()
	lastChange := time.Now()
	for time.Now().Before(deadline) {
		current := rec.Len()
		if current != last {
			last = current
			lastChange = time.Now()
		}
		if time.Since(lastChange) >= quiet {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// assertSameResponse requires two responses for the same request to be
// indistinguishable in status, content headers, and body bytes, returning the
// relay body.
func assertSameResponse(t *testing.T, label string, direct, relay *http.Response) []byte {
	t.Helper()
	directBody := readResponseBytes(t, direct)
	relayBody := readResponseBytes(t, relay)
	if direct.StatusCode != relay.StatusCode {
		t.Fatalf("%s: status direct=%d relay=%d", label, direct.StatusCode, relay.StatusCode)
	}
	for _, header := range []string{
		"Content-Type", "Content-Length", "Content-Range", "Content-Disposition",
		"Accept-Ranges", "Cache-Control",
	} {
		if got, want := relay.Header.Get(header), direct.Header.Get(header); got != want {
			t.Fatalf("%s: header %s direct=%q relay=%q", label, header, want, got)
		}
	}
	if !bytes.Equal(directBody, relayBody) {
		t.Fatalf("%s: body mismatch (direct %d bytes, relay %d bytes)", label, len(directBody), len(relayBody))
	}
	return relayBody
}
