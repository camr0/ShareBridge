package daemon

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"sharebridge/agent/internal/config"
	"sharebridge/agent/internal/signaling"
	"sharebridge/agent/internal/store"
	"sharebridge/agent/internal/transport"
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
	mu        sync.Mutex
	agentID   string
	sessions  map[string]store.SessionEntry
	downloads map[string]int
	saveError error
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

// mockTransport implements TransportInterface for testing.
type mockTransport struct {
	peerID      string
	onStream    transport.StreamHandler
	dialRelay   func(ctx context.Context, relayMultiaddr string) error
	dialRelayed bool
	dialedAddr  string
	ready       bool
	mu          sync.Mutex
}

func newMockTransport() *mockTransport {
	return &mockTransport{
		peerID: "12D3KooTestPeerID",
	}
}

func (m *mockTransport) DialRelay(ctx context.Context, relayMultiaddr string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dialedAddr = relayMultiaddr
	if m.dialRelay != nil {
		return m.dialRelay(ctx, relayMultiaddr)
	}
	m.dialRelayed = true
	return nil
}

func (m *mockTransport) OnStream(h transport.StreamHandler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onStream = h
}

func (m *mockTransport) PeerID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.peerID
}

func (m *mockTransport) Close() error {
	return nil
}

func (m *mockTransport) Ready() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ready
}

func (m *mockTransport) getOnStream() transport.StreamHandler {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.onStream
}

// mockSignalingClient implements SignalingClientInterface for testing.
type mockSignalingClient struct {
	mu             sync.Mutex
	serverURL      string
	apiKey         string
	agentID        string
	connected      bool
	onMessage      func(signaling.Message)
	relayMultiaddr string
	registerShare  func(ctx context.Context, shareURL, preferredCode string, relayOnly bool) (string, bool, error)
	sendMessages   []map[string]any
	codeCounter    int // Counter for generating unique codes
}

func newMockSignalingClient(serverURL, apiKey, agentID string) *mockSignalingClient {
	return &mockSignalingClient{
		serverURL:      serverURL,
		apiKey:         apiKey,
		agentID:        agentID,
		relayMultiaddr: "/dns4/relay.example.com/tcp/443/wss/p2p/12D3KooTest",
	}
}

func (m *mockSignalingClient) Connect(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connected = true
	return nil
}

func (m *mockSignalingClient) RegisterShare(ctx context.Context, shareURL, preferredCode string, relayOnly bool) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.registerShare != nil {
		return m.registerShare(ctx, shareURL, preferredCode, relayOnly)
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

func (m *mockSignalingClient) GetRelayMultiaddr() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.relayMultiaddr
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

func (m *mockSignalingClient) SetPeerID(_ string) {}

func (m *mockSignalingClient) sendMessage(msg signaling.Message) {
	m.mu.Lock()
	handler := m.onMessage
	m.mu.Unlock()
	if handler != nil {
		handler(msg)
	}
}

func (m *mockSignalingClient) sentType(msgType string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, msg := range m.sendMessages {
		if msg["type"] == msgType {
			return true
		}
	}
	return false
}

func (m *mockSignalingClient) lastSent() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sendMessages) == 0 {
		return nil
	}
	return m.sendMessages[len(m.sendMessages)-1]
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

// Test helpers for creating daemon and seeding data.
func newTestDaemon(t *testing.T, sig *mockSignalingClient, tr *mockTransport) *Daemon {
	t.Helper()
	cfg := &config.Config{
		SignalingURL: "ws://localhost:8080",
		APIKey:       "test-api-key",
		AllowedHost:  "opencloud.example.com",
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	d, err := NewWithSignaling(cfgMgr, st, sig, tr)
	if err != nil {
		t.Fatalf("NewWithSignaling() error: %v", err)
	}
	return d
}

func seedSession(t *testing.T, d *Daemon, password string) string {
	t.Helper()
	ctx := context.Background()
	code, err := d.CreateSession(ctx, "https://opencloud.example.com/s/abc123", "opencloud", password, 24*time.Hour, 10, false)
	if err != nil {
		t.Fatalf("CreateSession() error: %v", err)
	}
	return code
}

func seedNonce(t *testing.T, d *Daemon, connID string) string {
	t.Helper()
	// Manually inject a nonce for testing
	nonceBytes := make([]byte, 32)
	for i := range nonceBytes {
		nonceBytes[i] = byte(i)
	}
	nonce := hex.EncodeToString(nonceBytes)
	d.noncesMu.Lock()
	d.nonces[connID] = nonceEntry{nonce: nonce, expiresAt: time.Now().Add(60 * time.Second)}
	d.noncesMu.Unlock()
	return nonce
}

func computeHMAC(t *testing.T, password, nonce string) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(password))
	mac.Write([]byte(nonce))
	return hex.EncodeToString(mac.Sum(nil))
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	timeout := time.After(2 * time.Second)
	for !condition() {
		select {
		case <-timeout:
			t.Fatalf("timeout waiting for condition")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// pipeStream wraps a net.Pipe end to satisfy network.Stream interface for tests.
type pipeStream struct {
	io.ReadWriteCloser
}

func (p *pipeStream) Reset() error { return p.Close() }
func (p *pipeStream) Conn() interface {
	RemotePeer() string
} {
	return &pipeConn{remotePeer: "12D3KooTest"}
}

type pipeConn struct {
	remotePeer string
}

func (c *pipeConn) RemotePeer() string { return c.remotePeer }

// TestNew tests daemon creation.
func TestNew(t *testing.T) {
	cfg := &config.Config{
		SignalingURL: "ws://localhost:8080",
		APIKey:       "test-api-key",
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	tr := newMockTransport()

	d, err := New(cfgMgr, st, tr)
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
	tr := newMockTransport()

	d, err := NewWithSignaling(cfgMgr, st, sigClient, tr)
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
	tr := newMockTransport()

	d, err := NewWithSignaling(cfgMgr, st, sigClient, tr)
	if err != nil {
		t.Fatalf("NewWithSignaling() error: %v", err)
	}

	ctx := context.Background()
	code, err := d.CreateSession(ctx, "https://opencloud.example.com/s/abc123", "opencloud", "", 24*time.Hour, 10, false)
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
	tr := newMockTransport()

	d, err := NewWithSignaling(cfgMgr, st, sigClient, tr)
	if err != nil {
		t.Fatalf("NewWithSignaling() error: %v", err)
	}

	ctx := context.Background()
	_, err = d.CreateSession(ctx, "https://evil.example.com/s/abc123", "opencloud", "", 24*time.Hour, 10, false)
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
	tr := newMockTransport()

	d, err := NewWithSignaling(cfgMgr, st, sigClient, tr)
	if err != nil {
		t.Fatalf("NewWithSignaling() error: %v", err)
	}

	ctx := context.Background()
	code, err := d.CreateSession(ctx, "https://opencloud.example.com/s/abc123", "opencloud", "", 24*time.Hour, 10, false)
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
	tr := newMockTransport()

	d, err := NewWithSignaling(cfgMgr, st, sigClient, tr)
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
	tr := newMockTransport()

	d, err := NewWithSignaling(cfgMgr, st, sigClient, tr)
	if err != nil {
		t.Fatalf("NewWithSignaling() error: %v", err)
	}

	ctx := context.Background()
	code1, err := d.CreateSession(ctx, "https://opencloud.example.com/s/abc123", "opencloud", "", 24*time.Hour, 10, false)
	if err != nil {
		t.Fatalf("CreateSession() error: %v", err)
	}
	code2, err := d.CreateSession(ctx, "https://opencloud.example.com/s/def456", "opencloud", "", 24*time.Hour, 5, true)
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
	tr := newMockTransport()

	d, err := NewWithSignaling(cfgMgr, st, sigClient, tr)
	if err != nil {
		t.Fatalf("NewWithSignaling() error: %v", err)
	}

	// Create an expired session manually
	expiredSession := &Session{
		Code:      "expired-code",
		ShareURL:  "https://opencloud.example.com/s/expired",
		ExpiresAt: time.Now().Add(-1 * time.Hour), // Expired 1 hour ago
		CreatedAt: time.Now().Add(-2 * time.Hour),
		streams:   make(map[string]*transport.StreamAdapter),
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
		Code:      "active-code",
		ShareURL:  "https://opencloud.example.com/s/active",
		ExpiresAt: time.Now().Add(24 * time.Hour), // Expires in 24 hours
		CreatedAt: time.Now(),
		streams:   make(map[string]*transport.StreamAdapter),
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

// TestHandleSignalingMessage tests message dispatch.
func TestHandleSignalingMessage(t *testing.T) {
	cfg := &config.Config{
		SignalingURL: "ws://localhost:8080",
		APIKey:       "test-api-key",
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	sigClient := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())
	tr := newMockTransport()

	d, err := NewWithSignaling(cfgMgr, st, sigClient, tr)
	if err != nil {
		t.Fatalf("NewWithSignaling() error: %v", err)
	}

	// Test welcome message sets signalingConnected
	d.handleSignalingMessage(signaling.Message{Type: "welcome", RelayMultiaddr: "/dns4/relay.example.com/tcp/443/wss/p2p/12D3KooTest"})
	if !d.signalingConnected {
		t.Errorf("signalingConnected should be true after welcome")
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
	tr := newMockTransport()

	d, err := NewWithSignaling(cfgMgr, st, sigClient, tr)
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
	code, err := d.CreateSession(ctx, "https://opencloud.example.com/s/abc123", "opencloud", "", 24*time.Hour, 10, false)
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
		streams:      make(map[string]*transport.StreamAdapter),
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
		ShareType:    "opencloud",
		ExpiresAt:    time.Now().Add(24 * time.Hour),
		MaxDownloads: 10,
		Downloads:    3,
		RelayOnly:    false,
		CreatedAt:    time.Now().Add(-1 * time.Hour),
	})

	sigClient := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())
	sigClient.registerShare = func(ctx context.Context, shareURL, preferredCode string, relayOnly bool) (string, bool, error) {
		return preferredCode, true, nil
	}

	tr := newMockTransport()

	d, err := NewWithSignaling(cfgMgr, st, sigClient, tr)
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
		ShareType: "opencloud",
		ExpiresAt: time.Now().Add(-1 * time.Hour), // Expired
	})

	sigClient := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())
	tr := newMockTransport()

	d, err := NewWithSignaling(cfgMgr, st, sigClient, tr)
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

// TestLoadSessionsFromStore_PreservesFileID tests that FileID is loaded from store.
func TestLoadSessionsFromStore_PreservesFileID(t *testing.T) {
	cfg := &config.Config{
		SignalingURL: "ws://localhost:8080",
		APIKey:       "test-api-key",
		AllowedHost:  "opencloud.example.com",
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()

	st.SaveSession(store.SessionEntry{
		Code:         "file-code",
		ShareURL:     "https://opencloud.example.com/s/abc123",
		ShareType:    "opencloud",
		FileID:       "storage-1$foo!bar",
		ExpiresAt:    time.Now().Add(24 * time.Hour),
		MaxDownloads: 10,
		CreatedAt:    time.Now().Add(-1 * time.Hour),
	})

	sigClient := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())
	sigClient.registerShare = func(ctx context.Context, shareURL, preferredCode string, relayOnly bool) (string, bool, error) {
		return preferredCode, true, nil
	}

	tr := newMockTransport()

	d, err := NewWithSignaling(cfgMgr, st, sigClient, tr)
	if err != nil {
		t.Fatalf("NewWithSignaling() error: %v", err)
	}

	d.loadSessionsFromStore(context.Background())

	session := d.GetSession("file-code")
	if session == nil {
		t.Fatal("session not found after loadSessionsFromStore")
	}
	if session.FileID != "storage-1$foo!bar" {
		t.Errorf("FileID = %q, want storage-1$foo!bar", session.FileID)
	}
}

// TestLoadSessionsFromStore_LegacyMissingShareType skips legacy sessions that
// were persisted before ShareType existed.
func TestLoadSessionsFromStore_LegacyMissingShareType(t *testing.T) {
	cfg := &config.Config{
		SignalingURL: "ws://localhost:8080",
		APIKey:       "test-api-key",
		AllowedHost:  "opencloud.example.com",
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()

	st.SaveSession(store.SessionEntry{
		Code:      "legacy-code",
		ShareURL:  "https://opencloud.example.com/s/legacy",
		ExpiresAt: time.Now().Add(24 * time.Hour),
		CreatedAt: time.Now().Add(-1 * time.Hour),
	})

	sigClient := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())
	tr := newMockTransport()

	d, err := NewWithSignaling(cfgMgr, st, sigClient, tr)
	if err != nil {
		t.Fatalf("NewWithSignaling() error: %v", err)
	}

	var logBuf bytes.Buffer
	originalOutput := log.Writer()
	originalFlags := log.Flags()
	log.SetOutput(&logBuf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(originalOutput)
		log.SetFlags(originalFlags)
	})

	d.loadSessionsFromStore(context.Background())

	if session := d.GetSession("legacy-code"); session != nil {
		t.Fatalf("legacy session should have been skipped, got %+v", session)
	}

	if !strings.Contains(logBuf.String(), "warning") || !strings.Contains(logBuf.String(), "legacy-code") {
		t.Fatalf("expected warning about legacy session, got logs: %s", logBuf.String())
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
	tr := newMockTransport()

	d, err := NewWithSignaling(cfgMgr, st, sigClient, tr)
	if err != nil {
		t.Fatalf("NewWithSignaling() error: %v", err)
	}

	ws := &mockWebServer{}
	d.SetWebServer(ws)

	if d.webServer != ws {
		t.Errorf("web server not set correctly")
	}
}

// TestHandleJoin_HMACSuccessSendsAuthOK tests that a valid HMAC triggers auth_ok.
func TestHandleJoin_HMACSuccessSendsAuthOK(t *testing.T) {
	sig := newMockSignalingClient("ws://localhost:8080", "test-key", "test-agent")
	tr := newMockTransport()
	d := newTestDaemon(t, sig, tr)
	code := seedSession(t, d, "testpassword")
	nonce := seedNonce(t, d, "conn-1")
	hmacValue := computeHMAC(t, "testpassword", nonce)

	d.handleJoin("conn-1", code, hmacValue)

	// wait briefly for the goroutine to enqueue the send
	waitFor(t, func() bool { return sig.sentType("auth_ok") })

	last := sig.lastSent()
	if last["type"] != "auth_ok" || last["conn_id"] != "conn-1" || last["code"] != code {
		t.Fatalf("auth_ok not sent correctly, got %+v", last)
	}
}

// TestHandleJoin_InvalidHMACSendsAuthFailed tests that an invalid HMAC triggers auth_failed.
func TestHandleJoin_InvalidHMACSendsAuthFailed(t *testing.T) {
	sig := newMockSignalingClient("ws://localhost:8080", "test-key", "test-agent")
	tr := newMockTransport()
	d := newTestDaemon(t, sig, tr)
	code := seedSession(t, d, "testpassword")
	seedNonce(t, d, "conn-1")

	d.handleJoin("conn-1", code, "invalid-hmac-value")

	waitFor(t, func() bool { return sig.sentType("auth_failed") })

	last := sig.lastSent()
	if last["type"] != "auth_failed" || last["conn_id"] != "conn-1" {
		t.Fatalf("auth_failed not sent correctly, got %+v", last)
	}
}

// TestHandleKnock_SendsNonce tests that knock handler sends nonce.
func TestHandleKnock_SendsNonce(t *testing.T) {
	sig := newMockSignalingClient("ws://localhost:8080", "test-key", "test-agent")
	tr := newMockTransport()
	d := newTestDaemon(t, sig, tr)
	code := seedSession(t, d, "")

	d.handleKnock("conn-1", code)

	waitFor(t, func() bool { return sig.sentType("nonce") })

	last := sig.lastSent()
	if last["type"] != "nonce" || last["conn_id"] != "conn-1" || last["value"] == "" {
		t.Fatalf("nonce not sent correctly, got %+v", last)
	}
}

// TestWelcome_DialsRelay tests that welcome message triggers relay dial.
func TestWelcome_DialsRelay(t *testing.T) {
	sig := newMockSignalingClient("ws://localhost:8080", "test-key", "test-agent")
	sig.relayMultiaddr = "/ip4/127.0.0.1/tcp/4001/p2p/12D3KooRelayTest"
	tr := newMockTransport()
	d := newTestDaemon(t, sig, tr)

	d.handleSignalingMessage(signaling.Message{Type: "welcome"})

	// Brief wait for goroutine
	time.Sleep(50 * time.Millisecond)

	if tr.dialedAddr != sig.relayMultiaddr {
		t.Errorf("expected relay dial to %s, got %s", sig.relayMultiaddr, tr.dialedAddr)
	}
}

func TestIsConnected_RequiresRelayReady(t *testing.T) {
	sig := newMockSignalingClient("ws://localhost:8080", "test-key", "test-agent")
	sig.relayMultiaddr = "/ip4/127.0.0.1/tcp/4001/p2p/12D3KooRelayTest"
	tr := newMockTransport()
	d := newTestDaemon(t, sig, tr)

	d.handleSignalingMessage(signaling.Message{Type: "welcome"})
	time.Sleep(50 * time.Millisecond)

	if d.IsConnected() {
		t.Fatal("IsConnected should stay false until the relay reservation is ready")
	}

	tr.mu.Lock()
	tr.ready = true
	tr.mu.Unlock()

	if !d.IsConnected() {
		t.Fatal("IsConnected should be true once signaling and relay are both ready")
	}
}

// TestOnStreamHandlerWired tests that daemon properly registers stream handler on transport.
func TestOnStreamHandlerWired(t *testing.T) {
	sig := newMockSignalingClient("ws://localhost:8080", "test-key", "test-agent")
	tr := newMockTransport()
	_ = newTestDaemon(t, sig, tr)

	// Verify that the stream handler was registered
	handler := tr.getOnStream()
	if handler == nil {
		t.Fatal("OnStream handler should be registered during daemon creation")
	}
}

// TestHandleIncomingStream_ValidSessionAccepts tests that handleIncomingStream accepts streams for valid sessions.
func TestHandleIncomingStream_ValidSessionAccepts(t *testing.T) {
	sig := newMockSignalingClient("ws://localhost:8080", "test-key", "test-agent")
	tr := newMockTransport()
	d := newTestDaemon(t, sig, tr)
	code := seedSession(t, d, "")

	// Verify session exists and has empty streams map initially
	session := d.GetSession(code)
	if session == nil {
		t.Fatalf("session should exist")
	}

	session.mu.Lock()
	initialStreamCount := len(session.streams)
	session.mu.Unlock()

	if initialStreamCount != 0 {
		t.Errorf("expected 0 streams initially, got %d", initialStreamCount)
	}
}
