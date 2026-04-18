package handler

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"sharebridge/server/internal/config"
	"sharebridge/server/internal/hub"
	"sharebridge/server/internal/middleware"
	"sharebridge/server/internal/relay"
	"sharebridge/server/migrations"
)

func setupAgentTestApp(t *testing.T) (core.App, func()) {
	testApp, err := tests.NewTestApp(t.TempDir())
	require.NoError(t, err)

	// Bootstrap the app and run system migrations
	err = testApp.Bootstrap()
	require.NoError(t, err)
	err = testApp.RunSystemMigrations()
	require.NoError(t, err)

	// Run our custom migrations
	err = migrations.CreateCollections(testApp)
	require.NoError(t, err)

	cleanup := func() { testApp.Cleanup() }
	return testApp, cleanup
}

func createTestUser(app core.App, email string) (*core.Record, error) {
	usersCol, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		return nil, err
	}

	user := core.NewRecord(usersCol)
	user.SetEmail(email)
	user.SetPassword("testpassword123")
	if err := app.Save(user); err != nil {
		return nil, err
	}
	return user, nil
}

func createTestAPIKey(app core.App, userID string, secret string) (*core.Record, error) {
	apiKeysCol, err := app.FindCollectionByNameOrId("api_keys")
	if err != nil {
		return nil, err
	}

	// Save first to get the record ID, then hash fullKey = record.Id + "." + secret.
	// This matches the production CreateAPIKey handler.
	record := core.NewRecord(apiKeysCol)
	record.Set("account_id", userID)
	record.Set("is_active", true)
	record.Set("label", "test key")
	if err := app.Save(record); err != nil {
		return nil, err
	}

	fullKey := record.Id + "." + secret
	hash, err := bcrypt.GenerateFromPassword([]byte(fullKey), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	record.Set("key_hash", string(hash))
	if err := app.Save(record); err != nil {
		return nil, err
	}
	return record, nil
}

func createTestSession(app core.App, apiKeyID, agentID, code string) (*core.Record, error) {
	sessionsCol, err := app.FindCollectionByNameOrId("sessions")
	if err != nil {
		return nil, err
	}

	record := core.NewRecord(sessionsCol)
	record.Set("code", code)
	record.Set("api_key_id", apiKeyID)
	record.Set("agent_id", agentID)

	if err := app.Save(record); err != nil {
		return nil, err
	}
	return record, nil
}

func TestAgentWS_HelloFlow(t *testing.T) {
	app, cleanup := setupAgentTestApp(t)
	defer cleanup()

	// Create a test user and API key
	user, err := createTestUser(app, "test@example.com")
	require.NoError(t, err)

	secret := "agentsecret"
	apiKey, err := createTestAPIKey(app, user.Id, secret)
	require.NoError(t, err)

	h := hub.New()
	cfg := &config.Config{
		RelayAnnounceAddr: "/dns4/relay.test.local/tcp/443/wss",
		JWTSecret:         []byte("test-secret-do-not-use-in-prod-abcd1234"),
		JWTTTL:            5 * time.Minute,
	}
	rly := newTestRelay(t)

	// Generate a valid agent peer ID and register it in relay registry
	agentPrivKey, _, err := crypto.GenerateEd25519Key(rand.Reader)
	require.NoError(t, err)
	testAgentPeerID, err := peer.IDFromPrivateKey(agentPrivKey)
	require.NoError(t, err)
	rly.Agents().Register(apiKey.Id, testAgentPeerID)

	// Create HTTP test server with the AgentWS handler wrapped in auth middleware
	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, cfg, rly)
	mux := http.NewServeMux()
	mux.Handle("/ws/agent", authMiddleware(http.HandlerFunc(agentHandler)))

	server := httptest.NewServer(mux)
	defer server.Close()

	// Connect with valid API key: record_id.secret
	fullKey := apiKey.Id + "." + secret
	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent?api_key="+fullKey, nil)
	require.NoError(t, err)
	defer conn.CloseNow()

	// Send hello
	err = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","version":"1.0","agent_id":"test-agent-uuid"}`))
	require.NoError(t, err)

	// Expect welcome with relay_multiaddr
	_, data, err := conn.Read(ctx)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"type":"welcome"`)
	assert.Contains(t, string(data), `"relay_multiaddr"`)
	assert.Contains(t, string(data), `"stun_servers"`)
}

func TestAgentWS_InvalidAPIKey(t *testing.T) {
	app, cleanup := setupAgentTestApp(t)
	defer cleanup()

	h := hub.New()
	cfg := config.Load()
	rly := newTestRelay(t)

	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, cfg, rly)
	mux := http.NewServeMux()
	mux.Handle("/ws/agent", authMiddleware(http.HandlerFunc(agentHandler)))

	server := httptest.NewServer(mux)
	defer server.Close()

	// Connect with invalid API key
	ctx := context.Background()
	_, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent?api_key=invalid", nil)

	assert.Error(t, err)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAgentWS_CodeOwnership(t *testing.T) {
	app, cleanup := setupAgentTestApp(t)
	defer cleanup()

	// Create two test users and API keys
	alice, err := createTestUser(app, "alice@example.com")
	require.NoError(t, err)
	mallory, err := createTestUser(app, "mallory@example.com")
	require.NoError(t, err)

	aliceSecret := "alicesecret"
	mallorySecret := "mallorysecret"
	aliceKey, err := createTestAPIKey(app, alice.Id, aliceSecret)
	require.NoError(t, err)
	malloryKey, err := createTestAPIKey(app, mallory.Id, mallorySecret)
	require.NoError(t, err)

	// Alice creates session "CUSTOM01"
	_, err = createTestSession(app, aliceKey.Id, "alice-agent", "CUSTOM01")
	require.NoError(t, err)

	h := hub.New()
	cfg := config.Load()
	rly := newTestRelay(t)

	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, cfg, rly)
	mux := http.NewServeMux()
	mux.Handle("/ws/agent", authMiddleware(http.HandlerFunc(agentHandler)))

	server := httptest.NewServer(mux)
	defer server.Close()

	ctx := context.Background()

	// Mallory tries to claim "CUSTOM01"
	malloryFullKey := malloryKey.Id + "." + mallorySecret
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent?api_key="+malloryFullKey, nil)
	require.NoError(t, err)

	// Send hello
	err = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","version":"1.0","agent_id":"mallory-agent"}`))
	require.NoError(t, err)
	_, _, err = conn.Read(ctx) // welcome
	require.NoError(t, err)

	// Try to register same code
	err = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","share_url":"ocs://evil.com","code":"CUSTOM01"}`))
	require.NoError(t, err)
	_, data, err := conn.Read(ctx)
	require.NoError(t, err)

	assert.Contains(t, string(data), "error")
	assert.Contains(t, string(data), "code already in use")
	conn.CloseNow()
}

func TestAgentWS_authOkIssuesRelayInfoToBrowser(t *testing.T) {
	app, cleanup := setupAgentTestApp(t)
	defer cleanup()

	// Create a test user and API key
	user, err := createTestUser(app, "test@example.com")
	require.NoError(t, err)

	secret := "agentsecret"
	apiKey, err := createTestAPIKey(app, user.Id, secret)
	require.NoError(t, err)

	h := hub.New()
	cfg := &config.Config{
		RelayAnnounceAddr: "/dns4/relay.test.local/tcp/443/wss",
		JWTSecret:         []byte("test-secret-do-not-use-in-prod-abcd1234"),
		JWTTTL:            5 * time.Minute,
	}
	rly := newTestRelay(t)

	// Generate a valid agent peer ID and register it in relay registry
	agentPrivKey, _, err := crypto.GenerateEd25519Key(rand.Reader)
	require.NoError(t, err)
	testAgentPeerID, err := peer.IDFromPrivateKey(agentPrivKey)
	require.NoError(t, err)
	rly.Agents().Register(apiKey.Id, testAgentPeerID)

	// Create a session
	sessionCode := "test1234"
	_, err = createTestSession(app, apiKey.Id, "test-agent-uuid", sessionCode)
	require.NoError(t, err)

	// Generate a valid browser peer ID
	browserPrivKey, _, err := crypto.GenerateEd25519Key(rand.Reader)
	require.NoError(t, err)
	browserPeerID, err := peer.IDFromPrivateKey(browserPrivKey)
	require.NoError(t, err)

	// Create a fake browser WebSocket connection in the hub
	browserConnID := "browser-conn-123"
	browserCtx, cancelBrowser := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelBrowser()
	browserServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		h.RegisterBrowserConn(browserConnID, conn)
		h.RememberBrowserPeerID(browserConnID, browserPeerID.String())
		defer h.UnregisterBrowserConn(browserConnID)

		<-browserCtx.Done()
	}))
	defer browserServer.Close()

	// Connect browser to register it in the hub
	browserWsURL := "ws" + strings.TrimPrefix(browserServer.URL, "http")
	browserConn, _, err := websocket.Dial(browserCtx, browserWsURL, nil)
	require.NoError(t, err)

	// Give the browser connection time to register
	time.Sleep(100 * time.Millisecond)

	// Create HTTP test server with the AgentWS handler
	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, cfg, rly)
	mux := http.NewServeMux()
	mux.Handle("/ws/agent", authMiddleware(http.HandlerFunc(agentHandler)))

	server := httptest.NewServer(mux)
	defer server.Close()

	// Connect agent
	fullKey := apiKey.Id + "." + secret
	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent?api_key="+fullKey, nil)
	require.NoError(t, err)
	defer conn.CloseNow()

	// Send hello
	err = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","version":"1.0","agent_id":"test-agent-uuid"}`))
	require.NoError(t, err)
	_, _, err = conn.Read(ctx) // welcome
	require.NoError(t, err)

	// Send auth_ok
	authOkMsg := map[string]any{
		"type":    "auth_ok",
		"code":    sessionCode,
		"conn_id": browserConnID,
	}
	authOkJSON, _ := json.Marshal(authOkMsg)
	err = conn.Write(ctx, websocket.MessageText, authOkJSON)
	require.NoError(t, err)

	// Browser should receive relay_info with the original conn_id.
	_, data, err := browserConn.Read(browserCtx)
	require.NoError(t, err)
	var msg map[string]any
	require.NoError(t, json.Unmarshal(data, &msg))
	assert.Equal(t, "relay_info", msg["type"])
	assert.NotEmpty(t, msg["relay_multiaddr"])
	assert.NotEmpty(t, msg["agent_peer_id"])
	assert.NotEmpty(t, msg["jwt"])
	assert.Equal(t, browserConnID, msg["conn_id"])
	assert.NotNil(t, msg["relay_allowed"])
	assert.NotNil(t, msg["dcutr_allowed"])

	// Close browser connection to complete the test
	browserConn.CloseNow()
}

func TestRegisterRelayPeerID_registersDecodedPeerID(t *testing.T) {
	reg := relay.NewAgentRegistry()

	agentPrivKey, _, err := crypto.GenerateEd25519Key(rand.Reader)
	require.NoError(t, err)
	agentPeerID, err := peer.IDFromPrivateKey(agentPrivKey)
	require.NoError(t, err)

	err = registerRelayPeerID(reg, "api-key-123", agentPeerID.String())
	require.NoError(t, err)

	got, ok := reg.Lookup("api-key-123")
	require.True(t, ok)
	assert.Equal(t, agentPeerID, got)
}

// newTestRelay creates a relay instance for testing.
func newTestRelay(t *testing.T) *relay.Relay {
	t.Helper()
	rly, err := relay.New(context.Background(), relay.Config{
		ListenAddr: "/ip4/127.0.0.1/tcp/0",
		JWTSecret:  []byte("test-secret-do-not-use-in-prod-abcd1234"),
		JWTTTL:     time.Minute,
	})
	require.NoError(t, err)
	rly.SetCodeResolver(func(code string) (string, bool) {
		return "test-api-key", code != ""
	})
	t.Cleanup(func() { rly.Close() })
	rly.Start()
	return rly
}
