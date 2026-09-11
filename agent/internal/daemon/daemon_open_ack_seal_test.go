package daemon

// M4 remediation round C, fix round 3: the transport-level OK-ack seal as the
// DAEMON wires it.
//
// Fix round 2 pinned a single *conventional* OK-ack writer with an AST guard.
// That pins today's shape, not the transport: signaling.Client.OpenAck is
// public, so a new caller in any file can construct a status-"ok" ack and send
// it without the generation/lock validation. The signaling package proves the
// transport refusal with test guards; this file proves the guard the daemon
// actually installs (Daemon.openAckGuard):
//
//   - it refuses an ack with no validation context (a bypass constructed by a
//     helper in another file), a stale generation, or a port the port did not
//     re-confirm;
//   - it approves only a currently-committed open, and an approving guard
//     combined with a REAL signaling client lets a validated ack through while
//     a sentinel proves the refused one never reached the wire.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"sharebridge/agent/internal/signaling"
)

// newDaemonSealClient starts a WebSocket server that forwards every message it
// reads onto the returned channel, connects a real signaling client, and drains
// the connect hello. Ordering on the single connection makes the sentinel check
// in TestBypassOKAckCannotReachTheWire exact.
func newDaemonSealClient(t *testing.T) (*signaling.Client, <-chan []byte) {
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
			received <- raw
		}
	}))
	t.Cleanup(server.Close)

	client := signaling.New("ws"+strings.TrimPrefix(server.URL, "http"), "api", "agent")
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

// awaitDaemonSealMessage returns the next message the server read.
func awaitDaemonSealMessage(t *testing.T, received <-chan []byte) map[string]any {
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

// openSealFixture builds a fixture whose real OnDemandPort is open at
// generation 0 (the pre-lockdown generation a normal open is admitted under)
// and returns the port's committed granted port.
func openSealFixture(t *testing.T, granted int) (*reportOrderFixture, int) {
	t.Helper()
	f := newReportOrderFixture(t, newRemapDirectMapper(granted))
	if err := f.port.OpenForIf("SHARE123", time.Minute, 0); err != nil {
		t.Fatalf("open port: %v", err)
	}
	if !f.port.Open() {
		t.Fatalf("port did not open")
	}
	return f, f.port.GrantedPort()
}

// TestOpenAckGuardRejectsUnvalidatedAck is the core bypass case: a valid
// open exists, but the ack carries no validation context (exactly what a new
// daemon-package caller constructing signaling.OpenAck{Status: "ok"} would
// send). The guard must refuse WITHOUT touching the port.
func TestOpenAckGuardRejectsUnvalidatedAck(t *testing.T) {
	f, granted := openSealFixture(t, 53001)
	ack := bypassOpenAck("SHARE123", "nonce-bypass", 1, granted)
	if err := f.daemon.openAckGuard(ack); err == nil {
		t.Fatalf("an OK ack with no validation context was approved")
	}
	if !f.port.Open() {
		t.Fatalf("a nil-context refusal must not touch the port")
	}
}

// TestOpenAckGuardRejectsStaleGeneration: the validation context names a
// generation a §13.4 transition has since superseded. The guard must refuse AND
// the port must discard the superseded mapping — the same end state as the
// choke point's own CommitOpenAck.
func TestOpenAckGuardRejectsStaleGeneration(t *testing.T) {
	f, granted := openSealFixture(t, 53002)
	// Approve once with the current generation, proving the guard is not
	// blanket-refusing while the port is open at gen 0.
	if err := f.daemon.openAckGuard(withValidation(granted, 0)); err != nil {
		t.Fatalf("current generation was refused: %v", err)
	}
	f.port.SetGeneration(1, true) // the lockdown publishes a new generation
	if err := f.daemon.openAckGuard(withValidation(granted, 0)); err == nil {
		t.Fatalf("a superseded generation was approved")
	}
	if f.port.Open() {
		t.Fatalf("the superseded guard refusal must discard the mapping")
	}
}

// TestOpenAckGuardRejectsPortMismatch: a current generation with a port the
// port did not re-confirm is refused, and the refusal must not disturb the
// still-current mapping.
func TestOpenAckGuardRejectsPortMismatch(t *testing.T) {
	f, granted := openSealFixture(t, 53003)
	if err := f.daemon.openAckGuard(withValidation(granted+1, 0)); err == nil {
		t.Fatalf("an ack advertising the wrong granted port was approved")
	}
	if !f.port.Open() {
		t.Fatalf("a port-mismatch refusal must not touch the still-current mapping")
	}
	if err := f.daemon.openAckGuard(withValidation(granted, 0)); err != nil {
		t.Fatalf("the matching ack must still be approved: %v", err)
	}
}

// TestTransportRefusesOKOpenAckWithoutDaemonGuard pins the transport property
// without any network: a real signaling client with no guard installed refuses
// an OK open_ack at the seal, before it ever inspects the connection.
func TestTransportRefusesOKOpenAckWithoutDaemonGuard(t *testing.T) {
	client := signaling.New("ws://127.0.0.1:1", "api", "agent")
	err := client.OpenAck(context.Background(), withValidation(443, 0))
	if err == nil {
		t.Fatalf("OK open_ack with no guard installed was not refused")
	}
	if !strings.Contains(err.Error(), "no OK-ack guard installed") {
		t.Fatalf("refusal did not come from the transport seal: %v", err)
	}
}

// TestBypassOKAckCannotReachTheWire is the end-to-end property: a helper in
// another file (the "new daemon-package caller") constructs a status-"ok" ack
// without the choke point's validation context and sends it through a REAL
// signaling client carrying the daemon's guard. It must fail, and a sentinel
// must be the first thing on the wire. The validated ack the choke point would
// build is then accepted.
func TestBypassOKAckCannotReachTheWire(t *testing.T) {
	f, granted := openSealFixture(t, 53004)
	client, received := newDaemonSealClient(t)
	if err := client.SetOpenAckGuard(f.daemon.openAckGuard); err != nil {
		t.Fatalf("SetOpenAckGuard: %v", err)
	}

	// 1. Bare bypass: no validation context (the helper does not know about
	//    the transport seal).
	if err := client.OpenAck(context.Background(), bypassOpenAck("SHARE123", "nonce-bypass", 1, granted)); err == nil {
		t.Fatalf("an unvalidated OK ack was accepted by the transport")
	}
	// 2. Assignment-form bypass: the static guard also flags this shape, and
	//    the transport seal renders it harmless because it carries no context.
	assigned := bypassOpenAck("SHARE123", "nonce-assigned", 2, granted)
	assigned.Status = "ok"
	if err := client.OpenAck(context.Background(), assigned); err == nil {
		t.Fatalf("an assignment-form OK ack with no validation context was accepted")
	}
	// Neither refusal wrote anything: a sentinel must be the first message.
	if err := client.SubmitCSR(context.Background(), "seal-sentinel"); err != nil {
		t.Fatalf("sentinel send: %v", err)
	}
	if got := awaitDaemonSealMessage(t, received); got["type"] != "csr_submit" {
		t.Fatalf("a bypass OK ack reached the wire before the sentinel: %#v", got)
	}

	// 3. The ack the choke point builds (validation context + committed port)
	//    is approved and written.
	if err := client.OpenAck(context.Background(), withValidation(granted, 0)); err != nil {
		t.Fatalf("a validated OK ack was refused: %v", err)
	}
	got := awaitDaemonSealMessage(t, received)
	if got["type"] != "open_ack" || got["status"] != "ok" || got["granted_port"] != float64(granted) {
		t.Fatalf("validated OK ack wire shape = %#v", got)
	}
}
