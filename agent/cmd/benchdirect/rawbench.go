package main

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/pion/webrtc/v4"
)

type runConfig struct {
	mode         string
	rttMs        int
	loss         float64
	size         int64
	chunk        int
	backpressure string
}

type rawResult struct {
	Mode      string  `json:"mode"`
	RTT       int     `json:"rtt_ms"`
	SentBytes int64   `json:"sent_bytes"`
	Received  int64   `json:"received_bytes"`
	Mbps      float64 `json:"mbps"`
}

func runRaw(ctx context.Context, cfg runConfig) (rawResult, error) {
	res := rawResult{Mode: "raw", RTT: cfg.rttMs}
	delay := time.Duration(cfg.rttMs) * time.Millisecond / 2

	shim, err := NewShim(delay, cfg.loss)
	if err != nil {
		return res, fmt.Errorf("new shim: %w", err)
	}
	defer shim.Close()

	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return res, fmt.Errorf("new peer connection: %w", err)
	}
	defer pc.Close()

	dc, err := pc.CreateDataChannel("bench", nil)
	if err != nil {
		return res, fmt.Errorf("create data channel: %w", err)
	}
	openCh := make(chan struct{}, 1)
	dc.OnOpen(func() {
		select {
		case openCh <- struct{}{}:
		default:
		}
	})

	gatherDone := webrtc.GatheringCompletePromise(pc)
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return res, fmt.Errorf("create offer: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return res, fmt.Errorf("set local description: %w", err)
	}
	<-gatherDone
	offerSDP := pc.LocalDescription().SDP

	// Rewrite Go's host candidate to the shim and register Go as peer A.
	rewrittenOffer, _, goPort, err := RewriteHostCandidate(offerSDP, shim.Addr().Port)
	if err != nil {
		return res, fmt.Errorf("rewrite offer candidate: %w", err)
	}
	shim.SetPeerA(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: goPort})

	answerCh := make(chan string, 1)
	webRoot, err := fs.Sub(webFS, "web")
	if err != nil {
		return res, fmt.Errorf("sub web fs: %w", err)
	}
	srv := newBenchServer(func() string { return rewrittenOffer }, answerCh, webRoot)
	srv.mode, srv.size, srv.chunk = cfg.mode, cfg.size, cfg.chunk
	baseURL, err := srv.listen()
	if err != nil {
		return res, fmt.Errorf("listen bench server: %w", err)
	}

	b, err := startBrowser(ctx)
	if err != nil {
		return res, fmt.Errorf("start browser: %w", err)
	}
	defer b.close()
	if err := chromedp.Run(b.ctx, chromedp.Navigate(baseURL)); err != nil {
		return res, fmt.Errorf("navigate browser: %w", err)
	}

	select {
	case answer := <-answerCh:
		// Rewrite Chrome's host candidate to the shim; the shim auto-learns
		// Chrome's real address from the first datagram it receives.
		rewrittenAnswer, _, _, err := RewriteHostCandidate(answer, shim.Addr().Port)
		if err != nil {
			return res, err
		}
		if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: rewrittenAnswer}); err != nil {
			return res, fmt.Errorf("set remote description: %w", err)
		}
	case <-ctx.Done():
		return res, ctx.Err()
	}

	select {
	case <-openCh:
	case <-ctx.Done():
		return res, ctx.Err()
	}

	// Pump bytes until the channel is open and all data is sent.
	buf := make([]byte, cfg.chunk)
	var sent int64
	dc.SetBufferedAmountLowThreshold(128 * 1024)
	lowCh := make(chan struct{}, 1)
	dc.OnBufferedAmountLow(func() {
		select {
		case lowCh <- struct{}{}:
		default:
		}
	})
	for sent < cfg.size {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		switch cfg.backpressure {
		case "poll":
			for dc.BufferedAmount() > 5*1024*1024 {
				select {
				case <-ctx.Done():
					return res, ctx.Err()
				case <-time.After(10 * time.Millisecond):
				}
			}
		default: // "event"
			for dc.BufferedAmount() > 5*1024*1024 {
				select {
				case <-ctx.Done():
					return res, ctx.Err()
				case <-lowCh:
				}
			}
		}
		n := int64(cfg.chunk)
		if rem := cfg.size - sent; rem < n {
			n = rem
		}
		if err := dc.Send(buf[:n]); err != nil {
			return res, fmt.Errorf("send chunk after %d bytes: %w", sent, err)
		}
		sent += n
		res.SentBytes = sent
	}

	// Wait for the browser to receive everything.
	deadline := time.Now().Add(30 * time.Second)
	for {
		var received int64
		if err := b.eval("window.__bench.received", &received); err != nil {
			return res, fmt.Errorf("read browser received counter: %w", err)
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
		return res, fmt.Errorf("read browser summary: %w", err)
	}
	res.Received = summary.Received
	res.Mbps = summary.Mbps
	return res, nil
}
