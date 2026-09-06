// §11.3 control-side sync server (plan Task 11): a private-only, mutually
// authenticated HTTP listener that serves the route snapshot and ordered
// route deltas to the one configured gateway identity, receives the
// gateway-authoritative presence snapshot/event posts, and records explicit
// acknowledgements of the gateway's last-applied revision.
//
// Fail-closed posture: the configured bind address must be loopback or a
// private (RFC 1918/ULA) address — never the public interface; TLS is always
// TLS 1.3 with RequireAndVerifyClientCert against the pinned sync CA
// (independent of the content PKI); the client leaf certificate must carry
// the exact configured gateway SAN; every POST body is bounded, strict JSON
// with trailing-data rejection, and exact-version checked; a backend source
// that produces an invalid payload is never emitted (500, fail closed).
// Task 12 wires the publisher as RouteSource/StatusSink; Task 15 wires the
// presence view as PresenceSink. Nothing here is reachable from the public
// internet by construction.
package relayctl

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"strconv"
	"sync"
)

// Exact private sync endpoint paths, versioned by the v1 path segment plus
// the versioned payloads themselves.
const (
	PathSnapshot         = "/internal/relay/v1/snapshot"
	PathDeltas           = "/internal/relay/v1/deltas"
	PathPresenceSnapshot = "/internal/relay/v1/presence/snapshot"
	PathPresenceEvents   = "/internal/relay/v1/presence/events"
	PathStatus           = "/internal/relay/v1/status"
)

// RouteSource supplies the current route snapshot and ordered delta pages.
// The Task 12 publisher implements it; a nil source serves 503.
type RouteSource interface {
	RouteSnapshot() (Snapshot, error)
	RouteDeltas(since uint64) (DeltaPage, error)
}

// PresenceSink receives gateway-authoritative presence facts. The Task 15
// presence view implements it; a nil sink serves 503.
type PresenceSink interface {
	ApplyPresenceSnapshot(PresenceEnvelope) error
	ApplyPresenceEvents(PresenceEnvelope) error
}

// StatusSink receives explicit acknowledgements of the gateway's last-applied
// revision (the Task 12 publisher uses them to stop retrying deltas). A nil
// sink serves 503.
type StatusSink interface {
	Acknowledge(StatusAck) error
}

// ServerConfig is the complete sync server configuration. Certificate
// material is PEM-encoded and entirely independent of the content PKI: the
// server leaf is issued by the sync CA that gateway clients pin, and
// ClientCAPEM must contain the CA that issued the gateway's client leaf.
type ServerConfig struct {
	// BindAddress is the address the Task 12 wiring must listen on. It is
	// validated here and must be a numeric loopback or private address —
	// never unspecified, never public (§11.3, §17.1).
	BindAddress string
	// ServerCertPEM and ServerKeyPEM form the control sync server identity.
	ServerCertPEM []byte
	ServerKeyPEM  []byte
	// ClientCAPEM is the pinned sync client CA (gateway identities).
	ClientCAPEM []byte
	// ExpectedClientSAN is the one accepted gateway client SAN.
	ExpectedClientSAN string
	// RouteSource, PresenceSink, and StatusSink are the Task 12/15 backends.
	// nil backends make their endpoints serve 503 (fail closed).
	RouteSource  RouteSource
	PresenceSink PresenceSink
	StatusSink   StatusSink
	// Bounds: zero selects the protocol default; only values at or below the
	// defaults are accepted.
	MaxRequestBodyBytes int
	MaxRoutes           int
	MaxDeltas           int
	MaxPresenceEvents   int
	// Logger receives 500-class backend and validation events (no payload
	// content, no credential material). nil selects slog.Default.
	Logger *slog.Logger
}

// Server is the §11.3 sync server: an http.Handler plus its validated TLS
// configuration and bind address. Safe for concurrent use.
type Server struct {
	bindAddress         string
	tlsConfig           *tls.Config
	expectedClientSAN   string
	routeSource         RouteSource
	presenceSink        PresenceSink
	statusSink          StatusSink
	maxRequestBodyBytes int
	maxRoutes           int
	maxDeltas           int
	maxPresenceEvents   int
	logger              *slog.Logger

	mu                       sync.Mutex
	acknowledgedBootID       string
	lastAcknowledgedRevision uint64
	forwardFailed            bool
}

// NewServer validates the complete fail-closed configuration and returns the
// sync server. It does not listen: the Task 12 wiring listens on BindAddress
// with TLSConfig() and serves the returned handler.
func NewServer(config ServerConfig) (*Server, error) {
	bindAddress, err := validatePrivateBindAddress(config.BindAddress)
	if err != nil {
		return nil, err
	}
	if len(config.ServerCertPEM) == 0 || len(config.ServerKeyPEM) == 0 {
		return nil, fmt.Errorf("relayctl: sync server certificate and key required")
	}
	serverCertificate, err := tls.X509KeyPair(config.ServerCertPEM, config.ServerKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("relayctl: sync server key pair: %w", err)
	}
	if len(config.ClientCAPEM) == 0 {
		return nil, fmt.Errorf("relayctl: sync client CA required")
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(config.ClientCAPEM) {
		return nil, fmt.Errorf("relayctl: sync client CA PEM rejected")
	}
	if err := validateIdentity(config.ExpectedClientSAN); err != nil {
		return nil, fmt.Errorf("relayctl: expected client SAN: %w", err)
	}

	maxRequestBodyBytes, err := boundedValue(config.MaxRequestBodyBytes, MaxRequestBodyBytes)
	if err != nil {
		return nil, fmt.Errorf("relayctl: max request body bytes: %w", err)
	}
	maxRoutes, err := boundedValue(config.MaxRoutes, MaxRoutesPerSnapshot)
	if err != nil {
		return nil, fmt.Errorf("relayctl: max routes: %w", err)
	}
	maxDeltas, err := boundedValue(config.MaxDeltas, MaxDeltasPerPage)
	if err != nil {
		return nil, fmt.Errorf("relayctl: max deltas: %w", err)
	}
	maxPresenceEvents, err := boundedValue(config.MaxPresenceEvents, MaxPresenceEventsPerEnvelope)
	if err != nil {
		return nil, fmt.Errorf("relayctl: max presence events: %w", err)
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}

	tlsConfig := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{serverCertificate},
		ClientCAs:    clientCAs,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}
	return &Server{
		bindAddress:         bindAddress,
		tlsConfig:           tlsConfig,
		expectedClientSAN:   config.ExpectedClientSAN,
		routeSource:         config.RouteSource,
		presenceSink:        config.PresenceSink,
		statusSink:          config.StatusSink,
		maxRequestBodyBytes: maxRequestBodyBytes,
		maxRoutes:           maxRoutes,
		maxDeltas:           maxDeltas,
		maxPresenceEvents:   maxPresenceEvents,
		logger:              logger,
	}, nil
}

// BindAddress returns the validated private listener address for the Task 12
// wiring.
func (server *Server) BindAddress() string {
	return server.bindAddress
}

// TLSConfig returns a clone of the server's pinned TLS configuration.
func (server *Server) TLSConfig() *tls.Config {
	return server.tlsConfig.Clone()
}

// LastAcknowledgedRevision reports the last-applied revision recorded for the
// given gateway boot ID. A different boot ID has no recorded revision yet
// (gateway restarts may legitimately report lower revisions, §15.1).
func (server *Server) LastAcknowledgedRevision(gatewayBootID string) uint64 {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.acknowledgedBootID != gatewayBootID {
		return 0
	}
	return server.lastAcknowledgedRevision
}

func (server *Server) ServeHTTP(responseWriter http.ResponseWriter, request *http.Request) {
	responseWriter.Header().Set("Content-Type", "application/json")
	responseWriter.Header().Set("X-Content-Type-Options", "nosniff")

	// Mounted without TLS is a configuration error: refuse rather than serve.
	if request.TLS == nil {
		server.writeError(responseWriter, http.StatusBadRequest, "tls required")
		return
	}
	if !server.authorizedPeer(request) {
		server.writeError(responseWriter, http.StatusForbidden, "peer identity rejected")
		return
	}

	path, method := request.URL.Path, request.Method
	switch {
	case path == PathSnapshot && method == http.MethodGet:
		server.handleSnapshot(responseWriter)
	case path == PathDeltas && method == http.MethodGet:
		server.handleDeltas(responseWriter, request)
	case path == PathPresenceSnapshot && method == http.MethodPost:
		server.handlePresence(responseWriter, request, server.presenceSnapshotApplier())
	case path == PathPresenceEvents && method == http.MethodPost:
		server.handlePresence(responseWriter, request, server.presenceEventsApplier())
	case path == PathStatus && method == http.MethodPost:
		server.handleStatus(responseWriter, request)
	case path == PathSnapshot || path == PathDeltas || path == PathPresenceSnapshot ||
		path == PathPresenceEvents || path == PathStatus:
		server.writeError(responseWriter, http.StatusMethodNotAllowed, "method not allowed")
	default:
		server.writeError(responseWriter, http.StatusNotFound, "not found")
	}
}

// authorizedPeer re-checks, at the application layer, that the TLS handshake
// verified the peer against the pinned client CA and that the leaf carries
// exactly the configured gateway SAN (defense in depth over ClientAuth).
func (server *Server) authorizedPeer(request *http.Request) bool {
	if len(request.TLS.VerifiedChains) == 0 || len(request.TLS.PeerCertificates) == 0 {
		return false
	}
	leaf := request.TLS.PeerCertificates[0]
	if leaf.IsCA {
		return false
	}
	return leaf.VerifyHostname(server.expectedClientSAN) == nil
}

func (server *Server) handleSnapshot(responseWriter http.ResponseWriter) {
	if server.routeSource == nil {
		server.writeError(responseWriter, http.StatusServiceUnavailable, "route source unavailable")
		return
	}
	snapshot, err := server.routeSource.RouteSnapshot()
	if err != nil {
		server.logBackendFailure("snapshot", err)
		server.writeError(responseWriter, http.StatusInternalServerError, "route source failure")
		return
	}
	// A source that produces an invalid snapshot is never emitted.
	if err := ValidateSnapshot(snapshot, server.maxRoutes); err != nil {
		server.logBackendFailure("snapshot", err)
		server.writeError(responseWriter, http.StatusInternalServerError, "route source produced an invalid snapshot")
		return
	}
	server.writeJSON(responseWriter, http.StatusOK, snapshot)
}

func (server *Server) handleDeltas(responseWriter http.ResponseWriter, request *http.Request) {
	if server.routeSource == nil {
		server.writeError(responseWriter, http.StatusServiceUnavailable, "route source unavailable")
		return
	}
	sinceValues := request.URL.Query()["since"]
	if len(sinceValues) != 1 {
		server.writeError(responseWriter, http.StatusBadRequest, "exactly one since parameter required")
		return
	}
	since, err := strconv.ParseUint(sinceValues[0], 10, 64)
	if err != nil {
		server.writeError(responseWriter, http.StatusBadRequest, "since must be a non-negative revision")
		return
	}
	page, err := server.routeSource.RouteDeltas(since)
	if err != nil {
		server.logBackendFailure("deltas", err)
		server.writeError(responseWriter, http.StatusInternalServerError, "route source failure")
		return
	}
	// The page must answer exactly the requested since and be well ordered.
	if page.Since != since {
		err := fmt.Errorf("%w: source page since %d does not match request %d", ErrBadRevision, page.Since, since)
		server.logBackendFailure("deltas", err)
		server.writeError(responseWriter, http.StatusInternalServerError, "route source produced an invalid delta page")
		return
	}
	if err := ValidateDeltaPage(page, server.maxDeltas); err != nil {
		server.logBackendFailure("deltas", err)
		server.writeError(responseWriter, http.StatusInternalServerError, "route source produced an invalid delta page")
		return
	}
	server.writeJSON(responseWriter, http.StatusOK, page)
}

func (server *Server) presenceSnapshotApplier() func(PresenceEnvelope) error {
	if server.presenceSink == nil {
		return nil
	}
	return server.presenceSink.ApplyPresenceSnapshot
}

func (server *Server) presenceEventsApplier() func(PresenceEnvelope) error {
	if server.presenceSink == nil {
		return nil
	}
	return server.presenceSink.ApplyPresenceEvents
}

// handlePresence validates and forwards one presence envelope. The applier is
// nil when the PresenceSink backend is absent (503).
func (server *Server) handlePresence(responseWriter http.ResponseWriter, request *http.Request, apply func(PresenceEnvelope) error) {
	if apply == nil {
		server.writeError(responseWriter, http.StatusServiceUnavailable, "presence sink unavailable")
		return
	}
	var envelope PresenceEnvelope
	if !server.decodeJSONBody(responseWriter, request, &envelope) {
		return
	}
	// Both shapes share the wire form; the endpoint decides the semantics
	// (snapshot: one revision; events: strictly increasing revisions).
	validate := ValidatePresenceEventEnvelope
	if request.URL.Path == PathPresenceSnapshot {
		validate = ValidatePresenceSnapshotEnvelope
	}
	if err := validate(envelope, server.maxPresenceEvents); err != nil {
		server.writeProtocolError(responseWriter, err)
		return
	}
	if err := apply(envelope); err != nil {
		server.logBackendFailure(request.URL.Path, err)
		server.writeError(responseWriter, http.StatusInternalServerError, "presence sink failure")
		return
	}
	server.writeJSON(responseWriter, http.StatusOK, SyncReceipt{Version: ProtocolVersion, Accepted: true})
}

func (server *Server) handleStatus(responseWriter http.ResponseWriter, request *http.Request) {
	if server.statusSink == nil {
		server.writeError(responseWriter, http.StatusServiceUnavailable, "status sink unavailable")
		return
	}
	var ack StatusAck
	if !server.decodeJSONBody(responseWriter, request, &ack) {
		return
	}
	if err := ValidateStatusAck(ack); err != nil {
		server.writeProtocolError(responseWriter, err)
		return
	}

	server.mu.Lock()
	forward := false
	if ack.GatewayBootID != server.acknowledgedBootID {
		// New gateway epoch: its applied revision replaces the old record
		// wholesale — a restarted gateway may apply a lower revision again.
		server.acknowledgedBootID = ack.GatewayBootID
		server.lastAcknowledgedRevision = ack.LastAppliedRevision
		forward = true
	} else if ack.LastAppliedRevision > server.lastAcknowledgedRevision {
		server.lastAcknowledgedRevision = ack.LastAppliedRevision
		forward = true
	} else if server.forwardFailed && ack.LastAppliedRevision == server.lastAcknowledgedRevision {
		// The previous forward failed before the publisher saw it: re-deliver
		// the equal acknowledgement so a retry cannot be swallowed.
		forward = true
	}
	echoed := server.lastAcknowledgedRevision
	server.forwardFailed = false
	server.mu.Unlock()

	if forward {
		if err := server.statusSink.Acknowledge(ack); err != nil {
			server.mu.Lock()
			server.forwardFailed = true
			server.mu.Unlock()
			server.logBackendFailure(PathStatus, err)
			server.writeError(responseWriter, http.StatusInternalServerError, "status sink failure")
			return
		}
	}
	server.writeJSON(responseWriter, http.StatusOK, StatusAckResponse{
		Version:             ProtocolVersion,
		Acknowledged:        true,
		LastAppliedRevision: echoed,
	})
}

// decodeJSONBody enforces the bounded strict-body contract shared by every
// POST endpoint: application/json only, at most maxRequestBodyBytes, exact
// JSON with unknown fields tolerated and trailing data rejected.
func (server *Server) decodeJSONBody(responseWriter http.ResponseWriter, request *http.Request, target any) bool {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		server.writeError(responseWriter, http.StatusUnsupportedMediaType, "application/json required")
		return false
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, int64(server.maxRequestBodyBytes)+1))
	if err != nil {
		server.writeError(responseWriter, http.StatusBadRequest, "unreadable body")
		return false
	}
	if len(body) > server.maxRequestBodyBytes {
		server.writeError(responseWriter, http.StatusRequestEntityTooLarge, "body exceeds bounds")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(target); err != nil {
		server.writeError(responseWriter, http.StatusBadRequest, "invalid JSON")
		return false
	}
	if err := decoder.Decode(&struct{}{}); err == nil || err != io.EOF {
		server.writeError(responseWriter, http.StatusBadRequest, "trailing JSON data")
		return false
	}
	return true
}

func (server *Server) writeProtocolError(responseWriter http.ResponseWriter, err error) {
	// Every validation failure class is a client error with a bounded static
	// message; details are never echoed back.
	server.writeError(responseWriter, http.StatusBadRequest, "payload rejected")
}

func (server *Server) writeError(responseWriter http.ResponseWriter, status int, message string) {
	server.writeJSON(responseWriter, status, errorBody{Error: message})
}

func (server *Server) writeJSON(responseWriter http.ResponseWriter, status int, payload any) {
	responseWriter.WriteHeader(status)
	_ = json.NewEncoder(responseWriter).Encode(payload)
}

func (server *Server) logBackendFailure(endpoint string, err error) {
	server.logger.Warn("relayctl: sync backend rejected", "endpoint", endpoint, "error", err)
}

type errorBody struct {
	Error string `json:"error"`
}

// validatePrivateBindAddress accepts only numeric loopback or private
// (RFC 1918 / RFC 4193) addresses with a port — the §11.3 interface is
// private-network only, never the public interface, never unspecified.
func validatePrivateBindAddress(address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", fmt.Errorf("relayctl: sync bind address must be host:port: %w", err)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return "", fmt.Errorf("relayctl: sync bind port invalid")
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "", fmt.Errorf("relayctl: sync bind address must be a numeric IP")
	}
	if ip.IsUnspecified() || !(ip.IsLoopback() || ip.IsPrivate()) {
		return "", fmt.Errorf("relayctl: sync bind address must be loopback or private, got %q", host)
	}
	return address, nil
}

func validateIdentity(identity string) error {
	if identity == "" || len(identity) > MaxIdentityBytes {
		return fmt.Errorf("%w: identity empty or longer than %d bytes", ErrInvalidPayload, MaxIdentityBytes)
	}
	for index := 0; index < len(identity); index++ {
		if identity[index] < 0x21 || identity[index] > 0x7e {
			return fmt.Errorf("%w: identity contains a forbidden byte", ErrInvalidPayload)
		}
	}
	return nil
}

// boundedValue resolves a configured bound: zero selects the protocol default,
// negative values are configuration errors, and values above the protocol
// default are refused — configuration may tighten bounds only.
func boundedValue(configured int, defaultValue int) (int, error) {
	if configured < 0 {
		return 0, fmt.Errorf("%w: negative bound", ErrInvalidPayload)
	}
	if configured == 0 {
		return defaultValue, nil
	}
	if configured > defaultValue {
		return 0, fmt.Errorf("%w: bound %d exceeds protocol default %d", ErrPayloadTooLarge, configured, defaultValue)
	}
	return configured, nil
}
