package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/pocketbase/pocketbase/core"
	"github.com/stretchr/testify/require"
	"sharebridge/control/internal/config"
	"sharebridge/control/internal/directctl"
	"sharebridge/control/internal/hub"
	"sharebridge/control/internal/middleware"
	"sharebridge/control/internal/relayctl"
)

// setupAgentWSWithController boots an app (with agents + sessions.origin /
// sessions.is_active schema) and an AgentWS server wired with a real
// directctl.Controller so origin allocation is exercised.
func setupAgentWSWithController(t *testing.T) (core.App, string, func()) {
	t.Helper()

	app, appCleanup := setupAgentTestApp(t)
	h := hub.New()
	cfg := config.Load()

	// coord/dnsClient are nil: origin allocation only needs LoadOrCreateAgent
	// + AllocateOrigin, neither of which touch the coordinator or DDNS client.
	ctrl := directctl.NewController(app, h, nil, nil, directctl.Config{BaseDomain: "example.com"})

	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, cfg, ctrl)
	mux := http.NewServeMux()
	mux.Handle("/ws/agent", authMiddleware(http.HandlerFunc(agentHandler)))

	server := httptest.NewServer(mux)
	cleanup := func() {
		server.Close()
		appCleanup()
	}
	return app, server.URL, cleanup
}

// dialAgentAndEnroll dials the agent WS, sends hello, and consumes BOTH hello
// responses (welcome + enrolled). The controller sends welcome (ICE config)
// and enrolled (namespace) back-to-back, so callers must drain both before the
// next request/response pair or they will misread the queued enrolled frame.
func dialAgentAndEnroll(t *testing.T, serverURL, apiKey, agentID string) *websocket.Conn {
	t.Helper()
	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(serverURL, "http")+"/ws/agent?api_key="+apiKey, nil)
	require.NoError(t, err)
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(`{"type":"hello","agent_id":%q}`, agentID))))
	_, _, err = conn.Read(ctx) // welcome
	require.NoError(t, err)
	_, _, err = conn.Read(ctx) // enrolled
	require.NoError(t, err)
	return conn
}

func TestRegisterShareReturnsOrigin(t *testing.T) {
	app, serverURL, cleanup := setupAgentWSWithController(t)
	defer cleanup()

	apiKey := createTestAgentAPIKey(t, app)
	conn := dialAgentAndEnroll(t, serverURL, apiKey, "agent-origin")
	defer conn.CloseNow()
	ctx := context.Background()

	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","code":"ORIGIN01","share_type":"immich"}`)))
	_, raw, err := conn.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"type":"share_registered"`)

	var resp struct {
		Type        string `json:"type"`
		Code        string `json:"code"`
		Origin      string `json:"origin"`
		RelayOrigin string `json:"relay_origin"`
	}
	require.NoError(t, json.Unmarshal(raw, &resp))
	require.Equal(t, "share_registered", resp.Type)
	require.Equal(t, "ORIGIN01", resp.Code)
	require.Regexp(t, `^[0-9a-f]{12}\.sb[0-9a-f]{8}\.example\.com$`, resp.Origin)

	// §6/§11.2: share_registered also returns the control-derived relay origin:
	// the direct origin with ".relay." inserted before the namespace. The
	// persisted origin stays the direct one.
	require.Regexp(t, `^[0-9a-f]{12}\.relay\.sb[0-9a-f]{8}\.example\.com$`, resp.RelayOrigin)
	expectedRelayOrigin, err := relayctl.RelayOriginFromDirect(resp.Origin)
	require.NoError(t, err)
	require.Equal(t, expectedRelayOrigin, resp.RelayOrigin)
	require.NotEqual(t, resp.Origin, resp.RelayOrigin)

	// The control-allocated origin must be persisted on the session row.
	session := findSessionByCode(t, app, "ORIGIN01")
	require.NotNil(t, session)
	require.Equal(t, resp.Origin, session.GetString("origin"))
	require.NotEqual(t, resp.RelayOrigin, session.GetString("origin"), "sessions persist the direct origin only")
	require.True(t, session.GetBool("is_active"))
}

func TestUnregisterSoftDeletes(t *testing.T) {
	app, serverURL, cleanup := setupAgentWSWithController(t)
	defer cleanup()

	apiKey := createTestAgentAPIKey(t, app)
	conn := dialAgentAndEnroll(t, serverURL, apiKey, "agent-softdelete")
	defer conn.CloseNow()

	ctx := context.Background()
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","code":"SOFTDEL01","share_type":"immich"}`)))
	_, _, err := conn.Read(ctx)
	require.NoError(t, err)

	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"unregister_share","code":"SOFTDEL01"}`)))
	_, raw, err := conn.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"type":"share_unregistered"`)

	// The row must still exist (soft-delete) but be inactive, so an active-only
	// lookup (getSessionByCode) returns nil.
	records, err := app.FindRecordsByFilter("sessions", "code = {:code}", "", 1, 0, map[string]any{"code": "SOFTDEL01"})
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.False(t, records[0].GetBool("is_active"))

	require.Nil(t, findSessionByCode(t, app, "SOFTDEL01"))
}

// setupAgentWSWithControllerAndHub is setupAgentWSWithController but also
// returns the hub so tests can assert hub state (e.g. no orphaned code entry).
func setupAgentWSWithControllerAndHub(t *testing.T) (core.App, string, *hub.Hub, func()) {
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
	cleanup := func() {
		server.Close()
		appCleanup()
	}
	return app, server.URL, h, cleanup
}

// TestRegisterShareOriginAllocationFailureRollsBack verifies I7: when origin
// allocation fails, the session created in the same transaction is rolled back
// AND the hub is never told about the code (no orphaned active session + hub
// entry).
func TestRegisterShareOriginAllocationFailureRollsBack(t *testing.T) {
	app, serverURL, h, cleanup := setupAgentWSWithControllerAndHub(t)
	defer cleanup()

	// Force origin-label collisions: every allocation tries the same label, so
	// the second registration exhausts its retries and fails.
	directctl.SetGenerateOriginLabel(func() string { return "000000000000" })
	t.Cleanup(func() { directctl.SetGenerateOriginLabel(nil) })

	apiKey := createTestAgentAPIKey(t, app)
	conn := dialAgentAndEnroll(t, serverURL, apiKey, "agent-rollback")
	defer conn.CloseNow()
	ctx := context.Background()

	// First registration allocates the fixed label successfully.
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","code":"ROLLBACKOK1","share_type":"immich"}`)))
	_, _, err := conn.Read(ctx)
	require.NoError(t, err)

	// Second registration collides on the fixed label → allocation fails → the
	// session must be rolled back and no hub entry created.
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","code":"ROLLBACKFAIL","share_type":"immich"}`)))
	_, raw, err := conn.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"type":"error"`)

	records, err := app.FindRecordsByFilter("sessions", "code = {:code}", "", 1, 0, map[string]any{"code": "ROLLBACKFAIL"})
	require.NoError(t, err)
	require.Len(t, records, 0, "session must be rolled back on origin allocation failure")

	if _, ok := h.GetAgentConn("ROLLBACKFAIL"); ok {
		t.Fatalf("hub must not have a code entry for the rolled-back session")
	}
}
