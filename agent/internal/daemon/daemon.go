package daemon

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"sharebridge/agent/internal/cert"
	"sharebridge/agent/internal/config"
	"sharebridge/agent/internal/direct"
	"sharebridge/agent/internal/immich"
	"sharebridge/agent/internal/signaling"
	"sharebridge/agent/internal/store"
	"sharebridge/agent/internal/stun"
	"sharebridge/agent/internal/tunnel"
)

type immichPoller interface {
	PollShares(ctx context.Context) ([]immich.SharedLink, error)
}

type shareOptionRegistrar interface {
	RegisterShareWithOptions(ctx context.Context, opts signaling.RegisterShareOptions) (string, string, bool, error)
}

type shareUnregistrar interface {
	UnregisterShare(ctx context.Context, code string) error
}

type validationError struct {
	message string
}

func (e validationError) Error() string {
	return e.message
}

func (e validationError) IsValidationError() bool {
	return true
}

// WebServer is the interface for the admin UI web server.
// This interface avoids a circular import between daemon and web packages.
type WebServer interface {
	Start(ctx context.Context) error
	Stop() error
	SetDaemon(d *Daemon)
}

// ConfigManagerInterface defines the interface for config management.
type ConfigManagerInterface interface {
	Get() *config.Config
}

// StoreInterface defines the interface for session storage.
type StoreInterface interface {
	GetAgentID() string
	GetSession(code string) *store.SessionEntry
	GetByShareURL(shareURL string) *store.SessionEntry
	ListSessions(filterExpired bool) []store.SessionEntry
	SaveSession(session store.SessionEntry) error
	DeleteSession(code string) error
	IncrementDownloads(code string) (int, error)
}

// SignalingClientInterface defines the interface for signaling client.
type SignalingClientInterface interface {
	Connect(ctx context.Context) error
	DownloadComplete(ctx context.Context, code string, bytesTransferred int64) error
	Send(ctx context.Context, msg any) error
	Listen(ctx context.Context) error
	SetOnMessage(handler func(signaling.Message))
	SubmitCSR(ctx context.Context, csrPEM string) error
	OpenAck(ctx context.Context, ack signaling.OpenAck) error
	ReportEndpoint(ctx context.Context, ip string, port int, status string) error
	TLSReady(ctx context.Context, fingerprint, notAfter string) error
	TLSError(ctx context.Context, reason string) error
}

// Session represents an active share session.
type Session struct {
	Code         string
	ShareURL     string
	ShareType    string
	FileID       string // oc:fileid extracted via WebDAV PROPFIND on share root
	Password     string
	ExpiresAt    time.Time
	MaxDownloads int
	Downloads    int
	RelayOnly    bool
	CreatedAt    time.Time

	IsPasswordProtected bool
	immich              *immich.Client // concrete client for the content layer
	closing             bool
	mu                  sync.Mutex
}

// Daemon manages multiple concurrent sessions, a single signaling connection,
// and the web server.
type Daemon struct {
	config    *config.Config
	configMgr ConfigManagerInterface
	store     StoreInterface
	signaling SignalingClientInterface
	direct    *directState
	resolver  *direct.ResolverRegistry // share code -> SnapshotManager
	sessions  map[string]*Session      // code -> Session
	mu        sync.RWMutex

	// tunnel supervises the pinned frpc child (§7.4). Process-lifetime by
	// design: it is constructed in Start with the process context and — once
	// a healthy tunnel exists — survives every control-WebSocket reconnect
	// (§15.4), until replacement by a higher generation, lockdown, or daemon
	// shutdown. Written only before the signaling goroutines start and by the
	// tests' startTunnelManager; readers need no additional synchronization.
	tunnel *tunnel.Manager

	// STUN cross-check state (§10.1, Task 17 add-on): the exchange client, a
	// test clock seam, and the one-slot bound that keeps at most a single
	// challenge exchange in flight at a time.
	stunClient   *stun.Client
	stunInFlight chan struct{}
	now          func() time.Time

	webServer          WebServer
	startTime          time.Time
	signalingConnected bool // true once welcome received

	newImmichPoller func() (immichPoller, error)

	// Callbacks for external handling (e.g., web server refresh)
	OnSessionAdded   func(session *Session)
	OnSessionRemoved func(code string)
}

const (
	defaultBaseDomain     = "sharebridgeusercontent.com"
	directMaxContentBytes = int64(2 << 30) // placeholder download ceiling (2 GiB)
	directIntPort         = 8443
	directExtPort         = 443
	directPortIdle        = 5 * time.Minute
)

// originPair is one content session's §6 origin pair: the control-allocated
// direct origin and its deterministic relay origin. Both bind the SAME share
// code and are installed, re-admitted after rebuild, and revoked together —
// never as two independent routes.
type originPair struct {
	directOrigin string
	relayOrigin  string
}

// directState is the daemon's direct-TCP transport state. It is nil when direct
// transport is not configured (e.g. in tests using NewWithSignaling).
type directState struct {
	mu        sync.Mutex
	namespace string
	// ready is baseline enrollment readiness: enrollment_ready received for
	// the current epoch (§7.1 — TLS + relay DNS, never direct DDNS).
	ready      bool
	cert       *cert.Manager
	binder     *direct.Binder
	gate       *direct.SignalGate
	port       *direct.OnDemandPort
	mapper     direct.PortMapper
	server     *direct.DirectServer
	reporter   *direct.Reporter
	baseDomain string
	serveNS    string                // namespace the binder/server were built for
	origins    map[string]originPair // share code → §6 origin pair (direct + relay)
	// relayOrigins tracks relay-only sessions' single RouteRelay binding
	// (share code → relay origin). §13.3: a relay-only share gets zero
	// direct-path setup — its direct origin is never bound and no originPair
	// is created — so its lone binding is bookkept separately for rebuild
	// and revocation.
	relayOrigins map[string]string

	started    bool               // DirectServer.Start guard (under mu)
	startGen   uint64             // increments per start attempt (under mu)
	cancel     context.CancelFunc // cancels the direct server ctx on shutdown
	listenAddr string             // override for tests; empty => :directIntPort

	// pendingRelayBinds records relay-only shares whose single §13.3 binding
	// could not be installed at hydration time because the binder (namespace)
	// was not built yet: share code → the control-allocated direct origin (the
	// §6 derivation input). syncDirectServe replays them deterministically as
	// soon as the binder exists, instead of silently waiting for a future
	// control re-allocation (Task 27 carry-forward).
	pendingRelayBinds map[string]string

	cond *sync.Cond // readiness signal (lazily created; guarded by mu)
}

// relayStateSender is the signaling capability that forwards §11.1
// relay_client_state telemetry to control. It is asserted, not required: the
// production signaling client implements it; minimal signaling fakes without
// it simply drop the telemetry instead of failing the tunnel lifecycle.
type relayStateSender interface {
	SendRelayClientState(ctx context.Context, state signaling.RelayClientState) error
}

// TunnelStatusCallback returns the onStatus callback the Task 9 tunnel
// Manager is constructed with (agent/internal/tunnel.NewManager): it is the
// ONLY wiring between the FRP tunnel lifecycle and the control connection.
// The callback forwards every transition exclusively as a §11.1
// relay_client_state telemetry message (§7.4) — it never sends
// report_endpoint, never touches the direct transport state (reporter,
// mapper, port), and never creates direct or relay availability: control
// treats relay_client_state as telemetry, with relay availability coming
// solely from the gateway presence lease (§4.2, §12). Reasons are produced by
// the tunnel manager itself and never contain credential material (§16.6).
func (d *Daemon) TunnelStatusCallback() func(tunnel.StatusReport) {
	return d.handleTunnelStatus
}

// handleTunnelStatus maps one tunnel manager diagnostic to the §11.1
// relay_client_state wire message. The status vocabulary is identical by
// construction (tunnel.StatusKind and signaling.RelayClientStatus pin the
// same four values). Sending happens on a background context with a bounded
// timeout; a failure is logged and never retried here (the next lifecycle
// transition re-reports).
func (d *Daemon) handleTunnelStatus(report tunnel.StatusReport) {
	sender, ok := d.signaling.(relayStateSender)
	if !ok {
		return // telemetry sink unavailable: drop, never fall back to direct state
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	state := signaling.RelayClientState{
		Generation: report.Generation,
		Status:     signaling.RelayClientStatus(report.Status),
		Reason:     report.Reason,
	}
	if err := sender.SendRelayClientState(ctx, state); err != nil {
		log.Printf("send relay_client_state: %v", err)
	}
}

// relayCredentialRequestSender is the signaling capability the production
// credential requester is built on: the §11.1 relay_credential_request send.
// Capability-asserted (not required) so minimal signaling fakes without it
// simply leave the tunnel without a requester (the manager stays down and
// says so) instead of failing construction.
type relayCredentialRequestSender interface {
	SendRelayCredentialRequest(ctx context.Context, reason tunnel.CredentialRequestReason) error
}

// stunResultSender is the signaling capability the §10.1 challenge answer
// rides: the §11.1 stun_result echo over the authenticated WebSocket.
type stunResultSender interface {
	SendSTUNResult(ctx context.Context, result signaling.STUNResult) error
}

// startTunnelManager constructs the relay tunnel supervision (§7.4, Task 9)
// wired to the control connection. The manager and its credential requester
// live on the given process context — NOT on a WebSocket epoch: §15.4 keeps a
// healthy tunnel across reconnects, and §15.2's fresh-credential request must
// ride whichever connection (current or reconnected) is up when it fires.
// options forwards the tunnel package's test-visible knobs; production calls
// it without options.
func (d *Daemon) startTunnelManager(ctx context.Context, options ...tunnel.ManagerOption) {
	d.mu.Lock()
	if d.tunnel != nil {
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()

	cfg := d.GetConfig()
	if cfg == nil {
		log.Printf("relay tunnel supervision disabled: no configuration available")
		return
	}
	settings := tunnel.Settings{
		FRPCBinaryPath: cfg.TunnelFRPCPath,
		ConfigPath:     filepath.Join(cfg.TunnelDataDir, "frpc.toml"),
		TrustedCAFile:  cfg.TunnelCAFile,
		LocalTarget:    tunnel.LocalTarget,
	}
	if settings.FRPCBinaryPath == "" || settings.ConfigPath == "" {
		// Degenerate configuration (unit tests, hand-built configs): relay
		// supervision stays off rather than failing daemon construction.
		log.Printf("relay tunnel supervision disabled: frpc binary or config path not configured")
		return
	}

	// The credential requester (§7.2/§11.1) sends over the signaling client's
	// CURRENT connection: the send path re-resolves the WebSocket per call, so
	// a request enqueued before a reconnect rides the reconnected socket.
	var requester tunnel.CredentialRequester
	if sender, ok := d.signaling.(relayCredentialRequestSender); ok {
		requester = tunnel.NewControlCredentialRequester(ctx, sender.SendRelayCredentialRequest)
	} else {
		log.Printf("relay credential requester unavailable: signaling client lacks relay_credential_request")
	}

	manager, err := tunnel.NewManager(settings, requester, d.handleTunnelStatus, options...)
	if err != nil {
		log.Printf("relay tunnel supervision unavailable: %v", err)
		return
	}
	d.mu.Lock()
	d.tunnel = manager
	d.mu.Unlock()
}

// stopTunnelManager stops the frpc child and the supervision loop (§7.4
// step 6: lockdown and daemon shutdown are the only legitimate stops).
// Idempotent.
func (d *Daemon) stopTunnelManager() {
	d.mu.RLock()
	manager := d.tunnel
	d.mu.RUnlock()
	if manager != nil {
		manager.Stop()
	}
}

// applyRelayConfig routes one control relay_config message into the tunnel
// manager's generation-fenced ApplyConfig (§7.4 step 1 validation happens in
// the strict parser and again inside the manager). The message reaches the
// daemon only on the authenticated current epoch; an absent manager (relay
// supervision never enabled) drops it with a diagnostic.
func (d *Daemon) applyRelayConfig(msg signaling.Message) {
	d.mu.RLock()
	manager := d.tunnel
	d.mu.RUnlock()
	if manager == nil {
		log.Printf("relay_config ignored: tunnel supervision unavailable")
		return
	}
	if len(msg.Raw) == 0 {
		log.Printf("relay_config ignored: raw payload unavailable for strict parse")
		return
	}
	config, err := signaling.ParseRelayConfig(msg.Raw)
	if err != nil {
		log.Printf("invalid relay_config rejected: %v", err)
		return
	}
	if err := manager.ApplyConfig(config); err != nil {
		log.Printf("apply relay_config: %v", err)
	}
}

// stunExchangeSlot lazily creates and returns the single-exchange bound.
// Caller must hold d.mu (write lock not required: the channel is only read
// after creation — use the write lock at call sites for simplicity).
func (d *Daemon) stunExchangeSlotLocked() chan struct{} {
	if d.stunInFlight == nil {
		d.stunInFlight = make(chan struct{}, 1)
	}
	return d.stunInFlight
}

// beginStunExchange reports whether a new §10.1 exchange may start. The bound
// is one in-flight exchange at a time: challenges are one-use and short-lived,
// so a challenge arriving while another exchange runs is dropped (control
// re-issues; the failed observation falls back to relay per §10.3).
func (d *Daemon) beginStunExchange() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	slot := d.stunExchangeSlotLocked()
	select {
	case slot <- struct{}{}:
		return true
	default:
		return false
	}
}

// endStunExchange releases the single-exchange slot.
func (d *Daemon) endStunExchange() {
	d.mu.Lock()
	slot := d.stunExchangeSlotLocked()
	d.mu.Unlock()
	select {
	case <-slot:
	default:
	}
}

// stunNow returns the daemon's clock reading (test seam, defaulting to the
// real clock) so challenge TTL validation is deterministic under test.
func (d *Daemon) stunNow() time.Time {
	d.mu.RLock()
	now := d.now
	d.mu.RUnlock()
	if now == nil {
		return time.Now()
	}
	return now()
}

// handleStunChallenge answers one §11.1 stun_challenge (§10.1): validate the
// one-use credential (version, shape, server, expiry — fail-closed BEFORE any
// network activity), then run exactly one integrity-protected STUN exchange
// via the Task 17 client and echo {challenge, transaction_id, receipt} back
// over the authenticated WebSocket. The secret and the mapped address are
// never echoed or logged (§16.6). The exchange runs off the WebSocket read
// loop so the control connection stays responsive.
func (d *Daemon) handleStunChallenge(msg signaling.Message) {
	sender, ok := d.signaling.(stunResultSender)
	if !ok {
		return // no echo sink: nothing useful can be done with the challenge
	}
	expiresAt, err := time.Parse(time.RFC3339, msg.ExpiresAt)
	if err != nil {
		log.Printf("stun_challenge dropped: invalid expires_at")
		return
	}
	challenge, err := stun.ParseChallenge(msg.Version, msg.Challenge, msg.Server, expiresAt, d.stunNow())
	if err != nil {
		// Validation failures never carry credential material (§16.6).
		log.Printf("stun_challenge dropped: %v", err)
		return
	}
	if !d.beginStunExchange() {
		log.Printf("stun_challenge dropped: another exchange is in flight")
		return
	}
	go d.runStunExchange(sender, challenge, expiresAt)
}

// runStunExchange performs the single UDP exchange and the WebSocket echo.
// The context deadline is the challenge's own TTL: the echo must land inside
// control's claim window, and the client's own response timeout bounds the
// UDP wait (one request per challenge, no retransmission).
func (d *Daemon) runStunExchange(sender stunResultSender, challenge stun.Challenge, expiresAt time.Time) {
	defer d.endStunExchange()
	ctx, cancel := context.WithDeadline(context.Background(), expiresAt)
	defer cancel()

	d.mu.RLock()
	client := d.stunClient
	d.mu.RUnlock()
	if client == nil {
		client = stun.NewClient()
	}
	result, err := client.Exchange(ctx, challenge)
	if err != nil {
		log.Printf("stun exchange failed: %v", err)
		return
	}
	echo := signaling.STUNResult{
		Challenge:     result.ChallengeID,
		TransactionID: result.TransactionID,
		Receipt:       result.Receipt,
	}
	if err := sender.SendSTUNResult(ctx, echo); err != nil {
		log.Printf("send stun_result: %v", err)
	}
}

// canRegisterDirect reports whether shares may be registered: baseline
// enrollment must be complete. Baseline readiness is TLS + relay DNS only
// (§7.1) — a working direct transport is an optional capability, so a no-mapper
// agent registers shares just the same.
func (d *Daemon) canRegisterDirect() bool {
	if d.direct == nil {
		return false
	}
	d.direct.mu.Lock()
	defer d.direct.mu.Unlock()
	return d.direct.ready
}

// condLocked returns the readiness condition variable, creating it on first
// use. Caller must hold ds.mu.
func (ds *directState) condLocked() *sync.Cond {
	if ds.cond == nil {
		ds.cond = sync.NewCond(&ds.mu)
	}
	return ds.cond
}

// waitForDirectReady blocks until the CURRENT connection epoch has reached
// baseline enrollment (enrollment_ready, i.e. TLS + relay DNS; §7.1) or ctx is
// cancelled. It is a no-op when direct transport is not configured. The
// condition is reset on disconnect, so a waiter sleeps through reconnects and
// only proceeds once a live epoch signals readiness. Direct availability is
// NOT part of the waited-on condition.
func (d *Daemon) waitForDirectReady(ctx context.Context) error {
	ds := d.direct
	if ds == nil {
		return nil
	}
	ds.mu.Lock()
	defer ds.mu.Unlock()
	cond := ds.condLocked()
	stop := context.AfterFunc(ctx, func() {
		ds.mu.Lock()
		if ds.cond != nil {
			ds.cond.Broadcast()
		}
		ds.mu.Unlock()
	})
	defer stop()
	for !ds.ready {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		cond.Wait()
	}
	return nil
}

// directShareAuthorized is the SignalGate source-authorization check: a share is
// only openable over direct once its origin pair is locally bound. It stays
// direct-only by name and by contract — relay traffic never opens the direct
// public port (§13.1: SignalGate authorization remains direct-only; relay
// serving is authorized per-request by the Binder).
func (d *Daemon) directShareAuthorized(shareID string, kind direct.RouteKind) bool {
	if kind != direct.RouteDirect {
		return false
	}
	if d.direct == nil {
		return false
	}
	d.direct.mu.Lock()
	defer d.direct.mu.Unlock()
	_, ok := d.direct.origins[shareID]
	return ok
}

// bindOrigin records the control-allocated origin pair for a share code and
// admits BOTH route kinds in the binder as one atomic operation (§6, §13.1).
// The relay origin is derived locally by the same deterministic §6 rule the
// control plane applies — no agent message ever supplies an origin (§11.1).
// It reports the installed pair and whether the binding succeeded; it is a
// no-op (ok=false) until the binder exists (namespace known) or the direct
// origin is empty/undervivable.
func (d *Daemon) bindOrigin(code, directOrigin string) (originPair, bool) {
	ds := d.direct
	if ds == nil || directOrigin == "" {
		return originPair{}, false
	}
	// Snapshot the binder under the state lock: syncDirectServe swaps the
	// binder under ds.mu, so an unlocked read here would race a namespace
	// change/reconnect.
	ds.mu.Lock()
	binder := ds.binder
	ds.mu.Unlock()
	if binder == nil {
		return originPair{}, false
	}
	relayOrigin, err := binder.RelayOriginFor(directOrigin)
	if err != nil {
		log.Printf("derive relay origin for share %s: %v", code, err)
		return originPair{}, false
	}
	if err := binder.AllowShare(directOrigin, relayOrigin, code); err != nil {
		log.Printf("bind origins for share %s: %v", code, err)
		return originPair{}, false
	}
	pair := originPair{directOrigin: directOrigin, relayOrigin: relayOrigin}
	ds.mu.Lock()
	if ds.origins == nil {
		ds.origins = make(map[string]originPair)
	}
	ds.origins[code] = pair
	ds.mu.Unlock()
	return pair, true
}

// bindRelayOnlyOrigin admits ONLY the §6 relay origin of a relay-only share
// (§13.3: zero direct-path setup — the direct origin is never bound, and no
// originPair is created) and records the single binding for binder rebuild
// and revocation. The control-allocated direct origin is only the derivation
// input for the deterministic relay hostname (§6); it is never admitted, so
// SignalGate authorization can never open the share direct either. It
// reports the installed relay origin and whether the binding succeeded; it
// is a no-op (ok=false) until the binder exists or the direct origin is
// empty/undervivable.
func (d *Daemon) bindRelayOnlyOrigin(code, directOrigin string) (string, bool) {
	ds := d.direct
	if ds == nil || directOrigin == "" {
		return "", false
	}
	// Snapshot the binder under the state lock: syncDirectServe swaps the
	// binder under ds.mu, so an unlocked read here would race a namespace
	// change/reconnect.
	ds.mu.Lock()
	binder := ds.binder
	ds.mu.Unlock()
	if binder == nil {
		// The binder (namespace) is not built yet — e.g. hydration racing
		// ahead of `enrolled`. Record the pending binding so syncDirectServe
		// installs it deterministically as soon as the binder exists, instead
		// of silently waiting for a future control re-allocation (Task 27
		// carry-forward).
		ds.mu.Lock()
		if ds.pendingRelayBinds == nil {
			ds.pendingRelayBinds = make(map[string]string)
		}
		ds.pendingRelayBinds[code] = directOrigin
		ds.mu.Unlock()
		return "", false
	}
	relayOrigin, err := binder.RelayOriginFor(directOrigin)
	if err != nil {
		log.Printf("derive relay origin for share %s: %v", code, err)
		return "", false
	}
	if err := binder.Allow(relayOrigin, direct.RouteRelay, code); err != nil {
		log.Printf("bind relay origin for share %s: %v", code, err)
		return "", false
	}
	ds.mu.Lock()
	if ds.relayOrigins == nil {
		ds.relayOrigins = make(map[string]string)
	}
	ds.relayOrigins[code] = relayOrigin
	delete(ds.pendingRelayBinds, code)
	ds.mu.Unlock()
	return relayOrigin, true
}

// revokeOrigin drops the origin pair for a share code and revokes BOTH binder
// bindings as one logical operation (§6: revocation removes both bindings).
// For a relay-only share it drops the single relay-origin binding recorded by
// bindRelayOnlyOrigin instead. It is a no-op if no binding was recorded.
func (d *Daemon) revokeOrigin(code string) {
	ds := d.direct
	if ds == nil {
		return
	}
	ds.mu.Lock()
	pair, ok := ds.origins[code]
	if ok {
		delete(ds.origins, code)
	}
	relayOrigin, relayOnlyBound := ds.relayOrigins[code]
	if relayOnlyBound {
		delete(ds.relayOrigins, code)
	}
	binder := ds.binder
	ds.mu.Unlock()
	if binder == nil {
		return
	}
	if ok {
		binder.RevokeShare(pair.directOrigin, pair.relayOrigin)
	}
	if relayOnlyBound {
		binder.Revoke(relayOrigin)
	}
}

// syncDirectServe (re)builds the binder and direct server once the namespace is
// known. It is idempotent for an unchanged namespace so reconnect does not drop
// live origin bindings.
func (d *Daemon) syncDirectServe() {
	ds := d.direct
	if ds == nil {
		return
	}
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.namespace == "" {
		return
	}
	if ds.binder != nil && ds.serveNS == ds.namespace {
		return
	}
	// The namespace changed: the currently-serving listener (if any) is bound
	// to the previous namespace's server object, so it must leave service;
	// the next enrollment_ready rebinds the rebuilt server (§13.1 re-allow
	// after namespace/listener rebuild). A reconnect with an UNCHANGED
	// namespace never reaches this branch — the listener keeps serving.
	if ds.started && ds.cancel != nil {
		cancel := ds.cancel
		ds.cancel = nil
		ds.started = false
		ds.startGen++
		cancel()
	}
	ds.binder = direct.NewBinder(ds.namespace, ds.baseDomain)
	// Share ONE binder between the daemon (which records control-allocated
	// origin pairs via bindOrigin) and the DirectServer (which consults it for
	// SNI admission + per-request authorization). A private server binder would
	// reject every direct handshake as an unknown origin.
	ds.server = direct.NewDirectServerWithBinder(ds.namespace, ds.baseDomain, ds.port, ds.cert, ds.gate, directMaxContentBytes, ds.binder)
	// The §9.3 connect check must honor the resolved config.json value, not
	// only the environment variable (construction-time resolution is env-only).
	// An empty value (hand-built test configs) keeps the constructor default.
	if cfg := d.config; cfg != nil && cfg.ConnectAllowedOrigin != "" {
		ds.server.SetConnectAllowedOrigin(cfg.ConnectAllowedOrigin)
	}
	if d.resolver != nil {
		ds.server.SetResolver(d.resolver)
	}
	// Re-Allow currently-bound origin pairs into the fresh binder (atomic
	// under ds.mu): BOTH route kinds are re-admitted for every bound session
	// (§13.1). Relay-only sessions re-admit their single RouteRelay binding
	// (§13.3). Origins from a previous namespace are rejected by
	// AllowShare/Allow and are re-bound once the control re-allocates them
	// for the new namespace.
	for code, pair := range ds.origins {
		if err := ds.binder.AllowShare(pair.directOrigin, pair.relayOrigin, code); err != nil {
			log.Printf("re-allow origins for share %s after binder rebuild: %v", code, err)
		}
	}
	for code, relayOrigin := range ds.relayOrigins {
		if err := ds.binder.Allow(relayOrigin, direct.RouteRelay, code); err != nil {
			log.Printf("re-allow relay origin for share %s after binder rebuild: %v", code, err)
		}
	}
	// Relay-only bindings whose binder was not built at hydration time are
	// installed here deterministically (Task 27 carry-forward): as soon as the
	// binder exists, the pending §13.3 binding is derived and admitted — no
	// waiting for a future control re-allocation.
	for code, directOrigin := range ds.pendingRelayBinds {
		relayOrigin, err := ds.binder.RelayOriginFor(directOrigin)
		if err != nil {
			log.Printf("derive pending relay origin for share %s: %v", code, err)
			continue
		}
		if err := ds.binder.Allow(relayOrigin, direct.RouteRelay, code); err != nil {
			// Kept pending: a stale-namespace origin stays recorded until the
			// control re-allocates it for the current namespace (bounded by the
			// session count) — but a namespace-valid origin binds right here.
			log.Printf("re-allow pending relay origin for share %s after binder rebuild: %v", code, err)
			continue
		}
		if ds.relayOrigins == nil {
			ds.relayOrigins = make(map[string]string)
		}
		ds.relayOrigins[code] = relayOrigin
		delete(ds.pendingRelayBinds, code)
		log.Printf("re-bound pending relay-only origin for share %s after binder build", code)
	}
	ds.serveNS = ds.namespace
}

// buildDirectState constructs the daemon's direct-transport state. When withNetwork
// is false (tests) the port mapper and on-demand port are left nil.
func (d *Daemon) buildDirectState(withNetwork bool) {
	cfg := d.GetConfig()
	agentID := d.store.GetAgentID()

	baseDomain := cfg.BaseDomain
	if baseDomain == "" {
		baseDomain = defaultBaseDomain
	}

	ds := &directState{
		baseDomain:   baseDomain,
		origins:      make(map[string]originPair),
		relayOrigins: make(map[string]string),
	}
	d.direct = ds

	ds.cert = cert.NewManager(resolveDataDir(), baseDomain, systemRoots())
	if err := ds.cert.Load(); err != nil {
		log.Printf("load direct cert state: %v", err)
	}
	ds.namespace = ds.cert.Namespace()

	ds.gate = direct.NewSignalGate(agentID, d.directShareAuthorized)

	if withNetwork {
		mapperCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		mapper, err := direct.MapperForRouter(mapperCtx)
		cancel()
		if err != nil {
			log.Printf("direct port mapper unavailable: %v", err)
		} else {
			ds.mapper = mapper
			token := "sharebridge-" + shortHash(agentID)
			ds.port = direct.NewOnDemandPortOwned(mapper, directExtPort, directIntPort, directPortIdle, token, mapper.InternalIP())
			ds.reporter = direct.NewReporter(func(ip string, port int, status string) {
				_ = d.signaling.ReportEndpoint(context.Background(), ip, port, status)
			})
			ds.port.SetTransitionCallback(ds.reporter.OnTransition)
		}
	}

	d.syncDirectServe()
}

// shortHash returns a short stable hex digest of s, used to derive a per-agent
// port-mapping ownership token.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// resolveDataDir resolves the agent data directory, matching store.New.
func resolveDataDir() string {
	if env := os.Getenv("SHAREBRIDGE_DATA_DIR"); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".sharebridge"
	}
	return filepath.Join(home, ".sharebridge")
}

// systemRoots returns the system trust root pool the agent validates the issued
// chain against (Let's Encrypt roots live in the system trust store).
func systemRoots() *x509.CertPool {
	if sys, err := x509.SystemCertPool(); err == nil {
		return sys
	}
	return x509.NewCertPool()
}

// New creates a new Daemon with the given config manager and store.
func New(cfgMgr ConfigManagerInterface, st StoreInterface) (*Daemon, error) {
	cfg := cfgMgr.Get()
	agentID := st.GetAgentID()

	sig := signaling.New(cfg.SignalingURL, cfg.APIKey, agentID)

	d := &Daemon{
		config:    cfg,
		configMgr: cfgMgr,
		store:     st,
		signaling: sig,
		resolver:  direct.NewResolverRegistry(),
		sessions:  make(map[string]*Session),
		startTime: time.Now(),
	}
	d.buildDirectState(true)
	return d, nil
}

// NewWithSignaling creates a new Daemon with a custom signaling client.
// This is useful for testing or custom signaling implementations.
func NewWithSignaling(cfgMgr ConfigManagerInterface, st StoreInterface, sig SignalingClientInterface) (*Daemon, error) {
	cfg := cfgMgr.Get()

	return &Daemon{
		config:    cfg,
		configMgr: cfgMgr,
		store:     st,
		signaling: sig,
		resolver:  direct.NewResolverRegistry(),
		sessions:  make(map[string]*Session),
	}, nil
}

// SetWebServer sets the web server instance. Called after web server creation
// to avoid circular import issues.
func (d *Daemon) SetWebServer(ws WebServer) {
	d.webServer = ws
}

// Start connects to signaling server (with reconnect), loads sessions from
// store, starts the web server, and begins the expiry pruner + cert renewal.
// Returns an error channel that emits errors from background goroutines.
func (d *Daemon) Start(ctx context.Context) <-chan error {
	errChan := make(chan error, 10)

	// Relay tunnel supervision is process-lifetime (§7.4): the manager is
	// constructed once here — on the SAME signal.NotifyContext process context
	// the daemon runs on — and survives every control-WebSocket reconnect
	// (§15.4) until replacement, lockdown, or shutdown.
	d.startTunnelManager(ctx)

	// Set up message handler before the reconnect loop starts the listener.
	d.signaling.SetOnMessage(d.handleSignalingMessage)

	// Signaling connect + listen run in a reconnect loop (bounded backoff).
	go d.runSignalingLoop(ctx, errChan)

	// Renewal scheduler re-issues the cert at the 30-day threshold.
	go d.runRenewalScheduler(ctx)

	if d.config.ImmichURL != "" && d.config.ImmichAPIKey != "" {
		go d.runImmichPoller(ctx)
	}

	// Start expiry pruner
	go d.runExpiryPruner(ctx)

	// Start web server if configured
	if d.webServer != nil {
		go func() {
			if err := d.webServer.Start(ctx); err != nil {
				if !errors.Is(err, context.Canceled) {
					errChan <- fmt.Errorf("web server: %w", err)
				}
			}
		}()
	}

	return errChan
}

// runSignalingLoop drives connect + listen with bounded exponential backoff.
// Each successful connect re-registers persisted sessions; each disconnect
// resets direct-transport readiness and the open-signal gate for the new epoch.
func (d *Daemon) runSignalingLoop(ctx context.Context, errChan chan<- error) {
	backoff := signaling.NewBackoff()
	for {
		if ctx.Err() != nil {
			return
		}
		if err := d.signaling.Connect(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			errChan <- fmt.Errorf("connect to signaling server: %w", err)
			if !sleepCtx(ctx, backoff.Next()) {
				return
			}
			continue
		}
		backoff.Reset()
		log.Printf("connected to signaling server at %s", d.GetConfig().SignalingURL)

		// Listen is the sole WebSocket reader and must run before re-enrollment
		// / re-registration (RegisterShare waits on a channel Listen feeds).
		listenDone := make(chan error, 1)
		go func() { listenDone <- d.signaling.Listen(ctx) }()

		// Re-enrollment (hello → enrolled → cert → enrollment_ready) is driven
		// by the message handler asynchronously; here we re-register sessions.
		d.loadSessionsFromStore(ctx)

		select {
		case <-ctx.Done():
			return
		case err := <-listenDone:
			if ctx.Err() != nil {
				return
			}
			d.onSignalingDisconnect()
			if !errors.Is(err, context.Canceled) {
				errChan <- fmt.Errorf("signaling listener: %w", err)
			}
			if !sleepCtx(ctx, backoff.Next()) {
				return
			}
		}
	}
}

// onSignalingDisconnect resets the direct-transport epoch: readiness is
// connection-local, and the gate forgets nonces/sequence numbers so signals
// from the previous connection cannot replay. It deliberately does NOT touch
// the local HTTPS listener (§7.4: the serving listener left the WS-epoch
// lifecycle once a valid certificate exists — an established FRP tunnel
// serves through it during the reconnect) and does NOT touch the tunnel
// manager (§15.4: a healthy tunnel is preserved until replacement succeeds).
func (d *Daemon) onSignalingDisconnect() {
	if d.direct == nil {
		return
	}
	ds := d.direct
	ds.mu.Lock()
	ds.ready = false
	ds.mu.Unlock()
	if ds.gate != nil {
		ds.gate.Reset()
	}
}

// stopDirectServer tears the local HTTPS listener down (daemon shutdown —
// the only legitimate stop short of lockdown, §7.4). The generation bump
// ensures a stale bind-failure from the old attempt cannot re-latch state.
func (d *Daemon) stopDirectServer() {
	if d.direct == nil {
		return
	}
	ds := d.direct
	ds.mu.Lock()
	cancel := ds.cancel
	ds.cancel = nil
	started := ds.started
	ds.started = false
	ds.startGen++
	ds.mu.Unlock()
	if started && cancel != nil {
		cancel()
	}
}

// runRenewalScheduler periodically checks the cert and re-issues it at the
// 30-day renewal threshold (key reuse via cert.Manager.GenerateCSR).
func (d *Daemon) runRenewalScheduler(ctx context.Context) {
	if d.direct == nil || d.direct.cert == nil {
		return
	}
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.renewCertIfNeeded(ctx)
		}
	}
}

func (d *Daemon) renewCertIfNeeded(ctx context.Context) {
	ds := d.direct
	if ds == nil || ds.cert == nil || !ds.cert.Installed() {
		return // initial enrollment handles the first CSR
	}
	if !ds.cert.NeedsRenewal() {
		return
	}
	csr, err := ds.cert.GenerateCSR()
	if err != nil {
		log.Printf("renewal: generate CSR: %v", err)
		return
	}
	if err := d.signaling.SubmitCSR(ctx, string(csr)); err != nil {
		log.Printf("renewal: submit CSR: %v", err)
		return
	}
	log.Printf("renewal: submitted CSR (cert within 30-day window)")
}

// sleepCtx sleeps for d or until ctx is cancelled, returning false on cancel.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Stop gracefully shuts down the daemon.
func (d *Daemon) Stop() error {
	d.mu.RLock()
	sessions := make([]*Session, 0, len(d.sessions))
	for _, session := range d.sessions {
		sessions = append(sessions, session)
	}
	d.mu.RUnlock()

	for _, session := range sessions {
		d.closeSessionResources(session)
	}

	// Stop the relay tunnel first (§7.4 step 6): the frpc child is stopped
	// gracefully (killed if it ignores the graceful stop), gateway presence
	// then expires on its own.
	d.stopTunnelManager()

	// Tear down the direct serving path: the HTTPS listener leaves service
	// only on daemon shutdown (§7.4) — never on a control-WebSocket
	// disconnect — and the gate/readiness epoch state is reset for a clean
	// shutdown.
	d.stopDirectServer()
	d.onSignalingDisconnect()

	// Stop web server
	if d.webServer != nil {
		if err := d.webServer.Stop(); err != nil {
			log.Printf("stop web server: %v", err)
		}
	}

	// Close signaling connection (this will stop the listener)
	// Note: signaling.Client doesn't have a Close method, but Listen will
	// return when context is cancelled

	return nil
}

// CreateSession creates a new share session and registers it with the
// signaling server. Phase 4a serves direct and relay-only public Immich
// gallery shares (§13.3: the temporary relay-only rejection is removed;
// relay-only mode follows the persisted DefaultRelayOnly setting). WebDAV/
// file (opencloud/nextcloud) shares remain rejected deferred share types.
func (d *Daemon) CreateSession(ctx context.Context, shareURL, shareType, password string, expiryDuration time.Duration, maxDownloads int, relayOnly bool) (string, error) {
	switch shareType {
	case "immich":
		return d.createManualImmichSession(ctx, shareURL, expiryDuration, maxDownloads)
	case "opencloud", "nextcloud":
		return "", validationError{message: fmt.Sprintf("share type %q is not supported", shareType)}
	default:
		return "", validationError{message: fmt.Sprintf("share type %q is not supported", shareType)}
	}
}

func (d *Daemon) createManualImmichSession(ctx context.Context, shareURL string, expiryDuration time.Duration, maxDownloads int) (string, error) {
	const prefix = "immich://"
	if !strings.HasPrefix(shareURL, prefix) || strings.TrimPrefix(shareURL, prefix) == "" {
		return "", validationError{message: "share_url must be immich://KEY for manual Immich shares"}
	}
	key := strings.TrimPrefix(shareURL, prefix)

	d.mu.RLock()
	existing := d.sessions[key]
	d.mu.RUnlock()
	if existing != nil && existing.ShareType == "immich" {
		return existing.Code, nil
	}

	poller, err := d.getImmichPoller()
	if err != nil {
		return "", err
	}
	links, err := poller.PollShares(ctx)
	if err != nil {
		return "", err
	}

	var link immich.SharedLink
	found := false
	for _, candidate := range links {
		if candidate.Key == key {
			link = candidate
			found = true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("immich share %q not found", key)
	}
	// Phase 3: password-protected shares are deferred.
	if link.IsPasswordProtected() {
		return "", validationError{message: "password-protected Immich shares are not supported"}
	}

	session, err := d.registerImmichShare(ctx, link, maxDownloads, time.Now().Add(expiryDuration))
	if err != nil {
		return "", err
	}

	d.mu.Lock()
	d.sessions[session.Code] = session
	d.mu.Unlock()

	if d.OnSessionAdded != nil {
		d.OnSessionAdded(session)
	}
	return session.Code, nil
}

// RevokeSession removes a session by code, deregistering it from the
// signaling server and closing all peer connections.
func (d *Daemon) RevokeSession(code string) error {
	d.mu.Lock()
	session, ok := d.sessions[code]
	if !ok {
		d.mu.Unlock()
		return fmt.Errorf("session %s not found", code)
	}
	delete(d.sessions, code)
	d.mu.Unlock()

	d.closeSessionResources(session)

	// Remove from store
	if err := d.store.DeleteSession(code); err != nil {
		log.Printf("warning: could not delete session from store: %v", err)
	}
	d.revokeOrigin(code)

	// Notify server of deregistration (send deregister message).
	_ = d.deregisterShare(context.Background(), code, "revoked")

	log.Printf("session revoked: %s", code)

	// Notify callback
	if d.OnSessionRemoved != nil {
		d.OnSessionRemoved(code)
	}

	return nil
}

// ListSessions returns all active sessions.
func (d *Daemon) ListSessions() []*Session {
	d.mu.RLock()
	defer d.mu.RUnlock()

	sessions := make([]*Session, 0, len(d.sessions))
	for _, session := range d.sessions {
		sessions = append(sessions, session)
	}
	return sessions
}

// GetSession returns a session by code, or nil if not found.
func (d *Daemon) GetSession(code string) *Session {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.sessions[code]
}

// handleSignalingMessage dispatches incoming signaling messages to the
// appropriate handlers.
func (d *Daemon) handleSignalingMessage(msg signaling.Message) {
	switch msg.Type {
	case "welcome":
		log.Println("agent authenticated with signaling server")
		d.signalingConnected = true

	case "enrolled":
		d.handleEnrolled(msg)

	case "cert_issue":
		d.handleCertIssue(msg)

	case "cert_error":
		d.handleCertError(msg)

	case "enrollment_ready":
		d.handleEnrollmentReady(msg)

	case "open_signal":
		d.handleOpenSignal(msg)

	case "relay_config":
		d.applyRelayConfig(msg)

	case "stun_challenge":
		d.handleStunChallenge(msg)

	case "error":
		log.Printf("signaling error: %s", msg.Err)
	}
}

// handleEnrolled persists the control-assigned namespace, rebuilds the binder
// (if the namespace changed), and initiates or re-affirms certificate issuance.
func (d *Daemon) handleEnrolled(msg signaling.Message) {
	ds := d.direct
	if ds == nil || msg.Namespace == "" {
		return
	}

	ds.mu.Lock()
	nsChanged := ds.namespace != msg.Namespace
	ds.namespace = msg.Namespace
	ds.mu.Unlock()

	if err := ds.cert.SetNamespace(msg.Namespace); err != nil {
		log.Printf("persist namespace: %v", err)
	}
	d.syncDirectServe()

	if nsChanged || !ds.cert.Installed() || ds.cert.NeedsRenewal() {
		csr, err := ds.cert.GenerateCSR()
		if err != nil {
			log.Printf("generate CSR: %v", err)
			return
		}
		if err := d.signaling.SubmitCSR(context.Background(), string(csr)); err != nil {
			log.Printf("submit CSR: %v", err)
		}
		return
	}

	// Reconnect reconciliation: cert already installed and not expiring —
	// re-affirm tls_ready so the control marks the current epoch ready.
	d.sendTLSReady()
	d.learnAndReportPublicIP()
}

// handleCertIssue validates and installs the issued chain, then reports the
// installed leaf to the control (or a tls_error on validation failure).
func (d *Daemon) handleCertIssue(msg signaling.Message) {
	ds := d.direct
	if ds == nil || msg.ChainPEM == "" {
		return
	}
	if err := ds.cert.Install([]byte(msg.ChainPEM)); err != nil {
		log.Printf("install issued chain: %v", err)
		_ = d.signaling.TLSError(context.Background(), err.Error())
		return
	}
	d.sendTLSReady()
	// TLS is now ready; the control completes baseline enrollment from TLS +
	// relay DNS (§7.1). Learning and reporting the public IP here only feeds
	// the optional direct capability and never gates enrollment.
	d.learnAndReportPublicIP()
}

func (d *Daemon) handleCertError(msg signaling.Message) {
	log.Printf("cert issuance error: %s", msg.Reason)
}

// handleEnrollmentReady marks the current connection epoch baseline-ready:
// supported shares may now be registered and the HTTPS server/tunnel start.
// It no longer claims direct reachability (§11.2); direct state is an optional
// route capability.
func (d *Daemon) handleEnrollmentReady(msg signaling.Message) {
	ds := d.direct
	if ds == nil {
		return
	}
	ds.mu.Lock()
	ds.ready = true
	if ds.cond != nil {
		ds.cond.Broadcast()
	}
	ds.mu.Unlock()

	// Learn/report the public IP and start the direct HTTPS server. Both are
	// idempotent, re-run safely on every reconnect, and are no-ops when the
	// direct path is unavailable (e.g. no port mapper behind CGNAT).
	d.learnAndReportPublicIP()
	d.startDirectServer()
	log.Printf("baseline enrollment ready")
}

// sendTLSReady reports the installed leaf fingerprint + not_after to the control.
func (d *Daemon) sendTLSReady() {
	ds := d.direct
	if ds == nil || ds.cert == nil {
		return
	}
	fp, err := ds.cert.LeafFingerprint()
	if err != nil {
		log.Printf("leaf fingerprint: %v", err)
		return
	}
	notAfter, err := ds.cert.NotAfter()
	if err != nil {
		log.Printf("leaf not_after: %v", err)
		return
	}
	if err := d.signaling.TLSReady(context.Background(), fp, notAfter.Format(time.RFC3339)); err != nil {
		log.Printf("send tls_ready: %v", err)
	}
}

// learnAndReportPublicIP fetches the agent's public IP from the port mapper and
// (a) records it on the endpoint reporter so subsequent open/close transitions
// carry a fresh IP, and (b) sends an initial report_endpoint {ip, 0} so the
// control can provision the direct wildcard DDNS record. It is idempotent and a
// no-op when the mapper is unavailable or the IP is empty. It never affects
// baseline enrollment (§7.1) — only the optional direct capability.
func (d *Daemon) learnAndReportPublicIP() {
	ds := d.direct
	if ds == nil || ds.mapper == nil {
		return
	}
	ip, err := ds.mapper.ExternalIP()
	if err != nil || ip == "" {
		log.Printf("direct public IP unavailable: %v", err)
		return
	}
	if ds.reporter != nil {
		ds.reporter.SetIP(ip)
	}
	if err := d.signaling.ReportEndpoint(context.Background(), ip, 0, ""); err != nil {
		log.Printf("report endpoint: %v", err)
	}
}

// startDirectServer starts the direct HTTPS server on the internal listen
// address. It is idempotent (guarded by ds.started under ds.mu); the server is
// closed when the context is cancelled on disconnect (onSignalingDisconnect).
func (d *Daemon) startDirectServer() {
	ds := d.direct
	if ds == nil || ds.server == nil {
		return
	}
	ds.mu.Lock()
	if ds.started {
		ds.mu.Unlock()
		return
	}
	ds.started = true
	ds.startGen++
	gen := ds.startGen
	ctx, cancel := context.WithCancel(context.Background())
	ds.cancel = cancel
	listenAddr := ds.listenAddr
	ds.mu.Unlock()
	if listenAddr == "" {
		listenAddr = fmt.Sprintf(":%d", directIntPort)
	}

	go func() {
		if err := ds.server.Start(ctx, listenAddr); err != nil {
			log.Printf("direct server: %v", err)
			cancel() // release the epoch ctx
			// A bind/start failure must not latch the epoch as started: reset
			// the guard so a later enrollment_ready retries. The generation
			// check ensures a stale failure cannot clobber a newer start
			// attempt (or a disconnect that already reset the epoch).
			ds.mu.Lock()
			if ds.startGen == gen {
				ds.cancel = nil
				ds.started = false
			}
			ds.mu.Unlock()
		}
	}()
}

// handleOpenSignal admits a control-plane open-signal, opens the on-demand
// port, and acknowledges with the fresh public IP + granted port.
func (d *Daemon) handleOpenSignal(msg signaling.Message) {
	if !d.canRegisterDirect() {
		_ = d.signaling.OpenAck(context.Background(), signaling.OpenAck{
			ShareID: msg.ShareID, Nonce: msg.Nonce, Seq: msg.Seq, Status: "error", Error: "not_ready"})
		return
	}
	// readiness can be true even when the port mapper was unavailable at
	// construction (ds.mapper/ds.port stay nil), so guard every dereference.
	ds := d.direct
	if ds == nil || ds.gate == nil || ds.port == nil || ds.mapper == nil {
		_ = d.signaling.OpenAck(context.Background(), signaling.OpenAck{
			ShareID: msg.ShareID, Nonce: msg.Nonce, Seq: msg.Seq, Status: "error", Error: "direct_unavailable"})
		return
	}
	exp, err := time.Parse(time.RFC3339, msg.ExpiresAt)
	if err != nil {
		_ = d.signaling.OpenAck(context.Background(), signaling.OpenAck{
			ShareID: msg.ShareID, Nonce: msg.Nonce, Seq: msg.Seq, Status: "error", Error: "bad_expiry"})
		return
	}
	sig := direct.OpenSignal{
		Version:   msg.Version,
		AgentID:   msg.AgentID,
		ShareID:   msg.ShareID,
		RouteKind: direct.RouteDirect,
		Nonce:     msg.Nonce,
		Seq:       msg.Seq,
		ExpiresAt: exp,
		Lease:     time.Duration(msg.LeaseSeconds) * time.Second,
	}
	if err := ds.gate.Admit(sig); err != nil {
		_ = d.signaling.OpenAck(context.Background(), signaling.OpenAck{
			ShareID: msg.ShareID, Nonce: msg.Nonce, Seq: msg.Seq, Status: "error", Error: "rejected"})
		return
	}
	// Fetch the fresh public IP and record it on the reporter BEFORE opening
	// the port: OpenFor synchronously fires the open transition on the state
	// loop, which snapshots the reporter's IP. Doing this first keeps the open
	// report_endpoint consistent with open_ack even when the public IP changed
	// mid-epoch.
	ip, err := ds.mapper.ExternalIP() // FRESH on every ack
	if err != nil || ip == "" {
		_ = d.signaling.OpenAck(context.Background(), signaling.OpenAck{
			ShareID: msg.ShareID, Nonce: msg.Nonce, Seq: msg.Seq, Status: "error", Error: "no_public_ip"})
		return
	}
	if ds.reporter != nil {
		ds.reporter.SetIP(ip) // keep subsequent transition reports fresh
	}
	wasOpen := ds.port.Open()
	if err := ds.port.OpenFor(msg.ShareID, sig.Lease); err != nil {
		_ = d.signaling.OpenAck(context.Background(), signaling.OpenAck{
			ShareID: msg.ShareID, Nonce: msg.Nonce, Seq: msg.Seq, Status: "error", Error: "open_failed"})
		return
	}
	_ = d.signaling.OpenAck(context.Background(), signaling.OpenAck{
		ShareID: msg.ShareID, Nonce: msg.Nonce, Seq: msg.Seq,
		GrantedPort: ds.port.GrantedPort(), PublicIP: ip,
		WasAlreadyOpen: wasOpen, Status: "ok"})
}

// loadSessionsFromStore loads persisted sessions from the store and
// re-registers them with the signaling server.
func (d *Daemon) loadSessionsFromStore(ctx context.Context) {
	sessions := d.store.ListSessions(true) // Filter expired

	for _, entry := range sessions {
		if entry.ShareType == "" {
			log.Printf("warning: skipping legacy session %s: missing share_type", entry.Code)
			continue
		}

		// Deferred share types remain unsupported (§13.3): protected Immich
		// and WebDAV/file (opencloud/nextcloud) sessions are still removed
		// and their control registrations tombstoned. Relay-only public
		// Immich sessions are restored instead (§13.3).
		if entry.IsPasswordProtected || entry.ShareType != "immich" {
			log.Printf("warning: removing unsupported persisted session %s (share_type=%s relay_only=%v protected=%v)", entry.Code, entry.ShareType, entry.RelayOnly, entry.IsPasswordProtected)
			d.cleanupPersistedSession(ctx, entry)
			continue
		}

		// Every session waits for the current epoch's baseline readiness
		// (enrollment_ready: TLS + relay DNS, §7.1) before re-registration —
		// relay-only sessions included, since the same listener serves both
		// routes. Direct AVAILABILITY (DDNS/public port) is never waited on.
		if err := d.waitForDirectReady(ctx); err != nil {
			log.Printf("warning: direct transport not ready, deferring session %s: %v", entry.Code, err)
			continue
		}

		client, err := d.newImmichClient(entry.Code)
		if err != nil {
			log.Printf("warning: could not create Immich client for %s: %v", entry.Code, err)
			continue
		}
		// §15.3 restart ordering: hydrate the content snapshot BEFORE the
		// re-registration (the report that makes the share publicly reachable
		// again). The snapshot manager is keyed by the persisted share code,
		// which control preserves for a preferred-code registration.
		session := &Session{
			Code:                entry.Code,
			ShareURL:            entry.ShareURL,
			ShareType:           "immich",
			IsPasswordProtected: entry.IsPasswordProtected,
			ExpiresAt:           entry.ExpiresAt,
			MaxDownloads:        entry.MaxDownloads,
			Downloads:           entry.Downloads,
			RelayOnly:           entry.RelayOnly,
			CreatedAt:           entry.CreatedAt,
			immich:              client,
		}
		d.hydrateContentSession(session)

		reg, ok := d.signaling.(shareOptionRegistrar)
		if !ok {
			log.Printf("warning: signaling client does not support Immich registration options for %s", entry.Code)
			continue
		}
		code, origin, reconnected, err := reg.RegisterShareWithOptions(ctx, signaling.RegisterShareOptions{
			ShareURL:            entry.ShareURL,
			PreferredCode:       entry.Code,
			ShareType:           "immich",
			IsPasswordProtected: entry.IsPasswordProtected,
			RelayOnly:           entry.RelayOnly,
		})
		if err != nil {
			log.Printf("warning: could not re-register Immich session %s: %v", entry.Code, err)
			continue
		}
		if code != entry.Code {
			// A reassigned code would orphan the just-hydrated snapshot and
			// miskey download accounting: refuse to advertise the share rather
			// than serve bookkeeping under two identities.
			log.Printf("warning: control reassigned share code %s → %s during restore; skipping", entry.Code, code)
			continue
		}
		if origin == "" {
			origin = entry.Origin
		}
		// §13.3: a restored relay-only session binds its relay origin only —
		// zero direct-path setup, no originPair; direct sessions bind the
		// full §6 pair.
		if entry.RelayOnly {
			d.bindRelayOnlyOrigin(code, origin)
		} else {
			d.bindOrigin(code, origin)
		}

		d.mu.Lock()
		d.sessions[code] = session
		d.mu.Unlock()

		if reconnected {
			log.Printf("Immich session reconnected - code: %s", code)
		} else {
			log.Printf("Immich session loaded - code: %s", code)
		}
	}

	log.Printf("loaded %d sessions from store", len(d.sessions))
}

func (d *Daemon) runImmichPoller(ctx context.Context) {
	if err := d.syncImmichShares(ctx); err != nil {
		log.Printf("immich sync: %v", err)
	}
	interval := time.Duration(d.config.ImmichPollInterval) * time.Second
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.syncImmichShares(ctx); err != nil {
				log.Printf("immich sync: %v", err)
			}
		}
	}
}

func (d *Daemon) syncImmichShares(ctx context.Context) error {
	poller, err := d.getImmichPoller()
	if err != nil {
		return err
	}
	links, err := poller.PollShares(ctx)
	if err != nil {
		return err
	}

	// `present` tracks every non-empty Immich link key (including protected and
	// non-ALBUM links that Phase 3 skips) so the removal pass can distinguish an
	// unsupported share (still present but skipped → 410) from a revoked share
	// (deleted upstream → 404).
	present := make(map[string]immich.SharedLink, len(links))
	seen := make(map[string]immich.SharedLink, len(links))
	for _, link := range links {
		if link.Key == "" {
			continue
		}
		present[link.Key] = link
		if !strings.EqualFold(link.Type, "ALBUM") {
			continue
		}
		// Phase 3: password-protected shares are deferred. Skip before adding
		// to `seen` so any previously-registered protected session is removed
		// below (stale-registration clean-up).
		if link.IsPasswordProtected() {
			continue
		}
		seen[link.Key] = link

		d.mu.RLock()
		existing, exists := d.sessions[link.Key]
		d.mu.RUnlock()
		if exists {
			// Keep already-registered shares fresh: re-drive their snapshot
			// Build so the fail-closed refresh clock stays ahead of 2×poll.
			d.hydrateContentSession(existing)
			continue
		}

		session, err := d.registerImmichShare(ctx, link, d.GetConfig().DefaultMaxDownloads, time.Time{})
		if err != nil {
			return err
		}
		d.mu.Lock()
		d.sessions[session.Code] = session
		d.mu.Unlock()
		if d.OnSessionAdded != nil {
			d.OnSessionAdded(session)
		}
	}

	var removed []*Session
	d.mu.RLock()
	for code, session := range d.sessions {
		if session.ShareType == "immich" {
			if _, ok := seen[code]; !ok {
				removed = append(removed, session)
			}
		}
	}
	d.mu.RUnlock()

	for _, session := range removed {
		d.closeSessionResourcesWithError(session, "share has been removed")
		// A removed session whose link is still present in Immich but skipped
		// here (protected/non-ALBUM) is unsupported → tombstone 410; one that
		// was deleted upstream is revoked → 404.
		if _, unsupported := present[session.Code]; unsupported {
			if err := d.deregisterShare(ctx, session.Code, "unsupported"); err != nil {
				return err
			}
		} else if err := d.unregisterShare(ctx, session.Code); err != nil {
			return err
		}
		if err := d.store.DeleteSession(session.Code); err != nil {
			return err
		}
		d.revokeOrigin(session.Code)
		d.mu.Lock()
		if d.sessions[session.Code] == session {
			delete(d.sessions, session.Code)
		}
		d.mu.Unlock()
		if d.OnSessionRemoved != nil {
			d.OnSessionRemoved(session.Code)
		}
	}

	return nil
}

func (d *Daemon) getImmichPoller() (immichPoller, error) {
	if d.newImmichPoller != nil {
		return d.newImmichPoller()
	}
	return immich.New(immich.Config{
		BaseURL:     d.config.ImmichURL,
		AllowedHost: d.config.ImmichAllowedHost,
		APIKey:      d.config.ImmichAPIKey,
	})
}

func (d *Daemon) registerImmichShare(ctx context.Context, link immich.SharedLink, maxDownloads int, expiresAt time.Time) (*Session, error) {
	reg, ok := d.signaling.(shareOptionRegistrar)
	if !ok {
		return nil, fmt.Errorf("signaling client does not support option registration")
	}

	shareURL := "immich://" + link.Key
	passwordProtected := link.IsPasswordProtected()
	relayOnly := d.GetConfig().DefaultRelayOnly

	// Password-protected Immich shares remain a deferred share type (§13.3);
	// relay-only public Immich shares are restored (Phase 4a).
	if passwordProtected {
		return nil, validationError{message: "password-protected Immich shares are not supported"}
	}

	// Every registration waits for the current epoch's baseline readiness
	// (enrollment_ready: TLS + relay DNS, §7.1) — direct AND relay-only
	// shares alike, since the same listener serves both routes. Direct
	// availability (DDNS/public port) is never waited on.
	if err := d.waitForDirectReady(ctx); err != nil {
		return nil, fmt.Errorf("direct transport not ready: %w", err)
	}

	log.Printf("registering Immich share %s (relay_only=%v, password_protected=%v)", link.Key, relayOnly, passwordProtected)
	opts := signaling.RegisterShareOptions{
		ShareURL:            shareURL,
		PreferredCode:       link.Key,
		ShareType:           "immich",
		IsPasswordProtected: passwordProtected,
		RelayOnly:           relayOnly,
	}
	code, origin, _, err := reg.RegisterShareWithOptions(ctx, opts)
	if err != nil {
		return nil, err
	}
	// §13.3: a relay-only share binds its relay origin only — zero
	// direct-path setup (the direct origin stays unbound and no originPair
	// is created); a direct share binds the full §6 pair.
	var relayOrigin string
	if relayOnly {
		relayOrigin, _ = d.bindRelayOnlyOrigin(code, origin)
	} else if pair, bound := d.bindOrigin(code, origin); bound {
		relayOrigin = pair.relayOrigin
	}
	client, err := d.newImmichClient(code)
	if err != nil {
		_ = d.unregisterShare(ctx, code)
		return nil, err
	}

	now := time.Now()
	session := &Session{
		Code:                code,
		ShareURL:            shareURL,
		ShareType:           "immich",
		IsPasswordProtected: passwordProtected,
		RelayOnly:           relayOnly,
		MaxDownloads:        maxDownloads,
		ExpiresAt:           expiresAt,
		CreatedAt:           now,
		immich:              client,
	}
	if err := d.store.SaveSession(store.SessionEntry{
		Code:                code,
		ShareURL:            shareURL,
		ShareType:           "immich",
		IsPasswordProtected: passwordProtected,
		RelayOnly:           relayOnly,
		MaxDownloads:        maxDownloads,
		ExpiresAt:           expiresAt,
		CreatedAt:           now,
		Origin:              origin,
		RelayOrigin:         relayOrigin,
	}); err != nil {
		_ = d.unregisterShare(ctx, code)
		return nil, err
	}
	d.hydrateContentSession(session)
	return session, nil
}

func (d *Daemon) unregisterShare(ctx context.Context, code string) error {
	if unreg, ok := d.signaling.(shareUnregistrar); ok {
		return unreg.UnregisterShare(ctx, code)
	}
	return d.signaling.Send(ctx, map[string]string{"type": "unregister_share", "code": code})
}

// deregisterShare notifies the control plane of an agent-initiated lifecycle
// transition with an explicit inactive_reason discriminator (RevokeSession →
// "revoked", pruneExpiredSessions → "expired", unsupported cleanup →
// "unsupported").
func (d *Daemon) deregisterShare(ctx context.Context, code, reason string) error {
	return d.signaling.Send(ctx, map[string]string{
		"type":   "deregister",
		"code":   code,
		"reason": reason,
	})
}

// cleanupPersistedSession removes a persisted session that is unsupported in
// Phase 3: it deregisters the control-side session as unsupported (410) and
// deletes the local entry. It must NOT use unregister_share, which hardcodes
// inactive_reason="revoked" (404).
func (d *Daemon) cleanupPersistedSession(ctx context.Context, entry store.SessionEntry) {
	if err := d.deregisterShare(ctx, entry.Code, "unsupported"); err != nil {
		log.Printf("warning: could not deregister unsupported session %s: %v", entry.Code, err)
	}
	if err := d.store.DeleteSession(entry.Code); err != nil {
		log.Printf("warning: could not delete unsupported session %s: %v", entry.Code, err)
	}
}

func (d *Daemon) newImmichClient(code string) (*immich.Client, error) {
	return immich.New(immich.Config{
		BaseURL:     d.config.ImmichURL,
		AllowedHost: d.config.ImmichAllowedHost,
		APIKey:      d.config.ImmichAPIKey,
		ShareKey:    code,
	})
}

// hydrateContentSession ensures a SnapshotManager exists in the registry for
// the session's share and drives a snapshot Build. Build runs from a
// daemon/background context, never the HTTP request context: a request-scoped
// context cancelled during ListGallery would wrongly advance the fail-closed
// clock.
func (d *Daemon) hydrateContentSession(session *Session) {
	if session == nil || session.immich == nil || d.resolver == nil {
		return
	}
	mgr := d.resolver.Get(session.Code)
	if mgr == nil {
		mgr = direct.NewSnapshotManagerWithDownloads(session.immich, session.MaxDownloads, session.Downloads, d.immichPollInterval())
		d.resolver.Put(session.Code, mgr)
	}
	// Wire download-accounting persistence (§11.1): every committed download
	// (original-asset and album-archive alike) is durably recorded via
	// store.IncrementDownloads. The closure captures the immutable share code;
	// a persistence failure is logged inside the ledger and the in-memory count
	// still enforces MaxDownloads.
	mgr.SetPersistDownload(func(count int) error {
		_, err := d.store.IncrementDownloads(session.Code)
		return err
	})
	if err := mgr.Build(context.Background()); err != nil {
		log.Printf("hydrate content snapshot for %s: %v", session.Code, err)
	}
}

// immichPollInterval returns the configured Immich poll interval (default 30s),
// used as the SnapshotManager's fail-closed refresh bound.
func (d *Daemon) immichPollInterval() time.Duration {
	interval := time.Duration(d.config.ImmichPollInterval) * time.Second
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return interval
}

func (d *Daemon) closeSessionResources(session *Session) {
	d.closeSessionResourcesWithError(session, "")
}

func (d *Daemon) closeSessionResourcesWithError(session *Session, message string) {
	session.mu.Lock()
	session.closing = true
	session.mu.Unlock()
}

// runExpiryPruner periodically checks for and removes expired sessions.
func (d *Daemon) runExpiryPruner(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.pruneExpiredSessions()
		}
	}
}

// pruneExpiredSessions removes all expired sessions.
func (d *Daemon) pruneExpiredSessions() {
	now := time.Now()
	d.mu.Lock()
	type expiredSession struct {
		code    string
		session *Session
	}
	var expired []expiredSession
	for code, session := range d.sessions {
		if !session.ExpiresAt.IsZero() && session.ExpiresAt.Before(now) {
			delete(d.sessions, code)
			expired = append(expired, expiredSession{code: code, session: session})
		}
	}
	d.mu.Unlock()

	for _, item := range expired {
		log.Printf("session %s expired at %s", item.code, item.session.ExpiresAt)
		d.closeSessionResources(item.session)
		if err := d.store.DeleteSession(item.code); err != nil {
			log.Printf("warning: could not delete expired session: %v", err)
		}
		d.revokeOrigin(item.code)
		_ = d.deregisterShare(context.Background(), item.code, "expired")
		if d.OnSessionRemoved != nil {
			d.OnSessionRemoved(item.code)
		}
	}
}

// IsConnected returns whether the daemon has authenticated with the signaling server.
func (d *Daemon) IsConnected() bool {
	return d.signalingConnected
}

// HasTURN reports whether TURN servers are available. The v1 relay/TURN
// transport is deleted, so this is always false; it remains for the admin web
// UI's Daemon interface.
func (d *Daemon) HasTURN() bool {
	return false
}

// GetConfig returns a copy of the current configuration.
func (d *Daemon) GetConfig() *config.Config {
	d.mu.RLock()
	cfg := d.config
	d.mu.RUnlock()
	return cfg
}

// GetUptime returns the duration since the daemon started.
func (d *Daemon) GetUptime() time.Duration {
	return time.Since(d.startTime)
}

// GetConfigPath returns the path to the config file on disk.
func (d *Daemon) GetConfigPath() string {
	if d.configMgr != nil {
		if mgr, ok := d.configMgr.(*config.Manager); ok {
			return mgr.FilePath()
		}
	}
	return ""
}

// SaveConfig updates and persists the configuration.
// It uses the config manager to save to the config file.
func (d *Daemon) SaveConfig(cfg *config.Config) error {
	d.mu.Lock()
	d.config = cfg
	d.mu.Unlock()
	if d.configMgr != nil {
		// Cast to concrete type to access Save method
		if mgr, ok := d.configMgr.(*config.Manager); ok {
			return mgr.Save(cfg)
		}
	}
	return nil
}
