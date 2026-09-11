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
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"sharebridge/relay/internal/frpplugin"
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

// TestRealFRPAgentHTTPSReplacementAndFRPCRestartDuringIdleAndActiveTransfers
// proves §15.3 for both the idle and active cases as far as the relay-only
// harness faithfully can. This harness has NO agent daemon (the agent side is
// the fake Phase 3 HTTPS server plus the real frpc child), so a true in-process
// daemon restart is not representable here. What is exercised is precisely:
// the agent's HTTPS listener is stopped and rebound (terminating the in-flight
// transfer and cancelling its handler), and the frpc child is restarted with a
// fresh one-use credential; the route — which is control state and must
// survive — is NOT reapplied. No claim is made that a daemon restarted. The
// daemon-level guarantees the harness cannot reach are covered in
// agent/internal/daemon by TestAgentRestartHydratesBeforeContentReady
// (persisted sessions reloaded and hydrated before re-registration),
// TestDaemonShutdownStopsHTTPSAndFRPC (HTTPS + frpc child stop together), and
// TestFRPCRestartObtainsFreshCredentialAfterReplayRejection (fresh credential
// after a replay rejection).
func TestRealFRPAgentHTTPSReplacementAndFRPCRestartDuringIdleAndActiveTransfers(t *testing.T) {
	defer beginGateCase(t)()
	requireIntegration(t)
	s, spec := startPrimaryStack(t)

	t.Run("active transfer", func(t *testing.T) {
		baseline := runtime.NumGoroutine()
		established, slowClient := openSlowRelayRequest(t, s)

		// The agent's HTTPS listener is stopped first (as Daemon.Stop does),
		// terminating the in-flight response.
		s.restartAgentHTTPServer(t)
		requireStreamClosed(t, "agent HTTPS replacement", established, 15*time.Second)
		waitSlowCancelled(t, s, 15*time.Second)

		// The tunnel child stops with the agent.
		s.stopTunnel(spec.label)
		slowClient.CloseIdleConnections()
		requireGoroutinesReturn(t, baseline, 8, "agent HTTPS replacement", 5*time.Second)

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

// TestRealFRPDirectOriginFailureMidTransferRecoversThroughCanonicalRelayLink
// covers §15.6's "failure after navigation" case as far as this browserless
// harness faithfully can: Phase 4a never splices an in-flight response across
// origins, so a direct transfer that dies after the recipient has already
// navigated cannot be continued; the recipient returns through the canonical
// link. The harness models the already-navigated direct transfer as an
// established direct response, and then makes the direct ORIGIN unreachable
// mid-body (the agent's HTTPS listener is closed, so the transfer fails from
// the origin side — not because the test client cancelled its own request).
// The recipient's return through the canonical RELAY link must be a fresh and
// complete transaction carrying the same content — not a continuation or
// splice of the failed direct response — and the frpc tunnel was never
// restarted, so the route/lease state that serves it survived.
//
// Scope, stated precisely: this harness has one agent origin, so the relay
// path is down during the same window in which the direct origin is; the
// recovery is exercised once the origin is serving again. Browser and
// interstitial behaviour is NOT exercised here (it lives in control/agent and
// is covered by TestInterstitialNeverEmbedsContentIframe and
// TestRelayPathNeverEmitsOpenSignal, which remain separate architectural
// guards).
func TestRealFRPDirectOriginFailureMidTransferRecoversThroughCanonicalRelayLink(t *testing.T) {
	defer beginGateCase(t)()
	requireIntegration(t)
	s, spec := startPrimaryStack(t)

	// Baseline: the content the already-navigated recipient had (direct origin).
	direct := s.newDirectClient()
	baseline := readResponseBytes(t, doGet(t, direct, s.directURL("/s/"+fixtureCode+"/items"), nil))

	// An in-flight direct transfer: establish the response, then make the direct
	// origin unreachable mid-body. The failure is origin-side (the agent's HTTPS
	// listener closes), never a client cancellation.
	directResponse := doGet(t, direct, s.directURL("/s/"+fixtureCode+"/slow"), nil)
	if directResponse.StatusCode != http.StatusOK {
		directResponse.Body.Close()
		t.Fatalf("in-flight direct transfer status = %d, want 200", directResponse.StatusCode)
	}
	// Close the direct origin mid-transfer and bring it back serving again.
	// The frpc tunnel itself is never stopped, so no re-registration is involved.
	s.restartAgentHTTPServer(t)
	if _, readErr := io.ReadAll(directResponse.Body); readErr == nil {
		t.Fatal("the direct transfer completed after its origin became unreachable; post-navigation direct failure was not simulated")
	}
	directResponse.Body.Close()
	waitSlowCancelled(t, s, 15*time.Second)

	// The canonical link recovers without the tunnel being restarted.
	if !s.presence.Online(spec.agentID, spec.proxyPort, uint64(spec.generation)) {
		t.Fatal("the tunnel went offline across the direct-origin failure; the recovery must ride the surviving relay tunnel")
	}
	s.waitRouteReady(s.relayHost, setupTimeout)
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

// replayLoginCredential re-presents a credential to the real plugin endpoint
// exactly as a reconnecting frpc does: the same Login op and credential
// metadata. The plugin must reject it (its jti is already burned) and, per
// §15.2, emit the session-reset fact for the session that credential belonged
// to.
func (s *relayStack) replayLoginCredential(t *testing.T, token string, spec tunnelSpec, runID string) {
	t.Helper()
	envelope := map[string]any{
		"version": frpplugin.FRPPluginAPIVersion,
		"op":      "Login",
		"content": map[string]any{
			"version":        "0.71.0",
			"hostname":       "sharebridge-agent",
			"os":             "linux",
			"arch":           "amd64",
			"user":           "",
			"timestamp":      time.Now().Unix(),
			"run_id":         runID,
			"pool_count":     0,
			"client_address": "127.0.0.1:1",
			"metas": map[string]string{
				frpplugin.CredentialMetadataKey: token,
				frpplugin.GenerationMetadataKey: strconv.Itoa(spec.generation),
			},
		},
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal replayed Login: %v", err)
	}
	url := fmt.Sprintf("http://%s:%s@%s%s?version=%s&op=Login",
		frpplugin.PluginAuthUsername, pluginSharedSecret, s.pluginLn.Addr(), frpplugin.APIPath, frpplugin.FRPPluginAPIVersion)
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build replayed Login: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("replay Login against the plugin: %v", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read replayed Login response: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("replayed Login status = %d, want 200 (plugin reject), body=%s", response.StatusCode, raw)
	}
	var decoded struct {
		Reject bool `json:"reject"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode replayed Login response: %v; body=%s", err, raw)
	}
	if !decoded.Reject {
		t.Fatalf("a replayed one-use credential was admitted: %s", raw)
	}
}

// tunnelToken returns the exact one-use credential an already-started tunnel
// was given, so a test can re-present it byte-for-byte.
func (s *relayStack) tunnelToken(t *testing.T, label string) string {
	t.Helper()
	s.mu.Lock()
	tun := s.tunnels[label]
	s.mu.Unlock()
	if tun == nil || tun.token == "" {
		t.Fatalf("tunnel %q has no credential to replay", label)
	}
	return tun.token
}

// waitRelayPortReleased waits for frps to stop listening on a relay port after
// its frpc died, so a replacement session can re-register the same port.
func waitRelayPortReleased(t *testing.T, port int, timeout time.Duration) {
	t.Helper()
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 200*time.Millisecond)
		if err != nil {
			return
		}
		_ = conn.Close()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("frps still held relay port %d after its frpc stopped", port)
}

// TestRealFRPStaleSessionReplayDoesNotClearAHealthyReplacementTunnel pins the
// PRECISION of the §15.2 production trigger on the real data plane. A replayed
// one-use credential is evidence that the session that credential belonged to
// is dead — not that the agent's presence is stale — so a replay of a
// credential that a healthy replacement generation has already superseded must
// clear nothing: the live generation-2 tunnel, its established stream, and its
// public path all survive. The reset still fires for the session it names.
func TestRealFRPStaleSessionReplayDoesNotClearAHealthyReplacementTunnel(t *testing.T) {
	defer beginGateCase(t)()
	requireIntegration(t)
	s, spec := startPrimaryStack(t)

	// Capture the exact burned generation-1 credential before its frpc stops,
	// then supersede it with a fresh generation-2 credential for the same
	// agent and relay port (the normal post-exit re-credential path).
	staleToken := s.tunnelToken(t, spec.label)
	s.stopTunnel(spec.label)
	waitRelayPortReleased(t, spec.proxyPort, 10*time.Second)
	replacement := spec
	replacement.label = "replacement"
	replacement.generation = 2
	s.startTunnel(replacement)
	s.applyRoute(s.relayHost, replacement, 2)
	s.waitOnline(replacement, setupTimeout)
	s.waitRouteReady(s.relayHost, setupTimeout)

	baseline := runtime.NumGoroutine()
	established, slowClient := openSlowRelayRequest(t, s)
	if s.streams.Len() != 1 {
		t.Fatalf("gateway stream registry holds %d streams, want the established 1", s.streams.Len())
	}

	// The stale generation-1 credential is replayed while generation 2 is
	// healthy. The reset names the dead generation-1 session only.
	s.replayLoginCredential(t, staleToken, spec, "run-stale-replay")

	// Bounded observation: the healthy replacement tunnel must remain online.
	// The old behaviour cleared every tunnel of the agent, so this fails as
	// soon as the imprecise reset is dispatched.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !s.presence.Online(replacement.agentID, replacement.proxyPort, uint64(replacement.generation)) {
			t.Fatalf("a stale generation-1 credential replay cleared the healthy generation-2 tunnel")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The established stream and the public path both survive the replay.
	requireStreamStaysOpen(t, "stale-credential replay", established, 1500*time.Millisecond)
	if s.streams.Len() != 1 {
		t.Fatalf("gateway stream registry holds %d streams after the stale replay, want the established 1 preserved", s.streams.Len())
	}
	client := s.newRelayClient(tls.VersionTLS12, tls.VersionTLS13, []string{"http/1.1"}, 2).client
	resp := doGet(t, client, s.relayURL("/s/"+fixtureCode+"/items"), nil)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("relay status after the stale replay = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	established.Body.Close()
	slowClient.CloseIdleConnections()
	requireGoroutinesReturn(t, baseline, 8, "stale-session replay precision", 5*time.Second)

	// The same trigger still clears the session it actually names: replaying
	// the live generation-2 credential takes the healthy tunnel offline.
	s.replayLoginCredential(t, s.tunnelToken(t, replacement.label), replacement, "run-live-replay")
	waitOffline(t, s, replacement, 10*time.Second)
}

// TestRealFRPSameGenerationRecredentialSurvivesOlderCredentialReplay pins
// CREDENTIAL precision — not merely generation precision — of the §15.2
// production trigger on the real data plane. A same-generation re-credential
// (fresh one-use credential, strictly newer issued-at) is admitted as an
// authoritative replacement of the previous session at the SAME (agent, relay
// port, generation) join key; this is the normal post-exit recovery path. A
// delayed replay of the SUPERSEDED credential therefore names a dead session
// whose join key is now held by a healthy replacement, so it must be a no-op:
// the replacement's presence, its established stream and its public path all
// survive and nothing is drained. Only a replay of the credential that IS the
// current recorded identity clears the session. Without credential identity
// the replay clears the identical join key and tears the healthy replacement
// down, which is exactly the A1/A2 defect this case guards.
func TestRealFRPSameGenerationRecredentialSurvivesOlderCredentialReplay(t *testing.T) {
	defer beginGateCase(t)()
	requireIntegration(t)
	s, spec := startPrimaryStack(t)

	// Capture the exact burned credential of the first session, then replace it
	// with a fresh credential for the SAME (agent, port, generation).
	staleToken := s.tunnelToken(t, spec.label)
	s.stopTunnelGraceful(spec.label)
	waitRelayPortReleased(t, spec.proxyPort, 10*time.Second)
	// The dead session's presence must be absent before the replacement is
	// admitted at the SAME join key. Otherwise "online" could still describe
	// the dead session rather than the replacement, and the established-stream
	// assertion would race the new tunnel's registration.
	waitOffline(t, s, spec, 15*time.Second)
	replacement := spec
	replacement.label = "same-generation-replacement"
	// replacement.generation is deliberately left equal to spec.generation.
	s.startTunnel(replacement)
	s.applyRoute(s.relayHost, replacement, 2)
	s.waitOnline(replacement, setupTimeout)
	s.waitRouteReady(s.relayHost, setupTimeout)

	baseline := runtime.NumGoroutine()
	established, slowClient := openSlowRelayRequest(t, s)
	if s.streams.Len() != 1 {
		t.Fatalf("gateway stream registry holds %d streams, want the established 1", s.streams.Len())
	}

	// The superseded same-generation credential is replayed while its
	// replacement is healthy. The reset names the same join key, so only
	// credential precision can keep the healthy session alive.
	s.replayLoginCredential(t, staleToken, spec, "run-stale-same-generation-replay")

	// Bounded observation: the healthy same-generation replacement must remain
	// online. The old join-key-only reset cleared it immediately, so this fails
	// as soon as the imprecise reset is dispatched.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !s.presence.Online(replacement.agentID, replacement.proxyPort, uint64(replacement.generation)) {
			t.Fatalf("a delayed replay of the superseded same-generation credential cleared the healthy replacement session")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The established stream and the public path both survive the replay.
	requireStreamStaysOpen(t, "same-generation stale-credential replay", established, 1500*time.Millisecond)
	if s.streams.Len() != 1 {
		t.Fatalf("gateway stream registry holds %d streams after the stale same-generation replay, want the established 1 preserved", s.streams.Len())
	}
	client := s.newRelayClient(tls.VersionTLS12, tls.VersionTLS13, []string{"http/1.1"}, 2).client
	resp := doGet(t, client, s.relayURL("/s/"+fixtureCode+"/items"), nil)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("relay status after the stale same-generation replay = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	established.Body.Close()
	slowClient.CloseIdleConnections()
	requireGoroutinesReturn(t, baseline, 8, "same-generation replay precision", 5*time.Second)

	// A genuine reset still works: replaying the credential that IS the current
	// recorded identity clears the session it names.
	s.replayLoginCredential(t, s.tunnelToken(t, replacement.label), replacement, "run-live-same-generation-replay")
	waitOffline(t, s, replacement, 10*time.Second)
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
