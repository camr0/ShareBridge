package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sharebridge/agent/internal/signaling"
)

// These tests pin the fresh-boot relay admission gap found on a live
// deployment (2026-09-04): a daemon that boots UNLOCKED and was never locked
// never sent its initial relay_credential_request, so the reactive tunnel
// manager never started frpc and every route fell back to "relay
// unavailable". Only the operator lockdown/unlock ritual armed the tunnel.
//
// Baseline enrollment readiness (enrollment_ready) is the signaling-connect
// hook on which the initial request must fire while the daemon is unlocked;
// the request must be suppressed while a credential is already held so a
// reconnect cannot grow the request count.

const initialCredentialRequestWindow = 150 * time.Millisecond

// TestFreshUnlockedBootRequestsInitialRelayCredential is the RED-first
// regression for the live defect: a never-locked, freshly enrolled daemon
// must issue exactly ONE initial relay_credential_request (reason restart),
// which then arms and starts the tunnel — with no operator lockdown/unlock
// cycle. Once control has answered with a credential, a later reconnect
// (another enrollment_ready) must NOT duplicate the request, because a
// credential is currently held.
func TestFreshUnlockedBootRequestsInitialRelayCredential(t *testing.T) {
	fx := newLockdownFixture(t)
	require.False(t, fx.d.IsLocked(), "fixture must start unlocked")

	// Baseline enrollment completes on a fresh boot.
	require.NoError(t, fx.d.handleEnrollmentReady(signaling.Message{Type: "enrollment_ready"}))

	waitForCond(t, func() bool {
		return countSentMessages(fx.sig, "relay_credential_request", map[string]any{"reason": "restart"}) == 1
	})

	// Control answers with the first credential: the tunnel arms and starts.
	fx.d.handleSignalingMessage(relayConfigMessage(t, 1, "credential-fresh-boot"))
	waitForCond(t, func() bool { return fx.starter.startCount() == 1 })

	// A successful signaling reconnect re-runs enrollment readiness. With a
	// credential currently held it must not re-request one.
	require.NoError(t, fx.d.handleEnrollmentReady(signaling.Message{Type: "enrollment_ready"}))
	assertConditionStays(t, "no duplicate initial credential request while a credential is held",
		initialCredentialRequestWindow, func() bool {
			return countSentMessages(fx.sig, "relay_credential_request", map[string]any{"reason": "restart"}) == 1
		})
}

// TestFreshUnlockedBootReconnectRetriesWhenNoCredentialHeld pins the recovery
// arm of the same trigger: if the initial request was lost (or control never
// answered with a relay_config), a reconnect retries it exactly once per
// enrollment_ready — bounded and idempotent, never a hot loop.
func TestFreshUnlockedBootReconnectRetriesWhenNoCredentialHeld(t *testing.T) {
	fx := newLockdownFixture(t)

	require.NoError(t, fx.d.handleEnrollmentReady(signaling.Message{Type: "enrollment_ready"}))
	waitForCond(t, func() bool {
		return countSentMessages(fx.sig, "relay_credential_request", map[string]any{"reason": "restart"}) == 1
	})

	// No credential was answered; a reconnect retries the request once.
	require.NoError(t, fx.d.handleEnrollmentReady(signaling.Message{Type: "enrollment_ready"}))
	waitForCond(t, func() bool {
		return countSentMessages(fx.sig, "relay_credential_request", map[string]any{"reason": "restart"}) == 2
	})
	assertConditionStays(t, "no un-prompted credential request hot loop",
		initialCredentialRequestWindow, func() bool {
			return countSentMessages(fx.sig, "relay_credential_request", map[string]any{"reason": "restart"}) == 2
		})
}

// TestLockedBootDoesNotRequestRelayCredential pins §13.4 for the boot path: a
// daemon that is locked before baseline enrollment completes must not request
// a relay credential and must not start a tunnel. Explicit local unlock is the
// only way back.
func TestLockedBootDoesNotRequestRelayCredential(t *testing.T) {
	fx := newLockdownFixture(t)
	require.NoError(t, fx.d.Lockdown())
	require.True(t, fx.d.IsLocked())

	require.NoError(t, fx.d.handleEnrollmentReady(signaling.Message{Type: "enrollment_ready"}))

	assertConditionStays(t, "locked boot issues no relay credential request",
		initialCredentialRequestWindow, func() bool {
			return !fx.sig.hasSentMessage("relay_credential_request", nil)
		})
	if got := fx.starter.startCount(); got != 0 {
		t.Fatalf("locked boot started frpc %d time(s), want 0", got)
	}
}
