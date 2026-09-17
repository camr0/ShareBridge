package daemon

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"sharebridge/agent/internal/signaling"
)

// These tests pin the §13.4 lockdown window found in review of the fresh-boot
// relay-credential fix (8c0cb07e). handleEnrollmentReady snapshots the locked
// flag under ds.mu, releases the lock, does public-IP work, and only then asks
// for the initial relay credential — and the relay_config apply path never
// re-checked the locked flag at all. So a lockdown completing inside that
// window could still send a credential request, and a late relay_config reply
// could reach manager.ApplyConfig before the asynchronous lockdown stop lever
// stopped the manager, briefly starting frpc on a locked agent. The local
// listener/binder fences and the bounded stop narrowed it, but the §13.4
// invariant is literal: a locked agent must not request a credential or start
// a tunnel.

// lockFlagOnly publishes the §13.4 locked flag without running the lockdown
// levers. It reproduces the real ordering window in which Lockdown has set the
// flag (its first and final action, under ds.mu) but the concurrent
// best-effort "stop tunnel manager" lever has not completed yet — the window
// the config-side fence must close.
func lockFlagOnly(ds *directState) {
	ds.mu.Lock()
	ds.locked = true
	ds.lockdownEpoch++
	ds.mu.Unlock()
}

// gatedExternalIPMapper blocks ExternalIP until release is closed, widening
// the window between handleEnrollmentReady's locked snapshot and its initial
// relay credential request so a lockdown can be landed inside it
// deterministically.
type gatedExternalIPMapper struct {
	*recordingDirectMapper
	enteredOnce sync.Once
	entered     chan struct{}
	release     chan struct{}
}

func (m *gatedExternalIPMapper) ExternalIP() (string, error) {
	m.enteredOnce.Do(func() { close(m.entered) })
	<-m.release
	return m.recordingDirectMapper.ExternalIP()
}

// TestLockedDaemonRejectsRelayConfig pins the config-side fence: with the
// daemon locked but the tunnel manager not yet stopped (the window before the
// asynchronous stop lever completes), a valid relay_config must neither arm
// the manager nor start frpc. Fail-closed, like every other post-lockdown
// signal.
func TestLockedDaemonRejectsRelayConfig(t *testing.T) {
	fx := newLockdownFixture(t)

	// The daemon is locked; the manager is deliberately still running, which is
	// the real window between the locked flag and the stop lever.
	lockFlagOnly(fx.ds)
	require.True(t, fx.d.IsLocked(), "fixture must be locked")

	fx.d.handleSignalingMessage(relayConfigMessage(t, 1, "credential-post-lockdown"))

	assertConditionStays(t, "locked daemon never starts frpc for a late relay_config",
		initialCredentialRequestWindow, func() bool {
			return fx.starter.startCount() == 0
		})
	if fx.d.tunnel.HasArmedCredential() {
		t.Fatal("locked daemon armed a relay_config credential; a locked agent must not hold one")
	}
}

// TestLockdownDuringEnrollmentWindowDoesNotRequestCredential pins the request
// window: a lockdown that lands after handleEnrollmentReady's locked snapshot
// (while the public-IP work is in flight) must not leave a
// relay_credential_request on the wire. The config-side fence above keeps the
// late reply harmless, but the request itself must not be sent.
func TestLockdownDuringEnrollmentWindowDoesNotRequestCredential(t *testing.T) {
	fx := newLockdownFixture(t)
	gated := &gatedExternalIPMapper{
		recordingDirectMapper: fx.mapper,
		entered:               make(chan struct{}),
		release:               make(chan struct{}),
	}
	fx.ds.mapper = gated

	done := make(chan error, 1)
	go func() {
		done <- fx.d.handleEnrollmentReady(signaling.Message{Type: "enrollment_ready"})
	}()

	<-gated.entered // past the locked snapshot, inside learnAndReportPublicIP
	require.False(t, fx.d.IsLocked(), "lockdown must land after the snapshot, not before")
	require.NoError(t, fx.d.Lockdown())
	close(gated.release)
	require.NoError(t, <-done)

	assertConditionStays(t, "no credential request issued after a mid-window lockdown",
		initialCredentialRequestWindow, func() bool {
			return !fx.sig.hasSentMessage("relay_credential_request", nil)
		})
	if got := fx.starter.startCount(); got != 0 {
		t.Fatalf("locked enrollment window started frpc %d time(s), want 0", got)
	}
}
