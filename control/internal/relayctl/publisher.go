// Task 12 route publisher (plan Task 12; spec §§6, 8, 11.3, 15.7): the
// control-side RouteSource/StatusSink backend that turns PocketBase rows
// into the exact relay route snapshot and the ordered add/revoke/limit
// delta stream, and gates the 30-second route-lease refresh on explicit
// gateway acknowledgements.
//
// Source-of-truth rules (review mandate): every origin/agent/session field
// is derived from PocketBase rows — the persisted sessions.origin (relay
// origin via RelayOriginFromDirect) joined with the agents row's stable
// relay assignment — never from an agent message or HTTP parameter. A
// session without a usable origin or without a nonzero relay_port
// assignment yields NO route: absent, never partially routable (§15.7).
// Supported shares follow §13.3: public unprotected Immich rows INCLUDING
// relay_only ones — §12 makes sessions.relay_only routable and §6.1/§9.1
// keep its exclusion on the selection side, so its exact relay route is
// published like any other supported share's.
//
// Retention and health: the delta history kept for `GET ?since=` is bounded
// by an explicit window (DefaultDeltaRetentionWindow) and a count cap
// (DefaultMaxRetainedDeltas); a `since` older than the retained horizon is
// answered with a gap page — never stale deltas — forcing the gateway to
// reconcile from a full snapshot (§11.3: neither side guesses across a
// revision gap). Route leases are §14-finite (120 s on the gateway, Task
// 13): this publisher re-touches every active route with a route_limit
// delta every 30 seconds while sync is healthy, which is also the only
// writer of limit values — and limit values only ever tighten within a
// lease epoch (between a route's add and its revoke); widening requires a
// fresh registration.
//
// The publisher is safe for concurrent use: the sync server calls
// RouteSnapshot/RouteDeltas/Acknowledge from its handler goroutines while
// the agent WebSocket handlers call PublishAdd/PublishRevoke and the Run
// loop refreshes leases.
package relayctl

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"
)

// §14 lease and retention policy. RouteLeaseTTL is enforced gateway-side
// (Task 13: new-connection eligibility expires 120 seconds after the last
// applied route-touching delta); the publisher's contract is to refresh
// every DefaultLeaseRefreshInterval while sync is healthy.
const (
	// RouteLeaseTTL is the §14 gateway route lease. Documented here because
	// the refresh cadence is derived from it; enforced by the gateway.
	RouteLeaseTTL = 120 * time.Second
	// DefaultLeaseRefreshInterval is the §14 refresh cadence: well inside
	// RouteLeaseTTL so a single missed refresh never expires a lease.
	DefaultLeaseRefreshInterval = 30 * time.Second
	// DefaultDeltaRetentionWindow bounds how long a published delta stays
	// answerable for `GET ?since=`. It is 10× the 30-second refresh cadence:
	// a gateway on the private sync network realistically re-fetches deltas
	// within seconds, and even a gateway process restart plus re-auth
	// (§15.1) completes far inside five minutes — beyond that, incremental
	// catch-up is the wrong tool and the snapshot refetch (cheap,
	// authoritative, and what the gateway does on every boot anyway) is the
	// correct recovery. The window therefore never has to grow with
	// deployment size and is deliberately NOT unbounded.
	DefaultDeltaRetentionWindow = 5 * time.Minute
	// DefaultMaxRetainedDeltas caps the delta ring by count. It equals
	// MaxDeltasPerPage so the entire retained history always fits in ONE
	// delta page: a gap can then only ever be caused by the retention
	// window, never by page splitting.
	DefaultMaxRetainedDeltas = MaxDeltasPerPage
	// DefaultMaxPublisherRoutes bounds one snapshot; above it the publisher
	// fails closed (500) rather than silently truncating routes.
	DefaultMaxPublisherRoutes = MaxRoutesPerSnapshot
)

// DefaultRouteLimits returns the deterministic §14 concurrency ceilings the
// publisher attaches to every route: 32 streams per exact origin, 64 per
// agent, 8192 per gateway process. Values are configuration, not protocol
// constants; the publisher never invents values beyond what it is
// configured with.
func DefaultRouteLimits() Limits {
	return Limits{MaxStreamsPerOrigin: 32, MaxStreamsPerAgent: 64, MaxStreamsGlobal: 8192}
}

// PublisherConfig configures a Publisher. Zero durations/counts select the
// §14 defaults; negative values and non-positive limit values are rejected.
// RevisionSeed zero starts the monotonic route-revision counter at the
// current Unix time (seconds). Revision monotonicity holds ONLY within one
// control epoch: across restarts the per-boot control epoch (Epoch below)
// is the authority that makes new snapshots wholesale-adoptable regardless
// of revision regression, so the seed needs no cross-restart guarantees
// (R2, ruling 6). Tests inject small fixed seeds.
type PublisherConfig struct {
	// Limits attaches deterministic concurrency ceilings to every route
	// (DefaultRouteLimits when left zero). Within a lease epoch these can
	// only ever be tightened, never widened.
	Limits Limits
	// RevisionSeed starts the monotonic revision counter.
	RevisionSeed uint64
	// Epoch is the control epoch (boot identifier) stamped on every
	// snapshot, delta page, and acknowledgement this publisher serves. Zero
	// generates one from the boot time (UnixNano). The gateway compares
	// epochs — never revisions — to adopt a restarted control's state, and
	// acknowledgements from other epochs never satisfy sync health (R2).
	Epoch uint64
	// MaxRoutes bounds one snapshot (DefaultMaxPublisherRoutes).
	MaxRoutes int
	// DeltaRetentionWindow bounds the `since` history
	// (DefaultDeltaRetentionWindow).
	DeltaRetentionWindow time.Duration
	// MaxRetainedDeltas caps the delta ring (DefaultMaxRetainedDeltas).
	MaxRetainedDeltas int
	// LeaseRefreshInterval is the lease-refresh cadence
	// (DefaultLeaseRefreshInterval).
	LeaseRefreshInterval time.Duration
	// Now overrides the clock (test-only). nil means time.Now.
	Now func() time.Time
}

// routeIdentity is the §6/§8 route identity as derived from PocketBase
// rows: the relay hostname from the persisted direct origin plus the
// agent's stable assignment.
type routeIdentity struct {
	sessionID     string
	hostname      string
	agentRecordID string
	relayPort     int
	generation    uint64
}

// publisherRoute is the last published active route for one hostname.
type publisherRoute struct {
	route     Route
	revision  uint64
	touchedAt time.Time // last add/limit touch, drives stale-entry pruning
}

// retainedRouteDelta is one published delta inside the bounded ring.
type retainedRouteDelta struct {
	delta       RouteDelta
	publishedAt time.Time
}

// Publisher owns control's route-revision state and implements the Task 11
// RouteSource and StatusSink interfaces for the §11.3 sync server.
type Publisher struct {
	app               core.App
	limits            Limits
	maxRoutes         int
	retentionWindow   time.Duration
	maxRetainedDeltas int
	refreshInterval   time.Duration
	nowFn             func() time.Time
	logger            *slog.Logger

	mu sync.Mutex
	// epoch is the control boot identifier stamped on every served payload
	// (R2). Constant for the life of the process.
	epoch uint64
	// revision is the monotonic route-revision counter; it only moves
	// forward for the life of the process and is time-seeded. Its guarantee
	// is per-epoch only: cross-restart adoption is decided by epoch
	// comparison at the gateway, not by revisions (R2, ruling 6).
	revision uint64
	// lastPublishedAt is the wall time at which revision last advanced (or
	// the publisher's construction time for the initial revision). It is
	// stamped onto every snapshot/delta page as published_at so the gateway
	// can measure §17.3 route propagation lag without the timestamp ever
	// participating in ordering or adoption decisions.
	lastPublishedAt time.Time
	// entries tracks the last published active route per hostname. An
	// absent entry never implies absence on the gateway: after a control
	// restart the snapshot re-derives everything from PocketBase.
	entries map[string]publisherRoute
	// deltas is the bounded, revision-ordered retention ring (a contiguous
	// suffix of all published deltas).
	deltas []retainedRouteDelta
	// revokedAt records recent explicit revokes by hostname so a
	// concurrently-scheduled lease refresh can never resurrect a just-
	// revoked route from a pre-revoke snapshot of the DB rows. Entries age
	// out with the retention window.
	revokedAt map[string]time.Time
	// Acknowledgement state: which gateway boot last acked, and the last
	// revision it reported applying. Healthy() is exactly "every published
	// revision has been explicitly acknowledged by the current boot".
	ackBootID     string
	ackedRevision uint64
	hasAck        bool
}

// NewPublisher validates the configuration and returns a publisher over the
// given PocketBase app. It performs no I/O; queries run lazily per call.
func NewPublisher(app core.App, config PublisherConfig) (*Publisher, error) {
	if app == nil {
		return nil, fmt.Errorf("relayctl: publisher requires a PocketBase app")
	}
	limits := config.Limits
	if limits == (Limits{}) {
		limits = DefaultRouteLimits()
	}
	if limits.MaxStreamsPerOrigin <= 0 || limits.MaxStreamsPerAgent <= 0 || limits.MaxStreamsGlobal <= 0 {
		return nil, fmt.Errorf("relayctl: publisher limits must be positive, got %+v", limits)
	}
	maxRoutes, err := boundedValue(config.MaxRoutes, DefaultMaxPublisherRoutes)
	if err != nil {
		return nil, fmt.Errorf("relayctl: publisher max routes: %w", err)
	}
	if config.DeltaRetentionWindow < 0 {
		return nil, fmt.Errorf("relayctl: negative delta retention window")
	}
	if config.DeltaRetentionWindow == 0 {
		config.DeltaRetentionWindow = DefaultDeltaRetentionWindow
	}
	if config.MaxRetainedDeltas < 0 {
		return nil, fmt.Errorf("relayctl: negative max retained deltas")
	}
	if config.MaxRetainedDeltas == 0 {
		config.MaxRetainedDeltas = DefaultMaxRetainedDeltas
	}
	if config.LeaseRefreshInterval < 0 {
		return nil, fmt.Errorf("relayctl: negative lease refresh interval")
	}
	if config.LeaseRefreshInterval == 0 {
		config.LeaseRefreshInterval = DefaultLeaseRefreshInterval
	}
	revisionSeed := config.RevisionSeed
	if revisionSeed == 0 {
		revisionSeed = uint64(time.Now().UTC().Unix())
	}
	epoch := config.Epoch
	if epoch == 0 {
		epoch = uint64(time.Now().UTC().UnixNano())
	}
	nowFn := config.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	return &Publisher{
		app:               app,
		limits:            limits,
		maxRoutes:         maxRoutes,
		retentionWindow:   config.DeltaRetentionWindow,
		maxRetainedDeltas: config.MaxRetainedDeltas,
		refreshInterval:   config.LeaseRefreshInterval,
		nowFn:             nowFn,
		logger:            slog.Default(),
		epoch:             epoch,
		revision:          revisionSeed,
		lastPublishedAt:   nowFn(),
		entries:           make(map[string]publisherRoute),
		revokedAt:         make(map[string]time.Time),
	}, nil
}

// Epoch returns the control epoch (boot identifier) stamped on every
// snapshot, delta page, and acknowledgement this publisher serves.
func (p *Publisher) Epoch() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.epoch
}

// PublishAdd publishes the route_add delta for a session after its
// successful registration. Every field is re-derived from PocketBase rows
// (session row by record ID joined with the agent's persisted assignment);
// the caller supplies only the record ID. A session that is not an active
// supported public Immich share, without a persisted origin, or whose agent
// has no relay assignment yields no route (absent, never partially
// routable) and is logged, not errored; only database failures return an
// error. A registration failure never rolls back: the share is committed
// regardless, and the periodic snapshot/refresh reconciles.
func (p *Publisher) PublishAdd(sessionRecordID string) error {
	identity, routable, err := p.loadRouteIdentity(sessionRecordID, identityForAdd)
	if err != nil {
		return err
	}
	if !routable {
		return nil // absent by policy; loadRouteIdentity logged the reason
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.publishDeltaLocked(RouteOperationAdd, identity, p.limits, true)
}

// PublishRevoke publishes the route_revoke delta for a session before
// (or together with) its local lifecycle removal. It is deliberately not
// conditioned on tracked in-memory state: after a control restart the
// gateway may hold a route this process never published, so any derivable
// identity is revoked (revokes are idempotent tombstones at the gateway).
// Zeroed limits mirror the protocol golden payload for revokes.
func (p *Publisher) PublishRevoke(sessionRecordID string) error {
	identity, routable, err := p.loadRouteIdentity(sessionRecordID, identityForRevoke)
	if err != nil {
		return err
	}
	if !routable {
		return nil // was never routable; nothing to revoke
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.publishDeltaLocked(RouteOperationRevoke, identity, Limits{}, false)
}

// publishLimit publishes one route_limit delta for the identity, enforcing
// the review-mandated tighten-only rule: while a route's lease epoch is
// open (between its add and its revoke), a limit value may only stay equal
// or go down. Widening is refused without emitting anything; a fresh
// registration (revoke + add) resets the epoch. It is unexported because
// the only current writer is the lease refresh; Task 15+ consumers must go
// through a fresh registration to widen.
func (p *Publisher) publishLimit(identity routeIdentity, limits Limits) error {
	if limits.MaxStreamsPerOrigin < 0 || limits.MaxStreamsPerAgent < 0 || limits.MaxStreamsGlobal < 0 {
		return fmt.Errorf("%w: negative limit values %+v", ErrInvalidPayload, limits)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if entry, ok := p.entries[identity.hostname]; ok {
		if err := requireTightenOnly(entry.route.Limits, limits); err != nil {
			return err
		}
	}
	return p.publishDeltaLocked(RouteOperationLimit, identity, limits, true)
}

// requireTightenOnly rejects a limit set that widens any ceiling relative
// to the currently published values of the same route.
func requireTightenOnly(current, next Limits) error {
	if next.MaxStreamsPerOrigin > current.MaxStreamsPerOrigin ||
		next.MaxStreamsPerAgent > current.MaxStreamsPerAgent ||
		next.MaxStreamsGlobal > current.MaxStreamsGlobal {
		return fmt.Errorf("%w: limits %+v widen the published %+v within a lease epoch", ErrInvalidPayload, next, current)
	}
	return nil
}

// publishDeltaLocked appends one delta at the next revision and updates the
// tracked state. Caller holds p.mu. It fails closed on an active route whose
// ceilings are not all positive: zero is not a limit value (the gateway reads
// it as "unset" and restores the process default), so an active route may
// never carry one, and an inactive revoke tombstone never carries a
// meaningful limit. Validation runs before any mutation, so a refused delta
// emits nothing and leaves the revision and tracked state untouched.
func (p *Publisher) publishDeltaLocked(operation string, identity routeIdentity, limits Limits, active bool) error {
	if active && (limits.MaxStreamsPerOrigin <= 0 || limits.MaxStreamsPerAgent <= 0 || limits.MaxStreamsGlobal <= 0) {
		return fmt.Errorf("%w: active route limits must be positive, got %+v", ErrInvalidPayload, limits)
	}
	p.revision++
	revision := p.revision
	route := Route{
		Hostname:      identity.hostname,
		AgentRecordID: identity.agentRecordID,
		RelayPort:     identity.relayPort,
		Generation:    identity.generation,
		SessionID:     identity.sessionID,
		Revision:      revision,
		Active:        active,
		Limits:        limits,
	}
	p.deltas = append(p.deltas, retainedRouteDelta{
		delta:       RouteDelta{Revision: revision, Operation: operation, Route: route},
		publishedAt: p.nowFn(),
	})
	now := p.nowFn()
	p.lastPublishedAt = now
	if active {
		p.entries[identity.hostname] = publisherRoute{route: route, revision: revision, touchedAt: now}
		delete(p.revokedAt, identity.hostname)
	} else {
		delete(p.entries, identity.hostname)
		p.revokedAt[identity.hostname] = now
	}
	p.pruneLocked()
	return nil
}

// CurrentRevision returns control's current monotonic route revision — the
// revision every published delta so far is at or below, and the next delta
// will exceed. It is the route-currency term of the Task 15 presence view's
// Available join: a caller's route read is servable only while its revision
// is still current, so a route that moved (re-registered, claimed, revoked,
// or merely lease-refreshed) between the read and the predicate is refused
// fail-closed and the caller re-reads. Single lock read; no I/O. This
// accessor satisfies relayctl.RouteRevisionSource.
func (p *Publisher) CurrentRevision() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.revision
}

// RouteSnapshot implements RouteSource: the full §11.3 snapshot of every
// currently routable route, rebuilt from PocketBase. Routes never tracked
// since process start (registered before a control restart) enter the
// snapshot at the current revision; tracked routes carry their last
// published revision. The snapshot is deterministic for unchanged DB state
// (hostname-sorted) and performs no state mutation.
func (p *Publisher) RouteSnapshot() (Snapshot, error) {
	identities, err := p.queryPublishableRoutes()
	if err != nil {
		return Snapshot{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	routes := make([]Route, 0, len(identities))
	for _, identity := range identities {
		revision := p.revision
		if entry, ok := p.entries[identity.hostname]; ok {
			revision = entry.revision
		}
		routes = append(routes, Route{
			Hostname:      identity.hostname,
			AgentRecordID: identity.agentRecordID,
			RelayPort:     identity.relayPort,
			Generation:    identity.generation,
			SessionID:     identity.sessionID,
			Revision:      revision,
			Active:        true,
			Limits:        p.routeLimitsLocked(identity.hostname),
		})
	}
	sort.Slice(routes, func(a, b int) bool { return routes[a].Hostname < routes[b].Hostname })
	return Snapshot{Version: ProtocolVersion, Epoch: p.epoch, Revision: p.revision, Routes: routes, PublishedAt: formatPublishedAt(p.lastPublishedAt)}, nil
}

// routeLimitsLocked returns the limits a snapshot route must carry: the
// tracked (already published, tighten-bounded) values when present, the
// configured defaults otherwise. Caller holds p.mu.
func (p *Publisher) routeLimitsLocked(hostname string) Limits {
	if entry, ok := p.entries[hostname]; ok {
		return entry.route.Limits
	}
	return p.limits
}

// RouteDeltas implements RouteSource: the ordered delta page answering
// `since`, stamped with this control's epoch (R2: the gateway never applies
// deltas across an epoch boundary — a page from any other epoch than the one
// it last snapshotted forces reconciliation). The completeness rule is
// contiguity: an OK page is returned only
// when the ring still contains exactly the successor delta `since+1` (the
// ring is a contiguous revision suffix, so everything after it is present
// too); if the successor has aged out of the bounded retention — window or
// count — the deltas in (since, oldest] are irrevocably lost and the page
// is a gap with no deltas, forcing snapshot reconciliation (§11.3: neither
// side guesses across a revision gap). A `since` at the current revision
// yields an empty OK page; a `since` ahead of control's own revision is a
// hard protocol violation and fails closed.
func (p *Publisher) RouteDeltas(since uint64) (DeltaPage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if since > p.revision {
		return DeltaPage{}, fmt.Errorf("%w: requested since %d is ahead of control revision %d", ErrBadRevision, since, p.revision)
	}
	if since == p.revision {
		return DeltaPage{Version: ProtocolVersion, Epoch: p.epoch, Status: DeltaStatusOK, Since: since, LatestRevision: p.revision, PublishedAt: formatPublishedAt(p.lastPublishedAt)}, nil
	}
	page := DeltaPage{Version: ProtocolVersion, Epoch: p.epoch, Status: DeltaStatusGap, Since: since, LatestRevision: p.revision, PublishedAt: formatPublishedAt(p.lastPublishedAt)}
	collected := make([]RouteDelta, 0, len(p.deltas))
	latestPublishedAt := p.lastPublishedAt
	for _, retained := range p.deltas {
		if retained.delta.Revision > since {
			collected = append(collected, retained.delta)
			latestPublishedAt = retained.publishedAt
		}
	}
	if len(collected) > 0 && collected[0].Revision == since+1 {
		// The retained suffix starts exactly at the gateway's successor: the
		// page is complete, never a stale partial answer.
		page.Status = DeltaStatusOK
		page.Deltas = collected
		page.LatestRevision = collected[len(collected)-1].Revision
		page.PublishedAt = formatPublishedAt(latestPublishedAt)
	}
	return page, nil
}

// Acknowledge implements StatusSink: records the gateway's explicitly
// acknowledged last-applied revision and reports whether it was actually
// RECORDED. A new gateway boot ID replaces the recorded state wholesale
// (§15.1: a restarted gateway may report a lower revision again); the same
// boot only moves forward. Acks carrying another control's epoch are stale
// in-flight traffic across a control restart: they are refused with
// AckOutcome{Accepted:false, Reason:AckReasonForeignEpoch} and record
// nothing — they must never satisfy the current epoch's health watermark
// (R2), and the caller must be able to report the refusal truthfully rather
// than claim success. The gateway re-acks after reconciling to the new
// epoch's snapshot.
func (p *Publisher) Acknowledge(ack StatusAck) (AckOutcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ack.ControlEpoch != p.epoch {
		p.logger.Debug("relayctl: status ack for a foreign control epoch refused",
			"ack_epoch", ack.ControlEpoch, "epoch", p.epoch)
		return AckOutcome{Accepted: false, Reason: AckReasonForeignEpoch}, nil
	}
	if ack.GatewayBootID != p.ackBootID {
		p.ackBootID = ack.GatewayBootID
		p.ackedRevision = ack.LastAppliedRevision
		p.hasAck = true
		return AckOutcome{Accepted: true}, nil
	}
	if ack.LastAppliedRevision > p.ackedRevision {
		p.ackedRevision = ack.LastAppliedRevision
	}
	return AckOutcome{Accepted: true}, nil
}

// Healthy reports whether sync is healthy: the current gateway boot has
// explicitly acknowledged every revision published so far. The lease
// refresh runs only while healthy (§14 "refreshed every 30 seconds while
// control sync is healthy"); nothing here is derived from agent telemetry.
func (p *Publisher) Healthy() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.healthyLocked()
}

func (p *Publisher) healthyLocked() bool {
	return p.hasAck && p.ackedRevision >= p.revision
}

// AcknowledgedRevision reports the last route revision the current gateway
// boot has explicitly acknowledged applying within this control epoch, or 0
// before any acknowledgement. It is the route-currency term of the Task 15
// presence view's Available join under the RouteSyncStatus contract: unlike
// CurrentRevision (control's own publish watermark), this value proves what
// the GATEWAY actually holds — a route read is servable only at a revision
// the gateway has confirmed applying (R2, ruling 4; Sol Important-5). Single
// lock read; no I/O.
func (p *Publisher) AcknowledgedRevision() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.hasAck {
		return 0
	}
	return p.ackedRevision
}

// RefreshLeases re-touches every currently routable route with a
// route_limit delta at a fresh revision, restarting the gateway's finite
// 120-second lease (Task 13) for another period. It carries the tracked
// limit values verbatim — a refresh can never widen (tighten-only, review
// mandate). It no-ops while sync is unhealthy and skips hostnames revoked
// within the retention window so a refresh racing an explicit revoke can
// never resurrect it. Returns the number of routes touched.
func (p *Publisher) RefreshLeases() (int, error) {
	if !p.Healthy() {
		return 0, nil
	}
	identities, err := p.queryPublishableRoutes()
	if err != nil {
		return 0, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// Re-check under the lock: a concurrent publish/revoke may have made
	// sync unhealthy or moved revisions while the rows were being read.
	if !p.healthyLocked() {
		return 0, nil
	}
	now := p.nowFn()
	current := make(map[string]bool, len(identities))
	for _, identity := range identities {
		current[identity.hostname] = true
		if revoked, ok := p.revokedAt[identity.hostname]; ok && now.Sub(revoked) < p.retentionWindow {
			continue // explicit revoke wins over a pre-revoke read of the rows
		}
		limits := p.limits
		if entry, ok := p.entries[identity.hostname]; ok {
			limits = entry.route.Limits
			if err := requireTightenOnly(entry.route.Limits, limits); err != nil {
				return 0, err // unreachable; guards future limit-policy changes
			}
		}
		if err := p.publishDeltaLocked(RouteOperationLimit, identity, limits, true); err != nil {
			return 0, err // unreachable; every tracked/configured limit is positive
		}
	}
	// Drop tracked entries absent from the current DB-derived route set
	// once their last touch is older than the retention window: their
	// gateway lease has long expired (RouteLeaseTTL << window), the DB no
	// longer justifies them, and dropping keeps the map bounded. Entries
	// younger than the window are kept so a refresh that raced a just-
	// committed registration cannot orphan them.
	for hostname, entry := range p.entries {
		if !current[hostname] && now.Sub(entry.touchedAt) >= p.retentionWindow {
			delete(p.entries, hostname)
		}
	}
	return len(identities), nil
}

// Run drives the periodic lease refresh until the context is cancelled.
func (p *Publisher) Run(ctx context.Context) {
	ticker := time.NewTicker(p.refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			touched, err := p.RefreshLeases()
			if err != nil {
				p.logger.Warn("relayctl: route lease refresh failed", "error", err)
				continue
			}
			if touched > 0 {
				p.logger.Debug("relayctl: route leases refreshed", "routes", touched)
			}
		}
	}
}

// pruneLocked enforces the bounded retention: deltas leave the ring when
// they age past the window or the count cap is exceeded (oldest first),
// keeping the ring a contiguous revision suffix; revoke tombstones age out
// with the same window. Caller holds p.mu.
func (p *Publisher) pruneLocked() {
	cutoff := p.nowFn().Add(-p.retentionWindow)
	drop := 0
	for drop < len(p.deltas) && p.deltas[drop].publishedAt.Before(cutoff) {
		drop++
	}
	if drop > 0 {
		p.deltas = append(p.deltas[:0], p.deltas[drop:]...)
	}
	if excess := len(p.deltas) - p.maxRetainedDeltas; excess > 0 {
		p.deltas = append(p.deltas[:0], p.deltas[excess:]...)
	}
	for hostname, revoked := range p.revokedAt {
		if revoked.Before(cutoff) {
			delete(p.revokedAt, hostname)
		}
	}
}

// identityFilter selects the row filter variant: identityForAdd requires the
// full live-predicate set (active, supported, public, unexpired), while
// identityForRevoke drops the liveness filters so an already-removed row
// still yields the identity whose route must be tombstoned.
type identityFilter int

const (
	identityForAdd identityFilter = iota
	identityForRevoke
)

// sessionAgentJoin is one sessions⋈agents row as scanned from PocketBase.
// Nullable columns model "agent row gone / assignment missing".
type sessionAgentJoin struct {
	SessionID       string         `db:"session_id"`
	Origin          sql.NullString `db:"origin"`
	ExpiresAt       sql.NullString `db:"expires_at"`
	AgentRecordID   sql.NullString `db:"agent_record_id"`
	RelayPort       sql.NullInt64  `db:"relay_port"`
	RelayGeneration sql.NullInt64  `db:"relay_generation"`
}

// loadRouteIdentity derives the route identity of one session from
// PocketBase rows only. A missing origin, missing assignment, or (for the
// add filter) a row that is not an active supported public unexpired Immich
// session yields routable=false — the route is absent, never partially
// routable (§15.7). relay_only rows stay routable over relay (§12, §13.3);
// their exclusion is a §9.1 selection policy. Database failures return an
// error.
func (p *Publisher) loadRouteIdentity(sessionRecordID string, filter identityFilter) (routeIdentity, bool, error) {
	query := `SELECT s.id AS session_id, s.origin AS origin, s.expires_at AS expires_at,
		a.id AS agent_record_id, a.relay_port AS relay_port, a.relay_generation AS relay_generation
		FROM sessions s
		LEFT JOIN agents a ON a.api_key_id = s.api_key_id
		WHERE s.id = {:id}
		AND s.share_type = 'immich'
		AND COALESCE(s.is_password_protected, 0) = 0
		AND COALESCE(s.origin, '') != ''`
	if filter == identityForAdd {
		query += ` AND COALESCE(s.is_active, 0) = 1`
	}
	rows := []sessionAgentJoin{}
	err := p.app.DB().
		NewQuery(query).
		Bind(map[string]any{"id": sessionRecordID}).
		All(&rows)
	if err != nil {
		return routeIdentity{}, false, fmt.Errorf("relayctl: load session %s: %w", sessionRecordID, err)
	}
	if len(rows) == 0 {
		p.logger.Debug("relayctl: route not publishable from session row", "session", sessionRecordID)
		return routeIdentity{}, false, nil
	}
	return p.identityFromJoin(rows[0], filter == identityForAdd)
}

// queryPublishableRoutes derives the full DB-current route set for the
// snapshot and the lease refresh: active supported public unexpired Immich
// sessions (including relay_only, §12/§13.3) joined with their agents'
// assignments, hostname-sorted for determinism.
func (p *Publisher) queryPublishableRoutes() ([]routeIdentity, error) {
	rows := []sessionAgentJoin{}
	err := p.app.DB().NewQuery(`SELECT s.id AS session_id, s.origin AS origin, s.expires_at AS expires_at,
		a.id AS agent_record_id, a.relay_port AS relay_port, a.relay_generation AS relay_generation
		FROM sessions s
		LEFT JOIN agents a ON a.api_key_id = s.api_key_id
		WHERE COALESCE(s.is_active, 0) = 1
		AND s.share_type = 'immich'
		AND COALESCE(s.is_password_protected, 0) = 0
		AND COALESCE(s.origin, '') != ''
		ORDER BY s.id`).All(&rows)
	if err != nil {
		return nil, fmt.Errorf("relayctl: query publishable routes: %w", err)
	}
	if len(rows) > p.maxRoutes {
		return nil, fmt.Errorf("%w: %d publishable routes exceed the bound %d", ErrPayloadTooLarge, len(rows), p.maxRoutes)
	}
	identities := make([]routeIdentity, 0, len(rows))
	for _, row := range rows {
		identity, routable, err := p.identityFromJoin(row, true)
		if err != nil {
			return nil, err
		}
		if routable {
			identities = append(identities, identity)
		}
	}
	sort.Slice(identities, func(a, b int) bool { return identities[a].hostname < identities[b].hostname })
	return identities, nil
}

// identityFromJoin converts one joined row into a route identity, applying
// the §6 derivation and the assignment-presence rule. For live filtering
// (unexpired), a row past its expires_at is absent — route leases never
// outlive the share's own lifetime.
func (p *Publisher) identityFromJoin(row sessionAgentJoin, liveFiltered bool) (routeIdentity, bool, error) {
	if liveFiltered && row.ExpiresAt.Valid && row.ExpiresAt.String != "" {
		expiresAt, err := parsePocketBaseTime(row.ExpiresAt.String)
		if err != nil {
			return routeIdentity{}, false, fmt.Errorf("relayctl: session %s expiry %q: %w", row.SessionID, row.ExpiresAt.String, err)
		}
		if !expiresAt.IsZero() && !expiresAt.Time().After(p.nowFn()) {
			p.logger.Debug("relayctl: route not publishable: session expired", "session", row.SessionID)
			return routeIdentity{}, false, nil
		}
	}
	if !row.Origin.Valid || row.Origin.String == "" {
		return routeIdentity{}, false, nil
	}
	hostname, err := RelayOriginFromDirect(row.Origin.String)
	if err != nil {
		// A derivation failure means the persisted origin is not a direct
		// origin (never expected for control-allocated origins): absent,
		// and never an agent-influenced hostname (§11.1).
		p.logger.Warn("relayctl: relay origin derivation failed", "session", row.SessionID, "error", err)
		return routeIdentity{}, false, nil
	}
	if !row.AgentRecordID.Valid || row.AgentRecordID.String == "" {
		p.logger.Debug("relayctl: route not publishable: no agent row", "session", row.SessionID)
		return routeIdentity{}, false, nil
	}
	if !row.RelayPort.Valid || row.RelayPort.Int64 <= 0 {
		// Missing assignment ⇒ route absent, never partially routable.
		p.logger.Debug("relayctl: route not publishable: agent has no relay assignment", "session", row.SessionID)
		return routeIdentity{}, false, nil
	}
	if !row.RelayGeneration.Valid || row.RelayGeneration.Int64 < 0 {
		p.logger.Debug("relayctl: route not publishable: invalid relay generation", "session", row.SessionID)
		return routeIdentity{}, false, nil
	}
	return routeIdentity{
		sessionID:     row.SessionID,
		hostname:      hostname,
		agentRecordID: row.AgentRecordID.String,
		relayPort:     int(row.RelayPort.Int64),
		generation:    uint64(row.RelayGeneration.Int64),
	}, true, nil
}

// parsePocketBaseTime parses the datetime storage format PocketBase
// persists (types.ParseDateTime accepts both it and RFC 3339).
func parsePocketBaseTime(value string) (types.DateTime, error) {
	return types.ParseDateTime(value)
}
