package signaling

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	code, _, _, err := client.RegisterShareWithOptions(context.Background(), opts)
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

	_, _, _, err := client.RegisterShareWithOptions(context.Background(), RegisterShareOptions{
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
		_, _, _, err := client.RegisterShareWithOptions(firstCtx, RegisterShareOptions{
			ShareURL:      "immich://FIRST",
			PreferredCode: "FIRST",
			ShareType:     "immich",
		})
		firstDone <- err
	}()

	<-firstReceived

	secondCtx, cancelSecond := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelSecond()
	_, _, _, secondErr := client.RegisterShareWithOptions(secondCtx, RegisterShareOptions{
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

	_, _, _, err := client.RegisterShare(ctx, "https://example.com/s/abc", "", false, "04abcd")
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
	_, _, _, err := client.RegisterShare(ctx, "https://example.com/s/xyz", "", false, "")
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

func TestMessageFieldsRoundTrip(t *testing.T) {
	in := Message{
		Type:           "open_signal",
		ShareID:        "SHARE123",
		Nonce:          "nonce-abc",
		Seq:            7,
		Route:          "direct",
		LeaseSeconds:   300,
		Version:        1,
		ExpiresAt:      "2026-08-15T12:00:00Z",
		CSRPEM:         "-----BEGIN CERTIFICATE REQUEST-----",
		Fingerprint:    "deadbeef",
		NotAfter:       "2026-09-15T12:00:00Z",
		IP:             "1.2.3.4",
		Port:           443,
		Status:         "ok",
		GrantedPort:    443,
		PublicIP:       "1.2.3.4",
		WasAlreadyOpen: true,
		Error:          "boom",
		Origin:         "abc123.sharebridgeusercontent.com",
	}

	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out Message
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round-trip mismatch:\n in: %+v\nout: %+v", in, out)
	}
}

// captureAgentMessage spins up a one-shot WebSocket server, connects a Client,
// runs sendFn, and returns the JSON-decoded message the server received.
func captureAgentMessage(t *testing.T, sendFn func(ctx context.Context, c *Client) error) map[string]any {
	t.Helper()
	gotCh := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		if _, _, err := conn.Read(ctx); err != nil { // hello
			return
		}
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return
		}
		gotCh <- raw
	}))
	defer server.Close()

	client := New("ws"+strings.TrimPrefix(server.URL, "http"), "api", "agent")
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := sendFn(context.Background(), client); err != nil {
		t.Fatalf("send helper: %v", err)
	}
	raw := <-gotCh
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal received message %q: %v", string(raw), err)
	}
	return got
}

func TestSendHelpers_WireShape(t *testing.T) {
	t.Run("SubmitCSR", func(t *testing.T) {
		got := captureAgentMessage(t, func(ctx context.Context, c *Client) error {
			return c.SubmitCSR(ctx, "-----BEGIN CERTIFICATE REQUEST-----")
		})
		if got["type"] != "csr_submit" {
			t.Fatalf("type = %v, want csr_submit", got["type"])
		}
		if got["csr_pem"] != "-----BEGIN CERTIFICATE REQUEST-----" {
			t.Fatalf("csr_pem = %v", got["csr_pem"])
		}
	})

	t.Run("ReportEndpoint_omits_status_when_empty", func(t *testing.T) {
		got := captureAgentMessage(t, func(ctx context.Context, c *Client) error {
			return c.ReportEndpoint(ctx, "1.2.3.4", 443, "")
		})
		if got["type"] != "report_endpoint" {
			t.Fatalf("type = %v, want report_endpoint", got["type"])
		}
		if got["ip"] != "1.2.3.4" || got["port"] != float64(443) {
			t.Fatalf("ip/port = %v/%v", got["ip"], got["port"])
		}
		if _, ok := got["status"]; ok {
			t.Fatalf("status should be omitted when empty, got %v", got["status"])
		}
	})

	t.Run("ReportEndpoint_close_failed", func(t *testing.T) {
		got := captureAgentMessage(t, func(ctx context.Context, c *Client) error {
			return c.ReportEndpoint(ctx, "1.2.3.4", 443, "close_failed")
		})
		if got["status"] != "close_failed" {
			t.Fatalf("status = %v, want close_failed", got["status"])
		}
	})

	t.Run("OpenAck", func(t *testing.T) {
		got := captureAgentMessage(t, func(ctx context.Context, c *Client) error {
			return c.OpenAck(ctx, OpenAck{
				ShareID: "SHARE123", Nonce: "n", Seq: 7,
				GrantedPort: 443, PublicIP: "1.2.3.4",
				WasAlreadyOpen: true, Status: "ok",
			})
		})
		if got["type"] != "open_ack" {
			t.Fatalf("type = %v, want open_ack", got["type"])
		}
		if got["share_id"] != "SHARE123" || got["nonce"] != "n" || got["seq"] != float64(7) {
			t.Fatalf("share_id/nonce/seq = %v/%v/%v", got["share_id"], got["nonce"], got["seq"])
		}
		if got["granted_port"] != float64(443) || got["public_ip"] != "1.2.3.4" {
			t.Fatalf("granted_port/public_ip = %v/%v", got["granted_port"], got["public_ip"])
		}
		if got["was_already_open"] != true || got["status"] != "ok" {
			t.Fatalf("was_already_open/status = %v/%v", got["was_already_open"], got["status"])
		}
		if _, ok := got["error"]; ok {
			t.Fatalf("error should be omitted when empty, got %v", got["error"])
		}
	})

	t.Run("OpenAck_error", func(t *testing.T) {
		got := captureAgentMessage(t, func(ctx context.Context, c *Client) error {
			return c.OpenAck(ctx, OpenAck{ShareID: "SHARE123", Nonce: "n", Seq: 7, Status: "error", Error: "open_failed"})
		})
		if got["error"] != "open_failed" {
			t.Fatalf("error = %v, want open_failed", got["error"])
		}
	})

	t.Run("TLSReady", func(t *testing.T) {
		got := captureAgentMessage(t, func(ctx context.Context, c *Client) error {
			return c.TLSReady(ctx, "deadbeef", "2026-09-15T12:00:00Z")
		})
		if got["type"] != "tls_ready" {
			t.Fatalf("type = %v, want tls_ready", got["type"])
		}
		if got["fingerprint"] != "deadbeef" || got["not_after"] != "2026-09-15T12:00:00Z" {
			t.Fatalf("fingerprint/not_after = %v/%v", got["fingerprint"], got["not_after"])
		}
	})

	t.Run("TLSError", func(t *testing.T) {
		got := captureAgentMessage(t, func(ctx context.Context, c *Client) error {
			return c.TLSError(ctx, "issuance failed")
		})
		if got["type"] != "tls_error" {
			t.Fatalf("type = %v, want tls_error", got["type"])
		}
		if got["reason"] != "issuance failed" {
			t.Fatalf("reason = %v, want issuance failed", got["reason"])
		}
	})
}

func TestRegisterShareWithOptions_ReturnsOrigin(t *testing.T) {
	server := newRegisterShareTestServer(t, func(t *testing.T, raw []byte) []byte {
		return []byte(`{"type":"share_registered","code":"SHARE123","origin":"abc123.sharebridgeusercontent.com","reconnected":true}`)
	})
	defer server.Close()

	client := New("ws"+strings.TrimPrefix(server.URL, "http"), "api", "agent")
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	go client.Listen(context.Background())

	code, origin, reconnected, err := client.RegisterShareWithOptions(context.Background(), RegisterShareOptions{
		ShareURL:      "immich://SHARE123",
		PreferredCode: "SHARE123",
	})
	if err != nil {
		t.Fatalf("RegisterShareWithOptions: %v", err)
	}
	if code != "SHARE123" {
		t.Fatalf("code = %q, want SHARE123", code)
	}
	if origin != "abc123.sharebridgeusercontent.com" {
		t.Fatalf("origin = %q, want abc123.sharebridgeusercontent.com", origin)
	}
	if !reconnected {
		t.Fatalf("reconnected = false, want true")
	}
}
