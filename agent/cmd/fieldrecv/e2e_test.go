package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
	"sharebridge/agent/internal/cloudwebdav"
	"sharebridge/agent/internal/peer"
	"sharebridge/agent/internal/transfer"
)

type testStorage struct {
	name string
	size int64
}

func (s testStorage) ListFiles(string) ([]cloudwebdav.FileInfo, error) {
	return []cloudwebdav.FileInfo{{Name: s.name, Size: s.size, RequestPath: s.name}}, nil
}

func (s testStorage) GetFile(string, io.Writer) (int64, error) {
	return s.size, nil
}

func (s testStorage) GetSHA1(string) string { return "" }

// blockingStorage streams size bytes so the production transfer.Manager emits
// many real bulk frames.
type blockingStorage struct{ testStorage }

func (s blockingStorage) GetFile(_ string, w io.Writer) (int64, error) {
	buf := make([]byte, 16*1024)
	var total int64
	for total < s.size {
		n := int64(len(buf))
		if rem := s.size - total; rem < n {
			n = rem
		}
		if _, err := w.Write(buf[:n]); err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// fakeSignal plays the v1 signaling server's browser role and hosts the real
// agent-side offerer (peer.Peer + production transfer.Manager).
type fakeSignal struct {
	t    *testing.T
	size int64

	mu        sync.Mutex
	conn      *websocket.Conn
	pc        *peer.Peer
	remoteSet bool
	pending   []webrtc.ICECandidateInit
}

func (f *fakeSignal) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	f.mu.Lock()
	f.conn = conn
	f.mu.Unlock()

	f.write(map[string]any{"type": "ice_config", "ice_servers": []any{}, "relay_only": false})

	for {
		_, data, err := conn.Read(context.Background())
		if err != nil {
			return
		}
		var m browserMessage
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		switch m.Type {
		case "knock":
			f.write(map[string]any{"type": "nonce", "value": "testnonce", "has_password": false})
		case "join":
			p, err := peer.New(nil, false)
			if err != nil {
				f.t.Errorf("peer.New: %v", err)
				return
			}
			p.SetOnICECandidate(func(init webrtc.ICECandidateInit) {
				f.write(map[string]any{"type": "ice_candidate", "candidate": init})
			})
			p.SetOnOpen(func() {
				// Production send path, exactly as daemon.activateTransferSession.
				_ = transfer.NewManager(p, blockingStorage{testStorage{name: "bench.bin", size: f.size}}, 0)
			})
			f.mu.Lock()
			f.pc = p
			f.mu.Unlock()
			offer, err := p.CreateOffer()
			if err != nil {
				f.t.Errorf("CreateOffer: %v", err)
				return
			}
			f.write(map[string]any{"type": "offer", "sdp": offer})
		case "answer":
			f.mu.Lock()
			p, pending := f.pc, f.pending
			f.pending = nil
			f.remoteSet = true
			f.mu.Unlock()
			if p == nil {
				continue
			}
			if err := p.SetAnswer(m.SDP); err != nil {
				f.t.Errorf("SetAnswer: %v", err)
			}
			for _, c := range pending {
				_ = p.AddICECandidate(c)
			}
		case "ice_candidate":
			var init webrtc.ICECandidateInit
			if err := json.Unmarshal(m.Candidate, &init); err != nil {
				continue
			}
			f.mu.Lock()
			p, remoteSet := f.pc, f.remoteSet
			if !remoteSet {
				f.pending = append(f.pending, init)
			}
			f.mu.Unlock()
			if remoteSet && p != nil {
				_ = p.AddICECandidate(init)
			}
		}
	}
}

func (f *fakeSignal) write(msg any) {
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	if conn == nil {
		return
	}
	_ = conn.Write(context.Background(), websocket.MessageText, b)
}

func TestRunEndToEndProductionSendPath(t *testing.T) {
	const size = 2*1024*1024 + 12345

	fake := &fakeSignal{t: t, size: size}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Deterministic in-process ICE: the receiver advertises a loopback host
	// candidate and does not depend on mDNS resolution.
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	factory := func(cfg webrtc.Configuration) (*webrtc.PeerConnection, error) {
		return api.NewPeerConnection(cfg)
	}

	res, err := run(ctx, runConfig{
		signalURL:   wsURL,
		code:        "TESTSESS",
		path:        "bench.bin",
		duration:    45 * time.Second,
		requestID:   "e2e",
		peerFactory: factory,
	})
	if err != nil {
		t.Fatalf("run: %v (result %+v)", err, res)
	}
	if res.Status != "complete" {
		t.Fatalf("status = %q, want complete", res.Status)
	}
	if res.PayloadBytes != size {
		t.Fatalf("payload = %d, want %d", res.PayloadBytes, size)
	}
	if res.Frames == 0 {
		t.Fatalf("no frames counted")
	}
	if res.BadFrames != 0 {
		t.Fatalf("bad frames = %d, want 0", res.BadFrames)
	}
	if res.WireBytes != res.PayloadBytes+chunkEnvelopeBytes*res.Frames {
		t.Fatalf("accounting mismatch: wire=%d payload=%d frames=%d (expected wire=%d)",
			res.WireBytes, res.PayloadBytes, res.Frames, res.PayloadBytes+chunkEnvelopeBytes*res.Frames)
	}
	t.Logf("e2e ok: %s", res.summaryLine())
}
