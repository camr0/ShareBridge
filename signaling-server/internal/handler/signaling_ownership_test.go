package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pocketbase/pocketbase/core"
	"github.com/stretchr/testify/require"
	"sharebridge/server/internal/config"
	"sharebridge/server/internal/hub"
	"sharebridge/server/internal/middleware"
	"sharebridge/server/internal/relay"
)

type ownershipFixture struct {
	ctx                   context.Context
	app                   core.App
	owner, other, browser *websocket.Conn
	connID                string
	serverURL, ownerKey   string
}

func writeOwnershipMessage(t *testing.T, ctx context.Context, conn *websocket.Conn, message any) {
	t.Helper()
	data, err := json.Marshal(message)
	require.NoError(t, err)
	require.NoError(t, conn.Write(ctx, websocket.MessageText, data))
}

func readOwnershipMessage(t *testing.T, ctx context.Context, conn *websocket.Conn) map[string]any {
	t.Helper()
	_, data, err := conn.Read(ctx)
	require.NoError(t, err)
	var message map[string]any
	require.NoError(t, json.Unmarshal(data, &message))
	return message
}

func registerOwnershipShare(t *testing.T, f *ownershipFixture, agent *websocket.Conn, code string) {
	t.Helper()
	writeOwnershipMessage(t, f.ctx, agent, map[string]any{"type": "register_share", "code": code, "relay_static_pub": "0123456789abcdef"})
	require.Equal(t, "share_registered", readOwnershipMessage(t, f.ctx, agent)["type"])
}

func newOwnershipFixture(t *testing.T, sameAccount bool) *ownershipFixture {
	t.Helper()
	app, cleanup := setupAgentTestApp(t)
	t.Cleanup(cleanup)
	cfg := &config.Config{RelayJWTSecret: "ownership-test-secret", STUNURL: "stun:stun.example.test:3478"}
	h := hub.New()
	reg := relay.NewRegistry(time.Second)
	mux := http.NewServeMux()
	mux.Handle("/ws/agent", middleware.APIKeyAuth(app)(http.HandlerFunc(AgentWS(app, h, reg, cfg))))
	mux.Handle("/ws/client", BrowserWS(app, h, cfg))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	f := &ownershipFixture{ctx: ctx, app: app}
	owner, err := createTestUser(app, "owner@example.com")
	require.NoError(t, err)
	other := owner
	if !sameAccount {
		other, err = createTestUser(app, "other@example.com")
		require.NoError(t, err)
	}
	ownerKey, err := createTestAPIKey(app, owner.Id, "owner-secret")
	require.NoError(t, err)
	otherKey, err := createTestAPIKey(app, other.Id, "other-secret")
	require.NoError(t, err)
	f.serverURL, f.ownerKey = server.URL, ownerKey.Id+".owner-secret"
	f.owner = dialAgentAndHello(t, server.URL, f.ownerKey, "owner-agent")
	t.Cleanup(func() { f.owner.CloseNow() })
	f.other = dialAgentAndHello(t, server.URL, otherKey.Id+".other-secret", "other-agent")
	t.Cleanup(func() { f.other.CloseNow() })
	registerOwnershipShare(t, f, f.owner, "VICTIM01")
	registerOwnershipShare(t, f, f.other, "OTHER001")
	f.browser = dialBrowser(t, server.URL, "VICTIM01")
	t.Cleanup(func() { f.browser.CloseNow() })
	require.Equal(t, "ice_config", readOwnershipMessage(t, ctx, f.browser)["type"])
	writeOwnershipMessage(t, ctx, f.browser, map[string]any{"type": "knock"})
	knock := readOwnershipMessage(t, ctx, f.owner)
	require.Equal(t, "knock", knock["type"])
	f.connID = knock["conn_id"].(string)
	return f
}

func TestAgentWSRejectsOtherOwnersMessages(t *testing.T) {
	for _, sameAccount := range []bool{false, true} {
		name := "different-account"
		if sameAccount {
			name = "same-account-different-key"
		}
		t.Run(name, func(t *testing.T) {
			for _, kind := range []string{"offer", "ice_candidate", "nonce", "auth_ok", "auth_fail", "auth_failed"} {
				t.Run(kind, func(t *testing.T) {
					f := newOwnershipFixture(t, sameAccount)
					writeOwnershipMessage(t, f.ctx, f.other, map[string]any{"type": kind, "session_id": "VICTIM01", "code": "VICTIM01", "conn_id": f.connID, "sdp": "forged", "candidate": map[string]any{"candidate": "forged"}, "value": "forged"})
					// The same socket's registration response proves the forged message was processed.
					// An unauthorized auth_ok must not send the attacker a relay_prepare token either.
					registerOwnershipShare(t, f, f.other, "OTHER001")
					writeOwnershipMessage(t, f.ctx, f.owner, map[string]any{"type": "nonce", "conn_id": f.connID, "value": "owner-control"})
					message := readOwnershipMessage(t, f.ctx, f.browser)
					require.Equal(t, "nonce", message["type"], "unauthorized message reached the browser: %v", message)
					require.Equal(t, "owner-control", message["value"])
					if kind == "auth_fail" || kind == "auth_failed" {
						writeOwnershipMessage(t, f.ctx, f.owner, map[string]any{"type": kind, "conn_id": f.connID})
						message = readOwnershipMessage(t, f.ctx, f.browser)
						remaining := float64(4)
						if kind == "auth_failed" {
							remaining = 2
						}
						require.Equal(t, remaining, message["attempts_remaining"], "unauthorized failure changed the owner's counter")
					}
				})
			}
		})
	}
}

func TestAgentWSAuthOKMustMatchBrowserShare(t *testing.T) {
	f := newOwnershipFixture(t, false)
	registerOwnershipShare(t, f, f.owner, "OTHER002")
	writeOwnershipMessage(t, f.ctx, f.owner, map[string]any{"type": "auth_ok", "conn_id": f.connID, "code": "OTHER002"})
	registerOwnershipShare(t, f, f.owner, "OTHER002")
	writeOwnershipMessage(t, f.ctx, f.owner, map[string]any{"type": "nonce", "conn_id": f.connID, "value": "owner-control"})
	require.Equal(t, "owner-control", readOwnershipMessage(t, f.ctx, f.browser)["value"])
}

func TestAgentWSAuthOKChecksPersistedOwner(t *testing.T) {
	f := newOwnershipFixture(t, false)
	victim := findSessionByCode(t, f.app, "VICTIM01")
	other := findSessionByCode(t, f.app, "OTHER001")
	victim.Set("api_key_id", other.GetString("api_key_id"))
	require.NoError(t, f.app.Save(victim))
	writeOwnershipMessage(t, f.ctx, f.owner, map[string]any{"type": "auth_ok", "conn_id": f.connID, "code": "VICTIM01"})
	registerOwnershipShare(t, f, f.owner, "BARRIER1")
	writeOwnershipMessage(t, f.ctx, f.owner, map[string]any{"type": "nonce", "conn_id": f.connID, "value": "owner-control"})
	require.Equal(t, "owner-control", readOwnershipMessage(t, f.ctx, f.browser)["value"])
}

func TestAgentWSReconnectPreservesBrowserOwnership(t *testing.T) {
	f := newOwnershipFixture(t, false)
	replacement := dialAgentAndHello(t, f.serverURL, f.ownerKey, "owner-agent")
	defer replacement.CloseNow()
	writeOwnershipMessage(t, f.ctx, f.browser, map[string]any{"type": "knock"})
	knock := readOwnershipMessage(t, f.ctx, replacement)
	require.Equal(t, f.connID, knock["conn_id"])
	writeOwnershipMessage(t, f.ctx, replacement, map[string]any{"type": "nonce", "conn_id": f.connID, "value": "replacement-control"})
	ctx, cancel := context.WithTimeout(f.ctx, time.Second)
	defer cancel()
	require.Equal(t, "replacement-control", readOwnershipMessage(t, ctx, f.browser)["value"])
}

func TestAgentWSIgnoresRepeatedHello(t *testing.T) {
	f := newOwnershipFixture(t, false)
	writeOwnershipMessage(t, f.ctx, f.owner, map[string]any{"type": "hello", "agent_id": "changed-agent"})
	registerOwnershipShare(t, f, f.owner, "VICTIM01")
}
