package handler

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"sharebridge/control/internal/certcoordinator"
	"sharebridge/control/internal/config"
	"sharebridge/control/internal/directctl"
	"sharebridge/control/internal/hub"
	"sharebridge/control/internal/middleware"
	"sharebridge/control/internal/relayctl"
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
	err = migrations.AddAgentsRelaySTUN(testApp)
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

// setupRelayAgentWS boots an AgentWS server whose controller carries a stub
// cert coordinator (no network), stub DDNS + relay-DNS functions, and an
// enabled relay policy, so a real agent connection can be driven through
// baseline readiness (csr → tls_ready; relay DNS is provisioned at hello) to
// the relay_config emission.
func setupRelayAgentWS(t *testing.T) (core.App, string, *relayctl.Signer, relayctl.Settings, func()) {
	t.Helper()

	app, appCleanup := setupAgentTestApp(t)
	h := hub.New()
	cfg := config.Load()

	coord, err := certcoordinator.NewCoordinator(certcoordinator.CoordinatorConfig{AccountKeyPath: filepath.Join(t.TempDir(), "acct.pem")})
	require.NoError(t, err)
	coord.SetIssueFn(func(ctx context.Context, csrPEM []byte, namespace, apiKeyID string) ([]byte, error) {
		return stubLeafChainPEM(t), nil
	})

	ctrl := directctl.NewController(app, h, coord, nil, directctl.Config{
		BaseDomain:       "example.com",
		DDNSFunc:         func(ctx context.Context, name, ip string, ttl int) (string, error) { return "", nil },
		RelayGatewayIPv4: "203.0.113.10",
		RelayDNSFunc:     func(ctx context.Context, name, ip string, ttl int) (string, error) { return "", nil },
	})
	signer, err := relayctl.GenerateSigner()
	require.NoError(t, err)
	settings := relayctl.Settings{GatewayAddr: "relay.example.com", GatewayPort: 7000, PortRange: mustRelayPortRange(t, 11000, 11019)}
	ctrl.EnableRelay(settings, signer)

	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, cfg, ctrl)
	mux := http.NewServeMux()
	mux.Handle("/ws/agent", authMiddleware(http.HandlerFunc(agentHandler)))

	server := httptest.NewServer(mux)
	cleanup := func() {
		server.Close()
		appCleanup()
	}
	return app, server.URL, signer, settings, cleanup
}

func mustRelayPortRange(t *testing.T, min, max int) relayctl.PortRange {
	t.Helper()
	portRange, err := relayctl.NewPortRange(min, max)
	require.NoError(t, err)
	return portRange
}

// stubLeafChainPEM mints a valid future-dated self-signed leaf, mirroring the
// directctl test fixture, so coordinator leaf lookups succeed.
func stubLeafChainPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// stubLeafFingerprint returns the SHA-256 fingerprint of the stub chain's leaf.
func stubLeafFingerprint(t *testing.T, chainPEM []byte) string {
	t.Helper()
	block, _ := pem.Decode(chainPEM)
	require.NotNil(t, block)
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:])
}

// csrToFingerprint drives the csr_submit round trip and returns the leaf
// fingerprint from the cert_issue chain, exactly as a real agent does.
func csrToFingerprint(t *testing.T, conn *websocket.Conn) string {
	t.Helper()
	ctx := context.Background()

	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"csr_submit","csr_pem":"stub-csr"}`)))
	_, raw, err := conn.Read(ctx) // cert_issue
	require.NoError(t, err)
	require.Contains(t, string(raw), "cert_issue")
	var issued struct {
		ChainPEM string `json:"chain_pem"`
	}
	require.NoError(t, json.Unmarshal(raw, &issued))
	return stubLeafFingerprint(t, []byte(issued.ChainPEM))
}

// finishBaselineReadiness completes baseline readiness from an issued
// fingerprint: tls_ready → enrollment_ready (consumed). Relay DNS is
// provisioned during hello (§6), so no report_endpoint is required — direct
// endpoint/DDNS is an optional capability and never gates enrollment (§7.1).
func finishBaselineReadiness(t *testing.T, conn *websocket.Conn, fingerprint string) {
	t.Helper()
	ctx := context.Background()

	tlsReadyPayload := fmt.Sprintf(`{"type":"tls_ready","fingerprint":%q}`, fingerprint)
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(tlsReadyPayload)))
	_, raw, err := conn.Read(ctx) // enrollment_ready
	require.NoError(t, err)
	require.Contains(t, string(raw), "enrollment_ready")
}

// readExpectSilence asserts that no frame arrives within the wait window.
func readExpectSilence(t *testing.T, conn *websocket.Conn, wait time.Duration, msgAndArgs ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	_, raw, err := conn.Read(ctx)
	require.Error(t, err, "expected no message within %s, got: %s", wait, raw)
}

// TestRelayConfigSentOnlyAfterBaselineReadiness pins §7.2/§11.1: relay_config
// goes only to the authenticated current epoch AFTER baseline readiness, with
// the exact wire shape and an Ed25519 credential whose claims match the
// control-persisted assignment.
func TestRelayConfigSentOnlyAfterBaselineReadiness(t *testing.T) {
	app, serverURL, signer, settings, cleanup := setupRelayAgentWS(t)
	defer cleanup()

	fullKey := createTestAgentAPIKey(t, app)
	apiKeyID := strings.SplitN(fullKey, ".", 2)[0]
	ctx := context.Background()
	conn := dialAgentAndEnroll(t, serverURL, fullKey, "agent-relay-config")
	defer conn.CloseNow()

	// Before readiness (tls_ready missing): neither enrollment_ready nor
	// relay_config may be emitted — the relay wildcard is provisioned during
	// hello, but TLS is still missing. Proven without timeout reads (which
	// would kill the connection): the FIRST reply to csr_submit must be
	// cert_issue, never a relay_config/enrollment_ready frame.
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"report_endpoint","ip":"203.0.113.7","port":8443}`)))

	agentRecs, err := app.FindRecordsByFilter("agents", "api_key_id = {:k}", "", 1, 0, map[string]any{"k": apiKeyID})
	require.NoError(t, err)
	require.Len(t, agentRecs, 1)
	require.Zero(t, agentRecs[0].GetInt("relay_port"), "no assignment before baseline readiness")

	fingerprint := csrToFingerprint(t, conn) // first reply must be cert_issue

	// Completing readiness (tls_ready; relay DNS was provisioned at hello) is
	// exactly what unlocks enrollment_ready + relay_config.
	finishBaselineReadiness(t, conn, fingerprint)

	_, raw, err := conn.Read(ctx) // relay_config follows enrollment_ready
	require.NoError(t, err)

	var relayMsg map[string]any
	require.NoError(t, json.Unmarshal(raw, &relayMsg))
	require.Equal(t, "relay_config", relayMsg["type"])

	// Exact §11.1 wire keys (plus the framing "type"), nothing more.
	expectedKeys := []string{"type", "version", "generation", "gateway_addr", "gateway_port", "proxy_name", "relay_port", "credential", "expires_at"}
	gotKeys := make([]string, 0, len(relayMsg))
	for key := range relayMsg {
		gotKeys = append(gotKeys, key)
	}
	require.ElementsMatch(t, expectedKeys, gotKeys)

	// The assignment must be persisted and echoed consistently.
	persistedAgent, err := app.FindRecordById("agents", agentRecs[0].Id)
	require.NoError(t, err)
	assignedPort := persistedAgent.GetInt("relay_port")
	require.NotZero(t, assignedPort)
	require.True(t, settings.PortRange.Contains(assignedPort))
	require.EqualValues(t, assignedPort, relayMsg["relay_port"])
	require.EqualValues(t, persistedAgent.GetInt("relay_generation"), relayMsg["generation"])
	require.Equal(t, settings.GatewayAddr, relayMsg["gateway_addr"])
	require.EqualValues(t, settings.GatewayPort, relayMsg["gateway_port"])
	require.Equal(t, relayctl.ProxyNameFor(persistedAgent.GetString("namespace")), relayMsg["proxy_name"])

	// Credential: real signature verification + identity + fresh expiry.
	credential, ok := relayMsg["credential"].(string)
	require.True(t, ok)
	claims, err := relayctl.VerifyCredential(signer.PublicKey(), []byte(credential))
	require.NoError(t, err)
	require.Equal(t, apiKeyID, claims.APIKeyID)
	require.Equal(t, persistedAgent.Id, claims.AgentRecordID)
	require.Equal(t, persistedAgent.GetString("namespace"), claims.Namespace)
	require.Equal(t, assignedPort, claims.RelayPort)
	require.True(t, claims.ExpiresAt.After(time.Now()))
	expiresAt, ok := relayMsg["expires_at"].(string)
	require.True(t, ok)
	parsedExpiry, err := time.Parse(time.RFC3339, expiresAt)
	require.NoError(t, err)
	require.True(t, parsedExpiry.After(time.Now()))

	// A reconnect (fresh epoch) re-issues a fresh credential with a distinct
	// JTI — never a resend of the same token.
	conn.CloseNow()
	conn2 := dialAgentAndEnroll(t, serverURL, fullKey, "agent-relay-config")
	defer conn2.CloseNow()
	fingerprint2 := csrToFingerprint(t, conn2)
	finishBaselineReadiness(t, conn2, fingerprint2)
	_, raw2, err := conn2.Read(ctx)
	require.NoError(t, err)
	var relayMsg2 struct {
		Credential string `json:"credential"`
		RelayPort  int    `json:"relay_port"`
	}
	require.NoError(t, json.Unmarshal(raw2, &relayMsg2))
	require.Equal(t, "relay_config", mustMessageType(t, raw2))
	require.Equal(t, assignedPort, relayMsg2.RelayPort, "reconnect keeps the assigned port")
	claims2, err := relayctl.VerifyCredential(signer.PublicKey(), []byte(relayMsg2.Credential))
	require.NoError(t, err)
	require.NotEqual(t, claims.JTI, claims2.JTI, "reconnect requires (and gets) a fresh credential")
}

func mustMessageType(t *testing.T, raw []byte) string {
	t.Helper()
	var frame struct {
		Type string `json:"type"`
	}
	require.NoError(t, json.Unmarshal(raw, &frame))
	return frame.Type
}

// TestRelayClientStateTelemetryIsBoundedAndInert pins §11.1: relay_client_state
// is accepted only from the enrolled current epoch, validated against the
// bounded shape, and is pure telemetry — it never replies with availability,
// never mutates share lifecycle, and invalid payloads are dropped.
func TestRelayClientStateTelemetryIsBoundedAndInert(t *testing.T) {
	app, serverURL, cleanup := setupAgentWSWithController(t)
	defer cleanup()

	apiKey := createTestAgentAPIKey(t, app)
	conn := dialAgentAndEnroll(t, serverURL, apiKey, "agent-relay-telemetry")
	defer conn.CloseNow()
	ctx := context.Background()

	// A live share exists; telemetry must leave its lifecycle untouched.
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","code":"TELEMETRY1","share_type":"immich"}`)))
	_, raw, err := conn.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, string(raw), "share_registered")
	before := findSessionByCode(t, app, "TELEMETRY1")
	require.NotNil(t, before)

	// probeAfter sends a message that must NOT be answered, then a control
	// message with a deterministic reply (unregister_share on an unknown code
	// always answers share_unregistered). Reading exactly ONE frame proves the
	// probed message was silent — any reply would have arrived first.
	probeAfter := func(payload string) {
		t.Helper()
		require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(payload)))
		require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"unregister_share","code":"UNKNOWNCODE"}`)))
		_, probeRaw, probeErr := conn.Read(ctx)
		require.NoError(t, probeErr)
		require.Contains(t, string(probeRaw), "share_unregistered",
			"expected silence for %s, got an intervening reply first", payload)
	}

	// Valid telemetry: accepted silently (no reply, no state change), unknown
	// JSON fields ignored.
	probeAfter(`{"type":"relay_client_state","generation":1,"status":"running","reason":"tunnel up","unknown_field":"ignored"}`)

	// Invalid payloads: dropped, no reply, no panic.
	probeAfter(`{"type":"relay_client_state","generation":1,"status":"connected"}`)                                         // bad enum
	probeAfter(`{"type":"relay_client_state","generation":-2,"status":"running"}`)                                          // bad generation
	probeAfter(`{"type":"relay_client_state","generation":1,"status":"error","reason":"` + strings.Repeat("x", 257) + `"}`) // oversize reason

	// Inertness: the session row is unchanged on lifecycle fields.
	after := findSessionByCode(t, app, "TELEMETRY1")
	require.NotNil(t, after)
	require.Equal(t, before.GetString("origin"), after.GetString("origin"))
	require.Equal(t, before.GetBool("is_active"), after.GetBool("is_active"))
	require.Equal(t, before.GetString("inactive_reason"), after.GetString("inactive_reason"))
	require.True(t, after.GetBool("is_active"))
}

// testRelaySeedHex is a fixed 32-byte Ed25519 seed for the handler-owned
// credential-request policy in tests. It decodes to ASCII "0123456789abcdef"
// twice; it is test-only material and never a production key.
const testRelaySeedHex = "3031323334353637383961626364656630313233343536373839616263646566"

// setupRelayCredentialAgentWS boots an AgentWS server with the handler-owned
// relay credential policy enabled through the operator environment (the same
// config surface production main() passes). The controller deliberately has
// NO EnableRelay policy: the §11.1 relay_credential_request path is
// handler-owned and must work independently of the enrollment-time emitter.
func setupRelayCredentialAgentWS(t *testing.T) (core.App, string, ed25519.PublicKey, func()) {
	t.Helper()
	t.Setenv("RELAY_GATEWAY_HOST", "relay.example.com")
	t.Setenv("RELAY_GATEWAY_PORT", "7000")
	t.Setenv("RELAY_PORT_MIN", "11000")
	t.Setenv("RELAY_PORT_MAX", "11019")
	t.Setenv("RELAY_AUTH_KEY_SEED", testRelaySeedHex)
	return setupCredentialAgentWSWithLoadedConfig(t)
}

// setupRelayCredentialAgentWSDisabled boots the same server with relay policy
// explicitly empty: credential requests must fail closed (dropped, no reply).
func setupRelayCredentialAgentWSDisabled(t *testing.T) (core.App, string, func()) {
	t.Helper()
	t.Setenv("RELAY_GATEWAY_HOST", "")
	t.Setenv("RELAY_AUTH_KEY_SEED", "")
	app, serverURL, _, cleanup := setupCredentialAgentWSWithLoadedConfig(t)
	return app, serverURL, cleanup
}

func setupCredentialAgentWSWithLoadedConfig(t *testing.T) (core.App, string, ed25519.PublicKey, func()) {
	t.Helper()

	app, appCleanup := setupAgentTestApp(t)
	h := hub.New()
	cfg := config.Load()

	ctrl := directctl.NewController(app, h, nil, nil, directctl.Config{BaseDomain: "example.com"})

	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, cfg, ctrl)
	mux := http.NewServeMux()
	mux.Handle("/ws/agent", authMiddleware(http.HandlerFunc(agentHandler)))

	server := httptest.NewServer(mux)
	seed, err := hex.DecodeString(testRelaySeedHex)
	require.NoError(t, err)
	require.Len(t, seed, ed25519.SeedSize)
	publicKey, ok := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	require.True(t, ok)
	cleanup := func() {
		server.Close()
		appCleanup()
	}
	return app, server.URL, publicKey, cleanup
}

// sendRelayCredentialRequest writes one §11.1 relay_credential_request frame.
func sendRelayCredentialRequest(t *testing.T, conn *websocket.Conn, reason string) {
	t.Helper()
	ctx := context.Background()
	frame, err := json.Marshal(map[string]string{"type": "relay_credential_request", "reason": reason})
	require.NoError(t, err)
	require.NoError(t, conn.Write(ctx, websocket.MessageText, frame))
}

// readPersistedRelayAssignment loads the agent row and returns its stable
// relay port and generation.
func readPersistedRelayAssignment(t *testing.T, app core.App, apiKeyID string) (int, int) {
	t.Helper()
	agents, err := app.FindRecordsByFilter("agents", "api_key_id = {:k}", "", 1, 0, map[string]any{"k": apiKeyID})
	require.NoError(t, err)
	require.Len(t, agents, 1, "hello must have created the agent row")
	return agents[0].GetInt("relay_port"), agents[0].GetInt("relay_generation")
}

// verifyFreshRelayConfig asserts one relay_config reply is the exact §11.1
// shape carrying a freshly signed credential for the persisted assignment.
// It returns the credential JTI.
func verifyFreshRelayConfig(t *testing.T, raw []byte, publicKey ed25519.PublicKey, apiKeyID string, app core.App, gatewayPort int) string {
	t.Helper()

	var relayMsg map[string]any
	require.NoError(t, json.Unmarshal(raw, &relayMsg))
	require.Equal(t, "relay_config", relayMsg["type"])

	// Exact §11.1 wire keys (plus the framing "type"), nothing more.
	expectedKeys := []string{"type", "version", "generation", "gateway_addr", "gateway_port", "proxy_name", "relay_port", "credential", "expires_at"}
	gotKeys := make([]string, 0, len(relayMsg))
	for key := range relayMsg {
		gotKeys = append(gotKeys, key)
	}
	require.ElementsMatch(t, expectedKeys, gotKeys)

	// The assignment must be the CURRENT persisted one.
	persistedPort, persistedGeneration := readPersistedRelayAssignment(t, app, apiKeyID)
	require.NotZero(t, persistedPort, "assignment must exist")
	require.EqualValues(t, persistedPort, relayMsg["relay_port"])
	require.EqualValues(t, persistedGeneration, relayMsg["generation"])
	require.Equal(t, "relay.example.com", relayMsg["gateway_addr"])
	require.EqualValues(t, gatewayPort, relayMsg["gateway_port"])

	// Credential: real signature verification against the configured seed's
	// public key + identity + freshness (§7.2).
	credential, ok := relayMsg["credential"].(string)
	require.True(t, ok)
	claims, err := relayctl.VerifyCredential(publicKey, []byte(credential))
	require.NoError(t, err)
	require.Equal(t, apiKeyID, claims.APIKeyID)
	require.Equal(t, relayctl.Audience, claims.Audience)
	require.NotEmpty(t, claims.JTI, "every issued credential carries a fresh one-use jti")
	require.True(t, claims.ExpiresAt.After(time.Now()), "credential must not be expired at issue")
	require.Equal(t, relayctl.CredentialTTL, claims.ExpiresAt.Sub(claims.IssuedAt),
		"single-clock signing: admission lifetime must be exactly the ten-minute maximum")

	expiresAt, ok := relayMsg["expires_at"].(string)
	require.True(t, ok)
	parsedExpiry, err := time.Parse(time.RFC3339, expiresAt)
	require.NoError(t, err)
	require.True(t, parsedExpiry.Equal(claims.ExpiresAt.Truncate(time.Second)) || parsedExpiry.After(time.Now()))

	return claims.JTI
}

// TestRelayCredentialRequestIssuesFreshConfigRateLimited is the Task 6
// amendment test (§11.1, §16.4): a post-hello, current-epoch request is
// served with a freshly signed relay_config for the CURRENT assignment
// (fresh one-use jti every time, single-clock ten-minute lifetime), every
// listed reason is accepted, and the per-agent token bucket drops requests
// beyond the burst so a malicious or broken agent cannot churn credentials.
func TestRelayCredentialRequestIssuesFreshConfigRateLimited(t *testing.T) {
	app, serverURL, publicKey, cleanup := setupRelayCredentialAgentWS(t)
	defer cleanup()

	fullKey := createTestAgentAPIKey(t, app)
	apiKeyID := strings.SplitN(fullKey, ".", 2)[0]
	conn := dialAgentAndEnroll(t, serverURL, fullKey, "agent-cred-request")
	defer conn.CloseNow()

	// Reason matrix (§11.1): every enumerated reason is served identically.
	jtis := make([]string, 0, 4)
	for _, reason := range []string{"replay_rejected", "expired", "restart", "replay_rejected"} {
		sendRelayCredentialRequest(t, conn, reason)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, raw, err := conn.Read(ctx)
		cancel()
		require.NoError(t, err, "request with reason %q must be served", reason)
		jtis = append(jtis, verifyFreshRelayConfig(t, raw, publicKey, apiKeyID, app, 7000))
	}
	require.NotEqual(t, jtis[0], jtis[1], "a second issue must mint a fresh one-use jti, never resend the token")
	require.NotEqual(t, jtis[1], jtis[2], "jti uniqueness across issues")
	// R3-m1: non-adjacent distinctness too — adjacent pairs alone could miss
	// a repeating A/B/A/B pattern across the burst.
	require.NotEqual(t, jtis[0], jtis[3], "first and last jtis of the burst must differ, not only adjacent pairs")

	// The per-agent token bucket held exactly four tokens: the fifth request
	// in the burst is dropped with no reply, and the unknown reason is
	// rejected without a reply as well — neither consumes bucket state nor
	// produces credential material. Both probes are written BEFORE the single
	// silence window because a timed-out Read closes the websocket connection:
	// this must stay the last exchange on the socket.
	sendRelayCredentialRequest(t, conn, "replay_rejected")
	sendRelayCredentialRequest(t, conn, "because_i_said_so")
	readExpectSilence(t, conn, 500*time.Millisecond,
		"rate-limited and unknown-reason requests must not be served")
}

// TestRelayCredentialRequestLimiterBounded pins the §16.4 limiter mechanics:
// burst capacity, one-token-per-interval refill, per-agent isolation, and
// bounded tracked state (idle reclaim, fail-closed beyond the cap).
func TestRelayCredentialRequestLimiterBounded(t *testing.T) {
	base := time.Now()
	key := "limiter-unit-key"

	// A fresh bucket grants the full burst, then denies.
	for i := 0; i < relayCredentialRequestsPerMinute; i++ {
		if !allowRelayCredentialRequestAt(key, base) {
			t.Fatalf("burst request %d denied; capacity is %d", i+1, relayCredentialRequestsPerMinute)
		}
	}
	if allowRelayCredentialRequestAt(key, base.Add(time.Second)) {
		t.Fatal("request beyond the burst capacity must be denied")
	}

	// One token refills per interval (4/minute sustained).
	if allowRelayCredentialRequestAt(key, base.Add(14*time.Second)) {
		t.Fatal("no token may refill before one full interval")
	}
	if !allowRelayCredentialRequestAt(key, base.Add(15*time.Second)) {
		t.Fatal("exactly one token must refill after one interval")
	}
	if allowRelayCredentialRequestAt(key, base.Add(16*time.Second)) {
		t.Fatal("the refilled token was consumed; a second must not appear")
	}

	// Buckets are per agent.
	if !allowRelayCredentialRequestAt("limiter-other-key", base) {
		t.Fatal("a second agent must have its own budget")
	}

	// Bounded state: at the tracked-agent cap a fresh agent is admitted by
	// reclaiming an idle bucket; with no reclaimable state it fails closed.
	relayCredentialLimiter.Lock()
	saved := relayCredentialLimiter.buckets
	relayCredentialLimiter.buckets = make(map[string]*relayCredentialBucket, maxRelayCredentialAgents)
	for i := 0; i < maxRelayCredentialAgents-1; i++ {
		relayCredentialLimiter.buckets[fmt.Sprintf("cap-key-%d", i)] = &relayCredentialBucket{tokens: 0, lastRefill: base}
	}
	// One idle bucket is reclaimable (cap-1 fresh + 1 idle = at the cap).
	relayCredentialLimiter.buckets["limiter-idle-key"] = &relayCredentialBucket{tokens: 0, lastRefill: base.Add(-relayCredentialBucketIdleTTL)}
	relayCredentialLimiter.Unlock()

	if !allowRelayCredentialRequestAt("limiter-brand-new-key", base) {
		t.Fatal("a fresh agent must be admitted by reclaiming the idle bucket")
	}

	relayCredentialLimiter.Lock()
	relayCredentialLimiter.buckets = make(map[string]*relayCredentialBucket, maxRelayCredentialAgents)
	for i := 0; i < maxRelayCredentialAgents; i++ {
		relayCredentialLimiter.buckets[fmt.Sprintf("fresh-key-%d", i)] = &relayCredentialBucket{tokens: 0, lastRefill: base}
	}
	relayCredentialLimiter.Unlock()
	if allowRelayCredentialRequestAt("limiter-beyond-cap-key", base) {
		t.Fatal("beyond the agent cap with no reclaimable state must fail closed")
	}

	relayCredentialLimiter.Lock()
	relayCredentialLimiter.buckets = saved
	relayCredentialLimiter.Unlock()
}

// TestRelayCredentialRequestGuards pins the §11.1 guards: pre-hello is
// rejected with the standard error frame, a fenced (stale-epoch) socket is
// silently dropped while the replacement epoch is served, and a deployment
// without relay policy fails closed.
func TestRelayCredentialRequestGuards(t *testing.T) {
	t.Run("requires_hello_first", func(t *testing.T) {
		app, serverURL, _, cleanup := setupRelayCredentialAgentWS(t)
		defer cleanup()

		fullKey := createTestAgentAPIKey(t, app)
		ctx := context.Background()
		conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(serverURL, "http")+"/ws/agent?api_key="+fullKey, nil)
		require.NoError(t, err)
		defer conn.CloseNow()

		sendRelayCredentialRequest(t, conn, "replay_rejected")
		readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, raw, err := conn.Read(readCtx)
		cancel()
		require.NoError(t, err)
		require.Contains(t, string(raw), "hello required before relay_credential_request")
	})

	t.Run("stale_epoch_socket_dropped_current_epoch_served", func(t *testing.T) {
		app, serverURL, publicKey, cleanup := setupRelayCredentialAgentWS(t)
		defer cleanup()

		fullKey := createTestAgentAPIKey(t, app)
		apiKeyID := strings.SplitN(fullKey, ".", 2)[0]
		ctx := context.Background()

		stale := dialAgentAndEnroll(t, serverURL, fullKey, "agent-cred-epoch")
		defer stale.CloseNow()
		current := dialAgentAndEnroll(t, serverURL, fullKey, "agent-cred-epoch")
		defer current.CloseNow()

		// The replaced socket must never receive tunnel credentials.
		sendRelayCredentialRequest(t, stale, "replay_rejected")
		readExpectSilence(t, stale, 400*time.Millisecond, "stale epoch must be dropped")

		// The current epoch's request is served for the same assignment.
		sendRelayCredentialRequest(t, current, "replay_rejected")
		readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, raw, err := current.Read(readCtx)
		cancel()
		require.NoError(t, err)
		verifyFreshRelayConfig(t, raw, publicKey, apiKeyID, app, 7000)
	})

	t.Run("fails_closed_without_relay_policy", func(t *testing.T) {
		app, serverURL, cleanup := setupRelayCredentialAgentWSDisabled(t)
		defer cleanup()

		fullKey := createTestAgentAPIKey(t, app)
		conn := dialAgentAndEnroll(t, serverURL, fullKey, "agent-cred-disabled")
		defer conn.CloseNow()

		sendRelayCredentialRequest(t, conn, "replay_rejected")
		readExpectSilence(t, conn, 400*time.Millisecond, "relay-unconfigured deployments must not issue credentials")
	})
}

func TestRelayLockdownStatusTelemetryIsInert(t *testing.T) {
	app, serverURL, cleanup := setupAgentWSWithController(t)
	defer cleanup()

	apiKey := createTestAgentAPIKey(t, app)
	conn := dialAgentAndEnroll(t, serverURL, apiKey, "agent-lockdown-telemetry")
	defer conn.CloseNow()
	ctx := context.Background()

	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","code":"LOCKDWN001","share_type":"immich"}`)))
	_, raw, err := conn.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, string(raw), "share_registered")

	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"lockdown_status","generation":1,"locked":true}`)))
	readExpectSilence(t, conn, 300*time.Millisecond, "lockdown telemetry must not reply")

	session := findSessionByCode(t, app, "LOCKDWN001")
	require.NotNil(t, session)
	require.True(t, session.GetBool("is_active"), "lockdown telemetry must never mutate share lifecycle")

	// Pre-enrollment rejection: a connection that never sent hello is rejected
	// for telemetry just like every other control-plane message.
	rawConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(serverURL, "http")+"/ws/agent?api_key="+apiKey, nil)
	require.NoError(t, err)
	defer rawConn.CloseNow()
	require.NoError(t, rawConn.Write(ctx, websocket.MessageText, []byte(`{"type":"lockdown_status","generation":1,"locked":true}`)))
	_, raw, err = rawConn.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, string(raw), "hello required before lockdown_status")
}
