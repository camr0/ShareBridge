package main

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/pion/webrtc/v4"
	"sharebridge/agent/internal/multilane"
	"sharebridge/agent/internal/transfer"
)

// benchChannelSet wraps three pion DataChannels so the real transfer.Manager can
// run over a bench-controlled PeerConnection (peer.Peer cannot be used because
// its CreateOffer returns a candidate-less trickle SDP).
type benchChannelSet struct {
	mu        sync.Mutex
	endpoints map[multilane.Lane]*benchEndpoint
	onOpen    func()
	onClose   func()
	openCount int
	openFired bool
	closed    bool
}

func (c *benchChannelSet) Endpoint(lane multilane.Lane) multilane.Endpoint { return c.endpoints[lane] }
func (c *benchChannelSet) SetOnOpen(f func())                              { c.mu.Lock(); c.onOpen = f; c.mu.Unlock() }
func (c *benchChannelSet) SetOnClose(f func())                             { c.mu.Lock(); c.onClose = f; c.mu.Unlock() }

func (c *benchChannelSet) noteOpen() {
	c.mu.Lock()
	c.openCount++
	shouldFire := c.openCount == 3 && c.onOpen != nil && !c.openFired
	var onOpen func()
	if shouldFire {
		c.openFired = true
		onOpen = c.onOpen
	}
	c.mu.Unlock()
	if onOpen != nil {
		onOpen()
	}
}

func (c *benchChannelSet) noteClose() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	onClose := c.onClose
	c.mu.Unlock()
	if onClose != nil {
		onClose()
	}
}

func (c *benchChannelSet) AddOnClose(f func()) {
	if f == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	prev := c.onClose
	c.onClose = func() {
		if prev != nil {
			prev()
		}
		f()
	}
}
func (c *benchChannelSet) Close() error { return nil }

type benchEndpoint struct {
	lane multilane.Lane
	dc   *webrtc.DataChannel
}

func (e *benchEndpoint) SendText(s string) error   { return e.dc.SendText(s) }
func (e *benchEndpoint) SendBinary(b []byte) error { return e.dc.Send(b) }
func (e *benchEndpoint) BufferedAmount() uint64    { return e.dc.BufferedAmount() }
func (e *benchEndpoint) SetOnMessage(f func([]byte)) {
	e.dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if f != nil {
			f(msg.Data)
		}
	})
}
func (e *benchEndpoint) SendBinaryClass(class multilane.TrafficClass, b []byte) error {
	lane, err := multilane.LaneForClass(class)
	if err != nil {
		return err
	}
	if lane != e.lane {
		return fmt.Errorf("traffic class %d maps to lane %d, not %d", class, lane, e.lane)
	}
	return e.dc.Send(b)
}

func runProd(ctx context.Context, cfg runConfig) (rawResult, error) {
	res := rawResult{Mode: "prod", RTT: cfg.rttMs}
	delay := time.Duration(cfg.rttMs) * time.Millisecond / 2

	shim, err := NewShim(delay, cfg.loss)
	if err != nil {
		return res, err
	}
	defer shim.Close()

	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return res, err
	}
	defer pc.Close()

	labels := map[multilane.Lane]string{
		multilane.LaneControl: "control",
		multilane.LaneMedia:   "media",
		multilane.LaneBulk:    "bulk",
	}
	set := &benchChannelSet{endpoints: make(map[multilane.Lane]*benchEndpoint, 3)}
	openCh := make(chan struct{}, 3)
	for _, lane := range []multilane.Lane{multilane.LaneControl, multilane.LaneMedia, multilane.LaneBulk} {
		dc, err := pc.CreateDataChannel(labels[lane], nil)
		if err != nil {
			return res, err
		}
		set.endpoints[lane] = &benchEndpoint{lane: lane, dc: dc}
		dc.OnOpen(func() {
			select {
			case openCh <- struct{}{}:
			default:
			}
			set.noteOpen()
		})
		dc.OnClose(func() { set.noteClose() })
	}

	gatherDone := webrtc.GatheringCompletePromise(pc)
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return res, err
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return res, err
	}
	<-gatherDone
	offerSDP := pc.LocalDescription().SDP

	rewrittenOffer, _, goPort, err := RewriteHostCandidate(offerSDP, shim.Addr().Port)
	if err != nil {
		return res, err
	}
	shim.SetPeerA(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: goPort})

	answerCh := make(chan string, 1)
	webRoot, err := fs.Sub(webFS, "web")
	if err != nil {
		return res, err
	}
	srv := newBenchServer(func() string { return rewrittenOffer }, answerCh, webRoot)
	srv.mode, srv.size, srv.chunk = cfg.mode, cfg.size, cfg.chunk
	baseURL, err := srv.listen()
	if err != nil {
		return res, err
	}

	b, err := startBrowser(ctx)
	if err != nil {
		return res, err
	}
	defer b.close()
	if err := chromedp.Run(b.ctx, chromedp.Navigate(baseURL)); err != nil {
		return res, err
	}

	select {
	case answer := <-answerCh:
		rewrittenAnswer, _, _, err := RewriteHostCandidate(answer, shim.Addr().Port)
		if err != nil {
			return res, err
		}
		if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: rewrittenAnswer}); err != nil {
			return res, err
		}
	case <-ctx.Done():
		return res, ctx.Err()
	case <-time.After(30 * time.Second):
		return res, fmt.Errorf("timed out waiting for browser answer")
	}

	for i := 0; i < 3; i++ {
		select {
		case <-openCh:
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(10 * time.Second):
			return res, fmt.Errorf("timed out waiting for DataChannels to open")
		}
	}

	mgr := transfer.NewManager(set, benchStorage{size: cfg.size}, 0)
	go mgr.HandleMessage([]byte(`{"type":"file_request","path":"bench.bin","request_id":"bench"}`))

	deadline := time.Now().Add(cfg.deadline)
	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		var received int64
		if err := b.eval("window.__bench.received", &received); err != nil {
			return res, err
		}
		if received >= cfg.size {
			break
		}
		if time.Now().After(deadline) {
			return res, fmt.Errorf("timed out waiting for receiver: %d/%d", received, cfg.size)
		}
		time.Sleep(50 * time.Millisecond)
	}
	var summary struct {
		Received  int64   `json:"received"`
		ElapsedMs float64 `json:"elapsedMs"`
		Mbps      float64 `json:"mbps"`
	}
	if err := b.eval("window.__benchSummary()", &summary); err != nil {
		return res, err
	}
	res.Received = summary.Received
	res.Mbps = summary.Mbps
	res.SentBytes = cfg.size
	return res, nil
}
