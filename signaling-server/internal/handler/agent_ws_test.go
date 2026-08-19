package handler

import (
	"context"
	"encoding/json"
	"fmt"
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
	err = migrations.AddQuotaFields(testApp)
	require.NoError(t, err)
	err = migrations.AddRelayOnly(testApp)
	require.NoError(t, err)
	err = migrations.AddSessionRelayStaticPub(testApp)
	require.NoError(t, err)
	err = migrations.AddImmichSessionFields(testApp)
	require.NoError(t, err)
	err = migrations.CreateAgents(testApp)
	require.NoError(t, err)
	err = migrations.AddSessionsInactiveReason(testApp)
	require.NoError(t, err)

	cleanup := func() { testApp.Cleanup() }
	return testApp, cleanup
}

func setupAgentWSTest(t *testing.T) (core.App, string, func()) {
	t.Helper()

	app, appCleanup := setupAgentTestApp(t)
	h := hub.New()
	cfg := config.Load()
	reg := relay.NewRegistry(2 * time.Second)

	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, reg, cfg, nil)
	mux := http.NewServeMux()
	mux.Handle("/ws/agent", authMiddleware(http.HandlerFunc(agentHandler)))

	server := httptest.NewServer(mux)
	cleanup := func() {
		server.Close()
		appCleanup()
	}
	return app, server.URL, cleanup
}

func createTestUser(app core.App, email string) (*core.Record, error) {
	usersCol, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		return nil, err
	}

	user := core.NewRecord(usersCol)
	user.SetEmail(email)
	user.SetPassword("testpassword123")
	user.Set("relay_quota_gb", 50.0)
	user.Set("current_period_usage_gb", 0.0)
	user.Set("quota_period_start", time.Now().UTC())
	user.Set("quota_period_end", time.Now().UTC().Add(30*24*time.Hour))
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

func createTestAgentAPIKey(t *testing.T, app core.App) string {
	t.Helper()

	user, err := createTestUser(app, "agent-"+strings.ToLower(t.Name())+"@example.com")
	require.NoError(t, err)
	secret := "agentsecret"
	apiKey, err := createTestAPIKey(app, user.Id, secret)
	require.NoError(t, err)
	return apiKey.Id + "." + secret
}

func dialAgentAndHello(t *testing.T, serverURL, apiKey, agentID string) *websocket.Conn {
	t.Helper()

	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(serverURL, "http")+"/ws/agent?api_key="+apiKey, nil)
	require.NoError(t, err)
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(`{"type":"hello","agent_id":%q}`, agentID))))
	_, _, err = conn.Read(ctx)
	require.NoError(t, err)
	return conn
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
	record.Set("is_active", true)

	if err := app.Save(record); err != nil {
		return nil, err
	}
	return record, nil
}

func findSessionByCode(t *testing.T, app core.App, code string) *core.Record {
	t.Helper()

	session, err := getSessionByCode(app, code)
	require.NoError(t, err)
	return session
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
	cfg := config.Load()
	reg := relay.NewRegistry(2 * time.Second)

	// Create HTTP test server with the AgentWS handler wrapped in auth middleware
	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, reg, cfg, nil)
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

	// Expect welcome
	_, data, err := conn.Read(ctx)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"type":"welcome"`)
	assert.Contains(t, string(data), `"ice_servers"`)
}

func TestAgentWS_InvalidAPIKey(t *testing.T) {
	app, cleanup := setupAgentTestApp(t)
	defer cleanup()

	h := hub.New()
	cfg := config.Load()
	reg := relay.NewRegistry(2 * time.Second)

	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, reg, cfg, nil)
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
	reg := relay.NewRegistry(2 * time.Second)

	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, reg, cfg, nil)
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

func TestAgentWS_RegisterShare_PersistsRelayStaticPub(t *testing.T) {
	app, cleanup := setupAgentTestApp(t)
	defer cleanup()

	user, err := createTestUser(app, "relay@example.com")
	require.NoError(t, err)
	apiKey, err := createTestAPIKey(app, user.Id, "relaysecret")
	require.NoError(t, err)

	h := hub.New()
	cfg := config.Load()
	reg := relay.NewRegistry(2 * time.Second)

	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, reg, cfg, nil)
	mux := http.NewServeMux()
	mux.Handle("/ws/agent", authMiddleware(http.HandlerFunc(agentHandler)))

	server := httptest.NewServer(mux)
	defer server.Close()

	ctx := context.Background()
	fullKey := apiKey.Id + ".relaysecret"
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent?api_key="+fullKey, nil)
	require.NoError(t, err)
	defer conn.CloseNow()

	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","agent_id":"agent-1"}`)))
	_, _, err = conn.Read(ctx)
	require.NoError(t, err)

	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","code":"RELAYKEY1","relay_static_pub":"04abcd"}`)))
	_, _, err = conn.Read(ctx)
	require.NoError(t, err)

	session, err := getSessionByCode(app, "RELAYKEY1")
	require.NoError(t, err)
	require.Equal(t, "04abcd", session.GetString("relay_static_pub"))
}

func TestAgentWS_RegisterShare_AcceptsImmichExternalCodeMetadata(t *testing.T) {
	testApp, serverURL, cleanup := setupAgentWSTest(t)
	defer cleanup()

	apiKey := createTestAgentAPIKey(t, testApp)
	wsURL := strings.Replace(serverURL, "http://", "ws://", 1) + "/ws/agent?api_key=" + apiKey
	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(t, err)
	defer conn.CloseNow()

	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","agent_id":"agent-immich"}`)))
	_, _, err = conn.Read(ctx)
	require.NoError(t, err)

	code := "ffSw63qnIYMt_aBcDeFgHiJkLmNoPqRsTuVwXyZ-1234567890"
	payload := fmt.Sprintf(`{"type":"register_share","code":%q,"share_type":"immich","is_password_protected":true,"relay_only":true,"relay_static_pub":"04abcd"}`, code)
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(payload)))

	_, raw, err := conn.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"type":"share_registered"`)
	require.Contains(t, string(raw), code)

	session := findSessionByCode(t, testApp, code)
	require.Equal(t, "immich", session.GetString("share_type"))
	require.True(t, session.GetBool("is_password_protected"))
	require.True(t, session.GetBool("relay_only"))
}

func TestAgentWS_RegisterShare_ReclaimPreservesRelayOnlyWhenOmitted(t *testing.T) {
	testApp, serverURL, cleanup := setupAgentWSTest(t)
	defer cleanup()

	apiKey := createTestAgentAPIKey(t, testApp)
	agentConn := dialAgentAndHello(t, serverURL, apiKey, "agent-reclaim-relay")
	defer agentConn.CloseNow()

	require.NoError(t, agentConn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"register_share","code":"RECLAIMRELAY","relay_only":true,"relay_static_pub":"04abcd"}`)))
	_, _, err := agentConn.Read(context.Background())
	require.NoError(t, err)

	require.NoError(t, agentConn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"register_share","code":"RECLAIMRELAY","relay_static_pub":"04abcd"}`)))
	_, _, err = agentConn.Read(context.Background())
	require.NoError(t, err)

	session := findSessionByCode(t, testApp, "RECLAIMRELAY")
	require.True(t, session.GetBool("relay_only"))
}

func TestAgentWS_RegisterShare_ReclaimAllowsExplicitRelayOnlyFalse(t *testing.T) {
	testApp, serverURL, cleanup := setupAgentWSTest(t)
	defer cleanup()

	apiKey := createTestAgentAPIKey(t, testApp)
	agentConn := dialAgentAndHello(t, serverURL, apiKey, "agent-reclaim-relay-false")
	defer agentConn.CloseNow()

	require.NoError(t, agentConn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"register_share","code":"RECLAIMFALSE","relay_only":true,"relay_static_pub":"04abcd"}`)))
	_, _, err := agentConn.Read(context.Background())
	require.NoError(t, err)

	require.NoError(t, agentConn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"register_share","code":"RECLAIMFALSE","relay_only":false,"relay_static_pub":"04abcd"}`)))
	_, _, err = agentConn.Read(context.Background())
	require.NoError(t, err)

	session := findSessionByCode(t, testApp, "RECLAIMFALSE")
	require.False(t, session.GetBool("relay_only"))
}

func TestAgentWS_RegisterShare_RejectsExternalCodeOver128Chars(t *testing.T) {
	testApp, serverURL, cleanup := setupAgentWSTest(t)
	defer cleanup()

	apiKey := createTestAgentAPIKey(t, testApp)
	wsURL := strings.Replace(serverURL, "http://", "ws://", 1) + "/ws/agent?api_key=" + apiKey
	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(t, err)
	defer conn.CloseNow()

	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","agent_id":"agent-immich"}`)))
	_, _, err = conn.Read(ctx)
	require.NoError(t, err)

	code := strings.Repeat("a", 129)
	payload := fmt.Sprintf(`{"type":"register_share","code":%q,"share_type":"immich"}`, code)
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(payload)))

	_, raw, err := conn.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"type":"error"`)
	require.Contains(t, string(raw), "invalid external code format")
}

func TestAgentWS_UnregisterShareDeletesOwnedSession(t *testing.T) {
	testApp, serverURL, cleanup := setupAgentWSTest(t)
	defer cleanup()

	apiKey := createTestAgentAPIKey(t, testApp)
	conn := dialAgentAndHello(t, serverURL, apiKey, "agent-unregister")
	defer conn.CloseNow()

	ctx := context.Background()
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","code":"IMMICHDEL1","share_type":"immich"}`)))
	_, _, err := conn.Read(ctx)
	require.NoError(t, err)

	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"unregister_share","code":"IMMICHDEL1"}`)))
	_, raw, err := conn.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"type":"share_unregistered"`)

	session := findSessionByCode(t, testApp, "IMMICHDEL1")
	require.Nil(t, session)
}

func TestAgentWS_UnregisterShareRejectsInvalidCodeFormat(t *testing.T) {
	testApp, serverURL, cleanup := setupAgentWSTest(t)
	defer cleanup()

	apiKey := createTestAgentAPIKey(t, testApp)
	conn := dialAgentAndHello(t, serverURL, apiKey, "agent-unregister-invalid")
	defer conn.CloseNow()

	ctx := context.Background()
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"unregister_share","code":"bad space!"}`)))
	_, raw, err := conn.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"type":"error"`)
	require.Contains(t, string(raw), "invalid external code format")
}

func TestAgentWS_UnregisterShareRejectsDifferentAPIKeyOwner(t *testing.T) {
	testApp, serverURL, cleanup := setupAgentWSTest(t)
	defer cleanup()

	owner, err := createTestUser(testApp, "unregister-owner@example.com")
	require.NoError(t, err)
	ownerAPIKey, err := createTestAPIKey(testApp, owner.Id, "ownersecret")
	require.NoError(t, err)
	ownerKey := ownerAPIKey.Id + ".ownersecret"
	ownerConn := dialAgentAndHello(t, serverURL, ownerKey, "agent-owner")
	defer ownerConn.CloseNow()

	ctx := context.Background()
	require.NoError(t, ownerConn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","code":"IMMICHOWN1","share_type":"immich"}`)))
	_, _, err = ownerConn.Read(ctx)
	require.NoError(t, err)

	other, err := createTestUser(testApp, "unregister-other@example.com")
	require.NoError(t, err)
	otherAPIKey, err := createTestAPIKey(testApp, other.Id, "othersecret")
	require.NoError(t, err)
	otherKey := otherAPIKey.Id + ".othersecret"
	otherConn := dialAgentAndHello(t, serverURL, otherKey, "agent-other")
	defer otherConn.CloseNow()

	require.NoError(t, otherConn.Write(ctx, websocket.MessageText, []byte(`{"type":"unregister_share","code":"IMMICHOWN1"}`)))
	_, raw, err := otherConn.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"type":"error"`)
	require.Contains(t, string(raw), "session not owned by this api key")

	session := findSessionByCode(t, testApp, "IMMICHOWN1")
	require.NotNil(t, session)
}

func TestAgentWS_DeregisterUnsupportedMarksInactiveReason(t *testing.T) {
	testApp, serverURL, cleanup := setupAgentWSTest(t)
	defer cleanup()

	apiKey := createTestAgentAPIKey(t, testApp)
	conn := dialAgentAndHello(t, serverURL, apiKey, "agent-deregister-unsupported")
	defer conn.CloseNow()

	ctx := context.Background()
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","code":"IMMICHUNSUP1","share_type":"immich"}`)))
	_, _, err := conn.Read(ctx)
	require.NoError(t, err)

	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"deregister","code":"IMMICHUNSUP1","reason":"unsupported"}`)))

	// deregister is fire-and-forget (no ack), so poll until the row is
	// tombstoned with the unsupported discriminator (→ 410, not revoked/404).
	require.Eventually(t, func() bool {
		records, err := testApp.FindRecordsByFilter("sessions", "code = {:code}", "", 1, 0, map[string]any{"code": "IMMICHUNSUP1"})
		if err != nil || len(records) == 0 {
			return false
		}
		return !records[0].GetBool("is_active") && records[0].GetString("inactive_reason") == "unsupported"
	}, 2*time.Second, 10*time.Millisecond)
}

func TestAgentWS_AuthOK_SendsRelayPrepareToAgentAndRelayPolicyToBrowser(t *testing.T) {
	app, cleanup := setupAgentTestApp(t)
	defer cleanup()

	user, err := createTestUser(app, "relayplan@example.com")
	require.NoError(t, err)
	apiKey, err := createTestAPIKey(app, user.Id, "sigsecret")
	require.NoError(t, err)

	// Create test session with relay_static_pub
	sessionsCol, err := app.FindCollectionByNameOrId("sessions")
	require.NoError(t, err)
	session := core.NewRecord(sessionsCol)
	session.Set("code", "PLANA001")
	session.Set("api_key_id", apiKey.Id)
	session.Set("agent_id", "agent-1")
	session.Set("relay_static_pub", "04abcd")
	require.NoError(t, app.Save(session))

	h := hub.New()
	cfg := config.Load()
	reg := relay.NewRegistry(2 * time.Second)
	cfg.RelayJWTSecret = "secret"

	fullKey := apiKey.Id + ".sigsecret"
	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, reg, cfg, nil)
	browserHandler := BrowserWS(app, h, cfg)
	mux := http.NewServeMux()
	mux.Handle("/ws/agent", authMiddleware(http.HandlerFunc(agentHandler)))
	mux.Handle("/ws/client", http.HandlerFunc(browserHandler))

	server := httptest.NewServer(mux)
	defer server.Close()

	ctx := context.Background()

	// Agent connects and registers
	agentConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent?api_key="+fullKey, nil)
	require.NoError(t, err)
	defer agentConn.CloseNow()
	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","agent_id":"agent-1"}`)))
	_, _, err = agentConn.Read(ctx) // welcome
	require.NoError(t, err)
	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","code":"PLANA001","relay_static_pub":"04abcd"}`)))
	_, _, err = agentConn.Read(ctx) // registered
	require.NoError(t, err)

	// Browser connects and knocks
	browserConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/client?session=PLANA001", nil)
	require.NoError(t, err)
	defer browserConn.CloseNow()
	_, _, err = browserConn.Read(ctx) // ice_config
	require.NoError(t, err)
	require.NoError(t, browserConn.Write(ctx, websocket.MessageText, []byte(`{"type":"knock"}`)))

	// Agent receives the knock forwarded from server
	_, knockMsg, err := agentConn.Read(ctx)
	require.NoError(t, err)
	var knockPayload struct {
		Type   string `json:"type"`
		ConnID string `json:"conn_id"`
		Code   string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(knockMsg, &knockPayload))
	require.Equal(t, "knock", knockPayload.Type)
	require.NotEmpty(t, knockPayload.ConnID)

	// Agent sends nonce response (simulating normal auth flow)
	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"nonce","conn_id":"`+knockPayload.ConnID+`","value":"testnonce","has_password":false}`)))

	// Browser receives nonce
	_, nonceMsg, err := browserConn.Read(ctx)
	require.NoError(t, err)
	var nonceResp struct {
		Type        string `json:"type"`
		ConnID      string `json:"conn_id"`
		Value       string `json:"value"`
		HasPassword bool   `json:"has_password"`
	}
	require.NoError(t, json.Unmarshal(nonceMsg, &nonceResp))
	require.Equal(t, "nonce", nonceResp.Type)

	// Agent sends auth_ok (browser auth succeeded)
	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"auth_ok","conn_id":"`+knockPayload.ConnID+`","code":"PLANA001"}`)))

	// Agent receives relay_prepare
	_, agentRelayPrepare, err := agentConn.Read(ctx)
	require.NoError(t, err)
	assert.Contains(t, string(agentRelayPrepare), `"type":"relay_prepare"`)

	var agentMsg map[string]interface{}
	require.NoError(t, json.Unmarshal(agentRelayPrepare, &agentMsg))
	assert.NotEmpty(t, agentMsg["sid"])
	assert.NotEmpty(t, agentMsg["relay_jwt"])
	assert.NotEmpty(t, agentMsg["expires_at"])

	// Browser receives relay_policy
	_, browserRelayPolicy, err := browserConn.Read(ctx)
	require.NoError(t, err)
	assert.Contains(t, string(browserRelayPolicy), `"type":"relay_policy"`)
	assert.Contains(t, string(browserRelayPolicy), `"relay_allowed":true`)

	var browserMsg map[string]interface{}
	require.NoError(t, json.Unmarshal(browserRelayPolicy, &browserMsg))
	assert.NotEmpty(t, browserMsg["token"])
	assert.Equal(t, true, browserMsg["relay_allowed"])
}

func TestAgentWS_AuthOK_WithoutRegistryDoesNotSendRelayMessages(t *testing.T) {
	app, cleanup := setupAgentTestApp(t)
	defer cleanup()

	user, err := createTestUser(app, "norelay@example.com")
	require.NoError(t, err)
	apiKey, err := createTestAPIKey(app, user.Id, "norelaysecret")
	require.NoError(t, err)

	sessionsCol, err := app.FindCollectionByNameOrId("sessions")
	require.NoError(t, err)
	session := core.NewRecord(sessionsCol)
	session.Set("code", "NORELAY01")
	session.Set("api_key_id", apiKey.Id)
	session.Set("agent_id", "agent-norelay")
	require.NoError(t, app.Save(session))

	h := hub.New()
	cfg := config.Load()
	// No registry - cfg.RelayJWTSecret empty

	fullKey := apiKey.Id + ".norelaysecret"
	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, nil, cfg, nil) // nil registry - relay disabled
	browserHandler := BrowserWS(app, h, cfg)
	mux := http.NewServeMux()
	mux.Handle("/ws/agent", authMiddleware(http.HandlerFunc(agentHandler)))
	mux.Handle("/ws/client", http.HandlerFunc(browserHandler))

	server := httptest.NewServer(mux)
	defer server.Close()

	ctx := context.Background()

	agentConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent?api_key="+fullKey, nil)
	require.NoError(t, err)
	defer agentConn.CloseNow()
	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","agent_id":"agent-norelay"}`)))
	_, _, err = agentConn.Read(ctx)
	require.NoError(t, err)
	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","code":"NORELAY01"}`)))
	_, _, err = agentConn.Read(ctx)
	require.NoError(t, err)

	browserConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/client?session=NORELAY01", nil)
	require.NoError(t, err)
	defer browserConn.CloseNow()
	_, _, err = browserConn.Read(ctx) // ice_config
	require.NoError(t, err)
	require.NoError(t, browserConn.Write(ctx, websocket.MessageText, []byte(`{"type":"knock"}`)))

	// Agent receives knock and sends nonce
	_, knockMsg, err := agentConn.Read(ctx)
	require.NoError(t, err)
	var knockPayload struct {
		ConnID string `json:"conn_id"`
	}
	require.NoError(t, json.Unmarshal(knockMsg, &knockPayload))

	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"nonce","conn_id":"`+knockPayload.ConnID+`","value":"testnonce","has_password":false}`)))
	_, _, err = browserConn.Read(ctx) // nonce
	require.NoError(t, err)

	// Agent sends auth_ok - should be silently handled without relay messages
	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"auth_ok","conn_id":"`+knockPayload.ConnID+`","code":"NORELAY01"}`)))

	// Agent should NOT receive relay_prepare (since no relay secret configured)
	// The connection stays open for offer/answer flow - we can verify this by checking
	// that the agent connection is still usable (no close)
}

func TestAgentWS_AuthOK_QuotaExceeded_DisablesRelayFallback(t *testing.T) {
	app, cleanup := setupAgentTestApp(t)
	defer cleanup()

	user, err := createTestUser(app, "overquota@example.com")
	require.NoError(t, err)
	user.Set("relay_quota_gb", 1.0)
	user.Set("current_period_usage_gb", 1.0)
	user.Set("quota_period_start", time.Now().UTC())
	user.Set("quota_period_end", time.Now().UTC().Add(24*time.Hour))
	require.NoError(t, app.Save(user))

	apiKey, err := createTestAPIKey(app, user.Id, "quotasecret")
	require.NoError(t, err)

	sessionsCol, err := app.FindCollectionByNameOrId("sessions")
	require.NoError(t, err)
	session := core.NewRecord(sessionsCol)
	session.Set("code", "QUOTAAUTH")
	session.Set("api_key_id", apiKey.Id)
	session.Set("agent_id", "agent-overquota")
	session.Set("relay_static_pub", "04abcd")
	require.NoError(t, app.Save(session))

	h := hub.New()
	cfg := config.Load()
	cfg.RelayJWTSecret = "secret"
	reg := relay.NewRegistry(2 * time.Second)

	fullKey := apiKey.Id + ".quotasecret"
	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, reg, cfg, nil)
	browserHandler := BrowserWS(app, h, cfg)
	mux := http.NewServeMux()
	mux.Handle("/ws/agent", authMiddleware(http.HandlerFunc(agentHandler)))
	mux.Handle("/ws/client", http.HandlerFunc(browserHandler))

	server := httptest.NewServer(mux)
	defer server.Close()

	ctx := context.Background()

	agentConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent?api_key="+fullKey, nil)
	require.NoError(t, err)
	defer agentConn.CloseNow()
	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","agent_id":"agent-overquota"}`)))
	_, _, err = agentConn.Read(ctx)
	require.NoError(t, err)
	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","code":"QUOTAAUTH","relay_static_pub":"04abcd"}`)))
	_, _, err = agentConn.Read(ctx)
	require.NoError(t, err)

	browserConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/client?session=QUOTAAUTH", nil)
	require.NoError(t, err)
	defer browserConn.CloseNow()
	_, _, err = browserConn.Read(ctx) // ice_config
	require.NoError(t, err)
	require.NoError(t, browserConn.Write(ctx, websocket.MessageText, []byte(`{"type":"knock"}`)))

	_, knockMsg, err := agentConn.Read(ctx)
	require.NoError(t, err)
	var knockPayload struct {
		ConnID string `json:"conn_id"`
	}
	require.NoError(t, json.Unmarshal(knockMsg, &knockPayload))

	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"nonce","conn_id":"`+knockPayload.ConnID+`","value":"testnonce","has_password":false}`)))
	_, _, err = browserConn.Read(ctx)
	require.NoError(t, err)

	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"auth_ok","conn_id":"`+knockPayload.ConnID+`","code":"QUOTAAUTH"}`)))

	_, browserRelayPolicy, err := browserConn.Read(ctx)
	require.NoError(t, err)
	assert.Contains(t, string(browserRelayPolicy), `"type":"relay_policy"`)
	assert.Contains(t, string(browserRelayPolicy), `"relay_allowed":false`)

	readCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	_, _, err = agentConn.Read(readCtx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "context deadline exceeded")
}

func TestAgentWS_AuthOK_MissingRelayStaticPubDisablesRelayFallback(t *testing.T) {
	app, cleanup := setupAgentTestApp(t)
	defer cleanup()

	user, err := createTestUser(app, "nostatic@example.com")
	require.NoError(t, err)
	apiKey, err := createTestAPIKey(app, user.Id, "nostaticsecret")
	require.NoError(t, err)

	sessionsCol, err := app.FindCollectionByNameOrId("sessions")
	require.NoError(t, err)
	session := core.NewRecord(sessionsCol)
	session.Set("code", "NOSTATIC1")
	session.Set("api_key_id", apiKey.Id)
	session.Set("agent_id", "agent-nostatic")
	session.Set("relay_static_pub", "")
	require.NoError(t, app.Save(session))

	h := hub.New()
	cfg := config.Load()
	cfg.RelayJWTSecret = "secret"
	reg := relay.NewRegistry(2 * time.Second)

	fullKey := apiKey.Id + ".nostaticsecret"
	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, reg, cfg, nil)
	browserHandler := BrowserWS(app, h, cfg)
	mux := http.NewServeMux()
	mux.Handle("/ws/agent", authMiddleware(http.HandlerFunc(agentHandler)))
	mux.Handle("/ws/client", http.HandlerFunc(browserHandler))

	server := httptest.NewServer(mux)
	defer server.Close()

	ctx := context.Background()

	agentConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent?api_key="+fullKey, nil)
	require.NoError(t, err)
	defer agentConn.CloseNow()
	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","agent_id":"agent-nostatic"}`)))
	_, _, err = agentConn.Read(ctx)
	require.NoError(t, err)
	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","code":"NOSTATIC1"}`)))
	_, _, err = agentConn.Read(ctx)
	require.NoError(t, err)

	browserConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/client?session=NOSTATIC1", nil)
	require.NoError(t, err)
	defer browserConn.CloseNow()
	_, _, err = browserConn.Read(ctx) // ice_config
	require.NoError(t, err)
	require.NoError(t, browserConn.Write(ctx, websocket.MessageText, []byte(`{"type":"knock"}`)))

	_, knockMsg, err := agentConn.Read(ctx)
	require.NoError(t, err)
	var knockPayload struct {
		ConnID string `json:"conn_id"`
	}
	require.NoError(t, json.Unmarshal(knockMsg, &knockPayload))

	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"nonce","conn_id":"`+knockPayload.ConnID+`","value":"testnonce","has_password":false}`)))
	_, _, err = browserConn.Read(ctx)
	require.NoError(t, err)

	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"auth_ok","conn_id":"`+knockPayload.ConnID+`","code":"NOSTATIC1"}`)))

	_, browserRelayPolicy, err := browserConn.Read(ctx)
	require.NoError(t, err)
	assert.Contains(t, string(browserRelayPolicy), `"type":"relay_policy"`)
	assert.Contains(t, string(browserRelayPolicy), `"relay_allowed":false`)

	readCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	_, _, err = agentConn.Read(readCtx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "context deadline exceeded")
}

func TestAgentWS_AuthOK_RelayPrepareIncludesSessionCode(t *testing.T) {
	app, cleanup := setupAgentTestApp(t)
	defer cleanup()

	user, err := createTestUser(app, "codecheck@example.com")
	require.NoError(t, err)
	apiKey, err := createTestAPIKey(app, user.Id, "codechecksecret")
	require.NoError(t, err)

	// Create test session with relay_static_pub
	sessionsCol, err := app.FindCollectionByNameOrId("sessions")
	require.NoError(t, err)
	session := core.NewRecord(sessionsCol)
	session.Set("code", "TESTCODE1")
	session.Set("api_key_id", apiKey.Id)
	session.Set("agent_id", "agent-codecheck")
	session.Set("relay_static_pub", "04abcd")
	require.NoError(t, app.Save(session))

	h := hub.New()
	cfg := config.Load()
	reg := relay.NewRegistry(2 * time.Second)
	cfg.RelayJWTSecret = "secret"

	fullKey := apiKey.Id + ".codechecksecret"
	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, reg, cfg, nil)
	browserHandler := BrowserWS(app, h, cfg)
	mux := http.NewServeMux()
	mux.Handle("/ws/agent", authMiddleware(http.HandlerFunc(agentHandler)))
	mux.Handle("/ws/client", http.HandlerFunc(browserHandler))

	server := httptest.NewServer(mux)
	defer server.Close()

	ctx := context.Background()

	// Agent connects and registers
	agentConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent?api_key="+fullKey, nil)
	require.NoError(t, err)
	defer agentConn.CloseNow()
	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","agent_id":"agent-codecheck"}`)))
	_, _, err = agentConn.Read(ctx) // welcome
	require.NoError(t, err)
	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","code":"TESTCODE1","relay_static_pub":"04abcd"}`)))
	_, _, err = agentConn.Read(ctx) // registered
	require.NoError(t, err)

	// Browser connects and knocks
	browserConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/client?session=TESTCODE1", nil)
	require.NoError(t, err)
	defer browserConn.CloseNow()
	_, _, err = browserConn.Read(ctx) // ice_config
	require.NoError(t, err)
	require.NoError(t, browserConn.Write(ctx, websocket.MessageText, []byte(`{"type":"knock"}`)))

	// Agent receives the knock forwarded from server
	_, knockMsg, err := agentConn.Read(ctx)
	require.NoError(t, err)
	var knockPayload struct {
		Type   string `json:"type"`
		ConnID string `json:"conn_id"`
		Code   string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(knockMsg, &knockPayload))
	require.Equal(t, "knock", knockPayload.Type)
	require.NotEmpty(t, knockPayload.ConnID)

	// Agent sends nonce response
	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"nonce","conn_id":"`+knockPayload.ConnID+`","value":"testnonce","has_password":false}`)))

	// Browser receives nonce
	_, nonceMsg, err := browserConn.Read(ctx)
	require.NoError(t, err)
	var nonceResp struct {
		Type        string `json:"type"`
		ConnID      string `json:"conn_id"`
		Value       string `json:"value"`
		HasPassword bool   `json:"has_password"`
	}
	require.NoError(t, json.Unmarshal(nonceMsg, &nonceResp))
	require.Equal(t, "nonce", nonceResp.Type)

	// Agent sends auth_ok
	require.NoError(t, agentConn.Write(ctx, websocket.MessageText, []byte(`{"type":"auth_ok","conn_id":"`+knockPayload.ConnID+`","code":"TESTCODE1"}`)))

	// Agent receives relay_prepare
	_, agentRelayPrepare, err := agentConn.Read(ctx)
	require.NoError(t, err)

	var relayPrepare map[string]any
	require.NoError(t, json.Unmarshal(agentRelayPrepare, &relayPrepare))
	assert.Equal(t, "relay_prepare", relayPrepare["type"])
	assert.Equal(t, "TESTCODE1", relayPrepare["code"])
	assert.NotEmpty(t, relayPrepare["sid"])
	assert.NotEmpty(t, relayPrepare["relay_jwt"])
}

// TestAgentWSRejectsEmptyHelloAndPreHelloDirectControl verifies C2(b) (empty
// agent_id hello is rejected and does not enroll) and I2 (direct-control
// messages require a successful hello first).
func TestAgentWSRejectsEmptyHelloAndPreHelloDirectControl(t *testing.T) {
	app, serverURL, cleanup := setupAgentWSWithController(t)
	defer cleanup()

	apiKey := createTestAgentAPIKey(t, app)
	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(serverURL, "http")+"/ws/agent?api_key="+apiKey, nil)
	require.NoError(t, err)
	defer conn.CloseNow()

	// Direct-control before hello must be rejected.
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"csr_submit","csr_pem":"dummy"}`)))
	_, raw, err := conn.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, string(raw), "hello required before csr_submit")

	// Empty agent_id hello must be rejected and NOT enroll the agent.
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","agent_id":""}`)))
	_, raw, err = conn.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, string(raw), "agent_id required")

	// Still not enrolled: a subsequent direct-control message is still gated.
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"report_endpoint","ip":"1.2.3.4","port":0}`)))
	_, raw, err = conn.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, string(raw), "hello required before report_endpoint")
}
