package peer

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
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

func TestPeerAddOnClosePreservesOwnerAndRunsAfterClose(t *testing.T) {
	p := &Peer{}
	var owner, listener, late atomic.Int32
	p.SetOnClose(func() { owner.Add(1) })
	p.AddOnClose(func() { listener.Add(1) })
	p.notifyClosed()
	p.AddOnClose(func() { late.Add(1) })
	if owner.Load() != 1 || listener.Load() != 1 || late.Load() != 1 {
		t.Fatalf("close callbacks owner=%d listener=%d late=%d, want all 1", owner.Load(), listener.Load(), late.Load())
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
	p.SetOnOpen(func() { opened.Add(1) })
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
	p.SetOnClose(func() { closed.Add(1) })
	p.laneClosed(multilane.LaneMedia)
	p.laneClosed(multilane.LaneBulk)
	if got := closed.Load(); got != 1 {
		t.Fatalf("OnClosed calls = %d, want 1", got)
	}
}

func TestPeerCloseCallbackMayReenterClose(t *testing.T) {
	p, err := New(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	p.SetOnClose(func() {
		_ = p.Close()
		close(done)
	})

	go p.notifyClosed()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("re-entrant close callback deadlocked")
	}
}

func TestPeerCannotOpenAfterTerminalClose(t *testing.T) {
	p, err := New(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateOffer(); err != nil {
		t.Fatal(err)
	}
	p.endpoints[multilane.LaneControl].dc = &recordingDataChannel{}
	var opened atomic.Int32
	p.SetOnOpen(func() { opened.Add(1) })
	p.notifyClosed()

	p.laneOpened(multilane.LaneControl)
	p.laneOpened(multilane.LaneMedia)
	p.laneOpened(multilane.LaneBulk)
	p.endpoints[multilane.LaneControl].deliver([]byte(`{"type":"transport_hello","version":2}`))

	if got := opened.Load(); got != 0 {
		t.Fatalf("OnOpen calls after close = %d, want 0", got)
	}
}

func TestPeerLatestApplicationHandlerWinsAfterHandshake(t *testing.T) {
	p, err := New(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if _, err := p.CreateOffer(); err != nil {
		t.Fatal(err)
	}
	p.endpoints[multilane.LaneControl].dc = &recordingDataChannel{}
	p.SetOnMessage(func([]byte) { t.Error("stale handler called") })
	p.laneOpened(multilane.LaneControl)
	p.laneOpened(multilane.LaneMedia)
	p.laneOpened(multilane.LaneBulk)
	p.endpoints[multilane.LaneControl].deliver([]byte(`{"type":"transport_hello","version":2}`))

	called := make(chan struct{}, 1)
	p.SetOnMessage(func([]byte) { called <- struct{}{} })
	p.endpoints[multilane.LaneControl].deliver([]byte(`{"type":"list_request"}`))
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("latest handler was not called")
	}
}

func TestPeerCloseWaitsForRunningOpenCallback(t *testing.T) {
	p, err := New(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if _, err := p.CreateOffer(); err != nil {
		t.Fatal(err)
	}
	p.endpoints[multilane.LaneControl].dc = &recordingDataChannel{}
	started := make(chan struct{})
	release := make(chan struct{})
	closed := make(chan struct{}, 1)
	p.SetOnOpen(func() {
		close(started)
		<-release
	})
	p.SetOnClose(func() { closed <- struct{}{} })
	p.laneOpened(multilane.LaneControl)
	p.laneOpened(multilane.LaneMedia)
	p.laneOpened(multilane.LaneBulk)

	helloDone := make(chan struct{})
	go func() {
		p.endpoints[multilane.LaneControl].deliver([]byte(`{"type":"transport_hello","version":2}`))
		close(helloDone)
	}()
	<-started
	p.notifyClosed()
	select {
	case <-closed:
		t.Fatal("close callback ran before open callback completed")
	default:
	}
	close(release)
	<-helloDone
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("deferred close callback did not run")
	}
}

func TestPeerConcurrentHandlerRegistrationAndHandshake(t *testing.T) {
	p, err := New(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if _, err := p.CreateOffer(); err != nil {
		t.Fatal(err)
	}
	p.endpoints[multilane.LaneControl].dc = &recordingDataChannel{}
	p.laneOpened(multilane.LaneControl)
	p.laneOpened(multilane.LaneMedia)
	p.laneOpened(multilane.LaneBulk)

	var registrations sync.WaitGroup
	for i := 0; i < 100; i++ {
		registrations.Add(1)
		go func() {
			defer registrations.Done()
			p.SetOnOpen(func() {})
			p.SetOnClose(func() {})
			p.SetOnICECandidate(func(webrtc.ICECandidateInit) {})
			p.SetOnMessage(func([]byte) {})
		}()
	}
	p.endpoints[multilane.LaneControl].deliver([]byte(`{"type":"transport_hello","version":2}`))
	registrations.Wait()

	called := make(chan struct{}, 1)
	p.SetOnMessage(func([]byte) { called <- struct{}{} })
	p.endpoints[multilane.LaneControl].deliver([]byte(`{"type":"list_request"}`))
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("final handler did not win after concurrent registration")
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
