package directctl

// Tests for plan Task 18 (spec §§10.2, 4.4, 7.1, 10.3): scheduling of
// immediate, proactive and inline STUN challenges on the current WebSocket
// epoch, observation freshness, and the stun_result rejection taxonomy.
//
// Task 17 wire goldens are pinned control-side: the stun_challenge credential
// field packs "<32 lowercase-hex id>.<64 lowercase-hex secret>", and
// stun_result echoes the challenge ID only (32 hex chars), a 24-char
// lowercase-hex transaction ID and a lowercase-hex receipt that control
// hex-decodes before TakeObservation.
//
// The STUN listener runs for real on a loopback UDP socket (shared fake
// clock), and the test acts as the agent for the UDP half of the flow: it
// sends the integrity-protected Binding request and extracts the receipt from
// the integrity-protected response, exactly like the Task 17 client.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	pionstun "github.com/pion/stun/v3"
	"github.com/pocketbase/pocketbase/core"
	"sharebridge/control/internal/stun"
)

// ---------- test infrastructure ----------

// stunTestClock is a goroutine-safe controllable clock seam shared by the
// controller and the loopback STUN listener (§10.2 cadence/freshness and the
// listener's 60 s challenge/claim windows must agree on one clock).
type stunTestClock struct {
	nanos atomic.Int64
}

func newSTUNTestClock() *stunTestClock {
	c := &stunTestClock{}
	c.nanos.Store(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC).UnixNano())
	return c
}

func (c *stunTestClock) Now() time.Time          { return time.Unix(0, c.nanos.Load()) }
func (c *stunTestClock) Advance(d time.Duration) { c.nanos.Add(int64(d)) }

// stunSendRecorder permanently swaps ctrl.sendFn to record every outbound
// message (challenges are also emitted from timer callbacks, so a scoped
// capture would miss them).
type stunSendRecorder struct {
	mu   sync.Mutex
	msgs []map[string]any
}

func newSTUNSendRecorder(t *testing.T, ctrl *Controller) *stunSendRecorder {
	t.Helper()
	rec := &stunSendRecorder{}
	ctrl.sendFn = func(ctx context.Context, conn *websocket.Conn, msg any) error {
		var m map[string]any
		switch v := msg.(type) {
		case map[string]any:
			m = v
		case map[string]string:
			m = make(map[string]any, len(v))
			for k, s := range v {
				m[k] = s
			}
		default:
			t.Errorf("stun send recorder: unexpected message type %T", msg)
			return nil
		}
		rec.mu.Lock()
		rec.msgs = append(rec.msgs, m)
		rec.mu.Unlock()
		return nil
	}
	return rec
}

func (r *stunSendRecorder) snapshot() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]any(nil), r.msgs...)
}

func (r *stunSendRecorder) challenges() []map[string]any {
	var out []map[string]any
	for _, m := range r.snapshot() {
		if m["type"] == "stun_challenge" {
			out = append(out, m)
		}
	}
	return out
}

// newSTUNEnv builds a controller with a shared fake clock, deterministic
// jitter (randFn = 0) and a stubbed relay DNS wildcard so enrollment completes.
// Create the agent's api key with mustAPIKey(t, app, name).Id before enrolling.
func newSTUNEnv(t *testing.T) (core.App, *Controller, *stunTestClock) {
	t.Helper()
	app, ctrl := newTestController(t)
	clock := newSTUNTestClock()
	ctrl.nowFn = clock.Now
	ctrl.randFn = func() float64 { return 0 }
	ctrl.cfg.RelayGatewayIPv4 = testRelayGatewayIPv4
	ctrl.relayDNSFn = func(ctx context.Context, name, ip string, ttl int) (string, error) {
		return "", nil
	}
	return app, ctrl, clock
}

// attachSTUNListener starts a REAL stun listener on a loopback UDP socket
// sharing the controller's fake clock, installs it on the controller, and
// returns the advertise address used in stun_challenge messages.
func attachSTUNListener(t *testing.T, ctrl *Controller, clock *stunTestClock) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("stun key: %v", err)
	}
	srv, err := stun.NewServer(stun.Config{
		Key:      key,
		BindAddr: "127.0.0.1:0",
		Now:      clock.Now,
		// Headroom for multi-challenge test flows.
		MaxChallengesPerAgentPerMinute: 64,
		MaxPendingPerAgent:             8,
	})
	if err != nil {
		t.Fatalf("stun server: %v", err)
	}
	conn, err := srv.Listen()
	if err != nil {
		t.Fatalf("stun listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx, conn) }()
	t.Cleanup(func() {
		cancel()
		_ = conn.Close()
	})
	advertise := conn.LocalAddr().String()
	ctrl.EnableSTUN(srv, advertise)
	return advertise
}

// enrollReadyAgent runs the current-epoch enrollment handshake (hello +
// tls_ready with a stub-issued leaf) and registers cleanup that tears the
// epoch (and its timers) down at test end.
func enrollReadyAgent(t *testing.T, ctrl *Controller, apiKeyID string) {
	t.Helper()
	ctrl.HandleHello(context.Background(), nil, apiKeyID, "acct-1", "agent-1")
	t.Cleanup(func() { ctrl.AgentDisconnected(apiKeyID, nil) })
	enrollReadyAgentReadyTLS(t, ctrl, apiKeyID)
}

// enrollReadyAgentReadyTLS completes only the tls_ready half for the CURRENT
// epoch of key (used after a reconnect hello).
func enrollReadyAgentReadyTLS(t *testing.T, ctrl *Controller, apiKeyID string) {
	t.Helper()
	fp := issueTestLeaf(t, ctrl, apiKeyID)
	ctrl.HandleTLSReady(context.Background(), nil, apiKeyID, fp, time.Now().Add(24*time.Hour).Format(time.RFC3339))
}

// packedChallengeGolden asserts the Task 17 stun_challenge wire goldens on a
// captured message and returns the challenge ID (the ID-only echo contract).
func packedChallengeGolden(t *testing.T, ctrl *Controller, msg map[string]any, advertise string) string {
	t.Helper()
	if msg["type"] != "stun_challenge" {
		t.Fatalf("expected stun_challenge, got %v", msg["type"])
	}
	if v, ok := msg["version"].(float64); ok {
		// JSON-decoded number
		if int(v) != 1 {
			t.Fatalf("stun_challenge version must be 1, got %v", msg["version"])
		}
	} else if v, ok := msg["version"].(int); !ok || v != 1 {
		t.Fatalf("stun_challenge version must be 1, got %v", msg["version"])
	}
	packed, _ := msg["challenge"].(string)
	if !stunPackedChallengeRE.MatchString(packed) {
		t.Fatalf("packed challenge golden mismatch: %q", packed)
	}
	if msg["server"] != advertise {
		t.Fatalf("stun_challenge server = %v, want %q", msg["server"], advertise)
	}
	expRaw, _ := msg["expires_at"].(string)
	exp, err := time.Parse(time.RFC3339, expRaw)
	if err != nil {
		t.Fatalf("stun_challenge expires_at not RFC3339: %v", err)
	}
	now := ctrl.nowFn()
	if d := exp.Sub(now); d <= 0 || d > time.Minute {
		t.Fatalf("stun_challenge expires_at not within (now, now+60s]: %v", d)
	}
	id, _, _ := strings.Cut(packed, ".")
	return id
}

// stunExchange acts as the agent's UDP half (Task 17 client): it splits the
// packed credential, sends one integrity-protected Binding request, verifies
// the response integrity and returns the ID-only echo, the lowercase-hex
// transaction ID and the hex receipt, plus the client's local address (the
// expected observation source).
func stunExchange(t *testing.T, advertise, packed string) (challengeID, txnHex, receiptHex string, src netip.AddrPort) {
	t.Helper()
	idPart, secretPart, found := strings.Cut(packed, ".")
	if !found {
		t.Fatalf("packed challenge missing separator: %q", packed)
	}
	secret, err := hex.DecodeString(secretPart)
	if err != nil {
		t.Fatalf("secret part not hex: %v", err)
	}
	raddr, err := net.ResolveUDPAddr("udp4", advertise)
	if err != nil {
		t.Fatalf("resolve %q: %v", advertise, err)
	}
	conn, err := net.DialUDP("udp4", nil, raddr)
	if err != nil {
		t.Fatalf("dial stun: %v", err)
	}
	defer conn.Close()
	local := conn.LocalAddr().(*net.UDPAddr)
	var ip4bytes [4]byte
	copy(ip4bytes[:], local.IP.To4())
	src = netip.AddrPortFrom(netip.AddrFrom4(ip4bytes), uint16(local.Port))

	m := pionstun.New()
	m.Type = pionstun.BindingRequest
	m.TransactionID = pionstun.NewTransactionID()
	m.WriteHeader()
	m.Add(pionstun.AttrUsername, []byte(idPart))
	if err := pionstun.NewShortTermIntegrity(string(secret)).AddTo(m); err != nil {
		t.Fatalf("request integrity: %v", err)
	}
	if _, err := conn.Write(m.Raw); err != nil {
		t.Fatalf("send binding: %v", err)
	}
	buf := make([]byte, 1500)
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read binding response: %v", err)
	}
	resp := pionstun.New()
	resp.Raw = append([]byte(nil), buf[:n]...)
	if err := resp.Decode(); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Type != pionstun.BindingSuccess {
		t.Fatalf("expected BindingSuccess, got %v", resp.Type)
	}
	if resp.TransactionID != m.TransactionID {
		t.Fatalf("response transaction mismatch")
	}
	if err := pionstun.NewShortTermIntegrity(string(secret)).Check(resp); err != nil {
		t.Fatalf("response MESSAGE-INTEGRITY check failed: %v", err)
	}
	receipt, err := resp.Get(stun.AttrReceipt)
	if err != nil {
		t.Fatalf("response missing receipt attribute: %v", err)
	}
	txn := hex.EncodeToString(resp.TransactionID[:])
	if len(txn) != 24 || !stunLowerHexRE.MatchString(txn) {
		t.Fatalf("transaction id wire golden mismatch: %q", txn)
	}
	return idPart, txn, hex.EncodeToString(receipt), src
}

// deliverSTUNResult feeds one stun_result through the controller exactly as
// the agent-WS handler would (hex receipt; handler-level epoch fencing already
// ran).
func deliverSTUNResult(t *testing.T, ctrl *Controller, apiKeyID, challengeID, txnHex, receiptHex string) {
	t.Helper()
	ctrl.HandleSTUNResult(nil, apiKeyID, challengeID, txnHex, receiptHex)
}

func currentEpochNum(t *testing.T, ctrl *Controller, apiKeyID string) stun.Epoch {
	t.Helper()
	ctrl.epochMu.Lock()
	defer ctrl.epochMu.Unlock()
	e := ctrl.epochs[apiKeyID]
	if e == nil {
		return 0
	}
	return e.epoch
}

// stunStateSnapshot copies the per-epoch challenge state under the locks.
type stunStateSnapshot struct {
	inFlight      bool
	attempt       int
	timerAt       time.Time
	rechallengeAt time.Time
	obs           STUNObservation
	obsFresh      bool
}

func stunSnapshot(t *testing.T, ctrl *Controller, apiKeyID string) stunStateSnapshot {
	t.Helper()
	ctrl.epochMu.Lock()
	defer ctrl.epochMu.Unlock()
	e := ctrl.epochs[apiKeyID]
	if e == nil {
		t.Fatalf("no epoch for %s", apiKeyID)
	}
	ctrl.stunMu.Lock()
	defer ctrl.stunMu.Unlock()
	s := stunStateSnapshot{}
	if es := e.stun; es != nil {
		s.inFlight = es.inFlightID != ""
		s.attempt = es.attempt
		s.timerAt = es.timerAt
		s.rechallengeAt = es.rechallengeAt
		if es.obs != nil {
			s.obs = *es.obs
			s.obsFresh = ctrl.nowFn().Before(es.obs.AcceptedAt.Add(stunObservationTTL))
		}
	}
	return s
}

// waitForChallenges polls the recorder until n challenges have been sent
// (inline issuance happens on the await goroutine).
func waitForChallenges(t *testing.T, rec *stunSendRecorder, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := rec.challenges(); len(got) >= n {
			return got
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d challenges, saw %d", n, len(rec.challenges()))
	return nil
}

// ---------- named tests ----------

// TestSTUNChallengeImmediatelyAfterEnrollmentReadyForCurrentEpoch pins §10.2:
// control challenges the agent immediately after the current-epoch enrollment
// handshake completes (enrollment_ready), with the Task 17 wire goldens, and
// the resulting observation is bound to exactly that epoch.
func TestSTUNChallengeImmediatelyAfterEnrollmentReadyForCurrentEpoch(t *testing.T) {
	app, ctrl, clock := newSTUNEnv(t)
	advertise := attachSTUNListener(t, ctrl, clock)
	rec := newSTUNSendRecorder(t, ctrl)
	key := mustAPIKey(t, app, "key-stun-immediate").Id

	enrollReadyAgent(t, ctrl, key)

	msgs := rec.snapshot()
	if len(msgs) < 3 {
		t.Fatalf("expected enrolled + enrollment_ready + stun_challenge, got %v", msgs)
	}
	last := msgs[len(msgs)-1]
	readyIdx := -1
	for i, m := range msgs {
		if m["type"] == "enrollment_ready" {
			readyIdx = i
		}
	}
	if readyIdx < 0 {
		t.Fatalf("no enrollment_ready in %v", msgs)
	}
	if readyIdx != len(msgs)-2 || last["type"] != "stun_challenge" {
		t.Fatalf("stun_challenge must be the message immediately after enrollment_ready: %v", msgs)
	}
	packed, _ := last["challenge"].(string)
	packedChallengeGolden(t, ctrl, last, advertise)

	// The challenge is bound to the CURRENT epoch: a full agent exchange
	// yields a claimable, fresh observation carrying that epoch number.
	epoch := currentEpochNum(t, ctrl, key)
	if epoch == 0 {
		t.Fatalf("epoch number not assigned")
	}
	id, txnHex, receiptHex, _ := stunExchange(t, advertise, packed)
	deliverSTUNResult(t, ctrl, key, id, txnHex, receiptHex)

	obs, ok := ctrl.CurrentSTUNObservation(key, clock.Now())
	if !ok {
		t.Fatalf("observation not fresh immediately after stun_result")
	}
	if obs.Epoch != epoch {
		t.Fatalf("observation epoch = %d, want current epoch %d", obs.Epoch, epoch)
	}

	// Reconnect (new epoch): a new immediate challenge is issued for the new
	// epoch, and the OLD epoch's timer callbacks are fenced (firing the old
	// epoch's rechallenge timer must not challenge).
	ctrl.HandleHello(context.Background(), nil, key, "acct-1", "agent-1")
	oldEpoch := epoch
	enrollReadyAgentReadyTLS(t, ctrl, key)
	if got := len(rec.challenges()); got != 2 {
		t.Fatalf("expected exactly one new challenge after reconnect, got %d total", got)
	}
	if now := currentEpochNum(t, ctrl, key); now == oldEpoch {
		t.Fatalf("reconnect must install a new epoch number")
	}
	clock.Advance(10 * time.Minute)
	ctrl.stunTimerFired(key, oldEpoch)
	if got := len(rec.challenges()); got != 2 {
		t.Fatalf("old-epoch timer fire must not challenge: got %d total", got)
	}
}

// TestSTUNRechallengeAtFourMinutesWithJitter pins §10.2: while the socket
// stays connected, control re-challenges four minutes after each accepted
// observation, with small bounded jitter, and never earlier.
func TestSTUNRechallengeAtFourMinutesWithJitter(t *testing.T) {
	app, ctrl, clock := newSTUNEnv(t)
	advertise := attachSTUNListener(t, ctrl, clock)
	rec := newSTUNSendRecorder(t, ctrl)
	key := mustAPIKey(t, app, "key-stun-rechallenge").Id

	enrollReadyAgent(t, ctrl, key)
	if len(rec.challenges()) != 1 {
		t.Fatalf("expected exactly the enrollment challenge, got %d", len(rec.challenges()))
	}
	packed, _ := rec.challenges()[0]["challenge"].(string)
	id, txnHex, receiptHex, _ := stunExchange(t, advertise, packed)
	deliverSTUNResult(t, ctrl, key, id, txnHex, receiptHex)
	acceptedAt := clock.Now()
	if s := stunSnapshot(t, ctrl, key); !s.obsFresh || !s.rechallengeAt.Equal(acceptedAt.Add(4*time.Minute)) {
		t.Fatalf("rechallenge must be scheduled at acceptance+4m with zero jitter, got %v (accepted %v)", s.rechallengeAt, acceptedAt)
	}

	// Just before the deadline: the rechallenge timer must not issue.
	clock.Advance(4*time.Minute - time.Nanosecond)
	ctrl.stunTimerFired(key, currentEpochNum(t, ctrl, key))
	if got := len(rec.challenges()); got != 1 {
		t.Fatalf("rechallenge fired early: %d challenges", got)
	}
	// At the deadline: exactly one new challenge.
	clock.Advance(time.Nanosecond)
	ctrl.stunTimerFired(key, currentEpochNum(t, ctrl, key))
	if got := len(rec.challenges()); got != 2 {
		t.Fatalf("rechallenge at 4m must issue exactly one challenge, got %d total", got)
	}
	if s := stunSnapshot(t, ctrl, key); !s.inFlight {
		t.Fatalf("rechallenge must mark a challenge in flight")
	}

	// Jitter is bounded: base 4m + [0, 15s).
	if d := stunRechallengeDelay(0); d != 4*time.Minute {
		t.Fatalf("jitter draw 0 must yield exactly 4m, got %v", d)
	}
	if d := stunRechallengeDelay(1); d != 4*time.Minute+15*time.Second {
		t.Fatalf("jitter draw 1 must yield 4m15s, got %v", d)
	}
	for i := 0; i < 200; i++ {
		r := float64(i) / 200
		if d := stunRechallengeDelay(r); d < 4*time.Minute || d >= 4*time.Minute+15*time.Second {
			t.Fatalf("jitter bound violated at r=%f: %v", r, d)
		}
	}

	// Functional check with full jitter: acceptance reschedules at +4m15s.
	ctrl.randFn = func() float64 { return 1 }
	packed2, _ := rec.challenges()[1]["challenge"].(string)
	id2, txnHex2, receiptHex2, _ := stunExchange(t, advertise, packed2)
	deliverSTUNResult(t, ctrl, key, id2, txnHex2, receiptHex2)
	if s := stunSnapshot(t, ctrl, key); !s.rechallengeAt.Equal(clock.Now().Add(4*time.Minute + 15*time.Second)) {
		t.Fatalf("full jitter must schedule at +4m15s, got %v", s.rechallengeAt)
	}
}

// TestFreshObservationReusedByWarmPrepare pins the freshness half of §10.2:
// a fresh current-epoch observation is reused by warm preparation without any
// challenge, and it expires after five minutes.
func TestFreshObservationReusedByWarmPrepare(t *testing.T) {
	app, ctrl, clock := newSTUNEnv(t)
	advertise := attachSTUNListener(t, ctrl, clock)
	rec := newSTUNSendRecorder(t, ctrl)
	key := mustAPIKey(t, app, "key-stun-warm").Id

	enrollReadyAgent(t, ctrl, key)
	packed, _ := rec.challenges()[0]["challenge"].(string)
	id, txnHex, receiptHex, src := stunExchange(t, advertise, packed)
	deliverSTUNResult(t, ctrl, key, id, txnHex, receiptHex)

	before := len(rec.challenges())

	obs, ok := ctrl.AwaitFreshObservation(context.Background(), key)
	if !ok {
		t.Fatalf("fresh observation must be reused without a challenge")
	}
	if obs.IP != src.Addr() {
		t.Fatalf("observation IP = %v, want the actual UDP source %v", obs.IP, src.Addr())
	}
	if obs.PublicIPv4 {
		// Loopback is classified non-public by the listener; the field must
		// faithfully carry that classification (§10.3 policy is Task 19's).
		t.Fatalf("PublicIPv4 must be false for a loopback observation")
	}
	if after := len(rec.challenges()); after != before {
		t.Fatalf("warm reuse must not challenge: %d -> %d", before, after)
	}

	// Freshness expires after five minutes (single clock reading comparison).
	if _, ok := ctrl.CurrentSTUNObservation(key, clock.Now().Add(5*time.Minute)); ok {
		t.Fatalf("observation must be stale at exactly 5 minutes")
	}
	if _, ok := ctrl.CurrentSTUNObservation(key, clock.Now().Add(5*time.Minute-time.Nanosecond)); !ok {
		t.Fatalf("observation must still be fresh just before 5 minutes")
	}
}

// TestMissingOrStaleObservationChallengesInlineOnce pins §10.2/§4.4: a
// missing or stale observation issues exactly ONE inline challenge inside the
// caller's (four-second) context, reusing an in-flight challenge instead of
// issuing a second one, and never exceeds the parent deadline.
func TestMissingOrStaleObservationChallengesInlineOnce(t *testing.T) {
	type awaitResult struct {
		obs STUNObservation
		ok  bool
	}

	t.Run("missing observation reuses the in-flight challenge", func(t *testing.T) {
		app, ctrl, clock := newSTUNEnv(t)
		advertise := attachSTUNListener(t, ctrl, clock)
		rec := newSTUNSendRecorder(t, ctrl)
		key := mustAPIKey(t, app, "key-stun-inline-missing").Id

		enrollReadyAgent(t, ctrl, key) // enrollment challenge is in flight
		if len(rec.challenges()) != 1 {
			t.Fatalf("expected the enrollment challenge, got %d", len(rec.challenges()))
		}

		res := make(chan awaitResult, 1)
		go func() {
			obs, ok := ctrl.AwaitFreshObservation(context.Background(), key)
			res <- awaitResult{obs, ok}
		}()
		got := waitForChallenges(t, rec, 1)
		if len(got) != 1 {
			t.Fatalf("await must reuse the in-flight challenge, saw %d", len(got))
		}
		packed, _ := got[0]["challenge"].(string)
		id, txnHex, receiptHex, _ := stunExchange(t, advertise, packed)
		deliverSTUNResult(t, ctrl, key, id, txnHex, receiptHex)
		select {
		case r := <-res:
			if !r.ok {
				t.Fatalf("await must succeed once the in-flight challenge is answered")
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("await did not return after the observation landed")
		}
	})

	t.Run("stale observation challenges inline exactly once", func(t *testing.T) {
		app, ctrl, clock := newSTUNEnv(t)
		advertise := attachSTUNListener(t, ctrl, clock)
		rec := newSTUNSendRecorder(t, ctrl)
		key := mustAPIKey(t, app, "key-stun-inline-stale").Id

		enrollReadyAgent(t, ctrl, key)
		packed, _ := rec.challenges()[0]["challenge"].(string)
		id, txnHex, receiptHex, _ := stunExchange(t, advertise, packed)
		deliverSTUNResult(t, ctrl, key, id, txnHex, receiptHex)

		// Go stale (5 minutes pass; the 4-minute rechallenge timer is not
		// driven, mirroring a quiet socket with no in-flight challenge).
		clock.Advance(5*time.Minute + time.Second)

		res := make(chan awaitResult, 1)
		go func() {
			obs, ok := ctrl.AwaitFreshObservation(context.Background(), key)
			res <- awaitResult{obs, ok}
		}()
		got := waitForChallenges(t, rec, 2)
		if len(got) != 2 {
			t.Fatalf("stale observation must challenge inline exactly once, saw %d challenges", len(got))
		}
		packed2, _ := got[1]["challenge"].(string)
		id2, txnHex2, receiptHex2, _ := stunExchange(t, advertise, packed2)
		deliverSTUNResult(t, ctrl, key, id2, txnHex2, receiptHex2)
		select {
		case r := <-res:
			if !r.ok || !r.obs.AcceptedAt.After(clock.Now().Add(-time.Second)) {
				t.Fatalf("await must return the fresh inline observation, got %+v ok=%v", r.obs, r.ok)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("await did not return after the inline observation landed")
		}

		// The next preparation is warm: no challenge.
		before := len(rec.challenges())
		if _, ok := ctrl.AwaitFreshObservation(context.Background(), key); !ok {
			t.Fatalf("second await must reuse the fresh observation")
		}
		if after := len(rec.challenges()); after != before {
			t.Fatalf("warm await must not challenge: %d -> %d", before, after)
		}
	})

	t.Run("failed inline issuance fails closed without waiting", func(t *testing.T) {
		app, ctrl, clock := newSTUNEnv(t)
		advertise := attachSTUNListener(t, ctrl, clock)
		rec := newSTUNSendRecorder(t, ctrl)
		key := mustAPIKey(t, app, "key-stun-inline-fail").Id

		enrollReadyAgent(t, ctrl, key)
		packed, _ := rec.challenges()[0]["challenge"].(string)
		id, txnHex, receiptHex, _ := stunExchange(t, advertise, packed)
		deliverSTUNResult(t, ctrl, key, id, txnHex, receiptHex)
		ctrl.stunIssueFn = func(agentID string, epoch stun.Epoch) (stun.Challenge, error) {
			return stun.Challenge{}, stun.ErrRateLimited
		}
		clock.Advance(6 * time.Minute) // stale, nothing in flight
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, ok := ctrl.AwaitFreshObservation(ctx, key); ok {
			t.Fatalf("failed issuance must not report a fresh observation")
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("failed issuance must return promptly, took %v", elapsed)
		}
	})

	t.Run("cancelled parent context never challenges", func(t *testing.T) {
		app, ctrl, clock := newSTUNEnv(t)
		attachSTUNListener(t, ctrl, clock)
		rec := newSTUNSendRecorder(t, ctrl)
		key := mustAPIKey(t, app, "key-stun-inline-cancelled").Id

		enrollReadyAgent(t, ctrl, key)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, ok := ctrl.AwaitFreshObservation(ctx, key); ok {
			t.Fatalf("cancelled context must not report fresh")
		}
		if got := len(rec.challenges()); got != 1 {
			t.Fatalf("cancelled context must not challenge, saw %d", got)
		}
	})
}

// TestReconnectInvalidatesOldObservation pins §10.2's epoch binding: a
// reconnect (new epoch) invalidates the prior observation, fences the old
// epoch's timers, rejects replayed/old-epoch results, and reacquires
// immediately for the new epoch.
func TestReconnectInvalidatesOldObservation(t *testing.T) {
	app, ctrl, clock := newSTUNEnv(t)
	advertise := attachSTUNListener(t, ctrl, clock)
	rec := newSTUNSendRecorder(t, ctrl)
	key := mustAPIKey(t, app, "key-stun-reconnect").Id

	enrollReadyAgent(t, ctrl, key)
	epoch1 := currentEpochNum(t, ctrl, key)
	packed1, _ := rec.challenges()[0]["challenge"].(string)
	id1, txnHex1, receiptHex1, _ := stunExchange(t, advertise, packed1)
	// Deliberately do NOT deliver the epoch-1 result yet: an unclaimed
	// observation is what makes the post-reconnect replay a WRONG-EPOCH
	// rejection rather than a consumed-observation rejection.
	if _, ok := ctrl.CurrentSTUNObservation(key, clock.Now()); ok {
		t.Fatalf("no observation must exist before stun_result")
	}

	// Reconnect: the epoch is replaced and any state bound to the old epoch
	// is unreachable; completing the new epoch's handshake issues a new
	// immediate challenge (§10.2: after every reconnect).
	ctrl.HandleHello(context.Background(), nil, key, "acct-1", "agent-1")
	epoch2 := currentEpochNum(t, ctrl, key)
	if epoch2 == epoch1 {
		t.Fatalf("reconnect must install a new epoch")
	}
	if _, ok := ctrl.CurrentSTUNObservation(key, clock.Now()); ok {
		t.Fatalf("old-epoch observation must be invalidated by reconnect")
	}
	enrollReadyAgentReadyTLS(t, ctrl, key)
	if got := len(rec.challenges()); got != 2 {
		t.Fatalf("reconnect must challenge immediately after enrollment_ready, got %d challenges", got)
	}

	// The old epoch's timers are fenced: firing them neither challenges nor
	// resurrects state.
	ctrl.stunTimerFired(key, epoch1)
	if got := len(rec.challenges()); got != 2 {
		t.Fatalf("old-epoch timer must be fenced, got %d challenges", got)
	}

	// Delivering the epoch-1 result on the new epoch is rejected (wrong
	// epoch: current WS epoch at TakeObservation differs from the
	// observation's bound).
	deliverSTUNResult(t, ctrl, key, id1, txnHex1, receiptHex1)
	if _, ok := ctrl.CurrentSTUNObservation(key, clock.Now()); ok {
		t.Fatalf("replayed epoch-1 result must not produce a fresh observation")
	}

	// Reacquisition for the new epoch lands an observation bound to it.
	packed2, _ := rec.challenges()[1]["challenge"].(string)
	id2, txnHex2, receiptHex2, _ := stunExchange(t, advertise, packed2)
	deliverSTUNResult(t, ctrl, key, id2, txnHex2, receiptHex2)
	obs, ok := ctrl.CurrentSTUNObservation(key, clock.Now())
	if !ok || obs.Epoch != epoch2 {
		t.Fatalf("new-epoch observation missing or misbound: %+v ok=%v epoch=%d", obs, ok, epoch2)
	}

	// Replay of the just-consumed epoch-2 result is rejected (single use) and
	// leaves the accepted observation intact.
	challengesBefore := len(rec.challenges())
	deliverSTUNResult(t, ctrl, key, id2, txnHex2, receiptHex2)
	obs2, ok := ctrl.CurrentSTUNObservation(key, clock.Now())
	if !ok || obs2 != obs {
		t.Fatalf("replay must not disturb the accepted observation")
	}
	if got := len(rec.challenges()); got != challengesBefore {
		t.Fatalf("rejection must not trigger scheduling, got %d challenges", got)
	}

	// Even long after, the old epoch's timers stay fenced.
	clock.Advance(10 * time.Minute)
	ctrl.stunTimerFired(key, epoch1)
	if got := len(rec.challenges()); got != challengesBefore {
		t.Fatalf("late old-epoch timer fire must not challenge, got %d", got)
	}
}

// TestChallengeRetryBackoffIsBounded pins §10.2: challenge failures retry with
// bounded backoff (5 s doubling to a 30 s cap, plus bounded jitter), respect
// the Task 16 per-agent rate limit instead of bypassing it, and a successful
// observation resets the backoff sequence.
func TestChallengeRetryBackoffIsBounded(t *testing.T) {
	t.Run("issue failures back off 5s doubling to a 30s cap", func(t *testing.T) {
		app, ctrl, clock := newSTUNEnv(t)
		attachSTUNListener(t, ctrl, clock)
		newSTUNSendRecorder(t, ctrl)
		key := mustAPIKey(t, app, "key-stun-backoff").Id

		calls := 0
		ctrl.stunIssueFn = func(agentID string, epoch stun.Epoch) (stun.Challenge, error) {
			calls++
			return stun.Challenge{}, stun.ErrRateLimited
		}
		enrollReadyAgent(t, ctrl, key)
		if calls != 1 {
			t.Fatalf("enrollment must attempt one challenge, got %d calls", calls)
		}
		s := stunSnapshot(t, ctrl, key)
		if s.inFlight || s.attempt != 1 {
			t.Fatalf("failed issue: attempt=1 expected, got %+v", s)
		}
		wantDelays := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second, 30 * time.Second}
		for i, want := range wantDelays {
			next := s.timerAt.Sub(clock.Now())
			if next != want {
				t.Fatalf("retry %d scheduled at %v, want %v", i+1, next, want)
			}
			// Early fire is a no-op (the timer must not act before its time).
			clock.Advance(next - time.Nanosecond)
			before := calls
			ctrl.stunTimerFired(key, currentEpochNum(t, ctrl, key))
			if calls != before {
				t.Fatalf("timer fired %v early must not attempt", next)
			}
			clock.Advance(time.Nanosecond)
			ctrl.stunTimerFired(key, currentEpochNum(t, ctrl, key))
			s = stunSnapshot(t, ctrl, key)
			if s.attempt != i+2 {
				t.Fatalf("retry %d failure must bump attempt to %d, got %d", i+1, i+2, s.attempt)
			}
		}
		// The cap holds: no scheduled delay ever exceeds the cap (+ jitter).
		for i := 0; i < 5; i++ {
			s = stunSnapshot(t, ctrl, key)
			if d := s.timerAt.Sub(clock.Now()); d > stunRetryCap+stunRetryCap/4 {
				t.Fatalf("backoff exceeded the bounded cap: %v", d)
			}
			clock.Advance(s.timerAt.Sub(clock.Now()))
			ctrl.stunTimerFired(key, currentEpochNum(t, ctrl, key))
		}
	})

	t.Run("pure bounds with full jitter", func(t *testing.T) {
		if d := stunBackoffDelay(1, 1); d != 6250*time.Millisecond {
			t.Fatalf("first retry with full jitter = 5s*1.25, got %v", d)
		}
		if d := stunBackoffDelay(4, 1); d != 37500*time.Millisecond {
			t.Fatalf("capped retry with full jitter = 30s*1.25, got %v", d)
		}
		for a := 1; a <= 12; a++ {
			if d := stunBackoffDelay(a, 1); d > stunRetryCap+stunRetryCap/4 {
				t.Fatalf("backoff attempt %d unbounded: %v", a, d)
			}
		}
	})

	t.Run("expiry without a result retries with bounded backoff", func(t *testing.T) {
		app, ctrl, clock := newSTUNEnv(t)
		advertise := attachSTUNListener(t, ctrl, clock)
		rec := newSTUNSendRecorder(t, ctrl)
		key := mustAPIKey(t, app, "key-stun-expiry").Id

		enrollReadyAgent(t, ctrl, key)
		if s := stunSnapshot(t, ctrl, key); !s.inFlight {
			t.Fatalf("enrollment challenge must be in flight")
		}
		// The agent never answers; the challenge TTL elapses.
		clock.Advance(stun.ChallengeTTL)
		ctrl.stunTimerFired(key, currentEpochNum(t, ctrl, key))
		s := stunSnapshot(t, ctrl, key)
		if s.inFlight {
			t.Fatalf("expired challenge must clear the in-flight marker")
		}
		if s.attempt != 1 {
			t.Fatalf("expired challenge must count as one failure, got attempt=%d", s.attempt)
		}
		if d := s.timerAt.Sub(clock.Now()); d != 5*time.Second {
			t.Fatalf("post-expiry retry must be scheduled at the 5s base, got %v", d)
		}
		// The retry issues a fresh, usable challenge (the Task 16 token
		// bucket governs; test config has headroom).
		clock.Advance(5 * time.Second)
		ctrl.stunTimerFired(key, currentEpochNum(t, ctrl, key))
		if got := len(rec.challenges()); got != 2 {
			t.Fatalf("retry must issue one challenge, got %d total", got)
		}
		packed2, _ := rec.challenges()[1]["challenge"].(string)
		id2, txnHex2, receiptHex2, _ := stunExchange(t, advertise, packed2)
		deliverSTUNResult(t, ctrl, key, id2, txnHex2, receiptHex2)
		if s := stunSnapshot(t, ctrl, key); s.attempt != 0 || !s.obsFresh {
			t.Fatalf("acceptance must reset the backoff and freshen the observation, got %+v", s)
		}
	})

	t.Run("rechallenge failure restarts the backoff from the base", func(t *testing.T) {
		app, ctrl, clock := newSTUNEnv(t)
		advertise := attachSTUNListener(t, ctrl, clock)
		rec := newSTUNSendRecorder(t, ctrl)
		key := mustAPIKey(t, app, "key-stun-reset").Id

		enrollReadyAgent(t, ctrl, key)
		packed, _ := rec.challenges()[0]["challenge"].(string)
		id, txnHex, receiptHex, _ := stunExchange(t, advertise, packed)
		deliverSTUNResult(t, ctrl, key, id, txnHex, receiptHex)

		// Force every later issuance to fail; the 4m rechallenge then fails
		// and must restart from the 5 s base (attempt reset by acceptance).
		ctrl.stunIssueFn = func(agentID string, epoch stun.Epoch) (stun.Challenge, error) {
			return stun.Challenge{}, stun.ErrRateLimited
		}
		clock.Advance(4 * time.Minute)
		ctrl.stunTimerFired(key, currentEpochNum(t, ctrl, key))
		s := stunSnapshot(t, ctrl, key)
		delay := s.timerAt.Sub(clock.Now())
		if s.attempt != 1 || delay != 5*time.Second {
			t.Fatalf("post-acceptance failure must restart at base 5s (attempt=1), got attempt=%d delay=%v", s.attempt, delay)
		}
	})
}

// stunPackedChallengeRE is the Task 17 packed credential golden:
// "<32 lowercase-hex id>.<64 lowercase-hex secret>".
var stunPackedChallengeRE = regexp.MustCompile(`^[0-9a-f]{32}\.[0-9a-f]{64}$`)

// stunLowerHexRE matches nonempty lowercase hex.
var stunLowerHexRE = regexp.MustCompile(`^[0-9a-f]+$`)
