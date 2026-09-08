package signaling

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"sharebridge/agent/internal/tunnel"
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

// newConnectedClient returns a Client connected to a quiet WebSocket test
// server (reads hello, then holds the connection). It is used for
// sender-validation tests where nothing must be written.
func newConnectedClient(t *testing.T) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if _, _, err := conn.Read(r.Context()); err != nil { // hello
			return
		}
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	client := New("ws"+strings.TrimPrefix(server.URL, "http"), "api", "agent")
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return client
}

// assertExactlyKeys asserts the received message carries exactly the given
// field set — the shared-contract guard for the §11.1 wire shapes (extra
// fields would leak payload material or drift from the control-side parser).
func assertExactlyKeys(t *testing.T, got map[string]any, wantKeys ...string) {
	t.Helper()
	want := make(map[string]bool, len(wantKeys))
	for _, key := range wantKeys {
		want[key] = true
	}
	if len(got) != len(want) {
		t.Fatalf("message has %d fields (%v), want exactly %d: %v", len(got), got, len(want), wantKeys)
	}
	for key := range got {
		if !want[key] {
			t.Fatalf("message has unexpected field %q; exact §11.1 field set is %v", key, wantKeys)
		}
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
		RelayOrigin:    "abc123.relay.sharebridgeusercontent.com",
		Challenge:      "5b1e8f22a94c03d76bf091382eaa475c.112a435c758ea7c0d9f20b243d566f88a1bad3ec051e375069829bb4cde6ff18",
		Server:         "control.example.net:3478",
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

// fixed STUN test material mirroring the sizes the §10.1 flow guarantees:
// 12-byte transaction ID and a 16-byte opaque receipt, both hex-encoded on
// the wire (lowercase, deterministic — the shared contract with Task 18's
// control-side stun_result claim).
var (
	stunResultChallenge = "5b1e8f22a94c03d76bf091382eaa475c"
	stunResultTxnID     = "0123456789abcdef01234567"
	stunResultReceipt   = []byte{
		0xde, 0xad, 0xbe, 0xef, 0x01, 0x23, 0x45, 0x67,
		0x89, 0xab, 0xcd, 0xef, 0xfe, 0xdc, 0xba, 0x98,
	}
)

func TestSTUNResultWireShape(t *testing.T) {
	t.Run("exact_field_set_and_values", func(t *testing.T) {
		got := captureAgentMessage(t, func(ctx context.Context, c *Client) error {
			return c.SendSTUNResult(ctx, STUNResult{
				Challenge:     stunResultChallenge,
				TransactionID: stunResultTxnID,
				Receipt:       stunResultReceipt,
			})
		})
		assertExactlyKeys(t, got, "type", "challenge", "transaction_id", "receipt")
		if got["type"] != "stun_result" {
			t.Fatalf("type = %v, want stun_result", got["type"])
		}
		if got["challenge"] != stunResultChallenge {
			t.Fatalf("challenge = %v, want %q", got["challenge"], stunResultChallenge)
		}
		if got["transaction_id"] != stunResultTxnID {
			t.Fatalf("transaction_id = %v, want %q", got["transaction_id"], stunResultTxnID)
		}
		if want := hex.EncodeToString(stunResultReceipt); got["receipt"] != want {
			t.Fatalf("receipt = %v, want %q (lowercase hex of the opaque attribute bytes)", got["receipt"], want)
		}
	})

	t.Run("rejects_invalid_payloads_without_writing", func(t *testing.T) {
		client := newConnectedClient(t)
		ctx := context.Background()
		cases := map[string]STUNResult{
			"empty challenge":  {Challenge: "", TransactionID: stunResultTxnID, Receipt: stunResultReceipt},
			"long challenge":   {Challenge: strings.Repeat("a", 129), TransactionID: stunResultTxnID, Receipt: stunResultReceipt},
			"short txn":        {Challenge: stunResultChallenge, TransactionID: "0123456789abcdef", Receipt: stunResultReceipt},
			"non-hex txn":      {Challenge: stunResultChallenge, TransactionID: strings.Repeat("zz", 12), Receipt: stunResultReceipt},
			"empty receipt":    {Challenge: stunResultChallenge, TransactionID: stunResultTxnID, Receipt: nil},
			"oversize receipt": {Challenge: stunResultChallenge, TransactionID: stunResultTxnID, Receipt: bytes.Repeat([]byte{0xaa}, 129)},
		}
		for name, result := range cases {
			if err := client.SendSTUNResult(ctx, result); err == nil {
				t.Fatalf("%s: SendSTUNResult must reject the payload", name)
			}
		}
	})
}

func TestRelayConfigAndClientStateWireShapes(t *testing.T) {
	t.Run("relay_config_exact_wire_shape_parses_through_tunnel", func(t *testing.T) {
		// The exact §11.1 relay_config payload control emits (Task 6
		// RelayConfigMessage) must parse through the agent's strict parser
		// unchanged: this is the shared-contract guard for Tasks 6/9/28.
		wire := `{"type":"relay_config","version":1,"generation":3,` +
			`"gateway_addr":"relay.example.net","gateway_port":7000,` +
			`"proxy_name":"agent-3-relay","relay_port":41000,` +
			`"credential":"eyJhbGciOiJFZERTQSJ9.eyJhcGlfa2V5X2lkIjoiYWdlbnQxIn0.dGVzdF9zaWduYXR1cmU",` +
			`"expires_at":"2026-09-03T12:00:00Z"}`
		config, err := tunnel.ParseRelayConfig([]byte(wire))
		if err != nil {
			t.Fatalf("ParseRelayConfig: %v", err)
		}
		if config.Version != 1 || config.Generation != 3 {
			t.Fatalf("version/generation = %d/%d, want 1/3", config.Version, config.Generation)
		}
		if config.GatewayAddr != "relay.example.net" || config.GatewayPort != 7000 {
			t.Fatalf("gateway = %s:%d, want relay.example.net:7000", config.GatewayAddr, config.GatewayPort)
		}
		if config.ProxyName != "agent-3-relay" || config.RelayPort != 41000 {
			t.Fatalf("proxy/relay port = %s/%d, want agent-3-relay/41000", config.ProxyName, config.RelayPort)
		}
		if config.Credential != "eyJhbGciOiJFZERTQSJ9.eyJhcGlfa2V5X2lkIjoiYWdlbnQxIn0.dGVzdF9zaWduYXR1cmU" {
			t.Fatalf("credential = %q, want the exact wire value", config.Credential)
		}
		if config.ExpiresAt.IsZero() || config.ExpiresAt.Format(time.RFC3339) != "2026-09-03T12:00:00Z" {
			t.Fatalf("expires_at = %v, want 2026-09-03T12:00:00Z", config.ExpiresAt)
		}
	})

	t.Run("relay_client_state_exact_field_set", func(t *testing.T) {
		got := captureAgentMessage(t, func(ctx context.Context, c *Client) error {
			return c.SendRelayClientState(ctx, RelayClientState{
				Generation: 3,
				Status:     RelayStatusStarting,
				Reason:     "frpc started",
			})
		})
		assertExactlyKeys(t, got, "type", "generation", "status", "reason")
		if got["type"] != "relay_client_state" {
			t.Fatalf("type = %v, want relay_client_state", got["type"])
		}
		if got["generation"] != float64(3) {
			t.Fatalf("generation = %v, want 3", got["generation"])
		}
		if got["status"] != "starting" {
			t.Fatalf("status = %v, want starting", got["status"])
		}
		if got["reason"] != "frpc started" {
			t.Fatalf("reason = %v, want %q", got["reason"], "frpc started")
		}
	})

	t.Run("relay_client_state_omits_empty_reason", func(t *testing.T) {
		got := captureAgentMessage(t, func(ctx context.Context, c *Client) error {
			return c.SendRelayClientState(ctx, RelayClientState{Generation: 0, Status: RelayStatusRunning})
		})
		assertExactlyKeys(t, got, "type", "generation", "status")
		if got["status"] != "running" {
			t.Fatalf("status = %v, want running", got["status"])
		}
	})

	t.Run("relay_client_state_bounds_oversize_reason", func(t *testing.T) {
		// Control rejects reasons beyond 256 bytes; the bounded sender
		// truncates instead of losing the whole telemetry message.
		got := captureAgentMessage(t, func(ctx context.Context, c *Client) error {
			return c.SendRelayClientState(ctx, RelayClientState{
				Generation: 1,
				Status:     RelayStatusError,
				Reason:     strings.Repeat("a", 300),
			})
		})
		reason, ok := got["reason"].(string)
		if !ok {
			t.Fatalf("reason missing or not a string: %v", got["reason"])
		}
		if len(reason) != 256 {
			t.Fatalf("reason length = %d, want 256 (control's telemetry bound)", len(reason))
		}
	})

	t.Run("relay_client_state_truncates_on_rune_boundary", func(t *testing.T) {
		got := captureAgentMessage(t, func(ctx context.Context, c *Client) error {
			return c.SendRelayClientState(ctx, RelayClientState{
				Generation: 1,
				Status:     RelayStatusError,
				Reason:     strings.Repeat("é", 200), // 400 bytes; cut at 256 must not split the rune
			})
		})
		reason, ok := got["reason"].(string)
		if !ok {
			t.Fatalf("reason missing or not a string: %v", got["reason"])
		}
		if len(reason) != 256 || utf8.RuneCountInString(reason) != 128 {
			t.Fatalf("truncated reason = %d bytes / %d runes, want 256 bytes / 128 runes", len(reason), utf8.RuneCountInString(reason))
		}
	})

	t.Run("relay_client_state_rejects_invalid_status_and_generation", func(t *testing.T) {
		client := newConnectedClient(t)
		ctx := context.Background()
		if err := client.SendRelayClientState(ctx, RelayClientState{Generation: -1, Status: RelayStatusRunning}); err == nil {
			t.Fatalf("negative generation must be rejected (control rejects it)")
		}
		if err := client.SendRelayClientState(ctx, RelayClientState{Generation: 1, Status: RelayClientStatus("restarting")}); err == nil {
			t.Fatalf("unknown status must be rejected (closed §11.1 enum)")
		}
		if err := client.SendRelayClientState(ctx, RelayClientState{Generation: 1, Status: ""}); err == nil {
			t.Fatalf("empty status must be rejected")
		}
	})

	t.Run("relay_credential_request_exact_field_set", func(t *testing.T) {
		// §11.1: { reason: "replay_rejected"|"expired"|"restart" } plus the
		// framing "type" — exactly two fields, nothing more.
		for _, reason := range []tunnel.CredentialRequestReason{
			tunnel.ReasonReplayRejected, tunnel.ReasonExpired, tunnel.ReasonRestart,
		} {
			got := captureAgentMessage(t, func(ctx context.Context, c *Client) error {
				return c.SendRelayCredentialRequest(ctx, reason)
			})
			assertExactlyKeys(t, got, "type", "reason")
			if got["type"] != "relay_credential_request" {
				t.Fatalf("type = %v, want relay_credential_request", got["type"])
			}
			if got["reason"] != string(reason) {
				t.Fatalf("reason = %v, want %q", got["reason"], string(reason))
			}
		}
	})

	t.Run("relay_credential_request_rejects_unknown_reason", func(t *testing.T) {
		client := newConnectedClient(t)
		ctx := context.Background()
		if err := client.SendRelayCredentialRequest(ctx, tunnel.CredentialRequestReason("because_i_said_so")); err == nil {
			t.Fatal("unknown reason must be rejected (closed §11.1 enum; control strictly parses it)")
		}
		if err := client.SendRelayCredentialRequest(ctx, ""); err == nil {
			t.Fatal("empty reason must be rejected")
		}
	})

	t.Run("parse_relay_config_via_signaling_reuses_strict_parser", func(t *testing.T) {
		// The receive-side helper must be Task 9's strict parser (unknown
		// fields and trailing data rejected), not a second looser parser.
		wire := `{"type":"relay_config","version":1,"generation":2,` +
			`"gateway_addr":"relay.example.net","gateway_port":7000,` +
			`"proxy_name":"agent-2-relay","relay_port":41001,` +
			`"credential":"a.b.c","expires_at":"2026-09-03T12:00:00Z"}`
		config, err := ParseRelayConfig([]byte(wire))
		if err != nil {
			t.Fatalf("ParseRelayConfig: %v", err)
		}
		if config.Generation != 2 || config.RelayPort != 41001 {
			t.Fatalf("generation/relay_port = %d/%d, want 2/41001", config.Generation, config.RelayPort)
		}
		if _, err := ParseRelayConfig([]byte(`{"type":"relay_config","version":1,"generation":2,"gateway_addr":"g","gateway_port":7000,"proxy_name":"p","relay_port":1,"credential":"a.b.c","expires_at":"2026-09-03T12:00:00Z","extra":1}`)); err == nil {
			t.Fatal("unknown field must be rejected (strict parse)")
		}
		if _, err := ParseRelayConfig([]byte(wire + ` {"type":"relay_config"}`)); err == nil {
			t.Fatal("trailing data must be rejected (strict parse)")
		}
	})

	t.Run("lockdown_status_exact_field_set", func(t *testing.T) {
		for _, locked := range []bool{true, false} {
			got := captureAgentMessage(t, func(ctx context.Context, c *Client) error {
				return c.SendLockdownStatus(ctx, LockdownStatus{Generation: 5, Locked: locked})
			})
			assertExactlyKeys(t, got, "type", "generation", "locked")
			if got["type"] != "lockdown_status" {
				t.Fatalf("type = %v, want lockdown_status", got["type"])
			}
			if got["generation"] != float64(5) {
				t.Fatalf("generation = %v, want 5", got["generation"])
			}
			if got["locked"] != locked {
				t.Fatalf("locked = %v, want %v (always present, both polarities)", got["locked"], locked)
			}
		}
	})

	t.Run("lockdown_status_rejects_negative_generation", func(t *testing.T) {
		client := newConnectedClient(t)
		if err := client.SendLockdownStatus(context.Background(), LockdownStatus{Generation: -2, Locked: true}); err == nil {
			t.Fatalf("negative generation must be rejected (control rejects it)")
		}
	})

	t.Run("status_vocabulary_matches_tunnel_manager", func(t *testing.T) {
		// The §11.1 relay_client_state status set must stay identical to the
		// tunnel manager's diagnostics vocabulary (Task 9 StatusReport).
		if RelayStatusStarting != RelayClientStatus(tunnel.StatusStarting) {
			t.Fatalf("starting mismatch: %q vs %q", RelayStatusStarting, tunnel.StatusStarting)
		}
		if RelayStatusRunning != RelayClientStatus(tunnel.StatusRunning) {
			t.Fatalf("running mismatch: %q vs %q", RelayStatusRunning, tunnel.StatusRunning)
		}
		if RelayStatusStopped != RelayClientStatus(tunnel.StatusStopped) {
			t.Fatalf("stopped mismatch: %q vs %q", RelayStatusStopped, tunnel.StatusStopped)
		}
		if RelayStatusError != RelayClientStatus(tunnel.StatusError) {
			t.Fatalf("error mismatch: %q vs %q", RelayStatusError, tunnel.StatusError)
		}
	})
}

func TestParseSTUNChallengeTolerantStrict(t *testing.T) {
	const goldenShape = `{"type":"stun_challenge","version":1,` +
		`"challenge":"5b1e8f22a94c03d76bf091382eaa475c.112a435c758ea7c0d9f20b243d566f88a1bad3ec051e375069829bb4cde6ff18",` +
		`"server":"control.example.net:3478","expires_at":"2026-09-03T12:01:00Z"}`

	t.Run("parses_exact_shape", func(t *testing.T) {
		challenge, err := ParseSTUNChallenge([]byte(goldenShape))
		if err != nil {
			t.Fatalf("ParseSTUNChallenge: %v", err)
		}
		if challenge.Version != 1 {
			t.Fatalf("version = %d, want 1", challenge.Version)
		}
		wantChallenge := "5b1e8f22a94c03d76bf091382eaa475c.112a435c758ea7c0d9f20b243d566f88a1bad3ec051e375069829bb4cde6ff18"
		if challenge.Challenge != wantChallenge {
			t.Fatalf("challenge = %q, want %q", challenge.Challenge, wantChallenge)
		}
		if challenge.Server != "control.example.net:3478" {
			t.Fatalf("server = %q, want control.example.net:3478", challenge.Server)
		}
		wantExpiry, err := time.Parse(time.RFC3339, "2026-09-03T12:01:00Z")
		if err != nil {
			t.Fatalf("parse test timestamp: %v", err)
		}
		if !challenge.ExpiresAt.Equal(wantExpiry) {
			t.Fatalf("expires_at = %v, want %v", challenge.ExpiresAt, wantExpiry)
		}
	})

	t.Run("tolerates_unknown_fields", func(t *testing.T) {
		// Task 7 ruling: unknown JSON fields stay tolerated for versioned
		// wire compatibility (spec §11), trailing data is still rejected.
		withUnknown := `{"type":"stun_challenge","version":1,` +
			`"challenge":"a.b","server":"s:3478","expires_at":"2026-09-03T12:01:00Z",` +
			`"future_field":{"nested":true}}`
		challenge, err := ParseSTUNChallenge([]byte(withUnknown))
		if err != nil {
			t.Fatalf("ParseSTUNChallenge with unknown field: %v", err)
		}
		if challenge.Challenge != "a.b" || challenge.Server != "s:3478" {
			t.Fatalf("known fields corrupted by unknown-field tolerance: %+v", challenge)
		}
	})

	t.Run("rejects_trailing_data", func(t *testing.T) {
		if _, err := ParseSTUNChallenge([]byte(goldenShape + ` {"future":true}`)); err == nil {
			t.Fatalf("trailing data after the stun_challenge value must be rejected")
		}
	})

	t.Run("rejects_wrong_type", func(t *testing.T) {
		wrong := `{"type":"relay_config","version":1,"challenge":"a.b","server":"s:3478","expires_at":"2026-09-03T12:01:00Z"}`
		if _, err := ParseSTUNChallenge([]byte(wrong)); err == nil {
			t.Fatalf("message type relay_config must not parse as stun_challenge")
		}
	})

	t.Run("rejects_missing_or_malformed_expires_at", func(t *testing.T) {
		missing := `{"type":"stun_challenge","version":1,"challenge":"a.b","server":"s:3478"}`
		if _, err := ParseSTUNChallenge([]byte(missing)); err == nil {
			t.Fatalf("missing expires_at must be rejected")
		}
		malformed := `{"type":"stun_challenge","version":1,"challenge":"a.b","server":"s:3478","expires_at":"not-a-time"}`
		if _, err := ParseSTUNChallenge([]byte(malformed)); err == nil {
			t.Fatalf("malformed expires_at must be rejected")
		}
	})
}
