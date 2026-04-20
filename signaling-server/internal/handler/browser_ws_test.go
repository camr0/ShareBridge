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
	"sharebridge/server/internal/relay"
	"sharebridge/server/migrations"
)

func extractJSONField(t *testing.T, data []byte, key string) string {
	t.Helper()
	var payload map[string]any
	require.NoError(t, json.Unmarshal(data, &payload))
	value, ok := payload[key].(string)
	require.True(t, ok, "expected string field %q in payload %s", key, string(data))
	return value
}

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

func createTestSessionWithAPIKey(app core.App, apiKeyID, agentID, code string) (*core.Record, error) {
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

func TestCheckRelayQuota_Exceeded(t *testing.T) {
	app, cleanup := setupBrowserTestApp(t)
	defer cleanup()

	// Create test user with exceeded quota (5 GB limit, 6 GB used)
	user, err := createTestAccountWithQuota(app, "test@example.com", 5.0, 6.0)
	require.NoError(t, err)

	apiKey, err := createTestAPIKeyForUser(app, user.Id, "testsecret")
	require.NoError(t, err)

	_, err = createTestSessionWithAPIKey(app, apiKey.Id, "test-agent", "EXCEEDED01")
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

	_, err = createTestSessionWithAPIKey(app, apiKey.Id, "test-agent-2", "NOTEXCEED01")
	require.NoError(t, err)

	// Get the user record and check quota
	accountRecord, err := app.FindRecordById("users", user.Id)
	require.NoError(t, err)

	exceeded, periodEnd := checkRelayQuota(accountRecord)
	assert.False(t, exceeded, "Quota should NOT be exceeded when usage (5GB) < limit (10GB)")
	assert.True(t, periodEnd.After(time.Now()), "Period end should be in the future")
}

func TestBrowserWS_QuotaExceeded_SendsSTUNOnly(t *testing.T) {
	app, cleanup := setupBrowserTestApp(t)
	defer cleanup()

	// Create test user with exceeded quota
	user, err := createTestAccountWithQuota(app, "quotauser@example.com", 5.0, 6.0)
	require.NoError(t, err)

	apiKey, err := createTestAPIKeyForUser(app, user.Id, "quotasecret")
	require.NoError(t, err)

	_, err = createTestSessionWithAPIKey(app, apiKey.Id, "quota-agent", "QUOTA001")
	require.NoError(t, err)

	h := hub.New()
	cfg := config.Load()
	reg := relay.NewRegistry(2 * time.Second)

	// First connect an agent to the hub
	fullKey := apiKey.Id + ".quotasecret"
	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, reg, cfg)
	mux := http.NewServeMux()
	mux.Handle("/ws/agent", authMiddleware(http.HandlerFunc(agentHandler)))
	mux.Handle("/ws/client", http.HandlerFunc(BrowserWS(app, h, cfg)))

	server := httptest.NewServer(mux)
	defer server.Close()

	ctx := context.Background()

	// Connect agent first
	agentConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent?api_key="+fullKey, nil)
	require.NoError(t, err)
	defer agentConn.CloseNow()

	// Send hello from agent
	err = agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","version":"1.0","agent_id":"quota-agent"}`))
	require.NoError(t, err)

	// Read welcome
	_, _, err = agentConn.Read(ctx)
	require.NoError(t, err)

	// Register the share code
	err = agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","share_url":"ocs://test.com","code":"QUOTA001"}`))
	require.NoError(t, err)
	_, data, err := agentConn.Read(ctx)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"type":"share_registered"`)

	// Now connect browser
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/client?session=QUOTA001", nil)
	require.NoError(t, err)
	defer conn.CloseNow()

	// First message should be ice_config
	_, data, err = conn.Read(ctx)
	require.NoError(t, err)

	// Should receive ice_config with only STUN servers (no TURN)
	assert.Contains(t, string(data), `"type":"ice_config"`)
	assert.Contains(t, string(data), `"ice_servers"`)
	assert.Contains(t, string(data), `"relay_quota_exceeded"`)
}

func TestBrowserWS_QuotaNotExceeded_SendsFullICE(t *testing.T) {
	app, cleanup := setupBrowserTestApp(t)
	defer cleanup()

	// Create test user with available quota
	user, err := createTestAccountWithQuota(app, "normaluser@example.com", 100.0, 5.0)
	require.NoError(t, err)

	apiKey, err := createTestAPIKeyForUser(app, user.Id, "normalsecret")
	require.NoError(t, err)

	_, err = createTestSessionWithAPIKey(app, apiKey.Id, "normal-agent", "NORMAL001")
	require.NoError(t, err)

	h := hub.New()
	cfg := config.Load()
	reg := relay.NewRegistry(2 * time.Second)
	// Enable TURN for this test
	cfg.TurnSecret = "test-turn-secret-for-testing-only"
	cfg.TurnHost = "turn.example.com"

	// First connect an agent to the hub
	fullKey := apiKey.Id + ".normalsecret"
	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, reg, cfg)
	mux := http.NewServeMux()
	mux.Handle("/ws/agent", authMiddleware(http.HandlerFunc(agentHandler)))
	mux.Handle("/ws/client", http.HandlerFunc(BrowserWS(app, h, cfg)))

	server := httptest.NewServer(mux)
	defer server.Close()

	ctx := context.Background()

	// Connect agent first
	agentConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent?api_key="+fullKey, nil)
	require.NoError(t, err)
	defer agentConn.CloseNow()

	// Send hello from agent
	err = agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","version":"1.0","agent_id":"normal-agent"}`))
	require.NoError(t, err)

	// Read welcome
	_, _, err = agentConn.Read(ctx)
	require.NoError(t, err)

	// Register the share code
	err = agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","share_url":"ocs://test.com","code":"NORMAL001"}`))
	require.NoError(t, err)
	_, data, err := agentConn.Read(ctx)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"type":"share_registered"`)

	// Now connect browser
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/client?session=NORMAL001", nil)
	require.NoError(t, err)
	defer conn.CloseNow()

	// First message should be ice_config
	_, data, err = conn.Read(ctx)
	require.NoError(t, err)

	// Should receive ice_config with TURN servers
	assert.Contains(t, string(data), `"type":"ice_config"`)
	assert.Contains(t, string(data), `"ice_servers"`)
	// Should NOT have relay_quota_exceeded
	assert.NotContains(t, string(data), `"relay_quota_exceeded"`)
	// Should have TURN credentials (indicated by username field in ice_servers)
	assert.Contains(t, string(data), `"username"`)
}

func TestBrowserWS_DirectFlowStillSendsICEConfigFirst(t *testing.T) {
	app, cleanup := setupBrowserTestApp(t)
	defer cleanup()

	user, err := createTestAccountWithQuota(app, "direct@example.com", 50.0, 0.0)
	require.NoError(t, err)
	apiKey, err := createTestAPIKeyForUser(app, user.Id, "directsecret")
	require.NoError(t, err)

	_, err = createTestSessionWithAPIKey(app, apiKey.Id, "agent-direct", "DIRECT001")
	require.NoError(t, err)

	h := hub.New()
	cfg := config.Load()
	reg := relay.NewRegistry(2 * time.Second)

	fullKey := apiKey.Id + ".directsecret"
	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, reg, cfg)
	mux := http.NewServeMux()
	mux.Handle("/ws/agent", authMiddleware(http.HandlerFunc(agentHandler)))
	mux.Handle("/ws/client", http.HandlerFunc(BrowserWS(app, h, cfg)))

	server := httptest.NewServer(mux)
	defer server.Close()

	ctx := context.Background()

	agentConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent?api_key="+fullKey, nil)
	require.NoError(t, err)
	defer agentConn.CloseNow()
	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","agent_id":"agent-direct"}`)))
	_, _, err = agentConn.Read(ctx)
	require.NoError(t, err)
	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","code":"DIRECT001"}`)))
	_, _, err = agentConn.Read(ctx)
	require.NoError(t, err)

	browserConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/client?session=DIRECT001", nil)
	require.NoError(t, err)
	defer browserConn.CloseNow()

	_, firstMsg, err := browserConn.Read(ctx)
	require.NoError(t, err)
	assert.Contains(t, string(firstMsg), `"type":"ice_config"`)
}

func TestBrowserWS_RelayOnlyAnnotatesIceConfig(t *testing.T) {
	app, cleanup := setupBrowserTestApp(t)
	defer cleanup()

	user, err := createTestAccountWithQuota(app, "relayonly@example.com", 50.0, 0.0)
	require.NoError(t, err)
	apiKey, err := createTestAPIKeyForUser(app, user.Id, "relayonlysecret")
	require.NoError(t, err)

	session, err := createTestSessionWithAPIKey(app, apiKey.Id, "agent-relayonly", "RELAYONLY1")
	require.NoError(t, err)
	session.Set("relay_only", true)
	require.NoError(t, app.Save(session))

	h := hub.New()
	cfg := config.Load()
	reg := relay.NewRegistry(2 * time.Second)

	fullKey := apiKey.Id + ".relayonlysecret"
	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, reg, cfg)
	mux := http.NewServeMux()
	mux.Handle("/ws/agent", authMiddleware(http.HandlerFunc(agentHandler)))
	mux.Handle("/ws/client", http.HandlerFunc(BrowserWS(app, h, cfg)))

	server := httptest.NewServer(mux)
	defer server.Close()

	ctx := context.Background()

	agentConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent?api_key="+fullKey, nil)
	require.NoError(t, err)
	defer agentConn.CloseNow()
	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","agent_id":"agent-relayonly"}`)))
	_, _, err = agentConn.Read(ctx)
	require.NoError(t, err)
	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","code":"RELAYONLY1"}`)))
	_, _, err = agentConn.Read(ctx)
	require.NoError(t, err)

	browserConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/client?session=RELAYONLY1", nil)
	require.NoError(t, err)
	defer browserConn.CloseNow()

	_, firstMsg, err := browserConn.Read(ctx)
	require.NoError(t, err)
	assert.Contains(t, string(firstMsg), `"type":"ice_config"`)
	assert.Contains(t, string(firstMsg), `"relay_only":true`)

	_, knockMsg, err := agentConn.Read(ctx)
	require.NoError(t, err)
	assert.Contains(t, string(knockMsg), `"type":"knock"`)
	assert.Contains(t, string(knockMsg), `"code":"RELAYONLY1"`)

	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"nonce","conn_id":"`+extractJSONField(t, knockMsg, "conn_id")+`","value":"relaynonce","has_password":false}`)))

	_, nonceMsg, err := browserConn.Read(ctx)
	require.NoError(t, err)
	assert.Contains(t, string(nonceMsg), `"type":"nonce"`)
	assert.Contains(t, string(nonceMsg), `"value":"relaynonce"`)
}

func TestBrowserWS_KnockSurvivesRequestContextCancellation(t *testing.T) {
	app, cleanup := setupBrowserTestApp(t)
	defer cleanup()

	user, err := createTestAccountWithQuota(app, "ctxcancel@example.com", 50.0, 0.0)
	require.NoError(t, err)
	apiKey, err := createTestAPIKeyForUser(app, user.Id, "ctxcancelsecret")
	require.NoError(t, err)

	_, err = createTestSessionWithAPIKey(app, apiKey.Id, "agent-ctxcancel", "CTXCANCEL1")
	require.NoError(t, err)

	h := hub.New()
	cfg := config.Load()
	reg := relay.NewRegistry(2 * time.Second)

	fullKey := apiKey.Id + ".ctxcancelsecret"
	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, reg, cfg)
	browserHandler := BrowserWS(app, h, cfg)
	mux := http.NewServeMux()
	mux.Handle("/ws/agent", authMiddleware(http.HandlerFunc(agentHandler)))
	mux.Handle("/ws/client", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		r = r.WithContext(ctx)
		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()
		browserHandler(w, r)
	}))

	server := httptest.NewServer(mux)
	defer server.Close()

	ctx := context.Background()

	agentConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent?api_key="+fullKey, nil)
	require.NoError(t, err)
	defer agentConn.CloseNow()
	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","agent_id":"agent-ctxcancel"}`)))
	_, _, err = agentConn.Read(ctx)
	require.NoError(t, err)
	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","code":"CTXCANCEL1"}`)))
	_, _, err = agentConn.Read(ctx)
	require.NoError(t, err)

	browserConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/client?session=CTXCANCEL1", nil)
	require.NoError(t, err)
	defer browserConn.CloseNow()

	_, _, err = browserConn.Read(ctx) // ice_config
	require.NoError(t, err)

	time.Sleep(50 * time.Millisecond)
	require.NoError(t, browserConn.Write(ctx, websocket.MessageText, []byte(`{"type":"knock"}`)))

	readCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, knockMsg, err := agentConn.Read(readCtx)
	require.NoError(t, err)
	assert.Contains(t, string(knockMsg), `"type":"knock"`)
	assert.Contains(t, string(knockMsg), `"code":"CTXCANCEL1"`)
}
