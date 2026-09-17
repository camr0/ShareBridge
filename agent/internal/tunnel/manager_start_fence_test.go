package tunnel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests pin the start-permission fence closed by the §7.4/§13.4 fix
// round. The manager is fail-closed: NewManager constructs it DENIED, and the
// daemon arms it only in the unlock/start publication step that is serialized
// against the §13.4 Lockdown/Unlock transitions. Three properties are proven
// here, each designed so that deleting ONLY the production fence makes it FAIL:
//
//  1. fail-closed default — TestManagerFreshManagerIsDeniedByDefault;
//  2. the authoritative check inside startChildFenced, reached AFTER every
//     caller's early guard, denies a start whose permission was withdrawn in
//     the window between the early guard and the check —
//     TestManagerStartFenceDeniesConfigPausedBeforeFencedCheck;
//  3. the stopped-state transition is serialized with the check-and-spawn on
//     startMu, so Stop blocks until an in-flight start completes and no child
//     can be spawned after Stop returns —
//     TestManagerStopSerializesWithInFlightStart.
//
// The timer/retry paths are covered by
// TestManagerArmedConfigRetryFencedByLockdown (the retry reaches the fenced
// spawn after a transient write failure) and
// TestManagerArmedConfigRetryFencedByStop (the start funnel is driven after
// Stop).

// TestManagerFreshManagerIsDeniedByDefault pins the fail-closed construction
// default: a manager the daemon has not yet armed must not start a child, and
// only an explicit arm (the unlocked daemon's publication step) permits one.
// Reverting the default to permitted fails the first assertion; the
// subsequent behavioural checks fail if the fence is removed.
func TestManagerFreshManagerIsDeniedByDefault(t *testing.T) {
	settings := testManagerSettings(t)
	starter := &recordingStarter{}
	collector := &statusCollector{}
	manager, err := NewManager(settings, &fakeCredentialRequester{}, collector.record,
		withProcessStarter(starter.startProcess),
		withBackoffBase(time.Millisecond),
		withKillGracePeriod(20*time.Millisecond),
		withCredentialWaitTimeout(time.Hour),
		withRunningStabilityWindow(time.Hour),
	)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	t.Cleanup(func() { _ = manager.Stop() })

	if manager.StartPermitted() {
		t.Fatal("a freshly constructed manager must be DENIED (fail-closed default)")
	}
	if err := manager.ApplyConfig(validTestConfig()); err != nil {
		t.Fatalf("ApplyConfig() error = %v", err)
	}
	assertConditionStays(t, "a denied fresh manager never starts frpc",
		200*time.Millisecond, func() bool {
			return starter.startCount() == 0
		})

	// The daemon's unlocked publication step arms the manager; a subsequent
	// config must then start normally (the fence gates, it does not break).
	manager.SetStartPermitted(true)
	if err := manager.ApplyConfig(validTestConfig()); err != nil {
		t.Fatalf("ApplyConfig() after arming error = %v", err)
	}
	waitForCondition(t, "child start after the manager was armed", time.Second, func() bool {
		return starter.startCount() == 1
	})
}

// TestManagerStartFenceDeniesConfigPausedBeforeFencedCheck is the
// load-bearing regression for the authoritative check inside startChildFenced.
// The start is paused by the start-fence seam AFTER both early guards
// (handleApplyConfig and applyArmedConfigOrScheduleRetry) have passed and
// immediately before the check, so the early guards cannot be what denies it.
// The permission is then withdrawn and the start resumed: only the
// startChildFenced check can refuse it. Deleting that check lets the spawn
// through and fails the test.
func TestManagerStartFenceDeniesConfigPausedBeforeFencedCheck(t *testing.T) {
	settings := testManagerSettings(t)
	starter := &recordingStarter{}
	collector := &statusCollector{}
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	var releaseOnce sync.Once
	releaseGate := func() { releaseOnce.Do(func() { close(release) }) }

	manager, err := NewManager(settings, &fakeCredentialRequester{}, collector.record,
		withProcessStarter(starter.startProcess),
		withBackoffBase(time.Millisecond),
		withKillGracePeriod(20*time.Millisecond),
		withCredentialWaitTimeout(time.Hour),
		withRunningStabilityWindow(time.Hour),
		withStartFenceGate(func() {
			enteredOnce.Do(func() { close(entered) })
			<-release
		}),
	)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	// Cleanups are LIFO: release the paused start before Stop runs so a failed
	// assertion cannot strand Stop on a wedged run goroutine.
	t.Cleanup(func() { _ = manager.Stop() })
	t.Cleanup(releaseGate)

	// The unlocked daemon's publication step arms the manager, and the config
	// passes both early guards before the seam pauses.
	manager.SetStartPermitted(true)
	if err := manager.ApplyConfig(validTestConfig()); err != nil {
		t.Fatalf("ApplyConfig() error = %v", err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the start never reached the fenced spawn")
	}

	// The §13.4 withdrawal lands after the early guards and before the check.
	manager.SetStartPermitted(false)
	releaseGate()

	assertConditionStays(t, "no frpc start after the fenced check observed the withdrawal",
		200*time.Millisecond, func() bool {
			return starter.startCount() == 0
		})
}

// TestManagerStopSerializesWithInFlightStart pins the GAP-2 serialization: the
// stopped transition is published under startMu, the same mutex
// startChildFenced holds across its check-and-spawn. The start is paused by the
// spawn seam after its permission/stopped check, holding startMu. Stop is
// started concurrently and MUST block until that start completes; if Stop
// closes the stop channel without startMu (the bug), it returns at its shutdown
// bound while the starter is still paused and the start then spawns a child
// after Stop returned. Both the "Stop has not returned" assertion and the
// "no spawn after Stop returned" assertion fail under that mutation.
func TestManagerStopSerializesWithInFlightStart(t *testing.T) {
	settings := testManagerSettings(t)
	starter := &recordingStarter{stopsOnGraceful: true}
	collector := &statusCollector{}
	var stopReturned atomic.Bool
	var spawnedAfterStopReturn atomic.Bool
	processStarter := func(ctx context.Context, binaryPath string, arguments []string) (childProcess, error) {
		// Sample the ordering at the exact spawn point: a spawn that happens
		// after Stop returned flips the flag.
		if stopReturned.Load() {
			spawnedAfterStopReturn.Store(true)
		}
		return starter.startProcess(ctx, binaryPath, arguments)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	var releaseOnce sync.Once
	releaseGate := func() { releaseOnce.Do(func() { close(release) }) }

	manager, err := NewManager(settings, &fakeCredentialRequester{}, collector.record,
		withProcessStarter(processStarter),
		withBackoffBase(time.Millisecond),
		// Short child-stop bounds keep the buggy Stop's stopBound small, so the
		// "Stop has not returned" assertion is fast and unambiguous.
		withChildSignalTimeout(10*time.Millisecond),
		withKillGracePeriod(10*time.Millisecond),
		withKillWaitTimeout(10*time.Millisecond),
		withStatusDrainTimeout(10*time.Millisecond),
		withCredentialWaitTimeout(time.Hour),
		withRunningStabilityWindow(time.Hour),
		withSpawnGate(func() {
			enteredOnce.Do(func() { close(entered) })
			<-release
		}),
	)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	t.Cleanup(func() { _ = manager.Stop() })
	t.Cleanup(releaseGate)

	manager.SetStartPermitted(true)
	if err := manager.ApplyConfig(validTestConfig()); err != nil {
		t.Fatalf("ApplyConfig() error = %v", err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the start never reached the spawn gate")
	}

	stopDone := make(chan error, 1)
	go func() {
		err := manager.Stop()
		stopReturned.Store(true)
		stopDone <- err
	}()

	var stopErr error
	stopCompleted := false
	select {
	case stopErr = <-stopDone:
		stopCompleted = true
		t.Errorf("Stop returned while an in-flight start was paused between its permission check and its spawn (error %v)", stopErr)
	case <-time.After(700 * time.Millisecond):
	}

	releaseGate()
	if !stopCompleted {
		select {
		case stopErr = <-stopDone:
		case <-time.After(5 * time.Second):
			t.Fatal("Stop did not return after the in-flight start completed")
		}
		if stopErr != nil {
			t.Errorf("Stop() error = %v", stopErr)
		}
	}
	// Release lets the in-flight start complete. Wait for the spawn before the
	// ordering assertions so they are deterministic in both the fixed path
	// (spawn before Stop returned) and the mutated path (spawn after).
	waitForCondition(t, "the released in-flight start to spawn", 5*time.Second, func() bool {
		return starter.startCount() == 1
	})
	if spawnedAfterStopReturn.Load() {
		t.Fatal("frpc was spawned after Stop returned: the stopped transition was not serialized with the start")
	}
}

// TestManagerArmedConfigRetryFencedByLockdown pins the F2 retry path under the
// lockdown fence: after a transient armed-config write failure, the retry
// reaches the fenced spawn (both early guards have passed), is paused there,
// and the permission is withdrawn. The retry must then do nothing. Deleting the
// startChildFenced check lets the retry spawn and fails the test.
func TestManagerArmedConfigRetryFencedByLockdown(t *testing.T) {
	dataDirectory := t.TempDir()
	blockerPath := filepath.Join(dataDirectory, "blocked-parent")
	if err := os.WriteFile(blockerPath, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("seed blocker file: %v", err)
	}
	settings := Settings{
		FRPCBinaryPath: "/opt/sharebridge/bin/frpc",
		ConfigPath:     filepath.Join(blockerPath, "frpc.toml"),
		TrustedCAFile:  filepath.Join(dataDirectory, "relay-ca.pem"),
		LocalTarget:    LocalTarget,
	}
	starter := &recordingStarter{}
	collector := &statusCollector{}
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	var releaseOnce sync.Once
	releaseGate := func() { releaseOnce.Do(func() { close(release) }) }
	// A backoff short enough to reach the retry quickly, but long enough that
	// the fence flip below lands after the retry's early guards.
	manager, err := NewManager(settings, &fakeCredentialRequester{}, collector.record,
		withProcessStarter(starter.startProcess),
		withBackoffBase(100*time.Millisecond),
		withKillGracePeriod(20*time.Millisecond),
		withCredentialWaitTimeout(time.Hour),
		withRunningStabilityWindow(time.Hour),
		withStartFenceGate(func() {
			enteredOnce.Do(func() { close(entered) })
			<-release
		}),
	)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	t.Cleanup(func() { _ = manager.Stop() })
	t.Cleanup(releaseGate)

	manager.SetStartPermitted(true)
	if err := manager.ApplyConfig(validTestConfig()); err != nil {
		t.Fatalf("ApplyConfig() error = %v", err)
	}
	// Observe the first write failure: a timerApplyArmedConfig retry is now
	// pending.
	waitForCondition(t, "armed-config write failure", time.Second, func() bool {
		return collector.hasReasonContaining("create tunnel data directory")
	})

	// Clear the transient failure so the retry WOULD succeed, and let it reach
	// the fenced spawn.
	if err := os.Remove(blockerPath); err != nil {
		t.Fatalf("clear blocker: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the armed-config retry never reached the fenced spawn")
	}

	// §13.4 withdrawal lands after the retry's early guards, before the check.
	manager.SetStartPermitted(false)
	releaseGate()

	assertConditionStays(t, "no frpc start from a fenced armed-config retry",
		2*time.Second, func() bool {
			return starter.startCount() == 0
		})
}

// TestManagerArmedConfigRetryFencedByStop pins the stop arm of the
// startChildFenced check against a plain Stop(): a pending armed-config retry
// must not start a child after the stop channel closed. Stop is driven for
// real; because the run loop then exits, the test goroutine exclusively owns
// the manager state and drives the same start funnel the timer would have
// (startChildNow), which reaches the authoritative check directly — deleting
// the stopped arm lets that funnel spawn and fails the test.
func TestManagerArmedConfigRetryFencedByStop(t *testing.T) {
	dataDirectory := t.TempDir()
	blockerPath := filepath.Join(dataDirectory, "blocked-parent")
	if err := os.WriteFile(blockerPath, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("seed blocker file: %v", err)
	}
	settings := Settings{
		FRPCBinaryPath: "/opt/sharebridge/bin/frpc",
		ConfigPath:     filepath.Join(blockerPath, "frpc.toml"),
		TrustedCAFile:  filepath.Join(dataDirectory, "relay-ca.pem"),
		LocalTarget:    LocalTarget,
	}
	starter := &recordingStarter{}
	collector := &statusCollector{}
	// A one-hour backoff guarantees the real retry timer cannot fire before the
	// Stop below; the pending state is then driven explicitly.
	manager, err := NewManager(settings, &fakeCredentialRequester{}, collector.record,
		withProcessStarter(starter.startProcess),
		withBackoffBase(time.Hour),
		withKillGracePeriod(20*time.Millisecond),
		withCredentialWaitTimeout(time.Hour),
		withRunningStabilityWindow(time.Hour),
	)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	t.Cleanup(func() { _ = manager.Stop() })

	manager.SetStartPermitted(true)
	if err := manager.ApplyConfig(validTestConfig()); err != nil {
		t.Fatalf("ApplyConfig() error = %v", err)
	}
	waitForCondition(t, "armed-config write failure", time.Second, func() bool {
		return collector.hasReasonContaining("create tunnel data directory")
	})

	if err := manager.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	// Clear the transient failure so the retry WOULD succeed if the stop arm
	// did not deny it, then drive the start funnel the retry timer calls.
	if err := os.Remove(blockerPath); err != nil {
		t.Fatalf("clear blocker: %v", err)
	}
	manager.startChildNow()

	assertConditionStays(t, "no frpc start from an armed-config retry after Stop",
		200*time.Millisecond, func() bool {
			return starter.startCount() == 0
		})
}

// TestManagerStaleArmedConfigRetryAfterNewerApplyIsNoOp is the first F2 MINOR:
// a retry that fires after a newer ApplyConfig made a configuration effective
// must not re-apply it and must not start a second child. The test proves the
// stale timer actually FIRED (via the timer-fired seam) before asserting the
// negative, rather than relying on a window that a retry with a [1s,2s) delay
// might not cover.
func TestManagerStaleArmedConfigRetryAfterNewerApplyIsNoOp(t *testing.T) {
	dataDirectory := t.TempDir()
	blockerPath := filepath.Join(dataDirectory, "blocked-parent")
	if err := os.WriteFile(blockerPath, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("seed blocker file: %v", err)
	}
	settings := Settings{
		FRPCBinaryPath: "/opt/sharebridge/bin/frpc",
		ConfigPath:     filepath.Join(blockerPath, "frpc.toml"),
		TrustedCAFile:  filepath.Join(dataDirectory, "relay-ca.pem"),
		LocalTarget:    LocalTarget,
	}
	starter := &recordingStarter{stopsOnGraceful: true}
	collector := &statusCollector{}
	retryFired := make(chan struct{})
	var retryOnce sync.Once
	manager, err := NewManager(settings, &fakeCredentialRequester{}, collector.record,
		withProcessStarter(starter.startProcess),
		withBackoffBase(time.Second),
		withKillGracePeriod(20*time.Millisecond),
		withCredentialWaitTimeout(time.Hour),
		withRunningStabilityWindow(time.Hour),
		withTimerFiredHook(func(kind timerKind) {
			if kind == timerApplyArmedConfig {
				retryOnce.Do(func() { close(retryFired) })
			}
		}),
	)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	t.Cleanup(func() { _ = manager.Stop() })

	manager.SetStartPermitted(true)
	generationOne := validTestConfig()
	if err := manager.ApplyConfig(generationOne); err != nil {
		t.Fatalf("ApplyConfig(generation 1) error = %v", err)
	}
	waitForCondition(t, "armed-config write failure for generation 1", time.Second, func() bool {
		return collector.hasReasonContaining("create tunnel data directory")
	})

	// Clear the transient failure and deliver a newer generation BEFORE the
	// pending retry fires. This makes generation 2 effective and starts it.
	if err := os.Remove(blockerPath); err != nil {
		t.Fatalf("clear blocker: %v", err)
	}
	generationTwo := generationOne
	generationTwo.Generation = generationOne.Generation + 1
	generationTwo.ProxyName = "sb-gen2"
	generationTwo.RelayPort = 10099
	generationTwo.Credential = "credential-generation-two"
	if err := manager.ApplyConfig(generationTwo); err != nil {
		t.Fatalf("ApplyConfig(generation 2) error = %v", err)
	}
	waitForCondition(t, "generation 2 child start", 2*time.Second, func() bool {
		return starter.startCount() == 1
	})
	firstChild := starter.recordAt(0).child

	// Prove the stale retry timer fired (its delay is in [1s,2s)), then assert
	// it changed nothing.
	select {
	case <-retryFired:
	case <-time.After(3 * time.Second):
		t.Fatal("the stale timerApplyArmedConfig retry never fired")
	}
	assertConditionStays(t, "no stale retry re-apply or second child",
		300*time.Millisecond, func() bool {
			return starter.startCount() == 1 && firstChild.gracefulStopCount() == 0
		})
	if !strings.Contains(configFileContent(t, settings.ConfigPath), generationTwo.Credential) {
		t.Fatalf("generated config does not carry the newer generation's credential")
	}
}

// TestManagerHigherGenerationReplacementViaRetryReplacesOnce is the second F2
// MINOR: the child != nil higher-generation replacement path (reached from the
// armed-config retry) must replace the running child exactly once — one
// graceful stop of the old child, one start of the new one, and no churn after.
func TestManagerHigherGenerationReplacementViaRetryReplacesOnce(t *testing.T) {
	dataDirectory := t.TempDir()
	settings := Settings{
		FRPCBinaryPath: "/opt/sharebridge/bin/frpc",
		ConfigPath:     filepath.Join(dataDirectory, "frpc.toml"),
		TrustedCAFile:  filepath.Join(dataDirectory, "relay-ca.pem"),
		LocalTarget:    LocalTarget,
	}
	starter := &recordingStarter{stopsOnGraceful: true}
	collector := &statusCollector{}
	manager, err := NewManager(settings, &fakeCredentialRequester{}, collector.record,
		withProcessStarter(starter.startProcess),
		withBackoffBase(400*time.Millisecond),
		withKillGracePeriod(20*time.Millisecond),
		withCredentialWaitTimeout(time.Hour),
		withRunningStabilityWindow(time.Hour),
	)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	t.Cleanup(func() { _ = manager.Stop() })

	manager.SetStartPermitted(true)
	generationOne := validTestConfig()
	generationOne.Generation = 1
	generationOne.ProxyName = "sb-gen1"
	generationOne.RelayPort = 10001
	generationOne.Credential = "credential-generation-one"
	if err := manager.ApplyConfig(generationOne); err != nil {
		t.Fatalf("ApplyConfig(generation 1) error = %v", err)
	}
	waitForCondition(t, "generation 1 child start", time.Second, func() bool {
		return starter.startCount() == 1
	})
	firstChild := starter.recordAt(0).child

	// Force the higher generation's config write to fail transiently while the
	// child is running, so the replacement takes the armed-config retry path.
	if err := os.RemoveAll(dataDirectory); err != nil {
		t.Fatalf("remove tunnel data directory: %v", err)
	}
	if err := os.WriteFile(dataDirectory, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("seed data-directory blocker: %v", err)
	}
	generationTwo := generationOne
	generationTwo.Generation = 2
	generationTwo.Credential = "credential-generation-two"
	if err := manager.ApplyConfig(generationTwo); err != nil {
		t.Fatalf("ApplyConfig(generation 2) error = %v", err)
	}
	waitForCondition(t, "generation 2 armed-config write failure", time.Second, func() bool {
		return collector.hasReasonContaining("create tunnel data directory")
	})

	// Clear the transient failure: the retry must stop generation 1 and start
	// generation 2 exactly once.
	if err := os.Remove(dataDirectory); err != nil {
		t.Fatalf("clear data-directory blocker: %v", err)
	}
	waitForCondition(t, "replacement child start", 2*time.Second, func() bool {
		return starter.startCount() == 2
	})
	waitForCondition(t, "graceful stop of the replaced child", time.Second, func() bool {
		return firstChild.gracefulStopCount() == 1
	})
	if firstChild.killCount() != 0 {
		t.Errorf("healthy child replacement must not need a kill, got %d", firstChild.killCount())
	}
	secondChild := starter.recordAt(1).child
	assertConditionStays(t, "exactly one replacement, no churn",
		300*time.Millisecond, func() bool {
			return starter.startCount() == 2 &&
				firstChild.gracefulStopCount() == 1 &&
				secondChild.gracefulStopCount() == 0 &&
				secondChild.killCount() == 0
		})
	if !strings.Contains(configFileContent(t, settings.ConfigPath), generationTwo.Credential) {
		t.Fatalf("generated config does not carry generation 2's credential")
	}
}
