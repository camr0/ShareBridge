package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/spf13/cobra"
	"sharebridge/agent/internal/cloudwebdav"
	"sharebridge/agent/internal/config"
	"sharebridge/agent/internal/daemon"
	"sharebridge/agent/internal/peer"
	"sharebridge/agent/internal/signaling"
	"sharebridge/agent/internal/store"
	"sharebridge/agent/internal/transfer"
	"sharebridge/agent/internal/web"
)

var (
	password     string
	maxDownloads int
	expiryHours  int
	relayOnly    bool
)

// nonceEntry holds a per-connection nonce for HMAC pre-challenge (standalone mode).
type nonceEntry struct {
	nonce     string
	expiresAt time.Time
}

func init() {
	rootCmd.AddCommand(shareCmd)
	rootCmd.AddCommand(daemonCmd)

	shareCmd.Flags().StringVarP(&password, "password", "p", "", "Optional password for share access")
	shareCmd.Flags().IntVarP(&maxDownloads, "max-downloads", "n", 0, "Maximum number of downloads (0=unlimited)")
	shareCmd.Flags().IntVarP(&expiryHours, "expiry", "e", 24, "Expiry time in hours")
	shareCmd.Flags().BoolVarP(&relayOnly, "relay", "r", false, "Force relay-only mode (TURN required)")

	daemonCmd.Flags().StringVarP(&password, "password", "p", "", "Password for web UI (optional)")
}

var rootCmd = &cobra.Command{
	Use:   "sharebridge",
	Short: "ShareBridge agent for secure file sharing",
}

var shareCmd = &cobra.Command{
	Use:   "share [SHARE_URL]",
	Short: "Share files from a WebDAV URL",
	Args:  cobra.ExactArgs(1),
	RunE:  runShare,
}

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Start the persistent daemon with web UI",
	RunE:  runDaemon,
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// runDaemon starts the persistent daemon with web UI.
func runDaemon(cmd *cobra.Command, args []string) error {
	// Create config manager
	cfgMgr, err := config.NewManager()
	if err != nil {
		return fmt.Errorf("create config manager: %w", err)
	}
	cfg := cfgMgr.Get()

	// Validate API key is set
	if cfg.APIKey == "" {
		return fmt.Errorf("API key required — set in config.json or SHAREBRIDGE_API_KEY env var")
	}

	// Create store
	st, err := store.New()
	if err != nil {
		return fmt.Errorf("session store: %w", err)
	}

	// Create daemon
	d, err := daemon.New(cfgMgr, st)
	if err != nil {
		return fmt.Errorf("create daemon: %w", err)
	}

	// Create web server
	uiAddr := cfg.UIAddr
	uiPort := cfg.UIPort
	uiPassword := cfg.UIPassword
	if uiPassword == "" {
		uiPassword = password // Allow CLI override
	}

	webServer, err := web.NewWebServer(nil, uiAddr, uiPort, uiPassword)
	if err != nil {
		return fmt.Errorf("create web server: %w", err)
	}

	// Set up circular reference
	d.SetWebServer(webServer)
	webServer.SetDaemon(d)

	// Handle Ctrl-C gracefully
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// Start daemon
	errChan := d.Start(ctx)

	log.Printf("daemon started — web UI at http://%s:%d", uiAddr, uiPort)

	// Wait for interrupt or error
	select {
	case <-ctx.Done():
		log.Println("shutting down...")
	case err := <-errChan:
		log.Printf("daemon error: %v", err)
	}

	// Stop gracefully
	if err := d.Stop(); err != nil {
		log.Printf("stop error: %v", err)
	}

	return nil
}

// runShare shares a file URL, either by delegating to a running daemon
// or by running in single-session mode.
func runShare(cmd *cobra.Command, args []string) error {
	shareURL := args[0]

	// Check if daemon is running
	daemonURL := "http://127.0.0.1:7878/api/status"
	client := &http.Client{Timeout: 2 * time.Second}

	resp, err := client.Get(daemonURL)
	if err == nil && resp.StatusCode == http.StatusOK {
		resp.Body.Close()
		// Daemon is running — delegate to it
		return runShareClient(shareURL)
	}
	if resp != nil {
		resp.Body.Close()
	}

	// Daemon not running — run in single-session mode
	return runShareSingle(shareURL)
}

// runShareClient sends a create-share request to the running daemon.
func runShareClient(shareURL string) error {
	// Build form data
	formData := url.Values{}
	formData.Set("share_url", shareURL)
	if password != "" {
		formData.Set("password", password)
	}
	formData.Set("expiry_hours", strconv.Itoa(expiryHours))
	formData.Set("max_downloads", strconv.Itoa(maxDownloads))
	if relayOnly {
		formData.Set("relay_only", "true")
	}

	// POST to daemon API
	daemonURL := "http://127.0.0.1:7878/api/shares"
	client := &http.Client{Timeout: 10 * time.Second}

	req, err := http.NewRequest("POST", daemonURL, bytes.NewBufferString(formData.Encode()))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "XMLHttpRequest") // CSRF header

	// Add basic auth if UI password is set
	uiPassword := os.Getenv("UI_PASSWORD")
	if uiPassword != "" {
		req.SetBasicAuth("", uiPassword)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("daemon request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("daemon error (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// Parse the response HTML to extract the share code
	// The response is a share-card HTML fragment
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	// Extract code from HTML (simple parsing)
	// Looking for data-code attribute in share-card
	code := extractCodeFromHTML(string(body))
	if code == "" {
		// If we couldn't extract, just show success
		fmt.Printf("Share created — check daemon web UI at http://127.0.0.1:7878\n")
	} else {
		fmt.Printf("Session ready — code: %s\n", code)
		fmt.Printf("Open http://localhost:8080 and enter code: %s\n", code)
	}

	return nil
}

// extractCodeFromHTML extracts the share code from share-card HTML.
func extractCodeFromHTML(html string) string {
	// Look for <h3 style="margin: 0;">CODE</h3>
	h3Start := strings.Index(html, "<h3")
	if h3Start == -1 {
		return ""
	}
	contentStart := strings.Index(html[h3Start:], ">")
	if contentStart == -1 {
		return ""
	}
	contentStart += h3Start + 1

	h3End := strings.Index(html[contentStart:], "</h3>")
	if h3End == -1 {
		return ""
	}

	return strings.TrimSpace(html[contentStart : contentStart+h3End])
}

// runShareSingle runs the share in single-session mode without daemon.
func runShareSingle(shareURL string) error {
	cfg := config.Load()
	cfg.Password = password
	cfg.MaxDownloads = maxDownloads

	// Validate API key is set
	if cfg.APIKey == "" {
		return fmt.Errorf("SHAREBRIDGE_API_KEY environment variable required")
	}

	webdavClient, err := cloudwebdav.New(shareURL, []string{cfg.AllowedHost, cfg.NCAllowedHost}, password)
	if err != nil {
		return fmt.Errorf("create WebDAV client: %w", err)
	}

	st, err := store.New()
	if err != nil {
		return fmt.Errorf("session store: %w", err)
	}

	// Get AgentID
	agentID := st.GetAgentID()

	// Check for existing session for this share URL
	var preferredCode string
	existingSession := st.GetByShareURL(shareURL)
	if existingSession != nil {
		preferredCode = existingSession.Code
		log.Printf("found existing session for this URL — code: %s", preferredCode)
	}

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

		code, err := runSession(ctx, cfg, webdavClient, shareURL, st, preferredCode, agentID, relayOnly)
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

func runSession(ctx context.Context, cfg *config.Config, webdavClient *cloudwebdav.Client, shareURL string, st *store.Store, preferredCode string, agentID string, relayOnly bool) (string, error) {
	sig := signaling.New(cfg.SignalingURL, cfg.APIKey, agentID)

	if err := sig.Connect(ctx); err != nil {
		return "", fmt.Errorf("connect to signaling server: %w", err)
	}
	log.Printf("connected to signaling server at %s", cfg.SignalingURL)

	// Use new RegisterShare instead of CreateSession
	code, reconnected, err := sig.RegisterShare(ctx, shareURL, preferredCode, relayOnly)
	if err != nil {
		return "", fmt.Errorf("register share: %w", err)
	}
	if reconnected {
		log.Printf("session reclaimed — code: %s", code)
	} else {
		log.Printf("session ready — code: %s", code)
	}
	log.Printf("open browser: http://localhost:8080 then enter code: %s", code)

	// Save session to store
	now := time.Now()
	session := store.SessionEntry{
		Code:         code,
		ShareURL:     shareURL,
		Password:     password,
		ExpiresAt:    now.Add(time.Duration(expiryHours) * time.Hour),
		MaxDownloads: maxDownloads,
		Downloads:    0,
		RelayOnly:    relayOnly,
		CreatedAt:    now,
	}
	if err := st.SaveSession(session); err != nil {
		log.Printf("warning: could not save session: %v", err)
	}

	var (
		mu       sync.Mutex
		peers    = make(map[string]*peer.Peer) // peerID -> Peer
		nonces   = make(map[string]nonceEntry) // connID -> nonce
		noncesMu sync.Mutex
	)

	sig.OnMessage = func(msg signaling.Message) {
		switch msg.Type {
		case "welcome":
			log.Println("agent authenticated with signaling server")

		case "knock":
			connID := msg.ConnID
			// msg.Code is the session code — not needed for knock handling

			// Generate 32 random bytes → 64-char hex nonce
			nonceBytes := make([]byte, 32)
			if _, err := rand.Read(nonceBytes); err != nil {
				log.Printf("generate nonce: %v", err)
				return
			}
			nonce := hex.EncodeToString(nonceBytes)

			// Sweep expired nonces and store new one
			noncesMu.Lock()
			now := time.Now()
			for id, entry := range nonces {
				if now.After(entry.expiresAt) {
					delete(nonces, id)
				}
			}
			nonces[connID] = nonceEntry{nonce: nonce, expiresAt: now.Add(60 * time.Second)}
			noncesMu.Unlock()

			// Send nonce back to browser
			if err := sig.Send(ctx, map[string]any{
				"type":         "nonce",
				"conn_id":      connID,
				"value":        nonce,
				"has_password": password != "",
			}); err != nil {
				log.Printf("send nonce: %v", err)
			}

		case "join":
			connID := msg.ConnID
			sessionCode := msg.Code
			receivedHMAC := msg.HMAC

			// Atomically delete nonce entry before verifying
			noncesMu.Lock()
			entry, found := nonces[connID]
			delete(nonces, connID)
			noncesMu.Unlock()

			if !found || time.Now().After(entry.expiresAt) {
				log.Printf("join with expired/missing nonce: conn %s", connID)
				sig.Send(ctx, map[string]any{
					"type":    "auth_failed",
					"conn_id": connID,
				})
				return
			}

			// Verify HMAC for password-protected shares
			if password != "" {
				mac := hmac.New(sha256.New, []byte(password))
				mac.Write([]byte(entry.nonce))
				expectedMAC := mac.Sum(nil)

				receivedBytes, err := hex.DecodeString(receivedHMAC)
				if err != nil || !hmac.Equal(expectedMAC, receivedBytes) {
					log.Printf("HMAC mismatch for conn %s", connID)
					sig.Send(ctx, map[string]any{
						"type":    "auth_failed",
						"conn_id": connID,
					})
					return
				}
			}

			log.Printf("browser joined session %s (conn %s) — starting WebRTC handshake", sessionCode, connID)

			iceServers := sig.GetICEServers()
			if len(iceServers) == 0 {
				log.Printf("warning: no ICE servers received, using default STUN")
				iceServers = []webrtc.ICEServer{
					{URLs: []string{"stun:stun.cloudflare.com:3478"}},
				}
			}
			p, err := peer.New(iceServers, relayOnly)
			if err != nil {
				log.Printf("create peer: %v", err)
				return
			}

			mu.Lock()
			peers[connID] = p
			mu.Unlock()

			p.OnClosed = func() {
				log.Printf("peer closed (conn %s, session %s)", connID, sessionCode)
				mu.Lock()
				delete(peers, connID)
				mu.Unlock()
			}
			p.OnICECandidate = func(init webrtc.ICECandidateInit) {
				if err := sig.Send(ctx, map[string]any{
					"type":       "ice_candidate",
					"session_id": sessionCode,
					"peer_id":    connID,
					"candidate":  init,
				}); err != nil {
					log.Printf("send ICE candidate: %v", err)
				}
			}

			// Get persisted download count from store
			existingSession := st.GetSession(code)
			downloadCount := 0
			if existingSession != nil {
				downloadCount = existingSession.Downloads
			}

			tm := transfer.NewManager(p, webdavClient, cfg.MaxDownloads)
			tm.SetDownloadCount(downloadCount)

			// Wire callbacks before CreateOffer to avoid any race with a fast peer
			tm.OnSessionExpired = func() {
				if err := sig.Send(ctx, map[string]any{
					"type":       "session_expired",
					"session_id": sessionCode,
					"peer_id":    connID,
				}); err != nil {
					log.Printf("send session_expired: %v", err)
				}
			}
			tm.OnDownloadComplete = func(bytesTransferred int64) {
				// Persist download count
				if _, err := st.IncrementDownloads(code); err != nil {
					log.Printf("warning: could not persist download count: %v", err)
				}
				// Notify server for tier tracking
				if err := sig.DownloadComplete(ctx, code, bytesTransferred); err != nil {
					log.Printf("warning: could not send download_complete: %v", err)
				}
			}
			p.OnOpen = func() {
				log.Printf("DataChannel open! (conn %s, session %s)", connID, sessionCode)
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
				"session_id": sessionCode,
				"peer_id":    connID,
				"sdp":        sdp,
			}); err != nil {
				log.Printf("send offer: %v", err)
			}

		case "answer":
			mu.Lock()
			p, ok := peers[msg.PeerID]
			mu.Unlock()
			if !ok {
				log.Printf("answer for unknown peer %s", msg.PeerID)
				return
			}
			if err := p.SetAnswer(msg.SDP); err != nil {
				log.Printf("set answer: %v", err)
			}

		case "ice_candidate":
			mu.Lock()
			p, ok := peers[msg.PeerID]
			mu.Unlock()
			if !ok {
				log.Printf("ICE candidate for unknown peer %s", msg.PeerID)
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
