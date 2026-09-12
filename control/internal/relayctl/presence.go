// Task 15 presence view (plan Task 15; spec §§4.2, 7.1, 7.3, 12, 15.7):
// control's ephemeral, in-memory leased view of gateway-authoritative tunnel
// presence. The view implements the Task 11 PresenceSink and is the ONLY
// presence state route selection may read (§4.2: the gateway is authoritative;
// agent-reported frpc/relay_client_state is telemetry, never a routing fact).
//
// Source of facts (auth/rejection review): the ONLY writer is the Task 11
// sync server's presence endpoints — a private-only, mutually authenticated
// TLS 1.3 listener pinned to the one configured gateway identity (server.go).
// Nothing else in control can mutate this view: the two Apply methods below
// are the entire write surface. The agent WebSocket handler's
// relay_client_state message never reaches this package (it stays log/UI
// telemetry, §7.4).
//
// Freshness policy (owned by this task):
//
//   - The gateway's presence lease is 45 s (relay presence.DefaultLeaseTTL),
//     renewed by an authenticated Ping every 10 s. The view accepts the
//     gateway-stated RFC 3339 expiry as the lease, bounded by its own
//     conservative cap DefaultMaxLeaseTTL = 60 s measured from receipt:
//     a lease must never be shorter than the authoritative 45 s source (a
//     shorter cap would flap availability during bounded sync delay), and
//     15 s of headroom covers private-network sync latency and modest clock
//     skew between the two hosts. An expiry beyond receipt + 60 s is
//     future-dated (a legitimate gateway lease cannot exceed now+45 s plus
//     transit) and rejects the batch fail-closed; the sync server answers 500
//     and the gateway reconciles from a full presence snapshot.
//
//   - Expired ⇒ unavailable: expiry is enforced lazily at Available time
//     against the caller's `now`, exactly like the gateway registry's lazy
//     transition. An online fact already expired at receipt grants nothing.
//
//   - Boot/revision discipline (§7.3, §15.1): the view records the boot ID
//     and last applied presence revision. A presence SNAPSHOT is the
//     reconcile primitive — the gateway posts it on boot/reconnect — and is
//     applied as wholesale atomic replacement from any boot (this is how a
//     restarted gateway, §15.1, and a control restart, §4.2, converge).
//     Event batches from a boot other than the recorded one are DISCARDED
//     (a boot control has not snapshotted is potentially superseded; the
//     snapshot will announce the new boot). On the recorded boot, stale
//     revisions (≤ last applied) are discarded as replays and a revision GAP
//     (successor > last+1) rejects the whole batch with ErrBadRevision and
//     zero mutation — control never guesses across a gap (§11.3); the sync
//     server's 500 drives the gateway to repost its snapshot.
//
//   - Per-agent fencing: the gateway keeps at most one live tunnel per agent
//     (one proxy per agent, replacement generations fence older ones), so the
//     view is keyed by agent record ID. An online fact for a strictly newer
//     generation replaces the lease (new assignment era; the port may move
//     with it — reassignment always bumps the generation). Facts for an
//     older generation, or a same-generation port contradiction, are
//     discarded fail-closed; superseded generations can never resurrect.
//     Generations above math.MaxInt64 are discarded at the wire boundary:
//     the persisted agents.relay_generation column is an int64, so such a
//     generation can never join a route (uint64→int range-check rule).
//
// Route-revision join (the Available matching rule): Available(agentID,
// relayPort, generation, routeRevision, now) is true only when (a) the view
// holds an unexpired lease for exactly (agentID, relayPort, generation), and
// (b) routeRevision equals the route publisher's CURRENT revision
// (RouteRevisionSource, implemented by *Publisher.CurrentRevision). Condition
// (b) is optimistic read-validation: a route whose revision moved between the
// caller's route read and this predicate — re-registration, claim, revoke,
// reassignment — must not be served against presence captured under the old
// read; the caller re-reads. It is deliberately global-revision strict (any
// published delta, including the periodic lease refresh, moves it): the false
// direction is fail-closed and self-heals on the caller's re-read, never
// fail-open. The predicate is read-only; it mutates nothing and never reads
// the persisted relay_last_seen_at diagnostic (§12: diagnostics only).
package relayctl

import (
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

// DefaultMaxLeaseTTL is the control-side conservative lease bound: the
// gateway's 45-second presence lease (§7.3) plus 15 seconds of sync-latency
// and clock-skew headroom. See the package comment for the full policy.
const DefaultMaxLeaseTTL = 60 * time.Second

// RouteRevisionSource supplies the route publisher's current revision for the
// Available route-revision join. *Publisher implements it; tests stub it.
type RouteRevisionSource interface {
	CurrentRevision() uint64
}

// RouteSyncStatus is the optional, richer contract a RouteRevisionSource may
// also satisfy — *Publisher does. When the injected source implements it,
// the Available join upgrades from "control published this revision" (the
// publisher watermark) to the gateway-authoritative proof (R2, ruling 4;
// Sol Important-5):
//
//   - AcknowledgedRevision is the last revision the gateway explicitly
//     confirmed applying; comparing against it (instead of the local
//     CurrentRevision watermark) means a route is servable only at a
//     revision the gateway verifiably holds.
//   - Healthy is the sync-health term (§14): while control↔gateway sync is
//     down — including between a control restart and the gateway's first
//     re-ack — nothing is relay-selectable (belt-and-braces fail-closed).
//
// Sources implementing only RouteRevisionSource keep the historical
// watermark join (test stubs; a conservative-safe comparison documented on
// Available).
type RouteSyncStatus interface {
	AcknowledgedRevision() uint64
	Healthy() bool
}

// PresenceViewConfig is the fail-closed construction contract for the view.
type PresenceViewConfig struct {
	// App persists the relay_last_seen_at diagnostic (§12). nil disables the
	// diagnostic write (availability is unaffected either way — the write is
	// best-effort and never an input).
	App core.App
	// Routes supplies the current route revision for the Available join.
	// nil makes Available fail closed (false) — a caller that cannot have
	// its route read validated is never served.
	Routes RouteRevisionSource
	// MaxLeaseTTL overrides the conservative lease bound. Must be positive;
	// zero selects DefaultMaxLeaseTTL.
	MaxLeaseTTL time.Duration
	// Now is the clock seam; tests inject a fake clock. nil means time.Now.
	Now func() time.Time
	// Logger receives bounded diagnostics (never payload contents, never
	// credential material). nil selects slog.Default.
	Logger *slog.Logger
}

// presenceLease is one agent's live tunnel lease as mirrored from the
// gateway: the exact (port, generation) join key and the leased expiry.
type presenceLease struct {
	relayPort  int
	generation uint64
	expiresAt  time.Time
}

// PresenceView is control's ephemeral relay availability view (Task 15). It
// implements PresenceSink and is safe for concurrent use: the sync server's
// handler goroutines call the Apply methods while route selection calls
// Available.
type PresenceView struct {
	app         core.App
	routes      RouteRevisionSource
	maxLeaseTTL time.Duration
	now         func() time.Time
	logger      *slog.Logger

	mu           sync.Mutex
	bootID       string
	lastRevision uint64
	haveBoot     bool
	leases       map[string]presenceLease
}

// NewPresenceView validates the configuration and returns the view.
func NewPresenceView(config PresenceViewConfig) (*PresenceView, error) {
	maxLeaseTTL := config.MaxLeaseTTL
	if maxLeaseTTL == 0 {
		maxLeaseTTL = DefaultMaxLeaseTTL
	}
	if maxLeaseTTL < 0 {
		return nil, fmt.Errorf("%w: negative presence lease bound", ErrInvalidPayload)
	}
	nowFn := config.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &PresenceView{
		app:         config.App,
		routes:      config.Routes,
		maxLeaseTTL: maxLeaseTTL,
		now:         nowFn,
		logger:      logger,
		leases:      make(map[string]presenceLease),
	}, nil
}

// ApplyPresenceSnapshot implements PresenceSink for the snapshot endpoint:
// the gateway's full current presence at one revision, applied as wholesale
// atomic replacement. This is the §4.2 control-restart load, the §15.1
// gateway-restart reset (the snapshot is empty until FRP clients re-register),
// the periodic ≤60 s renewal republish, and the reconcile path after any
// rejected event batch. A snapshot is accepted from any boot: the posting
// peer is the pinned gateway identity, and its snapshot IS the current truth
// (the same wholesale-replacement-on-new-boot posture the Task 11 ack records
// take).
//
// An EMPTY snapshot is also the boot-ADOPTION primitive when it carries the
// top-level GatewayBootID/Revision (task #16, ledger I4-partial): control
// records the reporting boot, so the fresh boot's first real events apply
// (§15.1) instead of being discarded as a superseded boot's replay. Without
// the boot identity an empty snapshot was indistinguishable from "the
// previously recorded boot now has no routes", and a restarted gateway could
// never become adoptable until it happened to post a non-empty snapshot —
// which it cannot do before its first tunnel confirms, and whose events control
// was discarding. A legacy empty snapshot (no boot identity) still clears every
// lease and leaves boot/revision tracking untouched.
func (view *PresenceView) ApplyPresenceSnapshot(envelope PresenceEnvelope) error {
	if err := ValidatePresenceSnapshotEnvelope(envelope, MaxPresenceEventsPerEnvelope); err != nil {
		return err
	}
	now := view.now()

	// Resolve the reporting boot: the top-level fields describe the snapshot's
	// boot even when it carries no events; an older build's envelope only
	// stamps them on the events.
	bootID := envelope.GatewayBootID
	revision := envelope.Revision
	if bootID == "" && len(envelope.Events) > 0 {
		bootID = envelope.Events[0].GatewayBootID
		revision = envelope.Events[0].Revision
	}

	// An empty snapshot is legitimate §15.1 state ("on gateway restart the
	// snapshot is empty until FRP clients reconnect and re-register"). When it
	// names its boot it is additionally the adoption primitive described above.
	if len(envelope.Events) == 0 {
		view.mu.Lock()
		view.leases = make(map[string]presenceLease)
		if bootID != "" {
			view.bootID = bootID
			view.lastRevision = revision
			view.haveBoot = true
		}
		view.mu.Unlock()
		return nil
	}

	// Pre-pass: validate every lease expiry BEFORE mutating anything so the
	// replacement is atomic — a single future-dated entry rejects the whole
	// snapshot (fail closed; the gateway reconciles).
	accepted := make([]PresenceEvent, 0, len(envelope.Events))
	for _, event := range envelope.Events {
		if _, _, err := view.leaseExpiry(event, now); err != nil {
			return err
		}
		accepted = append(accepted, event)
	}

	view.mu.Lock()
	leases := make(map[string]presenceLease, len(accepted))
	for _, event := range accepted {
		view.applyEventLocked(leases, event, now)
	}
	view.bootID = bootID
	view.lastRevision = revision
	view.haveBoot = true
	view.leases = leases
	observed := accepted
	view.mu.Unlock()

	view.recordLastSeen(observed)
	return nil
}

// ApplyPresenceEvents implements PresenceSink for the ordered event endpoint.
// Boot and revision discipline: batches from an unknown/older boot are
// discarded without error (the snapshot announces a new boot); stale
// revisions are discarded as replays; a revision gap rejects the whole batch
// with ErrBadRevision and zero mutation. Valid contiguous events apply with
// per-agent generation fencing.
func (view *PresenceView) ApplyPresenceEvents(envelope PresenceEnvelope) error {
	if err := ValidatePresenceEventEnvelope(envelope, MaxPresenceEventsPerEnvelope); err != nil {
		return err
	}
	now := view.now()

	view.mu.Lock()

	if !view.haveBoot || envelope.Events[0].GatewayBootID != view.bootID {
		// Never apply presence facts for a boot this view has not snapshotted:
		// the boot is either the old gateway's (superseded) or a new boot
		// whose announcing snapshot is in flight. Discard without error —
		// the snapshot post is the reconcile path (§15.1, §4.2).
		view.mu.Unlock()
		view.logger.Debug("relayctl: presence events discarded for unsnapshotted boot",
			"boot", envelope.Events[0].GatewayBootID)
		return nil
	}

	// Contiguity pre-pass (§11.3: neither side guesses across a revision
	// gap): every non-stale revision must be the exact successor of the
	// previous applied revision. A violation rejects the whole batch before
	// any mutation.
	last := view.lastRevision
	for _, event := range envelope.Events {
		if event.Revision <= last {
			continue // replay of an already-applied revision
		}
		if event.Revision != last+1 {
			view.mu.Unlock()
			return fmt.Errorf("%w: presence revision %d does not succeed %d", ErrBadRevision, event.Revision, last)
		}
		last = event.Revision
		// Fail-closed expiry validation must also happen before mutation.
		if _, _, err := view.leaseExpiry(event, now); err != nil {
			view.mu.Unlock()
			return err
		}
	}

	// Apply pass: stale events are skipped; successors apply in order.
	applied := make([]PresenceEvent, 0, len(envelope.Events))
	for _, event := range envelope.Events {
		if event.Revision <= view.lastRevision {
			continue
		}
		view.applyEventLocked(view.leases, event, now)
		view.lastRevision = event.Revision
		applied = append(applied, event)
	}
	observed := applied
	view.mu.Unlock()

	view.recordLastSeen(observed)
	return nil
}

// leaseExpiry resolves one event's leased expiry under the freshness policy:
// the gateway-stated expiry when plausible (online facts only — the protocol
// validation already guarantees online carries a parseable RFC 3339 expiry
// and offline carries none), rejected as future-dated beyond receipt +
// maxLeaseTTL, possibly already expired (≤ now), which grants no lease.
func (view *PresenceView) leaseExpiry(event PresenceEvent, now time.Time) (time.Time, bool, error) {
	if event.State != PresenceStateOnline {
		return time.Time{}, false, nil
	}
	expiresAt, err := time.Parse(time.RFC3339, event.LeaseExpiresAt)
	if err != nil {
		// Unreachable behind ValidatePresenceEvent; fail closed regardless.
		return time.Time{}, false, fmt.Errorf("%w: lease expiry %q is not RFC 3339", ErrInvalidPayload, event.LeaseExpiresAt)
	}
	if expiresAt.After(now.Add(view.maxLeaseTTL)) {
		return time.Time{}, false, fmt.Errorf("%w: lease expiry %s is future-dated beyond the %s control bound",
			ErrInvalidPayload, expiresAt.Format(time.RFC3339), view.maxLeaseTTL)
	}
	return expiresAt, expiresAt.After(now), nil
}

// applyEventLocked folds one event into the lease map, applying the per-agent
// fencing rules. Caller holds view.mu; the map argument is the live map (a
// snapshot apply builds its replacement map before installing it).
func (view *PresenceView) applyEventLocked(leases map[string]presenceLease, event PresenceEvent, now time.Time) {
	// Wire-boundary range check (uint64 → persisted int64 domain): the
	// agents.relay_generation column is an int64, so a generation above
	// MaxInt64 can never join a route. Discard fail-closed.
	if event.Generation > math.MaxInt64 {
		return
	}
	existing, ok := leases[event.AgentRecordID]
	expiresAt, stillValid, _ := view.leaseExpiry(event, now)
	switch event.State {
	case PresenceStateOnline:
		if !stillValid {
			// Expired ⇒ unavailable: never grant an already-expired lease.
			return
		}
		if ok {
			if event.Generation < existing.generation {
				return // superseded generation: discard, never regress
			}
			if event.Generation == existing.generation && event.RelayPort != existing.relayPort {
				// Same-generation port contradiction cannot arise from the
				// gateway (port reassignment bumps the generation): fail closed.
				return
			}
		}
		// New lease, renewal, or replacement generation (fences the old one).
		leases[event.AgentRecordID] = presenceLease{
			relayPort:  event.RelayPort,
			generation: event.Generation,
			expiresAt:  expiresAt,
		}
	case PresenceStateOffline:
		if !ok {
			return
		}
		if event.Generation < existing.generation {
			return // stale offline for an already-fenced generation
		}
		// Exact match (or a newer-generation absence, e.g. a snapshot-era
		// fence): the lease is gone.
		delete(leases, event.AgentRecordID)
	}
}

// Available is the read-only relay availability predicate exposed to route
// selection (directctl). Matching rule (documented in the package comment):
//
//	Available = ∃ lease for agentID with lease.relayPort == relayPort
//	            AND lease.generation == generation        (exact join)
//	            AND lease.expiresAt.After(now)            (unexpired at now)
//	            AND route read is still current, proved gateway-side:
//	              sync Healthy() AND routeRevision == AcknowledgedRevision()
//	              (RouteSyncStatus, R2 ruling 4); sources implementing only
//	              RouteRevisionSource (test stubs) fall back to
//	              routeRevision == routes.CurrentRevision()
//
// The gateway-side proof (Sol Important-5): the acknowledged watermark is
// what the gateway verifiably holds, so an unacked route add is never
// selectable and a control restart (sync unhealthy until the gateway re-
// conciles and re-acks) fails every relay term closed until convergence.
//
// A nil route source, an out-of-range port, or an out-of-int64-domain
// generation is fail-closed false. The predicate is pure: it mutates nothing,
// performs no I/O, and never reads the relay_last_seen_at diagnostic (§12) —
// see TestRelayLastSeenIsDiagnosticOnly.
func (view *PresenceView) Available(agentID string, relayPort int, generation uint64, routeRevision uint64, now time.Time) bool {
	if view.routes == nil {
		return false // cannot validate route currency: fail closed
	}
	if relayPort <= 0 || relayPort > 65535 {
		return false
	}
	if generation > math.MaxInt64 {
		return false // cannot match a persisted int64 generation
	}
	if sync, ok := view.routes.(RouteSyncStatus); ok {
		if !sync.Healthy() {
			return false // sync down: relay unavailable, fail closed (R2 ruling 4)
		}
		if routeRevision != sync.AcknowledgedRevision() {
			return false // the gateway has not verifiably applied this revision
		}
	} else if routeRevision != view.routes.CurrentRevision() {
		return false // the caller's route read moved: re-read, never serve stale
	}
	view.mu.Lock()
	defer view.mu.Unlock()
	lease, ok := view.leases[agentID]
	if !ok {
		return false
	}
	return lease.relayPort == relayPort &&
		lease.generation == generation &&
		lease.expiresAt.After(now)
}

// recordLastSeen persists the relay_last_seen_at diagnostic (§12) for the
// agent records an accepted observation batch touched. Best-effort by
// contract: a failure is logged and never blocks the sync receipt, and the
// value is never read as an availability input. Runs outside the view lock;
// one bounded write per distinct agent per batch.
func (view *PresenceView) recordLastSeen(events []PresenceEvent) {
	if view.app == nil || len(events) == 0 {
		return
	}
	now := view.now()
	seen := make(map[string]struct{}, len(events))
	for _, event := range events {
		if _, done := seen[event.AgentRecordID]; done {
			continue
		}
		seen[event.AgentRecordID] = struct{}{}
		rec, err := view.app.FindRecordById("agents", event.AgentRecordID)
		if err != nil {
			view.logger.Warn("relayctl: relay_last_seen_at agent lookup failed", "agent", event.AgentRecordID)
			continue
		}
		rec.Set("relay_last_seen_at", now)
		if err := view.app.Save(rec); err != nil {
			view.logger.Warn("relayctl: relay_last_seen_at persist failed", "agent", event.AgentRecordID)
		}
	}
}
