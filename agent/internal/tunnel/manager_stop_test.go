package tunnel

import (
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Round D (post-remediation audit I4 / M5 / T33): Manager.Stop used to wait
// without a bound for the child's exit after the kill escalation. An errant
// (unkillable) frpc child therefore blocked shutdown forever — and because
// Daemon.Stop stopped the tunnel before closing the HTTPS listener, the
// listener kept serving indefinitely. These tests pin the fix:
//
//   - Stop's post-kill wait is bounded by a named constant with a test seam;
//   - on expiry Stop returns an explicit, distinguishable error instead of
//     hanging or silently succeeding;
//   - the bounded wait holds no lock and leaks no additional goroutine;
//   - a child that is reaped later clears the liveness bookkeeping even though
//     the supervision loop has exited (never permanently "running").
//
// Every wait here is channel- or deadline-gated; no sleep is used as a
// synchronization barrier.

// unkillableStopManager builds a manager whose started child ignores both the
// graceful stop and the kill, with the Round D post-kill bound exposed for
// tests. killWait is the bound under test (zero means the production default,
// used only by the normal-path test).
func unkillableStopManager(t *testing.T, killGrace, killWait time.Duration) (*Manager, *recordingStarter, *statusCollector) {
	t.Helper()
	settings := testManagerSettings(t)
	starter := &recordingStarter{stopsOnGraceful: false, ignoresKill: true}
	collector := &statusCollector{}
	options := []ManagerOption{
		withProcessStarter(starter.startProcess),
		withBackoffBase(time.Millisecond),
		withKillGracePeriod(killGrace),
		withCredentialWaitTimeout(time.Hour),
		withRunningStabilityWindow(time.Hour),
	}
	if killWait > 0 {
		options = append(options, withKillWaitTimeout(killWait))
	}
	manager, err := NewManager(settings, &fakeCredentialRequester{}, collector.record, options...)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	t.Cleanup(func() { _ = manager.Stop() })
	return manager, starter, collector
}

// startUnkillableChild arms the manager and waits for its first child to be
// tracked as live.
func startUnkillableChild(t *testing.T, manager *Manager, starter *recordingStarter) *fakeChildProcess {
	t.Helper()
	if err := manager.ApplyConfig(validTestConfig()); err != nil {
		t.Fatalf("ApplyConfig() error = %v", err)
	}
	waitForCondition(t, "child start", time.Second, func() bool { return starter.startCount() == 1 })
	child := starter.recordAt(0).child
	waitForCondition(t, "child tracked as live", time.Second, func() bool { return manager.childLive.Load() })
	return child
}

// TestManagerStopBoundedWhenChildIgnoresTermination pins the core audit fix:
// Stop must return within the post-kill bound with an explicit, distinguishable
// error — never hang and never silently report success — while still having
// attempted graceful stop then kill.
func TestManagerStopBoundedWhenChildIgnoresTermination(t *testing.T) {
	const (
		killGrace = 20 * time.Millisecond
		killWait  = 80 * time.Millisecond
	)
	manager, starter, collector := unkillableStopManager(t, killGrace, killWait)
	child := startUnkillableChild(t, manager, starter)

	stopReturned := make(chan error, 1)
	started := time.Now()
	go func() { stopReturned <- manager.Stop() }()

	var stopErr error
	select {
	case stopErr = <-stopReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return: the post-kill wait is unbounded")
	}
	elapsed := time.Since(started)

	if stopErr == nil {
		t.Fatal("Stop() = nil; an unkillable child must surface an explicit error, not a silent success")
	}
	if !errors.Is(stopErr, ErrChildKillTimeout) {
		t.Fatalf("Stop() error = %q, want it to wrap ErrChildKillTimeout", stopErr)
	}
	if !strings.Contains(stopErr.Error(), "did not exit") {
		t.Fatalf("Stop() error = %q, want a distinguishable \"did not exit within\" error", stopErr)
	}
	if elapsed < killGrace {
		t.Fatalf("Stop() returned after %v, before the %v graceful grace elapsed", elapsed, killGrace)
	}
	if elapsed > killGrace+killWait+time.Second {
		t.Fatalf("Stop() returned after %v; the %v post-kill wait must bound it", elapsed, killWait)
	}
	if child.gracefulStopCount() != 1 || child.killCount() != 1 {
		t.Fatalf("stop escalation = graceful %d / kill %d, want 1/1",
			child.gracefulStopCount(), child.killCount())
	}
	if !collector.hasStatus(StatusError) {
		t.Error("the unkillable child must be surfaced as an error status")
	}
	if !collector.hasStatus(StatusStopped) {
		t.Error("Stop must still report the final stopped status")
	}
	// Idempotent: repeating Stop returns the same recorded error rather than
	// hanging or reporting success.
	if again := manager.Stop(); again == nil || again.Error() != stopErr.Error() {
		t.Fatalf("second Stop() = %v, want the recorded error %v", again, stopErr)
	}
}

// TestManagerStopBoundedWaitHoldsNoLock pins that the bounded wait holds no
// lock: a concurrent ApplyConfig — and therefore the daemon shutdown path and
// the direct-serve handoff that share these transitions — must never block on
// the manager while Stop waits for an unkillable child.
func TestManagerStopBoundedWaitHoldsNoLock(t *testing.T) {
	const (
		killGrace = 150 * time.Millisecond
		killWait  = 300 * time.Millisecond
	)
	manager, starter, _ := unkillableStopManager(t, killGrace, killWait)
	child := startUnkillableChild(t, manager, starter)

	stopReturned := make(chan error, 1)
	go func() { stopReturned <- manager.Stop() }()

	// Gate on the observed graceful stop: the run loop is provably inside the
	// bounded wait from here on, so the stop channel is closed and ApplyConfig
	// must return synchronously through its stopped fence.
	waitForCondition(t, "graceful stop observed", time.Second, func() bool {
		return child.gracefulStopCount() == 1
	})
	applyReturned := make(chan error, 1)
	go func() { applyReturned <- manager.ApplyConfig(validTestConfig()) }()
	select {
	case err := <-applyReturned:
		if !errors.Is(err, errManagerStopped) {
			t.Fatalf("ApplyConfig() during Stop = %v, want errManagerStopped", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("ApplyConfig blocked while Stop was in its bounded wait: a lock is held across the wait")
	}

	select {
	case err := <-stopReturned:
		if err == nil {
			t.Fatal("Stop() = nil, want the unkillable-child error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return within the bounded wait")
	}
}

// TestManagerStopBoundedFailureLeavesNoGoroutineLeak pins the no-leak and
// bookkeeping guarantees: the bounded wait must not leave an extra waiter
// goroutine behind, and a child reaped after Stop returned must still clear the
// liveness bookkeeping (it must never stay permanently "running").
func TestManagerStopBoundedFailureLeavesNoGoroutineLeak(t *testing.T) {
	const (
		killGrace = 10 * time.Millisecond
		killWait  = 40 * time.Millisecond
	)
	baseline := runtime.NumGoroutine()
	manager, starter, _ := unkillableStopManager(t, killGrace, killWait)
	child := startUnkillableChild(t, manager, starter)

	stopReturned := make(chan error, 1)
	go func() { stopReturned <- manager.Stop() }()
	select {
	case err := <-stopReturned:
		if err == nil {
			t.Fatal("Stop() = nil, want the unkillable-child error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return within the bounded wait")
	}

	// The child was never reaped: the wait must have been performed by the one
	// existing watcher, not by a detached helper goroutine, and liveness must
	// still report the still-running process.
	if !manager.childLive.Load() {
		t.Fatal("child liveness must stay true while the process has not been reaped")
	}
	child.signalExit(errors.New("signal: killed"))
	waitForCondition(t, "liveness bookkeeping clears after reap", 2*time.Second, func() bool {
		return !manager.childLive.Load()
	})
	waitForCondition(t, "manager goroutines exit", 2*time.Second, func() bool {
		return runtime.NumGoroutine() <= baseline
	})
}

// TestManagerStopNormalPathIsPrompt pins the non-regression: a well-behaved
// child still stops promptly with no error, and the bound never slows the
// normal path.
func TestManagerStopNormalPathIsPrompt(t *testing.T) {
	settings := testManagerSettings(t)
	starter := &recordingStarter{stopsOnGraceful: true}
	collector := &statusCollector{}
	manager := newTestManager(t, settings, starter, &fakeCredentialRequester{}, collector)
	if err := manager.ApplyConfig(validTestConfig()); err != nil {
		t.Fatalf("ApplyConfig() error = %v", err)
	}
	waitForCondition(t, "child start", time.Second, func() bool { return starter.startCount() == 1 })

	started := time.Now()
	if err := manager.Stop(); err != nil {
		t.Fatalf("Stop() error = %v, want nil for a well-behaved child", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Stop() took %v for a well-behaved child; the bound must not slow the normal path", elapsed)
	}
	if !collector.hasStatus(StatusStopped) {
		t.Error("Stop must report the final stopped status")
	}
}

// TestManagerReplacementBoundedWaitKeepsSingleChildAndRecovers pins the
// replacement path (a strictly higher generation replacing an unkillable
// child): no second child may start while the old one is still tracked, the
// failure must be surfaced, and a later reap must clear bookkeeping and let the
// armed replacement start.
func TestManagerReplacementBoundedWaitKeepsSingleChildAndRecovers(t *testing.T) {
	const (
		killGrace = 10 * time.Millisecond
		killWait  = 40 * time.Millisecond
	)
	manager, starter, collector := unkillableStopManager(t, killGrace, killWait)
	child := startUnkillableChild(t, manager, starter)

	replacement := validTestConfig()
	replacement.Generation++
	if err := manager.ApplyConfig(replacement); err != nil {
		t.Fatalf("ApplyConfig(higher generation) error = %v", err)
	}
	waitForCondition(t, "replacement stop failure surfaced", time.Second, func() bool {
		return collector.hasStatus(StatusError)
	})
	assertConditionStays(t, "no second child while the old one is unreaped", 150*time.Millisecond, func() bool {
		return starter.startCount() == 1
	})

	// Reaping the old child clears bookkeeping; a fresh same-generation
	// credential (the §15.2 recovery path) then starts the replacement.
	child.signalExit(errors.New("exit status 1"))
	waitForCondition(t, "old child bookkeeping clears after reap", time.Second, func() bool {
		return !manager.childLive.Load()
	})
	refreshed := replacement
	refreshed.Credential = "credential-fresh-jti"
	if err := manager.ApplyConfig(refreshed); err != nil {
		t.Fatalf("ApplyConfig(refreshed credential) error = %v", err)
	}
	waitForCondition(t, "replacement child start after reap", 2*time.Second, func() bool {
		return starter.startCount() == 2
	})
	if !manager.childLive.Load() {
		t.Fatal("the replacement child must be tracked as live")
	}
}
