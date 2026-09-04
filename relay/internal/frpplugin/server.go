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
	"io"
	"mime"
	"net"
	"net/http"
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
)

// PresenceFact is the credential-free authorization fact stream consumed by
// Task 14's presence registry. Login and NewProxy facts confirm only that the
// FRP plugin authorized those calls; FRP v0.71.0 provides no callback that can
// confirm downstream proxy registration. Each fact contains only bounded
// routing identity and lifecycle data validated at the FRP boundary.
type PresenceFact struct {
	Operation     string `json:"operation"`
	AgentRecordID string `json:"agent_record_id"`
	Namespace     string `json:"namespace"`
	ProxyName     string `json:"proxy_name"`
	RelayPort     int    `json:"relay_port"`
	Generation    int    `json:"generation"`
	RunID         string `json:"run_id"`
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

	mu              sync.Mutex
	replayedJTI     map[string]time.Time
	agentSessions   map[string]*sessionState
	sessionsByToken map[[sha256.Size]byte]*sessionState
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
		agentSessions:      make(map[string]*sessionState),
		sessionsByToken:    make(map[[sha256.Size]byte]*sessionState),
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
	if json.Unmarshal(rawContent, &content) != nil || !validOptionalRunID(content.RunID) || content.PoolCount != 0 ||
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
		return false
	}
	current, exists := server.agentSessions[credential.claims.AgentRecordID]
	if exists && (current.claims.Generation > credential.claims.Generation ||
		(current.claims.Generation == credential.claims.Generation &&
			!credential.claims.IssuedAt.After(current.claims.IssuedAt))) {
		return false
	}
	if !exists && len(server.agentSessions) >= server.maxSessions {
		return false
	}
	if len(server.replayedJTI) >= server.maxReplayEntries || !server.reserveEventLocked() {
		return false
	}

	server.replayedJTI[credential.claims.JTI] = credential.claims.ExpiresAt
	if exists {
		delete(server.sessionsByToken, current.tokenHash)
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
