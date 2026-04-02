package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"opencloudshare/agent/internal/config"
	"opencloudshare/agent/internal/opencloud"
	"opencloudshare/agent/internal/peer"
	"opencloudshare/agent/internal/signaling"
	"opencloudshare/agent/internal/store"
	"opencloudshare/agent/internal/transfer"
)

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
	RegisterShare(ctx context.Context, shareURL, preferredCode string) (string, bool, error)
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
	Password     string
	ExpiresAt    time.Time
	MaxDownloads int
	Downloads    int
	RelayOnly    bool
	CreatedAt    time.Time

	webdavClient *opencloud.Client
	peers        map[string]*peer.Peer // peerID -> Peer
	mu           sync.Mutex
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

	webServer       WebServer
	startTime       time.Time
	signalingConnected bool // true once welcome received
	hasTURN         bool

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

	// Close all peer connections
	for _, session := range d.sessions {
		session.mu.Lock()
		for peerID, peerConn := range session.peers {
			if err := peerConn.Close(); err != nil {
				log.Printf("close peer %s: %v", peerID, err)
			}
		}
		session.peers = make(map[string]*peer.Peer)
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
// signaling server.
func (d *Daemon) CreateSession(ctx context.Context, shareURL, password string, expiryDuration time.Duration, maxDownloads int, relayOnly bool) (string, error) {
	cfg := d.GetConfig()

	// Validate share URL against allowed host
	if cfg.AllowedHost != "" {
		_, err := opencloud.New(shareURL, cfg.AllowedHost, password)
		if err != nil {
			return "", fmt.Errorf("validate share URL: %w", err)
		}
	}

	// Create WebDAV client
	webdavClient, err := opencloud.New(shareURL, cfg.AllowedHost, password)
	if err != nil {
		return "", fmt.Errorf("create WebDAV client: %w", err)
	}

	// Register with signaling server (no preferred code for new sessions)
	code, reconnected, err := d.signaling.RegisterShare(ctx, shareURL, "")
	if err != nil {
		return "", fmt.Errorf("register share: %w", err)
	}

	now := time.Now()
	session := &Session{
		Code:         code,
		ShareURL:     shareURL,
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

	case "join":
		go d.handleBrowserJoin(msg.SessionID, msg.PeerID)

	case "answer":
		d.handleAnswer(msg.PeerID, msg.SDP)

	case "ice_candidate":
		d.handleICECandidate(msg.PeerID, msg.Candidate)

	case "error":
		log.Printf("signaling error: %s", msg.Err)
	}
}

// handleBrowserJoin handles a browser joining a session.
func (d *Daemon) handleBrowserJoin(sessionCode, peerID string) {
	d.mu.RLock()
	session, ok := d.sessions[sessionCode]
	d.mu.RUnlock()

	if !ok {
		log.Printf("join for unknown session %s", sessionCode)
		return
	}

	log.Printf("browser joined session %s (peer %s) - starting WebRTC handshake", sessionCode, peerID)

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
	session.peers[peerID] = p
	session.mu.Unlock()

	// Set up peer callbacks
	p.OnClosed = func() {
		log.Printf("peer %s closed (session %s)", peerID, sessionCode)
		session.mu.Lock()
		delete(session.peers, peerID)
		session.mu.Unlock()
	}

	p.OnICECandidate = func(init webrtc.ICECandidateInit) {
		d.signaling.Send(context.Background(), map[string]any{
			"type":       "ice_candidate",
			"session_id": sessionCode,
			"peer_id":    peerID,
			"candidate":  init,
		})
	}

	// Create transfer manager
	tm := transfer.NewManager(p, session.webdavClient, session.Password, session.MaxDownloads)
	tm.SetDownloadCount(session.Downloads)

	// Wire transfer callbacks
	tm.OnAuthFailed = func() {
		d.signaling.Send(context.Background(), map[string]any{
			"type":       "auth_failed",
			"session_id": sessionCode,
			"peer_id":    peerID,
		})
	}
	tm.OnSessionExpired = func() {
		d.signaling.Send(context.Background(), map[string]any{
			"type":       "session_expired",
			"session_id": sessionCode,
			"peer_id":    peerID,
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
		log.Printf("DataChannel open for peer %s (session %s)", peerID, sessionCode)
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
		"peer_id":    peerID,
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

// loadSessionsFromStore loads persisted sessions from the store and
// re-registers them with the signaling server.
func (d *Daemon) loadSessionsFromStore(ctx context.Context) {
	sessions := d.store.ListSessions(true) // Filter expired

	cfg := d.GetConfig()
	for _, entry := range sessions {
		// Create WebDAV client
		webdavClient, err := opencloud.New(entry.ShareURL, cfg.AllowedHost, entry.Password)
		if err != nil {
			log.Printf("warning: could not create WebDAV client for %s: %v", entry.Code, err)
			continue
		}

		// Re-register with signaling server
		code, reconnected, err := d.signaling.RegisterShare(ctx, entry.ShareURL, entry.Code)
		if err != nil {
			log.Printf("warning: could not re-register session %s: %v", entry.Code, err)
			continue
		}

		// Use the returned code (might be different if reconnection failed)
		session := &Session{
			Code:         code,
			ShareURL:     entry.ShareURL,
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