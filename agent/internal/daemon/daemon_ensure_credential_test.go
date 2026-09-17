package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sharebridge/agent/internal/signaling"
	"sharebridge/agent/internal/tunnel"
)

// These tests pin the sibling release-blocking defect fixed together with the
// tunnel reconnect recovery: the agent's bootstrap/unlock relay credential
// request was a ONE-SHOT direct WebSocket send with no retry. A reconnect
// storm can consume control's relay_credential_request burst (4 immediate
// requests, one refill per 15s), and the final request was then silently
// dropped — leaving the relay down with the agent holding no credential and
// nothing to retry it.
//
// Both the initial-boot request and the unlock request now go through the
// tunnel manager's EnsureCredential event, which owns the existing bounded
// retry machinery (30-second credential-wait timeout, capped backoff).

// TestUnlockRebuildEndsTheSupersededCredentialRequesterWorker pins the
// lifecycle fix for the per-epoch ControlCredentialRequester: each unlock
// rebuild used to construct a requester whose worker parked until the PROCESS
// context ended, leaking one goroutine per lockdown/unlock cycle. The daemon
// now derives a cancellable epoch context; stopping that epoch MUST close it so
// the worker exits, while a live epoch's lifecycle stays open.
func TestUnlockRebuildEndsTheSupersededCredentialRequesterWorker(t *testing.T) {
	fx := newLockdownFixture(t)
	lifecycles := make(chan context.Context, 8)
	fx.d.tunnelRequesterObserver = func(ctx context.Context) { lifecycles <- ctx }

	// Rebuild once: the fresh manager's requester lifecycle is a new epoch.
	require.NoError(t, fx.d.Lockdown())
	require.NoError(t, fx.d.Unlock())

	var rebuildCtx context.Context
	select {
	case rebuildCtx = <-lifecycles:
	case <-time.After(2 * time.Second):
		t.Fatal("unlock rebuild did not construct a credential requester")
	}
	select {
	case <-rebuildCtx.Done():
		t.Fatal("the rebuilt requester lifecycle ended while its manager is still live")
	default:
	}

	// The next lockdown ends that epoch. Without the epoch-scoped cancel the
	// lifecycle is the process context's, whose Done never fires here, so this
	// blocks until the timeout and the worker leaks until process exit.
	require.NoError(t, fx.d.Lockdown())
	select {
	case <-rebuildCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("lockdown did not end the superseded requester lifecycle; its worker leaks until process exit")
	}
}

// TestBootstrapEnsureCredentialRetriesUntilControlAnswers proves the bootstrap
// path is no longer one-shot: control ignores the first request, and the
// manager's own wait timer re-issues it while the daemon holds no credential.
// A late answer still arms and starts the tunnel.
func TestBootstrapEnsureCredentialRetriesUntilControlAnswers(t *testing.T) {
	fx := newLockdownFixture(t, tunnel.WithCredentialWaitTimeout(40*time.Millisecond))

	require.NoError(t, fx.d.handleEnrollmentReady(signaling.Message{Type: "enrollment_ready"}))
	waitForCond(t, func() bool {
		return countSentMessages(fx.sig, "relay_credential_request", map[string]any{"reason": "restart"}) >= 1
	})

	// The first request is never answered; without the manager-owned retry the
	// count stays at exactly one forever.
	waitForCond(t, func() bool {
		return countSentMessages(fx.sig, "relay_credential_request", map[string]any{"reason": "restart"}) >= 2
	})

	// A late answer still arms and starts the tunnel (the retries are harmless).
	fx.d.handleSignalingMessage(relayConfigMessage(t, 1, "credential-late-answer"))
	waitForCond(t, func() bool { return fx.starter.startCount() == 1 })
}

// TestUnlockEnsureCredentialIsBoundedRetriedThroughTheManager proves the
// unlock path uses the same bounded retry: after a lockdown the rebuilt
// manager asks for the restart credential, and when control does not answer
// the manager re-asks on its own.
func TestUnlockEnsureCredentialIsBoundedRetriedThroughTheManager(t *testing.T) {
	fx := newLockdownFixture(t, tunnel.WithCredentialWaitTimeout(40*time.Millisecond))
	require.NoError(t, fx.d.Lockdown())
	require.NoError(t, fx.d.Unlock())

	waitForCond(t, func() bool {
		return countSentMessages(fx.sig, "relay_credential_request", map[string]any{"reason": "restart"}) >= 2
	})
}

// TestEnsureCredentialNeverRequestsOrStartsWhileLocked keeps the §13.4
// invariant literal for the new event: a locked daemon must not put a
// credential request on the wire and must not start a tunnel, even if the
// request is attempted directly.
func TestEnsureCredentialNeverRequestsOrStartsWhileLocked(t *testing.T) {
	fx := newLockdownFixture(t, tunnel.WithCredentialWaitTimeout(30*time.Millisecond))
	require.NoError(t, fx.d.Lockdown())
	require.True(t, fx.d.IsLocked())

	fx.d.requestRelayCredential()

	assertConditionStays(t, "locked daemon issues no credential request", 200*time.Millisecond, func() bool {
		return !fx.sig.hasSentMessage("relay_credential_request", nil)
	})
	if got := fx.starter.startCount(); got != 0 {
		t.Fatalf("locked daemon started frpc %d time(s), want 0", got)
	}
}
