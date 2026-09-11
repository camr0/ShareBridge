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
	"sync/atomic"
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

	// tunnelCtx/tunnelOptions retain the construction inputs of the tunnel
	// manager so §13.4 unlock can build a FRESH manager after lockdown stopped
	// the previous one (Manager.Stop is permanent: stopChannel/doneChannel are
	// closed). Production records the process context and no options; tests
	// record their fake process starter. Written under mu in
	// startTunnelManager.
	tunnelCtx     context.Context
	tunnelOptions []tunnel.ManagerOption

	// lockdownGen is the monotonic §11.1 lockdown_status generation (under
	// mu). It increments on every lockdown/unlock transition so control can
	// reject a stale report within the current connection epoch.
	lockdownGen uint64

	// lockdownMu serializes the §13.4 Lockdown/Unlock transitions so two
	// concurrent admin calls cannot interleave their best-effort actions.
	lockdownMu sync.Mutex

	// lockdownLeverTimeout bounds how long Lockdown waits for its concurrent
	// best-effort levers; zero means defaultLockdownLeverTimeout. Tests shorten
	// it to prove the bound without waiting the production value.
	lockdownLeverTimeout time.Duration

	// shutdownLeverTimeout bounds how long Stop waits for the tunnel-stop
	// lever before running the remaining teardown anyway; zero means
	// defaultShutdownLeverTimeout. The tunnel manager bounds its own post-kill
	// wait; this is an INDEPENDENT safety net so an unkillable frpc child can
	// never strand the HTTPS listener, connections, or web surface (audit I4).
	// Tests shorten it to prove the bound.
	shutdownLeverTimeout time.Duration

	// lockdownLeverGate is a TEST-ONLY seam: when non-nil it is called with a
	// best-effort lever's name immediately before that lever starts its work.
	// It lets the lockdown tests hold a named lever past the aggregation bound
	// and prove the §13.4 transition fences. Production leaves it nil.
	lockdownLeverGate func(name string)

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

	// defaultLockdownLeverTimeout bounds how long Lockdown waits for its
	// best-effort levers. The local locked gate is final before the wait
	// starts, so a stalled lever (a blocking router delete, an frpc kill
	// grace) can delay only its own completion — never the others, and never
	// the return.
	defaultLockdownLeverTimeout = 10 * time.Second

	// defaultShutdownLeverTimeout bounds how long Daemon.Stop waits for the
	// tunnel-stop lever before tearing down the remaining surfaces anyway. It
	// is an independent safety net above the tunnel manager's own bounded
	// post-kill wait (kill grace + kill wait, 7s by default): even a manager
	// that somehow never returns must not leave the HTTPS listener serving
	// during shutdown (audit I4).
	defaultShutdownLeverTimeout = 10 * time.Second

	// defaultDirectHandoffTimeout bounds how long a listener rebuild waits for
	// the previous listener goroutine to exit (releasing its socket) before it
	// refuses to bind the replacement. The wait is never performed while
	// holding ds.mu, so a listener that needs the lock to finish its exit can
	// always make progress; the bound is a fail-closed safety net, not a
	// routine timeout.
	defaultDirectHandoffTimeout = 2 * time.Second

	// defaultDirectListenStartTimeout bounds how long startDirectServer waits
	// for the listener's bind confirmation before failing the start closed. The
	// bind itself is synchronous (net.Listen on the start goroutine), so the
	// wait only covers goroutine scheduling; the bound is a safety net that
	// keeps Unlock — which must not open admission until the listener is
	// confirmed serving — from blocking indefinitely. The wait holds no
	// daemon lock, so the listener goroutine can always run.
	defaultDirectListenStartTimeout = 2 * time.Second

	// defaultOpenReportTimeout bounds the open path's endpoint-report
	// confirmation: the report carrying the router-granted external port must
	// be sent AND confirmed inside this window before the OK open_ack. The
	// report is sent from the Reporter's drain goroutine, so the wait is not a
	// router bound and never runs while holding ds.mu, ackMu, or the port
	// state loop. On timeout the open FAILS (error open_ack) and the mapping
	// is torn down — the control must never receive an OK ack for an endpoint
	// it was not told about.
	defaultOpenReportTimeout = 3 * time.Second
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
	// syncMu serializes syncDirectServe rebuilds with each other, so a listener
	// cannot be rebuilt on the old server while another rebuild is between its
	// drain/cancel and its single publication point. It is held across a
	// rebuild's bounded listener-exit wait, so it is deliberately NOT taken by
	// startDirectServer: holding it across the listener start's bind-confirmation
	// wait would serialize every rebuild (and the Unlock/enrollment paths that
	// drive one) behind that wait (finding A1). The start and the rebuild
	// coordinate through the rebuildInProgress/startInProgress single-flight
	// state below instead. The handoff releases ds.mu across the waiter, and
	// syncMu is acquired before ds.mu on every path and is never held while
	// blocking on another lock, so it cannot form a lock cycle.
	syncMu sync.Mutex

	// rebuildInProgress is set (under mu) for the duration of an ordered
	// rebuild's transition window: from the moment the old listener is marked
	// non-serving until the replacement is published or the handoff fails
	// closed. startDirectServer observes it and waits instead of starting the
	// old server being drained. It is a state flag, not the syncMu lock, because
	// the rebuild must not hold a lock that a concurrent start (or successor
	// rebuild) waits on while the rebuild performs its bounded listener-exit
	// wait.
	rebuildInProgress bool

	// startInProgress is set (under mu) from a start's transition until its bind
	// result is known, so a concurrent start waits for it and then re-evaluates
	// (never two live listeners, never a false success behind an unresolved
	// attempt). Like rebuildInProgress it is state, not a lock: the bind wait is
	// performed holding no lock, and a rebuild is free to supersede an in-flight
	// start.
	startInProgress bool

	// stopped is set by stopDirectServer (daemon shutdown). A start superseded by
	// shutdown must not loop and re-bind the listener.
	stopped bool

	// flowCh is the single-flight wake-up for startDirectServer's waiters: it is
	// closed and replaced (under mu) whenever rebuildInProgress or
	// startInProgress changes, so a waiter can block outside ds.mu without a
	// per-waiter channel. Lazily created (some tests build a directState literal).
	flowCh chan struct{}

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

	// listenerDone is closed when the current listener goroutine's Start call
	// returns, i.e. the listener's socket has been released. A rebuild waits on
	// it (bounded) before binding the replacement so the new listener can never
	// race the old socket (audit C1). Owned under mu.
	listenerDone chan struct{}

	// handoffErr is the fail-closed start guard: non-nil after a listener
	// handoff could not prove the old listener released its socket, so no
	// replacement may bind. syncDirectServe RETURNS this error to its caller
	// (the failure is never swallowed by private state) and startDirectServer
	// refuses to start while it is set; a later handoff that completes the wait
	// clears it before publishing. Owned under mu.
	handoffErr error

	// restorePending records that an Unlock did not complete: the listener
	// could not be brought back, so the relay restore lifecycle (fresh tunnel
	// manager + fresh credential) is still owed. Unlock re-runs the full
	// restore while it is set instead of short-circuiting as an idempotent
	// no-op, so a retried unlock after the handoff recovers completes the
	// restore rather than falsely reporting success. Owned under mu.
	restorePending bool

	// handoffTimeout overrides defaultDirectHandoffTimeout in tests (zero uses
	// the default). Owned under mu.
	handoffTimeout time.Duration

	// reportTimeout overrides defaultOpenReportTimeout in tests (zero uses the
	// default). It bounds the open path's endpoint-report confirmation wait.
	// It is read without ds.mu (it is set before any open-signal runs).
	reportTimeout time.Duration

	// startListenerFn overrides the listener's confirmed start
	// ((*direct.DirectServer).StartWithReady) in tests so a listener's bind/exit
	// can be delayed deterministically. ready reports the bind result (nil on
	// success) and must be called exactly once by the seam. Owned under mu.
	startListenerFn func(ctx context.Context, server *direct.DirectServer, addr string, ready func(error)) error

	// listenStartTimeout overrides defaultDirectListenStartTimeout in tests
	// (zero uses the default). It bounds how long startDirectServer waits for
	// the listener's bind confirmation; the wait holds no ds.mu. Owned under
	// mu.
	listenStartTimeout time.Duration

	// locked is the local §13.4 lockdown state: an availability stop, NOT
	// share revocation. While true the SignalGate refuses new opens, the
	// on-demand mapping stays closed, the listener stays out of service, and
	// the Binder holds no admissions — but origins/relayOrigins keep the
	// source-verified bindings recorded so unlock can restore them without a
	// tombstone. Local enforcement is final: control's advisory
	// lockdown_status can suppress attempts but never override this flag.
	locked bool

	// lockdownEpoch counts completed lockdown/unlock transitions (under mu).
	// Every best-effort lockdown lever captures the epoch of the transition
	// that created it and refuses to act once a newer transition owns the
	// state, so a lever that overruns the aggregation bound can never mutate
	// state behind an Unlock's back.
	lockdownEpoch uint64

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
	// Retain the construction inputs so §13.4 unlock can rebuild a fresh
	// manager after lockdown permanently stopped the previous one.
	d.tunnelCtx = ctx
	d.tunnelOptions = append([]tunnel.ManagerOption(nil), options...)
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
// step 6: lockdown and daemon shutdown are the only legitimate stops). The
// manager's Stop is bounded and returns an explicit error when an unkillable
// child does not exit, so callers surface it and keep shutdown moving rather
// than swallowing it (audit I4). Idempotent.
func (d *Daemon) stopTunnelManager() error {
	d.mu.RLock()
	manager := d.tunnel
	d.mu.RUnlock()
	if manager == nil {
		return nil
	}
	return manager.Stop()
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
//
// Re-binding an UNCHANGED pair is idempotent: the Binder keeps the pair's
// generation, so in-flight requests admitted from it survive the routine
// reconnect path that re-registers every persisted share. A pair that
// actually changes (a different origin, or a route-kind change) supersedes the
// recorded binding, which is withdrawn first so the superseded admission fails
// closed.
func (d *Daemon) bindOrigin(code, directOrigin string) (originPair, bool) {
	ds := d.direct
	if ds == nil || directOrigin == "" {
		return originPair{}, false
	}
	// Serialize the whole operation — Binder snapshot, Binder mutation, and the
	// daemon-side bookkeeping commit — under ds.mu. syncDirectServe publishes a
	// replacement binder only under ds.mu, so holding it across the mutation
	// means a rebuild can no longer swap the Binder between the snapshot and the
	// commit; the identity check below documents and enforces that invariant
	// (audit Important #1). The Binder's own lock is a leaf (it never calls back
	// into the daemon), and ds.mu → binder.mu is the order syncDirectServe
	// already uses, so there is no lock cycle and no router I/O in the critical
	// section.
	ds.mu.Lock()
	defer ds.mu.Unlock()
	binder := ds.binder
	if binder == nil {
		return originPair{}, false
	}
	relayOrigin, err := binder.RelayOriginFor(directOrigin)
	if err != nil {
		log.Printf("derive relay origin for share %s: %v", code, err)
		return originPair{}, false
	}
	pair := originPair{directOrigin: directOrigin, relayOrigin: relayOrigin}
	if ds.locked {
		// §13.4 step 4: lockdown removed the Binder admissions but retained
		// the source/session state. A share registered while locked is
		// recorded for unlock instead of being admitted now.
		if ds.origins == nil {
			ds.origins = make(map[string]originPair)
		}
		ds.origins[code] = pair
		return pair, true
	}
	// A re-bind that CHANGES the admission for this code supersedes the recorded
	// one: withdraw the old pair (and any relay-only binding the code used to
	// have) before installing the replacement, so an in-flight request admitted
	// from the superseded entry fails Revalidate and the recorded state always
	// matches what the Binder admits. An UNCHANGED re-bind falls through to the
	// Binder's own idempotency check, which preserves the generation — a routine
	// WS reconnect re-registers every persisted share and must not tear down
	// live transfers (M4 B2b fix round).
	if prev, ok := ds.origins[code]; ok && prev != pair {
		binder.RevokeShare(prev.directOrigin, prev.relayOrigin)
		delete(ds.origins, code)
	}
	if prevRelay, ok := ds.relayOrigins[code]; ok {
		// The code was relay-only and is now a direct pair: a route-kind change,
		// so the single relay binding must not survive alongside the pair.
		binder.Revoke(prevRelay)
		delete(ds.relayOrigins, code)
	}
	if err := binder.AllowShare(directOrigin, relayOrigin, code); err != nil {
		log.Printf("bind origins for share %s: %v", code, err)
		return originPair{}, false
	}
	if ds.binder != binder {
		// Unreachable while ds.mu is held across the mutation above; kept as a
		// fail-closed guard against a future refactor that moves the mutation
		// outside the lock. Withdraw the admission rather than book a binding
		// the serving Binder does not hold.
		binder.RevokeShare(directOrigin, relayOrigin)
		return originPair{}, false
	}
	if ds.origins == nil {
		ds.origins = make(map[string]originPair)
	}
	ds.origins[code] = pair
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
//
// Like bindOrigin, re-binding the UNCHANGED relay origin is idempotent (the
// Binder preserves the generation, so a routine reconnect does not invalidate
// in-flight requests), while a changed relay origin — or a route-kind change
// from a direct pair — withdraws the superseded binding first.
func (d *Daemon) bindRelayOnlyOrigin(code, directOrigin string) (string, bool) {
	ds := d.direct
	if ds == nil || directOrigin == "" {
		return "", false
	}
	// The whole Binder mutation + bookkeeping commit is one ds.mu critical
	// section (audit Important #1), exactly like bindOrigin: a rebuild cannot
	// swap the Binder mid-commit and the identity check below fails closed.
	ds.mu.Lock()
	defer ds.mu.Unlock()
	binder := ds.binder
	if binder == nil {
		// The binder (namespace) is not built yet — e.g. hydration racing
		// ahead of `enrolled`. Record the pending binding so syncDirectServe
		// installs it deterministically as soon as the binder exists, instead
		// of silently waiting for a future control re-allocation (Task 27
		// carry-forward). revokeOrigin deletes this entry, so a revoked share
		// can never be installed later.
		if ds.pendingRelayBinds == nil {
			ds.pendingRelayBinds = make(map[string]string)
		}
		ds.pendingRelayBinds[code] = directOrigin
		return "", false
	}
	relayOrigin, err := binder.RelayOriginFor(directOrigin)
	if err != nil {
		log.Printf("derive relay origin for share %s: %v", code, err)
		return "", false
	}
	if ds.locked {
		// §13.4 step 4: record the relay-only binding for unlock without
		// admitting it into the Binder while lockdown is active.
		if ds.relayOrigins == nil {
			ds.relayOrigins = make(map[string]string)
		}
		ds.relayOrigins[code] = relayOrigin
		delete(ds.pendingRelayBinds, code)
		return relayOrigin, true
	}
	// See bindOrigin: a changed relay-only binding (or a route-kind change from
	// a recorded direct pair) supersedes the previous admission, which is
	// withdrawn first. An identical re-bind is left to the Binder's idempotency
	// check and keeps the generation.
	if prev, ok := ds.relayOrigins[code]; ok && prev != relayOrigin {
		binder.Revoke(prev)
		delete(ds.relayOrigins, code)
	}
	if prevPair, ok := ds.origins[code]; ok {
		// The code was a direct pair and is now relay-only: a route-kind change,
		// so the §6 pair must not survive.
		binder.RevokeShare(prevPair.directOrigin, prevPair.relayOrigin)
		delete(ds.origins, code)
	}
	if err := binder.Allow(relayOrigin, direct.RouteRelay, code); err != nil {
		log.Printf("bind relay origin for share %s: %v", code, err)
		return "", false
	}
	if ds.binder != binder {
		binder.Revoke(relayOrigin)
		return "", false
	}
	if ds.relayOrigins == nil {
		ds.relayOrigins = make(map[string]string)
	}
	ds.relayOrigins[code] = relayOrigin
	delete(ds.pendingRelayBinds, code)
	return relayOrigin, true
}

// revokeOrigin drops the origin pair for a share code and revokes BOTH binder
// bindings as one logical operation (§6: revocation removes both bindings).
// For a relay-only share it drops the single relay-origin binding recorded by
// bindRelayOnlyOrigin instead. It is a no-op if no binding was recorded.
//
// Revocation is the invalidator (audit Critical #3 / Important #1): the Binder
// mutation and the daemon-side bookkeeping deletion happen in ONE ds.mu
// critical section, ordered strictly before the share's connections are
// closed. That ordering is what makes the handler's post-noteBinding
// revalidation decisive: a request admitted from the revoked entry either sees
// the mutation (and fails closed) or has already recorded its connection
// binding, so the close-by-share scan below matches it. Revocation also deletes
// any pending relay-only bind (so syncDirectServe cannot install a revoked
// binding later) and the share's resolver entry (so a raced authorization can
// never resolve content after revocation).
func (d *Daemon) revokeOrigin(code string) {
	ds := d.direct
	var server *direct.DirectServer
	if ds != nil {
		ds.mu.Lock()
		pair, ok := ds.origins[code]
		if ok {
			delete(ds.origins, code)
		}
		relayOrigin, relayOnlyBound := ds.relayOrigins[code]
		if relayOnlyBound {
			delete(ds.relayOrigins, code)
		}
		// A relay-only share whose binder did not exist yet has only a pending
		// bind recorded. Delete it here or syncDirectServe would install the
		// revoked binding as soon as the binder exists.
		delete(ds.pendingRelayBinds, code)
		binder := ds.binder
		server = ds.server
		if binder != nil {
			if ok {
				binder.RevokeShare(pair.directOrigin, pair.relayOrigin)
			}
			if relayOnlyBound {
				binder.Revoke(relayOrigin)
			}
		}
		ds.mu.Unlock()
	}
	// Resolver state is per-share content state and must not outlive the
	// binding, for revoke and expiry alike. It is deleted even when direct
	// transport is not configured, so the lifecycle cleanup stays consistent.
	if d.resolver != nil {
		d.resolver.Delete(code)
	}
	// T29 review Minor #1: an in-flight direct/relay stream on the revoked
	// share must not keep the on-demand port held until it drains. Closing the
	// share's connections tears them down through the normal ConnState path
	// (EndSession + hold release). This runs strictly after the Binder mutation
	// above, which is the ordering the handler revalidation proof relies on.
	if server != nil {
		server.CloseShareConns(code)
	}
}

// syncDirectServe (re)builds the binder and direct server once the namespace is
// known. It is idempotent for an unchanged namespace so reconnect does not drop
// live origin bindings.
//
// A rebuild is an ORDERED handoff (audit C1): the previous listener is drained
// through its own registry, cancelled, and waited on (bounded) for its socket
// to be released before the replacement binder/server is published. The wait
// is never performed while holding ds.mu, so the listener goroutine — which
// takes ds.mu to clear its start guard — can always exit. ds.syncMu serializes
// concurrent rebuilds so two handoffs cannot both publish a live server; the
// rebuild also raises ds.rebuildInProgress so a concurrent startDirectServer
// waits for the handoff instead of starting the server being drained. The
// rebuild never waits on an in-flight start: it cancels it and proceeds to its
// own bounded listener-exit wait (finding A1).
//
// It RETURNS the fail-closed handoff error instead of stashing it: the caller
// (Unlock, the signaling-loop enrolled/start paths, startup) must decide what
// to report, and must never report success while the listener could not be
// brought up. The ds.handoffErr latch is kept only as the start guard that
// makes startDirectServer refuse to bind behind the stuck listener.
func (d *Daemon) syncDirectServe() error {
	ds := d.direct
	if ds == nil {
		return nil
	}
	ds.syncMu.Lock()
	defer ds.syncMu.Unlock()

	ds.mu.Lock()
	if ds.namespace == "" {
		ds.mu.Unlock()
		return nil
	}
	if ds.binder != nil && ds.serveNS == ds.namespace {
		ds.mu.Unlock()
		return nil
	}
	// 1. Mark the old listener non-serving under ds.mu (no new start may latch
	// it), but KEEP ds.server pointing at the old object: its route-aware
	// registry must remain reachable to the T29 closers while it drains. The
	// rebuildInProgress flag is the single-flight gate startDirectServer
	// observes; it is cleared (deferred) once the replacement is published or
	// the handoff fails closed.
	oldServer := ds.server
	cancel := ds.cancel
	done := ds.listenerDone
	listenAddr := ds.listenAddr
	ds.started = false
	ds.startGen++
	ds.rebuildInProgress = true
	ds.flowSignalLocked()
	ds.mu.Unlock()
	defer func() {
		ds.mu.Lock()
		ds.rebuildInProgress = false
		ds.flowSignalLocked()
		ds.mu.Unlock()
	}()

	// 2. Drain the old server's active connections through its own registry,
	// then cancel/close its listener.
	if oldServer != nil {
		oldServer.CloseAllConns()
	}
	if cancel != nil {
		cancel()
	}
	// 3. Wait boundedly for the old listener goroutine to exit (release its
	// socket) before binding the replacement. If it does not exit in time,
	// fail closed: do NOT bind over a socket that may still be held, keep the
	// old listener tracked so a later rebuild (or shutdown) can still wait on /
	// tear down the same completion, and RETURN the error to the caller (the
	// latch below only makes the start guard refuse to bind).
	if done != nil {
		if !waitChanClosed(done, ds.listenerHandoffTimeout()) {
			err := fmt.Errorf("old listener at %q did not release its socket within %s", listenAddr, ds.listenerHandoffTimeout())
			log.Printf("direct server: listener handoff failed closed: %v; refusing to bind the replacement", err)
			ds.mu.Lock()
			ds.handoffErr = err
			ds.cancel = cancel
			ds.listenerDone = done
			// The listener goroutine has not returned, so the listener is still
			// (partially) serving; keep the start guard latched so no bind can
			// race it. A successful later handoff resets it before publishing.
			ds.started = true
			ds.mu.Unlock()
			return err
		}
	}

	ds.mu.Lock()
	defer ds.mu.Unlock()
	// 4. Single publication point: the old socket is provably released, so the
	// fresh state is published atomically. Clear the handoff bookkeeping so
	// startDirectServer may bind the replacement.
	ds.handoffErr = nil
	ds.cancel = nil
	ds.listenerDone = nil
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
	// §13.4: while locked the rebuilt binder gets NO admissions; the recorded
	// origins stay in ds.origins/relayOrigins/pendingRelayBinds and unlock
	// re-runs this function to restore them.
	if ds.locked {
		ds.serveNS = ds.namespace
		return nil
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
	return nil
}

// buildDirectState constructs the daemon's direct-transport state. When withNetwork
// is false (tests) the port mapper and on-demand port are left nil. It returns
// the initial listener rebuild's error so a startup that could not come into
// service is reported by New instead of silently running out of service. There
// is no prior listener at startup (ds.listenerDone is nil), so the ordered
// handoff cannot currently fail here; the propagation keeps that guarantee
// honest if a future startup ever inherits one.
func (d *Daemon) buildDirectState(withNetwork bool) error {
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
			ds.reporter = direct.NewReporter(func(ctx context.Context, ip string, port int, status string) error {
				return d.signaling.ReportEndpoint(ctx, ip, port, status)
			})
			ds.port.SetTransitionCallback(ds.reporter.OnTransition)
		}
	}

	return d.syncDirectServe()
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
	if err := d.buildDirectState(true); err != nil {
		return nil, fmt.Errorf("init direct transport: %w", err)
	}
	// The transport-level OK-ack seal is installed HERE, at construction, so
	// no daemon code path can emit a status-"ok" open_ack without it.
	if err := sig.SetOpenAckGuard(d.openAckGuard); err != nil {
		return nil, fmt.Errorf("install OK-ack guard: %w", err)
	}
	return d, nil
}

// openAckGuardInstaller is the optional seam a concrete signaling client
// exposes to receive the daemon's OK-ack guard. Mocks do not implement it.
type openAckGuardInstaller interface {
	SetOpenAckGuard(func(signaling.OpenAck) error) error
}

// NewWithSignaling creates a new Daemon with a custom signaling client.
// This is useful for testing or custom signaling implementations.
func NewWithSignaling(cfgMgr ConfigManagerInterface, st StoreInterface, sig SignalingClientInterface) (*Daemon, error) {
	cfg := cfgMgr.Get()

	d := &Daemon{
		config:    cfg,
		configMgr: cfgMgr,
		store:     st,
		signaling: sig,
		resolver:  direct.NewResolverRegistry(),
		sessions:  make(map[string]*Session),
	}
	// A real signaling client gets the same transport seal as production; a
	// mock (the test seam) does not implement the installer and is unchanged.
	if installer, ok := sig.(openAckGuardInstaller); ok {
		if err := installer.SetOpenAckGuard(d.openAckGuard); err != nil {
			return nil, fmt.Errorf("install OK-ack guard: %w", err)
		}
	}
	return d, nil
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
	// A start superseded by shutdown must not loop and re-bind the listener.
	ds.stopped = true
	ds.flowSignalLocked()
	server := ds.server
	ds.mu.Unlock()
	if server != nil {
		// Shutdown/lockdown: close every established connection explicitly so
		// no stream outlives the listener teardown. CloseAllConns is
		// idempotent with the srv.Close the cancelled ctx triggers below.
		server.CloseAllConns()
	}
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
//
// Teardown ordering (§7.4 step 6): the relay tunnel is stopped first, but the
// wait is BOUNDED and best-effort — an unkillable frpc child must never strand
// the remaining shutdown (audit I4). The tunnel stop runs on its own goroutine
// and Stop waits at most shutdownLeverTimeout for it; whether it completed or
// timed out, the HTTPS listener, established connections, signaling epoch, and
// web/admin surface are always torn down. The tunnel failure is returned to
// the caller (and logged), never swallowed.
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
	// then expires on its own. The manager bounds its own post-kill wait; the
	// outer timer below is an independent safety net in the spirit of T30's
	// bounded lockdown levers, so this lever can never delay the teardown that
	// follows it indefinitely. The result channel is buffered: a late manager
	// return after the timeout never blocks its goroutine.
	tunnelResult := make(chan error, 1)
	go func() { tunnelResult <- d.stopTunnelManager() }()

	timeout := d.shutdownLeverTimeout
	if timeout <= 0 {
		timeout = defaultShutdownLeverTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var tunnelErr error
	select {
	case tunnelErr = <-tunnelResult:
	case <-timer.C:
		tunnelErr = fmt.Errorf("tunnel stop did not complete within %s", timeout)
		log.Printf("daemon shutdown: %v; tearing down the remaining surfaces", tunnelErr)
	}

	// Tear down the direct serving path: the HTTPS listener leaves service
	// only on daemon shutdown (§7.4) — never on a control-WebSocket
	// disconnect — and the gate/readiness epoch state is reset for a clean
	// shutdown. This runs REGARDLESS of the tunnel lever's outcome.
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

	return tunnelErr
}

// CreateSession creates a new share session and registers it with the
// signaling server. Phase 4a serves direct and relay-only public Immich
// gallery shares (§13.3: the temporary relay-only rejection is removed).
// WebDAV/file (opencloud/nextcloud) shares remain rejected deferred share
// types.
//
// relayOnly is the FULLY RESOLVED per-share decision: an explicit per-share
// selection wins, and a caller with no per-share selection resolves it against
// the persisted DefaultRelayOnly setting before calling here (the web form and
// JSON API do exactly that). It is honored end to end — the session's mode,
// the T27 single relay binding, and the relay_only carried to control — rather
// than being dropped in favor of the global default (M4 closeout batch 4).
func (d *Daemon) CreateSession(ctx context.Context, shareURL, shareType, password string, expiryDuration time.Duration, maxDownloads int, relayOnly bool) (string, error) {
	switch shareType {
	case "immich":
		return d.createManualImmichSession(ctx, shareURL, expiryDuration, maxDownloads, relayOnly)
	case "opencloud", "nextcloud":
		return "", validationError{message: fmt.Sprintf("share type %q is not supported", shareType)}
	default:
		return "", validationError{message: fmt.Sprintf("share type %q is not supported", shareType)}
	}
}

func (d *Daemon) createManualImmichSession(ctx context.Context, shareURL string, expiryDuration time.Duration, maxDownloads int, relayOnly bool) (string, error) {
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

	session, err := d.registerImmichShare(ctx, link, maxDownloads, time.Now().Add(expiryDuration), relayOnly)
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
		// A direct-path rebuild failure must not be swallowed: report it while
		// the handler continues the §7.1 baseline enrollment work.
		if err := d.handleEnrolled(msg); err != nil {
			log.Printf("enrolled: %v", err)
		}

	case "cert_issue":
		d.handleCertIssue(msg)

	case "cert_error":
		d.handleCertError(msg)

	case "enrollment_ready":
		if err := d.handleEnrollmentReady(msg); err != nil {
			log.Printf("enrollment_ready: %v", err)
		}

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
// It returns the direct listener rebuild's fail-closed error, if any: §7.1
// baseline enrollment (TLS + relay DNS) is never gated on the optional direct
// path, so the certificate flow below still runs and the error is surfaced to
// the signaling loop instead of being dropped.
func (d *Daemon) handleEnrolled(msg signaling.Message) error {
	ds := d.direct
	if ds == nil || msg.Namespace == "" {
		return nil
	}

	ds.mu.Lock()
	nsChanged := ds.namespace != msg.Namespace
	ds.namespace = msg.Namespace
	ds.mu.Unlock()

	if err := ds.cert.SetNamespace(msg.Namespace); err != nil {
		log.Printf("persist namespace: %v", err)
	}
	directErr := d.syncDirectServe()

	if nsChanged || !ds.cert.Installed() || ds.cert.NeedsRenewal() {
		csr, err := ds.cert.GenerateCSR()
		if err != nil {
			log.Printf("generate CSR: %v", err)
			return directErr
		}
		if err := d.signaling.SubmitCSR(context.Background(), string(csr)); err != nil {
			log.Printf("submit CSR: %v", err)
		}
		return directErr
	}

	// Reconnect reconciliation: cert already installed and not expiring —
	// re-affirm tls_ready so the control marks the current epoch ready.
	d.sendTLSReady()
	d.learnAndReportPublicIP()
	return directErr
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
//
// It returns the listener start's fail-closed error, if any: baseline readiness
// (§7.1 TLS + relay DNS) is latched before the listener is attempted and is not
// rolled back, but the signaling loop and the tests must be able to see that the
// daemon did not come fully into service rather than reading a success log.
func (d *Daemon) handleEnrollmentReady(msg signaling.Message) error {
	ds := d.direct
	if ds == nil {
		return nil
	}
	ds.mu.Lock()
	ds.ready = true
	locked := ds.locked
	if ds.cond != nil {
		ds.cond.Broadcast()
	}
	ds.mu.Unlock()

	if locked {
		// §13.4: lockdown survives a control reconnect. Re-report the
		// advisory state for the new epoch and keep the listener/tunnel down;
		// explicit local unlock is the only way back.
		d.reportLockdownState()
		log.Printf("baseline enrollment ready (locked)")
		return nil
	}

	// Learn/report the public IP and start the direct HTTPS server. Both are
	// idempotent, re-run safely on every reconnect, and are no-ops when the
	// direct path is unavailable (e.g. no port mapper behind CGNAT).
	d.learnAndReportPublicIP()
	if err := d.startDirectServer(); err != nil {
		log.Printf("baseline enrollment ready (direct listener out of service): %v", err)
		return err
	}
	log.Printf("baseline enrollment ready")
	return nil
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
	if d.IsLocked() {
		return // §13.4: no direct endpoint work while locked
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
//
// It CONFIRMS the listener's bind before returning success (audit A1): the
// start goroutine reports the net.Listen result on a listen-result channel and
// this function waits (bounded) for it. A bind failure is returned to the
// caller, so Unlock and the signaling-loop handlers never report the daemon in
// service while the listener could not bind. A prior handoff that failed closed
// latches the start guard and is reported just the same. After a bind failure
// the start guard is reset (generation-checked) so a later retry can bind once
// the socket is free.
//
// The bind wait holds NO lock (finding A1): the start and the ordered rebuild
// coordinate through the single-flight rebuildInProgress/startInProgress state
// observed under ds.mu, so a rebuild can cancel an in-flight start and proceed
// to its own bounded listener-exit wait instead of serializing behind the bind
// wait. A concurrent start waits for the in-flight one and re-evaluates, so two
// starts never both bind; a start superseded by a rebuild loops and starts the
// server the rebuild published (or observes the rebuild's fail-closed latch).
func (d *Daemon) startDirectServer() error {
	ds := d.direct
	if ds == nil {
		return nil
	}
	for {
		ds.mu.Lock()
		// A rebuild's transition window leaves ds.server pointing at the server
		// being drained; starting it would race the socket the handoff is
		// releasing. The wait releases ds.mu (a rebuild takes it to publish) and
		// holds no other lock, so it can never block the rebuild's own bounded
		// listener-exit wait.
		for ds.rebuildInProgress || ds.startInProgress {
			ch := ds.flowWaitLocked()
			ds.mu.Unlock()
			<-ch
			ds.mu.Lock()
		}
		// Fail closed FIRST: a prior handoff could not prove the old socket was
		// released, and the guard it latched (ds.started) describes the old
		// listener that may still be serving, so the ordinary not-started checks
		// must not turn this into a silent no-op success. Binding here would race
		// that socket; a successful rebuild clears the error first.
		if ds.handoffErr != nil {
			err := ds.handoffErr
			ds.mu.Unlock()
			log.Printf("direct server: refusing to start while the listener handoff is fail-closed: %v", err)
			return err
		}
		if ds.server == nil || ds.locked || ds.started || ds.stopped {
			ds.mu.Unlock()
			return nil
		}
		server := ds.server
		startFn := ds.startListenerFn
		listenTimeout := ds.listenStartTimeoutResolved()
		ds.started = true
		ds.startInProgress = true
		ds.startGen++
		gen := ds.startGen
		ctx, cancel := context.WithCancel(context.Background())
		ds.cancel = cancel
		done := make(chan struct{})
		ds.listenerDone = done
		listenAddr := ds.listenAddr
		ds.mu.Unlock()
		if listenAddr == "" {
			listenAddr = fmt.Sprintf(":%d", directIntPort)
		}

		// listenResult carries the bind outcome exactly once; the buffered channel
		// keeps the reporting goroutine from blocking if the caller already gave up
		// on the bound. boundOK records whether that first report was a successful
		// bind, so the goroutine can reset the guard generation-checked when the
		// serve loop fails AFTER a confirmed bind (base behaviour).
		listenResult := make(chan error, 1)
		var reportReady sync.Once
		boundOK := false
		ready := func(err error) {
			reportReady.Do(func() {
				boundOK = err == nil
				listenResult <- err
			})
		}

		go func() {
			// Closing done (after Start has returned and the socket is released)
			// is what a listener rebuild waits on before binding the replacement.
			defer close(done)
			start := func(ctx context.Context, addr string, r func(error)) error {
				return server.StartWithReady(ctx, addr, r)
			}
			if startFn != nil {
				start = func(ctx context.Context, addr string, r func(error)) error {
					return startFn(ctx, server, addr, r)
				}
			}
			err := start(ctx, listenAddr, ready)
			// A start seam that returned without reporting the bind must not leave
			// the caller waiting: report whatever it returned (nil only if it bound).
			ready(err)
			if err != nil {
				log.Printf("direct server: %v", err)
				cancel() // release the epoch ctx
			}
			// A serve loop that failed after a confirmed bind leaves the outer wait
			// already returned; reset the start guard (generation-checked, so a
			// stale failure cannot clobber a newer attempt) so a later start can
			// retry, exactly as before. A failed bind is rolled back by the outer
			// wait instead, which owns clearing startInProgress.
			if err != nil && boundOK {
				ds.mu.Lock()
				if ds.startGen == gen {
					ds.cancel = nil
					ds.started = false
					ds.listenerDone = nil
				}
				ds.mu.Unlock()
			}
		}()

		// Wait (holding no lock) for the bind confirmation. The listener goroutine
		// runs net.Listen synchronously, so this is a scheduling bound, not a
		// network one; a miss is a failed start (fail closed).
		var err error
		timer := time.NewTimer(listenTimeout)
		select {
		case err = <-listenResult:
		case <-timer.C:
			err = fmt.Errorf("direct server: listener bind did not confirm within %s", listenTimeout)
		}
		timer.Stop()

		ds.mu.Lock()
		// A rebuild (or shutdown) that took over while we waited bumped the
		// generation. Do NOT clobber its state — the server this attempt was
		// binding was discarded — and re-evaluate instead: the replacement the
		// rebuild published still needs starting.
		superseded := ds.startGen != gen
		ds.startInProgress = false
		if !superseded && err != nil {
			// Generation-checked rollback: a stale failure must not clobber a
			// newer start attempt (or a disconnect that already reset the epoch).
			cancel()
			ds.cancel = nil
			ds.started = false
			ds.listenerDone = nil
		}
		ds.flowSignalLocked()
		ds.mu.Unlock()
		if superseded {
			continue
		}
		return err
	}
}

// listenerHandoffTimeout resolves the bounded wait for the previous listener's
// exit, honoring the test override. Callers read it without ds.mu (it is set
// before any listener starts).
func (ds *directState) listenerHandoffTimeout() time.Duration {
	if ds.handoffTimeout > 0 {
		return ds.handoffTimeout
	}
	return defaultDirectHandoffTimeout
}

// listenStartTimeoutResolved resolves the bounded wait for a listener bind
// confirmation, honoring the test override. Like listenerHandoffTimeout it is
// read without ds.mu (it is set before any listener starts).
func (ds *directState) listenStartTimeoutResolved() time.Duration {
	if ds.listenStartTimeout > 0 {
		return ds.listenStartTimeout
	}
	return defaultDirectListenStartTimeout
}

// openReportTimeout resolves the bounded wait for the open path's endpoint
// report confirmation, honoring the test override. Like listenerHandoffTimeout
// it is read without ds.mu (it is set before any open-signal runs).
func (ds *directState) openReportTimeout() time.Duration {
	if ds.reportTimeout > 0 {
		return ds.reportTimeout
	}
	return defaultOpenReportTimeout
}

// waitChanClosed reports whether ch was closed within bound. It is the
// non-blocking-safe bounded join the listener handoff uses; the caller must
// not hold ds.mu while waiting (the listener goroutine needs it to exit).
func waitChanClosed(ch <-chan struct{}, bound time.Duration) bool {
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-timer.C:
		return false
	}
}

// flowWaitLocked returns the current single-flight wake-up channel, lazily
// creating it. The caller holds ds.mu, releases it, and waits on the returned
// channel; flowSignalLocked closes and replaces the channel on any change to
// rebuildInProgress/startInProgress. Caller must hold ds.mu.
func (ds *directState) flowWaitLocked() chan struct{} {
	if ds.flowCh == nil {
		ds.flowCh = make(chan struct{})
	}
	return ds.flowCh
}

// flowSignalLocked wakes every startDirectServer waiter. Caller must hold ds.mu.
func (ds *directState) flowSignalLocked() {
	if ds.flowCh != nil {
		close(ds.flowCh)
		ds.flowCh = nil
	}
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
	// Capture the direct-state generation this open is admitted under BEFORE the
	// slow ExternalIP lookup. A §13.4 lockdown (or lockdown+unlock) that advances
	// the generation while we wait must fence this open: it may not create or
	// renew a mapping after lockdown's CloseIf already ran. The OnDemandPort
	// publishes that generation as a stamp the state loop re-checks immediately
	// before AND immediately after every mapping write (see OpenForIf), so this
	// capture is what makes the commit-time re-check decisive.
	openEpoch, openCurrent := d.snapshotDirectOpenGeneration()
	if !openCurrent {
		log.Printf("open_signal %s refused: direct path is locked", msg.ShareID)
		_ = d.signaling.OpenAck(context.Background(), signaling.OpenAck{
			ShareID: msg.ShareID, Nonce: msg.Nonce, Seq: msg.Seq, Status: "error", Error: "locked"})
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
	if err := ds.port.OpenForIf(msg.ShareID, sig.Lease, openEpoch); err != nil {
		if errors.Is(err, direct.ErrOpenSuperseded) {
			log.Printf("open_signal %s refused: superseded by a lockdown transition", msg.ShareID)
			_ = d.signaling.OpenAck(context.Background(), signaling.OpenAck{
				ShareID: msg.ShareID, Nonce: msg.Nonce, Seq: msg.Seq, Status: "error", Error: "superseded"})
			return
		}
		_ = d.signaling.OpenAck(context.Background(), signaling.OpenAck{
			ShareID: msg.ShareID, Nonce: msg.Nonce, Seq: msg.Seq, Status: "error", Error: "open_failed"})
		return
	}
	// The mapping now exists, but the control may not advertise direct
	// reachability until it has the ACTUAL mapped external port: the endpoint
	// report provisions the per-agent DDNS wildcard the recipient's direct URL
	// resolves through (audit Important #3). Report it synchronously and
	// CONFIRM delivery inside a bounded context BEFORE the OK open_ack. The
	// report is sent from the reporter's drain goroutine, so no port lock,
	// ackMu, or router I/O is held while we wait. A failed, dropped, or
	// unconfirmed report is an unsuccessful open: tear the mapping back down
	// (targeting the created mapping's identity, so no half-open or foreign
	// mapping is touched) and answer with an error ack instead of advertising
	// an unreported endpoint.
	//
	// A §13.4 lockdown may publish its generation and close the mapping at any
	// point between OpenForIf returning and the ack — in particular during the
	// bounded report confirmation below. The generation is therefore re-checked
	// TWICE: here, before the report is issued (so no DDNS record is provisioned
	// for an already-superseded open), and again inside the sole OK-ack writer
	// ackOpenSuccess, AFTER the report has been confirmed. The post-report check
	// is the load-bearing one: a superseded open must not be acked OK merely
	// because its report (which may even have provisioned DDNS) completed.
	preCommit, current := ds.port.CommitOpenAck(openEpoch)
	if !current {
		log.Printf("open_signal %s refused before reporting: superseded by a lockdown transition", msg.ShareID)
		_ = d.signaling.OpenAck(context.Background(), signaling.OpenAck{
			ShareID: msg.ShareID, Nonce: msg.Nonce, Seq: msg.Seq, Status: "error", Error: "superseded"})
		return
	}
	if reportErr := d.confirmOpenEndpoint(ds, preCommit.GrantedPort()); reportErr != nil {
		log.Printf("open_signal %s: endpoint report before open_ack failed: %v", msg.ShareID, reportErr)
		if closeErr := ds.port.DiscardOpenMapping(openEpoch); closeErr != nil {
			// The mapping delete is retried by the port's own close timer and
			// escalated via CloseError; the open is already unsuccessful.
			log.Printf("open_signal %s: mapping teardown after report failure: %v", msg.ShareID, closeErr)
		}
		_ = d.signaling.OpenAck(context.Background(), signaling.OpenAck{
			ShareID: msg.ShareID, Nonce: msg.Nonce, Seq: msg.Seq, Status: "error", Error: "open_failed"})
		return
	}
	if !d.ackOpenSuccess(msg, ds, openEpoch, ip, wasOpen) {
		log.Printf("open_signal %s refused after reporting: not the confirmed current open (superseded or OK-ack refused)", msg.ShareID)
		_ = d.signaling.OpenAck(context.Background(), signaling.OpenAck{
			ShareID: msg.ShareID, Nonce: msg.Nonce, Seq: msg.Seq, Status: "error", Error: "superseded"})
		return
	}
}

// ackOpenSuccess is the ONE code path in the daemon that emits a successful
// (status "ok") open_ack, and it re-validates the open's §13.4 generation
// INSIDE itself, immediately before it constructs the ack.
//
// It calls OnDemandPort.CommitOpenAck, which re-reads the published generation
// stamp under the same mutex SetGeneration publishes under, refuses and discards
// the created mapping once a lockdown has superseded gen (the
// discovered-identity discard, so no half-open mapping survives), and hands back
// the commit token the ack's granted port is read from. The token is produced
// HERE rather than accepted as a parameter, so no caller can validate early and
// then ack after the endpoint-report wait — the A1 bypass. It returns false on
// refusal, and the caller emits the error ack.
//
// The transport is a SECOND, unreachable-around gate: the OK ack carries the
// generation it was validated under (signaling.OpenAckValidation) and
// signaling.Client.OpenAck consults Daemon.openAckGuard before writing. Any
// other caller anywhere in the process that constructs a status-"ok" open_ack
// cannot put it on the wire without that guard approving the same
// CommitOpenAck re-validation.
//
// Do NOT add another OK open_ack anywhere: the structural guard
// TestOpenAckOKOnlyInsideTheValidatedChokePoint parses the whole production
// agent module and fails if a second status-"ok" signaling.OpenAck
// construction (literal or assignment) appears outside this function, or if
// this function stops calling CommitOpenAck (see daemon_open_ack_choke_test.go).
//
// This function's granted-port source must stay the commit token: the structural
// guard also pins that.
func (d *Daemon) ackOpenSuccess(msg signaling.Message, ds *directState, gen uint64, ip string, wasOpen bool) bool {
	commit, current := ds.port.CommitOpenAck(gen)
	if !current {
		return false
	}
	if err := d.signaling.OpenAck(context.Background(), signaling.OpenAck{
		ShareID: msg.ShareID, Nonce: msg.Nonce, Seq: msg.Seq,
		GrantedPort: commit.GrantedPort(), PublicIP: ip,
		WasAlreadyOpen: wasOpen, Status: "ok",
		Validation: &signaling.OpenAckValidation{Epoch: gen},
	}); err != nil {
		// The OK ack never reached control, so the open is unsuccessful and its
		// mapping must not outlive the undelivered ack (remediation finding 2):
		// a WebSocket loss between the endpoint report and the ack previously
		// left the mapping live for its whole lease. Tear it down through the
		// same generation-guarded, discovered-identity discard the
		// report-failure arm uses. This is a no-op when the transport guard
		// already discarded a superseded generation, and it never touches a
		// newer generation's mapping.
		if closeErr := ds.port.DiscardOpenMapping(gen); closeErr != nil {
			log.Printf("open_signal %s: mapping teardown after an undelivered OK open_ack: %v", msg.ShareID, closeErr)
		}
		return false
	}
	return true
}

// openAckGuard is the transport-level approval gate for status-"ok"
// open_acks, installed on the signaling client at construction (see New).
// signaling.Client.OpenAck consults it before writing anything, so there is no
// path — inside ackOpenSuccess or anywhere else in the process — that can put
// an OK ack on the wire without this validation.
//
// It re-runs the post-report generation re-validation (OnDemandPort.
// CommitOpenAck) for the generation the ack says the open was admitted under,
// and additionally requires the ack's advertised port to be the one the port
// re-confirmed. It is deliberately stateless: the OK ack carries its own
// validation context and the port is the authority, so a caller that supplies
// a currently-authorized generation has performed the same check the choke
// point performs. The OK-ack writer still calls CommitOpenAck itself to obtain
// the granted port, so this is the second, unreachable-around gate.
func (d *Daemon) openAckGuard(ack signaling.OpenAck) error {
	if ack.Validation == nil {
		return errors.New("open_ack carries no validation context")
	}
	ds := d.direct
	if ds == nil || ds.port == nil {
		return errors.New("direct transport is unavailable")
	}
	commit, current := ds.port.CommitOpenAck(ack.Validation.Epoch)
	if !current {
		return fmt.Errorf("generation %d is no longer the current committed open", ack.Validation.Epoch)
	}
	if got := commit.GrantedPort(); got != ack.GrantedPort {
		return fmt.Errorf("granted port %d does not match the committed port %d", ack.GrantedPort, got)
	}
	return nil
}

// confirmOpenEndpoint sends the open-path endpoint report carrying the
// router-granted external port and blocks until its delivery is confirmed or
// the bounded open-report window elapses. It must be called only after
// OnDemandPort.OpenForIf has returned: the wait holds no ds.mu, ackMu, or port
// state-loop lock, and performs no router I/O on the caller's goroutine (the
// Reporter's drain performs the send). A nil reporter, a full reporter queue, a
// failed send, or a missed deadline is returned as an error, and the caller
// treats the open as unsuccessful.
func (d *Daemon) confirmOpenEndpoint(ds *directState, grantedPort int) error {
	if ds.reporter == nil {
		return errors.New("direct: no endpoint reporter wired to confirm the open report")
	}
	ctx, cancel := context.WithTimeout(context.Background(), ds.openReportTimeout())
	defer cancel()
	return ds.reporter.ReportOpen(ctx, grantedPort)
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

		session, err := d.registerImmichShare(ctx, link, d.GetConfig().DefaultMaxDownloads, time.Time{}, d.GetConfig().DefaultRelayOnly)
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

// registerImmichShare registers one public Immich share with control, binding
// the T26 dual-origin pair for a normal share or the T27 relay-only single
// binding for a relay-only one. relayOnly is the already-resolved per-share
// decision supplied by the caller (CreateSession carries an explicit per-share
// selection; the poller passes the persisted DefaultRelayOnly default).
func (d *Daemon) registerImmichShare(ctx context.Context, link immich.SharedLink, maxDownloads int, expiresAt time.Time, relayOnly bool) (*Session, error) {
	reg, ok := d.signaling.(shareOptionRegistrar)
	if !ok {
		return nil, fmt.Errorf("signaling client does not support option registration")
	}

	shareURL := "immich://" + link.Key
	passwordProtected := link.IsPasswordProtected()

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
	// rollback undoes everything this registration installed after the
	// control-side registration succeeded: the local Binder admission, the
	// daemon bookkeeping (origins/relayOrigins/pendingRelayBinds), any resolver
	// state, and finally the control registration itself. Without it a failure
	// creating the Immich client or saving the session would leave a locally
	// admitted, signal-authorized half-binding behind (audit Important #2).
	rollback := func() {
		d.revokeOrigin(code)
		_ = d.unregisterShare(ctx, code)
	}
	client, err := d.newImmichClient(code)
	if err != nil {
		rollback()
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
		rollback()
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

// ---------------------------------------------------------------------------
// §13.4 reversible lockdown v2 (plan Task 30)
// ---------------------------------------------------------------------------

// IsLocked reports the local §13.4 lockdown state. It is the agent-side
// authority the advisory control-plane status can never override.
func (d *Daemon) IsLocked() bool {
	ds := d.direct
	if ds == nil {
		return false
	}
	ds.mu.Lock()
	defer ds.mu.Unlock()
	return ds.locked
}

// nextLockdownGeneration returns the next monotonic §11.1 lockdown_status
// generation. Control rejects a report whose generation is older than the one
// it already holds for the current connection epoch.
func (d *Daemon) nextLockdownGeneration() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lockdownGen++
	return int(d.lockdownGen)
}

// lockdownLever is one best-effort §13.4 action executed during Lockdown. The
// name is used only for per-lever diagnostics; a lever's error is logged and
// never aborts the others.
type lockdownLever struct {
	name string
	run  func() error
}

// Lockdown activates the reversible §13.4 emergency stop. The local locked
// flag and SignalGate are set FIRST and are final; the remaining levers then
// fan out independently and concurrently with a bounded wait, so no stalled
// lever (blocking router delete, frpc kill grace) can prevent another lever
// from running or make Lockdown hang. Every lever is additionally fenced to
// the transition that created it (see lockdownEpoch), so a lever that outlives
// the wait cannot mutate state an Unlock has since restored. Nothing here
// tombstones a share, so Unlock can restore availability. It is idempotent.
func (d *Daemon) Lockdown() error {
	d.lockdownMu.Lock()
	defer d.lockdownMu.Unlock()

	ds := d.direct
	if ds == nil {
		return nil
	}

	ds.mu.Lock()
	if ds.locked {
		ds.mu.Unlock()
		return nil
	}
	// 1. SignalGate lockdown: new direct opens fail. This is the local
	// enforcement and it is final — set before any best-effort lever runs so
	// a stalled lever can never leave the agent admitting traffic.
	ds.locked = true
	// Every lever below belongs to THIS transition. The epoch is bumped under
	// the same lock the Unlock transition mutates under, so a lever that
	// overruns the aggregation bound can be recognised as superseded and
	// refused by the fences below.
	ds.lockdownEpoch++
	epoch := ds.lockdownEpoch
	gate := ds.gate
	port := ds.port
	server := ds.server
	binder := ds.binder
	ds.mu.Unlock()

	// Publish the transition to the on-demand port BEFORE any lever runs (and
	// therefore before the mapping-close lever enqueues its CloseIf). The stamp
	// is the port's single serialization point for this transition: every
	// open/renewal, including one already inside the state loop, re-reads it
	// after its mapping write and refuses when it no longer matches. Publishing
	// here — before the gate closes and before the close is queued — is what
	// makes an open ordered after this fence command provably unable to create
	// or renew a mapping or ack it OK.
	if port != nil {
		port.SetGeneration(epoch, true)
	}
	if gate != nil {
		gate.SetLockdown(true)
	}

	// Snapshot the tunnel manager so a late stop lever can only stop the
	// pre-lockdown manager, never a fresh one built by Unlock.
	d.mu.RLock()
	manager := d.tunnel
	d.mu.RUnlock()

	// 6. Report the advisory locked state; the generation is assigned here so
	// a delayed report still loses to a later Unlock's higher generation.
	generation := d.nextLockdownGeneration()

	levers := []lockdownLever{
		// 2. Remove the UPnP/NAT-PMP mapping. The fence is evaluated on the
		// port's own state loop, atomically with OpenFor, so a lever that
		// overruns past an Unlock cannot close a mapping the newer generation
		// opened. The router I/O behind this call is bounded (RouterIOTimeout),
		// so the state loop — and therefore a post-unlock OpenFor — always
		// unblocks.
		{name: "close on-demand mapping", run: func() error {
			if port == nil {
				return nil
			}
			return port.CloseIf(func() bool { return d.lockdownEpochCurrent(epoch) })
		}},
		// 4. Withdraw the local Binder admissions, retaining the
		// source/session state for unlock. The lever acts on the binder this
		// generation owns: Unlock forces syncDirectServe to rebuild the binder,
		// so a superseded lever can only revoke a discarded binder.
		{name: "withdraw binder admissions", run: func() error {
			d.withdrawBinderAdmissions(binder)
			return nil
		}},
		// 5. Close established direct AND relay recipient connections on the
		// server this generation owns (Unlock builds a fresh DirectServer).
		{name: "close recipient connections", run: func() error {
			if server != nil {
				server.CloseRecipientConns()
			}
			return nil
		}},
		// §7.4: the local HTTPS listener leaves service on lockdown.
		{name: "stop direct listener", run: func() error {
			d.stopDirectServerForLockdown(epoch, server)
			return nil
		}},
		// 3. Stop frpc; gateway presence then expires on its own. The lever acts
		// on the snapshotted manager, never a manager Unlock built. The
		// manager's Stop is itself bounded, so this lever cannot outlive the
		// aggregation bound waiting on an unkillable child; its explicit error
		// is logged by the lever runner like any other best-effort failure.
		{name: "stop tunnel manager", run: func() error {
			if manager == nil {
				return nil
			}
			return manager.Stop()
		}},
		{name: "report lockdown status", run: func() error {
			d.sendLockdownStatus(generation, true)
			return nil
		}},
	}
	d.runLockdownLevers(levers)
	return nil
}

// lockdownEpochCurrent reports whether the given lockdown transition epoch
// still owns the daemon's direct state. It is safe to call from the
// OnDemandPort state loop: no goroutine holding ds.mu ever calls into that
// loop (syncDirectServe only stores the port pointer; start/stop take ds.mu
// after any port call has returned), so the lock cannot form a cycle.
func (d *Daemon) lockdownEpochCurrent(epoch uint64) bool {
	ds := d.direct
	if ds == nil {
		return false
	}
	ds.mu.Lock()
	defer ds.mu.Unlock()
	return ds.lockdownEpoch == epoch
}

// snapshotDirectOpenGeneration captures the direct-state generation an
// admitted open signal belongs to. It is taken BEFORE the slow ExternalIP
// work so an open that waits there can be refused once lockdown supersedes it.
// ok is false when the daemon is already locked at admission: there is no
// generation to observe, so the open must fail closed without any mapping work.
// The returned epoch is handed to OnDemandPort.OpenForIf, which re-checks it
// against the port's own published stamp before and after every mapping write.
func (d *Daemon) snapshotDirectOpenGeneration() (epoch uint64, ok bool) {
	ds := d.direct
	if ds == nil {
		return 0, false
	}
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.locked {
		return ds.lockdownEpoch, false
	}
	return ds.lockdownEpoch, true
}

// runLockdownLevers starts every best-effort lever concurrently and waits at
// most lockdownLeverTimeout (default defaultLockdownLeverTimeout) for them.
//
// Every lever is bounded by construction — the mapping close by
// direct.RouterIOTimeout, and the rest by in-process work or their own bounded
// waits — so a lever outliving the aggregation bound is a defensive
// safety-net case rather than the expected path. The completion signal is
// closed by the LAST lever rather than by a detached wg.Wait wrapper, so no
// helper goroutine is ever left waiting on the levers; a lever that does
// outrun the bound still exits when its work completes, and the transition
// fences in Lockdown make its late completion harmless to the current
// generation (see the levers' comments). Per-lever errors are logged with the
// lever name and never abort the others.
func (d *Daemon) runLockdownLevers(levers []lockdownLever) {
	if len(levers) == 0 {
		return
	}
	remaining := int64(len(levers))
	done := make(chan struct{})
	for _, lever := range levers {
		go func(l lockdownLever) {
			// The deferred recover is scoped to the lever's own work so the
			// completion count is always decremented, panic or not.
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("lockdown lever %q panicked: %v", l.name, r)
					}
				}()
				if hook := d.lockdownLeverGate; hook != nil {
					hook(l.name)
				}
				if err := l.run(); err != nil {
					log.Printf("lockdown lever %q: %v", l.name, err)
				}
			}()
			if atomic.AddInt64(&remaining, -1) == 0 {
				close(done)
			}
		}(lever)
	}

	timeout := d.lockdownLeverTimeout
	if timeout <= 0 {
		timeout = defaultLockdownLeverTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		log.Printf("lockdown: best-effort levers still running after %s; local enforcement is final", timeout)
	}
}

// Unlock reverses lockdown through explicit local action: it restores the
// retained source-verified Binder bindings, brings the listener back, rebuilds
// the tunnel manager and requests a FRESH credential (relay_credential_request
// reason restart) — never reusing the pre-lockdown one-use credential. It is
// idempotent.
//
// Failure semantics: if the ordered listener handoff fails closed (the old
// listener did not release its socket within the bound), Unlock returns that
// error to its caller (the admin API turns it into a non-2xx response) and
// emits NONE of the unlocked-success lifecycle actions — no tunnel restart, no
// credential request, no unlocked status report — so the daemon stays out of
// service (the latched start guard keeps the replacement from binding and the
// binder keeps its withdrawn admissions). Direct-open admission stays FENCED
// (locked port stamp + locked SignalGate) until the ordered handoff and the
// listener start both succeed, so a failed unlock cannot be followed by an
// open that creates a router mapping and acks OK. ds.restorePending records the
// owed relay restore, so a later Unlock retries it instead of short-circuiting
// as an idempotent no-op and falsely reporting success.
func (d *Daemon) Unlock() error {
	d.lockdownMu.Lock()
	defer d.lockdownMu.Unlock()
	ds := d.direct
	if ds == nil {
		d.sendLockdownStatus(d.nextLockdownGeneration(), false)
		return nil
	}
	ds.mu.Lock()
	// Idempotent no-op only when the daemon is genuinely unlocked AND no
	// restore is owed: a prior failed unlock must not be reported as success.
	if !ds.locked && !ds.restorePending {
		ds.mu.Unlock()
		return nil
	}
	wasLocked := ds.locked
	if wasLocked {
		ds.locked = false
		// Supersede every in-flight lockdown lever BEFORE the gate re-opens: a
		// post-unlock open must never be reachable by a stale close, and a stale
		// listener stop must not tear down the listener restored below. Bumping
		// under ds.mu makes the bump atomic with the fenced levers' checks.
		ds.lockdownEpoch++
		// Force syncDirectServe to rebuild the Binder/DirectServer and re-admit
		// every recorded origin (direct pairs, relay-only bindings, and pending
		// relay-only bindings), reusing the tested namespace-rebuild path. The
		// rebuild is required because lockdown withdrew the current binder's
		// admissions. A retry (restorePending) reuses a binder that is already
		// current; the owed relay lifecycle below is what must still run.
		ds.serveNS = ""
	}
	epoch := ds.lockdownEpoch
	gate := ds.gate
	port := ds.port
	ds.mu.Unlock()

	// M4 closeout batch 2: an owned router mapping whose deletion is
	// unresolved (close-failed) still forwards from the router to the
	// listener, so it is a live direct path even though the port is not
	// logically open. Unlock must not claim success — and must not perform any
	// unlocked-success lifecycle action — while that mapping lingers: the
	// direct content gate already fails closed, but a lingering owned mapping
	// is a service-integrity failure that must be surfaced, not hidden behind a
	// locked gate. CloseError is the port's record of an unresolved delete (set
	// by every failed delete, cleared only by a successful one), and the port's
	// ordinary Close RECLAIMS a close-failed mapping rather than merely
	// observing it — reclaiming is what lets a retried Unlock recover once the
	// router accepts the delete (its mapping lease expires or the router
	// recovers). The call may wait at most one bounded router round trip
	// (direct.RouterIOTimeout) behind an in-flight retry, holds no daemon lock,
	// and is never reached by a wedged first delete (whose CloseError is still
	// nil); the failure arm re-fences and reports, and no unlocked-success
	// lifecycle action runs. Recovery needs no operator action: the port's own
	// bounded retry schedule clears the mapping, and a retried Unlock then
	// completes.
	if port != nil && port.CloseError() != nil {
		if closeErr := port.Close(); closeErr != nil {
			ds.mu.Lock()
			ds.restorePending = true
			ds.mu.Unlock()
			fenceDirectAdmission(port, gate, epoch)
			log.Printf("unlock: refusing to restore direct service while an owned router mapping is unresolved: %v", closeErr)
			return fmt.Errorf("unlock: owned direct mapping unresolved: %w", closeErr)
		}
	}

	// Direct-open eligibility stays FENCED until the replacement listener is
	// confirmed serving (remediation finding 1). Publishing the unlocked
	// generation and opening the SignalGate BEFORE the ordered handoff meant a
	// failed handoff left admission OPEN: an open signal could then create a
	// router mapping, report it, and ack OK while the daemon claimed to be out
	// of service. Publishing the CURRENT epoch with the locked bit keeps the
	// port's stamp both current and observably locked across the handoff, and
	// the gate stays locked in lockstep; a retried unlock re-fences idempotently.
	fenceDirectAdmission(port, gate, epoch)

	if err := d.syncDirectServe(); err != nil {
		ds.mu.Lock()
		ds.restorePending = true
		ds.mu.Unlock()
		// Restore the locked stamp explicitly: the handoff failed, so admission
		// must stay closed however the failed path left the fence.
		fenceDirectAdmission(port, gate, epoch)
		log.Printf("unlock: direct listener handoff failed closed; the daemon stays out of service: %v", err)
		return fmt.Errorf("unlock: %w", err)
	}
	if err := d.startDirectServer(); err != nil {
		ds.mu.Lock()
		ds.restorePending = true
		ds.mu.Unlock()
		fenceDirectAdmission(port, gate, epoch)
		log.Printf("unlock: direct listener did not start; the daemon stays out of service: %v", err)
		return fmt.Errorf("unlock: %w", err)
	}
	// The ordered handoff completed and the replacement listener was brought
	// into service: re-open direct admission, publishing the unlocked port
	// generation BEFORE the gate so no open admitted after the gate opens is
	// fenced against a stale locked stamp.
	openDirectAdmission(port, gate, epoch)
	d.learnAndReportPublicIP()
	d.restartTunnelManager()
	d.sendLockdownStatus(d.nextLockdownGeneration(), false)
	d.requestRelayCredential()
	ds.mu.Lock()
	ds.restorePending = false
	ds.mu.Unlock()
	return nil
}

// fenceDirectAdmission keeps direct-open admission closed for the given epoch:
// the port's published generation stamp keeps its locked bit (so the port's
// OpenForIf/CommitOpenAck fences refuse an open admitted under epoch) and the
// SignalGate refuses every open signal. Unlock calls it while a listener
// handoff is unconfirmed, and restores it explicitly when the handoff fails
// closed. Both dependencies may be nil (a no-mapper agent), in which case the
// open path fails closed on its own nil checks.
func fenceDirectAdmission(port *direct.OnDemandPort, gate *direct.SignalGate, epoch uint64) {
	if port != nil {
		port.SetGeneration(epoch, true)
	}
	if gate != nil {
		gate.SetLockdown(true)
	}
}

// openDirectAdmission publishes the unlocked generation to the port BEFORE it
// opens the SignalGate, so no open admitted after the gate opens is fenced
// against a stale locked stamp, and there is no window where the port stamp is
// unlocked while the gate still refuses.
func openDirectAdmission(port *direct.OnDemandPort, gate *direct.SignalGate, epoch uint64) {
	if port != nil {
		port.SetGeneration(epoch, false)
	}
	if gate != nil {
		gate.SetLockdown(false)
	}
}

// withdrawBinderAdmissions removes every admission from the binder the
// lockdown generation owns, without deleting the recorded source/session state
// (§13.4 step 4). The binder is the one snapshotted at Lockdown time, not the
// live ds.binder: Unlock forces syncDirectServe to rebuild the binder, so a
// lever that overruns past an Unlock revokes admissions on the discarded
// binder and provably cannot clear the restored one.
func (d *Daemon) withdrawBinderAdmissions(binder *direct.Binder) {
	ds := d.direct
	if ds == nil {
		return
	}
	ds.mu.Lock()
	pairs := make([]originPair, 0, len(ds.origins))
	for _, pair := range ds.origins {
		pairs = append(pairs, pair)
	}
	relayOrigins := make([]string, 0, len(ds.relayOrigins))
	for _, relayOrigin := range ds.relayOrigins {
		relayOrigins = append(relayOrigins, relayOrigin)
	}
	ds.mu.Unlock()
	if binder == nil {
		return
	}
	for _, pair := range pairs {
		binder.RevokeShare(pair.directOrigin, pair.relayOrigin)
	}
	for _, relayOrigin := range relayOrigins {
		binder.Revoke(relayOrigin)
	}
}

// stopDirectServerForLockdown is the lockdown listener lever. It always closes
// the connections of the server the lockdown generation owns — that server
// object is replaced by Unlock's syncDirectServe rebuild, so closing its
// connections can never touch a newer one — and it stops the listener only
// while epoch still owns the daemon's direct state. The check and the cancel
// state mutation happen under the same ds.mu that the unlock transition
// mutates under, so a lever that overruns past an Unlock cannot tear down the
// listener Unlock just restored.
func (d *Daemon) stopDirectServerForLockdown(epoch uint64, server *direct.DirectServer) {
	if server != nil {
		server.CloseAllConns()
	}
	ds := d.direct
	if ds == nil {
		return
	}
	ds.mu.Lock()
	if ds.lockdownEpoch != epoch {
		ds.mu.Unlock()
		log.Printf("lockdown lever %q superseded by unlock; leaving the current listener in service", "stop direct listener")
		return
	}
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

// restartTunnelManager builds a FRESH tunnel manager after lockdown stopped
// the previous one. Manager.Stop is permanent (its stop/done channels close),
// so the daemon clears d.tunnel and reconstructs with the recorded
// construction inputs (the process context and any test options).
func (d *Daemon) restartTunnelManager() {
	d.mu.Lock()
	ctx := d.tunnelCtx
	options := append([]tunnel.ManagerOption(nil), d.tunnelOptions...)
	d.tunnel = nil
	d.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	d.startTunnelManager(ctx, options...)
}

// requestRelayCredential asks control for a fresh relay admission credential
// after unlock (§13.4 "reacquires fresh transport presence"; §11.1). The
// restart reason is the closed-enum value for a restart recovery.
func (d *Daemon) requestRelayCredential() {
	d.mu.RLock()
	hasManager := d.tunnel != nil
	d.mu.RUnlock()
	if !hasManager {
		return // no supervision to arm: a fresh credential would be dropped
	}
	sender, ok := d.signaling.(relayCredentialRequestSender)
	if !ok {
		log.Printf("relay credential requester unavailable: signaling client lacks relay_credential_request")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sender.SendRelayCredentialRequest(ctx, tunnel.ReasonRestart); err != nil {
		log.Printf("request relay credential on unlock: %v", err)
	}
}

// reportLockdownState re-sends the current advisory lockdown state for a new
// control epoch: a reconnect while locked must not look unlocked.
func (d *Daemon) reportLockdownState() {
	d.sendLockdownStatus(d.nextLockdownGeneration(), d.IsLocked())
}

// lockdownStatusSender is the signaling capability the §11.1 lockdown_status
// report rides. Capability-asserted so minimal signaling fakes without it
// simply drop the advisory telemetry instead of failing the transition.
type lockdownStatusSender interface {
	SendLockdownStatus(ctx context.Context, status signaling.LockdownStatus) error
}

// sendLockdownStatus forwards one advisory §11.1 lockdown_status report.
// Absence of the capability is not an error: control's status is a fast path
// only and the local gate/binder enforcement is final.
func (d *Daemon) sendLockdownStatus(generation int, locked bool) {
	sender, ok := d.signaling.(lockdownStatusSender)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sender.SendLockdownStatus(ctx, signaling.LockdownStatus{Generation: generation, Locked: locked}); err != nil {
		log.Printf("send lockdown_status: %v", err)
	}
}
