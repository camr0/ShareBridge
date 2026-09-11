// Package frpplugin implements the local fail-closed HTTP authorization
// boundary used by the pinned frps process. It intentionally mirrors only the
// FRP v0.71.0 server-plugin wire fields needed by ShareBridge.
package frpplugin

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
)

const (
	// FRPPluginAPIVersion is the API version emitted by FRP v0.71.0.
	FRPPluginAPIVersion = "0.1.0"

	OperationLogin      = "Login"
	OperationNewProxy   = "NewProxy"
	OperationCloseProxy = "CloseProxy"
	OperationPing       = "Ping"
	// OperationNewUserConn is the readiness-only op (Task 7 amendment, spec
	// §7.3): it never rejects and never authorizes, it only records the
	// readiness correlation tuple for the presence registry.
	OperationNewUserConn = "NewUserConn"
	// OperationSessionReset is the §15.2 frps session-reset lifecycle signal:
	// a Login re-presented an already-burned one-use credential. Because the
	// credential is one-use, that re-presentation means the FRP session it
	// created is gone (frps restarted and the agent's frpc reconnected), so
	// the presence registry clears that agent immediately instead of waiting
	// out the 45-second lease. It is not an authorization op and carries only
	// the rejected credential's bounded identity.
	OperationSessionReset = "SessionReset"

	// APIPath is configured as the HTTP server-plugin path in frps.
	APIPath = "/frp/authorize"

	// PluginAuthUsername is paired with the operator-only shared secret using
	// HTTP Basic authentication. FRP's HTTP plugin supports this without agent
	// access by putting userinfo in its loopback addr URL.
	PluginAuthUsername = "sharebridge-frps"

	// The credential and generation travel in login metadata; FRP v0.71.0
	// copies that authenticated login UserInfo into NewProxy/CloseProxy/Ping.
	CredentialMetadataKey = "sharebridge_credential"
	GenerationMetadataKey = "sharebridge_generation"

	MaxRequestBodyBytes = 64 * 1024

	defaultMaxReplayEntries = 4096
	defaultMaxSessions      = 1024
	defaultMaxPendingEvents = 1024
	maxRunIDBytes           = 128
	maxSharedSecretBytes    = 512

	// maxUserConnDedupEntries bounds the per-user-connection deduplication
	// set (Task 7 amendment). When full it is reset wholesale: dedup is
	// best-effort metadata hygiene — a dropped dedup entry can let a
	// duplicate readiness fact through, which the presence registry
	// ignores idempotently; the bound itself never lapses.
	maxUserConnDedupEntries = 1024
	// maxRemoteAddrBytes bounds one correlation remote address
	// ("127.0.0.1:port" and the IPv6 loopback form both fit comfortably).
	maxRemoteAddrBytes = 64

	// admissionStateFileVersion is the persisted admission-state schema
	// version; a file written by any other schema refuses startup.
	admissionStateFileVersion = 1
	// maxAdmissionStateBytes bounds the persisted admission-state file read
	// at startup. The entry caps below bound a legitimate file far below
	// this, so a larger file is corrupt or hostile and refuses startup.
	maxAdmissionStateBytes = 2 << 20
)

// PresenceFact is the credential-free fact stream consumed by Task 14's
// presence registry. Login/NewProxy/CloseProxy/Ping facts confirm only that
// the FRP plugin authorized those calls. SessionReset is the §15.2
// lifecycle fact emitted when a Login re-presents an already-burned one-use
// credential (an frps session reset). NewUserConn is the readiness-only
// correlation fact (Task 7 amendment): frps fires it from the accept loop of
// a listener it actually bound, and the registry confirms readiness only when
// its four correlation fields (proxy name, server-assigned run id, generation
// metadata, and remote_addr — the gateway probe socket's source address as
// seen by frps) match the exact current generation. Each fact contains only
// bounded routing identity and lifecycle data validated at the FRP boundary;
// never credential material.
type PresenceFact struct {
	Operation     string `json:"operation"`
	AgentRecordID string `json:"agent_record_id"`
	Namespace     string `json:"namespace"`
	ProxyName     string `json:"proxy_name"`
	RelayPort     int    `json:"relay_port"`
	Generation    int    `json:"generation"`
	RunID         string `json:"run_id"`
	// RemoteAddr is the NewUserConn correlation address ("ip:port" as seen
	// by frps); empty for all other operations.
	RemoteAddr string `json:"remote_addr,omitempty"`
}

// PresenceEvents is deliberately small so Task 14 can attach the leased
// registry without coupling it to FRP request structs. Server invokes one
// callback at a time, in authorization order, outside its admission mutex.
type PresenceEvents interface {
	ObserveFRPEvent(PresenceFact)
}

type discardPresenceEvents struct{}

func (discardPresenceEvents) ObserveFRPEvent(PresenceFact) {}

// Config is the complete authorization boundary configuration. The relay gets
// only ControlPublicKey, never control's signing seed or browser TLS material.
type Config struct {
	ControlPublicKey   ed25519.PublicKey
	PluginSharedSecret string
	RelayPortMin       int
	RelayPortMax       int
	MaxReplayEntries   int
	MaxSessions        int
	MaxPendingEvents   int
	Now                func() time.Time
	PresenceEvents     PresenceEvents
	// StatePath persists the admission replay state — the burned replay-JTI
	// set and the per-agent issued-at/generation high-water — across gateway
	// restarts for the ten-minute credential horizon (spec §7.2: any
	// reconnect requires a fresh credential, including after a restart).
	// The file holds identifiers and timestamps only, never token material;
	// it is written atomically (temp file + rename) with mode 0600 and is
	// TTL-pruned to the ten-minute window and hard-capped. An existing file
	// is loaded at startup: an unreadable, corrupt, unknown-schema, or
	// over-cap file fails startup (fail closed) rather than admitting with
	// wiped replay history. Empty disables persistence (process memory only).
	StatePath string
	// Chmod defaults to os.Chmod. It exists so tests can inject a failing
	// (or no-op) implementation to prove the loaded state file's 0600
	// permission repair fails closed — a read-only directory does not make
	// the owner's chmod(2) fail, so the seam is the only portable way to
	// exercise that path.
	Chmod func(string, os.FileMode) error
}

// Server is both the HTTP handler and the bounded, concurrency-safe admission
// state for active FRP client generations.
type Server struct {
	controlPublicKey   ed25519.PublicKey
	pluginSharedSecret string
	relayPortMin       int
	relayPortMax       int
	maxReplayEntries   int
	maxSessions        int
	now                func() time.Time
	presenceEvents     PresenceEvents
	eventQueue         chan PresenceFact
	eventSlots         chan struct{}

	mu          sync.Mutex
	replayedJTI map[string]time.Time
	// admissionHighWater is the per-agent issued-at/generation admission
	// fence: the newest credential ever admitted for that agent record. It
	// mirrors agentSessions' fence while a session is live and survives
	// gateway restarts via the state file, so a replayed or superseded
	// credential stays rejected within the ten-minute admission horizon.
	admissionHighWater map[string]admissionMark
	agentSessions      map[string]*sessionState
	sessionsByToken    map[[sha256.Size]byte]*sessionState
	userConnSeen       map[[sha256.Size]byte]struct{}
	statePath          string
	// chmod defaults to os.Chmod (see Config.Chmod): the injection seam that
	// lets tests prove the state file's 0600 repair fails closed.
	chmod func(string, os.FileMode) error
}

// admissionMark is the issued-at/generation admission high-water for one
// agent record (see Server.admissionHighWater).
type admissionMark struct {
	generation int
	issuedAt   time.Time
}

// persistedAdmissionState is the JSON schema of the admission-state file. It
// carries identifiers and timestamps only — never credential token material.
type persistedAdmissionState struct {
	Version             int                      `json:"version"`
	JTIs                []persistedJTI           `json:"jtis"`
	GenerationHighWater []persistedAdmissionMark `json:"generation_high_water"`
}

type persistedJTI struct {
	JTI       string    `json:"jti"`
	ExpiresAt time.Time `json:"expires_at"`
}

type persistedAdmissionMark struct {
	AgentRecordID string    `json:"agent_record_id"`
	Generation    int       `json:"generation"`
	IssuedAt      time.Time `json:"issued_at"`
}

type sessionState struct {
	claims          CredentialClaims
	tokenHash       [sha256.Size]byte
	runID           string
	proxyAuthorized bool
	closed          bool
}

type pluginRequest struct {
	Version string          `json:"version"`
	Op      string          `json:"op"`
	Content json.RawMessage `json:"content"`
}

type pluginResponse struct {
	Reject       bool   `json:"reject"`
	RejectReason string `json:"reject_reason"`
	Unchange     bool   `json:"unchange"`
	Content      any    `json:"content"`
}

type loginContent struct {
	Version       string            `json:"version,omitempty"`
	Hostname      string            `json:"hostname,omitempty"`
	OS            string            `json:"os,omitempty"`
	Arch          string            `json:"arch,omitempty"`
	User          string            `json:"user,omitempty"`
	PrivilegeKey  string            `json:"privilege_key,omitempty"`
	Timestamp     int64             `json:"timestamp,omitempty"`
	RunID         string            `json:"run_id,omitempty"`
	ClientID      string            `json:"client_id,omitempty"`
	Metas         map[string]string `json:"metas,omitempty"`
	ClientSpec    clientSpec        `json:"client_spec,omitempty"`
	PoolCount     int               `json:"pool_count,omitempty"`
	ClientAddress string            `json:"client_address,omitempty"`
}

type clientSpec struct {
	Type           string `json:"type,omitempty"`
	AlwaysAuthPass bool   `json:"always_auth_pass,omitempty"`
}

type userInfo struct {
	User  string            `json:"user"`
	Metas map[string]string `json:"metas"`
	RunID string            `json:"run_id"`
}

type newProxyContent struct {
	User               userInfo          `json:"user"`
	ProxyName          string            `json:"proxy_name,omitempty"`
	ProxyType          string            `json:"proxy_type,omitempty"`
	UseEncryption      bool              `json:"use_encryption,omitempty"`
	UseCompression     bool              `json:"use_compression,omitempty"`
	BandwidthLimit     string            `json:"bandwidth_limit,omitempty"`
	BandwidthLimitMode string            `json:"bandwidth_limit_mode,omitempty"`
	Group              string            `json:"group,omitempty"`
	GroupKey           string            `json:"group_key,omitempty"`
	Metas              map[string]string `json:"metas,omitempty"`
	Annotations        map[string]string `json:"annotations,omitempty"`
	RemotePort         int               `json:"remote_port,omitempty"`
	CustomDomains      []string          `json:"custom_domains,omitempty"`
	Subdomain          string            `json:"subdomain,omitempty"`
	Locations          []string          `json:"locations,omitempty"`
	HTTPUser           string            `json:"http_user,omitempty"`
	HTTPPassword       string            `json:"http_pwd,omitempty"`
	HostHeaderRewrite  string            `json:"host_header_rewrite,omitempty"`
	Headers            map[string]string `json:"headers,omitempty"`
	ResponseHeaders    map[string]string `json:"response_headers,omitempty"`
	RouteByHTTPUser    string            `json:"route_by_http_user,omitempty"`
	SecretKey          string            `json:"sk,omitempty"`
	AllowUsers         []string          `json:"allow_users,omitempty"`
	Multiplexer        string            `json:"multiplexer,omitempty"`
}

type pingContent struct {
	User         userInfo `json:"user"`
	PrivilegeKey string   `json:"privilege_key,omitempty"`
	Timestamp    int64    `json:"timestamp,omitempty"`
}

type closeProxyContent struct {
	User      userInfo `json:"user"`
	ProxyName string   `json:"proxy_name,omitempty"`
}

type newUserConnContent struct {
	User       userInfo `json:"user"`
	ProxyName  string   `json:"proxy_name,omitempty"`
	ProxyType  string   `json:"proxy_type,omitempty"`
	RemoteAddr string   `json:"remote_addr,omitempty"`
}

// NewServer validates all fail-closed dependencies before returning a handler.
func NewServer(config Config) (*Server, error) {
	if len(config.ControlPublicKey) != ed25519.PublicKeySize {
		return nil, errors.New("control Ed25519 public key required")
	}
	if config.PluginSharedSecret == "" || len(config.PluginSharedSecret) > maxSharedSecretBytes {
		return nil, errors.New("bounded plugin shared secret required")
	}
	if config.RelayPortMin < 1024 || config.RelayPortMax > 65535 || config.RelayPortMin > config.RelayPortMax {
		return nil, errors.New("invalid relay port range")
	}
	if config.MaxReplayEntries == 0 {
		config.MaxReplayEntries = defaultMaxReplayEntries
	}
	if config.MaxSessions == 0 {
		config.MaxSessions = defaultMaxSessions
	}
	if config.MaxPendingEvents == 0 {
		config.MaxPendingEvents = defaultMaxPendingEvents
	}
	if config.MaxReplayEntries < 0 || config.MaxSessions < 0 || config.MaxPendingEvents < 0 {
		return nil, errors.New("state bounds must be positive")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Chmod == nil {
		config.Chmod = os.Chmod
	}
	if config.PresenceEvents == nil {
		config.PresenceEvents = discardPresenceEvents{}
	}

	server := &Server{
		controlPublicKey:   append(ed25519.PublicKey(nil), config.ControlPublicKey...),
		pluginSharedSecret: config.PluginSharedSecret,
		relayPortMin:       config.RelayPortMin,
		relayPortMax:       config.RelayPortMax,
		maxReplayEntries:   config.MaxReplayEntries,
		maxSessions:        config.MaxSessions,
		now:                config.Now,
		presenceEvents:     config.PresenceEvents,
		eventQueue:         make(chan PresenceFact, config.MaxPendingEvents),
		eventSlots:         make(chan struct{}, config.MaxPendingEvents),
		replayedJTI:        make(map[string]time.Time),
		admissionHighWater: make(map[string]admissionMark),
		agentSessions:      make(map[string]*sessionState),
		sessionsByToken:    make(map[[sha256.Size]byte]*sessionState),
		userConnSeen:       make(map[[sha256.Size]byte]struct{}),
		statePath:          config.StatePath,
		chmod:              config.Chmod,
	}
	// Load persisted admission state BEFORE the event dispatcher starts and
	// before any request can be admitted: a corrupt or over-cap state file
	// fails startup (fail closed) instead of admitting with wiped history
	// (SOL mid-project review Important-6).
	if err := server.loadAdmissionState(); err != nil {
		return nil, err
	}
	go server.dispatchPresenceEvents()
	return server, nil
}

func (server *Server) ServeHTTP(responseWriter http.ResponseWriter, request *http.Request) {
	responseWriter.Header().Set("Content-Type", "application/json")
	responseWriter.Header().Set("X-Content-Type-Options", "nosniff")

	if !server.validTransportCaller(request) {
		server.writeRejected(responseWriter)
		return
	}
	requestEnvelope, err := server.decodeRequest(request)
	if err != nil {
		server.writeRejected(responseWriter)
		return
	}

	var accepted bool
	switch requestEnvelope.Op {
	case OperationLogin:
		accepted = server.handleLogin(requestEnvelope.Content)
	case OperationNewProxy:
		accepted = server.handleNewProxy(requestEnvelope.Content)
	case OperationCloseProxy:
		accepted = server.handleCloseProxy(requestEnvelope.Content)
	case OperationPing:
		accepted = server.handlePing(requestEnvelope.Content)
	case OperationNewUserConn:
		// Readiness-only: the response is FRP's accept response regardless of
		// the callback's content (accept-mode probe, spec §7.3). It is never
		// an authorization input and never rejects a user connection.
		server.handleNewUserConn(requestEnvelope.Content)
		accepted = true
	default:
		accepted = false
	}
	if !accepted {
		server.writeRejected(responseWriter)
		return
	}
	server.writeJSON(responseWriter, pluginResponse{Reject: false, Unchange: true})
}

func (server *Server) validTransportCaller(request *http.Request) bool {
	if request.Method != http.MethodPost || request.URL.Path != APIPath {
		return false
	}
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		return false
	}
	peerIP := net.ParseIP(host)
	if peerIP == nil || !peerIP.IsLoopback() {
		return false
	}
	username, password, ok := request.BasicAuth()
	if !ok || subtle.ConstantTimeCompare([]byte(username), []byte(PluginAuthUsername)) != 1 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(password), []byte(server.pluginSharedSecret)) == 1
}

func (server *Server) decodeRequest(request *http.Request) (pluginRequest, error) {
	var envelope pluginRequest
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return envelope, errors.New("invalid content type")
	}
	query := request.URL.Query()
	if len(query) != 2 || len(query["version"]) != 1 || len(query["op"]) != 1 ||
		query.Get("version") != FRPPluginAPIVersion {
		return envelope, errors.New("invalid plugin query")
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, MaxRequestBodyBytes+1))
	if err != nil || len(body) > MaxRequestBodyBytes {
		return envelope, errors.New("invalid plugin body")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&envelope); err != nil {
		return envelope, errors.New("invalid plugin JSON")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return envelope, errors.New("trailing plugin JSON")
	}
	if envelope.Version != FRPPluginAPIVersion || envelope.Op == "" || envelope.Op != query.Get("op") || len(envelope.Content) == 0 {
		return envelope, errors.New("invalid plugin envelope")
	}
	return envelope, nil
}

func (server *Server) handleLogin(rawContent json.RawMessage) bool {
	var content loginContent
	if json.Unmarshal(rawContent, &content) != nil || !validOptionalRunID(content.RunID) || content.PoolCount < 0 || content.PoolCount > 1 ||
		content.ClientID != "" || content.ClientSpec.Type != "" || content.ClientSpec.AlwaysAuthPass ||
		!validLoginMetadata(content.Metas) {
		return false
	}
	credential, err := verifyCredential(server.controlPublicKey, content.Metas[CredentialMetadataKey], server.now().UTC())
	// FRP prefixes every proxy name with a non-empty Login.user. ShareBridge's
	// control-signed proxy name is exact, so the FRP user must remain empty;
	// agent identity comes exclusively from the signed credential.
	if err != nil || content.User != "" ||
		content.Metas[GenerationMetadataKey] != strconv.Itoa(credential.claims.Generation) ||
		credential.claims.RelayPort < server.relayPortMin || credential.claims.RelayPort > server.relayPortMax {
		return false
	}

	server.mu.Lock()
	defer server.mu.Unlock()
	now := server.now().UTC()
	if !credential.claims.ExpiresAt.After(now) {
		return false
	}
	server.pruneExpiredReplayLocked(now)
	if _, replayed := server.replayedJTI[credential.claims.JTI]; replayed {
		// §15.2 production trigger. The credential is one-use, so a replay is
		// the agent's frpc reconnecting after the FRP session that consumed it
		// was reset (an frps-only restart leaves this plugin and its memory
		// untouched). The plugin only accepts loopback callers holding the
		// operator shared secret, so the replay cannot be forged from outside
		// the frps boundary. Emit the session-reset fact BEFORE rejecting so
		// the gateway clears this agent's presence and drains its established
		// streams immediately rather than at lease expiry.
		if server.reserveEventLocked() {
			server.emitReservedLocked(OperationSessionReset, credential.claims, content.RunID)
		}
		return false
	}
	current, exists := server.agentSessions[credential.claims.AgentRecordID]
	if exists && (current.claims.Generation > credential.claims.Generation ||
		(current.claims.Generation == credential.claims.Generation &&
			!credential.claims.IssuedAt.After(current.claims.IssuedAt))) {
		return false
	}
	// The persisted high-water mirrors the live session's fence while a
	// session exists and covers the post-restart window where agentSessions
	// is empty: a replayed or superseded credential stays rejected within
	// the ten-minute admission horizon (SOL mid-project review Important-6).
	if mark, fenced := server.admissionHighWater[credential.claims.AgentRecordID]; fenced &&
		(mark.generation > credential.claims.Generation ||
			(mark.generation == credential.claims.Generation && !credential.claims.IssuedAt.After(mark.issuedAt))) {
		return false
	}
	if !exists && len(server.agentSessions) >= server.maxSessions {
		return false
	}
	// Keep the persisted high-water cardinality inside its hard cap: a brand
	// new agent is admitted only while a high-water slot is free (fail
	// closed; horizon pruning frees slots within the credential window).
	if _, marked := server.admissionHighWater[credential.claims.AgentRecordID]; !marked && !exists &&
		len(server.admissionHighWater) >= server.maxSessions {
		return false
	}
	if len(server.replayedJTI) >= server.maxReplayEntries || !server.reserveEventLocked() {
		return false
	}

	// Persist BEFORE mutating memory: a persistence failure rejects the
	// login instead of admitting history the next boot would forget.
	if server.statePath != "" {
		if err := server.writeAdmissionStateLocked(credential.claims, now); err != nil {
			server.releaseEventSlotLocked()
			return false
		}
	}

	server.replayedJTI[credential.claims.JTI] = credential.claims.ExpiresAt
	if exists {
		delete(server.sessionsByToken, current.tokenHash)
	}
	server.admissionHighWater[credential.claims.AgentRecordID] = admissionMark{
		generation: credential.claims.Generation,
		issuedAt:   credential.claims.IssuedAt,
	}
	session := &sessionState{
		claims:    credential.claims,
		tokenHash: credential.tokenHash,
		runID:     content.RunID,
	}
	server.agentSessions[credential.claims.AgentRecordID] = session
	server.sessionsByToken[credential.tokenHash] = session
	server.emitReservedLocked(OperationLogin, credential.claims, content.RunID)
	return true
}

func (server *Server) handleNewProxy(rawContent json.RawMessage) bool {
	var content newProxyContent
	if json.Unmarshal(rawContent, &content) != nil || hasUnapprovedProxyOptions(content) {
		return false
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	session, ok := server.validSessionUserLocked(content.User)
	if !ok || content.ProxyName != session.claims.ProxyName ||
		content.ProxyType != "tcp" || content.RemotePort != session.claims.RelayPort ||
		content.RemotePort < server.relayPortMin || content.RemotePort > server.relayPortMax {
		return false
	}
	if session.proxyAuthorized {
		return true
	}
	if !server.reserveEventLocked() {
		return false
	}
	server.bindRunIDLocked(session, content.User.RunID)
	session.proxyAuthorized = true
	server.emitReservedLocked(OperationNewProxy, session.claims, session.runID)
	return true
}

func (server *Server) handlePing(rawContent json.RawMessage) bool {
	var content pingContent
	if json.Unmarshal(rawContent, &content) != nil {
		return false
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	session, ok := server.validSessionUserLocked(content.User)
	if !ok || !server.reserveEventLocked() {
		return false
	}
	server.bindRunIDLocked(session, content.User.RunID)
	server.emitReservedLocked(OperationPing, session.claims, session.runID)
	return true
}

func (server *Server) handleCloseProxy(rawContent json.RawMessage) bool {
	var content closeProxyContent
	if json.Unmarshal(rawContent, &content) != nil {
		return false
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	session, ok := server.validSessionUserLocked(content.User)
	if !ok || !session.proxyAuthorized || content.ProxyName != session.claims.ProxyName || !server.reserveEventLocked() {
		return false
	}
	server.bindRunIDLocked(session, content.User.RunID)
	session.closed = true
	server.emitReservedLocked(OperationCloseProxy, session.claims, session.runID)
	return true
}

// handleNewUserConn records the readiness correlation tuple of one frps
// user connection for the presence registry (Task 7 amendment, spec §7.3).
// It is strictly readiness-only: it never rejects, never mutates admission or
// authorization state, and its outcome gates nothing — frps receives the
// accept response unconditionally. Unattributable or malformed callbacks are
// accepted silently with no fact emitted.
func (server *Server) handleNewUserConn(rawContent json.RawMessage) {
	var content newUserConnContent
	if json.Unmarshal(rawContent, &content) != nil {
		return
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	session, ok := server.readinessSessionLocked(content.User)
	if !ok || !boundedUserConnTuple(content) {
		return
	}
	// Deduplicate per user connection: one bounded hash over the session and
	// the correlation tuple, so a repeated callback for the same connection
	// is recorded once.
	tuple := make([]byte, 0, sha256.Size+len(content.ProxyName)+len(content.User.RunID)+len(content.RemoteAddr)+8)
	tuple = append(tuple, session.tokenHash[:]...)
	tuple = append(tuple, '|')
	tuple = append(tuple, content.ProxyName...)
	tuple = append(tuple, '|')
	tuple = append(tuple, content.User.RunID...)
	tuple = append(tuple, '|')
	tuple = append(tuple, content.RemoteAddr...)
	sum := sha256.Sum256(tuple)
	if !server.reserveEventLocked() {
		return // queue full: the fact is dropped, the connection still accepted
	}
	// The dedup set holds delivered facts only: a fact dropped under
	// backpressure has not been seen by the registry, so a repeated callback
	// for the same connection can still be recorded once the queue drains.
	if _, duplicate := server.userConnSeen[sum]; duplicate {
		server.releaseEventSlotLocked()
		return
	}
	if len(server.userConnSeen) >= maxUserConnDedupEntries {
		server.userConnSeen = make(map[[sha256.Size]byte]struct{})
	}
	server.userConnSeen[sum] = struct{}{}
	server.eventQueue <- PresenceFact{
		Operation:     OperationNewUserConn,
		AgentRecordID: session.claims.AgentRecordID,
		Namespace:     session.claims.Namespace,
		ProxyName:     content.ProxyName,
		RelayPort:     session.claims.RelayPort,
		Generation:    session.claims.Generation,
		RunID:         content.User.RunID,
		RemoteAddr:    content.RemoteAddr,
	}
}

// readinessSessionLocked resolves the login session a user connection
// belongs to, deliberately WITHOUT the authorization-op checks: no closed-
// state or run-id pinning, so a stale listener's late callback is still
// recordable and the presence registry — not the plugin — rules on
// correlation. NewUserConn facts never authorize anything, so the looser
// lookup cannot widen authority.
func (server *Server) readinessSessionLocked(user userInfo) (*sessionState, bool) {
	if !validRunID(user.RunID) || !validLoginMetadata(user.Metas) || user.User != "" {
		return nil, false
	}
	providedHash := sha256.Sum256([]byte(user.Metas[CredentialMetadataKey]))
	session, ok := server.sessionsByToken[providedHash]
	if !ok {
		return nil, false
	}
	return session, true
}

func boundedUserConnTuple(content newUserConnContent) bool {
	return content.ProxyName != "" && len(content.ProxyName) <= maxIdentifierBytes &&
		len(content.RemoteAddr) > 0 && len(content.RemoteAddr) <= maxRemoteAddrBytes
}

func (server *Server) validSessionUserLocked(user userInfo) (*sessionState, bool) {
	if !validRunID(user.RunID) || !validLoginMetadata(user.Metas) {
		return nil, false
	}
	if user.User != "" {
		return nil, false
	}
	providedHash := sha256.Sum256([]byte(user.Metas[CredentialMetadataKey]))
	session, ok := server.sessionsByToken[providedHash]
	if !ok || session.closed ||
		user.Metas[GenerationMetadataKey] != strconv.Itoa(session.claims.Generation) ||
		server.agentSessions[session.claims.AgentRecordID] != session ||
		subtle.ConstantTimeCompare(providedHash[:], session.tokenHash[:]) != 1 {
		return nil, false
	}
	if session.runID != "" && user.RunID != session.runID {
		return nil, false
	}
	return session, true
}

func (server *Server) pruneExpiredReplayLocked(now time.Time) {
	for jti, expiry := range server.replayedJTI {
		if !expiry.After(now) {
			delete(server.replayedJTI, jti)
		}
	}
	// High-water marks fence only inside the ten-minute credential horizon:
	// past it, no credential with that issued_at can still be admitted or
	// replayed (admission rejects expired credentials outright), and a live
	// session's fence keeps covering the agent through agentSessions.
	for agent, mark := range server.admissionHighWater {
		if !mark.issuedAt.Add(credentialLifetime).After(now) {
			delete(server.admissionHighWater, agent)
		}
	}
}

// loadAdmissionState restores the persisted replay-JTI set and generation
// high-water at startup. Fail closed: an unreadable, corrupt,
// unknown-schema, or over-cap existing file is a startup error — the gateway
// must never admit with wiped replay history (SOL mid-project review
// Important-6). A missing file is a fresh first boot. Entries outside the
// ten-minute credential horizon are pruned on load.
func (server *Server) loadAdmissionState() error {
	if server.statePath == "" {
		return nil
	}
	data, err := os.ReadFile(server.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil // fresh first boot: nothing to restore
	}
	if err != nil {
		return fmt.Errorf("frpplugin: admission state %q unreadable: %w", server.statePath, err)
	}
	if len(data) > maxAdmissionStateBytes {
		return fmt.Errorf("frpplugin: admission state %q exceeds %d bytes", server.statePath, maxAdmissionStateBytes)
	}
	var state persistedAdmissionState
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return fmt.Errorf("frpplugin: admission state %q corrupt: %w", server.statePath, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("frpplugin: admission state %q corrupt: trailing data", server.statePath)
	}
	if state.Version != admissionStateFileVersion {
		return fmt.Errorf("frpplugin: admission state %q has unsupported schema version %d", server.statePath, state.Version)
	}
	now := server.now().UTC()
	jtis := make(map[string]time.Time, len(state.JTIs))
	for _, entry := range state.JTIs {
		if entry.JTI == "" || len(entry.JTI) > maxIdentifierBytes || entry.ExpiresAt.IsZero() {
			return fmt.Errorf("frpplugin: admission state %q contains an invalid replay entry", server.statePath)
		}
		if !entry.ExpiresAt.After(now) {
			continue // outside the ten-minute admission horizon
		}
		jtis[entry.JTI] = entry.ExpiresAt.UTC()
	}
	if len(jtis) > server.maxReplayEntries {
		return fmt.Errorf("frpplugin: admission state %q holds %d replay entries, cap %d", server.statePath, len(jtis), server.maxReplayEntries)
	}
	marks := make(map[string]admissionMark, len(state.GenerationHighWater))
	for _, entry := range state.GenerationHighWater {
		if entry.AgentRecordID == "" || len(entry.AgentRecordID) > maxIdentifierBytes ||
			entry.Generation < 0 || entry.IssuedAt.IsZero() {
			return fmt.Errorf("frpplugin: admission state %q contains an invalid high-water entry", server.statePath)
		}
		if !entry.IssuedAt.Add(credentialLifetime).After(now) {
			continue // outside the ten-minute admission horizon
		}
		marks[entry.AgentRecordID] = admissionMark{generation: entry.Generation, issuedAt: entry.IssuedAt.UTC()}
	}
	if len(marks) > server.maxSessions {
		return fmt.Errorf("frpplugin: admission state %q holds %d high-water entries, cap %d", server.statePath, len(marks), server.maxSessions)
	}
	server.replayedJTI = jtis
	server.admissionHighWater = marks
	// Repair permission drift on the loaded file: it holds replay
	// identifiers and must stay owner-only. A failed repair fails startup —
	// the 0600 guarantee must never be silently absent — and the repair is
	// verified afterwards so a chmod that reports success without taking
	// effect cannot pass either.
	if info, statErr := os.Stat(server.statePath); statErr == nil && info.Mode().Perm() != 0o600 {
		if err := server.chmod(server.statePath, 0o600); err != nil {
			return fmt.Errorf("frpplugin: admission state %q permission repair to 0600 failed: %w", server.statePath, err)
		}
		info, err := os.Stat(server.statePath)
		if err != nil {
			return fmt.Errorf("frpplugin: admission state %q post-repair stat failed: %w", server.statePath, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			return fmt.Errorf("frpplugin: admission state %q perms %o after repair, want 600", server.statePath, got)
		}
	}
	return nil
}

// writeAdmissionStateLocked atomically persists the post-admission replay
// state: temp file + rename in the state file's directory, mode 0600, entry
// sets TTL-pruned to the ten-minute credential horizon and hard-capped.
// Caller holds server.mu and passes the credential being admitted; the
// in-memory state is mutated only after the write succeeds, so a persistence
// failure rejects the login instead of admitting history the next boot would
// forget. The file carries identifiers and timestamps only — never token
// material.
func (server *Server) writeAdmissionStateLocked(claims CredentialClaims, now time.Time) error {
	jtis := make([]persistedJTI, 0, len(server.replayedJTI)+1)
	for jti, expiry := range server.replayedJTI {
		if expiry.After(now) {
			jtis = append(jtis, persistedJTI{JTI: jti, ExpiresAt: expiry})
		}
	}
	jtis = append(jtis, persistedJTI{JTI: claims.JTI, ExpiresAt: claims.ExpiresAt})
	highWater := make([]persistedAdmissionMark, 0, len(server.admissionHighWater)+1)
	for agent, mark := range server.admissionHighWater {
		if agent == claims.AgentRecordID {
			continue // the incoming credential's mark is appended below
		}
		if mark.issuedAt.Add(credentialLifetime).After(now) {
			highWater = append(highWater, persistedAdmissionMark{AgentRecordID: agent, Generation: mark.generation, IssuedAt: mark.issuedAt})
		}
	}
	highWater = append(highWater, persistedAdmissionMark{
		AgentRecordID: claims.AgentRecordID,
		Generation:    claims.Generation,
		IssuedAt:      claims.IssuedAt,
	})
	if len(jtis) > server.maxReplayEntries || len(highWater) > server.maxSessions {
		return errors.New("frpplugin: admission state entry cap exceeded")
	}
	sort.Slice(jtis, func(i, j int) bool { return jtis[i].JTI < jtis[j].JTI })
	sort.Slice(highWater, func(i, j int) bool { return highWater[i].AgentRecordID < highWater[j].AgentRecordID })
	data, err := json.Marshal(persistedAdmissionState{
		Version:             admissionStateFileVersion,
		JTIs:                jtis,
		GenerationHighWater: highWater,
	})
	if err != nil {
		return err
	}
	tempPath := server.statePath + ".tmp"
	file, err := os.OpenFile(tempPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(tempPath)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(tempPath)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	if err := os.Rename(tempPath, server.statePath); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	return nil
}

func (server *Server) bindRunIDLocked(session *sessionState, runID string) {
	if session.runID == "" {
		session.runID = runID
	}
}

// reserveEventLocked fails closed instead of waiting when the bounded ordered
// dispatcher already has its configured number of queued or in-flight facts.
func (server *Server) reserveEventLocked() bool {
	select {
	case server.eventSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

// releaseEventSlotLocked returns a reserved slot when a fact is not emitted
// after all (a deduplicated NewUserConn callback). Caller holds server.mu.
func (server *Server) releaseEventSlotLocked() {
	<-server.eventSlots
}

func (server *Server) emitReservedLocked(operation string, claims CredentialClaims, runID string) {
	server.eventQueue <- PresenceFact{
		Operation:     operation,
		AgentRecordID: claims.AgentRecordID,
		Namespace:     claims.Namespace,
		ProxyName:     claims.ProxyName,
		RelayPort:     claims.RelayPort,
		Generation:    claims.Generation,
		RunID:         runID,
	}
}

func (server *Server) dispatchPresenceEvents() {
	for fact := range server.eventQueue {
		server.presenceEvents.ObserveFRPEvent(fact)
		<-server.eventSlots
	}
}

func validRunID(runID string) bool {
	return runID != "" && len(runID) <= maxRunIDBytes
}

func validOptionalRunID(runID string) bool {
	return runID == "" || validRunID(runID)
}

func validLoginMetadata(metadata map[string]string) bool {
	return len(metadata) == 2 && metadata[CredentialMetadataKey] != "" && metadata[GenerationMetadataKey] != ""
}

func hasUnapprovedProxyOptions(content newProxyContent) bool {
	return content.UseEncryption || content.UseCompression || content.BandwidthLimit != "" ||
		content.BandwidthLimitMode != "" || content.Group != "" || content.GroupKey != "" ||
		len(content.Metas) != 0 || len(content.Annotations) != 0 || len(content.CustomDomains) != 0 ||
		content.Subdomain != "" || len(content.Locations) != 0 || content.HTTPUser != "" ||
		content.HTTPPassword != "" || content.HostHeaderRewrite != "" || len(content.Headers) != 0 ||
		len(content.ResponseHeaders) != 0 || content.RouteByHTTPUser != "" || content.SecretKey != "" ||
		len(content.AllowUsers) != 0 || content.Multiplexer != ""
}

func (server *Server) writeRejected(responseWriter http.ResponseWriter) {
	server.writeJSON(responseWriter, pluginResponse{
		Reject:       true,
		RejectReason: "request rejected",
		Unchange:     false,
	})
}

func (server *Server) writeJSON(responseWriter http.ResponseWriter, response pluginResponse) {
	responseWriter.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(responseWriter).Encode(response)
}
