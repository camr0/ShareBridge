package daemon

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"sharebridge/agent/internal/cert"
	"sharebridge/agent/internal/config"
	"sharebridge/agent/internal/direct"
	"sharebridge/agent/internal/signaling"
)

const (
	testDirectNS   = "v7q4km2x9pz6dn3w"
	testDirectBase = "sharebridgeusercontent.com"
)

func testOriginFor(label string) string {
	return label + "." + testDirectNS + "." + testDirectBase
}

func TestRegistrationGatedOnReadiness(t *testing.T) {
	d := &Daemon{direct: &directState{ready: false}}
	if d.canRegisterDirect() {
		t.Fatalf("must gate before readiness")
	}
	d.direct.ready = true
	if !d.canRegisterDirect() {
		t.Fatalf("must allow after readiness")
	}
}

func TestBinderBoundOnOrigin(t *testing.T) {
	d := &Daemon{direct: &directState{
		binder: direct.NewBinder(testDirectNS, testDirectBase),
		origin: map[string]string{},
	}}
	const code = "SHARE123"
	origin := testOriginFor("sbabc123")
	d.bindOrigin(code, origin)

	bd, err := d.direct.binder.AdmitSNI(origin)
	if err != nil {
		t.Fatalf("Allow was not recorded: %v", err)
	}
	if bd.ShareCode != code {
		t.Fatalf("binding share code = %q, want %q", bd.ShareCode, code)
	}
	if got := d.direct.origin[code]; got != origin {
		t.Fatalf("origin[%q] = %q, want %q", code, got, origin)
	}
}

func TestBinderRevokedOnDelete(t *testing.T) {
	d := &Daemon{direct: &directState{
		binder: direct.NewBinder(testDirectNS, testDirectBase),
		origin: map[string]string{},
	}}
	const code = "SHARE123"
	origin := testOriginFor("sbabc123")
	d.bindOrigin(code, origin)

	d.revokeOrigin(code)

	if _, err := d.direct.binder.AdmitSNI(origin); err == nil {
		t.Fatalf("Revoke was not recorded: origin still admitted")
	}
	if _, ok := d.direct.origin[code]; ok {
		t.Fatalf("origin[%q] should be cleared after revoke", code)
	}
}

// TestOnSignalingDisconnectResetsReadinessAndGate verifies the disconnect epoch
// reset: ready flips to false and the gate forgets sequence numbers/nonces so a
// low-sequence signal from the new connection is admissible again.
func TestOnSignalingDisconnectResetsReadinessAndGate(t *testing.T) {
	gate := direct.NewSignalGate("test-agent-id", func(shareID string, kind direct.RouteKind) bool {
		return true
	})
	d := &Daemon{direct: &directState{ready: true, gate: gate}}

	// Seed a high sequence number so the gate's watermark is observably cleared.
	seed := direct.OpenSignal{
		Version:   1,
		AgentID:   "test-agent-id",
		ShareID:   "SHARE123",
		RouteKind: direct.RouteDirect,
		Nonce:     "nonce-seed",
		Seq:       10,
		ExpiresAt: time.Now().Add(time.Minute),
		Lease:     30 * time.Second,
	}
	if err := gate.Admit(seed); err != nil {
		t.Fatalf("seed admit: %v", err)
	}

	d.onSignalingDisconnect()

	if d.direct.ready {
		t.Fatalf("ready should be false after disconnect")
	}

	// A lower sequence number must now be admissible (reset cleared the
	// highest-seq watermark); without reset it would be rejected as a replay.
	replay := seed
	replay.Nonce = "nonce-replay"
	replay.Seq = 1
	if err := gate.Admit(replay); err != nil {
		t.Fatalf("gate was not reset: low-seq signal rejected: %v", err)
	}
}

// TestRenewalSchedulerGeneratesAndSubmitsCSR drives renewCertIfNeeded directly
// (the tick function the hourly scheduler invokes) and asserts that a cert
// within the renewal window triggers GenerateCSR + SubmitCSR.
func TestRenewalSchedulerGeneratesAndSubmitsCSR(t *testing.T) {
	const ns = "v7q4km2x9pz6dn3w"
	const base = "sharebridgeusercontent.com"
	dir := t.TempDir()

	// Mint a self-signed root first so the manager can be constructed with a
	// trust pool that will validate the issued chain (roots are set at
	// construction only).
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rootTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-root"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)

	cm := cert.NewManager(dir, base, roots)
	if err := cm.SetNamespace(ns); err != nil {
		t.Fatal(err)
	}
	csrPEM, err := cm.GenerateCSR()
	if err != nil {
		t.Fatal(err)
	}

	// Recover the manager's public key from the CSR (the private key never
	// leaves the manager) so a short-lived chain can be minted for it.
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		t.Fatalf("bad CSR PEM block: %+v", block)
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatalf("parse CSR: %v", err)
	}
	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("CSR public key is %T", csr.PublicKey)
	}

	sans := []string{
		fmt.Sprintf("*.%s.%s", ns, base),
		fmt.Sprintf("*.relay.%s.%s", ns, base),
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: sans[0]},
		DNSNames:     sans,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour), // short-lived → NeedsRenewal
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, root, pub, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	chain := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})...)

	if err := cm.Install(chain); err != nil {
		t.Fatal(err)
	}
	if !cm.Installed() {
		t.Fatal("Installed() = false after successful Install")
	}
	if !cm.NeedsRenewal() {
		t.Fatal("NeedsRenewal() = false for a short-lived cert")
	}

	cfg := &config.Config{SignalingURL: "ws://localhost:8080", APIKey: "test-key"}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	sig := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())
	d, err := NewWithSignaling(cfgMgr, st, sig)
	if err != nil {
		t.Fatal(err)
	}
	d.direct = &directState{cert: cm}

	d.renewCertIfNeeded(context.Background())

	if !sig.sentContains("\"type\":\"csr_submit\"") {
		t.Fatalf("expected csr_submit to be sent, got %#v", sig.messagesSnapshot())
	}
	// Assert the submission actually carried a generated CSR body (i.e.
	// GenerateCSR ran and its output was submitted).
	found := false
	for _, msg := range sig.messagesSnapshot() {
		if msg["type"] != "csr_submit" {
			continue
		}
		if s, ok := msg["csr_pem"].(string); ok && strings.Contains(s, "BEGIN CERTIFICATE REQUEST") {
			found = true
		}
	}
	if !found {
		t.Fatalf("csr_submit did not carry a CSR body: %#v", sig.messagesSnapshot())
	}
}

// fakeDirectMapper implements direct.PortMapper without any network, so the
// daemon's open-signal handler can drive a real OnDemandPort in-process.
type fakeDirectMapper struct {
	ip    string
	added []int
}

func (f *fakeDirectMapper) AddPortMapping(ext, internal int, desc string, lease int) (int, error) {
	f.added = append(f.added, ext)
	return ext, nil
}

func (f *fakeDirectMapper) DeletePortMapping(ext int) error { return nil }

func (f *fakeDirectMapper) ExternalIP() (string, error) { return f.ip, nil }

func (f *fakeDirectMapper) InternalIP() string { return "192.168.1.20" }

func (f *fakeDirectMapper) ListPortMappings() ([]direct.PortMapping, error) { return nil, nil }

// newOpenSignalTestDaemon builds a Daemon whose direct state is ready and fully
// wired (real SignalGate + real OnDemandPort over a fake mapper) but whose
// signaling client is the in-memory mock, so handleOpenSignal can be exercised
// end-to-end without a router or WebSocket.
func newOpenSignalTestDaemon(t *testing.T, authz func(string, direct.RouteKind) bool) (*Daemon, *fakeDirectMapper, *mockSignalingClient) {
	t.Helper()
	cfg := &config.Config{SignalingURL: "ws://localhost:8080", APIKey: "test-key"}
	st := newMockStore()
	sig := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())
	mapper := &fakeDirectMapper{ip: "203.0.113.7"}
	port := direct.NewOnDemandPortOwned(mapper, 443, 8443, time.Minute, "test", "192.168.1.20")
	d := &Daemon{
		store:     st,
		signaling: sig,
		direct: &directState{
			ready:  true,
			gate:   direct.NewSignalGate(st.GetAgentID(), authz),
			port:   port,
			mapper: mapper,
		},
	}
	return d, mapper, sig
}

func openSignalMessage() signaling.Message {
	return signaling.Message{
		Type:         "open_signal",
		AgentID:      "test-agent-id",
		Version:      1,
		ShareID:      "SHARE123",
		Nonce:        "nonce-123",
		Seq:          1,
		ExpiresAt:    time.Now().Add(time.Minute).Format(time.RFC3339),
		LeaseSeconds: 30,
	}
}

// TestHandleOpenSignalAdmitsOpensAcks drives the full happy path: an admitted
// open_signal opens the on-demand port (AddPortMapping) and is acked with the
// echoed nonce + seq, the granted port, a fresh public IP, and status ok.
func TestHandleOpenSignalAdmitsOpensAcks(t *testing.T) {
	d, mapper, sig := newOpenSignalTestDaemon(t, func(string, direct.RouteKind) bool { return true })

	d.handleOpenSignal(openSignalMessage())

	if len(mapper.added) == 0 {
		t.Fatalf("OpenFor was not called: no AddPortMapping recorded")
	}
	if !sig.hasSentMessage("open_ack", map[string]any{
		"share_id":         "SHARE123",
		"nonce":            "nonce-123",
		"seq":              uint64(1),
		"granted_port":     443,
		"public_ip":        "203.0.113.7",
		"was_already_open": false,
		"status":           "ok",
	}) {
		t.Fatalf("expected ok open_ack, got %#v", sig.messagesSnapshot())
	}
}

// TestHandleOpenSignalRejectsUnauthorized asserts a gate rejection yields an
// open_ack with status error (and no port is opened).
func TestHandleOpenSignalRejectsUnauthorized(t *testing.T) {
	d, mapper, sig := newOpenSignalTestDaemon(t, func(string, direct.RouteKind) bool { return false })

	d.handleOpenSignal(openSignalMessage())

	if len(mapper.added) != 0 {
		t.Fatalf("OpenFor must not run on rejection, but AddPortMapping recorded %v", mapper.added)
	}
	if !sig.hasSentMessage("open_ack", map[string]any{
		"status": "error",
		"error":  "rejected",
	}) {
		t.Fatalf("expected rejected open_ack, got %#v", sig.messagesSnapshot())
	}
}

// reportedEndpoint is one (ip, port, status) triple delivered to a Reporter's
// send func.
type reportedEndpoint struct {
	ip     string
	port   int
	status string
}

// endpointRecorder captures the Reporter send func invocations so the
// transition → ReportEndpoint path can be asserted.
type endpointRecorder struct {
	mu     sync.Mutex
	events []reportedEndpoint
}

func (r *endpointRecorder) record(ip string, port int, status string) {
	r.mu.Lock()
	r.events = append(r.events, reportedEndpoint{ip: ip, port: port, status: status})
	r.mu.Unlock()
}

func (r *endpointRecorder) snapshot() []reportedEndpoint {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]reportedEndpoint(nil), r.events...)
}

// waitForEndpoints polls until n reports have been delivered (the Reporter
// drains its queue on a background goroutine) or fails after a deadline.
func waitForEndpoints(t *testing.T, r *endpointRecorder, n int) []reportedEndpoint {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := r.snapshot(); len(got) >= n {
			return got
		}
		time.Sleep(2 * time.Millisecond)
	}
	got := r.snapshot()
	t.Fatalf("timed out waiting for %d endpoint reports, got %d: %#v", n, len(got), got)
	return nil
}

// TestHandleOpenSignalReportsFreshIPOnTransition verifies the open/close
// transitions drive ReportEndpoint with the fresh public IP and correct port
// (open → granted port, close → 0). The reporter is seeded with a STALE IP to
// prove SetIP happens before OpenFor (the open transition snapshots the
// reporter's IP synchronously on the state loop).
func TestHandleOpenSignalReportsFreshIPOnTransition(t *testing.T) {
	cfg := &config.Config{SignalingURL: "ws://localhost:8080", APIKey: "test-key"}
	st := newMockStore()
	sig := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())

	const (
		staleIP = "203.0.113.7" // enrollment-time IP the reporter currently holds
		freshIP = "203.0.113.9" // IP the mapper now returns
	)
	mapper := &fakeDirectMapper{ip: freshIP}
	port := direct.NewOnDemandPortOwned(mapper, 443, 8443, time.Minute, "test", "192.168.1.20")

	rec := &endpointRecorder{}
	reporter := direct.NewReporter(rec.record)
	reporter.SetIP(staleIP)
	port.SetTransitionCallback(reporter.OnTransition)

	d := &Daemon{
		store:     st,
		signaling: sig,
		direct: &directState{
			ready:    true,
			gate:     direct.NewSignalGate(st.GetAgentID(), func(string, direct.RouteKind) bool { return true }),
			port:     port,
			mapper:   mapper,
			reporter: reporter,
		},
	}

	// admit → OpenFor fires the open transition.
	d.handleOpenSignal(openSignalMessage())

	// close fires the closed transition.
	if err := port.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reports := waitForEndpoints(t, rec, 2)

	if reports[0].ip != freshIP || reports[0].port != 443 || reports[0].status != "" {
		t.Fatalf("open report = %#v, want {ip:%s, port:443, status:\"\"}", reports[0], freshIP)
	}
	if reports[1].ip != freshIP || reports[1].port != 0 || reports[1].status != "" {
		t.Fatalf("close report = %#v, want {ip:%s, port:0, status:\"\"}", reports[1], freshIP)
	}
}

// TestLearnAndReportPublicIPReportsClosedEndpoint asserts the initial
// report_endpoint {ip, 0} is sent once the public IP is learned, unblocking
// the control's DDNS → enrollment_ready step.
func TestLearnAndReportPublicIPReportsClosedEndpoint(t *testing.T) {
	cfg := &config.Config{SignalingURL: "ws://localhost:8080", APIKey: "test-key"}
	st := newMockStore()
	sig := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())

	d := &Daemon{
		store:     st,
		signaling: sig,
		direct:    &directState{mapper: &fakeDirectMapper{ip: "203.0.113.7"}},
	}

	d.learnAndReportPublicIP()

	if !sig.hasSentMessage("report_endpoint", map[string]any{
		"ip":   "203.0.113.7",
		"port": 0,
	}) {
		t.Fatalf("expected initial report_endpoint {ip,0}, got %#v", sig.messagesSnapshot())
	}
}

// TestStartDirectServerGuardedAndStopsOnDisconnect asserts the double-start
// guard holds and that onSignalingDisconnect cancels the server context and
// clears the started flag.
func TestStartDirectServerGuardedAndStopsOnDisconnect(t *testing.T) {
	gate := direct.NewSignalGate("test-agent-id", func(string, direct.RouteKind) bool { return true })
	ds := &directState{
		server:     direct.NewDirectServer(testDirectNS, testDirectBase, nil, nil, gate, 1<<20),
		listenAddr: "127.0.0.1:0",
	}
	d := &Daemon{direct: ds}

	d.startDirectServer()
	if !ds.started {
		t.Fatalf("startDirectServer should set started=true")
	}
	if ds.cancel == nil {
		t.Fatalf("startDirectServer should install a cancel func")
	}

	// Double-start guard: a second call must be a no-op (started stays true and
	// no replacement cancel is installed).
	d.startDirectServer()
	if !ds.started {
		t.Fatalf("double start must leave started=true")
	}

	d.onSignalingDisconnect()
	if ds.started {
		t.Fatalf("onSignalingDisconnect should reset started=false")
	}
	if ds.cancel != nil {
		t.Fatalf("onSignalingDisconnect should clear cancel")
	}
}

// TestDirectServerSharesDaemonBinder proves the daemon's bindOrigin and the
// DirectServer consult the SAME binder. This is the split-Binder regression
// (C1): before the fix the server used its own private binder and rejected
// every direct handshake as an unknown origin.
func TestDirectServerSharesDaemonBinder(t *testing.T) {
	ds := &directState{
		namespace:  testDirectNS,
		baseDomain: testDirectBase,
		origin:     map[string]string{},
		gate:       direct.NewSignalGate("test-agent-id", func(string, direct.RouteKind) bool { return true }),
	}
	d := &Daemon{direct: ds}
	d.syncDirectServe()

	if ds.binder != ds.server.Binder() {
		t.Fatalf("daemon and server must share one binder instance")
	}

	const code = "SHARE123"
	origin := testOriginFor("sbabc123")
	d.bindOrigin(code, origin)

	// The server's binder (the one consulted for TLS admission) must admit the
	// origin the daemon just bound.
	if _, err := ds.server.Binder().AdmitSNI(origin); err != nil {
		t.Fatalf("server binder did not admit daemon-bound origin: %v", err)
	}
}

// TestDirectRegistrationWaitsForEnrollmentReady verifies waitForDirectReady
// blocks until enrollment_ready is received (the I1 startup gate).
func TestDirectRegistrationWaitsForEnrollmentReady(t *testing.T) {
	d := &Daemon{direct: &directState{}}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ready := make(chan error, 1)
	go func() { ready <- d.waitForDirectReady(ctx) }()

	// Not ready yet: must still block.
	select {
	case err := <-ready:
		t.Fatalf("waitForDirectReady returned before enrollment_ready: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	d.handleEnrollmentReady(signaling.Message{})

	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("waitForDirectReady: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitForDirectReady did not unblock after enrollment_ready")
	}
}

// TestDirectRegistrationBlockedAfterDisconnect verifies a disconnect clears
// readiness, so a subsequent wait blocks until the NEXT enrollment_ready.
func TestDirectRegistrationBlockedAfterDisconnect(t *testing.T) {
	d := &Daemon{direct: &directState{ready: true}}

	d.onSignalingDisconnect() // ready flips to false

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ready := make(chan error, 1)
	go func() { ready <- d.waitForDirectReady(ctx) }()

	select {
	case err := <-ready:
		t.Fatalf("waitForDirectReady returned after disconnect: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	d.handleEnrollmentReady(signaling.Message{})
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("waitForDirectReady: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitForDirectReady did not unblock after re-enrollment")
	}
}

// TestStartDirectServerResetsOnBindFailureAndRetries drives a REAL bind
// failure (occupied port) and asserts the started guard resets, then a retry
// after the port is freed actually binds (I6).
func TestStartDirectServerResetsOnBindFailureAndRetries(t *testing.T) {
	gate := direct.NewSignalGate("test-agent-id", func(string, direct.RouteKind) bool { return true })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()

	ds := &directState{
		server:     direct.NewDirectServer(testDirectNS, testDirectBase, nil, nil, gate, 1<<20),
		listenAddr: addr,
	}
	d := &Daemon{direct: ds}

	d.startDirectServer()

	// Wait for the bind failure to reset started/cancel.
	waitForCond(t, func() bool {
		ds.mu.Lock()
		defer ds.mu.Unlock()
		return !ds.started && ds.cancel == nil
	})

	// Free the port and retry; the server must actually bind.
	ln.Close()
	d.startDirectServer()

	// Prove a real re-bind by dialing the listener.
	deadline := time.Now().Add(2 * time.Second)
	dialed := false
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			dialed = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !dialed {
		t.Fatalf("retry did not bind the freed port %s", addr)
	}

	ds.mu.Lock()
	started, cancelSet := ds.started, ds.cancel != nil
	ds.mu.Unlock()
	if !started || !cancelSet {
		t.Fatalf("retry must leave server started (started=%v cancel=%v)", started, cancelSet)
	}
	d.onSignalingDisconnect()
}

// waitForCond polls cond until true or fails after a deadline.
func waitForCond(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}
