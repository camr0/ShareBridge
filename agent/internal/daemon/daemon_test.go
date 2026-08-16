package daemon

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
	"sharebridge/agent/internal/config"
	"sharebridge/agent/internal/immich"
	"sharebridge/agent/internal/multilane"
	"sharebridge/agent/internal/peer"
	"sharebridge/agent/internal/signaling"
	"sharebridge/agent/internal/store"
)

func TestImmichTransferAdapterMapsAlbumDownloadAndStreamsArchive(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/shared-links/me":
			_, _ = w.Write([]byte(`{"type":"ALBUM","album":{"id":"album-1","albumName":"Summer"}}`))
		case "/api/download/info":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"totalSize":4,"archives":[{"assetIds":["asset-1"],"size":4}]}`))
		case "/api/download/archive":
			_, _ = w.Write([]byte("zip!"))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer ts.Close()
	u, err := url.Parse(ts.URL)
	require.NoError(t, err)
	client, err := immich.New(immich.Config{BaseURL: ts.URL, AllowedHost: u.Host, ShareKey: "sharekey"})
	require.NoError(t, err)
	adapter := immichTransferAdapter{client: client}

	download, err := adapter.GetAlbumDownload(t.Context())
	require.NoError(t, err)
	require.Equal(t, "Summer", download.AlbumName)
	require.Equal(t, int64(4), download.TotalSize)
	require.Equal(t, []string{"asset-1"}, download.Archives[0].AssetIDs)
	require.Equal(t, int64(4), download.Archives[0].EstimatedSize)

	var archive bytes.Buffer
	n, err := adapter.StreamAlbumArchive(t.Context(), []string{"asset-1"}, &archive)
	require.NoError(t, err)
	require.Equal(t, int64(4), n)
	require.Equal(t, "zip!", archive.String())
}

// mockConfigManager implements ConfigManagerInterface for testing.
type mockConfigManager struct {
	cfg *config.Config
}

func (m *mockConfigManager) Get() *config.Config {
	return m.cfg
}

// mockStore implements StoreInterface for testing.
type mockStore struct {
	mu              sync.Mutex
	agentID         string
	relayStaticPriv []byte
	sessions        map[string]store.SessionEntry
	downloads       map[string]int
	saveError       error
	deleteError     error
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

func (m *mockStore) GetRelayStaticPrivateKey() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.relayStaticPriv == nil {
		// Generate a deterministic test key
		m.relayStaticPriv = make([]byte, 32)
		for i := range m.relayStaticPriv {
			m.relayStaticPriv[i] = byte(i)
		}
	}
	return m.relayStaticPriv, nil
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
	iceServers    []webrtc.ICEServer
	registerShare func(ctx context.Context, shareURL, preferredCode string, relayOnly bool, relayStaticPub string) (string, bool, error)
	shareOrigin   func(code, shareURL string) string
	sendMessages  []map[string]any
	codeCounter   int // Counter for generating unique codes
	registered    []signaling.RegisterShareOptions
	unregistered  []string
	unregisterErr error
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

func (m *mockSignalingClient) RegisterShare(ctx context.Context, shareURL, preferredCode string, relayOnly bool, relayStaticPub string) (string, string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var code string
	var reconnected bool
	var err error
	if m.registerShare != nil {
		code, reconnected, err = m.registerShare(ctx, shareURL, preferredCode, relayOnly, relayStaticPub)
	} else if preferredCode != "" {
		code, reconnected = preferredCode, true
	} else {
		m.codeCounter++
		code = fmt.Sprintf("test-code-%d", m.codeCounter)
	}
	origin := ""
	if m.shareOrigin != nil {
		origin = m.shareOrigin(code, shareURL)
	}
	return code, origin, reconnected, err
}

func (m *mockSignalingClient) RegisterShareWithOptions(ctx context.Context, opts signaling.RegisterShareOptions) (string, string, bool, error) {
	m.mu.Lock()
	m.registered = append(m.registered, opts)
	m.mu.Unlock()
	return m.RegisterShare(ctx, opts.ShareURL, opts.PreferredCode, opts.RelayOnly, opts.RelayStaticPub)
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
		DefaultRelayOnly: true,
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	sig := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())

	d, err := NewWithSignaling(cfgMgr, st, sig)
	require.NoError(t, err)
	return d, sig
}

type fakeImmichAuth struct {
	gotPassword string
	ok          bool
	err         error
}

func (f *fakeImmichAuth) ValidatePassword(ctx context.Context, password string) (bool, error) {
	f.gotPassword = password
	return f.ok, f.err
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

func TestCreateSessionManualImmichRegistersRelayOnlyShare(t *testing.T) {
	d, sig := newTestDaemon(t)
	d.config.ImmichURL = "http://immich.lan:2283"
	d.config.ImmichAllowedHost = "immich.lan:2283"
	d.config.ImmichAPIKey = "api"
	d.newImmichPoller = func() (immichPoller, error) {
		return &fakeImmichPoller{shares: []immich.SharedLink{
			{Key: "IMMICHMANUAL1", Type: "ALBUM", Password: "********"},
		}}, nil
	}

	code, err := d.CreateSession(context.Background(), "immich://IMMICHMANUAL1", "immich", "", 24*time.Hour, 10, false)

	require.NoError(t, err)
	require.Equal(t, "IMMICHMANUAL1", code)

	session := d.GetSession("IMMICHMANUAL1")
	require.NotNil(t, session)
	require.Equal(t, "immich://IMMICHMANUAL1", session.ShareURL)
	require.Equal(t, "immich", session.ShareType)
	require.True(t, session.RelayOnly)
	require.True(t, session.IsPasswordProtected)
	require.NotNil(t, session.immichClient)

	stored := d.store.GetSession("IMMICHMANUAL1")
	require.NotNil(t, stored)
	require.Equal(t, "immich://IMMICHMANUAL1", stored.ShareURL)
	require.Equal(t, "immich", stored.ShareType)
	require.True(t, stored.RelayOnly)
	require.True(t, stored.IsPasswordProtected)

	registered := sig.registeredSnapshot()
	require.Len(t, registered, 1)
	require.Equal(t, signaling.RegisterShareOptions{
		ShareURL:            "immich://IMMICHMANUAL1",
		PreferredCode:       "IMMICHMANUAL1",
		ShareType:           "immich",
		IsPasswordProtected: true,
		RelayOnly:           true,
		RelayStaticPub:      registered[0].RelayStaticPub,
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
		Code:          "IMMICHMANUAL1",
		ShareURL:      "immich://IMMICHMANUAL1",
		ShareType:     "immich",
		RelayOnly:     true,
		CreatedAt:     time.Now().Add(-time.Hour),
		peers:         make(map[string]*peer.Peer),
		relayChannels: make(map[string]relayTransferChannel),
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
		peers:     make(map[string]*peer.Peer),
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
		peers:     make(map[string]*peer.Peer),
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
		ShareType:    "opencloud",
		ExpiresAt:    time.Now().Add(24 * time.Hour),
		MaxDownloads: 10,
		Downloads:    3,
		RelayOnly:    false,
		CreatedAt:    time.Now().Add(-1 * time.Hour),
	})

	sigClient := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())
	sigClient.registerShare = func(ctx context.Context, shareURL, preferredCode string, relayOnly bool, relayStaticPub string) (string, bool, error) {
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
	sigClient.registerShare = func(ctx context.Context, shareURL, preferredCode string, relayOnly bool, relayStaticPub string) (string, bool, error) {
		return preferredCode, true, nil
	}

	d, err := NewWithSignaling(cfgMgr, st, sigClient)
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

// TestDaemonGetsRelayStaticKey tests that the daemon correctly retrieves
// the relay static private key and derives the public key.
func TestDaemonGetsRelayStaticKey(t *testing.T) {
	cfg := &config.Config{
		SignalingURL: "ws://localhost:8080",
		APIKey:       "test-api-key",
		AllowedHost:  "opencloud.example.com",
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()

	// Get relay static private key from mock store
	privKey, err := st.GetRelayStaticPrivateKey()
	if err != nil {
		t.Fatalf("GetRelayStaticPrivateKey() error: %v", err)
	}

	// Derive public key using the helper function
	pubHex, err := RelayStaticPubHex(privKey)
	if err != nil {
		t.Fatalf("RelayStaticPubHex() error: %v", err)
	}

	// Verify public key is hex-encoded and starts with expected prefix (uncompressed P-256)
	if len(pubHex) != 130 { // 65 bytes * 2 hex chars = 130
		t.Errorf("relay static public key hex length = %d, expected 130", len(pubHex))
	}
	if pubHex[:2] != "04" {
		t.Errorf("relay static public key should start with 04 (uncompressed point), got %s", pubHex[:2])
	}

	// Verify the mock signaling client receives the relay_static_pub
	sigClient := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())
	var receivedRelayPub string
	sigClient.registerShare = func(ctx context.Context, shareURL, preferredCode string, relayOnly bool, relayStaticPub string) (string, bool, error) {
		receivedRelayPub = relayStaticPub
		return "test-code", false, nil
	}

	d, err := NewWithSignaling(cfgMgr, st, sigClient)
	if err != nil {
		t.Fatalf("NewWithSignaling() error: %v", err)
	}

	ctx := context.Background()
	code, err := d.CreateSession(ctx, "https://opencloud.example.com/s/abc123", "opencloud", "", 24*time.Hour, 10, false)
	if err != nil {
		t.Fatalf("CreateSession() error: %v", err)
	}

	if code != "test-code" {
		t.Errorf("CreateSession() code = %s, expected test-code", code)
	}

	if receivedRelayPub != pubHex {
		t.Errorf("RegisterShare received relay_static_pub = %s, expected %s", receivedRelayPub, pubHex)
	}
}

// mockRelayChannel implements relayTransferChannel for testing.
type mockRelayChannel struct {
	startFn      func(context.Context) error
	closeFn      func() error
	onOpen       func()
	onClose      func()
	mu           sync.Mutex
	endpoints    map[multilane.Lane]*mockRelayEndpoint
	endpointHits map[multilane.Lane]int
	closeCalls   int
}

type mockRelayEndpoint struct {
	mu         sync.Mutex
	onMessage  func([]byte)
	sentText   []string
	sentClass  []multilane.TrafficClass
	sendTextFn func(string) error
}

func (m *mockRelayEndpoint) SendBinary([]byte) error { return nil }
func (m *mockRelayEndpoint) SendText(text string) error {
	m.mu.Lock()
	m.sentText = append(m.sentText, text)
	sendTextFn := m.sendTextFn
	m.mu.Unlock()
	if sendTextFn != nil {
		return sendTextFn(text)
	}
	return nil
}
func (m *mockRelayEndpoint) SendBinaryClass(class multilane.TrafficClass, _ []byte) error {
	m.mu.Lock()
	m.sentClass = append(m.sentClass, class)
	m.mu.Unlock()
	return nil
}
func (m *mockRelayEndpoint) BufferedAmount() uint64 { return 0 }
func (m *mockRelayEndpoint) SetOnMessage(handler func([]byte)) {
	m.mu.Lock()
	m.onMessage = handler
	m.mu.Unlock()
}
func (m *mockRelayEndpoint) deliver(text string) {
	m.mu.Lock()
	handler := m.onMessage
	m.mu.Unlock()
	if handler != nil {
		handler([]byte(text))
	}
}
func (m *mockRelayEndpoint) texts() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.sentText...)
}

func (m *mockRelayChannel) ensureEndpoints() {
	if m.endpoints != nil {
		return
	}
	m.endpoints = map[multilane.Lane]*mockRelayEndpoint{
		multilane.LaneControl: {},
		multilane.LaneMedia:   {},
		multilane.LaneBulk:    {},
	}
	m.endpointHits = make(map[multilane.Lane]int)
}

func (m *mockRelayChannel) Endpoint(lane multilane.Lane) multilane.Endpoint {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureEndpoints()
	m.endpointHits[lane]++
	return m.endpoints[lane]
}

func (m *mockRelayChannel) endpoint(lane multilane.Lane) *mockRelayEndpoint {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureEndpoints()
	return m.endpoints[lane]
}

func (m *mockRelayChannel) hits(lane multilane.Lane) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.endpointHits[lane]
}

func (m *mockRelayChannel) Start(ctx context.Context) error {
	if m.startFn != nil {
		return m.startFn(ctx)
	}
	return nil
}

func (m *mockRelayChannel) Close() error {
	m.mu.Lock()
	m.closeCalls++
	m.mu.Unlock()
	if m.closeFn != nil {
		return m.closeFn()
	}
	return nil
}

func (m *mockRelayChannel) SetOnOpen(handler func()) {
	m.onOpen = handler
}

func (m *mockRelayChannel) SetOnClose(handler func()) {
	m.onClose = handler
}

// hasSentMessage checks if a message was sent with the given type and fields.
func hasSentMessage(messages []map[string]any, msgType string, expectedFields map[string]any) bool {
	for _, msg := range messages {
		if msg["type"] == msgType {
			// Check all expected fields match
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

// TestHandleJoin_SendsAuthOKAfterHMACVerification tests that auth_ok is sent
// after successful HMAC verification, before creating the peer.
func TestHandleJoin_SendsAuthOKAfterHMACVerification(t *testing.T) {
	cfg := &config.Config{SignalingURL: "ws://localhost:8080", APIKey: "test-key"}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	sig := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())

	d, err := NewWithSignaling(cfgMgr, st, sig)
	if err != nil {
		t.Fatalf("NewWithSignaling: %v", err)
	}

	d.sessions["SHARE123"] = &Session{
		Code:      "SHARE123",
		Password:  "secret",
		CreatedAt: time.Now(),
		peers:     make(map[string]*peer.Peer),
	}

	d.nonces["conn-1"] = nonceEntry{
		nonce:     "abc123",
		expiresAt: time.Now().Add(time.Minute),
	}

	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write([]byte("abc123"))
	joinedHMAC := hex.EncodeToString(mac.Sum(nil))

	d.handleJoin("conn-1", "SHARE123", joinedHMAC)

	// Check that auth_ok was sent with correct fields
	if !sig.hasSentMessage("auth_ok", map[string]any{
		"conn_id": "conn-1",
		"code":    "SHARE123",
	}) {
		t.Fatalf("expected auth_ok to be sent, got %#v", sig.messagesSnapshot())
	}
}

func TestHandleJoin_RelayOnlySkipsDirectPeerCreation(t *testing.T) {
	cfg := &config.Config{SignalingURL: "ws://localhost:8080", APIKey: "test-key"}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	sig := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())

	d, err := NewWithSignaling(cfgMgr, st, sig)
	if err != nil {
		t.Fatalf("NewWithSignaling: %v", err)
	}

	session := &Session{
		Code:      "SHARE123",
		Password:  "secret",
		RelayOnly: true,
		CreatedAt: time.Now(),
		peers:     make(map[string]*peer.Peer),
	}
	d.sessions["SHARE123"] = session

	d.nonces["conn-1"] = nonceEntry{
		nonce:     "abc123",
		expiresAt: time.Now().Add(time.Minute),
	}

	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write([]byte("abc123"))
	joinedHMAC := hex.EncodeToString(mac.Sum(nil))

	d.handleJoin("conn-1", "SHARE123", joinedHMAC)
	time.Sleep(100 * time.Millisecond)

	if !sig.hasSentMessage("auth_ok", map[string]any{
		"conn_id": "conn-1",
		"code":    "SHARE123",
	}) {
		t.Fatalf("expected auth_ok to be sent, got %#v", sig.messagesSnapshot())
	}
	if sig.hasSentMessage("offer", map[string]any{
		"session_id": "SHARE123",
		"peer_id":    "conn-1",
	}) {
		t.Fatalf("relay_only session should not create direct offer, got %#v", sig.messagesSnapshot())
	}
}

func TestHandleJoin_ImmichUnprotectedRequiresNonceButSkipsHMAC(t *testing.T) {
	d, sig := newTestDaemon(t)
	d.sessions["IMMICHOPEN1"] = &Session{
		Code: "IMMICHOPEN1", ShareType: "immich", RelayOnly: true,
		peers: map[string]*peer.Peer{}, relayChannels: map[string]relayTransferChannel{},
	}
	d.nonces["conn1"] = nonceEntry{nonce: "abc123", expiresAt: time.Now().Add(time.Minute)}

	d.handleJoin("conn1", "IMMICHOPEN1", "")

	require.True(t, sig.sentContains(`"type":"auth_ok"`))
	require.True(t, sig.sentContains(`"code":"IMMICHOPEN1"`))
}

func TestHandleJoin_ImmichProtectedRejectsNonceJoin(t *testing.T) {
	d, sig := newTestDaemon(t)
	d.sessions["IMMICHPROTECTED1"] = &Session{
		Code: "IMMICHPROTECTED1", ShareType: "immich", IsPasswordProtected: true, RelayOnly: true,
		peers: map[string]*peer.Peer{}, relayChannels: map[string]relayTransferChannel{},
	}
	d.nonces["conn1"] = nonceEntry{nonce: "abc123", expiresAt: time.Now().Add(time.Minute)}

	d.handleJoin("conn1", "IMMICHPROTECTED1", "")

	require.True(t, sig.sentContains(`"type":"auth_failed"`))
	require.False(t, sig.sentContains(`"type":"auth_ok"`))
}

func TestHandlePasswordSubmit_ImmichValidatesWithClientBeforeAuthOK(t *testing.T) {
	d, sig := newTestDaemon(t)
	fake := &fakeImmichAuth{ok: true}
	d.sessions["IMMICHPASS1"] = &Session{
		Code: "IMMICHPASS1", ShareType: "immich", RelayOnly: true,
		immichClient: fake,
		peers:        map[string]*peer.Peer{}, relayChannels: map[string]relayTransferChannel{},
	}

	d.handlePasswordSubmit("conn1", "IMMICHPASS1", "secret")

	require.Equal(t, "secret", fake.gotPassword)
	require.True(t, sig.sentContains(`"type":"auth_ok"`))
}

func TestHandlePasswordSubmit_ImmichInvalidPasswordSendsAuthFail(t *testing.T) {
	d, sig := newTestDaemon(t)
	d.sessions["IMMICHPASS1"] = &Session{
		Code: "IMMICHPASS1", ShareType: "immich", RelayOnly: true,
		immichClient: &fakeImmichAuth{ok: false},
		peers:        map[string]*peer.Peer{}, relayChannels: map[string]relayTransferChannel{},
	}

	d.handlePasswordSubmit("conn1", "IMMICHPASS1", "wrong")

	require.True(t, sig.sentContains(`"type":"auth_fail"`))
	require.False(t, sig.sentContains(`"type":"auth_ok"`))
}

func TestHandlePasswordSubmit_NonImmichSendsAuthFail(t *testing.T) {
	d, sig := newTestDaemon(t)
	d.sessions["WEBDAVPASS1"] = &Session{
		Code: "WEBDAVPASS1", ShareType: "opencloud", RelayOnly: true,
		peers: map[string]*peer.Peer{}, relayChannels: map[string]relayTransferChannel{},
	}

	d.handlePasswordSubmit("conn1", "WEBDAVPASS1", "secret")

	require.True(t, sig.sentContains(`"type":"auth_fail"`))
	require.False(t, sig.sentContains(`"type":"auth_ok"`))
}

func TestSyncImmichSharesRegistersNewAndUnregistersRemoved(t *testing.T) {
	d, sig := newTestDaemon(t)
	d.config.ImmichURL = "http://immich.lan:2283"
	d.config.ImmichAllowedHost = "immich.lan:2283"
	d.config.ImmichAPIKey = "api"
	d.newImmichPoller = func() (immichPoller, error) {
		return &fakeImmichPoller{shares: []immich.SharedLink{
			{Key: "IMMICHNEW1", Type: "ALBUM", Password: "********"},
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

func TestSyncImmichSharesRemovedShareNotifiesRelayChannelBeforeClose(t *testing.T) {
	d, sig := newTestDaemon(t)
	d.config.ImmichURL = "http://immich.lan:2283"
	d.config.ImmichAllowedHost = "immich.lan:2283"
	d.config.ImmichAPIKey = "api"
	d.newImmichPoller = func() (immichPoller, error) {
		return &fakeImmichPoller{}, nil
	}

	var events []string
	session := &Session{
		Code:          "IMMICHOLD1",
		ShareType:     "immich",
		RelayOnly:     true,
		peers:         map[string]*peer.Peer{},
		relayChannels: map[string]relayTransferChannel{},
	}
	channel := &mockRelayChannel{
		closeFn: func() error {
			events = append(events, "close")
			return nil
		},
	}
	channel.endpoint(multilane.LaneControl).sendTextFn = func(text string) error {
		events = append(events, "send:"+text)
		return nil
	}
	session.relayChannels["sid-1"] = channel
	d.sessions["IMMICHOLD1"] = session

	require.NoError(t, d.syncImmichShares(context.Background()))

	require.True(t, sig.unregisteredCode("IMMICHOLD1"))
	require.Len(t, events, 2)
	require.Equal(t, "close", events[1])
	require.Contains(t, events[0], `"type":"error"`)
	require.Contains(t, events[0], `"scope":"connection"`)
	require.Contains(t, events[0], `"message":"share has been removed"`)
}

func TestSessionResourceCloseCallbacksCanReenterDaemonAndSessionLocks(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*Daemon, *Session)
	}{
		{name: "stop", run: func(d *Daemon, _ *Session) { _ = d.Stop() }},
		{name: "revoke", run: func(d *Daemon, session *Session) { _ = d.RevokeSession(session.Code) }},
		{name: "expiry", run: func(d *Daemon, _ *Session) { d.pruneExpiredSessions() }},
		{name: "removed share", run: func(d *Daemon, session *Session) {
			d.closeSessionResourcesWithError(session, "share has been removed")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			d, _ := newTestDaemon(t)
			session := &Session{
				Code: "SHARE123", ExpiresAt: time.Now().Add(-time.Minute), CreatedAt: time.Now(),
				peers: make(map[string]*peer.Peer), relayChannels: make(map[string]relayTransferChannel),
			}
			channel := &mockRelayChannel{}
			channel.closeFn = func() error {
				// Real channel close callbacks synchronously reenter session cleanup;
				// also touch the daemon lock to catch broad-lock closure.
				d.mu.RLock()
				d.mu.RUnlock()
				session.mu.Lock()
				session.mu.Unlock()
				return nil
			}
			session.relayChannels["sid-123"] = channel
			d.sessions[session.Code] = session

			done := make(chan struct{})
			go func() {
				test.run(d, session)
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(500 * time.Millisecond):
				t.Fatal("resource close deadlocked while callback reentered daemon/session locks")
			}
		})
	}
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
		peers: map[string]*peer.Peer{}, relayChannels: map[string]relayTransferChannel{},
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
		peers: map[string]*peer.Peer{}, relayChannels: map[string]relayTransferChannel{},
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
			{Key: "IMMICHCLIENT1", Type: "ALBUM", Password: "********"},
		}}, nil
	}

	require.NoError(t, d.syncImmichShares(context.Background()))

	session := d.GetSession("IMMICHCLIENT1")
	require.NotNil(t, session)
	require.NotNil(t, session.immichClient)
	require.True(t, session.IsPasswordProtected)

	stored := d.store.GetSession("IMMICHCLIENT1")
	require.NotNil(t, stored)
	require.True(t, stored.IsPasswordProtected)
}

func TestLoadSessionsFromStoreWiresPersistedImmichSession(t *testing.T) {
	cfg := &config.Config{
		SignalingURL:      "ws://localhost:8080",
		APIKey:            "test-key",
		ImmichURL:         "http://immich.lan:2283",
		ImmichAllowedHost: "immich.lan:2283",
		ImmichAPIKey:      "api",
		DefaultRelayOnly:  true,
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	require.NoError(t, st.SaveSession(store.SessionEntry{
		Code:                "IMMICHPERSIST1",
		ShareURL:            "immich://IMMICHPERSIST1",
		ShareType:           "immich",
		IsPasswordProtected: true,
		RelayOnly:           true,
		CreatedAt:           time.Now().Add(-time.Hour),
	}))
	sig := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())
	d, err := NewWithSignaling(cfgMgr, st, sig)
	require.NoError(t, err)

	d.loadSessionsFromStore(context.Background())

	session := d.GetSession("IMMICHPERSIST1")
	require.NotNil(t, session)
	require.Equal(t, "immich", session.ShareType)
	require.True(t, session.RelayOnly)
	require.True(t, session.IsPasswordProtected)
	require.NotNil(t, session.immichClient)
	require.True(t, sig.registeredCode("IMMICHPERSIST1"))
}

// TestHandleRelayPrepare_StartsRelayTransferChannel tests that relay_prepare
// starts a relay channel and adds it to the session.
func TestHandleRelayPrepare_StartsRelayTransferChannel(t *testing.T) {
	cfg := &config.Config{SignalingURL: "ws://localhost:8080", APIKey: "test-key"}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	sig := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())

	d, err := NewWithSignaling(cfgMgr, st, sig)
	if err != nil {
		t.Fatalf("NewWithSignaling: %v", err)
	}

	session := &Session{
		Code:          "SHARE123",
		CreatedAt:     time.Now(),
		peers:         make(map[string]*peer.Peer),
		relayChannels: make(map[string]relayTransferChannel),
	}
	d.sessions["SHARE123"] = session

	started := make(chan struct{}, 1)
	d.newRelayChannel = func(cfg relayChannelConfig) (relayTransferChannel, error) {
		return &mockRelayChannel{
			startFn: func(context.Context) error {
				started <- struct{}{}
				return nil
			},
		}, nil
	}

	d.handleSignalingMessage(signaling.Message{
		Type:     "relay_prepare",
		SID:      "sid-123",
		Code:     "SHARE123",
		RelayJWT: "relay.jwt.token",
	})

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("relay channel never started")
	}

	// Verify channel was added to session
	session.mu.Lock()
	channel := session.relayChannels["sid-123"]
	session.mu.Unlock()

	if channel == nil {
		t.Fatal("relay channel not added to session")
	}
}

func TestHandleRelayPrepare_RelayLaneManagerWaitsForTransportV2Handshake(t *testing.T) {
	d, _ := newTestDaemon(t)
	session := &Session{
		Code:          "SHARE123",
		ShareType:     "opencloud",
		CreatedAt:     time.Now(),
		peers:         make(map[string]*peer.Peer),
		relayChannels: make(map[string]relayTransferChannel),
	}
	d.sessions[session.Code] = session

	channel := &mockRelayChannel{}
	d.newRelayChannel = func(relayChannelConfig) (relayTransferChannel, error) {
		return channel, nil
	}

	d.handleRelayPrepare(signaling.Message{
		Type: "relay_prepare", SID: "sid-123", Code: session.Code, RelayJWT: "relay.jwt.token",
	})

	// The responder may claim the control lane, but application construction must
	// not touch media or bulk until transport_hello version 2 succeeds.
	require.Equal(t, 1, channel.hits(multilane.LaneControl))
	require.Zero(t, channel.hits(multilane.LaneMedia))
	require.Zero(t, channel.hits(multilane.LaneBulk))
	require.Empty(t, channel.endpoint(multilane.LaneControl).texts())

	channel.endpoint(multilane.LaneControl).deliver(`{"type":"transport_hello","version":2}`)

	require.Equal(t, 1, channel.hits(multilane.LaneMedia))
	require.Equal(t, 1, channel.hits(multilane.LaneBulk))
	controlText := strings.Join(channel.endpoint(multilane.LaneControl).texts(), "\n")
	require.Contains(t, controlText, `"type":"transport_ready"`)
	require.Contains(t, controlText, `"type":"hello"`)
	// File-only OpenCloud/Nextcloud sessions keep media ready but idle.
	require.Empty(t, channel.endpoint(multilane.LaneMedia).texts())
	require.Empty(t, channel.endpoint(multilane.LaneMedia).sentClass)
}

func TestDirectLaneManagerCreatedOnlyAfterChannelSetReady(t *testing.T) {
	d, _ := newTestDaemon(t)
	session := &Session{Code: "SHARE123", ShareType: "opencloud", CreatedAt: time.Now()}
	channels := &mockRelayChannel{}

	d.wireDirectTransferSession(session, "peer-123", channels, func() bool { return true })

	require.Zero(t, channels.hits(multilane.LaneControl))
	require.Zero(t, channels.hits(multilane.LaneMedia))
	require.Zero(t, channels.hits(multilane.LaneBulk))
	require.NotNil(t, channels.onOpen)

	channels.onOpen()

	require.Equal(t, 1, channels.hits(multilane.LaneControl))
	require.Equal(t, 1, channels.hits(multilane.LaneMedia))
	require.Equal(t, 1, channels.hits(multilane.LaneBulk))
	require.Contains(t, strings.Join(channels.endpoint(multilane.LaneControl).texts(), "\n"), `"type":"hello"`)
}

func TestDirectStaleReadyDoesNotActivateTransferManager(t *testing.T) {
	d, _ := newTestDaemon(t)
	session := &Session{Code: "SHARE123", ShareType: "opencloud", CreatedAt: time.Now()}
	channels := &mockRelayChannel{}
	d.wireDirectTransferSession(session, "peer-123", channels, func() bool { return false })

	channels.onOpen()
	channels.onOpen()

	require.Zero(t, channels.hits(multilane.LaneControl))
	require.Zero(t, channels.hits(multilane.LaneMedia))
	require.Zero(t, channels.hits(multilane.LaneBulk))
}

func TestCreatePeerAbandonsNewPeerWhenSessionRemovedDuringCreation(t *testing.T) {
	d, sig := newTestDaemon(t)
	session := &Session{
		Code: "SHARE123", CreatedAt: time.Now(), peers: make(map[string]*peer.Peer),
		relayChannels: make(map[string]relayTransferChannel),
	}
	d.sessions[session.Code] = session
	created, err := peer.New(nil, false)
	require.NoError(t, err)
	creationStarted := make(chan struct{})
	releaseCreation := make(chan struct{})
	d.newPeer = func([]webrtc.ICEServer, bool) (*peer.Peer, error) {
		close(creationStarted)
		<-releaseCreation
		return created, nil
	}
	done := make(chan struct{})
	go func() {
		d.createPeer("peer-123", session.Code)
		close(done)
	}()
	<-creationStarted
	d.mu.Lock()
	delete(d.sessions, session.Code)
	d.mu.Unlock()
	close(releaseCreation)
	<-done

	session.mu.Lock()
	require.Empty(t, session.peers)
	session.mu.Unlock()
	require.False(t, sig.sentContains(`"type":"offer"`))
}

func TestCreatePeerDuplicateCloseCannotDeleteReplacement(t *testing.T) {
	d, _ := newTestDaemon(t)
	session := &Session{
		Code: "SHARE123", CreatedAt: time.Now(), peers: make(map[string]*peer.Peer),
		relayChannels: make(map[string]relayTransferChannel),
	}
	d.sessions[session.Code] = session

	d.createPeer("peer-123", session.Code)
	session.mu.Lock()
	first := session.peers["peer-123"]
	session.mu.Unlock()
	require.NotNil(t, first)
	d.createPeer("peer-123", session.Code)
	session.mu.Lock()
	replacement := session.peers["peer-123"]
	session.mu.Unlock()
	require.NotNil(t, replacement)
	require.NotSame(t, first, replacement)

	require.NoError(t, first.Close())
	session.mu.Lock()
	current := session.peers["peer-123"]
	session.mu.Unlock()
	require.Same(t, replacement, current)
}

func TestRelayPrepareAbandonsChannelWhenSessionRemovedDuringCreation(t *testing.T) {
	d, _ := newTestDaemon(t)
	session := &Session{
		Code: "SHARE123", CreatedAt: time.Now(), peers: make(map[string]*peer.Peer),
		relayChannels: make(map[string]relayTransferChannel),
	}
	d.sessions[session.Code] = session
	channel := &mockRelayChannel{}
	creationStarted := make(chan struct{})
	releaseCreation := make(chan struct{})
	d.newRelayChannel = func(relayChannelConfig) (relayTransferChannel, error) {
		close(creationStarted)
		<-releaseCreation
		return channel, nil
	}
	done := make(chan struct{})
	go func() {
		d.handleRelayPrepare(signaling.Message{
			Type: "relay_prepare", SID: "sid-123", Code: session.Code, RelayJWT: "relay.jwt.token",
		})
		close(done)
	}()
	<-creationStarted
	d.mu.Lock()
	delete(d.sessions, session.Code)
	d.mu.Unlock()
	close(releaseCreation)
	<-done

	session.mu.Lock()
	require.Empty(t, session.relayChannels)
	session.mu.Unlock()
	channel.mu.Lock()
	require.Equal(t, 1, channel.closeCalls)
	channel.mu.Unlock()
}

func TestNewTransportsAreRejectedAfterSessionResourcesBeginClosing(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*Daemon, *Session, *mockRelayChannel)
	}{
		{
			name: "direct",
			run: func(d *Daemon, session *Session, _ *mockRelayChannel) {
				d.newPeer = peer.New
				d.createPeer("peer-123", session.Code)
			},
		},
		{
			name: "relay",
			run: func(d *Daemon, session *Session, channel *mockRelayChannel) {
				d.newRelayChannel = func(relayChannelConfig) (relayTransferChannel, error) { return channel, nil }
				d.handleRelayPrepare(signaling.Message{
					Type: "relay_prepare", SID: "sid-123", Code: session.Code, RelayJWT: "relay.jwt.token",
				})
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			d, sig := newTestDaemon(t)
			session := &Session{
				Code: "SHARE123", CreatedAt: time.Now(), peers: make(map[string]*peer.Peer),
				relayChannels: make(map[string]relayTransferChannel),
			}
			d.sessions[session.Code] = session
			d.closeSessionResources(session)
			channel := &mockRelayChannel{}

			test.run(d, session, channel)

			session.mu.Lock()
			require.Empty(t, session.peers)
			require.Empty(t, session.relayChannels)
			session.mu.Unlock()
			require.False(t, sig.sentContains(`"type":"offer"`))
			if test.name == "relay" {
				channel.mu.Lock()
				require.Equal(t, 1, channel.closeCalls)
				channel.mu.Unlock()
			}
		})
	}
}

func TestRelayRequiredLaneCloseTearsSessionDownOnce(t *testing.T) {
	d, _ := newTestDaemon(t)
	session := &Session{
		Code: "SHARE123", CreatedAt: time.Now(), peers: make(map[string]*peer.Peer),
		relayChannels: make(map[string]relayTransferChannel),
	}
	d.sessions[session.Code] = session
	channel := &mockRelayChannel{}
	d.newRelayChannel = func(relayChannelConfig) (relayTransferChannel, error) { return channel, nil }
	d.handleRelayPrepare(signaling.Message{
		Type: "relay_prepare", SID: "sid-123", Code: session.Code, RelayJWT: "relay.jwt.token",
	})

	require.NotNil(t, channel.onClose)
	channel.onClose()
	session.mu.Lock()
	_, exists := session.relayChannels["sid-123"]
	session.relayChannels["sid-123"] = channel
	session.mu.Unlock()
	require.False(t, exists)

	// A transport may surface the same terminal close from multiple required
	// lanes. The daemon session teardown remains single-shot.
	channel.onClose()
	session.mu.Lock()
	_, exists = session.relayChannels["sid-123"]
	session.mu.Unlock()
	require.True(t, exists)
}

func TestRelayStaleCloseDoesNotDeleteReplacementWithSameSID(t *testing.T) {
	d, _ := newTestDaemon(t)
	session := &Session{
		Code: "SHARE123", CreatedAt: time.Now(), peers: make(map[string]*peer.Peer),
		relayChannels: make(map[string]relayTransferChannel),
	}
	d.sessions[session.Code] = session
	oldChannel := &mockRelayChannel{}
	newChannel := &mockRelayChannel{}
	channels := []relayTransferChannel{oldChannel, newChannel}
	d.newRelayChannel = func(relayChannelConfig) (relayTransferChannel, error) {
		channel := channels[0]
		channels = channels[1:]
		return channel, nil
	}
	message := signaling.Message{Type: "relay_prepare", SID: "sid-123", Code: session.Code, RelayJWT: "relay.jwt.token"}
	d.handleRelayPrepare(message)
	d.handleRelayPrepare(message)

	oldChannel.onClose()

	session.mu.Lock()
	got := session.relayChannels["sid-123"]
	session.mu.Unlock()
	require.True(t, got == newChannel, "replacement relay channel was deleted")
}

func TestRelayStaleFailedStartDoesNotDeleteReplacementWithSameSID(t *testing.T) {
	d, _ := newTestDaemon(t)
	session := &Session{
		Code: "SHARE123", CreatedAt: time.Now(), peers: make(map[string]*peer.Peer),
		relayChannels: make(map[string]relayTransferChannel),
	}
	d.sessions[session.Code] = session
	startEntered := make(chan struct{})
	releaseStart := make(chan struct{})
	oldChannel := &mockRelayChannel{startFn: func(context.Context) error {
		close(startEntered)
		<-releaseStart
		return errors.New("stale start failed")
	}}
	newChannel := &mockRelayChannel{}
	var factoryMu sync.Mutex
	channels := []relayTransferChannel{oldChannel, newChannel}
	d.newRelayChannel = func(relayChannelConfig) (relayTransferChannel, error) {
		factoryMu.Lock()
		defer factoryMu.Unlock()
		channel := channels[0]
		channels = channels[1:]
		return channel, nil
	}
	message := signaling.Message{Type: "relay_prepare", SID: "sid-123", Code: session.Code, RelayJWT: "relay.jwt.token"}
	done := make(chan struct{})
	go func() {
		d.handleRelayPrepare(message)
		close(done)
	}()
	<-startEntered
	d.handleRelayPrepare(message)
	close(releaseStart)
	<-done

	session.mu.Lock()
	got := session.relayChannels["sid-123"]
	session.mu.Unlock()
	require.True(t, got == newChannel, "replacement relay channel was deleted by stale start failure")
}

func TestRelayStaleHandshakeDoesNotActivateReplacementOrRemovedSession(t *testing.T) {
	for _, test := range []struct {
		name  string
		stale func(*Daemon, *Session, *mockRelayChannel)
	}{
		{
			name: "replaced",
			stale: func(_ *Daemon, session *Session, _ *mockRelayChannel) {
				session.mu.Lock()
				session.relayChannels["sid-123"] = &mockRelayChannel{}
				session.mu.Unlock()
			},
		},
		{
			name: "removed",
			stale: func(d *Daemon, session *Session, _ *mockRelayChannel) {
				d.mu.Lock()
				delete(d.sessions, session.Code)
				d.mu.Unlock()
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			d, _ := newTestDaemon(t)
			session := &Session{
				Code: "SHARE123", CreatedAt: time.Now(), peers: make(map[string]*peer.Peer),
				relayChannels: make(map[string]relayTransferChannel),
			}
			d.sessions[session.Code] = session
			channel := &mockRelayChannel{}
			d.newRelayChannel = func(relayChannelConfig) (relayTransferChannel, error) { return channel, nil }
			d.handleRelayPrepare(signaling.Message{
				Type: "relay_prepare", SID: "sid-123", Code: session.Code, RelayJWT: "relay.jwt.token",
			})
			test.stale(d, session, channel)

			channel.endpoint(multilane.LaneControl).deliver(`{"type":"transport_hello","version":2}`)

			require.Zero(t, channel.hits(multilane.LaneMedia))
			require.Zero(t, channel.hits(multilane.LaneBulk))
			require.NotContains(t, strings.Join(channel.endpoint(multilane.LaneControl).texts(), "\n"), `"type":"hello"`)
		})
	}
}

// TestHandleRelayPrepare_RelayOnlySessionDoesNotCreateDirectPeer tests that
// relay_prepare in a relay-only session does not create a direct peer connection.
func TestHandleRelayPrepare_RelayOnlySessionDoesNotCreateDirectPeer(t *testing.T) {
	cfg := &config.Config{SignalingURL: "ws://localhost:8080", APIKey: "test-key"}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	sig := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())

	d, err := NewWithSignaling(cfgMgr, st, sig)
	if err != nil {
		t.Fatalf("NewWithSignaling: %v", err)
	}

	// Create a relay-only session
	session := &Session{
		Code:          "RELAYONLY123",
		CreatedAt:     time.Now(),
		RelayOnly:     true,
		peers:         make(map[string]*peer.Peer),
		relayChannels: make(map[string]relayTransferChannel),
	}
	d.sessions["RELAYONLY123"] = session

	// Track if relay channel was created using a channel (handleRelayPrepare runs in goroutine)
	relayChannelCreated := make(chan struct{}, 1)
	d.newRelayChannel = func(cfg relayChannelConfig) (relayTransferChannel, error) {
		relayChannelCreated <- struct{}{}
		return &mockRelayChannel{
			startFn: func(context.Context) error {
				return nil
			},
		}, nil
	}

	// Send relay_prepare for relay-only session
	d.handleSignalingMessage(signaling.Message{
		Type:     "relay_prepare",
		SID:      "sid-relayonly",
		Code:     "RELAYONLY123",
		RelayJWT: "relay.jwt.token",
	})

	// Wait for relay channel to be created (goroutine may take time)
	select {
	case <-relayChannelCreated:
	case <-time.After(2 * time.Second):
		t.Fatal("relay channel should be created for relay-only session")
	}

	// Verify no direct peer was created (peers map should be empty)
	session.mu.Lock()
	peerCount := len(session.peers)
	session.mu.Unlock()

	if peerCount != 0 {
		t.Fatalf("relay-only session should not create direct peers, got %d peers", peerCount)
	}

	require.Eventually(t, func() bool {
		session.mu.Lock()
		defer session.mu.Unlock()
		return session.relayChannels["sid-relayonly"] != nil
	}, 2*time.Second, time.Millisecond, "relay channel should be added to session")
}
