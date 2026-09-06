package controlsync

// Task 13 snapshot/delta applier (plan Task 13, spec §§8, 14, 15.1, 15.4).
// The applier is the single writer that turns the §11.3 wire payloads the
// Task 11 client fetches into gateway routing state: a full snapshot fetched
// at boot and on every revision gap is applied atomically (§8, §15.1), and
// ordered deltas are applied on top of it. It never guesses across a gap —
// a gap page surfaces as ErrBadRevision from the client and the recovery
// path is always a fresh snapshot — and it reports readiness only after the
// first successful snapshot apply, which is what gates public traffic.
//
// Two §14/§15 distinctions are enforced here on purpose:
//
//   - The finite 120-second route lease (RouteLeaseTTL) lives in the route
//     table and is restarted by every applied route update (control's 30-
//     second lease refresh is a route_limit delta per live route). Expiry
//     blocks only NEW connections; established streams are untouched.
//   - An explicit route_revoke delta tombstones the route and then drains
//     its established streams — mutation first, drain second (the ordering
//     invariant documented on gateway Streams.RegisterAdmitted). Lease
//     expiry never drains.
//
// Package controlsync stays a library: no production caller is wired here
// (the EnableRelay posture holds until Tasks 34's wiring).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"sharebridge/relay/internal/routes"
)

// StreamDrainer is the established-stream drain seam (satisfied by
// gateway.Streams). Only explicit revokes drain; see CloseRoute's ordering
// invariant — every call here follows the table mutation that motivates it.
type StreamDrainer interface {
	CloseRoute(hostname string) int
}

// relayNamespaceLabel is the fixed second label of every §6 relay origin:
// <origin>.relay.<namespace>.<zone>.
const relayNamespaceLabel = "relay"

// invalidRouteDiagnosticLimit bounds how many rejected-route hostnames one
// diagnostic line carries; the count is always exact.
const invalidRouteDiagnosticLimit = 3

// ApplierConfig is the fail-closed construction contract for the applier.
type ApplierConfig struct {
	// Client is the Task 11 sync client; required.
	Client *Client
	// Table is the gateway route table the applier owns writes to; required.
	Table *routes.Table
	// Streams drains established streams on explicit revokes. Optional only
	// so the applier can run before the stream registry exists (Task 4
	// wiring); production passes gateway.NewStreams().
	Streams StreamDrainer
	// Namespace is this gateway's §6 namespace ("sb" plus eight lowercase
	// hex characters, exactly the shape frpplugin validates); routes outside
	// it are never admitted. Required.
	Namespace string
	// BootID is this gateway process's sync identity for status acks
	// (§15.1: a new boot replaces control's recorded ack state wholesale).
	// Required.
	BootID string
	// Logger receives bounded diagnostics (never payload contents).
	Logger *slog.Logger
	// Clock overrides the wall clock for diagnostics. The route lease reads
	// the table's clock, not this one.
	Clock func() time.Time
}

// Applier applies control's route snapshots and deltas to the gateway's
// in-memory routing state and tracks sync readiness. Safe for concurrent
// use; the table and streams it drives are synchronized on their own.
type Applier struct {
	client    *Client
	table     *routes.Table
	streams   StreamDrainer
	namespace string
	bootID    string
	logger    *slog.Logger
	clock     func() time.Time

	mu          sync.Mutex
	applied     bool
	lastApplied uint64
}

// NewApplier validates the configuration fail-closed and returns the applier.
func NewApplier(config ApplierConfig) (*Applier, error) {
	if config.Client == nil {
		return nil, errors.New("controlsync: applier requires a sync client")
	}
	if config.Table == nil {
		return nil, errors.New("controlsync: applier requires a route table")
	}
	if !validApplierNamespace(config.Namespace) {
		return nil, fmt.Errorf("controlsync: applier namespace %q is not \"sb\" plus eight lowercase hex characters", config.Namespace)
	}
	if err := validateSyncIdentifier(config.BootID, "gateway_boot_id"); err != nil {
		return nil, err
	}
	clock := config.Clock
	if clock == nil {
		clock = time.Now
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Applier{
		client:    config.Client,
		table:     config.Table,
		streams:   config.Streams,
		namespace: config.Namespace,
		bootID:    config.BootID,
		logger:    logger,
		clock:     clock,
	}, nil
}

// Ready reports whether the gateway has applied its first full route
// snapshot. Public traffic must be rejected until this is true (spec §8,
// §15.1); it never regresses to false — a later gap blocks only further
// delta progress, while routes age out through the finite lease (§15.4).
func (applier *Applier) Ready() bool {
	applier.mu.Lock()
	defer applier.mu.Unlock()
	return applier.applied
}

// LastAppliedRevision reports the route revision the gateway has fully
// applied: the snapshot revision after a snapshot apply (even when a new
// control epoch's revision sits below a previous one — the delta cursor
// follows the epoch control is currently serving), or the latest revision of
// the last applied delta page.
func (applier *Applier) LastAppliedRevision() uint64 {
	applier.mu.Lock()
	defer applier.mu.Unlock()
	return applier.lastApplied
}

// ReconcileSnapshot fetches control's full route snapshot and applies it
// atomically. This is the boot/reconnect path (§15.1) and the only recovery
// from a revision gap (§11.3, §15.7). Errors from the client (transport,
// ErrUnauthorized, ErrOversize, ErrUnsupportedVersion, ErrInvalidPayload,
// ErrBadRevision) propagate unchanged; nothing is applied unless the fetch
// and validation succeeded.
func (applier *Applier) ReconcileSnapshot(ctx context.Context) error {
	snapshot, err := applier.client.FetchSnapshot(ctx)
	if err != nil {
		return err
	}
	applier.applySnapshot(snapshot)
	applier.acknowledge(ctx)
	return nil
}

// SyncDeltas fetches the ordered delta page answering the current cursor and
// applies it. A gap page (ErrBadRevision) is propagated: the caller must
// reconcile with a fresh snapshot instead of guessing (§15.7). A never-
// applied applier fetches since 0; a well-formed control answers a gap for
// any position the gateway cannot bridge, which routes the caller back to
// ReconcileSnapshot, so starting from an empty table is never trusted to be
// complete when it is not.
func (applier *Applier) SyncDeltas(ctx context.Context) error {
	page, err := applier.client.FetchDeltas(ctx, applier.LastAppliedRevision())
	if err != nil {
		return err
	}
	applier.applyDeltas(page)
	applier.acknowledge(ctx)
	return nil
}

// Reconcile is the one-step catch-up the sync loop calls: snapshot first on
// boot, deltas otherwise, and a fresh snapshot whenever a delta fetch
// reports a revision gap. Only ErrBadRevision triggers the recovery fetch —
// it is the protocol's sole sanctioned gap signal; every other error is
// returned for the caller's retry policy.
func (applier *Applier) Reconcile(ctx context.Context) error {
	if !applier.Ready() {
		return applier.ReconcileSnapshot(ctx)
	}
	if err := applier.SyncDeltas(ctx); err != nil {
		if errors.Is(err, ErrBadRevision) {
			return applier.ReconcileSnapshot(ctx)
		}
		return err
	}
	return nil
}

// applySnapshot admits a fetched snapshot: per-route §6 validation first
// (invalid entries are rejected individually with a bounded diagnostic — one
// bad route never drops an otherwise well-formed snapshot; malformed pages
// are already refused wholesale by the Task 11 validators), then the atomic
// table replacement, then — and only then — the drain of the routes the
// snapshot dropped (mutate-before-drain).
func (applier *Applier) applySnapshot(snapshot Snapshot) {
	admitted := make([]routes.Route, 0, len(snapshot.Routes))
	invalidCount := 0
	var invalidSamples []string
	var invalidCause error
	for _, route := range snapshot.Routes {
		admittedRoute, err := applier.validateRoute(route)
		if err != nil {
			invalidCount++
			if len(invalidSamples) < invalidRouteDiagnosticLimit {
				invalidSamples = append(invalidSamples, route.Hostname)
			}
			if invalidCause == nil {
				invalidCause = err
			}
			continue
		}
		admitted = append(admitted, admittedRoute)
	}
	if invalidCount > 0 {
		applier.logger.Warn("controlsync: snapshot carried routes rejected per-entry",
			"rejected", invalidCount,
			"samples", invalidSamples,
			"first_error", invalidCause)
	}

	// Mutation commits inside ReplaceSnapshot; the dropped set it returns is
	// therefore already ordered after the mutation (Streams ordering
	// invariant), and draining follows.
	dropped := applier.table.ReplaceSnapshot(admitted)
	applier.drain(dropped)

	applier.setState(snapshot.Revision)
	applier.logger.Info("controlsync: route snapshot applied",
		"routes", len(admitted),
		"dropped", len(dropped),
		"revision", snapshot.Revision)
}

// applyDeltas admits one validated, strictly ordered delta page. Every entry
// carries the full route state, so add and limit are the same table
// operation; per-route staleness (ErrStaleRevision) is an idempotent success
// — required after a control restart, where a kept higher gateway revision
// meets the new epoch's lower deltas. An explicit revoke mutates the table
// first and drains its established streams second (§8, §15.6). Lease expiry
// never reaches this path: it is the absence of applied refreshes, not a
// delta.
func (applier *Applier) applyDeltas(page DeltaPage) {
	invalidCount := 0
	var invalidSamples []string
	var invalidCause error
	for _, delta := range page.Deltas {
		switch delta.Operation {
		case RouteOperationRevoke:
			hostname, err := applier.validateRelayHostname(delta.Route.Hostname)
			if err != nil {
				invalidCount = applier.noteInvalidRoute(invalidCount, delta.Route.Hostname, &invalidSamples, &invalidCause, err)
				continue
			}
			// Mutate before drain: Revoke commits the tombstone before the
			// returned error is even inspected, and the drain runs only on
			// success (Streams.RegisterAdmitted ordering invariant).
			err = applier.table.Revoke(hostname, delta.Revision)
			switch {
			case err == nil:
				applier.drain([]string{hostname})
			case errors.Is(err, routes.ErrStaleRevision):
				// Idempotent success: the stored state already supersedes
				// the revoke; nothing to drain.
			case errors.Is(err, routes.ErrRouteNotFound):
				// The page is validated and in-order, so control believes we
				// hold this route; divergence means our state is behind —
				// but the desired end state (route absent) already holds and
				// a snapshot reconciliation converges identically. Log, do
				// not fail the page.
				applier.logger.Warn("controlsync: revoke for a route the gateway does not hold",
					"hostname", hostname, "revision", delta.Revision)
			default:
				applier.logger.Error("controlsync: revoke failed unexpectedly", "error", err)
			}
		case RouteOperationAdd, RouteOperationLimit:
			route, err := applier.validateRoute(delta.Route)
			if err != nil {
				invalidCount = applier.noteInvalidRoute(invalidCount, delta.Route.Hostname, &invalidSamples, &invalidCause, err)
				continue
			}
			if err := applier.table.Apply(route); err != nil {
				if errors.Is(err, routes.ErrStaleRevision) {
					continue // idempotent success (SDD carry-forward)
				}
				applier.logger.Error("controlsync: delta apply failed unexpectedly", "error", err)
			}
		}
	}
	if invalidCount > 0 {
		applier.logger.Warn("controlsync: delta page carried routes rejected per-entry",
			"rejected", invalidCount,
			"samples", invalidSamples,
			"first_error", invalidCause)
	}

	applier.setState(page.LatestRevision)
}

// noteInvalidRoute records one per-entry rejection for the bounded
// diagnostic. Kept as a helper so both apply paths count identically.
func (applier *Applier) noteInvalidRoute(count int, hostname string, samples *[]string, cause *error, err error) int {
	if len(*samples) < invalidRouteDiagnosticLimit {
		*samples = append(*samples, hostname)
	}
	if *cause == nil {
		*cause = err
	}
	return count + 1
}

// validateRoute converts a wire route into a table route after §6 hostname
// validation. The wire and table route shapes are field-for-field identical
// (int port/limits, uint64 revision/generation), so the conversion is a
// field copy: there is no uint64↔int conversion anywhere on this path, and
// out-of-int-range counts are refused by the JSON decode at the wire
// boundary (Task 11 client, ErrInvalidPayload).
func (applier *Applier) validateRoute(route Route) (routes.Route, error) {
	hostname, err := applier.validateRelayHostname(route.Hostname)
	if err != nil {
		return routes.Route{}, err
	}
	return routes.Route{
		Hostname:      hostname,
		AgentRecordID: route.AgentRecordID,
		RelayPort:     route.RelayPort,
		Generation:    route.Generation,
		SessionID:     route.SessionID,
		Revision:      route.Revision,
		Active:        route.Active,
		Limits: routes.Limits{
			MaxStreamsPerOrigin: route.Limits.MaxStreamsPerOrigin,
			MaxStreamsPerAgent:  route.Limits.MaxStreamsPerAgent,
			MaxStreamsGlobal:    route.Limits.MaxStreamsGlobal,
		},
	}, nil
}

// validateRelayHostname enforces the full §6 admission shape: a well-formed
// exact hostname (RFC 1123 labels, no wildcard — routes.ValidateRelayHostname)
// bound to this gateway's namespace:
//
//	<origin>.relay.<namespace>.<zone>
//
// The zone suffix itself is deployment configuration the gateway does not
// pin; the namespace label is the security boundary (one gateway never
// serves another namespace's origins).
func (applier *Applier) validateRelayHostname(hostname string) (string, error) {
	normalized, err := routes.ValidateRelayHostname(hostname)
	if err != nil {
		return "", err
	}
	labels := strings.Split(normalized, ".")
	if len(labels) < 4 {
		return "", fmt.Errorf("controlsync: relay hostname %q does not have the <origin>.relay.<namespace>.<zone> shape", hostname)
	}
	if labels[1] != relayNamespaceLabel {
		return "", fmt.Errorf("controlsync: relay hostname %q second label is %q, want %q", hostname, labels[1], relayNamespaceLabel)
	}
	if labels[2] != applier.namespace {
		return "", fmt.Errorf("controlsync: relay hostname %q is bound to namespace %q, not %q", hostname, labels[2], applier.namespace)
	}
	return normalized, nil
}

// drain closes the established streams of the given hostnames. It must only
// ever be called after the motivating table mutation has been committed —
// the ordering invariant documented on gateway Streams.RegisterAdmitted and
// routes.Table.ReplaceSnapshot. A revoked hostname with no indexed streams
// is the normal quiet case.
func (applier *Applier) drain(hostnames []string) {
	if applier.streams == nil || len(hostnames) == 0 {
		return
	}
	for _, hostname := range hostnames {
		if closed := applier.streams.CloseRoute(hostname); closed > 0 {
			applier.logger.Info("controlsync: revoked route closed established streams",
				"hostname", hostname, "streams", closed)
		}
	}
}

// setState records a fully applied revision. Snapshot applications flip
// readiness on; nothing flips it back off (§15.4: a later gap must not
// withdraw routing for already-served routes — the finite lease ages them
// out if control stays unreachable).
func (applier *Applier) setState(revision uint64) {
	applier.mu.Lock()
	defer applier.mu.Unlock()
	applier.applied = true
	applier.lastApplied = revision
}

// acknowledge reports the applied revision to control (§11.3 status). It is
// deliberately non-fatal: the apply already committed, control's own
// no-regression guard accepts equal re-acks, and the next reconcile re-acks.
// Without acks control would stop refreshing leases (Healthy() requires the
// current boot's explicit ack), so an ack failure is logged for operators.
func (applier *Applier) acknowledge(ctx context.Context) {
	ack := StatusAck{
		Version:             ProtocolVersion,
		GatewayBootID:       applier.bootID,
		LastAppliedRevision: applier.LastAppliedRevision(),
	}
	if _, err := applier.client.SendStatus(ctx, ack); err != nil {
		applier.logger.Warn("controlsync: status ack failed (the next reconcile re-acks)",
			"error", err)
	}
}

// validApplierNamespace mirrors frpplugin.validNamespace: control generates
// namespaces as "sb" plus eight lowercase hex characters, and the gateway's
// credential plugin already enforces exactly that shape.
func validApplierNamespace(namespace string) bool {
	if len(namespace) != 10 || !strings.HasPrefix(namespace, "sb") {
		return false
	}
	for index := 2; index < len(namespace); index++ {
		char := namespace[index]
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
