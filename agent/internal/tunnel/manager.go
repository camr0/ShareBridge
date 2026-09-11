package tunnel

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Lifecycle bounds (spec §14): restart delays grow exponentially with jitter
// and are capped at 60 seconds; a graceful stop gets a short grace period
// before the kill escalation; a credential request that control never answers
// is retried after a bounded wait.
const (
	backoffCapDelay              = 60 * time.Second
	defaultBackoffBase           = 1 * time.Second
	defaultKillGracePeriod       = 5 * time.Second
	defaultCredentialWaitTimeout = 30 * time.Second
	// defaultKillWaitTimeout bounds the post-kill wait: after SIGKILL has been
	// delivered the manager waits at most this long for the child to exit
	// before returning an explicit error. SIGKILL cannot be ignored, so this
	// covers only an unkillable process (e.g. blocked in uninterruptible
	// kernel I/O); without the bound such a child would block shutdown forever
	// (audit I4) and, because shutdown stopped the tunnel before the HTTPS
	// listener, leave the listener serving indefinitely.
	defaultKillWaitTimeout = 2 * time.Second
	// RunningStabilityWindow: a child that stays up this long is reported as
	// "running". This is process-liveness telemetry only (§7.4): control
	// never treats it as relay availability — the gateway presence view
	// (§7.3) is the sole authority for that.
	defaultRunningStabilityWindow = 10 * time.Second
	// A child that ran at least this long before exiting counts as a stable
	// run: the next exit restarts the backoff sequence from the base delay.
	stableUptimeResetThreshold = backoffCapDelay
	// Backoff ceilings double from this base: 1s, 2s, 4s ... capped at
	// backoffCapDelay.
	backoffCeilingGrowthLimit = 6
)

// errManagerStopped is returned by ApplyConfig after Stop has been called.
var errManagerStopped = errors.New("tunnel manager stopped")

// ErrChildKillTimeout is returned (wrapped) by Manager.Stop when the child has
// not exited within the bounded post-kill wait. Callers distinguish an
// unkillable child from other shutdown failures with errors.Is (audit I4).
var ErrChildKillTimeout = errors.New("tunnel child kill timed out")

// StatusKind is the closed telemetry status set of the §11.1
// relay_client_state message: "starting", "running", "stopped", "error".
type StatusKind string

// The four statuses control accepts for relay_client_state telemetry.
const (
	StatusStarting StatusKind = "starting"
	StatusRunning  StatusKind = "running"
	StatusStopped  StatusKind = "stopped"
	StatusError    StatusKind = "error"
)

// StatusReport is one diagnostic callback: the assignment generation, the
// status, and a short reason. Reasons never contain credential material.
type StatusReport struct {
	Generation int
	Status     StatusKind
	Reason     string
}

// CredentialRequestReason is one of the three closed §11.1
// relay_credential_request reason values: why the manager needs a fresh
// relay admission credential. The values are the exact wire values control
// strictly parses.
type CredentialRequestReason string

// The §11.1 relay_credential_request reasons:
//
//   - replay_rejected: the one-use jti was burned, so a re-Login would be
//     replay-rejected (any child exit is treated this way, fail-closed);
//   - expired: the armed credential is nearing expiry;
//   - restart: a fresh credential after a restart recovery.
const (
	ReasonReplayRejected CredentialRequestReason = "replay_rejected"
	ReasonExpired        CredentialRequestReason = "expired"
	ReasonRestart        CredentialRequestReason = "restart"
)

// validCredentialRequestReason reports whether reason is exactly one of the
// three §11.1 values.
func validCredentialRequestReason(reason CredentialRequestReason) bool {
	switch reason {
	case ReasonReplayRejected, ReasonExpired, ReasonRestart:
		return true
	default:
		return false
	}
}

// CredentialRequester asks control for a fresh relay admission credential by
// sending the §11.1 relay_credential_request message (with its reason) over
// the authenticated agent WebSocket. Implementations MUST NOT block the
// caller on WebSocket I/O: RequestCredential is invoked on the manager's
// supervision loop, which must never wait on the network (the production
// ControlCredentialRequester enqueues; the relay_config reply arrives later
// via ApplyConfig). Tests inject fakes.
type CredentialRequester interface {
	RequestCredential(ctx context.Context, reason CredentialRequestReason) error
}

// Control credential requester bounds (§16.4): the queue is bounded so a
// stalled WebSocket cannot buffer unbounded work, and each send gets a
// bounded write window.
const (
	// credentialRequestQueueCapacity bounds pending sends; an enqueue beyond
	// it fails fast and the manager retries with backoff.
	credentialRequestQueueCapacity = 4
	// credentialRequestSendTimeout bounds one WebSocket write.
	credentialRequestSendTimeout = 10 * time.Second
)

// ControlCredentialRequester is the production CredentialRequester: it owns a
// single worker goroutine that performs the §11.1 relay_credential_request
// send, so RequestCredential only enqueues and returns — the manager's run
// loop never blocks on WebSocket I/O (Task 9 carry-forward). The relay_config
// reply is NOT consumed here: it arrives on the WebSocket read loop and
// reaches the manager through ApplyConfig (the existing config-apply path).
type ControlCredentialRequester struct {
	send      func(ctx context.Context, reason CredentialRequestReason) error
	requests  chan CredentialRequestReason
	lifecycle context.Context
}

// NewControlCredentialRequester builds the production requester around the
// send function (wired to the signaling client's SendRelayCredentialRequest)
// and starts its worker. The lifecycle context is the WebSocket/session
// context: canceling it stops the worker and fails further requests.
func NewControlCredentialRequester(lifecycle context.Context, send func(ctx context.Context, reason CredentialRequestReason) error) *ControlCredentialRequester {
	requester := &ControlCredentialRequester{
		send:      send,
		requests:  make(chan CredentialRequestReason, credentialRequestQueueCapacity),
		lifecycle: lifecycle,
	}
	go requester.work()
	return requester
}

// work drains the queue until the lifecycle context ends. One send at a time,
// each with a bounded write window.
func (requester *ControlCredentialRequester) work() {
	for {
		select {
		case reason := <-requester.requests:
			sendCtx, cancelSend := context.WithTimeout(requester.lifecycle, credentialRequestSendTimeout)
			_ = requester.send(sendCtx, reason)
			cancelSend()
		case <-requester.lifecycle.Done():
			return
		}
	}
}

// RequestCredential validates the reason and enqueues the send without
// blocking (or erroring if the queue is full or the lifecycle has ended —
// the manager's retry machinery handles both). It never performs network
// I/O on the calling goroutine.
func (requester *ControlCredentialRequester) RequestCredential(ctx context.Context, reason CredentialRequestReason) error {
	if !validCredentialRequestReason(reason) {
		return fmt.Errorf("invalid relay credential request reason %q", string(reason))
	}
	if err := requester.lifecycle.Err(); err != nil {
		return fmt.Errorf("relay credential requester stopped: %w", err)
	}
	select {
	case requester.requests <- reason:
		return nil
	default:
		// Queue full, or the worker exited right after the lifecycle check
		// (benign race: a queued send would target a dead context anyway).
		// Either way fail fast; the manager retries with backoff.
		if err := requester.lifecycle.Err(); err != nil {
			return fmt.Errorf("relay credential requester stopped: %w", err)
		}
		return fmt.Errorf("relay credential request queue full (%d)", credentialRequestQueueCapacity)
	}
}

// childProcess is the supervisor's view of one frpc child.
type childProcess interface {
	// Wait blocks until the process exits and returns its wait error.
	Wait() error
	// GracefulStop asks the process to terminate cleanly (SIGTERM).
	GracefulStop() error
	// Kill forcefully terminates the process.
	Kill() error
}

// execChildProcess adapts an os/exec command. Wait is safe to call from
// multiple goroutines (the exit watcher and a concurrent stop escalation).
type execChildProcess struct {
	command  *exec.Cmd
	waitOnce sync.Once
	done     chan struct{}
	waitErr  error
}

func newExecChildProcess(command *exec.Cmd) *execChildProcess {
	return &execChildProcess{command: command, done: make(chan struct{})}
}

func (process *execChildProcess) Wait() error {
	process.waitOnce.Do(func() {
		process.waitErr = process.command.Wait()
		close(process.done)
	})
	<-process.done
	return process.waitErr
}

func (process *execChildProcess) GracefulStop() error {
	return process.command.Process.Signal(syscall.SIGTERM)
}

func (process *execChildProcess) Kill() error {
	return process.command.Process.Kill()
}

// processStarter launches one frpc child. The manager always passes an
// argument array (binary path plus "-c" and the generated config path), so no
// shell is ever involved; the seam exists for tests.
type processStarter func(ctx context.Context, binaryPath string, arguments []string) (childProcess, error)

// startFRPCProcess is the production starter: exec.CommandContext with an
// argument array, never a shell.
func startFRPCProcess(ctx context.Context, binaryPath string, arguments []string) (childProcess, error) {
	command := exec.CommandContext(ctx, binaryPath, arguments...)
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", binaryPath, err)
	}
	return newExecChildProcess(command), nil
}

// Settings are the fixed agent-owned tunnel settings (persisted by
// agent/internal/config): the bundled pinned frpc binary, the generated
// config path inside the tunnel data directory, the operator-provisioned
// relay transport CA bundle, and the fixed local HTTPS target.
type Settings struct {
	FRPCBinaryPath string
	ConfigPath     string
	TrustedCAFile  string
	LocalTarget    string
}

func (settings Settings) validate() error {
	if strings.TrimSpace(settings.FRPCBinaryPath) == "" {
		return errors.New("tunnel frpc binary path is required")
	}
	if strings.TrimSpace(settings.ConfigPath) == "" {
		return errors.New("tunnel config path is required")
	}
	if settings.LocalTarget != LocalTarget {
		return fmt.Errorf("tunnel local target must stay fixed at %s, got %q", LocalTarget, settings.LocalTarget)
	}
	// TrustedCAFile may legitimately be empty until the operator provisions
	// the relay CA bundle: rendering fails closed on it later.
	return nil
}

// eventKind discriminates the manager's internal event queue.
type eventKind int

const (
	eventApplyConfig eventKind = iota
	eventChildExited
	eventChildStable
)

type managerEvent struct {
	kind      eventKind
	config    Config
	child     childProcess
	waitError error
}

// timerKind discriminates the single pending restart timer.
type timerKind int

const (
	timerNone timerKind = iota
	timerStartChild
	timerRetryCredentialRequest
	timerCredentialWaitExpiry
)

// Manager supervises the pinned frpc child (§7.4): it validates relay_config
// payloads, atomically writes the mode-0600 generated config, starts the
// child without a shell, restarts it with bounded exponential backoff and
// jitter, replaces it only when a strictly higher generation arrives, and
// stops it on lockdown or daemon shutdown. All manager state is owned by the
// single run goroutine; external methods communicate through events.
type Manager struct {
	settings               Settings
	requester              CredentialRequester
	onStatus               func(StatusReport)
	startChild             processStarter
	backoffBase            time.Duration
	killGrace              time.Duration
	killWait               time.Duration
	credentialWaitTimeout  time.Duration
	runningStabilityWindow time.Duration

	rngMu sync.Mutex
	rng   *rand.Rand

	events      chan managerEvent
	stopChannel chan struct{}
	doneChannel chan struct{}
	stopOnce    sync.Once
	stopped     bool // set under stopOnce before stopChannel closes
	// stopErr records the outcome of the shutdown stop so every Stop caller
	// observes the SAME explicit error. Written by the run goroutine before
	// doneChannel closes and read only after <-doneChannel, so the channel
	// close provides the happens-before edge (no extra lock).
	stopErr error
	// publishedGeneration mirrors the armed generation for callers: -1 until
	// a first configuration is armed, otherwise the current generation. It
	// lets ApplyConfig reject stale generations synchronously.
	publishedGeneration atomic.Int64
	// childLive is the process-liveness bookkeeping: true from the moment a
	// child is started until its Wait returns (cleared by the watcher
	// goroutine, even after the supervision loop has exited). Atomic because
	// the watcher goroutine writes it outside the run goroutine's ownership;
	// it lets a late reap clear the "running" state instead of leaving it
	// permanently set (audit I4).
	childLive atomic.Bool

	// State below is owned by the run goroutine.
	armedConfig       Config
	hasArmedConfig    bool
	armedConsumed     bool // the armed credential was already used for one Login
	currentGeneration int
	child             childProcess
	childStartedAt    time.Time
	// childExited is closed when the current child's Wait returns; the run
	// goroutine waits on it instead of spawning a second Wait helper, so a
	// bounded post-kill wait leaks no goroutine. Owned by the run goroutine
	// (closed by the child's watcher goroutine).
	childExited             chan struct{}
	restartAttempt          int
	restartTimer            *time.Timer
	pendingTimer            timerKind
	waitingForCredential    bool
	pendingCredentialReason CredentialRequestReason
}

// ManagerOption adjusts test-visible lifecycle knobs.
type ManagerOption func(*Manager)

// ChildProcess is the exported alias of the supervisor's child-process view.
// The daemon package (and its tests) construct children through the
// WithProcessStarter seam, which needs a nameable type; the alias keeps the
// existing unexported spelling valid for the tunnel package's own tests.
type ChildProcess = childProcess

// ProcessStarter is the exported alias of the child-start seam (see
// ChildProcess).
type ProcessStarter = processStarter

// WithProcessStarter is the exported form of withProcessStarter: the daemon's
// tests supply a non-exec child seam so the full daemon→manager wiring runs
// without launching real processes.
func WithProcessStarter(starter ProcessStarter) ManagerOption {
	return withProcessStarter(starter)
}

// WithBackoffBase is the exported form of withBackoffBase (restart-backoff
// base; production default 1s).
func WithBackoffBase(base time.Duration) ManagerOption {
	return withBackoffBase(base)
}

// WithKillGracePeriod is the exported form of withKillGracePeriod (graceful
// stop → kill escalation window).
func WithKillGracePeriod(grace time.Duration) ManagerOption {
	return withKillGracePeriod(grace)
}

// WithKillWaitTimeout is the exported form of withKillWaitTimeout: the bound
// on the post-kill wait before Stop returns an explicit error (Round D, audit
// I4).
func WithKillWaitTimeout(timeout time.Duration) ManagerOption {
	return withKillWaitTimeout(timeout)
}

// WithCredentialWaitTimeout is the exported form of withCredentialWaitTimeout
// (how long the manager waits for control's relay_config before re-requesting).
func WithCredentialWaitTimeout(timeout time.Duration) ManagerOption {
	return withCredentialWaitTimeout(timeout)
}

// WithRunningStabilityWindow is the exported form of
// withRunningStabilityWindow (uptime before the "running" telemetry fires).
func WithRunningStabilityWindow(window time.Duration) ManagerOption {
	return withRunningStabilityWindow(window)
}

func withProcessStarter(starter processStarter) ManagerOption {
	return func(manager *Manager) { manager.startChild = starter }
}

func withBackoffBase(base time.Duration) ManagerOption {
	return func(manager *Manager) { manager.backoffBase = base }
}

func withKillGracePeriod(grace time.Duration) ManagerOption {
	return func(manager *Manager) { manager.killGrace = grace }
}

func withKillWaitTimeout(timeout time.Duration) ManagerOption {
	return func(manager *Manager) { manager.killWait = timeout }
}

func withCredentialWaitTimeout(timeout time.Duration) ManagerOption {
	return func(manager *Manager) { manager.credentialWaitTimeout = timeout }
}

func withRunningStabilityWindow(window time.Duration) ManagerOption {
	return func(manager *Manager) { manager.runningStabilityWindow = window }
}

// NewManager validates the fixed settings and starts the supervision loop.
// The status callback is invoked from the manager goroutine and must not
// block; reasons never contain credential material.
func NewManager(settings Settings, requester CredentialRequester, onStatus func(StatusReport), options ...ManagerOption) (*Manager, error) {
	if err := settings.validate(); err != nil {
		return nil, err
	}
	manager := &Manager{
		settings:               settings,
		requester:              requester,
		onStatus:               onStatus,
		startChild:             startFRPCProcess,
		backoffBase:            defaultBackoffBase,
		killGrace:              defaultKillGracePeriod,
		killWait:               defaultKillWaitTimeout,
		credentialWaitTimeout:  defaultCredentialWaitTimeout,
		runningStabilityWindow: defaultRunningStabilityWindow,
		events:                 make(chan managerEvent, 32),
		stopChannel:            make(chan struct{}),
		doneChannel:            make(chan struct{}),
	}
	for _, option := range options {
		option(manager)
	}
	if manager.rng == nil {
		manager.rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	manager.publishedGeneration.Store(-1)
	go manager.run()
	return manager, nil
}

// ApplyConfig feeds one validated relay_config into the supervision loop.
// Higher generations replace a running child; same-generation messages with a
// fresh credential arm it for the next (re)start; lower generations are
// rejected. The credential value is never logged and never appears in errors.
func (manager *Manager) ApplyConfig(config Config) error {
	if err := config.Validate(); err != nil {
		manager.emit(config.Generation, StatusError, fmt.Sprintf("invalid relay_config rejected: %v", err))
		return err
	}
	if manager.isStopped() {
		return errManagerStopped
	}
	// Generations are monotonic (§7.4): reject stale generations
	// synchronously so the caller learns the message was fenced out.
	if current := manager.publishedGeneration.Load(); current >= 0 && config.Generation < int(current) {
		manager.emit(config.Generation, StatusError,
			fmt.Sprintf("rejected stale relay_config generation %d below current %d", config.Generation, current))
		return fmt.Errorf("relay_config generation %d older than current generation %d", config.Generation, current)
	}
	select {
	case manager.events <- managerEvent{kind: eventApplyConfig, config: config}:
		return nil
	case <-manager.doneChannel:
		return errManagerStopped
	}
}

// Stop shuts the tunnel down: the child is stopped gracefully and killed if
// it ignores the graceful stop. The post-kill wait is BOUNDED (killWait): if
// the child has still not exited, Stop returns an explicit error instead of
// blocking shutdown forever (audit I4) — the daemon treats the failure as
// best-effort and runs its remaining teardown levers regardless. Stop is
// idempotent (every call returns the same recorded outcome) and blocks until
// the supervision loop has exited.
func (manager *Manager) Stop() error {
	manager.stopOnce.Do(func() {
		manager.stopped = true
		close(manager.stopChannel)
	})
	<-manager.doneChannel
	return manager.stopErr
}

func (manager *Manager) isStopped() bool {
	select {
	case <-manager.stopChannel:
		return true
	default:
		return false
	}
}

// run is the supervision loop; it owns all mutable manager state.
func (manager *Manager) run() {
	defer close(manager.doneChannel)
	manager.restartTimer = time.NewTimer(0)
	if !manager.restartTimer.Stop() {
		<-manager.restartTimer.C
	}
	manager.pendingTimer = timerNone
	for {
		select {
		case <-manager.stopChannel:
			// Record the outcome before the deferred close(doneChannel) makes it
			// visible to every Stop caller.
			manager.stopErr = manager.shutdownChild()
			return
		case event := <-manager.events:
			switch event.kind {
			case eventApplyConfig:
				manager.handleApplyConfig(event.config)
			case eventChildExited:
				manager.handleChildExited(event)
			case eventChildStable:
				if manager.child == event.child {
					manager.emit(manager.currentGeneration, StatusRunning, "frpc process stable")
				}
			}
		case <-manager.restartTimer.C:
			manager.handleTimerFired()
		}
	}
}

func (manager *Manager) handleApplyConfig(config Config) {
	if manager.hasArmedConfig {
		switch {
		case config.Generation < manager.currentGeneration:
			// Generations are monotonic (§7.4): a stale message is fenced
			// out without any state change.
			manager.emit(manager.currentGeneration, StatusError,
				fmt.Sprintf("ignored stale relay_config generation %d below current %d", config.Generation, manager.currentGeneration))
			return
		case config.Generation == manager.currentGeneration:
			if config.Credential == manager.armedConfig.Credential {
				return // identical message: no-op
			}
			// Same-generation credential refresh (fresh one-use jti for the
			// current assignment, §15.2): arm and persist it for the next
			// start, but never restart a healthy child — replacement of a
			// running child happens only on a strictly higher generation.
			manager.armedConfig = config
			manager.armedConsumed = false
			_ = manager.writeArmedConfig()
			if manager.waitingForCredential && manager.child == nil {
				manager.waitingForCredential = false
				manager.scheduleTimer(timerStartChild, manager.backoffDelayForAttempt())
			}
			return
		}
	}

	// First configuration or a strictly higher generation: arm it and make
	// it effective, replacing a running child (§7.4 step 5).
	manager.armedConfig = config
	manager.hasArmedConfig = true
	manager.armedConsumed = false
	manager.currentGeneration = config.Generation
	manager.publishedGeneration.Store(int64(config.Generation))
	manager.waitingForCredential = false
	if err := manager.writeArmedConfig(); err != nil {
		return // diagnostic already emitted; the start retry path re-renders
	}
	if manager.child != nil {
		if err := manager.stopChildGracefullyOrKill(); err != nil {
			// The old child could not be reaped within the bound: keep tracking
			// it and refuse to start a second child on top of it. Its watcher
			// still owns the exit event, so a later reap clears the bookkeeping
			// and the armed (newer) configuration restarts then.
			manager.emit(manager.currentGeneration, StatusError,
				fmt.Sprintf("replacing frpc child: %v", err))
			return
		}
	}
	manager.startChildNow()
}

func (manager *Manager) handleChildExited(event managerEvent) {
	if event.child != manager.child {
		return // exit of an already-replaced child
	}
	uptime := time.Since(manager.childStartedAt)
	manager.child = nil
	manager.childExited = nil
	if uptime >= stableUptimeResetThreshold {
		manager.restartAttempt = 0
	}
	manager.emit(manager.currentGeneration, StatusStopped, fmt.Sprintf("frpc exited: %v", event.waitError))
	// The credential's one-use jti may now be burned at the relay plugin:
	// never re-login with it (§15.2). Any restart goes through a fresh
	// relay_credential_request first, with the replay_rejected reason: the
	// next Login with the consumed jti would be replay-rejected (any child
	// exit is treated as a burned jti, fail-closed).
	manager.armedConsumed = true
	manager.waitingForCredential = true
	manager.pendingCredentialReason = ReasonReplayRejected
	manager.requestCredentialAndScheduleRestart()
}

func (manager *Manager) handleTimerFired() {
	kind := manager.pendingTimer
	manager.pendingTimer = timerNone
	switch kind {
	case timerStartChild:
		manager.startChildNow()
	case timerRetryCredentialRequest, timerCredentialWaitExpiry:
		if manager.child == nil && manager.waitingForCredential {
			manager.requestCredentialAndScheduleRestart()
		}
	}
}

// startChildNow starts the armed configuration if it has not been consumed;
// a consumed credential must first be refreshed (§15.2).
func (manager *Manager) startChildNow() {
	if manager.child != nil || !manager.hasArmedConfig {
		return
	}
	if manager.armedConsumed {
		manager.requestCredentialAndScheduleRestart()
		return
	}
	if err := manager.writeArmedConfig(); err != nil {
		manager.restartAttempt++
		manager.scheduleTimer(timerStartChild, manager.backoffDelayForAttempt())
		return
	}
	arguments := []string{"-c", manager.settings.ConfigPath}
	child, err := manager.startChild(context.Background(), manager.settings.FRPCBinaryPath, arguments)
	if err != nil {
		manager.emit(manager.currentGeneration, StatusError, fmt.Sprintf("frpc start failed: %v", err))
		manager.restartAttempt++
		manager.scheduleTimer(timerStartChild, manager.backoffDelayForAttempt())
		return
	}
	manager.child = child
	manager.childStartedAt = time.Now()
	manager.armedConsumed = true
	// The exit channel is owned by this child's watcher: the stop path waits
	// on it instead of spawning a second Wait helper goroutine (Round D).
	exited := make(chan struct{})
	manager.childExited = exited
	manager.childLive.Store(true)
	manager.emit(manager.currentGeneration, StatusStarting, "frpc started")
	go manager.watchChild(child, exited)
	if manager.runningStabilityWindow > 0 {
		go manager.notifyRunningAfterStability(child)
	}
}

// requestCredentialAndScheduleRestart asks control for a fresh admission
// credential over the authenticated WebSocket (§15.2) and arms the retry
// machinery: on success a bounded wait re-requests if control never answers;
// on failure the request itself is retried with backoff. Either way the
// manager never restarts the child with the stale credential. The §11.1
// reason carried is the one recorded when the wait began (replay_rejected
// today: every current recovery state is the burned-jti path).
func (manager *Manager) requestCredentialAndScheduleRestart() {
	if manager.requester == nil {
		manager.emit(manager.currentGeneration, StatusError, "credential requester unavailable; tunnel stays down")
		return
	}
	reason := manager.pendingCredentialReason
	if !validCredentialRequestReason(reason) {
		reason = ReasonReplayRejected
	}
	requestContext, cancelRequest := context.WithTimeout(context.Background(), manager.credentialWaitTimeout)
	defer cancelRequest()
	if err := manager.requester.RequestCredential(requestContext, reason); err != nil {
		manager.emit(manager.currentGeneration, StatusError, fmt.Sprintf("relay credential request failed: %v", err))
		manager.restartAttempt++
		manager.scheduleTimer(timerRetryCredentialRequest, manager.backoffDelayForAttempt())
		return
	}
	manager.scheduleTimer(timerCredentialWaitExpiry, manager.credentialWaitTimeout)
}

// watchChild relays the child's exit into the event loop. It clears the
// liveness bookkeeping and closes the per-child exit channel BEFORE the event
// send: clearing first means a replacement child started right after this
// exit can never be marked not-live by a late store, and it guarantees a
// reaped child is never left permanently "running" — even after the
// supervision loop exited.
func (manager *Manager) watchChild(child childProcess, exited chan struct{}) {
	waitError := child.Wait()
	manager.childLive.Store(false)
	close(exited)
	select {
	case manager.events <- managerEvent{kind: eventChildExited, child: child, waitError: waitError}:
	case <-manager.doneChannel:
	}
}

// notifyRunningAfterStability reports the process-liveness "running"
// telemetry once the child has stayed up for the stability window. This is
// diagnostic only and never a relay availability fact (§7.4).
func (manager *Manager) notifyRunningAfterStability(child childProcess) {
	timer := time.NewTimer(manager.runningStabilityWindow)
	defer timer.Stop()
	select {
	case <-timer.C:
		select {
		case manager.events <- managerEvent{kind: eventChildStable, child: child}:
		case <-manager.doneChannel:
		}
	case <-manager.doneChannel:
	}
}

// stopChildGracefullyOrKill stops the current child: graceful first, then a
// kill after the grace period, then a BOUNDED wait for the exit. If the child
// still has not exited, it is left tracked (its watcher owns the eventual
// exit) and an explicit error is returned so callers can surface it and keep
// shutdown moving. It is called from the run goroutine only, and holds no lock
// across any wait.
func (manager *Manager) stopChildGracefullyOrKill() error {
	child := manager.child
	if child == nil {
		return nil
	}
	exited := manager.childExited
	_ = child.GracefulStop()
	select {
	case <-exited:
	case <-time.After(manager.killGrace):
		_ = child.Kill()
		select {
		case <-exited:
		case <-time.After(manager.killWait):
			return fmt.Errorf("%w: did not exit within %s after kill", ErrChildKillTimeout, manager.killWait)
		}
	}
	manager.child = nil
	manager.childExited = nil
	return nil
}

// shutdownChild is the Stop path: stop the child and report final telemetry.
// The child-stop error (an unkillable process) is returned rather than
// swallowed, and the final stopped telemetry is always emitted.
func (manager *Manager) shutdownChild() error {
	stopErr := manager.stopChildGracefullyOrKill()
	manager.clearRestartTimer()
	if stopErr != nil {
		manager.emit(manager.currentGeneration, StatusError,
			fmt.Sprintf("tunnel child did not stop: %v", stopErr))
	}
	manager.emit(manager.currentGeneration, StatusStopped, "tunnel manager stopped")
	return stopErr
}

// writeArmedConfig renders the armed configuration and installs it
// atomically with mode 0600.
func (manager *Manager) writeArmedConfig() error {
	rendered, err := Render(manager.armedConfig, RenderSettings{
		TrustedCAFile: manager.settings.TrustedCAFile,
		LocalTarget:   manager.settings.LocalTarget,
	})
	if err != nil {
		manager.emit(manager.currentGeneration, StatusError, fmt.Sprintf("render tunnel config: %v", err))
		return err
	}
	if err := WriteConfigFile(manager.settings.ConfigPath, rendered); err != nil {
		manager.emit(manager.currentGeneration, StatusError, fmt.Sprintf("write tunnel config: %v", err))
		return err
	}
	return nil
}

func (manager *Manager) scheduleTimer(kind timerKind, delay time.Duration) {
	if !manager.restartTimer.Stop() {
		select {
		case <-manager.restartTimer.C:
		default:
		}
	}
	manager.pendingTimer = kind
	manager.restartTimer.Reset(delay)
}

func (manager *Manager) clearRestartTimer() {
	if !manager.restartTimer.Stop() {
		select {
		case <-manager.restartTimer.C:
		default:
		}
	}
	manager.pendingTimer = timerNone
}

func (manager *Manager) backoffDelayForAttempt() time.Duration {
	return backoffDelay(manager.backoffBase, backoffCapDelay, manager.restartAttempt, manager.randomFraction())
}

func (manager *Manager) randomFraction() float64 {
	manager.rngMu.Lock()
	defer manager.rngMu.Unlock()
	return manager.rng.Float64()
}

func (manager *Manager) emit(generation int, status StatusKind, reason string) {
	if manager.onStatus == nil {
		return
	}
	manager.onStatus(StatusReport{Generation: generation, Status: status, Reason: reason})
}

// backoffDelay computes one restart delay: the ceiling doubles per attempt
// from the base and is capped (spec §14: exponential with jitter, capped at
// 60 seconds); the delay is uniform in [ceiling/2, ceiling) so retries
// across a fleet stay desynchronized. The fraction must lie in [0, 1].
func backoffDelay(base time.Duration, capDelay time.Duration, attempt int, jitterFraction float64) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if jitterFraction != jitterFraction || jitterFraction < 0 {
		jitterFraction = 0
	}
	if jitterFraction > 1 {
		jitterFraction = 1
	}
	var ceiling time.Duration
	if attempt >= backoffCeilingGrowthLimit {
		ceiling = capDelay
	} else {
		ceiling = base << attempt
		if ceiling <= 0 || ceiling > capDelay {
			ceiling = capDelay
		}
	}
	half := ceiling / 2
	return half + time.Duration(jitterFraction*float64(half))
}
