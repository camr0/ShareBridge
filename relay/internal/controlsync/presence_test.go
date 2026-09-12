package controlsync

// Tests for the task #16 presence transport: the gateway-side loop that
// republishes the authoritative presence state to control over the §11.3 mTLS
// channel, carrying the reporting boot identity at the top level so an empty
// boot snapshot is adoptable and a renewed lease never silently expires.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

// presenceRecorder records the presence snapshots a stub control received.
type presenceRecorder struct {
	mu     sync.Mutex
	paths  []string
	bodies [][]byte
}

func (recorder *presenceRecorder) record(path string, body []byte) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.paths = append(recorder.paths, path)
	recorder.bodies = append(recorder.bodies, append([]byte(nil), body...))
}

func (recorder *presenceRecorder) snapshot() ([]string, [][]byte) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]string(nil), recorder.paths...), append([][]byte(nil), recorder.bodies...)
}

// scriptedPresenceSource is the gateway's authoritative state under test. It
// can change its lease between reads, which is exactly what a registry renewal
// does (a Ping extends the internal lease without emitting a transition).
type scriptedPresenceSource struct {
	mu      sync.Mutex
	bootID  string
	rev     uint64
	entries []PresenceSnapshotEntry
	leases  []time.Time // successive lease values returned per read
	reads   int
}

func (source *scriptedPresenceSource) PresenceSnapshot() (string, uint64, []PresenceSnapshotEntry) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.reads++
	entries := append([]PresenceSnapshotEntry(nil), source.entries...)
	if len(source.leases) > 0 {
		index := source.reads - 1
		if index >= len(source.leases) {
			index = len(source.leases) - 1
		}
		for i := range entries {
			entries[i].LeaseExpiresAt = source.leases[index]
		}
	}
	return source.bootID, source.rev, entries
}

func (source *scriptedPresenceSource) readCount() int {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.reads
}

func decodePresenceBody(t *testing.T, body []byte) PresenceEnvelope {
	t.Helper()
	var envelope PresenceEnvelope
	if err := decodeSyncBody(body, &envelope); err != nil {
		t.Fatalf("decode presence body %q: %v", body, err)
	}
	return envelope
}

// TestPresencePublisherCarriesBootIdentityOnAnEmptySnapshot proves the
// adoption primitive: a fresh gateway posts its empty boot snapshot with the
// top-level gateway_boot_id/revision so control can record the new boot (an
// empty snapshot without a boot identity is indistinguishable from "the
// previously recorded boot has no routes").
func TestPresencePublisherCarriesBootIdentityOnAnEmptySnapshot(t *testing.T) {
	certs := newSyncTestCertificates(t)
	recorder := &presenceRecorder{}
	stub := newStubControlServer(t, certs, func(_ *testing.T, request *http.Request) (int, []byte) {
		if request.URL.Path != PathPresenceSnapshot {
			return http.StatusNotFound, []byte(`{"error":"not found"}`)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read presence body: %v", err)
			return http.StatusInternalServerError, []byte(`{"error":"read"}`)
		}
		recorder.record(request.URL.Path, body)
		return http.StatusOK, []byte(`{"version":1,"accepted":true}`)
	})
	source := &scriptedPresenceSource{bootID: "gw-boot-fresh", rev: 0}
	publisher, err := NewPresencePublisher(PresencePublisherConfig{
		Client: stub.newTestClient(t, nil),
		Source: source,
	})
	if err != nil {
		t.Fatalf("NewPresencePublisher: %v", err)
	}
	if err := publisher.PublishOnce(context.Background()); err != nil {
		t.Fatalf("PublishOnce: %v", err)
	}
	paths, bodies := recorder.snapshot()
	if len(paths) != 1 || paths[0] != PathPresenceSnapshot {
		t.Fatalf("presence posts = %v, want exactly one snapshot post", paths)
	}
	envelope := decodePresenceBody(t, bodies[0])
	if envelope.GatewayBootID != "gw-boot-fresh" {
		t.Errorf("snapshot gateway_boot_id = %q, want gw-boot-fresh", envelope.GatewayBootID)
	}
	if envelope.Revision != 0 {
		t.Errorf("snapshot revision = %d, want 0", envelope.Revision)
	}
	if envelope.Events == nil || len(envelope.Events) != 0 {
		t.Errorf("empty snapshot events = %+v, want an empty (non-nil) list", envelope.Events)
	}
	if !bytes.Contains(bodies[0], []byte(`"events":[]`)) {
		t.Errorf("empty snapshot body %s does not carry events:[]", bodies[0])
	}
}

// TestPresencePublisherRepublishesRenewedLease proves the renewal property: a
// healthy tunnel whose lease was extended by a Ping (no transition event) is
// carried to control by the periodic full-state republish, so control's stored
// expiry advances instead of expiring at the confirmation-time value.
func TestPresencePublisherRepublishesRenewedLease(t *testing.T) {
	certs := newSyncTestCertificates(t)
	recorder := &presenceRecorder{}
	stub := newStubControlServer(t, certs, func(_ *testing.T, request *http.Request) (int, []byte) {
		body, _ := io.ReadAll(request.Body)
		recorder.record(request.URL.Path, body)
		return http.StatusOK, []byte(`{"version":1,"accepted":true}`)
	})
	first := time.Now().UTC().Add(45 * time.Second).Truncate(time.Second)
	second := first.Add(10 * time.Second)
	source := &scriptedPresenceSource{
		bootID: "gw-boot-renew",
		rev:    7,
		entries: []PresenceSnapshotEntry{{
			AgentRecordID: "agent-renew",
			RelayPort:     10042,
			Generation:    3,
		}},
		leases: []time.Time{first, second},
	}
	publisher, err := NewPresencePublisher(PresencePublisherConfig{
		Client:   stub.newTestClient(t, nil),
		Source:   source,
		Interval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewPresencePublisher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		publisher.Run(ctx)
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		paths, _ := recorder.snapshot()
		if len(paths) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("presence republished only %d times before the deadline", len(paths))
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("PresencePublisher.Run did not return after cancellation")
	}
	_, bodies := recorder.snapshot()
	firstEnvelope := decodePresenceBody(t, bodies[0])
	lastEnvelope := decodePresenceBody(t, bodies[len(bodies)-1])
	if firstEnvelope.GatewayBootID != "gw-boot-renew" || firstEnvelope.Revision != 7 {
		t.Fatalf("first snapshot boot/revision = %q/%d, want gw-boot-renew/7", firstEnvelope.GatewayBootID, firstEnvelope.Revision)
	}
	if len(firstEnvelope.Events) != 1 || firstEnvelope.Events[0].State != PresenceStateOnline {
		t.Fatalf("first snapshot events = %+v, want one online event", firstEnvelope.Events)
	}
	if got, want := firstEnvelope.Events[0].LeaseExpiresAt, first.Format(time.RFC3339); got != want {
		t.Errorf("first lease = %q, want %q", got, want)
	}
	if len(lastEnvelope.Events) != 1 {
		t.Fatalf("last snapshot events = %+v, want one online event", lastEnvelope.Events)
	}
	if got, want := lastEnvelope.Events[0].LeaseExpiresAt, second.Format(time.RFC3339); got != want {
		t.Errorf("renewed lease = %q, want %q (the republish must carry the renewed expiry)", got, want)
	}
}

// TestPresencePublisherNotifyTriggersImmediateRepublish proves a transition is
// carried without waiting for the interval, using the same coalescing signal
// the registry's sink drives.
func TestPresencePublisherNotifyTriggersImmediateRepublish(t *testing.T) {
	certs := newSyncTestCertificates(t)
	recorder := &presenceRecorder{}
	stub := newStubControlServer(t, certs, func(_ *testing.T, request *http.Request) (int, []byte) {
		body, _ := io.ReadAll(request.Body)
		recorder.record(request.URL.Path, body)
		return http.StatusOK, []byte(`{"version":1,"accepted":true}`)
	})
	source := &scriptedPresenceSource{bootID: "gw-boot-notify", rev: 1}
	publisher, err := NewPresencePublisher(PresencePublisherConfig{
		Client: stub.newTestClient(t, nil),
		Source: source,
		// A long interval: only Notify can explain a second publish.
		Interval: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPresencePublisher: %v", err)
	}
	// Boot publish.
	if err := publisher.PublishOnce(context.Background()); err != nil {
		t.Fatalf("boot PublishOnce: %v", err)
	}
	for i := 0; i < 5; i++ {
		publisher.Notify() // any number of signals coalesces into one publish
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		publisher.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		paths, _ := recorder.snapshot()
		if len(paths) >= 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Notify produced %d publishes, want at least 2", len(paths))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestPresencePublisherFailsClosedWithoutBootIdentity proves a source that
// cannot name its boot posts nothing (the snapshot would not be adoptable).
func TestPresencePublisherFailsClosedWithoutBootIdentity(t *testing.T) {
	certs := newSyncTestCertificates(t)
	requests := 0
	stub := newStubControlServer(t, certs, func(_ *testing.T, _ *http.Request) (int, []byte) {
		requests++
		return http.StatusOK, []byte(`{"version":1,"accepted":true}`)
	})
	source := &scriptedPresenceSource{bootID: ""}
	publisher, err := NewPresencePublisher(PresencePublisherConfig{Client: stub.newTestClient(t, nil), Source: source})
	if err != nil {
		t.Fatalf("NewPresencePublisher: %v", err)
	}
	if err := publisher.PublishOnce(context.Background()); err == nil {
		t.Fatal("PublishOnce with no boot identity error = nil, want a fail-closed rejection")
	}
	if requests != 0 {
		t.Fatalf("presence requests = %d, want 0 (nothing must reach the wire)", requests)
	}
}

// TestPresencePublisherValidatesConfiguration covers the constructor guards.
func TestPresencePublisherValidatesConfiguration(t *testing.T) {
	certs := newSyncTestCertificates(t)
	stub := newStubControlServer(t, certs, func(_ *testing.T, _ *http.Request) (int, []byte) {
		return http.StatusOK, []byte(`{"version":1,"accepted":true}`)
	})
	source := &scriptedPresenceSource{bootID: "gw-boot"}
	if _, err := NewPresencePublisher(PresencePublisherConfig{Source: source}); err == nil {
		t.Error("nil client accepted")
	}
	if _, err := NewPresencePublisher(PresencePublisherConfig{Client: stub.newTestClient(t, nil)}); err == nil {
		t.Error("nil source accepted")
	}
	if _, err := NewPresencePublisher(PresencePublisherConfig{Client: stub.newTestClient(t, nil), Source: source, Interval: -time.Second}); err == nil {
		t.Error("negative interval accepted")
	}
}

// TestPresencePublisherDefaultIntervalIsSafelyBelowTheLease pins the cadence
// rationale: the republish interval must be well inside the gateway's 45 s
// presence lease and control's 60 s receipt cap.
func TestPresencePublisherDefaultIntervalIsSafelyBelowTheLease(t *testing.T) {
	if DefaultPresenceRepublishInterval >= 45*time.Second {
		t.Fatalf("DefaultPresenceRepublishInterval = %s, must be well below the 45 s presence lease", DefaultPresenceRepublishInterval)
	}
	if DefaultPresenceRepublishInterval >= 60*time.Second {
		t.Fatalf("DefaultPresenceRepublishInterval = %s, must be well below control's 60 s receipt cap", DefaultPresenceRepublishInterval)
	}
}

// TestPresencePublisherSnapshotIsValidWireShape proves the republished payload
// passes the gateway's own wire validation (the producer must not emit a shape
// its peer would reject).
func TestPresencePublisherSnapshotIsValidWireShape(t *testing.T) {
	certs := newSyncTestCertificates(t)
	recorder := &presenceRecorder{}
	stub := newStubControlServer(t, certs, func(_ *testing.T, request *http.Request) (int, []byte) {
		body, _ := io.ReadAll(request.Body)
		recorder.record(request.URL.Path, body)
		return http.StatusOK, []byte(`{"version":1,"accepted":true}`)
	})
	source := &scriptedPresenceSource{
		bootID: "gw-boot-wire",
		rev:    4,
		entries: []PresenceSnapshotEntry{{
			AgentRecordID:  "agent-wire",
			RelayPort:      10042,
			Generation:     3,
			LeaseExpiresAt: time.Now().Add(45 * time.Second),
		}},
	}
	publisher, err := NewPresencePublisher(PresencePublisherConfig{Client: stub.newTestClient(t, nil), Source: source})
	if err != nil {
		t.Fatalf("NewPresencePublisher: %v", err)
	}
	if err := publisher.PublishOnce(context.Background()); err != nil {
		t.Fatalf("PublishOnce: %v", err)
	}
	_, bodies := recorder.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("presence posts = %d, want 1", len(bodies))
	}
	envelope := decodePresenceBody(t, bodies[0])
	if err := ValidatePresenceSnapshotEnvelope(envelope, MaxPresenceEventsPerEnvelope); err != nil {
		t.Fatalf("republished snapshot rejected by the gateway validator: %v", err)
	}
	raw, _ := json.Marshal(envelope)
	if !bytes.Contains(raw, []byte(`"gateway_boot_id":"gw-boot-wire"`)) {
		t.Errorf("republished snapshot %s lost the boot identity", raw)
	}
}
