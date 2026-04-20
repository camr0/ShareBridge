package signaling

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestRegisterShare_IncludesRelayStaticPub(t *testing.T) {
	var payload map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Fatalf("Accept: %v", err)
		}
		defer conn.CloseNow()

		ctx := r.Context()
		_, helloBytes, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("Read hello: %v", err)
		}
		var hello map[string]any
		if err := json.Unmarshal(helloBytes, &hello); err != nil {
			t.Fatalf("Unmarshal hello: %v", err)
		}

		_, registerBytes, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("Read register_share: %v", err)
		}
		if err := json.Unmarshal(registerBytes, &payload); err != nil {
			t.Fatalf("Unmarshal register_share: %v", err)
		}

		resp, _ := json.Marshal(map[string]any{
			"type": "share_registered",
			"code": "SHARE1234",
		})
		_ = conn.Write(ctx, websocket.MessageText, resp)
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	client := New(wsURL, "key", "agent-1")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- client.Listen(ctx) }()

	_, _, err := client.RegisterShare(ctx, "https://example.com/s/abc", "", false, "04abcd")
	if err != nil {
		t.Fatalf("RegisterShare: %v", err)
	}

	if got := payload["relay_static_pub"]; got != "04abcd" {
		t.Fatalf("relay_static_pub = %v, want 04abcd", got)
	}
}

func TestRegisterShare_OmitsEmptyRelayStaticPub(t *testing.T) {
	var payload map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Fatalf("Accept: %v", err)
		}
		defer conn.CloseNow()

		ctx := r.Context()
		_, helloBytes, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("Read hello: %v", err)
		}
		var hello map[string]any
		if err := json.Unmarshal(helloBytes, &hello); err != nil {
			t.Fatalf("Unmarshal hello: %v", err)
		}

		_, registerBytes, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("Read register_share: %v", err)
		}
		if err := json.Unmarshal(registerBytes, &payload); err != nil {
			t.Fatalf("Unmarshal register_share: %v", err)
		}

		resp, _ := json.Marshal(map[string]any{
			"type": "share_registered",
			"code": "SHARE5678",
		})
		_ = conn.Write(ctx, websocket.MessageText, resp)
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	client := New(wsURL, "key", "agent-2")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- client.Listen(ctx) }()

	// Call with empty relayStaticPub
	_, _, err := client.RegisterShare(ctx, "https://example.com/s/xyz", "", false, "")
	if err != nil {
		t.Fatalf("RegisterShare: %v", err)
	}

	// Verify relay_static_pub is NOT in the payload
	if _, exists := payload["relay_static_pub"]; exists {
		t.Fatalf("relay_static_pub should be omitted when empty, but was present in payload")
	}
}

func TestListen_ParsesRelayPrepare(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Fatalf("Accept: %v", err)
		}
		defer conn.CloseNow()

		ctx := r.Context()
		_, _, _ = conn.Read(ctx) // hello

		payload, _ := json.Marshal(map[string]any{
			"type":       "relay_prepare",
			"sid":        "sid-123",
			"code":       "SHARE123",
			"expires_at": "2026-04-19T12:00:00Z",
			"relay_jwt":  "relay.jwt.token",
		})
		_ = conn.Write(ctx, websocket.MessageText, payload)
		<-ctx.Done()
	}))
	defer server.Close()

	client := New("ws"+strings.TrimPrefix(server.URL, "http"), "key", "agent-1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	got := make(chan Message, 1)
	client.SetOnMessage(func(msg Message) {
		if msg.Type == "relay_prepare" {
			got <- msg
		}
	})

	go client.Listen(ctx)

	msg := <-got
	if msg.SID != "sid-123" || msg.Code != "SHARE123" || msg.RelayJWT != "relay.jwt.token" || msg.ExpiresAt != "2026-04-19T12:00:00Z" {
		t.Fatalf("relay_prepare parsed incorrectly: %+v", msg)
	}
}
