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
	"strings"
	"testing"
	"time"

	"sharebridge/agent/internal/cert"
	"sharebridge/agent/internal/config"
	"sharebridge/agent/internal/direct"
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
