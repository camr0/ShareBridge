package handler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/golang-jwt/jwt/v5"
	"sharebridge/server/internal/config"
	"sharebridge/server/internal/handler"
	"sharebridge/server/internal/relay"
)

func TestRelayWS_AgentAndBrowserPairAndForwardOpaqueBytes(t *testing.T) {
	// Setup registry with pending session
	reg := relay.NewRegistry(2 * time.Second)
	cfg := config.Load()
	cfg.RelayJWTSecret = "secret"

	now := time.Unix(1_800_000_000, 0)
	reg.CreatePendingSession(relay.PendingSession{
		SID:          "sid-1",
		AccountID:    "acct-1",
		AgentID:      "agent-1",
		RelayAllowed: true,
		JTI:          "jti-1",
		ExpiresAt:    now.Add(relay.TokenLifetime),
	}, now)

	agentToken, _ := relay.SignAgentRelayJWT("secret", relay.AgentRelayClaims{SID: "sid-1", AgentID: "agent-1"}, now)
	browserToken, _ := relay.SignBrowserPolicyJWT("secret", relay.BrowserPolicyClaims{
		SID:              "sid-1",
		RelayAllowed:     true,
		RegisteredClaims: jwt.RegisteredClaims{ID: "jti-1"},
	}, now)

	mux := http.NewServeMux()
	mux.Handle("/ws/relay", handler.RelayWS(nil, reg, cfg))
	server := httptest.NewServer(mux)
	defer server.Close()

	ctx := context.Background()
	// Agent connects
	agentConn, _, _ := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/relay", nil)
	defer agentConn.CloseNow()
	agentConn.Write(ctx, websocket.MessageText, []byte(`{"token":"`+agentToken+`"}`))

	// Browser connects
	browserConn, _, _ := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/relay", nil)
	defer browserConn.CloseNow()
	browserConn.Write(ctx, websocket.MessageText, []byte(`{"token":"`+browserToken+`"}`))

	// Browser sends ciphertext
	browserConn.Write(ctx, websocket.MessageBinary, []byte("ciphertext"))

	// Agent receives ciphertext
	typ, data, err := agentConn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if typ != websocket.MessageBinary {
		t.Errorf("expected binary, got %v", typ)
	}
	if string(data) != "ciphertext" {
		t.Errorf("data = %q, want 'ciphertext'", data)
	}
}

func TestRelayWS_RejectsReplayedBrowserJTI(t *testing.T) {
	reg := relay.NewRegistry(2 * time.Second)
	cfg := config.Load()
	cfg.RelayJWTSecret = "secret"

	now := time.Unix(1_800_000_000, 0)
	reg.CreatePendingSession(relay.PendingSession{
		SID: "sid-2", AccountID: "acct-1", AgentID: "agent-1",
		RelayAllowed: true, JTI: "jti-2", ExpiresAt: now.Add(relay.TokenLifetime),
	}, now)

	token, _ := relay.SignBrowserPolicyJWT("secret", relay.BrowserPolicyClaims{
		SID: "sid-2", RelayAllowed: true, RegisteredClaims: jwt.RegisteredClaims{ID: "jti-2"},
	}, now)

	mux := http.NewServeMux()
	mux.Handle("/ws/relay", handler.RelayWS(nil, reg, cfg))
	server := httptest.NewServer(mux)
	defer server.Close()

	ctx := context.Background()
	// First connection succeeds
	first, _, _ := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/relay", nil)
	defer first.CloseNow()
	first.Write(ctx, websocket.MessageText, []byte(`{"token":"`+token+`"}`))

	// Second connection should fail (replayed JTI)
	second, _, _ := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/relay", nil)
	defer second.CloseNow()
	second.Write(ctx, websocket.MessageText, []byte(`{"token":"`+token+`"}`))

	_, _, err := second.Read(ctx)
	if err == nil {
		t.Error("expected error for replayed JTI")
	}
}