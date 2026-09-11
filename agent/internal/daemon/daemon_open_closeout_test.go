package daemon

// M4 closeout batch 1 — remediation-introduced findings 1, 2 and the no-false-
// fencing control, from the post-remediation Sol audit.
//
// Finding 1 (Important): Unlock published locked=false, an unlocked port
// generation, and an open SignalGate BEFORE the ordered listener handoff
// succeeded. A failed handoff therefore left direct-open admission OPEN, so a
// later open signal could create a router mapping and ack OK while the daemon
// claimed to stay out of service. The fix keeps direct-open eligibility fenced
// (locked port stamp + locked gate) until the replacement listener is confirmed
// serving, and restores the locked stamp on unlock failure.
//
// Finding 2 (Important): ackOpenSuccess returned false on an OK-ack transport
// error without tearing the mapping back down, so a WebSocket loss after the
// endpoint report could leave the mapping live for its lease. The fix discards
// the created mapping (discovered identity) on every unsuccessful OK-ack send.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sharebridge/agent/internal/direct"
	"sharebridge/agent/internal/signaling"
)

// failOpenAckClient fails ONLY the status-"ok" open_ack write, leaving the
// endpoint report and error acks intact — the WebSocket-loss-after-report
// window of finding 2.
type failOpenAckClient struct {
	SignalingClientInterface
	failOK atomic.Bool
}

func (c *failOpenAckClient) OpenAck(ctx context.Context, ack signaling.OpenAck) error {
	if ack.Status == "ok" && c.failOK.Load() {
		return errors.New("signaling write failed")
	}
	return c.SignalingClientInterface.OpenAck(ctx, ack)
}

// mappingCount counts the router mappings the fixture's recording mapper
// currently holds.
func mappingCount(m *recordingDirectMapper) int {
	list, _ := m.ListPortMappings()
	return len(list)
}

// wireOpenPath gives a lockdownFixture the open-signal prerequisites
// (baseline readiness + a real Reporter over the mock signaling client) so
// handleOpenSignal runs the production direct-open flow end to end.
func wireOpenPath(t *testing.T, fx *lockdownFixture) {
	t.Helper()
	ds := fx.ds
	ds.ready = true
	ds.reporter = direct.NewReporter(func(ctx context.Context, ip string, port int, status string) error {
		return fx.sig.ReportEndpoint(ctx, ip, port, status)
	})
	ds.port.SetTransitionCallback(ds.reporter.OnTransition)
	t.Cleanup(ds.reporter.Close)
}

// TestUnlockHandoffFailureFencesDirectOpen is the finding-1 proof: after an
// Unlock whose ordered listener handoff failed closed, an open signal must not
// create a router mapping and must not be acked OK, the port's generation stamp
// must still carry the locked bit, and the gate must still refuse opens. Once
// the old socket is released and the retried Unlock completes the handoff, a
// normal open succeeds end to end (the no-false-fencing control).
func TestUnlockHandoffFailureFencesDirectOpen(t *testing.T) {
	fx := newLockdownFixture(t)
	ds := fx.ds
	wireOpenPath(t, fx)
	ds.handoffTimeout = 100 * time.Millisecond
	g := newListenerGates()
	ds.startListenerFn = g.start

	require.NoError(t, fx.d.startDirectServer())
	awaitRecv(t, g.firstBound, "listener never bound")
	require.NoError(t, fx.d.Lockdown())
	awaitRecv(t, g.cancelSeen, "lockdown never cancelled the listener")

	// The old listener holds its socket past the bound, so Unlock cannot prove
	// the handoff and must report the failure.
	require.Error(t, fx.d.Unlock(), "unlock must report the fail-closed handoff")

	// (a) The locked port stamp is restored/preserved: direct-open eligibility
	// is still fenced. The gate half of the fence is asserted by the rejected
	// open below (Admit refuses while locked).
	if _, locked := ds.port.Generation(); !locked {
		t.Fatalf("the port generation stamp lost its locked bit after a failed unlock")
	}

	// (b) An open attempt during the fenced window must not map or ack OK.
	fx.d.handleOpenSignal(openSignalMessage())
	if fx.sig.hasSentMessage("open_ack", map[string]any{"status": "ok"}) {
		t.Fatalf("an OK open_ack was sent while the listener was not confirmed serving: %#v", fx.sig.messagesSnapshot())
	}
	if !fx.sig.hasSentMessage("open_ack", map[string]any{"status": "error", "error": "rejected"}) {
		t.Fatalf("expected a rejected error open_ack while fenced, got %#v", fx.sig.messagesSnapshot())
	}
	if ds.port.Open() {
		t.Fatalf("a fenced open must not leave the mapping open")
	}
	if got := mappingCount(fx.mapper); got != 0 {
		t.Fatalf("router mappings after a fenced open = %d, want 0", got)
	}

	// (c) Recovery: release the old socket, retry Unlock, and prove a normal
	// open succeeds end to end (no false fencing).
	close(g.releaseFirst)
	awaitRecv(t, g.oldExited, "old listener never released its socket")
	require.NoError(t, fx.d.Unlock())
	require.False(t, fx.d.IsLocked())
	if _, locked := ds.port.Generation(); locked {
		t.Fatalf("the port generation stamp must be unlocked after a successful unlock")
	}

	fx.d.handleOpenSignal(openSignalMessageAt(2, "nonce-after-recovery"))
	if !fx.sig.hasSentMessage("open_ack", map[string]any{"status": "ok"}) {
		t.Fatalf("a normal open after recovery must succeed, got %#v", fx.sig.messagesSnapshot())
	}
	if !ds.port.Open() {
		t.Fatalf("the recovered open must leave the mapping open")
	}
	if got := mappingCount(fx.mapper); got != 1 {
		t.Fatalf("router mappings after the recovered open = %d, want 1", got)
	}
}

// TestOpenAckSendFailureDiscardsMapping is the finding-2 proof: with the
// endpoint report confirmed but the OK open_ack transport failing, the mapping
// must be torn down (discovered identity) instead of surviving for its lease,
// and a later open over a working transport must still succeed.
func TestOpenAckSendFailureDiscardsMapping(t *testing.T) {
	f := newReportOrderFixture(t, newRemapDirectMapper(52031))
	client := &failOpenAckClient{SignalingClientInterface: f.client}
	f.daemon.signaling = client

	client.failOK.Store(true)
	f.daemon.handleOpenSignal(openSignalMessage())

	if f.client.hasSentMessage("open_ack", map[string]any{"status": "ok"}) {
		t.Fatalf("an OK open_ack reached the client although its transport write failed: %#v", f.client.messagesSnapshot())
	}
	if !f.client.hasSentMessage("open_ack", map[string]any{"status": "error"}) {
		t.Fatalf("expected an error open_ack after the failed OK send, got %#v", f.client.messagesSnapshot())
	}
	if f.port.Open() {
		t.Fatalf("the mapping must not survive an undelivered OK open_ack")
	}
	if got := f.port.GrantedPort(); got != 0 {
		t.Fatalf("granted port = %d, want 0 after the rollback", got)
	}
	if listing, err := f.mapper.ListPortMappings(); err != nil || len(listing) != 0 {
		t.Fatalf("mappings after the rolled-back OK ack = %#v (err %v), want none", listing, err)
	}

	// A subsequent open with a working transport must not be wedged by the
	// rollback (the discard is a one-shot no-op once performed).
	client.failOK.Store(false)
	f.daemon.handleOpenSignal(openSignalMessageAt(2, "nonce-after-ack-failure"))
	if !f.client.hasSentMessage("open_ack", map[string]any{"status": "ok"}) {
		t.Fatalf("a later open with a working transport must succeed, got %#v", f.client.messagesSnapshot())
	}
	if !f.port.Open() {
		t.Fatalf("the later open must leave the mapping open")
	}
}
