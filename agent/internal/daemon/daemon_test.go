package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sharebridge/agent/internal/config"
	"sharebridge/agent/internal/immich"
	"sharebridge/agent/internal/signaling"
	"sharebridge/agent/internal/store"
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
	mu          sync.Mutex
	agentID     string
	sessions    map[string]store.SessionEntry
	downloads   map[string]int
	saveError   error
	deleteError error
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
	if m.deleteError != nil {
		return m.deleteError
	}
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
	shareOrigin   func(code, shareURL string) string
	sendMessages  []map[string]any
	codeCounter   int
	registered    []signaling.RegisterShareOptions
	unregistered  []string
	unregisterErr error
}

func newMockSignalingClient(serverURL, apiKey, agentID string) *mockSignalingClient {
	return &mockSignalingClient{
		serverURL: serverURL,
		apiKey:    apiKey,
		agentID:   agentID,
	}
}

func (m *mockSignalingClient) Connect(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connected = true
	return nil
}

func (m *mockSignalingClient) RegisterShareWithOptions(ctx context.Context, opts signaling.RegisterShareOptions) (string, string, bool, error) {
	m.mu.Lock()
	m.registered = append(m.registered, opts)
	m.mu.Unlock()

	var code string
	var reconnected bool
	if opts.PreferredCode != "" {
		code, reconnected = opts.PreferredCode, true
	} else {
		m.codeCounter++
		code = fmt.Sprintf("test-code-%d", m.codeCounter)
	}
	origin := ""
	if m.shareOrigin != nil {
		origin = m.shareOrigin(code, opts.ShareURL)
	}
	return code, origin, reconnected, nil
}

func (m *mockSignalingClient) UnregisterShare(ctx context.Context, code string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unregistered = append(m.unregistered, code)
	return m.unregisterErr
}

func (m *mockSignalingClient) DownloadComplete(ctx context.Context, code string, bytesTransferred int64) error {
	return nil
}

func (m *mockSignalingClient) Send(ctx context.Context, msg any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if msgMap, ok := msg.(map[string]any); ok {
		m.sendMessages = append(m.sendMessages, msgMap)
	} else if msgMapStr, ok := msg.(map[string]string); ok {
		converted := make(map[string]any)
		for key, value := range msgMapStr {
			converted[key] = value
		}
		m.sendMessages = append(m.sendMessages, converted)
	}
	return nil
}

func (m *mockSignalingClient) SubmitCSR(ctx context.Context, csrPEM string) error {
	return m.Send(ctx, map[string]any{"type": "csr_submit", "csr_pem": csrPEM})
}

func (m *mockSignalingClient) OpenAck(ctx context.Context, ack signaling.OpenAck) error {
	msg := map[string]any{
		"type": "open_ack", "share_id": ack.ShareID, "nonce": ack.Nonce, "seq": ack.Seq,
		"granted_port": ack.GrantedPort, "public_ip": ack.PublicIP,
		"was_already_open": ack.WasAlreadyOpen, "status": ack.Status,
	}
	if ack.Error != "" {
		msg["error"] = ack.Error
	}
	return m.Send(ctx, msg)
}

func (m *mockSignalingClient) ReportEndpoint(ctx context.Context, ip string, port int, status string) error {
	msg := map[string]any{"type": "report_endpoint", "ip": ip, "port": port}
	if status != "" {
		msg["status"] = status
	}
	return m.Send(ctx, msg)
}

func (m *mockSignalingClient) TLSReady(ctx context.Context, fingerprint, notAfter string) error {
	return m.Send(ctx, map[string]any{"type": "tls_ready", "fingerprint": fingerprint, "not_after": notAfter})
}

func (m *mockSignalingClient) TLSError(ctx context.Context, reason string) error {
	return m.Send(ctx, map[string]any{"type": "tls_error", "reason": reason})
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

func (m *mockSignalingClient) sentContains(fragment string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, msg := range m.sendMessages {
		raw, err := json.Marshal(msg)
		if err == nil && strings.Contains(string(raw), fragment) {
			return true
		}
	}
	return false
}

func (m *mockSignalingClient) messagesSnapshot() []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]map[string]any, len(m.sendMessages))
	for i, message := range m.sendMessages {
		result[i] = make(map[string]any, len(message))
		for key, value := range message {
			result[i][key] = value
		}
	}
	return result
}

func (m *mockSignalingClient) hasSentMessage(msgType string, expectedFields map[string]any) bool {
	return hasSentMessage(m.messagesSnapshot(), msgType, expectedFields)
}

func (m *mockSignalingClient) registeredSnapshot() []signaling.RegisterShareOptions {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]signaling.RegisterShareOptions(nil), m.registered...)
}

func (m *mockSignalingClient) registeredCode(code string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, opts := range m.registered {
		if opts.PreferredCode == code {
			return true
		}
	}
	return false
}

func (m *mockSignalingClient) unregisteredCode(code string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, got := range m.unregistered {
		if got == code {
			return true
		}
	}
	return false
}

func newTestDaemon(t *testing.T) (*Daemon, *mockSignalingClient) {
	t.Helper()
	cfg := &config.Config{
		SignalingURL:     "ws://localhost:8080",
		APIKey:           "test-key",
		DefaultRelayOnly: false,
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	sig := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())

	d, err := NewWithSignaling(cfgMgr, st, sig)
	require.NoError(t, err)
	return d, sig
}

type fakeImmichPoller struct {
	shares []immich.SharedLink
	err    error
}

func (f *fakeImmichPoller) PollShares(ctx context.Context) ([]immich.SharedLink, error) {
	return f.shares, f.err
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

// TestCreateSession verifies that WebDAV/file share creation is rejected in
// Phase 3 (only direct gallery shares are supported).
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
	_, err = d.CreateSession(ctx, "https://opencloud.example.com/s/abc123", "opencloud", "", 24*time.Hour, 10, false)
	if err == nil {
		t.Fatalf("expected error for WebDAV share type")
	}
	var verr validationError
	if !errors.As(err, &verr) {
		t.Fatalf("expected a validation error, got %v", err)
	}
}

func TestCreateSessionManualImmichPlumbsMaxDownloadsAndExpiry(t *testing.T) {
	d, sig := newTestDaemon(t)
	d.config.ImmichURL = "http://immich.lan:2283"
	d.config.ImmichAllowedHost = "immich.lan:2283"
	d.config.ImmichAPIKey = "api"
	d.newImmichPoller = func() (immichPoller, error) {
		return &fakeImmichPoller{shares: []immich.SharedLink{
			{Key: "IMMICHMANUAL1", Type: "ALBUM"},
		}}, nil
	}

	code, err := d.CreateSession(context.Background(), "immich://IMMICHMANUAL1", "immich", "", 24*time.Hour, 10, false)

	require.NoError(t, err)
	require.Equal(t, "IMMICHMANUAL1", code)

	session := d.GetSession("IMMICHMANUAL1")
	require.NotNil(t, session)
	require.Equal(t, "immich://IMMICHMANUAL1", session.ShareURL)
	require.Equal(t, "immich", session.ShareType)
	require.False(t, session.RelayOnly)
	require.False(t, session.IsPasswordProtected)
	require.Equal(t, 10, session.MaxDownloads)
	require.False(t, session.ExpiresAt.IsZero())
	require.NotNil(t, session.immich)

	stored := d.store.GetSession("IMMICHMANUAL1")
	require.NotNil(t, stored)
	require.Equal(t, "immich://IMMICHMANUAL1", stored.ShareURL)
	require.Equal(t, "immich", stored.ShareType)
	require.False(t, stored.RelayOnly)
	require.False(t, stored.IsPasswordProtected)
	require.Equal(t, 10, stored.MaxDownloads)
	require.False(t, stored.ExpiresAt.IsZero())

	registered := sig.registeredSnapshot()
	require.Len(t, registered, 1)
	require.Equal(t, signaling.RegisterShareOptions{
		ShareURL:            "immich://IMMICHMANUAL1",
		PreferredCode:       "IMMICHMANUAL1",
		ShareType:           "immich",
		IsPasswordProtected: false,
		RelayOnly:           false,
	}, registered[0])
}

func TestCreateSessionManualImmichRejectsNonImmichURL(t *testing.T) {
	d, _ := newTestDaemon(t)

	_, err := d.CreateSession(context.Background(), "https://immich.lan/share/KEY", "immich", "", 24*time.Hour, 10, false)

	require.ErrorContains(t, err, "share_url must be immich://KEY for manual Immich shares")
}

func TestCreateSessionManualImmichReturnsExistingSessionWithoutReregistering(t *testing.T) {
	d, sig := newTestDaemon(t)
	existing := &Session{
		Code:      "IMMICHMANUAL1",
		ShareURL:  "immich://IMMICHMANUAL1",
		ShareType: "immich",
		RelayOnly: true,
		CreatedAt: time.Now().Add(-time.Hour),
	}
	d.sessions["IMMICHMANUAL1"] = existing
	d.newImmichPoller = func() (immichPoller, error) {
		t.Fatal("manual Immich creation should not poll when session already exists")
		return nil, nil
	}

	code, err := d.CreateSession(context.Background(), "immich://IMMICHMANUAL1", "immich", "", 24*time.Hour, 10, false)

	require.NoError(t, err)
	require.Equal(t, "IMMICHMANUAL1", code)
	require.Same(t, existing, d.GetSession("IMMICHMANUAL1"))
	require.Empty(t, sig.registeredSnapshot())
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
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	sigClient := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())

	d, err := NewWithSignaling(cfgMgr, st, sigClient)
	if err != nil {
		t.Fatalf("NewWithSignaling() error: %v", err)
	}

	code := "revoke-code"
	d.mu.Lock()
	d.sessions[code] = &Session{
		Code:      code,
		ShareType: "immich",
	}
	d.mu.Unlock()
	st.SaveSession(store.SessionEntry{Code: code, ShareType: "immich"})

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
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	sigClient := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())

	d, err := NewWithSignaling(cfgMgr, st, sigClient)
	if err != nil {
		t.Fatalf("NewWithSignaling() error: %v", err)
	}

	d.mu.Lock()
	d.sessions["code-1"] = &Session{Code: "code-1", ShareType: "immich"}
	d.sessions["code-2"] = &Session{Code: "code-2", ShareType: "immich"}
	d.mu.Unlock()

	sessions := d.ListSessions()
	if len(sessions) != 2 {
		t.Errorf("expected 2 sessions, got %d", len(sessions))
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
		Code:      "expired-code",
		ShareURL:  "https://opencloud.example.com/s/expired",
		ExpiresAt: time.Now().Add(-1 * time.Hour), // Expired 1 hour ago
		CreatedAt: time.Now().Add(-2 * time.Hour),
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

// TestOnSessionCallbacks tests that callbacks are called.
func TestOnSessionCallbacks(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.config.ImmichURL = "http://immich.lan:2283"
	d.config.ImmichAllowedHost = "immich.lan:2283"
	d.config.ImmichAPIKey = "api"
	d.newImmichPoller = func() (immichPoller, error) {
		return &fakeImmichPoller{shares: []immich.SharedLink{{Key: "IMMICHCB1", Type: "ALBUM"}}}, nil
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
	code, err := d.CreateSession(ctx, "immich://IMMICHCB1", "immich", "", 24*time.Hour, 10, false)
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
		SignalingURL:      "ws://localhost:8080",
		APIKey:            "test-api-key",
		ImmichURL:         "http://immich.lan:2283",
		ImmichAllowedHost: "immich.lan:2283",
		ImmichAPIKey:      "api",
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()

	// Pre-populate store with a session
	st.SaveSession(store.SessionEntry{
		Code:         "persisted-code",
		ShareURL:     "immich://persisted-code",
		ShareType:    "immich",
		ExpiresAt:    time.Now().Add(24 * time.Hour),
		MaxDownloads: 10,
		Downloads:    3,
		RelayOnly:    false,
		CreatedAt:    time.Now().Add(-1 * time.Hour),
	})

	sigClient := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())

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
		ShareType: "opencloud",
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

// TestLoadSessionsFromStoreRejectsWebDAV verifies that a persisted WebDAV/file
// session is removed (and its control registration unregistered) at restore.
func TestLoadSessionsFromStoreRejectsWebDAV(t *testing.T) {
	cfg := &config.Config{
		SignalingURL: "ws://localhost:8080",
		APIKey:       "test-api-key",
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()

	st.SaveSession(store.SessionEntry{
		Code:      "file-code",
		ShareURL:  "https://opencloud.example.com/s/abc123",
		ShareType: "opencloud",
		FileID:    "storage-1$foo!bar",
		ExpiresAt: time.Now().Add(24 * time.Hour),
		CreatedAt: time.Now().Add(-1 * time.Hour),
	})

	sigClient := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())

	d, err := NewWithSignaling(cfgMgr, st, sigClient)
	if err != nil {
		t.Fatalf("NewWithSignaling() error: %v", err)
	}

	d.loadSessionsFromStore(context.Background())

	if d.GetSession("file-code") != nil {
		t.Fatal("WebDAV session should not be restored")
	}
	if st.GetSession("file-code") != nil {
		t.Fatal("WebDAV session should be deleted from the store")
	}
	if !sigClient.hasSentMessage("deregister", map[string]any{"code": "file-code", "reason": "unsupported"}) {
		t.Fatal("WebDAV session should be deregistered as unsupported (410), not revoked (404)")
	}
	if sigClient.unregisteredCode("file-code") {
		t.Fatal("WebDAV session must not be unregistered via unregister_share (revoked/404)")
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

	d, err := NewWithSignaling(cfgMgr, st, sigClient)
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

// hasSentMessage checks if a message was sent with the given type and fields.
func hasSentMessage(messages []map[string]any, msgType string, expectedFields map[string]any) bool {
	for _, msg := range messages {
		if msg["type"] == msgType {
			match := true
			for key, expected := range expectedFields {
				if msg[key] != expected {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
	}
	return false
}

func TestSyncImmichSharesRegistersNewAndUnregistersRemoved(t *testing.T) {
	d, sig := newTestDaemon(t)
	d.config.ImmichURL = "http://immich.lan:2283"
	d.config.ImmichAllowedHost = "immich.lan:2283"
	d.config.ImmichAPIKey = "api"
	d.newImmichPoller = func() (immichPoller, error) {
		return &fakeImmichPoller{shares: []immich.SharedLink{
			{Key: "IMMICHNEW1", Type: "ALBUM"},
		}}, nil
	}
	d.sessions["IMMICHOLD1"] = &Session{Code: "IMMICHOLD1", ShareType: "immich", RelayOnly: true}

	require.NoError(t, d.syncImmichShares(context.Background()))

	require.True(t, sig.registeredCode("IMMICHNEW1"))
	require.True(t, sig.unregisteredCode("IMMICHOLD1"))
}

func TestSyncImmichSharesIgnoresIndividualSharedLinks(t *testing.T) {
	d, sig := newTestDaemon(t)
	d.config.ImmichURL = "http://immich.lan:2283"
	d.config.ImmichAllowedHost = "immich.lan:2283"
	d.config.ImmichAPIKey = "api"
	d.newImmichPoller = func() (immichPoller, error) {
		return &fakeImmichPoller{shares: []immich.SharedLink{
			{Key: "IMMICHINDIVIDUAL1", Type: "INDIVIDUAL"},
			{Key: "IMMICHALBUM1", Type: "ALBUM"},
		}}, nil
	}

	require.NoError(t, d.syncImmichShares(context.Background()))

	require.False(t, sig.registeredCode("IMMICHINDIVIDUAL1"))
	require.Nil(t, d.GetSession("IMMICHINDIVIDUAL1"))
	require.True(t, sig.registeredCode("IMMICHALBUM1"))
	require.NotNil(t, d.GetSession("IMMICHALBUM1"))
}

func TestSyncImmichSharesUnregisterFailureLeavesRemovedSessionInMemory(t *testing.T) {
	d, sig := newTestDaemon(t)
	d.config.ImmichURL = "http://immich.lan:2283"
	d.config.ImmichAllowedHost = "immich.lan:2283"
	d.config.ImmichAPIKey = "api"
	d.newImmichPoller = func() (immichPoller, error) {
		return &fakeImmichPoller{}, nil
	}
	sig.unregisterErr = errors.New("unregister failed")
	d.sessions["IMMICHOLD1"] = &Session{
		Code: "IMMICHOLD1", ShareType: "immich", RelayOnly: true,
	}

	err := d.syncImmichShares(context.Background())

	require.ErrorContains(t, err, "unregister failed")
	require.NotNil(t, d.GetSession("IMMICHOLD1"))
	require.True(t, sig.unregisteredCode("IMMICHOLD1"))
}

func TestSyncImmichSharesStoreDeleteFailureLeavesRemovedSessionInMemory(t *testing.T) {
	d, sig := newTestDaemon(t)
	d.config.ImmichURL = "http://immich.lan:2283"
	d.config.ImmichAllowedHost = "immich.lan:2283"
	d.config.ImmichAPIKey = "api"
	d.newImmichPoller = func() (immichPoller, error) {
		return &fakeImmichPoller{}, nil
	}
	d.store.(*mockStore).deleteError = errors.New("delete failed")
	d.sessions["IMMICHOLD1"] = &Session{
		Code: "IMMICHOLD1", ShareType: "immich", RelayOnly: true,
	}

	err := d.syncImmichShares(context.Background())

	require.ErrorContains(t, err, "delete failed")
	require.NotNil(t, d.GetSession("IMMICHOLD1"))
	require.True(t, sig.unregisteredCode("IMMICHOLD1"))
}

func TestSyncImmichSharesRollsBackSignalingRegistrationOnStoreSaveFailure(t *testing.T) {
	d, sig := newTestDaemon(t)
	d.config.ImmichURL = "http://immich.lan:2283"
	d.config.ImmichAllowedHost = "immich.lan:2283"
	d.config.ImmichAPIKey = "api"
	d.newImmichPoller = func() (immichPoller, error) {
		return &fakeImmichPoller{shares: []immich.SharedLink{{Key: "IMMICHROLLBACK1", Type: "ALBUM"}}}, nil
	}
	d.store.(*mockStore).saveError = errors.New("save failed")

	err := d.syncImmichShares(context.Background())

	require.ErrorContains(t, err, "save failed")
	require.True(t, sig.registeredCode("IMMICHROLLBACK1"))
	require.True(t, sig.unregisteredCode("IMMICHROLLBACK1"))
	require.Nil(t, d.GetSession("IMMICHROLLBACK1"))
}

func TestSyncImmichSharesWiresClientForNewSession(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.config.ImmichURL = "http://immich.lan:2283"
	d.config.ImmichAllowedHost = "immich.lan:2283"
	d.config.ImmichAPIKey = "api"
	d.newImmichPoller = func() (immichPoller, error) {
		return &fakeImmichPoller{shares: []immich.SharedLink{
			{Key: "IMMICHCLIENT1", Type: "ALBUM"},
		}}, nil
	}

	require.NoError(t, d.syncImmichShares(context.Background()))

	session := d.GetSession("IMMICHCLIENT1")
	require.NotNil(t, session)
	require.NotNil(t, session.immich)
	require.False(t, session.IsPasswordProtected)

	stored := d.store.GetSession("IMMICHCLIENT1")
	require.NotNil(t, stored)
	require.False(t, stored.IsPasswordProtected)
}

// TestRegisterImmichShareWaitsForDirectReady covers the I1-gap for Immich
// polling: a direct (relay_only=false) Immich share must wait for the current
// epoch's enrollment_ready before registering. Phase 3 rejects relay-only
// registrations outright.
func TestRegisterImmichShareWaitsForDirectReady(t *testing.T) {
	setup := func(t *testing.T, relayOnly bool) (*Daemon, *mockSignalingClient) {
		t.Helper()
		d, sig := newTestDaemon(t)
		d.config.ImmichURL = "http://immich.lan:2283"
		d.config.ImmichAllowedHost = "immich.lan:2283"
		d.config.ImmichAPIKey = "api"
		d.config.DefaultRelayOnly = relayOnly
		// Give the daemon a real (not-ready) direct state so waitForDirectReady
		// actually gates direct registrations.
		d.direct = &directState{}
		return d, sig
	}

	link := immich.SharedLink{Key: "IMMICHDIRECT1", Type: "ALBUM"}

	t.Run("direct waits for readiness", func(t *testing.T) {
		d, sig := setup(t, false)

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		done := make(chan error, 1)
		go func() {
			_, err := d.registerImmichShare(ctx, link, 0, time.Time{})
			done <- err
		}()

		// Not ready: registration must not have happened yet.
		select {
		case err := <-done:
			t.Fatalf("registerImmichShare returned before enrollment_ready: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		require.False(t, sig.registeredCode("IMMICHDIRECT1"))

		d.handleEnrollmentReady(signaling.Message{})

		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(2 * time.Second):
			t.Fatal("registerImmichShare did not unblock after enrollment_ready")
		}
		require.True(t, sig.registeredCode("IMMICHDIRECT1"))
	})

	t.Run("relay-only rejected", func(t *testing.T) {
		d, sig := setup(t, true)

		_, err := d.registerImmichShare(context.Background(), link, 0, time.Time{})
		require.ErrorContains(t, err, "relay-only")
		require.False(t, sig.registeredCode("IMMICHDIRECT1"))
	})
}

func TestLoadSessionsFromStoreWiresPersistedImmichSession(t *testing.T) {
	cfg := &config.Config{
		SignalingURL:      "ws://localhost:8080",
		APIKey:            "test-key",
		ImmichURL:         "http://immich.lan:2283",
		ImmichAllowedHost: "immich.lan:2283",
		ImmichAPIKey:      "api",
		DefaultRelayOnly:  false,
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	require.NoError(t, st.SaveSession(store.SessionEntry{
		Code:                "IMMICHPERSIST1",
		ShareURL:            "immich://IMMICHPERSIST1",
		ShareType:           "immich",
		IsPasswordProtected: false,
		RelayOnly:           false,
		CreatedAt:           time.Now().Add(-time.Hour),
	}))
	sig := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())
	d, err := NewWithSignaling(cfgMgr, st, sig)
	require.NoError(t, err)

	d.loadSessionsFromStore(context.Background())

	session := d.GetSession("IMMICHPERSIST1")
	require.NotNil(t, session)
	require.Equal(t, "immich", session.ShareType)
	require.False(t, session.RelayOnly)
	require.False(t, session.IsPasswordProtected)
	require.NotNil(t, session.immich)
	require.True(t, sig.registeredCode("IMMICHPERSIST1"))
}
