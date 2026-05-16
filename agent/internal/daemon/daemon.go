package daemon

import (
	"context"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"sharebridge/agent/internal/cloudwebdav"
	"sharebridge/agent/internal/config"
	"sharebridge/agent/internal/immich"
	"sharebridge/agent/internal/peer"
	"sharebridge/agent/internal/relaychannel"
	"sharebridge/agent/internal/signaling"
	"sharebridge/agent/internal/store"
	"sharebridge/agent/internal/transfer"
)

// nonceEntry holds a per-connection nonce for HMAC pre-challenge.
type nonceEntry struct {
	nonce     string
	expiresAt time.Time
}

// relayTransferChannel is the interface for relay transfer channels.
// It matches transfer.DataChannel plus lifecycle methods.
type relayTransferChannel interface {
	Start(ctx context.Context) error
	SendBinary(data []byte) error
	SendText(text string) error
	BufferedAmount() uint64
	Close() error
	SetOnMessage(handler func([]byte))
	SetOnOpen(handler func())
	SetOnClose(handler func())
}

// relayChannelConfig holds configuration for creating a relay channel.
type relayChannelConfig struct {
	RelayURL      string
	RelayJWT      string
	StaticPrivate []byte
}

type immichAuthenticator interface {
	ValidatePassword(ctx context.Context, password string) (bool, error)
}

type immichGalleryBackend interface {
	immichAuthenticator
}

type immichPoller interface {
	PollShares(ctx context.Context) ([]immich.SharedLink, error)
}

type shareOptionRegistrar interface {
	RegisterShareWithOptions(ctx context.Context, opts signaling.RegisterShareOptions) (string, bool, error)
}

type shareUnregistrar interface {
	UnregisterShare(ctx context.Context, code string) error
}

// WebServer is the interface for the admin UI web server.
// This interface avoids a circular import between daemon and web packages.
type WebServer interface {
	Start(ctx context.Context) error
	Stop() error
	SetDaemon(d *Daemon)
}

// ConfigManagerInterface defines the interface for config management.
type ConfigManagerInterface interface {
	Get() *config.Config
}

// StoreInterface defines the interface for session storage.
type StoreInterface interface {
	GetAgentID() string
	GetRelayStaticPrivateKey() ([]byte, error)
	GetSession(code string) *store.SessionEntry
	GetByShareURL(shareURL string) *store.SessionEntry
	ListSessions(filterExpired bool) []store.SessionEntry
	SaveSession(session store.SessionEntry) error
	DeleteSession(code string) error
	IncrementDownloads(code string) (int, error)
}

// SignalingClientInterface defines the interface for signaling client.
type SignalingClientInterface interface {
	Connect(ctx context.Context) error
	RegisterShare(ctx context.Context, shareURL, preferredCode string, relayOnly bool, relayStaticPub string) (string, bool, error)
	DownloadComplete(ctx context.Context, code string, bytesTransferred int64) error
	Send(ctx context.Context, msg any) error
	GetICEServers() []webrtc.ICEServer
	Listen(ctx context.Context) error
	SetOnMessage(handler func(signaling.Message))
}

// Session represents an active share session with WebRTC peers.
type Session struct {
	Code         string
	ShareURL     string
	ShareType    string
	FileID       string // oc:fileid extracted via WebDAV PROPFIND on share root
	Password     string
	ExpiresAt    time.Time
	MaxDownloads int
	Downloads    int
	RelayOnly    bool
	CreatedAt    time.Time

	IsPasswordProtected bool
	immichClient        immichGalleryBackend
	webdavClient        *cloudwebdav.Client
	peers               map[string]*peer.Peer           // peerID -> Peer
	relayChannels       map[string]relayTransferChannel // sid -> relay channel
	mu                  sync.Mutex
}

// Daemon manages multiple concurrent sessions, a single signaling connection,
// and the web server.
type Daemon struct {
	config    *config.Config
	configMgr ConfigManagerInterface
	store     StoreInterface
	signaling SignalingClientInterface
	sessions  map[string]*Session // code -> Session
	mu        sync.RWMutex

	webServer          WebServer
	startTime          time.Time
	signalingConnected bool // true once welcome received
	hasTURN            bool

	// Nonce store for HMAC pre-challenge (connID -> nonce)
	nonces   map[string]nonceEntry
	noncesMu sync.Mutex

	// Factory for creating relay channels (injected for testing)
	newRelayChannel func(cfg relayChannelConfig) (relayTransferChannel, error)

	newImmichPoller func() (immichPoller, error)

	// Callbacks for external handling (e.g., web server refresh)
	OnSessionAdded   func(session *Session)
	OnSessionRemoved func(code string)
}

// New creates a new Daemon with the given config manager and store.
func New(cfgMgr ConfigManagerInterface, st StoreInterface) (*Daemon, error) {
	cfg := cfgMgr.Get()
	agentID := st.GetAgentID()

	sig := signaling.New(cfg.SignalingURL, cfg.APIKey, agentID)

	return &Daemon{
		config:    cfg,
		configMgr: cfgMgr,
		store:     st,
		signaling: sig,
		sessions:  make(map[string]*Session),
		nonces:    make(map[string]nonceEntry),
		startTime: time.Now(),
	}, nil
}

// NewWithSignaling creates a new Daemon with a custom signaling client.
// This is useful for testing or custom signaling implementations.
func NewWithSignaling(cfgMgr ConfigManagerInterface, st StoreInterface, sig SignalingClientInterface) (*Daemon, error) {
	cfg := cfgMgr.Get()

	return &Daemon{
		config:    cfg,
		configMgr: cfgMgr,
		store:     st,
		signaling: sig,
		sessions:  make(map[string]*Session),
		nonces:    make(map[string]nonceEntry),
	}, nil
}

// SetWebServer sets the web server instance. Called after web server creation
// to avoid circular import issues.
func (d *Daemon) SetWebServer(ws WebServer) {
	d.webServer = ws
}

// Start connects to signaling server, loads sessions from store, starts the
// web server, and begins the expiry pruner. Returns an error channel that
// emits errors from background goroutines.
func (d *Daemon) Start(ctx context.Context) <-chan error {
	errChan := make(chan error, 10)

	// Connect to signaling server
	if err := d.signaling.Connect(ctx); err != nil {
		errChan <- fmt.Errorf("connect to signaling server: %w", err)
		return errChan
	}
	log.Printf("connected to signaling server at %s", d.GetConfig().SignalingURL)

	// Set up message handler before starting listener
	d.signaling.SetOnMessage(d.handleSignalingMessage)

	// Start signaling listener before loading sessions — Listen is the sole
	// WebSocket reader, and RegisterShare (called during loadSessionsFromStore)
	// waits on a channel that Listen feeds. Starting it first avoids a
	// concurrent-read race that corrupts WebSocket frame boundaries.
	go func() {
		if err := d.signaling.Listen(ctx); err != nil {
			if !errors.Is(err, context.Canceled) {
				errChan <- fmt.Errorf("signaling listener: %w", err)
			}
		}
	}()

	// Load persisted sessions and re-register them
	d.loadSessionsFromStore(ctx)

	if d.config.ImmichURL != "" && d.config.ImmichAPIKey != "" {
		go d.runImmichPoller(ctx)
	}

	// Start expiry pruner
	go d.runExpiryPruner(ctx)

	// Start web server if configured
	if d.webServer != nil {
		go func() {
			if err := d.webServer.Start(ctx); err != nil {
				if !errors.Is(err, context.Canceled) {
					errChan <- fmt.Errorf("web server: %w", err)
				}
			}
		}()
	}

	return errChan
}

// Stop gracefully shuts down the daemon.
func (d *Daemon) Stop() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Close all peer connections and relay channels
	for _, session := range d.sessions {
		session.mu.Lock()
		for peerID, peerConn := range session.peers {
			if err := peerConn.Close(); err != nil {
				log.Printf("close peer %s: %v", peerID, err)
			}
		}
		for sid, rc := range session.relayChannels {
			if err := rc.Close(); err != nil {
				log.Printf("close relay channel %s: %v", sid, err)
			}
		}
		session.mu.Unlock()
	}

	// Stop web server
	if d.webServer != nil {
		if err := d.webServer.Stop(); err != nil {
			log.Printf("stop web server: %v", err)
		}
	}

	// Close signaling connection (this will stop the listener)
	// Note: signaling.Client doesn't have a Close method, but Listen will
	// return when context is cancelled

	return nil
}

// CreateSession creates a new share session and registers it with the
// signaling server. The shareType must be explicitly provided by the caller
// ("opencloud" or "nextcloud") - it is not derived from the URL.
func (d *Daemon) CreateSession(ctx context.Context, shareURL, shareType, password string, expiryDuration time.Duration, maxDownloads int, relayOnly bool) (string, error) {
	cfg := d.GetConfig()

	allowedHosts := []string{cfg.AllowedHost, cfg.NCAllowedHost}

	// Validate share URL against allowed hosts
	if cfg.AllowedHost != "" || cfg.NCAllowedHost != "" {
		_, err := cloudwebdav.New(shareType, shareURL, allowedHosts, password)
		if err != nil {
			return "", fmt.Errorf("validate share URL: %w", err)
		}
	}

	// Create WebDAV client
	webdavClient, err := cloudwebdav.New(shareType, shareURL, allowedHosts, password)
	if err != nil {
		return "", fmt.Errorf("create WebDAV client: %w", err)
	}

	// Extract oc:fileid from share root — best-effort; empty string on failure.
	fileID, err := webdavClient.GetRootFileID()
	if err != nil {
		log.Printf("warning: could not extract fileID for %s: %v", shareURL, err)
		fileID = ""
	}

	// Get relay static private key and derive public key
	relayStaticPriv, err := d.store.GetRelayStaticPrivateKey()
	if err != nil {
		return "", fmt.Errorf("get relay static key: %w", err)
	}
	relayStaticPub, err := RelayStaticPubHex(relayStaticPriv)
	if err != nil {
		return "", fmt.Errorf("derive relay static public key: %w", err)
	}

	// Register with signaling server (no preferred code for new sessions)
	code, reconnected, err := d.signaling.RegisterShare(ctx, shareURL, "", relayOnly, relayStaticPub)
	if err != nil {
		return "", fmt.Errorf("register share: %w", err)
	}

	now := time.Now()
	session := &Session{
		Code:         code,
		ShareURL:     shareURL,
		ShareType:    shareType,
		FileID:       fileID,
		Password:     password,
		ExpiresAt:    now.Add(expiryDuration),
		MaxDownloads: maxDownloads,
		Downloads:    0,
		RelayOnly:    relayOnly,
		CreatedAt:    now,
		webdavClient: webdavClient,
		peers:        make(map[string]*peer.Peer),
	}

	// Add to in-memory map
	d.mu.Lock()
	d.sessions[code] = session
	d.mu.Unlock()

	// Persist to store
	if err := d.store.SaveSession(store.SessionEntry{
		Code:         code,
		ShareURL:     shareURL,
		ShareType:    session.ShareType,
		FileID:       fileID,
		Password:     password,
		ExpiresAt:    session.ExpiresAt,
		MaxDownloads: maxDownloads,
		Downloads:    0,
		RelayOnly:    relayOnly,
		CreatedAt:    now,
	}); err != nil {
		log.Printf("warning: could not persist session: %v", err)
	}

	if reconnected {
		log.Printf("session reclaimed - code: %s", code)
	} else {
		log.Printf("session created - code: %s", code)
	}

	// Notify callback
	if d.OnSessionAdded != nil {
		d.OnSessionAdded(session)
	}

	return code, nil
}

// RevokeSession removes a session by code, deregistering it from the
// signaling server and closing all peer connections.
func (d *Daemon) RevokeSession(code string) error {
	d.mu.Lock()
	session, ok := d.sessions[code]
	if !ok {
		d.mu.Unlock()
		return fmt.Errorf("session %s not found", code)
	}
	delete(d.sessions, code)
	d.mu.Unlock()

	// Close all peer connections
	session.mu.Lock()
	for peerID, peerConn := range session.peers {
		if err := peerConn.Close(); err != nil {
			log.Printf("close peer %s: %v", peerID, err)
		}
	}
	// Close all relay channels
	for sid, rc := range session.relayChannels {
		if err := rc.Close(); err != nil {
			log.Printf("close relay channel %s: %v", sid, err)
		}
	}
	session.mu.Unlock()

	// Remove from store
	if err := d.store.DeleteSession(code); err != nil {
		log.Printf("warning: could not delete session from store: %v", err)
	}

	// Notify server of deregistration (send deregister message)
	d.signaling.Send(context.Background(), map[string]string{
		"type": "deregister",
		"code": code,
	})

	log.Printf("session revoked: %s", code)

	// Notify callback
	if d.OnSessionRemoved != nil {
		d.OnSessionRemoved(code)
	}

	return nil
}

// ListSessions returns all active sessions.
func (d *Daemon) ListSessions() []*Session {
	d.mu.RLock()
	defer d.mu.RUnlock()

	sessions := make([]*Session, 0, len(d.sessions))
	for _, session := range d.sessions {
		sessions = append(sessions, session)
	}
	return sessions
}

// GetSession returns a session by code, or nil if not found.
func (d *Daemon) GetSession(code string) *Session {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.sessions[code]
}

// handleSignalingMessage dispatches incoming signaling messages to the
// appropriate handlers.
func (d *Daemon) handleSignalingMessage(msg signaling.Message) {
	switch msg.Type {
	case "welcome":
		log.Println("agent authenticated with signaling server")
		d.signalingConnected = true
		iceServers := d.signaling.GetICEServers()
		d.hasTURN = hasTURNServer(iceServers)

	case "knock":
		go d.handleKnock(msg.ConnID, msg.Code)

	case "join":
		go d.handleJoin(msg.ConnID, msg.Code, msg.HMAC)

	case "password_submit":
		go d.handlePasswordSubmit(msg.ConnID, msg.Code, msg.Password)

	case "answer":
		d.handleAnswer(msg.PeerID, msg.SDP)

	case "ice_candidate":
		d.handleICECandidate(msg.PeerID, msg.Candidate)

	case "relay_prepare":
		go d.handleRelayPrepare(msg)

	case "error":
		log.Printf("signaling error: %s", msg.Err)
	}
}

// handleKnock handles a knock from a browser (via signaling server).
// It generates a per-connection nonce, stores it with a 60s TTL, and
// sends it back so the browser can compute the HMAC proof.
func (d *Daemon) handleKnock(connID, sessionCode string) {
	d.mu.RLock()
	session, ok := d.sessions[sessionCode]
	d.mu.RUnlock()
	if !ok {
		log.Printf("knock for unknown session %s", sessionCode)
		return
	}

	// Generate 32 random bytes → 64-char hex nonce
	nonceBytes := make([]byte, 32)
	if _, err := rand.Read(nonceBytes); err != nil {
		log.Printf("generate nonce for session %s: %v", sessionCode, err)
		return
	}
	nonce := hex.EncodeToString(nonceBytes)

	// Sweep expired nonces and store new one (under same lock)
	d.noncesMu.Lock()
	now := time.Now()
	for id, entry := range d.nonces {
		if now.After(entry.expiresAt) {
			delete(d.nonces, id)
		}
	}
	d.nonces[connID] = nonceEntry{nonce: nonce, expiresAt: now.Add(60 * time.Second)}
	d.noncesMu.Unlock()

	// Send nonce back to browser (via signaling server)
	d.signaling.Send(context.Background(), map[string]any{
		"type":         "nonce",
		"conn_id":      connID,
		"value":        nonce,
		"has_password": session.Password != "",
	})

	log.Printf("nonce sent for session %s conn %s", sessionCode, connID)
}

// handleJoin verifies the HMAC from the browser. If valid, creates a WebRTC
// peer. If invalid, notifies the signaling server (which tracks failures and
// closes the browser WS after 3 strikes).
func (d *Daemon) handleJoin(connID, sessionCode, receivedHMAC string) {
	d.mu.RLock()
	session, ok := d.sessions[sessionCode]
	d.mu.RUnlock()
	if !ok {
		log.Printf("join for unknown session %s", sessionCode)
		return
	}

	// Atomically delete nonce entry before verifying — prevents race where
	// two concurrent join messages both read the nonce before either deletes it.
	d.noncesMu.Lock()
	entry, found := d.nonces[connID]
	delete(d.nonces, connID)
	d.noncesMu.Unlock()

	if !found || time.Now().After(entry.expiresAt) {
		log.Printf("join with expired/missing nonce: session %s conn %s", sessionCode, connID)
		d.signaling.Send(context.Background(), map[string]any{
			"type":    "auth_failed",
			"conn_id": connID,
		})
		return
	}

	if session.ShareType == "immich" && session.IsPasswordProtected {
		log.Printf("protected Immich join requires password submit: session %s conn %s", sessionCode, connID)
		d.signaling.Send(context.Background(), map[string]any{
			"type":    "auth_failed",
			"conn_id": connID,
			"code":    sessionCode,
		})
		return
	}

	// Verify HMAC for password-protected shares. Password-less shares skip verification.
	if session.Password != "" && session.ShareType != "immich" {
		mac := hmac.New(sha256.New, []byte(session.Password))
		mac.Write([]byte(entry.nonce))
		expectedMAC := mac.Sum(nil)

		receivedBytes, err := hex.DecodeString(receivedHMAC)
		if err != nil || !hmac.Equal(expectedMAC, receivedBytes) {
			log.Printf("HMAC mismatch for session %s conn %s", sessionCode, connID)
			d.signaling.Send(context.Background(), map[string]any{
				"type":    "auth_failed",
				"conn_id": connID,
			})
			return
		}
	}

	log.Printf("HMAC verified for session %s conn %s — creating peer", sessionCode, connID)

	// Send auth_ok to signaling server after successful HMAC verification
	d.signaling.Send(context.Background(), map[string]any{
		"type":    "auth_ok",
		"conn_id": connID,
		"code":    sessionCode,
	})

	if session.RelayOnly {
		log.Printf("relay-only session %s conn %s — skipping direct WebRTC peer", sessionCode, connID)
		return
	}

	go d.createPeer(connID, sessionCode)
}

func (d *Daemon) handlePasswordSubmit(connID, sessionCode, password string) {
	d.mu.RLock()
	session := d.sessions[sessionCode]
	d.mu.RUnlock()
	if session == nil || session.ShareType != "immich" || session.immichClient == nil {
		_ = d.signaling.Send(context.Background(), map[string]any{"type": "auth_fail", "conn_id": connID, "code": sessionCode})
		return
	}
	ok, err := session.immichClient.ValidatePassword(context.Background(), password)
	if err != nil || !ok {
		_ = d.signaling.Send(context.Background(), map[string]any{"type": "auth_fail", "conn_id": connID, "code": sessionCode})
		return
	}
	_ = d.signaling.Send(context.Background(), map[string]any{"type": "auth_ok", "conn_id": connID, "code": sessionCode})
}

// createPeer creates a WebRTC peer connection for a browser joining a session.
func (d *Daemon) createPeer(connID, sessionCode string) {
	d.mu.RLock()
	session, ok := d.sessions[sessionCode]
	d.mu.RUnlock()

	if !ok {
		log.Printf("join for unknown session %s", sessionCode)
		return
	}

	log.Printf("browser joined session %s (conn %s) - starting WebRTC handshake", sessionCode, connID)

	// Get ICE servers from signaling client
	iceServers := d.signaling.GetICEServers()
	if len(iceServers) == 0 {
		log.Printf("warning: no ICE servers received, using default STUN")
		iceServers = []webrtc.ICEServer{
			{URLs: []string{"stun:stun.cloudflare.com:3478"}},
		}
	}

	// Create new peer connection
	p, err := peer.New(iceServers, session.RelayOnly)
	if err != nil {
		log.Printf("create peer for session %s: %v", sessionCode, err)
		return
	}

	// Add peer to session
	session.mu.Lock()
	session.peers[connID] = p
	session.mu.Unlock()

	// Set up peer callbacks
	p.OnClosed = func() {
		log.Printf("peer %s closed (session %s)", connID, sessionCode)
		session.mu.Lock()
		delete(session.peers, connID)
		session.mu.Unlock()
	}

	p.OnICECandidate = func(init webrtc.ICECandidateInit) {
		d.signaling.Send(context.Background(), map[string]any{
			"type":       "ice_candidate",
			"session_id": sessionCode,
			"peer_id":    connID,
			"candidate":  init,
		})
	}

	// Create transfer manager
	tm := transfer.NewManager(p, session.webdavClient, session.MaxDownloads)
	tm.SetDownloadCount(session.Downloads)

	// Wire transfer callbacks
	tm.OnSessionExpired = func() {
		d.signaling.Send(context.Background(), map[string]any{
			"type":       "session_expired",
			"session_id": sessionCode,
			"peer_id":    connID,
		})
	}
	tm.OnDownloadComplete = func(bytesTransferred int64) {
		// Update session download count
		session.mu.Lock()
		session.Downloads++
		newCount := session.Downloads
		session.mu.Unlock()

		// Persist to store
		if _, err := d.store.IncrementDownloads(sessionCode); err != nil {
			log.Printf("warning: could not persist download count: %v", err)
		}

		// Notify server for tier tracking
		d.signaling.DownloadComplete(context.Background(), sessionCode, bytesTransferred)

		log.Printf("download complete for session %s (count: %d)", sessionCode, newCount)
	}

	p.OnOpen = func() {
		log.Printf("DataChannel open for peer %s (session %s)", connID, sessionCode)
		tm.HandleOpen()
	}

	// Create offer and send to signaling server
	sdp, err := p.CreateOffer()
	if err != nil {
		log.Printf("create offer for session %s: %v", sessionCode, err)
		return
	}
	p.SetOnMessage(tm.HandleMessage)

	d.signaling.Send(context.Background(), map[string]any{
		"type":       "offer",
		"session_id": sessionCode,
		"peer_id":    connID,
		"sdp":        sdp,
	})
}

// handleAnswer applies the browser's SDP answer to the peer connection.
func (d *Daemon) handleAnswer(peerID, sdp string) {
	// Find the session containing this peer
	d.mu.RLock()
	var session *Session
	for _, sess := range d.sessions {
		sess.mu.Lock()
		if _, ok := sess.peers[peerID]; ok {
			session = sess
			sess.mu.Unlock()
			break
		}
		sess.mu.Unlock()
	}
	d.mu.RUnlock()

	if session == nil {
		log.Printf("answer for unknown peer %s", peerID)
		return
	}

	session.mu.Lock()
	p, ok := session.peers[peerID]
	session.mu.Unlock()

	if !ok {
		log.Printf("peer %s not found in session", peerID)
		return
	}

	if err := p.SetAnswer(sdp); err != nil {
		log.Printf("set answer for peer %s: %v", peerID, err)
	}
}

// handleICECandidate adds an ICE candidate to the peer connection.
func (d *Daemon) handleICECandidate(peerID string, candidate json.RawMessage) {
	// Find the session containing this peer
	d.mu.RLock()
	var session *Session
	for _, sess := range d.sessions {
		sess.mu.Lock()
		if _, ok := sess.peers[peerID]; ok {
			session = sess
			sess.mu.Unlock()
			break
		}
		sess.mu.Unlock()
	}
	d.mu.RUnlock()

	if session == nil {
		log.Printf("ICE candidate for unknown peer %s", peerID)
		return
	}

	session.mu.Lock()
	p, ok := session.peers[peerID]
	session.mu.Unlock()

	if !ok {
		log.Printf("peer %s not found in session", peerID)
		return
	}

	var init webrtc.ICECandidateInit
	if err := json.Unmarshal(candidate, &init); err != nil {
		log.Printf("parse ICE candidate for peer %s: %v", peerID, err)
		return
	}

	if err := p.AddICECandidate(init); err != nil {
		log.Printf("add ICE candidate for peer %s: %v", peerID, err)
	}
}

// handleRelayPrepare handles a relay_prepare message from the signaling server.
// It creates a SecureRelayChannel, wires it to a transfer manager, and starts it.
func (d *Daemon) handleRelayPrepare(msg signaling.Message) {
	log.Printf("relay_prepare received for session %s sid=%s", msg.Code, msg.SID)

	d.mu.RLock()
	session := d.sessions[msg.Code]
	d.mu.RUnlock()
	if session == nil {
		log.Printf("relay_prepare for unknown session %s", msg.Code)
		return
	}

	// Get relay static private key
	rawPriv, err := d.store.GetRelayStaticPrivateKey()
	if err != nil {
		log.Printf("get relay static key: %v", err)
		return
	}
	log.Printf("relay_prepare: got static key for session %s", msg.Code)

	// Convert raw bytes to ecdh.PrivateKey
	staticPriv, err := ecdh.P256().NewPrivateKey(rawPriv)
	if err != nil {
		log.Printf("import relay static key: %v", err)
		return
	}

	// Create relay channel
	relayURL := signaling.RelayWebSocketURL(d.config.SignalingURL)
	log.Printf("relay_prepare: connecting to relay at %s for session %s", relayURL, msg.Code)

	// Use factory function if set (for testing), otherwise create real channel
	var channel relayTransferChannel
	if d.newRelayChannel != nil {
		channel, err = d.newRelayChannel(relayChannelConfig{
			RelayURL:      relayURL,
			RelayJWT:      msg.RelayJWT,
			StaticPrivate: rawPriv,
		})
		if err != nil {
			log.Printf("new relay channel: %v", err)
			return
		}
	} else {
		// Production: create SecureRelayChannel directly
		rc, err := relaychannel.NewSecureRelayChannel(relaychannel.SecureRelayConfig{
			RelayURL:      relayURL,
			RelayJWT:      msg.RelayJWT,
			StaticPrivate: staticPriv,
		})
		if err != nil {
			log.Printf("new relay channel: %v", err)
			return
		}
		channel = rc
	}

	// Create transfer manager for this relay channel
	tm := transfer.NewManager(channel, session.webdavClient, session.MaxDownloads)
	tm.SetDownloadCount(session.Downloads)

	// Wire transfer callbacks
	tm.OnSessionExpired = func() {
		_ = d.signaling.Send(context.Background(), map[string]any{
			"type":       "session_expired",
			"session_id": msg.Code,
		})
	}
	tm.OnDownloadComplete = func(bytesTransferred int64) {
		session.mu.Lock()
		session.Downloads++
		session.mu.Unlock()
		if _, err := d.store.IncrementDownloads(msg.Code); err != nil {
			log.Printf("persist download count: %v", err)
		}
		_ = d.signaling.DownloadComplete(context.Background(), msg.Code, bytesTransferred)
	}

	// Wire channel callbacks
	channel.SetOnMessage(tm.HandleMessage)
	channel.SetOnOpen(tm.HandleOpen)
	channel.SetOnClose(func() {
		session.mu.Lock()
		delete(session.relayChannels, msg.SID)
		session.mu.Unlock()
	})

	// Add channel to session
	session.mu.Lock()
	if session.relayChannels == nil {
		session.relayChannels = make(map[string]relayTransferChannel)
	}
	session.relayChannels[msg.SID] = channel
	session.mu.Unlock()

	// Start relay channel (asynchronously handles handshake)
	log.Printf("relay_prepare: starting relay channel for session %s sid=%s", msg.Code, msg.SID)
	if err := channel.Start(context.Background()); err != nil {
		log.Printf("start relay channel sid=%s: %v", msg.SID, err)
		session.mu.Lock()
		delete(session.relayChannels, msg.SID)
		session.mu.Unlock()
		return
	}
	log.Printf("relay_prepare: relay channel started successfully for session %s sid=%s", msg.Code, msg.SID)
}

// loadSessionsFromStore loads persisted sessions from the store and
// re-registers them with the signaling server.
func (d *Daemon) loadSessionsFromStore(ctx context.Context) {
	sessions := d.store.ListSessions(true) // Filter expired

	// Get relay static key once for all sessions
	relayStaticPriv, err := d.store.GetRelayStaticPrivateKey()
	if err != nil {
		log.Printf("warning: could not get relay static key: %v", err)
		relayStaticPriv = nil
	}
	var relayStaticPub string
	if relayStaticPriv != nil {
		relayStaticPub, err = RelayStaticPubHex(relayStaticPriv)
		if err != nil {
			log.Printf("warning: could not derive relay static public key: %v", err)
			relayStaticPub = ""
		}
	}

	cfg := d.GetConfig()
	for _, entry := range sessions {
		if entry.ShareType == "" {
			log.Printf("warning: skipping legacy session %s: missing share_type", entry.Code)
			continue
		}

		if entry.ShareType == "immich" {
			client, err := d.newImmichClient(entry.Code)
			if err != nil {
				log.Printf("warning: could not create Immich client for %s: %v", entry.Code, err)
				continue
			}
			reg, ok := d.signaling.(shareOptionRegistrar)
			if !ok {
				log.Printf("warning: signaling client does not support Immich registration options for %s", entry.Code)
				continue
			}
			code, reconnected, err := reg.RegisterShareWithOptions(ctx, signaling.RegisterShareOptions{
				ShareURL:            entry.ShareURL,
				PreferredCode:       entry.Code,
				ShareType:           "immich",
				IsPasswordProtected: entry.IsPasswordProtected,
				RelayOnly:           true,
				RelayStaticPub:      relayStaticPub,
			})
			if err != nil {
				log.Printf("warning: could not re-register Immich session %s: %v", entry.Code, err)
				continue
			}

			session := &Session{
				Code:                code,
				ShareURL:            entry.ShareURL,
				ShareType:           "immich",
				IsPasswordProtected: entry.IsPasswordProtected,
				ExpiresAt:           entry.ExpiresAt,
				MaxDownloads:        entry.MaxDownloads,
				Downloads:           entry.Downloads,
				RelayOnly:           true,
				CreatedAt:           entry.CreatedAt,
				immichClient:        client,
				peers:               make(map[string]*peer.Peer),
				relayChannels:       make(map[string]relayTransferChannel),
			}
			d.mu.Lock()
			d.sessions[code] = session
			d.mu.Unlock()

			if reconnected {
				log.Printf("Immich session reconnected - code: %s", code)
			} else {
				log.Printf("Immich session loaded - code: %s", code)
			}
			continue
		}

		// Create WebDAV client
		webdavClient, err := cloudwebdav.New(entry.ShareType, entry.ShareURL, []string{cfg.AllowedHost, cfg.NCAllowedHost}, entry.Password)
		if err != nil {
			log.Printf("warning: could not create WebDAV client for %s: %v", entry.Code, err)
			continue
		}

		// Re-register with signaling server
		code, reconnected, err := d.signaling.RegisterShare(ctx, entry.ShareURL, entry.Code, entry.RelayOnly, relayStaticPub)
		if err != nil {
			log.Printf("warning: could not re-register session %s: %v", entry.Code, err)
			continue
		}

		// Use the returned code (might be different if reconnection failed)
		session := &Session{
			Code:         code,
			ShareURL:     entry.ShareURL,
			ShareType:    entry.ShareType,
			FileID:       entry.FileID,
			Password:     entry.Password,
			ExpiresAt:    entry.ExpiresAt,
			MaxDownloads: entry.MaxDownloads,
			Downloads:    entry.Downloads,
			RelayOnly:    entry.RelayOnly,
			CreatedAt:    entry.CreatedAt,
			webdavClient: webdavClient,
			peers:        make(map[string]*peer.Peer),
		}

		d.mu.Lock()
		d.sessions[code] = session
		d.mu.Unlock()

		if reconnected {
			log.Printf("session reconnected - code: %s", code)
		} else {
			log.Printf("session loaded - code: %s", code)
		}
	}

	log.Printf("loaded %d sessions from store", len(d.sessions))
}

func (d *Daemon) runImmichPoller(ctx context.Context) {
	if err := d.syncImmichShares(ctx); err != nil {
		log.Printf("immich sync: %v", err)
	}
	interval := time.Duration(d.config.ImmichPollInterval) * time.Second
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.syncImmichShares(ctx); err != nil {
				log.Printf("immich sync: %v", err)
			}
		}
	}
}

func (d *Daemon) syncImmichShares(ctx context.Context) error {
	poller, err := d.getImmichPoller()
	if err != nil {
		return err
	}
	links, err := poller.PollShares(ctx)
	if err != nil {
		return err
	}

	relayStaticPub, err := d.relayStaticPubHex()
	if err != nil {
		return err
	}

	seen := make(map[string]immich.SharedLink, len(links))
	for _, link := range links {
		if link.Key == "" {
			continue
		}
		seen[link.Key] = link

		d.mu.RLock()
		_, exists := d.sessions[link.Key]
		d.mu.RUnlock()
		if exists {
			continue
		}

		session, err := d.registerImmichShare(ctx, link, relayStaticPub)
		if err != nil {
			return err
		}
		d.mu.Lock()
		d.sessions[session.Code] = session
		d.mu.Unlock()
		if d.OnSessionAdded != nil {
			d.OnSessionAdded(session)
		}
	}

	var removed []*Session
	d.mu.RLock()
	for code, session := range d.sessions {
		if session.ShareType == "immich" {
			if _, ok := seen[code]; !ok {
				removed = append(removed, session)
			}
		}
	}
	d.mu.RUnlock()

	for _, session := range removed {
		d.closeSessionResources(session)
		if err := d.unregisterShare(ctx, session.Code); err != nil {
			return err
		}
		if err := d.store.DeleteSession(session.Code); err != nil {
			return err
		}
		d.mu.Lock()
		delete(d.sessions, session.Code)
		d.mu.Unlock()
		if d.OnSessionRemoved != nil {
			d.OnSessionRemoved(session.Code)
		}
	}

	return nil
}

func (d *Daemon) getImmichPoller() (immichPoller, error) {
	if d.newImmichPoller != nil {
		return d.newImmichPoller()
	}
	return immich.New(immich.Config{
		BaseURL:     d.config.ImmichURL,
		AllowedHost: d.config.ImmichAllowedHost,
		APIKey:      d.config.ImmichAPIKey,
	})
}

func (d *Daemon) registerImmichShare(ctx context.Context, link immich.SharedLink, relayStaticPub string) (*Session, error) {
	reg, ok := d.signaling.(shareOptionRegistrar)
	if !ok {
		return nil, fmt.Errorf("signaling client does not support option registration")
	}

	shareURL := "immich://" + link.Key
	passwordProtected := link.IsPasswordProtected()
	opts := signaling.RegisterShareOptions{
		ShareURL:            shareURL,
		PreferredCode:       link.Key,
		ShareType:           "immich",
		IsPasswordProtected: passwordProtected,
		RelayOnly:           true,
		RelayStaticPub:      relayStaticPub,
	}
	code, _, err := reg.RegisterShareWithOptions(ctx, opts)
	if err != nil {
		return nil, err
	}
	client, err := d.newImmichClient(code)
	if err != nil {
		_ = d.unregisterShare(ctx, code)
		return nil, err
	}

	now := time.Now()
	session := &Session{
		Code:                code,
		ShareURL:            shareURL,
		ShareType:           "immich",
		IsPasswordProtected: passwordProtected,
		RelayOnly:           true,
		CreatedAt:           now,
		immichClient:        client,
		peers:               make(map[string]*peer.Peer),
		relayChannels:       make(map[string]relayTransferChannel),
	}
	if err := d.store.SaveSession(store.SessionEntry{
		Code:                code,
		ShareURL:            shareURL,
		ShareType:           "immich",
		IsPasswordProtected: passwordProtected,
		RelayOnly:           true,
		CreatedAt:           now,
	}); err != nil {
		_ = d.unregisterShare(ctx, code)
		return nil, err
	}
	return session, nil
}

func (d *Daemon) unregisterShare(ctx context.Context, code string) error {
	if unreg, ok := d.signaling.(shareUnregistrar); ok {
		return unreg.UnregisterShare(ctx, code)
	}
	return d.signaling.Send(ctx, map[string]string{"type": "unregister_share", "code": code})
}

func (d *Daemon) newImmichClient(code string) (immichGalleryBackend, error) {
	return immich.New(immich.Config{
		BaseURL:     d.config.ImmichURL,
		AllowedHost: d.config.ImmichAllowedHost,
		APIKey:      d.config.ImmichAPIKey,
		ShareKey:    code,
	})
}

func (d *Daemon) relayStaticPubHex() (string, error) {
	relayStaticPriv, err := d.store.GetRelayStaticPrivateKey()
	if err != nil {
		return "", fmt.Errorf("get relay static key: %w", err)
	}
	relayStaticPub, err := RelayStaticPubHex(relayStaticPriv)
	if err != nil {
		return "", fmt.Errorf("derive relay static public key: %w", err)
	}
	return relayStaticPub, nil
}

func (d *Daemon) closeSessionResources(session *Session) {
	session.mu.Lock()
	defer session.mu.Unlock()
	for peerID, peerConn := range session.peers {
		if err := peerConn.Close(); err != nil {
			log.Printf("close peer %s: %v", peerID, err)
		}
	}
	for sid, rc := range session.relayChannels {
		if err := rc.Close(); err != nil {
			log.Printf("close relay channel %s: %v", sid, err)
		}
	}
}

// runExpiryPruner periodically checks for and removes expired sessions.
func (d *Daemon) runExpiryPruner(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.pruneExpiredSessions()
		}
	}
}

// pruneExpiredSessions removes all expired sessions.
func (d *Daemon) pruneExpiredSessions() {
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()

	for code, session := range d.sessions {
		if !session.ExpiresAt.IsZero() && session.ExpiresAt.Before(now) {
			log.Printf("session %s expired at %s", code, session.ExpiresAt)

			// Close all peer connections
			session.mu.Lock()
			for peerID, peerConn := range session.peers {
				if err := peerConn.Close(); err != nil {
					log.Printf("close peer %s: %v", peerID, err)
				}
			}
			// Close all relay channels
			for sid, rc := range session.relayChannels {
				if err := rc.Close(); err != nil {
					log.Printf("close relay channel %s: %v", sid, err)
				}
			}
			session.mu.Unlock()

			// Remove from in-memory map
			delete(d.sessions, code)

			// Remove from store
			if err := d.store.DeleteSession(code); err != nil {
				log.Printf("warning: could not delete expired session: %v", err)
			}

			// Notify server of deregistration
			d.signaling.Send(context.Background(), map[string]string{
				"type": "deregister",
				"code": code,
			})

			// Notify callback
			if d.OnSessionRemoved != nil {
				d.OnSessionRemoved(code)
			}
		}
	}
}

// IsConnected returns whether the daemon has authenticated with the signaling server.
func (d *Daemon) IsConnected() bool {
	return d.signalingConnected
}

// HasTURN returns whether TURN servers are available.
func (d *Daemon) HasTURN() bool {
	return d.hasTURN
}

// GetConfig returns a copy of the current configuration.
func (d *Daemon) GetConfig() *config.Config {
	d.mu.RLock()
	cfg := d.config
	d.mu.RUnlock()
	return cfg
}

// GetUptime returns the duration since the daemon started.
func (d *Daemon) GetUptime() time.Duration {
	return time.Since(d.startTime)
}

// GetConfigPath returns the path to the config file on disk.
func (d *Daemon) GetConfigPath() string {
	if d.configMgr != nil {
		if mgr, ok := d.configMgr.(*config.Manager); ok {
			return mgr.FilePath()
		}
	}
	return ""
}

// SaveConfig updates and persists the configuration.
// It uses the config manager to save to the config file.
func (d *Daemon) SaveConfig(cfg *config.Config) error {
	d.mu.Lock()
	d.config = cfg
	d.mu.Unlock()
	if d.configMgr != nil {
		// Cast to concrete type to access Save method
		if mgr, ok := d.configMgr.(*config.Manager); ok {
			return mgr.Save(cfg)
		}
	}
	return nil
}

// hasTURNServer checks if any ICE server is a TURN server.
func hasTURNServer(servers []webrtc.ICEServer) bool {
	for _, server := range servers {
		for _, url := range server.URLs {
			if strings.HasPrefix(url, "turn:") || strings.HasPrefix(url, "turns:") {
				return true
			}
		}
	}
	return false
}

// RelayStaticPubHex derives the hex-encoded P-256 public key from the raw private key bytes.
func RelayStaticPubHex(rawPrivateKey []byte) (string, error) {
	priv, err := ecdh.P256().NewPrivateKey(rawPrivateKey)
	if err != nil {
		return "", fmt.Errorf("import relay static key: %w", err)
	}
	return hex.EncodeToString(priv.PublicKey().Bytes()), nil
}
