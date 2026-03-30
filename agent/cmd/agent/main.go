package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"sync"

	"github.com/pion/webrtc/v4"
	"opencloudshare/agent/internal/config"
	"opencloudshare/agent/internal/peer"
	"opencloudshare/agent/internal/signaling"
)

func main() {
	cfg := config.Load()
	if cfg.ShareURL == "" {
		log.Fatal("SHARE_URL env var is required")
	}

	ctx := context.Background()

	sig := signaling.New(cfg.SignalingServer, cfg.AuthToken)
	if err := sig.Connect(ctx); err != nil {
		log.Fatalf("connect to signaling server: %v", err)
	}
	log.Printf("connected to signaling server at %s", cfg.SignalingServer)

	code, err := sig.CreateSession(ctx, cfg.ShareURL, "24h")
	if err != nil {
		log.Fatalf("create session: %v", err)
	}
	log.Printf("session ready — code: %s", code)
	log.Printf("open browser: http://localhost:8080  then enter code: %s", code)

	var (
		mu    sync.Mutex
		peers = make(map[string]*peer.Peer)
	)

	sig.OnMessage = func(msg signaling.Message) {
		switch msg.Type {
		case "registered":
			log.Println("agent registered with signaling server")

		case "join":
			log.Printf("browser joined session %s — starting WebRTC handshake", msg.SessionID)
			sessionID := msg.SessionID

			p, err := peer.New([]webrtc.ICEServer{
				{URLs: []string{"stun:stun.cloudflare.com:3478"}},
			})
			if err != nil {
				log.Printf("create peer: %v", err)
				return
			}

			mu.Lock()
			peers[sessionID] = p
			mu.Unlock()

			p.OnOpen = func() {
				log.Printf("✓ DataChannel open! (session %s)", sessionID)
			}
			p.OnClosed = func() {
				log.Printf("peer closed (session %s)", sessionID)
				mu.Lock()
				delete(peers, sessionID)
				mu.Unlock()
			}
			p.OnICECandidate = func(init webrtc.ICECandidateInit) {
				if err := sig.Send(ctx, map[string]any{
					"type":       "ice_candidate",
					"session_id": sessionID,
					"candidate":  init,
				}); err != nil {
					log.Printf("send ICE candidate: %v", err)
				}
			}

			sdp, err := p.CreateOffer()
			if err != nil {
				log.Printf("create offer: %v", err)
				return
			}
			if err := sig.Send(ctx, map[string]any{
				"type":       "offer",
				"session_id": sessionID,
				"sdp":        sdp,
			}); err != nil {
				log.Printf("send offer: %v", err)
			}

		case "answer":
			mu.Lock()
			p, ok := peers[msg.SessionID]
			mu.Unlock()
			if !ok {
				return
			}
			if err := p.SetAnswer(msg.SDP); err != nil {
				log.Printf("set answer: %v", err)
			}

		case "ice_candidate":
			mu.Lock()
			p, ok := peers[msg.SessionID]
			mu.Unlock()
			if !ok {
				return
			}
			var init webrtc.ICECandidateInit
			if err := json.Unmarshal(msg.Candidate, &init); err != nil {
				log.Printf("parse ICE candidate: %v", err)
				return
			}
			if err := p.AddICECandidate(init); err != nil {
				log.Printf("add ICE candidate: %v", err)
			}

		case "error":
			log.Printf("signaling error: %s", msg.Err)
		}
	}

	log.Println("waiting for browser connections (Ctrl-C to stop)...")
	if err := sig.Listen(ctx); err != nil {
		log.Printf("signaling disconnected: %v", err)
		os.Exit(1)
	}
}
