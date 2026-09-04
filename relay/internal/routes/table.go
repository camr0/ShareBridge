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
	"strings"
	"sync"
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
// is currently online (spec §7.3). The leased presence registry arrives in
// Task 14; until then tests stub this interface, and a nil Presence fails
// every lookup closed.
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
	// in particular any wildcard.
	ErrInvalidHostname = errors.New("routes: relay hostname is not an exact hostname")
)

// Table is the in-memory §8 route map. The second §8 map (agent record →
// tunnel generation, presence expiry, relay port) is the Task 14 presence
// registry, which the table joins through the Presence interface. Safe for
// concurrent use by every public connection and the control sync.
type Table struct {
	mu       sync.RWMutex
	byHost   map[string]Route
	presence Presence
}

// NewTable returns a route table that joins lookups against presence. A nil
// presence fails every lookup closed.
func NewTable(presence Presence) *Table {
	return &Table{
		byHost:   make(map[string]Route),
		presence: presence,
	}
}

// Apply inserts or updates one route from a control delta or snapshot. The
// hostname is normalized once here — never on the lookup path. A delta whose
// revision does not supersede the stored one is ignored and reported as
// ErrStaleRevision without mutating anything, so route state can never
// regress (spec §8, §11.3). Applying an inactive route stores a tombstone
// that fences the hostname against stale revivals.
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
	return nil
}

// Lookup resolves an exact relay hostname — already normalized by the
// caller, never normalized here — to its routable route entry. It fails
// closed unless the route exists, is active, and tunnel presence matches the
// route's agent record, relay port, and generation exactly (spec §8
// conditions 2–3, §15.7).
func (table *Table) Lookup(hostname string) (Route, error) {
	table.mu.RLock()
	route, ok := table.byHost[hostname]
	table.mu.RUnlock()
	if !ok {
		return Route{}, fmt.Errorf("routes: lookup %q: %w", hostname, ErrRouteNotFound)
	}
	if !route.Active {
		return Route{}, fmt.Errorf("routes: lookup %q: %w", hostname, ErrRouteInactive)
	}
	if table.presence == nil || !table.presence.Online(route.AgentRecordID, route.RelayPort, route.Generation) {
		return Route{}, fmt.Errorf("routes: lookup %q (agent %q, port %d, generation %d): %w",
			hostname, route.AgentRecordID, route.RelayPort, route.Generation, ErrPresenceAbsent)
	}
	return route, nil
}

// normalizeHostname lowercases and strips one trailing dot, then refuses
// anything that is not a plain exact hostname — in particular any wildcard —
// so the table can never hold or serve a pattern (spec §6, §16.2).
func normalizeHostname(hostname string) (string, error) {
	normalized := strings.ToLower(hostname)
	normalized = strings.TrimSuffix(normalized, ".")
	if normalized == "" {
		return "", fmt.Errorf("routes: empty relay hostname: %w", ErrInvalidHostname)
	}
	if strings.ContainsAny(normalized, "*?") {
		return "", fmt.Errorf("routes: relay hostname %q contains a wildcard: %w", hostname, ErrInvalidHostname)
	}
	return normalized, nil
}
