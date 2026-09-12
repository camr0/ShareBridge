// Package controlintegration is the cross-module control-sync integration
// test module: the REAL gateway control-sync client/loop (relay/internal/
// controlsync) driven against the REAL control sync listener (control/
// internal/relayctl) over a REAL mutually-authenticated TLS 1.3 socket.
//
// Why a separate module (and why the gateway half runs as a subprocess):
// Go's internal-package rule is an import-path prefix rule, so a single
// package can be rooted under sharebridge/relay/ OR sharebridge/control/ but
// never both. This module's path is under sharebridge/control/, so it imports
// the real control listener, publisher, and presence view in-process; the
// gateway half cannot be imported here at all, so the test builds a tiny
// gateway harness (gatewayharness/main.go.txt) into a throwaway module rooted
// at sharebridge/relay/ and drives it over stdin/stdout. Both halves are the
// production types; only the process boundary is synthetic. Neither
// production module gains a cross-module dependency.
//
// Everything is hermetic: ephemeral 127.0.0.1 ports, in-test CAs, a
// hermetic PocketBase app in t.TempDir(), injected clocks, and no external
// network (the harness build runs with GOPROXY=off; the relay module has no
// external requirements).
package controlintegration

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"sharebridge/control/internal/relayctl"
	"sharebridge/control/migrations"
)

// Sync identities and fixed scenario values. Production SANs are
// operator-provisioned; these mirror the §17.1 service roles.
const (
	controlIdentity = "sharebridge-control.sync.internal"
	gatewayIdentity = "sharebridge-relay-gateway.sync.internal"

	gatewayNamespace = "sb0a1b2c3d"
	gatewayBootID    = "gw-crossmodule-boot-1"
	freshGatewayBoot = "gw-crossmodule-boot-2"

	epochOne         = uint64(7777)
	epochTwo         = uint64(8888)
	revisionSeed     = uint64(1000)
	snapshotRevision = uint64(1001) // after PublishAdd(session1)
	deltaRevision    = uint64(1002) // after PublishAdd(session2)

	agentRelayPort  = 10001
	agentGeneration = uint64(3)

	// clockBase is the single fixed wall-clock base shared by the in-process
	// control clock and the subprocess gateway presence clock.
	clockBaseRFC3339 = "2026-09-03T12:00:00Z"

	presenceLeaseMS = int64(45000)
)

var clockBase = func() time.Time {
	parsed, err := time.Parse(time.RFC3339, clockBaseRFC3339)
	if err != nil {
		panic(err)
	}
	return parsed
}()

// --- certificate material (independent of the content PKI) ---

type syncCertificates struct {
	serverCAPEM     []byte
	serverCertPEM   []byte
	serverKeyPEM    []byte
	gatewayCAPEM    []byte
	gatewayCertPEM  []byte
	gatewayKeyPEM   []byte
	foreignCertPEM  []byte
	foreignKeyPEM   []byte
	wrongSANCertPEM []byte
	wrongSANKeyPEM  []byte
}

func generateTestCA(t *testing.T, commonName string) ([]byte, *x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{"sharebridge-test"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("generate CA certificate: %v", err)
	}
	caCertificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), caCertificate, caKey
}

func issueTestLeaf(t *testing.T, caCertificate *x509.Certificate, caKey *ecdsa.PrivateKey, dnsSAN string, serverAuth bool) ([]byte, []byte) {
	t.Helper()
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	extKeyUsage := []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	if serverAuth {
		extKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: dnsSAN, Organization: []string{"sharebridge-test"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  extKeyUsage,
		DNSNames:     []string{dnsSAN},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCertificate, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("issue leaf certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func newSyncCertificates(t *testing.T) syncCertificates {
	t.Helper()
	var certs syncCertificates
	serverCAPEM, serverIssuer, serverIssuerKey := generateTestCA(t, "sharebridge-crossmodule-control-ca")
	certs.serverCAPEM = serverCAPEM
	certs.serverCertPEM, certs.serverKeyPEM = issueTestLeaf(t, serverIssuer, serverIssuerKey, controlIdentity, true)

	gatewayCAPEM, gatewayIssuer, gatewayIssuerKey := generateTestCA(t, "sharebridge-crossmodule-gateway-ca")
	certs.gatewayCAPEM = gatewayCAPEM
	certs.gatewayCertPEM, certs.gatewayKeyPEM = issueTestLeaf(t, gatewayIssuer, gatewayIssuerKey, gatewayIdentity, false)
	certs.wrongSANCertPEM, certs.wrongSANKeyPEM = issueTestLeaf(t, gatewayIssuer, gatewayIssuerKey, "sharebridge-relay-gateway.impersonator.invalid", false)

	_, foreignIssuer, foreignIssuerKey := generateTestCA(t, "sharebridge-crossmodule-foreign-ca")
	certs.foreignCertPEM, certs.foreignKeyPEM = issueTestLeaf(t, foreignIssuer, foreignIssuerKey, gatewayIdentity, false)
	return certs
}

type gatewayCertificateFiles struct {
	serverCA     string
	clientCert   string
	clientKey    string
	foreignCert  string
	foreignKey   string
	wrongSANCert string
	wrongSANKey  string
}

func writeCertificateFiles(t *testing.T, certs syncCertificates) gatewayCertificateFiles {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}
	return gatewayCertificateFiles{
		serverCA:     write("server-ca.pem", certs.serverCAPEM),
		clientCert:   write("gateway.pem", certs.gatewayCertPEM),
		clientKey:    write("gateway-key.pem", certs.gatewayKeyPEM),
		foreignCert:  write("foreign.pem", certs.foreignCertPEM),
		foreignKey:   write("foreign-key.pem", certs.foreignKeyPEM),
		wrongSANCert: write("wrong-san.pem", certs.wrongSANCertPEM),
		wrongSANKey:  write("wrong-san-key.pem", certs.wrongSANKeyPEM),
	}
}

// --- hermetic PocketBase app + records (mirrors control's publisher tests) ---

func newPublisherTestApp(t *testing.T) core.App {
	t.Helper()
	app := core.NewBaseApp(core.BaseAppConfig{
		DataDir:       t.TempDir(),
		EncryptionEnv: "pb_crossmodule_test_env",
	})
	if err := app.Bootstrap(); err != nil {
		t.Fatalf("bootstrap PocketBase: %v", err)
	}
	for _, migration := range []func(core.App) error{
		migrations.CreateCollections,
		migrations.AddRelayOnly,
		migrations.AddSessionRelayStaticPub,
		migrations.AddImmichSessionFields,
		migrations.CreateAgents,
		migrations.AddSessionsInactiveReason,
		migrations.AddAgentsRelaySTUN,
	} {
		if err := migration(app); err != nil {
			t.Fatalf("apply migration: %v", err)
		}
	}
	return app
}

func createPublisherAgent(t *testing.T, app core.App, label, namespace string, relayPort, relayGeneration int) (string, string) {
	t.Helper()
	usersCollection, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatalf("users collection: %v", err)
	}
	user := core.NewRecord(usersCollection)
	user.SetEmail(label + "@crossmodule-test.example.com")
	user.SetPassword("crossmodule-test-password")
	user.Set("relay_quota_gb", 50.0)
	if err := app.Save(user); err != nil {
		t.Fatalf("save user: %v", err)
	}
	keysCollection, err := app.FindCollectionByNameOrId("api_keys")
	if err != nil {
		t.Fatalf("api_keys collection: %v", err)
	}
	key := core.NewRecord(keysCollection)
	key.Set("account_id", user.Id)
	key.Set("is_active", true)
	key.Set("label", "crossmodule test key")
	if err := app.Save(key); err != nil {
		t.Fatalf("save api key: %v", err)
	}
	agentsCollection, err := app.FindCollectionByNameOrId("agents")
	if err != nil {
		t.Fatalf("agents collection: %v", err)
	}
	agent := core.NewRecord(agentsCollection)
	agent.Set("api_key_id", key.Id)
	agent.Set("namespace", namespace)
	agent.Set("cert_status", "ready")
	agent.Set("endpoint_port", 0)
	agent.Set("relay_port", relayPort)
	agent.Set("relay_generation", relayGeneration)
	if err := app.Save(agent); err != nil {
		t.Fatalf("save agent: %v", err)
	}
	return key.Id, agent.Id
}

func createPublisherSession(t *testing.T, app core.App, apiKeyID, code, origin string) string {
	t.Helper()
	sessionsCollection, err := app.FindCollectionByNameOrId("sessions")
	if err != nil {
		t.Fatalf("sessions collection: %v", err)
	}
	record := core.NewRecord(sessionsCollection)
	record.Set("code", code)
	record.Set("api_key_id", apiKeyID)
	record.Set("agent_id", "agent-"+code)
	record.Set("origin", origin)
	record.Set("is_active", true)
	record.Set("share_type", "immich")
	record.Set("is_password_protected", false)
	record.Set("relay_only", false)
	if err := app.Save(record); err != nil {
		t.Fatalf("save session %s: %v", code, err)
	}
	return record.Id
}

// --- injectable control clock ---

type fakeControlClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeControlClock() *fakeControlClock {
	return &fakeControlClock{now: clockBase}
}

func (clock *fakeControlClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeControlClock) Advance(delta time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(delta)
	clock.mu.Unlock()
}

// --- one real control instance (publisher + presence view + sync server) ---

type controlInstance struct {
	publisher *relayctl.Publisher
	view      *relayctl.PresenceView
	server    *relayctl.Server
}

func newControlInstance(t *testing.T, app core.App, clock *fakeControlClock, certs syncCertificates, epoch, seed uint64) *controlInstance {
	t.Helper()
	publisher, err := relayctl.NewPublisher(app, relayctl.PublisherConfig{
		Epoch:        epoch,
		RevisionSeed: seed,
		Now:          clock.Now,
	})
	if err != nil {
		t.Fatalf("new control publisher: %v", err)
	}
	view, err := relayctl.NewPresenceView(relayctl.PresenceViewConfig{
		App:    app,
		Routes: publisher,
		Now:    clock.Now,
	})
	if err != nil {
		t.Fatalf("new control presence view: %v", err)
	}
	server, err := relayctl.NewServer(relayctl.ServerConfig{
		BindAddress:       "127.0.0.1:0",
		ServerCertPEM:     certs.serverCertPEM,
		ServerKeyPEM:      certs.serverKeyPEM,
		ClientCAPEM:       certs.gatewayCAPEM,
		ExpectedClientSAN: gatewayIdentity,
		RouteSource:       publisher,
		PresenceSink:      view,
		StatusSink:        publisher,
	})
	if err != nil {
		t.Fatalf("new control sync server: %v", err)
	}
	return &controlInstance{publisher: publisher, view: view, server: server}
}

// swapHandler is the stable HTTP handler the TLS listener is built with. A
// control restart is modelled by pointing it at a brand-new relayctl.Server
// (whose acknowledgement dedup cache is empty, exactly like a restarted
// process) while the TLS listener and the gateway's pinned endpoint stay up.
type swapHandler struct {
	mu         sync.Mutex
	handler    http.Handler
	ackGate    chan struct{}
	ackBlocked chan struct{}
}

func newSwapHandler(handler http.Handler) *swapHandler {
	return &swapHandler{handler: handler}
}

func (swap *swapHandler) set(handler http.Handler) {
	swap.mu.Lock()
	swap.handler = handler
	swap.mu.Unlock()
}

// armAckGate parks the NEXT /status request inside the handler until release
// is closed, letting the test swap in a restarted control between the
// gateway's fetch and its acknowledgement (a deterministic in-flight
// cross-restart ack). blocked is closed once a /status request is parked.
func (swap *swapHandler) armAckGate() (blocked chan struct{}, release chan struct{}) {
	blocked = make(chan struct{})
	release = make(chan struct{})
	swap.mu.Lock()
	swap.ackGate = release
	swap.ackBlocked = blocked
	swap.mu.Unlock()
	return blocked, release
}

func (swap *swapHandler) ServeHTTP(responseWriter http.ResponseWriter, request *http.Request) {
	if request.URL.Path == relayctl.PathStatus {
		swap.mu.Lock()
		gate, blocked := swap.ackGate, swap.ackBlocked
		if gate != nil {
			swap.ackGate, swap.ackBlocked = nil, nil
		}
		swap.mu.Unlock()
		if gate != nil {
			close(blocked)
			<-gate
		}
	}
	swap.mu.Lock()
	handler := swap.handler
	swap.mu.Unlock()
	handler.ServeHTTP(responseWriter, request)
}

func startControlListener(t *testing.T, certs syncCertificates, handler http.Handler, tlsConfig *tls.Config) *httptest.Server {
	t.Helper()
	listener := httptest.NewUnstartedServer(handler)
	listener.TLS = tlsConfig
	listener.StartTLS()
	t.Cleanup(listener.Close)
	return listener
}

// --- raw mTLS client (used for the mTLS-enforcement and wire assertions) ---

func newRawHTTPClient(t *testing.T, serverCAPEM, clientCertPEM, clientKeyPEM []byte, expectedServerSAN string) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(serverCAPEM) {
		t.Fatalf("server CA PEM rejected")
	}
	var certificates []tls.Certificate
	if clientCertPEM != nil {
		pair, err := tls.X509KeyPair(clientCertPEM, clientKeyPEM)
		if err != nil {
			t.Fatalf("client key pair: %v", err)
		}
		certificates = []tls.Certificate{pair}
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:      pool,
		Certificates: certificates,
		ServerName:   expectedServerSAN,
		MinVersion:   tls.VersionTLS13,
	}}}
}

func postStatusAckRaw(t *testing.T, client *http.Client, url string, ack relayctl.StatusAck) relayctl.StatusAckResponse {
	t.Helper()
	body, err := json.Marshal(ack)
	if err != nil {
		t.Fatalf("marshal status ack: %v", err)
	}
	response, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /status: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(response.Body)
		t.Fatalf("POST /status status = %d body %s, want 200", response.StatusCode, payload)
	}
	var decoded relayctl.StatusAckResponse
	decoder := json.NewDecoder(response.Body)
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("decode status ack response: %v", err)
	}
	return decoded
}

// --- gateway subprocess ---

// harnessResponse mirrors the harness's JSON protocol.
type harnessResponse struct {
	OK              bool   `json:"ok"`
	Error           string `json:"error,omitempty"`
	Ready           bool   `json:"ready"`
	Synced          bool   `json:"synced"`
	Healthy         bool   `json:"healthy"`
	AppliedEpoch    uint64 `json:"applied_epoch"`
	AppliedRevision uint64 `json:"applied_revision"`
	Routes          int    `json:"routes"`
	PublishCount    int    `json:"publish_count"`
}

type gatewayProcess struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *bytes.Buffer
}

func buildGatewayHarness(t *testing.T) string {
	t.Helper()
	relayDir, err := filepath.Abs("../../relay")
	if err != nil {
		t.Fatalf("resolve relay module dir: %v", err)
	}
	source, err := os.ReadFile("gatewayharness/main.go.txt")
	if err != nil {
		t.Fatalf("read harness source: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), source, 0o600); err != nil {
		t.Fatalf("write harness source: %v", err)
	}
	goMod := fmt.Sprintf("module sharebridge/relay/gwharness\n\ngo 1.26.1\n\nrequire sharebridge/relay v0.0.0\n\nreplace sharebridge/relay => %s\n", relayDir)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o600); err != nil {
		t.Fatalf("write harness go.mod: %v", err)
	}
	binary := filepath.Join(dir, "gwharness")
	build := exec.Command("go", "build", "-mod=mod", "-o", binary, ".")
	build.Dir = dir
	build.Env = append(os.Environ(), "GOWORK=off", "GOPROXY=off")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build gateway harness: %v\n%s", err, output)
	}
	return binary
}

func gatewayEnv(t *testing.T, baseURL string, files gatewayCertificateFiles, bootID, presenceIntervalMS string, autostart bool) map[string]string {
	t.Helper()
	autostartValue := "1"
	if !autostart {
		autostartValue = "0"
	}
	return map[string]string{
		"GW_BASE_URL":             baseURL,
		"GW_SERVER_SAN":           controlIdentity,
		"GW_SERVER_CA_FILE":       files.serverCA,
		"GW_CLIENT_CERT_FILE":     files.clientCert,
		"GW_CLIENT_KEY_FILE":      files.clientKey,
		"GW_BOOT_ID":              bootID,
		"GW_NAMESPACE":            gatewayNamespace,
		"GW_PRESENCE_INTERVAL_MS": presenceIntervalMS,
		"GW_PRESENCE_AUTOSTART":   autostartValue,
		"GW_CLOCK_BASE":           clockBaseRFC3339,
	}
}

func startGatewayProcess(t *testing.T, binary string, env map[string]string) *gatewayProcess {
	t.Helper()
	cmd := exec.Command(binary)
	cmd.Env = os.Environ()
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("gateway stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("gateway stdout: %v", err)
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start gateway harness: %v", err)
	}
	process := &gatewayProcess{t: t, cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout), stderr: stderr}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	var banner harnessResponse
	if err := process.receive(&banner); err != nil {
		t.Fatalf("gateway harness did not start: %v\nstderr:\n%s", err, stderr.String())
	}
	return process
}

func (process *gatewayProcess) send(request any) {
	process.t.Helper()
	if err := json.NewEncoder(process.stdin).Encode(request); err != nil {
		process.t.Fatalf("send harness request: %v\nstderr:\n%s", err, process.stderr.String())
	}
}

func (process *gatewayProcess) receive(response *harnessResponse) error {
	line, err := process.stdout.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("read harness response: %w", err)
	}
	if err := json.Unmarshal(line, response); err != nil {
		return fmt.Errorf("decode harness response %q: %w", line, err)
	}
	return nil
}

func (process *gatewayProcess) do(request any) harnessResponse {
	process.t.Helper()
	process.send(request)
	var response harnessResponse
	if err := process.receive(&response); err != nil {
		process.t.Fatalf("%v\nstderr:\n%s", err, process.stderr.String())
	}
	return response
}

// --- assertions ---

type expectedState struct {
	ready           bool
	synced          bool
	healthy         bool
	appliedEpoch    uint64
	appliedRevision uint64
	routes          int
}

func requireState(t *testing.T, got harnessResponse, want expectedState) {
	t.Helper()
	if !got.OK {
		t.Fatalf("harness command failed: %s", got.Error)
	}
	if got.Ready != want.ready {
		t.Fatalf("snapshot ready = %v, want %v", got.Ready, want.ready)
	}
	if got.Synced != want.synced {
		t.Fatalf("control-sync health = %v, want %v (error %q)", got.Synced, want.synced, got.Error)
	}
	if got.Healthy != want.healthy {
		t.Fatalf("ControlSyncHealthy = %v, want %v (error %q)", got.Healthy, want.healthy, got.Error)
	}
	if got.AppliedEpoch != want.appliedEpoch {
		t.Fatalf("applied epoch = %d, want %d", got.AppliedEpoch, want.appliedEpoch)
	}
	if got.AppliedRevision != want.appliedRevision {
		t.Fatalf("applied revision = %d, want %d", got.AppliedRevision, want.appliedRevision)
	}
	if got.Routes != want.routes {
		t.Fatalf("gateway route table length = %d, want %d", got.Routes, want.routes)
	}
}

// --- the tests ---

// TestCrossModuleSyncAckPresenceAndEpochRecovery drives the real gateway
// client/loop against the real control listener over real mTLS and proves:
// the accepted-ack watermark advances for the current epoch and drives health
// (snapshot and delta paths); presence renewal survives past the 45 s lease;
// a control restart refuses a stale-epoch ack so the gateway records nothing
// and withdraws health, then recovers on the new epoch; and mTLS rejects a
// wrong CA/SAN client.
func TestCrossModuleSyncAckPresenceAndEpochRecovery(t *testing.T) {
	certs := newSyncCertificates(t)
	certFiles := writeCertificateFiles(t, certs)
	app := newPublisherTestApp(t)
	apiKeyID, agentRecordID := createPublisherAgent(t, app, "crossmodule", gatewayNamespace, agentRelayPort, int(agentGeneration))
	sessionOne := createPublisherSession(t, app, apiKeyID, "XMOD001", "photos."+gatewayNamespace+".example.com")
	var sessionTwo string
	ensureSessionTwo := func() string {
		if sessionTwo == "" {
			sessionTwo = createPublisherSession(t, app, apiKeyID, "XMOD002", "videos."+gatewayNamespace+".example.com")
		}
		return sessionTwo
	}

	clock := newFakeControlClock()
	instanceOne := newControlInstance(t, app, clock, certs, epochOne, revisionSeed)
	if err := instanceOne.publisher.PublishAdd(sessionOne); err != nil {
		t.Fatalf("publish session one: %v", err)
	}
	if got := instanceOne.publisher.CurrentRevision(); got != snapshotRevision {
		t.Fatalf("control revision after first add = %d, want %d", got, snapshotRevision)
	}

	handler := newSwapHandler(instanceOne.server)
	listener := startControlListener(t, certs, handler, instanceOne.server.TLSConfig())

	binary := buildGatewayHarness(t)
	gateway := startGatewayProcess(t, binary, gatewayEnv(t, listener.URL, certFiles, gatewayBootID, "10", true))

	t.Run("happy path: snapshot apply and accepted ack advance control's watermark", func(t *testing.T) {
		if instanceOne.publisher.Healthy() {
			t.Fatal("control reports healthy before any gateway acknowledgement")
		}
		state := gateway.do(map[string]any{"cmd": "reconcile"})
		requireState(t, state, expectedState{
			ready: true, synced: true, healthy: true,
			appliedEpoch: epochOne, appliedRevision: snapshotRevision, routes: 1,
		})
		if !instanceOne.publisher.Healthy() {
			t.Fatal("control publisher not healthy after the gateway's accepted acknowledgement")
		}
		if got := instanceOne.publisher.AcknowledgedRevision(); got != snapshotRevision {
			t.Fatalf("control acknowledged revision = %d, want %d", got, snapshotRevision)
		}
	})

	t.Run("delta apply advances the watermark and the gateway route table", func(t *testing.T) {
		if err := instanceOne.publisher.PublishAdd(ensureSessionTwo()); err != nil {
			t.Fatalf("publish session two: %v", err)
		}
		if got := instanceOne.publisher.CurrentRevision(); got != deltaRevision {
			t.Fatalf("control revision after second add = %d, want %d", got, deltaRevision)
		}
		state := gateway.do(map[string]any{"cmd": "reconcile"})
		requireState(t, state, expectedState{
			ready: true, synced: true, healthy: true,
			appliedEpoch: epochOne, appliedRevision: deltaRevision, routes: 2,
		})
		if got := instanceOne.publisher.AcknowledgedRevision(); got != deltaRevision {
			t.Fatalf("control acknowledged revision after the delta = %d, want %d", got, deltaRevision)
		}
	})

	t.Run("presence republish keeps control's lease alive past 45s", func(t *testing.T) {
		available := func() bool {
			return instanceOne.view.Available(agentRecordID, agentRelayPort, agentGeneration,
				instanceOne.publisher.AcknowledgedRevision(), clock.Now())
		}
		gateway.do(map[string]any{"cmd": "set_presence", "entries": []map[string]any{{
			"agent_record_id": agentRecordID, "relay_port": agentRelayPort,
			"generation": agentGeneration, "lease_offset_ms": presenceLeaseMS,
		}}})
		gateway.do(map[string]any{"cmd": "publish_presence"})
		if !available() {
			t.Fatal("presence not available after the initial republish")
		}
		publishesBefore := gateway.do(map[string]any{"cmd": "state"}).PublishCount

		// Past the original 45-second lease, with no republish: expired.
		clock.Advance(46 * time.Second)
		if available() {
			t.Fatal("presence still available after the original lease expired")
		}

		// Advance the gateway's presence clock and let the RUNNING publisher
		// loop republish the renewed lease (no explicit publish call here).
		gateway.do(map[string]any{"cmd": "advance_clock", "ms": 46000})
		deadline := time.Now().Add(5 * time.Second)
		for !available() {
			if time.Now().After(deadline) {
				t.Fatalf("presence was not renewed by the running republish loop within the deadline")
			}
			time.Sleep(5 * time.Millisecond)
		}
		if got := gateway.do(map[string]any{"cmd": "state"}).PublishCount; got <= publishesBefore {
			t.Fatalf("presence publish count = %d, want more than %d (the running loop must republish)", got, publishesBefore)
		}
	})

	t.Run("mtls rejects a client with the wrong CA or SAN", func(t *testing.T) {
		foreign := newRawHTTPClient(t, certs.serverCAPEM, certs.foreignCertPEM, certs.foreignKeyPEM, controlIdentity)
		if _, err := foreign.Post(listener.URL+relayctl.PathStatus, "application/json", strings.NewReader(`{}`)); err == nil {
			t.Fatal("foreign client CA completed the mTLS handshake")
		}
		wrongSAN := newRawHTTPClient(t, certs.serverCAPEM, certs.wrongSANCertPEM, certs.wrongSANKeyPEM, controlIdentity)
		response, err := wrongSAN.Post(listener.URL+relayctl.PathStatus, "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("wrong-SAN POST: %v", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("wrong-SAN status = %d, want 403", response.StatusCode)
		}
	})

	t.Run("control restart refuses a stale-epoch ack and the gateway recovers", func(t *testing.T) {
		blocked, release := handler.armAckGate()
		gateway.send(map[string]any{"cmd": "reconcile"})
		select {
		case <-blocked:
		case <-time.After(10 * time.Second):
			t.Fatalf("gateway never reached the parked /status request\nstderr:\n%s", gateway.stderr.String())
		}

		// Control "restarts": a new epoch, a new publisher/view, and a new
		// relayctl.Server (empty acknowledgement dedup cache). The gateway's
		// in-flight ack for the old epoch must be refused.
		instanceTwo := newControlInstance(t, app, clock, certs, epochTwo, deltaRevision)
		if err := instanceTwo.publisher.PublishAdd(ensureSessionTwo()); err != nil {
			t.Fatalf("publish on the restarted control: %v", err)
		}
		if got := instanceTwo.publisher.CurrentRevision(); got != deltaRevision+1 {
			t.Fatalf("restarted control revision = %d, want %d", got, deltaRevision+1)
		}
		handler.set(instanceTwo.server)
		close(release)

		var refused harnessResponse
		if err := gateway.receive(&refused); err != nil {
			t.Fatalf("read refused reconcile: %v", err)
		}
		if refused.OK {
			t.Fatal("reconcile against the restarted control reported success, want a refusal")
		}
		if !strings.Contains(refused.Error, relayctl.AckReasonForeignEpoch) {
			t.Fatalf("refusal error %q does not carry the bounded %q reason", refused.Error, relayctl.AckReasonForeignEpoch)
		}
		if refused.Synced || refused.Healthy {
			t.Fatalf("gateway still claims sync health after a refused ack (synced=%v healthy=%v)", refused.Synced, refused.Healthy)
		}
		if refused.AppliedEpoch != epochOne || refused.AppliedRevision != deltaRevision {
			t.Fatalf("applied state moved during the refusal: epoch %d revision %d", refused.AppliedEpoch, refused.AppliedRevision)
		}
		if instanceTwo.publisher.Healthy() || instanceTwo.publisher.AcknowledgedRevision() != 0 {
			t.Fatal("restarted control recorded an acknowledgement it refused")
		}

		// Wire-level proof: the SAME stale-epoch ack answers acknowledged:false
		// with the bounded foreign_epoch reason (not a blanket success).
		raw := newRawHTTPClient(t, certs.serverCAPEM, certs.gatewayCertPEM, certs.gatewayKeyPEM, controlIdentity)
		wire := postStatusAckRaw(t, raw, listener.URL+relayctl.PathStatus, relayctl.StatusAck{
			Version:             relayctl.ProtocolVersion,
			GatewayBootID:       gatewayBootID,
			ControlEpoch:        epochOne,
			LastAppliedRevision: deltaRevision,
		})
		if wire.Acknowledged {
			t.Fatal("stale-epoch ack was acknowledged on the wire")
		}
		if wire.Reason != relayctl.AckReasonForeignEpoch {
			t.Fatalf("wire refusal reason = %q, want %q", wire.Reason, relayctl.AckReasonForeignEpoch)
		}

		// Recovery: the next reconcile observes the new epoch and re-acks it.
		state := gateway.do(map[string]any{"cmd": "reconcile"})
		requireState(t, state, expectedState{
			ready: true, synced: true, healthy: true,
			appliedEpoch: epochTwo, appliedRevision: deltaRevision + 1, routes: 2,
		})
		if !instanceTwo.publisher.Healthy() {
			t.Fatal("restarted control not healthy after the gateway re-acked the new epoch")
		}
		if got := instanceTwo.publisher.AcknowledgedRevision(); got != deltaRevision+1 {
			t.Fatalf("restarted control acknowledged revision = %d, want %d", got, deltaRevision+1)
		}
	})
}

// TestCrossModulePresenceBootAdoption drives the real presence client against
// the real control presence view and proves an EMPTY presence snapshot that
// carries the gateway boot identity is adopted: before adoption the boot's
// event batch is discarded, after the empty snapshot the same batch applies
// and the tunnel becomes available.
func TestCrossModulePresenceBootAdoption(t *testing.T) {
	certs := newSyncCertificates(t)
	certFiles := writeCertificateFiles(t, certs)
	app := newPublisherTestApp(t)
	apiKeyID, agentRecordID := createPublisherAgent(t, app, "crossmodule-boot", gatewayNamespace, agentRelayPort, int(agentGeneration))
	sessionOne := createPublisherSession(t, app, apiKeyID, "XMOD003", "boots."+gatewayNamespace+".example.com")

	clock := newFakeControlClock()
	instance := newControlInstance(t, app, clock, certs, epochOne, revisionSeed)
	if err := instance.publisher.PublishAdd(sessionOne); err != nil {
		t.Fatalf("publish session: %v", err)
	}
	handler := newSwapHandler(instance.server)
	listener := startControlListener(t, certs, handler, instance.server.TLSConfig())

	binary := buildGatewayHarness(t)
	// Presence autostart is off so the test controls when the boot announces
	// itself; the sync loop is still driven explicitly.
	gateway := startGatewayProcess(t, binary, gatewayEnv(t, listener.URL, certFiles, freshGatewayBoot, "0", false))

	requireState(t, gateway.do(map[string]any{"cmd": "reconcile"}), expectedState{
		ready: true, synced: true, healthy: true,
		appliedEpoch: epochOne, appliedRevision: snapshotRevision, routes: 1,
	})

	available := func() bool {
		return instance.view.Available(agentRecordID, agentRelayPort, agentGeneration,
			instance.publisher.AcknowledgedRevision(), clock.Now())
	}
	onlineEvent := map[string]any{
		"agent_record_id": agentRecordID, "relay_port": agentRelayPort,
		"generation": agentGeneration, "state": "online", "lease_offset_ms": presenceLeaseMS,
	}

	// Before the boot has announced itself, an ordered event batch is
	// discarded (the view has no boot to attribute it to).
	pre := gateway.do(map[string]any{"cmd": "publish_events", "revision": 1, "events": []map[string]any{onlineEvent}})
	if !pre.OK {
		t.Fatalf("pre-adoption event publish failed: %s", pre.Error)
	}
	if available() {
		t.Fatal("presence applied before the boot was adopted")
	}

	// The EMPTY boot snapshot carrying the gateway boot id is the adoption
	// primitive.
	gateway.do(map[string]any{"cmd": "set_presence", "entries": []map[string]any{}})
	if resp := gateway.do(map[string]any{"cmd": "publish_presence"}); !resp.OK {
		t.Fatalf("empty boot snapshot publish failed: %s", resp.Error)
	}

	// The same event batch now applies and the tunnel becomes available.
	post := gateway.do(map[string]any{"cmd": "publish_events", "revision": 1, "events": []map[string]any{onlineEvent}})
	if !post.OK {
		t.Fatalf("post-adoption event publish failed: %s", post.Error)
	}
	if !available() {
		t.Fatal("presence was not adopted after the empty boot snapshot")
	}
}
