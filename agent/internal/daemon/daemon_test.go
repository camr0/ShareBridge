package daemon

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sharebridge/agent/internal/config"
	"sharebridge/agent/internal/direct"
	"sharebridge/agent/internal/immich"
	"sharebridge/agent/internal/signaling"
	"sharebridge/agent/internal/store"
	"sharebridge/agent/internal/tunnel"
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

// SendSTUNResult records one §11.1 stun_result echo through the shared
// message log (same shape the real signaling client puts on the wire), so the
// daemon's STUN challenge handler can be observed end to end. Defining it on
// the mock (test helper) lets the stunResultSender capability assertion find
// the sink in tests.
func (m *mockSignalingClient) SendSTUNResult(ctx context.Context, result signaling.STUNResult) error {
	return m.Send(ctx, map[string]any{
		"type":           "stun_result",
		"challenge":      result.Challenge,
		"transaction_id": result.TransactionID,
		"receipt":        result.Receipt,
	})
}

// SendRelayCredentialRequest records one §11.1 relay_credential_request send
// (test helper): the daemon's production credential requester is wired to the
// signaling client's sender, so the amendment test observes the fresh-
// credential request the tunnel manager issues after a replay rejection.
func (m *mockSignalingClient) SendRelayCredentialRequest(ctx context.Context, reason tunnel.CredentialRequestReason) error {
	return m.Send(ctx, map[string]any{
		"type":   "relay_credential_request",
		"reason": string(reason),
	})
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
// polling: every share — direct AND relay-only (§13.3: relay-only sessions
// wait for BASELINE readiness, i.e. enrollment_ready, never direct DDNS
// availability) — must wait for the current epoch's enrollment_ready before
// registering.
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

	t.Run("relay-only waits for baseline readiness too", func(t *testing.T) {
		d, sig := setup(t, true)

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		done := make(chan error, 1)
		go func() {
			_, err := d.registerImmichShare(ctx, link, 0, time.Time{})
			done <- err
		}()

		// Not ready: a relay-only registration must also wait — the same
		// listener serves both routes, so baseline readiness is required.
		select {
		case err := <-done:
			t.Fatalf("relay-only registerImmichShare returned before enrollment_ready: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		require.False(t, sig.registeredCode("IMMICHDIRECT1"))

		d.handleEnrollmentReady(signaling.Message{})

		select {
		case err := <-done:
			require.NoError(t, err, "relay-only registration must be accepted after baseline readiness")
		case <-time.After(2 * time.Second):
			t.Fatal("relay-only registerImmichShare did not unblock after enrollment_ready")
		}
		require.True(t, sig.registeredCode("IMMICHDIRECT1"))
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

// ---------------------------------------------------------------------------
// Task 30 — §13.4 reversible lockdown v2
// ---------------------------------------------------------------------------

// SendLockdownStatus records one §11.1 lockdown_status send through the shared
// message log so the daemon's advisory lockdown report is observable.
func (m *mockSignalingClient) SendLockdownStatus(ctx context.Context, status signaling.LockdownStatus) error {
	return m.Send(ctx, map[string]any{
		"type":       "lockdown_status",
		"generation": status.Generation,
		"locked":     status.Locked,
	})
}

// recordingDirectMapper counts mapping deletions so the lockdown test can
// assert the UPnP/NAT-PMP mapping was actually removed.
type recordingDirectMapper struct {
	mu       sync.Mutex
	ip       string
	deletes  int
	mappings map[int]direct.PortMapping
}

func (m *recordingDirectMapper) AddPortMapping(ext, internal int, desc string, lease int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mappings == nil {
		m.mappings = map[int]direct.PortMapping{}
	}
	// Mirror what DeleteOwnedMapping's ownership check compares: the exact
	// description, internal port/client, and protocol of this mapping.
	m.mappings[ext] = direct.PortMapping{
		ExternalPort: ext, InternalPort: internal, InternalClient: m.InternalIP(),
		Protocol: "TCP", Description: desc,
	}
	return ext, nil
}

func (m *recordingDirectMapper) DeletePortMapping(ext int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletes++
	delete(m.mappings, ext)
	return nil
}

func (m *recordingDirectMapper) ExternalIP() (string, error) { return m.ip, nil }
func (m *recordingDirectMapper) InternalIP() string          { return "192.168.1.20" }

func (m *recordingDirectMapper) ListPortMappings() ([]direct.PortMapping, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]direct.PortMapping, 0, len(m.mappings))
	for _, mapping := range m.mappings {
		out = append(out, mapping)
	}
	return out, nil
}

func (m *recordingDirectMapper) deleteCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deletes
}

// lockdownFixture is the full §13.4 surface: a real Binder+DirectServer
// sharing one binder, an OnDemandPort over a recording mapper, a running
// tunnel manager on a fake process starter, and one bound direct+relay share.
type lockdownFixture struct {
	d           *Daemon
	sig         *mockSignalingClient
	mapper      *recordingDirectMapper
	starter     *recordingTunnelStarter
	ds          *directState
	code        string
	origin      string
	relayOrigin string
}

func newLockdownFixture(t *testing.T) *lockdownFixture {
	t.Helper()
	rec := &recordingDirectMapper{ip: "203.0.113.7"}
	return newLockdownFixtureWithPortMapper(t, rec, rec)
}

// blockingCloseMapper stalls DeletePortMapping until release. The OnDemandPort
// Close path performs the router delete synchronously on its state loop, so
// this makes the mapping-close lever (port.Close) block on a channel exactly
// like a wedged router. It backs the same recording accounting as the base
// mapper so the existing delete-count assertions keep working.
type blockingCloseMapper struct {
	*recordingDirectMapper
	deleteStarted chan struct{}
	release       chan struct{}
	deleteOnce    sync.Once
}

func (m *blockingCloseMapper) DeletePortMapping(ext int) error {
	m.deleteOnce.Do(func() { close(m.deleteStarted) })
	<-m.release
	return m.recordingDirectMapper.DeletePortMapping(ext)
}

// newLockdownFixtureWithPortMapper builds the same §13.4 fixture but lets the
// caller substitute the OnDemandPort's PortMapper (rec still backs fx.mapper
// and its delete accounting). Used by the bounded fan-out test to stall the
// mapping-close lever.
func newLockdownFixtureWithPortMapper(t *testing.T, rec *recordingDirectMapper, portMapper direct.PortMapper) *lockdownFixture {
	t.Helper()
	cfg := tunnelTestConfig(t)
	st := newMockStore()
	sig := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())
	d := &Daemon{
		config: cfg, configMgr: &mockConfigManager{cfg: cfg}, store: st, signaling: sig,
		sessions: map[string]*Session{},
	}

	dir := t.TempDir()
	cm, chainPEM := newBaselineCertFixture(t, dir)
	if err := cm.Install([]byte(chainPEM)); err != nil {
		t.Fatalf("install cert: %v", err)
	}
	mapper := rec
	port := direct.NewOnDemandPortOwned(portMapper, 443, 8443, time.Minute, "test", mapper.InternalIP())
	gate := direct.NewSignalGate(st.GetAgentID(), func(string, direct.RouteKind) bool { return true })
	ds := &directState{
		namespace: testDirectNS, baseDomain: testDirectBase,
		cert: cm, gate: gate, port: port, mapper: mapper,
		origins: map[string]originPair{}, relayOrigins: map[string]string{},
		listenAddr: reserveLoopbackAddr(t),
	}
	d.direct = ds
	d.syncDirectServe()
	ds.server.SetResolver(stubResolver{})
	t.Cleanup(func() { _ = port.Close() })

	starter := startTunnelForTest(t, d)

	const code = "LOCKDOWN1"
	origin := testOriginFor("sblockdwn1")
	relayOrigin := relayOriginForLabel("sblockdwn1")
	if _, ok := d.bindOrigin(code, origin); !ok {
		t.Fatalf("bind direct+relay origins for %s", code)
	}
	return &lockdownFixture{
		d: d, sig: sig, mapper: mapper, starter: starter, ds: ds,
		code: code, origin: origin, relayOrigin: relayOrigin,
	}
}

// openRouteConn dials the running listener with the given SNI, serves one
// authorized request (which records the Binder-admitted route on the
// connection), and returns the still-open keep-alive connection.
func openRouteConn(t *testing.T, ds *directState, sni, code string) net.Conn {
	t.Helper()
	conn, err := tls.Dial("tcp", ds.listenAddr, &tls.Config{
		ServerName: sni, InsecureSkipVerify: true, NextProtos: []string{"http/1.1"},
	})
	if err != nil {
		t.Fatalf("dial %s: %v", sni, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	req := fmt.Sprintf("GET /s/%s/ HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive\r\n\r\n", code, sni)
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	header := make([]byte, 0, 1024)
	one := make([]byte, 1)
	for !bytes.HasSuffix(header, []byte("\r\n\r\n")) {
		n, err := conn.Read(one)
		if err != nil {
			t.Fatalf("read response header: %v", err)
		}
		header = append(header, one[:n]...)
		if len(header) > 64*1024 {
			t.Fatalf("response header too large")
		}
	}
	if !bytes.HasPrefix(header, []byte("HTTP/1.1 200")) {
		t.Fatalf("unexpected response for %s: %q", sni, header)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		t.Fatalf("clear deadline: %v", err)
	}
	return conn
}

// assertConnClosed asserts the server closed an established connection within
// the observation window (any read error after draining the buffered body).
func assertConnClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 256)
	for {
		_, err := conn.Read(buf)
		if err == nil {
			continue // drain any already-buffered body
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("connection still open after lockdown")
		}
		return // EOF / reset: the server closed it
	}
}

// assertListenerClosed waits for the local HTTPS listener to stop accepting.
func assertListenerClosed(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			return
		}
		_ = conn.Close()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("listener %s still accepting after lockdown", addr)
}

// validOpenSignal builds one admissible direct open signal for the fixture.
func validOpenSignal(agentID, shareID, nonce string) direct.OpenSignal {
	return direct.OpenSignal{
		Version: 1, AgentID: agentID, ShareID: shareID,
		RouteKind: direct.RouteDirect, Nonce: nonce, Seq: 1,
		ExpiresAt: time.Now().Add(time.Minute), Lease: 30 * time.Second,
	}
}

// TestLockdownSetsGateDeletesMappingStopsTunnelRevokesBinderAndClosesBothRoutes
// pins the six §13.4 actions of one reversible lockdown: the direct
// SignalGate refuses new opens, the on-demand mapping is deleted, frpc stops,
// both Binder admissions are withdrawn (while the source/session state stays
// recorded for unlock), established direct AND relay connections close, and
// the advisory locked state is reported to control.
func TestLockdownSetsGateDeletesMappingStopsTunnelRevokesBinderAndClosesBothRoutes(t *testing.T) {
	fx := newLockdownFixture(t)

	// A healthy tunnel and an open direct mapping are the pre-lockdown state.
	fx.d.handleSignalingMessage(relayConfigMessage(t, 1, "credential-before-lockdown"))
	waitForCond(t, func() bool { return fx.starter.startCount() == 1 })
	if err := fx.ds.port.OpenFor(fx.code, time.Minute); err != nil {
		t.Fatalf("open mapping: %v", err)
	}
	if !fx.ds.port.Open() {
		t.Fatalf("mapping must be open before lockdown")
	}

	fx.d.startDirectServer()
	waitDialable(t, fx.ds.listenAddr)
	directConn := openRouteConn(t, fx.ds, fx.origin, fx.code)
	relayConn := openRouteConn(t, fx.ds, fx.relayOrigin, fx.code)

	require.NoError(t, fx.d.Lockdown())
	require.True(t, fx.d.IsLocked())

	// 1. SignalGate lockdown: new direct opens fail.
	if err := fx.ds.gate.Admit(validOpenSignal("test-agent-id", fx.code, "nonce-locked")); !errors.Is(err, direct.ErrSignalLockdown) {
		t.Fatalf("gate admit during lockdown = %v, want ErrSignalLockdown", err)
	}

	// 2. UPnP/NAT-PMP mapping deleted (the state loop performs the delete on
	// its next tick).
	waitForCond(t, func() bool { return fx.mapper.deleteCount() > 0 })
	if fx.ds.port.Open() {
		t.Fatalf("mapping must be deleted on lockdown")
	}

	// 3. frpc stopped (gateway presence then expires).
	child := fx.starter.recordAt(0).child
	waitForCond(t, func() bool { return child.gracefulStopCount() == 1 })
	if child.killCount() != 0 {
		t.Fatalf("graceful lockdown must not need a kill, got %d", child.killCount())
	}

	// 4. Binder admissions withdrawn, source/session state retained.
	if _, err := fx.ds.binder.AdmitSNI(fx.origin); err == nil {
		t.Fatalf("direct origin still admitted after lockdown")
	}
	if _, err := fx.ds.binder.AdmitSNI(fx.relayOrigin); err == nil {
		t.Fatalf("relay origin still admitted after lockdown")
	}
	if _, ok := fx.ds.origins[fx.code]; !ok {
		t.Fatalf("origin pair must be retained for unlock")
	}

	// 5. Both route kinds' established connections close.
	assertConnClosed(t, directConn)
	assertConnClosed(t, relayConn)

	// §7.4: the local HTTPS listener leaves service on lockdown.
	assertListenerClosed(t, fx.ds.listenAddr)

	// 6. Advisory locked state reported to control.
	waitForCond(t, func() bool {
		return fx.sig.hasSentMessage("lockdown_status", map[string]any{"locked": true})
	})
}

// TestLockdownGateIsFinalAndLeversFanOutBounded pins the §13.4 concurrency
// contract: the local locked gate is set FIRST and is final, the remaining
// best-effort levers run independently and concurrently, and a stalled lever
// (a blocking router delete here) can neither prevent another lever from
// running nor make Lockdown hang unboundedly.
func TestLockdownGateIsFinalAndLeversFanOutBounded(t *testing.T) {
	bm := &blockingCloseMapper{
		recordingDirectMapper: &recordingDirectMapper{ip: "203.0.113.7"},
		deleteStarted:         make(chan struct{}),
		release:               make(chan struct{}),
	}
	fx := newLockdownFixtureWithPortMapper(t, bm.recordingDirectMapper, bm)
	// The mapping-close lever is deliberately held open; every other lever
	// must still run and Lockdown must still return at its bound.
	fx.d.lockdownLeverTimeout = 200 * time.Millisecond
	// Guarantee the stalled lever is released before the fixture's port.Close
	// cleanup runs (defers run before t.Cleanup).
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(bm.release) }) }
	defer release()

	// Pre-lockdown: healthy tunnel, open mapping, live listener, one
	// established direct connection (so every lever has observable work).
	fx.d.handleSignalingMessage(relayConfigMessage(t, 1, "credential-bounded"))
	waitForCond(t, func() bool { return fx.starter.startCount() == 1 })
	require.NoError(t, fx.ds.port.OpenFor(fx.code, time.Minute))
	if !fx.ds.port.Open() {
		t.Fatalf("mapping must be open before lockdown")
	}
	fx.d.startDirectServer()
	waitDialable(t, fx.ds.listenAddr)
	conn := openRouteConn(t, fx.ds, fx.origin, fx.code)

	start := time.Now()
	lockDone := make(chan error, 1)
	go func() { lockDone <- fx.d.Lockdown() }()

	// The stalled lever actually started (the router delete is in flight).
	select {
	case <-bm.deleteStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("mapping-close lever never reached the router delete")
	}

	// (c) Lockdown returns within its bound while the lever is still blocked.
	select {
	case err := <-lockDone:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Lockdown did not return within its bound while the mapping-close lever was blocked")
	}
	if elapsed := time.Since(start); elapsed >= 3*time.Second {
		t.Fatalf("Lockdown took %s, want bounded return while the slowest lever is stalled", elapsed)
	}

	// (a) The local gate is already set and final while the slowest lever is
	// still blocked: no new direct open is admitted.
	require.True(t, fx.d.IsLocked(), "gate must be locked once Lockdown returns")
	if err := fx.ds.gate.Admit(validOpenSignal("test-agent-id", fx.code, "nonce-bounded")); !errors.Is(err, direct.ErrSignalLockdown) {
		t.Fatalf("gate admit while the mapping-close lever is blocked = %v, want ErrSignalLockdown", err)
	}

	// (b) The other levers still executed despite the stall.
	if _, err := fx.ds.binder.AdmitSNI(fx.origin); err == nil {
		t.Fatalf("binder admission not withdrawn while mapping close was stalled")
	}
	assertConnClosed(t, conn)
	assertListenerClosed(t, fx.ds.listenAddr)
	child := fx.starter.recordAt(0).child
	waitForCond(t, func() bool { return child.gracefulStopCount() == 1 })
	waitForCond(t, func() bool {
		return fx.sig.hasSentMessage("lockdown_status", map[string]any{"locked": true})
	})

	// The stalled lever completes once the router recovers; it never blocked
	// the others or the return above.
	release()
	waitForCond(t, func() bool { return fx.mapper.deleteCount() > 0 })
}

// TestUnlockRestoresSourceVerifiedBindingsAndUsesFreshCredential pins the
// §13.4 unlock contract: explicit local action clears the gate, re-admits the
// retained source-verified bindings, brings the listener back, requests a
// FRESH tunnel credential (relay_credential_request, reason restart) and
// never restarts frpc with the pre-lockdown credential.
func TestUnlockRestoresSourceVerifiedBindingsAndUsesFreshCredential(t *testing.T) {
	fx := newLockdownFixture(t)

	fx.d.handleSignalingMessage(relayConfigMessage(t, 1, "credential-before-lockdown"))
	waitForCond(t, func() bool { return fx.starter.startCount() == 1 })

	require.NoError(t, fx.d.Lockdown())
	require.True(t, fx.d.IsLocked())

	require.NoError(t, fx.d.Unlock())
	require.False(t, fx.d.IsLocked())

	// Gate re-opens.
	if err := fx.ds.gate.Admit(validOpenSignal("test-agent-id", fx.code, "nonce-unlocked")); err != nil {
		t.Fatalf("gate must re-open on unlock: %v", err)
	}

	// Both source-verified bindings are restored.
	if _, err := fx.ds.binder.AdmitSNI(fx.origin); err != nil {
		t.Fatalf("direct origin must be re-admitted on unlock: %v", err)
	}
	if _, err := fx.ds.binder.AdmitSNI(fx.relayOrigin); err != nil {
		t.Fatalf("relay origin must be re-admitted on unlock: %v", err)
	}

	// Listener back in service.
	waitDialable(t, fx.ds.listenAddr)

	// A fresh credential/presence is requested; the pre-lockdown credential is
	// never reused.
	waitForCond(t, func() bool {
		return fx.sig.hasSentMessage("relay_credential_request", map[string]any{"reason": "restart"})
	})
	assertConditionStays(t, "no restart with the stale credential", 150*time.Millisecond, func() bool {
		return fx.starter.startCount() == 1
	})

	// Control answers with a fresh relay_config: frpc restarts exactly once
	// with the fresh one-use credential rendered into the config file.
	fx.d.handleSignalingMessage(relayConfigMessage(t, 2, "credential-after-unlock"))
	waitForCond(t, func() bool { return fx.starter.startCount() == 2 })
	configBytes, err := os.ReadFile(filePath(fx.d.config.TunnelDataDir, "frpc.toml"))
	require.NoError(t, err)
	require.Contains(t, string(configBytes), "credential-after-unlock")
	require.NotContains(t, string(configBytes), "credential-before-lockdown")
}

// TestLockdownDoesNotTombstoneSession pins the §13.4 reversibility rule:
// lockdown is an availability state, NOT revocation — the session stays in
// memory and in the store, no deregister/unregister is sent, and unlock keeps
// it intact.
func TestLockdownDoesNotTombstoneSession(t *testing.T) {
	fx := newLockdownFixture(t)
	session := &Session{
		Code: fx.code, ShareURL: "immich://LOCKDOWNKEY", ShareType: "immich",
		ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(),
	}
	fx.d.mu.Lock()
	fx.d.sessions[fx.code] = session
	fx.d.mu.Unlock()
	require.NoError(t, fx.d.store.SaveSession(store.SessionEntry{
		Code: fx.code, ShareURL: session.ShareURL, ShareType: "immich",
		ExpiresAt: session.ExpiresAt, CreatedAt: session.CreatedAt,
	}))

	require.NoError(t, fx.d.Lockdown())
	require.True(t, fx.d.IsLocked())

	// A control reconnect while locked must not restart the listener: the
	// daemon re-reports the advisory state and stays down until explicit
	// local unlock.
	fx.d.handleEnrollmentReady(signaling.Message{Type: "enrollment_ready"})
	require.True(t, fx.d.IsLocked())
	fx.ds.mu.Lock()
	started := fx.ds.started
	fx.ds.mu.Unlock()
	require.False(t, started, "locked enrollment_ready must not start the listener")

	require.NotNil(t, fx.d.GetSession(fx.code), "lockdown must not remove the session")
	require.NotNil(t, fx.d.store.GetSession(fx.code), "lockdown must not delete the persisted row")
	require.False(t, fx.sig.sentContains("deregister"), "lockdown must not tombstone the control registration")
	require.False(t, fx.sig.sentContains("unregister_share"), "lockdown must not unregister the share")

	require.NoError(t, fx.d.Unlock())
	require.NotNil(t, fx.d.GetSession(fx.code), "unlock must keep the session intact")
}
