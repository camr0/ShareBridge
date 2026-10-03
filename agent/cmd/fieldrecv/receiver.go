package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
	"sharebridge/agent/internal/multilane"
)

const (
	laneControl = "control"
	laneMedia   = "media"
	laneBulk    = "bulk"
)

// peerFactory lets tests inject a pion API (e.g. with loopback candidates
// enabled) instead of building the PeerConnection from the default API.
type peerFactory func(cfg webrtc.Configuration) (*webrtc.PeerConnection, error)

func defaultPeerFactory(cfg webrtc.Configuration) (*webrtc.PeerConnection, error) {
	return webrtc.NewPeerConnection(cfg)
}

// bulkCounters is the E28 "bare" equivalent: payload, wire and frame counts for
// the bulk lane only. All fields are updated from pion's receive goroutine.
type bulkCounters struct {
	payload atomic.Int64
	wire    atomic.Int64
	frames  atomic.Int64
	bad     atomic.Int64
}

// receiver is a pion answerer for one ShareBridge direct session. It accepts
// the agent's three data channels, performs the transport v2 handshake, and
// counts bulk-lane bytes.
type receiver struct {
	pc     *webrtc.PeerConnection
	labels map[string]*webrtc.DataChannel

	mu               sync.Mutex
	opened           int
	handshakeStarted bool
	remoteSet        bool
	pendingCand      []webrtc.ICECandidateInit
	onCandidate      func(webrtc.ICECandidateInit)
	onControl        func([]byte)
	onStateChange    func(webrtc.PeerConnectionState)

	handshakeDone atomic.Bool
	readyCh       chan struct{}
	readyOnce     sync.Once
	failOnce      sync.Once
	failErr       atomic.Value // error
	closed        atomic.Bool

	counter bulkCounters

	firstFrameUnixNano atomic.Int64
	lastFrameUnixNano  atomic.Int64
}

func newReceiver(factory peerFactory, cfg webrtc.Configuration) (*receiver, error) {
	if factory == nil {
		factory = defaultPeerFactory
	}
	pc, err := factory(cfg)
	if err != nil {
		return nil, fmt.Errorf("new peer connection: %w", err)
	}
	r := &receiver{
		pc:      pc,
		labels:  make(map[string]*webrtc.DataChannel, 3),
		readyCh: make(chan struct{}),
	}
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		r.mu.Lock()
		cb := r.onCandidate
		r.mu.Unlock()
		if cb != nil {
			cb(c.ToJSON())
		}
	})
	pc.OnDataChannel(r.handleDataChannel)
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		r.mu.Lock()
		cb := r.onStateChange
		r.mu.Unlock()
		if cb != nil {
			cb(state)
		}
	})
	return r, nil
}

func (r *receiver) SetOnCandidate(cb func(webrtc.ICECandidateInit)) {
	r.mu.Lock()
	r.onCandidate = cb
	r.mu.Unlock()
}

// SetOnControl installs the post-handshake application control handler.
func (r *receiver) SetOnControl(cb func([]byte)) {
	r.mu.Lock()
	r.onControl = cb
	r.mu.Unlock()
}

func (r *receiver) SetOnStateChange(cb func(webrtc.PeerConnectionState)) {
	r.mu.Lock()
	r.onStateChange = cb
	r.mu.Unlock()
}

// Ready is closed once transport_ready has been acknowledged.
func (r *receiver) Ready() <-chan struct{} { return r.readyCh }

func (r *receiver) Err() error {
	if v := r.failErr.Load(); v != nil {
		return v.(error)
	}
	return nil
}

func (r *receiver) fail(err error) {
	r.failOnce.Do(func() {
		r.failErr.Store(err)
	})
}

func (r *receiver) close() {
	if r.closed.CompareAndSwap(false, true) {
		_ = r.pc.Close()
	}
}

func (r *receiver) handleDataChannel(dc *webrtc.DataChannel) {
	label := dc.Label()
	r.mu.Lock()
	r.labels[label] = dc
	r.mu.Unlock()

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		switch label {
		case laneBulk:
			r.countBulk(msg.Data)
		case laneControl:
			r.handleControl(msg.Data)
		}
	})
	dc.OnOpen(func() { r.noteLaneOpen() })
	// OnDataChannel normally fires before the channel opens, but a fast peer
	// could have opened it already; count that immediately.
	if dc.ReadyState() == webrtc.DataChannelStateOpen {
		r.noteLaneOpen()
	}
}

func (r *receiver) noteLaneOpen() {
	r.mu.Lock()
	r.opened++
	shouldStart := r.opened == 3 && !r.handshakeStarted
	if shouldStart {
		r.handshakeStarted = true
	}
	r.mu.Unlock()
	if shouldStart {
		r.beginHandshake()
	}
}

func (r *receiver) lane(label string) *webrtc.DataChannel {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.labels[label]
}

func (r *receiver) beginHandshake() {
	control := r.lane(laneControl)
	if control == nil {
		r.fail(fmt.Errorf("control lane missing at handshake start"))
		return
	}
	// The agent arms its transport_hello responder from its own control-lane
	// OnOpen callback. That callback can fire marginally after ours, in which
	// case a hello sent immediately is silently dropped (directEndpoint.deliver
	// discards messages while onMessage is nil). Settle briefly before sending.
	time.Sleep(300 * time.Millisecond)
	hello, _ := json.Marshal(map[string]any{
		"type":    "transport_hello",
		"version": multilane.ProtocolVersion,
	})
	if err := control.SendText(string(hello)); err != nil {
		r.fail(fmt.Errorf("send transport_hello: %w", err))
		return
	}
	go func() {
		select {
		case <-r.readyCh:
		case <-time.After(10 * time.Second):
			r.fail(fmt.Errorf("transport handshake timeout after 10s (no transport_ready)"))
		}
	}()
}

func (r *receiver) handleControl(data []byte) {
	if !r.handshakeDone.Load() {
		var hs struct {
			Type    string `json:"type"`
			Version int    `json:"version"`
			Scope   string `json:"scope"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(data, &hs); err != nil {
			return
		}
		switch hs.Type {
		case "transport_ready":
			if hs.Version != multilane.ProtocolVersion {
				r.fail(fmt.Errorf("incompatible transport version: %d", hs.Version))
				return
			}
			r.handshakeDone.Store(true)
			r.readyOnce.Do(func() { close(r.readyCh) })
		case "error":
			r.fail(fmt.Errorf("transport rejected: scope=%s message=%s", hs.Scope, hs.Message))
		}
		return
	}
	r.mu.Lock()
	cb := r.onControl
	r.mu.Unlock()
	if cb != nil {
		cb(data)
	}
}

func (r *receiver) countBulk(data []byte) {
	r.counter.wire.Add(int64(len(data)))
	payloadLen, kind, err := framePayloadLen(data)
	if err != nil || kind != frameKindFileChunk {
		r.counter.bad.Add(1)
		return
	}
	r.counter.frames.Add(1)
	r.counter.payload.Add(int64(payloadLen))
	now := time.Now().UnixNano()
	if r.firstFrameUnixNano.Load() == 0 {
		r.firstFrameUnixNano.CompareAndSwap(0, now)
	}
	r.lastFrameUnixNano.Store(now)
}

// HandleOffer applies the agent's offer, buffers any pre-offer candidates,
// creates a complete (non-trickle) answer, and returns its SDP.
func (r *receiver) handleOffer(sdp string) (string, error) {
	if err := r.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
		return "", fmt.Errorf("set remote offer: %w", err)
	}
	r.mu.Lock()
	r.remoteSet = true
	pending := r.pendingCand
	r.pendingCand = nil
	r.mu.Unlock()
	for _, c := range pending {
		_ = r.pc.AddICECandidate(c)
	}

	answer, err := r.pc.CreateAnswer(nil)
	if err != nil {
		return "", fmt.Errorf("create answer: %w", err)
	}
	gatherDone := webrtc.GatheringCompletePromise(r.pc)
	if err := r.pc.SetLocalDescription(answer); err != nil {
		return "", fmt.Errorf("set local answer: %w", err)
	}
	// Wait for a complete SDP so the answer carries our candidates. The agent
	// applies its remote description from this answer; trickled candidates sent
	// before that are dropped by pion.
	select {
	case <-gatherDone:
	case <-time.After(15 * time.Second):
	}
	local := r.pc.LocalDescription()
	if local == nil {
		return "", fmt.Errorf("no local description after answer")
	}
	return local.SDP, nil
}

// AddICECandidate adds a remote candidate, buffering it until the offer has
// been applied.
func (r *receiver) AddICECandidate(raw json.RawMessage) error {
	var init webrtc.ICECandidateInit
	if err := json.Unmarshal(raw, &init); err != nil {
		return fmt.Errorf("decode candidate: %w", err)
	}
	r.mu.Lock()
	if !r.remoteSet {
		r.pendingCand = append(r.pendingCand, init)
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()
	return r.pc.AddICECandidate(init)
}

// SelectedPair returns a human-readable description of the selected ICE pair
// (host/srflx/relay) or "unknown".
func (r *receiver) SelectedPair() string {
	if r.pc.SCTP() == nil || r.pc.SCTP().Transport() == nil || r.pc.SCTP().Transport().ICETransport() == nil {
		return "unknown"
	}
	pair, err := r.pc.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil || pair == nil || pair.Local == nil || pair.Remote == nil {
		return "unknown"
	}
	return fmt.Sprintf("local=%s remote=%s", pair.Local.Typ.String(), pair.Remote.Typ.String())
}

func (r *receiver) Stats() (payload, wire, frames, bad int64) {
	return r.counter.payload.Load(), r.counter.wire.Load(), r.counter.frames.Load(), r.counter.bad.Load()
}

// SendControl writes a JSON application message on the control lane (the
// DataChannel, not the signaling WebSocket).
func (r *receiver) SendControl(msg any) error {
	control := r.lane(laneControl)
	if control == nil {
		return fmt.Errorf("control lane not open")
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal control message: %w", err)
	}
	return control.SendText(string(data))
}

// FrameWindow returns the wall-clock span between the first and last bulk
// frame, or false if no frame has arrived yet.
func (r *receiver) FrameWindow() (time.Duration, bool) {
	first := r.firstFrameUnixNano.Load()
	last := r.lastFrameUnixNano.Load()
	if first == 0 || last == 0 || last <= first {
		return 0, false
	}
	return time.Duration(last - first), true
}
