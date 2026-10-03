package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
)

// runConfig holds everything run needs. peerFactory is nil in production.
type runConfig struct {
	signalURL   string
	code        string
	path        string
	password    string
	duration    time.Duration
	insecure    bool
	requestID   string
	dumpControl bool
	peerFactory peerFactory
	// maxRxBuf, when > 0, overrides pion's default 1 MiB SCTP advertised
	// receive window (sctp.initialRecvBufSize). Diagnostic only: the stock
	// binary leaves this at 0 and keeps pion's default.
	maxRxBuf uint32
}

// runResult is the machine-parseable outcome of one receive attempt.
type runResult struct {
	PayloadBytes int64
	WireBytes    int64
	Frames       int64
	BadFrames    int64
	HeaderSize   int64
	WallSeconds  float64
	Mbps         float64
	Status       string
	SelectedPair string
}

func (res runResult) summaryLine() string {
	return fmt.Sprintf(
		"SUMMARY payload_bytes=%d wire_bytes=%d frames=%d bad_frames=%d header_bytes=%d wall_s=%.3f mbps=%.3f status=%s selected=%q",
		res.PayloadBytes, res.WireBytes, res.Frames, res.BadFrames, res.HeaderSize,
		res.WallSeconds, res.Mbps, res.Status, res.SelectedPair,
	)
}

// run performs one full browser-role session: signaling join, pion answer,
// transport handshake, file request, and bulk-lane byte counting.
func run(ctx context.Context, cfg runConfig) (runResult, error) {
	var res runResult
	if cfg.duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.duration)
		defer cancel()
	}
	if cfg.requestID == "" {
		cfg.requestID = "fieldrecv"
	}

	client, err := dialBrowser(ctx, cfg.signalURL, cfg.code, cfg.insecure)
	if err != nil {
		return res, err
	}
	defer client.close()

	// Keep the signaling socket alive through Cloudflare's 100s idle timeout.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = client.ping(ctx)
			}
		}
	}()

	var (
		recvPtr    atomic.Pointer[receiver]
		headerSize atomic.Int64
		listSent   atomic.Bool
		startedAt  atomic.Int64
	)

	doneCh := make(chan string, 4)
	finished := func(status string) {
		select {
		case doneCh <- status:
		default:
		}
	}

	send := func(msg any) {
		if err := client.send(ctx, msg); err != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "fieldrecv: signaling send failed: %v\n", err)
		}
	}

	triggerRequest := func(path string) {
		r := recvPtr.Load()
		if r == nil {
			return
		}
		if path == "" {
			if listSent.CompareAndSwap(false, true) {
				_ = r.SendControl(map[string]any{"type": "list_request", "path": "", "request_id": cfg.requestID})
			}
			return
		}
		// Let the agent's onReady install the transfer manager before the
		// first application message arrives.
		time.Sleep(100 * time.Millisecond)
		if err := r.SendControl(map[string]any{"type": "file_request", "path": path, "request_id": cfg.requestID}); err != nil {
			fmt.Fprintf(os.Stderr, "fieldrecv: file_request send failed: %v\n", err)
		}
	}

	onControl := func(data []byte) {
		if cfg.dumpControl {
			fmt.Fprintf(os.Stderr, "CONTROL %s\n", string(data))
		}
		var msg struct {
			Type        string `json:"type"`
			Scope       string `json:"scope"`
			Name        string `json:"name"`
			Size        int64  `json:"size"`
			OperationID string `json:"operation_id"`
			Message     string `json:"message"`
			BytesSent   string `json:"bytes_sent"`
			Files       []struct {
				Name  string `json:"name"`
				Size  int64  `json:"size"`
				IsDir bool   `json:"isDir"`
			} `json:"files"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			return
		}
		switch msg.Type {
		case "file_list":
			if cfg.path != "" {
				return // explicit path: request already sent
			}
			best, bestSize := "", int64(-1)
			for _, f := range msg.Files {
				if f.IsDir {
					continue
				}
				if f.Size > bestSize {
					best, bestSize = f.Name, f.Size
				}
			}
			if best == "" {
				finished("error: no downloadable files in share")
				return
			}
			fmt.Fprintf(os.Stderr, "fieldrecv: file_list selected %q (%d bytes)\n", best, bestSize)
			go triggerRequest(best)
		case "file_header":
			if msg.Scope != "" && msg.Scope != "bulk" {
				return
			}
			headerSize.Store(msg.Size)
			startedAt.CompareAndSwap(0, time.Now().UnixNano())
			fmt.Fprintf(os.Stderr, "fieldrecv: file_header name=%q size=%d operation_id=%s\n", msg.Name, msg.Size, msg.OperationID)
		case "chunk_end":
			if msg.Scope != "bulk" {
				return
			}
			expected, _ := strconv.ParseInt(msg.BytesSent, 10, 64)
			if expected <= 0 {
				expected = headerSize.Load()
			}
			// chunk_end travels on control, so it can overtake the final bulk
			// frames. Wait for the counted payload to catch up before finishing.
			go func(expected int64) {
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					if r := recvPtr.Load(); r != nil {
						payload, _, _, _ := r.Stats()
						if expected <= 0 || payload >= expected {
							break
						}
					}
					time.Sleep(20 * time.Millisecond)
				}
				finished("complete")
			}(expected)
		case "error":
			finished("error: " + msg.Message)
		}
	}

	errCh := make(chan error, 1)
	go func() {
		for {
			msg, err := client.read(ctx)
			if err != nil {
				if ctx.Err() == nil {
					errCh <- fmt.Errorf("signaling read: %w", err)
				}
				return
			}
			switch msg.Type {
			case "ice_config":
				iceServers := make([]webrtc.ICEServer, 0, len(msg.ICEServers))
				for _, s := range msg.ICEServers {
					iceServers = append(iceServers, webrtc.ICEServer{URLs: s.URLs, Username: s.Username, Credential: s.Credential})
				}
				config := webrtc.Configuration{ICEServers: iceServers}
				if msg.RelayOnly {
					config.ICETransportPolicy = webrtc.ICETransportPolicyRelay
				}
				factory := cfg.peerFactory
				if factory == nil && cfg.maxRxBuf > 0 {
					se := webrtc.SettingEngine{}
					se.SetSCTPMaxReceiveBufferSize(cfg.maxRxBuf)
					api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
					factory = func(c webrtc.Configuration) (*webrtc.PeerConnection, error) {
						return api.NewPeerConnection(c)
					}
					fmt.Fprintf(os.Stderr, "fieldrecv: DIAGNOSTIC max_rx_buf=%d (pion default would be 1048576)\n", cfg.maxRxBuf)
				}
				r, err := newReceiver(factory, config)
				if err != nil {
					errCh <- err
					return
				}
				r.SetOnCandidate(func(init webrtc.ICECandidateInit) {
					send(map[string]any{"type": "ice_candidate", "candidate": init})
				})
				r.SetOnControl(onControl)
				r.SetOnStateChange(func(state webrtc.PeerConnectionState) {
					fmt.Fprintf(os.Stderr, "fieldrecv: peer connection state=%s\n", state)
				})
				recvPtr.Store(r)
				fmt.Fprintf(os.Stderr, "fieldrecv: ice_config received (%d servers, relay_only=%v), sending knock\n", len(iceServers), msg.RelayOnly)
				send(map[string]any{"type": "knock"})
			case "nonce":
				fmt.Fprintf(os.Stderr, "fieldrecv: nonce received (has_password=%v), sending join\n", msg.HasPassword)
				send(map[string]any{"type": "join", "hmac": joinHMAC(cfg.password, msg.Value)})
			case "offer":
				r := recvPtr.Load()
				if r == nil {
					errCh <- fmt.Errorf("offer before ice_config")
					return
				}
				answer, err := r.handleOffer(msg.SDP)
				if err != nil {
					errCh <- err
					return
				}
				fmt.Fprintf(os.Stderr, "fieldrecv: offer answered\n")
				send(map[string]any{"type": "answer", "sdp": answer})
				go func() {
					select {
					case <-r.Ready():
						fmt.Fprintf(os.Stderr, "fieldrecv: transport_ready; requesting file\n")
						if cfg.path != "" {
							go triggerRequest(cfg.path)
						} else {
							go triggerRequest("")
						}
					case <-ctx.Done():
					}
				}()
			case "ice_candidate":
				if r := recvPtr.Load(); r != nil {
					if err := r.AddICECandidate(msg.Candidate); err != nil {
						fmt.Fprintf(os.Stderr, "fieldrecv: add candidate: %v\n", err)
					}
				}
			case "error":
				errCh <- fmt.Errorf("signaling error: %s", msg.Message)
				return
			case "auth_failed", "auth_fail":
				errCh <- fmt.Errorf("authentication failed")
				return
			}
		}
	}()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	start := time.Now()
	var lastPayload int64
	result := func(status string) runResult {
		r := recvPtr.Load()
		out := runResult{Status: status, WallSeconds: time.Since(start).Seconds()}
		if r != nil {
			out.PayloadBytes, out.WireBytes, out.Frames, out.BadFrames = r.Stats()
			out.SelectedPair = r.SelectedPair()
			if d, ok := r.FrameWindow(); ok {
				out.WallSeconds = d.Seconds()
			}
		}
		out.HeaderSize = headerSize.Load()
		if out.WallSeconds > 0 {
			out.Mbps = float64(out.PayloadBytes) * 8 / out.WallSeconds / 1e6
		}
		if err := r.Err(); err != nil && status == "complete" {
			out.Status = "error: " + err.Error()
		}
		return out
	}

	for {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return result("deadline"), nil
			}
			return result("cancelled"), nil
		case status := <-doneCh:
			res = result(status)
			if res.Status == "complete" {
				return res, nil
			}
			return res, fmt.Errorf("%s", res.Status)
		case err := <-errCh:
			res = result("error")
			return res, err
		case <-ticker.C:
			var payload, wire, frames, bad int64
			if r := recvPtr.Load(); r != nil {
				payload, wire, frames, bad = r.Stats()
				if err := r.Err(); err != nil {
					res = result("error")
					return res, err
				}
			}
			inst := float64(payload-lastPayload) * 8 / 1e6
			lastPayload = payload
			fmt.Fprintf(os.Stderr,
				"PROGRESS t=%.1f payload_bytes=%d wire_bytes=%d frames=%d bad=%d mbps_inst=%.3f\n",
				time.Since(start).Seconds(), payload, wire, frames, bad, inst)
		}
	}
}
