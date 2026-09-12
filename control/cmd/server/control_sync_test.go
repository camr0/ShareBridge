package main

// Tests for the task #16 control-side §11.3 sync listener wiring: the
// production configuration entry point builds the real relayctl server (real
// route publisher + presence view in main), binds private/loopback by default,
// and refuses a partial or unreadable configuration fail-closed. The full
// authenticated-wire behaviour of the listener is proven by
// relayctl's own end-to-end test; this file proves the production entrypoint
// actually constructs and configures it.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sharebridge/control/internal/config"
)

var controlSyncEnvNames = []string{
	"CONTROL_SYNC_BIND_ADDR",
	"CONTROL_SYNC_CERT_FILE",
	"CONTROL_SYNC_KEY_FILE",
	"CONTROL_SYNC_CLIENT_CA_FILE",
	"CONTROL_SYNC_EXPECTED_CLIENT_SAN",
}

func clearControlSyncEnv(t *testing.T) {
	t.Helper()
	for _, name := range controlSyncEnvNames {
		t.Setenv(name, "")
	}
}

// writeControlSyncTestMaterial writes a self-signed leaf usable as both the
// server key pair and the client CA pool (the wiring test only needs parseable
// PEM, not a real PKI; the listener's wire behaviour is covered elsewhere).
func writeControlSyncTestMaterial(t *testing.T) (certFile, keyFile, caFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "control-sync-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IsCA:         true,
		DNSNames:     []string{"control-sync-test"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	dir := t.TempDir()
	certFile = filepath.Join(dir, "control-sync.crt")
	keyFile = filepath.Join(dir, "control-sync.key")
	caFile = filepath.Join(dir, "sync-ca.crt")
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	if err := os.WriteFile(caFile, certPEM, 0o600); err != nil {
		t.Fatalf("write CA: %v", err)
	}
	return certFile, keyFile, caFile
}

func TestConfiguredControlSyncServerDisabledWithoutConfig(t *testing.T) {
	clearControlSyncEnv(t)
	cfg := config.Load()
	server, err := configuredControlSyncServer(cfg, nil, nil, nil)
	if err != nil {
		t.Fatalf("configuredControlSyncServer error = %v, want nil when unconfigured", err)
	}
	if server != nil {
		t.Fatal("configuredControlSyncServer built a listener without configuration")
	}
}

func TestConfiguredControlSyncServerFailsClosedOnPartialConfig(t *testing.T) {
	clearControlSyncEnv(t)
	certFile, _, _ := writeControlSyncTestMaterial(t)
	t.Setenv("CONTROL_SYNC_CERT_FILE", certFile) // deliberately incomplete
	cfg := config.Load()
	if _, err := configuredControlSyncServer(cfg, nil, nil, nil); err == nil {
		t.Fatal("partial control-sync configuration was accepted; it must refuse startup")
	}
}

func TestConfiguredControlSyncServerFailsClosedOnUnreadableMaterial(t *testing.T) {
	clearControlSyncEnv(t)
	_, keyFile, caFile := writeControlSyncTestMaterial(t)
	t.Setenv("CONTROL_SYNC_CERT_FILE", filepath.Join(t.TempDir(), "missing.crt"))
	t.Setenv("CONTROL_SYNC_KEY_FILE", keyFile)
	t.Setenv("CONTROL_SYNC_CLIENT_CA_FILE", caFile)
	t.Setenv("CONTROL_SYNC_EXPECTED_CLIENT_SAN", "gateway-sync.internal")
	cfg := config.Load()
	if _, err := configuredControlSyncServer(cfg, nil, nil, nil); err == nil {
		t.Fatal("unreadable certificate material was accepted; it must refuse startup")
	}
}

func TestConfiguredControlSyncServerBuildsPrivateMTLSListener(t *testing.T) {
	clearControlSyncEnv(t)
	certFile, keyFile, caFile := writeControlSyncTestMaterial(t)
	t.Setenv("CONTROL_SYNC_CERT_FILE", certFile)
	t.Setenv("CONTROL_SYNC_KEY_FILE", keyFile)
	t.Setenv("CONTROL_SYNC_CLIENT_CA_FILE", caFile)
	t.Setenv("CONTROL_SYNC_EXPECTED_CLIENT_SAN", "gateway-sync.internal")
	cfg := config.Load()
	server, err := configuredControlSyncServer(cfg, nil, nil, nil)
	if err != nil {
		t.Fatalf("configuredControlSyncServer: %v", err)
	}
	if server == nil {
		t.Fatal("configuredControlSyncServer returned a nil listener for a full configuration")
	}
	if got := server.BindAddress(); got != config.DefaultControlSyncBindAddr {
		t.Fatalf("bind address = %q, want the loopback default %q", got, config.DefaultControlSyncBindAddr)
	}
	tlsConfig := server.TLSConfig()
	if tlsConfig.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("ClientAuth = %v, want RequireAndVerifyClientCert", tlsConfig.ClientAuth)
	}
	if tlsConfig.MinVersion != tls.VersionTLS13 {
		t.Fatalf("MinVersion = %v, want TLS 1.3", tlsConfig.MinVersion)
	}
	if len(tlsConfig.Certificates) != 1 {
		t.Fatalf("server certificates = %d, want 1", len(tlsConfig.Certificates))
	}
}

func TestConfiguredControlSyncServerRefusesAPublicBind(t *testing.T) {
	clearControlSyncEnv(t)
	certFile, keyFile, caFile := writeControlSyncTestMaterial(t)
	t.Setenv("CONTROL_SYNC_BIND_ADDR", "0.0.0.0:9443")
	t.Setenv("CONTROL_SYNC_CERT_FILE", certFile)
	t.Setenv("CONTROL_SYNC_KEY_FILE", keyFile)
	t.Setenv("CONTROL_SYNC_CLIENT_CA_FILE", caFile)
	t.Setenv("CONTROL_SYNC_EXPECTED_CLIENT_SAN", "gateway-sync.internal")
	cfg := config.Load()
	if _, err := configuredControlSyncServer(cfg, nil, nil, nil); err == nil {
		t.Fatal("public (unspecified) bind address was accepted; the sync listener must stay private")
	}
}
