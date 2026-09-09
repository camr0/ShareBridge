package daemon

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"sharebridge/agent/internal/cert"
	"sharebridge/agent/internal/config"
	"sharebridge/agent/internal/direct"
	"sharebridge/agent/internal/immich"
	"sharebridge/agent/internal/signaling"
	"sharebridge/agent/internal/tunnel"
)

const (
	testDirectNS   = "v7q4km2x9pz6dn3w"
	testDirectBase = "sharebridgeusercontent.com"
)

func testOriginFor(label string) string {
	return label + "." + testDirectNS + "." + testDirectBase
}

// newBaselineCertFixture builds an UNINSTALLED cert.Manager for testDirectNS
// plus a minted chain (both §6 SANs, signed by a fresh test root). Feeding the
// returned chain through cert_issue drives the manager's real install path.
func newBaselineCertFixture(t *testing.T, dir string) (*cert.Manager, string) {
	t.Helper()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rootTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "baseline-test-root"},
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
		t.Fatalf("parse root: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)

	cm := cert.NewManager(dir, testDirectBase, roots)
	if err := cm.SetNamespace(testDirectNS); err != nil {
		t.Fatal(err)
	}
	csrPEM, err := cm.GenerateCSR()
	if err != nil {
		t.Fatal(err)
	}
	csrBlock, _ := pem.Decode(csrPEM)
	if csrBlock == nil || csrBlock.Type != "CERTIFICATE REQUEST" {
		t.Fatalf("bad CSR PEM block: %+v", csrBlock)
	}
	csr, err := x509.ParseCertificateRequest(csrBlock.Bytes)
	if err != nil {
		t.Fatalf("parse CSR: %v", err)
	}
	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("CSR public key is %T", csr.PublicKey)
	}
	sans := []string{
		fmt.Sprintf("*.%s.%s", testDirectNS, testDirectBase),
		fmt.Sprintf("*.relay.%s.%s", testDirectNS, testDirectBase),
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: sans[0]},
		DNSNames:     sans,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, root, pub, rootKey)
	if err != nil {
		t.Fatalf("create leaf: %v", err)
	}
	chainPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	chainPEM = append(chainPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})...)
	return cm, string(chainPEM)
}

// SendRelayClientState records one §11.1 relay_client_state telemetry send
// through the shared message log (same shape the real signaling client puts
// on the wire). Defining it on the mock (same package, separate file) lets the
// daemon's relayStateSender capability assertion observe telemetry in tests.
func (m *mockSignalingClient) SendRelayClientState(ctx context.Context, state signaling.RelayClientState) error {
	msg := map[string]any{
		"type":       "relay_client_state",
		"generation": state.Generation,
		"status":     string(state.Status),
	}
	if state.Reason != "" {
		msg["reason"] = state.Reason
	}
	return m.Send(ctx, msg)
}

// TestDirectDDNSFailureDoesNotBlockShareRegistration pins the §7.1 baseline
// decoupling at the daemon: an agent whose direct path is dead — no port
// mapper, so no public IP can be learned and direct DDNS can never succeed —
// still completes baseline enrollment from enrollment_ready alone and
// registers a supported share afterwards.
func TestDirectDDNSFailureDoesNotBlockShareRegistration(t *testing.T) {
	dir := t.TempDir()
	cm, chainPEM := newBaselineCertFixture(t, dir)

	cfg := &config.Config{
		SignalingURL:      "ws://localhost:8080",
		APIKey:            "test-key",
		DefaultRelayOnly:  false,
		ImmichURL:         "http://immich.lan:2283",
		ImmichAllowedHost: "immich.lan:2283",
		ImmichAPIKey:      "api",
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	sig := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())

	// Direct state WITHOUT mapper/port/reporter: the direct transport (and
	// with it the public-IP report that feeds control-side direct DDNS) can
	// never come up.
	ds := &directState{
		namespace:  testDirectNS,
		baseDomain: testDirectBase,
		cert:       cm,
		gate:       direct.NewSignalGate(st.GetAgentID(), func(string, direct.RouteKind) bool { return true }),
		origins:    map[string]originPair{},
		listenAddr: "127.0.0.1:0",
	}
	d := &Daemon{
		config:    cfg,
		configMgr: cfgMgr,
		store:     st,
		signaling: sig,
		resolver:  direct.NewResolverRegistry(),
		sessions:  make(map[string]*Session),
		direct:    ds,
	}
	d.syncDirectServe()

	// The agent installs the issued chain (cert_issue) and reports
	// tls_ready. Without a mapper it can NEVER follow up with a public-IP
	// report_endpoint.
	certIssue := signaling.Message{Type: "cert_issue", ChainPEM: chainPEM}
	d.handleSignalingMessage(certIssue)
	if !sig.hasSentMessage("tls_ready", nil) {
		t.Fatalf("expected tls_ready after cert_issue, got %#v", sig.messagesSnapshot())
	}
	if sig.hasSentMessage("report_endpoint", nil) {
		t.Fatalf("a no-mapper agent must never send report_endpoint, got %#v", sig.messagesSnapshot())
	}

	// Baseline enrollment completes control-side (TLS + relay DNS); the
	// daemon only ever sees the resulting enrollment_ready.
	d.handleSignalingMessage(signaling.Message{Type: "enrollment_ready"})

	if !d.canRegisterDirect() {
		t.Fatalf("baseline readiness must not depend on the direct mapper/DDNS path")
	}

	// Share registration proceeds end-to-end despite the dead direct path.
	d.newImmichPoller = func() (immichPoller, error) {
		return &fakeImmichPoller{shares: []immich.SharedLink{
			{Key: "IMMICHBASE", Type: "ALBUM"},
		}}, nil
	}
	session, err := d.registerImmichShare(context.Background(), immich.SharedLink{Key: "IMMICHBASE", Type: "ALBUM"}, 10, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("share registration must not be blocked by direct DDNS failure: %v", err)
	}
	if session == nil || session.Code != "IMMICHBASE" {
		t.Fatalf("unexpected session %+v", session)
	}
	if !sig.registeredCode("IMMICHBASE") {
		t.Fatalf("share was not registered with the control: %#v", sig.registeredSnapshot())
	}
	d.onSignalingDisconnect()
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
		binder:  direct.NewBinder(testDirectNS, testDirectBase),
		origins: map[string]originPair{},
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
	if got, ok := d.direct.origins[code]; !ok || got.directOrigin != origin {
		t.Fatalf("origins[%q] = %+v, want direct origin %q", code, got, origin)
	}
}

func TestBinderRevokedOnDelete(t *testing.T) {
	d := &Daemon{direct: &directState{
		binder:  direct.NewBinder(testDirectNS, testDirectBase),
		origins: map[string]originPair{},
	}}
	const code = "SHARE123"
	origin := testOriginFor("sbabc123")
	d.bindOrigin(code, origin)

	d.revokeOrigin(code)

	if _, err := d.direct.binder.AdmitSNI(origin); err == nil {
		t.Fatalf("Revoke was not recorded: origin still admitted")
	}
	if _, ok := d.direct.origins[code]; ok {
		t.Fatalf("origins[%q] should be cleared after revoke", code)
	}
}

// relayOriginForLabel derives the §6 relay origin for a test direct origin
// label: the same origin label under the .relay.<ns>.<base> namespace.
func relayOriginForLabel(label string) string {
	return label + ".relay." + testDirectNS + "." + testDirectBase
}

// TestShareRegisteredReturnsAndPersistsBothOrigins pins §6/§11.1/§13.1 on the
// registration flow: control returns both origins for a share (the mock
// registrar stands in for the share_registered response carrying origin +
// relay_origin), and the agent persists BOTH on the ONE session row — the
// direct origin unchanged under its existing key — and installs BOTH binder
// bindings (RouteDirect + RouteRelay) pointing at the same content session.
func TestShareRegisteredReturnsAndPersistsBothOrigins(t *testing.T) {
	dir := t.TempDir()
	cm, _ := newBaselineCertFixture(t, dir)

	cfg := &config.Config{
		SignalingURL:      "ws://localhost:8080",
		APIKey:            "test-key",
		DefaultRelayOnly:  false,
		ImmichURL:         "http://immich.lan:2283",
		ImmichAllowedHost: "immich.lan:2283",
		ImmichAPIKey:      "api",
	}
	st := newMockStore()
	sig := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())
	// share_registered returns the control-allocated DIRECT origin; the relay
	// origin is its §6 deterministic pair.
	directOrigin := testOriginFor("sbpair01")
	relayOrigin := relayOriginForLabel("sbpair01")
	sig.shareOrigin = func(code, shareURL string) string { return directOrigin }

	ds := &directState{
		namespace:  testDirectNS,
		baseDomain: testDirectBase,
		cert:       cm,
		ready:      true,
		gate:       direct.NewSignalGate(st.GetAgentID(), func(string, direct.RouteKind) bool { return true }),
		origins:    map[string]originPair{},
		listenAddr: "127.0.0.1:0",
	}
	d := &Daemon{
		config:    cfg,
		store:     st,
		signaling: sig,
		resolver:  direct.NewResolverRegistry(),
		sessions:  make(map[string]*Session),
		direct:    ds,
	}
	d.syncDirectServe()

	const code = "BOTHPAIR1"
	session, err := d.registerImmichShare(context.Background(), immich.SharedLink{Key: code, Type: "ALBUM"}, 10, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("registerImmichShare: %v", err)
	}
	if session == nil || session.Code != code {
		t.Fatalf("unexpected session %+v", session)
	}

	// Persisted: exactly ONE row carrying BOTH origins, direct unchanged.
	rows := st.ListSessions(false)
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 persisted session row (one row, both origins), got %d", len(rows))
	}
	entry := st.GetSession(code)
	if entry == nil {
		t.Fatalf("session %s not persisted", code)
	}
	if entry.Origin != directOrigin {
		t.Fatalf("persisted origin = %q, want %q (unchanged)", entry.Origin, directOrigin)
	}
	if entry.RelayOrigin != relayOrigin {
		t.Fatalf("persisted relay_origin = %q, want %q", entry.RelayOrigin, relayOrigin)
	}

	// Bound: both origins are admitted for the same content session.
	db, err := ds.binder.AdmitSNI(directOrigin)
	if err != nil {
		t.Fatalf("direct origin not bound after registration: %v", err)
	}
	rb, err := ds.binder.AdmitSNI(relayOrigin)
	if err != nil {
		t.Fatalf("relay origin not bound after registration: %v", err)
	}
	if db.RouteKind != direct.RouteDirect || rb.RouteKind != direct.RouteRelay {
		t.Fatalf("route kinds = %s/%s, want direct/relay", db.RouteKind, rb.RouteKind)
	}
	if db.ShareCode != code || rb.ShareCode != code {
		t.Fatalf("share codes = %q/%q, want both %q", db.ShareCode, rb.ShareCode, code)
	}
}

// TestBothOriginsResolveSameContentSession pins §6: the direct and relay
// bindings of one share both point at the SAME content session — the same
// share code in the Binder and exactly ONE resolver/snapshot manager for the
// code, with no parallel bookkeeping for the second route kind.
func TestBothOriginsResolveSameContentSession(t *testing.T) {
	d := &Daemon{
		config: &config.Config{
			ImmichURL:         "http://immich.lan:2283",
			ImmichAllowedHost: "immich.lan:2283",
			ImmichAPIKey:      "api",
		},
		direct: &directState{
			binder:  direct.NewBinder(testDirectNS, testDirectBase),
			origins: map[string]originPair{},
		},
		resolver: direct.NewResolverRegistry(),
		sessions: make(map[string]*Session),
	}

	const code = "SHARE123"
	directOrigin := testOriginFor("sbabc123")
	relayOrigin := relayOriginForLabel("sbabc123")
	d.bindOrigin(code, directOrigin)

	db, err := d.direct.binder.AdmitSNI(directOrigin)
	if err != nil {
		t.Fatalf("direct origin not bound: %v", err)
	}
	rb, err := d.direct.binder.AdmitSNI(relayOrigin)
	if err != nil {
		t.Fatalf("relay origin not bound: %v", err)
	}
	if db.ShareCode != code || rb.ShareCode != code {
		t.Fatalf("bindings resolve to %q/%q, want the same content session %q", db.ShareCode, rb.ShareCode, code)
	}
	if db.RouteKind != direct.RouteDirect || rb.RouteKind != direct.RouteRelay {
		t.Fatalf("route kinds = %s/%s, want direct/relay", db.RouteKind, rb.RouteKind)
	}

	// ONE resolver/snapshot per content session: hydrating the share creates
	// a single manager keyed by the code, and re-hydrating (the poller's
	// keep-fresh path) reuses it instead of bookkeeping a second one.
	client, err := d.newImmichClient(code)
	if err != nil {
		t.Fatalf("newImmichClient: %v", err)
	}
	session := &Session{Code: code, immich: client}
	d.hydrateContentSession(session)
	mgr := d.resolver.Get(code)
	if mgr == nil {
		t.Fatalf("no snapshot manager for the content session")
	}
	d.hydrateContentSession(session)
	if again := d.resolver.Get(code); again != mgr {
		t.Fatalf("hydration produced a second snapshot manager: parallel bookkeeping")
	}
}

// TestRevocationRemovesBothBindings pins §6: revocation removes BOTH origin
// bindings and the daemon's origin bookkeeping for the share, as one logical
// operation — neither route kind stays routable after revoke.
func TestRevocationRemovesBothBindings(t *testing.T) {
	d := &Daemon{direct: &directState{
		binder:  direct.NewBinder(testDirectNS, testDirectBase),
		origins: map[string]originPair{},
	}}
	const code = "SHARE123"
	directOrigin := testOriginFor("sbabc123")
	relayOrigin := relayOriginForLabel("sbabc123")
	d.bindOrigin(code, directOrigin)

	// Sanity: both are admitted before revocation.
	if _, err := d.direct.binder.AdmitSNI(directOrigin); err != nil {
		t.Fatalf("direct origin not admitted before revoke: %v", err)
	}
	if _, err := d.direct.binder.AdmitSNI(relayOrigin); err != nil {
		t.Fatalf("relay origin not admitted before revoke: %v", err)
	}

	d.revokeOrigin(code)

	if _, err := d.direct.binder.AdmitSNI(directOrigin); err == nil {
		t.Fatalf("direct origin still admitted after revoke")
	}
	if _, err := d.direct.binder.AdmitSNI(relayOrigin); err == nil {
		t.Fatalf("relay origin still admitted after revoke (pair not revoked)")
	}
	if _, ok := d.direct.origins[code]; ok {
		t.Fatalf("origin bookkeeping for %q should be cleared after revoke", code)
	}
}

// TestBinderRebuildReallowsBothRouteKinds pins §13.1: after a binder/listener
// rebuild (namespace known, serve state resynced) BOTH route kinds of every
// bound session are re-admitted into the fresh binder, and the rebuilt server
// still shares that one binder with the daemon.
func TestBinderRebuildReallowsBothRouteKinds(t *testing.T) {
	ds := &directState{
		namespace:  testDirectNS,
		baseDomain: testDirectBase,
		origins:    map[string]originPair{},
		gate:       direct.NewSignalGate("test-agent-id", func(string, direct.RouteKind) bool { return true }),
	}
	d := &Daemon{direct: ds}
	d.syncDirectServe()

	const code = "SHARE123"
	directOrigin := testOriginFor("sbabc123")
	relayOrigin := relayOriginForLabel("sbabc123")
	d.bindOrigin(code, directOrigin)

	// Force a rebuild of the binder/server (as a listener rebuild would).
	ds.mu.Lock()
	ds.binder = nil
	ds.mu.Unlock()
	d.syncDirectServe()

	if ds.server == nil || ds.server.Binder() != ds.binder {
		t.Fatalf("rebuilt server must share the daemon's fresh binder")
	}
	db, err := ds.binder.AdmitSNI(directOrigin)
	if err != nil {
		t.Fatalf("direct origin not re-allowed after binder rebuild: %v", err)
	}
	rb, err := ds.binder.AdmitSNI(relayOrigin)
	if err != nil {
		t.Fatalf("relay origin not re-allowed after binder rebuild: %v", err)
	}
	if db.RouteKind != direct.RouteDirect || rb.RouteKind != direct.RouteRelay {
		t.Fatalf("re-allowed kinds = %s/%s, want direct/relay", db.RouteKind, rb.RouteKind)
	}
	if db.ShareCode != code || rb.ShareCode != code {
		t.Fatalf("re-allowed share codes = %q/%q, want both %q", db.ShareCode, rb.ShareCode, code)
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
// clears the started flag. All flag reads take ds.mu: the server goroutine
// writes started/cancel under the lock when a bind fails.
func TestStartDirectServerGuardedAndStopsOnDisconnect(t *testing.T) {
	gate := direct.NewSignalGate("test-agent-id", func(string, direct.RouteKind) bool { return true })
	ds := &directState{
		server:     direct.NewDirectServer(testDirectNS, testDirectBase, nil, nil, gate, 1<<20),
		listenAddr: "127.0.0.1:0",
	}
	d := &Daemon{direct: ds}

	directStateSnapshot := func() (started, cancelSet bool) {
		ds.mu.Lock()
		defer ds.mu.Unlock()
		return ds.started, ds.cancel != nil
	}

	d.startDirectServer()
	started, cancelSet := directStateSnapshot()
	if !started {
		t.Fatalf("startDirectServer should set started=true")
	}
	if !cancelSet {
		t.Fatalf("startDirectServer should install a cancel func")
	}

	// Double-start guard: a second call must be a no-op (started stays true and
	// no replacement cancel is installed).
	d.startDirectServer()
	started, _ = directStateSnapshot()
	if !started {
		t.Fatalf("double start must leave started=true")
	}

	d.onSignalingDisconnect()
	started, cancelSet = directStateSnapshot()
	if started {
		t.Fatalf("onSignalingDisconnect should reset started=false")
	}
	if cancelSet {
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
		origins:    map[string]originPair{},
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

// TestDirectServerEndToEndSNIAdmissionAndDownload runs the REAL agent stack —
// cert.Manager (with a test CA), the daemon-shared Binder, DirectServer, and an
// OnDemandPort over a fake mapper — and drives a real TLS handshake + download
// with SNI/Host admission and real trust verification. This fails if the
// split-Binder bug (C1) regresses: bindOrigin would populate a binder the
// server does not consult, so even the KNOWN origin would be rejected during
// the handshake.
func TestDirectServerEndToEndSNIAdmissionAndDownload(t *testing.T) {
	const ns = testDirectNS
	const base = testDirectBase
	dir := t.TempDir()

	// Mint a test CA root.
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rootTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "e2e-root"},
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
	csrBlock, _ := pem.Decode(csrPEM)
	csr, err := x509.ParseCertificateRequest(csrBlock.Bytes)
	if err != nil {
		t.Fatal(err)
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
		NotAfter:     time.Now().Add(time.Hour),
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

	// Wire the real agent stack: daemon-shared Binder + DirectServer over an
	// OnDemandPort backed by a fake mapper.
	mapper := &fakeDirectMapper{ip: "203.0.113.7"}
	port := direct.NewOnDemandPortOwned(mapper, 443, 8443, time.Minute, "test", "192.168.1.20")
	gate := direct.NewSignalGate("test-agent-id", func(string, direct.RouteKind) bool { return true })
	ds := &directState{
		namespace:  ns,
		baseDomain: base,
		cert:       cm,
		gate:       gate,
		port:       port,
		mapper:     mapper,
		origins:    map[string]originPair{},
	}
	d := &Daemon{direct: ds}
	d.syncDirectServe()
	ds.server.SetResolver(stubResolver{})

	const code = "SHARE123"
	origin := testOriginFor("sbabc123")
	d.bindOrigin(code, origin)
	if err := ds.port.OpenFor(code, time.Minute); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewUnstartedServer(ds.server.Handler())
	ts.TLS = ds.server.TLSConfig()
	ts.StartTLS()
	defer ts.Close()
	addr := ts.Listener.Addr().String()

	// Real trust: verify against the test CA, with SNI = origin, Host = origin.
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			ServerName: origin,
			RootCAs:    roots,
			NextProtos: []string{"http/1.1"},
		},
		ForceAttemptHTTP2: false,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	client := &http.Client{Transport: transport}

	req, _ := http.NewRequest("GET", "https://"+origin+"/s/"+code+"/download?size=32", nil)
	req.Host = origin
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 32 {
		t.Fatalf("download body = %d bytes, want 32", len(body))
	}
	for _, b := range body {
		if b != 0 {
			t.Fatalf("download body not synthetic zeros: %v", body)
		}
	}

	// An unknown SNI must fail the handshake (the same code path that would
	// reject the KNOWN origin if the daemon and server used different binders).
	badTransport := &http.Transport{
		TLSClientConfig: &tls.Config{
			ServerName: "other." + ns + "." + base,
			RootCAs:    roots,
			NextProtos: []string{"http/1.1"},
		},
		ForceAttemptHTTP2: false,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	badClient := &http.Client{Transport: badTransport}
	badReq, _ := http.NewRequest("GET", "https://"+origin+"/s/"+code, nil)
	badReq.Host = origin
	if _, err := badClient.Do(badReq); err == nil {
		t.Fatalf("unknown SNI must fail the TLS handshake")
	}
}

// TestRelayTunnelOnlyAgentDoesNotReportOrAuthorizeFakePublicEndpoint is the
// agent half of the §11.2 relay-only isolation (control half lives in
// control/internal/directctl/directpredicate_test.go): an agent whose ONLY
// established transport is the FRP tunnel (Task 9 manager) sends
// relay_client_state telemetry and NOTHING else that could manufacture direct
// endpoint state — no report_endpoint, and an open_signal is answered with an
// error ack that never carries a public IP or granted port.
func TestRelayTunnelOnlyAgentDoesNotReportOrAuthorizeFakePublicEndpoint(t *testing.T) {
	dir := t.TempDir()
	cm, chainPEM := newBaselineCertFixture(t, dir)

	cfg := &config.Config{
		SignalingURL:      "ws://localhost:8080",
		APIKey:            "test-key",
		DefaultRelayOnly:  false,
		ImmichURL:         "http://immich.lan:2283",
		ImmichAllowedHost: "immich.lan:2283",
		ImmichAPIKey:      "api",
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	sig := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())

	// Direct transport state WITHOUT mapper/port/reporter: this agent can
	// never serve direct traffic (CGNAT/UPnP failure); only the relay tunnel
	// will ever be established.
	ds := &directState{
		namespace:  testDirectNS,
		baseDomain: testDirectBase,
		cert:       cm,
		gate:       direct.NewSignalGate(st.GetAgentID(), func(string, direct.RouteKind) bool { return true }),
		origins:    map[string]originPair{},
		listenAddr: "127.0.0.1:0",
	}
	d := &Daemon{
		config:    cfg,
		configMgr: cfgMgr,
		store:     st,
		signaling: sig,
		resolver:  direct.NewResolverRegistry(),
		sessions:  make(map[string]*Session),
		direct:    ds,
	}
	d.syncDirectServe()

	// Complete baseline enrollment (cert_issue → tls_ready) so the daemon is
	// fully operational as a relay-only agent.
	d.handleSignalingMessage(signaling.Message{Type: "cert_issue", ChainPEM: chainPEM})
	if !sig.hasSentMessage("tls_ready", nil) {
		t.Fatalf("expected tls_ready after cert_issue, got %#v", sig.messagesSnapshot())
	}
	d.handleSignalingMessage(signaling.Message{Type: "enrollment_ready"})
	if !d.canRegisterDirect() {
		t.Fatalf("relay-only agent must reach baseline readiness")
	}

	// Establish ONLY the FRP tunnel: the Task 9 manager reports lifecycle
	// transitions through its status callback, and the daemon must forward
	// them exclusively as relay_client_state telemetry.
	tunnelStatus := d.TunnelStatusCallback()
	if tunnelStatus == nil {
		t.Fatalf("tunnel status callback must be wired (Task 9 manager onStatus)")
	}
	tunnelStatus(tunnel.StatusReport{Generation: 3, Status: tunnel.StatusStarting, Reason: "frpc started"})
	tunnelStatus(tunnel.StatusReport{Generation: 3, Status: tunnel.StatusRunning, Reason: "frpc process stable"})

	if !sig.hasSentMessage("relay_client_state", map[string]any{"generation": 3, "status": "running"}) {
		t.Fatalf("tunnel lifecycle must surface as relay_client_state telemetry, got %#v", sig.messagesSnapshot())
	}

	// The tunnel must NEVER manufacture a direct endpoint report.
	for _, msg := range sig.messagesSnapshot() {
		if msg["type"] == "report_endpoint" {
			t.Fatalf("a relay-only tunnel must never send report_endpoint, saw %#v", msg)
		}
	}

	// And it must never authorize a public endpoint: an open_signal against
	// the mapper-less agent is answered with an error ack that carries no
	// public IP and no granted port (the mock records zero values for what
	// the real client's omitempty wire tags leave absent entirely).
	d.handleOpenSignal(openSignalMessage())
	for _, msg := range sig.messagesSnapshot() {
		if msg["type"] != "open_ack" {
			continue
		}
		if msg["status"] != "error" {
			t.Fatalf("open_signal must fail closed for a relay-only agent, got %#v", msg)
		}
		if ip, _ := msg["public_ip"].(string); ip != "" {
			t.Fatalf("error open_ack must not carry a manufactured public_ip, got %#v", msg)
		}
		if port, _ := msg["granted_port"].(int); port != 0 {
			t.Fatalf("error open_ack must not carry a granted_port, got %#v", msg)
		}
	}
	d.onSignalingDisconnect()
}

// TestDirectEligibleAgentStillReportsRealPublicEndpoint pins the no-regression
// half of Task 19 (§11.2): a direct-capable agent (mapper present) keeps the
// existing real reporting behavior — the initial report_endpoint {ip,0} after
// enrollment_ready, a transition report on port open, and an open_ack echoing
// the mapper's real public IP — and it sends no relay_client_state telemetry
// because no tunnel status was reported.
func TestDirectEligibleAgentStillReportsRealPublicEndpoint(t *testing.T) {
	cfg := &config.Config{SignalingURL: "ws://localhost:8080", APIKey: "test-key"}
	st := newMockStore()
	sig := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())

	const realIP = "203.0.113.7"
	mapper := &fakeDirectMapper{ip: realIP}
	port := direct.NewOnDemandPortOwned(mapper, 443, 8443, time.Minute, "test", "192.168.1.20")
	rec := &endpointRecorder{}
	reporter := direct.NewReporter(rec.record)
	reporter.SetIP(realIP)
	port.SetTransitionCallback(reporter.OnTransition)

	d := &Daemon{
		store:     st,
		signaling: sig,
		direct: &directState{
			ready:    false,
			gate:     direct.NewSignalGate(st.GetAgentID(), func(string, direct.RouteKind) bool { return true }),
			port:     port,
			mapper:   mapper,
			reporter: reporter,
		},
	}

	// Baseline readiness learns and reports the REAL mapper public IP.
	d.handleEnrollmentReady(signaling.Message{})
	if !sig.hasSentMessage("report_endpoint", map[string]any{"ip": realIP, "port": 0}) {
		t.Fatalf("direct-capable agent must report its real public IP after enrollment_ready, got %#v", sig.messagesSnapshot())
	}

	// The open-signal flow acks the real IP + granted port and the open
	// transition produces the real-IP endpoint report.
	d.handleOpenSignal(openSignalMessage())
	if !sig.hasSentMessage("open_ack", map[string]any{
		"status":       "ok",
		"public_ip":    realIP,
		"granted_port": 443,
	}) {
		t.Fatalf("direct-capable agent must ack with the real public endpoint, got %#v", sig.messagesSnapshot())
	}
	reports := waitForEndpoints(t, rec, 1)
	if reports[0].ip != realIP || reports[0].port != 443 {
		t.Fatalf("open transition must report the real IP + port, got %#v", reports[0])
	}

	// No tunnel status was reported, so no relay telemetry may appear.
	for _, msg := range sig.messagesSnapshot() {
		if msg["type"] == "relay_client_state" {
			t.Fatalf("direct reporting must not be accompanied by tunnel telemetry, saw %#v", msg)
		}
	}
	d.onSignalingDisconnect()
}
