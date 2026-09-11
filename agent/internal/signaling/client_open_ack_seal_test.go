package signaling

// M4 remediation round C, fix round 3: the transport-level seal for
// status-"ok" open_acks.
//
// Fix round 2 established a single *conventional* OK-ack writer
// (Daemon.ackOpenSuccess) plus an AST guard over daemon.go. That pins today's
// shape but not the transport: signaling.Client.OpenAck is public, so any new
// caller in any package can construct a status-"ok" ack and send it without
// the generation/lock validation. This suite pins the transport property: an
// OK open_ack must be approved by a guard installed on the client, or it is
// never written.
//
// The two tests in this file compile and run against the pre-fix source. At
// 4d3dddc6 TestOpenAckOKWithoutGuardIsRefusedAndNotSent FAILS behaviourally
// (OpenAck returns nil and the OK ack reaches the wire) — that is the RED
// artifact. The guard-specific cases (rejecting/approving guard) drive the
// injected seam and are added with the seal; they cannot compile before it.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// newSealServerClient starts a WebSocket server that forwards every message it
// reads onto the returned channel, connects a Client, and drains the connect
// hello. WebSocket preserves ordering on one connection, which is what makes
// the sentinel check below exact.
func newSealServerClient(t *testing.T) (*Client, <-chan []byte) {
	t.Helper()
	received := make(chan []byte, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		for {
			_, raw, err := conn.Read(ctx)
			if err != nil {
				return
			}
			select {
			case received <- raw:
			default:
			}
		}
	}))
	t.Cleanup(server.Close)

	client := New("ws"+strings.TrimPrefix(server.URL, "http"), "api", "agent")
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	select {
	case <-received: // connect hello
	case <-time.After(2 * time.Second):
		t.Fatalf("server did not read the connect hello")
	}
	return client, received
}

// awaitSealMessage returns the next message the server read, failing on timeout.
func awaitSealMessage(t *testing.T, received <-chan []byte) map[string]any {
	t.Helper()
	select {
	case raw := <-received:
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("Unmarshal %q: %v", string(raw), err)
		}
		return got
	case <-time.After(2 * time.Second):
		t.Fatalf("no message reached the server")
		return nil
	}
}

// assertNothingWrittenBeforeSentinel writes an unguarded sentinel message and
// asserts the sentinel is the first thing on the wire. The seal test writes it
// only after the refused OpenAck returned, so a would-be open_ack must precede
// it if one were written.
func assertNothingWrittenBeforeSentinel(t *testing.T, client *Client, received <-chan []byte) {
	t.Helper()
	if err := client.SubmitCSR(context.Background(), "seal-sentinel"); err != nil {
		t.Fatalf("sentinel send: %v", err)
	}
	got := awaitSealMessage(t, received)
	if got["type"] != "csr_submit" {
		t.Fatalf("a message preceded the sentinel, so an OK open_ack reached the wire: %#v", got)
	}
}

// TestOpenAckOKWithoutGuardIsRefusedAndNotSent is the core transport property:
// with NO OK-ack guard installed, a status-"ok" open_ack must fail closed and
// never reach the wire. Against the pre-fix source it returns nil and writes
// the ack — the RED failure.
func TestOpenAckOKWithoutGuardIsRefusedAndNotSent(t *testing.T) {
	client, received := newSealServerClient(t)
	err := client.OpenAck(context.Background(), OpenAck{
		ShareID: "SHARE123", Nonce: "n", Seq: 7,
		GrantedPort: 443, PublicIP: "1.2.3.4", Status: "ok",
	})
	if err == nil {
		t.Fatalf(`OpenAck(status "ok") with no guard returned nil, want a hard error`)
	}
	assertNothingWrittenBeforeSentinel(t, client, received)
}

// TestOpenAckErrorStatusWithoutGuardStillSends pins that the seal covers ONLY
// status "ok": an error ack must keep working with no guard installed.
func TestOpenAckErrorStatusWithoutGuardStillSends(t *testing.T) {
	client, received := newSealServerClient(t)
	if err := client.OpenAck(context.Background(), OpenAck{
		ShareID: "SHARE123", Nonce: "n", Seq: 7, Status: "error", Error: "open_failed",
	}); err != nil {
		t.Fatalf("error open_ack with no guard: %v", err)
	}
	got := awaitSealMessage(t, received)
	if got["type"] != "open_ack" || got["status"] != "error" || got["error"] != "open_failed" {
		t.Fatalf("error open_ack wire shape = %#v", got)
	}
}

// TestOpenAckOKWithRejectingGuardIsRefusedAndNotSent pins approval: a guard
// that refuses the ack turns the send into a hard error with nothing written.
func TestOpenAckOKWithRejectingGuardIsRefusedAndNotSent(t *testing.T) {
	client, received := newSealServerClient(t)
	if err := client.SetOpenAckGuard(func(OpenAck) error {
		return errors.New("rejected by test guard")
	}); err != nil {
		t.Fatalf("SetOpenAckGuard: %v", err)
	}
	err := client.OpenAck(context.Background(), OpenAck{
		ShareID: "SHARE123", Nonce: "n", Seq: 7,
		GrantedPort: 443, PublicIP: "1.2.3.4", Status: "ok",
	})
	if err == nil {
		t.Fatalf("OpenAck with a rejecting guard returned nil, want a hard error")
	}
	if !strings.Contains(err.Error(), "rejected by test guard") {
		t.Fatalf("error %q does not surface the guard's rejection", err)
	}
	assertNothingWrittenBeforeSentinel(t, client, received)
}

// TestOpenAckOKWithApprovingGuardSends pins the positive half of the seal: an
// approving guard lets the OK ack through with its wire shape intact.
func TestOpenAckOKWithApprovingGuardSends(t *testing.T) {
	client, received := newSealServerClient(t)
	var approved []OpenAck
	if err := client.SetOpenAckGuard(func(ack OpenAck) error {
		approved = append(approved, ack)
		return nil
	}); err != nil {
		t.Fatalf("SetOpenAckGuard: %v", err)
	}
	if err := client.OpenAck(context.Background(), OpenAck{
		ShareID: "SHARE123", Nonce: "n", Seq: 7,
		GrantedPort: 443, PublicIP: "1.2.3.4", WasAlreadyOpen: true, Status: "ok",
	}); err != nil {
		t.Fatalf("OpenAck with an approving guard: %v", err)
	}
	got := awaitSealMessage(t, received)
	if got["type"] != "open_ack" || got["status"] != "ok" {
		t.Fatalf("OK open_ack wire shape = %#v", got)
	}
	if got["share_id"] != "SHARE123" || got["nonce"] != "n" || got["seq"] != float64(7) ||
		got["granted_port"] != float64(443) || got["public_ip"] != "1.2.3.4" || got["was_already_open"] != true {
		t.Fatalf("OK open_ack fields = %#v", got)
	}
	if len(approved) != 1 || approved[0].ShareID != "SHARE123" || approved[0].Status != "ok" {
		t.Fatalf("guard approvals = %#v, want exactly the sent ack", approved)
	}
}

// TestSendRawOKOpenAckWithoutGuardIsRefused pins that the seal covers the raw
// transport entry too: a hand-rolled map that bypasses the typed OpenAck
// front-end is refused with no guard installed.
func TestSendRawOKOpenAckWithoutGuardIsRefused(t *testing.T) {
	client, received := newSealServerClient(t)
	err := client.Send(context.Background(), map[string]any{
		"type": "open_ack", "share_id": "SHARE123", "status": "ok",
	})
	if err == nil {
		t.Fatalf("a raw map OK open_ack was written with no guard installed")
	}
	assertNothingWrittenBeforeSentinel(t, client, received)
}

// TestSendRawOKOpenAckWithoutValidationContextIsRefused pins the case the
// static shape scan cannot see: the guard is installed, but a raw map carries
// no recoverable validation context, so it is refused.
func TestSendRawOKOpenAckWithoutValidationContextIsRefused(t *testing.T) {
	client, received := newSealServerClient(t)
	if err := client.SetOpenAckGuard(func(ack OpenAck) error {
		if ack.Validation == nil {
			return errors.New("no validation context")
		}
		return nil
	}); err != nil {
		t.Fatalf("SetOpenAckGuard: %v", err)
	}
	err := client.Send(context.Background(), map[string]any{
		"type": "open_ack", "share_id": "SHARE123", "nonce": "n", "seq": 7,
		"granted_port": 443, "public_ip": "1.2.3.4", "status": "ok",
	})
	if err == nil {
		t.Fatalf("a raw map OK open_ack with no validation context was written")
	}
	assertNothingWrittenBeforeSentinel(t, client, received)
}

// TestOpenAckGuardIsSetOnceAndNonNil pins the hardening: the gate cannot be
// widened by a later caller (no replacing it with a permissive guard) and
// cannot be cleared.
func TestOpenAckGuardIsSetOnceAndNonNil(t *testing.T) {
	client, _ := newSealServerClient(t)
	if err := client.SetOpenAckGuard(nil); err == nil {
		t.Fatalf("installing a nil guard succeeded, want an error")
	}
	if err := client.SetOpenAckGuard(func(OpenAck) error { return nil }); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if err := client.SetOpenAckGuard(func(OpenAck) error { return nil }); err == nil {
		t.Fatalf("re-installing the guard succeeded, want an error")
	}
}
