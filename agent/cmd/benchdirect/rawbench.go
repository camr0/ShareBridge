package main

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"os"
	"strings"
	"sync"
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
	deadline     time.Duration
	window       int64
	minCwnd      int64
	jitter       time.Duration
	bandwidth    int64
	queue        int64
	rtoMax       time.Duration
	cwndCAStep   int64
	fastRtxWnd   int64
	maxRxBuf     int64
	maxMsg       int64
	conns        int
	sharing      string
}

// connStat reports one association's share of the transfer. Divergence between
// connections is the signal we are looking for: if N independent associations
// fix the high-RTT collapse, per-connection throughput should cluster; if one
// association collapses, its received bytes and last-byte timestamp will trail.
type connStat struct {
	Index        int     `json:"index"`
	ShardBytes   int64   `json:"shard_bytes"`
	SentBytes    int64   `json:"sent_bytes"`
	Received     int64   `json:"received"`
	FirstByteMs  float64 `json:"first_byte_ms"`
	LastByteMs   float64 `json:"last_byte_ms"`
	Mbps         float64 `json:"mbps"`
	SendMs       float64 `json:"send_ms"`
	SendErr      string  `json:"send_err,omitempty"`
	ShimFwd      int64   `json:"shim_fwd"`
	ShimDrop     int64   `json:"shim_drop"`
	ShimWriteErr int64   `json:"shim_write_err"`
}

type rawResult struct {
	Mode         string     `json:"mode"`
	RTT          int        `json:"rtt_ms"`
	Loss         float64    `json:"loss"`
	Chunk        int64      `json:"chunk"`
	JitterMs     int        `json:"jitter_ms"`
	Bandwidth    int64      `json:"bandwidth_bps"`
	Queue        int64      `json:"queue_bytes"`
	RtoMaxMs     int64      `json:"rto_max_ms"`
	CwndCAStep   int64      `json:"cwnd_ca_step"`
	MinCwnd      int64      `json:"min_cwnd"`
	FastRtxWnd   int64      `json:"fast_rtx_wnd"`
	MaxRxBuf     int64      `json:"max_rx_buf"`
	MaxMsg       int64      `json:"max_msg"`
	Conns        int        `json:"conns"`
	Sharing      string     `json:"sharing"`
	SentBytes    int64      `json:"sent_bytes"`
	Received     int64      `json:"received_bytes"`
	Mbps         float64    `json:"mbps"`
	WallMbps     float64    `json:"wall_mbps"`
	ElapsedMs    float64    `json:"elapsed_ms"`
	ChromeCPUSec float64    `json:"chrome_cpu_seconds"`
	ChromeCores  float64    `json:"chrome_cores"`
	GoCPUSec     float64    `json:"go_cpu_seconds"`
	PerConn      []connStat `json:"per_conn,omitempty"`
	Samples      []int64    `json:"samples,omitempty"`
	Err          string     `json:"error,omitempty"`
}

// newShapers builds the bottleneck model. "shared" (default) puts every flow on
// one Shaper, so N connections compete for one link - the realistic home-uplink
// case. "independent" gives each connection its own Shaper and its own capacity.
func newShapers(cfg runConfig, n int, delay time.Duration) ([]*Shaper, []*Flow, error) {
	independent := cfg.sharing == "independent"
	count := 1
	if independent {
		count = n
	}
	shapers := make([]*Shaper, 0, count)
	for i := 0; i < count; i++ {
		sh, err := NewShaper(delay, cfg.loss)
		if err != nil {
			return nil, nil, err
		}
		sh.SetJitter(cfg.jitter)
		sh.SetBandwidth(cfg.bandwidth)
		sh.SetQueueBytes(cfg.queue)
		shapers = append(shapers, sh)
	}
	flows := make([]*Flow, 0, n)
	for i := 0; i < n; i++ {
		sh := shapers[0]
		if independent {
			sh = shapers[i]
		}
		f, err := sh.NewFlow()
		if err != nil {
			return nil, nil, err
		}
		flows = append(flows, f)
	}
	return shapers, flows, nil
}

// pump writes total bytes to dc, respecting the configured backpressure mode.
func pump(ctx context.Context, cfg runConfig, dc *webrtc.DataChannel, total int64) (int64, error) {
	buf := make([]byte, cfg.chunk)
	var sent int64
	if cfg.backpressure == "event" {
		dc.SetBufferedAmountLowThreshold(128 * 1024)
	}
	lowCh := make(chan struct{}, 1)
	dc.OnBufferedAmountLow(func() {
		select {
		case lowCh <- struct{}{}:
		default:
		}
	})
	for sent < total {
		if err := ctx.Err(); err != nil {
			return sent, err
		}
		for dc.BufferedAmount() > uint64(cfg.window) {
			if cfg.backpressure == "poll" {
				select {
				case <-ctx.Done():
					return sent, ctx.Err()
				case <-time.After(10 * time.Millisecond):
				}
			} else {
				select {
				case <-ctx.Done():
					return sent, ctx.Err()
				case <-lowCh:
				}
			}
		}
		n := int64(cfg.chunk)
		if rem := total - sent; rem < n {
			n = rem
		}
		if err := dc.Send(buf[:n]); err != nil {
			return sent, fmt.Errorf("send chunk after %d bytes: %w", sent, err)
		}
		sent += n
	}
	return sent, nil
}

type benchConn struct {
	pc     *webrtc.PeerConnection
	dc     *webrtc.DataChannel
	flow   *Flow
	shaper *Shaper
	opened chan struct{}
}

// shardSizes splits total into n contiguous near-equal shards. The first
// total%n shards carry one extra byte, so the shards always sum to total.
func shardSizes(total int64, n int) []int64 {
	shards := make([]int64, n)
	if n <= 0 {
		return shards
	}
	base := total / int64(n)
	rem := total % int64(n)
	for i := range shards {
		shards[i] = base
		if int64(i) < rem {
			shards[i]++
		}
	}
	return shards
}

func runRaw(ctx context.Context, cfg runConfig) (rawResult, error) {
	n := cfg.conns
	if n < 1 {
		n = 1
	}
	if cfg.sharing == "" {
		cfg.sharing = "shared"
	}
	res := rawResult{
		Mode: "raw", RTT: cfg.rttMs, Loss: cfg.loss, Conns: n, Sharing: cfg.sharing,
		JitterMs: int(cfg.jitter / time.Millisecond), Bandwidth: cfg.bandwidth,
		Queue: cfg.queue, RtoMaxMs: int64(cfg.rtoMax / time.Millisecond),
		Chunk: int64(cfg.chunk),
		CwndCAStep: cfg.cwndCAStep, FastRtxWnd: cfg.fastRtxWnd,
		MaxRxBuf: cfg.maxRxBuf, MaxMsg: cfg.maxMsg, MinCwnd: cfg.minCwnd,
	}
	delay := time.Duration(cfg.rttMs) * time.Millisecond / 2

	shapers, flows, err := newShapers(cfg, n, delay)
	if err != nil {
		return res, fmt.Errorf("new shaper: %w", err)
	}
	defer func() {
		for _, sh := range shapers {
			_ = sh.Close()
		}
	}()

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	conns := make([]*benchConn, n)
	offers := make([]string, n)
	independent := cfg.sharing == "independent"

	for i := 0; i < n; i++ {
		se := webrtc.SettingEngine{}
		se.SetIncludeLoopbackCandidate(true)
		if cfg.minCwnd > 0 {
			se.SetSCTPMinCwnd(uint32(cfg.minCwnd))
		}
		if cfg.rtoMax > 0 {
			se.SetSCTPRTOMax(cfg.rtoMax)
		}
		if cfg.cwndCAStep > 0 {
			se.SetSCTPCwndCAStep(uint32(cfg.cwndCAStep))
		}
		if cfg.fastRtxWnd > 0 {
			se.SetSCTPFastRtxWnd(uint32(cfg.fastRtxWnd))
		}
		if cfg.maxRxBuf > 0 {
			se.SetSCTPMaxReceiveBufferSize(uint32(cfg.maxRxBuf))
		}
		if cfg.maxMsg > 0 {
			se.SetSCTPMaxMessageSize(uint32(cfg.maxMsg))
		}
		api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
		pc, err := api.NewPeerConnection(webrtc.Configuration{})
		if err != nil {
			return res, fmt.Errorf("new peer connection %d: %w", i, err)
		}
		dc, err := pc.CreateDataChannel("bench", nil)
		if err != nil {
			return res, fmt.Errorf("create data channel %d: %w", i, err)
		}
		c := &benchConn{pc: pc, dc: dc, flow: flows[i], opened: make(chan struct{}, 1)}
		if independent {
			c.shaper = shapers[i]
		} else {
			c.shaper = shapers[0]
		}
		dc.OnOpen(func() {
			select {
			case c.opened <- struct{}{}:
			default:
			}
		})
		conns[i] = c

		gatherDone := webrtc.GatheringCompletePromise(pc)
		offer, err := pc.CreateOffer(nil)
		if err != nil {
			return res, fmt.Errorf("create offer %d: %w", i, err)
		}
		if err := pc.SetLocalDescription(offer); err != nil {
			return res, fmt.Errorf("set local description %d: %w", i, err)
		}
		<-gatherDone

		rewritten, _, goPort, err := RewriteHostCandidate(pc.LocalDescription().SDP, flows[i].Addr().Port)
		if err != nil {
			return res, fmt.Errorf("rewrite offer %d: %w", i, err)
		}
		flows[i].SetPeerA(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: goPort})
		offers[i] = rewritten
	}
	defer func() {
		for _, c := range conns {
			_ = c.pc.Close()
		}
	}()

	answerCh := make(chan answerMsg, n)
	webRoot, err := fs.Sub(webFS, "web")
	if err != nil {
		return res, fmt.Errorf("sub web fs: %w", err)
	}
	srv := newBenchServer(offers, answerCh, webRoot)
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

	got := make([]bool, n)
	for remaining := n; remaining > 0; {
		select {
		case ans := <-answerCh:
			if got[ans.Index] {
				continue
			}
			rewritten, _, _, err := RewriteHostCandidate(ans.SDP, flows[ans.Index].Addr().Port)
			if err != nil {
				return res, fmt.Errorf("rewrite answer %d: %w", ans.Index, err)
			}
			if err := conns[ans.Index].pc.SetRemoteDescription(webrtc.SessionDescription{
				Type: webrtc.SDPTypeAnswer, SDP: rewritten,
			}); err != nil {
				return res, fmt.Errorf("set remote description %d: %w", ans.Index, err)
			}
			got[ans.Index] = true
			remaining--
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(30 * time.Second):
			return res, fmt.Errorf("timed out waiting for browser answers (%d/%d)", n-remaining, n)
		}
	}

	for i, c := range conns {
		select {
		case <-c.opened:
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(15 * time.Second):
			return res, fmt.Errorf("timed out waiting for DataChannel %d to open", i)
		}
	}

	// Stripe the payload into n contiguous shards, one per association.
	shards := shardSizes(cfg.size, n)

	cpu := startCPUSampler()
	goCPUStart := selfCPUSeconds()

	sentBytes := make([]int64, n)
	sendErrs := make([]error, n)
	sendMs := make([]float64, n)
	sendStart := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sentBytes[i], sendErrs[i] = pump(runCtx, cfg, conns[i].dc, shards[i])
			sendMs[i] = float64(time.Since(sendStart).Milliseconds())
			res.SentBytes += sentBytes[i]
		}(i)
	}

	// Wait for the browser to receive everything (or time out: a stall is data).
	deadline := time.Now().Add(cfg.deadline)
	timedOut := false
	for {
		if err := ctx.Err(); err != nil {
			cancelRun()
			return res, err
		}
		var received int64
		if err := b.eval("window.__bench.received", &received); err != nil {
			cancelRun()
			return res, fmt.Errorf("read browser received counter: %w", err)
		}
		if received >= cfg.size {
			break
		}
		if time.Now().After(deadline) {
			timedOut = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancelRun()
	wg.Wait()
	goCPU := selfCPUSeconds() - goCPUStart
	chromeSec, chromeCores := cpu.Stop()

	var summary struct {
		Received  int64   `json:"received"`
		ElapsedMs float64 `json:"elapsedMs"`
		Mbps      float64 `json:"mbps"`
		WallMs    float64 `json:"wallMs"`
		WallMbps  float64 `json:"wallMbps"`
		Samples   []int64 `json:"samples"`
		PerConn   []struct {
			Received    int64   `json:"received"`
			FirstByteTs float64 `json:"firstByteTs"`
			LastByteTs  float64 `json:"lastByteTs"`
		} `json:"perConn"`
	}
	if err := b.eval("window.__benchSummary()", &summary); err != nil {
		return res, fmt.Errorf("read browser summary: %w", err)
	}

	res.Received = summary.Received
	res.Mbps = summary.Mbps
	res.WallMbps = summary.WallMbps
	res.ElapsedMs = summary.ElapsedMs
	res.Samples = summary.Samples
	res.ChromeCPUSec = chromeSec
	res.ChromeCores = chromeCores
	res.GoCPUSec = goCPU
	if timedOut {
		res.Err = fmt.Sprintf("receiver timeout: %d/%d bytes", summary.Received, cfg.size)
		res.Mbps = summary.WallMbps
	}

	firstTs := 0.0
	for _, p := range summary.PerConn {
		if p.FirstByteTs > 0 && (firstTs == 0 || p.FirstByteTs < firstTs) {
			firstTs = p.FirstByteTs
		}
	}
	for i := 0; i < n; i++ {
		st := connStat{Index: i, ShardBytes: shards[i], SentBytes: sentBytes[i], SendMs: sendMs[i]}
		if sendErrs[i] != nil {
			st.SendErr = sendErrs[i].Error()
		}
		if i < len(summary.PerConn) {
			p := summary.PerConn[i]
			st.Received = p.Received
			if firstTs > 0 && p.FirstByteTs > 0 {
				st.FirstByteMs = p.FirstByteTs - firstTs
				st.LastByteMs = p.LastByteTs - firstTs
				if d := (p.LastByteTs - p.FirstByteTs) / 1000; d > 0 {
					st.Mbps = float64(p.Received) * 8 / d / 1e6
				}
			}
		}
		st.ShimFwd, st.ShimWriteErr, st.ShimDrop = conns[i].flow.Stats()
		res.PerConn = append(res.PerConn, st)
	}

	var sb strings.Builder
	for i := 1; i < len(summary.Samples); i++ {
		d := summary.Samples[i] - summary.Samples[i-1]
		fmt.Fprintf(&sb, " %.0f", float64(d)*8/0.1/1e6)
	}
	fw, we, dropped := shapers[0].Stats()
	// TEMPORARY exp24: read the association's own MTU and SCTP-level byte
	// counters so the datagram size is corroborated by a second, independent
	// instrument (pion's state, not the shim's).
	for i, c := range conns {
		if os.Getenv("SB_SHIM_HIST") == "" {
			break
		}
		st := c.pc.SCTP().Stats()
		fmt.Fprintf(os.Stderr, "exp24 sctp conn=%d mtu=%d bytes_sent=%d bytes_recv=%d cwnd=%d\n", i, st.MTU, st.BytesSent, st.BytesReceived, st.CongestionWindow)
	}
	fmt.Fprintf(os.Stderr, "trace %s conns=%d rtt=%d loss=%v mbps/100ms:%s | shim fwd=%d writeErr=%d drop=%d | chrome_cpu=%.2fs (%.2f cores) go_cpu=%.2fs\n",
		cfg.mode, n, cfg.rttMs, cfg.loss, sb.String(), fw, we, dropped, chromeSec, chromeCores, goCPU)
	return res, nil
}
