package daemon

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"opencloudshare/agent/internal/config"
	"opencloudshare/agent/internal/peer"
	"opencloudshare/agent/internal/signaling"
	"opencloudshare/agent/internal/store"
)

// mockConfigManager implements ConfigManagerInterface for testing.
type mockConfigManager struct {
	cfg *config.Config
}

func (m *mockConfigManager) Get() *config.Config {
	return m.cfg
}

// mockStore implements StoreInterface for testing.
type mockStore struct {
	mu         sync.Mutex
	agentID    string
	sessions   map[string]store.SessionEntry
	downloads  map[string]int
	saveError  error
}

func newMockStore() *mockStore {
	return &mockStore{
		agentID:   "test-agent-id",
		sessions:  make(map[string]store.SessionEntry),
		downloads: make(map[string]int),
	}
}

func (m *mockStore) GetAgentID() string {
	return m.agentID
}

func (m *mockStore) GetSession(code string) *store.SessionEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[code]; ok {
		return &s
	}
	return nil
}

func (m *mockStore) GetByShareURL(shareURL string) *store.SessionEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if s.ShareURL == shareURL {
			return &s
		}
	}
	return nil
}

func (m *mockStore) ListSessions(filterExpired bool) []store.SessionEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []store.SessionEntry
	now := time.Now()
	for _, s := range m.sessions {
		if !filterExpired || s.ExpiresAt.IsZero() || s.ExpiresAt.After(now) {
			result = append(result, s)
		}
	}
	return result
}

func (m *mockStore) SaveSession(session store.SessionEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.saveError != nil {
		return m.saveError
	}
	m.sessions[session.Code] = session
	return nil
}

func (m *mockStore) DeleteSession(code string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, code)
	return nil
}

func (m *mockStore) IncrementDownloads(code string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.downloads[code]++
	return m.downloads[code], nil
}

// mockSignalingClient implements SignalingClientInterface for testing.
type mockSignalingClient struct {
	mu            sync.Mutex
	serverURL     string
	apiKey        string
	agentID       string
	connected     bool
	onMessage     func(signaling.Message)
	iceServers    []webrtc.ICEServer
	registerShare func(ctx context.Context, shareURL, preferredCode string) (string, bool, error)
	sendMessages  []map[string]any
	codeCounter   int // Counter for generating unique codes
}

func newMockSignalingClient(serverURL, apiKey, agentID string) *mockSignalingClient {
	return &mockSignalingClient{
		serverURL:  serverURL,
		apiKey:     apiKey,
		agentID:    agentID,
		iceServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.cloudflare.com:3478"}}},
	}
}

func (m *mockSignalingClient) Connect(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connected = true
	return nil
}

func (m *mockSignalingClient) RegisterShare(ctx context.Context, shareURL, preferredCode string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.registerShare != nil {
		return m.registerShare(ctx, shareURL, preferredCode)
	}
	// Default: generate a unique code
	if preferredCode != "" {
		return preferredCode, true, nil
	}
	m.codeCounter++
	return fmt.Sprintf("test-code-%d", m.codeCounter), false, nil
}

func (m *mockSignalingClient) DownloadComplete(ctx context.Context, code string, bytesTransferred int64) error {
	return nil
}

func (m *mockSignalingClient) Send(ctx context.Context, msg any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Store the message regardless of type
	if msgMap, ok := msg.(map[string]any); ok {
		m.sendMessages = append(m.sendMessages, msgMap)
	} else if msgMapStr, ok := msg.(map[string]string); ok {
		// Convert map[string]string to map[string]any for consistent storage
		converted := make(map[string]any)
		for key, value := range msgMapStr {
			converted[key] = value
		}
		m.sendMessages = append(m.sendMessages, converted)
	}
	return nil
}

func (m *mockSignalingClient) GetICEServers() []webrtc.ICEServer {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.iceServers
}

func (m *mockSignalingClient) Listen(ctx context.Context) error {
	// Block until context is cancelled
	<-ctx.Done()
	return ctx.Err()
}

func (m *mockSignalingClient) SetOnMessage(handler func(signaling.Message)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onMessage = handler
}

func (m *mockSignalingClient) sendMessage(msg signaling.Message) {
	m.mu.Lock()
	handler := m.onMessage
	m.mu.Unlock()
	if handler != nil {
		handler(msg)
	}
}

// mockWebServer implements WebServer interface for testing.
type mockWebServer struct {
	started bool
	stopped bool
	daemon  *Daemon
}

func (m *mockWebServer) Start(ctx context.Context) error {
	m.started = true
	<-ctx.Done()
	return ctx.Err()
}

func (m *mockWebServer) Stop() error {
	m.stopped = true
	return nil
}

func (m *mockWebServer) SetDaemon(d *Daemon) {
	m.daemon = d
}

// TestNew tests daemon creation.
func TestNew(t *testing.T) {
	cfg := &config.Config{
		SignalingURL: "ws://localhost:8080",
		APIKey:       "test-api-key",
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()

	d, err := New(cfgMgr, st)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	if d.config.SignalingURL != cfg.SignalingURL {
		t.Errorf("config.SignalingURL mismatch")
	}
	if d.config.APIKey != cfg.APIKey {
		t.Errorf("config.APIKey mismatch")
	}
	if len(d.sessions) != 0 {
		t.Errorf("expected empty sessions map")
	}
}

// TestNewWithSignaling tests daemon creation with custom signaling client.
func TestNewWithSignaling(t *testing.T) {
	cfg := &config.Config{
		SignalingURL: "ws://localhost:8080",
		APIKey:       "test-api-key",
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	sigClient := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())

	d, err := NewWithSignaling(cfgMgr, st, sigClient)
	if err != nil {
		t.Fatalf("NewWithSignaling() error: %v", err)
	}

	if d.config.SignalingURL != cfg.SignalingURL {
		t.Errorf("config.SignalingURL mismatch")
	}
	if d.signaling != sigClient {
		t.Errorf("signaling client mismatch")
	}
}

// TestCreateSession tests session creation.
func TestCreateSession(t *testing.T) {
	cfg := &config.Config{
		SignalingURL: "ws://localhost:8080",
		APIKey:       "test-api-key",
		AllowedHost:  "opencloud.example.com",
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	sigClient := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())

	d, err := NewWithSignaling(cfgMgr, st, sigClient)
	if err != nil {
		t.Fatalf("NewWithSignaling() error: %v", err)
	}

	ctx := context.Background()
	code, err := d.CreateSession(ctx, "https://opencloud.example.com/s/abc123", "", 24*time.Hour, 10, false)
	if err != nil {
		t.Fatalf("CreateSession() error: %v", err)
	}

	if code == "" {
		t.Errorf("expected non-empty code")
	}

	// Verify session was added to in-memory map
	session := d.GetSession(code)
	if session == nil {
		t.Fatalf("session not found in daemon")
	}

	if session.ShareURL != "https://opencloud.example.com/s/abc123" {
		t.Errorf("ShareURL mismatch")
	}
	if session.MaxDownloads != 10 {
		t.Errorf("MaxDownloads mismatch")
	}
	if session.RelayOnly != false {
		t.Errorf("RelayOnly should be false")
	}

	// Verify session was persisted to store
	storeSession := st.GetSession(code)
	if storeSession == nil {
		t.Errorf("session not persisted to store")
	}
}

// TestCreateSessionWithInvalidHost tests session creation with invalid host.
func TestCreateSessionWithInvalidHost(t *testing.T) {
	cfg := &config.Config{
		SignalingURL: "ws://localhost:8080",
		APIKey:       "test-api-key",
		AllowedHost:  "opencloud.example.com",
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	sigClient := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())

	d, err := NewWithSignaling(cfgMgr, st, sigClient)
	if err != nil {
		t.Fatalf("NewWithSignaling() error: %v", err)
	}

	ctx := context.Background()
	_, err = d.CreateSession(ctx, "https://evil.example.com/s/abc123", "", 24*time.Hour, 10, false)
	if err == nil {
		t.Errorf("expected error for invalid host")
	}
}

// TestRevokeSession tests session revocation.
func TestRevokeSession(t *testing.T) {
	cfg := &config.Config{
		SignalingURL: "ws://localhost:8080",
		APIKey:       "test-api-key",
		AllowedHost:  "opencloud.example.com",
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	sigClient := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())

	d, err := NewWithSignaling(cfgMgr, st, sigClient)
	if err != nil {
		t.Fatalf("NewWithSignaling() error: %v", err)
	}

	ctx := context.Background()
	code, err := d.CreateSession(ctx, "https://opencloud.example.com/s/abc123", "", 24*time.Hour, 10, false)
	if err != nil {
		t.Fatalf("CreateSession() error: %v", err)
	}

	// Verify session exists
	if d.GetSession(code) == nil {
		t.Fatalf("session not found before revoke")
	}

	// Revoke session
	err = d.RevokeSession(code)
	if err != nil {
		t.Fatalf("RevokeSession() error: %v", err)
	}

	// Verify session was removed from in-memory map
	if d.GetSession(code) != nil {
		t.Errorf("session still exists in daemon after revoke")
	}

	// Verify session was removed from store
	if st.GetSession(code) != nil {
		t.Errorf("session still exists in store after revoke")
	}
}

// TestRevokeNonexistentSession tests revoking a nonexistent session.
func TestRevokeNonexistentSession(t *testing.T) {
	cfg := &config.Config{
		SignalingURL: "ws://localhost:8080",
		APIKey:       "test-api-key",
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	sigClient := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())

	d, err := NewWithSignaling(cfgMgr, st, sigClient)
	if err != nil {
		t.Fatalf("NewWithSignaling() error: %v", err)
	}

	err = d.RevokeSession("nonexistent-code")
	if err == nil {
		t.Errorf("expected error for nonexistent session")
	}
}

// TestListSessions tests listing sessions.
func TestListSessions(t *testing.T) {
	cfg := &config.Config{
		SignalingURL: "ws://localhost:8080",
		APIKey:       "test-api-key",
		AllowedHost:  "opencloud.example.com",
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	sigClient := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())

	d, err := NewWithSignaling(cfgMgr, st, sigClient)
	if err != nil {
		t.Fatalf("NewWithSignaling() error: %v", err)
	}

	ctx := context.Background()
	code1, err := d.CreateSession(ctx, "https://opencloud.example.com/s/abc123", "", 24*time.Hour, 10, false)
	if err != nil {
		t.Fatalf("CreateSession() error: %v", err)
	}
	code2, err := d.CreateSession(ctx, "https://opencloud.example.com/s/def456", "", 24*time.Hour, 5, true)
	if err != nil {
		t.Fatalf("CreateSession() error: %v", err)
	}

	sessions := d.ListSessions()
	if len(sessions) != 2 {
		t.Errorf("expected 2 sessions, got %d", len(sessions))
	}

	// Check that both sessions are in the list
	foundCode1 := false
	foundCode2 := false
	for _, session := range sessions {
		if session.Code == code1 {
			foundCode1 = true
		}
		if session.Code == code2 {
			foundCode2 = true
		}
	}
	if !foundCode1 || !foundCode2 {
		t.Errorf("expected both sessions in list")
	}
}

// TestPruneExpiredSessions tests expiry pruning.
func TestPruneExpiredSessions(t *testing.T) {
	cfg := &config.Config{
		SignalingURL: "ws://localhost:8080",
		APIKey:       "test-api-key",
		AllowedHost:  "opencloud.example.com",
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	sigClient := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())

	d, err := NewWithSignaling(cfgMgr, st, sigClient)
	if err != nil {
		t.Fatalf("NewWithSignaling() error: %v", err)
	}

	// Create an expired session manually
	expiredSession := &Session{
		Code:         "expired-code",
		ShareURL:     "https://opencloud.example.com/s/expired",
		ExpiresAt:    time.Now().Add(-1 * time.Hour), // Expired 1 hour ago
		CreatedAt:    time.Now().Add(-2 * time.Hour),
		peers:        make(map[string]*peer.Peer),
	}
	d.mu.Lock()
	d.sessions["expired-code"] = expiredSession
	d.mu.Unlock()
	st.SaveSession(store.SessionEntry{
		Code:      "expired-code",
		ShareURL:  "https://opencloud.example.com/s/expired",
		ExpiresAt: time.Now().Add(-1 * time.Hour),
	})

	// Create an active session manually
	activeSession := &Session{
		Code:         "active-code",
		ShareURL:     "https://opencloud.example.com/s/active",
		ExpiresAt:    time.Now().Add(24 * time.Hour), // Expires in 24 hours
		CreatedAt:    time.Now(),
		peers:        make(map[string]*peer.Peer),
	}
	d.mu.Lock()
	d.sessions["active-code"] = activeSession
	d.mu.Unlock()
	st.SaveSession(store.SessionEntry{
		Code:      "active-code",
		ShareURL:  "https://opencloud.example.com/s/active",
		ExpiresAt: time.Now().Add(24 * time.Hour),
	})

	// Prune expired sessions
	d.pruneExpiredSessions()

	// Verify expired session was removed
	if d.GetSession("expired-code") != nil {
		t.Errorf("expired session still exists in daemon")
	}
	if st.GetSession("expired-code") != nil {
		t.Errorf("expired session still exists in store")
	}

	// Verify active session still exists
	if d.GetSession("active-code") == nil {
		t.Errorf("active session was removed")
	}
	if st.GetSession("active-code") == nil {
		t.Errorf("active session was removed from store")
	}
}

// TestHasTURNServer tests TURN server detection.
func TestHasTURNServer(t *testing.T) {
	tests := []struct {
		name     string
		servers  []webrtc.ICEServer
		expected bool
	}{
		{
			name:     "empty servers",
			servers:  []webrtc.ICEServer{},
			expected: false,
		},
		{
			name:     "STUN only",
			servers:  []webrtc.ICEServer{{URLs: []string{"stun:stun.cloudflare.com:3478"}}},
			expected: false,
		},
		{
			name:     "TURN only",
			servers:  []webrtc.ICEServer{{URLs: []string{"turn:turn.example.com:3478"}}},
			expected: true,
		},
		{
			name:     "TURNs only",
			servers:  []webrtc.ICEServer{{URLs: []string{"turns:turn.example.com:5349"}}},
			expected: true,
		},
		{
			name: "STUN and TURN",
			servers: []webrtc.ICEServer{
				{URLs: []string{"stun:stun.cloudflare.com:3478"}},
				{URLs: []string{"turn:turn.example.com:3478"}},
			},
			expected: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := hasTURNServer(test.servers)
			if result != test.expected {
				t.Errorf("hasTURNServer() = %v, expected %v", result, test.expected)
			}
		})
	}
}

// TestHandleSignalingMessage tests message dispatch.
func TestHandleSignalingMessage(t *testing.T) {
	cfg := &config.Config{
		SignalingURL: "ws://localhost:8080",
		APIKey:       "test-api-key",
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	sigClient := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())
	sigClient.iceServers = []webrtc.ICEServer{
		{URLs: []string{"turn:turn.example.com:3478"}},
	}

	d, err := NewWithSignaling(cfgMgr, st, sigClient)
	if err != nil {
		t.Fatalf("NewWithSignaling() error: %v", err)
	}

	// Test welcome message sets hasTURN
	d.handleSignalingMessage(signaling.Message{Type: "welcome"})
	if !d.hasTURN {
		t.Errorf("hasTURN should be true after welcome with TURN server")
	}

	// Test error message
	d.handleSignalingMessage(signaling.Message{Type: "error", Err: "test error"})
	// Just verify it doesn't crash
}

// TestOnSessionCallbacks tests that callbacks are called.
func TestOnSessionCallbacks(t *testing.T) {
	cfg := &config.Config{
		SignalingURL: "ws://localhost:8080",
		APIKey:       "test-api-key",
		AllowedHost:  "opencloud.example.com",
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	sigClient := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())

	d, err := NewWithSignaling(cfgMgr, st, sigClient)
	if err != nil {
		t.Fatalf("NewWithSignaling() error: %v", err)
	}

	var addedSession *Session
	var removedCode string

	d.OnSessionAdded = func(session *Session) {
		addedSession = session
	}
	d.OnSessionRemoved = func(code string) {
		removedCode = code
	}

	ctx := context.Background()
	code, err := d.CreateSession(ctx, "https://opencloud.example.com/s/abc123", "", 24*time.Hour, 10, false)
	if err != nil {
		t.Fatalf("CreateSession() error: %v", err)
	}

	if addedSession == nil {
		t.Errorf("OnSessionAdded callback not called")
	}
	if addedSession.Code != code {
		t.Errorf("OnSessionAdded received wrong session")
	}

	err = d.RevokeSession(code)
	if err != nil {
		t.Fatalf("RevokeSession() error: %v", err)
	}

	if removedCode != code {
		t.Errorf("OnSessionRemoved callback not called or wrong code")
	}
}

// TestSessionDownloads tracks download count.
func TestSessionDownloads(t *testing.T) {
	sess := &Session{
		Code:         "test-code",
		Downloads:    0,
		MaxDownloads: 5,
		peers:        make(map[string]*peer.Peer),
	}

	// Increment downloads manually
	sess.mu.Lock()
	sess.Downloads = 1
	sess.mu.Unlock()

	if sess.Downloads != 1 {
		t.Errorf("Downloads count mismatch")
	}
}

// TestLoadSessionsFromStore tests loading persisted sessions.
func TestLoadSessionsFromStore(t *testing.T) {
	cfg := &config.Config{
		SignalingURL: "ws://localhost:8080",
		APIKey:       "test-api-key",
		AllowedHost:  "opencloud.example.com",
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()

	// Pre-populate store with a session
	st.SaveSession(store.SessionEntry{
		Code:         "persisted-code",
		ShareURL:     "https://opencloud.example.com/s/persisted",
		ExpiresAt:    time.Now().Add(24 * time.Hour),
		MaxDownloads: 10,
		Downloads:    3,
		RelayOnly:    false,
		CreatedAt:    time.Now().Add(-1 * time.Hour),
	})

	sigClient := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())
	sigClient.registerShare = func(ctx context.Context, shareURL, preferredCode string) (string, bool, error) {
		return preferredCode, true, nil
	}

	d, err := NewWithSignaling(cfgMgr, st, sigClient)
	if err != nil {
		t.Fatalf("NewWithSignaling() error: %v", err)
	}

	ctx := context.Background()
	d.loadSessionsFromStore(ctx)

	// Verify session was loaded
	session := d.GetSession("persisted-code")
	if session == nil {
		t.Fatalf("persisted session not loaded")
	}

	if session.Downloads != 3 {
		t.Errorf("Downloads mismatch: got %d, expected 3", session.Downloads)
	}
}

// TestLoadSessionsFromStoreFiltersExpired tests that expired sessions are not loaded.
func TestLoadSessionsFromStoreFiltersExpired(t *testing.T) {
	cfg := &config.Config{
		SignalingURL: "ws://localhost:8080",
		APIKey:       "test-api-key",
		AllowedHost:  "opencloud.example.com",
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()

	// Pre-populate store with an expired session
	st.SaveSession(store.SessionEntry{
		Code:      "expired-code",
		ShareURL:  "https://opencloud.example.com/s/expired",
		ExpiresAt: time.Now().Add(-1 * time.Hour), // Expired
	})

	sigClient := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())

	d, err := NewWithSignaling(cfgMgr, st, sigClient)
	if err != nil {
		t.Fatalf("NewWithSignaling() error: %v", err)
	}

	ctx := context.Background()
	d.loadSessionsFromStore(ctx)

	// Verify expired session was not loaded
	session := d.GetSession("expired-code")
	if session != nil {
		t.Errorf("expired session should not be loaded")
	}
}

// TestSetWebServer tests setting the web server.
func TestSetWebServer(t *testing.T) {
	cfg := &config.Config{
		SignalingURL: "ws://localhost:8080",
		APIKey:       "test-api-key",
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	sigClient := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())

	d, err := NewWithSignaling(cfgMgr, st, sigClient)
	if err != nil {
		t.Fatalf("NewWithSignaling() error: %v", err)
	}

	ws := &mockWebServer{}
	d.SetWebServer(ws)

	if d.webServer != ws {
		t.Errorf("web server not set correctly")
	}
}

// TestHasTURN tests HasTURN method.
func TestHasTURN(t *testing.T) {
	cfg := &config.Config{
		SignalingURL: "ws://localhost:8080",
		APIKey:       "test-api-key",
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	sigClient := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())

	d, err := NewWithSignaling(cfgMgr, st, sigClient)
	if err != nil {
		t.Fatalf("NewWithSignaling() error: %v", err)
	}

	// Initially false
	if d.HasTURN() {
		t.Errorf("HasTURN should be false initially")
	}

	// Set hasTURN manually
	d.hasTURN = true
	if !d.HasTURN() {
		t.Errorf("HasTURN should be true after setting")
	}
}