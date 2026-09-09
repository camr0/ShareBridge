// Package stun implements control's authenticated STUN observation listener
// (plan Task 16, spec §§10.1, 10.3, 16.4).
//
// Flow (§10.1): control's WebSocket layer issues a one-use short-term
// credential (IssueChallenge) and delivers it in a stun_challenge over the
// agent's authenticated WebSocket. The agent sends one STUN Binding request to
// the UDP 3478 listener with USERNAME=<challenge id> and MESSAGE-INTEGRITY
// keyed with the one-use secret. The listener verifies the integrity of the
// request, records the ACTUAL UDP source address, and answers with the normal
// mapped address plus an unpredictable receipt value inside the
// MESSAGE-INTEGRITY-protected response. The agent echoes the receipt in
// stun_result over the WebSocket; the WebSocket layer claims the observation
// with TakeObservation, which accepts only an exact match of receipt, WS
// epoch, challenge, transaction and a bounded claim window — so a blind
// source-spoofed UDP packet can never manufacture an observation (§16.4).
//
// Hardening summary:
//   - challenges are single-use: the credential is burned atomically on the
//     first authenticated request AND on any integrity failure (fail-closed);
//   - challenges expire 60 seconds after a single clock reading at issuance;
//   - issuance is rate limited per agent (bounded token bucket) and bounded
//     by per-agent outstanding and global caps, so reconnect churn cannot
//     mint unbounded credentials (§16.4);
//   - unknown challenges, missing USERNAME, non-STUN datagrams, oversized
//     datagrams, indications and other methods are silently dropped (no
//     reflection vector); known challenges get a bare 401 error response;
//   - all state is bounded: ≤2 pending + 1 accepted observation per agent,
//     a global tombstone cap, and a periodic sweep of expired material;
//   - one-use secrets and receipt bytes are never logged (the package has no
//     logging of material at all).
//
// Address policy ruling (§10.3): the listener faithfully records the observed
// source regardless of address class and attaches an isPublicIPv4
// classification. Private/reserved/CGNAT and non-IPv4 sources are NOT
// rejected here — §10.1 defines the listener's contract as recording the
// source and returning the receipt, while §10.3 makes the address class a
// routing-policy consequence (relay fallback) at the direct-eligibility layer
// (Tasks 19/20). A tamper-proof non-public observation is exactly the
// evidence that layer needs.
package stun

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/pion/stun/v3"
)

// AttrReceipt is the custom comprehension-optional attribute carrying the
// integrity-protected observation receipt in the Binding success response
// (§10.1 step 3). 0xFF01 sits in the private-use range (0xC000-0xFFFF) of the
// RFC 8489 §18.2 attribute space; it is added BEFORE MESSAGE-INTEGRITY so it
// is covered by the response integrity.
const AttrReceipt = stun.AttrType(0xFF01)

// ChallengeTTL is the §10.1 challenge lifetime: challenges expire 60 seconds
// after the single clock reading taken at issuance.
const ChallengeTTL = 60 * time.Second

// receiptSize is the exact receipt length (128 bits of HMAC-SHA256 output):
// unpredictable per receipt, bounded on the wire and in storage.
const receiptSize = 16

const (
	stunHeaderSize      = 20
	attributeHeaderSize = 4
	// maxSTUNMessageSize is the conservative UDP STUN message cap (§10.1
	// requests carry only USERNAME + MESSAGE-INTEGRITY). Larger datagrams
	// are dropped unconditionally.
	maxSTUNMessageSize = 548
	maxReadSize        = 1500

	// secretSize is the one-use short-term credential secret (256 bits).
	secretSize = 32
	// idSize is the challenge identifier (128 bits, hex-encoded as the
	// STUN USERNAME).
	idSize = 16

	// DefaultMaxPendingPerAgent bounds one-use credentials in flight per
	// agent (§16.4).
	DefaultMaxPendingPerAgent = 2
	// DefaultMaxChallengesPerAgentPerMinute bounds credential minting per
	// agent per minute (§16.4 "rate limits bound credential requests and
	// reconnect churn"); the bucket refills one token every 60s/N.
	DefaultMaxChallengesPerAgentPerMinute = 4
	// DefaultMaxAgents bounds total tracked agent state (fail-closed at
	// issuance beyond the cap).
	DefaultMaxAgents = 4096
	// maxTotalChallenges is a global tombstone bound (pending + used/expired
	// entries awaiting sweep): issuance fails closed beyond it.
	maxTotalChallenges = DefaultMaxAgents * 4

	// sweepInterval is the state-hygiene cadence; expiry is always enforced
	// exactly at decision time, the sweep only reclaims memory.
	sweepInterval = 15 * time.Second
	// observationClaimWindow bounds how long after acceptance the receipt
	// can be claimed via TakeObservation (the agent echoes stun_result
	// immediately per §10.1 step 4).
	observationClaimWindow = ChallengeTTL
	// agentIdleTTL reclaims per-agent state for agents with nothing pending.
	agentIdleTTL = 10 * time.Minute

	receiptDomain = "sharebridge/stun-receipt/v1"
)

// Errors returned by IssueChallenge and TakeObservation. Callers (the Task 18
// WebSocket scheduler) must treat all of them as rejections.
var (
	ErrRateLimited         = errors.New("stun: per-agent challenge rate limit exceeded")
	ErrTooManyPending      = errors.New("stun: per-agent pending challenge cap exceeded")
	ErrAgentCapacity       = errors.New("stun: tracked agent capacity exceeded")
	ErrNoObservation       = errors.New("stun: no matching accepted observation")
	ErrEpochMismatch       = errors.New("stun: observation bound to a different WS epoch")
	ErrChallengeMismatch   = errors.New("stun: observation bound to a different challenge")
	ErrTransactionMismatch = errors.New("stun: observation bound to a different transaction")
	ErrReceiptMismatch     = errors.New("stun: receipt mismatch")
	ErrObservationExpired  = errors.New("stun: observation claim window expired")
)

// Epoch is the opaque WS connection epoch an observation is bound to (§10.1
// step 5: control accepts the observation only when the receipt, connection
// epoch, transaction, and expiry match). The WebSocket layer supplies the
// CURRENT epoch at issuance and at claim time.
type Epoch uint64

// Config is the fail-closed construction contract for the listener. Zero
// value rate fields select the package defaults.
type Config struct {
	// Key is the server-side HMAC key used to derive receipt values. It
	// MUST be at least 32 bytes. Receipts are process-local evidence: the
	// suggested production key is a fresh random 32 bytes per control
	// process start (pending challenges die with the process anyway).
	Key []byte
	// BindAddr is the UDP listen address for Listen() (default
	// "0.0.0.0:3478"). UDP 3478 is the only public control listener this
	// package may bind; ParseBindAddr enforces the port for operator input.
	BindAddr string
	// Now is the clock seam; nil means time.Now. Every issuance and every
	// datagram decision reads the clock exactly once.
	Now func() time.Time

	// MaxChallengesPerAgentPerMinute caps credential minting per agent per
	// minute (token bucket: capacity N, refill N per 60 s). Zero selects
	// DefaultMaxChallengesPerAgentPerMinute.
	MaxChallengesPerAgentPerMinute int
	// MaxPendingPerAgent caps unanswered one-use credentials per agent.
	// Zero selects DefaultMaxPendingPerAgent.
	MaxPendingPerAgent int
	// MaxAgents caps the number of agents with tracked state. Zero selects
	// DefaultMaxAgents.
	MaxAgents int
}

// Challenge is the one-use short-term credential delivered to the agent in a
// stun_challenge over its authenticated WebSocket. Secret is the short-term
// credential password (RFC 8489 §9); it must never be logged (§16.6 log
// policy) and is exactly the value the Task 17 client keys its
// MESSAGE-INTEGRITY with.
type Challenge struct {
	ID        string    // USERNAME value for the Binding request
	Secret    []byte    // one-use secret; deliver over the WS epoch only
	ExpiresAt time.Time // issued_at (single clock reading) + ChallengeTTL
	Epoch     Epoch     // WS epoch the receipt will be bound to
}

// Observation is one control-observed UDP source, claimable exactly once via
// TakeObservation. Source/SourcePort are the ACTUAL UDP source (never an
// agent-declared value); PublicIPv4 carries the §10.3 address-class
// classification for the policy layer.
type Observation struct {
	AgentID     string
	ChallengeID string
	TxnID       string // hex-encoded 96-bit transaction ID
	Source      netip.Addr
	SourcePort  int
	PublicIPv4  bool
	Epoch       Epoch
	Receipt     []byte
	AcceptedAt  time.Time
}

// challengeState is one one-use credential. All fields except used are
// immutable after creation; used is guarded by Server.mu.
type challengeState struct {
	id        string
	agentID   string
	epoch     Epoch
	secret    []byte
	expiresAt time.Time
	used      bool
}

// agentState is the bounded per-agent view: at most maxPending pending
// challenges, the latest accepted observation, and the rate bucket.
type agentState struct {
	pending     []*challengeState
	observation *Observation
	tokens      float64
	lastRefill  time.Time
	lastSeen    time.Time
}

// Server is the authenticated STUN observation listener. It is safe for
// concurrent use: IssueChallenge/TakeObservation run on WebSocket-handler
// goroutines while Serve owns the UDP socket.
type Server struct {
	key      []byte
	bindAddr string
	now      func() time.Time

	maxPerMinute float64
	refillEvery  time.Duration
	maxPending   int
	maxAgents    int

	mu          sync.Mutex
	byAgent     map[string]*agentState
	byChallenge map[string]*challengeState // pending + tombstones awaiting sweep
}

// NewServer validates the config and returns a listener. It fails closed on a
// short key.
func NewServer(cfg Config) (*Server, error) {
	if len(cfg.Key) < secretSize {
		return nil, fmt.Errorf("stun: key must be at least %d bytes", secretSize)
	}
	maxPerMinute := float64(DefaultMaxChallengesPerAgentPerMinute)
	if cfg.MaxChallengesPerAgentPerMinute > 0 {
		maxPerMinute = float64(cfg.MaxChallengesPerAgentPerMinute)
	}
	maxPending := DefaultMaxPendingPerAgent
	if cfg.MaxPendingPerAgent > 0 {
		maxPending = cfg.MaxPendingPerAgent
	}
	maxAgents := DefaultMaxAgents
	if cfg.MaxAgents > 0 {
		maxAgents = cfg.MaxAgents
	}
	bindAddr := cfg.BindAddr
	if bindAddr == "" {
		bindAddr = "0.0.0.0:3478"
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	return &Server{
		key:          append([]byte(nil), cfg.Key...),
		bindAddr:     bindAddr,
		now:          nowFn,
		maxPerMinute: maxPerMinute,
		refillEvery:  time.Minute / time.Duration(maxPerMinute),
		maxPending:   maxPending,
		maxAgents:    maxAgents,
		byAgent:      map[string]*agentState{},
		byChallenge:  map[string]*challengeState{},
	}, nil
}

// NewKey returns a fresh 32-byte receipt key for process-local use.
func NewKey() ([]byte, error) {
	key := make([]byte, secretSize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("stun: generate receipt key: %w", err)
	}
	return key, nil
}

// ParseBindAddr validates an operator-supplied STUN_BIND_ADDR value: it must
// resolve as a UDP address and bind port 3478 (§10.1: UDP 3478 is the only
// public control listener this package adds). Anything else — including any
// other port — fails closed at startup.
func ParseBindAddr(s string) (*net.UDPAddr, error) {
	if s == "" {
		return nil, errors.New("stun: bind address is empty")
	}
	addr, err := net.ResolveUDPAddr("udp", s)
	if err != nil {
		return nil, fmt.Errorf("stun: bind address %q: %w", s, err)
	}
	if addr.Port != 3478 {
		return nil, fmt.Errorf("stun: listener must bind UDP 3478, got %q", s)
	}
	return addr, nil
}

// Listen binds the configured UDP address. The returned conn is passed back to
// Serve; the listener does not serve until Serve is called.
func (s *Server) Listen() (*net.UDPConn, error) {
	addr, err := net.ResolveUDPAddr("udp", s.bindAddr)
	if err != nil {
		return nil, fmt.Errorf("stun: bind address %q: %w", s.bindAddr, err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("stun: listen %s: %w", s.bindAddr, err)
	}
	return conn, nil
}

// Serve reads and handles datagrams until ctx is cancelled. It must be called
// with the conn returned by Listen. A single goroutine handles every packet:
// per-packet work is one HMAC and bounded map operations, so the listener adds
// no unbounded concurrency.
func (s *Server) Serve(ctx context.Context, conn *net.UDPConn) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	sweeper := time.NewTicker(sweepInterval)
	defer sweeper.Stop()
	// Memory hygiene only (expiry is enforced exactly at decision time); a
	// dedicated goroutine so an idle listener still sweeps. Exactly one per
	// Serve call.
	sweepStop := make(chan struct{})
	defer close(sweepStop)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-sweepStop:
				return
			case <-sweeper.C:
				now := s.now() // single clock reading per sweep
				s.sweep(now)
			}
		}
	}()

	buf := make([]byte, maxReadSize)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("stun: read: %w", err)
		}
		if n > 0 {
			s.handlePacket(conn, buf[:n], src)
		}
	}
}

// IssueChallenge mints one one-use short-term credential for the agent at the
// given WS epoch. issued_at/expires_at come from a single clock reading. It
// returns ErrTooManyPending, ErrRateLimited or ErrAgentCapacity when a §16.4
// bound is hit (all fail-closed: without a challenge there is no observation
// and direct mode falls back to relay per §10.3).
func (s *Server) IssueChallenge(agentID string, epoch Epoch) (Challenge, error) {
	if agentID == "" {
		return Challenge{}, errors.New("stun: agent id required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now() // single clock reading per issuance

	as := s.byAgent[agentID]
	// Global tombstone bound (§16.4, T16-m1): enforced on EVERY issuance —
	// an existing agent whose pending entries keep the map at cap is also
	// rejected here (fail closed before minting), not only brand-new agents.
	if len(s.byChallenge) >= maxTotalChallenges {
		return Challenge{}, ErrAgentCapacity
	}
	if as == nil {
		if len(s.byAgent) >= s.maxAgents {
			return Challenge{}, ErrAgentCapacity
		}
		as = &agentState{tokens: s.maxPerMinute, lastRefill: now, lastSeen: now}
		s.byAgent[agentID] = as
	}
	as.lastSeen = now

	// Refill the token bucket for the elapsed wall time.
	if elapsed := now.Sub(as.lastRefill); elapsed > 0 {
		as.tokens = math.Min(s.maxPerMinute, as.tokens+elapsed.Seconds()/s.refillEvery.Seconds())
		as.lastRefill = now
	}
	if len(as.pending) >= s.maxPending {
		return Challenge{}, ErrTooManyPending
	}
	if as.tokens < 1 {
		return Challenge{}, ErrRateLimited
	}
	as.tokens--

	idBytes := make([]byte, idSize)
	if err := randomBytes(idBytes); err != nil {
		return Challenge{}, err
	}
	secret := make([]byte, secretSize)
	if err := randomBytes(secret); err != nil {
		return Challenge{}, err
	}
	cs := &challengeState{
		id:        hex.EncodeToString(idBytes),
		agentID:   agentID,
		epoch:     epoch,
		secret:    secret,
		expiresAt: now.Add(ChallengeTTL),
	}
	as.pending = append(as.pending, cs)
	s.byChallenge[cs.id] = cs
	return Challenge{ID: cs.id, Secret: secret, ExpiresAt: cs.expiresAt, Epoch: epoch}, nil
}

// TakeObservation claims the agent's latest accepted observation. It accepts
// only an exact match of the presented WS epoch, challenge ID, transaction ID
// and receipt, within the bounded claim window, and consumes the observation
// (one stun_result per observation; replays are ErrNoObservation). Mismatches
// do not consume — the caller rejects the stun_result either way.
func (s *Server) TakeObservation(agentID string, epoch Epoch, challengeID, txnID string, receipt []byte) (Observation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now() // single clock reading per claim

	as := s.byAgent[agentID]
	if as == nil || as.observation == nil {
		return Observation{}, ErrNoObservation
	}
	obs := as.observation
	if !now.Before(obs.AcceptedAt.Add(observationClaimWindow)) {
		as.observation = nil
		return Observation{}, ErrObservationExpired
	}
	if obs.Epoch != epoch {
		return Observation{}, ErrEpochMismatch
	}
	if obs.ChallengeID != challengeID {
		return Observation{}, ErrChallengeMismatch
	}
	if obs.TxnID != txnID {
		return Observation{}, ErrTransactionMismatch
	}
	if !hmac.Equal(obs.Receipt, receipt) {
		return Observation{}, ErrReceiptMismatch
	}
	as.observation = nil
	return *obs, nil
}

// ---------- UDP packet path ----------

// handlePacket implements the §10.1 request path. Rejection taxonomy:
//   - oversized, non-STUN, undecodable, non-Binding, indications, unknown
//     challenge, missing USERNAME: silent drop (no reflection, no state);
//   - known challenge that is expired, reused, integrity-missing or
//     integrity-invalid: bare 401 Binding error response, credential burned.
func (s *Server) handlePacket(conn *net.UDPConn, buf []byte, src *net.UDPAddr) {
	if len(buf) > maxSTUNMessageSize || !stun.IsMessage(buf) {
		return
	}
	req := stun.New()
	req.Raw = buf
	if err := req.Decode(); err != nil {
		return
	}
	if req.Type != stun.BindingRequest {
		return // indications, responses and unknown methods get no answer
	}
	srcAddr, ok := normalizeSource(src)
	if !ok {
		return
	}
	now := s.now() // single clock reading per datagram decision

	rawUser, err := req.Get(stun.AttrUsername)
	if err != nil {
		return // cannot identify the credential: silent drop
	}
	s.mu.Lock()
	cs := s.byChallenge[string(rawUser)]
	if cs == nil {
		s.mu.Unlock()
		return // unknown challenge: silent drop
	}
	if !now.Before(cs.expiresAt) {
		s.expireLocked(cs)
		s.mu.Unlock()
		sendError(conn, req.TransactionID, src, stun.CodeUnauthorized)
		return
	}
	if cs.used {
		s.mu.Unlock()
		sendError(conn, req.TransactionID, src, stun.CodeUnauthorized)
		return
	}
	// Atomic single-use reservation: the first request (valid or not)
	// consumes the credential. Integrity below only decides whether an
	// observation is recorded; a failed check can never leave the
	// credential live.
	cs.used = true
	s.removePendingLocked(cs)
	secret := cs.secret
	challengeID := cs.id
	epoch := cs.epoch
	agentID := cs.agentID
	s.mu.Unlock()

	// MESSAGE-INTEGRITY (short-term credential, RFC 8489 §9): the HMAC key
	// is the one-use secret delivered only over the agent's authenticated
	// WS epoch.
	integrity := stun.NewShortTermIntegrity(string(secret))
	if !req.Contains(stun.AttrMessageIntegrity) || integrity.Check(req) != nil {
		sendError(conn, req.TransactionID, src, stun.CodeUnauthorized)
		return
	}

	// Accepted: record the ACTUAL UDP source and mint the receipt bound to
	// challenge + transaction + source + epoch + fresh nonce. A receipt
	// entropy failure drops the packet entirely (fail closed: no receipt,
	// no observation, no success response).
	receipt := s.receipt(challengeID, epoch, req.TransactionID, srcAddr)
	if receipt == nil {
		return
	}
	obs := &Observation{
		AgentID:     agentID,
		ChallengeID: challengeID,
		TxnID:       hex.EncodeToString(req.TransactionID[:]),
		Source:      srcAddr.Addr(),
		SourcePort:  int(srcAddr.Port()),
		PublicIPv4:  isPublicIPv4(srcAddr.Addr()),
		Epoch:       epoch,
		Receipt:     receipt,
		AcceptedAt:  now,
	}
	s.mu.Lock()
	if as := s.byAgent[agentID]; as != nil {
		as.observation = obs // latest accepted observation wins; ≤1 per agent
	}
	s.mu.Unlock()

	sendSuccess(conn, req.TransactionID, src, srcAddr, secret, receipt)
}

// receipt derives the unpredictable receipt value: HMAC-SHA256 over the
// challenge, transaction ID, observed source, epoch and a fresh random nonce,
// truncated to 128 bits. The nonce keeps two identical observations
// (same challenge impossible, same source+txn plausible under replay)
// from producing identical receipt material.
func (s *Server) receipt(challengeID string, epoch Epoch, txn [stun.TransactionIDSize]byte, src netip.AddrPort) []byte {
	nonce := make([]byte, 8)
	if err := randomBytes(nonce); err != nil {
		// crypto/rand failure is unrecoverable for a security value; fail
		// the packet rather than emit a predictable receipt.
		return nil
	}
	h := hmac.New(sha256.New, s.key)
	h.Write([]byte(receiptDomain))
	h.Write([]byte(challengeID))
	h.Write(txn[:])
	ipBytes := src.Addr().As16() // canonical form: same address, same bytes
	h.Write(ipBytes[:])
	var portBytes [2]byte
	binary.BigEndian.PutUint16(portBytes[:], src.Port())
	h.Write(portBytes[:])
	var epochBytes [8]byte
	binary.BigEndian.PutUint64(epochBytes[:], uint64(epoch))
	h.Write(epochBytes[:])
	h.Write(nonce)
	return h.Sum(nil)[:receiptSize]
}

// sendSuccess answers with XOR-MAPPED-ADDRESS + MAPPED-ADDRESS + RECEIPT, all
// covered by MESSAGE-INTEGRITY keyed with the one-use secret (§10.1 step 3).
// The header is written first (WriteHeader) and MESSAGE-INTEGRITY is added
// LAST: pion's integrity AddTo computes the HMAC over the exact Raw bytes that
// go on the wire, so the message must never be re-Encoded afterwards.
func sendSuccess(conn *net.UDPConn, txn [stun.TransactionIDSize]byte, src *net.UDPAddr, mapped netip.AddrPort, secret, receipt []byte) {
	resp := stun.New()
	resp.Type = stun.BindingSuccess
	resp.TransactionID = txn
	resp.WriteHeader()
	ip := net.IP(mapped.Addr().AsSlice())
	xorAddr := stun.XORMappedAddress{IP: ip, Port: int(mapped.Port())}
	if err := xorAddr.AddTo(resp); err != nil {
		return
	}
	mappedAddr := stun.MappedAddress{IP: ip, Port: int(mapped.Port())}
	if err := mappedAddr.AddTo(resp); err != nil {
		return
	}
	if receipt != nil {
		resp.Add(AttrReceipt, receipt)
	}
	if err := stun.NewShortTermIntegrity(string(secret)).AddTo(resp); err != nil {
		return
	}
	_, _ = conn.WriteToUDP(resp.Raw, src)
}

// sendError answers a KNOWN challenge with a bare (unauthenticated) Binding
// error response, as allowed for short-term credentials.
func sendError(conn *net.UDPConn, txn [stun.TransactionIDSize]byte, src *net.UDPAddr, code stun.ErrorCode) {
	resp := stun.New()
	resp.Type = stun.BindingError
	resp.TransactionID = txn
	resp.WriteHeader()
	if err := (stun.ErrorCodeAttribute{Code: code, Reason: []byte("Unauthorized")}).AddTo(resp); err != nil {
		return
	}
	_, _ = conn.WriteToUDP(resp.Raw, src)
}

// ---------- state helpers (callers hold s.mu) ----------

func (s *Server) removePendingLocked(cs *challengeState) {
	as := s.byAgent[cs.agentID]
	if as == nil {
		return
	}
	for i, p := range as.pending {
		if p == cs {
			as.pending = append(as.pending[:i], as.pending[i+1:]...)
			return
		}
	}
}

func (s *Server) expireLocked(cs *challengeState) {
	cs.used = true
	s.removePendingLocked(cs)
	delete(s.byChallenge, cs.id)
}

// sweep reclaims expired and idle state. Expiry is always enforced exactly at
// decision time; the sweep only bounds memory.
func (s *Server) sweep(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, cs := range s.byChallenge {
		if now.After(cs.expiresAt) {
			s.expireLocked(cs)
			_ = id
		}
	}
	for agentID, as := range s.byAgent {
		// Drop aged accepted observations (their claim window has long
		// passed) and whole agent entries with nothing live and no recent
		// activity.
		if as.observation != nil && now.After(as.observation.AcceptedAt.Add(2*ChallengeTTL)) {
			as.observation = nil
		}
		if len(as.pending) == 0 && as.observation == nil && now.After(as.lastSeen.Add(agentIdleTTL)) {
			delete(s.byAgent, agentID)
		}
	}
}

// ---------- helpers ----------

func randomBytes(b []byte) error {
	if _, err := rand.Read(b); err != nil {
		return fmt.Errorf("stun: random bytes: %w", err)
	}
	return nil
}

// normalizeSource converts the UDP source into a canonical netip.AddrPort,
// mapping IPv4-in-IPv6 forms to plain IPv4 (§10.3 requires an exact IPv4
// comparison later).
func normalizeSource(src *net.UDPAddr) (netip.AddrPort, bool) {
	if src == nil || src.IP == nil {
		return netip.AddrPort{}, false
	}
	if ip4 := src.IP.To4(); ip4 != nil {
		var a [4]byte
		copy(a[:], ip4)
		return netip.AddrPortFrom(netip.AddrFrom4(a), uint16(src.Port)), true
	}
	addr, ok := netip.AddrFromSlice(src.IP)
	if !ok {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(addr.Unmap(), uint16(src.Port)), true
}

// nonPublicIPv4CIDRs are the §10.3 address classes that never qualify for
// direct eligibility: private, reserved, CGNAT, loopback, link-local,
// benchmarking, multicast. Mirrors directctl's SSRF denylist semantics so the
// policy layer receives an already-conservative classification.
var nonPublicIPv4CIDRs = mustParsePrefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
	"192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
	"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	"192.31.196.0/24", "192.52.193.0/24", "192.175.48.0/24",
)

func mustParsePrefixes(cidrs ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		out = append(out, netip.MustParsePrefix(c))
	}
	return out
}

// isPublicIPv4 reports whether the observed source is a globally routable
// IPv4 unicast address. Non-IPv4 sources are never public (§10.3: exact IPv4
// match is a direct-eligibility requirement).
func isPublicIPv4(addr netip.Addr) bool {
	if !addr.Is4() || addr.Is4In6() {
		return false
	}
	for _, p := range nonPublicIPv4CIDRs {
		if p.Contains(addr.Unmap()) {
			return false
		}
	}
	return true
}
