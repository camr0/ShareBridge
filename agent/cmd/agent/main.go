package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/spf13/cobra"
	"opencloudshare/agent/internal/config"
	"opencloudshare/agent/internal/opencloud"
	"opencloudshare/agent/internal/peer"
	"opencloudshare/agent/internal/signaling"
	"opencloudshare/agent/internal/store"
	"opencloudshare/agent/internal/transfer"
)

var (
	password     string
	maxDownloads int
)

func init() {
	rootCmd.AddCommand(shareCmd)
	shareCmd.Flags().StringVarP(&password, "password", "p", "", "Optional password for share access")
	shareCmd.Flags().IntVarP(&maxDownloads, "max-downloads", "n", 0, "Maximum number of downloads (0=unlimited)")
}

var rootCmd = &cobra.Command{
	Use:   "opencloudshare",
	Short: "OpenCloudShare agent for secure file sharing",
}

var shareCmd = &cobra.Command{
	Use:   "share [SHARE_URL]",
	Short: "Share files from a WebDAV URL",
	Args:  cobra.ExactArgs(1),
	RunE:  runShare,
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runShare(cmd *cobra.Command, args []string) error {
	shareURL := args[0]

	cfg := config.Load()
	cfg.Password = password
	cfg.MaxDownloads = maxDownloads

	webdavClient, err := opencloud.New(shareURL, cfg.AllowedHost, password)
	if err != nil {
		return fmt.Errorf("create WebDAV client: %w", err)
	}

	st, err := store.New()
	if err != nil {
		return fmt.Errorf("session store: %w", err)
	}

	// Load persisted state before reconnect loop
	preferredCode := st.GetCode(shareURL)

	// Handle Ctrl-C gracefully
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	backoff := signaling.NewBackoff()

	for {
		select {
		case <-ctx.Done():
			log.Println("shutting down...")
			return nil
		default:
		}

		code, err := runSession(ctx, cfg, webdavClient, shareURL, st, preferredCode)
		if err != nil {
			log.Printf("session ended: %v", err)
		} else {
			backoff.Reset()
		}
		// Update preferredCode for next reconnect
		if code != "" {
			preferredCode = code
		}

		// Check if context was cancelled before retrying
		select {
		case <-ctx.Done():
			log.Println("shutting down...")
			return nil
		default:
		}

		delay := backoff.Next()
		log.Printf("reconnecting in %v...", delay)

		select {
		case <-ctx.Done():
			log.Println("shutting down...")
			return nil
		case <-time.After(delay):
			// Continue to retry
		}
	}
}

func runSession(ctx context.Context, cfg *config.Config, webdavClient *opencloud.Client, shareURL string, st *store.Store, preferredCode string) (string, error) {
	sig := signaling.New(cfg.SignalingServer, cfg.AuthToken)

	if err := sig.Connect(ctx); err != nil {
		return "", fmt.Errorf("connect to signaling server: %w", err)
	}
	log.Printf("connected to signaling server at %s", cfg.SignalingServer)

	code, err := sig.CreateSession(ctx, shareURL, preferredCode)
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	log.Printf("session ready — code: %s", code)
	log.Printf("open browser: http://localhost:8080  then enter code: %s", code)

	if err := st.SetCode(shareURL, code); err != nil {
		return code, fmt.Errorf("save session code: %w", err)
	}

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

			tm := transfer.NewManager(p, webdavClient, cfg.Password, cfg.MaxDownloads)

			// Restore persisted download count
			tm.SetDownloadCount(st.GetDownloadCount(shareURL))
			// Wire persistence callback
			tm.OnDownloadComplete = func() {
				if _, err := st.IncrementDownloadCount(shareURL); err != nil {
					log.Printf("warning: could not persist download count: %v", err)
				}
			}

			// Wire callbacks before CreateOffer to avoid any race with a fast peer
			tm.OnAuthFailed = func() {
				if err := sig.Send(ctx, map[string]any{
					"type":       "auth_failed",
					"session_id": sessionID,
				}); err != nil {
					log.Printf("send auth_failed: %v", err)
				}
			}
			tm.OnSessionExpired = func() {
				if err := sig.Send(ctx, map[string]any{
					"type":       "session_expired",
					"session_id": sessionID,
				}); err != nil {
					log.Printf("send session_expired: %v", err)
				}
			}
			p.OnOpen = func() {
				log.Printf("✓ DataChannel open! (session %s)", sessionID)
				tm.HandleOpen()
			}

			// CreateOffer creates the DataChannel internally — SetOnMessage must come after
			sdp, err := p.CreateOffer()
			if err != nil {
				log.Printf("create offer: %v", err)
				return
			}
			p.SetOnMessage(tm.HandleMessage)

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
		return code, fmt.Errorf("signaling disconnected: %w", err)
	}

	return code, nil
}
