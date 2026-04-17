package daemon

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"sharebridge/agent/internal/cloudwebdav"
	"sharebridge/agent/internal/config"
	"sharebridge/agent/internal/signaling"
	"sharebridge/agent/internal/store"
	"sharebridge/agent/internal/transfer"
	"sharebridge/agent/internal/transport"
)

// nonceEntry holds a per-connection nonce for HMAC pre-challenge.
type nonceEntry struct {
	nonce     string
	expiresAt time.Time
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
	RegisterShare(ctx context.Context, shareURL, preferredCode string, relayOnly bool) (string, bool, error)
	DownloadComplete(ctx context.Context, code string, bytesTransferred int64) error
	Send(ctx context.Context, msg any) error
	GetRelayMultiaddr() string
	Listen(ctx context.Context) error
	SetOnMessage(handler func(signaling.Message))
	SetPeerID(peerID string)
}

// TransportInterface defines the subset of transport.Transport the daemon uses.
type TransportInterface interface {
	DialRelay(ctx context.Context, relayMultiaddr string) error
	OnStream(h transport.StreamHandler)
	PeerID() string
	Close() error
}

// Session represents an active share session with libp2p streams.
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

	webdavClient *cloudwebdav.Client
	streams      map[string]*transport.StreamAdapter // connID → adapter
	mu           sync.Mutex
}

// Daemon manages multiple concurrent sessions, a single signaling connection,
// and the web server.
type Daemon struct {
	config    *config.Config
	configMgr ConfigManagerInterface
	store     StoreInterface
	signaling SignalingClientInterface
	transport TransportInterface
	sessions  map[string]*Session // code -> Session
	mu        sync.RWMutex

	webServer          WebServer
	startTime          time.Time
	signalingConnected bool // true once welcome received

	// Nonce store for HMAC pre-challenge (connID -> nonce)
	nonces   map[string]nonceEntry
	noncesMu sync.Mutex

	// Callbacks for external handling (e.g., web server refresh)
	OnSessionAdded   func(session *Session)
	OnSessionRemoved func(code string)
}

// New creates a new Daemon with the given config manager and store.
func New(cfgMgr ConfigManagerInterface, st StoreInterface, tr TransportInterface) (*Daemon, error) {
	cfg := cfgMgr.Get()
	agentID := st.GetAgentID()

	sig := signaling.New(cfg.SignalingURL, cfg.APIKey, agentID)

	sig.SetPeerID(tr.PeerID())

	d := &Daemon{
		config:    cfg,
		configMgr: cfgMgr,
		store:     st,
		signaling: sig,
		transport: tr,
		sessions:  make(map[string]*Session),
		nonces:    make(map[string]nonceEntry),
		startTime: time.Now(),
	}
	tr.OnStream(d.handleIncomingStream)
	return d, nil
}

// NewWithSignaling creates a new Daemon with a custom signaling client.
// This is useful for testing or custom signaling implementations.
func NewWithSignaling(cfgMgr ConfigManagerInterface, st StoreInterface, sig SignalingClientInterface, tr TransportInterface) (*Daemon, error) {
	cfg := cfgMgr.Get()

	d := &Daemon{
		config:    cfg,
		configMgr: cfgMgr,
		store:     st,
		signaling: sig,
		transport: tr,
		sessions:  make(map[string]*Session),
		nonces:    make(map[string]nonceEntry),
	}
	tr.OnStream(d.handleIncomingStream)
	return d, nil
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

	// Close all streams
	for _, session := range d.sessions {
		session.mu.Lock()
		for connID, adapter := range session.streams {
			if err := adapter.Close(); err != nil {
				log.Printf("close stream %s: %v", connID, err)
			}
		}
		session.streams = make(map[string]*transport.StreamAdapter)
		session.mu.Unlock()
	}
	if d.transport != nil {
		if err := d.transport.Close(); err != nil {
			log.Printf("close transport: %v", err)
		}
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

	// Register with signaling server (no preferred code for new sessions)
	code, reconnected, err := d.signaling.RegisterShare(ctx, shareURL, "", relayOnly)
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
		streams:      make(map[string]*transport.StreamAdapter),
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
// signaling server and closing all streams.
func (d *Daemon) RevokeSession(code string) error {
	d.mu.Lock()
	session, ok := d.sessions[code]
	if !ok {
		d.mu.Unlock()
		return fmt.Errorf("session %s not found", code)
	}
	delete(d.sessions, code)
	d.mu.Unlock()

	// Close all streams
	session.mu.Lock()
	for connID, adapter := range session.streams {
		if err := adapter.Close(); err != nil {
			log.Printf("close stream %s: %v", connID, err)
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
		relayMA := d.signaling.GetRelayMultiaddr()
		if relayMA == "" {
			log.Printf("welcome missing relay_multiaddr — transport will be unreachable")
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := d.transport.DialRelay(ctx, relayMA); err != nil {
				log.Printf("dial relay %s: %v", relayMA, err)
			} else {
				log.Printf("connected to relay %s", relayMA)
			}
		}()

	case "knock":
		go d.handleKnock(msg.ConnID, msg.Code)

	case "join":
		go d.handleJoin(msg.ConnID, msg.Code, msg.HMAC)

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

// handleJoin verifies the HMAC from the browser. If valid, sends auth_ok to
// the signaling server. If invalid, notifies the signaling server (which
// tracks failures and closes the browser WS after 3 strikes).
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

	// Verify HMAC for password-protected shares. Password-less shares skip verification.
	if session.Password != "" {
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

	log.Printf("HMAC verified for session %s conn %s — sending auth_ok", sessionCode, connID)
	go d.sendAuthOK(connID, sessionCode)
}

// sendAuthOK notifies the signaling server that a browser cleared the HMAC
// pre-challenge. The server uses this as the trigger to issue a JWT and
// return relay_multiaddr + token to the browser. The browser then dials the
// relay, which validates the JWT and opens a circuit to this agent; the
// stream arrives via handleIncomingStream.
func (d *Daemon) sendAuthOK(connID, sessionCode string) {
	d.mu.RLock()
	_, ok := d.sessions[sessionCode]
	d.mu.RUnlock()
	if !ok {
		log.Printf("sendAuthOK for unknown session %s", sessionCode)
		return
	}
	if err := d.signaling.Send(context.Background(), map[string]any{
		"type":    "auth_ok",
		"conn_id": connID,
		"code":    sessionCode,
	}); err != nil {
		log.Printf("send auth_ok for session %s conn %s: %v", sessionCode, connID, err)
	}
}

// handleIncomingStream is invoked by Transport for each inbound file stream.
// Framing: the first frame on the stream is a FrameText JSON envelope
//   {"type":"open","share_code":"<code>","conn_id":"<id>"}
// The agent looks up the session, attaches a transfer.Manager, and serves.
func (d *Daemon) handleIncomingStream(info transport.StreamInfo) {
	stream := info.Stream
	kind, payload, err := transport.ReadFrame(stream)
	if err != nil {
		log.Printf("read open frame from %s: %v", info.Peer, err)
		stream.Reset()
		return
	}
	if kind != transport.FrameText {
		log.Printf("first frame from %s is binary; expected text open envelope", info.Peer)
		stream.Reset()
		return
	}
	var env struct {
		Type      string `json:"type"`
		ShareCode string `json:"share_code"`
		ConnID    string `json:"conn_id"`
	}
	if err := json.Unmarshal(payload, &env); err != nil || env.Type != "open" {
		log.Printf("bad open envelope from %s: %v (raw=%s)", info.Peer, err, string(payload))
		stream.Reset()
		return
	}

	d.mu.RLock()
	session, ok := d.sessions[env.ShareCode]
	d.mu.RUnlock()
	if !ok {
		log.Printf("open for unknown session %s from %s", env.ShareCode, info.Peer)
		stream.Reset()
		return
	}

	adapter := transport.NewStreamAdapter(stream)
	session.mu.Lock()
	session.streams[env.ConnID] = adapter
	downloads := session.Downloads
	session.mu.Unlock()

	tm := transfer.NewManager(adapter, session.webdavClient, session.MaxDownloads)
	tm.SetDownloadCount(downloads)
	tm.OnSessionExpired = func() {
		d.signaling.Send(context.Background(), map[string]any{
			"type":       "session_expired",
			"session_id": env.ShareCode,
			"peer_id":    env.ConnID,
		})
	}
	tm.OnDownloadComplete = func(bytesTransferred int64) {
		session.mu.Lock()
		session.Downloads++
		newCount := session.Downloads
		session.mu.Unlock()
		if _, err := d.store.IncrementDownloads(env.ShareCode); err != nil {
			log.Printf("warning: could not persist download count: %v", err)
		}
		d.signaling.DownloadComplete(context.Background(), env.ShareCode, bytesTransferred)
		log.Printf("download complete for session %s (count: %d)", env.ShareCode, newCount)
	}

	log.Printf("file stream open: session=%s conn=%s peer=%s", env.ShareCode, env.ConnID, info.Peer)
	tm.HandleOpen()

	// Read loop: decode frames and route text frames to the manager.
	go func() {
		defer func() {
			session.mu.Lock()
			delete(session.streams, env.ConnID)
			session.mu.Unlock()
			stream.Close()
		}()
		for {
			kind, payload, err := transport.ReadFrame(stream)
			if err != nil {
				return
			}
			if kind == transport.FrameText {
				tm.HandleMessage(payload)
			}
			// Binary frames from browser are unexpected today; ignored.
		}
	}()
}

// loadSessionsFromStore loads persisted sessions from the store and
// re-registers them with the signaling server.
func (d *Daemon) loadSessionsFromStore(ctx context.Context) {
	sessions := d.store.ListSessions(true) // Filter expired

	cfg := d.GetConfig()
	for _, entry := range sessions {
		if entry.ShareType == "" {
			log.Printf("warning: skipping legacy session %s: missing share_type", entry.Code)
			continue
		}

		// Create WebDAV client
		webdavClient, err := cloudwebdav.New(entry.ShareType, entry.ShareURL, []string{cfg.AllowedHost, cfg.NCAllowedHost}, entry.Password)
		if err != nil {
			log.Printf("warning: could not create WebDAV client for %s: %v", entry.Code, err)
			continue
		}

		// Re-register with signaling server
		code, reconnected, err := d.signaling.RegisterShare(ctx, entry.ShareURL, entry.Code, entry.RelayOnly)
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
			streams:      make(map[string]*transport.StreamAdapter),
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

			// Close all streams
			session.mu.Lock()
			for connID, adapter := range session.streams {
				if err := adapter.Close(); err != nil {
					log.Printf("close stream %s: %v", connID, err)
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
