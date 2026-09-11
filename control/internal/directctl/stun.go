package directctl

// STUN challenge scheduling (plan Task 18, spec §§10.2, 4.4, 7.1, 10.3).
//
// Cadence (§10.2): control challenges the agent immediately after the
// current-epoch enrollment handshake completes (enrollment_ready), including
// after every reconnect; while the socket stays connected it re-challenges
// four minutes after each accepted observation with small bounded jitter;
// observations expire after five minutes and are bound to one WebSocket
// epoch. A missing or stale observation is refreshed INLINE inside the
// caller's preparation context (§4.4) — at most one refresh per await, never
// exceeding the parent deadline — and may run concurrently with open_signal/
// open_ack; the public probe sequencing is Task 21's.
//
// Auth/rejection taxonomy for stun_result (§10.2, §16.4): accepted ONLY from
// the current API-key socket after hello; unknown challenge/transaction/
// receipt, wrong epoch, expiry (60 s claim window at the listener + 5 min
// observation freshness here), replay (observations are single-use) and
// malformed sizes are all rejected. Surplus stun_result messages are bounded
// by statelessness: at most one scheduler challenge + one inline challenge
// can be in flight per epoch (the Task 16 listener's per-agent pending cap),
// and a claim against no observation is a constant-time rejection — no second
// rate limiter is built; issuance reuses Task 16's per-agent token bucket via
// IssueChallenge and treats ErrRateLimited/ErrTooManyPending as bounded
// backoff failures.
//
// Bounds and hygiene (§14, §16.6): all challenge state is per-epoch (the
// struct dies with the epoch entry on reconnect/disconnect, timers are
// stopped and waiters failed on teardown — no goroutine leaks); challenge
// secrets, receipt bytes and challenge IDs are never logged.

import (
	"context"
	"encoding/hex"
	"log"
	"net/netip"
	"time"

	"github.com/coder/websocket"
	"sharebridge/control/internal/stun"
)

// Cadence and freshness constants (§10.2). The rechallenge fires at
// 4 minutes + [0, 15 s) jitter, so a fresh rechallenge answer normally lands
// at least ~45 s before the 5-minute freshness expiry, leaving the spec's
// "at least one minute to retry" margin from the 4-minute mark.
const (
	// stunObservationTTL is the §10.3 observation freshness window.
	stunObservationTTL = 5 * time.Minute
	// stunRechallengeAfter is the §10.2 proactive rechallenge base delay.
	stunRechallengeAfter = 4 * time.Minute
	// stunRechallengeJitter is the upper bound of the jitter ADDED to the
	// base (small, to avoid synchronized bursts across agents).
	stunRechallengeJitter = 15 * time.Second
	// stunRetryBase / stunRetryCap bound the §10.2 failure backoff:
	// base 5 s doubling per consecutive failure, capped at 30 s, plus
	// [0, delay/4) jitter (worst case 37.5 s). The base stays above a third
	// of the Task 16 token-bucket refill period (4/minute) so sustained
	// retries remain inside the per-agent rate limit instead of starving.
	stunRetryBase = 5 * time.Second
	stunRetryCap  = 30 * time.Second
	// stunMaxWaiters bounds concurrent inline-refresh waiters per epoch
	// (§14 bounded state); beyond it, awaits fail closed.
	stunMaxWaiters = 64

	// stunChallengeVersion matches the Task 17 client's ChallengeVersion.
	stunChallengeVersion = 1

	// stunChallengeEchoHexLen is the exact length of the challenge ID echo
	// (128-bit ID, lowercase hex) per the Task 17 ID-only echo contract.
	stunChallengeEchoHexLen = 32
	// stunTxnHexLen is the hex length of the 96-bit STUN transaction ID.
	stunTxnHexLen = 24
	// stunMaxReceiptBytes mirrors the wire bound the Task 17 sender enforces
	// (control issues 16-byte receipts; anything up to the bound is decoded
	// and then rejected by the exact-match comparison).
	stunMaxReceiptBytes = 128
)

// stunTimerKind discriminates the single per-epoch timer.
type stunTimerKind uint8

const (
	stunTimerNone stunTimerKind = iota
	// stunTimerChallengeExpiry fires at issuedAt+ChallengeTTL: if the
	// in-flight challenge produced no accepted observation, the attempt
	// failed and a bounded-backoff retry is scheduled.
	stunTimerChallengeExpiry
	// stunTimerRetry fires when a failed attempt may issue again.
	stunTimerRetry
	// stunTimerRechallenge fires at acceptedAt+4m+jitter (§10.2).
	stunTimerRechallenge
)

// STUNObservation is one accepted current-epoch STUN observation, carried
// into freshness/policy checks (AcceptedAt drives the 5-minute freshness;
// PublicIPv4 carries the listener's §10.3 address classification; the exact
// match against report/open_ack IPs is Task 19's).
type STUNObservation struct {
	IP         netip.Addr
	PublicIPv4 bool
	AcceptedAt time.Time
	Epoch      stun.Epoch
}

// epochSTUN is the bounded per-epoch challenge state. Guarded by
// Controller.stunMu; the timer is always stopped and waiters failed on epoch
// teardown (reconnect/disconnect), so nothing outlives the epoch.
type epochSTUN struct {
	obs        *STUNObservation
	inFlightID string    // "" = no challenge outstanding
	inFlightAt time.Time // single clock reading at issuance
	attempt    int       // consecutive failures; reset on acceptance

	timer     *time.Timer
	timerKind stunTimerKind
	timerAt   time.Time
	// rechallengeAt is the exact jittered deadline for the next proactive
	// rechallenge (computed once per acceptance).
	rechallengeAt time.Time

	waiters []chan awaitOutcome
}

// awaitOutcome wakes one inline AwaitFreshObservation call.
type awaitOutcome struct {
	obs STUNObservation
	ok  bool
}

// EnableSTUN installs the Task 16 listener on the controller and enables the
// §10.2 challenge scheduler. advertise is the public "host:port" agents use
// for the stun_challenge `server` field; an empty advertise (or nil server)
// keeps STUN scheduling disabled entirely. Call before serving traffic.
func (c *Controller) EnableSTUN(server *stun.Server, advertise string) {
	if server == nil || advertise == "" {
		log.Printf("stun scheduling disabled: listener or advertise address missing")
		return
	}
	c.stunServer = server
	c.stunAdvertise = advertise
	c.stunIssueFn = server.IssueChallenge
}

// stunEnabled reports whether challenge scheduling is wired.
func (c *Controller) stunEnabled() bool {
	return c.stunServer != nil && c.stunAdvertise != "" && c.stunIssueFn != nil
}

// stunRechallengeDelay returns the §10.2 rechallenge delay for jitter draw r
// (r ∈ [0,1]): 4 minutes + [0, 15 s).
func stunRechallengeDelay(r float64) time.Duration {
	return stunRechallengeAfter + time.Duration(r*float64(stunRechallengeJitter))
}

// stunBackoffDelay returns the bounded backoff after the attempt-th
// consecutive failure: base 5 s doubling, capped at 30 s, plus [0, d/4)
// jitter for draw r ∈ [0,1]. Never exceeds 37.5 s.
func stunBackoffDelay(attempt int, r float64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := stunRetryBase << uint(attempt-1)
	if d <= 0 || d > stunRetryCap {
		d = stunRetryCap
	}
	return d + time.Duration(r*float64(d)/4)
}

// ---------- scheduling entry points ----------

// stunChallengeAfterEnrollment issues the immediate §10.2 challenge for the
// CURRENT epoch after enrollment_ready was sent. Called from
// sendEnrollmentReady (enroll.go); a fenced epoch or disabled STUN is a no-op.
func (c *Controller) stunChallengeAfterEnrollment(apiKeyID string, conn *websocket.Conn, e *epochState) {
	if !c.stunEnabled() {
		return
	}
	now := c.nowFn()
	var msg map[string]any
	c.epochMu.Lock()
	if cur := c.epochs[apiKeyID]; cur != e || e.conn != conn {
		c.epochMu.Unlock()
		return // fenced: never challenge for a superseded socket
	}
	c.stunMu.Lock()
	es := c.stunStateLocked(e)
	msg, _ = c.stunIssueLocked(apiKeyID, e, es, now)
	c.stunMu.Unlock()
	c.epochMu.Unlock()
	if msg != nil {
		c.sendSTUNChallenge(apiKeyID, conn, msg)
	}
}

// stunTimerFired is the single timer callback for an epoch's challenge state.
// It is fenced on the epoch number, so a fired callback from a superseded
// epoch (or after disconnect) is a no-op — this is what makes timer teardown
// under reconnect/dropout leak-free.
func (c *Controller) stunTimerFired(apiKeyID string, epoch stun.Epoch) {
	if !c.stunEnabled() {
		return
	}
	now := c.nowFn() // single clock reading per tick
	var msg map[string]any
	var conn *websocket.Conn
	c.epochMu.Lock()
	e := c.epochs[apiKeyID]
	if e == nil || e.epoch != epoch {
		c.epochMu.Unlock()
		return
	}
	conn = e.conn // immutable after the epoch was installed
	c.stunMu.Lock()
	es := e.stun
	if es == nil {
		c.stunMu.Unlock()
		c.epochMu.Unlock()
		return
	}
	if now.Before(es.timerAt) {
		// Fired before its (fake-clock) deadline: re-arm the remainder.
		c.armTimerLocked(apiKeyID, e, es, es.timerKind, es.timerAt)
		c.stunMu.Unlock()
		c.epochMu.Unlock()
		return
	}
	switch es.timerKind {
	case stunTimerChallengeExpiry:
		// The in-flight challenge produced no observation in its TTL: one
		// bounded failure, then a backoff retry (§10.2).
		if es.inFlightID != "" {
			recordSTUN(metricSTUNTimeout)
			c.stunFailLocked(apiKeyID, e, es, now)
		}
	case stunTimerRetry, stunTimerRechallenge:
		if es.timerKind == stunTimerRechallenge && es.obs == nil {
			break
		}
		if es.timerKind == stunTimerRechallenge && now.Before(es.rechallengeAt) {
			// A newer acceptance moved the deadline: re-arm the remainder.
			c.armTimerLocked(apiKeyID, e, es, stunTimerRechallenge, es.rechallengeAt)
			break
		}
		if es.inFlightID != "" {
			break // bounded: never two scheduler challenges outstanding
		}
		m, _ := c.stunIssueLocked(apiKeyID, e, es, now)
		msg = m
	}
	c.stunMu.Unlock()
	c.epochMu.Unlock()
	if msg != nil {
		c.sendSTUNChallenge(apiKeyID, conn, msg)
	}
}

// stunIssueLocked issues one challenge via the Task 16 listener (the
// per-agent token bucket lives there — no second limiter). On success it
// marks the attempt in flight and arms the challenge-expiry timer; on failure
// it records one bounded failure and arms the retry timer, waking inline
// waiters (fail closed). Returns the wire message to send, or nil.
// Caller holds stunMu (and epochMu for the epoch identity).
func (c *Controller) stunIssueLocked(apiKeyID string, e *epochState, es *epochSTUN, now time.Time) (map[string]any, bool) {
	if es.inFlightID != "" {
		return nil, false // bounded: one outstanding challenge per epoch
	}
	ch, err := c.stunIssueFn(apiKeyID, e.epoch)
	if err != nil {
		// Rate-limited/pending-capped per §16.4: fail this attempt with
		// bounded backoff. The error text is static (no secret material).
		log.Printf("stun challenge issue failed for %s: %v", apiKeyID, err)
		c.stunFailLocked(apiKeyID, e, es, now)
		return nil, false
	}
	es.inFlightID = ch.ID
	es.inFlightAt = now
	c.armTimerLocked(apiKeyID, e, es, stunTimerChallengeExpiry, now.Add(stun.ChallengeTTL))
	return map[string]any{
		"type":       "stun_challenge",
		"version":    stunChallengeVersion,
		"challenge":  ch.ID + "." + hex.EncodeToString(ch.Secret),
		"server":     c.stunAdvertise,
		"expires_at": ch.ExpiresAt.Format(time.RFC3339),
	}, true
}

// stunFailLocked records one failed attempt: bump attempt, clear the
// in-flight marker, fail inline waiters, and schedule the bounded-backoff
// retry. Caller holds stunMu.
func (c *Controller) stunFailLocked(apiKeyID string, e *epochState, es *epochSTUN, now time.Time) {
	es.inFlightID = ""
	es.attempt++
	c.notifyWaitersLocked(es, STUNObservation{}, false)
	c.armTimerLocked(apiKeyID, e, es, stunTimerRetry, now.Add(stunBackoffDelay(es.attempt, c.randFn())))
}

// armTimerLocked (re)arms the epoch's single timer for kind at the given
// (already jittered) deadline. Caller holds stunMu.
func (c *Controller) armTimerLocked(apiKeyID string, e *epochState, es *epochSTUN, kind stunTimerKind, at time.Time) {
	if es.timer != nil {
		es.timer.Stop()
		es.timer = nil
	}
	es.timerKind = kind
	es.timerAt = at
	delay := at.Sub(c.nowFn())
	if delay < 10*time.Millisecond {
		delay = 10 * time.Millisecond
	}
	epoch := e.epoch
	es.timer = time.AfterFunc(delay, func() { c.stunTimerFired(apiKeyID, epoch) })
}

// stunStateLocked lazily creates the per-epoch state. Caller holds stunMu.
func (c *Controller) stunStateLocked(e *epochState) *epochSTUN {
	if e.stun == nil {
		e.stun = &epochSTUN{}
	}
	return e.stun
}

// stunTeardownLocked stops the epoch timer and fails its waiters (no
// goroutine leaks, no dangling awaits across reconnect/disconnect). Caller
// holds stunMu.
func (c *Controller) stunTeardownLocked(e *epochState) {
	es := e.stun
	if es == nil {
		return
	}
	if es.timer != nil {
		es.timer.Stop()
		es.timer = nil
	}
	es.inFlightID = ""
	es.timerKind = stunTimerNone
	c.notifyWaitersLocked(es, STUNObservation{}, false)
	e.stun = nil
}

// notifyWaitersLocked wakes every inline waiter (buffered channels, so this
// never blocks). Caller holds stunMu.
func (c *Controller) notifyWaitersLocked(es *epochSTUN, obs STUNObservation, ok bool) {
	for _, ch := range es.waiters {
		select {
		case ch <- awaitOutcome{obs: obs, ok: ok}:
		default:
		}
	}
	es.waiters = nil
}

// sendSTUNChallenge writes the stun_challenge message outside all locks. A
// send failure counts as a failed attempt (the agent never saw the
// credential; the listener burns the challenge at its TTL).
func (c *Controller) sendSTUNChallenge(apiKeyID string, conn *websocket.Conn, msg map[string]any) {
	if err := c.sendFn(context.Background(), conn, msg); err != nil {
		log.Printf("stun_challenge send failed for %s", apiKeyID)
		now := c.nowFn()
		c.epochMu.Lock()
		e := c.epochs[apiKeyID]
		if e != nil {
			c.stunMu.Lock()
			if es := e.stun; es != nil && es.inFlightID != "" {
				c.stunFailLocked(apiKeyID, e, es, now)
			}
			c.stunMu.Unlock()
		}
		c.epochMu.Unlock()
	}
}

// ---------- stun_result acceptance ----------

// HandleSTUNResult processes one §11.1 stun_result (challenge ID echo,
// transaction ID, hex receipt). It is the ONLY path that creates an
// observation. Rejections — wrong socket/epoch, malformed sizes, unknown
// challenge/transaction/receipt, replay, expiry — never mutate scheduling
// state and never touch relay availability (§10.3: observations are input to
// direct eligibility only).
func (c *Controller) HandleSTUNResult(conn *websocket.Conn, apiKeyID, challenge, txnID, receiptHex string) {
	if !c.stunEnabled() {
		return
	}
	// Wire validation first (Task 17 goldens: ID-only echo, 24-char
	// lowercase-hex txn, hex receipt within the bounded size).
	if len(challenge) != stunChallengeEchoHexLen || !isLowerHex(challenge) {
		recordSTUN(metricSTUNMismatch)
		log.Printf("stun_result rejected for %s: malformed challenge echo length/case", apiKeyID)
		return
	}
	if len(txnID) != stunTxnHexLen || !isLowerHex(txnID) {
		recordSTUN(metricSTUNMismatch)
		log.Printf("stun_result rejected for %s: malformed transaction id length/case", apiKeyID)
		return
	}
	receipt, err := hex.DecodeString(receiptHex)
	if err != nil || len(receipt) == 0 || len(receipt) > stunMaxReceiptBytes {
		recordSTUN(metricSTUNMismatch)
		log.Printf("stun_result rejected for %s: malformed receipt size/hex", apiKeyID)
		return
	}

	c.epochMu.Lock()
	e := c.epochs[apiKeyID]
	if e == nil || e.conn != conn {
		c.epochMu.Unlock()
		recordSTUN(metricSTUNMismatch)
		log.Printf("stun_result rejected for %s: not the current epoch socket", apiKeyID)
		return
	}
	epoch := e.epoch
	c.stunMu.Lock()
	// Current WS epoch at BOTH IssueChallenge and TakeObservation (the
	// listener additionally binds the observation to the challenge's epoch,
	// so a stale-epoch result fails closed with ErrEpochMismatch).
	obs, err := c.stunServer.TakeObservation(apiKeyID, epoch, challenge, txnID, receipt)
	if err != nil {
		c.stunMu.Unlock()
		c.epochMu.Unlock()
		// Unknown challenge/txn/receipt, replay (single use), wrong epoch or
		// expired claim window; error text is static (no material).
		recordSTUN(metricSTUNMismatch)
		log.Printf("stun_result rejected for %s: %v", apiKeyID, err)
		return
	}
	es := c.stunStateLocked(e)
	es.obs = &STUNObservation{
		IP:         obs.Source,
		PublicIPv4: obs.PublicIPv4,
		AcceptedAt: obs.AcceptedAt,
		Epoch:      obs.Epoch,
	}
	es.attempt = 0
	es.inFlightID = ""
	es.rechallengeAt = obs.AcceptedAt.Add(stunRechallengeDelay(c.randFn()))
	recordSTUN(metricSTUNMatch)
	log.Printf("stun observation accepted for %s (source observed; details in direct posture, not logged)", apiKeyID)
	c.armTimerLocked(apiKeyID, e, es, stunTimerRechallenge, es.rechallengeAt)
	c.notifyWaitersLocked(es, *es.obs, true)
	c.stunMu.Unlock()
	c.epochMu.Unlock()
}

// ---------- observation reads (freshness) ----------

// CurrentSTUNObservation returns the current epoch's observation iff it is
// fresh (accepted within stunObservationTTL of the caller's single clock
// reading, now). A reconnect implicitly invalidates: only the current epoch's
// state is ever consulted.
func (c *Controller) CurrentSTUNObservation(apiKeyID string, now time.Time) (STUNObservation, bool) {
	c.epochMu.Lock()
	e := c.epochs[apiKeyID]
	if e == nil {
		c.epochMu.Unlock()
		return STUNObservation{}, false
	}
	c.stunMu.Lock()
	defer c.stunMu.Unlock()
	defer c.epochMu.Unlock()
	if e.stun == nil || e.stun.obs == nil {
		return STUNObservation{}, false
	}
	o := *e.stun.obs
	if !now.Before(o.AcceptedAt.Add(stunObservationTTL)) {
		return STUNObservation{}, false
	}
	return o, true
}

// AwaitFreshObservation implements the §4.4 inline refresh: it returns the
// fresh current-epoch observation when one exists, otherwise it awaits ONE
// refresh — reusing an in-flight challenge if the scheduler already has one
// outstanding, issuing exactly one inline challenge otherwise — and returns
// as soon as the observation lands, the refresh fails, or the parent context
// is done. It never exceeds the parent deadline (the four-second preparation
// context) and never blocks on the wire write (background send). A failed
// issuance returns immediately (fail closed → relay fallback, §10.3).
func (c *Controller) AwaitFreshObservation(ctx context.Context, apiKeyID string) (STUNObservation, bool) {
	if !c.stunEnabled() {
		return STUNObservation{}, false
	}
	if err := ctx.Err(); err != nil {
		return STUNObservation{}, false
	}
	now := c.nowFn() // single clock reading for the freshness decision
	if obs, ok := c.CurrentSTUNObservation(apiKeyID, now); ok {
		return obs, true
	}

	ch := make(chan awaitOutcome, 1)
	var (
		send   map[string]any
		conn   *websocket.Conn
		issued bool
		failed bool
	)
	c.epochMu.Lock()
	e := c.epochs[apiKeyID]
	if e == nil {
		c.epochMu.Unlock()
		return STUNObservation{}, false
	}
	c.stunMu.Lock()
	// Re-check freshness under the locks (missed-wakeup guard: an
	// acceptance may have landed between the fast path and here).
	if e.stun != nil && e.stun.obs != nil && now.Before(e.stun.obs.AcceptedAt.Add(stunObservationTTL)) {
		o := *e.stun.obs
		c.stunMu.Unlock()
		c.epochMu.Unlock()
		return o, true
	}
	es := c.stunStateLocked(e)
	if len(es.waiters) >= stunMaxWaiters {
		c.stunMu.Unlock()
		c.epochMu.Unlock()
		return STUNObservation{}, false
	}
	es.waiters = append(es.waiters, ch)
	if es.inFlightID == "" {
		var ok bool
		send, ok = c.stunIssueLocked(apiKeyID, e, es, now)
		issued, failed = ok, !ok
		conn = e.conn
	}
	c.stunMu.Unlock()
	c.epochMu.Unlock()

	if issued {
		// Background send: the await selects on the outcome and the parent
		// deadline only (§4.4 — never exceeds the parent deadline).
		go c.sendSTUNChallenge(apiKeyID, conn, send)
	}

	if failed {
		// stunFailLocked already notified this waiter (buffered).
		<-ch
		return STUNObservation{}, false
	}
	select {
	case out := <-ch:
		return out.obs, out.ok
	case <-ctx.Done():
		c.epochMu.Lock()
		c.stunMu.Lock()
		if es := e.stun; es != nil {
			for i, w := range es.waiters {
				if w == ch {
					es.waiters = append(es.waiters[:i], es.waiters[i+1:]...)
					break
				}
			}
		}
		c.stunMu.Unlock()
		c.epochMu.Unlock()
		return STUNObservation{}, false
	}
}

// isLowerHex reports whether s is nonempty lowercase hex.
func isLowerHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return false
		}
	}
	return true
}
