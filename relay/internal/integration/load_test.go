// load_test.go is the §23.8 / §18.5 relay capacity and safety baseline
// (plan Task 36). It composes the REAL data plane (pinned frps/frpc + real
// plugin, presence registry, route table and gateway) over loopback, exactly
// like the §23.3 gate, and bounds a deliberately conservative local load:
//
//   - one-agent and multi-agent relay throughput;
//   - §14 limit saturation WITHOUT cross-agent starvation;
//   - an active stream surviving beyond the no-byte idle window;
//   - the no-byte idle close;
//   - the absolute lifetime hard close under continuous activity;
//   - cancellation releasing every admission slot;
//   - goroutine, file-descriptor and copy-buffer plateau under cumulative load.
//
// Scope (plan Task 36 Step 3): the target relay VM belongs to Task 37 and is
// NOT provisioned here. Everything in this file is meaningful on the local
// loopback data plane. Hardware-dependent numbers (throughput/CPU/RSS/NIC
// saturation on the target VM size, and the resulting global FD-budget
// choice) are produced by scripts/relay-capacity-gate.sh and recorded as
// PENDING HARDWARE (Task 37) in docs/operations/phase4a-relay.md. Nothing in
// this file invents a target number, and the §14 product-tier bandwidth
// throttle stays DISABLED (the cap is deferred to Phase 4b per §14).
//
// Every measured result is emitted as a CAPACITY(EVIDENCE) line so the
// operations doc records observed values, never extrapolations.
package integration

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sharebridge/relay/internal/gateway"
	"sharebridge/relay/internal/limits"
	"sharebridge/relay/internal/metrics"
)

const (
	// capacityEvidencePrefix marks machine-emitted measurement lines. The
	// operations doc copies these values verbatim and never derives a number
	// the harness did not observe.
	capacityEvidencePrefix = "CAPACITY(EVIDENCE)"

	// Conservative LOCAL throughput floors. They are deliberately far below
	// the loopback/FRP/TLS rates this host sustains so the case proves the
	// relay moves real bytes and completes cleanly, not that a laptop hits a
	// specific number. Real measured values are logged; the target-VM
	// throughput is PENDING HARDWARE (Task 37).
	capacityMinRequestsPerSecond = 5.0
	capacityMinBytesPerSecond    = 8 << 10

	// Peak-resource slack above the pre-load baseline. Transient HTTP
	// transport helpers and runtime bookkeeping account for the small delta;
	// resource counts must PLATEAU at this bound, not grow with the number of
	// requests served.
	capacityGoroutineSlack = 24
	capacityFDSlack        = 24
	capacityHeapSlack      = 64 << 20
)

// capacityHarness is the hermetic stack with a deterministic §14 limiter and
// a private §17.3 registry installed on its gateway, so a load case can assert
// real admission counters and per-agent accounting instead of inferring them.
type capacityHarness struct {
	*relayStack
	limiter  *limits.Limiter
	registry *metrics.Registry
	specA    tunnelSpec
	specB    tunnelSpec
}

// newCapacityHarness builds the stack with an explicit limiter/registry and
// starts one (or two) real agent tunnels with published exact routes.
func newCapacityHarness(t *testing.T, config limits.Config, multiAgent bool) *capacityHarness {
	t.Helper()
	requireIntegration(t)
	stack := newRelayStack(t)
	registry := metrics.NewRegistry(metrics.Relay)
	limiter := limits.NewLimiter(config)
	installCapacityGateway(t, stack, limiter, registry)

	specA := tunnelSpec{
		label:      "capacity-a",
		agentID:    "agent-capacity-a",
		namespace:  fixtureNamespace,
		generation: 1,
		proxyPort:  stack.allocPort(t),
		localPort:  portOf(t, stack.agentAddr),
	}
	stack.startTunnel(specA)
	stack.applyRoute(stack.relayHost, specA, 1)
	stack.waitOnline(specA, setupTimeout)
	stack.waitRouteReady(stack.relayHost, setupTimeout)

	harness := &capacityHarness{relayStack: stack, limiter: limiter, registry: registry, specA: specA}
	if !multiAgent {
		return harness
	}
	specB := tunnelSpec{
		label:      "capacity-b",
		agentID:    "agent-capacity-b",
		namespace:  fixtureNamespaceB,
		generation: 1,
		proxyPort:  stack.allocPort(t),
		localPort:  portOf(t, stack.agentAddr),
	}
	stack.startTunnel(specB)
	stack.applyRoute(stack.relayHostB, specB, 1)
	stack.waitOnline(specB, setupTimeout)
	stack.waitRouteReady(stack.relayHostB, setupTimeout)
	harness.specB = specB
	return harness
}

// installCapacityGateway replaces the fixture's default-limiter gateway with
// one carrying the explicit limiter and registry. It is called before any
// public request, and the old acceptor is closed first.
func installCapacityGateway(t *testing.T, stack *relayStack, limiter *limits.Limiter, registry *metrics.Registry) {
	t.Helper()
	if stack.gatewaySrv != nil {
		stack.gatewaySrv.Close()
		stack.gatewaySrv.Wait()
	}
	stack.gatewaySrv = gateway.NewServer(stack.routesTable, stack.streams,
		gateway.WithDialer(stack.recordDial),
		gateway.WithLimiter(limiter),
		gateway.WithMetrics(registry))
	listener := listenLoopback(t)
	stack.gatewayLn = listener
	stack.gatewayAddr = listener.Addr().String()
	go func() { _ = stack.gatewaySrv.Serve(listener) }()
}

// oneShotClient returns an HTTP/1.1 client that opens a fresh TLS session to
// the gateway for every request (no keep-alive), so one request is exactly one
// public connection/stream and every concurrency slot is released on response
// completion. The SNI is the requested URL host, which lets one client drive
// both agents' exact relay origins.
func (harness *capacityHarness) oneShotClient() *http.Client {
	protocols := &http.Protocols{}
	protocols.SetHTTP1(true)
	stack := harness.relayStack
	return &http.Client{Transport: &http.Transport{
		Protocols:         protocols,
		DisableKeepAlives: true,
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", stack.gatewayAddr)
			if err != nil {
				return nil, err
			}
			tlsConn := tls.Client(raw, &tls.Config{
				ServerName: host,
				RootCAs:    stack.pki.pool,
				MinVersion: tls.VersionTLS12,
				MaxVersion: tls.VersionTLS13,
				NextProtos: []string{"http/1.1"},
			})
			if err := tlsConn.HandshakeContext(ctx); err != nil {
				_ = raw.Close()
				return nil, err
			}
			return tlsConn, nil
		},
	}}
}

// runLoad drives workers concurrently, each performing perWorker GETs with a
// fresh connection per request. It returns the completed request count, the
// total response body bytes, the wall time, and the error count; it never
// calls t.Fatal from a goroutine.
func runLoad(client *http.Client, hosts []string, path string, workers, perWorker int) (int, int64, time.Duration, int) {
	var completed, totalBytes, failures atomic.Int64
	started := time.Now()
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		host := hosts[worker%len(hosts)]
		group.Add(1)
		go func(host string) {
			defer group.Done()
			for attempt := 0; attempt < perWorker; attempt++ {
				response, err := client.Get("https://" + host + path)
				if err != nil {
					failures.Add(1)
					continue
				}
				body, readErr := io.ReadAll(response.Body)
				response.Body.Close()
				if readErr != nil || response.StatusCode != http.StatusOK {
					failures.Add(1)
					continue
				}
				completed.Add(1)
				totalBytes.Add(int64(len(body)))
			}
		}(host)
	}
	group.Wait()
	return int(completed.Load()), totalBytes.Load(), time.Since(started), int(failures.Load())
}

// settleLoad waits until the stream registry and every admission slot drain,
// then returns. It is the quiescence point for plateau measurements: a
// resource that does not return here is a leak, not a transient.
func settleLoad(t *testing.T, harness *capacityHarness, deadline time.Duration) {
	t.Helper()
	limit := time.Now().Add(deadline)
	for time.Now().Before(limit) {
		runtime.Gosched()
		if harness.streams.Len() == 0 && harness.limiter.ActiveStreams() == 0 {
			runtime.GC()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("load did not settle: stream registry=%d active streams=%d after %s",
		harness.streams.Len(), harness.limiter.ActiveStreams(), deadline)
}

// openFDCount reports this process's open file descriptor count from the
// platform's fd directory. Readdirnames is used deliberately: it enumerates
// names without stat-ing each entry, which is what makes it reliable against
// macOS's dynamic /dev/fd (os.ReadDir fails mid-enumeration there with EBADF).
// The second result is false when the platform exposes neither directory (the
// FD plateau assertion is then reported unavailable rather than guessed).
func openFDCount() (int, bool) {
	for _, directory := range []string{"/proc/self/fd", "/dev/fd"} {
		file, err := os.Open(directory)
		if err != nil {
			continue
		}
		names, readErr := file.Readdirnames(-1)
		_ = file.Close()
		if readErr == nil || len(names) > 0 {
			return len(names), true
		}
	}
	return 0, false
}

// heapInUseBytes returns HeapInuse after a forced collection, the coarse
// witness for copy-buffer plateau. The tight bound is the live-stream count
// (see TestRelayCapacityResourcePlateauUnderLoad); this is the supporting
// measurement that no cumulative buffering survives.
func heapInUseBytes() uint64 {
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.HeapInuse
}

// waitForActiveStreams blocks until the limiter reports the wanted per-agent
// active stream count.
func waitForActiveStreams(t *testing.T, limiter *limits.Limiter, agentID string, want int, deadline time.Duration) {
	t.Helper()
	limit := time.Now().Add(deadline)
	for time.Now().Before(limit) {
		if limiter.ActiveStreamsForAgent(agentID) == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("agent %q holds %d active streams, want %d", agentID, limiter.ActiveStreamsForAgent(agentID), want)
}

// tryOpenRelayTLS is the non-fatal counterpart of openRelayTLS: it returns the
// handshake error so a saturation case can assert a generic rejection.
func tryOpenRelayTLS(ctx context.Context, stack *relayStack, host string) (net.Conn, error) {
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", stack.gatewayAddr)
	if err != nil {
		return nil, err
	}
	tlsConn := tls.Client(raw, &tls.Config{
		ServerName: host,
		RootCAs:    stack.pki.pool,
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, err
	}
	return tlsConn, nil
}

// ---------------------------------------------------------------------------
// Throughput: one agent, then two agents
// ---------------------------------------------------------------------------

// TestRelayCapacityThroughputSingleAgent establishes the one-agent baseline:
// a bounded local load completes without error, moves real bytes, and the
// §17.3 bounded-scope byte counters and per-agent accounting agree with the
// served traffic. The floor is conservative; the measured rate is evidence,
// not a target.
func TestRelayCapacityThroughputSingleAgent(t *testing.T) {
	requireIntegration(t)
	config := limits.DefaultConfig()
	harness := newCapacityHarness(t, config, false)
	client := harness.oneShotClient()

	// Warm-up: one request so the first TLS/FRP dial does not dominate.
	warmup, err := client.Get("https://" + harness.relayHost + "/s/" + fixtureCode + "/items")
	if err != nil {
		t.Fatalf("warm-up request failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, warmup.Body)
	warmup.Body.Close()

	const workers = 4
	const perWorker = 12
	completed, bytes, elapsed, failures := runLoad(client, []string{harness.relayHost}, "/s/"+fixtureCode+"/items", workers, perWorker)
	if failures != 0 {
		t.Fatalf("single-agent load had %d failed request(s) of %d", failures, workers*perWorker)
	}
	if completed != workers*perWorker {
		t.Fatalf("single-agent load completed %d requests, want %d", completed, workers*perWorker)
	}
	requestsPerSecond := float64(completed) / elapsed.Seconds()
	bytesPerSecond := float64(bytes) / elapsed.Seconds()
	if requestsPerSecond < capacityMinRequestsPerSecond {
		t.Fatalf("single-agent throughput %.2f req/s is below the conservative floor %.2f", requestsPerSecond, capacityMinRequestsPerSecond)
	}
	if bytesPerSecond < capacityMinBytesPerSecond {
		t.Fatalf("single-agent throughput %.2f B/s is below the conservative floor %.2f", bytesPerSecond, float64(capacityMinBytesPerSecond))
	}

	agentBytes := harness.limiter.BytesForAgent(harness.specA.agentID)
	if agentBytes == 0 {
		t.Fatal("per-agent byte counter stayed at zero after a completed load")
	}
	globalBytes := harness.registry.Value("sharebridge_relay_relayed_bytes_total", metrics.ScopeGlobal)
	agentScope := harness.registry.Value("sharebridge_relay_relayed_bytes_total", metrics.ScopeAgent)
	originScope := harness.registry.Value("sharebridge_relay_relayed_bytes_total", metrics.ScopeOrigin)
	if globalBytes <= 0 || globalBytes != agentScope || globalBytes != originScope {
		t.Fatalf("relayed byte scopes disagree under load: global=%d agent=%d origin=%d", globalBytes, agentScope, originScope)
	}
	accepted := harness.registry.Value("sharebridge_relay_public_connections_total", metrics.OutcomeAccepted)
	if accepted < int64(completed) {
		t.Fatalf("accepted connection counter %d is below the %d completed requests", accepted, completed)
	}
	t.Logf("%s case=single-agent requests=%d errors=%d elapsed_s=%.3f req_per_s=%.2f bytes=%d bytes_per_s=%.2f agent_bytes=%d relayed_global=%d accepted=%d",
		capacityEvidencePrefix, completed, failures, elapsed.Seconds(), requestsPerSecond, bytes, bytesPerSecond, agentBytes, globalBytes, accepted)

	settleLoad(t, harness, 10*time.Second)
}

// TestRelayCapacityThroughputMultiAgent establishes the two-agent baseline:
// concurrent load split across two exact relay origins completes on both
// agents, both per-agent byte counters advance, and the aggregate rate stays
// above the conservative floor. Selection is never influenced: both routes are
// explicit exact origins and the load harness drives each one directly.
func TestRelayCapacityThroughputMultiAgent(t *testing.T) {
	requireIntegration(t)
	config := limits.DefaultConfig()
	harness := newCapacityHarness(t, config, true)
	client := harness.oneShotClient()

	for _, host := range []string{harness.relayHost, harness.relayHostB} {
		warmup, err := client.Get("https://" + host + "/s/" + fixtureCode + "/items")
		if err != nil {
			t.Fatalf("warm-up request to %s failed: %v", host, err)
		}
		_, _ = io.Copy(io.Discard, warmup.Body)
		warmup.Body.Close()
	}

	const workers = 8
	const perWorker = 8
	completed, bytes, elapsed, failures := runLoad(client,
		[]string{harness.relayHost, harness.relayHostB}, "/s/"+fixtureCode+"/items", workers, perWorker)
	if failures != 0 {
		t.Fatalf("multi-agent load had %d failed request(s) of %d", failures, workers*perWorker)
	}
	if completed != workers*perWorker {
		t.Fatalf("multi-agent load completed %d requests, want %d", completed, workers*perWorker)
	}
	requestsPerSecond := float64(completed) / elapsed.Seconds()
	bytesPerSecond := float64(bytes) / elapsed.Seconds()
	if requestsPerSecond < capacityMinRequestsPerSecond {
		t.Fatalf("multi-agent throughput %.2f req/s is below the conservative floor %.2f", requestsPerSecond, capacityMinRequestsPerSecond)
	}
	if bytesPerSecond < capacityMinBytesPerSecond {
		t.Fatalf("multi-agent throughput %.2f B/s is below the conservative floor %.2f", bytesPerSecond, float64(capacityMinBytesPerSecond))
	}
	bytesA := harness.limiter.BytesForAgent(harness.specA.agentID)
	bytesB := harness.limiter.BytesForAgent(harness.specB.agentID)
	if bytesA == 0 || bytesB == 0 {
		t.Fatalf("per-agent byte counters must both advance: agent-a=%d agent-b=%d", bytesA, bytesB)
	}
	if got := harness.limiter.TrackedAgents(); got != 2 {
		t.Fatalf("tracked agent count = %d, want exactly the 2 loaded agents", got)
	}
	t.Logf("%s case=multi-agent requests=%d errors=%d elapsed_s=%.3f req_per_s=%.2f bytes=%d bytes_per_s=%.2f agent_a_bytes=%d agent_b_bytes=%d tracked_agents=%d",
		capacityEvidencePrefix, completed, failures, elapsed.Seconds(), requestsPerSecond, bytes, bytesPerSecond, bytesA, bytesB, harness.limiter.TrackedAgents())

	settleLoad(t, harness, 10*time.Second)
}

// ---------------------------------------------------------------------------
// Saturation without cross-agent starvation
// ---------------------------------------------------------------------------

// TestRelayCapacityLimitSaturationWithoutCrossAgentStarvation saturates one
// agent's §14 per-agent ceiling while a second agent keeps serving. The
// per-source-IP ceiling is configured above the load so a rejection can only be
// attributed to the AGENT ceiling, not to the shared loopback source address:
// with the default 16/source-IP bound every local connection shares
// 127.0.0.1 and a naive test could not tell the two ceilings apart.
//
// It asserts: the saturated agent generically closes new streams, the second
// agent's stream and content path are unaffected, every release path returns
// the counters to zero, and the bounded §17.3 rejection counter records the
// saturation.
func TestRelayCapacityLimitSaturationWithoutCrossAgentStarvation(t *testing.T) {
	requireIntegration(t)
	config := limits.DefaultConfig()
	config.MaxStreamsPerAgent = 2
	config.MaxStreamsPerOrigin = 4
	config.MaxStreamsPerSourceIP = 64
	config.MaxStreamsGlobal = 64
	config.MaxTrackedAgents = 16
	config.IdleTimeout = 2 * time.Minute
	config.AbsoluteLifetime = 5 * time.Minute
	harness := newCapacityHarness(t, config, true)

	const agentALimit = 2
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	heldA := make([]net.Conn, 0, agentALimit)
	for i := 0; i < agentALimit; i++ {
		conn, err := tryOpenRelayTLS(ctx, harness.relayStack, harness.relayHost)
		if err != nil {
			t.Fatalf("holding stream %d for agent A failed: %v", i+1, err)
		}
		heldA = append(heldA, conn)
	}
	t.Cleanup(func() {
		for _, conn := range heldA {
			_ = conn.Close()
		}
	})
	waitForActiveStreams(t, harness.limiter, harness.specA.agentID, agentALimit, 5*time.Second)

	// The saturated agent rejects a new stream generically: the TLS handshake
	// cannot complete because the gateway closes the connection before dialing.
	rejected, rejectErr := tryOpenRelayTLS(ctx, harness.relayStack, harness.relayHost)
	if rejectErr == nil {
		_ = rejected.Close()
		t.Fatalf("agent A admitted a %drd stream against its per-agent ceiling %d", agentALimit+1, agentALimit)
	}

	// No cross-agent starvation: the second agent still admits and serves.
	heldB, err := tryOpenRelayTLS(ctx, harness.relayStack, harness.relayHostB)
	if err != nil {
		t.Fatalf("agent B was starved while agent A saturates its per-agent ceiling: %v", err)
	}
	defer heldB.Close()
	waitForActiveStreams(t, harness.limiter, harness.specB.agentID, 1, 5*time.Second)

	client := harness.oneShotClient()
	served, err := client.Get("https://" + harness.relayHostB + "/s/" + fixtureCode + "/items")
	if err != nil {
		t.Fatalf("agent B content request failed while agent A was saturated: %v", err)
	}
	body, readErr := io.ReadAll(served.Body)
	served.Body.Close()
	if readErr != nil || served.StatusCode != http.StatusOK {
		t.Fatalf("agent B content status=%d err=%v while agent A was saturated", served.StatusCode, readErr)
	}
	if len(body) == 0 {
		t.Fatal("agent B returned an empty body while agent A was saturated")
	}

	// A request to the saturated agent fails while B succeeds: the ceiling is
	// attributable to agent A, not the shared source IP.
	if _, err := client.Get("https://" + harness.relayHost + "/s/" + fixtureCode + "/items"); err == nil {
		t.Fatal("agent A served a request while at its per-agent ceiling")
	}

	rejections := harness.registry.Value("sharebridge_relay_public_connection_rejections_total", metrics.ReasonLimits)
	if rejections == 0 {
		t.Fatal("bounded limits rejection counter stayed at zero after a saturated agent rejection")
	}

	t.Logf("%s case=saturation-without-starvation agent_a_limit=%d agent_a_active=%d agent_a_rejected=yes agent_b_active=%d agent_b_served=yes limits_rejections=%d source_ip_ceiling=%d",
		capacityEvidencePrefix, agentALimit, harness.limiter.ActiveStreamsForAgent(harness.specA.agentID),
		harness.limiter.ActiveStreamsForAgent(harness.specB.agentID), rejections, config.MaxStreamsPerSourceIP)

	// Release every held stream: both agents' counters must return to zero and
	// the previously saturated agent must admit again.
	for _, conn := range heldA {
		_ = conn.Close()
	}
	heldA = heldA[:0]
	_ = heldB.Close()
	waitForActiveStreams(t, harness.limiter, harness.specA.agentID, 0, 10*time.Second)
	waitForActiveStreams(t, harness.limiter, harness.specB.agentID, 0, 10*time.Second)
	waitForStreams(t, harness.relayStack, 0, 10*time.Second)
	if harness.limiter.ActiveStreams() != 0 {
		t.Fatalf("limiter still reports %d active streams after release", harness.limiter.ActiveStreams())
	}

	recovered, err := tryOpenRelayTLS(ctx, harness.relayStack, harness.relayHost)
	if err != nil {
		t.Fatalf("agent A did not recover admission after its streams were released: %v", err)
	}
	_ = recovered.Close()
	t.Logf("%s case=saturation-without-starvation phase=release agent_a_active=0 agent_b_active=0 agent_a_readmitted=yes",
		capacityEvidencePrefix)
}

// ---------------------------------------------------------------------------
// Idle timeout, activity, absolute lifetime
// ---------------------------------------------------------------------------

// heldRelayStream is a live relayed TLS stream that carries real
// client-to-agent application bytes, letting a case observe the gateway's
// shared activity clock without depending on an HTTP response body.
type heldRelayStream struct {
	conn net.Conn
}

// openHeldRelayStream opens a relayed TLS session and holds it open.
func openHeldRelayStream(t *testing.T, stack *relayStack, host string) *heldRelayStream {
	t.Helper()
	return &heldRelayStream{conn: openRelayTLS(t, stack, host)}
}

// feed writes one byte of client-to-agent application data, resetting the
// gateway's shared activity clock. The byte is deliberately an unterminated
// HTTP request-line fragment: the agent's HTTP server buffers it waiting for a
// line terminator, so it neither responds nor closes the connection, and only
// the gateway's clock is under test.
func (stream *heldRelayStream) feed() error {
	_, err := stream.conn.Write([]byte{'x'})
	return err
}

// alive reports whether the stream is still open by attempting a bounded read.
// A timeout means "no bytes, still open"; any other error means the gateway
// closed the stream.
func (stream *heldRelayStream) alive(window time.Duration) bool {
	_ = stream.conn.SetReadDeadline(time.Now().Add(window))
	buffer := make([]byte, 1)
	_, err := stream.conn.Read(buffer)
	_ = stream.conn.SetReadDeadline(time.Time{})
	if err == nil {
		return true
	}
	var netErr net.Error
	if asNetError(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

func (stream *heldRelayStream) close() { _ = stream.conn.Close() }

// asNetError is errors.As without importing errors solely for one call site.
func asNetError(err error, target *net.Error) bool {
	for err != nil {
		if netErr, ok := err.(net.Error); ok {
			*target = netErr
			return true
		}
		type unwrapper interface{ Unwrap() error }
		unwrapped, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = unwrapped.Unwrap()
	}
	return false
}

// TestRelayCapacityIdleCloseWithoutBytes proves the §14 no-byte idle close on
// the real data plane: with a short configured idle window, a stream that moves
// no byte after its handshake is closed, and it survives the full window rather
// than closing immediately.
func TestRelayCapacityIdleCloseWithoutBytes(t *testing.T) {
	requireIntegration(t)
	config := limits.DefaultConfig()
	const idleWindow = 2 * time.Second
	config.IdleTimeout = idleWindow
	config.AbsoluteLifetime = 2 * time.Minute
	harness := newCapacityHarness(t, config, false)

	conn := openRelayTLS(t, harness.relayStack, harness.relayHost)
	if harness.streams.Len() != 1 {
		t.Fatalf("stream registry holds %d streams after handshake, want 1", harness.streams.Len())
	}
	started := time.Now()
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	buffer := make([]byte, 4096)
	closedAfter := time.Duration(0)
	for {
		n, err := conn.Read(buffer)
		if n > 0 {
			// TLS session tickets or other post-handshake records are not user
			// bytes; keep draining until the idle close arrives.
			continue
		}
		if err != nil {
			closedAfter = time.Since(started)
			break
		}
	}
	_ = conn.Close()
	if closedAfter < idleWindow/2 {
		t.Fatalf("idle stream closed after %s, far short of the %s idle window", closedAfter, idleWindow)
	}
	if closedAfter > idleWindow+8*time.Second {
		t.Fatalf("idle stream was still open after %s with a %s idle window", closedAfter, idleWindow)
	}
	waitForStreams(t, harness.relayStack, 0, 10*time.Second)
	if harness.limiter.ActiveStreams() != 0 {
		t.Fatalf("limiter reports %d active streams after the idle close", harness.limiter.ActiveStreams())
	}
	t.Logf("%s case=idle-close-without-bytes idle_window_s=%.1f closed_after_s=%.3f stream_registry=0 active_streams=0",
		capacityEvidencePrefix, idleWindow.Seconds(), closedAfter.Seconds())
}

// TestRelayCapacityIdleActiveStreamSurvivesIdleWindow proves §18.5's "stream
// longer than the idle window while bytes remain active": with the same short
// idle window, a stream that keeps moving client-to-agent bytes survives well
// beyond it, and only closes after the bytes stop.
func TestRelayCapacityIdleActiveStreamSurvivesIdleWindow(t *testing.T) {
	requireIntegration(t)
	config := limits.DefaultConfig()
	const idleWindow = 2 * time.Second
	config.IdleTimeout = idleWindow
	config.AbsoluteLifetime = 5 * time.Minute
	harness := newCapacityHarness(t, config, false)

	stream := openHeldRelayStream(t, harness.relayStack, harness.relayHost)
	t.Cleanup(stream.close)
	started := time.Now()
	activeDeadline := started.Add(5 * time.Second)
	for time.Now().Before(activeDeadline) {
		if err := stream.feed(); err != nil {
			t.Fatalf("active stream closed %s into the active phase: %v", time.Since(started), err)
		}
		if !stream.alive(250 * time.Millisecond) {
			t.Fatalf("active stream was closed %s into the active phase, inside the %s idle window plus activity", time.Since(started), idleWindow)
		}
		time.Sleep(200 * time.Millisecond)
	}
	activeFor := time.Since(started)
	if activeFor <= 2*idleWindow {
		t.Fatalf("active phase %s did not clearly exceed two idle windows (%s)", activeFor, 2*idleWindow)
	}

	// Stop feeding: the idle clock must now expire and close the stream.
	stoppedAt := time.Now()
	closedAfterStop := time.Duration(0)
	limit := stoppedAt.Add(20 * time.Second)
	for time.Now().Before(limit) {
		if !stream.alive(500 * time.Millisecond) {
			closedAfterStop = time.Since(stoppedAt)
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if closedAfterStop == 0 {
		t.Fatal("stream with no further bytes was never closed after the idle window")
	}
	if closedAfterStop > idleWindow+8*time.Second {
		t.Fatalf("idle close after activity took %s with a %s idle window", closedAfterStop, idleWindow)
	}
	waitForStreams(t, harness.relayStack, 0, 10*time.Second)
	t.Logf("%s case=active-stream-survives-idle idle_window_s=%.1f active_for_s=%.3f closed_after_stop_s=%.3f stream_registry=0",
		capacityEvidencePrefix, idleWindow.Seconds(), activeFor.Seconds(), closedAfterStop.Seconds())
}

// TestRelayCapacityAbsoluteLifetimeHardClosesActiveStream proves the §14
// absolute lifetime: continuous activity that would keep resetting the idle
// clock cannot extend the hard close. The idle window is configured far longer
// than the lifetime so only the absolute bound can explain the close.
func TestRelayCapacityAbsoluteLifetimeHardClosesActiveStream(t *testing.T) {
	requireIntegration(t)
	config := limits.DefaultConfig()
	const lifetime = 2 * time.Second
	config.IdleTimeout = 5 * time.Minute
	config.AbsoluteLifetime = lifetime
	harness := newCapacityHarness(t, config, false)

	stream := openHeldRelayStream(t, harness.relayStack, harness.relayHost)
	t.Cleanup(stream.close)
	started := time.Now()
	closedAfter := time.Duration(0)
	limit := started.Add(12 * time.Second)
	for time.Now().Before(limit) {
		if err := stream.feed(); err != nil {
			closedAfter = time.Since(started)
			break
		}
		if !stream.alive(250 * time.Millisecond) {
			closedAfter = time.Since(started)
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if closedAfter == 0 {
		t.Fatal("absolute lifetime did not hard-close a continuously active stream")
	}
	if closedAfter < lifetime/2 {
		t.Fatalf("active stream closed after %s, far short of the %s absolute lifetime", closedAfter, lifetime)
	}
	if closedAfter > lifetime+8*time.Second {
		t.Fatalf("absolute lifetime close took %s against a %s bound", closedAfter, lifetime)
	}
	waitForStreams(t, harness.relayStack, 0, 10*time.Second)
	if harness.limiter.ActiveStreams() != 0 {
		t.Fatalf("limiter reports %d active streams after the absolute lifetime close", harness.limiter.ActiveStreams())
	}
	t.Logf("%s case=absolute-lifetime-hard-close absolute_lifetime_s=%.1f closed_after_s=%.3f idle_window_s=%.0f stream_registry=0",
		capacityEvidencePrefix, lifetime.Seconds(), closedAfter.Seconds(), config.IdleTimeout.Seconds())
}

// ---------------------------------------------------------------------------
// Cancellation
// ---------------------------------------------------------------------------

// TestRelayCapacityCancellationReleasesCapacity proves that cancelling active
// relayed streams tears them down end to end: the agent observes the
// cancellation, every §14 admission slot is released, and the relay admits new
// work afterwards.
func TestRelayCapacityCancellationReleasesCapacity(t *testing.T) {
	requireIntegration(t)
	config := limits.DefaultConfig()
	config.IdleTimeout = 2 * time.Minute
	config.AbsoluteLifetime = 5 * time.Minute
	harness := newCapacityHarness(t, config, false)
	client := harness.oneShotClient()

	const streams = 3
	type pending struct {
		response *http.Response
		cancel   context.CancelFunc
	}
	open := make([]pending, 0, streams)
	for i := 0; i < streams; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+harness.relayHost+"/s/"+fixtureCode+"/slow", nil)
		if err != nil {
			cancel()
			t.Fatalf("build slow request %d: %v", i+1, err)
		}
		response, err := client.Do(request)
		if err != nil {
			cancel()
			t.Fatalf("open slow stream %d: %v", i+1, err)
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			cancel()
			t.Fatalf("slow stream %d status = %d, want 200", i+1, response.StatusCode)
		}
		open = append(open, pending{response: response, cancel: cancel})
	}
	waitForActiveStreams(t, harness.limiter, harness.specA.agentID, streams, 10*time.Second)

	for _, item := range open {
		item.cancel()
	}
	for _, item := range open {
		item.response.Body.Close()
	}

	waitSlowCancelled(t, harness.relayStack, 15*time.Second)
	waitForActiveStreams(t, harness.limiter, harness.specA.agentID, 0, 15*time.Second)
	waitForStreams(t, harness.relayStack, 0, 15*time.Second)
	if harness.limiter.ActiveStreams() != 0 {
		t.Fatalf("limiter still reports %d active streams after cancelling %d", harness.limiter.ActiveStreams(), streams)
	}

	// Capacity is reusable: a fresh request succeeds.
	after, err := client.Get("https://" + harness.relayHost + "/s/" + fixtureCode + "/items")
	if err != nil {
		t.Fatalf("relay request after cancellation failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, after.Body)
	after.Body.Close()
	if after.StatusCode != http.StatusOK {
		t.Fatalf("relay status after cancellation = %d, want 200", after.StatusCode)
	}
	t.Logf("%s case=cancellation cancelled_streams=%d agent_cancelled=yes active_streams=0 stream_registry=0 reuse_after_cancel=yes",
		capacityEvidencePrefix, streams)

	settleLoad(t, harness, 10*time.Second)
}

// ---------------------------------------------------------------------------
// Resource plateau (goroutines, FDs, buffers)
// ---------------------------------------------------------------------------

// TestRelayCapacityResourcePlateauUnderLoad is the M5 safety property: under
// cumulative load the gateway's goroutines, file descriptors, live streams and
// copy buffers must PLATEAU, not grow with the number of requests served.
//
// The baseline is measured per run, after warm-up and quiescence, because the
// test process hosts many hermetic stacks under `-count=N` and the absolute
// process goroutine count therefore drifts with the number of stacks created
// (a harness-lifecycle effect, not a gateway effect). The property under test
// is the WITHIN-run delta: resources must return to the run's own baseline and
// must not scale with the cumulative request count.
//
// The tight bound is the live-stream count: each stream owns at most two 32 KiB
// pooled copy buffers (gateway.streamCopyBufferSize), so copy-buffer memory is
// O(concurrent streams), and a peak live-stream count at the configured
// concurrency proves it never scales with cumulative requests. Goroutine and FD
// counts are then asserted to return to the pre-load baseline, and an
// independent heap-plateau measurement confirms no cumulative buffering
// survives the load.
func TestRelayCapacityResourcePlateauUnderLoad(t *testing.T) {
	requireIntegration(t)
	config := limits.DefaultConfig()
	config.MaxStreamsGlobal = 64
	config.MaxStreamsPerSourceIP = 64
	config.IdleTimeout = 2 * time.Minute
	config.AbsoluteLifetime = 10 * time.Minute
	harness := newCapacityHarness(t, config, true)
	client := harness.oneShotClient()

	// Warm up so runtime and transport pools reach steady state before the
	// baseline is taken.
	for round := 0; round < 3; round++ {
		response, err := client.Get("https://" + harness.relayHost + "/s/" + fixtureCode + "/items")
		if err != nil {
			t.Fatalf("warm-up request failed: %v", err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
	}
	settleLoad(t, harness, 10*time.Second)

	baselineGoroutines := runtime.NumGoroutine()
	baselineFDs, fdsAvailable := openFDCount()
	baselineHeap := heapInUseBytes()

	const workers = 8
	const rounds = 4
	peakGoroutines := baselineGoroutines
	peakFDs := baselineFDs
	peakStreams := 0
	samplerStop := make(chan struct{})
	var samplerGroup sync.WaitGroup
	samplerGroup.Add(1)
	go func() {
		defer samplerGroup.Done()
		for {
			select {
			case <-samplerStop:
				return
			default:
			}
			if goroutines := runtime.NumGoroutine(); goroutines > peakGoroutines {
				peakGoroutines = goroutines
			}
			if fds, ok := openFDCount(); ok && fds > peakFDs {
				peakFDs = fds
			}
			if live := harness.streams.Len(); live > peakStreams {
				peakStreams = live
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	for round := 0; round < rounds; round++ {
		completed, _, _, failures := runLoad(client,
			[]string{harness.relayHost, harness.relayHostB}, "/s/"+fixtureCode+"/items", workers, 2)
		if failures != 0 {
			close(samplerStop)
			samplerGroup.Wait()
			t.Fatalf("round %d had %d failed requests", round+1, failures)
		}
		if completed != workers*2 {
			close(samplerStop)
			samplerGroup.Wait()
			t.Fatalf("round %d completed %d requests, want %d", round+1, completed, workers*2)
		}
		settleLoad(t, harness, 10*time.Second)
	}
	close(samplerStop)
	samplerGroup.Wait()

	settleLoad(t, harness, 10*time.Second)
	finalGoroutines := runtime.NumGoroutine()
	finalFDs := baselineFDs
	if fdsAvailable {
		finalFDs, _ = openFDCount()
	}
	finalHeap := heapInUseBytes()

	if finalGoroutines > baselineGoroutines+capacityGoroutineSlack {
		t.Fatalf("goroutines did not plateau: baseline=%d final=%d slack=%d after serving %d requests",
			baselineGoroutines, finalGoroutines, capacityGoroutineSlack, rounds*workers*2)
	}
	if fdsAvailable && finalFDs > baselineFDs+capacityFDSlack {
		t.Fatalf("file descriptors did not plateau: baseline=%d final=%d slack=%d after serving %d requests",
			baselineFDs, finalFDs, capacityFDSlack, rounds*workers*2)
	}
	if finalHeap > baselineHeap+capacityHeapSlack {
		t.Fatalf("heap did not plateau: baseline=%d final=%d growth_slack=%d after serving %d requests",
			baselineHeap, finalHeap, capacityHeapSlack, rounds*workers*2)
	}
	if peakGoroutines > baselineGoroutines+6*workers+capacityGoroutineSlack {
		t.Fatalf("peak goroutines %d exceeded the concurrency bound baseline=%d + 6*workers=%d + slack=%d; goroutines must scale with concurrent streams, not the %d cumulative requests",
			peakGoroutines, baselineGoroutines, 6*workers, capacityGoroutineSlack, rounds*workers*2)
	}
	if peakStreams > workers+2 {
		t.Fatalf("peak live streams %d exceeded the configured concurrency %d (copy buffers must scale with concurrent streams, not cumulative requests)", peakStreams, workers)
	}
	if harness.streams.Len() != 0 {
		t.Fatalf("stream registry holds %d streams after settling", harness.streams.Len())
	}
	if harness.limiter.ActiveStreams() != 0 {
		t.Fatalf("limiter holds %d active streams after settling", harness.limiter.ActiveStreams())
	}
	// Copy buffers: each live stream may hold at most two pooled 32 KiB
	// buffers, so the peak buffer footprint is bounded by the peak stream
	// count, not by the cumulative request count.
	const copyBuffersPerStream = 2
	const copyBufferBytes = 32 << 10
	maxBufferBytes := int64((peakStreams + 1) * copyBuffersPerStream * copyBufferBytes)
	t.Logf("%s case=resource-plateau requests_served=%d workers=%d rounds=%d baseline_goroutines=%d final_goroutines=%d peak_goroutines=%d baseline_fds=%d final_fds=%d peak_fds=%d fds_available=%t baseline_heap=%d final_heap=%d peak_streams=%d max_copy_buffer_bytes=%d",
		capacityEvidencePrefix, rounds*workers*2, workers, rounds,
		baselineGoroutines, finalGoroutines, peakGoroutines,
		baselineFDs, finalFDs, peakFDs, fdsAvailable, baselineHeap, finalHeap, peakStreams, maxBufferBytes)
}
