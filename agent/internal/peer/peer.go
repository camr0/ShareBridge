package peer

import (
	"fmt"
	"sync"

	"github.com/pion/webrtc/v4"
	"sharebridge/agent/internal/multilane"
)

var requiredLanes = []struct {
	lane  multilane.Lane
	label string
}{
	{multilane.LaneControl, "control"},
	{multilane.LaneMedia, "media"},
	{multilane.LaneBulk, "bulk"},
}

// Peer manages one WebRTC PeerConnection containing the three required lanes.
type Peer struct {
	pc        *webrtc.PeerConnection
	endpoints map[multilane.Lane]*directEndpoint

	mu                  sync.Mutex
	opened              map[multilane.Lane]bool
	handshakeArmed      bool
	ready               bool
	onMessage           func([]byte)
	closed              bool
	openCallbackRunning bool
	closePending        bool
	closeNotified       bool
	onOpen              func()
	onClosed            func()
	closeListeners      []func()
	onICECandidate      func(init webrtc.ICECandidateInit)
}

// sctpMinCwnd is the minimum SCTP congestion window (4 MiB). pion's default
// (no minimum) lets spurious retransmission timeouts at high RTT collapse the
// congestion window to ~1 MTU, which craters download throughput (measured ~2x
// improvement at 100 ms RTT in agent/cmd/benchdirect; see BENCH_RESULTS.md).
const sctpMinCwnd uint32 = 4 * 1024 * 1024

// New creates a PeerConnection with the given ICE servers.
// If relayOnly is true, forces ICETransportPolicyRelay to hide the agent's IP.
func New(iceServers []webrtc.ICEServer, relayOnly bool) (*Peer, error) {
	config := webrtc.Configuration{ICEServers: iceServers}
	if relayOnly {
		config.ICETransportPolicy = webrtc.ICETransportPolicyRelay
	}

	se := webrtc.SettingEngine{}
	se.SetSCTPMinCwnd(sctpMinCwnd)
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))

	pc, err := api.NewPeerConnection(config)
	if err != nil {
		return nil, fmt.Errorf("new peer connection: %w", err)
	}
	p := &Peer{
		pc:        pc,
		endpoints: make(map[multilane.Lane]*directEndpoint, len(requiredLanes)),
		opened:    make(map[multilane.Lane]bool, len(requiredLanes)),
	}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		p.mu.Lock()
		handler := p.onICECandidate
		p.mu.Unlock()
		if handler != nil {
			handler(c.ToJSON())
		}
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			p.notifyClosed()
		}
	})
	return p, nil
}

// CreateOffer creates all required lane channels before generating the offer.
func (p *Peer) CreateOffer() (string, error) {
	if len(p.endpoints) != 0 {
		return "", fmt.Errorf("create offer: data channels already created")
	}
	for _, required := range requiredLanes {
		dc, err := p.pc.CreateDataChannel(required.label, nil)
		if err != nil {
			_ = p.Close()
			return "", fmt.Errorf("create %s data channel: %w", required.label, err)
		}
		adapter := &pionDataChannel{dc: dc}
		endpoint := &directEndpoint{lane: required.lane, dc: adapter}
		p.endpoints[required.lane] = endpoint
		dc.OnOpen(func() { p.laneOpened(required.lane) })
		dc.OnClose(func() { p.laneClosed(required.lane) })
		dc.OnMessage(func(msg webrtc.DataChannelMessage) { endpoint.deliver(msg.Data) })
	}

	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		return "", fmt.Errorf("create offer: %w", err)
	}
	if err := p.pc.SetLocalDescription(offer); err != nil {
		return "", fmt.Errorf("set local description: %w", err)
	}
	return offer.SDP, nil
}

func (p *Peer) laneOpened(lane multilane.Lane) {
	p.mu.Lock()
	if p.closed || !lane.Valid() || p.opened[lane] {
		p.mu.Unlock()
		return
	}
	p.opened[lane] = true
	if len(p.opened) != len(requiredLanes) || p.handshakeArmed {
		p.mu.Unlock()
		return
	}
	p.handshakeArmed = true
	p.mu.Unlock()

	multilane.InstallHandshakeResponder(p, func() {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return
		}
		p.ready = true
		p.control().SetOnMessage(p.onMessage)
		onOpen := p.onOpen
		p.openCallbackRunning = true
		p.mu.Unlock()
		if onOpen != nil {
			onOpen()
		}

		p.mu.Lock()
		p.openCallbackRunning = false
		var callbacks []func()
		if p.closePending && !p.closeNotified {
			p.closeNotified = true
			callbacks = p.closeCallbacksLocked()
		}
		p.mu.Unlock()
		for _, callback := range callbacks {
			callback()
		}
	})
}

func (p *Peer) laneClosed(multilane.Lane) {
	p.notifyClosed()
	_ = p.pc.Close()
}

func (p *Peer) notifyClosed() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	if p.openCallbackRunning {
		p.closePending = true
		p.mu.Unlock()
		return
	}
	p.closeNotified = true
	callbacks := p.closeCallbacksLocked()
	p.mu.Unlock()
	for _, callback := range callbacks {
		callback()
	}
}

func (p *Peer) closeCallbacksLocked() []func() {
	callbacks := make([]func(), 0, 1+len(p.closeListeners))
	if p.onClosed != nil {
		callbacks = append(callbacks, p.onClosed)
	}
	callbacks = append(callbacks, p.closeListeners...)
	return callbacks
}

// Endpoint returns a required lane, or nil for an unknown lane.
func (p *Peer) Endpoint(lane multilane.Lane) multilane.Endpoint {
	return p.endpoints[lane]
}

func (p *Peer) SetOnOpen(handler func()) {
	p.mu.Lock()
	p.onOpen = handler
	p.mu.Unlock()
}

func (p *Peer) SetOnClose(handler func()) {
	p.mu.Lock()
	p.onClosed = handler
	p.mu.Unlock()
}

func (p *Peer) AddOnClose(handler func()) {
	if handler == nil {
		return
	}
	p.mu.Lock()
	if p.closeNotified {
		p.mu.Unlock()
		handler()
		return
	}
	p.closeListeners = append(p.closeListeners, handler)
	p.mu.Unlock()
}

func (p *Peer) SetOnICECandidate(handler func(webrtc.ICECandidateInit)) {
	p.mu.Lock()
	p.onICECandidate = handler
	p.mu.Unlock()
}

// Compatibility shims keep existing transfer callers on control until routing
// moves to explicit endpoints.
func (p *Peer) SendBinary(data []byte) error { return p.control().SendBinary(data) }
func (p *Peer) SendText(text string) error   { return p.control().SendText(text) }
func (p *Peer) BufferedAmount() uint64       { return p.control().BufferedAmount() }
func (p *Peer) SetOnMessage(handler func([]byte)) {
	p.mu.Lock()
	p.onMessage = handler
	ready := p.ready
	p.mu.Unlock()
	if ready {
		p.control().SetOnMessage(handler)
	}
}

func (p *Peer) control() *directEndpoint { return p.endpoints[multilane.LaneControl] }

// SetAnswer applies the browser's SDP answer as the remote description.
func (p *Peer) SetAnswer(sdp string) error {
	return p.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp})
}

func (p *Peer) AddICECandidate(init webrtc.ICECandidateInit) error {
	return p.pc.AddICECandidate(init)
}

func (p *Peer) Close() error {
	err := p.pc.Close()
	p.notifyClosed()
	return err
}

// LaneLabelsForTest reports the stable offer creation order.
func (p *Peer) LaneLabelsForTest() []string {
	labels := make([]string, 0, len(requiredLanes))
	for _, required := range requiredLanes {
		if _, ok := p.endpoints[required.lane]; ok {
			labels = append(labels, required.label)
		}
	}
	return labels
}

type dataChannel interface {
	SendText(string) error
	Send([]byte) error
	BufferedAmount() uint64
	OnMessage(func([]byte))
}

type pionDataChannel struct{ dc *webrtc.DataChannel }

func (d *pionDataChannel) SendText(value string) error { return d.dc.SendText(value) }
func (d *pionDataChannel) Send(value []byte) error     { return d.dc.Send(value) }
func (d *pionDataChannel) BufferedAmount() uint64      { return d.dc.BufferedAmount() }
func (d *pionDataChannel) OnMessage(handler func([]byte)) {
	d.dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if handler != nil {
			handler(msg.Data)
		}
	})
}

type directEndpoint struct {
	lane      multilane.Lane
	dc        dataChannel
	mu        sync.RWMutex
	onMessage func([]byte)
}

func (e *directEndpoint) SendText(text string) error   { return e.dc.SendText(text) }
func (e *directEndpoint) SendBinary(data []byte) error { return e.dc.Send(data) }
func (e *directEndpoint) SendBinaryClass(class multilane.TrafficClass, data []byte) error {
	lane, err := multilane.LaneForClass(class)
	if err != nil {
		return err
	}
	if lane != e.lane {
		return fmt.Errorf("traffic class %d maps to lane %d, not endpoint lane %d", class, lane, e.lane)
	}
	return e.dc.Send(data)
}
func (e *directEndpoint) BufferedAmount() uint64 { return e.dc.BufferedAmount() }
func (e *directEndpoint) SetOnMessage(handler func([]byte)) {
	e.mu.Lock()
	e.onMessage = handler
	e.mu.Unlock()
}
func (e *directEndpoint) deliver(data []byte) {
	e.mu.RLock()
	handler := e.onMessage
	e.mu.RUnlock()
	if handler != nil {
		handler(append([]byte(nil), data...))
	}
}
