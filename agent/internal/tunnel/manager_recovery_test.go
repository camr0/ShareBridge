package tunnel

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These tests pin the release-blocking relay-recovery defect found live on
// 2026-09-05 (ledger: "THE RELAY TUNNEL CANNOT RECOVER FROM A DROP"): an
// ESTABLISHED frpc session outlives its single-use 10-minute credential, and
// when the session ends every re-login replays a burned jti and is rejected
// forever. frp v0.71 applies loginFailExit only to the FIRST login
// (client/service.go:262) and hard-codes false on the reconnect path
// (:277-287), so the rejected frpc child keeps running and the manager's only
// reactive trigger (a child exit) never fires.
//
// The fix detects the pinned v0.71 reconnect-failure output, requests a fresh
// credential, and replaces the still-running child through the existing single
// start funnel.

// Real frpc v0.71.0 client output, captured from the checksum-pinned v0.71.0
// binary against the live relay with a deliberately invalid credential. frpc
// wraps these lines in ANSI SGR colour codes even when stdout/stderr is a pipe,
// which does not affect a substring match, so ansiWrap below covers the
// coloured form. The fixtures are spelled as literals here on purpose: the tests
// must fail if the pinned production substrings drift, so they cannot reuse the
// production constants.
const (
	// realFRPCReconnectFailureLine is the EXACT captured client line on the
	// reconnect/retry path (frpc logs one per failed Login attempt).
	realFRPCReconnectFailureLine = "connect to server error: request rejected"
	// realFRPCFirstLoginFailureLine is the captured first-login form
	// (loginFailExit=true). frpc logs it immediately before the child EXITS, so
	// the exit path — not output detection — owns that recovery, and it must not
	// be mistaken for a reconnect rejection.
	realFRPCFirstLoginFailureLine = "login to the server failed: request rejected. With loginFailExit enabled, no additional retries will be attempted"
	// realFRPCRewordedRejectionLine is a HYPOTHETICAL future wording for the same
	// plugin rejection: the failed-Login prefix is unchanged but the rejection
	// text differs, so only the count-based safety net can recover. It exercises
	// graceful degradation on an FRP wording change.
	realFRPCRewordedRejectionLine = "connect to server error: login rejected by relay plugin"
	// realFRPCConnectionRefusedLine is a real frps-DOWNTIME line: the same
	// prefix with a transport error and no rejection marker. It must not, on its
	// own, request a credential.
	realFRPCConnectionRefusedLine = "connect to server error: dial tcp 203.0.113.7:7000: connect: connection refused"
	// realFRPCRetryAttemptLine is the benign line frpc logs before every
	// attempt (client/service.go:312).
	realFRPCRetryAttemptLine = "try to connect to server..."
)

// ansiWrap reproduces the ANSI SGR colour codes frpc v0.71 emits around its log
// lines even when its output is piped.
func ansiWrap(line string) string {
	return "\x1b[1;33m" + line + "\x1b[0m"
}

// newRecoveryManager builds a manager with the fast knobs the recovery tests
// need, plus any extra options (later options win). The manager is armed the
// way the unlocked daemon's publication step arms it.
func newRecoveryManager(t *testing.T, settings Settings, starter *recordingStarter, requester CredentialRequester, collector *statusCollector, options ...ManagerOption) *Manager {
	t.Helper()
	base := []ManagerOption{
		withProcessStarter(starter.startProcess),
		withBackoffBase(time.Millisecond),
		withKillGracePeriod(20 * time.Millisecond),
		withCredentialWaitTimeout(time.Hour),
		withRunningStabilityWindow(time.Hour),
	}
	manager, err := NewManager(settings, requester, collector.record, append(base, options...)...)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	manager.SetStartPermitted(true)
	t.Cleanup(func() { _ = manager.Stop() })
	return manager
}

// recoveryGenerationOne is a deterministic generation-1 assignment.
func recoveryGenerationOne() Config {
	config := validTestConfig()
	config.Generation = 1
	config.ProxyName = "sb-recovery-one"
	config.RelayPort = 10001
	config.Credential = "credential-generation-one"
	config.ExpiresAt = time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	return config
}

// startRecoveryChild applies generation one and waits for its child.
func startRecoveryChild(t *testing.T, manager *Manager, starter *recordingStarter, config Config) *fakeChildProcess {
	t.Helper()
	if err := manager.ApplyConfig(config); err != nil {
		t.Fatalf("ApplyConfig(generation 1) error = %v", err)
	}
	waitForCondition(t, "generation 1 child start", time.Second, func() bool {
		return starter.startCount() == 1
	})
	return starter.recordAt(0).child
}

// TestFRPCReconnectFailureLineMatchesPinnedV071Output pins the matcher against
// the REAL captured v0.71.0 client bytes: the exact reconnect line and its
// ANSI-coloured form MUST be recognised, while the frps server-side wording,
// the first-login exit line, a plain connection-refused (frps downtime) line
// and a reworded rejection MUST NOT match the primary pair, so recovery cannot
// be triggered by unrelated log traffic.
func TestFRPCReconnectFailureLineMatchesPinnedV071Output(t *testing.T) {
	// The one pinned spelling must be exactly the real captured client text and
	// must agree with the prefix/marker pair the matcher actually uses.
	if frpcReconnectFailureLine != realFRPCReconnectFailureLine {
		t.Fatalf("pinned reconnect-failure substring = %q, want the real v0.71 client text %q",
			frpcReconnectFailureLine, realFRPCReconnectFailureLine)
	}
	if want := frpcConnectionErrorPrefix + " " + frpcRejectionMarker; frpcReconnectFailureLine != want {
		t.Fatalf("pinned literal %q disagrees with the matcher pair %q", frpcReconnectFailureLine, want)
	}

	matching := []string{
		realFRPCReconnectFailureLine,
		"2026/09/03 15:25:11 [W] [client/service.go:323] " + realFRPCReconnectFailureLine,
		ansiWrap(realFRPCReconnectFailureLine),
		realFRPCReconnectFailureLine + ": context deadline exceeded",
	}
	for _, line := range matching {
		if !isReconnectFailureLine(line) {
			t.Errorf("isReconnectFailureLine(%q) = false, want true", line)
		}
	}
	nonMatching := []string{
		"",
		"register control error", // the frps SERVER-side wording: never on the client
		realFRPCFirstLoginFailureLine,
		realFRPCConnectionRefusedLine,
		realFRPCRewordedRejectionLine, // only the count-based safety net may catch this
		"connect to server error:",
		realFRPCRetryAttemptLine,
		"login to server success, get run id [abc]",
	}
	for _, line := range nonMatching {
		if isReconnectFailureLine(line) {
			t.Errorf("isReconnectFailureLine(%q) = true, want false", line)
		}
	}
}

// TestReconnectFailureDetectorCountBasedSafetyNet pins the bounded fallback:
// three consecutive failed-Login lines with a CHANGED rejection wording trigger
// a refresh, frpc's interleaved pre-attempt line does not break the run, and a
// progress line (a successful login) resets it.
func TestReconnectFailureDetectorCountBasedSafetyNet(t *testing.T) {
	t.Run("reworded rejection trips after the threshold", func(t *testing.T) {
		detector := &reconnectFailureDetector{}
		for attempt := 1; attempt <= frpcConsecutiveConnectionErrorThreshold; attempt++ {
			detector.observe(realFRPCRetryAttemptLine)
			trigger := detector.observe(realFRPCRewordedRejectionLine)
			wantTrigger := attempt == frpcConsecutiveConnectionErrorThreshold
			if trigger != wantTrigger {
				t.Fatalf("attempt %d trigger = %v, want %v", attempt, trigger, wantTrigger)
			}
		}
	})

	t.Run("successful login resets the run", func(t *testing.T) {
		detector := &reconnectFailureDetector{}
		for attempt := 1; attempt < frpcConsecutiveConnectionErrorThreshold; attempt++ {
			detector.observe(realFRPCRetryAttemptLine)
			if detector.observe(realFRPCRewordedRejectionLine) {
				t.Fatalf("attempt %d triggered below the threshold", attempt)
			}
		}
		// A successful Login is progress and must reset the run.
		if detector.observe("login to server success, get run id [run-1]") {
			t.Fatal("a successful login must not itself trigger recovery")
		}
		for attempt := 1; attempt < frpcConsecutiveConnectionErrorThreshold; attempt++ {
			detector.observe(realFRPCRetryAttemptLine)
			if detector.observe(realFRPCRewordedRejectionLine) {
				t.Fatalf("post-reset attempt %d triggered below the threshold", attempt)
			}
		}
	})
}

// TestReconnectRecoveryMatchesANSIColouredRejection proves the real captured
// line still drives recovery when frpc wraps it in ANSI colour codes (which it
// does even when piped).
func TestReconnectRecoveryMatchesANSIColouredRejection(t *testing.T) {
	settings := testManagerSettings(t)
	starter := &recordingStarter{stopsOnGraceful: true}
	requester := &fakeCredentialRequester{}
	collector := &statusCollector{}
	manager := newRecoveryManager(t, settings, starter, requester, collector)

	child := startRecoveryChild(t, manager, starter, recoveryGenerationOne())
	child.emitOutputLine(ansiWrap(realFRPCReconnectFailureLine))

	waitForCondition(t, "ANSI-coloured rejection credential request", time.Second, func() bool {
		return requester.requestCount() == 1
	})
	if reasons := requester.requestReasons(); len(reasons) != 1 || reasons[0] != ReasonReplayRejected {
		t.Fatalf("recovery credential request reasons = %v, want exactly [%s]", reasons, ReasonReplayRejected)
	}
}

// TestReconnectRecoveryIgnoresFRPSDowntimeConnectionRefused is the explicit
// negative: during frps downtime frpc prints the same failed-Login prefix with
// a transport error and no rejection marker. That is not a burned credential,
// so a single such line must NOT request one or stop the child; only the
// bounded consecutive-failure net can ever act on sustained downtime.
func TestReconnectRecoveryIgnoresFRPSDowntimeConnectionRefused(t *testing.T) {
	settings := testManagerSettings(t)
	starter := &recordingStarter{stopsOnGraceful: true}
	requester := &fakeCredentialRequester{}
	collector := &statusCollector{}
	manager := newRecoveryManager(t, settings, starter, requester, collector)

	generationOne := recoveryGenerationOne()
	child := startRecoveryChild(t, manager, starter, generationOne)

	// Exactly what a dropped frps produces while it restarts: the benign
	// pre-attempt line followed by one connection-refused report.
	child.emitOutputLine(realFRPCRetryAttemptLine)
	child.emitOutputLine(realFRPCConnectionRefusedLine)

	assertConditionStays(t, "an frps-downtime line never requests a credential", 300*time.Millisecond, func() bool {
		return requester.requestCount() == 0 &&
			starter.startCount() == 1 &&
			child.gracefulStopCount() == 0
	})
	assertReportsNeverContainRawChildOutput(t, collector)
}

// TestReconnectRecoveryFromCountBasedFallbackOnRewordedRejection is the
// graceful-degradation path end to end: a future FRP wording change makes the
// primary pair go dark, but the bounded count-based net still detects three
// consecutive failed Logins, requests a fresh credential and replaces the
// still-running child through the single start funnel.
func TestReconnectRecoveryFromCountBasedFallbackOnRewordedRejection(t *testing.T) {
	settings := testManagerSettings(t)
	starter := &recordingStarter{stopsOnGraceful: true}
	requester := &fakeCredentialRequester{}
	collector := &statusCollector{}
	manager := newRecoveryManager(t, settings, starter, requester, collector)

	generationOne := recoveryGenerationOne()
	child := startRecoveryChild(t, manager, starter, generationOne)

	for attempt := 1; attempt <= frpcConsecutiveConnectionErrorThreshold; attempt++ {
		child.emitOutputLine(realFRPCRetryAttemptLine)
		child.emitOutputLine(realFRPCRewordedRejectionLine)
	}

	waitForCondition(t, "count-based fallback credential request", time.Second, func() bool {
		return requester.requestCount() == 1
	})
	if got := starter.startCount(); got != 1 {
		t.Fatalf("child was replaced before a fresh credential arrived: %d starts, want 1", got)
	}

	refreshed := generationOne
	refreshed.Credential = "credential-count-fallback"
	refreshed.ExpiresAt = time.Now().Add(20 * time.Minute).UTC().Truncate(time.Second)
	if err := manager.ApplyConfig(refreshed); err != nil {
		t.Fatalf("ApplyConfig(refreshed) error = %v", err)
	}
	waitForCondition(t, "replacement child start after the count-based fallback", 2*time.Second, func() bool {
		return starter.startCount() == 2
	})
	if got := child.gracefulStopCount(); got != 1 {
		t.Errorf("rejected child graceful stops = %d, want 1", got)
	}
	assertReportsNeverContainCredential(t, collector, generationOne.Credential, refreshed.Credential)
	assertReportsNeverContainRawChildOutput(t, collector)
}

// TestReconnectFailureOnLiveChildReplacesChildWithFreshCredential is the
// happy-path recovery: a LIVE child that emits the real v0.71 line and does NOT
// exit must cause a fresh credential request and, once control answers with a
// same-generation credential, an atomic config rewrite, a graceful stop of the
// looping child and a replacement through the single start funnel.
func TestReconnectFailureOnLiveChildReplacesChildWithFreshCredential(t *testing.T) {
	settings := testManagerSettings(t)
	starter := &recordingStarter{stopsOnGraceful: true}
	requester := &fakeCredentialRequester{}
	collector := &statusCollector{}
	manager := newRecoveryManager(t, settings, starter, requester, collector)

	generationOne := recoveryGenerationOne()
	child := startRecoveryChild(t, manager, starter, generationOne)

	// The live child's reconnect Login was rejected; it keeps retrying
	// internally and never exits.
	child.emitOutputLine(realFRPCReconnectFailureLine)

	waitForCondition(t, "recovery credential request", time.Second, func() bool {
		return requester.requestCount() == 1
	})
	if reasons := requester.requestReasons(); len(reasons) != 1 || reasons[0] != ReasonReplayRejected {
		t.Fatalf("recovery credential request reasons = %v, want exactly [%s]", reasons, ReasonReplayRejected)
	}
	if got := starter.startCount(); got != 1 {
		t.Fatalf("child was replaced before a fresh credential arrived: %d starts, want 1", got)
	}

	// Control answers with a fresh same-generation credential.
	refreshed := generationOne
	refreshed.Credential = "credential-generation-one-refreshed"
	refreshed.ExpiresAt = time.Now().Add(20 * time.Minute).UTC().Truncate(time.Second)
	if err := manager.ApplyConfig(refreshed); err != nil {
		t.Fatalf("ApplyConfig(refreshed) error = %v", err)
	}

	waitForCondition(t, "replacement child start", 2*time.Second, func() bool {
		return starter.startCount() == 2
	})
	if got := child.gracefulStopCount(); got != 1 {
		t.Errorf("rejected child graceful stops = %d, want 1", got)
	}
	if got := child.killCount(); got != 0 {
		t.Errorf("rejected child kills = %d, want 0 (a responsive child stops gracefully)", got)
	}
	if content := configFileContent(t, settings.ConfigPath); !strings.Contains(content, refreshed.Credential) {
		t.Errorf("generated config does not carry the refreshed credential")
	}
	// Exactly one request per recovery cycle: the replacement child must not
	// trigger another one, and the manager must settle.
	replacement := starter.recordAt(1).child
	assertConditionStays(t, "recovery settles after one replacement", 250*time.Millisecond, func() bool {
		return starter.startCount() == 2 &&
			child.gracefulStopCount() == 1 &&
			replacement.gracefulStopCount() == 0 &&
			requester.requestCount() == 1
	})
	assertReportsNeverContainCredential(t, collector, generationOne.Credential, refreshed.Credential)
	assertReportsNeverContainRawChildOutput(t, collector)
}

// TestReconnectRecoveryKillTimeoutRequestsCredentialOnce pins the single-
// credential property across a replacement whose stop hits the kill bound: the
// rejected child cannot be reaped within killWait, so the recovery credential
// is armed but unused when the child exits LATE. That exit must finish the
// cycle with the credential already in hand — exactly one request, one
// replacement — never a second relay_credential_request (which would burn
// another credential and consume control's limiter). It also pins the output
// path: a repeat of the rejected reconnect line in this state coalesces instead
// of asking again.
func TestReconnectRecoveryKillTimeoutRequestsCredentialOnce(t *testing.T) {
	settings := testManagerSettings(t)
	// The child ignores the graceful stop and the kill: the manager's bounded
	// post-kill wait expires and the child stays alive until the test exits it.
	starter := &recordingStarter{stopsOnGraceful: false, ignoresKill: true}
	requester := &fakeCredentialRequester{}
	collector := &statusCollector{}
	manager := newRecoveryManager(t, settings, starter, requester, collector,
		withKillWaitTimeout(60*time.Millisecond),
	)

	generationOne := recoveryGenerationOne()
	child := startRecoveryChild(t, manager, starter, generationOne)
	child.emitOutputLine(realFRPCReconnectFailureLine)
	waitForCondition(t, "recovery credential request", time.Second, func() bool {
		return requester.requestCount() == 1
	})

	// Control answers: the recovery branch rewrites the config and tries to
	// replace the still-running child.
	refreshed := generationOne
	refreshed.Credential = "credential-kill-timeout"
	refreshed.ExpiresAt = time.Now().Add(20 * time.Minute).UTC().Truncate(time.Second)
	if err := manager.ApplyConfig(refreshed); err != nil {
		t.Fatalf("ApplyConfig(refreshed) error = %v", err)
	}

	// The replacement cannot reap the child: the bounded post-kill wait expires.
	waitForCondition(t, "kill-timeout replacement failure", 2*time.Second, func() bool {
		return collector.hasReasonContaining("did not exit within")
	})
	if got := starter.startCount(); got != 1 {
		t.Fatalf("replacement started despite the failed stop: %d starts, want 1 (never double-start)", got)
	}

	// A repeat of the rejected reconnect line while the credential is armed but
	// unused must coalesce, not request another credential (the retry reuses the
	// credential in hand).
	child.emitOutputLine(realFRPCReconnectFailureLine)
	assertConditionStays(t, "armed recovery credential is never re-requested", 250*time.Millisecond, func() bool {
		return requester.requestCount() == 1 && starter.startCount() == 1
	})

	// The rejected child exits AFTER the kill bound. The already-armed fresh
	// credential must start the replacement; control is NOT asked again.
	child.signalExit(errors.New("exit status 1"))
	waitForCondition(t, "replacement child from the armed recovery credential", 2*time.Second, func() bool {
		return starter.startCount() == 2
	})
	if reasons := requester.requestReasons(); len(reasons) != 1 {
		t.Fatalf("credential requests = %v, want exactly one for the failure", reasons)
	}
	if content := configFileContent(t, settings.ConfigPath); !strings.Contains(content, refreshed.Credential) {
		t.Errorf("replacement config does not carry the recovery credential")
	}
}

// TestReconnectRecoveryRetriesWhenCredentialReplyIsLost covers a reply control
// never sends: the credential-wait timer must keep re-requesting WHILE the
// rejected child is still alive (the pre-fix timer only retried once the child
// had exited, which is exactly the state frpc does not reach), and a late answer
// must still recover the tunnel.
func TestReconnectRecoveryRetriesWhenCredentialReplyIsLost(t *testing.T) {
	settings := testManagerSettings(t)
	starter := &recordingStarter{stopsOnGraceful: true}
	requester := &fakeCredentialRequester{}
	collector := &statusCollector{}
	manager := newRecoveryManager(t, settings, starter, requester, collector,
		withCredentialWaitTimeout(30*time.Millisecond))

	generationOne := recoveryGenerationOne()
	child := startRecoveryChild(t, manager, starter, generationOne)
	child.emitOutputLine(realFRPCReconnectFailureLine)

	waitForCondition(t, "bounded credential retry while the rejected child is alive", 2*time.Second, func() bool {
		return requester.requestCount() >= 3
	})
	if got := starter.startCount(); got != 1 {
		t.Fatalf("child replaced without a credential: %d starts, want 1", got)
	}

	// The late answer still recovers: no permanent latch.
	refreshed := generationOne
	refreshed.Credential = "credential-late-answer"
	refreshed.ExpiresAt = time.Now().Add(20 * time.Minute).UTC().Truncate(time.Second)
	if err := manager.ApplyConfig(refreshed); err != nil {
		t.Fatalf("ApplyConfig(late answer) error = %v", err)
	}
	waitForCondition(t, "replacement child start after the late answer", 2*time.Second, func() bool {
		return starter.startCount() == 2
	})
}

// TestReconnectRecoveryRetriesWhenTheRequestItselfFails covers the requester
// error arm (control's queue full / rate limiter): the request is retried with
// backoff and, once it succeeds and control answers, the child is replaced.
func TestReconnectRecoveryRetriesWhenTheRequestItselfFails(t *testing.T) {
	settings := testManagerSettings(t)
	starter := &recordingStarter{stopsOnGraceful: true}
	requester := &fakeCredentialRequester{requestError: errors.New("relay credential request queue full (4)")}
	collector := &statusCollector{}
	manager := newRecoveryManager(t, settings, starter, requester, collector)

	generationOne := recoveryGenerationOne()
	child := startRecoveryChild(t, manager, starter, generationOne)
	child.emitOutputLine(realFRPCReconnectFailureLine)

	waitForCondition(t, "credential request retried after the send failed", 2*time.Second, func() bool {
		return requester.requestCount() >= 3
	})

	requester.setRequestError(nil)
	refreshed := generationOne
	refreshed.Credential = "credential-after-send-recovery"
	refreshed.ExpiresAt = time.Now().Add(20 * time.Minute).UTC().Truncate(time.Second)
	if err := manager.ApplyConfig(refreshed); err != nil {
		t.Fatalf("ApplyConfig(refreshed) error = %v", err)
	}
	waitForCondition(t, "replacement child start after the send recovered", 2*time.Second, func() bool {
		return starter.startCount() == 2
	})
}

// TestReconnectRecoverySuppressedByStartFence pins §13.4 for the recovery
// path: with the start permission withdrawn mid-recovery the manager must not
// keep asking control for credentials and must not replace the child when a
// late credential arrives. The short credential-wait timeout makes the
// suppression load-bearing: without the fence check the retry timer would fire
// and request again.
func TestReconnectRecoverySuppressedByStartFence(t *testing.T) {
	settings := testManagerSettings(t)
	starter := &recordingStarter{stopsOnGraceful: true}
	requester := &fakeCredentialRequester{}
	collector := &statusCollector{}
	manager := newRecoveryManager(t, settings, starter, requester, collector,
		withCredentialWaitTimeout(25*time.Millisecond))

	generationOne := recoveryGenerationOne()
	child := startRecoveryChild(t, manager, starter, generationOne)
	child.emitOutputLine(realFRPCReconnectFailureLine)
	waitForCondition(t, "first recovery credential request", time.Second, func() bool {
		return requester.requestCount() == 1
	})

	// §13.4 withdrawal lands while the recovery wait is in flight.
	manager.SetStartPermitted(false)

	assertConditionStays(t, "no credential request under the withdrawn start fence", 200*time.Millisecond, func() bool {
		return requester.requestCount() == 1
	})

	// A late reply is harmless: the config-side fence refuses it and no child
	// is replaced.
	refreshed := generationOne
	refreshed.Credential = "credential-post-fence"
	refreshed.ExpiresAt = time.Now().Add(20 * time.Minute).UTC().Truncate(time.Second)
	if err := manager.ApplyConfig(refreshed); err != nil {
		t.Fatalf("ApplyConfig(post-fence) error = %v", err)
	}
	assertConditionStays(t, "no child replacement under the withdrawn start fence", 200*time.Millisecond, func() bool {
		return starter.startCount() == 1 && child.gracefulStopCount() == 0
	})
	if !collector.hasReasonContaining("start permission withdrawn") {
		t.Fatal("the fenced recovery did not report the withdrawal")
	}
}

// TestReconnectFailureExitRaceRequestsCredentialOnce pins the output-vs-exit
// dedupe. Both orderings must yield exactly one credential request: the
// recovery arming is single-flight per child and a stale failure event for a
// child that already exited (or was already replaced) must be ignored.
func TestReconnectFailureExitRaceRequestsCredentialOnce(t *testing.T) {
	t.Run("failure_then_exit", func(t *testing.T) {
		settings := testManagerSettings(t)
		starter := &recordingStarter{stopsOnGraceful: true}
		requester := &fakeCredentialRequester{}
		collector := &statusCollector{}
		manager := newRecoveryManager(t, settings, starter, requester, collector)

		generationOne := recoveryGenerationOne()
		child := startRecoveryChild(t, manager, starter, generationOne)
		child.emitOutputLine(realFRPCReconnectFailureLine)
		waitForCondition(t, "recovery credential request", time.Second, func() bool {
			return requester.requestCount() == 1
		})

		// The rejected child now actually exits (e.g. frpc finally gives up).
		child.signalExit(errors.New("exit status 1"))
		waitForCondition(t, "child exit reported", time.Second, func() bool {
			return collector.hasReasonContaining("frpc exited")
		})
		assertConditionStays(t, "no duplicate credential request after the exit", 200*time.Millisecond, func() bool {
			return requester.requestCount() == 1
		})

		// The one requested credential still brings the tunnel back.
		refreshed := generationOne
		refreshed.Credential = "credential-after-exit-race"
		refreshed.ExpiresAt = time.Now().Add(20 * time.Minute).UTC().Truncate(time.Second)
		if err := manager.ApplyConfig(refreshed); err != nil {
			t.Fatalf("ApplyConfig(refreshed) error = %v", err)
		}
		waitForCondition(t, "replacement child start after the exit race", 2*time.Second, func() bool {
			return starter.startCount() == 2
		})
	})

	t.Run("exit_then_stale_failure", func(t *testing.T) {
		settings := testManagerSettings(t)
		starter := &recordingStarter{stopsOnGraceful: true}
		requester := &fakeCredentialRequester{}
		collector := &statusCollector{}
		manager := newRecoveryManager(t, settings, starter, requester, collector)

		generationOne := recoveryGenerationOne()
		child := startRecoveryChild(t, manager, starter, generationOne)

		// The child exits first; the exit recovery requests one credential.
		child.signalExit(errors.New("exit status 1"))
		waitForCondition(t, "child exit reported", time.Second, func() bool {
			return collector.hasReasonContaining("frpc exited")
		})
		waitForCondition(t, "exit recovery credential request", time.Second, func() bool {
			return requester.requestCount() == 1
		})

		// A failure signal from that dead child's output stream arrives late.
		manager.events <- managerEvent{kind: eventChildReconnectFailed, child: child}
		assertConditionStays(t, "a dead child's late failure does not re-request", 200*time.Millisecond, func() bool {
			return requester.requestCount() == 1 && starter.startCount() == 1
		})
	})
}

// TestStaleChildReconnectFailureCannotDriveRecovery proves the child-identity
// tag: a failure event queued for an already-replaced child must not request a
// credential or touch the newer child, while the current child still can.
func TestStaleChildReconnectFailureCannotDriveRecovery(t *testing.T) {
	settings := testManagerSettings(t)
	starter := &recordingStarter{stopsOnGraceful: true}
	requester := &fakeCredentialRequester{}
	collector := &statusCollector{}
	manager := newRecoveryManager(t, settings, starter, requester, collector)

	generationOne := recoveryGenerationOne()
	startRecoveryChild(t, manager, starter, generationOne)
	staleChild := starter.recordAt(0).child

	generationTwo := generationOne
	generationTwo.Generation = 2
	generationTwo.ProxyName = "sb-recovery-two"
	generationTwo.RelayPort = 10002
	generationTwo.Credential = "credential-generation-two"
	if err := manager.ApplyConfig(generationTwo); err != nil {
		t.Fatalf("ApplyConfig(generation 2) error = %v", err)
	}
	waitForCondition(t, "generation 2 child start", 2*time.Second, func() bool {
		return starter.startCount() == 2
	})
	currentChild := starter.recordAt(1).child

	// Exactly what the replaced child's output watcher would have queued.
	manager.events <- managerEvent{kind: eventChildReconnectFailed, child: staleChild}
	assertConditionStays(t, "stale child output cannot drive recovery", 250*time.Millisecond, func() bool {
		return requester.requestCount() == 0 &&
			starter.startCount() == 2 &&
			currentChild.gracefulStopCount() == 0
	})

	// The current child still recovers normally.
	currentChild.emitOutputLine(realFRPCReconnectFailureLine)
	waitForCondition(t, "current child recovery credential request", time.Second, func() bool {
		return requester.requestCount() == 1
	})
}

// TestRecoveryRejectsStaleAndExpiredRefreshedCredentials pins the freshness
// gate: during recovery a same-generation credential whose expires_at is older
// than the armed one, or already in the past, must be refused (it cannot
// supersede a newer credential), a lower generation stays fenced out, and a
// genuinely fresh credential still completes the recovery — so the rejection
// is not a latch.
func TestRecoveryRejectsStaleAndExpiredRefreshedCredentials(t *testing.T) {
	settings := testManagerSettings(t)
	starter := &recordingStarter{stopsOnGraceful: true}
	requester := &fakeCredentialRequester{}
	collector := &statusCollector{}
	manager := newRecoveryManager(t, settings, starter, requester, collector)

	now := time.Now()
	generationOne := recoveryGenerationOne()
	generationOne.ExpiresAt = now.Add(10 * time.Minute).UTC().Truncate(time.Second)
	child := startRecoveryChild(t, manager, starter, generationOne)
	child.emitOutputLine(realFRPCReconnectFailureLine)
	waitForCondition(t, "recovery credential request", time.Second, func() bool {
		return requester.requestCount() == 1
	})

	stale := generationOne
	stale.Credential = "credential-stale-expiry"
	stale.ExpiresAt = now.Add(5 * time.Minute).UTC().Truncate(time.Second)
	if err := manager.ApplyConfig(stale); err != nil {
		t.Fatalf("ApplyConfig(stale) error = %v", err)
	}
	waitForCondition(t, "older same-generation credential rejection", time.Second, func() bool {
		return collector.hasReasonContaining("older than the armed credential")
	})
	assertConditionStays(t, "no replacement for an older expires_at", 150*time.Millisecond, func() bool {
		return starter.startCount() == 1 && child.gracefulStopCount() == 0
	})

	expired := generationOne
	expired.Credential = "credential-already-expired"
	expired.ExpiresAt = now.Add(-1 * time.Minute).UTC().Truncate(time.Second)
	if err := manager.ApplyConfig(expired); err != nil {
		t.Fatalf("ApplyConfig(expired) error = %v", err)
	}
	waitForCondition(t, "already-expired credential rejection", time.Second, func() bool {
		return collector.hasReasonContaining("already expired")
	})
	assertConditionStays(t, "no replacement for an expired credential", 150*time.Millisecond, func() bool {
		return starter.startCount() == 1 && child.gracefulStopCount() == 0
	})

	staleGeneration := generationOne
	staleGeneration.Generation = generationOne.Generation - 1
	staleGeneration.Credential = "credential-lower-generation"
	if err := manager.ApplyConfig(staleGeneration); err == nil {
		t.Fatal("a lower generation must be fenced out synchronously")
	}

	fresh := generationOne
	fresh.Credential = "credential-truly-fresh"
	fresh.ExpiresAt = now.Add(20 * time.Minute).UTC().Truncate(time.Second)
	if err := manager.ApplyConfig(fresh); err != nil {
		t.Fatalf("ApplyConfig(fresh) error = %v", err)
	}
	waitForCondition(t, "recovery completion with the fresh credential", 2*time.Second, func() bool {
		return starter.startCount() == 2
	})
}

// TestRecoveryArmedConfigWriteRetryFunnelsThroughStartFence pins requirement
// (g): when the recovery's config rewrite fails transiently, the armed-config
// retry path must still stop the rejected child and start the replacement
// through startChildFenced (the start-fence seam is hit), never through a
// second, unfenced start path.
func TestRecoveryArmedConfigWriteRetryFunnelsThroughStartFence(t *testing.T) {
	settings := testManagerSettings(t)
	dataDirectory := filepath.Dir(settings.ConfigPath)
	starter := &recordingStarter{stopsOnGraceful: true}
	requester := &fakeCredentialRequester{}
	collector := &statusCollector{}
	var fenceEntries atomic.Int64
	manager := newRecoveryManager(t, settings, starter, requester, collector,
		withBackoffBase(50*time.Millisecond),
		withStartFenceGate(func() { fenceEntries.Add(1) }),
	)

	generationOne := recoveryGenerationOne()
	child := startRecoveryChild(t, manager, starter, generationOne)
	if got := fenceEntries.Load(); got != 1 {
		t.Fatalf("start-fence entries after generation 1 = %d, want 1", got)
	}

	child.emitOutputLine(realFRPCReconnectFailureLine)
	waitForCondition(t, "recovery credential request", time.Second, func() bool {
		return requester.requestCount() == 1
	})

	// Break the config path so the recovery rewrite fails transiently.
	if err := os.RemoveAll(dataDirectory); err != nil {
		t.Fatalf("remove tunnel data directory: %v", err)
	}
	if err := os.WriteFile(dataDirectory, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("seed data-directory blocker: %v", err)
	}
	refreshed := generationOne
	refreshed.Credential = "credential-recovery-retry"
	refreshed.ExpiresAt = time.Now().Add(20 * time.Minute).UTC().Truncate(time.Second)
	if err := manager.ApplyConfig(refreshed); err != nil {
		t.Fatalf("ApplyConfig(refreshed) error = %v", err)
	}
	waitForCondition(t, "recovery armed-config write failure", time.Second, func() bool {
		return collector.hasReasonContaining("create tunnel data directory")
	})
	if got := child.gracefulStopCount(); got != 0 {
		t.Fatalf("rejected child stopped %d time(s) despite the failed rewrite; the replacement must wait for the retry", got)
	}

	// Clear the transient failure: the retry must write, stop and restart
	// through the fenced funnel.
	if err := os.Remove(dataDirectory); err != nil {
		t.Fatalf("clear data-directory blocker: %v", err)
	}
	waitForCondition(t, "replacement child start from the armed-config retry", 2*time.Second, func() bool {
		return starter.startCount() == 2
	})
	if got := child.gracefulStopCount(); got != 1 {
		t.Errorf("rejected child graceful stops = %d, want 1", got)
	}
	if got := fenceEntries.Load(); got != 2 {
		t.Fatalf("start-fence entries after recovery = %d, want 2 (the retry must funnel through startChildFenced)", got)
	}
	if content := configFileContent(t, settings.ConfigPath); !strings.Contains(content, refreshed.Credential) {
		t.Errorf("generated config does not carry the retried credential")
	}
}

// TestEnsureCredentialRequestsOnceWhenNoCredentialIsHeld pins the daemon's
// bootstrap event: with no credential held the manager issues a request with
// the given reason, and with a credential already armed (or a request already
// covered by an active wait cycle) it must not ask again.
func TestEnsureCredentialRequestsOnceWhenNoCredentialIsHeld(t *testing.T) {
	settings := testManagerSettings(t)
	starter := &recordingStarter{stopsOnGraceful: true}
	requester := &fakeCredentialRequester{}
	collector := &statusCollector{}
	manager := newRecoveryManager(t, settings, starter, requester, collector)

	if err := manager.EnsureCredential(ReasonRestart); err != nil {
		t.Fatalf("EnsureCredential(restart) error = %v", err)
	}
	waitForCondition(t, "bootstrap credential request", time.Second, func() bool {
		return requester.requestCount() == 1
	})
	if reasons := requester.requestReasons(); reasons[0] != ReasonRestart {
		t.Fatalf("bootstrap credential request reason = %v, want %s", reasons, ReasonRestart)
	}

	// With no credential held, a second EnsureCredential is the daemon's
	// reconnect retry: it re-issues once (never a hot loop inside the manager).
	if err := manager.EnsureCredential(ReasonRestart); err != nil {
		t.Fatalf("EnsureCredential(restart, retry) error = %v", err)
	}
	waitForCondition(t, "second bootstrap credential request", time.Second, func() bool {
		return requester.requestCount() == 2
	})

	// Once control answers, the manager holds a credential and further
	// EnsureCredential events are no-ops.
	if err := manager.ApplyConfig(recoveryGenerationOne()); err != nil {
		t.Fatalf("ApplyConfig() error = %v", err)
	}
	waitForCondition(t, "child start after bootstrap credential", time.Second, func() bool {
		return starter.startCount() == 1
	})
	if err := manager.EnsureCredential(ReasonRestart); err != nil {
		t.Fatalf("EnsureCredential after arming error = %v", err)
	}
	assertConditionStays(t, "EnsureCredential with a credential held is a no-op", 150*time.Millisecond, func() bool {
		return requester.requestCount() == 2 && starter.startCount() == 1
	})

	if err := manager.EnsureCredential("not-a-reason"); err == nil {
		t.Fatal("EnsureCredential must reject a reason outside the closed §11.1 enum")
	}
}

// TestEnsureCredentialIsSuppressedByStartFenceAndStop pins §13.4 for the new
// event: a fenced or stopped manager never asks control for a credential.
func TestEnsureCredentialIsSuppressedByStartFenceAndStop(t *testing.T) {
	settings := testManagerSettings(t)
	starter := &recordingStarter{stopsOnGraceful: true}
	requester := &fakeCredentialRequester{}
	collector := &statusCollector{}
	manager := newRecoveryManager(t, settings, starter, requester, collector)

	manager.SetStartPermitted(false)
	if err := manager.EnsureCredential(ReasonRestart); err != nil {
		t.Fatalf("EnsureCredential(fenced) error = %v", err)
	}
	assertConditionStays(t, "fenced manager never requests a credential", 150*time.Millisecond, func() bool {
		return requester.requestCount() == 0
	})
	if !collector.hasReasonContaining("start permission withdrawn") {
		t.Fatal("the fenced credential request was not reported")
	}

	if err := manager.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := manager.EnsureCredential(ReasonRestart); !errors.Is(err, errManagerStopped) {
		t.Fatalf("EnsureCredential after Stop error = %v, want %v", err, errManagerStopped)
	}
}

// TestReadBoundedLineDiscardsOverlongOutputWithoutUnboundedMemory pins the
// bounded reader: a line longer than the cap is reported truncated (and never
// matched), and the reader keeps going so the child's pipe is always drained.
func TestReadBoundedLineDiscardsOverlongOutputWithoutUnboundedMemory(t *testing.T) {
	overlong := strings.Repeat("x", maxChildOutputLineLength*3)
	input := overlong + "\n" + realFRPCReconnectFailureLine + "\n" + "tail"
	reader := bufio.NewReaderSize(strings.NewReader(input), childOutputReadBufferSize)

	line, truncated, err := readBoundedLine(reader)
	if err != nil {
		t.Fatalf("first readBoundedLine() error = %v", err)
	}
	if !truncated {
		t.Fatal("an over-long line must be reported truncated")
	}
	if len(line) > maxChildOutputLineLength {
		t.Fatalf("retained line length = %d, want <= %d", len(line), maxChildOutputLineLength)
	}
	if isReconnectFailureLine(line) {
		t.Fatal("a truncated line must never be treated as a reconnect failure")
	}

	line, truncated, err = readBoundedLine(reader)
	if err != nil || truncated {
		t.Fatalf("second readBoundedLine() = (%q, %v, %v), want the pinned line untruncated", line, truncated, err)
	}
	if !isReconnectFailureLine(line) {
		t.Fatalf("second line = %q, want the pinned reconnect-failure line", line)
	}

	line, truncated, err = readBoundedLine(reader)
	if err == nil || truncated || line != "tail" {
		t.Fatalf("final readBoundedLine() = (%q, %v, %v), want (\"tail\", false, EOF)", line, truncated, err)
	}
}

const frpcOutputHelperEnvironment = "SHAREBRIDGE_TUNNEL_OUTPUT_HELPER"

// TestFRPCProcessCapturesStdoutAndStderrAtExit runs the real
// startFRPCProcess plumbing (os.Pipe stdout/stderr -> bounded line pumps) with
// a helper process that writes the pinned line to stdout and another line to
// stderr, then exits. Both streams must be captured and the line channel must
// close once the process is reaped, so no pump goroutine or pipe survives.
func TestFRPCProcessCapturesStdoutAndStderrAtExit(t *testing.T) {
	if os.Getenv(frpcOutputHelperEnvironment) == "1" {
		// This branch runs inside the helper child process.
		os.Stdout.WriteString(realFRPCReconnectFailureLine + "\n")
		os.Stderr.WriteString("2026/09/03 15:25:11 proxy closing\n")
		os.Exit(0)
	}
	t.Setenv(frpcOutputHelperEnvironment, "1")

	child, err := startFRPCProcess(t.Context(), os.Args[0], []string{"-test.run=TestFRPCProcessCapturesStdoutAndStderrAtExit"})
	if err != nil {
		t.Fatalf("startFRPCProcess() error = %v", err)
	}
	output, ok := child.(childOutputStream)
	if !ok {
		t.Fatal("the production child adapter must expose its captured output")
	}

	lines := make(chan string, 8)
	collected := make(chan struct{})
	go func() {
		defer close(collected)
		for line := range output.OutputLines() {
			lines <- line
		}
	}()

	waitForCondition(t, "both captured child lines", 5*time.Second, func() bool {
		return len(lines) == 2
	})
	select {
	case <-collected:
	case <-time.After(5 * time.Second):
		t.Fatal("the captured output channel did not close after the child exited")
	}
	close(lines)
	captured := make([]string, 0, 2)
	for line := range lines {
		captured = append(captured, line)
	}
	joined := strings.Join(captured, "\n")
	if !strings.Contains(joined, realFRPCReconnectFailureLine) {
		t.Fatalf("captured output %q does not contain the pinned reconnect-failure line", joined)
	}
	if !strings.Contains(joined, "proxy closing") {
		t.Fatalf("captured output %q does not contain the stderr line", joined)
	}
	if err := child.Wait(); err != nil {
		t.Fatalf("child.Wait() error = %v", err)
	}
}
