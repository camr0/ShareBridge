// agent/internal/direct/opensignal.go
package direct

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// OpenSignal is a short-lived, versioned, idempotent request to open the public
// port for one share. It is bound to (agent, share, route, nonce) and carries
// an expiration, a monotonically increasing sequence number, and a per-share
// open lease (spec §5.1). The sequence number is what lets the gate reject a
// distinct-nonce signal that is older than one already admitted (reorder).
type OpenSignal struct {
	Version   int
	AgentID   string
	ShareID   string
	RouteKind RouteKind
	Nonce     string
	Seq       uint64
	ExpiresAt time.Time
	Lease     time.Duration
}

const signalVersion = 1

var (
	ErrBadVersion     = errors.New("direct: unsupported open-signal version")
	ErrWrongAgent     = errors.New("direct: open signal for a different agent")
	ErrExpiredSignal  = errors.New("direct: open signal expired")
	ErrReplaySignal   = errors.New("direct: replayed open signal")
	ErrNonceReuse     = errors.New("direct: nonce reused for a different share/route")
	ErrSignalLockdown = errors.New("direct: open signals refused during lockdown")
	ErrSignalNotAuth  = errors.New("direct: share not registered/source-authorized locally")
	ErrSignalRate     = errors.New("direct: open-signal rate limit exceeded")
	ErrBadLease       = errors.New("direct: open-signal lease out of bounds")
	ErrBadLifetime    = errors.New("direct: open-signal expiry out of bounds")
)

// shareAuthorizer reports whether the agent has independently registered and
// source-verified a share for a route. The signal only ACTIVATES a route the
// agent already knows; it never creates one.
type shareAuthorizer func(shareID string, kind RouteKind) bool

// SignalGate validates open signals at the agent. It drops expired, replayed,
// or nonce-reused signals, refuses all signals during lockdown, re-verifies
// local source authorization, and rate-limits per window and per share.
type SignalGate struct {
	mu       sync.Mutex
	agentID  string
	lockdown bool
	now      func() time.Time
	authz    shareAuthorizer

	seen    map[string]nonceUse
	applied map[string]time.Time // "share:route" -> appliedAt
	highest uint64               // highest sequence number admitted so far

	winStart time.Time
	winCount int
	perShare map[string]int
}

type nonceUse struct {
	shareID string
	kind    RouteKind
	seenAt  time.Time
}

const (
	signalWindow     = time.Minute
	maxSignalsPerWin = 60

	maxLease          = 15 * time.Minute                // per-share open lease ceiling
	maxSignalLifetime = 5 * time.Minute                 // how far in the future ExpiresAt may be
	nonceRetention    = maxSignalLifetime + time.Minute // nonce remembered through validity + skew
)

func NewSignalGate(agentID string, authz shareAuthorizer) *SignalGate {
	return &SignalGate{
		agentID:  agentID,
		now:      time.Now,
		authz:    authz,
		seen:     map[string]nonceUse{},
		applied:  map[string]time.Time{},
		perShare: map[string]int{},
	}
}

func (g *SignalGate) SetLockdown(on bool) {
	g.mu.Lock()
	g.lockdown = on
	g.mu.Unlock()
}

// Reset clears all gate state for a new epoch. Sequence numbers, nonces,
// applied leases, and the rate-limit window all restart, so a signal carrying
// a low sequence number is once again admissible.
func (g *SignalGate) Reset() {
	g.mu.Lock()
	g.highest = 0
	g.seen = map[string]nonceUse{}
	g.applied = map[string]time.Time{}
	g.winStart = time.Time{}
	g.winCount = 0
	g.perShare = map[string]int{}
	g.mu.Unlock()
}

// VerifyNonce reports whether nonce was recently admitted for shareID, pruning
// expired entries so an idle agent cannot echo an old nonce indefinitely.
func (g *SignalGate) VerifyNonce(nonce, shareID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	for n, u := range g.seen {
		if now.Sub(u.seenAt) > nonceRetention {
			delete(g.seen, n)
		}
	}
	u, ok := g.seen[nonce]
	return ok && u.shareID == shareID
}

// Admit validates one open signal. It returns nil when the signal may proceed
// (the caller then calls OnDemandPort.OpenFor, which refreshes the lease if the
// share is already applied, making the signal idempotent).
func (g *SignalGate) Admit(sig OpenSignal) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()

	if sig.Version != signalVersion {
		return fmt.Errorf("%w: got %d want %d", ErrBadVersion, sig.Version, signalVersion)
	}
	if sig.AgentID != g.agentID {
		return ErrWrongAgent
	}
	if !sig.ExpiresAt.After(now) {
		return ErrExpiredSignal
	}
	if sig.ExpiresAt.After(now.Add(maxSignalLifetime)) {
		return fmt.Errorf("%w: expiry too far in the future", ErrBadLifetime)
	}
	if sig.Lease <= 0 || sig.Lease > maxLease {
		return fmt.Errorf("%w: lease %s (max %s)", ErrBadLease, sig.Lease, maxLease)
	}
	if sig.RouteKind != RouteDirect {
		return fmt.Errorf("%w: route %q cannot open the direct public port", ErrWrongRouteKind, sig.RouteKind)
	}
	// Reject replayed or out-of-order signals. The wire protocol has no
	// explicit ordering field beyond Seq, so an older, previously-unseen,
	// unexpired signal must not stay admissible after a newer one.
	if sig.Seq <= g.highest {
		return ErrReplaySignal
	}
	if u, ok := g.seen[sig.Nonce]; ok {
		if u.shareID != sig.ShareID || u.kind != sig.RouteKind {
			return ErrNonceReuse
		}
		return ErrReplaySignal
	}
	if g.lockdown {
		return ErrSignalLockdown
	}
	if !g.authz(sig.ShareID, sig.RouteKind) {
		return ErrSignalNotAuth
	}
	if err := g.rateLimit(now, sig.ShareID); err != nil {
		return err
	}

	g.seen[sig.Nonce] = nonceUse{shareID: sig.ShareID, kind: sig.RouteKind, seenAt: now}
	g.applied[sig.ShareID+":"+string(sig.RouteKind)] = now
	g.highest = sig.Seq

	// Best-effort prune of expired nonce entries to bound memory. Retention is
	// maxSignalLifetime + skew so a valid signal's nonce is remembered through
	// its full validity window (and a little past, to absorb clock skew).
	for nonce, u := range g.seen {
		if now.Sub(u.seenAt) > nonceRetention {
			delete(g.seen, nonce)
		}
	}
	return nil
}

func (g *SignalGate) rateLimit(now time.Time, shareID string) error {
	if now.Sub(g.winStart) >= signalWindow {
		g.winStart = now
		g.winCount = 0
		g.perShare = map[string]int{}
	}
	if g.winCount >= maxSignalsPerWin {
		return ErrSignalRate
	}
	g.winCount++
	g.perShare[shareID]++
	return nil
}
