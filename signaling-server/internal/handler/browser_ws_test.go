package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"sharebridge/server/internal/config"
	"sharebridge/server/internal/hub"
	"sharebridge/server/internal/middleware"
	"sharebridge/server/migrations"
)

func setupBrowserTestApp(t *testing.T) (core.App, func()) {
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

func createTestAccountWithQuota(app core.App, email string, quotaGB float64, usageGB float64) (*core.Record, error) {
	usersCol, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	periodEnd := now.AddDate(0, 1, 0) // 1 month from now

	user := core.NewRecord(usersCol)
	user.SetEmail(email)
	user.SetPassword("testpassword123")
	user.Set("relay_quota_gb", quotaGB)
	user.Set("current_period_usage_gb", usageGB)
	user.Set("quota_period_start", now)
	user.Set("quota_period_end", periodEnd)
	if err := app.Save(user); err != nil {
		return nil, err
	}
	return user, nil
}

func createTestAPIKeyForUser(app core.App, userID string, secret string) (*core.Record, error) {
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

func createTestSessionWithAPIKey(app core.App, apiKeyID, agentID, code string, relayOnly bool) (*core.Record, error) {
	sessionsCol, err := app.FindCollectionByNameOrId("sessions")
	if err != nil {
		return nil, err
	}

	record := core.NewRecord(sessionsCol)
	record.Set("code", code)
	record.Set("api_key_id", apiKeyID)
	record.Set("agent_id", agentID)
	record.Set("relay_only", relayOnly)

	if err := app.Save(record); err != nil {
		return nil, err
	}
	return record, nil
}

func TestCheckRelayQuota_Exceeded(t *testing.T) {
	app, cleanup := setupBrowserTestApp(t)
	defer cleanup()

	// Create test user with exceeded quota (5 GB limit, 6 GB used)
	user, err := createTestAccountWithQuota(app, "test@example.com", 5.0, 6.0)
	require.NoError(t, err)

	apiKey, err := createTestAPIKeyForUser(app, user.Id, "testsecret")
	require.NoError(t, err)

	_, err = createTestSessionWithAPIKey(app, apiKey.Id, "test-agent", "EXCEEDED01", false)
	require.NoError(t, err)

	// Get the user record and check quota
	accountRecord, err := app.FindRecordById("users", user.Id)
	require.NoError(t, err)

	exceeded, periodEnd := checkRelayQuota(accountRecord)
	assert.True(t, exceeded, "Quota should be exceeded when usage (6GB) >= limit (5GB)")
	assert.True(t, periodEnd.After(time.Now()), "Period end should be in the future")
}

func TestCheckRelayQuota_NotExceeded(t *testing.T) {
	app, cleanup := setupBrowserTestApp(t)
	defer cleanup()

	// Create test user with available quota (10 GB limit, 5 GB used)
	user, err := createTestAccountWithQuota(app, "test2@example.com", 10.0, 5.0)
	require.NoError(t, err)

	apiKey, err := createTestAPIKeyForUser(app, user.Id, "testsecret2")
	require.NoError(t, err)

	_, err = createTestSessionWithAPIKey(app, apiKey.Id, "test-agent-2", "NOTEXCEED01", false)
	require.NoError(t, err)

	// Get the user record and check quota
	accountRecord, err := app.FindRecordById("users", user.Id)
	require.NoError(t, err)

	exceeded, periodEnd := checkRelayQuota(accountRecord)
	assert.False(t, exceeded, "Quota should NOT be exceeded when usage (5GB) < limit (10GB)")
	assert.True(t, periodEnd.After(time.Now()), "Period end should be in the future")
}

func TestBrowserWS_RelayOnly_QuotaExceeded_Rejected(t *testing.T) {
	app, cleanup := setupBrowserTestApp(t)
	defer cleanup()

	// Create test user with exceeded quota
	user, err := createTestAccountWithQuota(app, "relayonly@example.com", 5.0, 6.0)
	require.NoError(t, err)

	apiKey, err := createTestAPIKeyForUser(app, user.Id, "relayonlysecret")
	require.NoError(t, err)

	// Create relay_only session
	_, err = createTestSessionWithAPIKey(app, apiKey.Id, "relay-agent", "RELAYONLY", true)
	require.NoError(t, err)

	h := hub.New()
	rly := newTestRelay(t)
	cfg := &config.Config{
		RelayAnnounceAddr: "/ip4/127.0.0.1/tcp/9001/ws",
		JWTSecret:         []byte("test-secret-do-not-use-in-prod-abcd1234"),
		JWTTTL:            time.Minute,
	}

	// First connect an agent to the hub
	fullKey := apiKey.Id + ".relayonlysecret"
	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, cfg, rly)
	mux := http.NewServeMux()
	mux.Handle("/ws/agent", authMiddleware(http.HandlerFunc(agentHandler)))
	mux.Handle("/ws/client", http.HandlerFunc(BrowserWS(app, h, cfg, rly)))

	server := httptest.NewServer(mux)
	defer server.Close()

	ctx := context.Background()

	// Connect agent first
	agentConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent?api_key="+fullKey, nil)
	require.NoError(t, err)
	defer agentConn.CloseNow()

	// Send hello from agent
	err = agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","version":"1.0","agent_id":"relay-agent"}`))
	require.NoError(t, err)

	// Read welcome
	_, _, err = agentConn.Read(ctx)
	require.NoError(t, err)

	// Register the share code
	err = agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","share_url":"ocs://test.com","code":"RELAYONLY"}`))
	require.NoError(t, err)
	_, data, err := agentConn.Read(ctx)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"type":"share_registered"`)

	// Now connect browser - should be rejected because relay_only + quota exceeded
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/client?session=RELAYONLY", nil)
	require.NoError(t, err)
	defer conn.CloseNow()

	// Should receive error message about quota exceeded
	_, data, err = conn.Read(ctx)
	require.NoError(t, err)

	var resp map[string]string
	err = json.Unmarshal(data, &resp)
	require.NoError(t, err)
	assert.Equal(t, "error", resp["type"])
	assert.Contains(t, resp["message"], "relay quota exceeded")
}

func TestBrowserWS_KnockForwardedToAgent(t *testing.T) {
	app, cleanup := setupBrowserTestApp(t)
	defer cleanup()

	// Create test user with available quota
	user, err := createTestAccountWithQuota(app, "knock@example.com", 100.0, 5.0)
	require.NoError(t, err)

	apiKey, err := createTestAPIKeyForUser(app, user.Id, "knocksecret")
	require.NoError(t, err)

	_, err = createTestSessionWithAPIKey(app, apiKey.Id, "knock-agent", "KNOCK001", false)
	require.NoError(t, err)

	h := hub.New()
	rly := newTestRelay(t)
	cfg := &config.Config{
		RelayAnnounceAddr: "/ip4/127.0.0.1/tcp/9001/ws",
		JWTSecret:         []byte("test-secret-do-not-use-in-prod-abcd1234"),
		JWTTTL:            time.Minute,
	}

	// First connect an agent to the hub
	fullKey := apiKey.Id + ".knocksecret"
	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, cfg, rly)
	mux := http.NewServeMux()
	mux.Handle("/ws/agent", authMiddleware(http.HandlerFunc(agentHandler)))
	mux.Handle("/ws/client", http.HandlerFunc(BrowserWS(app, h, cfg, rly)))

	server := httptest.NewServer(mux)
	defer server.Close()

	ctx := context.Background()

	// Connect agent first
	agentConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent?api_key="+fullKey, nil)
	require.NoError(t, err)
	defer agentConn.CloseNow()

	// Send hello from agent
	err = agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","version":"1.0","agent_id":"knock-agent"}`))
	require.NoError(t, err)

	// Read welcome
	_, _, err = agentConn.Read(ctx)
	require.NoError(t, err)

	// Register the share code
	err = agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","share_url":"ocs://test.com","code":"KNOCK001"}`))
	require.NoError(t, err)
	_, data, err := agentConn.Read(ctx)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"type":"share_registered"`)

	// Now connect browser
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/client?session=KNOCK001", nil)
	require.NoError(t, err)
	defer conn.CloseNow()

	// Send knock with browser_peer_id
	browserPeerID := "12D3KooWTestBrowserPeerID"
	knockMsg := map[string]string{"type": "knock", "browser_peer_id": browserPeerID}
	knockJSON, _ := json.Marshal(knockMsg)
	err = conn.Write(ctx, websocket.MessageText, knockJSON)
	require.NoError(t, err)

	// Read from agent - should receive the knock with conn_id
	_, data, err = agentConn.Read(ctx)
	require.NoError(t, err)

	var knockReceived map[string]any
	err = json.Unmarshal(data, &knockReceived)
	require.NoError(t, err)
	assert.Equal(t, "knock", knockReceived["type"])
	assert.Equal(t, "KNOCK001", knockReceived["code"])
	assert.NotEmpty(t, knockReceived["conn_id"])

	// Verify browser_peer_id was stored in hub
	connID := knockReceived["conn_id"].(string)
	storedPeerID, ok := h.GetBrowserPeerID(connID)
	assert.True(t, ok)
	assert.Equal(t, browserPeerID, storedPeerID)
}

func TestBrowserWS_JoinForwardedToAgent(t *testing.T) {
	app, cleanup := setupBrowserTestApp(t)
	defer cleanup()

	// Create test user with available quota
	user, err := createTestAccountWithQuota(app, "join@example.com", 100.0, 5.0)
	require.NoError(t, err)

	apiKey, err := createTestAPIKeyForUser(app, user.Id, "joinsecret")
	require.NoError(t, err)

	_, err = createTestSessionWithAPIKey(app, apiKey.Id, "join-agent", "JOIN0001", false)
	require.NoError(t, err)

	h := hub.New()
	rly := newTestRelay(t)
	cfg := &config.Config{
		RelayAnnounceAddr: "/ip4/127.0.0.1/tcp/9001/ws",
		JWTSecret:         []byte("test-secret-do-not-use-in-prod-abcd1234"),
		JWTTTL:            time.Minute,
	}

	// First connect an agent to the hub
	fullKey := apiKey.Id + ".joinsecret"
	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, cfg, rly)
	mux := http.NewServeMux()
	mux.Handle("/ws/agent", authMiddleware(http.HandlerFunc(agentHandler)))
	mux.Handle("/ws/client", http.HandlerFunc(BrowserWS(app, h, cfg, rly)))

	server := httptest.NewServer(mux)
	defer server.Close()

	ctx := context.Background()

	// Connect agent first
	agentConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent?api_key="+fullKey, nil)
	require.NoError(t, err)
	defer agentConn.CloseNow()

	// Send hello from agent
	err = agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","version":"1.0","agent_id":"join-agent"}`))
	require.NoError(t, err)

	// Read welcome
	_, _, err = agentConn.Read(ctx)
	require.NoError(t, err)

	// Register the share code
	err = agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","share_url":"ocs://test.com","code":"JOIN0001"}`))
	require.NoError(t, err)
	_, data, err := agentConn.Read(ctx)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"type":"share_registered"`)

	// Now connect browser
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/client?session=JOIN0001", nil)
	require.NoError(t, err)
	defer conn.CloseNow()

	// Send join with browser_peer_id and hmac
	browserPeerID := "12D3KooWTestBrowserJoinPeerID"
	hmac := "test-hmac-value"
	joinMsg := map[string]string{"type": "join", "browser_peer_id": browserPeerID, "hmac": hmac}
	joinJSON, _ := json.Marshal(joinMsg)
	err = conn.Write(ctx, websocket.MessageText, joinJSON)
	require.NoError(t, err)

	// Read from agent - should receive the join with conn_id and hmac
	_, data, err = agentConn.Read(ctx)
	require.NoError(t, err)

	var joinReceived map[string]any
	err = json.Unmarshal(data, &joinReceived)
	require.NoError(t, err)
	assert.Equal(t, "join", joinReceived["type"])
	assert.Equal(t, "JOIN0001", joinReceived["code"])
	assert.NotEmpty(t, joinReceived["conn_id"])
	assert.Equal(t, hmac, joinReceived["hmac"])

	// Verify browser_peer_id was stored in hub
	connID := joinReceived["conn_id"].(string)
	storedPeerID, ok := h.GetBrowserPeerID(connID)
	assert.True(t, ok)
	assert.Equal(t, browserPeerID, storedPeerID)
}