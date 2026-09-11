// failure_test.go is the §18.5 relay failure/recovery suite (plan Task 33):
// independent gateway and frps restarts, a real agent restart (HTTPS server +
// tunnel) while idle and while a transfer is active, a post-navigation direct
// failure recovering through the canonical relay link, control-sync loss past
// the finite route lease, presence expiry without CloseProxy, and revoke
// during a long transfer.
//
// Every case composes the REAL data plane (pinned frps/frpc + real plugin,
// presence registry, route table and gateway) over loopback, exactly like the
// §23.3 gate. Every case is ALSO registered with the §23.3 required-mode gate
// registry (`defer beginGateCase(t)()` plus a requiredGateCases entry), so an
// unset environment or a dropped case can never be cited as green evidence;
// see TestFailureSuiteCasesAreGateRegistered.
package integration

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"sharebridge/relay/internal/gateway"
	"sharebridge/relay/internal/presence"
	"sharebridge/relay/internal/routes"
)

// failureClock is a mutex-guarded clock seam for the route table so the finite
// 120-second §14 route lease can be crossed without sleeping. It only ever
// moves forward.
type failureClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFailureClock() *failureClock {
	return &failureClock{now: time.Unix(1_700_000_000, 0)}
}

func (c *failureClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *failureClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// restartFrps kills the running frps and starts a fresh one from the same
// on-disk config, modelling an frps-only restart (§15.2). The gateway process,
// its plugin and the agent's frpc are untouched.
func (s *relayStack) restartFrps(t *testing.T) {
	t.Helper()
	if s.frpsCmd != nil && s.frpsCmd.Process != nil {
		_ = s.frpsCmd.Process.Kill()
		_, _ = s.frpsCmd.Process.Wait()
		s.frpsCmd = nil
	}
	s.startFrps()
}

// restartGatewayEmpty models a gateway process restart: the public acceptor is
// stopped, all in-memory presence is cleared (a fresh process boots with an
// empty registry, §15.1), the route table is replaced with a brand-new empty
// one, and a fresh acceptor is started on a new loopback listener. A real
// gateway restart must serve nothing until it has loaded a full snapshot and
// fresh presence has re-confirmed.
func (s *relayStack) restartGatewayEmpty(t *testing.T) {
	t.Helper()
	if s.gatewaySrv != nil {
		s.gatewaySrv.Close()
		s.gatewaySrv.Wait()
	}
	s.presence.ClearAll()

	// The streams registry is reused, not replaced: the presence registry's
	// §15.2 drain seam is fixed to it, and no pre-restart stream survives the
	// tunnel stop this helper is always called after.
	s.routesTable = routes.NewTable(s.presence)
	s.gatewaySrv = gateway.NewServer(s.routesTable, s.streams, gateway.WithDialer(s.recordDial))
	ln := listenLoopback(t)
	s.gatewayLn = ln
	s.gatewayAddr = ln.Addr().String()
	go func() { _ = s.gatewaySrv.Serve(ln) }()
}

// restartAgentHTTPServer restarts the agent's HTTPS listener and server the
// way an agent process restart does: the old server is closed (terminating
// in-flight responses and cancelling their handler contexts) and a fresh
// server is bound at the same loopback address with the same certificate,
// content handler and byte tap. The frpc tunnel is restarted separately; the
// agent daemon's persisted-state reload is covered at the daemon level by
// agent/internal/daemon's restart tests (see the case comment).
func (s *relayStack) restartAgentHTTPServer(t *testing.T) {
	t.Helper()
	address := s.agentAddr
	if s.agentSrv != nil {
		_ = s.agentSrv.Close()
	}
	var (
		listener net.Listener
		err      error
	)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		listener, err = net.Listen("tcp", address)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("rebind agent HTTPS listener at %s: %v", address, err)
	}
	s.agentTap = &byteRecorder{}
	s.agentLn = listener
	s.agentAddr = listener.Addr().String()
	s.agentSrv = &http.Server{
		Handler: s.content,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{s.pki.cert},
			NextProtos:   []string{"h2", "http/1.1"},
			MinVersion:   tls.VersionTLS12,
		},
	}
	go func() {
		_ = s.agentSrv.ServeTLS(&tapListener{Listener: listener, rec: s.agentTap}, "", "")
	}()
}

// useClockDrivenRoutes replaces the public acceptor and route table with a
// fresh pair whose finite route lease reads the injected clock, so a 120-second
// lease can be crossed deterministically. The presence registry is unchanged.
func (s *relayStack) useClockDrivenRoutes(t *testing.T, now func() time.Time) {
	t.Helper()
	if s.gatewaySrv != nil {
		s.gatewaySrv.Close()
		s.gatewaySrv.Wait()
	}
	// Reuse the streams registry so the presence registry's drain seam keeps
	// pointing at the live registry.
	s.routesTable = routes.NewTable(s.presence, routes.WithClock(now))
	s.gatewaySrv = gateway.NewServer(s.routesTable, s.streams, gateway.WithDialer(s.recordDial))
	ln := listenLoopback(t)
	s.gatewayLn = ln
	s.gatewayAddr = ln.Addr().String()
	go func() { _ = s.gatewaySrv.Serve(ln) }()
}

// expectRelayRequestFails asserts a fresh relay request is refused while the
// gateway is unready or the route/lease/presence join fails. Every rejection
// is a generic close, so the client surfaces a transport error (not a 5xx).
func expectRelayRequestFails(t *testing.T, s *relayStack, path string) {
	t.Helper()
	client := s.newRelayClient(tls.VersionTLS12, tls.VersionTLS13, []string{"http/1.1"}, 2).client
	defer client.CloseIdleConnections()
	req, err := http.NewRequest(http.MethodGet, s.relayURL(path), nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := client.Do(req)
	if err == nil {
		defer resp.Body.Close()
		t.Fatalf("relay request %s succeeded with status %d; the gateway must fail closed", path, resp.StatusCode)
	}
}

// openSlowRelayRequest opens an established relay request against the agent's
// /slow endpoint: the agent flushes a 200 and then holds the connection until
// its request context is cancelled. It returns the live response body together
// with the client so callers can quiesce and close idle connections.
func openSlowRelayRequest(t *testing.T, s *relayStack) (*http.Response, *http.Client) {
	t.Helper()
	client := s.newRelayClient(tls.VersionTLS12, tls.VersionTLS13, []string{"http/1.1"}, 2).client
	resp := doGet(t, client, s.relayURL("/s/"+fixtureCode+"/slow"), nil)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("slow relay status = %d, want 200 (established stream)", resp.StatusCode)
	}
	return resp, client
}

// requireGoroutinesReturn is the deadline-based leak assertion for the restart
// and revoke cases: after a failure tears a path down, the process goroutine
// count must settle back to at most the pre-failure baseline plus a small slack
// (transient HTTP-transport helpers). It polls to a deadline rather than
// sleeping for a fixed interval, so it neither flakes on slow machines nor
// passes a fixed sleep by luck.
func requireGoroutinesReturn(t *testing.T, baseline, slack int, label string, deadline time.Duration) {
	t.Helper()
	limit := time.Now().Add(deadline)
	for time.Now().Before(limit) {
		runtime.Gosched()
		if runtime.NumGoroutine() <= baseline+slack {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s: goroutines did not settle back to the %d baseline (+%d slack); still %d after %s",
		label, baseline, slack, runtime.NumGoroutine(), deadline)
}

// requireStreamStaysOpen asserts an established stream is still open (no EOF
// or error) for a bounded observation window, distinguishing "preserved" from
// "drained". A read that returns before the deadline is a drain.
func requireStreamStaysOpen(t *testing.T, label string, resp *http.Response, window time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		buffer := make([]byte, 1)
		_, err := resp.Body.Read(buffer)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("%s: established stream ended during the observation window (read returned %v); it must be preserved", label, err)
	case <-time.After(window):
	}
}

// requireStreamClosed asserts an established stream terminates within the
// deadline (revoke, frps death, or agent death must tear it down).
func requireStreamClosed(t *testing.T, label string, resp *http.Response, deadline time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		buffer := make([]byte, 1)
		_, err := resp.Body.Read(buffer)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("%s: established stream returned a byte instead of closing", label)
		}
		resp.Body.Close()
	case <-time.After(deadline):
		t.Fatalf("%s: established stream was still open after %s; it must be torn down", label, deadline)
	}
}

func waitSlowCancelled(t *testing.T, s *relayStack, deadline time.Duration) {
	t.Helper()
	limit := time.Now().Add(deadline)
	for time.Now().Before(limit) {
		select {
		case <-s.content.slowCancelled:
			return
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the agent never observed cancellation of the slow transfer within %s", deadline)
}

// TestRealFRPGatewayRestartServesNothingUntilSnapshotAndPresence proves §15.1:
// a restarted gateway boots with an empty route table and no presence, serves
// nothing until a full route snapshot has been loaded, and only serves again
// after fresh Login + authorized NewProxy + probe-confirmed NewUserConn.
func TestRealFRPGatewayRestartServesNothingUntilSnapshotAndPresence(t *testing.T) {
	defer beginGateCase(t)()
	requireIntegration(t)
	s, spec := startPrimaryStack(t)

	// Positive control: the pre-restart stack serves the route.
	resp := doGet(t, s.newRelayClient(tls.VersionTLS12, tls.VersionTLS13, []string{"http/1.1"}, 2).client, s.relayURL("/s/"+fixtureCode), nil)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("pre-restart relay status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// The prior tunnel cannot survive a real gateway restart: stop it, then
	// restart the gateway into empty route and presence state.
	baseline := runtime.NumGoroutine()
	s.stopTunnel(spec.label)
	s.restartGatewayEmpty(t)

	// Empty route table: nothing is served, not even the route that was live.
	if _, err := s.routesTable.Lookup(s.relayHost); !errors.Is(err, routes.ErrRouteNotFound) {
		t.Fatalf("restarted gateway lookup error = %v, want ErrRouteNotFound (empty table)", err)
	}
	expectRelayRequestFails(t, s, "/s/"+fixtureCode)
	// The old acceptor's handler goroutines drained before the replacement
	// was published (B1 ordering), so the restart leaves no handler behind.
	requireGoroutinesReturn(t, baseline, 8, "gateway restart", 5*time.Second)

	// A loaded route alone is not enough: presence has not re-confirmed, so the
	// join still fails closed and the route is not routable.
	s.applyRoute(s.relayHost, spec, 1)
	if _, err := s.routesTable.Lookup(s.relayHost); !errors.Is(err, routes.ErrPresenceAbsent) {
		t.Fatalf("lookup after snapshot but before presence error = %v, want ErrPresenceAbsent", err)
	}
	expectRelayRequestFails(t, s, "/s/"+fixtureCode)

	// A fresh tunnel (new one-use credential) re-registers and confirms
	// presence; only then does the route carry traffic.
	s.startTunnel(spec)
	s.waitOnline(spec, setupTimeout)
	s.waitRouteReady(s.relayHost, setupTimeout)

	client := s.newRelayClient(tls.VersionTLS12, tls.VersionTLS13, []string{"http/1.1"}, 2).client
	resp = doGet(t, client, s.relayURL("/s/"+fixtureCode), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-restart relay status = %d, want 200 after re-confirmation", resp.StatusCode)
	}
	_ = readResponseBytes(t, resp)
}

// TestRealFRPFRPSRestartClearsPresenceAndTerminatesEstablishedStreams proves
// §15.2 on the REAL data plane: an frps-only restart (the gateway and its
// plugin stay up, and the agent's frpc keeps running exactly as in
// production) clears the tunnel's presence IMMEDIATELY through the production
// trigger — frpc's reconnect re-presents its already-burned one-use
// credential, the plugin's replay rejection is the frps session-reset
// lifecycle signal, and the presence registry clears the agent and drains its
// established streams. The test never calls ClearAll. The route state remains
// but is unroutable until fresh Login/NewProxy/probe-confirmed NewUserConn
// re-registers with a newly issued one-use credential.
func TestRealFRPFRPSRestartClearsPresenceAndTerminatesEstablishedStreams(t *testing.T) {
	defer beginGateCase(t)()
	requireIntegration(t)
	s, spec := startPrimaryStack(t)
	baseline := runtime.NumGoroutine()

	// Establish a live relay stream through the real FRP data plane.
	established, slowClient := openSlowRelayRequest(t, s)
	if s.streams.Len() != 1 {
		t.Fatalf("gateway stream registry holds %d streams, want 1 before the restart", s.streams.Len())
	}

	// frps-only restart: the gateway process and its plugin stay up, and the
	// agent's frpc is NOT stopped (a production frps restart does not kill
	// frpc; it reconnects and re-presents its burned credential).
	s.restartFrps(t)

	// The data plane died with frps: the established stream terminates.
	requireStreamClosed(t, "frps restart", established, 15*time.Second)
	slowClient.CloseIdleConnections()

	// §15.2 immediate presence clear, driven by the production trigger: no
	// test-side ClearAll and no 45-second lease wait. waitOffline is bounded
	// well inside the lease, so a lingering online presence is a real failure.
	waitOffline(t, s, spec, 15*time.Second)
	if s.presence.Online(spec.agentID, spec.proxyPort, uint64(spec.generation)) {
		t.Fatal("presence survived the frps reset; §15.2 requires an immediate clear")
	}
	// Route state remains, but the presence join fails closed: new connections
	// are refused until the tunnel re-registers.
	if _, err := s.routesTable.Lookup(s.relayHost); !errors.Is(err, routes.ErrPresenceAbsent) {
		t.Fatalf("lookup after frps reset error = %v, want ErrPresenceAbsent", err)
	}
	expectRelayRequestFails(t, s, "/s/"+fixtureCode)
	requireGoroutinesReturn(t, baseline, 8, "frps restart", 5*time.Second)

	// Re-establishment: stop the stale-credential retry loop and re-register
	// with a fresh one-use credential (the agent's post-restart
	// relay_credential_request path, §11.1/§15.2).
	s.stopTunnel(spec.label)
	s.startTunnel(spec)
	s.waitOnline(spec, setupTimeout)
	s.waitRouteReady(s.relayHost, setupTimeout)

	client := s.newRelayClient(tls.VersionTLS12, tls.VersionTLS13, []string{"http/1.1"}, 2).client
	resp := doGet(t, client, s.relayURL("/s/"+fixtureCode), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("relay status after re-registration = %d, want 200", resp.StatusCode)
	}
	_ = readResponseBytes(t, resp)
}

// TestRealFRPAgentRestartDuringIdleAndActiveTransfers proves §15.3 for both
// the idle and active cases. The integration harness has no agent daemon (the
// agent side is the fake Phase 3 HTTPS server plus the real frpc child), so a
// true in-process daemon restart is infeasible here. The closest faithful
// equivalent is exercised: the agent's HTTPS server is stopped and rebound
// (terminating the in-flight transfer and cancelling its handler), the frpc
// tunnel is restarted with a fresh one-use credential, and the route — which
// is control state and must survive — is NOT reapplied. The daemon-level
// guarantees the harness cannot reach are covered by
// agent/internal/daemon TestAgentRestartHydratesBeforeContentReady (persisted
// sessions reloaded and hydrated before re-registration),
// TestDaemonShutdownStopsHTTPSAndFRPC (HTTPS + frpc child stop together), and
// TestFRPCRestartObtainsFreshCredentialAfterReplayRejection (fresh credential
// after a replay rejection).
func TestRealFRPAgentRestartDuringIdleAndActiveTransfers(t *testing.T) {
	defer beginGateCase(t)()
	requireIntegration(t)
	s, spec := startPrimaryStack(t)

	t.Run("active transfer", func(t *testing.T) {
		baseline := runtime.NumGoroutine()
		established, slowClient := openSlowRelayRequest(t, s)

		// Real agent restart, server first (as Daemon.Stop does): the HTTPS
		// server stops, terminating the in-flight response.
		s.restartAgentHTTPServer(t)
		requireStreamClosed(t, "agent HTTPS restart", established, 15*time.Second)
		waitSlowCancelled(t, s, 15*time.Second)

		// The tunnel child stops with the agent.
		s.stopTunnel(spec.label)
		slowClient.CloseIdleConnections()
		requireGoroutinesReturn(t, baseline, 8, "agent restart", 5*time.Second)

		// The gateway must fail closed while the tunnel is down: a fresh relay
		// request cannot silently reuse the old tunnel state.
		expectRelayRequestFails(t, s, "/s/"+fixtureCode)

		// Recovery: the restarted agent re-registers with a fresh credential;
		// the route is NOT reapplied, proving route/lease state survived the
		// agent restart.
		s.startTunnel(spec)
		s.waitOnline(spec, setupTimeout)
		s.waitRouteReady(s.relayHost, setupTimeout)
		client := s.newRelayClient(tls.VersionTLS12, tls.VersionTLS13, []string{"http/1.1"}, 2).client
		resp := doGet(t, client, s.relayURL("/s/"+fixtureCode), nil)
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			t.Fatalf("relay status after active restart = %d, want 200", resp.StatusCode)
		}
		resp.Body.Close()
	})

	t.Run("idle restart", func(t *testing.T) {
		s.restartAgentHTTPServer(t)
		s.stopTunnel(spec.label)
		s.startTunnel(spec)
		s.waitOnline(spec, setupTimeout)
		s.waitRouteReady(s.relayHost, setupTimeout)
		client := s.newRelayClient(tls.VersionTLS12, tls.VersionTLS13, []string{"http/1.1"}, 2).client
		resp := doGet(t, client, s.relayURL("/s/"+fixtureCode+"/items"), nil)
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			t.Fatalf("relay status after idle restart = %d, want 200", resp.StatusCode)
		}
		resp.Body.Close()
	})
}

// TestRealFRPDirectFailureAfterNavigationRecoversThroughCanonicalRelayLink
// covers §15.6's "failure after navigation" case as far as a browserless
// harness faithfully can: Phase 4a never splices an in-flight response across
// origins, so a direct transfer that dies after the recipient has already
// navigated cannot be continued; the recipient returns through the canonical
// link. The harness models the already-navigated direct transfer as an
// established direct response that fails mid-body, then proves the recipient's
// return through the canonical RELAY link is a fresh, complete transaction
// that carries the same content — not a continuation or splice of the failed
// direct response. It does not claim to exercise browser or interstitial
// behaviour (those live in control/agent and are covered there).
func TestRealFRPDirectFailureAfterNavigationRecoversThroughCanonicalRelayLink(t *testing.T) {
	defer beginGateCase(t)()
	requireIntegration(t)
	s, spec := startPrimaryStack(t)

	// Baseline: the content the already-navigated recipient had (direct origin).
	direct := s.newDirectClient()
	baseline := readResponseBytes(t, doGet(t, direct, s.directURL("/s/"+fixtureCode+"/items"), nil))

	// An in-flight direct transfer that fails after navigation: establish the
	// response, then abort it (the direct connection dies).
	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.directURL("/s/"+fixtureCode+"/slow"), nil)
	if err != nil {
		cancel()
		t.Fatalf("build direct request: %v", err)
	}
	directResponse, err := direct.Do(request)
	if err != nil {
		cancel()
		t.Fatalf("start in-flight direct transfer: %v", err)
	}
	if directResponse.StatusCode != http.StatusOK {
		directResponse.Body.Close()
		cancel()
		t.Fatalf("in-flight direct transfer status = %d, want 200", directResponse.StatusCode)
	}
	cancel()
	if _, readErr := io.ReadAll(directResponse.Body); readErr == nil {
		t.Fatal("the aborted direct transfer completed; post-navigation direct failure was not simulated")
	}
	directResponse.Body.Close()
	waitSlowCancelled(t, s, 15*time.Second)

	// The recipient returns through the canonical relay link: a fresh and
	// COMPLETE transaction, byte-identical to the direct baseline.
	relay := s.newRelayClient(tls.VersionTLS12, tls.VersionTLS13, []string{"http/1.1"}, 2).client
	recovered := readResponseBytes(t, doGet(t, relay, s.relayURL("/s/"+fixtureCode+"/items"), nil))
	if !bytes.Equal(recovered, baseline) {
		t.Fatalf("relay recovery body differs from the direct baseline (got %d bytes, want %d); the post-navigation recovery must be a complete canonical transaction",
			len(recovered), len(baseline))
	}
	// The recovery rode the relay data plane exactly: dials target only the
	// resolved route's loopback FRP port.
	if err := checkDialTargets(s.dialTargetsSnapshot(), spec.proxyPort); err != nil {
		t.Fatalf("relay recovery dial audit failed: %v", err)
	}
}

// TestRealFRPControlSyncLossPastRouteLeaseKeepsEstablishedStream proves §15.4:
// when control cannot renew route state, the finite 120-second route lease
// expiry blocks only NEW connections; established streams keep running under
// their own idle and absolute-lifetime limits until an explicit revoke or
// lockdown.
func TestRealFRPControlSyncLossPastRouteLeaseKeepsEstablishedStream(t *testing.T) {
	defer beginGateCase(t)()
	requireIntegration(t)
	s := newRelayStack(t)
	clock := newFailureClock()
	s.useClockDrivenRoutes(t, clock.Now)

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

	baseline := runtime.NumGoroutine()
	established, slowClient := openSlowRelayRequest(t, s)

	// Control stops renewing: cross the §14 120-second lease with no applied
	// route update.
	clock.Advance(routes.RouteLeaseTTL + time.Second)

	if _, err := s.routesTable.Lookup(s.relayHost); !errors.Is(err, routes.ErrRouteLeaseExpired) {
		t.Fatalf("lookup past the route lease error = %v, want ErrRouteLeaseExpired", err)
	}
	expectRelayRequestFails(t, s, "/s/"+fixtureCode)

	// The established stream is untouched by lease expiry (no drain).
	requireStreamStaysOpen(t, "route-lease expiry", established, 1500*time.Millisecond)
	if s.streams.Len() != 1 {
		t.Fatalf("gateway stream registry holds %d streams, want the established 1 preserved across lease expiry", s.streams.Len())
	}
	established.Body.Close()
	slowClient.CloseIdleConnections()
	requireGoroutinesReturn(t, baseline, 8, "route-lease expiry", 5*time.Second)
}

// TestRealFRPRevokeDuringLongTransferClosesEstablishedStream proves §15.6:
// an explicit revoke (tombstone then drain, mutate-before-drain) closes an
// active long relay transfer and blocks new connections, while a missing
// control refresh (lease expiry, the previous case) does not.
func TestRealFRPRevokeDuringLongTransferClosesEstablishedStream(t *testing.T) {
	defer beginGateCase(t)()
	requireIntegration(t)
	s, _ := startPrimaryStack(t)
	baseline := runtime.NumGoroutine()

	established, slowClient := openSlowRelayRequest(t, s)
	if s.streams.Len() != 1 {
		t.Fatalf("gateway stream registry holds %d streams, want 1", s.streams.Len())
	}

	// Explicit revoke: mutate the route table first, then drain the exact
	// route's established streams (the Streams.RegisterAdmitted ordering
	// invariant).
	if err := s.routesTable.Revoke(s.relayHost, 2); err != nil {
		t.Fatalf("revoke route: %v", err)
	}
	if closed := s.streams.CloseRoute(s.relayHost); closed != 1 {
		t.Fatalf("CloseRoute closed %d streams, want exactly 1", closed)
	}

	requireStreamClosed(t, "explicit revoke", established, 10*time.Second)
	waitSlowCancelled(t, s, 10*time.Second)
	slowClient.CloseIdleConnections()
	requireGoroutinesReturn(t, baseline, 8, "explicit revoke", 5*time.Second)

	if _, err := s.routesTable.Lookup(s.relayHost); !errors.Is(err, routes.ErrRouteInactive) {
		t.Fatalf("lookup of revoked route error = %v, want ErrRouteInactive", err)
	}
	expectRelayRequestFails(t, s, "/s/"+fixtureCode)
}

// TestRealFRPPresenceExpiryWithoutCloseProxy proves §18.5's bounded offline
// detection on the REAL data plane: the real frpc is frozen (SIGSTOP) so its
// Pings stop while its control connection stays open — frps therefore never
// delivers CloseProxy — and the 45-second presence lease expires on its own at
// the exact boundary. The registry runs on an injected clock so the boundary
// is deterministic, but the tunnel, route, stream registry and content plane
// are all the real stack. The established stream (which the gateway never
// re-checks on expiry) is preserved; only the frps-reset clear path or an
// explicit revoke drains established streams.
func TestRealFRPPresenceExpiryWithoutCloseProxy(t *testing.T) {
	defer beginGateCase(t)()
	requireIntegration(t)

	clock := newFailureClock()
	s := newRelayStackWithClock(t, clock.Now)
	spec := tunnelSpec{
		label:      "primary",
		agentID:    "agent-expiry",
		namespace:  fixtureNamespace,
		generation: 1,
		proxyPort:  s.allocPort(t),
		localPort:  portOf(t, s.agentAddr),
	}
	s.startTunnel(spec)
	s.applyRoute(s.relayHost, spec, 1)
	s.waitOnline(spec, setupTimeout)
	s.waitRouteReady(s.relayHost, setupTimeout)

	baseline := runtime.NumGoroutine()
	established, slowClient := openSlowRelayRequest(t, s)
	if s.streams.Len() != 1 {
		t.Fatalf("gateway stream registry holds %d streams, want the established 1", s.streams.Len())
	}

	// Freeze the real frpc: Pings stop, the control connection stays open, and
	// no CloseProxy can arrive.
	s.signalTunnel(t, spec.label, syscall.SIGSTOP)
	t.Cleanup(func() { s.signalTunnelQuiet(spec.label, syscall.SIGCONT) })
	time.Sleep(200 * time.Millisecond)

	// One missed Ping window is tolerated, then the exact lease boundary ends
	// authority with no CloseProxy.
	clock.Advance(presence.DefaultLeaseTTL - time.Second)
	if !s.presence.Online(spec.agentID, spec.proxyPort, uint64(spec.generation)) {
		t.Fatal("presence flapped before the 45s lease boundary")
	}
	clock.Advance(time.Second)
	if s.presence.Online(spec.agentID, spec.proxyPort, uint64(spec.generation)) {
		t.Fatal("presence survived the exact lease boundary without a Ping or CloseProxy")
	}
	if _, err := s.routesTable.Lookup(s.relayHost); !errors.Is(err, routes.ErrPresenceAbsent) {
		t.Fatalf("lookup after lease expiry error = %v, want ErrPresenceAbsent", err)
	}

	// Expiry must not drain established streams (§15.4); only the frps-reset
	// clear path does.
	requireStreamStaysOpen(t, "presence lease expiry", established, 1500*time.Millisecond)
	if s.streams.Len() != 1 {
		t.Fatalf("gateway stream registry holds %d streams after lease expiry, want the established 1 preserved", s.streams.Len())
	}

	// Bounded offline detection: exactly one online transition then one
	// offline transition carrying the current boot ID and a fresh revision.
	events := s.presenceEvents.snapshot()
	if len(events) != 2 || events[1].State != presence.StateOffline {
		t.Fatalf("presence events = %+v, want one online then one offline", events)
	}
	if events[1].BootID != "integration-boot" || events[1].Revision <= events[0].Revision {
		t.Fatalf("offline event = %+v, want boot %q and a fresh revision", events[1], "integration-boot")
	}

	established.Body.Close()
	slowClient.CloseIdleConnections()
	requireGoroutinesReturn(t, baseline, 8, "presence lease expiry", 5*time.Second)
}
