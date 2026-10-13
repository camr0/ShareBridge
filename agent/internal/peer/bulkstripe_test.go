package peer

import (
	"sync/atomic"
	"testing"

	"sharebridge/agent/internal/bulkfan"
	"sharebridge/agent/internal/multilane"
)

func TestBulkEndpointsNilInDefaultConfiguration(t *testing.T) {
	t.Setenv(bulkfan.EnvLanes, "1")
	p, err := New(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if _, err := p.CreateOffer(); err != nil {
		t.Fatal(err)
	}
	if got := p.LaneLabelsForTest(); len(got) != 3 {
		t.Fatalf("labels = %v, want the three required lanes only", got)
	}
	if got := p.BulkEndpoints(); got != nil {
		t.Fatalf("BulkEndpoints() = %v, want nil with SB_BULK_LANES unset", got)
	}
}

func TestCreateOfferAddsStripeLanesWhenEnabled(t *testing.T) {
	t.Setenv(bulkfan.EnvLanes, "3")
	p, err := New(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if _, err := p.CreateOffer(); err != nil {
		t.Fatal(err)
	}

	want := []string{"control", "media", "bulk", "bulk-1", "bulk-2"}
	got := p.LaneLabelsForTest()
	if len(got) != len(want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("labels = %v, want stable order %v", got, want)
		}
	}

	endpoints := p.BulkEndpoints()
	if len(endpoints) != 3 {
		t.Fatalf("BulkEndpoints() = %d lanes, want 3", len(endpoints))
	}
	if endpoints[0] != p.endpoints[multilane.LaneBulk] {
		t.Fatal("BulkEndpoints()[0] must be the primary bulk lane")
	}
	// Every advertised lane must route ClassBulk through its own channel.
	p.endpoints[multilane.LaneBulk].dc = &recordingDataChannel{}
	for _, endpoint := range p.extraBulk {
		endpoint.dc = &recordingDataChannel{}
	}
	for i, endpoint := range endpoints {
		if err := endpoint.SendBinaryClass(multilane.ClassBulk, []byte{byte(i)}); err != nil {
			t.Fatalf("lane %d rejected ClassBulk: %v", i, err)
		}
	}
	for i, endpoint := range p.extraBulk {
		recorded := endpoint.dc.(*recordingDataChannel)
		if len(recorded.binary) != 1 || recorded.binary[0][0] != byte(i+1) {
			t.Fatalf("stripe lane %d recorded %v, want one chunk with id %d", i, recorded.binary, i+1)
		}
	}
}

func TestPeerWaitsForExtraBulkLanesBeforeReady(t *testing.T) {
	t.Setenv(bulkfan.EnvLanes, "2")
	p, err := New(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if _, err := p.CreateOffer(); err != nil {
		t.Fatal(err)
	}
	p.endpoints[multilane.LaneControl].dc = &recordingDataChannel{}

	var opened atomic.Int32
	p.SetOnOpen(func() { opened.Add(1) })

	// All three required lanes open, but the N5 stripe lane has not.
	p.laneOpened(multilane.LaneBulk)
	p.laneOpened(multilane.LaneControl)
	p.laneOpened(multilane.LaneMedia)
	if got := opened.Load(); got != 0 {
		t.Fatalf("OnOpen fired with a stripe lane still closed")
	}

	// The last stripe lane arriving must arm the handshake responder.
	p.bulkLaneOpened()
	p.endpoints[multilane.LaneControl].deliver([]byte(`{"type":"transport_hello","version":2}`))
	if got := opened.Load(); got != 1 {
		t.Fatalf("OnOpen calls = %d, want 1 after every lane and the hello", got)
	}
}
