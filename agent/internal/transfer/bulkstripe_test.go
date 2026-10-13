package transfer

import (
	"context"
	"testing"
	"time"

	"sharebridge/agent/internal/bulkfan"
	"sharebridge/agent/internal/multilane"
)

// mockBulkChannelSet advertises extra bulk lanes the way peer.Peer does for N5
// and the way multilane.FanoutChannelSet does for A1.
type mockBulkChannelSet struct {
	*mockChannelSet
	extra []*mockDC
}

func (m *mockBulkChannelSet) BulkEndpoints() []multilane.Endpoint {
	out := make([]multilane.Endpoint, 0, 1+len(m.extra))
	if m.bulk != nil {
		out = append(out, m.bulk)
	}
	for _, dc := range m.extra {
		out = append(out, dc)
	}
	return out
}

func newMockBulkChannelSet(extra int) *mockBulkChannelSet {
	set := &mockBulkChannelSet{mockChannelSet: newMockChannelSet()}
	for i := 0; i < extra; i++ {
		set.extra = append(set.extra, &mockDC{})
	}
	return set
}

func TestStripedEndpointRoundRobinsAndStampsSequence(t *testing.T) {
	a, b := &mockDC{}, &mockDC{}
	striped := newStripedEndpoint([]multilane.Endpoint{a, b, nil})
	if striped == nil {
		t.Fatal("newStripedEndpoint returned nil for two lanes")
	}

	const chunks = 4
	for i := 0; i < chunks; i++ {
		frame := encodeChunkFrame(99, 3, []byte{byte(i)})
		if err := striped.SendBinary(frame); err != nil {
			t.Fatalf("SendBinary #%d: %v", i, err)
		}
	}

	if len(a.binaryData)+len(b.binaryData) != chunks {
		t.Fatalf("delivered %d frames, want %d", len(a.binaryData)+len(b.binaryData), chunks)
	}

	// Lanes alternate: a gets sequences 0 and 2, b gets 1 and 3.
	for lane, dc := range map[string]*mockDC{"a": a, "b": b} {
		for i, raw := range dc.binaryData {
			operation, generation, _, _, err := bulkfan.DecodeChunkFrame(raw)
			if err != nil {
				t.Fatalf("lane %s frame did not decode: %v", lane, err)
			}
			if operation != 99 || generation != 3 {
				t.Fatalf("lane %s frame = (op=%d gen=%d), want (99,3)", lane, operation, generation)
			}
			if dc.binaryClasses[i] != multilane.ClassBulk {
				t.Fatalf("lane %s frame %d class = %d, want ClassBulk", lane, i, dc.binaryClasses[i])
			}
		}
	}
	if len(a.binaryData) != 2 || len(b.binaryData) != 2 {
		t.Fatalf("lane split = (%d,%d), want an even 2/2 round-robin", len(a.binaryData), len(b.binaryData))
	}

	// Reassemble with the package reorder buffer to prove the two lanes
	// reconstruct the source order.
	reorder := bulkfan.NewReorderBuffer()
	var got []byte
	interleave := []*mockDC{a, b}
	// Feed strictly in the sequence order the striper assigned.
	frames := map[uint64][]byte{}
	for _, dc := range interleave {
		for _, raw := range dc.binaryData {
			_, _, seq, payload, err := bulkfan.DecodeChunkFrame(raw)
			if err != nil {
				t.Fatal(err)
			}
			frames[seq] = payload
		}
	}
	for _, seq := range []uint64{0, 2, 1, 3} { // deliberately out of order
		out, err := reorder.Push(seq, frames[seq])
		if err != nil {
			t.Fatal(err)
		}
		for _, payload := range out {
			got = append(got, payload[0])
		}
	}
	want := []byte{0, 1, 2, 3}
	if string(got) != string(want) {
		t.Fatalf("reassembled %v, want %v", got, want)
	}
}

func TestStripedEndpointRejectsNonBulkClass(t *testing.T) {
	striped := newStripedEndpoint([]multilane.Endpoint{&mockDC{}, &mockDC{}})
	if err := striped.SendBinaryClass(multilane.ClassThumbnail, []byte("x")); err == nil {
		t.Fatal("striped endpoint accepted a non-bulk traffic class")
	}
}

func TestStripedEndpointBufferedAmountIsMax(t *testing.T) {
	a, b := &mockDC{buffered: 10}, &mockDC{buffered: 40}
	striped := newStripedEndpoint([]multilane.Endpoint{a, b})
	if got := striped.BufferedAmount(); got != 40 {
		t.Fatalf("BufferedAmount() = %d, want 40", got)
	}
}

func TestManagerLeversOffByDefault(t *testing.T) {
	t.Setenv(bulkfan.EnvConns, "")
	t.Setenv(bulkfan.EnvLanes, "")
	t.Setenv(bulkfan.EnvPaceMbps, "")

	set := newMockBulkChannelSet(2) // lanes exist, but no lever enables them
	m := NewManager(set, nil, 0)
	if m.bulk != m.rawBulk {
		t.Fatal("default configuration replaced the bulk endpoint")
	}
	if _, ok := m.bulk.(*stripedEndpoint); ok {
		t.Fatal("striped endpoint active with every lever off")
	}
	if m.bulkPacer != nil {
		t.Fatal("pacer active with every lever off")
	}
}

func TestManagerStripesWhenLeverEnabled(t *testing.T) {
	set := newMockBulkChannelSet(2)
	m := NewManager(set, nil, 0)
	m.SetBulkConfig(bulkfan.Config{Conns: 1, Lanes: 3, PaceMbps: 0})

	striped, ok := m.bulk.(*stripedEndpoint)
	if !ok {
		t.Fatalf("bulk endpoint = %T, want *stripedEndpoint", m.bulk)
	}
	if striped.fan.Len() != 3 {
		t.Fatalf("stripe fan has %d lanes, want 3", striped.fan.Len())
	}

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := m.sendWithBackpressure(ctx, m.bulk, multilane.ClassBulk, encodeChunkFrame(7, 0, []byte("x"))); err != nil {
			t.Fatalf("send #%d: %v", i, err)
		}
	}
	delivered := len(set.bulk.binaryData)
	for _, dc := range set.extra {
		delivered += len(dc.binaryData)
	}
	if delivered != 3 {
		t.Fatalf("delivered %d frames, want 3", delivered)
	}
	if len(set.bulk.binaryData) != 1 || len(set.extra[0].binaryData) != 1 || len(set.extra[1].binaryData) != 1 {
		t.Fatal("round-robin did not give each lane exactly one chunk")
	}
}

func TestManagerPacerWiring(t *testing.T) {
	set := newMockChannelSet()
	m := NewManager(set, nil, 0)

	m.SetBulkConfig(bulkfan.Config{Conns: 1, Lanes: 1, PaceMbps: 1000})
	if m.bulkPacer == nil {
		t.Fatal("pacer not created despite PaceMbps > 0")
	}
	// A gigabit-rate bucket must not stall a single small chunk.
	done := make(chan error, 1)
	go func() { done <- m.pace(context.Background(), 1) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("pace() = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pace stalled on a free chunk")
	}

	m.SetBulkConfig(bulkfan.Default())
	if m.bulkPacer != nil {
		t.Fatal("pacer not cleared when pacing is disabled")
	}
}

func TestManagerPacerHonoursContextCancellation(t *testing.T) {
	set := newMockChannelSet()
	m := NewManager(set, nil, 0)
	m.SetBulkConfig(bulkfan.Config{Conns: 1, Lanes: 1, PaceMbps: 0.000001}) // ~1 byte/s

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.pace(ctx, chunkSize*2); err == nil {
		t.Fatal("pace() ignored a cancelled context")
	}
}
