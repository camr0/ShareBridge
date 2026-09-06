package stun

// Tests for the Task 16 authenticated STUN observation listener (plan Task 16,
// spec §§10.1, 10.3, 16.4): control-observed UDP flow with one-use short-term
// credentials and STUN MESSAGE-INTEGRITY.
//
// Security property under test (§16.4): only a caller that received the
// one-use secret over its authenticated WebSocket epoch can obtain a receipt,
// and the receipt is bound to the challenge, transaction ID, actual UDP source
// address and epoch — so a blind source-spoofed UDP packet cannot manufacture
// an observation. All sockets in these tests are real UDP sockets.

import (
	"context"
	"encoding/hex"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/stun/v3"
)

// ---------- test infrastructure ----------

// fakeClock is a controllable clock seam. It is goroutine-safe because the
// Serve goroutine reads it while the test advances it.
type fakeClock struct {
	nanos atomic.Int64
}

func newFakeClock() *fakeClock {
	c := &fakeClock{}
	c.nanos.Store(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC).UnixNano())
	return c
}

func (c *fakeClock) Now() time.Time { return time.Unix(0, c.nanos.Load()) }

func (c *fakeClock) Advance(d time.Duration) { c.nanos.Add(int64(d)) }

// newTestServer builds a Server bound to an ephemeral loopback port and
// serving on a real UDP socket. Extra cfg overrides are applied by the caller.
func newTestServer(t *testing.T, mutate func(*Config)) (*Server, *net.UDPConn, *fakeClock) {
	t.Helper()
	clock := newFakeClock()
	cfg := Config{
		Key:      bytesOf(0x37, 32),
		BindAddr: "127.0.0.1:0",
		Now:      clock.Now,
		// Headroom for multi-challenge test flows; the production defaults
		// themselves are asserted by TestSTUNRateLimitsPerAgent.
		MaxChallengesPerAgentPerMinute: 64,
		MaxPendingPerAgent:             8,
		MaxAgents:                      64,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	conn, err := srv.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx, conn) }()
	t.Cleanup(func() {
		cancel()
		_ = conn.Close()
	})
	return srv, conn, clock
}

// testClient is one UDP endpoint (one distinct source address per instance).
type testClient struct {
	conn *net.UDPConn
	addr netip.AddrPort
}

func newTestClient(t *testing.T, server *net.UDPConn) *testClient {
	t.Helper()
	conn, err := net.DialUDP("udp", nil, server.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("DialUDP: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	local := conn.LocalAddr().(*net.UDPAddr)
	ip, ok := netip.AddrFromSlice(local.IP.To4())
	if !ok {
		t.Fatalf("client local address is not IPv4: %v", local)
	}
	return &testClient{conn: conn, addr: netip.AddrPortFrom(ip, uint16(local.Port))}
}

func txn() [stun.TransactionIDSize]byte { return stun.NewTransactionID() }

// sendBinding builds and sends a Binding request with USERNAME=challengeID and
// MESSAGE-INTEGRITY keyed with secret. secret == nil omits integrity.
func (c *testClient) sendBinding(t *testing.T, challengeID string, secret []byte, id [stun.TransactionIDSize]byte) {
	t.Helper()
	m := stun.New()
	m.Type = stun.BindingRequest
	m.TransactionID = id
	m.WriteHeader()
	m.Add(stun.AttrUsername, []byte(challengeID))
	if secret != nil {
		if err := stun.NewShortTermIntegrity(string(secret)).AddTo(m); err != nil {
			t.Fatalf("integrity AddTo: %v", err)
		}
	}
	c.sendRaw(t, m.Raw)
}

func (c *testClient) sendRaw(t *testing.T, b []byte) {
	t.Helper()
	if _, err := c.conn.Write(b); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

// readMsg reads one datagram and decodes it as a STUN message.
func (c *testClient) readMsg(t *testing.T, timeout time.Duration) *stun.Message {
	t.Helper()
	buf := make([]byte, 1500)
	if err := c.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	n, err := c.conn.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	m := stun.New()
	m.Raw = append([]byte(nil), buf[:n]...)
	if err := m.Decode(); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return m
}

// readSilent asserts that no datagram arrives within the timeout.
func (c *testClient) readSilent(t *testing.T, timeout time.Duration) {
	t.Helper()
	buf := make([]byte, 1500)
	if err := c.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	_, err := c.conn.Read(buf)
	if err == nil {
		t.Fatalf("expected no response, got one")
	}
	if !isTimeout(err) {
		t.Fatalf("expected read timeout, got %v", err)
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// expectBindingError reads one response and requires a Binding error response
// with the given transaction ID and STUN error code.
func expectBindingError(t *testing.T, c *testClient, id [stun.TransactionIDSize]byte, wantCode stun.ErrorCode) {
	t.Helper()
	m := c.readMsg(t, 2*time.Second)
	if m.Type != stun.BindingError {
		t.Fatalf("expected BindingError response, got %v", m.Type)
	}
	if m.TransactionID != id {
		t.Fatalf("error response transaction mismatch: got %x want %x", m.TransactionID, id)
	}
	var ec stun.ErrorCodeAttribute
	if err := ec.GetFrom(m); err != nil {
		t.Fatalf("error response missing ERROR-CODE: %v", err)
	}
	if ec.Code != wantCode {
		t.Fatalf("error code: got %d want %d", ec.Code, wantCode)
	}
}

func bytesOf(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func issueOK(t *testing.T, srv *Server, agentID string, epoch Epoch) Challenge {
	t.Helper()
	ch, err := srv.IssueChallenge(agentID, epoch)
	if err != nil {
		t.Fatalf("IssueChallenge(%s): %v", agentID, err)
	}
	return ch
}

// ---------- named tests ----------

// TestSTUNRecordsUDPSourceAndReturnsIntegrityProtectedReceipt covers §10.1
// steps 2–5: a valid one-use credential Binding request records the actual UDP
// source and returns the mapped address plus an unpredictable receipt inside
// the MESSAGE-INTEGRITY-protected response; the observation is claimable
// exactly once and only with matching epoch, transaction and receipt.
func TestSTUNRecordsUDPSourceAndReturnsIntegrityProtectedReceipt(t *testing.T) {
	srv, srvConn, _ := newTestServer(t, nil)
	client := newTestClient(t, srvConn)

	ch := issueOK(t, srv, "agent-1", Epoch(7))
	if len(ch.Secret) != 32 {
		t.Fatalf("one-use secret length: got %d want 32", len(ch.Secret))
	}

	id := txn()
	client.sendBinding(t, ch.ID, ch.Secret, id)

	resp := client.readMsg(t, 2*time.Second)
	if resp.Type != stun.BindingSuccess {
		t.Fatalf("expected success response, got %v", resp.Type)
	}
	if resp.TransactionID != id {
		t.Fatalf("transaction ID mismatch in response")
	}
	// The response must be integrity protected with the one-use secret.
	if err := stun.NewShortTermIntegrity(string(ch.Secret)).Check(resp); err != nil {
		t.Fatalf("response MESSAGE-INTEGRITY check failed: %v", err)
	}
	// The normal mapped address (§10.1 step 3).
	var xor stun.XORMappedAddress
	if err := xor.GetFrom(resp); err != nil {
		t.Fatalf("XOR-MAPPED-ADDRESS missing: %v", err)
	}
	if xor.Port != int(client.addr.Port()) || xor.IP.String() != client.addr.Addr().String() {
		t.Fatalf("mapped address: got %s:%d want %s", xor.IP, xor.Port, client.addr)
	}
	var mapped stun.MappedAddress
	if err := mapped.GetFrom(resp); err != nil {
		t.Fatalf("MAPPED-ADDRESS missing: %v", err)
	}
	if mapped.Port != int(client.addr.Port()) || mapped.IP.String() != client.addr.Addr().String() {
		t.Fatalf("MAPPED-ADDRESS mismatch: got %s:%d want %s", mapped.IP, mapped.Port, client.addr)
	}
	// The integrity-protected receipt attribute (§10.1 step 3).
	receipt, err := resp.Get(AttrReceipt)
	if err != nil {
		t.Fatalf("receipt attribute missing: %v", err)
	}
	if len(receipt) != receiptSize {
		t.Fatalf("receipt size: got %d want %d", len(receipt), receiptSize)
	}

	// Two challenges at the same source must yield different receipts
	// (§10.1: a fresh UNPREDICTABLE receipt per observation).
	ch2 := issueOK(t, srv, "agent-1", Epoch(7))
	id2 := txn()
	client.sendBinding(t, ch2.ID, ch2.Secret, id2)
	resp2 := client.readMsg(t, 2*time.Second)
	receipt2, err := resp2.Get(AttrReceipt)
	if err != nil {
		t.Fatalf("second receipt missing: %v", err)
	}
	if string(receipt) == string(receipt2) {
		t.Fatalf("receipts must be unpredictable and distinct per challenge")
	}

	// The observation is recorded from the actual UDP source and bound to the
	// challenge, transaction and epoch (§10.1 steps 3, 5). The latest
	// accepted observation (ch2) is the claimable one.
	// Mismatch probes must not consume the observation...
	badReceipt := append([]byte(nil), receipt2...)
	badReceipt[0] ^= 0x01
	if _, err := srv.TakeObservation("agent-1", Epoch(7), ch2.ID, hex.EncodeToString(id2[:]), badReceipt); !errors.Is(err, ErrReceiptMismatch) {
		t.Fatalf("wrong-receipt TakeObservation must fail: got %v", err)
	}
	if _, err := srv.TakeObservation("agent-1", Epoch(8), ch2.ID, hex.EncodeToString(id2[:]), receipt2); !errors.Is(err, ErrEpochMismatch) {
		t.Fatalf("wrong-epoch TakeObservation must fail with ErrEpochMismatch: got %v", err)
	}
	if _, err := srv.TakeObservation("agent-1", Epoch(7), ch2.ID, hex.EncodeToString(id[:]), receipt2); !errors.Is(err, ErrTransactionMismatch) {
		t.Fatalf("wrong-transaction TakeObservation must fail: got %v", err)
	}
	// ...and the exact match claims it exactly once.
	obs, err := srv.TakeObservation("agent-1", Epoch(7), ch2.ID, hex.EncodeToString(id2[:]), receipt2)
	if err != nil {
		t.Fatalf("TakeObservation: %v", err)
	}
	if obs.Source != client.addr.Addr() || obs.SourcePort != int(client.addr.Port()) {
		t.Fatalf("recorded source: got %v want %v", obs.Source, client.addr)
	}
	if obs.Epoch != Epoch(7) {
		t.Fatalf("observation epoch: got %d want 7", obs.Epoch)
	}
	if obs.ChallengeID != ch2.ID {
		t.Fatalf("observation challenge mismatch")
	}
	if obs.TxnID != hex.EncodeToString(id2[:]) {
		t.Fatalf("observation transaction mismatch")
	}
	// One claim per observation.
	if _, err := srv.TakeObservation("agent-1", Epoch(7), ch2.ID, hex.EncodeToString(id2[:]), receipt2); !errors.Is(err, ErrNoObservation) {
		t.Fatalf("second TakeObservation must fail: got %v", err)
	}
}

// TestSTUNChallengeSingleUseAnd60SecondExpiry covers §10.1: challenges are
// single-use and expire after 60 seconds (expires_at = issued_at + 60 s from a
// single clock reading at issuance).
func TestSTUNChallengeSingleUseAnd60SecondExpiry(t *testing.T) {
	srv, srvConn, clock := newTestServer(t, nil)
	client := newTestClient(t, srvConn)

	// Single use: the first accepted request consumes the challenge; the
	// exact same packet replayed is rejected.
	ch := issueOK(t, srv, "agent-1", Epoch(1))
	id := txn()
	client.sendBinding(t, ch.ID, ch.Secret, id)
	if resp := client.readMsg(t, 2*time.Second); resp.Type != stun.BindingSuccess {
		t.Fatalf("first use must succeed, got %v", resp.Type)
	}
	client.sendBinding(t, ch.ID, ch.Secret, id)
	expectBindingError(t, client, id, stun.CodeUnauthorized)

	// Expiry boundary: a challenge is usable strictly before issued_at+60s.
	ch59 := issueOK(t, srv, "agent-1", Epoch(1))
	clock.Advance(59 * time.Second)
	id59 := txn()
	client.sendBinding(t, ch59.ID, ch59.Secret, id59)
	if resp := client.readMsg(t, 2*time.Second); resp.Type != stun.BindingSuccess {
		t.Fatalf("request at +59s must succeed, got %v", resp.Type)
	}

	// At exactly 60 seconds after issuance the challenge has expired. A
	// different agent is used so no earlier observation of agent-1 can
	// interfere with the no-observation assertion.
	ch60 := issueOK(t, srv, "agent-2", Epoch(1))
	clock.Advance(60 * time.Second)
	id60 := txn()
	client.sendBinding(t, ch60.ID, ch60.Secret, id60)
	expectBindingError(t, client, id60, stun.CodeUnauthorized)
	// An expired challenge must not yield an observation.
	if _, err := srv.TakeObservation("agent-2", Epoch(1), ch60.ID, hex.EncodeToString(id60[:]), bytesOf(0, receiptSize)); !errors.Is(err, ErrNoObservation) {
		t.Fatalf("expired challenge must not record an observation: got %v", err)
	}
}

// TestSTUNRejectsBadIntegrityReplaySpoofAndUnknownTransaction covers the
// §10.1/§16.4 rejection paths: wrong MESSAGE-INTEGRITY, replay, a spoofed
// source with stolen credentials (the recorded source stays the actual UDP
// source and the receipt stays bound to it), unknown challenge/transaction,
// missing attributes, wrong message class and non-STUN datagrams.
func TestSTUNRejectsBadIntegrityReplaySpoofAndUnknownTransaction(t *testing.T) {
	srv, srvConn, _ := newTestServer(t, nil)
	client := newTestClient(t, srvConn)

	// (a) Wrong integrity: rejected, the one-use credential is burned
	// (fail-closed), and no observation is recorded.
	ch := issueOK(t, srv, "agent-x", Epoch(3))
	wrongSecret := append([]byte(nil), ch.Secret...)
	wrongSecret[7] ^= 0xff
	id := txn()
	client.sendBinding(t, ch.ID, wrongSecret, id)
	expectBindingError(t, client, id, stun.CodeUnauthorized)
	// Even the correct secret no longer works on the burned challenge.
	id2 := txn()
	client.sendBinding(t, ch.ID, ch.Secret, id2)
	expectBindingError(t, client, id2, stun.CodeUnauthorized)
	if _, err := srv.TakeObservation("agent-x", Epoch(3), ch.ID, hex.EncodeToString(id[:]), bytesOf(0, receiptSize)); !errors.Is(err, ErrNoObservation) {
		t.Fatalf("bad integrity must not record an observation: got %v", err)
	}

	// (b) Replay of a genuine request after acceptance is rejected.
	ch = issueOK(t, srv, "agent-x", Epoch(3))
	id = txn()
	req := func() []byte {
		m := stun.New()
		m.Type = stun.BindingRequest
		m.TransactionID = id
		m.WriteHeader()
		m.Add(stun.AttrUsername, []byte(ch.ID))
		if err := stun.NewShortTermIntegrity(string(ch.Secret)).AddTo(m); err != nil {
			t.Fatalf("integrity AddTo: %v", err)
		}
		return m.Raw
	}()
	client.sendRaw(t, req)
	if resp := client.readMsg(t, 2*time.Second); resp.Type != stun.BindingSuccess {
		t.Fatalf("genuine request must succeed, got %v", resp.Type)
	}
	client.sendRaw(t, req)
	expectBindingError(t, client, id, stun.CodeUnauthorized)

	// (c) Spoofed source with stolen credentials: the listener records the
	// ACTUAL UDP source (the spoofer's), not any agent-declared address, and
	// the receipt stays bound to that source and transaction.
	ch = issueOK(t, srv, "agent-x", Epoch(3))
	spoofer := newTestClient(t, srvConn)
	if spoofer.addr == client.addr {
		t.Fatalf("spoof test requires two distinct UDP sources")
	}
	idSpoof := txn()
	spoofer.sendBinding(t, ch.ID, ch.Secret, idSpoof)
	resp := spoofer.readMsg(t, 2*time.Second)
	if resp.Type != stun.BindingSuccess {
		t.Fatalf("spoofed-source request must still be integrity-gated success, got %v", resp.Type)
	}
	receipt, err := resp.Get(AttrReceipt)
	if err != nil {
		t.Fatalf("receipt missing on spoofed-source response: %v", err)
	}
	// The agent's own socket can no longer use the same challenge.
	idAgent := txn()
	client.sendBinding(t, ch.ID, ch.Secret, idAgent)
	expectBindingError(t, client, idAgent, stun.CodeUnauthorized)
	// The receipt does not transfer to a different transaction (probed
	// before the legitimate claim consumes the observation).
	if _, err := srv.TakeObservation("agent-x", Epoch(3), ch.ID, hex.EncodeToString(idAgent[:]), receipt); !errors.Is(err, ErrTransactionMismatch) {
		t.Fatalf("receipt must stay bound to its transaction: got %v", err)
	}
	obs, err := srv.TakeObservation("agent-x", Epoch(3), ch.ID, hex.EncodeToString(idSpoof[:]), receipt)
	if err != nil {
		t.Fatalf("TakeObservation: %v", err)
	}
	if obs.Source != spoofer.addr.Addr() || obs.SourcePort != int(spoofer.addr.Port()) {
		t.Fatalf("recorded source must be the actual UDP source %v, got %v:%d", spoofer.addr, obs.Source, obs.SourcePort)
	}

	// (d) Unknown challenge: silently dropped (no reflection vector).
	unknown := txn()
	client.sendBinding(t, "00000000000000000000000000000000", []byte("irrelevant"), unknown)
	client.readSilent(t, 400*time.Millisecond)

	// Missing USERNAME: silently dropped.
	m := stun.New()
	m.Type = stun.BindingRequest
	m.TransactionID = txn()
	m.WriteHeader()
	if err := stun.NewShortTermIntegrity("irrelevant").AddTo(m); err != nil {
		t.Fatalf("integrity AddTo: %v", err)
	}
	client.sendRaw(t, m.Raw)
	client.readSilent(t, 400*time.Millisecond)

	// Missing MESSAGE-INTEGRITY on a known challenge: rejected and burned.
	ch = issueOK(t, srv, "agent-x", Epoch(3))
	idNoInt := txn()
	client.sendBinding(t, ch.ID, nil, idNoInt)
	expectBindingError(t, client, idNoInt, stun.CodeUnauthorized)

	// A Binding indication is never answered and must not burn a live
	// challenge.
	ch = issueOK(t, srv, "agent-x", Epoch(3))
	ind := stun.New()
	ind.Type = stun.NewType(stun.MethodBinding, stun.ClassIndication)
	ind.TransactionID = txn()
	ind.WriteHeader()
	ind.Add(stun.AttrUsername, []byte(ch.ID))
	client.sendRaw(t, ind.Raw)
	client.readSilent(t, 400*time.Millisecond)
	idLive := txn()
	client.sendBinding(t, ch.ID, ch.Secret, idLive)
	if resp := client.readMsg(t, 2*time.Second); resp.Type != stun.BindingSuccess {
		t.Fatalf("live challenge must survive an indication, got %v", resp.Type)
	}

	// Non-STUN garbage: silently dropped.
	client.sendRaw(t, []byte("not a stun message at all, just noise bytes padding.."))
	client.readSilent(t, 400*time.Millisecond)
}

// TestSTUNRateLimitsPerAgent covers §16.4: challenge issuance (credential
// minting) is rate limited per agent with bounded per-agent state, so
// reconnect churn cannot mint unbounded one-use credentials.
func TestSTUNRateLimitsPerAgent(t *testing.T) {
	// Zero values select the package defaults asserted here: 4 challenges per
	// agent per minute (bucket refills 1 token per 15 s) and at most 2
	// outstanding (pending) challenges per agent.
	srv, srvConn, clock := newTestServer(t, func(cfg *Config) {
		cfg.MaxChallengesPerAgentPerMinute = 0
		cfg.MaxPendingPerAgent = 0
	})
	client := newTestClient(t, srvConn)

	// Outstanding cap: two challenges minted, a third is refused while both
	// are unanswered.
	ch1 := issueOK(t, srv, "agent-r", Epoch(1))
	ch2 := issueOK(t, srv, "agent-r", Epoch(1))
	if _, err := srv.IssueChallenge("agent-r", Epoch(1)); !errors.Is(err, ErrTooManyPending) {
		t.Fatalf("third outstanding challenge must be refused: got %v", err)
	}

	// Accepting ch1 over UDP frees its pending slot (token 3 is then spent).
	complete := func(ch Challenge) {
		t.Helper()
		id := txn()
		client.sendBinding(t, ch.ID, ch.Secret, id)
		if resp := client.readMsg(t, 2*time.Second); resp.Type != stun.BindingSuccess {
			t.Fatalf("challenge %s must complete, got %v", ch.ID, resp.Type)
		}
	}
	complete(ch1)
	ch3 := issueOK(t, srv, "agent-r", Epoch(1))
	// Accepting ch2 frees another slot so the fourth token can be spent
	// without exceeding the outstanding cap.
	complete(ch2)
	ch4 := issueOK(t, srv, "agent-r", Epoch(1))

	// The token bucket is now empty: drain the outstanding challenges, then
	// a further challenge within the minute is rate limited.
	complete(ch3)
	complete(ch4)
	if _, err := srv.IssueChallenge("agent-r", Epoch(1)); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("fifth challenge within a minute must be rate limited: got %v", err)
	}
	// Nothing refills before 15 seconds elapse.
	clock.Advance(14 * time.Second)
	if _, err := srv.IssueChallenge("agent-r", Epoch(1)); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("challenge before refill must be rate limited: got %v", err)
	}
	// Exactly one token refills after 15 seconds.
	clock.Advance(1 * time.Second)
	ch5, err := srv.IssueChallenge("agent-r", Epoch(1))
	if err != nil {
		t.Fatalf("challenge after refill: %v", err)
	}
	if _, err := srv.IssueChallenge("agent-r", Epoch(1)); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("second challenge after a single refill must be rate limited: got %v", err)
	}
	complete(ch5)

	// Per-agent isolation: another agent is unaffected by agent-r's limits.
	if _, err := srv.IssueChallenge("agent-other", Epoch(1)); err != nil {
		t.Fatalf("other agent must be unaffected: %v", err)
	}

	// The global agent cap bounds tracked state (fail-closed at issuance).
	srv2, _, _ := newTestServer(t, func(cfg *Config) {
		cfg.MaxChallengesPerAgentPerMinute = 64
		cfg.MaxPendingPerAgent = 8
		cfg.MaxAgents = 1
	})
	if _, err := srv2.IssueChallenge("agent-a", Epoch(1)); err != nil {
		t.Fatalf("first agent must be admitted: %v", err)
	}
	if _, err := srv2.IssueChallenge("agent-b", Epoch(1)); !errors.Is(err, ErrAgentCapacity) {
		t.Fatalf("agent beyond capacity must be refused: got %v", err)
	}
}

// TestResponseReceiptIsIntegrityProtected is the Step 4 packet fixture: it
// proves the receipt and the mapped address bytes sit inside the
// MESSAGE-INTEGRITY protected region — tampering with either breaks the
// integrity check, so an off-path attacker cannot alter the receipt (or the
// mapped address) in flight.
func TestResponseReceiptIsIntegrityProtected(t *testing.T) {
	srv, srvConn, _ := newTestServer(t, nil)
	client := newTestClient(t, srvConn)

	ch := issueOK(t, srv, "agent-f", Epoch(9))
	id := txn()
	client.sendBinding(t, ch.ID, ch.Secret, id)

	buf := make([]byte, 1500)
	if err := client.conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	n, err := client.conn.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	raw := append([]byte(nil), buf[:n]...)

	check := func(packet []byte) error {
		m := stun.New()
		m.Raw = append([]byte(nil), packet...)
		if err := m.Decode(); err != nil {
			return err
		}
		return stun.NewShortTermIntegrity(string(ch.Secret)).Check(m)
	}
	if err := check(raw); err != nil {
		t.Fatalf("untampered fixture must pass integrity: %v", err)
	}

	// Locate the receipt VALUE inside the raw packet by walking the attribute
	// TLVs from the message header.
	m := stun.New()
	m.Raw = append([]byte(nil), raw...)
	if err := m.Decode(); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if _, err := m.Get(AttrReceipt); err != nil {
		t.Fatalf("receipt missing: %v", err)
	}
	receiptOffset := -1
	offset := stunHeaderSize
	for _, a := range m.Attributes {
		if a.Type == AttrReceipt {
			receiptOffset = offset + attributeHeaderSize
			break
		}
		offset += attributeHeaderSize + (int(a.Length)+3)&^3
	}
	if receiptOffset < 0 || receiptOffset >= len(raw) {
		t.Fatalf("receipt bytes not located in fixture (offset %d, len %d)", receiptOffset, len(raw))
	}
	tampered := append([]byte(nil), raw...)
	tampered[receiptOffset] ^= 0x01
	if err := check(tampered); err == nil {
		t.Fatalf("tampered receipt must fail MESSAGE-INTEGRITY")
	}

	// Flip a byte of the XOR-MAPPED-ADDRESS value (its XOR-obscured port):
	// also covered by the same MESSAGE-INTEGRITY.
	tamperedAddr := append([]byte(nil), raw...)
	tamperedAddr[stunHeaderSize+attributeHeaderSize+2] ^= 0x01
	if err := check(tamperedAddr); err == nil {
		t.Fatalf("tampered mapped address must fail MESSAGE-INTEGRITY")
	}
}

// TestClassifySource locks the §10.3 address classification the listener
// attaches to observations: public IPv4 is direct-eligible evidence;
// private/reserved/CGNAT and non-IPv4 sources are recorded faithfully but
// flagged non-public (§10.3 sends them to relay fallback at the policy layer).
func TestClassifySource(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"8.8.8.8", true},
		{"10.1.2.3", false},
		{"172.16.0.9", false},
		{"192.168.1.1", false},
		{"100.64.0.1", false},  // CGNAT
		{"127.0.0.1", false},   // loopback
		{"169.254.1.1", false}, // link-local
		{"192.0.2.9", false},   // TEST-NET-1 (reserved)
		{"198.18.0.5", false},  // benchmarking (reserved)
		{"224.0.0.1", false},   // multicast
		{"240.0.0.1", false},   // reserved
		{"::1", false},         // non-IPv4
		{"2001:db8::1", false}, // non-IPv4
	}
	for _, tc := range cases {
		addr := netip.MustParseAddr(tc.ip)
		if got := isPublicIPv4(addr); got != tc.want {
			t.Errorf("isPublicIPv4(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

// TestParseBindAddr locks the fail-closed bind-address rule: only UDP 3478 may
// be bound (§10.1) — any other port or junk input must fail at startup rather
// than silently binding elsewhere.
func TestParseBindAddr(t *testing.T) {
	if addr, err := ParseBindAddr("0.0.0.0:3478"); err != nil || addr == nil {
		t.Fatalf("0.0.0.0:3478 must parse: %v", err)
	}
	if addr, err := ParseBindAddr("[::]:3478"); err != nil || addr == nil {
		t.Fatalf("[::]:3478 must parse: %v", err)
	}
	if _, err := ParseBindAddr("0.0.0.0:3479"); err == nil {
		t.Fatalf("a port other than 3478 must be rejected")
	}
	if _, err := ParseBindAddr("not-an-address"); err == nil {
		t.Fatalf("junk bind address must be rejected")
	}
	if _, err := ParseBindAddr(""); err == nil {
		t.Fatalf("empty bind address must be rejected")
	}
}
