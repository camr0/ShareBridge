// Package routes keeps the gateway's authoritative in-memory route table
// (spec §8): an exact relay-hostname map joined against tunnel presence by
// agent record, relay port, and generation. Routing is exact-match only —
// there is no wildcard matching anywhere — and every lookup fails closed on
// unknown, tombstoned, or presence-mismatched hostnames (spec §6, §15.7,
// §16.2).
package routes

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Route is one control-distributed exact relay route (spec §8). Its shape
// mirrors the control-side relayctl.Route DTO planned for the §11.3 internal
// protocol but stays deliberately decoupled from it.
type Route struct {
	// Hostname is the exact relay hostname, normalized (lowercase, no
	// trailing dot) once at insertion.
	Hostname string
	// AgentRecordID is the agent record the route serves.
	AgentRecordID string
	// RelayPort is the agent's assigned gateway-loopback FRP port.
	RelayPort int
	// Generation is the tunnel generation the route is bound to.
	Generation uint64
	// SessionID is the shared content session the route exposes.
	SessionID string
	// Revision is the monotonic route revision from control; stale
	// revisions never mutate the table.
	Revision uint64
	// Active is false for tombstoned routes, which never route again.
	Active bool
	// Limits carries the §14 concurrency ceilings control attached to the
	// route. The gateway applies spec defaults where a ceiling is unset.
	Limits Limits
}

// Limits carries the §14 concurrency ceilings distributed with a route.
type Limits struct {
	// MaxStreamsPerOrigin caps concurrent streams on one exact origin.
	MaxStreamsPerOrigin int
	// MaxStreamsPerAgent caps concurrent streams across one agent's routes.
	MaxStreamsPerAgent int
	// MaxStreamsGlobal caps concurrent streams for the whole gateway.
	MaxStreamsGlobal int
}

// Presence reports whether the tunnel for one agent/port/generation join key
// is currently online (spec §7.3). presence.Registry implements this for the
// gateway (Task 14): online means a valid Login plus an authorized NewProxy
// plus a probe-confirmed current-generation NewUserConn, leased 45 seconds
// and renewed only by authenticated current-generation Pings. Online answers
// from in-memory state without blocking and never re-enters the streams
// registry — Streams.RegisterAdmitted calls it under its own lock — and a
// nil Presence still fails every lookup closed (Task 34 owns the wiring).
// Join domain: facts enter the registry as non-negative int credential
// generations range-checked at the frpplugin fact boundary (the Task 13
// uint64↔int carry-forward), so a route whose uint64 generation exceeds
// math.MaxInt64 can never join and always fails closed.
type Presence interface {
	Online(agentRecordID string, relayPort int, generation uint64) bool
}

// Failure classes. The gateway closes public connections generically on any
// of them (spec §8); the distinctions exist for logs, metrics, and tests.
var (
	// ErrRouteNotFound covers hostnames with no route entry.
	ErrRouteNotFound = errors.New("routes: no such route")
	// ErrRouteInactive covers tombstoned or snapshot-inactive routes.
	ErrRouteInactive = errors.New("routes: route is inactive")
	// ErrPresenceAbsent covers routes whose agent/port/generation join key
	// has no matching tunnel presence.
	ErrPresenceAbsent = errors.New("routes: tunnel presence does not match the route")
	// ErrStaleRevision covers deltas whose revision does not supersede the
	// stored one; callers treat it as an applied no-op.
	ErrStaleRevision = errors.New("routes: stale route revision")
	// ErrInvalidHostname covers hostnames that are not plain exact names,
	// in particular any wildcard, and any name whose labels violate RFC 1123.
	ErrInvalidHostname = errors.New("routes: relay hostname is not an exact hostname")
	// ErrRouteLeaseExpired covers active routes whose finite §14 route lease
	// elapsed without a control refresh. Lease expiry blocks NEW connections
	// only (§15.4): it never closes or drains established streams — only an
	// explicit revoke or lockdown does that (§15.6).
	ErrRouteLeaseExpired = errors.New("routes: route lease expired")
)

// RouteLeaseTTL is the §14 gateway route lease: a route's new-connection
// eligibility expires this long after its last applied route update. Control
// refreshes every route every 30 seconds while sync is healthy, so a healthy
// sync never lets a live route expire; when control cannot renew route state,
// each route stays routable for at most one lease beyond its last touch.
const RouteLeaseTTL = 120 * time.Second

const (
	maxHostnameBytes = 253
	maxLabelBytes    = 63
)

// Table is the in-memory §8 route map. The second §8 map (agent record →
// tunnel generation, presence expiry, relay port) is the Task 14 presence
// registry, which the table joins through the Presence interface. Safe for
// concurrent use by every public connection and the control sync.
// Each active route additionally carries a finite lease (RouteLeaseTTL): the
// lease is (re)started by every applied route update — full snapshot, add,
// or limit delta — and its expiry blocks only new lookups. The lease lives
// beside the route entry, never inside Route, so route values stay plain
// comparable state.
type Table struct {
	mu       sync.RWMutex
	byHost   map[string]Route
	leases   map[string]time.Time
	presence Presence
	now      func() time.Time
}

// Option configures an optional Table collaborator.
type Option func(*Table)

// WithClock overrides the wall clock the lease bookkeeping reads. Tests use
// it to make the 120-second lease deterministic; production code should not.
func WithClock(now func() time.Time) Option {
	return func(table *Table) {
		if now != nil {
			table.now = now
		}
	}
}

// NewTable returns a route table that joins lookups against presence. A nil
// presence fails every lookup closed.
func NewTable(presence Presence, options ...Option) *Table {
	table := &Table{
		byHost:   make(map[string]Route),
		leases:   make(map[string]time.Time),
		presence: presence,
		now:      time.Now,
	}
	for _, option := range options {
		option(table)
	}
	return table
}

// Apply inserts or updates one route from a control delta or snapshot. The
// hostname is normalized once here — never on the lookup path. A delta whose
// revision does not supersede the stored one is ignored and reported as
// ErrStaleRevision without mutating anything, so route state can never
// regress (spec §8, §11.3); callers (the Task 13 applier) treat that as an
// idempotent success. Applying an inactive route stores a tombstone that
// fences the hostname against stale revivals. Applying an active route
// (re)starts its finite §14 route lease.
func (table *Table) Apply(route Route) error {
	hostname, err := normalizeHostname(route.Hostname)
	if err != nil {
		return err
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	if existing, ok := table.byHost[hostname]; ok && route.Revision <= existing.Revision {
		return fmt.Errorf("routes: revision %d does not supersede %d for %q: %w",
			route.Revision, existing.Revision, hostname, ErrStaleRevision)
	}
	route.Hostname = hostname
	table.byHost[hostname] = route
	table.renewLeaseLocked(hostname, route.Active)
	return nil
}

// Revoke tombstones the route for hostname at the given revision so the
// hostname stops routing immediately. Stale revocations are reported as
// ErrStaleRevision and ignored; revoking an unknown hostname is reported as
// ErrRouteNotFound so revision gaps surface instead of being guessed away
// (spec §15.7).
func (table *Table) Revoke(hostname string, revision uint64) error {
	normalized, err := normalizeHostname(hostname)
	if err != nil {
		return err
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	existing, ok := table.byHost[normalized]
	if !ok {
		return fmt.Errorf("routes: revoke for unknown hostname %q: %w", normalized, ErrRouteNotFound)
	}
	if revision <= existing.Revision {
		return fmt.Errorf("routes: revoke revision %d does not supersede %d for %q: %w",
			revision, existing.Revision, normalized, ErrStaleRevision)
	}
	existing.Revision = revision
	existing.Active = false
	table.byHost[normalized] = existing
	delete(table.leases, normalized) // a tombstone never needs a lease
	return nil
}

// Peek returns the stored route entry for hostname — normalized exactly as
// Apply/Revoke would — without checking its lease, presence, or active flag.
// It exists so the Task 13 applier can distinguish a stale-revision revoke
// against a still-ACTIVE route (divergence: must reconcile, never an
// idempotent no-op — R2, Sol Critical-2) from one against an already-
// tombstoned route (desired end state holds). The result is a point-in-time
// read: the applier's sync loop is the single writer, and the caller treats
// a concurrently-changed entry as the next reconcile's business.
func (table *Table) Peek(hostname string) (Route, bool) {
	normalized, err := normalizeHostname(hostname)
	if err != nil {
		return Route{}, false
	}
	table.mu.RLock()
	defer table.mu.RUnlock()
	route, ok := table.byHost[normalized]
	return route, ok
}

// ReplaceSnapshot atomically replaces the whole table with a full control
// snapshot (spec §8, §15.1): the next lookup observes either the entire
// previous state or the entire new one — never a partial blend. It returns
// the hostnames that were live in the table and are gone from the snapshot's
// live set (omitted, or superseded by a tombstone): control's full snapshot
// is the authoritative route set, so such routes stop routing immediately
// and the caller must drain their established streams AFTER this call
// returns — the ordering invariant documented on Streams.RegisterAdmitted is
// structural here, because the mutation is committed before the dropped set
// is even returned. Dropped hostnames are sorted for deterministic handling.
//
// A snapshot entry whose revision does not supersede the stored entry keeps
// the stored entry without error (idempotent re-push; the SDD carry-forward
// for Task 13): within one control epoch, control re-derives routes it
// never tracked at fresh, possibly lower revisions, and the newer
// gateway-held state must survive. Entries whose hostname is not a
// well-formed exact hostname are skipped (treated as absent) rather than
// failing the whole snapshot; the Task 13 applier validates before admitting
// and reports them, so this is defense in depth. Every live entry the
// snapshot affirms gets a fresh §14 lease.
func (table *Table) ReplaceSnapshot(incoming []Route) (dropped []string) {
	return table.replaceSnapshot(incoming, false)
}

// ReplaceSnapshotFromNewEpoch atomically replaces the whole table with a
// full control snapshot from a NEWER control epoch (R2, §15.1): the epoch is
// the authority, so per-route stored revisions — published by older control
// boots — carry no weight and every incoming entry replaces its stored
// counterpart even at a lower revision (per-route revisions restart within
// an epoch). This is what dissolves the cross-restart revision-regression
// hazard: revokes issued by the new epoch are always effective against
// pre-restart state. Dropped-set, lease, and drain semantics are exactly
// ReplaceSnapshot's.
func (table *Table) ReplaceSnapshotFromNewEpoch(incoming []Route) (dropped []string) {
	return table.replaceSnapshot(incoming, true)
}

// replaceSnapshot is the shared atomic-replacement body. Caller chooses
// whether stored revisions survive (same-epoch re-push) or not (new-epoch
// wholesale authority). Caller holds no locks.
func (table *Table) replaceSnapshot(incoming []Route, fromNewEpoch bool) (dropped []string) {
	nextHost := make(map[string]Route, len(incoming))
	nextLeases := make(map[string]time.Time, len(incoming))
	for _, route := range incoming {
		hostname, err := normalizeHostname(route.Hostname)
		if err != nil {
			continue
		}
		route.Hostname = hostname
		nextHost[hostname] = route
	}

	table.mu.Lock()
	now := table.now()
	// Resolve stale entries against the stored state before anything is
	// committed: a superseded incoming entry keeps the stored route — within
	// the same epoch only (a newer epoch's snapshot replaces wholesale).
	if !fromNewEpoch {
		for hostname, stored := range table.byHost {
			arrival, ok := nextHost[hostname]
			if !ok || arrival.Revision > stored.Revision {
				continue
			}
			nextHost[hostname] = stored
			if stored.Active {
				nextLeases[hostname] = now.Add(RouteLeaseTTL) // the snapshot re-affirmed it
			}
		}
	}
	for hostname, route := range nextHost {
		if route.Active {
			nextLeases[hostname] = now.Add(RouteLeaseTTL)
		}
	}
	// Compute the dropped set from the committed transition: stored-live →
	// absent-or-tombstoned.
	for hostname, stored := range table.byHost {
		if !stored.Active {
			continue
		}
		if arrival, ok := nextHost[hostname]; !ok || !arrival.Active {
			dropped = append(dropped, hostname)
		}
	}
	table.byHost = nextHost
	table.leases = nextLeases
	table.mu.Unlock()
	sort.Strings(dropped)
	return dropped
}

// Lookup resolves an exact relay hostname — already normalized by the
// caller, never normalized here — to its routable route entry. It fails
// closed unless the route exists, is active, and tunnel presence matches the
// route's agent record, relay port, and generation exactly (spec §8
// conditions 2–3, §15.7).
func (table *Table) Lookup(hostname string) (Route, error) {
	table.mu.RLock()
	route, ok := table.byHost[hostname]
	expiry, leased := table.leases[hostname]
	table.mu.RUnlock()
	if !ok {
		return Route{}, fmt.Errorf("routes: lookup %q: %w", hostname, ErrRouteNotFound)
	}
	if !route.Active {
		return Route{}, fmt.Errorf("routes: lookup %q: %w", hostname, ErrRouteInactive)
	}
	// Finite §14 route lease: without a control refresh inside RouteLeaseTTL
	// the route blocks NEW connections (fail closed at exactly the boundary),
	// while established streams are untouched (§15.4).
	if !leased || !table.now().Before(expiry) {
		return Route{}, fmt.Errorf("routes: lookup %q: %w", hostname, ErrRouteLeaseExpired)
	}
	if table.presence == nil || !table.presence.Online(route.AgentRecordID, route.RelayPort, route.Generation) {
		return Route{}, fmt.Errorf("routes: lookup %q (agent %q, port %d, generation %d): %w",
			hostname, route.AgentRecordID, route.RelayPort, route.Generation, ErrPresenceAbsent)
	}
	return route, nil
}

// renewLeaseLocked (re)starts or clears the §14 lease for hostname. Caller
// holds table.mu.
func (table *Table) renewLeaseLocked(hostname string, active bool) {
	if active {
		table.leases[hostname] = table.now().Add(RouteLeaseTTL)
	} else {
		delete(table.leases, hostname)
	}
}

// ValidateRelayHostname normalizes an exact relay hostname exactly the way
// the table does at insertion — lowercase, one trailing dot stripped — and
// reports whether the result is a well-formed exact name: RFC 1123 labels
// (1–63 bytes of [a-z0-9] with interior hyphens only), total length ≤ 253,
// and no wildcard anywhere. It returns the normalized form for callers that
// must validate a route before admitting it (the Task 13 applier). The §6
// namespace binding — <origin>.relay.<namespace>.<zone> — is deployment
// knowledge the table does not hold; the applier enforces it on top.
func ValidateRelayHostname(hostname string) (string, error) {
	return normalizeHostname(hostname)
}

// normalizeHostname lowercases and strips one trailing dot, then refuses
// anything that is not a plain exact hostname — in particular any wildcard,
// and any name whose labels violate RFC 1123 — so the table can never hold
// or serve a pattern (spec §6, §16.2).
func normalizeHostname(hostname string) (string, error) {
	normalized := strings.ToLower(hostname)
	normalized = strings.TrimSuffix(normalized, ".")
	if normalized == "" {
		return "", fmt.Errorf("routes: empty relay hostname: %w", ErrInvalidHostname)
	}
	if strings.ContainsAny(normalized, "*?") {
		return "", fmt.Errorf("routes: relay hostname %q contains a wildcard: %w", hostname, ErrInvalidHostname)
	}
	if len(normalized) > maxHostnameBytes {
		return "", fmt.Errorf("routes: relay hostname %q exceeds %d bytes: %w", hostname, maxHostnameBytes, ErrInvalidHostname)
	}
	for _, label := range strings.Split(normalized, ".") {
		if err := validateLabel(label); err != nil {
			return "", fmt.Errorf("routes: relay hostname %q has an invalid label %q: %w", hostname, label, err)
		}
	}
	return normalized, nil
}

// validateLabel enforces one RFC 1123 hostname label: 1–63 bytes of ASCII
// letters, digits, and interior hyphens (no leading or trailing hyphen).
func validateLabel(label string) error {
	if label == "" || len(label) > maxLabelBytes {
		return fmt.Errorf("label length %d outside 1..%d", len(label), maxLabelBytes)
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return errors.New("label has a leading or trailing hyphen")
	}
	for index := 0; index < len(label); index++ {
		char := label[index]
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
			return fmt.Errorf("label contains byte %#02x outside [a-z0-9-]", char)
		}
	}
	return nil
}
