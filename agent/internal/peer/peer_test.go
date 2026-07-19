package peer

import (
	"sync/atomic"
	"testing"

	"sharebridge/agent/internal/multilane"
)

func TestCreateOfferCreatesRequiredLaneLabels(t *testing.T) {
	p, err := New(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })

	if _, err := p.CreateOffer(); err != nil {
		t.Fatal(err)
	}
	got := p.LaneLabelsForTest()
	want := []string{"control", "media", "bulk"}
	if len(got) != len(want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("labels = %v, want stable order %v", got, want)
		}
	}
}

func TestPeerReadyAfterEveryLaneAndVersionHelloExactlyOnce(t *testing.T) {
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
	var applicationMessages atomic.Int32
	p.OnOpen = func() { opened.Add(1) }
	p.SetOnMessage(func([]byte) { applicationMessages.Add(1) })
	p.laneOpened(multilane.LaneBulk)
	p.laneOpened(multilane.LaneControl)
	p.laneOpened(multilane.LaneControl)
	if got := opened.Load(); got != 0 {
		t.Fatalf("OnOpen before all lanes = %d", got)
	}
	p.laneOpened(multilane.LaneMedia)
	if got := opened.Load(); got != 0 {
		t.Fatalf("OnOpen before version hello = %d", got)
	}

	p.endpoints[multilane.LaneControl].deliver([]byte(`{"type":"transport_hello","version":2}`))
	p.endpoints[multilane.LaneControl].deliver([]byte(`{"type":"list_request"}`))
	if got := opened.Load(); got != 1 {
		t.Fatalf("OnOpen calls = %d, want 1", got)
	}
	if got := applicationMessages.Load(); got != 1 {
		t.Fatalf("application control messages = %d, want 1 after handshake", got)
	}
}

func TestPeerAnyRequiredLaneCloseClosesSessionOnce(t *testing.T) {
	p, err := New(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateOffer(); err != nil {
		t.Fatal(err)
	}

	var closed atomic.Int32
	p.OnClosed = func() { closed.Add(1) }
	p.laneClosed(multilane.LaneMedia)
	p.laneClosed(multilane.LaneBulk)
	if got := closed.Load(); got != 1 {
		t.Fatalf("OnClosed calls = %d, want 1", got)
	}
}

func TestDirectEndpointRoutesAndValidatesTrafficClass(t *testing.T) {
	dc := &recordingDataChannel{}
	e := &directEndpoint{lane: multilane.LaneMedia, dc: dc}

	if err := e.SendText("hello"); err != nil {
		t.Fatal(err)
	}
	if err := e.SendBinaryClass(multilane.ClassThumbnail, []byte{1, 2}); err != nil {
		t.Fatal(err)
	}
	if err := e.SendBinaryClass(multilane.ClassBulk, []byte{3}); err == nil {
		t.Fatal("expected class-to-lane validation error")
	}
	if len(dc.text) != 1 || dc.text[0] != "hello" {
		t.Fatalf("text sends = %v", dc.text)
	}
	if len(dc.binary) != 1 || string(dc.binary[0]) != string([]byte{1, 2}) {
		t.Fatalf("binary sends = %v", dc.binary)
	}
}

type recordingDataChannel struct {
	text   []string
	binary [][]byte
}

func (d *recordingDataChannel) SendText(value string) error {
	d.text = append(d.text, value)
	return nil
}

func (d *recordingDataChannel) Send(value []byte) error {
	d.binary = append(d.binary, append([]byte(nil), value...))
	return nil
}

func (d *recordingDataChannel) BufferedAmount() uint64 { return 0 }
func (d *recordingDataChannel) OnMessage(func([]byte)) {}
