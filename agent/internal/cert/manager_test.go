package cert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// issueTestChain mints a chain (leaf with the two wildcard SANs + a self-signed
// root) so Install's ValidateChain path can succeed, and returns a trust pool
// containing the root.
func issueTestChain(t *testing.T, leafPub *ecdsa.PublicKey, namespace, baseDomain string, notAfter time.Time) ([]byte, *x509.CertPool) {
	t.Helper()
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

	sans := wildcardSANs(namespace, baseDomain)
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: sans[0]},
		DNSNames:     sans,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, root, leafPub, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	chain := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})...)

	pool := x509.NewCertPool()
	pool.AddCert(root)
	return chain, pool
}

func TestManagerGenerateInstallRenewal(t *testing.T) {
	const ns = "v7q4km2x9pz6dn3w"
	dir := t.TempDir()
	m := NewManager(dir, testBase, nil)

	if err := m.SetNamespace(ns); err != nil {
		t.Fatal(err)
	}

	csrPEM, err := m.GenerateCSR()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "direct", "key.pem")); err != nil {
		t.Fatalf("key not at <dir>/direct/key.pem: %v", err)
	}

	// Recover the public key from the CSR (the private key never leaves the
	// manager) so a chain can be minted for it.
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
		t.Fatalf("CSR public key is %T, want *ecdsa.PublicKey", csr.PublicKey)
	}

	// 120-day chain: install succeeds, and it does not need renewal.
	chain, pool := issueTestChain(t, pub, ns, testBase, time.Now().Add(120*24*time.Hour))
	m.roots = pool // white-box injection (same package)
	if err := m.Install(chain); err != nil {
		t.Fatal(err)
	}
	if !m.Installed() {
		t.Fatal("Installed() = false after successful Install")
	}
	if fp, err := m.LeafFingerprint(); err != nil || fp == "" {
		t.Fatalf("LeafFingerprint() = %q, %v", fp, err)
	}
	if m.NeedsRenewal() {
		t.Fatal("NeedsRenewal() = true for a 120-day cert")
	}

	// Key reuse: GenerateCSR again must not mint a new key.
	keyPEMBefore := append([]byte(nil), m.keyPEM...)
	csrPEM2, err := m.GenerateCSR()
	if err != nil {
		t.Fatal(err)
	}
	block2, _ := pem.Decode(csrPEM2)
	csr2, err := x509.ParseCertificateRequest(block2.Bytes)
	if err != nil {
		t.Fatalf("parse second CSR: %v", err)
	}
	pub2, ok := csr2.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("second CSR public key is %T", csr2.PublicKey)
	}
	if !publicKeysEqual(pub, pub2) {
		t.Fatal("GenerateCSR minted a new key on re-run")
	}
	keyOnDisk, err := os.ReadFile(filepath.Join(dir, "direct", "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if string(keyOnDisk) != string(keyPEMBefore) {
		t.Fatal("key.pem on disk changed across GenerateCSR")
	}

	// Short-lived chain: needs renewal.
	shortChain, shortPool := issueTestChain(t, pub, ns, testBase, time.Now().Add(time.Hour))
	m.roots = shortPool
	if err := m.Install(shortChain); err != nil {
		t.Fatal(err)
	}
	if !m.NeedsRenewal() {
		t.Fatal("NeedsRenewal() = false for a short-lived cert")
	}
}

func TestManagerLoadReusesPersistedKeyWithoutChain(t *testing.T) {
	const ns = "v7q4km2x9pz6dn3w"
	dir := t.TempDir()
	m := NewManager(dir, testBase, nil)
	if err := m.SetNamespace(ns); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GenerateCSR(); err != nil {
		t.Fatal(err)
	}
	keyOnDisk, err := os.ReadFile(filepath.Join(dir, "direct", "key.pem"))
	if err != nil {
		t.Fatal(err)
	}

	// A fresh manager over the same dir, Load()ed BEFORE any chain is
	// installed, must restore the persisted key (not mint a new one on the
	// next GenerateCSR) — otherwise a restart between CSR and install changes
	// the CSR fingerprint and stalls re-enrollment under the control plane's
	// issuance cooldown.
	m2 := NewManager(dir, testBase, nil)
	if err := m2.Load(); err != nil {
		t.Fatal(err)
	}
	if _, err := m2.GenerateCSR(); err != nil {
		t.Fatal(err)
	}
	keyOnDisk2, err := os.ReadFile(filepath.Join(dir, "direct", "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if string(keyOnDisk2) != string(keyOnDisk) {
		t.Fatal("Load() did not restore the persisted key; GenerateCSR regenerated it")
	}
}
