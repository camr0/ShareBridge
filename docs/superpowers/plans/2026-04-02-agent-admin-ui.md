# Agent Admin UI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a local web UI for managing OpenCloudShare shares, served by a persistent daemon that handles multiple concurrent sessions.

**Architecture:** The current single-share CLI (`opencloudshare share <url>`) becomes a daemon process with an embedded web server. The daemon maintains a single WebSocket connection to the signaling server, multiplexing messages to multiple session handlers. Sessions are stored in `sessions.json` with new fields (password, expiry, relay mode). A config file (`config.json`) persists settings from the UI.

**Tech Stack:** Go 1.26, PicoCSS, HTMX, Go `html/template`, `go:embed`

---

## File Structure

**New Files:**
```
agent/
├── internal/
│   ├── config/
│   │   └── config.go          # MODIFIED: add JSON persistence, new fields
│   ├── daemon/
│   │   ├── daemon.go          # NEW: Daemon struct, session management
│   │   └── daemon_test.go     # NEW
│   ├── peer/
│   │   └── peer.go            # MODIFIED: add relayOnly parameter
│   ├── store/
│   │   └── store.go           # MODIFIED: new SessionEntry struct
│   └── web/
│       ├── server.go          # NEW: HTTP server, routes, go:embed
│       ├── handlers.go        # NEW: page handlers
│       ├── api.go             # NEW: HTMX API endpoints
│       ├── static/            # NEW: embedded static files
│       │   ├── pico.min.css
│       │   ├── htmx.min.js
│       │   └── custom.css
│       └── templates/         # NEW: embedded templates
│           ├── layout.html
│           ├── dashboard.html
│           ├── settings.html
│           ├── history.html
│           ├── share-form.html
│           └── share-card.html
└── cmd/agent/
    └── main.go                # MODIFIED: add daemon subcommand, client mode
```

**Modified Files:**
- `agent/internal/peer/peer.go` — add `relayOnly` parameter to `New()`
- `agent/internal/store/store.go` — new `SessionEntry` struct, keyed by code
- `agent/internal/config/config.go` — add JSON persistence, new fields
- `agent/cmd/agent/main.go` — add `daemon` subcommand, client mode fallback

---

## Task 0: Update peer.New for Relay Mode

**Files:**
- Modify: `agent/internal/peer/peer.go`
- Modify: `agent/internal/peer/peer_test.go` (if exists)

**Context:** The peer connection needs to support forcing relay-only mode for sessions where the user wants to hide their IP.

- [ ] **Step 1: Add relayOnly parameter to New()**

Modify `agent/internal/peer/peer.go`:

```go
// New creates a PeerConnection with the given ICE servers.
// If relayOnly is true, forces ICETransportPolicyRelay to hide the agent's IP.
func New(iceServers []webrtc.ICEServer, relayOnly bool) (*Peer, error) {
	config := webrtc.Configuration{
		ICEServers: iceServers,
	}
	if relayOnly {
		config.ICETransportPolicy = webrtc.ICETransportPolicyRelay
	}

	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return nil, fmt.Errorf("new peer connection: %w", err)
	}

	p := &Peer{pc: pc}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		if p.OnICECandidate != nil {
			p.OnICECandidate(c.ToJSON())
		}
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			if p.OnClosed != nil {
				p.OnClosed()
			}
		}
	})

	return p, nil
}
```

- [ ] **Step 2: Update call sites in main.go**

The current call at line 172:
```go
p, err := peer.New(iceServers)
```

Change to:
```go
p, err := peer.New(iceServers, false) // false = Direct mode (default)
```

- [ ] **Step 3: Run tests**

Run: `cd agent && go test ./internal/peer/... -v`
Expected: PASS (or update tests if needed)

- [ ] **Step 4: Add PeerID field to signaling Message struct**

Modify `agent/internal/signaling/client.go`, add PeerID field:

```go
// Message is any message received from the signaling server.
type Message struct {
	Type        string          `json:"type"`
	SessionID   string          `json:"session_id,omitempty"`  // Share code
	PeerID      string          `json:"peer_id,omitempty"`     // Unique peer connection ID
	SDP         string          `json:"sdp,omitempty"`
	Candidate   json.RawMessage `json:"candidate,omitempty"`
	Err         string          `json:"message,omitempty"`
	Code        string          `json:"code,omitempty"`
	Reconnected bool            `json:"reconnected,omitempty"`
	ICEServers  []ICEServer     `json:"ice_servers,omitempty"`
}
```

This enables routing answers and ICE candidates to the correct peer when multiple browsers connect to the same share.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/peer/peer.go agent/cmd/agent/main.go agent/internal/signaling/client.go
git commit -m "feat(agent): add relayOnly parameter and PeerID for multi-connection routing"
```

---

## Task 1: Update Session Model in Store

**Files:**
- Modify: `agent/internal/store/store.go`
- Modify: `agent/internal/store/store_test.go`

**Context:** The current store uses `shareURL` as the key and only stores `code`, `download_count`, `api_key_id`. The new model stores sessions keyed by `code` with full metadata.

- [ ] **Step 1: Define new SessionEntry struct**

Modify `agent/internal/store/store.go`, replace the existing `sessionEntry` struct and add new methods:

```go
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

// SessionEntry represents a single share session with full metadata.
type SessionEntry struct {
	Code         string    `json:"code"`
	ShareURL     string    `json:"share_url"`
	Password     string    `json:"password,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	MaxDownloads int       `json:"max_downloads,omitempty"` // 0 = unlimited
	Downloads    int       `json:"downloads"`
	RelayOnly    bool      `json:"relay_only"`
	CreatedAt    time.Time `json:"created_at"`
}

type storeData struct {
	AgentID  string          `json:"agent_id"`
	Sessions []SessionEntry  `json:"sessions"` // Array on disk
}

// Store persists session data to sessions.json.
type Store struct {
	mu       sync.Mutex
	filePath string
	data     *storeData // In-memory cache
}
```

- [ ] **Step 2: Update New() to load sessions**

```go
// New creates a Store, loading existing data from sessions.json.
func New() (*Store, error) {
	var dataDir string

	if envDir := os.Getenv("OPENCLOUDSHARE_DATA_DIR"); envDir != "" {
		dataDir = envDir
	} else {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("failed to get user home directory: %w", err)
		}
		dataDir = filepath.Join(homeDir, ".opencloudshare")
	}

	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create data directory %s: %w", dataDir, err)
	}

	filePath := filepath.Join(dataDir, "sessions.json")

	store := &Store{
		filePath: filePath,
	}

	// Load existing data
	data, err := store.load()
	if err != nil {
		return nil, fmt.Errorf("sessions.json is malformed: %w", err)
	}
	store.data = data

	return store, nil
}
```

- [ ] **Step 3: Add new methods for session management**

```go
// GetAgentID returns the agent's unique ID, generating one if needed.
func (s *Store) GetAgentID() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.data.AgentID == "" {
		s.data.AgentID = uuid.New().String()
		s.save()
	}

	return s.data.AgentID
}

// GetSession returns a session by code, or nil if not found.
func (s *Store) GetSession(code string) *SessionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.data.Sessions {
		if s.data.Sessions[i].Code == code {
			return &s.data.Sessions[i]
		}
	}
	return nil
}

// ListSessions returns all sessions (non-expired only if filter is true).
func (s *Store) ListSessions(filterExpired bool) []SessionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !filterExpired {
		return s.data.Sessions
	}

	var result []SessionEntry
	now := time.Now()
	for _, session := range s.data.Sessions {
		if session.ExpiresAt.After(now) {
			result = append(result, session)
		}
	}
	return result
}

// SaveSession adds or updates a session.
func (s *Store) SaveSession(session SessionEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Find and update existing, or append new
	for i, existing := range s.data.Sessions {
		if existing.Code == session.Code {
			s.data.Sessions[i] = session
			return s.save()
		}
	}

	s.data.Sessions = append(s.data.Sessions, session)
	return s.save()
}

// DeleteSession removes a session by code.
func (s *Store) DeleteSession(code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, session := range s.data.Sessions {
		if session.Code == code {
			s.data.Sessions = append(s.data.Sessions[:i], s.data.Sessions[i+1:]...)
			return s.save()
		}
	}
	return nil
}

// IncrementDownloads increments the download count for a session.
func (s *Store) IncrementDownloads(code string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.data.Sessions {
		if s.data.Sessions[i].Code == code {
			s.data.Sessions[i].Downloads++
			count := s.data.Sessions[i].Downloads
			return count, s.save()
		}
	}
	return 0, fmt.Errorf("session not found: %s", code)
}
```

- [ ] **Step 4: Update load() and save() methods**

```go
// load reads sessions.json. Returns empty storeData if file doesn't exist.
func (s *Store) load() (*storeData, error) {
	data := &storeData{
		Sessions: []SessionEntry{},
	}

	fileContent, err := os.ReadFile(s.filePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return data, nil
		}
		return nil, fmt.Errorf("failed to read sessions file: %w", err)
	}

	if err := json.Unmarshal(fileContent, data); err != nil {
		return nil, fmt.Errorf("malformed sessions.json: %w", err)
	}

	return data, nil
}

// save writes storeData to sessions.json atomically.
func (s *Store) save() error {
	tempFile := s.filePath + ".tmp"

	jsonData, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal store data: %w", err)
	}

	if err := os.WriteFile(tempFile, jsonData, 0600); err != nil {
		return fmt.Errorf("failed to write temp file: %w", err)
	}

	if err := os.Rename(tempFile, s.filePath); err != nil {
		os.Remove(tempFile)
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	return nil
}
```

- [ ] **Step 5: Remove old methods**

Delete the old methods: `GetCode`, `SetCode`, `GetDownloadCount`, `IncrementDownloadCount`, `SetAPIKeyID`, `GetAPIKeyID`.

- [ ] **Step 6: Run tests**

Run: `cd agent && go test ./internal/store/... -v`
Expected: Some tests may fail due to API changes — update them.

- [ ] **Step 7: Commit**

```bash
git add agent/internal/store/
git commit -m "feat(store): replace session model with full metadata, keyed by code"
```

---

## Task 2: Add Config File Persistence

**Files:**
- Modify: `agent/internal/config/config.go`
- Create: `agent/internal/config/config_test.go`

**Context:** Config currently only loads from env vars. Need to add JSON file persistence for the Settings UI. Env vars should only fill in empty/zero fields (file takes precedence).

- [ ] **Step 1: Update Config struct with new fields**

Modify `agent/internal/config/config.go`:

```go
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

type Config struct {
	SignalingURL        string `json:"signaling_url"`
	APIKey              string `json:"api_key"`
	AllowedHost         string `json:"allowed_host"`
	DefaultExpiry       int    `json:"default_expiry_hours"`    // Default: 24
	DefaultMaxDownloads int    `json:"default_max_downloads"`  // Default: 10, 0 = unlimited
	DefaultRelayOnly    bool   `json:"default_relay_only"`
	UIPort              int    `json:"ui_port"`                // Default: 7878
	UIPassword          string `json:"ui_password,omitempty"`
}

type Manager struct {
	filePath string
	config   *Config
}

// NewManager creates a config manager that loads from file with env var fallback.
func NewManager() (*Manager, error) {
	var dataDir string

	if envDir := os.Getenv("OPENCLOUDSHARE_DATA_DIR"); envDir != "" {
		dataDir = envDir
	} else {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("failed to get user home directory: %w", err)
		}
		dataDir = filepath.Join(homeDir, ".opencloudshare")
	}

	filePath := filepath.Join(dataDir, "config.json")

	m := &Manager{
		filePath: filePath,
	}

	cfg, err := m.load()
	if err != nil {
		return nil, err
	}
	m.config = cfg

	return m, nil
}

// Get returns the current config.
func (m *Manager) Get() *Config {
	return m.config
}

// Save persists config to file.
func (m *Manager) Save(cfg *Config) error {
	m.config = cfg
	return m.save()
}

// load reads config.json, then fills empty fields from env vars.
// File takes precedence; env vars are fallback only.
func (m *Manager) load() (*Config, error) {
	cfg := &Config{
		DefaultExpiry:       24,
		DefaultMaxDownloads: 10,
		UIPort:              7878,
	}

	// Try reading file first
	fileContent, err := os.ReadFile(m.filePath)
	if err == nil {
		if err := json.Unmarshal(fileContent, cfg); err != nil {
			return nil, fmt.Errorf("malformed config.json: %w", err)
		}
	}

	// Fill empty/zero fields from env vars (env is fallback, not override)
	if cfg.SignalingURL == "" {
		if v := os.Getenv("SIGNALING_SERVER"); v != "" {
			cfg.SignalingURL = v
		}
	}
	if cfg.APIKey == "" {
		if v := os.Getenv("OPENCLOUDSHARE_API_KEY"); v != "" {
			cfg.APIKey = v
		}
	}
	if cfg.AllowedHost == "" {
		if v := os.Getenv("ALLOWED_OPENCLOUD_HOST"); v != "" {
			cfg.AllowedHost = v
		}
	}
	if cfg.UIPort == 0 {
		if v := os.Getenv("UI_PORT"); v != "" {
			fmt.Sscanf(v, "%d", &cfg.UIPort)
		}
	}
	if cfg.UIPassword == "" {
		if v := os.Getenv("UI_PASSWORD"); v != "" {
			cfg.UIPassword = v
		}
	}

	return cfg, nil
}

// save writes config to config.json atomically.
func (m *Manager) save() error {
	tempFile := m.filePath + ".tmp"

	jsonData, err := json.MarshalIndent(m.config, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	if err := os.WriteFile(tempFile, jsonData, 0600); err != nil {
		return fmt.Errorf("failed to write temp file: %w", err)
	}

	if err := os.Rename(tempFile, m.filePath); err != nil {
		os.Remove(tempFile)
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	return nil
}

// Load is kept for backward compatibility with existing code.
// Deprecated: Use NewManager().Get() instead.
func Load() *Config {
	m, err := NewManager()
	if err != nil {
		return &Config{
			SignalingURL:        getEnv("SIGNALING_SERVER", "ws://localhost:8080"),
			APIKey:              getEnv("OPENCLOUDSHARE_API_KEY", ""),
			AllowedHost:         getEnv("ALLOWED_OPENCLOUD_HOST", ""),
			DefaultExpiry:       24,
			DefaultMaxDownloads: 10,
			UIPort:              7878,
		}
	}
	return m.Get()
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
```

- [ ] **Step 2: Commit**

```bash
git add agent/internal/config/
git commit -m "feat(config): add JSON file persistence with env var fallback"
```

---

## Task 3: Create Daemon Package

**Files:**
- Create: `agent/internal/daemon/daemon.go`
- Create: `agent/internal/daemon/daemon_test.go`

**Context:** The daemon manages multiple concurrent sessions, a single signaling connection, and the web server.

- [ ] **Step 1: Create Daemon struct**

Create `agent/internal/daemon/daemon.go`:

```go
package daemon

import (
	"context"
	"encoding/json"
	"log"
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

// Session represents an active share session.
type Session struct {
	Code         string
	ShareURL     string
	Password     string
	ExpiresAt    time.Time
	MaxDownloads int
	Downloads    int
	RelayOnly    bool
	CreatedAt    time.Time

	// Runtime state
	webdavClient *opencloud.Client
	peers        map[string]*peer.Peer // peerID -> Peer (peerID from signaling server)
	mu           sync.Mutex
}

// Daemon manages multiple concurrent sessions and the web UI.
type Daemon struct {
	config     *config.Config
	configMgr  *config.Manager
	store      *store.Store
	signaling  *signaling.Client
	sessions   map[string]*Session // code -> Session
	mu         sync.RWMutex
	webServer  *WebServer

	// Callbacks
	OnSessionAdded   func(code string)
	OnSessionRemoved func(code string)
}

// New creates a new daemon instance.
func New(cfgMgr *config.Manager, st *store.Store) (*Daemon, error) {
	return &Daemon{
		config:    cfgMgr.Get(),
		configMgr: cfgMgr,
		store:     st,
		sessions:  make(map[string]*Session),
	}, nil
}

// Start begins the daemon: connects to signaling, starts web server, loads sessions.
// Returns an error channel that will receive web server errors.
func (d *Daemon) Start(ctx context.Context) <-chan error {
	errCh := make(chan error, 1)

	// Connect to signaling server
	agentID := d.store.GetAgentID()
	sig := signaling.New(d.config.SignalingURL, d.config.APIKey, agentID)

	if err := sig.Connect(ctx); err != nil {
		go func() { errCh <- err }()
		return errCh
	}
	d.signaling = sig

	// Set up message handler
	sig.OnMessage = d.handleSignalingMessage

	// Load and re-register existing sessions
	d.loadSessionsFromStore(ctx)

	// Start expiry pruner
	go d.runExpiryPruner(ctx)

	// Start web server
	webServer, err := NewWebServer(d, d.config.UIPort, d.config.UIPassword)
	if err != nil {
		go func() { errCh <- err }()
		return errCh
	}
	d.webServer = webServer
	go func() {
		if err := webServer.Start(); err != nil {
			errCh <- err
		}
	}()

	return errCh
}

	return nil
}

// Stop gracefully shuts down the daemon.
func (d *Daemon) Stop() error {
	if d.webServer != nil {
		d.webServer.Stop()
	}
	if d.signaling != nil {
		// Close signaling connection
	}
	return nil
}

// CreateSession creates a new share session.
func (d *Daemon) CreateSession(ctx context.Context, shareURL, password string, expiryDuration time.Duration, maxDownloads int, relayOnly bool) (string, error) {
	// Validate share URL against allowed host
	webdavClient, err := opencloud.New(shareURL, d.config.AllowedHost, password)
	if err != nil {
		return "", err
	}

	// Register with signaling server
	code, _, err := d.signaling.RegisterShare(ctx, shareURL, "")
	if err != nil {
		return "", err
	}

	// Create session
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

	// Persist to store
	d.store.SaveSession(store.SessionEntry{
		Code:         session.Code,
		ShareURL:     session.ShareURL,
		Password:     session.Password,
		ExpiresAt:    session.ExpiresAt,
		MaxDownloads: session.MaxDownloads,
		Downloads:    session.Downloads,
		RelayOnly:    session.RelayOnly,
		CreatedAt:    session.CreatedAt,
	})

	// Add to memory
	d.mu.Lock()
	d.sessions[code] = session
	d.mu.Unlock()

	if d.OnSessionAdded != nil {
		d.OnSessionAdded(code)
	}

	return code, nil
}

// RevokeSession removes a session.
func (d *Daemon) RevokeSession(code string) error {
	d.mu.Lock()
	session, exists := d.sessions[code]
	if exists {
		// Close all peers
		for _, p := range session.peers {
			p.Close()
		}
		delete(d.sessions, code)
	}
	d.mu.Unlock()

	// Remove from store
	d.store.DeleteSession(code)

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
	for _, s := range d.sessions {
		sessions = append(sessions, s)
	}
	return sessions
}

// GetSession returns a session by code.
func (d *Daemon) GetSession(code string) *Session {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.sessions[code]
}

// handleSignalingMessage dispatches signaling messages to the correct session.
func (d *Daemon) handleSignalingMessage(msg signaling.Message) {
	switch msg.Type {
	case "join":
		d.handleBrowserJoin(msg.SessionID, msg.PeerID)
	case "answer":
		d.handleAnswer(msg.PeerID, msg.SDP)
	case "ice_candidate":
		d.handleICECandidate(msg.PeerID, msg.Candidate)
	}
}

// handleBrowserJoin handles a new browser connection.
// sessionCode is the share code, peerID is a unique identifier for this connection.
func (d *Daemon) handleBrowserJoin(sessionCode string, peerID string) {
	d.mu.RLock()
	session, exists := d.sessions[sessionCode]
	d.mu.RUnlock()

	if !exists {
		log.Printf("join for unknown session: %s", sessionCode)
		return
	}

	// Check if session expired
	if time.Now().After(session.ExpiresAt) {
		log.Printf("join for expired session: %s", sessionCode)
		return
	}

	// Check max downloads
	if session.MaxDownloads > 0 && session.Downloads >= session.MaxDownloads {
		log.Printf("join for session at max downloads: %s", sessionCode)
		return
	}

	// Create peer
	iceServers := d.signaling.GetICEServers()
	p, err := peer.New(iceServers, session.RelayOnly)
	if err != nil {
		log.Printf("create peer: %v", err)
		return
	}

	session.mu.Lock()
	session.peers[peerID] = p
	session.mu.Unlock()

	p.OnClosed = func() {
		session.mu.Lock()
		delete(session.peers, peerID)
		session.mu.Unlock()
	}

	// Set up transfer manager
	tm := transfer.NewManager(p, session.webdavClient, session.Password, session.MaxDownloads)
	tm.SetDownloadCount(session.Downloads)

	// Wire callbacks
	p.OnOpen = func() {
		tm.HandleOpen()
	}

	// Create offer
	sdp, err := p.CreateOffer()
	if err != nil {
		log.Printf("create offer: %v", err)
		return
	}
	p.SetOnMessage(tm.HandleMessage)

	// Send offer via signaling (use peerID for routing back to this peer)
	d.signaling.Send(context.Background(), map[string]any{
		"type":       "offer",
		"session_id": sessionCode,
		"peer_id":    peerID,
		"sdp":        sdp,
	})
}

func (d *Daemon) handleAnswer(peerID string, sdp string) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	// Find the session that owns this peer
	for _, session := range d.sessions {
		session.mu.Lock()
		if p, ok := session.peers[peerID]; ok {
			p.SetAnswer(sdp)
			session.mu.Unlock()
			return
		}
		session.mu.Unlock()
	}
}

func (d *Daemon) handleICECandidate(peerID string, candidate json.RawMessage) {
	var init webrtc.ICECandidateInit
	if err := json.Unmarshal(candidate, &init); err != nil {
		return
	}

	d.mu.RLock()
	defer d.mu.RUnlock()

	// Find the session that owns this peer
	for _, session := range d.sessions {
		session.mu.Lock()
		if p, ok := session.peers[peerID]; ok {
			p.AddICECandidate(init)
			session.mu.Unlock()
			return
		}
		session.mu.Unlock()
	}
}

// loadSessionsFromStore loads sessions from store and re-registers them.
func (d *Daemon) loadSessionsFromStore(ctx context.Context) {
	sessions := d.store.ListSessions(false) // Include expired for cleanup
	now := time.Now()

	for _, entry := range sessions {
		if entry.ExpiresAt.Before(now) {
			// Skip expired
			continue
		}

		// Re-register with signaling
		code, _, err := d.signaling.RegisterShare(ctx, entry.ShareURL, entry.Code)
		if err != nil {
			log.Printf("failed to re-register session %s: %v", entry.Code, err)
			continue
		}

		// Create webdav client
		webdavClient, err := opencloud.New(entry.ShareURL, d.config.AllowedHost, entry.Password)
		if err != nil {
			log.Printf("failed to create webdav client for %s: %v", entry.Code, err)
			continue
		}

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
	}
}

// runExpiryPruner periodically removes expired sessions.
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

func (d *Daemon) pruneExpiredSessions() {
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()

	for code, session := range d.sessions {
		if session.ExpiresAt.Before(now) {
			log.Printf("pruning expired session: %s", code)
			// Close peers
			for _, p := range session.peers {
				p.Close()
			}
			delete(d.sessions, code)
			d.store.DeleteSession(code)
		}
	}
}
```

- [ ] **Step 2: Commit**

```bash
git add agent/internal/daemon/
git commit -m "feat(daemon): add Daemon struct with session management and expiry pruning"
```

---

## Task 4: Create Web Server Package

**Files:**
- Create: `agent/internal/web/server.go`

**Context:** HTTP server with embedded assets, CSRF protection, and routes.

- [ ] **Step 1: Create server.go with embedded assets**

Create `agent/internal/web/server.go`:

```go
package web

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"

	"opencloudshare/agent/internal/daemon"
)

//go:embed static/* templates/*
var embeddedFS embed.FS

type WebServer struct {
	daemon     *daemon.Daemon
	port       int
	password   string
	server     *http.Server
	templates  *template.Template
	staticFS   http.FileSystem
}

func NewWebServer(d *daemon.Daemon, port int, password string) (*WebServer, error) {
	// Parse templates - layout must be parsed first, then pages
	tmpl := template.New("")
	
	// Parse layout first
	layoutContent, err := embeddedFS.ReadFile("templates/layout.html")
	if err != nil {
		return nil, fmt.Errorf("read layout: %w", err)
	}
	if _, err := tmpl.Parse(string(layoutContent)); err != nil {
		return nil, fmt.Errorf("parse layout: %w", err)
	}
	
	// Parse all other templates
	tmpl, err = tmpl.ParseFS(embeddedFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}

	// Create sub-filesystem for static files
	staticSub, err := fs.Sub(embeddedFS, "static")
	if err != nil {
		return nil, fmt.Errorf("create static subfs: %w", err)
	}

	return &WebServer{
		daemon:    d,
		port:      port,
		password:  password,
		templates: tmpl,
		staticFS:  http.FS(staticSub),
	}, nil
}

func (s *WebServer) Start() error {
	mux := http.NewServeMux()

	// Static files
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(s.staticFS)))

	// Page routes
	mux.HandleFunc("/", s.csrfMiddleware(s.handleDashboard))
	mux.HandleFunc("/settings", s.csrfMiddleware(s.handleSettings))
	mux.HandleFunc("/history", s.csrfMiddleware(s.handleHistory))

	// API routes
	mux.HandleFunc("/api/shares", s.csrfMiddleware(s.handleSharesAPI))
	mux.HandleFunc("DELETE /api/shares/{code}", s.csrfMiddleware(s.handleRevokeShare))
	mux.HandleFunc("/api/share-form", s.csrfMiddleware(s.handleShareForm))
	mux.HandleFunc("/api/status", s.csrfMiddleware(s.handleStatus))
	mux.HandleFunc("/api/settings", s.csrfMiddleware(s.handleSettingsAPI))

	s.server = &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", s.port),
		Handler: s.authMiddleware(mux),
	}

	log.Printf("web UI listening on http://127.0.0.1:%d", s.port)
	return s.server.ListenAndServe()
}

func (s *WebServer) Stop() error {
	if s.server != nil {
		return s.server.Close()
	}
	return nil
}

// csrfMiddleware verifies X-Requested-With header on non-GET requests.
func (s *WebServer) csrfMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			if r.Header.Get("X-Requested-With") != "XMLHttpRequest" {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	}
}

// authMiddleware adds optional basic auth.
func (s *WebServer) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.password == "" {
			next.ServeHTTP(w, r)
			return
		}

		_, pass, ok := r.BasicAuth()
		if !ok || pass != s.password {
			w.Header().Set("WWW-Authenticate", `Basic realm="OpenCloudShare"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
```

- [ ] **Step 2: Commit**

```bash
git add agent/internal/web/server.go
git commit -m "feat(web): add HTTP server with embedded assets and CSRF protection"
```

---

## Task 5: Create Templates

**Files:**
- Create: `agent/internal/web/templates/layout.html`
- Create: `agent/internal/web/templates/dashboard.html`
- Create: `agent/internal/web/templates/settings.html`
- Create: `agent/internal/web/templates/history.html`
- Create: `agent/internal/web/templates/share-form.html`
- Create: `agent/internal/web/templates/share-card.html`

- [ ] **Step 1: Create layout.html**

```html
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>OpenCloudShare Agent</title>
  <link rel="stylesheet" href="/static/pico.min.css">
  <link rel="stylesheet" href="/static/custom.css">
  <script src="/static/htmx.min.js" defer></script>
</head>
<body>
  <header class="container">
    <nav>
      <ul>
        <li><strong><a href="/">OpenCloudShare</a></strong></li>
      </ul>
      <ul>
        <li><a href="/">Dashboard</a></li>
        <li><a href="/history">History</a></li>
        <li><a href="/settings">Settings</a></li>
      </ul>
    </nav>
  </header>

  <main class="container">
    {{template "content" .}}
  </main>

  <footer class="container">
    <small>OpenCloudShare Agent v1.0.0 · <a href="#">GitHub</a> · <a href="#">Docs</a></small>
  </footer>

  <div id="modal-container"></div>
</body>
</html>
```

- [ ] **Step 2: Create dashboard.html**

```html
{{define "content"}}
<div class="grid">
  <div>
    <h2>Active Shares</h2>
  </div>
  <div style="text-align: right;">
    <button hx-get="/api/share-form" hx-target="#modal-container" hx-swap="innerHTML">+ New Share</button>
  </div>
</div>

<div id="shares-list" hx-get="/api/shares" hx-trigger="load, every 10s">
  <p><em>Loading...</em></p>
</div>
{{end}}
```

- [ ] **Step 3: Create share-card.html**

```html
{{define "share-card"}}
<article>
  <div class="grid">
    <div>
      <strong>{{.Code}}</strong>
      <br>
      <small>{{.ShareURL}}</small>
    </div>
    <div style="text-align: right;">
      {{if .RelayOnly}}
      <span class="badge badge-relay">Relay</span>
      {{else}}
      <span class="badge badge-direct">Direct</span>
      {{end}}
    </div>
  </div>
  <div class="grid">
    <div>
      <small>Downloads: {{.Downloads}}{{if gt .MaxDownloads 0}}/{{.MaxDownloads}}{{else}}/∞{{end}}</small>
    </div>
    <div>
      <small>Expires: {{.ExpiresAt.Format "2006-01-02"}}</small>
    </div>
  </div>
  <button class="btn-revoke" hx-delete="/api/shares/{{.Code}}" hx-target="closest article" hx-swap="outerHTML swap:0.5s">Revoke</button>
</article>
{{end}}
```

- [ ] **Step 4: Create settings.html**

```html
{{define "content"}}
<h2>Settings</h2>

<form hx-put="/api/settings" hx-swap="none">
  <h3>Signaling Server</h3>
  <label>
    Server URL
    <input type="url" name="signaling_url" value="{{.SignalingURL}}" required>
  </label>
  <label>
    API Key
    <input type="password" name="api_key" value="{{.APIKey}}">
  </label>

  <h3>OpenCloud</h3>
  <label>
    Allowed Host
    <input type="text" name="allowed_host" value="{{.AllowedHost}}" placeholder="opencloud.example.com">
    <small>Only shares from this host are allowed (SSRF protection)</small>
  </label>

  <h3>Default Share Settings</h3>
  <div class="grid">
    <label>
      Default Expiry
      <select name="default_expiry">
        <option value="1" {{if eq .DefaultExpiry 1}}selected{{end}}>1 hour</option>
        <option value="24" {{if eq .DefaultExpiry 24}}selected{{end}}>24 hours</option>
        <option value="168" {{if eq .DefaultExpiry 168}}selected{{end}}>7 days</option>
      </select>
    </label>
    <label>
      Default Max Downloads
      <input type="number" name="default_max_downloads" value="{{.DefaultMaxDownloads}}" placeholder="0 = unlimited">
    </label>
  </div>
  <label>
    Default Mode
    <select name="default_relay_only">
      <option value="false" {{if not .DefaultRelayOnly}}selected{{end}}>Direct (recommended)</option>
      <option value="true" {{if .DefaultRelayOnly}}selected{{end}}>Relay (hide IP)</option>
    </select>
  </label>

  <button type="submit">Save Settings</button>
</form>

<hr>

<h3>About</h3>
<p><strong>OpenCloudShare Agent</strong> v1.0.0</p>
<p><a href="#">GitHub</a> · <a href="#">Documentation</a> · <a href="#">Report Issue</a></p>
{{end}}
```

- [ ] **Step 5: Create history.html**

```html
{{define "content"}}
<h2>Download History</h2>

<article class="placeholder">
  <p>📋 Download history coming soon</p>
  <p><small>Track what files were downloaded and when.</small></p>
</article>

<details>
  <summary>Implementation TODO</summary>
  <pre>
- Store: session_code, file_name, size, timestamp
- Display: table with filters
- Retention: last 30 days
  </pre>
</details>
{{end}}
```

- [ ] **Step 6: Create share-form.html**

```html
<dialog id="share-dialog">
  <article>
    <header>
      <a href="#/" class="close" hx-on:click="document.getElementById('share-dialog').close()">&times;</a>
      <h3>Create New Share</h3>
    </header>

    <form hx-post="/api/shares" hx-target="#shares-list" hx-swap="beforeend" hx-on::after-request="document.getElementById('share-dialog').close()">
      <label>
        OpenCloud Share URL
        <input type="url" name="share_url" placeholder="https://opencloud.example.com/s/xyz789" required>
      </label>

      <label>
        Password (optional)
        <input type="password" name="password" placeholder="Leave empty if share has no password">
      </label>

      <fieldset>
        <legend>Connection Mode</legend>

        <label>
          <input type="radio" name="relay_only" value="false" checked>
          <strong>Direct (recommended)</strong><br>
          <small>Fast, free, uses TURN only as fallback</small>
          <div class="warning warning-amber">
            ⚠️ Your IP will be visible to recipients if direct connection succeeds
          </div>
        </label>

        <label>
          <input type="radio" name="relay_only" value="true">
          <strong>Relay (hide IP)</strong><br>
          <small>All traffic via TURN server, IP never exposed</small>
        </label>
      </fieldset>

      <div class="grid">
        <label>
          Expires After
          <select name="expiry_hours">
            <option value="1">1 hour</option>
            <option value="24" selected>24 hours</option>
            <option value="168">7 days</option>
          </select>
        </label>
        <label>
          Max Downloads
          <input type="number" name="max_downloads" value="10" placeholder="0 = unlimited">
          <small>💡 Recommended: limit downloads to prevent link abuse</small>
        </label>
      </div>

      <footer>
        <button type="button" hx-on:click="document.getElementById('share-dialog').close()">Cancel</button>
        <button type="submit">Create Share</button>
      </footer>
    </form>
  </article>
  <script>document.getElementById('share-dialog').showModal()</script>
</dialog>
```
    </form>
  </article>
</dialog>
```

- [ ] **Step 7: Commit**

```bash
git add agent/internal/web/templates/
git commit -m "feat(web): add HTML templates for dashboard, settings, history, share form"
```

---

## Task 6: Create Static Files

**Files:**
- Create: `agent/internal/web/static/pico.min.css`
- Create: `agent/internal/web/static/htmx.min.js`
- Create: `agent/internal/web/static/custom.css`

**Context:** Download PicoCSS and HTMX from their CDNs and embed in the binary.

- [ ] **Step 1: Download PicoCSS**

Run:
```bash
curl -L "https://cdn.jsdelivr.net/npm/@picocss/pico@2/css/pico.min.css" -o agent/internal/web/static/pico.min.css
```

- [ ] **Step 2: Download HTMX**

Run:
```bash
curl -L "https://unpkg.com/htmx.org@1.9.10/dist/htmx.min.js" -o agent/internal/web/static/htmx.min.js
```

- [ ] **Step 3: Create custom.css**

Create `agent/internal/web/static/custom.css`:

```css
/* Mode badges */
.badge {
  display: inline-block;
  padding: 0.25rem 0.5rem;
  border-radius: 4px;
  font-size: 0.75rem;
  font-weight: 500;
}

.badge-direct {
  background: #dbeafe;
  color: #1d4ed8;
}

.badge-relay {
  background: #fef3c7;
  color: #b45309;
}

/* Warning boxes */
.warning {
  padding: 0.5rem;
  border-radius: 4px;
  margin-top: 0.25rem;
  font-size: 0.875rem;
}

.warning-amber {
  background: #fef3c7;
  color: #b45309;
}

.warning-red {
  background: #fee2e2;
  color: #dc2626;
}

/* Revoke button */
.btn-revoke {
  background: #fee2e2;
  color: #dc2626;
  border: none;
  padding: 0.25rem 0.75rem;
  border-radius: 4px;
  cursor: pointer;
}

/* Placeholder */
.placeholder {
  text-align: center;
  padding: 2rem;
  color: #666;
}

/* Dialog */
dialog article {
  max-width: 500px;
}

dialog .close {
  float: right;
  text-decoration: none;
  font-size: 1.5rem;
}
```

- [ ] **Step 4: Commit**

```bash
git add agent/internal/web/static/
git commit -m "feat(web): add embedded PicoCSS, HTMX, and custom styles"
```

---

## Task 7: Create API Handlers

**Files:**
- Create: `agent/internal/web/handlers.go`
- Create: `agent/internal/web/api.go`

- [ ] **Step 1: Create handlers.go for page routes**

Create `agent/internal/web/handlers.go`:

```go
package web

import (
	"net/http"
)

// handleDashboard renders the main page.
func (s *WebServer) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	s.renderPage(w, "dashboard.html", nil)
}

// handleSettings renders the settings page.
func (s *WebServer) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, "settings.html", s.daemon.GetConfig())
}

// handleHistory renders the history page.
func (s *WebServer) handleHistory(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, "history.html", nil)
}

// renderPage renders a template with the layout.
func (s *WebServer) renderPage(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html")
	if err := s.templates.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
```

- [ ] **Step 2: Create api.go for HTMX endpoints**

Create `agent/internal/web/api.go`:

```go
package web

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"opencloudshare/agent/internal/config"
)

// handleSharesAPI handles GET (list) and POST (create) for shares.
func (s *WebServer) handleSharesAPI(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listShares(w, r)
	case http.MethodPost:
		s.createShare(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *WebServer) listShares(w http.ResponseWriter, r *http.Request) {
	sessions := s.daemon.ListSessions()

	w.Header().Set("Content-Type", "text/html")
	for _, session := range sessions {
		s.templates.ExecuteTemplate(w, "share-card", session)
	}
}

func (s *WebServer) createShare(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}

	shareURL := r.FormValue("share_url")
	password := r.FormValue("password")
	relayOnly := r.FormValue("relay_only") == "true"
	maxDownloads, _ := strconv.Atoi(r.FormValue("max_downloads"))
	expiryHours, _ := strconv.Atoi(r.FormValue("expiry_hours"))
	if expiryHours == 0 {
		expiryHours = s.daemon.GetConfig().DefaultExpiry
	}

	code, err := s.daemon.CreateSession(r.Context(), shareURL, password, time.Duration(expiryHours)*time.Hour, maxDownloads, relayOnly)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Return the new card HTML
	session := s.daemon.GetSession(code)
	w.Header().Set("Content-Type", "text/html")
	s.templates.ExecuteTemplate(w, "share-card", session)
}

// handleRevokeShare handles DELETE /api/shares/{code}
func (s *WebServer) handleRevokeShare(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")

	if err := s.daemon.RevokeSession(code); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// handleShareForm returns the new share form modal.
func (s *WebServer) handleShareForm(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	s.templates.ExecuteTemplate(w, "share-form.html", s.daemon.GetConfig())
}

// handleStatus returns connection status.
func (s *WebServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	status := "Connected"
	if !s.daemon.IsConnected() {
		status = "Disconnected"
	}

	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, `<span class="status">%s</span>`, status)
}

// handleSettingsAPI handles PUT to save settings (form-encoded).
func (s *WebServer) handleSettingsAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}

	cfg := s.daemon.GetConfig()
	cfg.SignalingURL = r.FormValue("signaling_url")
	cfg.APIKey = r.FormValue("api_key")
	cfg.AllowedHost = r.FormValue("allowed_host")
	cfg.DefaultExpiry, _ = strconv.Atoi(r.FormValue("default_expiry"))
	cfg.DefaultMaxDownloads, _ = strconv.Atoi(r.FormValue("default_max_downloads"))
	cfg.DefaultRelayOnly = r.FormValue("default_relay_only") == "true"

	if err := s.daemon.SaveConfig(cfg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}
```
	}

	if err := s.daemon.RevokeSession(code); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// handleShareForm returns the new share form modal.
func (s *WebServer) handleShareForm(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	s.templates.ExecuteTemplate(w, "share-form.html", s.daemon.GetConfig())
}

// handleStatus returns connection status.
func (s *WebServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	status := "Connected"
	if !s.daemon.IsConnected() {
		status = "Disconnected"
	}

	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, `<span class="status">%s</span>`, status)
}

// handleSettingsAPI handles PUT to save settings.
func (s *WebServer) handleSettingsAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var cfg config.Config
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if err := s.daemon.SaveConfig(&cfg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}
```

- [ ] **Step 3: Commit**

```bash
git add agent/internal/web/handlers.go agent/internal/web/api.go
git commit -m "feat(web): add page handlers and HTMX API endpoints"
```

---

## Task 8: Update main.go for Daemon Subcommand

**Files:**
- Modify: `agent/cmd/agent/main.go`

**Context:** Add `daemon` subcommand and client mode fallback.

- [ ] **Step 1: Add imports and daemon command**

Modify `agent/cmd/agent/main.go`, add imports:

```go
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/spf13/cobra"
	"opencloudshare/agent/internal/config"
	"opencloudshare/agent/internal/daemon"
	"opencloudshare/agent/internal/opencloud"
	"opencloudshare/agent/internal/peer"
	"opencloudshare/agent/internal/signaling"
	"opencloudshare/agent/internal/store"
	"opencloudshare/agent/internal/transfer"
)
```

- [ ] **Step 2: Add daemon subcommand**

Add after `shareCmd` definition:

```go
var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Start the persistent daemon with web UI",
	RunE:  runDaemon,
}

func init() {
	rootCmd.AddCommand(daemonCmd)
	rootCmd.AddCommand(shareCmd)
	shareCmd.Flags().StringVarP(&password, "password", "p", "", "Optional password for share access")
	shareCmd.Flags().IntVarP(&maxDownloads, "max-downloads", "n", 0, "Maximum number of downloads (0=unlimited)")
}
```

- [ ] **Step 3: Implement runDaemon function**

Add new function:

```go
func runDaemon(cmd *cobra.Command, args []string) error {
	cfgMgr, err := config.NewManager()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	cfg := cfgMgr.Get()

	if cfg.APIKey == "" {
		return fmt.Errorf("OPENCLOUDSHARE_API_KEY environment variable required")
	}

	st, err := store.New()
	if err != nil {
		return fmt.Errorf("session store: %w", err)
	}

	d, err := daemon.New(cfgMgr, st)
	if err != nil {
		return fmt.Errorf("create daemon: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	log.Printf("starting OpenCloudShare daemon...")
	if err := d.Start(ctx); err != nil {
		return fmt.Errorf("daemon error: %w", err)
	}

	<-ctx.Done()
	log.Println("shutting down...")
	return d.Stop()
}
```

- [ ] **Step 4: Update runShare for client mode**

Replace existing `runShare` function:

```go
func runShare(cmd *cobra.Command, args []string) error {
	shareURL := args[0]

	// Check if daemon is running
	if daemonRunning() {
		return runShareClient(shareURL)
	}

	// Fallback: single-session mode (current behavior)
	return runShareSingle(shareURL)
}

func daemonRunning() bool {
	client := &http.Client{Timeout: 1 * time.Second}
	resp, err := client.Get("http://127.0.0.1:7878/api/status")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func runShareClient(shareURL string) error {
	cfg := config.Load()

	payload := map[string]any{
		"share_url":     shareURL,
		"password":      password,
		"max_downloads": maxDownloads,
		"expiry_hours":  cfg.DefaultExpiry,
		"relay_only":    cfg.DefaultRelayOnly,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	resp, err := http.Post("http://127.0.0.1:7878/api/shares", "application/json", bytes.NewReader(jsonData))
	if err != nil {
		return fmt.Errorf("post to daemon: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("daemon error: %s", string(body))
	}

	// Read the response (HTML card) but just print success
	fmt.Printf("Share added to daemon. Open http://127.0.0.1:7878 to manage.\n")
	return nil
}

func runShareSingle(shareURL string) error {
	// Single-session mode (fallback when daemon not running)
	// This is the legacy CLI behavior - one share, one process
	cfg := config.Load()

	if cfg.APIKey == "" {
		return fmt.Errorf("OPENCLOUDSHARE_API_KEY environment variable required")
	}

	webdavClient, err := opencloud.New(shareURL, cfg.AllowedHost, password)
	if err != nil {
		return fmt.Errorf("create WebDAV client: %w", err)
	}

	st, err := store.New()
	if err != nil {
		return fmt.Errorf("session store: %w", err)
	}

	agentID := st.GetAgentID()
	// Note: Single-session mode doesn't use preferredCode persistence
	// Each run starts fresh - daemon mode handles persistence

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

		code, err := runSession(ctx, cfg, webdavClient, shareURL, password, maxDownloads, st, agentID)
		if err != nil {
			log.Printf("session ended: %v", err)
		} else {
			backoff.Reset()
		}

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
		}
	}
}

// runSession is the legacy single-session connection loop.
// Note: password and maxDownloads are per-session, not config fields.
func runSession(ctx context.Context, cfg *config.Config, webdavClient *opencloud.Client, shareURL string, sessionPassword string, sessionMaxDownloads int, st *store.Store, agentID string) (string, error) {
	sig := signaling.New(cfg.SignalingURL, cfg.APIKey, agentID)

	if err := sig.Connect(ctx); err != nil {
		return "", fmt.Errorf("connect to signaling server: %w", err)
	}
	log.Printf("connected to signaling server at %s", cfg.SignalingURL)

	// Register share (no preferredCode in single-session mode)
	code, reconnected, err := sig.RegisterShare(ctx, shareURL, "")
	if err != nil {
		return "", fmt.Errorf("register share: %w", err)
	}
	if reconnected {
		log.Printf("session reclaimed — code: %s", code)
	} else {
		log.Printf("session ready — code: %s", code)
	}
	log.Printf("open browser: http://localhost:8080 then enter code: %s", code)

	var (
		mu    sync.Mutex
		peers = make(map[string]*peer.Peer)
	)

	sig.OnMessage = func(msg signaling.Message) {
		switch msg.Type {
		case "welcome":
			log.Println("agent authenticated with signaling server")

		case "join":
			log.Printf("browser joined session %s — starting WebRTC handshake", msg.SessionID)
			peerID := msg.PeerID

			iceServers := sig.GetICEServers()
			if len(iceServers) == 0 {
				log.Printf("warning: no ICE servers received, using default STUN")
				iceServers = []webrtc.ICEServer{
					{URLs: []string{"stun:stun.cloudflare.com:3478"}},
				}
			}
			p, err := peer.New(iceServers, false) // false = Direct mode in single-session fallback
			if err != nil {
				log.Printf("create peer: %v", err)
				return
			}

			mu.Lock()
			peers[peerID] = p
			mu.Unlock()

			p.OnClosed = func() {
				log.Printf("peer closed (peer %s)", peerID)
				mu.Lock()
				delete(peers, peerID)
				mu.Unlock()
			}
			p.OnICECandidate = func(init webrtc.ICECandidateInit) {
				if err := sig.Send(ctx, map[string]any{
					"type":       "ice_candidate",
					"session_id": msg.SessionID,
					"peer_id":    peerID,
					"candidate":  init,
				}); err != nil {
					log.Printf("send ICE candidate: %v", err)
				}
			}

			tm := transfer.NewManager(p, webdavClient, sessionPassword, sessionMaxDownloads)

			p.OnOpen = func() {
				log.Printf("✓ DataChannel open! (peer %s)", peerID)
				tm.HandleOpen()
			}

			sdp, err := p.CreateOffer()
			if err != nil {
				log.Printf("create offer: %v", err)
				return
			}
			p.SetOnMessage(tm.HandleMessage)

			if err := sig.Send(ctx, map[string]any{
				"type":       "offer",
				"session_id": msg.SessionID,
				"peer_id":    peerID,
				"sdp":        sdp,
			}); err != nil {
				log.Printf("send offer: %v", err)
			}

		case "answer":
			mu.Lock()
			p, ok := peers[msg.PeerID]
			mu.Unlock()
			if !ok {
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
```

- [ ] **Step 5: Add Daemon helper methods**

Add these methods to the Daemon struct (in daemon.go):

```go
// GetConfig returns the current config.
func (d *Daemon) GetConfig() *config.Config {
	return d.config
}

// SaveConfig persists config changes.
func (d *Daemon) SaveConfig(cfg *config.Config) error {
	return d.configMgr.Save(cfg)
}

// IsConnected returns true if connected to signaling server.
func (d *Daemon) IsConnected() bool {
	return d.signaling != nil
}
```

- [ ] **Step 6: Commit**

```bash
git add agent/cmd/agent/main.go agent/internal/daemon/daemon.go
git commit -m "feat(cli): add daemon subcommand, client mode fallback, and peer routing"
```

---

## Task 9: Integration Testing

**Files:**
- Manual testing

**Context:** Verify the daemon starts, UI works, and shares can be created/revoked.

- [ ] **Step 1: Build and run daemon**

Run:
```bash
cd agent && go build -o opencloudshare ./cmd/agent
./opencloudshare daemon
```

- [ ] **Step 2: Open UI**

Open `http://localhost:7878` in browser.

- [ ] **Step 3: Create a share**

Use the UI to create a share. Verify the session card appears.

- [ ] **Step 4: Test revoke**

Click Revoke on a session. Verify it disappears.

- [ ] **Step 5: Test settings**

Change settings. Verify they persist after restart.

- [ ] **Step 6: Test CLI client**

Run `./opencloudshare share <url>` while daemon is running. Verify it adds a share to the daemon.

---