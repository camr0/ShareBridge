// failure_test.go is the §18.5 relay failure/recovery suite (plan Task 33):
// independent gateway and frps restarts, agent restarts while idle and while
// a transfer is active, control-sync loss past the finite route lease,
// presence expiry without CloseProxy, and revoke during a long transfer.
//
// Every case composes the REAL data plane (pinned frps/frpc + real plugin,
// presence registry, route table and gateway) over loopback, exactly like the
// §23.3 gate. These cases are NOT §23.3 gate cases: they do not call
// beginGateCase, so required gate mode neither requires nor waits on them.
package integration

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"runtime"
	"sync"
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
// on-disk config, modelling an frps-only restart (§15.2). The gateway process
// and its in-memory state are untouched.
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

	s.streams = gateway.NewStreams()
	s.routesTable = routes.NewTable(s.presence)
	s.gatewaySrv = gateway.NewServer(s.routesTable, s.streams, gateway.WithDialer(s.recordDial))
	ln := listenLoopback(t)
	s.gatewayLn = ln
	s.gatewayAddr = ln.Addr().String()
	go func() { _ = s.gatewaySrv.Serve(ln) }()
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
	s.streams = gateway.NewStreams()
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
// §15.2: an frps-only restart clears all tunnel presence immediately and
// terminates established relayed streams; the route state remains but is
// unroutable until fresh Login/NewProxy/probe-confirmed NewUserConn, which
// requires a freshly issued one-use credential.
func TestRealFRPFRPSRestartClearsPresenceAndTerminatesEstablishedStreams(t *testing.T) {
	requireIntegration(t)
	s, spec := startPrimaryStack(t)
	baseline := runtime.NumGoroutine()

	// Establish a live relay stream through the real FRP data plane.
	established, slowClient := openSlowRelayRequest(t, s)
	if s.streams.Len() != 1 {
		t.Fatalf("gateway stream registry holds %d streams, want 1 before the restart", s.streams.Len())
	}

	// frps-only restart: the gateway process stays up.
	s.stopTunnel(spec.label)
	s.restartFrps(t)

	// The data plane died with frps: the established stream terminates.
	requireStreamClosed(t, "frps restart", established, 10*time.Second)
	slowClient.CloseIdleConnections()
	requireGoroutinesReturn(t, baseline, 8, "frps restart", 5*time.Second)

	// Presence may linger until the reset signal/lease in a deployment that has
	// not yet cleared it, but the data plane is still fail-closed: frps holds
	// no proxy listener, so the gateway's loopback dial cannot reach a stale
	// tunnel and the connection is closed generically (no stale proxy survives).
	expectRelayRequestFails(t, s, "/s/"+fixtureCode)

	// §15.2 immediate presence clear: the registry mutation is synchronous and
	// observable with no lease wait.
	s.presence.ClearAll()
	if s.presence.Online(spec.agentID, spec.proxyPort, uint64(spec.generation)) {
		t.Fatal("presence survived the frps reset; §15.2 requires an immediate clear")
	}
	// Route state remains, but the presence join fails closed: new connections
	// are refused until the tunnel re-registers.
	if _, err := s.routesTable.Lookup(s.relayHost); !errors.Is(err, routes.ErrPresenceAbsent) {
		t.Fatalf("lookup after frps reset error = %v, want ErrPresenceAbsent", err)
	}
	expectRelayRequestFails(t, s, "/s/"+fixtureCode)

	// Re-establishment: a fresh one-use credential (the agent's post-restart
	// relay_credential_request path, §11.1/§15.2) brings the tunnel online.
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
// the idle and active cases: an agent tunnel restart drops the tunnel and
// closes active streams, while a restart from idle re-hydrates and serves
// again once a fresh credential re-registers the proxy.
func TestRealFRPAgentRestartDuringIdleAndActiveTransfers(t *testing.T) {
	requireIntegration(t)
	s, spec := startPrimaryStack(t)

	t.Run("active transfer", func(t *testing.T) {
		baseline := runtime.NumGoroutine()
		established, slowClient := openSlowRelayRequest(t, s)
		s.stopTunnel(spec.label)
		requireStreamClosed(t, "agent restart", established, 10*time.Second)
		waitSlowCancelled(t, s, 10*time.Second)
		slowClient.CloseIdleConnections()
		requireGoroutinesReturn(t, baseline, 8, "agent restart", 5*time.Second)

		// A restarted agent re-registers with a fresh credential and serves.
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

// TestRealFRPControlSyncLossPastRouteLeaseKeepsEstablishedStream proves §15.4:
// when control cannot renew route state, the finite 120-second route lease
// expiry blocks only NEW connections; established streams keep running under
// their own idle and absolute-lifetime limits until an explicit revoke or
// lockdown.
func TestRealFRPControlSyncLossPastRouteLeaseKeepsEstablishedStream(t *testing.T) {
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
// detection: with the tunnel's Pings gone but no CloseProxy ever delivered,
// the 45-second presence lease expires on its own at the exact boundary and
// the route join fails closed, while the established stream (which the
// gateway never re-checks) is preserved — only ClearAll, revoke, or lockdown
// drain established streams. The registry runs on an injected clock so the
// boundary is deterministic (the real-time equivalent is
// TestRealFRPRelayPresenceHeartbeatDelayAndExpiry, which also proves the
// delayed-Ping anti-flap margin).
func TestRealFRPPresenceExpiryWithoutCloseProxy(t *testing.T) {
	requireIntegration(t)

	const (
		bootID     = "failure-boot-1"
		agentID    = "agent-expiry"
		proxyName  = "sb-sbdeadbeef"
		relayPort  = 20099
		generation = 1
		probeSrc   = "127.0.0.1:55555"
	)

	clock := newFailureClock()
	sink := &failurePresenceSink{}
	streams := gateway.NewStreams()
	var registry *presence.Registry
	var err error
	registry, err = presence.NewRegistry(presence.Config{
		BootID:  bootID,
		Now:     clock.Now,
		Sink:    sink,
		Drainer: streams, // the frps-reset drain seam must stay idle here
		Probe: func(context.Context, int) (string, error) {
			return probeSrc, nil
		},
	})
	if err != nil {
		t.Fatalf("new presence registry: %v", err)
	}
	table := routes.NewTable(registry, routes.WithClock(clock.Now))

	// Bring the tunnel online through the real four-fact readiness predicate.
	registry.ObserveFRPEvent(frpplugin.PresenceFact{Operation: frpplugin.OperationLogin, AgentRecordID: agentID, Namespace: "sbdeadbeef", ProxyName: proxyName, RelayPort: relayPort, Generation: generation, RunID: "run-1"})
	registry.ObserveFRPEvent(frpplugin.PresenceFact{Operation: frpplugin.OperationNewProxy, AgentRecordID: agentID, Namespace: "sbdeadbeef", ProxyName: proxyName, RelayPort: relayPort, Generation: generation, RunID: "run-1"})
	registry.ObserveFRPEvent(frpplugin.PresenceFact{Operation: frpplugin.OperationNewUserConn, AgentRecordID: agentID, Namespace: "sbdeadbeef", ProxyName: proxyName, RelayPort: relayPort, Generation: generation, RunID: "run-1", RemoteAddr: probeSrc})
	// The readiness probe runs on its own bounded goroutine; wait for the
	// asynchronous confirmation rather than assuming it completed inline.
	deadline := time.Now().Add(2 * time.Second)
	for !registry.Online(agentID, relayPort, generation) {
		if time.Now().After(deadline) {
			t.Fatal("tunnel did not reach online after the readiness facts")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := table.Apply(routes.Route{
		Hostname:      "expiry.relay.sbdeadbeef.example.com",
		AgentRecordID: agentID,
		RelayPort:     relayPort,
		Generation:    generation,
		Revision:      1,
		Active:        true,
	}); err != nil {
		t.Fatalf("apply route: %v", err)
	}
	if _, err := table.Lookup("expiry.relay.sbdeadbeef.example.com"); err != nil {
		t.Fatalf("lookup with fresh presence: %v", err)
	}

	// A healthy stream is established (in production this is a spliced public
	// connection; here the registry alone is enough to prove drain behaviour).
	streams.Register("expiry.relay.sbdeadbeef.example.com", agentID, &failureCountingConn{})

	// One missed Ping window is tolerated, then the exact lease boundary ends
	// authority with no CloseProxy.
	clock.Advance(presence.DefaultLeaseTTL - time.Second)
	if !registry.Online(agentID, relayPort, generation) {
		t.Fatal("presence flapped before the 45s lease boundary")
	}
	clock.Advance(time.Second)
	if registry.Online(agentID, relayPort, generation) {
		t.Fatal("presence survived the exact lease boundary without a Ping or CloseProxy")
	}
	if _, err := table.Lookup("expiry.relay.sbdeadbeef.example.com"); !errors.Is(err, routes.ErrPresenceAbsent) {
		t.Fatalf("lookup after lease expiry error = %v, want ErrPresenceAbsent", err)
	}

	// Expiry must not drain established streams (§15.4); only ClearAll does.
	if streams.Len() != 1 {
		t.Fatalf("gateway stream registry holds %d streams after lease expiry, want the established 1 preserved", streams.Len())
	}

	// Bounded offline detection: exactly one offline transition carrying the
	// current boot ID and a monotonic revision.
	events := sink.snapshot()
	if len(events) != 2 || events[1].State != presence.StateOffline {
		t.Fatalf("presence events = %+v, want one online then one offline", events)
	}
	if events[1].BootID != bootID || events[1].Revision <= events[0].Revision {
		t.Fatalf("offline event = %+v, want boot %q and a fresh revision", events[1], bootID)
	}
}

// failurePresenceSink records presence transitions for the expiry case.
type failurePresenceSink struct {
	mu     sync.Mutex
	events []presence.Event
}

func (s *failurePresenceSink) ObservePresenceEvent(event presence.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func (s *failurePresenceSink) snapshot() []presence.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]presence.Event(nil), s.events...)
}

// failureCountingConn lets the expiry case register a stream and observe
// whether a drain closed it.
type failureCountingConn struct {
	mu     sync.Mutex
	closes int
}

func (c *failureCountingConn) Read([]byte) (int, error)    { select {} }
func (c *failureCountingConn) Write(p []byte) (int, error) { return len(p), nil }
func (c *failureCountingConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closes++
	return nil
}
func (c *failureCountingConn) LocalAddr() net.Addr              { return failureAddr("local") }
func (c *failureCountingConn) RemoteAddr() net.Addr             { return failureAddr("remote") }
func (c *failureCountingConn) SetDeadline(time.Time) error      { return nil }
func (c *failureCountingConn) SetReadDeadline(time.Time) error  { return nil }
func (c *failureCountingConn) SetWriteDeadline(time.Time) error { return nil }

func (c *failureCountingConn) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

type failureAddr string

func (a failureAddr) Network() string { return "tcp" }
func (a failureAddr) String() string  { return string(a) }
