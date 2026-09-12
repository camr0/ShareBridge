package main

// Tests for the task #16 gateway-side presence transport wiring: the
// production composition (operator env → real controlsync client → real
// PresencePublisher) posts the gateway's authoritative boot snapshot, and the
// registry adapter carries the live tunnel's renewed lease.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"sharebridge/relay/internal/controlsync"
	"sharebridge/relay/internal/frpplugin"
	"sharebridge/relay/internal/gateway"
	"sharebridge/relay/internal/metrics"
	"sharebridge/relay/internal/presence"
	"sharebridge/relay/internal/routes"
)

// gatewayPresenceRecorder records the presence snapshots the fake control
// received.
type gatewayPresenceRecorder struct {
	mu     sync.Mutex
	paths  []string
	bodies [][]byte
}

func (recorder *gatewayPresenceRecorder) record(path string, body []byte) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.paths = append(recorder.paths, path)
	recorder.bodies = append(recorder.bodies, append([]byte(nil), body...))
}

func (recorder *gatewayPresenceRecorder) count() int {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return len(recorder.paths)
}

func (recorder *gatewayPresenceRecorder) last() (string, []byte) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.paths) == 0 {
		return "", nil
	}
	return recorder.paths[len(recorder.paths)-1], recorder.bodies[len(recorder.bodies)-1]
}

func gatewayTestCA(t *testing.T, commonName string) ([]byte, *x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), parsed, key
}

func gatewayTestLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, san string, serverAuth bool) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	usage := x509.ExtKeyUsageClientAuth
	if serverAuth {
		usage = x509.ExtKeyUsageServerAuth
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: san},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		DNSNames:     []string{san},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("issue leaf: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// newGatewayPresenceControl starts a mutually authenticated fake control that
// records presence posts and returns the gateway client material on disk plus
// the endpoint URL.
func newGatewayPresenceControl(t *testing.T, recorder *gatewayPresenceRecorder) (clientCertFile, clientKeyFile, caFile, url string) {
	t.Helper()
	controlCAPEM, controlCACert, controlCAKey := gatewayTestCA(t, "gateway-presence-control-ca")
	controlCertPEM, controlKeyPEM := gatewayTestLeaf(t, controlCACert, controlCAKey, "control-sync.test.internal", true)
	gatewayCAPEM, gatewayCACert, gatewayCAKey := gatewayTestCA(t, "gateway-presence-client-ca")
	gatewayCertPEM, gatewayKeyPEM := gatewayTestLeaf(t, gatewayCACert, gatewayCAKey, "gateway-sync.test.internal", false)

	dir := t.TempDir()
	clientCertFile = filepath.Join(dir, "gateway-sync.crt")
	clientKeyFile = filepath.Join(dir, "gateway-sync.key")
	caFile = filepath.Join(dir, "control-ca.crt")
	for path, data := range map[string][]byte{
		clientCertFile: gatewayCertPEM,
		clientKeyFile:  gatewayKeyPEM,
		caFile:         controlCAPEM,
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	serverCertificate, err := tls.X509KeyPair(controlCertPEM, controlKeyPEM)
	if err != nil {
		t.Fatalf("control key pair: %v", err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(gatewayCAPEM) {
		t.Fatalf("gateway CA PEM rejected")
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		recorder.record(request.URL.Path, body)
		responseWriter.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case controlsync.PathPresenceSnapshot, controlsync.PathPresenceEvents:
			_, _ = responseWriter.Write([]byte(`{"version":1,"accepted":true}`))
		case controlsync.PathStatus:
			_, _ = responseWriter.Write([]byte(`{"version":1,"acknowledged":true}`))
		default:
			responseWriter.WriteHeader(http.StatusOK)
			_, _ = responseWriter.Write([]byte(`{}`))
		}
	}))
	server.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCertificate},
		ClientCAs:    clientCAs,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	return clientCertFile, clientKeyFile, caFile, server.URL
}

// TestPresenceTransportProductionCompositionPostsBootSnapshot proves the
// production wiring: with the six operator sync variables configured, the
// gateway builds the real sync client and a PresencePublisher over the real
// presence registry, and its first publish is the gateway's boot snapshot with
// the top-level boot identity (the adoption payload).
func TestPresenceTransportProductionCompositionPostsBootSnapshot(t *testing.T) {
	recorder := &gatewayPresenceRecorder{}
	clientCertFile, clientKeyFile, caFile, url := newGatewayPresenceControl(t, recorder)
	t.Setenv(envControlSyncURL, url)
	t.Setenv(envControlSyncSAN, "control-sync.test.internal")
	t.Setenv(envControlSyncCAFile, caFile)
	t.Setenv(envGatewaySyncCertFile, clientCertFile)
	t.Setenv(envGatewaySyncKeyFile, clientKeyFile)
	t.Setenv(envGatewayNamespace, "sbdeadbeef")

	logger := slog.New(slog.DiscardHandler)
	metricsRegistry := metrics.NewRegistry(metrics.Relay)
	streams := gateway.NewStreams()
	presenceSync := newPresenceSyncSink(metricsRegistry, newTunnelRestorationTracker(time.Now))
	registry, err := newPresenceRegistry(streams, presenceSync)
	if err != nil {
		t.Fatalf("newPresenceRegistry: %v", err)
	}
	table := routes.NewTable(registry)
	health := gateway.NewHealth()
	_, client, enabled, err := configuredControlSync(health, metricsRegistry, table, streams, registry.BootID(), logger)
	if err != nil {
		t.Fatalf("configuredControlSync: %v", err)
	}
	if !enabled || client == nil {
		t.Fatal("configuredControlSync did not build the sync client for a full configuration")
	}
	publisher, err := controlsync.NewPresencePublisher(controlsync.PresencePublisherConfig{
		Client: client,
		Source: gatewayPresenceSource{registry: registry},
		Logger: logger,
	})
	if err != nil {
		t.Fatalf("NewPresencePublisher: %v", err)
	}
	presenceSync.attach(publisher)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		publisher.Run(ctx)
		close(done)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for recorder.count() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the gateway presence transport never posted a boot snapshot")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	path, body := recorder.last()
	if path != controlsync.PathPresenceSnapshot {
		t.Fatalf("presence post path = %q, want the snapshot endpoint", path)
	}
	var envelope controlsync.PresenceEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode presence snapshot %s: %v", body, err)
	}
	if envelope.GatewayBootID != registry.BootID() {
		t.Fatalf("snapshot boot = %q, want the registry boot %q", envelope.GatewayBootID, registry.BootID())
	}
	if envelope.Events == nil || len(envelope.Events) != 0 {
		t.Fatalf("fresh registry snapshot events = %+v, want an empty list", envelope.Events)
	}
}

// TestGatewayPresenceSourceCarriesRenewedLease proves the adapter from the
// authoritative registry to the wire entry: a live tunnel's CURRENT lease
// (renewed by a Ping with no transition event) is what gets republished.
func TestGatewayPresenceSourceCarriesRenewedLease(t *testing.T) {
	probe := &gatewayProbeStub{source: "127.0.0.1:55555"}
	registry, err := presence.NewRegistry(presence.Config{BootID: "gw-boot-adapter", Probe: probe.probe})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	const (
		agent = "agent-adapter"
		port  = 10042
	)
	registry.ObserveFRPEvent(frpplugin.PresenceFact{Operation: frpplugin.OperationLogin, AgentRecordID: agent, ProxyName: "proxy-1", RelayPort: port, Generation: 1, RunID: "run-1"})
	registry.ObserveFRPEvent(frpplugin.PresenceFact{Operation: frpplugin.OperationNewProxy, AgentRecordID: agent, ProxyName: "proxy-1", RelayPort: port, Generation: 1, RunID: "run-1"})
	registry.ObserveFRPEvent(frpplugin.PresenceFact{Operation: frpplugin.OperationNewUserConn, AgentRecordID: agent, ProxyName: "proxy-1", RelayPort: port, Generation: 1, RunID: "run-1", RemoteAddr: probe.source})
	deadline := time.Now().Add(2 * time.Second)
	for !registry.Online(agent, port, 1) {
		if time.Now().After(deadline) {
			t.Fatal("tunnel never confirmed online for the adapter test")
		}
		time.Sleep(2 * time.Millisecond)
	}

	bootID, revision, entries := (gatewayPresenceSource{registry: registry}).PresenceSnapshot()
	if bootID != "gw-boot-adapter" || revision == 0 {
		t.Fatalf("adapter snapshot boot/revision = %q/%d, want gw-boot-adapter and a nonzero revision", bootID, revision)
	}
	if len(entries) != 1 || entries[0].AgentRecordID != agent || entries[0].RelayPort != port || entries[0].Generation != 1 {
		t.Fatalf("adapter entries = %+v, want the live tunnel", entries)
	}
	if !entries[0].LeaseExpiresAt.After(time.Now()) {
		t.Fatalf("adapter lease %s is already expired", entries[0].LeaseExpiresAt)
	}
}

type gatewayProbeStub struct {
	source string
}

func (stub *gatewayProbeStub) probe(_ context.Context, _ int) (string, error) {
	return stub.source, nil
}
