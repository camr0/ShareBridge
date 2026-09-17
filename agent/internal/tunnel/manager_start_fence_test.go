package tunnel

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests pin the BLOCKING concurrency finding closed by the
// start-permission fence: there was no start-permission fence consulted at the
// moment a child was started, so two paths could start frpc on a locked agent:
//
//  1. the F1 residual race — applyRelayConfig checks IsLocked() and then calls
//     manager.ApplyConfig; a handler that passes the check just before the
//     transition can still enqueue, and the manager's event loop then starts
//     the child after the transition;
//  2. the F2 retry path — timerApplyArmedConfig can win the run loop's select
//     and reach startChildNow without consulting stopped or daemon-lock state,
//     so a pending armed-config retry can start a child after Stop closed the
//     stop channel, or during the lockdown window.
//
// The fence is evaluated atomically with the start (under startMu, which
// SetStartPermitted also takes), so every start that begins after the flip is
// denied, including from timers.

// gatedApplyManager builds a manager whose event-loop apply path is paused at
// the gate until release is closed, so a test can deterministically land a
// fence flip between the enqueue (ApplyConfig) and the manager processing the
// config.
func gatedApplyManager(t *testing.T, settings Settings, starter *recordingStarter, collector *statusCollector, entered, release chan struct{}) *Manager {
	t.Helper()
	var enteredOnce sync.Once
	manager, err := NewManager(settings, &fakeCredentialRequester{}, collector.record,
		withProcessStarter(starter.startProcess),
		withBackoffBase(time.Millisecond),
		withKillGracePeriod(20*time.Millisecond),
		withCredentialWaitTimeout(time.Hour),
		withRunningStabilityWindow(time.Hour),
		withApplyGate(func() {
			enteredOnce.Do(func() { close(entered) })
			<-release
		}),
	)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	t.Cleanup(func() { _ = manager.Stop() })
	return manager
}

// TestManagerStartFenceDeniesQueuedConfigAfterFlip is the F1-residual
// regression at the manager boundary: a config that was accepted (queued)
// BEFORE the transition, but processed AFTER it, must neither arm nor start.
// Without the fence the run loop reaches startChildNow and starts frpc.
func TestManagerStartFenceDeniesQueuedConfigAfterFlip(t *testing.T) {
	settings := testManagerSettings(t)
	starter := &recordingStarter{}
	collector := &statusCollector{}
	entered := make(chan struct{})
	release := make(chan struct{})
	manager := gatedApplyManager(t, settings, starter, collector, entered, release)

	if err := manager.ApplyConfig(validTestConfig()); err != nil {
		t.Fatalf("ApplyConfig() error = %v", err)
	}
	<-entered // the run goroutine is paused before processing the queued config

	// The §13.4 transition: Lockdown flips the fence synchronously at its
	// start; the flip itself is what the start must observe.
	manager.SetStartPermitted(false)
	close(release)

	assertConditionStays(t, "no frpc start after the start fence was withdrawn",
		200*time.Millisecond, func() bool {
			return starter.startCount() == 0
		})
	// "No armed config became effective": the credential-bearing generated
	// config must not have been written at all.
	if _, err := os.Stat(settings.ConfigPath); !os.IsNotExist(err) {
		t.Fatalf("a fenced relay_config became effective on disk: stat(%s) err = %v",
			settings.ConfigPath, err)
	}
}

// TestManagerArmedConfigRetryFencedByLockdown pins the F2 retry path under the
// lockdown fence: with a timerApplyArmedConfig retry pending (the armed-config
// write failed transiently) and the transient failure then cleared, the retry
// would normally re-render, write and start. After the fence flip it must do
// nothing — not even write the config.
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
	// A long enough backoff that the retry cannot fire before the fence flip
	// below, but short enough that the 2s observation window covers it many
	// times over.
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

	if err := manager.ApplyConfig(validTestConfig()); err != nil {
		t.Fatalf("ApplyConfig() error = %v", err)
	}
	// Observe the first write failure: a timerApplyArmedConfig retry is now
	// pending.
	waitForCondition(t, "armed-config write failure", time.Second, func() bool {
		return collector.hasReasonContaining("create tunnel data directory")
	})

	// §13.4 transition, then clear the transient failure so the retry WOULD
	// succeed and start frpc if it were not fenced.
	manager.SetStartPermitted(false)
	if err := os.Remove(blockerPath); err != nil {
		t.Fatalf("clear blocker: %v", err)
	}
	assertConditionStays(t, "no frpc start from a fenced armed-config retry",
		2*time.Second, func() bool {
			return starter.startCount() == 0
		})
}

// TestManagerArmedConfigRetryFencedByStop pins the same retry path against a
// plain Stop(): the timer case winning the run loop's select must not start a
// child after the stop channel closed. Stop is driven for real; because the
// run loop exits on the stop channel, the test then reproduces the
// select-winning branch deterministically by driving the pending timer exactly
// as the loop would have (the loop has already exited, so the run-goroutine
// state is exclusively owned by the test goroutine).
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

	if err := manager.ApplyConfig(validTestConfig()); err != nil {
		t.Fatalf("ApplyConfig() error = %v", err)
	}
	waitForCondition(t, "armed-config write failure", time.Second, func() bool {
		return collector.hasReasonContaining("create tunnel data directory")
	})

	if err := manager.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	// Clear the transient failure so the retry WOULD succeed if the stop fence
	// did not deny it. shutdownChild cleared the pending timer, so restore the
	// pending kind to reproduce the select-winning branch.
	if err := os.Remove(blockerPath); err != nil {
		t.Fatalf("clear blocker: %v", err)
	}
	manager.pendingTimer = timerApplyArmedConfig
	manager.handleTimerFired()

	assertConditionStays(t, "no frpc start from an armed-config retry after Stop",
		200*time.Millisecond, func() bool {
			return starter.startCount() == 0
		})
}

// TestManagerStaleArmedConfigRetryAfterNewerApplyIsNoOp is the first F2 MINOR:
// a retry that fires after a newer ApplyConfig made a configuration effective
// must not re-apply it and must not start a second child. The manager tracks
// armedApplyPending for exactly this; the test pins it to the observable
// behaviour (no replacement churn) rather than the flag.
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
	manager, err := NewManager(settings, &fakeCredentialRequester{}, collector.record,
		withProcessStarter(starter.startProcess),
		withBackoffBase(time.Second),
		withKillGracePeriod(20*time.Millisecond),
		withCredentialWaitTimeout(time.Hour),
		withRunningStabilityWindow(time.Hour),
	)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	t.Cleanup(func() { _ = manager.Stop() })

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

	// Let the stale timerApplyArmedConfig retry fire (its delay is at most 1s).
	assertConditionStays(t, "no stale retry re-apply or second child",
		2*time.Second, func() bool {
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
