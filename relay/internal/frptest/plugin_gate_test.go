package frptest

import (
	"io"
	"net"
	"testing"
	"time"

	"sharebridge/relay/internal/frpplugin"
)

// §23.1 — the pinned release must invoke every fail-closed plugin operation
// with the metadata the gateway correlates on, in the order the presence
// design assumes (NewProxy authorization precedes any user-connection
// callback, and CloseProxy arrives on graceful client shutdown).
func TestPinnedFRPInvokesRequiredPluginOperations(t *testing.T) {
	fixture := newPinnedFRPFixture(t, gateFixtureOptions{})
	fixture.startClient(gateClientOptions{Label: "primary"})

	logins := fixture.plugin.waitFor(fixture.t, frpplugin.OperationLogin, 1, 5*time.Second)
	newProxy := fixture.waitNthNewProxyCall("1", 1, 10*time.Second)
	runID := pluginUserString(newProxy.Content, "run_id")
	if runID == "" {
		t.Fatal("authorized NewProxy call carried no server-assigned run_id")
	}
	for _, login := range logins {
		if login.Operation != frpplugin.OperationLogin {
			continue
		}
		metas, _ := login.Content["metas"].(map[string]any)
		if metas[frpplugin.CredentialMetadataKey] == nil || metas[frpplugin.GenerationMetadataKey] != "1" {
			t.Fatal("real Login did not carry credential and generation metadata (values redacted)")
		}
	}

	// A real user connection must trigger NewUserConn carrying the full
	// correlation tuple, including the connecting socket's source address as
	// seen by frps.
	source, err := net.DialTimeout("tcp", fixture.proxyAddr(), 2*time.Second)
	if err != nil {
		t.Fatalf("registered proxy refused a user connection: %v", err)
	}
	sourceAddr := source.LocalAddr().String()
	greeting := make([]byte, len(gateTargetGreeting))
	_ = source.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(source, greeting); err != nil || string(greeting) != gateTargetGreeting {
		t.Fatalf("proxy user connection did not reach the loopback target: %q, %v", greeting, err)
	}
	_ = source.Close()

	deadline := time.Now().Add(5 * time.Second)
	var connEvents []userConnEvent
	for time.Now().Before(deadline) {
		if connEvents = fixture.interceptor.snapshot(); len(connEvents) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(connEvents) == 0 {
		t.Fatal("real frps never invoked the NewUserConn plugin operation for a user connection")
	}
	event := connEvents[0]
	if event.ProxyName != gateProxyName || event.ProxyType != "tcp" || event.User != "" {
		t.Fatalf("unexpected NewUserConn identity: %+v", event)
	}
	if event.RunID != runID || event.Generation != "1" {
		t.Fatalf("NewUserConn correlation mismatch: run_id %q vs NewProxy %q, generation %q", event.RunID, runID, event.Generation)
	}
	if event.RemoteAddr != sourceAddr {
		t.Fatalf("NewUserConn remote_addr %q is not the user socket %q as seen by frps", event.RemoteAddr, sourceAddr)
	}

	// Graceful stop must deliver CloseProxy after the other operations.
	fixture.stopClient("primary", true)
	fixture.plugin.waitFor(fixture.t, frpplugin.OperationCloseProxy, 1, 5*time.Second)

	calls := fixture.plugin.snapshot()
	if calls[0].Operation != frpplugin.OperationLogin {
		t.Fatalf("first observed plugin operation was %s; want Login", calls[0].Operation)
	}
	if calls[len(calls)-1].Operation != frpplugin.OperationCloseProxy {
		t.Fatalf("last observed plugin operation was %s; want CloseProxy", calls[len(calls)-1].Operation)
	}
	if event.At.Before(newProxy.At) {
		t.Fatal("NewUserConn for the registered proxy preceded its NewProxy authorization")
	}
	if !fixture.presence.has(frpplugin.OperationLogin) || !fixture.presence.has(frpplugin.OperationNewProxy) {
		t.Fatal("real plugin did not emit Login/NewProxy presence facts")
	}
	t.Logf("plugin ops: first=%s last=%s total=%d NewUserConn=%dmsAfterNewProxy",
		calls[0].Operation, calls[len(calls)-1].Operation, len(calls), event.At.Sub(newProxy.At).Milliseconds())
}

// §23.1 — an enforced plugin rejection at Login must leave no session, no
// authorized proxy, and no bound listener, across the client's whole
// reconnect loop.
func TestPinnedFRPDisconnectsOnPluginRejection(t *testing.T) {
	fixture := newPinnedFRPFixture(t, gateFixtureOptions{RejectLogin: true})
	fixture.startClient(gateClientOptions{Label: "rejected"})

	fixture.plugin.waitFor(fixture.t, frpplugin.OperationLogin, 1, 5*time.Second)
	time.Sleep(2 * time.Second)
	calls := fixture.plugin.snapshot()
	for _, call := range calls {
		if call.Operation != frpplugin.OperationLogin {
			t.Fatalf("plugin operation %s observed after enforced Login rejection (payloads redacted)", call.Operation)
		}
	}
	for _, body := range fixture.plugin.responsesFor(frpplugin.OperationLogin) {
		if !containsReject(body) {
			t.Fatal("plugin accepted a Login in reject mode")
		}
	}
	if connection, err := net.DialTimeout("tcp", fixture.proxyAddr(), 200*time.Millisecond); err == nil {
		_ = connection.Close()
		t.Fatal("proxy listener existed after enforced Login rejection")
	}
	t.Logf("enforced Login rejection: %d rejected Login attempts, zero other ops, port never bound", len(calls))
}

// §23.1 as amended — the readiness predicate: a bounded loopback probe whose
// correlated NewUserConn (proxy name + server run id + generation metadata +
// probe-socket source address) confirms only a proxy frps actually
// registered. Forced registration failure, stale generations, frpc restarts,
// and frps restarts must never confirm the current generation.
func TestPinnedFRPReadinessProbeConfirmsOnlyRegisteredProxy(t *testing.T) {
	t.Run("healthy registration confirms in one probe attempt", func(t *testing.T) {
		fixture := newPinnedFRPFixture(t, gateFixtureOptions{})
		fixture.startClient(gateClientOptions{Label: "healthy"})
		newProxy := fixture.waitNthNewProxyCall("1", 1, 10*time.Second)
		expectation := probeExpectationFromNewProxyCall(newProxy)
		if expectation.RunID == "" {
			t.Fatal("authorized NewProxy carried no run_id; correlation impossible")
		}

		// A browser-like stream rides the same proxy port before and after the
		// probe, proving the probe neither corrupts nor replaces real traffic.
		browser, err := net.DialTimeout("tcp", fixture.proxyAddr(), 2*time.Second)
		if err != nil {
			t.Fatalf("browser stream could not connect: %v", err)
		}
		greeting := make([]byte, len(gateTargetGreeting))
		_ = browser.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.ReadFull(browser, greeting); err != nil || string(greeting) != gateTargetGreeting {
			t.Fatalf("browser greeting mismatch: %q, %v", greeting, err)
		}
		payload := "sharebridge-browser-payload-0123456789"
		if _, err := browser.Write([]byte(payload)); err != nil {
			t.Fatalf("browser write: %v", err)
		}
		echo := make([]byte, len(payload))
		_ = browser.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.ReadFull(browser, echo); err != nil || string(echo) != payload {
			t.Fatalf("browser echo mismatch: %q, %v", echo, err)
		}

		eventsBefore := fixture.interceptor.count()
		result := fixture.readinessProbe(expectation, newProxy.At)
		if !result.Confirmed {
			t.Fatalf("bounded readiness probe did not confirm the registered proxy: %+v", result)
		}
		if result.Attempts != 1 {
			t.Fatalf("healthy registration needed %d probe attempts; want exactly 1", result.Attempts)
		}
		event := result.Event
		if event.ProxyName != expectation.ProxyName || event.RunID != expectation.RunID ||
			event.Generation != "1" || event.ProxyType != "tcp" || event.User != "" {
			t.Fatalf("confirmation correlation mismatch: event %+v vs expectation %+v", event, expectation)
		}
		if event.RemoteAddr != result.SourceAddr {
			t.Fatalf("confirmation remote_addr %q is not the probe socket %q as seen by frps", event.RemoteAddr, result.SourceAddr)
		}
		if result.AuthorizedToEvent < 0 {
			t.Fatal("confirmation preceded its NewProxy authorization")
		}
		if result.Total > gateProbeDeadline {
			t.Fatalf("probe exceeded the %s hard deadline: %s", gateProbeDeadline, result.Total)
		}
		if newEvents := fixture.interceptor.count() - eventsBefore; newEvents != 1 {
			t.Fatalf("probe produced %d NewUserConn callbacks; want exactly 1", newEvents)
		}

		// The browser stream continues after the probe; the probe injected no
		// bytes and left exactly one empty (accept-mode) target connection.
		if _, err := browser.Write([]byte(payload)); err != nil {
			t.Fatalf("browser stream broken by the probe: %v", err)
		}
		echo2 := make([]byte, len(payload))
		_ = browser.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.ReadFull(browser, echo2); err != nil || string(echo2) != payload {
			t.Fatalf("browser echo after probe mismatch: %q, %v", echo2, err)
		}
		_ = browser.Close()
		time.Sleep(600 * time.Millisecond)
		stats := fixture.target.snapshot()
		var totalBytes int64
		empty := 0
		for _, conn := range stats {
			totalBytes += conn.Bytes
			if conn.Bytes == 0 {
				empty++
			}
		}
		if totalBytes != int64(2*len(payload)) {
			t.Fatalf("agent target saw %d bytes; want exactly %d from the browser stream: %+v", totalBytes, 2*len(payload), stats)
		}
		if empty != 1 {
			t.Fatalf("accept-mode probe left %d empty target connections; want exactly 1: %+v", empty, stats)
		}
		t.Logf("healthy probe: attempts=1 authorizedToEvent=%s connectToEvent=%s total=%s targetBytes=%d",
			result.AuthorizedToEvent.Round(time.Millisecond), result.ConnectToEvent.Round(time.Millisecond),
			result.Total.Round(time.Millisecond), totalBytes)
	})

	t.Run("forced registration failure never confirms while Ping continues", func(t *testing.T) {
		fixture := newPinnedFRPFixture(t, gateFixtureOptions{BlockProxyRegistration: true})
		fixture.startClient(gateClientOptions{Label: "blocked"})
		newProxy := fixture.waitNthNewProxyCall("1", 1, 10*time.Second)
		expectation := probeExpectationFromNewProxyCall(newProxy)
		eventsBefore := fixture.interceptor.count()

		result := fixture.readinessProbe(expectation, newProxy.At)
		if result.Confirmed {
			t.Fatalf("probe confirmed a proxy that never registered downstream: %+v", result)
		}
		if result.Connected == 0 {
			t.Fatal("probe never TCP-connected to the pre-bound port; connect success must be shown to be meaningless")
		}
		if got := fixture.interceptor.count() - eventsBefore; got != 0 {
			t.Fatalf("%d NewUserConn callbacks fired for an unregistered proxy", got)
		}

		// Authenticated Pings continue after the failed registration: the next
		// cadence Ping must arrive after the probe already gave up.
		probeDone := time.Now()
		latePing := fixture.waitForPingAfter(probeDone, 20*time.Second)
		time.Sleep(5 * time.Second)
		if newProxyCalls := len(fixture.callsOf(frpplugin.OperationNewProxy)); newProxyCalls != 1 {
			t.Fatalf("NewProxy retried after downstream registration failure: %d calls", newProxyCalls)
		}
		if got := fixture.interceptor.count() - eventsBefore; got != 0 {
			t.Fatalf("confirmation appeared after the failed registration: %d callbacks", got)
		}
		t.Logf("failedRegistration: attempts=%d connectedToBlocker=%d confirmations=0 noRetryWindow>=%s latePingAfterProbe=%s",
			result.Attempts, result.Connected, time.Since(newProxy.At).Round(time.Second), latePing.At.Sub(probeDone).Round(time.Millisecond))
	})

	t.Run("stale old-generation callbacks never confirm the current generation", func(t *testing.T) {
		fixture := newPinnedFRPFixture(t, gateFixtureOptions{})
		fixture.startClient(gateClientOptions{Label: "gen1"})
		call1 := fixture.waitNthNewProxyCall("1", 1, 10*time.Second)
		runID1 := pluginUserString(call1.Content, "run_id")
		if confirmed := fixture.readinessProbe(probeExpectationFromNewProxyCall(call1), call1.At); !confirmed.Confirmed {
			t.Fatalf("generation-1 registration never confirmed: %+v", confirmed)
		}

		// Superseding generation for the same agent identity: its NewProxy is
		// authorized, but downstream registration fails while generation 1
		// still holds the proxy ("already exists").
		fixture.startClient(gateClientOptions{Label: "gen2", Generation: 2})
		call2 := fixture.waitNthNewProxyCall("2", 1, 10*time.Second)
		runID2 := pluginUserString(call2.Content, "run_id")
		if runID2 == "" || runID2 == runID1 {
			t.Fatalf("expected distinct server-assigned run ids across processes: %q vs %q", runID1, runID2)
		}

		eventsBefore := fixture.interceptor.count()
		result := fixture.readinessProbe(probeExpectationFromNewProxyCall(call2), call2.At)
		if result.Confirmed {
			t.Fatalf("unregistered generation 2 confirmed: %+v", result)
		}
		for _, event := range fixture.interceptor.snapshot()[eventsBefore:] {
			if event.Generation == "2" {
				t.Fatalf("generation-2 callback fired without registration: %+v", event)
			}
			if event.RemoteAddr == result.SourceAddr && event.RunID == runID1 && event.Generation == "1" {
				t.Logf("stale generation-1 listener answered probe socket %s with old identity (name-only matching would falsely confirm)", event.RemoteAddr)
			}
		}
		time.Sleep(2 * time.Second)
		if got := len(fixture.newProxyCallsOfGeneration("2")); got != 1 {
			t.Fatalf("generation-2 NewProxy count %d; want exactly 1 (no retry)", got)
		}
		if again := fixture.readinessProbe(probeExpectationFromNewProxyCall(call2), call2.At); again.Confirmed {
			t.Fatalf("generation 2 confirmed later without re-registration: %+v", again)
		}
		t.Logf("staleGeneration: oldRunID=%q newRunID=%q gen2Confirmations=0", runID1, runID2)
	})

	t.Run("frpc restart never confirms the old generation", func(t *testing.T) {
		fixture := newPinnedFRPFixture(t, gateFixtureOptions{})
		fixture.startClient(gateClientOptions{Label: "first"})
		call1 := fixture.waitNthNewProxyCall("1", 1, 10*time.Second)
		runID1 := pluginUserString(call1.Content, "run_id")
		if confirmed := fixture.readinessProbe(probeExpectationFromNewProxyCall(call1), call1.At); !confirmed.Confirmed {
			t.Fatalf("first registration never confirmed: %+v", confirmed)
		}

		fixture.stopClient("first", true)
		fixture.plugin.waitFor(fixture.t, frpplugin.OperationCloseProxy, 1, 5*time.Second)
		eventsBefore := fixture.interceptor.count()
		if after := fixture.readinessProbe(probeExpectationFromNewProxyCall(call1), call1.At); after.Confirmed || after.Connected != 0 {
			t.Fatalf("graceful stop left the old expectation confirmable: %+v", after)
		}
		if fixture.interceptor.count() != eventsBefore {
			t.Fatal("NewUserConn fired for a closed proxy")
		}

		fixture.startClient(gateClientOptions{Label: "second"})
		call2 := fixture.waitNthNewProxyCall("1", 2, 10*time.Second)
		runID2 := pluginUserString(call2.Content, "run_id")
		if runID2 == "" || runID2 == runID1 {
			t.Fatalf("restarted frpc must carry a fresh server-assigned run id: %q vs %q", runID1, runID2)
		}

		oldResult := fixture.readinessProbe(probeExpectationFromNewProxyCall(call1), call1.At)
		if oldResult.Confirmed {
			t.Fatalf("stale run id confirmed the restarted session: %+v", oldResult)
		}
		answeredByNewSession := false
		for _, event := range fixture.interceptor.snapshot() {
			if event.RemoteAddr == oldResult.SourceAddr && event.RunID == runID2 {
				answeredByNewSession = true
			}
		}
		if !answeredByNewSession {
			t.Fatalf("expected the restarted session's listener to answer probe socket %s with run id %q; events: %+v",
				oldResult.SourceAddr, runID2, fixture.interceptor.snapshot())
		}
		newResult := fixture.readinessProbe(probeExpectationFromNewProxyCall(call2), call2.At)
		if !newResult.Confirmed || newResult.Event.RunID != runID2 {
			t.Fatalf("restarted registration failed to confirm exactly: %+v", newResult)
		}
		t.Logf("frpcRestart: oldRunID=%q newRunID=%q oldConfirmed=false newConfirmed=true", runID1, runID2)
	})

	t.Run("frps restart clears readiness until fresh-credential re-registration", func(t *testing.T) {
		fixture := newPinnedFRPFixture(t, gateFixtureOptions{})
		fixture.startClient(gateClientOptions{Label: "before"})
		call1 := fixture.waitNthNewProxyCall("1", 1, 10*time.Second)
		runID1 := pluginUserString(call1.Content, "run_id")
		if confirmed := fixture.readinessProbe(probeExpectationFromNewProxyCall(call1), call1.At); !confirmed.Confirmed {
			t.Fatalf("initial registration never confirmed: %+v", confirmed)
		}
		eventsAtKill := fixture.interceptor.count()

		fixture.stopFrps()
		if out := fixture.readinessProbe(probeExpectationFromNewProxyCall(call1), call1.At); out.Confirmed || out.Connected != 0 {
			t.Fatalf("readiness survived frps death: %+v", out)
		}
		if fixture.interceptor.count() != eventsAtKill {
			t.Fatal("NewUserConn fired while frps was down")
		}

		fixture.startFrps()
		// Stock frpc reconnects reusing its server-assigned run id and the
		// SAME one-use credential: the plugin must replay-reject it and no
		// proxy may re-register — recovery requires a fresh credential.
		deadline := time.Now().Add(20 * time.Second)
		for len(fixture.callsOf(frpplugin.OperationLogin)) < 2 && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		relogins := fixture.callsOf(frpplugin.OperationLogin)
		if len(relogins) < 2 {
			t.Fatal("frpc never attempted to re-login after the frps restart")
		}
		reloginRunID, _ := relogins[1].Content["run_id"].(string)
		if reloginRunID == "" || reloginRunID != runID1 {
			t.Fatalf("reconnect Login should reuse server-assigned run id %q, got %q", runID1, reloginRunID)
		}
		time.Sleep(3 * time.Second)
		if got := len(fixture.callsOf(frpplugin.OperationNewProxy)); got != 1 {
			t.Fatalf("replayed-credential reconnect re-registered a proxy: %d NewProxy calls", got)
		}
		if out := fixture.readinessProbe(probeExpectationFromNewProxyCall(call1), call1.At); out.Confirmed {
			t.Fatalf("readiness survived the frps restart without re-registration: %+v", out)
		}

		fixture.stopClient("before", false)
		fixture.startClient(gateClientOptions{Label: "after"})
		call2 := fixture.waitNthNewProxyCall("1", 2, 10*time.Second)
		runID2 := pluginUserString(call2.Content, "run_id")
		reconfirmed := fixture.readinessProbe(probeExpectationFromNewProxyCall(call2), call2.At)
		if !reconfirmed.Confirmed || reconfirmed.Event.RunID != runID2 || runID2 == runID1 {
			t.Fatalf("fresh-credential re-registration failed to confirm exactly: %+v", reconfirmed)
		}
		t.Logf("frpsRestart: runID1=%q reusedRunIDOnRejectedReconnect=%q outageConfirmations=0 freshRunID=%q reconfirmed=true",
			runID1, reloginRunID, runID2)
	})
}
