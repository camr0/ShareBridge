package metrics_test

// Task 34 named tests: the §17.3 metric inventory, the private-only operator
// endpoint, log hygiene, and the truthful two-truth gateway health split.
//
// The log-hygiene test is deliberately two-pronged, because each prong covers
// a different failure mode and each has explicit limits:
//
//   - The static scan parses every relay production file and the control
//     direct-preparation/STUN files and flags a log call whose literal key or
//     argument identifier names credential/code/path/header/body/ClientHello
//     material. It covers the call sites a reviewer would read, but it CANNOT
//     see a value passed through an opaque slice variable — the gateway's
//     logRejection builds its key/value slice dynamically — so it can prove
//     "no sensitive key is written at a visible call site", not "no sensitive
//     value reaches the logger at runtime".
//   - The runtime canary drives the real gateway with ClientHello bytes that
//     embed a canary and asserts the canary never reaches the captured log
//     output. It covers the dynamic gateway path the static scan cannot, but
//     only exercises the paths it drives.
//
// Together they cover the gateway datapath plus the statically visible control
// and plugin call sites. They do not cover the agent, which is not the relay
// (spec §16.6 scopes the prohibition to relay logs) and does intentionally log
// agent-side share codes.

import (
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"sharebridge/relay/internal/gateway"
	"sharebridge/relay/internal/metrics"
	"sharebridge/relay/internal/routes"
)

// ---------------------------------------------------------------------------
// Test 1 — exact §17.3 signal inventory and bounded cardinality
// ---------------------------------------------------------------------------

func TestMetricsCoverRequiredRelayAndDirectSignals(t *testing.T) {
	bullets := map[string]bool{}
	for _, signal := range metrics.RequiredSignals {
		if signal.Name == "" {
			t.Fatalf("signal with empty name: %+v", signal)
		}
		if signal.Help == "" {
			t.Fatalf("%s: empty help", signal.Name)
		}
		if signal.Spec == "" {
			t.Fatalf("%s: missing §17.3 bullet mapping", signal.Name)
		}
		bullets[signal.Spec] = true
		if signal.Label == "" {
			if len(signal.Values) != 0 {
				t.Fatalf("%s: unlabeled signal carries label values", signal.Name)
			}
			continue
		}
		switch signal.Label {
		case "state", "outcome", "reason", "scope":
		default:
			t.Fatalf("%s: label %q is not a bounded policy dimension", signal.Name, signal.Label)
		}
		if len(signal.Values) == 0 {
			t.Fatalf("%s: labeled signal without enumerated values would be unbounded", signal.Name)
		}
		seen := map[string]bool{}
		for _, value := range signal.Values {
			if value == "" {
				t.Fatalf("%s: empty label value", signal.Name)
			}
			if seen[value] {
				t.Fatalf("%s: duplicate label value %q", signal.Name, value)
			}
			seen[value] = true
		}
	}

	// Every §17.3 bullet must be represented: routes, tunnels, connections,
	// bytes, ClientHello, FRP connect, direct preparation, STUN, restoration,
	// revocation.
	for number := 1; number <= 11; number++ {
		bullet := "17.3#" + itoa(number)
		if !bullets[bullet] {
			t.Errorf("§17.3 %s has no signal in RequiredSignals", bullet)
		}
	}

	relayRegistry := metrics.NewRegistry(metrics.Relay)
	controlRegistry := metrics.NewRegistry(metrics.Control)
	for _, signal := range metrics.RequiredSignals {
		target := relayRegistry
		if signal.Owner == metrics.Control {
			target = controlRegistry
		}
		if !strings.Contains(target.Render(), signal.Name) {
			t.Errorf("%s: owner registry does not expose the signal", signal.Name)
		}
	}
	if relayRegistry.SeriesCount() == 0 || controlRegistry.SeriesCount() == 0 {
		t.Fatalf("relay/control registries must both expose signals (relay=%d control=%d)",
			relayRegistry.SeriesCount(), controlRegistry.SeriesCount())
	}

	// Cardinality proof: hostile, user-derived label values are dropped and
	// never create a series. The label space is fixed at registry construction.
	hostile := []string{
		"share-code-tYwYQPI0",
		"425bcaea60e1.sbd0903ed4.sharebridgeusercontent.com",
		"203.0.113.9",
		"/s/tYwYQPI0/connect?token=deadbeef",
		"credential-jti-0123456789abcdef",
		"Bearer verysecret",
	}
	before := relayRegistry.SeriesCount()
	for _, signal := range metrics.RequiredSignals {
		target := relayRegistry
		if signal.Owner == metrics.Control {
			target = controlRegistry
		}
		for _, value := range hostile {
			target.Inc(signal.Name, value)
			target.Add(signal.Name, 7, value)
			target.Set(signal.Name, 7, value)
			target.Observe(signal.Name, 0.5, value)
		}
		if !strings.Contains(target.Render(), signal.Name) {
			t.Errorf("%s disappeared after hostile input", signal.Name)
		}
	}
	if after := relayRegistry.SeriesCount(); after != before {
		t.Fatalf("hostile label values created series: before=%d after=%d", before, after)
	}
	// A known bounded value still works after hostile input.
	relayRegistry.Set("sharebridge_relay_tunnel_state", 3, metrics.StateOnline)
	if got := relayRegistry.Value("sharebridge_relay_tunnel_state", metrics.StateOnline); got != 3 {
		t.Fatalf("bounded label value did not record: got %d", got)
	}
}

// ---------------------------------------------------------------------------
// Test 2 — the operator endpoint is private monitoring only
// ---------------------------------------------------------------------------

func TestMetricsEndpointIsPrivateOnly(t *testing.T) {
	registry := metrics.NewRegistry(metrics.Relay)
	registry.Set("sharebridge_relay_tunnel_state", 2, metrics.StateOnline)
	handler := registry.Handler(stubHealth{routeReady: true, frpsHealthy: true})

	cases := []struct {
		name       string
		method     string
		path       string
		remote     string
		host       string
		wantStatus int
	}{
		{"loopback ipv4 host", http.MethodGet, "/metrics", "127.0.0.1:5555", "127.0.0.1:9101", http.StatusOK},
		{"loopback localhost host", http.MethodGet, "/healthz", "127.0.0.1:5555", "localhost:9101", http.StatusOK},
		{"loopback ipv6 host", http.MethodGet, "/metrics", "[::1]:5555", "[::1]:9101", http.StatusOK},
		{"public host rejected", http.MethodGet, "/metrics", "127.0.0.1:5555", "relay.sharebridgeusercontent.com", http.StatusForbidden},
		{"public host with port rejected", http.MethodGet, "/healthz", "127.0.0.1:5555", "sharebridge.app:443", http.StatusForbidden},
		{"empty host rejected", http.MethodGet, "/metrics", "127.0.0.1:5555", "", http.StatusForbidden},
		{"public peer rejected", http.MethodGet, "/metrics", "203.0.113.9:40000", "127.0.0.1:9101", http.StatusForbidden},
		{"post rejected", http.MethodPost, "/metrics", "127.0.0.1:5555", "127.0.0.1:9101", http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, "http://"+tc.host+tc.path, nil)
			request.RemoteAddr = tc.remote
			request.Host = tc.host
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			if recorder.Code == http.StatusForbidden {
				if body := recorder.Body.String(); strings.Contains(body, "sharebridge") || strings.Contains(body, "route") {
					t.Fatalf("forbidden response leaks detail: %q", body)
				}
			}
		})
	}

	t.Run("no route list or source ip in the exposed body", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9101/metrics", nil)
		request.RemoteAddr = "127.0.0.1:5555"
		request.Host = "127.0.0.1:9101"
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		body := recorder.Body.String()
		for _, forbidden := range []string{"203.0.113.9", "sharebridgeusercontent.com", "tYwYQPI0", "Bearer", "credential-jti"} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("metrics body leaks %q:\n%s", forbidden, body)
			}
		}
	})

	t.Run("health reports two independent truths", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9101/healthz", nil)
		request.RemoteAddr = "127.0.0.1:5555"
		request.Host = "127.0.0.1:9101"
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		body := recorder.Body.String()
		if !strings.Contains(body, `"route_ready":true`) || !strings.Contains(body, `"frps_process_healthy":true`) {
			t.Fatalf("health body missing the two truths: %q", body)
		}
		if strings.Contains(body, "uptime") || strings.Contains(body, "routes") {
			t.Fatalf("health body exposes extra detail: %q", body)
		}
		// The §17.1 endpoint carries exactly the two truths: the frps freshness
		// mechanism must never leak an observation timestamp or window.
		var fields map[string]bool
		if err := json.Unmarshal([]byte(body), &fields); err != nil {
			t.Fatalf("health body is not the two-truth JSON object: %v (%q)", err, body)
		}
		if len(fields) != 2 {
			t.Fatalf("health body must expose exactly two truths, got %v", fields)
		}
		for _, forbidden := range []string{"observed", "timestamp", "freshness", "window", "age"} {
			if strings.Contains(strings.ToLower(body), forbidden) {
				t.Fatalf("health body exposes freshness internals %q: %q", forbidden, body)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Test 3 — log hygiene
// ---------------------------------------------------------------------------

func TestLogsNeverContainCredentialCodePathHeaderBodyOrClientHello(t *testing.T) {
	t.Run("static scan of relay and control preparation call sites", func(t *testing.T) {
		files := productionLogFiles(t)
		if len(files) < 8 {
			t.Fatalf("static scan found only %d files; the scan is not covering the tree", len(files))
		}
		for _, file := range files {
			scanLogCallSites(t, file)
		}
	})

	t.Run("runtime canary never reaches the gateway log", func(t *testing.T) {
		recorder := &recordingHandler{}
		registry := metrics.NewRegistry(metrics.Relay)
		table := routes.NewTable(alwaysOfflinePresence{})
		streams := gateway.NewStreams()
		server := gateway.NewServer(table, streams,
			gateway.WithLogger(slog.New(recorder)),
			gateway.WithMetrics(registry))
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		go func() { _ = server.Serve(listener) }()
		t.Cleanup(func() { listener.Close(); server.Close(); server.Wait() })

		conn, err := net.DialTimeout("tcp", listener.Addr().String(), 2*time.Second)
		if err != nil {
			t.Fatalf("dial gateway: %v", err)
		}
		defer conn.Close()
		canary := []byte("CANARYCREDENTIALBODYPATHHEADER")
		if _, err := conn.Write(canary); err != nil {
			t.Fatalf("write canary: %v", err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = io.Copy(io.Discard, conn)

		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && recorder.len() == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		if recorder.len() == 0 {
			t.Fatal("gateway logged nothing for the malformed hello; canary proof is vacuous")
		}
		if strings.Contains(recorder.String(), string(canary)) {
			t.Fatalf("gateway log leaked raw ClientHello bytes: %q", recorder.String())
		}
	})
}

var forbiddenLogIdentifier = regexp.MustCompile(`(?i)(credential|password|passwd|secret|cookie|authorization|bearer|jti|issuedat|clienthello|sharecode|share_code|\bbody\b|\bbuffer\b|\bheader\b|\bprefix\b)`)

// forbiddenLogKeys are the literal structured-log keys that would carry
// forbidden material. "sni"/"hostname"/"remote" are deliberately allowed by
// §16.6 (exact origin, source IP under bounded retention).
var forbiddenLogKeys = []string{
	`"credential"`, `"credentials"`, `"password"`, `"secret"`, `"cookie"`,
	`"authorization"`, `"body"`, `"header"`, `"headers"`, `"path"`,
	`"share_code"`, `"sharecode"`, `"clienthello"`, `"prefix"`, `"jti"`,
}

var logMethodNames = map[string]bool{
	"Printf": true, "Println": true, "Print": true,
	"Fatalf": true, "Fatalln": true, "Fatal": true,
	"Panicf": true, "Panicln": true, "Panic": true,
	"Info": true, "Warn": true, "Error": true, "Debug": true,
	"Log": true, "LogAttrs": true,
	"InfoContext": true, "WarnContext": true, "ErrorContext": true, "DebugContext": true,
}

func productionLogFiles(t *testing.T) []string {
	t.Helper()
	relayRoot := filepath.Join("..", "..")
	controlRoot := filepath.Join("..", "..", "..", "control")
	var files []string
	// The relay gateway/frpplugin/controlsync packages are the §16.6 relay
	// log surface.
	relayScope := []string{
		filepath.Join(relayRoot, "internal", "gateway"),
		filepath.Join(relayRoot, "internal", "frpplugin"),
		filepath.Join(relayRoot, "internal", "controlsync"),
		filepath.Join(relayRoot, "cmd", "gateway"),
	}
	for _, dir := range relayScope {
		collectGoFiles(t, dir, &files)
	}
	// Control's direct-preparation/STUN path and its metrics server.
	for _, name := range []string{
		filepath.Join(controlRoot, "internal", "directctl", "prepare.go"),
		filepath.Join(controlRoot, "internal", "directctl", "stun.go"),
		filepath.Join(controlRoot, "cmd", "server", "main.go"),
	} {
		if _, err := os.Stat(name); err == nil {
			files = append(files, name)
		}
	}
	sort.Strings(files)
	return files
}

func collectGoFiles(t *testing.T, root string, files *[]string) {
	t.Helper()
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		*files = append(*files, path)
		return nil
	})
}

func scanLogCallSites(t *testing.T, file string) {
	t.Helper()
	source, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	// Literal-key check is done on the raw line so dynamically assembled field
	// slices still surface a sensitive key string.
	for _, line := range strings.Split(string(source), "\n") {
		if !looksLikeLogLine(line) {
			continue
		}
		for _, key := range forbiddenLogKeys {
			if strings.Contains(line, key) {
				t.Errorf("%s: log line carries forbidden key %s: %s", file, key, strings.TrimSpace(line))
			}
		}
	}

	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, source, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	ast.Inspect(parsed, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !logMethodNames[selector.Sel.Name] {
			return true
		}
		for _, argument := range call.Args {
			ast.Inspect(argument, func(inner ast.Node) bool {
				switch expression := inner.(type) {
				case *ast.Ident:
					if forbiddenLogIdentifier.MatchString(expression.Name) {
						t.Errorf("%s: log call argument identifier %q matches forbidden log material",
							fset.Position(inner.Pos()).String(), expression.Name)
					}
				case *ast.SelectorExpr:
					if forbiddenLogIdentifier.MatchString(expression.Sel.Name) {
						t.Errorf("%s: log call selector .%s matches forbidden log material",
							fset.Position(inner.Pos()).String(), expression.Sel.Name)
					}
				}
				return true
			})
		}
		return true
	})
}

func looksLikeLogLine(line string) bool {
	for name := range logMethodNames {
		if strings.Contains(line, "."+name+"(") || strings.Contains(line, "log."+name+"(") {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Test 4 — truthful gateway health split
// ---------------------------------------------------------------------------

func TestGatewayHealthRequiresSnapshotAndControlSync(t *testing.T) {
	health := gateway.NewHealth()
	if health.RouteReady() {
		t.Fatal("a fresh gateway must not report route-ready")
	}
	if health.FRPSProcessHealthy() {
		t.Fatal("frps process health defaults to unknown/unhealthy, never inferred")
	}

	// A route snapshot alone is not readiness: control sync must also be
	// healthy (spec §17.1).
	health.SetSnapshotReady(true)
	if health.RouteReady() {
		t.Fatal("route-ready must require both an applied snapshot and healthy control sync")
	}
	health.SetControlSynced(true)
	if !health.RouteReady() {
		t.Fatal("route-ready must be true once snapshot and control sync are both healthy")
	}

	// frps process health is a different truth: it neither gates nor implies
	// route readiness.
	health.SetFRPSHealthy(true)
	if !health.FRPSProcessHealthy() {
		t.Fatal("frps health must report independently")
	}
	if !health.RouteReady() {
		t.Fatal("frps health must not clear route readiness")
	}
	health.SetSnapshotReady(false)
	if health.RouteReady() {
		t.Fatal("route-ready must fall when the snapshot is withdrawn")
	}
	if !health.FRPSProcessHealthy() {
		t.Fatal("withdrawing the snapshot must not affect frps process health")
	}
	health.SetSnapshotReady(true)
	health.SetControlSynced(false)
	if health.RouteReady() {
		t.Fatal("route-ready must require healthy control sync")
	}
	if !health.FRPSProcessHealthy() {
		t.Fatal("control-sync loss must not affect frps process health")
	}
}

// ---------------------------------------------------------------------------
// bounded log-rate handler (§17.1)
// ---------------------------------------------------------------------------

func TestRateLimitedHandlerBoundsLogRate(t *testing.T) {
	recorder := &recordingHandler{}
	handler := metrics.NewRateLimitedHandler(recorder, 10, 5)
	logger := slog.New(handler)
	for i := 0; i < 50; i++ {
		logger.Info("flood", "n", i)
	}
	if recorder.count() > 6 {
		t.Fatalf("rate limiter let %d records through a burst of 5 at 10/s", recorder.count())
	}
	if recorder.count() == 0 {
		t.Fatal("rate limiter dropped everything; the burst budget must pass")
	}
}

// TestGatewayMetricsEmitBoundedReasonAndScopeLabels proves the gateway wires
// real §17.3 emissions onto the bounded registry: rejections carry only the
// enumerated reason class, ClientHello failures only the enumerated parse
// class, and relayed bytes only the bounded scope dimension.
func TestGatewayMetricsEmitBoundedReasonAndScopeLabels(t *testing.T) {
	const host = "alpha.relay.ns1.sharebridgeusercontent.com"
	agentListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("agent listener: %v", err)
	}
	defer agentListener.Close()
	agentPort := agentListener.Addr().(*net.TCPAddr).Port

	table := routes.NewTable(staticOnlinePresence{})
	if err := table.Apply(routes.Route{
		Hostname: host, AgentRecordID: "agent-metrics", RelayPort: agentPort,
		Generation: 1, SessionID: "session-metrics", Revision: 1, Active: true,
	}); err != nil {
		t.Fatalf("apply route: %v", err)
	}
	registry := metrics.NewRegistry(metrics.Relay)
	server := gateway.NewServer(table, gateway.NewStreams(),
		gateway.WithLogger(slog.New(slog.DiscardHandler)), gateway.WithMetrics(registry))
	publicListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("public listener: %v", err)
	}
	go func() { _ = server.Serve(publicListener) }()
	t.Cleanup(func() { publicListener.Close(); server.Close(); server.Wait() })

	// Unknown SNI -> bounded route rejection.
	driveRejectedConnection(t, publicListener.Addr().String(), clientHelloRecord("unrouted.example.com"))
	// Malformed ClientHello -> bounded clienthello rejection and parse class.
	driveRejectedConnection(t, publicListener.Addr().String(), []byte("CANARY-not-a-tls-record"))

	// A routed connection relays the inspected prefix and counts bytes.
	browser, err := net.DialTimeout("tcp", publicListener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	hello := clientHelloRecord(host)
	if _, err := browser.Write(hello); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	if err := agentListener.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("arm agent accept: %v", err)
	}
	agentConn, err := agentListener.Accept()
	if err != nil {
		t.Fatalf("accept agent: %v", err)
	}
	buffer := make([]byte, len(hello))
	if _, err := io.ReadFull(agentConn, buffer); err != nil {
		t.Fatalf("read replayed prefix: %v", err)
	}
	if !bytes.Equal(buffer, hello) {
		t.Fatal("replayed prefix is not byte-exact")
	}
	browser.Close()
	agentConn.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && registry.Value("sharebridge_relay_public_connections_total", metrics.OutcomeAccepted) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := registry.Value("sharebridge_relay_public_connection_rejections_total", metrics.ReasonRoute); got != 1 {
		t.Fatalf("route rejections = %d, want 1", got)
	}
	if got := registry.Value("sharebridge_relay_public_connection_rejections_total", metrics.ReasonClientHello); got != 1 {
		t.Fatalf("clienthello rejections = %d, want 1", got)
	}
	if got := registry.Value("sharebridge_relay_clienthello_total", metrics.HelloMalformed); got != 1 {
		t.Fatalf("malformed hello count = %d, want 1", got)
	}
	if got := registry.Value("sharebridge_relay_public_connections_total", metrics.OutcomeAccepted); got != 1 {
		t.Fatalf("accepted connections = %d, want 1", got)
	}
	if got := registry.Value("sharebridge_relay_relayed_bytes_total", metrics.ScopeGlobal); got != int64(len(hello)) {
		t.Fatalf("relayed global bytes = %d, want %d", got, len(hello))
	}
}

func driveRejectedConnection(t *testing.T, address string, payload []byte) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = io.Copy(io.Discard, conn)
}

func TestMetricsHistogramRendersBoundedBuckets(t *testing.T) {
	registry := metrics.NewRegistry(metrics.Relay)
	for i := 0; i < 5; i++ {
		registry.Observe("sharebridge_relay_frp_connect_seconds", 0.02)
	}
	rendered := registry.Render()
	if !strings.Contains(rendered, "sharebridge_relay_frp_connect_seconds_count") {
		t.Fatalf("histogram count missing:\n%s", rendered)
	}
	if strings.Contains(rendered, "sharebridge_relay_frp_connect_seconds_bucket{le=\"0.001\"}") {
		t.Fatal("histogram rendered a bucket outside the fixed set")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

type stubHealth struct {
	routeReady  bool
	frpsHealthy bool
}

func (s stubHealth) RouteReady() bool         { return s.routeReady }
func (s stubHealth) FRPSProcessHealthy() bool { return s.frpsHealthy }

type staticOnlinePresence struct{}

func (staticOnlinePresence) Online(string, int, uint64) bool { return true }

// clientHelloRecord builds a structurally valid single-record TLS ClientHello
// whose only extension is server_name carrying sni.
func clientHelloRecord(sni string) []byte {
	hostName := []byte{0x00, byte(len(sni) >> 8), byte(len(sni))}
	hostName = append(hostName, sni...)
	nameList := []byte{byte(len(hostName) >> 8), byte(len(hostName))}
	nameList = append(nameList, hostName...)
	extensions := []byte{0x00, 0x00, byte(len(nameList) >> 8), byte(len(nameList))}
	extensions = append(extensions, nameList...)

	body := []byte{0x03, 0x03}
	body = append(body, bytes.Repeat([]byte{0xA5}, 32)...)
	body = append(body, 0x00)
	body = append(body, 0x00, 0x02, 0x13, 0x01)
	body = append(body, 0x01, 0x00)
	body = append(body, byte(len(extensions)>>8), byte(len(extensions)))
	body = append(body, extensions...)

	handshake := []byte{0x01, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}
	handshake = append(handshake, body...)
	record := []byte{0x16, 0x03, 0x01, byte(len(handshake) >> 8), byte(len(handshake))}
	return append(record, handshake...)
}

type alwaysOfflinePresence struct{}

func (alwaysOfflinePresence) Online(string, int, uint64) bool { return false }

type recordingHandler struct {
	mu      sync.Mutex
	builder strings.Builder
	records int
}

func (r *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (r *recordingHandler) Handle(_ context.Context, record slog.Record) error {
	var buffer bytes.Buffer
	buffer.WriteString(record.Message)
	record.Attrs(func(attr slog.Attr) bool {
		buffer.WriteString(" ")
		buffer.WriteString(attr.Key)
		buffer.WriteString("=")
		buffer.WriteString(attr.Value.String())
		return true
	})
	buffer.WriteString("\n")
	r.mu.Lock()
	r.builder.WriteString(buffer.String())
	r.records++
	r.mu.Unlock()
	return nil
}

func (r *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *recordingHandler) WithGroup(string) slog.Handler      { return r }

func (r *recordingHandler) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.builder.Len()
}

func (r *recordingHandler) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.records
}

func (r *recordingHandler) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.builder.String()
}

func itoa(number int) string { return strconv.Itoa(number) }
