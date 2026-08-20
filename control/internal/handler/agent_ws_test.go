package handler

import (
	"context"
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
	"sharebridge/control/internal/config"
	"sharebridge/control/internal/hub"
	"sharebridge/control/internal/middleware"
	"sharebridge/control/migrations"
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

	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, cfg, nil)
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

	// Create HTTP test server with the AgentWS handler wrapped in auth middleware
	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, cfg, nil)
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
}

func TestAgentWS_InvalidAPIKey(t *testing.T) {
	app, cleanup := setupAgentTestApp(t)
	defer cleanup()

	h := hub.New()
	cfg := config.Load()

	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, cfg, nil)
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

	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, cfg, nil)
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
	err = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","share_url":"immich://evil","code":"CUSTOM01","share_type":"immich"}`))
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

	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, cfg, nil)
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

	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","code":"RELAYKEY1","share_type":"immich","relay_static_pub":"04abcd"}`)))
	_, _, err = conn.Read(ctx)
	require.NoError(t, err)

	session, err := getSessionByCode(app, "RELAYKEY1")
	require.NoError(t, err)
	require.Equal(t, "04abcd", session.GetString("relay_static_pub"))
}

func TestAgentWS_RegisterShare_RejectsUnsupportedPayloadBeforeSessionCreation(t *testing.T) {
	testApp, serverURL, cleanup := setupAgentWSWithController(t)
	defer cleanup()

	apiKey := createTestAgentAPIKey(t, testApp)
	ctx := context.Background()
	conn := dialAgentAndEnroll(t, serverURL, apiKey, "agent-immich")
	defer conn.CloseNow()

	for _, tc := range []struct {
		name, payload string
	}{
		{"wrong type", `{"share_type":"webdav"}`},
		{"relay only", `{"share_type":"immich","relay_only":true}`},
		{"protected", `{"share_type":"immich","is_password_protected":true}`},
	} {
		code := "UNSUPPORTED" + strings.ReplaceAll(strings.ToUpper(tc.name), " ", "")
		payload := fmt.Sprintf(`{"type":"register_share","code":%q,%s}`, code, tc.payload[1:len(tc.payload)-1])
		require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(payload)), tc.name)
		_, raw, err := conn.Read(ctx)
		require.NoError(t, err, tc.name)
		require.Contains(t, string(raw), `"type":"error"`, tc.name)
		require.Nil(t, findSessionByCode(t, testApp, code), "unsupported registration must not allocate a live session", tc.name)
	}
}

func TestAgentWS_RegisterShare_ReclaimSupportedImmich(t *testing.T) {
	testApp, serverURL, cleanup := setupAgentWSTest(t)
	defer cleanup()

	apiKey := createTestAgentAPIKey(t, testApp)
	agentConn := dialAgentAndHello(t, serverURL, apiKey, "agent-reclaim-relay")
	defer agentConn.CloseNow()

	require.NoError(t, agentConn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"register_share","code":"RECLAIMRELAY","share_type":"immich","relay_static_pub":"04abcd"}`)))
	_, _, err := agentConn.Read(context.Background())
	require.NoError(t, err)

	require.NoError(t, agentConn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"register_share","code":"RECLAIMRELAY","share_type":"immich","relay_static_pub":"04abcd"}`)))
	_, _, err = agentConn.Read(context.Background())
	require.NoError(t, err)

	session := findSessionByCode(t, testApp, "RECLAIMRELAY")
	require.False(t, session.GetBool("relay_only"))
}

func TestAgentWS_RegisterShare_ReclaimAllowsSupportedDirectPayload(t *testing.T) {
	testApp, serverURL, cleanup := setupAgentWSTest(t)
	defer cleanup()

	apiKey := createTestAgentAPIKey(t, testApp)
	agentConn := dialAgentAndHello(t, serverURL, apiKey, "agent-reclaim-relay-false")
	defer agentConn.CloseNow()

	require.NoError(t, agentConn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"register_share","code":"RECLAIMFALSE","share_type":"immich","relay_only":false,"relay_static_pub":"04abcd"}`)))
	_, _, err := agentConn.Read(context.Background())
	require.NoError(t, err)

	require.NoError(t, agentConn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"register_share","code":"RECLAIMFALSE","share_type":"immich","relay_only":false,"relay_static_pub":"04abcd"}`)))
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
