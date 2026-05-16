package signaling

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func newRegisterShareTestServer(t *testing.T, handle func(t *testing.T, raw []byte) []byte) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Fatalf("Accept: %v", err)
		}
		defer conn.CloseNow()

		ctx := r.Context()
		_, _, err = conn.Read(ctx) // hello
		if err != nil {
			t.Fatalf("Read hello: %v", err)
		}

		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("Read message: %v", err)
		}

		resp := handle(t, raw)
		if len(resp) > 0 {
			if err := conn.Write(ctx, websocket.MessageText, resp); err != nil {
				t.Fatalf("Write response: %v", err)
			}
		}
	}))
}

func TestRegisterShare_IncludesImmichMetadata(t *testing.T) {
	server := newRegisterShareTestServer(t, func(t *testing.T, raw []byte) []byte {
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("Unmarshal register_share: %v", err)
		}
		if got["type"] != "register_share" {
			t.Fatalf("type = %v, want register_share", got["type"])
		}
		if got["share_type"] != "immich" {
			t.Fatalf("share_type = %v, want immich", got["share_type"])
		}
		if got["code"] != "ffSw63qnIYMt_aBcDeFgHiJkLmNoPqRsTuVwXyZ" {
			t.Fatalf("code = %v, want ffSw63qnIYMt_aBcDeFgHiJkLmNoPqRsTuVwXyZ", got["code"])
		}
		if got["share_url"] != "immich://ffSw63qnIYMt_aBcDeFgHiJkLmNoPqRsTuVwXyZ" {
			t.Fatalf("share_url = %v, want immich://ffSw63qnIYMt_aBcDeFgHiJkLmNoPqRsTuVwXyZ", got["share_url"])
		}
		if got["is_password_protected"] != true {
			t.Fatalf("is_password_protected = %v, want true", got["is_password_protected"])
		}
		if got["relay_only"] != true {
			t.Fatalf("relay_only = %v, want true", got["relay_only"])
		}
		if got["relay_static_pub"] != "04abcd" {
			t.Fatalf("relay_static_pub = %v, want 04abcd", got["relay_static_pub"])
		}
		return []byte(`{"type":"share_registered","code":"ffSw63qnIYMt_aBcDeFgHiJkLmNoPqRsTuVwXyZ"}`)
	})
	defer server.Close()

	client := New(strings.Replace(server.URL, "http://", "ws://", 1), "api", "agent")
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	go client.Listen(context.Background())

	opts := RegisterShareOptions{
		ShareURL:            "immich://ffSw63qnIYMt_aBcDeFgHiJkLmNoPqRsTuVwXyZ",
		PreferredCode:       "ffSw63qnIYMt_aBcDeFgHiJkLmNoPqRsTuVwXyZ",
		ShareType:           "immich",
		IsPasswordProtected: true,
		RelayOnly:           true,
		RelayStaticPub:      "04abcd",
	}
	code, _, err := client.RegisterShareWithOptions(context.Background(), opts)
	if err != nil {
		t.Fatalf("RegisterShareWithOptions: %v", err)
	}
	if code != opts.PreferredCode {
		t.Fatalf("code = %q, want %q", code, opts.PreferredCode)
	}
}

func TestRegisterShare_OmitsFalseImmichPasswordProtected(t *testing.T) {
	server := newRegisterShareTestServer(t, func(t *testing.T, raw []byte) []byte {
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("Unmarshal register_share: %v", err)
		}
		if _, ok := got["is_password_protected"]; ok {
			t.Fatalf("is_password_protected should be omitted when false: %s", string(raw))
		}
		return []byte(`{"type":"share_registered","code":"IMMICHNOPASS"}`)
	})
	defer server.Close()

	client := New(strings.Replace(server.URL, "http://", "ws://", 1), "api", "agent")
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	go client.Listen(context.Background())

	_, _, err := client.RegisterShareWithOptions(context.Background(), RegisterShareOptions{
		ShareURL:      "immich://IMMICHNOPASS",
		PreferredCode: "IMMICHNOPASS",
		ShareType:     "immich",
	})
	if err != nil {
		t.Fatalf("RegisterShareWithOptions: %v", err)
	}
}

func TestRegisterShareWithOptions_RejectsConcurrentRegistration(t *testing.T) {
	firstReceived := make(chan struct{})
	releaseServer := make(chan struct{})
	defer close(releaseServer)
	var registerMessages atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Fatalf("Accept: %v", err)
		}
		defer conn.CloseNow()

		ctx := r.Context()
		_, _, err = conn.Read(ctx) // hello
		if err != nil {
			t.Fatalf("Read hello: %v", err)
		}

		_, _, err = conn.Read(ctx)
		if err != nil {
			t.Fatalf("Read first register_share: %v", err)
		}
		registerMessages.Add(1)
		close(firstReceived)

		readCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
		defer cancel()
		if _, _, err := conn.Read(readCtx); err == nil {
			registerMessages.Add(1)
		}

		<-releaseServer
	}))
	defer server.Close()

	client := New(strings.Replace(server.URL, "http://", "ws://", 1), "api", "agent")
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	go client.Listen(context.Background())

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, _, err := client.RegisterShareWithOptions(firstCtx, RegisterShareOptions{
			ShareURL:      "immich://FIRST",
			PreferredCode: "FIRST",
			ShareType:     "immich",
		})
		firstDone <- err
	}()

	<-firstReceived

	secondCtx, cancelSecond := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelSecond()
	_, _, secondErr := client.RegisterShareWithOptions(secondCtx, RegisterShareOptions{
		ShareURL:      "immich://SECOND",
		PreferredCode: "SECOND",
		ShareType:     "immich",
	})
	time.Sleep(250 * time.Millisecond)

	cancelFirst()
	firstErr := <-firstDone

	if secondErr == nil || secondErr.Error() != "registration already in progress" {
		t.Fatalf("second RegisterShareWithOptions error = %v, want registration already in progress", secondErr)
	}
	if got := registerMessages.Load(); got != 1 {
		t.Fatalf("register message count = %d, want 1", got)
	}
	if !errors.Is(firstErr, context.Canceled) {
		t.Fatalf("first RegisterShareWithOptions error = %v, want context canceled", firstErr)
	}
}

func TestUnregisterShare_SendsMessage(t *testing.T) {
	server := newRegisterShareTestServer(t, func(t *testing.T, raw []byte) []byte {
		want := `{"type":"unregister_share","code":"IMMICHDEL1"}`
		var gotJSON, wantJSON map[string]any
		if err := json.Unmarshal(raw, &gotJSON); err != nil {
			t.Fatalf("Unmarshal unregister_share: %v", err)
		}
		if err := json.Unmarshal([]byte(want), &wantJSON); err != nil {
			t.Fatalf("Unmarshal want: %v", err)
		}
		if gotJSON["type"] != wantJSON["type"] || gotJSON["code"] != wantJSON["code"] {
			t.Fatalf("unregister_share = %s, want %s", string(raw), want)
		}
		return []byte(`{"type":"share_unregistered","code":"IMMICHDEL1"}`)
	})
	defer server.Close()

	client := New(strings.Replace(server.URL, "http://", "ws://", 1), "api", "agent")
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := client.UnregisterShare(context.Background(), "IMMICHDEL1"); err != nil {
		t.Fatalf("UnregisterShare: %v", err)
	}
}

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

func TestRelayWebSocketURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{in: "wss://sharebridge.app", want: "wss://sharebridge.app/ws/relay"},
		{in: "ws://localhost:8787", want: "ws://localhost:8787/ws/relay"},
		{in: "https://sharebridge.app", want: "wss://sharebridge.app/ws/relay"},
		{in: "http://localhost:8787", want: "ws://localhost:8787/ws/relay"},
		{in: "wss://sharebridge.app/some/path?query=1", want: "wss://sharebridge.app/ws/relay"},
	}

	for _, tc := range cases {
		if got := RelayWebSocketURL(tc.in); got != tc.want {
			t.Fatalf("RelayWebSocketURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
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
