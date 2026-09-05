package frptest

// Real-binary gate fixture for the pinned FRP release. Every gate test runs a
// temporary checksum-verified frps plus real frpc clients against the real
// Task 7 plugin, with one harness-only NewUserConn interceptor in front of it
// (the production plugin learns NewUserConn in the Task 7 amendment; this
// harness proves the wire behavior at the pinned release). Payload bodies are
// captured for response-shape assertions only and are never logged; credential
// material never appears in test output.

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"sharebridge/relay/internal/frpplugin"
)

const (
	gatePluginSecret   = "test-only-plugin-secret"
	gateProxyName      = "sb-sbdeadbeef"
	gateDefaultAgentID = "agent-gate"
	gateTargetGreeting = "sharebridge-gate"

	// Bounded gateway readiness probe budget from §4.4: at most five loopback
	// connect attempts with ~500 ms backoff against a 2.5-second hard
	// deadline, re-checked before each wait. All loops are inline; no
	// goroutines, queues, or unbounded retries.
	gateProbeMaxAttempts = 5
	gateProbeBackoff     = 500 * time.Millisecond
	gateProbeDeadline    = 2500 * time.Millisecond
	gateProbeDialTimeout = 300 * time.Millisecond
)

type gateFixtureOptions struct {
	RejectLogin            bool
	BlockProxyRegistration bool
}

type observedPluginCall struct {
	Operation string
	At        time.Time
	Content   map[string]any
}

type pluginCallRecorder struct {
	mu        sync.Mutex
	calls     []observedPluginCall
	responses map[string][]string
	next      http.Handler
}

type captureResponseWriter struct {
	http.ResponseWriter
	body bytes.Buffer
}

func (writer *captureResponseWriter) Write(payload []byte) (int, error) {
	return writer.body.Write(payload)
}

func (recorder *pluginCallRecorder) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	body, _ := io.ReadAll(request.Body)
	request.Body = io.NopCloser(bytes.NewReader(body))
	var envelope struct {
		Op      string         `json:"op"`
		Content map[string]any `json:"content"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		recorder.mu.Lock()
		recorder.calls = append(recorder.calls, observedPluginCall{Operation: envelope.Op, At: time.Now(), Content: envelope.Content})
		recorder.mu.Unlock()
	}
	captured := &captureResponseWriter{ResponseWriter: writer}
	recorder.next.ServeHTTP(captured, request)
	recorder.mu.Lock()
	if recorder.responses == nil {
		recorder.responses = make(map[string][]string)
	}
	recorder.responses[envelope.Op] = append(recorder.responses[envelope.Op], captured.body.String())
	recorder.mu.Unlock()
	_, _ = writer.Write(captured.body.Bytes())
}

func (recorder *pluginCallRecorder) snapshot() []observedPluginCall {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]observedPluginCall(nil), recorder.calls...)
}

func (recorder *pluginCallRecorder) responsesFor(operation string) []string {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]string(nil), recorder.responses[operation]...)
}

func (recorder *pluginCallRecorder) waitFor(t *testing.T, operation string, count int, timeout time.Duration) []observedPluginCall {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		found := 0
		for _, call := range recorder.snapshot() {
			if call.Operation == operation {
				found++
			}
		}
		if found >= count {
			return recorder.snapshot()
		}
		time.Sleep(25 * time.Millisecond)
	}
	recorder.mu.Lock()
	responseCount := len(recorder.responses[operation])
	recorder.mu.Unlock()
	t.Fatalf("timed out waiting for %d real frps plugin %s calls; saw %d total calls and %d responses (payloads intentionally redacted)", count, operation, len(recorder.snapshot()), responseCount)
	return nil
}

type gatePresenceRecorder struct {
	mu    sync.Mutex
	facts []frpplugin.PresenceFact
}

func (recorder *gatePresenceRecorder) ObserveFRPEvent(fact frpplugin.PresenceFact) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.facts = append(recorder.facts, fact)
}

func (recorder *gatePresenceRecorder) has(operation string) bool {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	for _, fact := range recorder.facts {
		if fact.Operation == operation {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Harness-only NewUserConn interceptor (accept mode, mirroring the production
// plugin's transport policy and the Task 7 amendment's accept-mode readiness
// probe). Everything else passes through to the real plugin untouched.
// ---------------------------------------------------------------------------

type userConnEvent struct {
	At         time.Time
	User       string
	RunID      string
	Generation string
	ProxyName  string
	ProxyType  string
	RemoteAddr string
}

type newUserConnInterceptor struct {
	mu     sync.Mutex
	events []userConnEvent
	next   http.Handler
}

func (interceptor *newUserConnInterceptor) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	peer := net.ParseIP(host)
	username, password, hasAuth := request.BasicAuth()
	if request.Method != http.MethodPost || request.URL.Path != frpplugin.APIPath || err != nil || peer == nil ||
		!peer.IsLoopback() || !hasAuth || username != frpplugin.PluginAuthUsername || password != gatePluginSecret {
		_, _ = io.WriteString(writer, `{"reject":true,"reject_reason":"request rejected","unchange":false}`)
		return
	}
	if request.URL.Query().Get("op") != "NewUserConn" {
		interceptor.next.ServeHTTP(writer, request)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(request.Body, frpplugin.MaxRequestBodyBytes))
	var envelope struct {
		Op      string          `json:"op"`
		Content json.RawMessage `json:"content"`
	}
	var content struct {
		User struct {
			User  string            `json:"user"`
			Metas map[string]string `json:"metas"`
			RunID string            `json:"run_id"`
		} `json:"user"`
		ProxyName  string `json:"proxy_name"`
		ProxyType  string `json:"proxy_type"`
		RemoteAddr string `json:"remote_addr"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Op != "NewUserConn" || json.Unmarshal(envelope.Content, &content) != nil {
		_, _ = io.WriteString(writer, `{"reject":true,"reject_reason":"request rejected","unchange":false}`)
		return
	}
	interceptor.mu.Lock()
	interceptor.events = append(interceptor.events, userConnEvent{
		At: time.Now(), User: content.User.User, RunID: content.User.RunID,
		Generation: content.User.Metas[frpplugin.GenerationMetadataKey],
		ProxyName:  content.ProxyName, ProxyType: content.ProxyType, RemoteAddr: content.RemoteAddr,
	})
	interceptor.mu.Unlock()
	_, _ = io.WriteString(writer, `{"reject":false,"unchange":true}`)
}

func (interceptor *newUserConnInterceptor) snapshot() []userConnEvent {
	interceptor.mu.Lock()
	defer interceptor.mu.Unlock()
	return append([]userConnEvent(nil), interceptor.events...)
}

func (interceptor *newUserConnInterceptor) count() int {
	interceptor.mu.Lock()
	defer interceptor.mu.Unlock()
	return len(interceptor.events)
}

// ---------------------------------------------------------------------------
// Agent-side loopback target: greeting on connect, then echo; per-connection
// inbound byte accounting proves the probe injects no payload bytes.
// ---------------------------------------------------------------------------

type gateTargetConnStats struct {
	Bytes   int64
	Payload string
}

type gateTarget struct {
	listener net.Listener
	mu       sync.Mutex
	conns    []gateTargetConnStats
}

func startGateTarget(t *testing.T) *gateTarget {
	t.Helper()
	listener := listenLoopback(t)
	target := &gateTarget{listener: listener}
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go target.serve(connection)
		}
	}()
	return target
}

func (target *gateTarget) serve(connection net.Conn) {
	defer connection.Close()
	_, _ = connection.Write([]byte(gateTargetGreeting))
	_ = connection.SetReadDeadline(time.Now().Add(15 * time.Second))
	var payload bytes.Buffer
	buffer := make([]byte, 4096)
	for {
		read, err := connection.Read(buffer)
		if read > 0 {
			payload.Write(buffer[:read])
			if _, writeErr := connection.Write(buffer[:read]); writeErr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	target.conns = append(target.conns, gateTargetConnStats{Bytes: int64(payload.Len()), Payload: payload.String()})
}

func (target *gateTarget) snapshot() []gateTargetConnStats {
	target.mu.Lock()
	defer target.mu.Unlock()
	return append([]gateTargetConnStats(nil), target.conns...)
}

// ---------------------------------------------------------------------------
// Process output buffer safe for concurrent pipe writes and test reads.
// ---------------------------------------------------------------------------

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (buffer *syncBuffer) Write(p []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buf.Write(p)
}

func (buffer *syncBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buf.String()
}

// ---------------------------------------------------------------------------
// Real frps + real frpc clients.
// ---------------------------------------------------------------------------

type gateProxySpec struct {
	Name       string
	RemotePort int
}

type gateClientOptions struct {
	Label        string
	AgentID      string
	Generation   int
	ProxyName    string
	RemotePort   int
	UntrustedCA  bool
	ExtraProxies []gateProxySpec
}

type gateClient struct {
	label      string
	generation int
	configPath string
	adminPort  int
	cmd        *exec.Cmd
	out        *syncBuffer
}

type probeExpectation struct {
	ProxyName  string
	RunID      string
	Generation string
}

type probeResult struct {
	Confirmed         bool
	Attempts          int
	Connected         int
	Refused           int
	Foreign           int
	Event             *userConnEvent
	SourceAddr        string
	AuthorizedToEvent time.Duration
	ConnectToEvent    time.Duration
	Total             time.Duration
}

type pinnedFRPFixture struct {
	t    *testing.T
	opts gateFixtureOptions
	dir  string

	frpsPath, frpcPath string
	transportPort      int
	proxyPort          int
	certPath, keyPath  string
	certificateSeq     int
	frpsConfigPath     string

	privateKey   ed25519.PrivateKey
	plugin       *pluginCallRecorder
	interceptor  *newUserConnInterceptor
	pluginServer *http.Server
	pluginLn     net.Listener
	presence     *gatePresenceRecorder

	target  *gateTarget
	blocker net.Listener

	frpsCmd *exec.Cmd
	frpsOut *syncBuffer

	jtiSeq int

	mu      sync.Mutex
	clients map[string]*gateClient
	stopped bool
}

func newPinnedFRPFixture(t *testing.T, opts gateFixtureOptions) *pinnedFRPFixture {
	t.Helper()
	if os.Getenv("SHAREBRIDGE_FRP_GATE") != "1" {
		t.Skip("set SHAREBRIDGE_FRP_GATE=1 to run pinned real-binary gate tests")
	}
	fixture := &pinnedFRPFixture{
		t: t, opts: opts, dir: t.TempDir(), frpsOut: &syncBuffer{},
		clients: make(map[string]*gateClient), presence: &gatePresenceRecorder{},
	}
	// Register cleanup before anything can leak: close() is nil-safe for every
	// field, so a Fatalf between a process Start and later setup registration
	// can never strand the frps/frpc children.
	t.Cleanup(fixture.close)

	fixture.frpsPath, fixture.frpcPath = pinnedFRPBinaries(t)
	fixture.target = startGateTarget(t)
	fixture.proxyPort = freeTCPPort(t)
	fixture.transportPort = freeTCPPort(t)
	if opts.BlockProxyRegistration {
		blocker, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(fixture.proxyPort))
		if err != nil {
			t.Fatalf("pre-bind proxy port to force downstream-registration failure: %v", err)
		}
		fixture.blocker = blocker
	}

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate plugin credential key: %v", err)
	}
	fixture.privateKey = privateKey
	realPlugin, err := frpplugin.NewServer(frpplugin.Config{
		ControlPublicKey: publicKey, PluginSharedSecret: gatePluginSecret,
		RelayPortMin: 1024, RelayPortMax: 65535, PresenceEvents: fixture.presence,
	})
	if err != nil {
		t.Fatalf("new real-plugin handler: %v", err)
	}
	var next http.Handler = realPlugin
	if opts.RejectLogin {
		next = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"reject":true,"reject_reason":"request rejected","unchange":false}`)
		})
	}
	fixture.interceptor = &newUserConnInterceptor{next: next}
	fixture.plugin = &pluginCallRecorder{next: fixture.interceptor}
	fixture.pluginLn = listenLoopback(t)
	fixture.pluginServer = &http.Server{Handler: fixture.plugin}
	go func() { _ = fixture.pluginServer.Serve(fixture.pluginLn) }()

	fixture.certPath, fixture.keyPath = fixture.writeTransportCertificate()
	fixture.writeFrpsConfig()
	fixture.startFrps()
	return fixture
}

func pinnedFRPBinaries(t *testing.T) (string, string) {
	t.Helper()
	manifest := loadFRPManifest(t)
	artifact := manifest.Artifacts["darwin_arm64"]
	if artifact.SHA256 == "" {
		t.Fatal("missing current pinned FRP artifact")
	}
	cache := resolveFRPCacheRoot(t)
	key := filepath.Join(cache, "v"+manifest.Version+"-sha256-"+artifact.SHA256)
	for _, name := range []string{"frps", "frpc"} {
		if _, err := os.Stat(filepath.Join(key, name)); err != nil {
			_, stderr, fetchErr := runFetchFRPScript(t, cache)
			if fetchErr != nil {
				t.Fatalf("fetch checksum-verified pinned FRP: %v: %s", fetchErr, stderr)
			}
			break
		}
	}
	return filepath.Join(key, "frps"), filepath.Join(key, "frpc")
}

func (fixture *pinnedFRPFixture) writeFrpsConfig() {
	fixture.t.Helper()
	pluginAddr := fmt.Sprintf("http://%s:%s@%s", frpplugin.PluginAuthUsername, gatePluginSecret, fixture.pluginLn.Addr())
	fixture.frpsConfigPath = filepath.Join(fixture.dir, "frps.toml")
	config := fmt.Sprintf(`bindAddr = "127.0.0.1"
bindPort = %d
proxyBindAddr = "127.0.0.1"
allowPorts = [{ single = %d }]
maxPortsPerClient = 1
userConnTimeout = 10
transport.tcpMux = false
transport.heartbeatTimeout = 45
transport.tls.force = true
transport.tls.certFile = %q
transport.tls.keyFile = %q
[[httpPlugins]]
name = "sharebridge-authorize-presence"
addr = %q
path = %q
ops = ["Login", "NewProxy", "CloseProxy", "Ping", "NewUserConn"]
`, fixture.transportPort, fixture.proxyPort, fixture.certPath, fixture.keyPath, pluginAddr, frpplugin.APIPath)
	writeGateFile(fixture.t, fixture.frpsConfigPath, config)
}

func (fixture *pinnedFRPFixture) startFrps() {
	fixture.t.Helper()
	cmd := exec.Command(fixture.frpsPath, "-c", fixture.frpsConfigPath)
	cmd.Stdout, cmd.Stderr = fixture.frpsOut, fixture.frpsOut
	if err := cmd.Start(); err != nil {
		fixture.t.Fatalf("start real frps: %v", err)
	}
	fixture.frpsCmd = cmd
	fixture.waitForTCPOpen(fixture.transportAddr(), 3*time.Second)
}

func (fixture *pinnedFRPFixture) frpsLog() string {
	return fixture.frpsOut.String()
}

// adminProxyStatuses queries the real frpc admin API for the runtime state of
// every configured proxy, proving enforcement on the wire rather than in
// config text. Polls briefly because the proxy table fills asynchronously.
func (fixture *pinnedFRPFixture) adminProxyStatuses(t *testing.T, client *gateClient) []adminProxyStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		request, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/api/status", client.adminPort), nil)
		if err != nil {
			t.Fatalf("build admin status request: %v", err)
		}
		request.SetBasicAuth("gate", "gate")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			lastErr = err
			time.Sleep(100 * time.Millisecond)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
		_ = response.Body.Close()
		var status struct {
			Code int                `json:"code"`
			TCP  []adminProxyStatus `json:"tcp"`
		}
		if json.Unmarshal(body, &status) == nil && status.Code == 0 && len(status.TCP) > 0 {
			return status.TCP
		}
		lastErr = fmt.Errorf("status code %d body %dB", status.Code, len(body))
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out querying frpc admin status on port %d: %v", client.adminPort, lastErr)
	return nil
}

type adminProxyStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Err    string `json:"err"`
}

func (fixture *pinnedFRPFixture) stopFrps() {
	if fixture.frpsCmd != nil && fixture.frpsCmd.Process != nil {
		_ = fixture.frpsCmd.Process.Kill()
		_, _ = fixture.frpsCmd.Process.Wait()
	}
	fixture.frpsCmd = nil
}

func (fixture *pinnedFRPFixture) startClient(opts gateClientOptions) *gateClient {
	fixture.t.Helper()
	if opts.AgentID == "" {
		opts.AgentID = gateDefaultAgentID
	}
	if opts.Generation == 0 {
		opts.Generation = 1
	}
	if opts.ProxyName == "" {
		opts.ProxyName = gateProxyName
	}
	if opts.RemotePort == 0 {
		opts.RemotePort = fixture.proxyPort
	}
	fixture.jtiSeq++
	jti := fmt.Sprintf("%s-%d", opts.Label, fixture.jtiSeq)
	token := fixture.signCredential(opts.RemotePort, opts.AgentID, opts.Generation, jti)
	adminPort := freeTCPPort(fixture.t)
	configPath := filepath.Join(fixture.dir, "frpc-"+opts.Label+".toml")
	trustedCA := fixture.certPath
	if opts.UntrustedCA {
		// A distinct self-signed CA: the client pins verification to a CA that
		// did not sign the relay's presented certificate, so the transport
		// handshake must fail closed before any plugin traffic.
		untrustedCertPath, _ := fixture.writeTransportCertificate()
		trustedCA = untrustedCertPath
	}
	var proxies strings.Builder
	writeProxy := func(spec gateProxySpec) {
		fmt.Fprintf(&proxies, `[[proxies]]
name = %q
type = "tcp"
localIP = "127.0.0.1"
localPort = %d
remotePort = %d
transport.useCompression = false
`, spec.Name, fixture.target.listener.Addr().(*net.TCPAddr).Port, spec.RemotePort)
	}
	writeProxy(gateProxySpec{Name: opts.ProxyName, RemotePort: opts.RemotePort})
	for _, extra := range opts.ExtraProxies {
		writeProxy(extra)
	}
	config := fmt.Sprintf(`serverAddr = "127.0.0.1"
serverPort = %d
loginFailExit = false
metadatas.sharebridge_credential = %q
metadatas.sharebridge_generation = %q
transport.tcpMux = false
transport.poolCount = 1
transport.tls.enable = true
transport.tls.trustedCaFile = %q
transport.tls.serverName = "localhost"
transport.heartbeatInterval = 10
transport.heartbeatTimeout = 45
webServer.addr = "127.0.0.1"
webServer.port = %d
webServer.user = "gate"
webServer.password = "gate"
%s`, fixture.transportPort, token, strconv.Itoa(opts.Generation), trustedCA, adminPort, proxies.String())
	writeGateFile(fixture.t, configPath, config)
	out := &syncBuffer{}
	cmd := exec.Command(fixture.frpcPath, "-c", configPath)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		fixture.t.Fatalf("start frpc %s: %v", opts.Label, err)
	}
	client := &gateClient{label: opts.Label, generation: opts.Generation, configPath: configPath, adminPort: adminPort, cmd: cmd, out: out}
	fixture.mu.Lock()
	fixture.clients[opts.Label] = client
	fixture.mu.Unlock()
	return client
}

// stopClient stops one labeled frpc. Graceful uses the admin `frpc stop` plus
// SIGINT so frps observes CloseProxy; hard kills the process so the server
// observes an abrupt control loss.
func (fixture *pinnedFRPFixture) stopClient(label string, graceful bool) {
	fixture.mu.Lock()
	client, ok := fixture.clients[label]
	delete(fixture.clients, label)
	fixture.mu.Unlock()
	if !ok || client.cmd == nil || client.cmd.Process == nil {
		return
	}
	if graceful {
		_ = exec.Command(fixture.frpcPath, "stop", "-c", client.configPath).Run()
		_ = client.cmd.Process.Signal(os.Interrupt)
	} else {
		_ = client.cmd.Process.Kill()
	}
	_, _ = client.cmd.Process.Wait()
	client.cmd = nil
}

func (fixture *pinnedFRPFixture) client() *gateClient {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	for _, client := range fixture.clients {
		return client
	}
	return nil
}

func (fixture *pinnedFRPFixture) freezeClient(label string) {
	fixture.signalClient(label, syscall.SIGSTOP)
}

func (fixture *pinnedFRPFixture) thawClient(label string) {
	fixture.mu.Lock()
	client, ok := fixture.clients[label]
	fixture.mu.Unlock()
	if !ok || client.cmd == nil || client.cmd.Process == nil {
		return
	}
	_ = client.cmd.Process.Signal(syscall.SIGCONT)
}

func (fixture *pinnedFRPFixture) signalClient(label string, signal syscall.Signal) {
	fixture.t.Helper()
	fixture.mu.Lock()
	client, ok := fixture.clients[label]
	fixture.mu.Unlock()
	if !ok || client.cmd == nil || client.cmd.Process == nil {
		fixture.t.Fatalf("cannot signal unknown client %q", label)
	}
	if err := client.cmd.Process.Signal(signal); err != nil {
		fixture.t.Fatalf("signal frpc %s with %v: %v", label, signal, err)
	}
}

func (fixture *pinnedFRPFixture) close() {
	if fixture.stopped {
		return
	}
	fixture.stopped = true
	fixture.mu.Lock()
	labels := make([]string, 0, len(fixture.clients))
	for label := range fixture.clients {
		labels = append(labels, label)
	}
	fixture.mu.Unlock()
	for _, label := range labels {
		fixture.stopClient(label, true)
	}
	fixture.stopFrps()
	if fixture.pluginServer != nil {
		_ = fixture.pluginServer.Close()
	}
	if fixture.blocker != nil {
		_ = fixture.blocker.Close()
	}
	if fixture.target != nil {
		_ = fixture.target.listener.Close()
	}
}

func (fixture *pinnedFRPFixture) signCredential(port int, agentID string, generation int, jti string) string {
	fixture.t.Helper()
	// Single clock reading: two separate time.Now() calls can tick past the
	// plugin's exact 10-minute maximum credential lifetime and be rejected.
	now := time.Now()
	claims := map[string]any{
		"iss": "sharebridge-control", "aud": "sharebridge-relay", "api_key_id": "api-gate",
		"agent_record_id": agentID, "namespace": "sbdeadbeef", "proxy_name": gateProxyName,
		"relay_port": port, "generation": generation, "issued_at": now.Add(-time.Minute).UTC(),
		"expires_at": now.Add(9 * time.Minute).UTC(), "jti": jti,
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		fixture.t.Fatalf("marshal credential: %v", err)
	}
	signature := ed25519.Sign(fixture.privateKey, payload)
	return "sbrelay1." + base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func (fixture *pinnedFRPFixture) writeTransportCertificate() (string, string) {
	fixture.t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		fixture.t.Fatalf("generate transport TLS key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"localhost"}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true,
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		fixture.t.Fatalf("create transport TLS certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		fixture.t.Fatalf("marshal transport TLS key: %v", err)
	}
	fixture.certificateSeq++
	certificatePath := filepath.Join(fixture.dir, fmt.Sprintf("transport-%d.crt", fixture.certificateSeq))
	keyPath := filepath.Join(fixture.dir, fmt.Sprintf("transport-%d.key", fixture.certificateSeq))
	writeGateFile(fixture.t, certificatePath, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER})))
	writeGateFile(fixture.t, keyPath, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})))
	return certificatePath, keyPath
}

// ---------------------------------------------------------------------------
// Bounded gateway readiness probe (the §4.4/§7.3 production mechanism).
// ---------------------------------------------------------------------------

// readinessProbe dials the loopback proxy port from its own sockets and waits
// a bounded time for a NewUserConn callback whose remote_addr is exactly the
// probe socket's address and whose proxy_name, server run id, and generation
// metadata match the current authorized session. It writes and reads no
// payload bytes; at most five attempts with ~500 ms backoff under a 2.5 s
// hard deadline that is re-checked before every wait.
func (fixture *pinnedFRPFixture) readinessProbe(expectation probeExpectation, authorizedAt time.Time) probeResult {
	start := time.Now()
	deadline := start.Add(gateProbeDeadline)
	result := probeResult{}
	for result.Attempts < gateProbeMaxAttempts && time.Now().Before(deadline) {
		result.Attempts++
		dialTimeout := gateProbeDialTimeout
		if remaining := time.Until(deadline); remaining < dialTimeout {
			dialTimeout = remaining
		}
		connectedAt := time.Now()
		connection, err := net.DialTimeout("tcp", fixture.proxyAddr(), dialTimeout)
		if err != nil {
			result.Refused++
			if !time.Now().Add(gateProbeBackoff).Before(deadline) {
				break
			}
			time.Sleep(gateProbeBackoff)
			continue
		}
		result.Connected++
		result.SourceAddr = connection.LocalAddr().String()
		waitEnd := time.Now().Add(gateProbeBackoff)
		if waitEnd.After(deadline) {
			waitEnd = deadline
		}
		if event, ok := fixture.awaitProbeEvent(expectation, result.SourceAddr, waitEnd); ok {
			result.Event = event
			result.Confirmed = true
			result.ConnectToEvent = event.At.Sub(connectedAt)
			if !authorizedAt.IsZero() {
				result.AuthorizedToEvent = event.At.Sub(authorizedAt)
			}
			result.noteForeign(fixture.interceptor.snapshot(), expectation)
			_ = connection.Close()
			result.Total = time.Since(start)
			return result
		}
		_ = connection.Close()
		if !time.Now().Add(gateProbeBackoff).Before(deadline) {
			break
		}
		time.Sleep(gateProbeBackoff)
	}
	result.noteForeign(fixture.interceptor.snapshot(), expectation)
	result.Total = time.Since(start)
	return result
}

// awaitProbeEvent polls already-observed NewUserConn callbacks until waitEnd
// for one whose full correlation tuple matches this probe socket. Callbacks
// that share the socket but not the session identity are foreign.
func (fixture *pinnedFRPFixture) awaitProbeEvent(expectation probeExpectation, sourceAddr string, waitEnd time.Time) (*userConnEvent, bool) {
	for time.Now().Before(waitEnd) {
		for _, event := range fixture.interceptor.snapshot() {
			if event.RemoteAddr != sourceAddr {
				continue
			}
			if event.ProxyName == expectation.ProxyName && event.RunID == expectation.RunID && event.Generation == expectation.Generation {
				eventCopy := event
				return &eventCopy, true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil, false
}

func (result *probeResult) noteForeign(events []userConnEvent, expectation probeExpectation) {
	seen := make(map[string]bool)
	for _, event := range events {
		if event.RemoteAddr != result.SourceAddr {
			continue
		}
		if event.ProxyName == expectation.ProxyName && event.RunID == expectation.RunID && event.Generation == expectation.Generation {
			continue
		}
		key := event.At.String()
		if !seen[key] {
			seen[key] = true
			result.Foreign++
		}
	}
}

// ---------------------------------------------------------------------------
// Observation helpers.
// ---------------------------------------------------------------------------

func pluginUserString(content map[string]any, field string) string {
	user, _ := content["user"].(map[string]any)
	value, _ := user[field].(string)
	return value
}

func pluginUserMeta(content map[string]any, key string) string {
	user, _ := content["user"].(map[string]any)
	metas, _ := user["metas"].(map[string]any)
	value, _ := metas[key].(string)
	return value
}

func (fixture *pinnedFRPFixture) callsOf(operation string) []observedPluginCall {
	var result []observedPluginCall
	for _, call := range fixture.plugin.snapshot() {
		if call.Operation == operation {
			result = append(result, call)
		}
	}
	return result
}

func (fixture *pinnedFRPFixture) pings() []observedPluginCall {
	return fixture.callsOf(frpplugin.OperationPing)
}

func (fixture *pinnedFRPFixture) waitForPings(count int, timeout time.Duration) []observedPluginCall {
	fixture.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pings := fixture.pings(); len(pings) >= count {
			return pings
		}
		time.Sleep(50 * time.Millisecond)
	}
	fixture.t.Fatalf("timed out waiting for %d authenticated Pings; saw %d", count, len(fixture.pings()))
	return nil
}

func (fixture *pinnedFRPFixture) waitForPingAfter(after time.Time, timeout time.Duration) observedPluginCall {
	fixture.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, call := range fixture.pings() {
			if call.At.After(after) {
				return call
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	fixture.t.Fatalf("no authenticated Ping arrived after %s within %s", after.Format(time.RFC3339Nano), timeout)
	return observedPluginCall{}
}

func (fixture *pinnedFRPFixture) newProxyCallsOfGeneration(generation string) []observedPluginCall {
	var matches []observedPluginCall
	for _, call := range fixture.callsOf(frpplugin.OperationNewProxy) {
		if pluginUserMeta(call.Content, frpplugin.GenerationMetadataKey) == generation {
			matches = append(matches, call)
		}
	}
	return matches
}

func (fixture *pinnedFRPFixture) newProxyCallsFor(proxyName string) []observedPluginCall {
	var matches []observedPluginCall
	for _, call := range fixture.callsOf(frpplugin.OperationNewProxy) {
		if fmt.Sprint(call.Content["proxy_name"]) == proxyName {
			matches = append(matches, call)
		}
	}
	return matches
}

func (fixture *pinnedFRPFixture) waitNthNewProxyCall(generation string, index int, timeout time.Duration) observedPluginCall {
	fixture.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if matches := fixture.newProxyCallsOfGeneration(generation); len(matches) >= index {
			return matches[index-1]
		}
		time.Sleep(25 * time.Millisecond)
	}
	fixture.t.Fatalf("timed out waiting for NewProxy call #%d of generation %s; frps output:\n%s", index, generation, fixture.frpsOut.String())
	return observedPluginCall{}
}

func probeExpectationFromNewProxyCall(call observedPluginCall) probeExpectation {
	return probeExpectation{
		ProxyName:  fmt.Sprint(call.Content["proxy_name"]),
		RunID:      pluginUserString(call.Content, "run_id"),
		Generation: pluginUserMeta(call.Content, frpplugin.GenerationMetadataKey),
	}
}

// ---------------------------------------------------------------------------
// Transport and address helpers.
// ---------------------------------------------------------------------------

func (fixture *pinnedFRPFixture) proxyAddr() string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(fixture.proxyPort))
}

func (fixture *pinnedFRPFixture) transportAddr() string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(fixture.transportPort))
}

func (fixture *pinnedFRPFixture) waitForTCPOpen(address string, timeout time.Duration) {
	fixture.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	fixture.t.Fatalf("timed out waiting for %s; frps output:\n%s", address, fixture.frpsOut.String())
}

func (fixture *pinnedFRPFixture) waitTCPClosed(address string, timeout time.Duration) (time.Time, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp", address, 250*time.Millisecond)
		if err != nil {
			return time.Now(), true
		}
		_ = connection.Close()
		time.Sleep(250 * time.Millisecond)
	}
	return time.Now(), false
}

func readProxyGreeting(t *testing.T, address string) {
	t.Helper()
	connection, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		t.Fatalf("proxy refused a user connection: %v", err)
	}
	defer connection.Close()
	greeting := make([]byte, len(gateTargetGreeting))
	_ = connection.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(connection, greeting); err != nil || string(greeting) != gateTargetGreeting {
		t.Fatalf("proxy did not forward the loopback target greeting: %q, %v", greeting, err)
	}
}

func sleepUntil(t *testing.T, until time.Time) {
	t.Helper()
	if remaining := time.Until(until); remaining > 0 {
		time.Sleep(remaining)
	}
}

func listenLoopback(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen loopback: %v", err)
	}
	return listener
}

func freeTCPPort(t *testing.T) int {
	listener := listenLoopback(t)
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// freeQuietPort returns a port from a range the kernel does not use for
// automatic ephemeral allocations, so neighbor assertions stay reliable.
func freeQuietPort(t *testing.T) int {
	t.Helper()
	for port := 20000; port < 30000; port++ {
		listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err == nil {
			_ = listener.Close()
			return port
		}
	}
	t.Fatal("no free quiet-range port")
	return 0
}

func writeGateFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func nonLoopbackIPv4Addresses() []string {
	var result []string
	interfaces, _ := net.Interfaces()
	for _, iface := range interfaces {
		addresses, _ := iface.Addrs()
		for _, address := range addresses {
			ip, _, _ := net.ParseCIDR(address.String())
			if ip != nil && ip.To4() != nil && !ip.IsLoopback() {
				result = append(result, ip.String())
			}
		}
	}
	return result
}
