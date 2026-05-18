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

	// Agent can still send back to browser after the normal pre-registration flow.
	agentConn.Write(ctx, websocket.MessageBinary, []byte("return-path"))
	typ, data, err = browserConn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if typ != websocket.MessageBinary {
		t.Errorf("expected binary, got %v", typ)
	}
	if string(data) != "return-path" {
		t.Errorf("data = %q, want 'return-path'", data)
	}
}

func TestRelayWS_ForwardsOpaqueFrameLargerThanDefaultReadLimit(t *testing.T) {
	reg := relay.NewRegistry(2 * time.Second)
	cfg := config.Load()
	cfg.RelayJWTSecret = "secret"

	now := time.Unix(1_800_000_000, 0)
	reg.CreatePendingSession(relay.PendingSession{
		SID:          "sid-large",
		AccountID:    "acct-1",
		AgentID:      "agent-1",
		RelayAllowed: true,
		JTI:          "jti-large",
		ExpiresAt:    now.Add(relay.TokenLifetime),
	}, now)

	agentToken, _ := relay.SignAgentRelayJWT("secret", relay.AgentRelayClaims{SID: "sid-large", AgentID: "agent-1"}, now)
	browserToken, _ := relay.SignBrowserPolicyJWT("secret", relay.BrowserPolicyClaims{
		SID:              "sid-large",
		RelayAllowed:     true,
		RegisteredClaims: jwt.RegisteredClaims{ID: "jti-large"},
	}, now)

	mux := http.NewServeMux()
	mux.Handle("/ws/relay", handler.RelayWS(nil, reg, cfg))
	server := httptest.NewServer(mux)
	defer server.Close()

	ctx := context.Background()
	agentConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/relay", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer agentConn.CloseNow()
	agentConn.Write(ctx, websocket.MessageText, []byte(`{"token":"`+agentToken+`"}`))

	browserConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/relay", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer browserConn.CloseNow()
	browserConn.SetReadLimit(128 * 1024)
	browserConn.Write(ctx, websocket.MessageText, []byte(`{"token":"`+browserToken+`"}`))

	largePayload := strings.Repeat("x", 64*1024)
	if err := agentConn.Write(ctx, websocket.MessageBinary, []byte(largePayload)); err != nil {
		t.Fatal(err)
	}

	typ, data, err := browserConn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if typ != websocket.MessageBinary {
		t.Errorf("expected binary, got %v", typ)
	}
	if string(data) != largePayload {
		t.Errorf("large relay payload corrupted: got %d bytes, want %d", len(data), len(largePayload))
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

func TestRelayWS_HelloTimeout(t *testing.T) {
	reg := relay.NewRegistry(2 * time.Second)
	cfg := config.Load()
	cfg.RelayJWTSecret = "secret"

	mux := http.NewServeMux()
	mux.Handle("/ws/relay", handler.RelayWS(nil, reg, cfg))
	server := httptest.NewServer(mux)
	defer server.Close()

	// Set a short timeout for testing (restore after test)
	originalTimeout := handler.HelloTimeout()
	defer handler.SetHelloTimeout(originalTimeout)
	handler.SetHelloTimeout(500 * time.Millisecond)

	ctx := context.Background()
	// Client connects but never sends hello
	conn, _, _ := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/relay", nil)
	defer conn.CloseNow()

	// Read should fail with timeout after helloTimeout
	testCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	_, _, err := conn.Read(testCtx)
	if err == nil {
		t.Error("expected error due to hello timeout")
	}
}

func TestRelayWS_ProxySessionOutlivesHelloTimeout(t *testing.T) {
	reg := relay.NewRegistry(2 * time.Second)
	cfg := config.Load()
	cfg.RelayJWTSecret = "secret"

	now := time.Unix(1_800_000_000, 0)
	reg.CreatePendingSession(relay.PendingSession{
		SID:          "sid-3",
		AccountID:    "acct-1",
		AgentID:      "agent-1",
		RelayAllowed: true,
		JTI:          "jti-3",
		ExpiresAt:    now.Add(relay.TokenLifetime),
	}, now)

	agentToken, _ := relay.SignAgentRelayJWT("secret", relay.AgentRelayClaims{SID: "sid-3", AgentID: "agent-1"}, now)
	browserToken, _ := relay.SignBrowserPolicyJWT("secret", relay.BrowserPolicyClaims{
		SID:              "sid-3",
		RelayAllowed:     true,
		RegisteredClaims: jwt.RegisteredClaims{ID: "jti-3"},
	}, now)

	mux := http.NewServeMux()
	mux.Handle("/ws/relay", handler.RelayWS(nil, reg, cfg))
	server := httptest.NewServer(mux)
	defer server.Close()

	originalTimeout := handler.HelloTimeout()
	defer handler.SetHelloTimeout(originalTimeout)
	handler.SetHelloTimeout(100 * time.Millisecond)

	ctx := context.Background()
	agentConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/relay", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer agentConn.CloseNow()
	agentConn.Write(ctx, websocket.MessageText, []byte(`{"token":"`+agentToken+`"}`))

	browserConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/relay", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer browserConn.CloseNow()
	browserConn.Write(ctx, websocket.MessageText, []byte(`{"token":"`+browserToken+`"}`))

	time.Sleep(250 * time.Millisecond)

	browserConn.Write(ctx, websocket.MessageBinary, []byte("after-timeout"))
	typ, data, err := agentConn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if typ != websocket.MessageBinary {
		t.Errorf("expected binary, got %v", typ)
	}
	if string(data) != "after-timeout" {
		t.Errorf("data = %q, want 'after-timeout'", data)
	}
}
