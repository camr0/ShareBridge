package controlsync

// Tests for the gateway-side §11.3 controlsync client (plan Task 11): a
// mutually authenticated private-network HTTP client with a pinned control CA
// and exact server SAN, bounded responses, and fail-closed payload validation.
// Golden payloads under ../../../testdata/relay-sync are consumed by both Go
// modules to pin the exact wire bytes on each side.

import (
	"bytes"
	"context"
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

func decodeGoldenJSON(t *testing.T, data []byte, target any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(target); err != nil {
		t.Fatalf("decode golden payload: %v", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatalf("golden payload has trailing JSON data")
	}
}

// --- certificate test helpers (independent of the content PKI; duplicated
// per module because test assets may not be shared across modules) ---

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
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), caCertificate, caKey
}

func issueSyncTestLeaf(t *testing.T, caCertificate *x509.Certificate, caKey *ecdsa.PrivateKey, dnsSAN string, serverAuth bool) ([]byte, []byte) {
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
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

type syncTestCertificates struct {
	controlCAPEM   []byte
	controlCertPEM []byte
	controlKeyPEM  []byte
	gatewayCAPEM   []byte
	gatewayCertPEM []byte
	gatewayKeyPEM  []byte
	foreignCAPEM   []byte
	foreignCertPEM []byte
	foreignKeyPEM  []byte
}

func newSyncTestCertificates(t *testing.T) syncTestCertificates {
	t.Helper()
	var certs syncTestCertificates
	// The control CA issues the control server leaf; gateway clients pin it.
	controlCAPEM, controlCert, controlKey := generateSyncTestCA(t, "sharebridge-sync-control-ca")
	certs.controlCAPEM = controlCAPEM
	certs.controlCertPEM, certs.controlKeyPEM = issueSyncTestLeaf(t, controlCert, controlKey, testControlIdentity, true)

	gatewayCAPEM, gatewayCert, gatewayKey := generateSyncTestCA(t, "sharebridge-sync-gateway-ca")
	certs.gatewayCAPEM = gatewayCAPEM
	certs.gatewayCertPEM, certs.gatewayKeyPEM = issueSyncTestLeaf(t, gatewayCert, gatewayKey, testGatewayIdentity, false)

	foreignCAPEM, foreignCert, foreignKey := generateSyncTestCA(t, "sharebridge-sync-foreign-ca")
	certs.foreignCAPEM = foreignCAPEM
	certs.foreignCertPEM, certs.foreignKeyPEM = issueSyncTestLeaf(t, foreignCert, foreignKey, testGatewayIdentity, false)
	return certs
}

// stubControlServer is the hostile/friendly control fixture: a TLS server the
// client can point at, with per-test handler behavior, requiring the gateway
// client certificate exactly like the production control listener.
type stubControlServer struct {
	t           *testing.T
	certs       syncTestCertificates
	requests    int
	handlerFunc func(t *testing.T, request *http.Request) (status int, body []byte)
	testServer  *httptest.Server
}

func newStubControlServer(t *testing.T, certs syncTestCertificates, handlerFunc func(t *testing.T, request *http.Request) (status int, body []byte)) *stubControlServer {
	t.Helper()
	stub := &stubControlServer{t: t, certs: certs, handlerFunc: handlerFunc}
	serverCertificate, err := tls.X509KeyPair(certs.controlCertPEM, certs.controlKeyPEM)
	if err != nil {
		t.Fatalf("control server key pair: %v", err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(certs.gatewayCAPEM) {
		t.Fatalf("gateway CA PEM rejected")
	}
	testServer := httptest.NewUnstartedServer(http.HandlerFunc(stub.serve))
	testServer.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCertificate},
		ClientCAs:    clientCAs,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}
	testServer.StartTLS()
	t.Cleanup(testServer.Close)
	stub.testServer = testServer
	return stub
}

func (stub *stubControlServer) serve(responseWriter http.ResponseWriter, request *http.Request) {
	stub.requests++
	if len(request.TLS.PeerCertificates) == 0 {
		responseWriter.WriteHeader(http.StatusForbidden)
		return
	}
	status, body := stub.handlerFunc(stub.t, request)
	responseWriter.Header().Set("Content-Type", "application/json")
	responseWriter.WriteHeader(status)
	_, _ = responseWriter.Write(body)
}

func (stub *stubControlServer) newTestClient(t *testing.T, mutate func(*ClientConfig)) *Client {
	t.Helper()
	config := ClientConfig{
		BaseURL:           stub.testServer.URL,
		ExpectedServerSAN: testControlIdentity,
		ServerCAPEM:       stub.certs.controlCAPEM,
		ClientCertPEM:     stub.certs.gatewayCertPEM,
		ClientKeyPEM:      stub.certs.gatewayKeyPEM,
	}
	if mutate != nil {
		mutate(&config)
	}
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
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
		decodeGoldenJSON(t, golden, &decoded)

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
			PublishedAt: "2026-09-03T17:55:00Z",
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
		decodeGoldenJSON(t, golden, &decoded)

		reEncoded, err := json.Marshal(decoded)
		if err != nil {
			t.Fatalf("marshal decoded delta page: %v", err)
		}
		if !bytes.Equal(reEncoded, golden) {
			t.Fatalf("decoded delta page does not re-encode to the golden bytes")
		}
		if err := ValidateDeltaPage(decoded, MaxDeltasPerPage); err != nil {
			t.Fatalf("golden delta page rejected: %v", err)
		}
	})

	t.Run("presence", func(t *testing.T) {
		golden := readGoldenPayload(t, "presence.json")
		var decoded PresenceEnvelope
		decodeGoldenJSON(t, golden, &decoded)

		reEncoded, err := json.Marshal(decoded)
		if err != nil {
			t.Fatalf("marshal decoded presence envelope: %v", err)
		}
		if !bytes.Equal(reEncoded, golden) {
			t.Fatalf("decoded presence envelope does not re-encode to the golden bytes")
		}
		if err := ValidatePresenceEventEnvelope(decoded, MaxPresenceEventsPerEnvelope); err != nil {
			t.Fatalf("golden presence envelope rejected: %v", err)
		}
	})

	t.Run("empty boot snapshot carries its boot and revision", func(t *testing.T) {
		golden := readGoldenPayload(t, "presence-snapshot-empty.json")
		var decoded PresenceEnvelope
		decodeGoldenJSON(t, golden, &decoded)

		reEncoded, err := json.Marshal(decoded)
		if err != nil {
			t.Fatalf("marshal decoded empty snapshot: %v", err)
		}
		if !bytes.Equal(reEncoded, golden) {
			t.Fatalf("decoded empty snapshot does not re-encode to the golden bytes")
		}
		if err := ValidatePresenceSnapshotEnvelope(decoded, MaxPresenceEventsPerEnvelope); err != nil {
			t.Fatalf("golden empty snapshot rejected: %v", err)
		}
	})
}

// --- Task 11 named test 2: mutual TLS is mandatory ---

func TestSyncRequiresMutualTLS(t *testing.T) {
	certs := newSyncTestCertificates(t)
	snapshotPayload, err := json.Marshal(Snapshot{Version: ProtocolVersion, Epoch: 7, Revision: 7, Routes: []Route{syncTestRoute("photos.relay.sb1a2b3c4.photos.example.com", 7, true)}})
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}

	t.Run("pinned CA and exact server SAN succeed", func(t *testing.T) {
		stub := newStubControlServer(t, certs, func(t *testing.T, request *http.Request) (int, []byte) {
			if request.URL.Path != PathSnapshot {
				t.Errorf("unexpected path %q", request.URL.Path)
			}
			return http.StatusOK, snapshotPayload
		})
		client := stub.newTestClient(t, nil)
		snapshot, err := client.FetchSnapshot(context.Background())
		if err != nil {
			t.Fatalf("FetchSnapshot: %v", err)
		}
		if snapshot.Revision != 7 || len(snapshot.Routes) != 1 {
			t.Fatalf("unexpected snapshot %+v", snapshot)
		}
	})

	t.Run("wrong pinned CA fails closed", func(t *testing.T) {
		stub := newStubControlServer(t, certs, func(t *testing.T, request *http.Request) (int, []byte) {
			return http.StatusOK, snapshotPayload
		})
		client := stub.newTestClient(t, func(config *ClientConfig) {
			config.ServerCAPEM = certs.foreignCAPEM
		})
		if _, err := client.FetchSnapshot(context.Background()); err == nil {
			t.Fatalf("client accepted a server certificate outside the pinned CA")
		}
	})

	t.Run("wrong expected server SAN fails closed", func(t *testing.T) {
		stub := newStubControlServer(t, certs, func(t *testing.T, request *http.Request) (int, []byte) {
			return http.StatusOK, snapshotPayload
		})
		client := stub.newTestClient(t, func(config *ClientConfig) {
			config.ExpectedServerSAN = "sharebridge-control.impersonator.invalid"
		})
		if _, err := client.FetchSnapshot(context.Background()); err == nil {
			t.Fatalf("client accepted a server with the wrong SAN")
		}
	})

	t.Run("gateway client certificate is always presented", func(t *testing.T) {
		// The stub requires and verifies a client certificate against the
		// gateway CA; a successful fetch proves the client presented one.
		stub := newStubControlServer(t, certs, func(t *testing.T, request *http.Request) (int, []byte) {
			return http.StatusOK, snapshotPayload
		})
		client := stub.newTestClient(t, nil)
		if _, err := client.FetchSnapshot(context.Background()); err != nil {
			t.Fatalf("FetchSnapshot with client certificate: %v", err)
		}
		if stub.requests != 1 {
			t.Fatalf("stub saw %d requests, want 1", stub.requests)
		}
	})

	t.Run("http base URL is rejected at configuration", func(t *testing.T) {
		if _, err := NewClient(ClientConfig{
			BaseURL:           "http://control.internal:9443",
			ExpectedServerSAN: testControlIdentity,
			ServerCAPEM:       certs.controlCAPEM,
			ClientCertPEM:     certs.gatewayCertPEM,
			ClientKeyPEM:      certs.gatewayKeyPEM,
		}); err == nil {
			t.Fatalf("plain http base URL accepted")
		}
	})
}

// --- Task 11 named test 3: oversize, unknown version, bad revision ---

func TestSyncRejectsOversizeUnknownVersionAndBadRevision(t *testing.T) {
	certs := newSyncTestCertificates(t)
	validRoute := syncTestRoute("photos.relay.sb1a2b3c4.photos.example.com", 9, true)

	t.Run("oversize response body", func(t *testing.T) {
		stub := newStubControlServer(t, certs, func(t *testing.T, request *http.Request) (int, []byte) {
			return http.StatusOK, bytes.Repeat([]byte("a"), 4096)
		})
		client := stub.newTestClient(t, func(config *ClientConfig) {
			config.MaxResponseBytes = 1024
		})
		if _, err := client.FetchSnapshot(context.Background()); !errors.Is(err, ErrOversize) {
			t.Fatalf("oversize response error = %v, want ErrOversize", err)
		}
	})

	t.Run("unknown version in snapshot response", func(t *testing.T) {
		payload, err := json.Marshal(Snapshot{Version: ProtocolVersion + 1, Epoch: 9, Revision: 9, Routes: []Route{validRoute}})
		if err != nil {
			t.Fatalf("marshal snapshot: %v", err)
		}
		stub := newStubControlServer(t, certs, func(t *testing.T, request *http.Request) (int, []byte) {
			return http.StatusOK, payload
		})
		client := stub.newTestClient(t, nil)
		if _, err := client.FetchSnapshot(context.Background()); !errors.Is(err, ErrUnsupportedVersion) {
			t.Fatalf("unknown version error = %v, want ErrUnsupportedVersion", err)
		}
	})

	t.Run("bad revision: snapshot revision below a route revision", func(t *testing.T) {
		payload, err := json.Marshal(Snapshot{
			Version:  ProtocolVersion,
			Epoch:    9,
			Revision: 5,
			Routes:   []Route{syncTestRoute("photos.relay.sb1a2b3c4.photos.example.com", 6, true)},
		})
		if err != nil {
			t.Fatalf("marshal snapshot: %v", err)
		}
		stub := newStubControlServer(t, certs, func(t *testing.T, request *http.Request) (int, []byte) {
			return http.StatusOK, payload
		})
		client := stub.newTestClient(t, nil)
		if _, err := client.FetchSnapshot(context.Background()); !errors.Is(err, ErrBadRevision) {
			t.Fatalf("bad snapshot revision error = %v, want ErrBadRevision", err)
		}
	})

	t.Run("bad revision: regressing delta revisions", func(t *testing.T) {
		page := DeltaPage{
			Version:        ProtocolVersion,
			Epoch:          9,
			Status:         DeltaStatusOK,
			Since:          4,
			LatestRevision: 6,
			Deltas: []RouteDelta{
				{Revision: 6, Operation: RouteOperationAdd, Route: syncTestRoute("photos.relay.sb1a2b3c4.photos.example.com", 6, true)},
				{Revision: 6, Operation: RouteOperationRevoke, Route: syncTestRoute("photos.relay.sb1a2b3c4.photos.example.com", 6, false)},
			},
		}
		payload, err := json.Marshal(page)
		if err != nil {
			t.Fatalf("marshal page: %v", err)
		}
		stub := newStubControlServer(t, certs, func(t *testing.T, request *http.Request) (int, []byte) {
			return http.StatusOK, payload
		})
		client := stub.newTestClient(t, nil)
		if _, err := client.FetchDeltas(context.Background(), 4); !errors.Is(err, ErrBadRevision) {
			t.Fatalf("regressing deltas error = %v, want ErrBadRevision", err)
		}
	})

	t.Run("bad revision: page does not answer the requested since", func(t *testing.T) {
		page := DeltaPage{
			Version:        ProtocolVersion,
			Epoch:          9,
			Status:         DeltaStatusOK,
			Since:          3,
			LatestRevision: 5,
			Deltas:         []RouteDelta{{Revision: 5, Operation: RouteOperationAdd, Route: validRoute}},
		}
		payload, err := json.Marshal(page)
		if err != nil {
			t.Fatalf("marshal page: %v", err)
		}
		stub := newStubControlServer(t, certs, func(t *testing.T, request *http.Request) (int, []byte) {
			if request.URL.RawQuery != "since=4" {
				t.Errorf("deltas query = %q, want since=4", request.URL.RawQuery)
			}
			return http.StatusOK, payload
		})
		client := stub.newTestClient(t, nil)
		if _, err := client.FetchDeltas(context.Background(), 4); !errors.Is(err, ErrBadRevision) {
			t.Fatalf("wrong page since error = %v, want ErrBadRevision", err)
		}
	})

	t.Run("bad revision: gap page carrying deltas", func(t *testing.T) {
		page := DeltaPage{
			Version:        ProtocolVersion,
			Epoch:          9,
			Status:         DeltaStatusGap,
			Since:          4,
			LatestRevision: 40,
			Deltas:         []RouteDelta{{Revision: 40, Operation: RouteOperationAdd, Route: validRoute}},
		}
		payload, err := json.Marshal(page)
		if err != nil {
			t.Fatalf("marshal page: %v", err)
		}
		stub := newStubControlServer(t, certs, func(t *testing.T, request *http.Request) (int, []byte) {
			return http.StatusOK, payload
		})
		client := stub.newTestClient(t, nil)
		if _, err := client.FetchDeltas(context.Background(), 4); !errors.Is(err, ErrBadRevision) {
			t.Fatalf("gap page with deltas error = %v, want ErrBadRevision", err)
		}
	})

	t.Run("bad revision: valid gap page forces snapshot reconciliation", func(t *testing.T) {
		// A well-formed gap page (no deltas, latest strictly ahead) is served
		// 200 by control but must reach the Task 13 applier as ErrBadRevision —
		// never as an empty success at a stale since (§11.3, §15.7).
		gap := DeltaPage{Version: ProtocolVersion, Epoch: 9, Status: DeltaStatusGap, Since: 4, LatestRevision: 40}
		payload, err := json.Marshal(gap)
		if err != nil {
			t.Fatalf("marshal gap page: %v", err)
		}
		stub := newStubControlServer(t, certs, func(t *testing.T, request *http.Request) (int, []byte) {
			if request.URL.RawQuery != "since=4" {
				t.Errorf("deltas query = %q, want since=4", request.URL.RawQuery)
			}
			return http.StatusOK, payload
		})
		client := stub.newTestClient(t, nil)
		page, err := client.FetchDeltas(context.Background(), 4)
		if !errors.Is(err, ErrBadRevision) {
			t.Fatalf("valid gap page error = %v, want ErrBadRevision", err)
		}
		if page.Status != "" || page.Since != 0 || page.LatestRevision != 0 || page.Deltas != nil {
			t.Fatalf("gap page returned a payload, want zero value: %+v", page)
		}
	})

	t.Run("missing control epoch refuses snapshots and delta pages", func(t *testing.T) {
		// R2: the control epoch is ALWAYS present on served payloads (even
		// empty ones) — a payload without one is malformed and must fail
		// closed at the client before the applier could ever trust it.
		client := newStubControlServer(t, certs, func(t *testing.T, request *http.Request) (int, []byte) {
			switch request.URL.Path {
			case PathSnapshot:
				return http.StatusOK, []byte(`{"version":1,"epoch":0,"revision":9,"routes":[]}`)
			case PathDeltas:
				return http.StatusOK, []byte(`{"version":1,"epoch":0,"status":"ok","since":4,"latest_revision":4,"deltas":[]}`)
			default:
				t.Errorf("unexpected path %q", request.URL.Path)
				return http.StatusNotFound, nil
			}
		}).newTestClient(t, nil)
		if _, err := client.FetchSnapshot(context.Background()); !errors.Is(err, ErrInvalidPayload) {
			t.Fatalf("epoch-less snapshot error = %v, want %v", err, ErrInvalidPayload)
		}
		if _, err := client.FetchDeltas(context.Background(), 4); !errors.Is(err, ErrInvalidPayload) {
			t.Fatalf("epoch-less delta page error = %v, want %v", err, ErrInvalidPayload)
		}
	})

	t.Run("oversize POST body is refused before sending", func(t *testing.T) {
		stub := newStubControlServer(t, certs, func(t *testing.T, request *http.Request) (int, []byte) {
			t.Errorf("oversize envelope must not reach the wire")
			return http.StatusOK, nil
		})
		client := stub.newTestClient(t, func(config *ClientConfig) {
			config.MaxRequestBytes = 16
		})
		envelope := PresenceEnvelope{Version: ProtocolVersion, Events: []PresenceEvent{
			syncTestPresenceEvent("gwboot4k9x2m7qzr15", 1, PresenceStateOnline),
			syncTestPresenceEvent("gwboot4k9x2m7qzr15", 2, PresenceStateOnline),
		}}
		if err := client.PublishPresenceEvents(context.Background(), envelope); !errors.Is(err, ErrOversize) {
			t.Fatalf("oversize envelope error = %v, want ErrOversize", err)
		}
		if stub.requests != 0 {
			t.Fatalf("oversize envelope produced %d requests, want 0", stub.requests)
		}
	})

	t.Run("invalid outbound payloads never reach the wire", func(t *testing.T) {
		stub := newStubControlServer(t, certs, func(t *testing.T, request *http.Request) (int, []byte) {
			t.Errorf("invalid payload must not reach the wire")
			return http.StatusOK, nil
		})
		client := stub.newTestClient(t, nil)

		badState := PresenceEnvelope{Version: ProtocolVersion, Events: []PresenceEvent{syncTestPresenceEvent("gwboot4k9x2m7qzr15", 1, "teleporting")}}
		if err := client.PublishPresenceEvents(context.Background(), badState); !errors.Is(err, ErrInvalidPayload) {
			t.Fatalf("invalid state error = %v, want ErrInvalidPayload", err)
		}
		wrongVersion := PresenceEnvelope{Version: ProtocolVersion + 1, Events: []PresenceEvent{syncTestPresenceEvent("gwboot4k9x2m7qzr15", 1, PresenceStateOnline)}}
		if err := client.PublishPresenceSnapshot(context.Background(), wrongVersion); !errors.Is(err, ErrUnsupportedVersion) {
			t.Fatalf("wrong version error = %v, want ErrUnsupportedVersion", err)
		}
		if _, err := client.SendStatus(context.Background(), StatusAck{Version: ProtocolVersion, GatewayBootID: "", LastAppliedRevision: 3}); !errors.Is(err, ErrInvalidPayload) {
			t.Fatalf("empty boot id error = %v, want ErrInvalidPayload", err)
		}
		if _, err := client.SendStatus(context.Background(), StatusAck{Version: ProtocolVersion, GatewayBootID: "gwboot4k9x2m7qzr15", ControlEpoch: 0, LastAppliedRevision: 3}); !errors.Is(err, ErrInvalidPayload) {
			t.Fatalf("missing control epoch error = %v, want ErrInvalidPayload (R2: acks carry the epoch)", err)
		}
		if stub.requests != 0 {
			t.Fatalf("invalid payloads produced %d requests, want 0", stub.requests)
		}
	})

	t.Run("non-JSON and trailing-data responses are refused", func(t *testing.T) {
		stub := newStubControlServer(t, certs, func(t *testing.T, request *http.Request) (int, []byte) {
			return http.StatusOK, []byte("not json at all")
		})
		client := stub.newTestClient(t, nil)
		if _, err := client.FetchSnapshot(context.Background()); !errors.Is(err, ErrInvalidPayload) {
			t.Fatalf("non-JSON response error = %v, want ErrInvalidPayload", err)
		}

		payload, err := json.Marshal(Snapshot{Version: ProtocolVersion, Epoch: 9, Revision: 9, Routes: []Route{validRoute}})
		if err != nil {
			t.Fatalf("marshal snapshot: %v", err)
		}
		trailing := newStubControlServer(t, certs, func(t *testing.T, request *http.Request) (int, []byte) {
			return http.StatusOK, append(payload, []byte(" {}")...)
		})
		trailingClient := trailing.newTestClient(t, nil)
		if _, err := trailingClient.FetchSnapshot(context.Background()); !errors.Is(err, ErrInvalidPayload) {
			t.Fatalf("trailing data error = %v, want ErrInvalidPayload", err)
		}
	})

	t.Run("forbidden and error statuses are classified", func(t *testing.T) {
		forbidden := newStubControlServer(t, certs, func(t *testing.T, request *http.Request) (int, []byte) {
			return http.StatusForbidden, []byte(`{"error":"forbidden"}`)
		})
		forbiddenClient := forbidden.newTestClient(t, nil)
		if _, err := forbiddenClient.FetchSnapshot(context.Background()); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("forbidden error = %v, want ErrUnauthorized", err)
		}

		broken := newStubControlServer(t, certs, func(t *testing.T, request *http.Request) (int, []byte) {
			return http.StatusInternalServerError, []byte(`{"error":"boom"}`)
		})
		brokenClient := broken.newTestClient(t, nil)
		if _, err := brokenClient.FetchSnapshot(context.Background()); err == nil || errors.Is(err, ErrUnauthorized) {
			t.Fatalf("server error = %v, want a non-nil transport error", err)
		}
	})
}

// --- Task 11 named test 4: explicit acknowledgements of last-applied revision ---

func TestSyncAcknowledgesLastAppliedRevision(t *testing.T) {
	certs := newSyncTestCertificates(t)

	t.Run("acknowledgement reaches control with the exact payload", func(t *testing.T) {
		var received map[string]any
		stub := newStubControlServer(t, certs, func(t *testing.T, request *http.Request) (int, []byte) {
			if request.URL.Path != PathStatus {
				t.Errorf("unexpected path %q", request.URL.Path)
			}
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("read ack body: %v", err)
			}
			received = map[string]any{}
			if err := json.Unmarshal(body, &received); err != nil {
				t.Errorf("decode ack body: %v", err)
			}
			response, err := json.Marshal(StatusAckResponse{Version: ProtocolVersion, Acknowledged: true, LastAppliedRevision: 11})
			if err != nil {
				t.Errorf("marshal ack response: %v", err)
			}
			return http.StatusOK, response
		})
		client := stub.newTestClient(t, nil)

		response, err := client.SendStatus(context.Background(), StatusAck{Version: ProtocolVersion, GatewayBootID: "gwboot4k9x2m7qzr15", ControlEpoch: 5, LastAppliedRevision: 11})
		if err != nil {
			t.Fatalf("SendStatus: %v", err)
		}
		if !response.Acknowledged || response.LastAppliedRevision != 11 {
			t.Fatalf("ack response = %+v, want acknowledged at 11", response)
		}
		if received["gateway_boot_id"] != "gwboot4k9x2m7qzr15" || received["last_applied_revision"] != float64(11) {
			t.Fatalf("control received %v", received)
		}
		if received["control_epoch"] != float64(5) {
			t.Fatalf("control received control_epoch %v, want 5 (R2: acks carry the applied epoch)", received["control_epoch"])
		}
	})

	t.Run("presence snapshot and events publish successfully", func(t *testing.T) {
		var snapshotEvents, eventEvents int
		stub := newStubControlServer(t, certs, func(t *testing.T, request *http.Request) (int, []byte) {
			switch request.URL.Path {
			case PathPresenceSnapshot:
				snapshotEvents++
			case PathPresenceEvents:
				eventEvents++
			default:
				t.Errorf("unexpected path %q", request.URL.Path)
			}
			receipt, err := json.Marshal(SyncReceipt{Version: ProtocolVersion, Accepted: true})
			if err != nil {
				t.Errorf("marshal receipt: %v", err)
			}
			return http.StatusOK, receipt
		})
		client := stub.newTestClient(t, nil)

		snapshotEnvelope := PresenceEnvelope{Version: ProtocolVersion, Events: []PresenceEvent{syncTestPresenceEvent("gwboot4k9x2m7qzr15", 100, PresenceStateOnline)}}
		if err := client.PublishPresenceSnapshot(context.Background(), snapshotEnvelope); err != nil {
			t.Fatalf("PublishPresenceSnapshot: %v", err)
		}
		eventEnvelope := PresenceEnvelope{Version: ProtocolVersion, Events: []PresenceEvent{
			syncTestPresenceEvent("gwboot4k9x2m7qzr15", 101, PresenceStateOnline),
			syncTestPresenceEvent("gwboot4k9x2m7qzr15", 102, PresenceStateOffline),
		}}
		if err := client.PublishPresenceEvents(context.Background(), eventEnvelope); err != nil {
			t.Fatalf("PublishPresenceEvents: %v", err)
		}
		if snapshotEvents != 1 || eventEvents != 1 {
			t.Fatalf("stub saw snapshot=%d events=%d, want 1 and 1", snapshotEvents, eventEvents)
		}
	})

	t.Run("stale-acknowledgement guard stays control-side", func(t *testing.T) {
		// The gateway reports exactly what it applied; the no-regression guard
		// is enforced by control (Task 12 publisher). The client must send the
		// reported revision unchanged rather than guessing.
		var sentRevision any
		stub := newStubControlServer(t, certs, func(t *testing.T, request *http.Request) (int, []byte) {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("read ack body: %v", err)
			}
			var ack StatusAck
			if err := json.Unmarshal(body, &ack); err != nil {
				t.Errorf("decode ack: %v", err)
			}
			sentRevision = ack.LastAppliedRevision
			response, err := json.Marshal(StatusAckResponse{Version: ProtocolVersion, Acknowledged: true, LastAppliedRevision: 20})
			if err != nil {
				t.Errorf("marshal ack response: %v", err)
			}
			return http.StatusOK, response
		})
		client := stub.newTestClient(t, nil)

		response, err := client.SendStatus(context.Background(), StatusAck{Version: ProtocolVersion, GatewayBootID: "gwboot4k9x2m7qzr15", ControlEpoch: 5, LastAppliedRevision: 5})
		if err != nil {
			t.Fatalf("SendStatus: %v", err)
		}
		if sentRevision != uint64(5) {
			t.Fatalf("client rewrote the applied revision to %v", sentRevision)
		}
		if response.LastAppliedRevision != 20 {
			t.Fatalf("control-side guard response lost: %+v", response)
		}
	})
}

// TestValidateDeltaPageZeroLimitAgreement pins the gateway half of the
// zero-semantics agreement with control: an active route (add/limit) carrying
// a zero ceiling is a protocol violation, because the limits layer would read
// zero as "unset" and restore the process default — turning a control
// "tightening" into a widening. A revoke tombstone still carries zeroed
// limits and must stay valid.
func TestValidateDeltaPageZeroLimitAgreement(t *testing.T) {
	page := func(operation string, active bool, limits Limits) DeltaPage {
		return DeltaPage{
			Version:        ProtocolVersion,
			Epoch:          7777,
			Status:         DeltaStatusOK,
			Since:          41,
			LatestRevision: 42,
			Deltas: []RouteDelta{{
				Revision:  42,
				Operation: operation,
				Route: Route{
					Hostname:      "docs.relay.sb5t4g3h2.docs.example.com",
					AgentRecordID: "agtw8hks2m4qp01",
					RelayPort:     10000,
					Generation:    3,
					SessionID:     "sessp8lkr3tvq52",
					Revision:      42,
					Active:        active,
					Limits:        limits,
				},
			}},
		}
	}

	for _, operation := range []string{RouteOperationAdd, RouteOperationLimit} {
		err := ValidateDeltaPage(page(operation, true, Limits{}), MaxDeltasPerPage)
		if !errors.Is(err, ErrInvalidPayload) {
			t.Fatalf("ValidateDeltaPage(%s with zero limits) error = %v, want %v", operation, err, ErrInvalidPayload)
		}
	}
	if err := ValidateDeltaPage(page(RouteOperationRevoke, false, Limits{}), MaxDeltasPerPage); err != nil {
		t.Fatalf("ValidateDeltaPage(revoke with zeroed limits) = %v, want nil", err)
	}
	positive := Limits{MaxStreamsPerOrigin: 16, MaxStreamsPerAgent: 32, MaxStreamsGlobal: 8192}
	if err := ValidateDeltaPage(page(RouteOperationLimit, true, positive), MaxDeltasPerPage); err != nil {
		t.Fatalf("ValidateDeltaPage(active positive limits) = %v, want nil", err)
	}
}
