package tunnel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
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
	// defaultChildSignalTimeout bounds each child signal CALL (graceful stop,
	// kill). The calls are process/OS interactions and return promptly in
	// practice; the bound exists so a call that never returns cannot strand
	// shutdown (Round D fix-round, audit A: "GracefulStop, Kill ... remain
	// unbounded by Manager-owned timers").
	defaultChildSignalTimeout = 1 * time.Second
	// defaultStatusDrainTimeout bounds how long shutdown waits for the status
	// emission worker to deliver the reports already queued. A control-facing
	// status callback that blocks (production: a relay_client_state send whose
	// context timeout is cooperative) is abandoned at the bound instead of
	// parking the supervision loop (Round D fix-round, audit A).
	defaultStatusDrainTimeout = 1 * time.Second
	// defaultStatusQueueSize is the bounded emission queue: enough headroom
	// for the manager's low-rate transitions with a responsive callback, small
	// enough that a wedged callback cannot accumulate unbounded reports.
	defaultStatusQueueSize = 64
	// defaultStopMargin is the slack Stop allows the supervision loop beyond
	// its own worst-case bounded shutdown before Stop stops waiting and reports
	// the failure itself. It only absorbs scheduling latency: the loop's bound
	// is derived from the same knobs.
	defaultStopMargin = 250 * time.Millisecond
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

// Reconnect-failure detection bounds. The relay credential is single-use and
// short-lived, and frp v0.71 applies loginFailExit only to the FIRST login
// (client/service.go:262) while hard-coding false on the reconnect path
// (:277-287) — so a rejected RE-login leaves frpc running and retrying a
// burned credential forever, with no child exit for the supervisor to observe.
// The supervisor therefore watches the child's output for a rejected
// reconnect Login. It matches a PAIR of substrings rather than one long pinned
// phrase: the prefix frpc prints for ANY failed Login attempt, plus the
// rejection marker that means the relay plugin refused the credential. A plain
// connection failure (frps down: refused, timed out, DNS failure) carries the
// same prefix but no rejection marker, and must not on its own trigger a
// credential refresh — an unreachable server is not a burned credential. A
// bounded consecutive-failure safety net (below) keeps a future FRP wording
// change from silently disabling recovery entirely.
const (
	// frpcReconnectFailureLine is the EXACT frpc v0.71.0 CLIENT-side text of a
	// rejected reconnect Login, captured from the real checksum-pinned v0.71.0
	// client binary (sha256 3ce4ba70ffce7da4026940586c5f3454df50814f4c050d6560efc556b3adef48)
	// by running it against the live relay with a deliberately invalid
	// credential. frpc's own format string is "connect to server error: %v"
	// (client/service.go:323), the relay plugin's "request rejected" refusal
	// reason reaches that %v, and frpc logs one such line per failed Login
	// attempt on the reconnect path. NOTE: "register control error" is the frps
	// SERVER-side wording (it does not occur in the client binary) — keying on
	// it made the previous detection inert. This is the single canonical
	// spelling of the pinned phrase; it MUST be re-captured and regression-tested
	// against a live rejected reconnect on ANY FRP upgrade.
	frpcReconnectFailureLine = "connect to server error: request rejected"
	// frpcConnectionErrorPrefix is the invariant part of frpc's failed-Login
	// report; the %v it wraps differs by cause, so this prefix alone is NOT a
	// rejection (an unreachable frps produces it too).
	frpcConnectionErrorPrefix = "connect to server error:"
	// frpcRejectionMarker is the rejection text the relay plugin's Login
	// refusal carries through frpc's %v. The primary matcher requires BOTH this
	// marker and frpcConnectionErrorPrefix, which is what keeps frps downtime
	// from triggering a credential refresh.
	frpcRejectionMarker = "request rejected"
	// frpcRetryAttemptLine is the benign INFO line frpc logs immediately before
	// EVERY Login attempt (client/service.go:312). The consecutive-error counter
	// ignores it so the interleaved "try to connect" line cannot break a run of
	// real failures.
	frpcRetryAttemptLine = "try to connect to server..."
	// frpcConsecutiveConnectionErrorThreshold is the bounded safety net that
	// keeps recovery alive if a future FRP release renames the rejection text:
	// after this many CONSECUTIVE failed-Login lines for the current child (no
	// intervening progress line), a refresh is requested even though the primary
	// pair did not match. Three attempts is deliberate: frpc backs off between
	// attempts (jittered 1s base, doubling, capped at 20s — client/service.go:355-361),
	// so three consecutive failures span several backoff steps and cannot be one
	// transient blip (e.g. a brief frps restart), while recovery still completes
	// well inside the ten-minute credential lifetime. The trade-off is that
	// sustained transport failures (a long frps outage) also reach the threshold
	// and request a credential; the primary pair is unaffected and a single
	// downtime line never trips the net.
	frpcConsecutiveConnectionErrorThreshold = 3

	// maxChildOutputLineLength bounds one retained child output line, so a
	// pathological or hostile child stream cannot grow manager memory. A line
	// beyond the bound is discarded (it cannot be the short pinned line).
	maxChildOutputLineLength = 8 * 1024
	// childOutputReadBufferSize is the fixed bufio buffer used per child stream.
	childOutputReadBufferSize = 4096
)

// isReconnectFailureLine reports whether one captured child output line is the
// PRIMARY reconnect-rejection signal: frpc's failed-Login prefix AND the
// server-side rejection marker. Requiring both is what keeps an unreachable
// relay (a plain "connect to server error: ... connection refused") from
// requesting a credential refresh. It exists so the matching is a single,
// directly testable predicate.
func isReconnectFailureLine(line string) bool {
	return isConnectionErrorLine(line) && strings.Contains(line, frpcRejectionMarker)
}

// isConnectionErrorLine reports whether the line is ANY failed-Login report,
// whatever the wrapped error text. Used only by the bounded
// consecutive-failure safety net.
func isConnectionErrorLine(line string) bool {
	return strings.Contains(line, frpcConnectionErrorPrefix)
}

// isRetryAttemptLine reports whether the line is frpc's benign pre-attempt INFO
// line, which must not reset the consecutive-failure run.
func isRetryAttemptLine(line string) bool {
	return strings.Contains(line, frpcRetryAttemptLine)
}

// reconnectFailureDetector decides when one child's captured output should
// drive credential-refresh recovery. It is owned by that child's output watcher
// goroutine, so a replacement child starts with a FRESH detector: the
// consecutive-failure count is per current child and cannot leak across a
// replacement. observe reports whether the line is a recovery trigger, either:
//
//   - the primary pair (frpc's failed-Login prefix + the rejection marker), or
//   - the safety net tripping after frpcConsecutiveConnectionErrorThreshold
//     consecutive failed-Login lines for this child.
//
// A run is reset by any line that is neither a connection error nor frpc's
// interleaved "try to connect" pre-attempt line: such a line means the child
// made progress — notably frpc's "login to server success" line, which a child
// that reaches the stability window must have emitted — so the earlier failures
// are no longer consecutive. A reset therefore covers both a successful login
// and a stable child.
type reconnectFailureDetector struct {
	consecutiveConnectionErrors int
}

func (detector *reconnectFailureDetector) observe(line string) bool {
	switch {
	case isReconnectFailureLine(line):
		// The rejection is definitive: recover now and end the run.
		detector.consecutiveConnectionErrors = 0
		return true
	case isConnectionErrorLine(line):
		detector.consecutiveConnectionErrors++
		return detector.consecutiveConnectionErrors >= frpcConsecutiveConnectionErrorThreshold
	case isRetryAttemptLine(line):
		// Interleaved with every attempt: neither progress nor a new failure.
		return false
	default:
		// Any other output is progress (a successful Login, or traffic after it):
		// the consecutive-failure run is over.
		detector.consecutiveConnectionErrors = 0
		return false
	}
}

// errManagerStopped is returned by ApplyConfig after Stop has been called.
var errManagerStopped = errors.New("tunnel manager stopped")

// ErrChildKillTimeout is returned (wrapped) by Manager.Stop when the child has
// not exited within the bounded post-kill wait. Callers distinguish an
// unkillable child from other shutdown failures with errors.Is (audit I4).
var ErrChildKillTimeout = errors.New("tunnel child kill timed out")

// ErrChildSignalTimeout is returned (wrapped) by Manager.Stop when a child
// signal call (graceful stop or kill) did not return within the
// Manager-owned bound: the process/OS interaction itself is wedged, so the
// child's stop could not be driven to completion (Round D fix-round).
var ErrChildSignalTimeout = errors.New("tunnel child signal timed out")

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

// childSignalCall is the single-flight record of the manager's one allowed
// outstanding child signal call. Go cannot cancel a blocking syscall, so a
// signal call that ignores its bound is abandoned on its goroutine; the
// manager keeps at most ONE such goroutine alive at a time, so repeated
// Stop/replacement calls cannot accumulate them (Round D fix-round 2, audit
// A).
type childSignalCall struct {
	name string
	// returned receives the call's own result exactly once. It is buffered so
	// the goroutine always finishes its send and exits, even when the waiter
	// has already given up on the bound.
	returned chan error
	// timeoutErr is the bound error recorded when the waiter gave up. It is
	// written and read only under Manager.signalMu.
	timeoutErr error
	// finishOnce makes the slot release idempotent between the returning
	// goroutine and a waiter that observed the result.
	finishOnce sync.Once
}

// execChildProcess adapts an os/exec command. Wait is safe to call from
// multiple goroutines (the exit watcher and a concurrent stop escalation). It
// also owns the child's captured stdout/stderr: both streams are pumped
// through BOUNDED line readers and forwarded to the supervisor's output
// watcher, which needs them only to detect the pinned reconnect-failure line.
// Raw child output is never logged, persisted, or copied into telemetry.
type execChildProcess struct {
	command  *exec.Cmd
	waitOnce sync.Once
	done     chan struct{}
	waitErr  error

	// outputLines carries the child's combined stdout/stderr lines to the
	// output watcher. The closer goroutine closes it exactly once, after both
	// pumps return, which is what ends the watcher.
	outputLines chan string
	// outputDone releases a pump blocked on outputLines once the process has
	// exited and no consumer remains. It is closed by Wait, before done.
	outputDone chan struct{}
	outputWG   sync.WaitGroup
}

// newExecChildProcess builds the adapter around a started command and its
// captured stream read ends. Each reader is drained by its own pump; the
// closer goroutine closes outputLines after the last pump returns.
func newExecChildProcess(command *exec.Cmd, readers []io.ReadCloser) *execChildProcess {
	process := &execChildProcess{
		command:     command,
		done:        make(chan struct{}),
		outputLines: make(chan string),
		outputDone:  make(chan struct{}),
	}
	for _, reader := range readers {
		process.outputWG.Add(1)
		go process.pumpOutput(reader)
	}
	go func() {
		process.outputWG.Wait()
		close(process.outputLines)
	}()
	return process
}

// OutputLines exposes the bounded, sanitised line stream of this child. It is
// the optional capability the supervisor's output watcher consumes.
func (process *execChildProcess) OutputLines() <-chan string {
	return process.outputLines
}

// pumpOutput drains one child stream to EOF, closing the read end so no
// descriptor survives the child.
func (process *execChildProcess) pumpOutput(reader io.ReadCloser) {
	defer process.outputWG.Done()
	defer reader.Close()
	pumpBoundedLines(bufio.NewReaderSize(reader, childOutputReadBufferSize), process.outputLines, process.outputDone)
}

// pumpBoundedLines forwards each bounded line to lines until the reader ends
// or done closes. Every line is discarded by the caller once matched; nothing
// here logs or persists it.
func pumpBoundedLines(reader *bufio.Reader, lines chan<- string, done <-chan struct{}) {
	for {
		line, truncated, err := readBoundedLine(reader)
		if line != "" && !truncated {
			select {
			case lines <- line:
			case <-done:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// readBoundedLine reads one newline-terminated line, retaining at most
// maxChildOutputLineLength bytes and discarding the remainder of an over-long
// line while still draining it (the child's pipe must never block). truncated
// reports that the returned text is not the whole line, in which case it must
// not be matched against the pinned failure substring. err is the read error
// (io.EOF at stream end); a partial final line is returned alongside it.
func readBoundedLine(reader *bufio.Reader) (line string, truncated bool, err error) {
	var retained []byte
	for {
		fragment, readErr := reader.ReadSlice('\n')
		if len(fragment) > 0 {
			if remaining := maxChildOutputLineLength - len(retained); remaining >= len(fragment) {
				retained = append(retained, fragment...)
			} else {
				truncated = true
				if remaining > 0 {
					retained = append(retained, fragment[:remaining]...)
				}
			}
		}
		switch {
		case readErr == nil:
			return string(retained), truncated, nil
		case errors.Is(readErr, bufio.ErrBufferFull):
			continue
		default:
			return string(retained), truncated, readErr
		}
	}
}

func (process *execChildProcess) Wait() error {
	process.waitOnce.Do(func() {
		process.waitErr = process.command.Wait()
		// Release any pump blocked on a line send, then signal completion. Both
		// closes are idempotent via waitOnce.
		close(process.outputDone)
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
// argument array, never a shell. Both stdout and stderr are piped into bounded
// line readers (see execChildProcess) so the supervisor can detect the pinned
// reconnect-failure line; the parent's write-end copies are closed immediately
// after Start so the read ends see EOF as soon as the child exits.
func startFRPCProcess(ctx context.Context, binaryPath string, arguments []string) (childProcess, error) {
	command := exec.CommandContext(ctx, binaryPath, arguments...)
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create frpc stdout pipe: %w", err)
	}
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
		return nil, fmt.Errorf("create frpc stderr pipe: %w", err)
	}
	command.Stdout = stdoutWriter
	command.Stderr = stderrWriter
	if err := command.Start(); err != nil {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
		_ = stderrReader.Close()
		_ = stderrWriter.Close()
		return nil, fmt.Errorf("start %s: %w", binaryPath, err)
	}
	// The child holds duplicated write ends now. exec.Cmd does not own *os.File
	// writers, so closing the parent's copies here is what lets the pumps see
	// EOF on child exit without leaking a descriptor.
	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()
	return newExecChildProcess(command, []io.ReadCloser{stdoutReader, stderrReader}), nil
}

// childOutputStream is the optional capability of a child process whose
// combined stdout/stderr the supervisor watches for the pinned
// reconnect-failure line. The production execChildProcess implements it, and
// test fakes may; a child without it simply has no output-based detection and
// still gets exit-based recovery.
type childOutputStream interface {
	OutputLines() <-chan string
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
	// eventChildReconnectFailed is the sanitised internal signal that the
	// CURRENT child needs credential-refresh recovery: either its reconnect
	// Login was rejected (the pinned frpc v0.71 pair was seen) or its output hit
	// the bounded consecutive-failure safety net. It carries the child identity
	// so a stale child's output can never drive recovery for a newer child, and
	// it exists because frp v0.71 does NOT exit frpc on a rejected reconnect.
	eventChildReconnectFailed
	// eventEnsureCredential is the daemon's bootstrap/unlock trigger asking the
	// manager to ensure a usable credential is held, with the manager's bounded
	// retry machinery behind it.
	eventEnsureCredential
)

type managerEvent struct {
	kind             eventKind
	config           Config
	child            childProcess
	waitError        error
	credentialReason CredentialRequestReason
}

// timerKind discriminates the single pending restart timer.
type timerKind int

const (
	timerNone timerKind = iota
	timerStartChild
	timerApplyArmedConfig
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
	childSignalTimeout     time.Duration
	statusDrainTimeout     time.Duration
	credentialWaitTimeout  time.Duration
	runningStabilityWindow time.Duration

	rngMu sync.Mutex
	rng   *rand.Rand

	events      chan managerEvent
	stopChannel chan struct{}
	doneChannel chan struct{}
	stopOnce    sync.Once
	// stopped is the manager's authoritative stopped state. It is written by
	// Stop's stopOnce closure and read by startChildFenced, both under startMu,
	// so the stopped transition is atomic with the check-and-spawn it fences.
	// stopChannel is closed under the same lock (see Stop), which keeps the
	// cheap isStopped() early exits coherent with this boolean.
	stopped bool
	// stopErr records the outcome of the shutdown stop so every Stop caller
	// that observes doneChannel observes the SAME explicit error. Written by
	// the run goroutine before doneChannel closes and read only after
	// <-doneChannel, so the channel close provides the happens-before edge (no
	// extra lock). A Stop caller whose own bound expires never reads it.
	stopErr error
	// statusQueue carries status reports to the single emission worker, which
	// invokes onStatus strictly in FIFO order. Emission is asynchronous so a
	// blocking callback can never wedge the supervision loop or shutdown
	// (Round D fix-round, audit A).
	statusQueue chan StatusReport
	// emitStop asks the worker to drain what is queued and exit; emitDone
	// closes when it has. Both are closed/written only through
	// finalizeStatus/emitStopOnce.
	emitStop     chan struct{}
	emitDone     chan struct{}
	emitStopOnce sync.Once
	// emitMu guards emissionOpen. It is only ever held for the flag check and
	// a non-blocking channel send — never across a wait — so it cannot
	// deadlock shutdown or the direct-serve handoff.
	emitMu       sync.Mutex
	emissionOpen bool
	// droppedStatus counts status reports that were never delivered: either
	// the bounded queue was full (the callback cannot keep up) or shutdown's
	// bounded drain expired with reports still queued. Every drop is also
	// logged, so a loss is never silent.
	droppedStatus atomic.Uint64
	// signalMu guards the single-flight signal slot: outstandingSignal and the
	// bound error recorded on it. It is only ever held for the slot
	// check/install/clear and the recorded-error read — never across a wait —
	// so it cannot deadlock shutdown.
	signalMu sync.Mutex
	// outstandingSignal is the single-flight slot: non-nil from the instant the
	// manager's one allowed signal-call goroutine is spawned until that
	// goroutine returns and releases it. While it is set every further child
	// signal request is refused without spawning, so a wedged GracefulStop/Kill
	// can never accumulate abandoned goroutines. A rebuilt manager gets its own
	// slot; one manager instance never exceeds one outstanding call.
	outstandingSignal *childSignalCall
	// outstandingSignalCalls counts the manager's live signal-call goroutines.
	// It is a test seam and the asserted invariant: it never exceeds one.
	outstandingSignalCalls atomic.Int64
	// publishedGeneration mirrors the armed generation for callers: -1 until
	// a first configuration is armed, otherwise the current generation. It
	// lets ApplyConfig reject stale generations synchronously.
	publishedGeneration atomic.Int64
	// credentialArmed is true once ANY valid relay_config has been accepted
	// (armed), for the lifetime of this manager. ApplyConfig sets it
	// synchronously before returning, so a caller that just delivered a
	// credential (or a daemon deciding whether an initial request is still
	// owed) observes a race-free answer. A fresh manager after lockdown or a
	// restart starts false, which is what makes the daemon's initial
	// relay_credential_request fire exactly once per credential-poor manager.
	credentialArmed atomic.Bool
	// childLive is the process-liveness bookkeeping: true from the moment a
	// child is started until its Wait returns (cleared by the watcher
	// goroutine, even after the supervision loop has exited). Atomic because
	// the watcher goroutine writes it outside the run goroutine's ownership;
	// it lets a late reap clear the "running" state instead of leaving it
	// permanently set (audit I4).
	childLive atomic.Bool

	// startMu serializes the start-permission decision with the child start it
	// guards. SetStartPermitted takes the same mutex, so a permission flip
	// either waits for an in-flight start to finish (and then applies to every
	// later start) or is already visible to a start that has not yet acquired
	// the mutex. It is held ONLY across the permission check, the child start
	// call and the bookkeeping that publishes the started child — never across
	// a graceful stop/kill, a credential wait, a status emission, or any other
	// blocking wait.
	startMu sync.Mutex
	// startPermitted is the start-permission fence (§7.4/§13.4): false from
	// the instant the daemon's Lockdown transition begins, until a legitimate
	// re-arm (unlock, or a manager rebuilt on an unlocked daemon) sets it back.
	// It is read under startMu for the authoritative decision, together with
	// the isStopped() arm that covers shutdown; the atomic also lets the run
	// goroutine take cheap early exits without the mutex. A fresh manager
	// starts permitted so an unlocked daemon's first relay_config can start the
	// tunnel.
	startPermitted atomic.Bool
	// applyGate is a test-only seam (withApplyGate): when non-nil the run
	// goroutine calls it before handling a queued relay_config, so a test can
	// pause the manager deterministically between the daemon's IsLocked()
	// pre-check and the manager's processing of the config.
	applyGate func()
	// startFenceGate is a test-only seam (withStartFenceGate): when non-nil the
	// start funnel calls it at the very top of startChildFenced — after every
	// caller's early permission guard, before the authoritative check — so a
	// test can pause a start that already passed the early guards, withdraw the
	// permission, and prove the startChildFenced check (not the early guard) is
	// what denies it. Production never sets it.
	startFenceGate func()
	// spawnGate is a test-only seam (withSpawnGate): when non-nil the start
	// funnel calls it after the authoritative permission check and immediately
	// before the child spawn, WHILE HOLDING startMu, so a test can pause an
	// in-flight start and prove that Stop and SetStartPermitted block until it
	// completes. Production never sets it.
	spawnGate func()
	// timerFiredHook is a test-only seam (withTimerFiredHook): when non-nil the
	// run loop calls it with the kind of the timer it just fired, before acting
	// on it, so a test can prove a timer actually fired instead of asserting a
	// negative over a window shorter than the timer's delay. It is called only
	// on the run goroutine and must not block. Production never sets it.
	timerFiredHook func(kind timerKind)

	// State below is owned by the run goroutine.
	armedConfig    Config
	hasArmedConfig bool
	armedConsumed  bool // the armed credential was already used for one Login
	// armedApplyPending is true while a failed armed-config write still owes a
	// retry (timerApplyArmedConfig). It stops a stale retry from re-applying —
	// and needlessly restarting a running child for — a configuration a newer
	// ApplyConfig has since made effective. Owned by the run goroutine.
	armedApplyPending bool
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
	// reconnectFailed is true when the CURRENT child has reported the pinned
	// reconnect-failure line and the manager is waiting for a fresh credential
	// to replace it. It makes output-based recovery single-flight per child
	// (repeated failure lines are coalesced), keeps the credential-wait timer
	// retrying while the rejected child is still ALIVE, and is cleared when the
	// replacement credential is armed, when the child exits, or when a newer
	// generation supersedes it. Owned by the run goroutine.
	reconnectFailed bool
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

// WithChildSignalTimeout is the exported form of withChildSignalTimeout: the
// bound on each child signal call (graceful stop, kill) before Stop surfaces
// ErrChildSignalTimeout (Round D fix-round, audit A).
func WithChildSignalTimeout(timeout time.Duration) ManagerOption {
	return withChildSignalTimeout(timeout)
}

// WithStatusDrainTimeout is the exported form of withStatusDrainTimeout: the
// bound on delivering the status reports queued at shutdown before the wedged
// callback is abandoned (Round D fix-round, audit A).
func WithStatusDrainTimeout(timeout time.Duration) ManagerOption {
	return withStatusDrainTimeout(timeout)
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

func withChildSignalTimeout(timeout time.Duration) ManagerOption {
	return func(manager *Manager) { manager.childSignalTimeout = timeout }
}

func withStatusDrainTimeout(timeout time.Duration) ManagerOption {
	return func(manager *Manager) { manager.statusDrainTimeout = timeout }
}

// withStatusQueueSize shrinks the bounded emission queue (test seam for the
// overflow/drop path).
func withStatusQueueSize(size int) ManagerOption {
	return func(manager *Manager) { manager.statusQueue = make(chan StatusReport, size) }
}

func withCredentialWaitTimeout(timeout time.Duration) ManagerOption {
	return func(manager *Manager) { manager.credentialWaitTimeout = timeout }
}

func withRunningStabilityWindow(window time.Duration) ManagerOption {
	return func(manager *Manager) { manager.runningStabilityWindow = window }
}

// WithApplyGate is the exported form of withApplyGate: a test-only seam. When
// set, the supervision loop calls gate before handling a queued relay_config,
// so a test can pause the manager deterministically between the daemon's
// IsLocked() pre-check and the manager's processing of the config. Production
// never sets it.
func WithApplyGate(gate func()) ManagerOption {
	return withApplyGate(gate)
}

func withApplyGate(gate func()) ManagerOption {
	return func(manager *Manager) { manager.applyGate = gate }
}

// WithStartFenceGate is the exported form of withStartFenceGate: a test-only
// seam that pauses the start funnel after every early permission guard and
// before the authoritative startChildFenced check. Production never sets it.
func WithStartFenceGate(gate func()) ManagerOption {
	return withStartFenceGate(gate)
}

func withStartFenceGate(gate func()) ManagerOption {
	return func(manager *Manager) { manager.startFenceGate = gate }
}

// WithSpawnGate is the exported form of withSpawnGate: a test-only seam that
// pauses the start funnel after the authoritative permission check and
// immediately before the child spawn, while startMu is held. Production never
// sets it.
func WithSpawnGate(gate func()) ManagerOption {
	return withSpawnGate(gate)
}

func withSpawnGate(gate func()) ManagerOption {
	return func(manager *Manager) { manager.spawnGate = gate }
}

// withTimerFiredHook installs a run-goroutine hook invoked with the kind of a
// just-fired supervision timer, before the switch acts on it (test seam).
func withTimerFiredHook(hook func(kind timerKind)) ManagerOption {
	return func(manager *Manager) { manager.timerFiredHook = hook }
}

// NewManager validates the fixed settings and starts the supervision loop.
// The status callback is invoked from a dedicated emission worker (FIFO, one
// report at a time) so a slow or blocking callback can neither reorder
// transitions nor wedge supervision or shutdown; reasons never contain
// credential material.
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
		childSignalTimeout:     defaultChildSignalTimeout,
		statusDrainTimeout:     defaultStatusDrainTimeout,
		credentialWaitTimeout:  defaultCredentialWaitTimeout,
		runningStabilityWindow: defaultRunningStabilityWindow,
		events:                 make(chan managerEvent, 32),
		stopChannel:            make(chan struct{}),
		doneChannel:            make(chan struct{}),
		statusQueue:            make(chan StatusReport, defaultStatusQueueSize),
		emitStop:               make(chan struct{}),
		emitDone:               make(chan struct{}),
		emissionOpen:           true,
	}
	for _, option := range options {
		option(manager)
	}
	if manager.rng == nil {
		manager.rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	manager.publishedGeneration.Store(-1)
	// Fail-closed start-permission default: a freshly constructed manager is
	// DENIED. Only the owning daemon arms it, and it does so under lockdownMu
	// in the same critical section that publishes it (see
	// daemon.publishTunnelManagerLocked), so a construction that races a
	// §13.4 Lockdown can never be published permitted while the daemon is
	// locked. The shutdown arm of the same fence is the stopped check taken
	// alongside this flag under startMu.
	manager.startPermitted.Store(false)
	go manager.emitLoop()
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
		// A credential is now held (armed for this manager). Recorded here,
		// on the caller's goroutine, so a sequential caller cannot observe a
		// stale false and re-request a credential control already delivered.
		manager.credentialArmed.Store(true)
		return nil
	case <-manager.doneChannel:
		return errManagerStopped
	}
}

// HasArmedCredential reports whether this manager has accepted a relay_config
// credential. The daemon uses it to decide whether an initial
// relay_credential_request is still owed: false means no credential is held
// (fresh boot, or a request control never answered), true means the tunnel is
// armed and must not be re-requested on every reconnect.
func (manager *Manager) HasArmedCredential() bool {
	return manager.credentialArmed.Load()
}

// EnsureCredential asks the manager to ensure a usable relay credential is
// held, issuing one §11.1 relay_credential_request with the given reason when
// none is, and arming the manager's bounded retry cycle behind it. It is the
// daemon's bootstrap and unlock trigger: routing those through the manager —
// instead of a one-shot direct WebSocket send — is what guarantees the request
// is retried when control does not answer it (or when reconnect churn has
// consumed control's burst), so the relay cannot be left down by a single lost
// request. It is idempotent: with a credential already held it does nothing,
// and it never requests while the start fence is withdrawn or the manager is
// stopped (the run goroutine re-checks the fence, so a race cannot leak a
// request).
func (manager *Manager) EnsureCredential(reason CredentialRequestReason) error {
	if !validCredentialRequestReason(reason) {
		return fmt.Errorf("invalid relay credential request reason %q", string(reason))
	}
	if manager.isStopped() {
		return errManagerStopped
	}
	select {
	case manager.events <- managerEvent{kind: eventEnsureCredential, credentialReason: reason}:
		return nil
	case <-manager.doneChannel:
		return errManagerStopped
	}
}

// StartPermitted reports this manager's current child-start permission
// (observability for the daemon's publication/deny wiring and for tests). The
// value is read without startMu: it is a pure diagnostic, and the
// authoritative decision is still made under startMu in startChildFenced.
func (manager *Manager) StartPermitted() bool {
	return manager.startPermitted.Load()
}

// Stopped reports whether Stop has published the permanent stopped state (its
// stop channel is closed). Like StartPermitted it is pure observability: it
// takes no lock and is not the authoritative decision — startChildFenced still
// reads the stopped boolean under startMu — so it must never be used to gate a
// start. The daemon's restart path uses it to assert that a superseded manager
// was stopped rather than dropped.
func (manager *Manager) Stopped() bool {
	return manager.isStopped()
}

// SetStartPermitted arms or denies this manager's child-start permission. The
// daemon calls it with false SYNCHRONOUSLY at the start of the §13.4 lockdown
// transition (before publishing the locked flag and before any best-effort
// lever), and with true on a legitimate re-arm (unlock, or a manager rebuilt
// on an unlocked daemon).
//
// It takes startMu so the flip is serialized with the check-and-start it
// governs: when it returns, any start already in flight has completed and
// every start that BEGINS afterwards sees the new value. That is what makes
// the §7.4/§13.4 invariant literally true — a locked (or stopped) agent never
// starts or replaces a tunnel child, including from the armed-config retry
// timer — without the daemon's pre-check having to observe the transition
// (it cannot; a pre-check is not atomic with the start).
func (manager *Manager) SetStartPermitted(permitted bool) {
	manager.startMu.Lock()
	manager.startPermitted.Store(permitted)
	manager.startMu.Unlock()
}

// Stop shuts the tunnel down: the child is stopped gracefully and killed if
// it ignores the graceful stop. Every wait is BOUNDED by a Manager-owned
// timer: the graceful/kill signal calls, the post-kill wait, the delivery of
// the queued status reports, and — here — the wait for the supervision loop
// itself. If the child has still not exited, Stop returns ErrChildKillTimeout
// (wrapped); a signal call that never returns returns ErrChildSignalTimeout. A
// status callback that blocks is never waited on: emission is asynchronous,
// and Stop stops waiting for the loop at stopBound regardless (Round D
// fix-round, audit A). The daemon treats the failure as best-effort and runs
// its remaining teardown levers regardless. Stop is idempotent: as long as the
// loop exits within the bound every call returns the same recorded outcome.
func (manager *Manager) Stop() error {
	// Boundedness note: the startMu acquisition below sits deliberately
	// OUTSIDE the stopBound budget, which starts only after the state is
	// published. Go's sync.Mutex cannot be acquired with a timeout, and this
	// acquisition IS part of the fence: startChildFenced holds startMu across
	// its check-and-spawn, so Stop must block here until an in-flight start
	// finishes, exactly as SetStartPermitted's flip does for the daemon's
	// synchronous Lockdown deny. If the process starter is wedged inside exec,
	// this wait lasts as long as that start, not merely stopBound. The daemon
	// bounds the caller instead: shutdown waits shutdownLeverTimeout for its
	// tunnel-stop lever and Lockdown waits lockdownLeverTimeout for its lever
	// fan-out (daemon.go), so the residual is a leaked Stop goroutine that
	// completes when the wedged start does — not a daemon hang. Publishing
	// `stopped` from a detached goroutine or adding a cooperative stop to the
	// process starter would restore boundedness only by letting a start begin
	// after Stop returned, which is precisely the check-and-spawn atomicity the
	// verified start fence (and TestLockdownDeniesInFlightStartBeforeItReturns)
	// depends on; the acquisition therefore stays here.
	manager.stopOnce.Do(func() {
		// Publish the stopped state under startMu — the same mutex
		// startChildFenced holds across its check-and-spawn. A start already in
		// flight completes before this returns (so Stop blocks until it does),
		// and every start that begins afterwards observes manager.stopped.
		// Closing stopChannel under the same lock keeps the cheap isStopped()
		// early exits coherent with the boolean. The lock is released before any
		// of the bounded waits below, and is never held across the child wait or
		// kill: only the state transition is serialized.
		manager.startMu.Lock()
		manager.stopped = true
		close(manager.stopChannel)
		manager.startMu.Unlock()
	})
	if manager.waitForSupervision(manager.stopBound()) {
		return manager.stopErr
	}
	// The supervision loop did not exit within its own worst-case bound. Do not
	// park the caller on a goroutine that may be wedged (e.g. inside a status
	// callback): report the same failure class a bounded child stop reports.
	return fmt.Errorf("%w: tunnel manager shutdown did not complete within %s",
		ErrChildKillTimeout, manager.stopBound())
}

// stopBound is the Manager-owned upper bound on a whole shutdown: both signal
// calls, the grace window, the post-kill wait, the bounded delivery of the
// queued status reports, and a scheduling margin. It is derived from the same
// knobs the run loop uses, so the timer can never win against a loop that is
// merely finishing on time.
func (manager *Manager) stopBound() time.Duration {
	return 2*manager.childSignalTimeout + manager.killGrace + manager.killWait +
		manager.statusDrainTimeout + defaultStopMargin
}

// waitForSupervision reports whether the supervision loop exited within the
// bound, without ever waiting longer than it.
func (manager *Manager) waitForSupervision(bound time.Duration) bool {
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-manager.doneChannel:
		return true
	case <-timer.C:
		return false
	}
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
			// visible to every Stop caller. Everything below is bounded: the child
			// stop, the drain of the queued status reports (a wedged callback is
			// abandoned, never waited on), and the emission worker's exit.
			manager.stopErr = manager.shutdownChild()
			manager.finalizeStatus()
			return
		case event := <-manager.events:
			switch event.kind {
			case eventApplyConfig:
				manager.handleApplyConfig(event.config)
			case eventChildExited:
				manager.handleChildExited(event)
			case eventChildReconnectFailed:
				manager.handleChildReconnectFailed(event)
			case eventEnsureCredential:
				manager.handleEnsureCredential(event.credentialReason)
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
	// Test seam: pause before processing so a daemon-level test can land a
	// lockdown transition between ApplyConfig's enqueue and this handling.
	if manager.applyGate != nil {
		manager.applyGate()
	}
	// §7.4/§13.4 start-permission fence (early arm guard): a config that was
	// queued before a lockdown transition (or before Stop) must neither arm
	// nor start the tunnel. The authoritative fence is still evaluated
	// atomically with the start in startChildFenced; this early check keeps a
	// fenced manager from even writing the credential-bearing config to disk
	// after the transition. It reads the atomic without startMu: a flip racing
	// this check can only let the config be armed, and the start is still
	// denied under startMu.
	if !manager.startPermitted.Load() || manager.isStopped() {
		manager.emit(manager.currentGeneration, StatusError,
			"relay_config ignored: tunnel start permission withdrawn")
		return
	}
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
			if !manager.acceptRefreshedCredential(config) {
				return
			}
			// Same-generation credential refresh (fresh one-use jti for the
			// current assignment, §15.2): arm and persist it for the next
			// start, but never restart a healthy child — replacement of a
			// running child happens only on a strictly higher generation, or
			// as reconnect recovery below.
			manager.armedConfig = config
			manager.armedConsumed = false
			if manager.reconnectFailed {
				// Reconnect recovery answer: the pinned frpc failure line armed
				// recovery for a STILL-RUNNING child, and this fresh credential
				// is control's answer. Rewrite the config, stop the looping
				// child and replace it through the single start funnel (the
				// write→stop→startChildNow sequence of
				// applyArmedConfigOrScheduleRetry, whose retry also funnels
				// through startChildFenced).
				manager.reconnectFailed = false
				manager.waitingForCredential = false
				manager.applyArmedConfigOrScheduleRetry()
				return
			}
			_ = manager.writeArmedConfig()
			if manager.waitingForCredential && manager.child == nil {
				manager.waitingForCredential = false
				manager.scheduleTimer(timerStartChild, manager.backoffDelayForAttempt())
			}
			return
		}
	}

	// First configuration or a strictly higher generation: arm it and make
	// it effective, replacing a running child (§7.4 step 5). A higher
	// generation supersedes any in-flight reconnect recovery for the old
	// child.
	manager.armedConfig = config
	manager.hasArmedConfig = true
	manager.armedConsumed = false
	manager.reconnectFailed = false
	manager.currentGeneration = config.Generation
	manager.publishedGeneration.Store(int64(config.Generation))
	manager.waitingForCredential = false
	manager.applyArmedConfigOrScheduleRetry()
}

// applyArmedConfigOrScheduleRetry persists the armed configuration and makes
// it effective: it replaces a running child (§7.4 step 5) or starts one. A
// transient write failure MUST NOT leave the manager armed in memory but never
// started. HasArmedCredential already reports true the instant ApplyConfig
// queues the credential (that synchronous publish is what stops a sequential
// caller from re-requesting a credential control just delivered), so on this
// path nothing else would ever retry: the daemon's reconnect-based re-request
// stays suppressed and the relay is silently down until an unrelated event.
// The failure therefore schedules the same bounded backoff startChildNow uses;
// the retry re-renders the armed config, so a persistent failure backs off
// instead of hot-looping.
func (manager *Manager) applyArmedConfigOrScheduleRetry() {
	// A retry that fires after the start-permission fence was withdrawn (or
	// after Stop) must not re-apply the armed config, must not stop a healthy
	// child, and must not start one. The authoritative fence is taken
	// atomically with the start; this early check avoids mutating the on-disk
	// config or the child at all.
	if !manager.startPermitted.Load() || manager.isStopped() {
		return
	}
	if err := manager.writeArmedConfig(); err != nil {
		manager.armedApplyPending = true
		manager.restartAttempt++
		manager.scheduleTimer(timerApplyArmedConfig, manager.backoffDelayForAttempt())
		return
	}
	manager.armedApplyPending = false
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
	// An exit ends any reconnect-recovery cycle for this child: the child is
	// gone, so the replacement half of recovery no longer applies.
	recovering := manager.reconnectFailed
	manager.reconnectFailed = false
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
	if recovering {
		// The reconnect-failure path already requested this credential, and its
		// bounded retry cycle is still armed. Do not issue a duplicate request:
		// the queued or in-flight answer restarts the tunnel.
		return
	}
	manager.requestCredentialAndScheduleRestart()
}

// handleChildReconnectFailed handles the sanitised internal signal that the
// CURRENT child's reconnect Login was rejected while the child keeps running
// (frp v0.71 never exits it). It arms one recovery cycle for that child and
// asks control for a fresh credential; the credential-wait timer then retries
// while the rejected child is still alive, and a fresh same-generation
// credential replaces the child through the single start funnel. The event is
// ignored when it names a child that is no longer current, and repeated
// failures for the same child coalesce into one request.
func (manager *Manager) handleChildReconnectFailed(event managerEvent) {
	if event.child != manager.child {
		return // stale or already-replaced child: it cannot drive recovery
	}
	if manager.reconnectFailed {
		return // this child's recovery cycle is already armed
	}
	manager.reconnectFailed = true
	// The rejected Login burned the child's one-use jti exactly like the
	// exit path: no further Login with the armed credential can succeed, so
	// the credential is spent and the next start must wait for a fresh one.
	manager.armedConsumed = true
	manager.waitingForCredential = true
	manager.pendingCredentialReason = ReasonReplayRejected
	manager.emit(manager.currentGeneration, StatusError,
		"frpc reconnect login rejected; requesting a fresh relay credential")
	manager.requestCredentialAndScheduleRestart()
}

// handleEnsureCredential handles the daemon's bootstrap/unlock trigger. The
// manager owns the request so it can retry it with the same bounded machinery
// as every other credential need: a one-shot direct send could be silently
// dropped when control's relay_credential_request burst is exhausted by
// reconnect churn, leaving the relay down with nothing to retry it. It is a
// no-op when a usable credential is already held, and it never asks while the
// start fence is withdrawn or the manager is stopped.
func (manager *Manager) handleEnsureCredential(reason CredentialRequestReason) {
	if !manager.startPermitted.Load() || manager.isStopped() {
		manager.emit(manager.currentGeneration, StatusError,
			"relay credential request suppressed: tunnel start permission withdrawn")
		return
	}
	if manager.hasArmedConfig && !manager.reconnectFailed {
		// A credential is already armed for this manager: it is either about to
		// start the child or already in use by a running one, and a second
		// credential could not be applied without a restart this event must not
		// force. Nothing to ensure.
		return
	}
	if !validCredentialRequestReason(reason) {
		reason = ReasonRestart
	}
	manager.waitingForCredential = true
	manager.pendingCredentialReason = reason
	manager.requestCredentialAndScheduleRestart()
}

// acceptRefreshedCredential reports whether a same-generation credential may
// supersede the armed one. It refuses a credential that is already expired and
// one whose expires_at is older than the armed credential's, so a delayed or
// reordered relay_config can never replace a newer credential — during
// recovery that would restart the rejected child with an older, still
// unusable credential. The credential and its expiry are never logged.
func (manager *Manager) acceptRefreshedCredential(config Config) bool {
	if !config.ExpiresAt.After(time.Now()) {
		manager.emit(manager.currentGeneration, StatusError,
			"rejected refreshed relay_config: credential is already expired")
		return false
	}
	if config.ExpiresAt.Before(manager.armedConfig.ExpiresAt) {
		manager.emit(manager.currentGeneration, StatusError,
			"rejected refreshed relay_config: expires_at is older than the armed credential")
		return false
	}
	return true
}

func (manager *Manager) handleTimerFired() {
	kind := manager.pendingTimer
	manager.pendingTimer = timerNone
	if manager.timerFiredHook != nil {
		manager.timerFiredHook(kind)
	}
	switch kind {
	case timerStartChild:
		manager.startChildNow()
	case timerApplyArmedConfig:
		if manager.armedApplyPending {
			manager.applyArmedConfigOrScheduleRetry()
		}
	case timerRetryCredentialRequest, timerCredentialWaitExpiry:
		// Retry whether or not a child is running: output-based recovery
		// leaves the REJECTED child alive while it waits for the replacement
		// credential (frp v0.71 never exits it), so the pre-fix `child == nil`
		// gate would stall here forever.
		if manager.waitingForCredential {
			manager.requestCredentialAndScheduleRestart()
		}
	}
}

// startChildNow starts the armed configuration if it has not been consumed;
// a consumed credential must first be refreshed (§15.2). It is the single
// funnel through which EVERY child start passes — the event-loop apply path
// (handleApplyConfig), the timerStartChild backoff retry, the
// timerApplyArmedConfig armed-config retry, and the child != nil
// higher-generation replacement — so fencing its actual start fences all of
// them. The start itself is performed by startChildFenced, which consults the
// fence atomically with the start.
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
	manager.startChildFenced()
}

// startChildFenced performs the child start behind the start-permission fence.
// The permission and stopped reads and the start call happen under startMu,
// which SetStartPermitted and Stop's state transition also take, so a flip
// that lands while a start is in flight waits for that start to finish, and
// every start that begins after the flip observes the new value. The mutex is
// never held across a graceful stop/kill or any other blocking wait. A refused
// start schedules nothing: the manager is either stopped or fenced, and a
// retry would only repeat the refusal.
func (manager *Manager) startChildFenced() {
	arguments := []string{"-c", manager.settings.ConfigPath}
	// Test seam: after every caller's early guard, before the authoritative
	// check. A pause here lets a test withdraw the permission and prove this
	// check is what denies the start.
	if manager.startFenceGate != nil {
		manager.startFenceGate()
	}
	manager.startMu.Lock()
	if !manager.startPermitted.Load() || manager.stopped {
		manager.startMu.Unlock()
		manager.emit(manager.currentGeneration, StatusError,
			"frpc start denied: tunnel start permission withdrawn")
		return
	}
	// Test seam: after the check, holding startMu, immediately before the
	// spawn. A pause here proves Stop/SetStartPermitted block on the in-flight
	// start instead of letting a fence land between the check and the spawn.
	if manager.spawnGate != nil {
		manager.spawnGate()
	}
	child, err := manager.startChild(context.Background(), manager.settings.FRPCBinaryPath, arguments)
	if err != nil {
		manager.startMu.Unlock()
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
	manager.startMu.Unlock()
	manager.emit(manager.currentGeneration, StatusStarting, "frpc started")
	go manager.watchChild(child, exited)
	go manager.watchChildOutput(child, exited)
	if manager.runningStabilityWindow > 0 {
		go manager.notifyRunningAfterStability(child)
	}
}

// requestCredentialAndScheduleRestart asks control for a fresh admission
// credential over the authenticated WebSocket (§15.2) and arms the retry
// machinery: on success a bounded wait re-requests if control never answers;
// on failure the request itself is retried with backoff. Either way the
// manager never restarts the child with the stale credential. The §11.1
// reason carried is the one recorded when the wait began (replay_rejected for
// a burned jti, restart for a daemon bootstrap/unlock trigger).
func (manager *Manager) requestCredentialAndScheduleRestart() {
	// Request-side arm of the §13.4/§7.4 fence: a locked or stopped agent must
	// not put a relay_credential_request on the wire for a credential the start
	// funnel would refuse to use. The start itself is still denied
	// authoritatively inside startChildFenced, so a request that races the
	// fence is harmless; this check keeps the fenced manager from asking at all.
	if !manager.startPermitted.Load() || manager.isStopped() {
		manager.emit(manager.currentGeneration, StatusError,
			"relay credential request suppressed: tunnel start permission withdrawn")
		return
	}
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

// watchChildOutput relays a recovery signal from one child's captured output
// into the event loop, tagged with that child's identity so a stale or
// already-replaced child can never drive recovery for a newer one. Raw output
// text never leaves this function: only the sanitised event is emitted. The
// detector is created fresh per child, so its consecutive-failure counter is
// scoped to the CURRENT child and is reset by child replacement. Every trigger
// line is forwarded (frpc retries on a bounded backoff, so the volume is low)
// and the event loop's reconnectFailed guard coalesces them into one recovery
// cycle per child; forwarding them all lets a recovery that could not replace
// the child (e.g. a stop that timed out) be re-armed by the child's next failed
// attempt instead of latching. The watcher ends when the child exits, when its
// output stream closes, or when the manager stops.
func (manager *Manager) watchChildOutput(child childProcess, exited chan struct{}) {
	output, ok := child.(childOutputStream)
	if !ok {
		return // no captured output: exit-based recovery still applies
	}
	detector := &reconnectFailureDetector{}
	lines := output.OutputLines()
	for {
		select {
		case line, open := <-lines:
			if !open {
				return
			}
			if !detector.observe(line) {
				continue
			}
			select {
			case manager.events <- managerEvent{kind: eventChildReconnectFailed, child: child}:
			case <-manager.doneChannel:
				return
			}
		case <-exited:
			// The child is gone; any pending line no longer describes a live
			// recovery target and the exit path owns recovery from here.
			return
		case <-manager.doneChannel:
			return
		}
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
// kill after the grace period, then a BOUNDED wait for the exit. Every step is
// bounded by a Manager-owned timer, including the two signal calls themselves
// (a child/OS call that never returns must not strand shutdown). Signal calls
// are single-flighted (signalChild): a graceful call that did not return is
// surfaced, and the kill escalation is then refused while the wedged call
// occupies the manager's one signal slot — so a pathological child/OS call can
// never accumulate goroutines across the repeated stop/replacement calls the
// run loop makes. A graceful call that RETURNED but did not stop the child
// keeps the normal kill escalation after the grace window. If the child still
// has not exited, it is left tracked (its watcher owns the eventual exit) and
// an explicit error is returned so callers can surface it and keep shutdown
// moving. It is called from the run goroutine only, and holds no lock across
// any wait.
func (manager *Manager) stopChildGracefullyOrKill() error {
	child := manager.child
	if child == nil {
		return nil
	}
	exited := manager.childExited

	gracefulTimedOut := false
	if err := manager.signalChild("graceful stop", child.GracefulStop); err != nil {
		if errors.Is(err, ErrChildSignalTimeout) {
			gracefulTimedOut = true
			manager.emit(manager.currentGeneration, StatusError, fmt.Sprintf("frpc %v", err))
		}
		// A returned signal error (e.g. the process already exited) is not a
		// shutdown failure here: the exit channel decides.
	}
	if !gracefulTimedOut {
		select {
		case <-exited:
			manager.child = nil
			manager.childExited = nil
			return nil
		case <-time.After(manager.killGrace):
		}
	}

	if err := manager.signalChild("kill", child.Kill); err != nil && errors.Is(err, ErrChildSignalTimeout) {
		return err
	}
	select {
	case <-exited:
		manager.child = nil
		manager.childExited = nil
		return nil
	case <-time.After(manager.killWait):
		return fmt.Errorf("%w: did not exit within %s after kill", ErrChildKillTimeout, manager.killWait)
	}
}

// signalChild invokes one child signal call under a Manager-owned bound, with
// single-flight semantics: the manager allows at most ONE outstanding signal
// call. The call runs on its own goroutine (Go cannot cancel a blocking
// syscall, so abandoning it is inherent); if the bound expires the caller gets
// an explicit ErrChildSignalTimeout while that goroutine is abandoned until
// the call returns. While the slot is occupied every further signal request is
// refused with the recorded bound error WITHOUT spawning, so a wedged
// GracefulStop/Kill can never accumulate goroutines across the repeated
// stop/replacement calls the supervision loop makes (Round D fix-round 2,
// audit A). When the abandoned call eventually returns, its slot is released
// and a later, legitimate call (e.g. a subsequent kill escalation) can proceed
// — still never more than one outstanding at any instant.
func (manager *Manager) signalChild(name string, signal func() error) error {
	manager.signalMu.Lock()
	if outstanding := manager.outstandingSignal; outstanding != nil {
		err := outstanding.timeoutErr
		manager.signalMu.Unlock()
		if err == nil {
			// Defensive: an outstanding call without a recorded bound (cannot
			// happen from the run goroutine, the only caller) still yields the
			// correct failure class.
			err = fmt.Errorf("%w: %s did not return within %s",
				ErrChildSignalTimeout, outstanding.name, manager.childSignalTimeout)
		}
		return fmt.Errorf("%w: %s skipped while %s is still outstanding", err, name, outstanding.name)
	}
	call := &childSignalCall{name: name, returned: make(chan error, 1)}
	manager.outstandingSignal = call
	manager.signalMu.Unlock()

	manager.outstandingSignalCalls.Add(1)
	go func() {
		defer manager.finishSignalCall(call)
		call.returned <- signal()
	}()

	timer := time.NewTimer(manager.childSignalTimeout)
	defer timer.Stop()
	select {
	case err := <-call.returned:
		// Release eagerly so the same run goroutine's next signal call never
		// observes a stale slot for a call that already returned.
		manager.finishSignalCall(call)
		return err
	case <-timer.C:
		manager.signalMu.Lock()
		if call.timeoutErr == nil {
			call.timeoutErr = fmt.Errorf("%w: %s did not return within %s",
				ErrChildSignalTimeout, name, manager.childSignalTimeout)
		}
		err := call.timeoutErr
		manager.signalMu.Unlock()
		return err
	}
}

// finishSignalCall releases the single-flight slot exactly once: it decrements
// the live-signal counter BEFORE clearing the slot, so a spawn that observes
// the free slot can never overlap it with the finishing call (the counter
// therefore never exceeds one).
func (manager *Manager) finishSignalCall(call *childSignalCall) {
	call.finishOnce.Do(func() {
		manager.outstandingSignalCalls.Add(-1)
		manager.signalMu.Lock()
		if manager.outstandingSignal == call {
			manager.outstandingSignal = nil
		}
		manager.signalMu.Unlock()
	})
}

// emitLoop is the single status emission worker. It delivers reports strictly
// in FIFO order, so control sees the lifecycle transitions in the order the
// manager produced them, and it never runs on the run goroutine: a callback
// that blocks therefore cannot wedge supervision or shutdown. On shutdown it
// drains what is already queued and exits.
func (manager *Manager) emitLoop() {
	defer close(manager.emitDone)
	for {
		select {
		case report := <-manager.statusQueue:
			manager.onStatus(report)
		case <-manager.emitStop:
			for {
				select {
				case report := <-manager.statusQueue:
					manager.onStatus(report)
				default:
					return
				}
			}
		}
	}
}

// finalizeStatus stops accepting new reports and waits, bounded, for the
// emission worker to deliver what is already queued. A callback still blocked
// at the bound is abandoned: the worker exits as soon as the callback returns
// (so nothing leaks permanently), and the reports it did not deliver are
// counted and logged rather than silently lost or waited on forever.
func (manager *Manager) finalizeStatus() {
	manager.emitMu.Lock()
	manager.emissionOpen = false
	manager.emitMu.Unlock()
	manager.emitStopOnce.Do(func() { close(manager.emitStop) })

	timer := time.NewTimer(manager.statusDrainTimeout)
	defer timer.Stop()
	select {
	case <-manager.emitDone:
		return
	case <-timer.C:
	}
	pending := len(manager.statusQueue)
	if pending > 0 {
		manager.droppedStatus.Add(uint64(pending))
	}
	log.Printf("tunnel status emission: callback still blocked after %s; %d queued report(s) undelivered at the bound (dropped so far %d)",
		manager.statusDrainTimeout, pending, manager.droppedStatus.Load())
}

// shutdownChild is the Stop path: stop the child and report final telemetry.
// The child-stop error (an unkillable process or a wedged signal call) is
// returned rather than swallowed, and the final stopped telemetry is always
// emitted (asynchronously: a wedged callback cannot delay the return).
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
	report := StatusReport{Generation: generation, Status: status, Reason: reason}
	manager.emitMu.Lock()
	if !manager.emissionOpen {
		manager.emitMu.Unlock()
		manager.dropStatus(report, "manager stopping")
		return
	}
	select {
	case manager.statusQueue <- report:
		manager.emitMu.Unlock()
		return
	default:
	}
	manager.emitMu.Unlock()
	manager.dropStatus(report, "status queue full")
}

// dropStatus records a report that will never be delivered. Drops are counted
// and logged — never silent — because a lost transition (e.g. the final
// stopped state) is operational information even though relay telemetry is
// best-effort.
func (manager *Manager) dropStatus(report StatusReport, cause string) {
	dropped := manager.droppedStatus.Add(1)
	log.Printf("tunnel status %s (generation %d) dropped (%s): %s; %d report(s) dropped in total",
		report.Status, report.Generation, cause, report.Reason, dropped)
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
