package daemon

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"sharebridge/agent/internal/tunnel"
)

// TestLockdownFencesQueuedRelayConfigStart pins the cross-boundary half of the
// BLOCKING start-fence finding: applyRelayConfig checks IsLocked() and then
// calls manager.ApplyConfig. A handler that passes the pre-check just before
// the transition can still enqueue the config, and the manager's event loop
// then starts frpc after the transition — a pre-check in the daemon cannot
// observe a transition that happens after it. The manager's start-permission
// fence, flipped synchronously by Lockdown, is the authority: it is evaluated
// atomically with the start, so a config queued in the race window can
// neither arm nor start the tunnel.
//
// The pause is deterministic: the manager's apply gate holds the run goroutine
// between the enqueue and the config's processing while a full Lockdown
// completes, then the pause is released and the queued config must be refused.
func TestLockdownFencesQueuedRelayConfigStart(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	fx := newLockdownFixture(t, tunnel.WithApplyGate(func() {
		enteredOnce.Do(func() { close(entered) })
		<-release
	}))
	require.False(t, fx.d.IsLocked(), "fixture must start unlocked")

	// The handler passes the daemon's IsLocked() pre-check (unlocked), the
	// config is validated and queued on the manager, and the run goroutine is
	// then paused before processing it.
	fx.d.handleSignalingMessage(relayConfigMessage(t, 1, "credential-lockdown-race"))
	<-entered

	// A full §13.4 transition completes while the config is queued but
	// unprocessed. The manager's fence is flipped synchronously before the
	// locked flag is published, so it is already false once IsLocked() reports.
	lockdownDone := make(chan error, 1)
	go func() { lockdownDone <- fx.d.Lockdown() }()
	waitForCond(t, func() bool { return fx.d.IsLocked() })
	close(release)
	require.NoError(t, <-lockdownDone)

	assertConditionStays(t, "locked daemon never starts frpc for a config queued in the race window",
		initialCredentialRequestWindow, func() bool {
			return fx.starter.startCount() == 0
		})
	// "No armed config became effective": the credential-bearing generated
	// config must not have been written.
	configPath := filepath.Join(fx.d.GetConfig().TunnelDataDir, "frpc.toml")
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("a queued relay_config became effective on disk despite the lockdown fence: stat(%s) err = %v",
			configPath, err)
	}
}
