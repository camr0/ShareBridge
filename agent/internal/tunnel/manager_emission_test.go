package tunnel

import (
	"errors"
	"log"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Round D fix-round (audit A: "the bound is not self-owned; an unbounded
// callback path remains"). Round D bounded the wait for the child's exit, but
// shutdownChild still called emit twice synchronously and Manager.Stop waited
// unconditionally on the supervision loop, so a control-facing status callback
// that blocks (production: the daemon's relay_client_state send, whose context
// timeout is cooperative) parked the run goroutine — and every Stop caller —
// forever, and ErrChildKillTimeout was never returned.
//
// These tests pin the fix:
//
//   - Stop's wait for the supervision loop is bounded by a Manager-owned timer,
//     so Stop returns within its own bound no matter what a callback does;
//   - status emission is asynchronous and FIFO through a bounded worker, so a
//     blocking callback cannot wedge supervision or shutdown;
//   - drops are never silent — they are counted and logged — while a
//     responsive callback still receives every transition in order;
//   - a wedged callback or a wedged child/OS signal call leaves no permanent
//     goroutine leak once it is released.
//
// Every wait here is channel- or deadline-gated; no sleep is used as a
// synchronization barrier.

// syncBuffer is a concurrency-safe io.Writer used to capture the package's
// drop telemetry (log.Printf) without racing the emitting goroutine.
type syncBuffer struct {
	mu      sync.Mutex
	builder strings.Builder
}

func (buffer *syncBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.builder.Write(data)
}

func (buffer *syncBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.builder.String()
}

// blockingStatusCallback models a control-facing status callback that ignores
// its (cooperative) timeout: the FIRST invocation blocks until release is
// closed; every invocation is recorded, and finished counts invocations that
// ran to completion, so a test can prove Stop returned while the callback was
// still wedged.
type blockingStatusCallback struct {
	mu      sync.Mutex
	reports []StatusReport
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	calls   atomic.Int64
	done    atomic.Int64
}

func newBlockingStatusCallback() *blockingStatusCallback {
	return &blockingStatusCallback{entered: make(chan struct{}), release: make(chan struct{})}
}

func (callback *blockingStatusCallback) record(report StatusReport) {
	if callback.calls.Add(1) == 1 {
		close(callback.entered)
		<-callback.release
	}
	callback.mu.Lock()
	callback.reports = append(callback.reports, report)
	callback.mu.Unlock()
	callback.done.Add(1)
}

// waitEntered blocks until the callback is provably inside its wedged first
// invocation.
func (callback *blockingStatusCallback) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-callback.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("status callback was never invoked")
	}
}

// releaseNow releases the wedged callback (idempotent).
func (callback *blockingStatusCallback) releaseNow() {
	callback.once.Do(func() { close(callback.release) })
}

// releaseAtCleanup releases the wedged callback before the test's manager
// cleanup runs (t.Cleanup is LIFO, so the callback must be released after the
// manager is constructed).
func (callback *blockingStatusCallback) releaseAtCleanup(t *testing.T) {
	t.Helper()
	t.Cleanup(callback.releaseNow)
}

func (callback *blockingStatusCallback) reportSnapshot() []StatusReport {
	callback.mu.Lock()
	defer callback.mu.Unlock()
	return append([]StatusReport(nil), callback.reports...)
}

func (callback *blockingStatusCallback) hasStatus(status StatusKind) bool {
	for _, report := range callback.reportSnapshot() {
		if report.Status == status {
			return true
		}
	}
	return false
}

// newEmissionTestManager builds a manager around the given child starter and
// status callback with fast timers and a long stability window.
func newEmissionTestManager(t *testing.T, starter *recordingStarter, onStatus func(StatusReport), options ...ManagerOption) *Manager {
	t.Helper()
	managerOptions := append([]ManagerOption{
		withProcessStarter(starter.startProcess),
		withBackoffBase(time.Millisecond),
		withCredentialWaitTimeout(time.Hour),
		withRunningStabilityWindow(time.Hour),
	}, options...)
	manager, err := NewManager(testManagerSettings(t), &fakeCredentialRequester{}, onStatus, managerOptions...)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	t.Cleanup(func() { _ = manager.Stop() })
	return manager
}

// stopWithin runs Stop on its own goroutine and fails if it has not returned
// by the deadline, returning the error and the elapsed time otherwise.
func stopWithin(t *testing.T, manager *Manager, deadline time.Duration) (error, time.Duration) {
	t.Helper()
	stopReturned := make(chan error, 1)
	started := time.Now()
	go func() { stopReturned <- manager.Stop() }()
	select {
	case err := <-stopReturned:
		return err, time.Since(started)
	case <-time.After(deadline):
		t.Fatalf("Stop() did not return within %v", deadline)
		return nil, 0
	}
}

// TestManagerStopOwnBoundFiresWithoutSupervisionLoop pins that Stop's wait for
// the supervision loop is bounded by a Manager-owned timer: when the loop
// never signals completion (a Wedged callback in production), Stop returns the
// wrapped kill-timeout class error at its own bound instead of waiting
// forever.
func TestManagerStopOwnBoundFiresWithoutSupervisionLoop(t *testing.T) {
	manager := &Manager{
		stopChannel:        make(chan struct{}),
		doneChannel:        make(chan struct{}),
		childSignalTimeout: 5 * time.Millisecond,
		killGrace:          5 * time.Millisecond,
		killWait:           5 * time.Millisecond,
		statusDrainTimeout: 5 * time.Millisecond,
	}
	bound := manager.stopBound()
	if bound <= 0 {
		t.Fatalf("stopBound() = %v, want a positive Manager-owned bound", bound)
	}

	err, elapsed := stopWithin(t, manager, bound+time.Second)
	if err == nil {
		t.Fatal("Stop() = nil; a supervision loop that never exits must surface an explicit error")
	}
	if !errors.Is(err, ErrChildKillTimeout) {
		t.Fatalf("Stop() error = %q, want it to wrap ErrChildKillTimeout", err)
	}
	if !strings.Contains(err.Error(), "did not complete") {
		t.Fatalf("Stop() error = %q, want a distinguishable own-bound error", err)
	}
	if elapsed > bound+250*time.Millisecond {
		t.Fatalf("Stop() returned after %v; its own bound is %v", elapsed, bound)
	}
	// Idempotent: the bound is enforced on every call, not only the first.
	if again, _ := stopWithin(t, manager, bound+time.Second); again == nil {
		t.Fatal("second Stop() = nil, want the own-bound error again")
	}
}

// TestManagerStopReturnsWithinOwnBoundWhenCallbackBlocks is the core
// fix-round regression: an unkillable child AND a status callback that blocks
// forever must still yield ErrChildKillTimeout from Stop within the manager's
// own bound — not after the daemon's outer 10s lever bound, and never only
// once the callback is released.
func TestManagerStopReturnsWithinOwnBoundWhenCallbackBlocks(t *testing.T) {
	const (
		killGrace = 20 * time.Millisecond
		killWait  = 80 * time.Millisecond
		drain     = 30 * time.Millisecond
	)
	starter := &recordingStarter{stopsOnGraceful: false, ignoresKill: true}
	callback := newBlockingStatusCallback()
	manager := newEmissionTestManager(t, starter, callback.record,
		withKillGracePeriod(killGrace),
		withKillWaitTimeout(killWait),
		withStatusDrainTimeout(drain),
	)
	callback.releaseAtCleanup(t)

	if err := manager.ApplyConfig(validTestConfig()); err != nil {
		t.Fatalf("ApplyConfig() error = %v", err)
	}
	waitForCondition(t, "child started", time.Second, func() bool { return starter.startCount() == 1 })
	callback.waitEntered(t)

	bound := manager.stopBound()
	stopErr, elapsed := stopWithin(t, manager, bound+time.Second)

	if stopErr == nil {
		t.Fatal("Stop() = nil; an unkillable child must surface an explicit error")
	}
	if !errors.Is(stopErr, ErrChildKillTimeout) {
		t.Fatalf("Stop() error = %q, want it to wrap ErrChildKillTimeout", stopErr)
	}
	if elapsed > bound {
		t.Fatalf("Stop() returned after %v; its own bound is %v", elapsed, bound)
	}
	if callback.done.Load() != 0 {
		t.Fatalf("the status callback completed %d delivery(ies) before Stop returned; Stop must not wait on it",
			callback.done.Load())
	}
	if child := starter.recordAt(0).child; child.gracefulStopCount() != 1 || child.killCount() != 1 {
		t.Fatalf("stop escalation = graceful %d / kill %d, want 1/1",
			child.gracefulStopCount(), child.killCount())
	}
	// The reports stranded behind the wedged callback were dropped VISIBLY at
	// the drain bound rather than silently lost.
	if dropped := manager.droppedStatus.Load(); dropped == 0 {
		t.Fatal("status reports stranded behind a wedged callback must be counted as dropped")
	}
}

// TestManagerStatusEmissionDoesNotWedgeSupervision pins that emission is
// asynchronous: while the callback is wedged, the supervision loop still
// replaces children, and once the callback is responsive it receives every
// transition, in order, with no spurious drop.
func TestManagerStatusEmissionDoesNotWedgeSupervision(t *testing.T) {
	starter := &recordingStarter{stopsOnGraceful: true}
	callback := newBlockingStatusCallback()
	manager := newEmissionTestManager(t, starter, callback.record, withKillGracePeriod(20*time.Millisecond))
	callback.releaseAtCleanup(t)

	first := validTestConfig()
	if err := manager.ApplyConfig(first); err != nil {
		t.Fatalf("ApplyConfig(generation 1) error = %v", err)
	}
	waitForCondition(t, "first child started", time.Second, func() bool { return starter.startCount() == 1 })
	callback.waitEntered(t)

	second := first
	second.Generation++
	second.Credential = "credential-generation-two"
	if err := manager.ApplyConfig(second); err != nil {
		t.Fatalf("ApplyConfig(generation 2) error = %v", err)
	}
	// Synchronous emission would park the supervision loop inside the wedged
	// callback, so the replacement child could never start.
	waitForCondition(t, "replacement child started while the callback blocks", time.Second, func() bool {
		return starter.startCount() == 2
	})

	callback.releaseNow()
	if err := manager.Stop(); err != nil {
		t.Fatalf("Stop() error = %v, want nil for a well-behaved child", err)
	}

	if dropped := manager.droppedStatus.Load(); dropped != 0 {
		t.Fatalf("a callback that keeps up lost %d status report(s); drops must not be spurious", dropped)
	}
	reports := callback.reportSnapshot()
	firstIndex, secondIndex := -1, -1
	for index, report := range reports {
		if report.Status != StatusStarting {
			continue
		}
		if report.Generation == first.Generation && firstIndex < 0 {
			firstIndex = index
		}
		if report.Generation == second.Generation && secondIndex < 0 {
			secondIndex = index
		}
	}
	if firstIndex < 0 || secondIndex < 0 {
		t.Fatalf("both starting transitions must be delivered, got %#v", reports)
	}
	if firstIndex > secondIndex {
		t.Fatalf("starting transitions delivered out of order (generation 1 at %d, generation 2 at %d)", firstIndex, secondIndex)
	}
	if !callback.hasStatus(StatusStopped) {
		t.Error("the final stopped transition must be delivered before Stop returns")
	}
}

// TestManagerStatusDropsAreCountedAndLogged pins that the bounded queue never
// loses a report silently: a callback that cannot keep up overflows the queue,
// and each overflow is counted and logged while the supervision loop continues
// to work.
func TestManagerStatusDropsAreCountedAndLogged(t *testing.T) {
	var logs syncBuffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	starter := &recordingStarter{stopsOnGraceful: true}
	callback := newBlockingStatusCallback()
	manager := newEmissionTestManager(t, starter, callback.record,
		withKillGracePeriod(20*time.Millisecond),
		withStatusQueueSize(1),
	)
	callback.releaseAtCleanup(t)

	first := validTestConfig()
	if err := manager.ApplyConfig(first); err != nil {
		t.Fatalf("ApplyConfig(generation 1) error = %v", err)
	}
	waitForCondition(t, "first child started", time.Second, func() bool { return starter.startCount() == 1 })
	callback.waitEntered(t)

	// The worker holds one report and the queue holds one: the third
	// transition overflows.
	for generation := first.Generation + 1; generation <= first.Generation+2; generation++ {
		next := first
		next.Generation = generation
		next.Credential = "credential-generation-multiple"
		if err := manager.ApplyConfig(next); err != nil {
			t.Fatalf("ApplyConfig(generation %d) error = %v", generation, err)
		}
	}
	waitForCondition(t, "third child started while the callback blocks", 2*time.Second, func() bool {
		return starter.startCount() == 3
	})
	waitForCondition(t, "overflow counted", 2*time.Second, func() bool {
		return manager.droppedStatus.Load() >= 1
	})
	if !strings.Contains(logs.String(), "dropped") {
		t.Fatalf("an overflowed status report must be logged, got %q", logs.String())
	}
}

// TestManagerBlockedCallbackLeavesNoGoroutineLeak pins the no-leak guarantee:
// the emission worker is abandoned (never waited on) when a callback is still
// wedged at the shutdown bound, and it exits as soon as the callback is
// released, so the goroutine count returns to baseline.
func TestManagerBlockedCallbackLeavesNoGoroutineLeak(t *testing.T) {
	baseline := runtime.NumGoroutine()
	starter := &recordingStarter{stopsOnGraceful: true}
	callback := newBlockingStatusCallback()
	manager := newEmissionTestManager(t, starter, callback.record,
		withKillGracePeriod(20*time.Millisecond),
		withStatusDrainTimeout(30*time.Millisecond),
	)
	callback.releaseAtCleanup(t)

	if err := manager.ApplyConfig(validTestConfig()); err != nil {
		t.Fatalf("ApplyConfig() error = %v", err)
	}
	waitForCondition(t, "child started", time.Second, func() bool { return starter.startCount() == 1 })
	callback.waitEntered(t)

	stopErr, elapsed := stopWithin(t, manager, manager.stopBound()+time.Second)
	if stopErr != nil {
		t.Fatalf("Stop() error = %v, want nil for a well-behaved child", stopErr)
	}
	if elapsed > manager.stopBound() {
		t.Fatalf("Stop() returned after %v; its own bound is %v", elapsed, manager.stopBound())
	}
	if callback.done.Load() != 0 {
		t.Fatal("Stop() waited for the wedged callback")
	}
	if dropped := manager.droppedStatus.Load(); dropped == 0 {
		t.Fatal("the stopped transition stranded behind the wedged callback must be counted as dropped")
	}

	callback.releaseNow()
	waitForCondition(t, "manager goroutines return to baseline after release", 2*time.Second, func() bool {
		return runtime.NumGoroutine() <= baseline
	})
	if manager.childLive.Load() {
		t.Error("the gracefully stopped child must not stay marked live")
	}
}

// TestManagerStopBoundsWedgedGracefulStop pins that a child/OS GracefulStop
// call that never returns is bounded by a Manager-owned timer: the stop still
// escalates to the kill, the wedged call is surfaced, and Stop returns
// promptly.
func TestManagerStopBoundsWedgedGracefulStop(t *testing.T) {
	const signalTimeout = 30 * time.Millisecond
	baseline := runtime.NumGoroutine()
	gracefulBlock := make(chan struct{})
	var releaseOnce sync.Once
	releaseSignal := func() { releaseOnce.Do(func() { close(gracefulBlock) }) }
	starter := &recordingStarter{stopsOnGraceful: false, gracefulStopBlock: gracefulBlock}
	collector := &statusCollector{}
	manager := newEmissionTestManager(t, starter, collector.record,
		withKillGracePeriod(20*time.Millisecond),
		withKillWaitTimeout(time.Second),
		withChildSignalTimeout(signalTimeout),
	)
	t.Cleanup(releaseSignal)

	if err := manager.ApplyConfig(validTestConfig()); err != nil {
		t.Fatalf("ApplyConfig() error = %v", err)
	}
	waitForCondition(t, "child started", time.Second, func() bool { return starter.startCount() == 1 })

	stopErr, elapsed := stopWithin(t, manager, manager.stopBound()+time.Second)
	if stopErr != nil {
		t.Fatalf("Stop() error = %v, want nil: the wedged graceful call is bounded and the kill reaps the child", stopErr)
	}
	if elapsed > manager.stopBound() {
		t.Fatalf("Stop() returned after %v; its own bound is %v", elapsed, manager.stopBound())
	}
	child := starter.recordAt(0).child
	if child.gracefulStopCount() != 1 || child.killCount() != 1 {
		t.Fatalf("stop escalation = graceful %d / kill %d, want 1/1 (the kill must not wait for the wedged graceful call)",
			child.gracefulStopCount(), child.killCount())
	}
	if !collector.hasStatus(StatusError) {
		t.Error("the wedged graceful stop must be surfaced as an error status")
	}

	releaseSignal()
	waitForCondition(t, "manager goroutines return to baseline after release", 2*time.Second, func() bool {
		return runtime.NumGoroutine() <= baseline
	})
}

// TestManagerStopBoundsWedgedKill pins the other signal bound: a Kill call
// that never returns is bounded and its failure is surfaced (as
// ErrChildSignalTimeout), with the child still tracked for a later reap.
func TestManagerStopBoundsWedgedKill(t *testing.T) {
	const signalTimeout = 30 * time.Millisecond
	baseline := runtime.NumGoroutine()
	killBlock := make(chan struct{})
	var releaseOnce sync.Once
	releaseSignal := func() { releaseOnce.Do(func() { close(killBlock) }) }
	starter := &recordingStarter{stopsOnGraceful: false, ignoresKill: true, killBlock: killBlock}
	manager := newEmissionTestManager(t, starter, (&statusCollector{}).record,
		withKillGracePeriod(20*time.Millisecond),
		withKillWaitTimeout(time.Second),
		withChildSignalTimeout(signalTimeout),
	)
	t.Cleanup(releaseSignal)

	if err := manager.ApplyConfig(validTestConfig()); err != nil {
		t.Fatalf("ApplyConfig() error = %v", err)
	}
	waitForCondition(t, "child started", time.Second, func() bool { return starter.startCount() == 1 })
	child := starter.recordAt(0).child

	stopErr, elapsed := stopWithin(t, manager, manager.stopBound()+time.Second)
	if stopErr == nil {
		t.Fatal("Stop() = nil; a wedged kill call must surface an explicit error")
	}
	if !errors.Is(stopErr, ErrChildSignalTimeout) {
		t.Fatalf("Stop() error = %q, want it to wrap ErrChildSignalTimeout", stopErr)
	}
	if errors.Is(stopErr, ErrChildKillTimeout) {
		t.Fatalf("Stop() error = %q; a wedged signal call is not a kill timeout", stopErr)
	}
	if elapsed > manager.stopBound() {
		t.Fatalf("Stop() returned after %v; its own bound is %v", elapsed, manager.stopBound())
	}
	if elapsed < 20*time.Millisecond {
		t.Fatalf("Stop() returned after %v, before the graceful grace elapsed", elapsed)
	}
	if child.killCount() != 1 {
		t.Fatalf("kill attempts = %d, want 1", child.killCount())
	}
	if !manager.childLive.Load() {
		t.Error("an unreaped child must stay tracked as live")
	}

	releaseSignal()
	child.signalExit(errors.New("signal: killed"))
	waitForCondition(t, "manager goroutines return to baseline after release", 2*time.Second, func() bool {
		return runtime.NumGoroutine() <= baseline
	})
}
