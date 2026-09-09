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
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
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

// ------------------------------------------------------------------
// STUN NAT gate harness (plan Task 25, spec §§10.1, 10.3, 23.5).
//
// The gate script (scripts/stun-nat-gate.sh) drives these tests as its Go
// helper (decided helper path: `go test -run`, NOT a `go run` cmd — the gate
// needs in-process access to the real Task 16 listener, the Task 18
// controller and the shared clock seam, and the agent client library is
// another module's internal package; the UDP half below sends byte-identical
// wire messages to the Task 17 client).
//
// Contract with the script (one line each, stdout):
//	STUN_GATE_CASE <name> <PASS|FAIL|SKIP> <detail>
//	STUN_GATE_EVIDENCE <name> <key>=<value>...
// Evidence values are SHA-256 hashes (receipt, transaction, challenge id) or
// the NAT-observed public mapping — never a secret (§23.5/§16.6).
//
// Case names are the gate contract:
//	owner-router-nat phone-hotspot blocked-udp spoof
//	mismatched-egress receipt-replay expired-challenge
// ------------------------------------------------------------------

// stunGateCases is the ordered gate contract shared by both modes.
var stunGateCases = []string{
	"owner-router-nat", "phone-hotspot", "blocked-udp", "spoof",
	"mismatched-egress", "receipt-replay", "expired-challenge",
}

// gateRun carries one case's outcome detail so the deferred emitter can
// print the marker even when the case stops via t.Fatalf (Goexit runs
// defers). Details are static/classification text only — never secrets.
type gateRun struct {
	t          *testing.T
	name       string
	passDetail string
	failDetail string
}

func (g *gateRun) fatalf(format string, args ...any) {
	g.failDetail = fmt.Sprintf(format, args...)
	g.t.Fatalf("%s", g.failDetail)
}

// gateEvidence prints one evidence line (hashes/public values only).
func gateEvidence(caseName string, kv ...string) {
	line := caseName
	for i := 0; i+1 < len(kv); i += 2 {
		line += " " + kv[i] + "=" + kv[i+1]
	}
	fmt.Printf("STUN_GATE_EVIDENCE %s\n", line)
}

func gateSHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// runGateCase wraps one case body: selection (STUN_GATE_CASES), marker
// emission, and detail plumbing.
func runGateCase(t *testing.T, name string, fn func(g *gateRun)) {
	t.Helper()
	if selected := os.Getenv("STUN_GATE_CASES"); selected != "" {
		pick := false
		for _, want := range strings.Split(selected, ",") {
			if strings.TrimSpace(want) == name {
				pick = true
				break
			}
		}
		if !pick {
			fmt.Printf("STUN_GATE_CASE %s SKIP not selected\n", name)
			t.Skipf("case not selected (STUN_GATE_CASES=%q)", selected)
			return
		}
	}
	g := &gateRun{t: t, name: name}
	defer func() {
		detail := g.passDetail
		result := "PASS"
		switch {
		case g.failDetail != "":
			result, detail = "FAIL", g.failDetail
		case t.Failed():
			result, detail = "FAIL", "assertion failed"
		case t.Skipped():
			// A skip emits NO marker here: Goexit still runs this defer and
			// t.Failed() is false for a skip, so without this guard a skip
			// fell through to the PASS default and the script's last-wins
			// parser counted it as PASS. The case function prints its own
			// explicit SKIP marker BEFORE skipping (see
			// gateRemoteMismatchedEgress).
			return
		case detail == "":
			detail = "ok"
		}
		detail = strings.ReplaceAll(detail, "\n", " ")
		fmt.Printf("STUN_GATE_CASE %s %s %s\n", name, result, detail)
	}()
	fn(g)
}

// ---------- local-mode UDP half (byte-identical wire protocol to the Task
// 17 client: USERNAME=<challenge id>, short-term MESSAGE-INTEGRITY, receipt
// attribute 0xFF01 in the success response) ----------

type gateUDPOutcome struct {
	accepted    bool // integrity-verified success response
	errResponse bool // known-transaction Binding error (401)
	noResponse  bool // timeout / unreachable / garbage only
	receipt     []byte
	txn         []byte
	src         netip.AddrPort // this socket's REAL local address
	mapped      netip.AddrPort // listener-reported XOR-MAPPED-ADDRESS
}

// gateUDPExchange sends exactly ONE Binding request (like the Task 17
// client) with the given credential and classifies the first
// known-transaction response. Failures are returned, never fatal.
func gateUDPExchange(advertise, challengeID string, secret []byte, timeout time.Duration) (gateUDPOutcome, error) {
	raddr, err := net.ResolveUDPAddr("udp4", advertise)
	if err != nil {
		return gateUDPOutcome{}, fmt.Errorf("resolve %q: %w", advertise, err)
	}
	conn, err := net.DialUDP("udp4", nil, raddr)
	if err != nil {
		return gateUDPOutcome{}, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	out := gateUDPOutcome{}
	if local, ok := conn.LocalAddr().(*net.UDPAddr); ok && local.IP.To4() != nil {
		var ip4 [4]byte
		copy(ip4[:], local.IP.To4())
		out.src = netip.AddrPortFrom(netip.AddrFrom4(ip4), uint16(local.Port))
	}
	m := pionstun.New()
	m.Type = pionstun.BindingRequest
	m.TransactionID = pionstun.NewTransactionID()
	m.WriteHeader()
	m.Add(pionstun.AttrUsername, []byte(challengeID))
	if err := pionstun.NewShortTermIntegrity(string(secret)).AddTo(m); err != nil {
		return out, fmt.Errorf("request integrity: %w", err)
	}
	if _, err := conn.Write(m.Raw); err != nil {
		return out, fmt.Errorf("send: %w", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return out, fmt.Errorf("deadline: %w", err)
	}
	buf := make([]byte, 1500)
	n, err := conn.Read(buf)
	if err != nil {
		out.noResponse = true
		return out, nil // blocked/unreachable/timeout: a first-class outcome
	}
	resp := pionstun.New()
	resp.Raw = append([]byte(nil), buf[:n]...)
	if err := resp.Decode(); err != nil {
		out.noResponse = true
		return out, nil
	}
	if resp.TransactionID != m.TransactionID {
		out.noResponse = true
		return out, nil
	}
	out.txn = append([]byte(nil), m.TransactionID[:]...)
	if resp.Type == pionstun.BindingError {
		out.errResponse = true
		return out, nil
	}
	if resp.Type != pionstun.BindingSuccess {
		out.noResponse = true
		return out, nil
	}
	if err := pionstun.NewShortTermIntegrity(string(secret)).Check(resp); err != nil {
		out.noResponse = true // spoofed/corrupt response never wins
		return out, nil
	}
	receipt, err := resp.Get(stun.AttrReceipt)
	if err != nil || len(receipt) == 0 {
		out.noResponse = true
		return out, nil
	}
	out.accepted = true
	out.receipt = append([]byte(nil), receipt...)
	var xor pionstun.XORMappedAddress
	if err := xor.GetFrom(resp); err == nil {
		if ip4 := xor.IP.To4(); ip4 != nil {
			var ipb [4]byte
			copy(ipb[:], ip4)
			out.mapped = netip.AddrPortFrom(netip.AddrFrom4(ipb), uint16(xor.Port))
		}
	}
	return out, nil
}

// gateRealExchange runs the full honest §10.1 UDP half against the in-process
// listener and FAILs the case unless it yields a verified receipt.
func gateRealExchange(g *gateRun, advertise, packed string) (challengeID, txnHex, receiptHex string, out gateUDPOutcome) {
	idPart, secretPart, found := strings.Cut(packed, ".")
	if !found {
		g.fatalf("packed challenge missing separator")
	}
	secret, err := hex.DecodeString(secretPart)
	if err != nil {
		g.fatalf("packed secret not hex")
	}
	out, err = gateUDPExchange(advertise, idPart, secret, 2*time.Second)
	if err != nil {
		g.fatalf("udp exchange: %v", err)
	}
	if !out.accepted {
		g.fatalf("no integrity-verified Binding success response (errResponse=%v noResponse=%v)", out.errResponse, out.noResponse)
	}
	return idPart, hex.EncodeToString(out.txn), hex.EncodeToString(out.receipt), out
}

// gateEnv is one isolated local-mode environment per case.
type gateEnv struct {
	app       core.App
	ctrl      *Controller
	clock     *stunTestClock
	rec       *stunSendRecorder
	advertise string
}

func newGateEnv(t *testing.T) *gateEnv {
	t.Helper()
	app, ctrl, clock := newSTUNEnv(t)
	advertise := attachSTUNListener(t, ctrl, clock)
	rec := newSTUNSendRecorder(t, ctrl)
	return &gateEnv{app: app, ctrl: ctrl, clock: clock, rec: rec, advertise: advertise}
}

// enrollGateAgent enrolls one fresh agent key and returns (key, packed
// credential of the immediate §10.2 enrollment challenge, epoch). The case
// may share its recorder with earlier enrollments, so the assertion is on
// the NEW challenge only.
func enrollGateAgent(g *gateRun, env *gateEnv, name string) (string, string, stun.Epoch) {
	before := len(env.rec.challenges())
	key := mustAPIKey(g.t, env.app, name).Id
	enrollReadyAgent(g.t, env.ctrl, key)
	challenges := env.rec.challenges()
	if len(challenges) != before+1 {
		g.fatalf("expected exactly one new enrollment challenge, got %d (before %d)", len(challenges)-before, before)
	}
	packed, _ := challenges[len(challenges)-1]["challenge"].(string)
	id := packedChallengeGolden(g.t, env.ctrl, challenges[len(challenges)-1], env.advertise)
	_ = id
	return key, packed, currentEpochNum(g.t, env.ctrl, key)
}

// gateProbeRecorder is a RoundTripper for ctrl.probeClient that counts HTTP
// requests; the §10.3 gate must stop BEFORE any reachability probe, so the
// count must stay zero on every refused path.
type gateProbeRecorder struct {
	mu       sync.Mutex
	requests int
}

func (r *gateProbeRecorder) RoundTrip(*http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.requests++
	r.mu.Unlock()
	return nil, fmt.Errorf("gate: reachability probe must never be reached after a §10.3 stop")
}

func (r *gateProbeRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests
}

// gateRefusedProbe installs the recording probe client, attempts the probe
// gate with the given ack surface, and asserts the §10.3 stop: a STUN-policy
// refusal with ZERO HTTP requests.
func gateRefusedProbe(g *gateRun, env *gateEnv, key string) {
	rec := &gateProbeRecorder{}
	env.ctrl.probeClient = &http.Client{Transport: rec}
	err := env.ctrl.Probe(context.Background(), "share.example.com", "abcd1234", key, OpenAck{
		ShareID: "share-1", Nonce: "nonce-1", Seq: 1,
		GrantedPort: 8443, PublicIP: "203.0.113.44", Status: "ok",
	})
	if err == nil {
		g.fatalf("Probe must be refused without a qualifying observation")
	}
	if !strings.Contains(err.Error(), "not authorized by STUN policy") {
		g.fatalf("Probe refusal must be the STUN-policy gate, got: %v", err)
	}
	if n := rec.count(); n != 0 {
		g.fatalf("§10.3 stop violated: %d reachability probe request(s) left the process", n)
	}
}

// ---------- TestSTUNGateLocalAllCasesPass ----------

// TestSTUNGateLocalAllCasesPass is the gate script's local mode: the REAL
// Task 16 listener + Task 18 controller in-process (loopback stands in for
// the NAT), every named case including all fail-closed negatives, and the
// §10.3 no-probe stop asserted on every refused path. This is the normative
// protocol proof; the real-NAT §23.5 evidence is the remote run (runbook).
func TestSTUNGateLocalAllCasesPass(t *testing.T) {
	if target := os.Getenv("STUN_GATE_TARGET"); target != "" && target != "local" {
		t.Skipf("local-mode gate not selected (STUN_GATE_TARGET=%q)", target)
	}
	for _, name := range stunGateCases {
		name := name
		t.Run(name, func(t *testing.T) {
			runGateCase(t, name, gateLocalCases[name])
		})
	}
}

var gateLocalCases = map[string]func(g *gateRun){
	"owner-router-nat":  gateLocalOwnerRouterNAT,
	"phone-hotspot":     gateLocalPhoneHotspot,
	"blocked-udp":       gateLocalBlockedUDP,
	"spoof":             gateLocalSpoof,
	"mismatched-egress": gateLocalMismatchedEgress,
	"receipt-replay":    gateLocalReceiptReplay,
	"expired-challenge": gateLocalExpiredChallenge,
}

// gateLocalOwnerRouterNAT: the full §10.1 happy path through the real stack —
// challenge over the (stubbed) authenticated epoch, real integrity-protected
// Binding, receipt bound to challenge+transaction+ACTUAL source+epoch,
// claimable exactly once, and bound to the current WS epoch. On loopback the
// observation is correctly classified non-public (§10.3 makes the class a
// routing consequence, never a listener rejection); the real-NAT equivalence
// of this case is the remote run.
func gateLocalOwnerRouterNAT(g *gateRun) {
	env := newGateEnv(g.t)
	key, packed, epoch := enrollGateAgent(g, env, "key-gate-owner")
	id, txnHex, receiptHex, out := gateRealExchange(g, env.advertise, packed)
	deliverSTUNResult(g.t, env.ctrl, key, id, txnHex, receiptHex)

	obs, ok := env.ctrl.CurrentSTUNObservation(key, env.clock.Now())
	if !ok {
		g.fatalf("observation not fresh after the accepted stun_result")
	}
	if obs.IP != out.src.Addr() {
		g.fatalf("observation IP %v must be the ACTUAL UDP source %v", obs.IP, out.src.Addr())
	}
	if obs.Epoch != epoch {
		g.fatalf("observation epoch %d must be the current epoch %d", obs.Epoch, epoch)
	}
	if obs.PublicIPv4 {
		g.fatalf("loopback observation must be classified non-public (§10.3)")
	}
	gateEvidence(g.name,
		"receipt_sha256", gateSHA256Hex(out.receipt),
		"txn_sha256", gateSHA256Hex(out.txn),
		"challenge_id_sha256", gateSHA256Hex([]byte(id)),
		"observed_source", out.src.String(),
	)
	g.passDetail = "full §10.1 flow: integrity-protected Binding accepted, receipt bound to actual source+epoch, claimable once"
}

// gateLocalPhoneHotspot: a second, fully independent agent+socket on the same
// listener (the remote run performs this behind the phone hotspot/cellular
// NAT). Proves per-agent observation isolation: observations track each
// agent's own challenge/source, and one agent's receipt can never satisfy
// another agent's stun_result.
func gateLocalPhoneHotspot(g *gateRun) {
	env := newGateEnv(g.t)
	keyA, packedA, epochA := enrollGateAgent(g, env, "key-gate-hotspot-a")
	keyB, packedB, epochB := enrollGateAgent(g, env, "key-gate-hotspot-b")
	if epochA == epochB {
		// Distinct keys get independent epochs; identical numbers would make
		// the epoch-binding assertions below vacuous.
		g.fatalf("distinct agents must hold independent epochs")
	}
	idA, txnHexA, receiptHexA, outA := gateRealExchange(g, env.advertise, packedA)
	idB, txnHexB, receiptHexB, outB := gateRealExchange(g, env.advertise, packedB)
	if outA.src == outB.src {
		g.fatalf("independent sockets must have distinct UDP sources")
	}

	// Cross-agent echo first: A's receipt can never satisfy B's stun_result.
	deliverSTUNResult(g.t, env.ctrl, keyB, idA, txnHexA, receiptHexA)
	if _, ok := env.ctrl.CurrentSTUNObservation(keyB, env.clock.Now()); ok {
		g.fatalf("cross-agent receipt must never mint an observation for the other agent")
	}
	// Honest echoes, in order.
	deliverSTUNResult(g.t, env.ctrl, keyA, idA, txnHexA, receiptHexA)
	deliverSTUNResult(g.t, env.ctrl, keyB, idB, txnHexB, receiptHexB)
	obsA, okA := env.ctrl.CurrentSTUNObservation(keyA, env.clock.Now())
	obsB, okB := env.ctrl.CurrentSTUNObservation(keyB, env.clock.Now())
	if !okA || !okB {
		g.fatalf("both agents must hold fresh observations (okA=%v okB=%v)", okA, okB)
	}
	if obsA.IP != outA.src.Addr() || obsB.IP != outB.src.Addr() {
		g.fatalf("each observation must track its own agent's UDP source")
	}
	if obsA.Epoch != epochA || obsB.Epoch != epochB {
		g.fatalf("observations must be bound to their own agent's epoch")
	}
	gateEvidence(g.name,
		"receipt_sha256_a", gateSHA256Hex(outA.receipt),
		"receipt_sha256_b", gateSHA256Hex(outB.receipt),
		"txn_sha256_a", gateSHA256Hex(outA.txn),
		"txn_sha256_b", gateSHA256Hex(outB.txn),
	)
	g.passDetail = "second NAT path: independent per-agent observations; cross-agent receipt rejected fail-closed"
}

// gateLocalBlockedUDP: the UDP path to the listener is blocked (remote: a
// firewall toggle or an unroutable target) — no receipt, no observation,
// §10.3 relay fallback (stun_timeout), the reachability probe refused with
// zero HTTP requests, and a later challenge recovers (blocked UDP never
// poisons state).
func gateLocalBlockedUDP(g *gateRun) {
	env := newGateEnv(g.t)
	key, _, _ := enrollGateAgent(g, env, "key-gate-blocked")

	// A closed loopback UDP port: no listener answers, exactly like a
	// firewall drop (ICMP refusal / read timeout are the same client-side
	// outcome class: no valid response).
	dead, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		g.fatalf("reserve dead port: %v", err)
	}
	deadPort := dead.LocalAddr().(*net.UDPAddr).Port
	if err := dead.Close(); err != nil {
		g.fatalf("close dead port: %v", err)
	}
	deadAddr := net.JoinHostPort("127.0.0.1", fmt.Sprint(deadPort))

	id, secretPart, _ := strings.Cut(env.rec.challenges()[0]["challenge"].(string), ".")
	secret, _ := hex.DecodeString(secretPart)
	out, err := gateUDPExchange(deadAddr, id, secret, 400*time.Millisecond)
	if err != nil {
		g.fatalf("blocked exchange errored unexpectedly: %v", err)
	}
	if out.accepted || out.errResponse {
		g.fatalf("a blocked UDP path must not produce a response (accepted=%v errResponse=%v)", out.accepted, out.errResponse)
	}
	if _, ok := env.ctrl.CurrentSTUNObservation(key, env.clock.Now()); ok {
		g.fatalf("no observation may exist without a delivered receipt")
	}
	match := env.ctrl.CurrentDirectMatch(key, "203.0.113.44")
	if match.Matched || match.Reason != DirectReasonSTUNTimeout {
		g.fatalf("blocked UDP must yield relay_fallback/stun_timeout, got %+v", match)
	}
	gateRefusedProbe(g, env, key)

	// Recovery: after the challenge TTL the scheduler records one failure,
	// backs off 5 s (deterministic jitter), and the retried challenge works.
	epoch := currentEpochNum(g.t, env.ctrl, key)
	env.clock.Advance(stun.ChallengeTTL)
	env.ctrl.stunTimerFired(key, epoch)
	env.clock.Advance(5 * time.Second)
	env.ctrl.stunTimerFired(key, epoch)
	challenges := env.rec.challenges()
	if len(challenges) != 2 {
		g.fatalf("recovery must issue exactly one retried challenge, got %d", len(challenges))
	}
	packed2, _ := challenges[1]["challenge"].(string)
	id2, txnHex2, receiptHex2, _ := gateRealExchange(g, env.advertise, packed2)
	deliverSTUNResult(g.t, env.ctrl, key, id2, txnHex2, receiptHex2)
	if _, ok := env.ctrl.CurrentSTUNObservation(key, env.clock.Now()); !ok {
		g.fatalf("a post-block challenge must recover a fresh observation")
	}
	g.passDetail = "UDP blocked: no response, no observation, relay_fallback/stun_timeout, probe refused with 0 HTTP requests; fresh challenge recovers"
}

// gateLocalSpoof: an off-path/on-path attacker that learned the USERNAME
// (challenge IDs are cleartext STUN attributes) sends a Binding request with
// WRONG integrity from its own socket. The listener burns the one-use
// credential, answers 401, and records NO observation for the attacker's
// source; the honest agent's subsequent exchange also 401s (credential
// burned) so no observation can be manufactured (§10.1 receipt proof).
func gateLocalSpoof(g *gateRun) {
	env := newGateEnv(g.t)
	key, packed, _ := enrollGateAgent(g, env, "key-gate-spoof")
	id, secretPart, _ := strings.Cut(packed, ".")
	realSecret, err := hex.DecodeString(secretPart)
	if err != nil {
		g.fatalf("packed secret not hex")
	}

	// Attacker: correct USERNAME, wrong integrity, own socket.
	badSecret := make([]byte, len(realSecret))
	if _, err := rand.Read(badSecret); err != nil {
		g.fatalf("attacker secret: %v", err)
	}
	atkOut, err := gateUDPExchange(env.advertise, id, badSecret, time.Second)
	if err != nil {
		g.fatalf("attacker exchange errored unexpectedly: %v", err)
	}
	if atkOut.accepted {
		g.fatalf("a wrong-integrity request must never yield a verified receipt")
	}
	if !atkOut.errResponse {
		g.fatalf("a known challenge with wrong integrity must draw a 401 Binding error")
	}

	// Honest agent: the credential is burned; fail closed.
	honestOut, err := gateUDPExchange(env.advertise, id, realSecret, time.Second)
	if err != nil {
		g.fatalf("honest exchange errored unexpectedly: %v", err)
	}
	if honestOut.accepted {
		g.fatalf("the honest exchange must fail closed after the spoof burned the credential")
	}
	if !honestOut.errResponse {
		g.fatalf("the burned credential must answer the honest request with a 401 Binding error")
	}
	if _, ok := env.ctrl.CurrentSTUNObservation(key, env.clock.Now()); ok {
		g.fatalf("no observation may exist after a spoofed-integrity burn")
	}
	match := env.ctrl.CurrentDirectMatch(key, honestOut.src.Addr().String())
	if match.Matched || match.Reason != DirectReasonSTUNTimeout {
		g.fatalf("post-spoof state must be relay_fallback/stun_timeout, got %+v", match)
	}
	gateRefusedProbe(g, env, key)
	gateEvidence(g.name,
		"attacker_source", atkOut.src.String(),
		"honest_source", honestOut.src.String(),
		"receipt_sha256", "none",
	)
	g.passDetail = "wrong-integrity request: 401 + one-use credential burned; honest exchange fails closed; no observation; probe refused with 0 HTTP requests"
}

// gateLocalMismatchedEgress: the §10.3 policy stop. A fresh observation that
// disagrees (exact IPv4) with a required surface IP is stun_mismatch; a
// private-class observation is stun_not_public; both are relay_fallback and
// both stop BEFORE the reachability probe (zero HTTP requests leave the
// process). The exact-match comparison is proven in both directions with the
// pure predicate; the real gated Probe path is proven with the real
// (loopback) observation.
func gateLocalMismatchedEgress(g *gateRun) {
	env := newGateEnv(g.t)
	key, packed, epoch := enrollGateAgent(g, env, "key-gate-mismatch")
	id, txnHex, receiptHex, out := gateRealExchange(g, env.advertise, packed)
	deliverSTUNResult(g.t, env.ctrl, key, id, txnHex, receiptHex)
	obs, ok := env.ctrl.CurrentSTUNObservation(key, env.clock.Now())
	if !ok {
		g.fatalf("mismatch case needs its fresh observation")
	}
	_ = epoch

	// Exact-IPv4 predicate, both directions, on a PUBLIC-class observation
	// (the real deployed case behind mismatched egress).
	publicObs := STUNObservation{IP: netip.MustParseAddr("203.0.113.99"), PublicIPv4: true, AcceptedAt: obs.AcceptedAt, Epoch: obs.Epoch}
	mismatch := evaluateDirectSTUNMatch(publicObs, true, "203.0.113.44")
	if mismatch.Matched || mismatch.Reason != DirectReasonSTUNMismatch || mismatch.Status != DirectStatusRelayFallback {
		g.fatalf("public observation vs different surface must be relay_fallback/stun_mismatch, got %+v", mismatch)
	}
	matched := evaluateDirectSTUNMatch(publicObs, true, "203.0.113.99")
	if !matched.Matched || matched.Reason != DirectReasonNone {
		g.fatalf("exact match must be eligible, got %+v", matched)
	}
	// IPv4-mapped and IPv6 forms never match (§10.3: the direct path is IPv4).
	if evaluateDirectSTUNMatch(publicObs, true, "::ffff:203.0.113.99").Matched {
		g.fatalf("IPv4-mapped surface text must never exact-match")
	}

	// The real gated Probe path with the actual observation: the loopback
	// source is non-public, so §10.3 refuses regardless of the surface — and
	// the refusal must precede the reachability probe entirely.
	gateRefusedProbe(g, env, key)
	gateEvidence(g.name,
		"observed_source", out.src.String(),
		"required_surface", "203.0.113.44",
		"outcome", string(DirectReasonSTUNMismatch),
	)
	g.passDetail = "exact-IPv4 mismatch -> relay_fallback/stun_mismatch (both predicate directions); real Probe refused before any HTTP request (§10.3 stop)"
}

// gateLocalReceiptReplay: observations are single-use. A replayed stun_result
// (same challenge/txn/receipt) is rejected and disturbs nothing; the same
// replay across a reconnect (new epoch) is rejected by epoch binding.
func gateLocalReceiptReplay(g *gateRun) {
	env := newGateEnv(g.t)
	key, packed, epoch := enrollGateAgent(g, env, "key-gate-replay")
	id, txnHex, receiptHex, out := gateRealExchange(g, env.advertise, packed)
	deliverSTUNResult(g.t, env.ctrl, key, id, txnHex, receiptHex)
	obs, ok := env.ctrl.CurrentSTUNObservation(key, env.clock.Now())
	if !ok {
		g.fatalf("accepted observation missing before replay")
	}

	// Listener-level double claim: the observation was consumed by the first
	// claim inside HandleSTUNResult; a second claim is ErrNoObservation.
	receiptBytes, err := hex.DecodeString(receiptHex)
	if err != nil {
		g.fatalf("receipt hex: %v", err)
	}
	if _, err := env.ctrl.stunServer.TakeObservation(key, epoch, id, txnHex, receiptBytes); err == nil {
		g.fatalf("second claim of the same observation must be rejected")
	}

	// Controller-level replay: same stun_result delivered again is rejected;
	// the accepted observation is undisturbed and no scheduling fires.
	challengesBefore := len(env.rec.challenges())
	deliverSTUNResult(g.t, env.ctrl, key, id, txnHex, receiptHex)
	obs2, ok := env.ctrl.CurrentSTUNObservation(key, env.clock.Now())
	if !ok || obs2 != obs {
		g.fatalf("replay must not disturb the accepted observation")
	}
	if got := len(env.rec.challenges()); got != challengesBefore {
		g.fatalf("replay must not trigger scheduling: %d -> %d challenges", challengesBefore, got)
	}

	// Replay across a reconnect: the captured stun_result is bound to the old
	// epoch and must fail closed on the new one.
	env.ctrl.HandleHello(context.Background(), nil, key, "acct-1", "agent-1")
	enrollReadyAgentReadyTLS(g.t, env.ctrl, key)
	newEpoch := currentEpochNum(g.t, env.ctrl, key)
	if newEpoch == epoch {
		g.fatalf("reconnect must install a new epoch")
	}
	deliverSTUNResult(g.t, env.ctrl, key, id, txnHex, receiptHex)
	if _, ok := env.ctrl.CurrentSTUNObservation(key, env.clock.Now()); ok {
		g.fatalf("old-epoch replayed result must never mint a new-epoch observation")
	}
	gateEvidence(g.name,
		"receipt_sha256", gateSHA256Hex(out.receipt),
		"txn_sha256", gateSHA256Hex(out.txn),
		"replay_outcome", "rejected_single_use_and_epoch_bound",
	)
	g.passDetail = "replayed stun_result rejected (single-use); cross-epoch replay rejected; accepted observation undisturbed"
}

// gateLocalExpiredChallenge: a Binding request sent after the 60 s challenge
// TTL draws a 401 and burns the credential — even with CORRECT integrity
// (listener-side expiry defense; the Task 17 client additionally refuses
// client-side). No observation, §10.3 relay fallback, probe refused, and the
// scheduler recovers with a fresh challenge after bounded backoff.
func gateLocalExpiredChallenge(g *gateRun) {
	env := newGateEnv(g.t)
	key, packed, epoch := enrollGateAgent(g, env, "key-gate-expired")
	id, secretPart, _ := strings.Cut(packed, ".")
	secret, err := hex.DecodeString(secretPart)
	if err != nil {
		g.fatalf("packed secret not hex")
	}

	env.clock.Advance(stun.ChallengeTTL + time.Millisecond)
	lateOut, err := gateUDPExchange(env.advertise, id, secret, time.Second)
	if err != nil {
		g.fatalf("late exchange errored unexpectedly: %v", err)
	}
	if lateOut.accepted {
		g.fatalf("a post-TTL request must never yield a verified receipt")
	}
	if !lateOut.errResponse {
		g.fatalf("an expired-but-known challenge must draw a 401 Binding error")
	}
	if _, ok := env.ctrl.CurrentSTUNObservation(key, env.clock.Now()); ok {
		g.fatalf("no observation may exist from an expired challenge")
	}
	match := env.ctrl.CurrentDirectMatch(key, "203.0.113.44")
	if match.Matched || match.Reason != DirectReasonSTUNTimeout {
		g.fatalf("expired challenge must yield relay_fallback/stun_timeout, got %+v", match)
	}
	gateRefusedProbe(g, env, key)

	// Scheduler recovery: expiry counts as one failure, bounded backoff, and
	// the retried challenge exchanges normally.
	env.ctrl.stunTimerFired(key, epoch)
	env.clock.Advance(5 * time.Second)
	env.ctrl.stunTimerFired(key, epoch)
	challenges := env.rec.challenges()
	if len(challenges) != 2 {
		g.fatalf("recovery must issue exactly one retried challenge, got %d", len(challenges))
	}
	packed2, _ := challenges[1]["challenge"].(string)
	id2, txnHex2, receiptHex2, _ := gateRealExchange(g, env.advertise, packed2)
	deliverSTUNResult(g.t, env.ctrl, key, id2, txnHex2, receiptHex2)
	if _, ok := env.ctrl.CurrentSTUNObservation(key, env.clock.Now()); !ok {
		g.fatalf("the fresh post-expiry challenge must recover a claimable observation")
	}
	gateEvidence(g.name,
		"late_request_outcome", "401_binding_error",
		"receipt_sha256", "none",
	)
	g.passDetail = "Binding after 60s TTL: 401, credential burned, no observation, probe refused with 0 HTTP requests; bounded-backoff retry recovers"
}

// ---------- remote mode (deployed control; user-assisted §23.5 runs) ----------

// gateWSMessage is one control -> agent WebSocket message, decoded tolerantly
// (unknown fields and types are recorded, never fatal).
type gateWSMessage struct {
	Type      string `json:"type"`
	Challenge string `json:"challenge"`
	Server    string `json:"server"`
	ExpiresAt string `json:"expires_at"`
	Reason    string `json:"reason"`
	Message   string `json:"message"`
}

// gateChallenge is one received §11.1 stun_challenge.
type gateChallenge struct {
	packed    string // "<hex id>.<hex secret>" — never printed
	server    string // control-advertised UDP listener host:port
	expiresAt time.Time
}

// gateRemoteAgent is the minimal test-agent WS half for the deployed-control
// runs: api_key query auth (the same wire auth the shipped client uses),
// hello, enrollment (tls_ready fast path with the persisted fingerprint, or
// the shipped csr_submit -> cert_issue -> tls_ready flow), then the
// stun_challenge / stun_result exchange. It reads in a single pump goroutine.
type gateRemoteAgent struct {
	conn       *websocket.Conn
	ctx        context.Context
	cancel     context.CancelFunc
	events     chan gateWSMessage
	mu         sync.Mutex
	lastType   string
	lastError  string
	pumpFailed bool
}

type gateRemoteConfig struct {
	server     string // ws(s)://control-host (no path)
	apiKey     string // env-only; never printed
	stunAddr   string // optional UDP listener override
	expectedIP string // mismatched-egress required surface
	certFP     string // optional persisted cert fingerprint (skips CSR issuance)
}

// gateScrub removes credential material from error text (defense in depth:
// nothing in the driver prints the key, even inside library errors).
func gateScrub(s, apiKey string) string {
	if apiKey != "" {
		s = strings.ReplaceAll(s, apiKey, "[scrubbed]")
	}
	return s
}

func gateRemoteDial(g *gateRun, cfg gateRemoteConfig, agentID string) *gateRemoteAgent {
	g.t.Helper()
	wsURL := strings.TrimSuffix(cfg.server, "/") + "/ws/agent?api_key=" + url.QueryEscape(cfg.apiKey)
	ctx, cancel := context.WithCancel(context.Background())
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		cancel()
		g.fatalf("dial deployed control: %s", gateScrub(err.Error(), cfg.apiKey))
	}
	a := &gateRemoteAgent{conn: conn, ctx: ctx, cancel: cancel, events: make(chan gateWSMessage, 64)}
	g.t.Cleanup(func() { cancel(); _ = conn.CloseNow() })
	go a.pump()
	if err := a.send(map[string]string{"type": "hello", "agent_id": agentID}); err != nil {
		g.fatalf("send hello: %v", err)
	}
	return a
}

// pump reads every control message until the connection dies; stun_challenge
// payloads are surfaced via the events channel.
func (a *gateRemoteAgent) pump() {
	defer close(a.events)
	for {
		typ, data, err := a.conn.Read(a.ctx)
		if err != nil {
			a.mu.Lock()
			a.pumpFailed = true
			a.mu.Unlock()
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var msg gateWSMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		a.mu.Lock()
		a.lastType = msg.Type
		if msg.Type == "error" {
			a.lastError = msg.Message
		}
		a.mu.Unlock()
		select {
		case a.events <- msg:
		default: // bounded: surplus messages (e.g. expired retry challenges) drop
		}
	}
}

func (a *gateRemoteAgent) send(msg map[string]string) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return a.conn.Write(a.ctx, websocket.MessageText, data)
}

// waitEvent awaits the next message of one of the given types.
func (a *gateRemoteAgent) waitEvent(timeout time.Duration, types ...string) (gateWSMessage, error) {
	deadline := time.After(timeout)
	for {
		select {
		case msg, ok := <-a.events:
			if !ok {
				return gateWSMessage{}, fmt.Errorf("control connection closed (last type %q, last error %q)", a.snapshot().lastType, a.snapshot().lastError)
			}
			for _, want := range types {
				if msg.Type == want {
					return msg, nil
				}
			}
		case <-deadline:
			s := a.snapshot()
			return gateWSMessage{}, fmt.Errorf("timeout after %s waiting for %v (last type %q, last error %q)", timeout, types, s.lastType, s.lastError)
		}
	}
}

func (a *gateRemoteAgent) snapshot() (s struct{ lastType, lastError string }) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s.lastType, s.lastError = a.lastType, a.lastError
	return s
}

// gateRemoteEnroll completes the enrollment handshake for the CURRENT epoch:
// either the persisted-fingerprint fast path (no issuance on the deployed
// control) or the shipped csr_submit -> cert_issue -> tls_ready flow.
func gateRemoteEnroll(g *gateRun, a *gateRemoteAgent, cfg gateRemoteConfig, agentID string) {
	g.t.Helper()
	if cfg.certFP != "" {
		if err := a.send(map[string]string{"type": "tls_ready", "fingerprint": cfg.certFP, "not_after": ""}); err != nil {
			g.fatalf("send tls_ready: %v", err)
		}
		return
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		g.fatalf("csr key generation: %v", err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: agentID},
	}, key)
	if err != nil {
		g.fatalf("csr creation: %v", err)
	}
	csrPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}))
	if err := a.send(map[string]string{"type": "csr_submit", "csr_pem": csrPEM}); err != nil {
		g.fatalf("send csr_submit: %v", err)
	}
	msg, err := a.waitEvent(30*time.Second, "cert_issue", "cert_error")
	if err != nil {
		g.fatalf("waiting for cert issuance: %v", err)
	}
	if msg.Type == "cert_error" {
		g.fatalf("deployed control refused issuance (reason %q); set STUN_GATE_CERT_FINGERPRINT to the enrolled agent's cert fingerprint to skip issuance", msg.Reason)
	}
	block, _ := pem.Decode([]byte(msg.Message))
	if block == nil || block.Type != "CERTIFICATE" {
		g.fatalf("cert_issue carried no leaf certificate")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		g.fatalf("parse issued leaf: %v", err)
	}
	fp := gateSHA256Hex(block.Bytes)
	na := leaf.NotAfter.Format(time.RFC3339)
	if err := a.send(map[string]string{"type": "tls_ready", "fingerprint": fp, "not_after": na}); err != nil {
		g.fatalf("send tls_ready: %v", err)
	}
}

// gateRemoteChallenge enrolls and waits for the §10.2 immediate challenge.
func gateRemoteChallenge(g *gateRun, cfg gateRemoteConfig, agentID string) (*gateRemoteAgent, gateChallenge) {
	g.t.Helper()
	a := gateRemoteDial(g, cfg, agentID)
	gateRemoteEnroll(g, a, cfg, agentID)
	msg, err := a.waitEvent(20*time.Second, "stun_challenge", "cert_error")
	if err != nil {
		g.fatalf("waiting for the post-enrollment stun_challenge: %v", err)
	}
	if msg.Type == "cert_error" {
		g.fatalf("deployed control sent %q (reason %q); check STUN_GATE_CERT_FINGERPRINT", msg.Type, msg.Reason)
	}
	if msg.Challenge == "" || msg.Server == "" {
		g.fatalf("stun_challenge missing challenge/server fields")
	}
	exp, err := time.Parse(time.RFC3339, msg.ExpiresAt)
	if err != nil {
		g.fatalf("stun_challenge expires_at not RFC3339")
	}
	return a, gateChallenge{packed: msg.Challenge, server: msg.Server, expiresAt: exp}
}

// gateRemoteExchange splits the packed credential and runs the honest UDP
// half against the challenge's advertised server (or the override), exactly
// like the Task 17 client.
func gateRemoteExchange(g *gateRun, ch gateChallenge, stunAddr string, timeout time.Duration) (id, txnHex, receiptHex string, out gateUDPOutcome) {
	g.t.Helper()
	idPart, secretPart, found := strings.Cut(ch.packed, ".")
	if !found {
		g.fatalf("packed challenge malformed")
	}
	secret, err := hex.DecodeString(secretPart)
	if err != nil {
		g.fatalf("packed secret not hex")
	}
	addr := stunAddr
	if addr == "" {
		addr = ch.server
	}
	out, err = gateUDPExchange(addr, idPart, secret, timeout)
	if err != nil {
		g.fatalf("udp exchange: %v", err)
	}
	if !out.accepted {
		g.fatalf("no integrity-verified receipt through the NAT path (errResponse=%v noResponse=%v)", out.errResponse, out.noResponse)
	}
	return idPart, hex.EncodeToString(out.txn), hex.EncodeToString(out.receipt), out
}

func gateRemoteEchoResult(a *gateRemoteAgent, id, txnHex, receiptHex string) error {
	return a.send(map[string]string{
		"type": "stun_result", "challenge": id, "transaction_id": txnHex, "receipt": receiptHex,
	})
}

// gateWaitRechallenge reports whether the wire-visible acceptance proof (§10.2
// rechallenge at +4m–4m15s, vs a backoff retry at ~+65s on rejection) is on
// for the case. Default: the two happy-path cases and receipt-replay.
func gateWaitRechallenge(name string) bool {
	switch v := os.Getenv("STUN_GATE_WAIT_RECHALLENGE"); v {
	case "":
		return name == "owner-router-nat" || name == "phone-hotspot" || name == "receipt-replay"
	case "0", "false":
		return false
	default:
		return true
	}
}

// gateRemoteProveAcceptance waits for the next stun_challenge after the echo
// and classifies it: >=4m is the §10.2 rechallenge (only armed by a successful
// HandleSTUNResult acceptance), <4m is a bounded-backoff retry (the result was
// rejected). This is the wire-visible proof that the exact observed source
// reached control.
func gateRemoteProveAcceptance(g *gateRun, a *gateRemoteAgent, echoedAt time.Time) time.Duration {
	g.t.Helper()
	msg, err := a.waitEvent(4*time.Minute+40*time.Second, "stun_challenge")
	if err != nil {
		g.fatalf("no follow-up challenge after the echo (no rechallenge means control never accepted the observation): %v", err)
	}
	_ = msg
	elapsed := time.Since(echoedAt)
	if elapsed < 3*time.Minute+59*time.Second {
		g.fatalf("control scheduled a backoff retry at +%s — the stun_result was NOT accepted", elapsed.Truncate(time.Second))
	}
	return elapsed
}

// Regression (fix round): a skip raised inside fn AFTER the deferred marker
// emitter is installed must emit NO marker. Goexit still runs the defer and
// t.Failed() is false for a skip, so the emitter used to fall through to the
// PASS default — the script's last-wins parser then counted skipped remote
// cases (e.g. mismatched-egress without STUN_GATE_EXPECTED_PUBLIC_IP) as
// PASS, reporting a false full-matrix §23.5 result. A SKIP marker is the
// case function's own responsibility (see gateRemoteMismatchedEgress).
func TestSTUNGateSkipInsideFnEmitsNoPassMarker(t *testing.T) {
	t.Setenv("STUN_GATE_CASES", "")
	t.Setenv("STUN_GATE_TARGET", "")

	stdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	os.Stdout = w
	captured := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		captured <- buf.String()
	}()

	t.Run("synthetic-skip-probe", func(t *testing.T) {
		runGateCase(t, "skip-probe", func(g *gateRun) {
			g.t.Skipf("synthetic skip after the marker emitter is installed")
		})
	})

	os.Stdout = stdout
	_ = w.Close()
	out := <-captured
	_ = r.Close()
	for _, marker := range []string{"STUN_GATE_CASE skip-probe PASS", "STUN_GATE_CASE skip-probe FAIL"} {
		if strings.Contains(out, marker) {
			t.Fatalf("skip inside fn emitted a marker (%s) — a skip must never emit a PASS:\n%s", marker, out)
		}
	}
}

// TestSTUNGateRemoteCases is the gate script's remote mode: a minimal
// test-agent WS client drives the DEPLOYED control through the same seven
// cases from behind the NAT under test. Agent-side verifications only — the
// control-side accept/reject semantics are normatively asserted by
// TestSTUNGateLocalAllCasesPass against the same listener code.
func TestSTUNGateRemoteCases(t *testing.T) {
	if target := os.Getenv("STUN_GATE_TARGET"); target != "remote" {
		t.Skipf("remote-mode gate not selected (STUN_GATE_TARGET=%q)", target)
	}
	server, apiKey := os.Getenv("STUN_GATE_SERVER"), os.Getenv("STUN_GATE_API_KEY")
	if server == "" || apiKey == "" {
		t.Skip("remote NAT-gate cases require STUN_GATE_SERVER and STUN_GATE_API_KEY")
	}
	cfg := gateRemoteConfig{
		server:     server,
		apiKey:     apiKey,
		stunAddr:   os.Getenv("STUN_GATE_STUN_ADDR"),
		expectedIP: os.Getenv("STUN_GATE_EXPECTED_PUBLIC_IP"),
		certFP:     os.Getenv("STUN_GATE_CERT_FINGERPRINT"),
	}
	for _, name := range stunGateCases {
		name := name
		t.Run(name, func(t *testing.T) {
			runGateCase(t, name, func(g *gateRun) { gateRemoteCase(g, cfg, name) })
		})
	}
}

func gateRemoteCase(g *gateRun, cfg gateRemoteConfig, name string) {
	switch name {
	case "owner-router-nat", "phone-hotspot":
		gateRemoteHappyPath(g, cfg, name)
	case "blocked-udp":
		gateRemoteBlockedUDP(g, cfg)
	case "spoof":
		gateRemoteSpoof(g, cfg)
	case "mismatched-egress":
		gateRemoteMismatchedEgress(g, cfg)
	case "receipt-replay":
		gateRemoteReceiptReplay(g, cfg)
	case "expired-challenge":
		gateRemoteExpiredChallenge(g, cfg)
	default:
		g.fatalf("unknown remote case %q", name)
	}
}

// gateRemoteHappyPath: the real §23.5 happy path behind the NAT under test.
func gateRemoteHappyPath(g *gateRun, cfg gateRemoteConfig, name string) {
	a, ch := gateRemoteChallenge(g, cfg, "stun-gate-"+name)
	id, txnHex, receiptHex, out := gateRemoteExchange(g, ch, cfg.stunAddr, 5*time.Second)
	if err := gateRemoteEchoResult(a, id, txnHex, receiptHex); err != nil {
		g.fatalf("echo stun_result: %v", err)
	}
	gateEvidence(name,
		"receipt_sha256", gateSHA256Hex(out.receipt),
		"txn_sha256", gateSHA256Hex(out.txn),
		"challenge_id_sha256", gateSHA256Hex([]byte(id)),
		"observed_source", out.mapped.String(),
		"listener", ch.server,
	)
	detail := "real challenge over the deployed control WS; integrity-verified receipt through the NAT path; stun_result echoed"
	if gateWaitRechallenge(name) {
		elapsed := gateRemoteProveAcceptance(g, a, time.Now())
		detail += fmt.Sprintf("; §10.2 rechallenge at +%s proves control accepted the observation", elapsed.Truncate(time.Second))
	}
	g.passDetail = detail
}

// gateRemoteBlockedUDP: with UDP to the listener blocked, the exchange must
// fail closed: no response at all (an error response would mean the datagram
// REACHED the listener, i.e. UDP is not blocked or the challenge was burned
// by another flow).
func gateRemoteBlockedUDP(g *gateRun, cfg gateRemoteConfig) {
	_, ch := gateRemoteChallenge(g, cfg, "stun-gate-blocked")
	idPart, secretPart, _ := strings.Cut(ch.packed, ".")
	secret, err := hex.DecodeString(secretPart)
	if err != nil {
		g.fatalf("packed secret not hex")
	}
	addr := cfg.stunAddr
	if addr == "" {
		addr = ch.server
	}
	out, err := gateUDPExchange(addr, idPart, secret, 4*time.Second)
	if err != nil {
		g.fatalf("blocked exchange errored unexpectedly: %v", err)
	}
	if out.accepted {
		g.fatalf("UDP is NOT blocked: a verified receipt came back (check the firewall toggle / target address)")
	}
	if out.errResponse {
		g.fatalf("unexpected 401 on the blocked path — the challenge was likely consumed by another flow; rerun this case")
	}
	// Fail closed, wire-side: nothing is echoed, so control can only expire
	// the challenge; no observation can exist. The WS stays healthy.
	gateEvidence(g.name, "receipt_sha256", "none", "listener", ch.server)
	g.passDetail = "UDP blocked: no response, nothing echoed, no observation possible, fail closed"
}

// gateRemoteSpoof: wrong-integrity request from a second socket burns the
// one-use credential at the deployed listener; the honest exchange then fails
// closed with 401 (normative listener semantics proven in the local run).
func gateRemoteSpoof(g *gateRun, cfg gateRemoteConfig) {
	_, ch := gateRemoteChallenge(g, cfg, "stun-gate-spoof")
	id, secretPart, _ := strings.Cut(ch.packed, ".")
	realSecret, err := hex.DecodeString(secretPart)
	if err != nil {
		g.fatalf("packed secret not hex")
	}
	badSecret := make([]byte, len(realSecret))
	if _, err := rand.Read(badSecret); err != nil {
		g.fatalf("attacker secret: %v", err)
	}
	addr := cfg.stunAddr
	if addr == "" {
		addr = ch.server
	}
	atkOut, err := gateUDPExchange(addr, id, badSecret, 4*time.Second)
	if err != nil {
		g.fatalf("attacker exchange errored unexpectedly: %v", err)
	}
	if atkOut.accepted {
		g.fatalf("a wrong-integrity request must never yield a verified receipt")
	}
	honestOut, err := gateUDPExchange(addr, id, realSecret, 4*time.Second)
	if err != nil {
		g.fatalf("honest exchange errored unexpectedly: %v", err)
	}
	if honestOut.accepted {
		g.fatalf("the honest exchange must fail closed after the spoof burned the credential")
	}
	if !honestOut.errResponse {
		g.fatalf("the burned credential must answer the honest request with a 401 Binding error (got noResponse)")
	}
	// Nothing is echoed: no observation can exist from this exchange.
	gateEvidence(g.name,
		"attacker_source", atkOut.src.String(),
		"honest_source", honestOut.src.String(),
		"receipt_sha256", "none",
		"listener", ch.server,
	)
	g.passDetail = "wrong-integrity request through the real path: 401 + credential burned; honest exchange failed closed; nothing echoed"
}

// gateRemoteMismatchedEgress: with a VPN or split tunnel changing the UDP
// egress, the successful receipt carries a mapped address that differs from
// the required surface IP — exactly the §10.3 comparison that must fail and
// stop before the public probe (normatively asserted by the local run).
func gateRemoteMismatchedEgress(g *gateRun, cfg gateRemoteConfig) {
	if cfg.expectedIP == "" {
		fmt.Printf("STUN_GATE_CASE mismatched-egress SKIP set STUN_GATE_EXPECTED_PUBLIC_IP to this network's expected egress IP\n")
		g.t.Skipf("STUN_GATE_EXPECTED_PUBLIC_IP not set")
		return
	}
	a, ch := gateRemoteChallenge(g, cfg, "stun-gate-mismatch")
	id, txnHex, receiptHex, out := gateRemoteExchange(g, ch, cfg.stunAddr, 5*time.Second)
	if err := gateRemoteEchoResult(a, id, txnHex, receiptHex); err != nil {
		g.fatalf("echo stun_result: %v", err)
	}
	gateEvidence(g.name,
		"receipt_sha256", gateSHA256Hex(out.receipt),
		"txn_sha256", gateSHA256Hex(out.txn),
		"observed_source", out.mapped.String(),
		"required_surface", cfg.expectedIP,
	)
	if out.mapped.Addr().String() == strings.TrimSpace(cfg.expectedIP) {
		g.fatalf("no egress mismatch observed (observed %s equals the required surface); check that the VPN/route under test is actually active", out.mapped.Addr())
	}
	g.passDetail = "mismatched egress observable: mapped source differs from the required surface; §10.3 exact-match fails and stops before the public probe"
}

// gateRemoteReceiptReplay: the same stun_result is echoed twice on the real
// WS. Control must reject the replay silently (single-use) and stay healthy;
// the rechallenge wait proves acceptance and that the replay disturbed
// nothing.
func gateRemoteReceiptReplay(g *gateRun, cfg gateRemoteConfig) {
	a, ch := gateRemoteChallenge(g, cfg, "stun-gate-replay")
	id, txnHex, receiptHex, out := gateRemoteExchange(g, ch, cfg.stunAddr, 5*time.Second)
	if err := gateRemoteEchoResult(a, id, txnHex, receiptHex); err != nil {
		g.fatalf("echo stun_result: %v", err)
	}
	echoedAt := time.Now()
	if err := gateRemoteEchoResult(a, id, txnHex, receiptHex); err != nil {
		g.fatalf("replayed stun_result broke the WS: %v", err)
	}
	gateEvidence(g.name,
		"receipt_sha256", gateSHA256Hex(out.receipt),
		"txn_sha256", gateSHA256Hex(out.txn),
		"replay_outcome", "duplicate_sent_connection_healthy",
	)
	detail := "duplicate stun_result sent on the real WS; connection stayed healthy; control-side single-use rejection normatively asserted by the local run"
	if gateWaitRechallenge("receipt-replay") {
		elapsed := gateRemoteProveAcceptance(g, a, echoedAt)
		detail += fmt.Sprintf("; §10.2 rechallenge at +%s proves acceptance and intact scheduling after the replay", elapsed.Truncate(time.Second))
	}
	g.passDetail = detail
}

// gateRemoteExpiredChallenge: the challenge is held past its 60 s TTL and the
// real Binding is then sent with CORRECT integrity (deliberately bypassing
// the Task 17 client-side expiry check) to prove the deployed listener's own
// expiry defense: 401, no receipt.
func gateRemoteExpiredChallenge(g *gateRun, cfg gateRemoteConfig) {
	_, ch := gateRemoteChallenge(g, cfg, "stun-gate-expired")
	time.Sleep(stun.ChallengeTTL + 2*time.Second)
	id, secretPart, _ := strings.Cut(ch.packed, ".")
	secret, err := hex.DecodeString(secretPart)
	if err != nil {
		g.fatalf("packed secret not hex")
	}
	addr := cfg.stunAddr
	if addr == "" {
		addr = ch.server
	}
	out, err := gateUDPExchange(addr, id, secret, 4*time.Second)
	if err != nil {
		g.fatalf("late exchange errored unexpectedly: %v", err)
	}
	if out.accepted {
		g.fatalf("a post-TTL request must never yield a verified receipt")
	}
	if !out.errResponse {
		g.fatalf("an expired-but-known challenge must draw a 401 Binding error (got silence)")
	}
	gateEvidence(g.name,
		"late_request_outcome", "401_binding_error",
		"receipt_sha256", "none",
		"listener", ch.server,
	)
	g.passDetail = "listener-side expiry: Binding after 60s TTL answered 401, credential burned, no receipt"
}
