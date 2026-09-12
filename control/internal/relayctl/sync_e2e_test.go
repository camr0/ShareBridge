package relayctl

// End-to-end proof for task #16 (ledger I4-partial): the wired §11.3 sync
// listener serves an authenticated /status that advances the REAL publisher's
// acknowledgement watermark, and the presence view it feeds stays available
// across a lease renewal because the gateway republishes the renewed lease.
//
// The client half is a faithful mTLS client built exactly like the gateway's
// controlsync client (TLS 1.3, pinned server CA + exact server SAN, presented
// gateway leaf); the server half, the route publisher, and the presence view
// are the real production types. The relay module's own client/applier tests
// cover the other half of the wire; the two modules cannot import each other,
// so this is the strongest in-process cross-module-shaped proof available (see
// the task report for the exact limits).

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

func postPresence(t *testing.T, client *http.Client, url string, envelope PresenceEnvelope) SyncReceipt {
	t.Helper()
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal presence envelope: %v", err)
	}
	response, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST presence: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(response.Body)
		t.Fatalf("POST presence status = %d body %s, want 200", response.StatusCode, payload)
	}
	var receipt SyncReceipt
	decodeStrictJSON(t, mustReadAll(t, response.Body), &receipt)
	if !receipt.Accepted {
		t.Fatalf("presence receipt = %+v, want accepted", receipt)
	}
	return receipt
}

func postStatusAck(t *testing.T, client *http.Client, url string, ack StatusAck) StatusAckResponse {
	t.Helper()
	body, err := json.Marshal(ack)
	if err != nil {
		t.Fatalf("marshal status ack: %v", err)
	}
	response, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST status: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(response.Body)
		t.Fatalf("POST status status = %d body %s, want 200", response.StatusCode, payload)
	}
	var decoded StatusAckResponse
	decodeStrictJSON(t, mustReadAll(t, response.Body), &decoded)
	return decoded
}

func mustReadAll(t *testing.T, reader io.Reader) []byte {
	t.Helper()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return data
}

func TestSyncListenerEndToEndDrivesAckAndPresenceRenewal(t *testing.T) {
	app := newPublisherTestApp(t)
	apiKeyID, agentID := createPublisherAgent(t, app, "t16-e2e", "sb0a1b2c3d", 10001, 3)
	sessionID := createPublisherSession(t, app, apiKeyID, "T16E2E01", "photos.sb0a1b2c3d.example.com", nil)

	const (
		publisherEpoch = uint64(7777)
		bootID         = "gw-e2e-boot-1"
		freshBootID    = "gw-e2e-boot-2"
		routeRevision  = uint64(101)
	)
	publisher := newTestPublisher(t, app, PublisherConfig{RevisionSeed: 100, Epoch: publisherEpoch})
	if err := publisher.PublishAdd(sessionID); err != nil {
		t.Fatalf("PublishAdd: %v", err)
	}
	if got := publisher.CurrentRevision(); got != routeRevision {
		t.Fatalf("publisher revision = %d, want %d", got, routeRevision)
	}

	clock := newFakeClock(presenceViewBase)
	view, err := NewPresenceView(PresenceViewConfig{App: app, Routes: publisher, Now: clock.Now})
	if err != nil {
		t.Fatalf("NewPresenceView: %v", err)
	}

	certs := newSyncTestCertificates(t)
	_, testServer := newSyncTestServer(t, certs, func(config *ServerConfig) {
		config.RouteSource = publisher
		config.StatusSink = publisher
		config.PresenceSink = view
	})
	client := newSyncTestHTTPClient(t, certs.serverCAPEM, certs.gatewayCertPEM, certs.gatewayKeyPEM, testControlIdentity)

	t.Run("authenticated snapshot fetch serves the real publisher", func(t *testing.T) {
		response, err := client.Get(testServer.URL + PathSnapshot)
		if err != nil {
			t.Fatalf("GET snapshot: %v", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("snapshot status = %d, want 200", response.StatusCode)
		}
		var snapshot Snapshot
		decodeStrictJSON(t, mustReadAll(t, response.Body), &snapshot)
		if snapshot.Epoch != publisherEpoch || snapshot.Revision != routeRevision {
			t.Fatalf("snapshot epoch/revision = %d/%d, want %d/%d", snapshot.Epoch, snapshot.Revision, publisherEpoch, routeRevision)
		}
		if len(snapshot.Routes) != 1 || snapshot.Routes[0].AgentRecordID != agentID {
			t.Fatalf("snapshot routes = %+v, want the published route for %s", snapshot.Routes, agentID)
		}
	})

	t.Run("status ack advances the publisher watermark", func(t *testing.T) {
		if publisher.Healthy() {
			t.Fatal("publisher healthy before any acknowledgement")
		}
		decoded := postStatusAck(t, client, testServer.URL+PathStatus, StatusAck{
			Version:             ProtocolVersion,
			GatewayBootID:       bootID,
			ControlEpoch:        publisher.Epoch(),
			LastAppliedRevision: routeRevision,
		})
		if !decoded.Acknowledged {
			t.Fatalf("status ack response = %+v, want acknowledged", decoded)
		}
		if !publisher.Healthy() {
			t.Fatal("publisher still unhealthy after the accepted acknowledgement")
		}
		if got := publisher.AcknowledgedRevision(); got != routeRevision {
			t.Fatalf("acknowledged revision = %d, want %d", got, routeRevision)
		}
	})

	t.Run("presence snapshot grants availability at the acknowledged revision", func(t *testing.T) {
		lease := clock.Now().Add(45 * time.Second)
		postPresence(t, client, testServer.URL+PathPresenceSnapshot, PresenceEnvelope{
			Version:       ProtocolVersion,
			GatewayBootID: bootID,
			Revision:      1,
			Events: []PresenceEvent{{
				GatewayBootID:  bootID,
				Revision:       1,
				AgentRecordID:  agentID,
				RelayPort:      10001,
				Generation:     3,
				State:          PresenceStateOnline,
				LeaseExpiresAt: lease.Format(time.RFC3339),
			}},
		})
		if !view.Available(agentID, 10001, 3, publisher.AcknowledgedRevision(), clock.Now()) {
			t.Fatal("relay not available after the online snapshot")
		}
	})

	t.Run("republished renewed lease keeps presence alive past the original expiry", func(t *testing.T) {
		// Move past the original 45-second lease: without a republish control
		// correctly expires the entry.
		clock.Advance(46 * time.Second)
		if view.Available(agentID, 10001, 3, publisher.AcknowledgedRevision(), clock.Now()) {
			t.Fatal("presence still available after the original lease expired")
		}
		// The gateway republishes the full state with the renewed lease (what
		// its periodic presence transport does); availability returns.
		renewed := clock.Now().Add(45 * time.Second)
		postPresence(t, client, testServer.URL+PathPresenceSnapshot, PresenceEnvelope{
			Version:       ProtocolVersion,
			GatewayBootID: bootID,
			Revision:      2,
			Events: []PresenceEvent{{
				GatewayBootID:  bootID,
				Revision:       2,
				AgentRecordID:  agentID,
				RelayPort:      10001,
				Generation:     3,
				State:          PresenceStateOnline,
				LeaseExpiresAt: renewed.Format(time.RFC3339),
			}},
		})
		if !view.Available(agentID, 10001, 3, publisher.AcknowledgedRevision(), clock.Now()) {
			t.Fatal("renewed presence snapshot did not restore availability")
		}
	})

	t.Run("empty snapshot adopts a fresh gateway boot over the real wire", func(t *testing.T) {
		// A restarted gateway posts its empty boot snapshot: it must carry the
		// top-level boot identity so control records the new boot.
		postPresence(t, client, testServer.URL+PathPresenceSnapshot, PresenceEnvelope{
			Version:       ProtocolVersion,
			GatewayBootID: freshBootID,
			Revision:      0,
			Events:        []PresenceEvent{},
		})
		if view.Available(agentID, 10001, 3, publisher.AcknowledgedRevision(), clock.Now()) {
			t.Fatal("empty boot snapshot did not clear the previous boot's lease")
		}
		// The new boot's first confirmed tunnel emits an ordered event batch;
		// it must apply (before this fix the boot was unsnapshotted and the
		// batch was discarded).
		lease := clock.Now().Add(45 * time.Second)
		postPresence(t, client, testServer.URL+PathPresenceEvents, PresenceEnvelope{
			Version: ProtocolVersion,
			Events: []PresenceEvent{{
				GatewayBootID:  freshBootID,
				Revision:       1,
				AgentRecordID:  agentID,
				RelayPort:      10001,
				Generation:     3,
				State:          PresenceStateOnline,
				LeaseExpiresAt: lease.Format(time.RFC3339),
			}},
		})
		if !view.Available(agentID, 10001, 3, publisher.AcknowledgedRevision(), clock.Now()) {
			t.Fatal("fresh boot's first event was not adopted")
		}
	})

	t.Run("mtls is mandatory and identity-pinned", func(t *testing.T) {
		// Untrusted client CA: the TLS handshake itself fails.
		foreign := newSyncTestHTTPClient(t, certs.serverCAPEM, certs.foreignCertPEM, certs.foreignKeyPEM, testControlIdentity)
		if _, err := foreign.Post(testServer.URL+PathStatus, "application/json", bytes.NewReader([]byte(`{}`))); err == nil {
			t.Fatal("foreign client CA completed the mTLS handshake")
		}
		// Trusted CA, wrong SAN: TLS succeeds, the application-layer identity
		// check rejects (403).
		wrongSAN := newSyncTestHTTPClient(t, certs.serverCAPEM, certs.wrongSANCertPEM, certs.wrongSANKeyPEM, testControlIdentity)
		response, err := wrongSAN.Post(testServer.URL+PathStatus, "application/json", bytes.NewReader([]byte(`{}`)))
		if err != nil {
			t.Fatalf("wrong-SAN POST: %v", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("wrong-SAN status = %d, want 403", response.StatusCode)
		}
	})
}
