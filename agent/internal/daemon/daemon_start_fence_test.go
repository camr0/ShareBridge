package daemon

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sharebridge/agent/internal/signaling"
	"sharebridge/agent/internal/tunnel"
)

// TestLockdownFencesQueuedRelayConfigStart pins the cross-boundary half of the
// BLOCKING start-fence finding at the EARLY guard: applyRelayConfig checks
// IsLocked() and then calls manager.ApplyConfig. A handler that passes the
// pre-check just before the transition can still enqueue the config, and the
// pause below holds the run goroutine between the enqueue and the config's
// processing while a full Lockdown completes. The manager's early arm guard
// (which reads the fence flipped synchronously by Lockdown before ds.locked is
// published) then refuses the queued config, so it is neither armed nor
// written to disk nor started.
//
// The authoritative startChildFenced check and the Lockdown deny wiring are
// covered by the two tests below, which are positioned past the early guards.
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

// TestLockdownDeniesInFlightStartBeforeItReturns pins the GAP-1 deny wiring at
// the moment of the start: Lockdown's synchronous SetStartPermitted(false) and
// the manager's check-and-spawn are serialized on startMu. The start below is
// paused AFTER its permission check and immediately before its spawn, holding
// startMu, so the check can no longer save it. Lockdown MUST therefore block on
// startMu until the start completes and return only with the manager denied. If
// the Lockdown deny wiring is deleted, Lockdown returns at its lever bound
// while the starter is still paused (the assertion fails), and the start then
// spawns after Lockdown returned (the ordering assertion fails).
func TestLockdownDeniesInFlightStartBeforeItReturns(t *testing.T) {
	spawnEntered := make(chan struct{})
	spawnRelease := make(chan struct{})
	var enteredOnce sync.Once
	fx := newLockdownFixture(t, tunnel.WithSpawnGate(func() {
		enteredOnce.Do(func() { close(spawnEntered) })
		<-spawnRelease
	}))
	require.False(t, fx.d.IsLocked(), "fixture must start unlocked")
	// A short lever bound keeps a deny-less Lockdown's return within the
	// assertion window instead of the production 10s.
	fx.d.lockdownLeverTimeout = 100 * time.Millisecond

	fx.d.handleSignalingMessage(relayConfigMessage(t, 1, "credential-inflight-lockdown"))
	select {
	case <-spawnEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("the start never reached the spawn gate")
	}

	lockdownDone := make(chan error, 1)
	go func() { lockdownDone <- fx.d.Lockdown() }()

	var lockdownErr error
	lockdownCompleted := false
	select {
	case lockdownErr = <-lockdownDone:
		lockdownCompleted = true
		t.Errorf("Lockdown returned while an in-flight start was paused between its permission check and its spawn (error %v)", lockdownErr)
	case <-time.After(500 * time.Millisecond):
	}

	close(spawnRelease)
	if !lockdownCompleted {
		select {
		case lockdownErr = <-lockdownDone:
		case <-time.After(5 * time.Second):
			t.Fatal("Lockdown did not return after the in-flight start completed")
		}
	}
	require.NoError(t, lockdownErr)
	require.True(t, fx.d.IsLocked(), "daemon must be locked")

	settled := fx.starter.startCount()
	assertConditionStays(t, "no frpc spawn after Lockdown returned",
		initialCredentialRequestWindow, func() bool {
			return fx.starter.startCount() == settled
		})
	fx.d.mu.RLock()
	manager := fx.d.tunnel
	fx.d.mu.RUnlock()
	require.NotNil(t, manager, "the fixture manager must still be published")
	require.False(t, manager.StartPermitted(), "a manager denied by Lockdown must stay denied")
}

// TestTunnelManagerPublicationSerializedWithLockdown pins the GAP-1
// construction/publication race deterministically. startTunnelManager arms and
// publishes a manager under lockdownMu; the pause below opens that arm+publish
// critical section (while lockdownMu is held) so a concurrent Lockdown must
// wait. When the publication wins the race and Lockdown follows, Lockdown
// observes the published manager and denies it; when Lockdown wins, the
// publisher observes the locked state and publishes the manager DENIED. Either
// way no permitted manager is ever published while locked, and no child is
// started.
func TestTunnelManagerPublicationSerializedWithLockdown(t *testing.T) {
	fx := newLockdownFixture(t)
	require.False(t, fx.d.IsLocked(), "fixture must start unlocked")

	// Reset to the pre-construction state so startTunnelManager runs its
	// construction/arm/publish path under the gate below.
	require.NoError(t, fx.d.stopTunnelManager())
	fx.d.mu.Lock()
	fx.d.tunnel = nil
	fx.d.mu.Unlock()

	starter := &recordingTunnelStarter{stopsOnGraceful: true}
	publishEntered := make(chan struct{})
	publishRelease := make(chan struct{})
	var enteredOnce sync.Once
	var releaseOnce sync.Once
	releasePublish := func() { releaseOnce.Do(func() { close(publishRelease) }) }
	fx.d.tunnelPublishGate = func() {
		enteredOnce.Do(func() { close(publishEntered) })
		<-publishRelease
	}
	fx.d.lockdownLeverTimeout = 100 * time.Millisecond
	t.Cleanup(releasePublish)

	constructionDone := make(chan struct{})
	go func() {
		fx.d.startTunnelManager(context.Background(),
			tunnel.WithProcessStarter(starter.start),
			tunnel.WithBackoffBase(time.Millisecond),
			tunnel.WithKillGracePeriod(20*time.Millisecond),
			tunnel.WithCredentialWaitTimeout(time.Hour),
			tunnel.WithRunningStabilityWindow(time.Hour),
		)
		close(constructionDone)
	}()

	select {
	case <-publishEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("startTunnelManager never reached the arm/publish step")
	}

	lockdownDone := make(chan error, 1)
	go func() { lockdownDone <- fx.d.Lockdown() }()

	// The arm+publish step holds lockdownMu, so Lockdown cannot complete (nor
	// interleave) while it is open. This is the mutual exclusion that closes
	// the publication race.
	var lockdownErr error
	lockdownCompleted := false
	select {
	case lockdownErr = <-lockdownDone:
		lockdownCompleted = true
		t.Errorf("Lockdown interleaved with the arm+publish critical section: %v", lockdownErr)
	case <-time.After(500 * time.Millisecond):
	}

	releasePublish()
	select {
	case <-constructionDone:
	case <-time.After(5 * time.Second):
		t.Fatal("startTunnelManager did not finish")
	}
	if !lockdownCompleted {
		select {
		case lockdownErr = <-lockdownDone:
		case <-time.After(5 * time.Second):
			t.Fatal("Lockdown did not finish")
		}
	}
	require.NoError(t, lockdownErr)

	require.True(t, fx.d.IsLocked(), "daemon must be locked")
	fx.d.mu.RLock()
	manager := fx.d.tunnel
	fx.d.mu.RUnlock()
	require.NotNil(t, manager, "the manager must have been published")
	require.False(t, manager.StartPermitted(), "a manager published into a locked daemon must be DENIED")

	fx.d.handleSignalingMessage(relayConfigMessage(t, 1, "credential-published-while-locked"))
	assertConditionStays(t, "locked daemon never starts frpc after the publication race",
		initialCredentialRequestWindow, func() bool {
			return starter.startCount() == 0
		})
}

// TestLockdownWinsTunnelManagerPublicationRace pins the opposite ordering from
// TestTunnelManagerPublicationSerializedWithLockdown: Lockdown completes FIRST
// and the manager construction then observes the locked daemon. Lockdown holds
// lockdownMu for its whole body while its levers fan out, so pausing a lever
// deterministically holds the transition open past the end of the synchronous
// deny; a construction started in that window blocks on lockdownMu and can
// only be denied by publishTunnelManagerLocked's own arm-from-IsLocked() step,
// because the Lockdown deny already ran against a nil/old manager. This is the
// fail-closed branch of the publication race.
func TestLockdownWinsTunnelManagerPublicationRace(t *testing.T) {
	fx := newLockdownFixture(t)
	require.False(t, fx.d.IsLocked(), "fixture must start unlocked")

	// Reset to the pre-construction state so startTunnelManager below runs its
	// construction/arm/publish path after the lockdown transition.
	require.NoError(t, fx.d.stopTunnelManager())
	fx.d.mu.Lock()
	fx.d.tunnel = nil
	fx.d.mu.Unlock()

	// Pause Lockdown inside its lever fan-out. Lockdown holds lockdownMu for
	// its entire body (the levers run under it), so a construction started now
	// blocks on that mutex until the transition completes. The locked flag is
	// already published at this point: it is set under ds.mu before any lever
	// is started.
	leverEntered := make(chan struct{})
	leverRelease := make(chan struct{})
	var enteredOnce sync.Once
	fx.d.lockdownLeverGate = func(string) {
		enteredOnce.Do(func() { close(leverEntered) })
		<-leverRelease
	}

	lockdownDone := make(chan error, 1)
	go func() { lockdownDone <- fx.d.Lockdown() }()
	select {
	case <-leverEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("Lockdown never reached its lever fan-out")
	}
	require.True(t, fx.d.IsLocked(), "lockdown must publish the locked state before its levers")

	starter := &recordingTunnelStarter{stopsOnGraceful: true}
	constructionDone := make(chan struct{})
	go func() {
		fx.d.startTunnelManager(context.Background(),
			tunnel.WithProcessStarter(starter.start),
			tunnel.WithBackoffBase(time.Millisecond),
			tunnel.WithKillGracePeriod(20*time.Millisecond),
			tunnel.WithCredentialWaitTimeout(time.Hour),
			tunnel.WithRunningStabilityWindow(time.Hour),
		)
		close(constructionDone)
	}()

	// The construction MUST wait for the transition: lockdownMu is held until
	// Lockdown returns.
	select {
	case <-constructionDone:
		t.Fatal("manager construction completed while Lockdown still held lockdownMu")
	case <-time.After(500 * time.Millisecond):
	}

	close(leverRelease)
	select {
	case err := <-lockdownDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Lockdown did not finish")
	}
	select {
	case <-constructionDone:
	case <-time.After(5 * time.Second):
		t.Fatal("startTunnelManager did not finish after Lockdown released lockdownMu")
	}

	require.True(t, fx.d.IsLocked(), "daemon must be locked")
	fx.d.mu.RLock()
	manager := fx.d.tunnel
	fx.d.mu.RUnlock()
	require.NotNil(t, manager, "the manager must have been published")
	require.False(t, manager.StartPermitted(),
		"a manager constructed after Lockdown won the race must be published DENIED")

	// Bypass the daemon's IsLocked() pre-check and drive the parsed config
	// straight into the published manager: the manager's own fence must be what
	// refuses the start.
	config, err := signaling.ParseRelayConfig(relayConfigMessage(t, 1, "credential-lockdown-wins").Raw)
	require.NoError(t, err)
	require.NoError(t, manager.ApplyConfig(config))
	assertConditionStays(t, "a denied manager never starts frpc for a config applied while locked",
		initialCredentialRequestWindow, func() bool {
			return starter.startCount() == 0
		})
}

// TestRestartTunnelManagerStopsSupersededManager pins the restart path's
// teardown obligation: restartTunnelManager replaces d.tunnel with a freshly
// constructed manager, and the superseded manager must be STOPPED, not merely
// dropped. An unstopped manager keeps its supervision loop, child watcher,
// restart timer and status emission goroutines alive with no owner, and its
// frpc child can outlive the manager that was supposed to fence it.
func TestRestartTunnelManagerStopsSupersededManager(t *testing.T) {
	fx := newLockdownFixture(t)
	require.False(t, fx.d.IsLocked(), "fixture must start unlocked")

	fx.d.mu.RLock()
	superseded := fx.d.tunnel
	fx.d.mu.RUnlock()
	require.NotNil(t, superseded, "the fixture must own a tunnel manager")
	require.False(t, superseded.Stopped(), "the fixture manager must start live")

	fx.d.restartTunnelManager()

	fx.d.mu.RLock()
	replacement := fx.d.tunnel
	fx.d.mu.RUnlock()
	require.NotNil(t, replacement, "the restart must publish a replacement manager")
	require.NotSame(t, superseded, replacement, "the restart must publish a fresh manager")
	require.True(t, superseded.Stopped(),
		"the superseded manager must be stopped, not silently orphaned")
	require.False(t, replacement.Stopped(), "the replacement manager must be live")
	require.True(t, replacement.StartPermitted(),
		"a restart on an unlocked daemon must re-arm the replacement")
}
