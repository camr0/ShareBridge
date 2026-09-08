package relayctl

// Tests for the §11.3 control↔gateway internal sync protocol (plan Task 11).
// The gateway is the HTTP client on every endpoint; control serves the
// mutually authenticated private-network listener. Golden payloads under
// ../../../testdata/relay-sync are consumed by both Go modules to pin the
// exact wire bytes on each side.

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	// Sync test identities. Production SANs are operator-provisioned; these
	// mirror the §17.1 service roles.
	testControlIdentity = "sharebridge-control.sync.internal"
	testGatewayIdentity = "sharebridge-relay-gateway.sync.internal"

	testGoldenDirectory = "../../../testdata/relay-sync"
)

func readGoldenPayload(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(testGoldenDirectory, name))
	if err != nil {
		t.Fatalf("read golden payload %s: %v", name, err)
	}
	return bytes.TrimRight(raw, "\n")
}

func decodeStrictJSON(t *testing.T, data []byte, target any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(target); err != nil {
		t.Fatalf("decode JSON payload: %v", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatalf("payload has trailing JSON data")
	}
}

// --- certificate test helpers (independent of the content PKI) ---

func generateSyncTestCA(t *testing.T, commonName string) ([]byte, *x509.Certificate, *ecdsa.PrivateKey) {
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
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return caPEM, caCertificate, caKey
}

func issueSyncTestLeaf(t *testing.T, caCertificate *x509.Certificate, caKey *ecdsa.PrivateKey, dnsSAN string, serverAuth bool) ([]byte, []byte) {
	t.Helper()
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	keyUsage := x509.KeyUsageDigitalSignature
	extKeyUsage := []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	if serverAuth {
		extKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: dnsSAN, Organization: []string{"sharebridge-test"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     keyUsage,
		ExtKeyUsage:  extKeyUsage,
		DNSNames:     []string{dnsSAN},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCertificate, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("issue leaf certificate: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

type syncTestCertificates struct {
	serverCAPEM     []byte
	serverCertPEM   []byte
	serverKeyPEM    []byte
	gatewayCAPEM    []byte
	gatewayCertPEM  []byte
	gatewayKeyPEM   []byte
	foreignCAPEM    []byte
	foreignCertPEM  []byte
	foreignKeyPEM   []byte
	wrongSANCertPEM []byte
	wrongSANKeyPEM  []byte
}

func newSyncTestCertificates(t *testing.T) syncTestCertificates {
	t.Helper()
	var certs syncTestCertificates
	// The server CA both issues the control server leaf and is what gateway
	// clients pin as their server CA.
	serverCAPEM, serverIssuerCert, serverIssuerKey := generateSyncTestCA(t, "sharebridge-sync-control-ca")
	certs.serverCAPEM = serverCAPEM
	certs.serverCertPEM, certs.serverKeyPEM = issueSyncTestLeaf(t, serverIssuerCert, serverIssuerKey, testControlIdentity, true)

	gatewayCAPEM, gatewayCert, gatewayKey := generateSyncTestCA(t, "sharebridge-sync-gateway-ca")
	certs.gatewayCAPEM = gatewayCAPEM
	certs.gatewayCertPEM, certs.gatewayKeyPEM = issueSyncTestLeaf(t, gatewayCert, gatewayKey, testGatewayIdentity, false)

	foreignCAPEM, foreignCert, foreignKey := generateSyncTestCA(t, "sharebridge-sync-foreign-ca")
	certs.foreignCAPEM = foreignCAPEM
	certs.foreignCertPEM, certs.foreignKeyPEM = issueSyncTestLeaf(t, foreignCert, foreignKey, testGatewayIdentity, false)

	// Right CA, wrong SAN: must pass TLS but fail the application-layer
	// identity check with 403.
	certs.wrongSANCertPEM, certs.wrongSANKeyPEM = issueSyncTestLeaf(t, gatewayCert, gatewayKey, "sharebridge-relay-gateway.impersonator.invalid", false)
	return certs
}

// newSyncTestHTTPClient pins the control server CA and presents the given
// client certificate, mirroring what the gateway controlsync client does.
func newSyncTestHTTPClient(t *testing.T, serverCAPEM []byte, clientCertPEM, clientKeyPEM []byte, expectedServerSAN string) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(serverCAPEM) {
		t.Fatalf("server CA PEM rejected")
	}
	var clientCertificates []tls.Certificate
	if clientCertPEM != nil {
		pair, err := tls.X509KeyPair(clientCertPEM, clientKeyPEM)
		if err != nil {
			t.Fatalf("client key pair: %v", err)
		}
		clientCertificates = []tls.Certificate{pair}
	}
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:      pool,
			Certificates: clientCertificates,
			ServerName:   expectedServerSAN,
			MinVersion:   tls.VersionTLS13,
		},
	}}
}

// --- stubs for the Task 12 publisher / Task 15 presence+status sinks ---

type stubRouteSource struct {
	snapshot    Snapshot
	snapshotErr error
	page        DeltaPage
	pageErr     error
	seenSince   []uint64
}

func (stub *stubRouteSource) RouteSnapshot() (Snapshot, error) {
	return stub.snapshot, stub.snapshotErr
}

func (stub *stubRouteSource) RouteDeltas(since uint64) (DeltaPage, error) {
	stub.seenSince = append(stub.seenSince, since)
	return stub.page, stub.pageErr
}

type stubPresenceSink struct {
	snapshots []PresenceEnvelope
	events    []PresenceEnvelope
	err       error
}

func (stub *stubPresenceSink) ApplyPresenceSnapshot(envelope PresenceEnvelope) error {
	if stub.err != nil {
		return stub.err
	}
	stub.snapshots = append(stub.snapshots, envelope)
	return nil
}

func (stub *stubPresenceSink) ApplyPresenceEvents(envelope PresenceEnvelope) error {
	if stub.err != nil {
		return stub.err
	}
	stub.events = append(stub.events, envelope)
	return nil
}

type stubStatusSink struct {
	acks []StatusAck
	err  error
}

func (stub *stubStatusSink) Acknowledge(ack StatusAck) error {
	if stub.err != nil {
		return stub.err
	}
	stub.acks = append(stub.acks, ack)
	return nil
}

// startSyncTestServer mounts the control sync server behind httptest with its
// own pinned TLS configuration, exactly as the Task 12 private listener will.
func startSyncTestServer(t *testing.T, server *Server) *httptest.Server {
	t.Helper()
	testServer := httptest.NewUnstartedServer(server)
	testServer.TLS = server.TLSConfig()
	testServer.StartTLS()
	t.Cleanup(testServer.Close)
	return testServer
}

func newSyncTestServer(t *testing.T, certs syncTestCertificates, mutate func(*ServerConfig)) (*Server, *httptest.Server) {
	t.Helper()
	config := ServerConfig{
		BindAddress:       "127.0.0.1:9443",
		ServerCertPEM:     certs.serverCertPEM,
		ServerKeyPEM:      certs.serverKeyPEM,
		ClientCAPEM:       certs.gatewayCAPEM,
		ExpectedClientSAN: testGatewayIdentity,
		RouteSource:       &stubRouteSource{},
		PresenceSink:      &stubPresenceSink{},
		StatusSink:        &stubStatusSink{},
	}
	if mutate != nil {
		mutate(&config)
	}
	server, err := NewServer(config)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return server, startSyncTestServer(t, server)
}

func syncTestRoute(hostname string, revision uint64, active bool) Route {
	return Route{
		Hostname:      hostname,
		AgentRecordID: "agtw8hks2m4qp01",
		RelayPort:     10000,
		Generation:    3,
		SessionID:     "sess7d2kx9pqrb04",
		Revision:      revision,
		Active:        active,
		Limits:        Limits{MaxStreamsPerOrigin: 32, MaxStreamsPerAgent: 64, MaxStreamsGlobal: 8192},
	}
}

func syncTestPresenceEvent(bootID string, revision uint64, state string) PresenceEvent {
	event := PresenceEvent{
		GatewayBootID: bootID,
		Revision:      revision,
		AgentRecordID: "agtw8hks2m4qp01",
		RelayPort:     10000,
		Generation:    3,
		State:         state,
	}
	if state == PresenceStateOnline {
		event.LeaseExpiresAt = "2026-09-03T18:00:45Z"
	}
	return event
}

// --- Task 11 named test 1: golden payloads round-trip in both modules ---

func TestSyncGoldenPayloadsRoundTripBothModules(t *testing.T) {
	t.Run("snapshot", func(t *testing.T) {
		golden := readGoldenPayload(t, "snapshot.json")
		var decoded Snapshot
		decodeStrictJSON(t, golden, &decoded)

		reEncoded, err := json.Marshal(decoded)
		if err != nil {
			t.Fatalf("marshal decoded snapshot: %v", err)
		}
		if !bytes.Equal(reEncoded, golden) {
			t.Fatalf("decoded snapshot does not re-encode to the golden bytes")
		}

		constructed := Snapshot{
			Version:  ProtocolVersion,
			Epoch:    7777,
			Revision: 42,
			Routes: []Route{
				{
					Hostname:      "photos.relay.sb1a2b3c4.photos.example.com",
					AgentRecordID: "agtw8hks2m4qp01",
					RelayPort:     10000,
					Generation:    3,
					SessionID:     "sess7d2kx9pqrb04",
					Revision:      42,
					Active:        true,
					Limits:        Limits{MaxStreamsPerOrigin: 32, MaxStreamsPerAgent: 64, MaxStreamsGlobal: 8192},
				},
				{
					Hostname:      "videos.relay.sb9f8e7d6.videos.example.com",
					AgentRecordID: "agtq1z9mxr5nv73",
					RelayPort:     10037,
					Generation:    11,
					SessionID:     "sessm2v6bwn0ch18",
					Revision:      40,
					Active:        true,
					Limits:        Limits{MaxStreamsPerOrigin: 16, MaxStreamsPerAgent: 32, MaxStreamsGlobal: 8192},
				},
			},
		}
		constructedBytes, err := json.Marshal(constructed)
		if err != nil {
			t.Fatalf("marshal constructed snapshot: %v", err)
		}
		if !bytes.Equal(constructedBytes, golden) {
			t.Fatalf("constructed snapshot does not match the golden bytes")
		}
		if err := ValidateSnapshot(decoded, MaxRoutesPerSnapshot); err != nil {
			t.Fatalf("golden snapshot rejected: %v", err)
		}
	})

	t.Run("delta", func(t *testing.T) {
		golden := readGoldenPayload(t, "delta.json")
		var decoded DeltaPage
		decodeStrictJSON(t, golden, &decoded)

		reEncoded, err := json.Marshal(decoded)
		if err != nil {
			t.Fatalf("marshal decoded delta page: %v", err)
		}
		if !bytes.Equal(reEncoded, golden) {
			t.Fatalf("decoded delta page does not re-encode to the golden bytes")
		}

		constructed := DeltaPage{
			Version:        ProtocolVersion,
			Epoch:          7777,
			Status:         DeltaStatusOK,
			Since:          40,
			LatestRevision: 43,
			Deltas: []RouteDelta{
				{
					Revision:  41,
					Operation: RouteOperationLimit,
					Route: Route{
						Hostname:      "videos.relay.sb9f8e7d6.videos.example.com",
						AgentRecordID: "agtq1z9mxr5nv73",
						RelayPort:     10037,
						Generation:    11,
						SessionID:     "sessm2v6bwn0ch18",
						Revision:      41,
						Active:        true,
						Limits:        Limits{MaxStreamsPerOrigin: 24, MaxStreamsPerAgent: 48, MaxStreamsGlobal: 8192},
					},
				},
				{
					Revision:  42,
					Operation: RouteOperationAdd,
					Route: Route{
						Hostname:      "docs.relay.sb5t4g3h2.docs.example.com",
						AgentRecordID: "agtw8hks2m4qp01",
						RelayPort:     10000,
						Generation:    3,
						SessionID:     "sessp8lkr3tvq52",
						Revision:      42,
						Active:        true,
						Limits:        Limits{MaxStreamsPerOrigin: 32, MaxStreamsPerAgent: 64, MaxStreamsGlobal: 8192},
					},
				},
				{
					Revision:  43,
					Operation: RouteOperationRevoke,
					Route: Route{
						Hostname:      "videos.relay.sb9f8e7d6.videos.example.com",
						AgentRecordID: "agtq1z9mxr5nv73",
						RelayPort:     10037,
						Generation:    11,
						SessionID:     "sessm2v6bwn0ch18",
						Revision:      43,
						Active:        false,
					},
				},
			},
		}
		constructedBytes, err := json.Marshal(constructed)
		if err != nil {
			t.Fatalf("marshal constructed delta page: %v", err)
		}
		if !bytes.Equal(constructedBytes, golden) {
			t.Fatalf("constructed delta page does not match the golden bytes")
		}
		if err := ValidateDeltaPage(decoded, MaxDeltasPerPage); err != nil {
			t.Fatalf("golden delta page rejected: %v", err)
		}
	})

	t.Run("presence", func(t *testing.T) {
		golden := readGoldenPayload(t, "presence.json")
		var decoded PresenceEnvelope
		decodeStrictJSON(t, golden, &decoded)

		reEncoded, err := json.Marshal(decoded)
		if err != nil {
			t.Fatalf("marshal decoded presence envelope: %v", err)
		}
		if !bytes.Equal(reEncoded, golden) {
			t.Fatalf("decoded presence envelope does not re-encode to the golden bytes")
		}

		constructed := PresenceEnvelope{
			Version: ProtocolVersion,
			Events: []PresenceEvent{
				{
					GatewayBootID:  "gwboot4k9x2m7qzr15",
					Revision:       9001,
					AgentRecordID:  "agtw8hks2m4qp01",
					RelayPort:      10000,
					Generation:     3,
					State:          PresenceStateOnline,
					LeaseExpiresAt: "2026-09-03T18:00:45Z",
				},
				{
					GatewayBootID:  "gwboot4k9x2m7qzr15",
					Revision:       9002,
					AgentRecordID:  "agtq1z9mxr5nv73",
					RelayPort:      10037,
					Generation:     11,
					State:          PresenceStateOnline,
					LeaseExpiresAt: "2026-09-03T18:01:00Z",
				},
				{
					GatewayBootID: "gwboot4k9x2m7qzr15",
					Revision:      9003,
					AgentRecordID: "agtq1z9mxr5nv73",
					RelayPort:     10037,
					Generation:    11,
					State:         PresenceStateOffline,
				},
			},
		}
		constructedBytes, err := json.Marshal(constructed)
		if err != nil {
			t.Fatalf("marshal constructed presence envelope: %v", err)
		}
		if !bytes.Equal(constructedBytes, golden) {
			t.Fatalf("constructed presence envelope does not match the golden bytes")
		}
		if err := ValidatePresenceEventEnvelope(decoded, MaxPresenceEventsPerEnvelope); err != nil {
			t.Fatalf("golden presence event envelope rejected: %v", err)
		}
	})

	t.Run("presence snapshot envelope shares the event wire shape", func(t *testing.T) {
		envelope := PresenceEnvelope{
			Version: ProtocolVersion,
			Events: []PresenceEvent{
				syncTestPresenceEvent("gwboot4k9x2m7qzr15", 9001, PresenceStateOnline),
				syncTestPresenceEvent("gwboot4k9x2m7qzr15", 9001, PresenceStateOnline),
			},
		}
		if err := ValidatePresenceSnapshotEnvelope(envelope, MaxPresenceEventsPerEnvelope); err != nil {
			t.Fatalf("snapshot envelope rejected: %v", err)
		}
		// The same shape is not a valid incremental event page: revisions of
		// ordered events must strictly increase.
		if err := ValidatePresenceEventEnvelope(envelope, MaxPresenceEventsPerEnvelope); err == nil {
			t.Fatalf("event envelope accepted repeated revisions")
		}
	})
}

// --- Task 11 named test 2: mutual TLS is mandatory ---

func TestSyncRequiresMutualTLS(t *testing.T) {
	certs := newSyncTestCertificates(t)

	t.Run("accepted gateway identity receives the snapshot", func(t *testing.T) {
		source := &stubRouteSource{snapshot: Snapshot{Version: ProtocolVersion, Epoch: 7, Revision: 7, Routes: []Route{syncTestRoute("photos.relay.sb1a2b3c4.photos.example.com", 7, true)}}}
		server, testServer := newSyncTestServer(t, certs, func(config *ServerConfig) {
			config.RouteSource = source
		})
		_ = server
		client := newSyncTestHTTPClient(t, certs.serverCAPEM, certs.gatewayCertPEM, certs.gatewayKeyPEM, testControlIdentity)

		response, err := client.Get(testServer.URL + PathSnapshot)
		if err != nil {
			t.Fatalf("GET snapshot: %v", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("snapshot status = %d, want 200", response.StatusCode)
		}
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatalf("read snapshot body: %v", err)
		}
		var decoded Snapshot
		decodeStrictJSON(t, body, &decoded)
		if !reflect.DeepEqual(decoded, source.snapshot) {
			t.Fatalf("snapshot mismatch")
		}
	})

	t.Run("missing client certificate is refused", func(t *testing.T) {
		_, testServer := newSyncTestServer(t, certs, nil)
		client := newSyncTestHTTPClient(t, certs.serverCAPEM, nil, nil, testControlIdentity)
		if _, err := client.Get(testServer.URL + PathSnapshot); err == nil {
			t.Fatalf("request without a client certificate was accepted")
		}
	})

	t.Run("client certificate from a foreign CA is refused", func(t *testing.T) {
		_, testServer := newSyncTestServer(t, certs, nil)
		client := newSyncTestHTTPClient(t, certs.serverCAPEM, certs.foreignCertPEM, certs.foreignKeyPEM, testControlIdentity)
		if _, err := client.Get(testServer.URL + PathSnapshot); err == nil {
			t.Fatalf("request with a foreign client certificate was accepted")
		}
	})

	t.Run("wrong client SAN gets 403", func(t *testing.T) {
		_, testServer := newSyncTestServer(t, certs, nil)
		client := newSyncTestHTTPClient(t, certs.serverCAPEM, certs.wrongSANCertPEM, certs.wrongSANKeyPEM, testControlIdentity)
		response, err := client.Get(testServer.URL + PathSnapshot)
		if err != nil {
			t.Fatalf("GET snapshot with wrong-SAN certificate: %v", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("wrong-SAN status = %d, want 403", response.StatusCode)
		}
	})

	t.Run("plain HTTP mount is refused", func(t *testing.T) {
		server, err := NewServer(ServerConfig{
			BindAddress:       "127.0.0.1:9443",
			ServerCertPEM:     certs.serverCertPEM,
			ServerKeyPEM:      certs.serverKeyPEM,
			ClientCAPEM:       certs.gatewayCAPEM,
			ExpectedClientSAN: testGatewayIdentity,
			RouteSource:       &stubRouteSource{},
		})
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		plainServer := httptest.NewServer(server)
		defer plainServer.Close()
		response, err := http.Get(plainServer.URL + PathSnapshot)
		if err != nil {
			t.Fatalf("plain GET: %v", err)
		}
		defer response.Body.Close()
		if response.StatusCode == http.StatusOK {
			t.Fatalf("plain HTTP request was served the snapshot")
		}
	})

	t.Run("client rejects a foreign server certificate", func(t *testing.T) {
		_, testServer := newSyncTestServer(t, certs, nil)
		client := newSyncTestHTTPClient(t, certs.foreignCAPEM, certs.gatewayCertPEM, certs.gatewayKeyPEM, testControlIdentity)
		if _, err := client.Get(testServer.URL + PathSnapshot); err == nil {
			t.Fatalf("client accepted a server certificate outside its pinned CA")
		}
	})

	t.Run("client rejects wrong server SAN", func(t *testing.T) {
		_, testServer := newSyncTestServer(t, certs, nil)
		client := newSyncTestHTTPClient(t, certs.serverCAPEM, certs.gatewayCertPEM, certs.gatewayKeyPEM, "sharebridge-control.impersonator.invalid")
		if _, err := client.Get(testServer.URL + PathSnapshot); err == nil {
			t.Fatalf("client accepted a server with the wrong SAN")
		}
	})

	t.Run("public bind address is rejected at configuration", func(t *testing.T) {
		for _, address := range []string{"0.0.0.0:9443", ":9443", "8.8.8.8:9443", "203.0.113.10:9443"} {
			if _, err := NewServer(ServerConfig{
				BindAddress:       address,
				ServerCertPEM:     certs.serverCertPEM,
				ServerKeyPEM:      certs.serverKeyPEM,
				ClientCAPEM:       certs.gatewayCAPEM,
				ExpectedClientSAN: testGatewayIdentity,
			}); err == nil {
				t.Fatalf("bind address %q accepted", address)
			}
		}
	})
}

// --- Task 11 named test 3: oversize, unknown version, bad revision ---

func TestSyncRejectsOversizeUnknownVersionAndBadRevision(t *testing.T) {
	certs := newSyncTestCertificates(t)

	postJSON := func(t *testing.T, client *http.Client, baseURL string, path string, payload []byte) *http.Response {
		t.Helper()
		request, err := http.NewRequest(http.MethodPost, baseURL+path, bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("build POST: %v", err)
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		t.Cleanup(func() { response.Body.Close() })
		return response
	}

	t.Run("oversize presence body", func(t *testing.T) {
		_, testServer := newSyncTestServer(t, certs, func(config *ServerConfig) {
			config.MaxRequestBodyBytes = 1024
		})
		client := newSyncTestHTTPClient(t, certs.serverCAPEM, certs.gatewayCertPEM, certs.gatewayKeyPEM, testControlIdentity)
		oversize := bytes.Repeat([]byte("a"), 2048)
		response := postJSON(t, client, testServer.URL, PathPresenceEvents, oversize)
		if response.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversize body status = %d, want 413", response.StatusCode)
		}
	})

	t.Run("unknown version on presence events", func(t *testing.T) {
		_, testServer := newSyncTestServer(t, certs, nil)
		client := newSyncTestHTTPClient(t, certs.serverCAPEM, certs.gatewayCertPEM, certs.gatewayKeyPEM, testControlIdentity)
		envelope := PresenceEnvelope{Version: ProtocolVersion + 1, Events: []PresenceEvent{syncTestPresenceEvent("gwboot4k9x2m7qzr15", 9001, PresenceStateOnline)}}
		payload, err := json.Marshal(envelope)
		if err != nil {
			t.Fatalf("marshal envelope: %v", err)
		}
		response := postJSON(t, client, testServer.URL, PathPresenceEvents, payload)
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("unknown version status = %d, want 400", response.StatusCode)
		}
	})

	t.Run("unknown version on status ack", func(t *testing.T) {
		_, testServer := newSyncTestServer(t, certs, nil)
		client := newSyncTestHTTPClient(t, certs.serverCAPEM, certs.gatewayCertPEM, certs.gatewayKeyPEM, testControlIdentity)
		ack := StatusAck{Version: ProtocolVersion + 1, GatewayBootID: "gwboot4k9x2m7qzr15", LastAppliedRevision: 9}
		payload, err := json.Marshal(ack)
		if err != nil {
			t.Fatalf("marshal ack: %v", err)
		}
		response := postJSON(t, client, testServer.URL, PathStatus, payload)
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("unknown version status = %d, want 400", response.StatusCode)
		}
	})

	t.Run("deltas require an explicit non-negative numeric since", func(t *testing.T) {
		_, testServer := newSyncTestServer(t, certs, nil)
		client := newSyncTestHTTPClient(t, certs.serverCAPEM, certs.gatewayCertPEM, certs.gatewayKeyPEM, testControlIdentity)

		for _, query := range []string{"", "?since=abc", "?since=-1", "?since=1&since=2"} {
			response, err := client.Get(testServer.URL + PathDeltas + query)
			if err != nil {
				t.Fatalf("GET deltas %q: %v", query, err)
			}
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("deltas query %q status = %d, want 400 (body %q)", query, response.StatusCode, strings.TrimSpace(string(body)))
			}
		}
	})

	t.Run("source snapshot with bad revision is never emitted", func(t *testing.T) {
		source := &stubRouteSource{snapshot: Snapshot{
			Version:  ProtocolVersion,
			Epoch:    9,
			Revision: 1,
			Routes:   []Route{syncTestRoute("photos.relay.sb1a2b3c4.photos.example.com", 2, true)},
		}}
		_, testServer := newSyncTestServer(t, certs, func(config *ServerConfig) {
			config.RouteSource = source
		})
		client := newSyncTestHTTPClient(t, certs.serverCAPEM, certs.gatewayCertPEM, certs.gatewayKeyPEM, testControlIdentity)
		response, err := client.Get(testServer.URL + PathSnapshot)
		if err != nil {
			t.Fatalf("GET snapshot: %v", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusInternalServerError {
			t.Fatalf("invalid source snapshot status = %d, want 500", response.StatusCode)
		}
	})

	t.Run("source snapshot without a control epoch is never emitted", func(t *testing.T) {
		// R2: the epoch is ALWAYS present, even on an empty snapshot — a
		// backend source that omits it produces an invalid payload (500).
		source := &stubRouteSource{snapshot: Snapshot{Version: ProtocolVersion, Revision: 7}}
		_, testServer := newSyncTestServer(t, certs, func(config *ServerConfig) {
			config.RouteSource = source
		})
		client := newSyncTestHTTPClient(t, certs.serverCAPEM, certs.gatewayCertPEM, certs.gatewayKeyPEM, testControlIdentity)
		response, err := client.Get(testServer.URL + PathSnapshot)
		if err != nil {
			t.Fatalf("GET snapshot: %v", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusInternalServerError {
			t.Fatalf("epoch-less source snapshot status = %d, want 500", response.StatusCode)
		}
	})

	t.Run("source delta page with regressing revisions is never emitted", func(t *testing.T) {
		page := DeltaPage{
			Version:        ProtocolVersion,
			Epoch:          9,
			Status:         DeltaStatusOK,
			Since:          4,
			LatestRevision: 5,
			Deltas: []RouteDelta{
				{Revision: 5, Operation: RouteOperationAdd, Route: syncTestRoute("a.relay.sb1a2b3c4.photos.example.com", 5, true)},
				{Revision: 5, Operation: RouteOperationAdd, Route: syncTestRoute("b.relay.sb1a2b3c4.photos.example.com", 5, true)},
			},
		}
		_, testServer := newSyncTestServer(t, certs, func(config *ServerConfig) {
			config.RouteSource = &stubRouteSource{page: page}
		})
		client := newSyncTestHTTPClient(t, certs.serverCAPEM, certs.gatewayCertPEM, certs.gatewayKeyPEM, testControlIdentity)
		response, err := client.Get(testServer.URL + PathDeltas + "?since=4")
		if err != nil {
			t.Fatalf("GET deltas: %v", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusInternalServerError {
			t.Fatalf("invalid source delta page status = %d, want 500", response.StatusCode)
		}
	})

	t.Run("routes beyond the configured bound are refused", func(t *testing.T) {
		routes := []Route{
			syncTestRoute("a.relay.sb1a2b3c4.photos.example.com", 1, true),
			syncTestRoute("b.relay.sb1a2b3c4.photos.example.com", 1, true),
			syncTestRoute("c.relay.sb1a2b3c4.photos.example.com", 1, true),
		}
		_, testServer := newSyncTestServer(t, certs, func(config *ServerConfig) {
			config.RouteSource = &stubRouteSource{snapshot: Snapshot{Version: ProtocolVersion, Epoch: 1, Revision: 1, Routes: routes}}
			config.MaxRoutes = 2
		})
		client := newSyncTestHTTPClient(t, certs.serverCAPEM, certs.gatewayCertPEM, certs.gatewayKeyPEM, testControlIdentity)
		response, err := client.Get(testServer.URL + PathSnapshot)
		if err != nil {
			t.Fatalf("GET snapshot: %v", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusInternalServerError {
			t.Fatalf("over-bound snapshot status = %d, want 500", response.StatusCode)
		}
	})

	t.Run("trailing JSON data on presence events", func(t *testing.T) {
		_, testServer := newSyncTestServer(t, certs, nil)
		client := newSyncTestHTTPClient(t, certs.serverCAPEM, certs.gatewayCertPEM, certs.gatewayKeyPEM, testControlIdentity)
		envelope := PresenceEnvelope{Version: ProtocolVersion, Events: []PresenceEvent{syncTestPresenceEvent("gwboot4k9x2m7qzr15", 9001, PresenceStateOnline)}}
		payload, err := json.Marshal(envelope)
		if err != nil {
			t.Fatalf("marshal envelope: %v", err)
		}
		response := postJSON(t, client, testServer.URL, PathPresenceEvents, append(payload, []byte(` {"version":1}`)...))
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("trailing data status = %d, want 400", response.StatusCode)
		}
	})
}

// --- Task 11 named test 4: explicit acknowledgements of last-applied revision ---

func TestSyncAcknowledgesLastAppliedRevision(t *testing.T) {
	certs := newSyncTestCertificates(t)

	postAck := func(t *testing.T, client *http.Client, baseURL string, ack StatusAck) (StatusAckResponse, int) {
		t.Helper()
		payload, err := json.Marshal(ack)
		if err != nil {
			t.Fatalf("marshal ack: %v", err)
		}
		request, err := http.NewRequest(http.MethodPost, baseURL+PathStatus, bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("build POST: %v", err)
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("POST status: %v", err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatalf("read status body: %v", err)
		}
		var decoded StatusAckResponse
		if response.StatusCode == http.StatusOK {
			decodeStrictJSON(t, body, &decoded)
		}
		return decoded, response.StatusCode
	}

	t.Run("acknowledged revision is recorded, echoed, and forwarded to the sink", func(t *testing.T) {
		sink := &stubStatusSink{}
		server, testServer := newSyncTestServer(t, certs, func(config *ServerConfig) {
			config.StatusSink = sink
		})
		client := newSyncTestHTTPClient(t, certs.serverCAPEM, certs.gatewayCertPEM, certs.gatewayKeyPEM, testControlIdentity)

		decoded, code := postAck(t, client, testServer.URL, StatusAck{Version: ProtocolVersion, GatewayBootID: "gwboot4k9x2m7qzr15", ControlEpoch: 9, LastAppliedRevision: 7})
		if code != http.StatusOK {
			t.Fatalf("ack status = %d, want 200", code)
		}
		if !decoded.Acknowledged || decoded.LastAppliedRevision != 7 {
			t.Fatalf("ack response = %+v, want acknowledged at 7", decoded)
		}
		if server.LastAcknowledgedRevision("gwboot4k9x2m7qzr15") != 7 {
			t.Fatalf("LastAcknowledgedRevision = %d, want 7", server.LastAcknowledgedRevision("gwboot4k9x2m7qzr15"))
		}
		if len(sink.acks) != 1 || sink.acks[0].LastAppliedRevision != 7 || sink.acks[0].GatewayBootID != "gwboot4k9x2m7qzr15" {
			t.Fatalf("sink acks = %+v, want one ack at revision 7", sink.acks)
		}
	})

	t.Run("stale acknowledgement cannot regress the recorded revision", func(t *testing.T) {
		sink := &stubStatusSink{}
		server, testServer := newSyncTestServer(t, certs, func(config *ServerConfig) {
			config.StatusSink = sink
		})
		client := newSyncTestHTTPClient(t, certs.serverCAPEM, certs.gatewayCertPEM, certs.gatewayKeyPEM, testControlIdentity)

		if _, code := postAck(t, client, testServer.URL, StatusAck{Version: ProtocolVersion, GatewayBootID: "gwboot4k9x2m7qzr15", ControlEpoch: 9, LastAppliedRevision: 7}); code != http.StatusOK {
			t.Fatalf("first ack status = %d, want 200", code)
		}
		decoded, code := postAck(t, client, testServer.URL, StatusAck{Version: ProtocolVersion, GatewayBootID: "gwboot4k9x2m7qzr15", ControlEpoch: 9, LastAppliedRevision: 5})
		if code != http.StatusOK {
			t.Fatalf("stale ack status = %d, want idempotent 200", code)
		}
		if decoded.LastAppliedRevision != 7 {
			t.Fatalf("stale ack echoed revision %d, want 7", decoded.LastAppliedRevision)
		}
		if server.LastAcknowledgedRevision("gwboot4k9x2m7qzr15") != 7 {
			t.Fatalf("recorded revision regressed to %d", server.LastAcknowledgedRevision("gwboot4k9x2m7qzr15"))
		}
		if len(sink.acks) != 1 {
			t.Fatalf("stale ack reached the sink %d times, want once", len(sink.acks))
		}
	})

	t.Run("invalid acknowledgement identity is rejected", func(t *testing.T) {
		_, testServer := newSyncTestServer(t, certs, nil)
		client := newSyncTestHTTPClient(t, certs.serverCAPEM, certs.gatewayCertPEM, certs.gatewayKeyPEM, testControlIdentity)
		_, code := postAck(t, client, testServer.URL, StatusAck{Version: ProtocolVersion, GatewayBootID: "", ControlEpoch: 9, LastAppliedRevision: 7})
		if code != http.StatusBadRequest {
			t.Fatalf("empty boot id status = %d, want 400", code)
		}
	})

	t.Run("acknowledgement without a control epoch is rejected", func(t *testing.T) {
		// R2: acks carry the control epoch of the applied state; a missing
		// epoch is a malformed payload (fail closed, never forwarded).
		_, testServer := newSyncTestServer(t, certs, nil)
		client := newSyncTestHTTPClient(t, certs.serverCAPEM, certs.gatewayCertPEM, certs.gatewayKeyPEM, testControlIdentity)
		_, code := postAck(t, client, testServer.URL, StatusAck{Version: ProtocolVersion, GatewayBootID: "gwboot4k9x2m7qzr15", ControlEpoch: 0, LastAppliedRevision: 7})
		if code != http.StatusBadRequest {
			t.Fatalf("missing control epoch status = %d, want 400", code)
		}
	})

	t.Run("sink failure fails closed", func(t *testing.T) {
		_, testServer := newSyncTestServer(t, certs, func(config *ServerConfig) {
			config.StatusSink = &stubStatusSink{err: errors.New("publisher unavailable")}
		})
		client := newSyncTestHTTPClient(t, certs.serverCAPEM, certs.gatewayCertPEM, certs.gatewayKeyPEM, testControlIdentity)
		_, code := postAck(t, client, testServer.URL, StatusAck{Version: ProtocolVersion, GatewayBootID: "gwboot4k9x2m7qzr15", ControlEpoch: 9, LastAppliedRevision: 7})
		if code != http.StatusInternalServerError {
			t.Fatalf("sink failure status = %d, want 500", code)
		}
	})

	t.Run("presence snapshot and events reach the presence sink", func(t *testing.T) {
		sink := &stubPresenceSink{}
		_, testServer := newSyncTestServer(t, certs, func(config *ServerConfig) {
			config.PresenceSink = sink
		})
		client := newSyncTestHTTPClient(t, certs.serverCAPEM, certs.gatewayCertPEM, certs.gatewayKeyPEM, testControlIdentity)

		snapshotEnvelope := PresenceEnvelope{Version: ProtocolVersion, Events: []PresenceEvent{syncTestPresenceEvent("gwboot4k9x2m7qzr15", 100, PresenceStateOnline)}}
		payload, err := json.Marshal(snapshotEnvelope)
		if err != nil {
			t.Fatalf("marshal snapshot envelope: %v", err)
		}
		request, err := http.NewRequest(http.MethodPost, testServer.URL+PathPresenceSnapshot, bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("build POST: %v", err)
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("POST presence snapshot: %v", err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("presence snapshot status = %d, want 200", response.StatusCode)
		}

		eventEnvelope := PresenceEnvelope{Version: ProtocolVersion, Events: []PresenceEvent{
			syncTestPresenceEvent("gwboot4k9x2m7qzr15", 101, PresenceStateOnline),
			syncTestPresenceEvent("gwboot4k9x2m7qzr15", 102, PresenceStateOffline),
		}}
		payload, err = json.Marshal(eventEnvelope)
		if err != nil {
			t.Fatalf("marshal event envelope: %v", err)
		}
		request, err = http.NewRequest(http.MethodPost, testServer.URL+PathPresenceEvents, bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("build POST: %v", err)
		}
		request.Header.Set("Content-Type", "application/json")
		response, err = client.Do(request)
		if err != nil {
			t.Fatalf("POST presence events: %v", err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("presence events status = %d, want 200", response.StatusCode)
		}

		if len(sink.snapshots) != 1 || len(sink.events) != 1 {
			t.Fatalf("sink saw %d snapshots and %d event batches, want 1 and 1", len(sink.snapshots), len(sink.events))
		}
	})

	t.Run("missing backend fails closed with 503", func(t *testing.T) {
		_, testServer := newSyncTestServer(t, certs, func(config *ServerConfig) {
			config.RouteSource = nil
			config.PresenceSink = nil
		})
		client := newSyncTestHTTPClient(t, certs.serverCAPEM, certs.gatewayCertPEM, certs.gatewayKeyPEM, testControlIdentity)

		response, err := client.Get(testServer.URL + PathSnapshot)
		if err != nil {
			t.Fatalf("GET snapshot: %v", err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("nil source snapshot status = %d, want 503", response.StatusCode)
		}

		envelope := PresenceEnvelope{Version: ProtocolVersion, Events: []PresenceEvent{syncTestPresenceEvent("gwboot4k9x2m7qzr15", 100, PresenceStateOnline)}}
		payload, err := json.Marshal(envelope)
		if err != nil {
			t.Fatalf("marshal envelope: %v", err)
		}
		request, err := http.NewRequest(http.MethodPost, testServer.URL+PathPresenceEvents, bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("build POST: %v", err)
		}
		request.Header.Set("Content-Type", "application/json")
		response, err = client.Do(request)
		if err != nil {
			t.Fatalf("POST presence events: %v", err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("nil sink presence status = %d, want 503", response.StatusCode)
		}
	})
}
