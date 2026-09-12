// Package controlsync is the gateway's client for the §11.3 control↔gateway
// internal sync protocol (plan Task 11): mutually authenticated private-
// network HTTP between the separate relay and control Hetzner hosts. The
// client pins the control sync CA and the exact control server SAN
// (independent of the content PKI), always presents its gateway client
// certificate, bounds every response body, validates every payload's version
// and revision ordering before use, and never guesses across a revision gap —
// a gap page sends the caller back to a full snapshot reconciliation (the
// Task 13 applier).
//
// The wire JSON mirrors control's relayctl protocol DTOs field-for-field (the
// modules may not import each other); the golden payloads under
// testdata/relay-sync pin both sides to the exact same bytes.
//
// Boot/reconnect wiring into the gateway process and the snapshot applier
// arrive with Tasks 13/34; this package is deliberately library-only so the
// EnableRelay posture (no production caller until Tasks 12/34) is preserved.
package controlsync

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"sharebridge/relay/internal/metrics"
)

// Protocol version, bounds, endpoint paths, enums, and error classes mirror
// control's relayctl protocol contract exactly (§11.3, §14). Both sides
// enforce both directions.
const (
	ProtocolVersion = 1

	MaxRoutesPerSnapshot         = 4096
	MaxDeltasPerPage             = 4096
	MaxPresenceEventsPerEnvelope = 4096
	MaxRequestBodyBytes          = 1024 * 1024
	MaxResponseBytes             = 4 * 1024 * 1024
	MaxHostnameBytes             = 253
	MaxIdentityBytes             = 64

	PathSnapshot         = "/internal/relay/v1/snapshot"
	PathDeltas           = "/internal/relay/v1/deltas"
	PathPresenceSnapshot = "/internal/relay/v1/presence/snapshot"
	PathPresenceEvents   = "/internal/relay/v1/presence/events"
	PathStatus           = "/internal/relay/v1/status"

	RouteOperationAdd    = "route_add"
	RouteOperationRevoke = "route_revoke"
	RouteOperationLimit  = "route_limit"

	DeltaStatusOK  = "ok"
	DeltaStatusGap = "gap"

	PresenceStateOnline  = "online"
	PresenceStateOffline = "offline"

	defaultRequestTimeout = 10 * time.Second
)

// Sync client error classes (mirrored from the protocol contract).
var (
	// ErrUnauthorized covers any 401/403 answer: missing, untrusted, or
	// wrong-identity certificates on either side.
	ErrUnauthorized = errors.New("controlsync: sync peer rejected our identity")
	// ErrOversize covers bodies or arrays beyond the configured bounds.
	ErrOversize = errors.New("controlsync: sync payload exceeds bounds")
	// ErrUnsupportedVersion covers any payload with an unknown version.
	ErrUnsupportedVersion = errors.New("controlsync: unsupported sync protocol version")
	// ErrBadRevision covers revision-ordering violations; the caller must
	// reconcile with a fresh snapshot instead of guessing (§11.3, §15.7).
	ErrBadRevision = errors.New("controlsync: sync revision ordering violated")
	// ErrInvalidPayload covers malformed payloads: wrong enums, bad
	// identities, unparseable expiries, non-JSON, and trailing data.
	ErrInvalidPayload = errors.New("controlsync: sync payload rejected")
	// ErrAckRejected is the explicit NEGATIVE answer to a status ack: control
	// handled the request but did not record the acknowledgement (its
	// response carried acknowledged:false). It is deliberately distinct from
	// a transport failure: a transport failure may leave a previously
	// recorded ack standing, while an explicit rejection means control holds
	// no ack for the submitted epoch and the gateway must withdraw any
	// watermark it was claiming for it (M5 r2 Fix A2).
	ErrAckRejected = errors.New("controlsync: status acknowledgement rejected")
)

// Limits carries the §14 concurrency ceilings distributed with a route.
type Limits struct {
	MaxStreamsPerOrigin int `json:"max_streams_per_origin"`
	MaxStreamsPerAgent  int `json:"max_streams_per_agent"`
	MaxStreamsGlobal    int `json:"max_streams_global"`
}

// Route is one exact relay route on the §11.3 wire (mirror of control's
// relayctl.Route).
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

// Snapshot is the full route snapshot fetched at boot and on revision gaps.
// Epoch is the control boot identifier that produced it — always present,
// including on empty snapshots. The applier compares epochs (never
// revisions) to decide that a newer control epoch wholesale-replaces route
// state and that an older epoch must be rejected (R2, §15.1).
type Snapshot struct {
	Version  int     `json:"version"`
	Epoch    uint64  `json:"epoch"`
	Revision uint64  `json:"revision"`
	Routes   []Route `json:"routes"`
	// PublishedAt is the RFC 3339 UTC time at which the revision stamped on
	// this snapshot was published by control's route publisher. It feeds the
	// §17.3 route propagation lag histogram and is OBSERVABILITY ONLY: the
	// applier ignores an absent or malformed value (§17.3 must never
	// fabricate a lag number) and the R2 epoch authority (Epoch) remains the
	// sole adoption decision. Mirrors control's relayctl.Snapshot field.
	PublishedAt string `json:"published_at,omitempty"`
}

// RouteDelta is one ordered route mutation.
type RouteDelta struct {
	Revision  uint64 `json:"revision"`
	Operation string `json:"operation"`
	Route     Route  `json:"route"`
}

// DeltaPage answers the gateway's ordered delta fetch. Status Gap forces
// snapshot reconciliation; neither side guesses across a revision gap. The
// page carries the control epoch that published it: the applier applies
// deltas only from the epoch it last snapshotted (R2).
type DeltaPage struct {
	Version        int          `json:"version"`
	Epoch          uint64       `json:"epoch"`
	Status         string       `json:"status"`
	Since          uint64       `json:"since"`
	LatestRevision uint64       `json:"latest_revision"`
	Deltas         []RouteDelta `json:"deltas"`
	// PublishedAt is the RFC 3339 UTC publish time of LatestRevision. It is
	// OBSERVABILITY ONLY and omitempty for older control builds; an absent
	// or malformed value records no lag observation. Mirrors control's
	// relayctl.DeltaPage field.
	PublishedAt string `json:"published_at,omitempty"`
}

// PresenceEvent is one gateway-authoritative tunnel lease fact (mirror of
// control's relayctl.PresenceEvent).
type PresenceEvent struct {
	GatewayBootID  string `json:"gateway_boot_id"`
	Revision       uint64 `json:"revision"`
	AgentRecordID  string `json:"agent_record_id"`
	RelayPort      int    `json:"relay_port"`
	Generation     uint64 `json:"generation"`
	State          string `json:"state"`
	LeaseExpiresAt string `json:"lease_expires_at,omitempty"`
}

// PresenceEnvelope carries a full presence snapshot or an ordered event
// batch; the endpoint decides the semantics.
type PresenceEnvelope struct {
	Version int             `json:"version"`
	Events  []PresenceEvent `json:"events"`
}

// StatusAck is the gateway's explicit acknowledgement of its last-applied
// route revision, tagged with its boot ID and the control epoch of the
// applied state (R2: control ignores acks from foreign epochs, so a control
// restart resets the sync-health watermark until reconciliation).
type StatusAck struct {
	Version             int    `json:"version"`
	GatewayBootID       string `json:"gateway_boot_id"`
	ControlEpoch        uint64 `json:"control_epoch"`
	LastAppliedRevision uint64 `json:"last_applied_revision"`
}

// AckReasonForeignEpoch mirrors control's bounded reason for an ack it did
// not record because the submitted control epoch was not control's own. Only
// this fixed token (and the generic fallback) may appear in an error or log
// line; see BoundedAckReason.
const AckReasonForeignEpoch = "foreign_epoch"

// StatusAckResponse echoes control's recorded last-applied revision for the
// reporting boot. Acknowledged is true only when control actually RECORDED
// the acknowledgement; a foreign-epoch (or otherwise unrecorded) ack answers
// acknowledged:false with a bounded Reason, and LastAppliedRevision is not
// meaningful. The gateway must treat acknowledged:false as "not acked".
type StatusAckResponse struct {
	Version             int    `json:"version"`
	Acknowledged        bool   `json:"acknowledged"`
	Reason              string `json:"reason,omitempty"`
	LastAppliedRevision uint64 `json:"last_applied_revision"`
}

// BoundedAckReason maps a control-reported acknowledgement reason to the
// fixed allowlist that may appear in an error (and therefore a log line), so
// an unexpected or high-cardinality reason can never leak into diagnostics.
func BoundedAckReason(reason string) string {
	if reason == AckReasonForeignEpoch {
		return reason
	}
	return "not_accepted"
}

// SyncReceipt is the bounded response to a presence publish.
type SyncReceipt struct {
	Version  int  `json:"version"`
	Accepted bool `json:"accepted"`
}

// ClientConfig is the complete gateway-side sync client configuration. The
// certificate material is PEM-encoded and independent of the content PKI:
// ServerCAPEM must contain the CA that issued control's sync server leaf,
// and the client leaf is issued by the CA control pins for gateway clients.
type ClientConfig struct {
	// BaseURL is control's private sync endpoint, https only.
	BaseURL string
	// ExpectedServerSAN is the exact control sync server identity pinned for
	// verification — never the URL host, never a system-CA decision.
	ExpectedServerSAN string
	// ServerCAPEM pins the control sync CA.
	ServerCAPEM []byte
	// ClientCertPEM and ClientKeyPEM form the gateway sync client identity.
	ClientCertPEM []byte
	ClientKeyPEM  []byte
	// Bounds: zero selects the protocol default; only values at or below the
	// defaults are accepted.
	MaxResponseBytes int
	MaxRequestBytes  int
	// RequestTimeout bounds every HTTP round trip. Zero selects the default.
	RequestTimeout time.Duration
	// Metrics receives the bounded §17.3 route revision gauges. Optional; the
	// client is a library and never registers a global registry itself.
	Metrics *metrics.Registry
}

// Client is the gateway's controlsync client. Safe for concurrent use.
type Client struct {
	baseURL          string
	httpClient       *http.Client
	maxResponseBytes int
	maxRequestBytes  int
	metrics          *metrics.Registry
}

// NewClient validates the fail-closed configuration and returns the client.
func NewClient(config ClientConfig) (*Client, error) {
	baseURL, err := validateSyncBaseURL(config.BaseURL)
	if err != nil {
		return nil, err
	}
	if err := validateSyncIdentity(config.ExpectedServerSAN); err != nil {
		return nil, fmt.Errorf("controlsync: expected server SAN: %w", err)
	}
	if len(config.ServerCAPEM) == 0 {
		return nil, errors.New("controlsync: pinned server CA required")
	}
	serverCAs := x509.NewCertPool()
	if !serverCAs.AppendCertsFromPEM(config.ServerCAPEM) {
		return nil, errors.New("controlsync: server CA PEM rejected")
	}
	if len(config.ClientCertPEM) == 0 || len(config.ClientKeyPEM) == 0 {
		return nil, errors.New("controlsync: client certificate and key required")
	}
	clientCertificate, err := tls.X509KeyPair(config.ClientCertPEM, config.ClientKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("controlsync: client key pair: %w", err)
	}
	maxResponseBytes, err := boundedSyncValue(config.MaxResponseBytes, MaxResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("controlsync: max response bytes: %w", err)
	}
	maxRequestBytes, err := boundedSyncValue(config.MaxRequestBytes, MaxRequestBodyBytes)
	if err != nil {
		return nil, fmt.Errorf("controlsync: max request bytes: %w", err)
	}
	requestTimeout := config.RequestTimeout
	if requestTimeout < 0 {
		return nil, errors.New("controlsync: negative request timeout")
	}
	if requestTimeout == 0 {
		requestTimeout = defaultRequestTimeout
	}

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			ServerName:   config.ExpectedServerSAN,
			RootCAs:      serverCAs,
			Certificates: []tls.Certificate{clientCertificate},
		},
	}
	return &Client{
		baseURL:          baseURL,
		httpClient:       &http.Client{Transport: transport, Timeout: requestTimeout},
		maxResponseBytes: maxResponseBytes,
		maxRequestBytes:  maxRequestBytes,
		metrics:          config.Metrics,
	}, nil
}

// FetchSnapshot retrieves control's full route snapshot (§11.3 full route
// snapshot fetch) and validates it before returning.
func (client *Client) FetchSnapshot(ctx context.Context) (Snapshot, error) {
	body, err := client.get(ctx, PathSnapshot)
	if err != nil {
		return Snapshot{}, err
	}
	var snapshot Snapshot
	if err := decodeSyncBody(body, &snapshot); err != nil {
		return Snapshot{}, err
	}
	if err := ValidateSnapshot(snapshot, MaxRoutesPerSnapshot); err != nil {
		return Snapshot{}, err
	}
	if client.metrics != nil {
		client.metrics.Set("sharebridge_relay_route_snapshot_revision", int64(snapshot.Revision))
	}
	return snapshot, nil
}

// FetchDeltas retrieves the ordered route deltas in (since, latest_revision].
// A gap page (or any revision violation) returns ErrBadRevision: the caller
// must refetch a full snapshot instead of guessing (§11.3, §15.7).
func (client *Client) FetchDeltas(ctx context.Context, since uint64) (DeltaPage, error) {
	body, err := client.get(ctx, PathDeltas+"?since="+strconv.FormatUint(since, 10))
	if err != nil {
		return DeltaPage{}, err
	}
	var page DeltaPage
	if err := decodeSyncBody(body, &page); err != nil {
		return DeltaPage{}, err
	}
	if err := ValidateDeltaPage(page, MaxDeltasPerPage); err != nil {
		return DeltaPage{}, err
	}
	if page.Since != since {
		return DeltaPage{}, fmt.Errorf("%w: page since %d does not answer requested %d", ErrBadRevision, page.Since, since)
	}
	if page.Status == DeltaStatusGap {
		// A well-formed gap page is still an error for the caller: control has
		// no deltas in (since, latest] and the gateway must reconcile from a
		// fresh snapshot instead of believing it is caught up (§11.3, §15.7).
		return DeltaPage{}, fmt.Errorf("%w: gap at revision %d", ErrBadRevision, page.LatestRevision)
	}
	if client.metrics != nil {
		client.metrics.Set("sharebridge_relay_route_delta_revision", int64(page.LatestRevision))
	}
	return page, nil
}

// PublishPresenceSnapshot posts the gateway's full presence state (boot and
// reconnect; the endpoint carries snapshot semantics).
func (client *Client) PublishPresenceSnapshot(ctx context.Context, envelope PresenceEnvelope) error {
	return client.publishPresence(ctx, PathPresenceSnapshot, envelope)
}

// PublishPresenceEvents posts an ordered batch of presence lease events.
func (client *Client) PublishPresenceEvents(ctx context.Context, envelope PresenceEnvelope) error {
	return client.publishPresence(ctx, PathPresenceEvents, envelope)
}

// SendStatus posts the explicit acknowledgement of the last-applied route
// revision and returns control's echoed record. The reported revision is sent
// exactly as applied — control owns the no-regression guard (Task 12).
func (client *Client) SendStatus(ctx context.Context, ack StatusAck) (StatusAckResponse, error) {
	if err := ValidateStatusAck(ack); err != nil {
		return StatusAckResponse{}, err
	}
	body, err := client.post(ctx, PathStatus, ack)
	if err != nil {
		return StatusAckResponse{}, err
	}
	var response StatusAckResponse
	if err := decodeSyncBody(body, &response); err != nil {
		return StatusAckResponse{}, err
	}
	if !response.Acknowledged {
		return StatusAckResponse{}, fmt.Errorf("%w: control refused to record the acknowledgement (reason %s)",
			ErrAckRejected, BoundedAckReason(response.Reason))
	}
	return response, nil
}

func (client *Client) publishPresence(ctx context.Context, path string, envelope PresenceEnvelope) error {
	validate := ValidatePresenceEventEnvelope
	if path == PathPresenceSnapshot {
		validate = ValidatePresenceSnapshotEnvelope
	}
	// Validate before anything touches the wire: our own producer bugs must
	// fail closed locally.
	if err := validate(envelope, MaxPresenceEventsPerEnvelope); err != nil {
		return err
	}
	body, err := client.post(ctx, path, envelope)
	if err != nil {
		return err
	}
	var receipt SyncReceipt
	if err := decodeSyncBody(body, &receipt); err != nil {
		return err
	}
	if !receipt.Accepted {
		return fmt.Errorf("%w: presence publish not accepted", ErrInvalidPayload)
	}
	return nil
}

func (client *Client) get(ctx context.Context, pathWithQuery string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, client.baseURL+pathWithQuery, nil)
	if err != nil {
		return nil, fmt.Errorf("controlsync: build request: %w", err)
	}
	return client.do(request)
}

func (client *Client) post(ctx context.Context, path string, payload any) ([]byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("controlsync: encode payload: %w", err)
	}
	if len(encoded) > client.maxRequestBytes {
		return nil, fmt.Errorf("%w: request body %d bytes, bound is %d", ErrOversize, len(encoded), client.maxRequestBytes)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("controlsync: build request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	return client.do(request)
}

func (client *Client) do(request *http.Request) ([]byte, error) {
	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("controlsync: request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("%w: status %d", ErrUnauthorized, response.StatusCode)
		}
		return nil, fmt.Errorf("controlsync: unexpected status %d from %s", response.StatusCode, request.URL.Path)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(client.maxResponseBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("controlsync: read response: %w", err)
	}
	if len(body) > client.maxResponseBytes {
		return nil, fmt.Errorf("%w: response body %d bytes, bound is %d", ErrOversize, len(body), client.maxResponseBytes)
	}
	return body, nil
}

// decodeSyncBody decodes exact JSON with unknown fields tolerated and
// trailing data rejected (fail closed, wire forward-compatibility preserved).
func decodeSyncBody(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%w: invalid JSON", ErrInvalidPayload)
	}
	if err := decoder.Decode(&struct{}{}); err == nil || err != io.EOF {
		return fmt.Errorf("%w: trailing JSON data", ErrInvalidPayload)
	}
	return nil
}

// Validation mirrors (identical rules to control's relayctl protocol).

func ValidateSnapshot(snapshot Snapshot, maxRoutes int) error {
	if snapshot.Version != ProtocolVersion {
		return fmt.Errorf("%w: snapshot version %d", ErrUnsupportedVersion, snapshot.Version)
	}
	if snapshot.Epoch == 0 {
		return fmt.Errorf("%w: snapshot control epoch is missing", ErrInvalidPayload)
	}
	if len(snapshot.Routes) > maxRoutes {
		return fmt.Errorf("%w: snapshot carries %d routes, bound is %d", ErrOversize, len(snapshot.Routes), maxRoutes)
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

func ValidateDeltaPage(page DeltaPage, maxDeltas int) error {
	if page.Version != ProtocolVersion {
		return fmt.Errorf("%w: delta page version %d", ErrUnsupportedVersion, page.Version)
	}
	if page.Epoch == 0 {
		return fmt.Errorf("%w: delta page control epoch is missing", ErrInvalidPayload)
	}
	if len(page.Deltas) > maxDeltas {
		return fmt.Errorf("%w: delta page carries %d deltas, bound is %d", ErrOversize, len(page.Deltas), maxDeltas)
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

func ValidatePresenceEvent(event PresenceEvent) error {
	if err := validateSyncIdentifier(event.GatewayBootID, "gateway_boot_id"); err != nil {
		return err
	}
	if err := validateSyncIdentifier(event.AgentRecordID, "agent_record_id"); err != nil {
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

func ValidatePresenceSnapshotEnvelope(envelope PresenceEnvelope, maxEvents int) error {
	if err := validatePresenceHeader(envelope, maxEvents); err != nil {
		return err
	}
	for _, event := range envelope.Events {
		if err := ValidatePresenceEvent(event); err != nil {
			return err
		}
		if event.GatewayBootID != envelope.Events[0].GatewayBootID {
			return fmt.Errorf("%w: presence snapshot mixes boot IDs", ErrInvalidPayload)
		}
		if event.Revision != envelope.Events[0].Revision {
			return fmt.Errorf("%w: presence snapshot mixes revisions", ErrBadRevision)
		}
	}
	return nil
}

func ValidatePresenceEventEnvelope(envelope PresenceEnvelope, maxEvents int) error {
	if err := validatePresenceHeader(envelope, maxEvents); err != nil {
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

func ValidateStatusAck(ack StatusAck) error {
	if ack.Version != ProtocolVersion {
		return fmt.Errorf("%w: status ack version %d", ErrUnsupportedVersion, ack.Version)
	}
	if ack.ControlEpoch == 0 {
		return fmt.Errorf("%w: status ack control epoch is missing", ErrInvalidPayload)
	}
	return validateSyncIdentifier(ack.GatewayBootID, "gateway_boot_id")
}

func validatePresenceHeader(envelope PresenceEnvelope, maxEvents int) error {
	if envelope.Version != ProtocolVersion {
		return fmt.Errorf("%w: presence envelope version %d", ErrUnsupportedVersion, envelope.Version)
	}
	if len(envelope.Events) > maxEvents {
		return fmt.Errorf("%w: presence envelope carries %d events, bound is %d", ErrOversize, len(envelope.Events), maxEvents)
	}
	return nil
}

func validateSyncHostname(hostname string) error {
	if hostname == "" || len(hostname) > MaxHostnameBytes {
		return fmt.Errorf("%w: hostname empty or longer than %d bytes", ErrInvalidPayload, MaxHostnameBytes)
	}
	if bytes.ContainsAny([]byte(hostname), "*? \t\r\n:/") {
		return fmt.Errorf("%w: hostname %q contains a forbidden character", ErrInvalidPayload, hostname)
	}
	return nil
}

// validateActiveLimits rejects a zero (or negative) ceiling on an ACTIVE
// route. Zero is not a limit value: the limits layer reads a zero route
// value as "unset" and restores the process default, so accepting a control
// "tightening to zero" would silently widen the ceiling. Revoke tombstones
// (inactive) legitimately carry zeroed limits and are not passed here. This
// is the gateway half of the by-construction zero-semantics agreement with
// control's publisher (relayctl.publishDeltaLocked rejects the same shape).
func validateActiveLimits(limits Limits) error {
	if limits.MaxStreamsPerOrigin <= 0 || limits.MaxStreamsPerAgent <= 0 || limits.MaxStreamsGlobal <= 0 {
		return fmt.Errorf("%w: active route limits must be positive, got %+v", ErrInvalidPayload, limits)
	}
	return nil
}

func ValidateRoute(route Route) error {
	if err := validateSyncHostname(route.Hostname); err != nil {
		return err
	}
	if err := validateSyncIdentifier(route.AgentRecordID, "agent_record_id"); err != nil {
		return err
	}
	if err := validateSyncIdentifier(route.SessionID, "session_id"); err != nil {
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

func validateSyncIdentifier(value string, field string) error {
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

func validateSyncIdentity(identity string) error {
	return validateSyncIdentifier(identity, "identity")
}

// parsePublishedAt parses an observability published_at stamp. ok=false for an
// absent or malformed value, in which case callers record no lag observation
// rather than fabricating one.
func parsePublishedAt(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

// validateSyncBaseURL accepts only an https URL with a host.
func validateSyncBaseURL(baseURL string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("controlsync: base URL: %w", err)
	}
	if parsed.Scheme != "https" || parsed.Host == "" {
		return "", errors.New("controlsync: base URL must be https://host[:port]")
	}
	return baseURL, nil
}

// boundedSyncValue resolves a configured bound: zero selects the protocol
// default, negative values are configuration errors, and values above the
// protocol default are refused — configuration may tighten bounds only.
func boundedSyncValue(configured int, defaultValue int) (int, error) {
	if configured < 0 {
		return 0, fmt.Errorf("%w: negative bound", ErrInvalidPayload)
	}
	if configured == 0 {
		return defaultValue, nil
	}
	if configured > defaultValue {
		return 0, fmt.Errorf("%w: bound %d exceeds protocol default %d", ErrOversize, configured, defaultValue)
	}
	return configured, nil
}
