package frptest

import (
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"sharebridge/relay/internal/frpplugin"
)

// §23.2 — proxy listeners bind only to the loopback proxyBindAddr and reach
// the agent-side loopback target.
func TestPinnedFRPProxyBindAddrIsLoopback(t *testing.T) {
	fixture := newPinnedFRPFixture(t, gateFixtureOptions{})
	fixture.startClient(gateClientOptions{Label: "loopback"})
	fixture.waitNthNewProxyCall("1", 1, 10*time.Second)

	connection, err := net.DialTimeout("tcp", fixture.proxyAddr(), time.Second)
	if err != nil {
		t.Fatalf("loopback proxy was not reachable: %v", err)
	}
	greeting := make([]byte, len(gateTargetGreeting))
	_ = connection.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(connection, greeting); err != nil || string(greeting) != gateTargetGreeting {
		t.Fatalf("real loopback proxy did not reach the local target: %q, %v", greeting, err)
	}
	_ = connection.Close()
	for _, address := range nonLoopbackIPv4Addresses() {
		if connection, err := net.DialTimeout("tcp", net.JoinHostPort(address, strconv.Itoa(fixture.proxyPort)), 250*time.Millisecond); err == nil {
			_ = connection.Close()
			t.Fatalf("proxy port accepted a non-loopback dial at %s", address)
		}
	}
}

// §23.2 — narrow allowPorts and exactly one authorized proxy listener.
func TestPinnedFRPAllowPortsAndOneProxy(t *testing.T) {
	fixture := newPinnedFRPFixture(t, gateFixtureOptions{})
	fixture.startClient(gateClientOptions{Label: "single"})
	fixture.waitNthNewProxyCall("1", 1, 10*time.Second)

	if got := len(fixture.callsOf(frpplugin.OperationNewProxy)); got != 1 {
		t.Fatalf("plugin authorized %d proxies; want exactly one", got)
	}
	if connection, err := net.DialTimeout("tcp", fixture.proxyAddr(), time.Second); err != nil {
		t.Fatalf("assigned proxy port not reachable: %v", err)
	} else {
		_ = connection.Close()
	}
	// Runtime one-proxy proof from the real frpc: exactly one proxy is
	// registered and running.
	statuses := fixture.adminProxyStatuses(t, fixture.client())
	if len(statuses) != 1 || statuses[0].Name != gateProxyName || statuses[0].Status != "running" {
		t.Fatalf("expected exactly one running proxy %q; admin status: %+v", gateProxyName, statuses)
	}
	// Prove frps bound no listener at an unassigned quiet-range port by taking
	// that port in the test process (neighbor probing of OS-assigned
	// ephemeral ports races unrelated local listeners).
	assertPortUnbound(t, freeQuietPort(t))
}

// §23.2 — transport TLS verification: a client whose pinned CA did not sign
// the relay certificate must fail closed before any plugin traffic. (v0.71.0
// treats an ABSENT trustedCaFile as insecure skip-verify, so every rendered
// production config must set both trustedCaFile and serverName.)
func TestPinnedFRPVerifiesTransportTLS(t *testing.T) {
	fixture := newPinnedFRPFixture(t, gateFixtureOptions{})
	fixture.startClient(gateClientOptions{Label: "untrusted", UntrustedCA: true})

	// A healthy client reaches Login well under a second; across this window
	// an unverified client must never reach the plugin, on any reconnect try.
	time.Sleep(2500 * time.Millisecond)
	if calls := fixture.plugin.snapshot(); len(calls) != 0 {
		t.Fatalf("untrusted transport TLS client reached the plugin: %d calls (payloads redacted)", len(calls))
	}
	if connection, err := net.DialTimeout("tcp", fixture.proxyAddr(), 200*time.Millisecond); err == nil {
		_ = connection.Close()
		t.Fatal("proxy listener existed for a client that never authenticated")
	}
	t.Log("untrusted CA client: zero plugin calls over 2.5s reconnect window; no listener bound")
}

// §23.2 — RUNTIME enforcement: a real client attempting a second proxy, or a
// port outside the configured allowPorts, must be rejected on the wire with
// no listener ever bound. Config-text assertions alone are insufficient.
func TestPinnedFRPRuntimeRejectsSecondProxyAndOutOfRangePort(t *testing.T) {
	t.Run("runtime second proxy is rejected and never listens", func(t *testing.T) {
		fixture := newPinnedFRPFixture(t, gateFixtureOptions{})
		extraPort := freeQuietPort(t)
		fixture.startClient(gateClientOptions{
			Label: "greedy", ExtraProxies: []gateProxySpec{{Name: "sb-sbdeadbeef-extra", RemotePort: extraPort}},
		})
		fixture.waitNthNewProxyCall("1", 1, 10*time.Second)

		// The second proxy's NewProxy must actually reach the plugin and be
		// rejected there at runtime.
		deadline := time.Now().Add(5 * time.Second)
		for len(fixture.newProxyCallsFor("sb-sbdeadbeef-extra")) == 0 && time.Now().Before(deadline) {
			time.Sleep(25 * time.Millisecond)
		}
		extraCalls := fixture.newProxyCallsFor("sb-sbdeadbeef-extra")
		if len(extraCalls) == 0 {
			t.Fatal("frps never delivered the second proxy's NewProxy to the plugin")
		}
		responses := fixture.plugin.responsesFor(frpplugin.OperationNewProxy)
		if len(responses) != len(fixture.callsOf(frpplugin.OperationNewProxy)) {
			t.Fatalf("recorded %d NewProxy responses for %d calls", len(responses), len(fixture.callsOf(frpplugin.OperationNewProxy)))
		}
		rejects := 0
		for _, body := range responses {
			if containsReject(body) {
				rejects++
			}
		}
		if rejects != 1 {
			t.Fatalf("expected exactly one runtime NewProxy rejection (the second proxy), saw %d of %d responses", rejects, len(responses))
		}

		// Rejection must hold at runtime: the extra proxy never reaches
		// running state in the real frpc, its port is never bound by frps
		// (proved by binding it in the test process), and the authorized
		// proxy keeps serving.
		time.Sleep(2 * time.Second)
		statuses := fixture.adminProxyStatuses(t, fixture.client())
		byName := make(map[string]adminProxyStatus)
		for _, status := range statuses {
			byName[status.Name] = status
		}
		if byName[gateProxyName].Status != "running" {
			t.Fatalf("authorized proxy not running after the second-proxy rejection: %+v", statuses)
		}
		extra := byName["sb-sbdeadbeef-extra"]
		if extra.Status == "running" || extra.Err == "" {
			t.Fatalf("second proxy has runtime status %q err %q; want a non-running state with the rejection error", extra.Status, extra.Err)
		}
		assertPortUnbound(t, extraPort)
		readProxyGreeting(t, fixture.proxyAddr())
		time.Sleep(time.Second)
		assertPortUnbound(t, extraPort)
		readProxyGreeting(t, fixture.proxyAddr())
		t.Logf("secondProxy: primary=running, extra status=%q err=%q, extra port never bound", extra.Status, extra.Err)
	})

	t.Run("runtime out-of-range port is rejected beyond plugin trust", func(t *testing.T) {
		fixture := newPinnedFRPFixture(t, gateFixtureOptions{})
		// The credential matches the requested port, so the plugin accepts the
		// NewProxy; frps's own allowPorts enforcement must still refuse to
		// bind a listener outside the configured single allowed port. The
		// requested port comes from the quiet range: outside allowPorts, inside
		// the plugin's accepted credential range, and free of kernel ephemeral
		// allocation noise.
		outOfRangePort := freeQuietPort(t)
		fixture.startClient(gateClientOptions{Label: "rangepush", RemotePort: outOfRangePort})
		fixture.waitNthNewProxyCall("1", 1, 10*time.Second)

		time.Sleep(2 * time.Second)
		// frps's own allowPorts enforcement must have refused to bind the
		// listener; the real frpc reports the registration failure at runtime.
		statuses := fixture.adminProxyStatuses(t, fixture.client())
		if len(statuses) != 1 || statuses[0].Status == "running" || statuses[0].Err == "" {
			t.Fatalf("out-of-range proxy runtime status %+v; want a single non-running proxy with a registration error", statuses)
		}
		// The port stays free; prove it by binding it in the test process.
		assertPortUnbound(t, outOfRangePort)
		if fixture.interceptor.count() != 0 {
			t.Fatalf("NewUserConn fired for an out-of-range proxy: %+v", fixture.interceptor.snapshot())
		}
		for _, body := range fixture.plugin.responsesFor(frpplugin.OperationNewProxy) {
			if containsReject(body) {
				t.Fatal("plugin rejected a NewProxy whose port matched the signed credential; the frps allowPorts layer must be the observed runtime enforcement")
			}
		}
		if got := len(fixture.callsOf(frpplugin.OperationNewProxy)); got != 1 {
			t.Fatalf("expected exactly one authorized NewProxy call, saw %d", got)
		}
		for _, line := range strings.Split(fixture.frpsLog(), "\n") {
			if strings.Contains(line, "port") || strings.Contains(line, "not allowed") || strings.Contains(line, "error") {
				t.Logf("frps: %s", strings.TrimSpace(line))
			}
		}
		t.Logf("outOfRangePort: status=%q err=%q; plugin-authorized NewProxy for port %d never bound (frps allowPorts runtime rejection), zero NewUserConn",
			statuses[0].Status, statuses[0].Err, outOfRangePort)
	})
}

func containsReject(body string) bool {
	return strings.Contains(body, `"reject":true`)
}

// assertPortUnbound proves frps never bound the given loopback port by
// binding it in the test process. Dial-based "must refuse" probes are
// unreliable next to an OS-assigned ephemeral port: unrelated local
// listeners can occupy the neighborhood.
func assertPortUnbound(t *testing.T, port int) {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("port %d is already bound (frps must not have bound it): %v", port, err)
	}
	_ = listener.Close()
}
