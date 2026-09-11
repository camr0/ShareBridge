package tunnel

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

// Round D fix-round 2 (audit A, category A): signalChild used to spawn one
// goroutine per child signal call and abandon it on timeout. Go cannot cancel
// a blocking syscall, so an abandoned call is inherent — but a wedged
// GracefulStop/Kill combined with the repeated stop/replacement calls the
// supervision loop makes accumulated one abandoned goroutine per call. These
// tests pin the single-flight fix:
//
//   - at most ONE signal-call goroutine may be outstanding per manager
//     instance at any instant, so repeated stop-path invocations (a wedged
//     child replaced over and over) spawn nothing new;
//   - when the abandoned call eventually returns its slot is released and a
//     legitimate later call (the normal kill escalation) can proceed;
//   - the normal graceful/kill paths are unchanged (covered by the existing
//     stop tests, which still assert 1/1 escalation and the same errors).
//
// Every wait here is channel- or deadline-gated; no sleep is used as a
// synchronization barrier.

// TestManagerSignalCallsSingleFlightBounded is the accumulation regression: a
// child whose GracefulStop and Kill never return, driven through 50 repeated
// higher-generation replacements. At the pre-fix commit each replacement
// abandoned two more signal-call goroutines; the single-flight slot bounds the
// manager to at most one.
func TestManagerSignalCallsSingleFlightBounded(t *testing.T) {
	const (
		signalTimeout = 5 * time.Millisecond
		iterations    = 50
	)
	gracefulBlock := make(chan struct{})
	killBlock := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(gracefulBlock)
			close(killBlock)
		})
	}
	t.Cleanup(release)

	starter := &recordingStarter{
		stopsOnGraceful:   false,
		ignoresKill:       true,
		gracefulStopBlock: gracefulBlock,
		killBlock:         killBlock,
	}
	collector := &statusCollector{}
	manager := newEmissionTestManager(t, starter, collector.record,
		withKillGracePeriod(signalTimeout),
		withKillWaitTimeout(signalTimeout),
		withChildSignalTimeout(signalTimeout),
	)

	if err := manager.ApplyConfig(validTestConfig()); err != nil {
		t.Fatalf("ApplyConfig(initial) error = %v", err)
	}
	waitForCondition(t, "initial child start", time.Second, func() bool { return starter.startCount() == 1 })

	// Baseline the goroutine count with the manager's run/emission/watcher
	// goroutines already running, so the only admissible growth is the single
	// abandoned signal call.
	goroutineBaseline := runtime.NumGoroutine()

	for i := 0; i < iterations; i++ {
		config := validTestConfig()
		config.Generation = validTestConfig().Generation + 1 + i
		config.Credential = fmt.Sprintf("credential-single-flight-%d", i)
		if err := manager.ApplyConfig(config); err != nil {
			t.Fatalf("ApplyConfig(generation %d) error = %v", config.Generation, err)
		}
	}
	// Every processed replacement emits two error reports (the stop failure and
	// the replacement failure): gate on the run loop having drained the queue.
	waitForCondition(t, "all replacements processed", 10*time.Second, func() bool {
		return collector.reportCount() >= 2*iterations
	})

	if got := manager.outstandingSignalCalls.Load(); got > 1 {
		t.Fatalf("outstanding signal-call goroutines = %d, want at most 1", got)
	}
	child := starter.recordAt(0).child
	if got := child.gracefulStopCount() + child.killCount(); got > 1 {
		t.Fatalf("signal-call invocations = %d after %d repeated stop-path invocations, want at most 1",
			got, iterations)
	}
	if growth := runtime.NumGoroutine() - goroutineBaseline; growth > 2 {
		t.Fatalf("goroutine growth = %d after %d stop-path invocations, want it bounded by the single signal slot",
			growth, iterations)
	}

	release()
	waitForCondition(t, "wedged signal call exits after release", 2*time.Second, func() bool {
		return manager.outstandingSignalCalls.Load() == 0
	})
	if got := child.gracefulStopCount() + child.killCount(); got > 1 {
		t.Fatalf("signal-call invocations = %d after release, want at most 1", got)
	}
}

// TestManagerSignalSlotReleasedAfterWedgedCallReturns pins the release half of
// single-flight: while a wedged call occupies the slot the kill escalation is
// refused and no second child starts; once the wedged call returns the slot is
// released and a legitimate later replacement proceeds through the normal
// graceful → grace → kill escalation (still never more than one outstanding).
func TestManagerSignalSlotReleasedAfterWedgedCallReturns(t *testing.T) {
	const signalTimeout = 30 * time.Millisecond
	gracefulBlock := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gracefulBlock) }) }
	t.Cleanup(release)

	starter := &recordingStarter{stopsOnGraceful: false, gracefulStopBlock: gracefulBlock}
	collector := &statusCollector{}
	manager := newEmissionTestManager(t, starter, collector.record,
		withKillGracePeriod(20*time.Millisecond),
		withKillWaitTimeout(time.Second),
		withChildSignalTimeout(signalTimeout),
	)

	first := validTestConfig()
	if err := manager.ApplyConfig(first); err != nil {
		t.Fatalf("ApplyConfig(first) error = %v", err)
	}
	waitForCondition(t, "first child start", time.Second, func() bool { return starter.startCount() == 1 })
	firstChild := starter.recordAt(0).child

	// First replacement: the graceful call wedges, so the slot is occupied and
	// the kill escalation is refused; the old child stays tracked.
	second := first
	second.Generation++
	second.Credential = "credential-slot-second"
	if err := manager.ApplyConfig(second); err != nil {
		t.Fatalf("ApplyConfig(second) error = %v", err)
	}
	waitForCondition(t, "wedged graceful call times out", 2*time.Second, func() bool {
		return firstChild.gracefulStopCount() == 1
	})
	waitForCondition(t, "slot occupied by the wedged call", 2*time.Second, func() bool {
		return manager.outstandingSignalCalls.Load() == 1
	})
	if firstChild.gracefulStopCount() != 1 || firstChild.killCount() != 0 {
		t.Fatalf("first stop path = graceful %d / kill %d, want 1/0 while the slot is occupied",
			firstChild.gracefulStopCount(), firstChild.killCount())
	}
	assertConditionStays(t, "no second child while the old one is unreaped", 100*time.Millisecond, func() bool {
		return starter.startCount() == 1
	})

	// Release the wedged call: the slot frees.
	release()
	waitForCondition(t, "slot released after the wedged call returns", 2*time.Second, func() bool {
		return manager.outstandingSignalCalls.Load() == 0
	})

	// A later, legitimate replacement now proceeds: graceful returns (the block
	// is released), the grace window elapses, the kill reaps the child, and the
	// replacement starts.
	third := second
	third.Generation++
	third.Credential = "credential-slot-third"
	if err := manager.ApplyConfig(third); err != nil {
		t.Fatalf("ApplyConfig(third) error = %v", err)
	}
	waitForCondition(t, "replacement child starts after the slot is released", 3*time.Second, func() bool {
		return starter.startCount() == 2
	})
	if got := firstChild.gracefulStopCount(); got < 2 {
		t.Fatalf("graceful stop attempts = %d, want a second attempt after the slot was released", got)
	}
	if got := firstChild.killCount(); got != 1 {
		t.Fatalf("kill attempts = %d, want the normal 1 after the released graceful call", got)
	}
	if !manager.childLive.Load() {
		t.Fatal("the replacement child must be tracked as live")
	}
	if got := manager.outstandingSignalCalls.Load(); got > 1 {
		t.Fatalf("outstanding signal-call goroutines = %d, want at most 1", got)
	}
}

// TestManagerSignalSlotIsPerManager pins that the single-flight state is
// per-manager: a rebuilt manager starts with a free slot and its own bound,
// while a manager that already abandoned a call never spawns a second one.
func TestManagerSignalSlotIsPerManager(t *testing.T) {
	const signalTimeout = 20 * time.Millisecond
	block := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(block) }) }
	t.Cleanup(release)

	buildWedged := func(t *testing.T) (*Manager, *fakeChildProcess) {
		t.Helper()
		starter := &recordingStarter{stopsOnGraceful: false, ignoresKill: true, gracefulStopBlock: block, killBlock: block}
		manager := newEmissionTestManager(t, starter, (&statusCollector{}).record,
			withKillGracePeriod(signalTimeout),
			withKillWaitTimeout(signalTimeout),
			withChildSignalTimeout(signalTimeout),
		)
		if err := manager.ApplyConfig(validTestConfig()); err != nil {
			t.Fatalf("ApplyConfig() error = %v", err)
		}
		waitForCondition(t, "child start", time.Second, func() bool { return starter.startCount() == 1 })
		return manager, starter.recordAt(0).child
	}

	firstManager, firstChild := buildWedged(t)
	stopWithin(t, firstManager, firstManager.stopBound()+time.Second)
	if got := firstManager.outstandingSignalCalls.Load(); got != 1 {
		t.Fatalf("first manager outstanding signal calls = %d, want 1", got)
	}

	// A rebuilt manager gets its own single slot: it may abandon one call, and
	// still only one.
	secondManager, secondChild := buildWedged(t)
	stopWithin(t, secondManager, secondManager.stopBound()+time.Second)
	if got := secondManager.outstandingSignalCalls.Load(); got != 1 {
		t.Fatalf("second manager outstanding signal calls = %d, want 1", got)
	}
	if firstChild.gracefulStopCount() != 1 || firstChild.killCount() != 0 {
		t.Fatalf("first manager signals = graceful %d / kill %d, want 1/0",
			firstChild.gracefulStopCount(), firstChild.killCount())
	}
	if secondChild.gracefulStopCount() != 1 || secondChild.killCount() != 0 {
		t.Fatalf("second manager signals = graceful %d / kill %d, want 1/0",
			secondChild.gracefulStopCount(), secondChild.killCount())
	}

	// Releasing frees both managers' slots.
	release()
	waitForCondition(t, "both slots released", 2*time.Second, func() bool {
		return firstManager.outstandingSignalCalls.Load() == 0 &&
			secondManager.outstandingSignalCalls.Load() == 0
	})
}

// TestManagerSignalRefusalKeepsFailureClass pins the refused call's error
// class: a signal request made while the single slot is occupied must surface
// as ErrChildSignalTimeout (not a silent success), so the stop path never
// mistakes a skipped kill for a delivered one.
func TestManagerSignalRefusalKeepsFailureClass(t *testing.T) {
	const signalTimeout = 20 * time.Millisecond
	block := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(block) }) }
	t.Cleanup(release)

	manager := newEmissionTestManager(t, &recordingStarter{}, (&statusCollector{}).record,
		withChildSignalTimeout(signalTimeout),
	)

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- manager.signalChild("graceful stop", func() error {
			<-block
			return nil
		})
	}()
	select {
	case err := <-firstDone:
		if !errors.Is(err, ErrChildSignalTimeout) {
			t.Fatalf("first signal error = %q, want ErrChildSignalTimeout", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first signal call did not return")
	}

	var secondRan bool
	secondErr := manager.signalChild("kill", func() error {
		secondRan = true
		return nil
	})
	if secondRan {
		t.Fatal("the refused signal call must not run")
	}
	if !errors.Is(secondErr, ErrChildSignalTimeout) {
		t.Fatalf("refused signal error = %q, want it to wrap ErrChildSignalTimeout", secondErr)
	}
	if got := manager.outstandingSignalCalls.Load(); got != 1 {
		t.Fatalf("outstanding signal calls = %d, want 1", got)
	}

	release()
	waitForCondition(t, "slot released", 2*time.Second, func() bool {
		return manager.outstandingSignalCalls.Load() == 0
	})
}
