package tunnel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeChildProcess is a controllable childProcess: the test decides whether
// a graceful stop terminates the process and when the process exits.
type fakeChildProcess struct {
	mu                sync.Mutex
	gracefulStopCalls int
	killCalls         int
	waitError         error
	stopsOnGraceful   bool
	exitSignal        chan struct{}
	exitOnce          sync.Once
}

func newFakeChildProcess(stopsOnGraceful bool) *fakeChildProcess {
	return &fakeChildProcess{
		stopsOnGraceful: stopsOnGraceful,
		exitSignal:      make(chan struct{}),
	}
}

func (fake *fakeChildProcess) Wait() error {
	<-fake.exitSignal
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.waitError
}

func (fake *fakeChildProcess) GracefulStop() error {
	fake.mu.Lock()
	fake.gracefulStopCalls++
	stops := fake.stopsOnGraceful
	fake.mu.Unlock()
	if stops {
		fake.signalExit(nil)
	}
	return nil
}

func (fake *fakeChildProcess) Kill() error {
	fake.mu.Lock()
	fake.killCalls++
	fake.mu.Unlock()
	fake.signalExit(errors.New("signal: killed"))
	return nil
}

func (fake *fakeChildProcess) signalExit(waitError error) {
	fake.mu.Lock()
	fake.waitError = waitError
	fake.mu.Unlock()
	fake.exitOnce.Do(func() { close(fake.exitSignal) })
}

func (fake *fakeChildProcess) gracefulStopCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.gracefulStopCalls
}

func (fake *fakeChildProcess) killCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.killCalls
}

// startRecord captures one child start invocation for inspection.
type startRecord struct {
	binaryPath string
	arguments  []string
	child      *fakeChildProcess
}

// recordingStarter replaces the real exec seam so tests observe exactly what
// would be executed without launching processes.
type recordingStarter struct {
	mu              sync.Mutex
	records         []startRecord
	startError      error
	stopsOnGraceful bool
}

func (starter *recordingStarter) startProcess(ctx context.Context, binaryPath string, arguments []string) (childProcess, error) {
	starter.mu.Lock()
	defer starter.mu.Unlock()
	if starter.startError != nil {
		return nil, starter.startError
	}
	child := newFakeChildProcess(starter.stopsOnGraceful)
	starter.records = append(starter.records, startRecord{binaryPath: binaryPath, arguments: arguments, child: child})
	return child, nil
}

func (starter *recordingStarter) startCount() int {
	starter.mu.Lock()
	defer starter.mu.Unlock()
	return len(starter.records)
}

func (starter *recordingStarter) recordAt(index int) startRecord {
	starter.mu.Lock()
	defer starter.mu.Unlock()
	if index >= len(starter.records) {
		return startRecord{}
	}
	return starter.records[index]
}

// fakeCredentialRequester stands in for the production control-WebSocket
// relay_credential_request sender (production: ControlCredentialRequester).
// It records the §11.1 reason of every request so tests pin the reason
// matrix: replay-rejected relogin ⇒ replay_rejected (§15.2).
type fakeCredentialRequester struct {
	mu           sync.Mutex
	requests     int
	reasons      []CredentialRequestReason
	requestError error
}

func (requester *fakeCredentialRequester) RequestCredential(ctx context.Context, reason CredentialRequestReason) error {
	requester.mu.Lock()
	defer requester.mu.Unlock()
	requester.requests++
	requester.reasons = append(requester.reasons, reason)
	return requester.requestError
}

func (requester *fakeCredentialRequester) requestCount() int {
	requester.mu.Lock()
	defer requester.mu.Unlock()
	return requester.requests
}

func (requester *fakeCredentialRequester) requestReasons() []CredentialRequestReason {
	requester.mu.Lock()
	defer requester.mu.Unlock()
	return append([]CredentialRequestReason(nil), requester.reasons...)
}

// statusCollector receives manager diagnostics from the run goroutine.
type statusCollector struct {
	mu      sync.Mutex
	reports []StatusReport
}

func (collector *statusCollector) record(report StatusReport) {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	collector.reports = append(collector.reports, report)
}

func (collector *statusCollector) hasStatus(status StatusKind) bool {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	for _, report := range collector.reports {
		if report.Status == status {
			return true
		}
	}
	return false
}

// assertReportsNeverContainCredential scans every collected status report for
// credential material (Task 9 review carry-forward): diagnostics must never
// carry tunnel credential values (§7.2 log policy, §16.6). Callers pass every
// distinct credential value the scenario armed so a leak of any of them is
// caught.
func assertReportsNeverContainCredential(t *testing.T, collector *statusCollector, credentials ...string) {
	t.Helper()
	collector.mu.Lock()
	defer collector.mu.Unlock()
	for _, report := range collector.reports {
		for _, credential := range credentials {
			if credential != "" && strings.Contains(report.Reason, credential) {
				t.Fatalf("status report %q leaked credential material", report.Reason)
			}
		}
	}
}

// testManagerSettings builds manager settings inside a test temporary
// directory.
func testManagerSettings(t *testing.T) Settings {
	t.Helper()
	dataDirectory := t.TempDir()
	return Settings{
		FRPCBinaryPath: "/opt/sharebridge/bin/frpc",
		ConfigPath:     filepath.Join(dataDirectory, "frpc.toml"),
		TrustedCAFile:  filepath.Join(dataDirectory, "relay-ca.pem"),
		LocalTarget:    LocalTarget,
	}
}

// newTestManager constructs a manager with fast backoff and long stability
// windows so tests are fast and free of timer interference.
func newTestManager(t *testing.T, settings Settings, starter *recordingStarter, requester *fakeCredentialRequester, collector *statusCollector) *Manager {
	t.Helper()
	manager, err := NewManager(settings, requester, collector.record,
		withProcessStarter(starter.startProcess),
		withBackoffBase(time.Millisecond),
		withKillGracePeriod(20*time.Millisecond),
		withCredentialWaitTimeout(time.Hour),
		withRunningStabilityWindow(time.Hour),
	)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	t.Cleanup(manager.Stop)
	return manager
}

// waitForCondition polls until the condition holds or the timeout expires.
func waitForCondition(t *testing.T, description string, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

// assertConditionStays asserts that the condition keeps holding across an
// observation window during which a wrongly looping manager would act (its
// backoff base is 1ms, far below the window).
func assertConditionStays(t *testing.T, description string, observeWindow time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(observeWindow)
	for time.Now().Before(deadline) {
		if !condition() {
			t.Fatalf("condition violated while observing %s", description)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func configFileContent(t *testing.T, configPath string) string {
	t.Helper()
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read generated config %s: %v", configPath, err)
	}
	return string(data)
}

func TestManagerStartsWithoutShell(t *testing.T) {
	settings := testManagerSettings(t)
	starter := &recordingStarter{}
	collector := &statusCollector{}
	manager := newTestManager(t, settings, starter, &fakeCredentialRequester{}, collector)

	if err := manager.ApplyConfig(validTestConfig()); err != nil {
		t.Fatalf("ApplyConfig() error = %v", err)
	}
	waitForCondition(t, "frpc child start", time.Second, func() bool { return starter.startCount() == 1 })

	record := starter.recordAt(0)
	if record.binaryPath != settings.FRPCBinaryPath {
		t.Errorf("child binary = %q, want the configured pinned frpc path %q", record.binaryPath, settings.FRPCBinaryPath)
	}
	wantArguments := []string{"-c", settings.ConfigPath}
	if !reflect.DeepEqual(record.arguments, wantArguments) {
		t.Errorf("child arguments = %v, want the exact argument array %v (no shell, no command string)", record.arguments, wantArguments)
	}
	if len(record.arguments) != 2 || record.arguments[0] != "-c" {
		t.Errorf("child must be started as %q -c <config>, got %v", settings.FRPCBinaryPath, record.arguments)
	}
	if strings.HasSuffix(record.binaryPath, "sh") {
		t.Errorf("child binary %q must never be a shell", record.binaryPath)
	}
	if _, err := os.Stat(settings.ConfigPath); err != nil {
		t.Errorf("generated config missing after start: %v", err)
	}
	assertReportsNeverContainCredential(t, collector, validTestConfig().Credential)
}

func TestManagerReplacesOnlyHigherGeneration(t *testing.T) {
	settings := testManagerSettings(t)
	starter := &recordingStarter{stopsOnGraceful: true}
	collector := &statusCollector{}
	manager := newTestManager(t, settings, starter, &fakeCredentialRequester{}, collector)

	generationOne := validTestConfig()
	generationOne.Generation = 1
	generationOne.ProxyName = "sb-gen1"
	generationOne.RelayPort = 10001
	generationOne.Credential = "credential-generation-one"

	if err := manager.ApplyConfig(generationOne); err != nil {
		t.Fatalf("ApplyConfig(generation 1) error = %v", err)
	}
	waitForCondition(t, "initial child start", time.Second, func() bool { return starter.startCount() == 1 })

	// A same-generation credential refresh (fresh one-use jti for the same
	// assignment, spec §15.2) arms the new credential and rewrites the
	// config but must not restart a healthy child: replacement happens only
	// on a strictly higher generation.
	refreshed := generationOne
	refreshed.Credential = "credential-generation-one-refreshed"
	if err := manager.ApplyConfig(refreshed); err != nil {
		t.Fatalf("ApplyConfig(same-generation refresh) error = %v", err)
	}
	waitForCondition(t, "config rewrite with refreshed credential", time.Second, func() bool {
		return strings.Contains(configFileContent(t, settings.ConfigPath), refreshed.Credential)
	})
	assertConditionStays(t, "no same-generation child restart", 120*time.Millisecond, func() bool {
		return starter.startCount() == 1
	})
	if starter.recordAt(0).child.gracefulStopCount() != 0 {
		t.Error("same-generation refresh must not stop the running child")
	}

	// A lower generation is stale: rejected without any state change.
	stale := generationOne
	stale.Generation = 0
	stale.Credential = "credential-stale"
	if err := manager.ApplyConfig(stale); err == nil {
		t.Error("ApplyConfig(stale generation) succeeded, want error")
	}
	assertConditionStays(t, "no restart for stale generation", 120*time.Millisecond, func() bool {
		return starter.startCount() == 1
	})
	if !strings.Contains(configFileContent(t, settings.ConfigPath), refreshed.Credential) {
		t.Error("stale generation must not overwrite the generated config")
	}

	// A strictly higher generation replaces the running child gracefully.
	generationTwo := generationOne
	generationTwo.Generation = 2
	generationTwo.Credential = "credential-generation-two"
	if err := manager.ApplyConfig(generationTwo); err != nil {
		t.Fatalf("ApplyConfig(generation 2) error = %v", err)
	}
	waitForCondition(t, "replacement child start", time.Second, func() bool { return starter.startCount() == 2 })
	firstChild := starter.recordAt(0).child
	secondChild := starter.recordAt(1).child
	waitForCondition(t, "graceful stop of replaced child", time.Second, func() bool {
		return firstChild.gracefulStopCount() == 1
	})
	if firstChild.killCount() != 0 {
		t.Error("healthy child replacement must not need a kill")
	}
	waitForCondition(t, "config rewrite for higher generation", time.Second, func() bool {
		return strings.Contains(configFileContent(t, settings.ConfigPath), generationTwo.Credential)
	})

	// An older generation after the replacement stays fenced out.
	if err := manager.ApplyConfig(generationOne); err == nil {
		t.Error("ApplyConfig(generation below current) succeeded, want error")
	}
	assertConditionStays(t, "no restarts after fenced generation", 120*time.Millisecond, func() bool {
		return starter.startCount() == 2
	})
	if secondChild.gracefulStopCount() != 0 {
		t.Error("fenced generation must not stop the current child")
	}
	assertReportsNeverContainCredential(t, collector,
		"credential-generation-one", "credential-generation-one-refreshed",
		"credential-stale", "credential-generation-two")
}

func TestManagerRefreshesExpiredReconnectCredential(t *testing.T) {
	settings := testManagerSettings(t)
	starter := &recordingStarter{}
	requester := &fakeCredentialRequester{}
	collector := &statusCollector{}
	manager := newTestManager(t, settings, starter, requester, collector)

	initial := validTestConfig()
	initial.Generation = 3
	initial.Credential = "credential-burned-jti"
	if err := manager.ApplyConfig(initial); err != nil {
		t.Fatalf("ApplyConfig() error = %v", err)
	}
	waitForCondition(t, "initial child start", time.Second, func() bool { return starter.startCount() == 1 })
	if requester.requestCount() != 0 {
		t.Errorf("credential requests while the tunnel was healthy = %d, want 0", requester.requestCount())
	}

	// The pinned release exits frpc when its re-Login after an frps restart
	// is replay-rejected (burned one-use jti, Task 8 gate evidence). The
	// manager must then request a fresh credential instead of restarting
	// with the stale one (spec §15.2).
	starter.recordAt(0).child.signalExit(errors.New("exit status 1"))
	waitForCondition(t, "credential request after child exit", time.Second, func() bool {
		return requester.requestCount() == 1
	})
	// Reason matrix (§11.1): the exited child's re-Login was replay-rejected
	// (burned one-use jti, §15.2), so the request must carry replay_rejected.
	if reasons := requester.requestReasons(); len(reasons) != 1 || reasons[0] != ReasonReplayRejected {
		t.Errorf("credential request reasons = %v, want [replay_rejected]", reasons)
	}

	// No relogin retry-loop with the stale credential: with 1ms backoff a
	// looping manager would accumulate many starts inside this window.
	assertConditionStays(t, "no stale-credential restart loop", 150*time.Millisecond, func() bool {
		return starter.startCount() == 1
	})

	// Control answers relay_credential_request with a fresh relay_config for
	// the same generation (new jti); the manager restarts with it, exactly
	// once.
	refreshed := initial
	refreshed.Credential = "credential-fresh-jti"
	if err := manager.ApplyConfig(refreshed); err != nil {
		t.Fatalf("ApplyConfig(refreshed credential) error = %v", err)
	}
	waitForCondition(t, "restart with fresh credential", time.Second, func() bool {
		return starter.startCount() == 2
	})
	waitForCondition(t, "generated config carries the fresh credential", time.Second, func() bool {
		return strings.Contains(configFileContent(t, settings.ConfigPath), refreshed.Credential)
	})
	assertConditionStays(t, "no duplicate restart with fresh credential", 120*time.Millisecond, func() bool {
		return starter.startCount() == 2
	})
	if requester.requestCount() != 1 {
		t.Errorf("credential requests after fresh relogin = %d, want 1", requester.requestCount())
	}

	// The cycle repeats for the next burn: another exit requests another
	// fresh credential before restarting.
	starter.recordAt(1).child.signalExit(errors.New("exit status 1"))
	waitForCondition(t, "second credential request", time.Second, func() bool {
		return requester.requestCount() == 2
	})
	if reasons := requester.requestReasons(); len(reasons) != 2 || reasons[1] != ReasonReplayRejected {
		t.Errorf("second credential request reason = %v, want replay_rejected", reasons)
	}
	assertConditionStays(t, "no restart between exit and fresh credential", 120*time.Millisecond, func() bool {
		return starter.startCount() == 2
	})

	replacement := initial
	replacement.Generation = 4
	replacement.Credential = "credential-generation-four"
	if err := manager.ApplyConfig(replacement); err != nil {
		t.Fatalf("ApplyConfig(generation 4) error = %v", err)
	}
	waitForCondition(t, "restart on higher generation", time.Second, func() bool {
		return starter.startCount() == 3
	})
	if requester.requestCount() != 2 {
		t.Errorf("credential requests overall = %d, want exactly 2", requester.requestCount())
	}

	// Credential-never-in-reports scan (Task 9 review carry-forward): none of
	// the armed credential values may appear in any diagnostic reason.
	assertReportsNeverContainCredential(t, collector,
		"credential-burned-jti", "credential-fresh-jti", "credential-generation-four")
}

func TestManagerStopsAndKillsChild(t *testing.T) {
	t.Run("graceful stop terminates the child", func(t *testing.T) {
		settings := testManagerSettings(t)
		starter := &recordingStarter{stopsOnGraceful: true}
		collector := &statusCollector{}
		manager := newTestManager(t, settings, starter, &fakeCredentialRequester{}, collector)

		if err := manager.ApplyConfig(validTestConfig()); err != nil {
			t.Fatalf("ApplyConfig() error = %v", err)
		}
		waitForCondition(t, "child start", time.Second, func() bool { return starter.startCount() == 1 })
		child := starter.recordAt(0).child

		manager.Stop()

		if child.gracefulStopCount() != 1 {
			t.Errorf("graceful stop calls = %d, want 1", child.gracefulStopCount())
		}
		if child.killCount() != 0 {
			t.Errorf("kill calls = %d, want 0 (child honored the graceful stop)", child.killCount())
		}
		if !collector.hasStatus(StatusStopped) {
			t.Error("manager must report stopped status after Stop()")
		}
		if err := manager.ApplyConfig(validTestConfig()); !errors.Is(err, errManagerStopped) {
			t.Errorf("ApplyConfig() after Stop() = %v, want errManagerStopped", err)
		}
		assertReportsNeverContainCredential(t, collector, validTestConfig().Credential)
	})

	t.Run("unresponsive child is killed after the grace period", func(t *testing.T) {
		settings := testManagerSettings(t)
		starter := &recordingStarter{stopsOnGraceful: false}
		collector := &statusCollector{}
		manager := newTestManager(t, settings, starter, &fakeCredentialRequester{}, collector)

		if err := manager.ApplyConfig(validTestConfig()); err != nil {
			t.Fatalf("ApplyConfig() error = %v", err)
		}
		waitForCondition(t, "child start", time.Second, func() bool { return starter.startCount() == 1 })
		child := starter.recordAt(0).child

		manager.Stop()

		if child.gracefulStopCount() != 1 {
			t.Errorf("graceful stop calls = %d, want 1", child.gracefulStopCount())
		}
		if child.killCount() != 1 {
			t.Errorf("kill calls = %d, want 1 (child ignored the graceful stop)", child.killCount())
		}
		if !collector.hasStatus(StatusStopped) {
			t.Error("manager must report stopped status after killing the child")
		}
		assertReportsNeverContainCredential(t, collector, validTestConfig().Credential)
	})
}

// TestControlCredentialRequesterSendsExactReasonWithoutBlockingLoop covers
// the production CredentialRequester (§11.1): RequestCredential must enqueue
// without blocking the caller (the manager's run loop must never wait on
// WebSocket I/O — Task 9 carry-forward), pass the §11.1 reason through
// untouched, reject unknown reasons, and keep the queue bounded.
func TestControlCredentialRequesterSendsExactReasonWithoutBlockingLoop(t *testing.T) {
	t.Run("enqueues without blocking and forwards the exact reason", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		type sendRecord struct {
			reason CredentialRequestReason
		}
		sent := make(chan sendRecord, 8)
		requester := NewControlCredentialRequester(ctx, func(ctx context.Context, reason CredentialRequestReason) error {
			time.Sleep(150 * time.Millisecond) // simulated WebSocket write latency
			sent <- sendRecord{reason: reason}
			return nil
		})

		start := time.Now()
		if err := requester.RequestCredential(ctx, ReasonReplayRejected); err != nil {
			t.Fatalf("RequestCredential() error = %v", err)
		}
		if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
			t.Errorf("RequestCredential blocked for %v; the manager loop must never wait on WS I/O", elapsed)
		}

		select {
		case record := <-sent:
			if record.reason != ReasonReplayRejected {
				t.Errorf("sent reason = %q, want replay_rejected", record.reason)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("enqueued request was never sent")
		}
	})

	t.Run("rejects unknown reasons before any send", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var sendCalls int
		var mu sync.Mutex
		requester := NewControlCredentialRequester(ctx, func(ctx context.Context, reason CredentialRequestReason) error {
			mu.Lock()
			mu.Unlock()
			sendCalls++
			return nil
		})

		if err := requester.RequestCredential(ctx, CredentialRequestReason("because_i_said_so")); err == nil {
			t.Fatal("unknown reason must be rejected")
		}
		if err := requester.RequestCredential(ctx, ""); err == nil {
			t.Fatal("empty reason must be rejected")
		}
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		if sendCalls != 0 {
			t.Errorf("send calls = %d, want 0 for rejected reasons", sendCalls)
		}
	})

	t.Run("bounded queue fails fast instead of blocking", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		release := make(chan struct{})
		requester := NewControlCredentialRequester(ctx, func(ctx context.Context, reason CredentialRequestReason) error {
			<-release // block the single worker so the queue fills
			return nil
		})

		// Fill the queue to its bound: every enqueue returns immediately.
		for i := 0; i < credentialRequestQueueCapacity; i++ {
			if err := requester.RequestCredential(ctx, ReasonExpired); err != nil {
				t.Fatalf("RequestCredential(%d) error = %v", i, err)
			}
		}
		// One beyond the bound must fail fast (the manager retries with
		// backoff) instead of blocking the supervision loop.
		if err := requester.RequestCredential(ctx, ReasonExpired); err == nil {
			t.Fatal("enqueue beyond the queue bound must fail")
		}
		close(release)
	})

	t.Run("stops with the lifecycle context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		requester := NewControlCredentialRequester(ctx, func(ctx context.Context, reason CredentialRequestReason) error {
			return nil
		})
		cancel()
		if err := requester.RequestCredential(context.Background(), ReasonRestart); err == nil {
			t.Fatal("RequestCredential after lifecycle cancellation must fail")
		}
	})
}

func TestManagerBackoffCapsAt60SecondsWithJitter(t *testing.T) {
	// The ceiling doubles per attempt from the 1-second base and is capped
	// at 60 seconds (spec §14); the delay is uniform in [ceiling/2, ceiling).
	backoffBase := time.Second
	expectedCeilings := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		32 * time.Second,
		backoffCapDelay,
		backoffCapDelay,
		backoffCapDelay,
	}
	for attempt, expectedCeiling := range expectedCeilings {
		lowerBound := expectedCeiling / 2
		if got := backoffDelay(backoffBase, backoffCapDelay, attempt, 0); got != lowerBound {
			t.Errorf("backoffDelay(attempt %d, jitter 0) = %v, want %v", attempt, got, lowerBound)
		}
		if got := backoffDelay(backoffBase, backoffCapDelay, attempt, 1); got != expectedCeiling {
			t.Errorf("backoffDelay(attempt %d, jitter 1) = %v, want %v", attempt, got, expectedCeiling)
		}
		midpoint := backoffDelay(backoffBase, backoffCapDelay, attempt, 0.5)
		if midpoint <= lowerBound || midpoint >= expectedCeiling {
			t.Errorf("backoffDelay(attempt %d, jitter 0.5) = %v, want strictly inside (%v, %v)", attempt, midpoint, lowerBound, expectedCeiling)
		}
	}

	// Delays grow strictly across attempts until the cap engages.
	previous := time.Duration(0)
	for attempt := 0; attempt < 6; attempt++ {
		delay := backoffDelay(backoffBase, backoffCapDelay, attempt, 0.9)
		if delay <= previous {
			t.Errorf("backoffDelay(attempt %d) = %v did not grow past %v", attempt, delay, previous)
		}
		previous = delay
	}

	// Every attempt beyond the growth phase stays inside the cap, no matter
	// how large the attempt counter gets (overflow guard).
	for _, attempt := range []int{6, 20, 1000, 1 << 20} {
		for fraction := 0.0; fraction <= 1.0; fraction += 0.25 {
			delay := backoffDelay(backoffBase, backoffCapDelay, attempt, fraction)
			if delay > backoffCapDelay || delay < backoffCapDelay/2 {
				t.Fatalf("backoffDelay(attempt %d, jitter %v) = %v outside [%v, %v]", attempt, fraction, delay, backoffCapDelay/2, backoffCapDelay)
			}
		}
	}

	// Jitter must actually vary the delay for the same attempt.
	if backoffDelay(backoffBase, backoffCapDelay, 2, 0.1) == backoffDelay(backoffBase, backoffCapDelay, 2, 0.9) {
		t.Error("backoff delays with different jitter fractions are identical; jitter is missing")
	}
	if backoffDelay(backoffBase, backoffCapDelay, -5, 0.5) != backoffDelay(backoffBase, backoffCapDelay, 0, 0.5) {
		t.Error("negative attempt must clamp to the first attempt delay")
	}
}
