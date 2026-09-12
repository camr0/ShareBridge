// §11.3 control↔gateway internal sync protocol DTOs (plan Task 11): versioned,
// bounded wire payloads for the route snapshot, ordered route deltas,
// gateway-authoritative presence, and explicit acknowledgements of the
// last-applied revision. The wire JSON here is mirrored field-for-field in
// relay/internal/controlsync (the two Go modules may not import each other);
// the golden payloads under testdata/relay-sync pin both sides to the exact
// same bytes via TestSyncGoldenPayloadsRoundTripBothModules.
//
// Global constraints honored here: every payload is versioned and bounded,
// unknown JSON fields are ignored (Task 7 ruling — forward wire compatibility),
// invalid enum/range/identity values are rejected, and nothing in these
// payloads is credential material.
package relayctl

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ProtocolVersion versions every §11.3 sync payload; both sides reject any// other version instead of guessing (fail closed).
const ProtocolVersion = 1

// Protocol bounds (§11.3 bounded payloads, §14). They are generous defaults
// with real ceilings: the configured relay port range (default 100 ports)
// bounds live tunnels, while routes are per share session. Server and client
// configuration may only tighten these, never lift them beyond the defaults.
const (
	// MaxRoutesPerSnapshot bounds the routes array of one snapshot.
	MaxRoutesPerSnapshot = 4096
	// MaxDeltasPerPage bounds the deltas array of one ordered delta page.
	MaxDeltasPerPage = 4096
	// MaxPresenceEventsPerEnvelope bounds one presence snapshot or event batch.
	MaxPresenceEventsPerEnvelope = 4096
	// MaxRequestBodyBytes bounds every POST body accepted by the sync server
	// (§11.3 bounded payloads).
	MaxRequestBodyBytes = 1024 * 1024
	// MaxResponseBytes bounds every response body accepted by the gateway
	// client; it must cover a default-bounded snapshot plus envelope slack.
	MaxResponseBytes = 4 * 1024 * 1024
	// MaxHostnameBytes is the longest RFC 1035 hostname (253 bytes) and bounds
	// route hostnames.
	MaxHostnameBytes = 253
	// MaxIdentityBytes bounds agent record IDs, session IDs, gateway boot IDs,
	// and certificate SAN identities on the wire.
	MaxIdentityBytes = 64
)

// Route delta operations and delta page statuses (§11.3 ordered route
// add/revoke/limit deltas and snapshot reconciliation).
const (
	RouteOperationAdd    = "route_add"
	RouteOperationRevoke = "route_revoke"
	RouteOperationLimit  = "route_limit"

	DeltaStatusOK  = "ok"
	DeltaStatusGap = "gap"
)

// Presence lease states (§7.3): a tunnel is online under a leased expiry or
// offline; there is no third state.
const (
	PresenceStateOnline  = "online"
	PresenceStateOffline = "offline"
)

// Sync protocol error classes. Callers map these to HTTP statuses (server) or
// typed failures (client); the distinctions are log/metric-grade only.
var (
	// ErrUnsupportedVersion covers any payload whose version is not
	// ProtocolVersion — never guessed at, never migrated on the fly.
	ErrUnsupportedVersion = errors.New("relayctl: unsupported sync protocol version")
	// ErrPayloadTooLarge covers arrays or bodies beyond the configured bounds.
	ErrPayloadTooLarge = errors.New("relayctl: sync payload exceeds bounds")
	// ErrBadRevision covers revision-ordering violations: regressions,
	// duplicates, pages that do not answer the requested since, or gap pages
	// that illegally carry deltas. Neither side guesses across a revision gap.
	ErrBadRevision = errors.New("relayctl: sync revision ordering violated")
	// ErrInvalidPayload covers wrong enums, malformed identities, missing or
	// unparseable lease expiries, and trailing JSON data.
	ErrInvalidPayload = errors.New("relayctl: sync payload rejected")
)

// Limits carries the §14 concurrency ceilings control attaches to a route.
// Zero values mean "apply the gateway default ceiling" for that field;
// negative values are invalid.
type Limits struct {
	MaxStreamsPerOrigin int `json:"max_streams_per_origin"`
	MaxStreamsPerAgent  int `json:"max_streams_per_agent"`
	MaxStreamsGlobal    int `json:"max_streams_global"`
}

// Route is one exact relay route on the §11.3 wire (the shared-contract
// "relayctl.Route"): exact relay hostname, agent record ID, relay port,
// generation, session ID, route revision, limit values, and the active flag.
// Field order is wire-stable: the golden payloads pin it.
type Route struct {
	Hostname      string `json:"hostname"`
	AgentRecordID string `json:"agent_record_id"`
	RelayPort     int    `json:"relay_port"`
	Generation    uint64 `json:"generation"`
	SessionID     string `json:"session_id"`
	Revision      uint64 `json:"revision"`
	Active        bool   `json:"active"`
	Limits        Limits `json:"limits"`
}

// Snapshot is the full route snapshot served to the gateway (§11.3 full
// snapshot fetch): the control epoch that produced it, control's current
// route revision within that epoch, and every route in one atomic payload.
// The epoch is ALWAYS present — including on empty snapshots — because it is
// the sole authority a restarted control carries: the gateway compares
// epochs (never revisions) to decide that a new boot's snapshot wholesale-
// replaces route state even where its per-route revisions restart lower
// (R2, Sol Critical-2). A re-pushed same-epoch snapshot over unchanged
// routes is idempotent at the applier (routes.ErrStaleRevision maps to
// success — SDD carry-forward).
type Snapshot struct {
	Version  int     `json:"version"`
	Epoch    uint64  `json:"epoch"`
	Revision uint64  `json:"revision"`
	Routes   []Route `json:"routes"`
	// PublishedAt is the RFC 3339 UTC time at which the revision stamped on
	// this snapshot was published by control's route publisher (the last
	// time its revision counter advanced, or the publisher's start time for
	// the initial revision). It is OBSERVABILITY ONLY: the gateway uses it
	// to measure §17.3 route propagation lag. It is absent for older
	// control builds (omitempty) and the gateway MUST NOT fabricate a lag
	// value when it is missing or malformed — it simply records no
	// observation. The R2 epoch authority (Epoch) remains the sole
	// adoption decision.
	PublishedAt string `json:"published_at,omitempty"`
}

// RouteDelta is one ordered route mutation. The delta revision and the route
// revision always match; a revoke carries an inactive route entry.
type RouteDelta struct {
	Revision  uint64 `json:"revision"`
	Operation string `json:"operation"`
	Route     Route  `json:"route"`
}

// DeltaPage answers the gateway's ordered delta fetch. Status OK carries the
// deltas in (since, latest_revision] with strictly increasing revisions; a
// page with Status Gap carries no deltas and reports the control revision the
// gateway cannot reach incrementally, forcing snapshot reconciliation
// (§11.3: a lost delta triggers snapshot reconciliation). Every page carries
// the control epoch that published it: the gateway never applies deltas
// across an epoch boundary — a page from any other epoch than the one the
// gateway last snapshotted forces reconciliation instead (R2).
type DeltaPage struct {
	Version        int          `json:"version"`
	Epoch          uint64       `json:"epoch"`
	Status         string       `json:"status"`
	Since          uint64       `json:"since"`
	LatestRevision uint64       `json:"latest_revision"`
	Deltas         []RouteDelta `json:"deltas"`
	// PublishedAt is the RFC 3339 UTC publish time of LatestRevision (the
	// newest retained delta on an OK page, or the current revision on a
	// gap/empty page). It is OBSERVABILITY ONLY and omitempty for older
	// control builds; an absent or malformed value records no lag
	// observation. See Snapshot.PublishedAt.
	PublishedAt string `json:"published_at,omitempty"`
}

// PresenceEvent is one gateway-authoritative tunnel lease fact (the
// shared-contract "relayctl.PresenceEvent"): gateway boot ID, monotonically
// increasing revision, agent record ID, relay port, generation, state, and —
// for online leases — the lease expiry (RFC 3339). Boot ID and revision ride
// on every event so each fact self-describes its epoch.
type PresenceEvent struct {
	GatewayBootID  string `json:"gateway_boot_id"`
	Revision       uint64 `json:"revision"`
	AgentRecordID  string `json:"agent_record_id"`
	RelayPort      int    `json:"relay_port"`
	Generation     uint64 `json:"generation"`
	State          string `json:"state"`
	LeaseExpiresAt string `json:"lease_expires_at,omitempty"`
}

// PresenceEnvelope carries either a full presence snapshot (every current
// lease at one revision, posted on boot/reconnect and periodically thereafter)
// or an ordered batch of presence events (strictly increasing revisions). The
// wire shape is shared; the endpoint gives it semantics.
//
// GatewayBootID and Revision are the reporting boot's identity, carried at the
// TOP LEVEL so that a FULL SNAPSHOT describes its boot even when it has no
// events at all. This is what makes a freshly restarted gateway (whose
// presence is legitimately empty until its FRP clients reconnect, §15.1)
// ADOPTABLE: control records the new boot from the empty snapshot, so the new
// boot's first real events apply instead of being discarded as a replay of a
// superseded boot. Without them an empty snapshot was indistinguishable from
// "the previously recorded boot now has no routes", and a fresh boot could
// never adopt. They are omitempty so an ordered event batch (which already
// stamps boot/revision per event, §7.3) is wire-identical to earlier builds.
type PresenceEnvelope struct {
	Version       int             `json:"version"`
	GatewayBootID string          `json:"gateway_boot_id,omitempty"`
	Revision      uint64          `json:"revision,omitempty"`
	Events        []PresenceEvent `json:"events"`
}

// StatusAck is the gateway's explicit acknowledgement: its boot ID, the
// control epoch of the state it applied, and the last route revision it
// applied within that epoch (§11.3 explicit acknowledgements and
// last-applied revision). Control's no-regression guard is per boot ID so a
// restarted gateway (§15.1) can legitimately report a lower revision again,
// and per control epoch so a control restart resets the health watermark
// until the gateway has reconciled to the new epoch's snapshot (R2).
type StatusAck struct {
	Version             int    `json:"version"`
	GatewayBootID       string `json:"gateway_boot_id"`
	ControlEpoch        uint64 `json:"control_epoch"`
	LastAppliedRevision uint64 `json:"last_applied_revision"`
}

// AckReasonForeignEpoch is the bounded reason control reports for a status ack
// it did not record: a StatusAck is accepted only when it carries the
// publisher's own control epoch, and any other epoch is in-flight traffic from
// a gateway that has not yet reconciled across a control restart (R2). The
// value is a fixed, low-cardinality token, safe to report on the wire and to
// log.
const AckReasonForeignEpoch = "foreign_epoch"

// AckOutcome is control's truthful answer to a status acknowledgement: whether
// the acknowledgement was actually RECORDED (the control epoch matched and the
// watermark advanced). A request that was handled but recorded nothing is not
// an accepted acknowledgement, and the bounded Reason says why. The zero value
// is the accepted outcome.
type AckOutcome struct {
	Accepted bool
	// Reason is AckReasonForeignEpoch (or a future bounded token) when the ack
	// was not recorded, and empty when Accepted is true.
	Reason string
}

// StatusAckResponse echoes control's recorded last-applied revision for the
// reporting boot. Acknowledged is true only when the acknowledgement was
// actually recorded: a foreign-epoch (or otherwise unrecorded) ack answers
// acknowledged:false with the bounded Reason, and LastAppliedRevision is not
// meaningful. A client must treat acknowledged:false as "not acked".
type StatusAckResponse struct {
	Version             int    `json:"version"`
	Acknowledged        bool   `json:"acknowledged"`
	Reason              string `json:"reason,omitempty"`
	LastAppliedRevision uint64 `json:"last_applied_revision"`
}

// SyncReceipt is the bounded response to a presence publish.
type SyncReceipt struct {
	Version  int  `json:"version"`
	Accepted bool `json:"accepted"`
}

// BoundedAckReason maps a sink-reported acknowledgement reason to the fixed
// allowlist that may appear on the wire, so an unexpected (or high-cardinality)
// reason can never leak into a response or a log line.
func BoundedAckReason(reason string) string {
	if reason == AckReasonForeignEpoch {
		return reason
	}
	return "not_accepted"
}

// validateActiveLimits rejects a zero (or negative) ceiling on an ACTIVE
// route. Zero is not a limit value: the gateway's limits layer reads a zero
// route value as "unset" and restores the process default, so emitting a
// control "tightening to zero" would silently widen the ceiling. Revoke
// tombstones (inactive) legitimately carry zeroed limits and are not passed
// here. This is the control half of the by-construction zero-semantics
// agreement, paired with the gateway's controlsync.validateActiveLimits.
func validateActiveLimits(limits Limits) error {
	if limits.MaxStreamsPerOrigin <= 0 || limits.MaxStreamsPerAgent <= 0 || limits.MaxStreamsGlobal <= 0 {
		return fmt.Errorf("%w: active route limits must be positive, got %+v", ErrInvalidPayload, limits)
	}
	return nil
}

// ValidateRoute checks one wire route: bounded exact hostname (wildcards and
// separators rejected), bounded identity strings, port in range, and
// non-negative limit ceilings. Full §6 label validation and lowercasing
// normalization stay with the gateway's route table on application.
func ValidateRoute(route Route) error {
	if err := validateHostname(route.Hostname); err != nil {
		return err
	}
	if err := validateIdentifier(route.AgentRecordID, "agent_record_id"); err != nil {
		return err
	}
	if err := validateIdentifier(route.SessionID, "session_id"); err != nil {
		return err
	}
	if route.RelayPort <= 0 || route.RelayPort > 65535 {
		return fmt.Errorf("%w: relay port %d out of range", ErrInvalidPayload, route.RelayPort)
	}
	if route.Limits.MaxStreamsPerOrigin < 0 || route.Limits.MaxStreamsPerAgent < 0 || route.Limits.MaxStreamsGlobal < 0 {
		return fmt.Errorf("%w: negative route limit", ErrInvalidPayload)
	}
	return nil
}

// ValidateSnapshot validates a full route snapshot: exact version, an
// always-present control epoch (the empty new-boot snapshot is exactly the
// payload the epoch authority exists for, R2), bounded route count, every
// route valid, and the snapshot revision never below any route revision
// (§11.3 bad-revision rejection).
func ValidateSnapshot(snapshot Snapshot, maxRoutes int) error {
	if snapshot.Version != ProtocolVersion {
		return fmt.Errorf("%w: snapshot version %d", ErrUnsupportedVersion, snapshot.Version)
	}
	if snapshot.Epoch == 0 {
		return fmt.Errorf("%w: snapshot control epoch is missing", ErrInvalidPayload)
	}
	if len(snapshot.Routes) > maxRoutes {
		return fmt.Errorf("%w: snapshot carries %d routes, bound is %d", ErrPayloadTooLarge, len(snapshot.Routes), maxRoutes)
	}
	for _, route := range snapshot.Routes {
		if err := ValidateRoute(route); err != nil {
			return err
		}
		if err := validateActiveLimits(route.Limits); err != nil {
			return err
		}
		if route.Revision > snapshot.Revision {
			return fmt.Errorf("%w: route revision %d exceeds snapshot revision %d", ErrBadRevision, route.Revision, snapshot.Revision)
		}
	}
	return nil
}

// ValidateDeltaPage validates an ordered delta page: exact version, known
// status, bounded count, and the §11.3 ordering rules — OK pages carry
// strictly increasing revisions out of (since, latest_revision] with each
// delta revision matching its route revision and its operation legal for the
// route's active flag; gap pages carry no deltas and must point strictly
// ahead of the requested since.
func ValidateDeltaPage(page DeltaPage, maxDeltas int) error {
	if page.Version != ProtocolVersion {
		return fmt.Errorf("%w: delta page version %d", ErrUnsupportedVersion, page.Version)
	}
	if page.Epoch == 0 {
		return fmt.Errorf("%w: delta page control epoch is missing", ErrInvalidPayload)
	}
	if len(page.Deltas) > maxDeltas {
		return fmt.Errorf("%w: delta page carries %d deltas, bound is %d", ErrPayloadTooLarge, len(page.Deltas), maxDeltas)
	}
	switch page.Status {
	case DeltaStatusGap:
		if len(page.Deltas) != 0 {
			return fmt.Errorf("%w: gap page carries deltas", ErrBadRevision)
		}
		if page.LatestRevision <= page.Since {
			return fmt.Errorf("%w: gap latest revision %d not ahead of since %d", ErrBadRevision, page.LatestRevision, page.Since)
		}
		return nil
	case DeltaStatusOK:
	default:
		return fmt.Errorf("%w: delta status %q", ErrInvalidPayload, page.Status)
	}

	previousRevision := page.Since
	for _, delta := range page.Deltas {
		switch delta.Operation {
		case RouteOperationAdd:
			if !delta.Route.Active {
				return fmt.Errorf("%w: route_add carries an inactive route", ErrInvalidPayload)
			}
			if err := validateActiveLimits(delta.Route.Limits); err != nil {
				return err
			}
		case RouteOperationRevoke:
			if delta.Route.Active {
				return fmt.Errorf("%w: route_revoke carries an active route", ErrInvalidPayload)
			}
		case RouteOperationLimit:
			if !delta.Route.Active {
				return fmt.Errorf("%w: route_limit carries an inactive route", ErrInvalidPayload)
			}
			if err := validateActiveLimits(delta.Route.Limits); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: delta operation %q", ErrInvalidPayload, delta.Operation)
		}
		if delta.Revision != delta.Route.Revision {
			return fmt.Errorf("%w: delta revision %d does not match route revision %d", ErrBadRevision, delta.Revision, delta.Route.Revision)
		}
		if delta.Revision <= previousRevision {
			return fmt.Errorf("%w: delta revision %d does not supersede %d", ErrBadRevision, delta.Revision, previousRevision)
		}
		if err := ValidateRoute(delta.Route); err != nil {
			return err
		}
		previousRevision = delta.Revision
	}
	if len(page.Deltas) == 0 {
		if page.LatestRevision != page.Since {
			return fmt.Errorf("%w: empty page latest revision %d does not equal since %d", ErrBadRevision, page.LatestRevision, page.Since)
		}
	} else if previousRevision != page.LatestRevision {
		return fmt.Errorf("%w: page latest revision %d does not match last delta revision %d", ErrBadRevision, page.LatestRevision, previousRevision)
	}
	return nil
}

// ValidatePresenceEvent checks one presence lease fact: bounded boot ID and
// agent identity, port in range, exact state enum, and the lease-expiry rule —
// online requires a parseable RFC 3339 expiry, offline carries none. Lease
// freshness policy stays with the Task 15 presence view.
func ValidatePresenceEvent(event PresenceEvent) error {
	if err := validateIdentifier(event.GatewayBootID, "gateway_boot_id"); err != nil {
		return err
	}
	if err := validateIdentifier(event.AgentRecordID, "agent_record_id"); err != nil {
		return err
	}
	if event.RelayPort <= 0 || event.RelayPort > 65535 {
		return fmt.Errorf("%w: presence relay port %d out of range", ErrInvalidPayload, event.RelayPort)
	}
	switch event.State {
	case PresenceStateOnline:
		if event.LeaseExpiresAt == "" {
			return fmt.Errorf("%w: online lease without expiry", ErrInvalidPayload)
		}
		if _, err := time.Parse(time.RFC3339, event.LeaseExpiresAt); err != nil {
			return fmt.Errorf("%w: lease expiry %q is not RFC 3339", ErrInvalidPayload, event.LeaseExpiresAt)
		}
	case PresenceStateOffline:
		if event.LeaseExpiresAt != "" {
			return fmt.Errorf("%w: offline lease carries an expiry", ErrInvalidPayload)
		}
	default:
		return fmt.Errorf("%w: presence state %q", ErrInvalidPayload, event.State)
	}
	return nil
}

// ValidatePresenceSnapshotEnvelope validates a full presence snapshot: exact
// version, bounded count, every event valid, and all events sharing one boot
// ID at one revision (the snapshot revision).
func ValidatePresenceSnapshotEnvelope(envelope PresenceEnvelope, maxEvents int) error {
	if err := validatePresenceEnvelopeHeader(envelope, maxEvents); err != nil {
		return err
	}
	// The top-level boot identity is what makes an EMPTY snapshot adoptable, so
	// it is bounded/validated exactly like an event's boot ID when present.
	if envelope.GatewayBootID != "" {
		if err := validateIdentifier(envelope.GatewayBootID, "gateway_boot_id"); err != nil {
			return err
		}
	}
	for _, event := range envelope.Events {
		if err := ValidatePresenceEvent(event); err != nil {
			return err
		}
		if event.GatewayBootID != envelope.Events[0].GatewayBootID {
			return fmt.Errorf("%w: presence snapshot mixes boot IDs", ErrInvalidPayload)
		}
		if envelope.GatewayBootID != "" && event.GatewayBootID != envelope.GatewayBootID {
			return fmt.Errorf("%w: presence snapshot top-level boot does not match its events", ErrInvalidPayload)
		}
		if event.Revision != envelope.Events[0].Revision {
			return fmt.Errorf("%w: presence snapshot mixes revisions", ErrBadRevision)
		}
	}
	// When both describe the same boot they must agree; a disagreement is a
	// producer bug and is never guessed at.
	if len(envelope.Events) > 0 && envelope.GatewayBootID != "" && envelope.Revision != envelope.Events[0].Revision {
		return fmt.Errorf("%w: presence snapshot top-level revision %d does not match event revision %d",
			ErrBadRevision, envelope.Revision, envelope.Events[0].Revision)
	}
	return nil
}

// ValidatePresenceEventEnvelope validates an ordered presence event batch:
// exact version, bounded count, at least one event, every event valid, one
// boot ID, and strictly increasing revisions.
func ValidatePresenceEventEnvelope(envelope PresenceEnvelope, maxEvents int) error {
	if err := validatePresenceEnvelopeHeader(envelope, maxEvents); err != nil {
		return err
	}
	if len(envelope.Events) == 0 {
		return fmt.Errorf("%w: presence event batch is empty", ErrInvalidPayload)
	}
	for index, event := range envelope.Events {
		if err := ValidatePresenceEvent(event); err != nil {
			return err
		}
		if event.GatewayBootID != envelope.Events[0].GatewayBootID {
			return fmt.Errorf("%w: presence batch mixes boot IDs", ErrInvalidPayload)
		}
		if index > 0 && event.Revision <= envelope.Events[index-1].Revision {
			return fmt.Errorf("%w: presence revision %d does not supersede %d", ErrBadRevision, event.Revision, envelope.Events[index-1].Revision)
		}
	}
	return nil
}

func validatePresenceEnvelopeHeader(envelope PresenceEnvelope, maxEvents int) error {
	if envelope.Version != ProtocolVersion {
		return fmt.Errorf("%w: presence envelope version %d", ErrUnsupportedVersion, envelope.Version)
	}
	if len(envelope.Events) > maxEvents {
		return fmt.Errorf("%w: presence envelope carries %d events, bound is %d", ErrPayloadTooLarge, len(envelope.Events), maxEvents)
	}
	return nil
}

// ValidateStatusAck validates an explicit acknowledgement: exact version, a
// bounded boot ID, and the always-present control epoch of the applied state
// (R2: acks carry the same epoch as the snapshot/deltas they follow).
func ValidateStatusAck(ack StatusAck) error {
	if ack.Version != ProtocolVersion {
		return fmt.Errorf("%w: status ack version %d", ErrUnsupportedVersion, ack.Version)
	}
	if ack.ControlEpoch == 0 {
		return fmt.Errorf("%w: status ack control epoch is missing", ErrInvalidPayload)
	}
	return validateIdentifier(ack.GatewayBootID, "gateway_boot_id")
}

// formatPublishedAt renders an observability timestamp as RFC 3339 UTC, or an
// empty string when unset (which omitempty then drops from the wire).
func formatPublishedAt(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func validateHostname(hostname string) error {
	if hostname == "" || len(hostname) > MaxHostnameBytes {
		return fmt.Errorf("%w: hostname empty or longer than %d bytes", ErrInvalidPayload, MaxHostnameBytes)
	}
	if strings.ContainsAny(hostname, "*? \t\r\n:/") {
		return fmt.Errorf("%w: hostname %q contains a forbidden character", ErrInvalidPayload, hostname)
	}
	return nil
}

func validateIdentifier(value string, field string) error {
	if value == "" || len(value) > MaxIdentityBytes {
		return fmt.Errorf("%w: %s empty or longer than %d bytes", ErrInvalidPayload, field, MaxIdentityBytes)
	}
	for index := 0; index < len(value); index++ {
		if value[index] < 0x21 || value[index] > 0x7e {
			return fmt.Errorf("%w: %s contains a forbidden byte", ErrInvalidPayload, field)
		}
	}
	return nil
}
