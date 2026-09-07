package directctl

import (
	"context"
	"errors"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/pocketbase/pocketbase/core"
	"sharebridge/control/internal/certcoordinator"
	"sharebridge/control/internal/ddns"
	"sharebridge/control/internal/hub"
	"sharebridge/control/internal/relayctl"
	"sharebridge/control/internal/stun"
)

// Config holds controller configuration.
type Config struct {
	BaseDomain string
	// RelayGatewayIPv4 is the public IPv4 of the relay gateway (§6). Baseline
	// enrollment points the per-namespace content wildcard
	// *.relay.<namespace>.<base-domain> at it; an absent or invalid value
	// keeps baseline readiness unreachable (§7.1).
	RelayGatewayIPv4 string
	// RelaySelectionEnabled is the Task 20 §9.1 operator flag. Zero value
	// (false) is the safe default: rollback mode — direct candidates get the
	// Phase 4a interstitial, relay is never selected (relay-dependent cases
	// return 503), and the legacy Phase 3 direct 302 is never restored.
	// Every relay selection in this package is gated on it.
	RelaySelectionEnabled bool
	// RelayPresence installs the Task 15 gateway-authoritative presence view
	// at construction (preferred over the removed EnableRelayPresence setter,
	// Task 20 carry-forward c: the view must be in place before the
	// controller serves traffic, and constructor wiring makes that
	// precondition structural). nil makes RelayAvailable fail closed (never
	// available) — e.g. Phase 3-style deployments with no relay sync.
	RelayPresence *relayctl.PresenceView
	// Routes supplies the route publisher's current revision for the Task 15
	// read-then-check contract (see routes.go readRouteFacts). nil makes the
	// revision read 0, which the presence view's Available rejects — fail
	// closed.
	Routes relayctl.RouteRevisionSource
	// Test-only (default zero values = production behavior):
	AllowPrivateProbes bool                                                                // disables the SSRF denylist
	DDNSFunc           func(ctx context.Context, name, ip string, ttl int) (string, error) // overrides the real ddns client
	RelayDNSFunc       func(ctx context.Context, name, ip string, ttl int) (string, error) // overrides the relay wildcard provisioning client
}

// epochState captures the readiness of a single agent WebSocket connection.
// Readiness is connection-epoch-local: a persisted cert_status=ready does NOT
// authorize readiness alone — the current connection must complete
// enrolled → tls_ready (+ relay DNS provisioning) → enrollment_ready itself
// (§7.1: baseline = TLS + relay DNS; direct DDNS is an optional capability).
//
// epoch is a monotonic per-process connection epoch number (Task 18): it is
// the value bound into STUN challenges at IssueChallenge and re-checked at
// TakeObservation, so a reconnect (new number) can never claim an old
// observation and a control restart (fresh counter + fresh per-process
// receipt key) behaves exactly like a reconnect. conn is written once at
// install time and never mutated.
type epochState struct {
	agentID       string
	namespace     string
	conn          *websocket.Conn
	epoch         stun.Epoch
	tlsReady      bool
	relayDNSReady bool
	ready         bool
	stun          *epochSTUN // per-epoch STUN challenge state; guarded by stunMu
}

// OpenAck is the control-side acknowledgement of an open_signal. It is a
// SHARED type (declared once here, used by Task 13's EmitOpen/HandleOpenAck).
type OpenAck struct {
	ShareID        string
	Nonce          string
	Seq            uint64
	GrantedPort    int
	PublicIP       string
	WasAlreadyOpen bool
	Status         string
	Error          string
}

// openWaiter is a pending open_signal awaiting an open_ack. SHARED type,
// declared once here so Task 13 can populate the waiters map.
type openWaiter struct {
	apiKeyID string
	shareID  string
	seq      uint64
	ch       chan OpenAck
}

// Controller is the direct-mode control-plane handler.
type Controller struct {
	app   core.App
	hub   *hub.Hub
	coord *certcoordinator.Coordinator
	ddns  *ddns.Cloudflare
	cfg   Config

	sendFn        func(ctx context.Context, conn *websocket.Conn, msg any) error
	ddnsFn        func(ctx context.Context, name, ip string, ttl int) (string, error)
	relayDNSFn    func(ctx context.Context, name, ip string, ttl int) (string, error)
	sendToAgentFn func(ctx context.Context, apiKeyID string, msg any) error
	emitOpenFn    func(ctx context.Context, apiKeyID, shareID, origin string, lease time.Duration) (OpenAck, error)
	probeFn       func(ctx context.Context, origin, code, apiKeyID string, ack OpenAck) error

	// relayDNSProvisioned caches the namespaces whose relay wildcard was
	// already ensured at the current gateway IPv4 in this process, so repeated
	// enrollments never churn the provider (the record is static for the
	// gateway assignment lifetime; a control restart re-checks at the provider
	// once via the idempotent EnsureA).
	relayDNSMu          sync.Mutex
	relayDNSProvisioned map[string]string

	epochMu sync.Mutex
	epochs  map[string]*epochState // apiKeyID -> current connection epoch

	// open-signal waiters + sequence (Task 13 populates; declared here)
	waiterMu sync.Mutex
	waiters  map[string]*openWaiter
	seqMu    sync.Mutex
	seq      map[string]uint64

	// probe (Task 14 populates; declared here)
	verifiedMu   sync.Mutex
	verified     map[string]string
	allowPrivate bool
	probeClient  *http.Client

	// relay holds the §4.5 tunnel policy + credential signer when relay is
	// enabled; nil disables relay_config emission entirely. See enroll.go.
	relay *relayEmitter

	// relayPresence holds the Task 15 gateway-authoritative presence view
	// when injected at construction (Config.RelayPresence); nil makes
	// RelayAvailable fail closed (never available). It is the ONLY presence
	// state selection may read (§4.2, §12: never the relay_last_seen_at
	// diagnostic). Installed exclusively via Config — no post-construction
	// setter survives Task 20 (carry-forward c), so the before-serving
	// precondition is structural.
	relayPresence *relayctl.PresenceView

	// routes supplies the current route revision for the Task 15
	// read-then-check contract (routes.go). *relayctl.Publisher implements
	// it; nil reads revision 0, which the presence view rejects.
	routes relayctl.RouteRevisionSource

	// STUN challenge scheduling (Task 18, stun.go). stunServer is installed
	// with EnableSTUN before serving; nil keeps scheduling disabled. nowFn
	// and randFn are clock/jitter seams (§10.2 cadence tests inject a fake
	// clock like Task 15/16); stunIssueFn defaults to the listener's
	// IssueChallenge (the ONLY rate limiter — the Task 16 per-agent token
	// bucket, §16.4) and exists so tests can exercise bounded backoff.
	stunServer    *stun.Server
	stunAdvertise string
	stunIssueFn   func(agentID string, epoch stun.Epoch) (stun.Challenge, error)
	nowFn         func() time.Time
	randFn        func() float64
	stunMu        sync.Mutex

	// epochSeq feeds epochState.epoch (monotonic, per-process; §10.2
	// "including after every reconnect").
	epochSeq uint64

	ackTimeout time.Duration
}

// NewController constructs a Controller with the default send/ddns/sendToAgent
// wiring.
func NewController(app core.App, h *hub.Hub, coord *certcoordinator.Coordinator, dnsClient *ddns.Cloudflare, cfg Config) *Controller {
	c := &Controller{
		app: app, hub: h, coord: coord, ddns: dnsClient, cfg: cfg,
		relayPresence:       cfg.RelayPresence,
		routes:              cfg.Routes,
		epochs:              map[string]*epochState{},
		waiters:             map[string]*openWaiter{},
		seq:                 map[string]uint64{},
		verified:            map[string]string{},
		relayDNSProvisioned: map[string]string{},
		ackTimeout:          3 * time.Second,
		nowFn:               time.Now,
		randFn:              rand.Float64,
	}
	c.sendFn = func(ctx context.Context, conn *websocket.Conn, msg any) error {
		return hub.SendDirect(ctx, conn, msg)
	}
	c.ddnsFn = func(ctx context.Context, name, ip string, ttl int) (string, error) {
		if dnsClient == nil {
			return "", errors.New("ddns not configured")
		}
		return dnsClient.UpsertA(ctx, name, ip, ttl)
	}
	c.relayDNSFn = func(ctx context.Context, name, ip string, ttl int) (string, error) {
		if dnsClient == nil {
			return "", errors.New("relay dns not configured")
		}
		return dnsClient.EnsureA(ctx, name, ip, ttl)
	}
	c.sendToAgentFn = func(ctx context.Context, apiKeyID string, msg any) error {
		return h.SendToAgent(ctx, apiKeyID, msg)
	}
	c.emitOpenFn = c.EmitOpen
	c.probeFn = c.Probe
	if cfg.AllowPrivateProbes {
		c.allowPrivate = true
	}
	if cfg.DDNSFunc != nil {
		c.ddnsFn = cfg.DDNSFunc
	}
	if cfg.RelayDNSFunc != nil {
		c.relayDNSFn = cfg.RelayDNSFunc
	}
	return c
}

// epochReady reports whether the CURRENT connection epoch for apiKeyID has
// reached readiness. Used by tests to assert readiness is not inherited across
// epochs.
func (c *Controller) epochReady(apiKeyID string) bool {
	c.epochMu.Lock()
	defer c.epochMu.Unlock()
	e := c.epochs[apiKeyID]
	return e != nil && e.ready
}

// isCurrentEpoch reports whether conn is the connection that owns the current
// epoch for apiKeyID. A fenced (superseded) socket must not drive any
// epoch-sensitive state transition (CSR issuance, TLS ready, DDNS, open_ack).
func (c *Controller) isCurrentEpoch(apiKeyID string, conn *websocket.Conn) bool {
	c.epochMu.Lock()
	defer c.epochMu.Unlock()
	e := c.epochs[apiKeyID]
	return e != nil && e.conn == conn
}

// IsCurrentEpoch is isCurrentEpoch exported for the handler package's
// telemetry gating: an agent message from a superseded socket must be
// rejected before it is treated as current-epoch telemetry.
func (c *Controller) IsCurrentEpoch(apiKeyID string, conn *websocket.Conn) bool {
	return c.isCurrentEpoch(apiKeyID, conn)
}

// AgentDisconnected drops the epoch for apiKeyID only if conn is still the
// connection that owns it (a stale old-socket disconnect must not disrupt a
// replacement socket). It also drops that epoch's waiters WITHOUT closing
// their channels (closing would make a later EmitOpen read a zero-ack and
// treat it as success), and tears down the epoch's STUN challenge state
// (stopping its timer and failing inline waiters — no goroutine leaks).
func (c *Controller) AgentDisconnected(apiKeyID string, conn *websocket.Conn) {
	c.epochMu.Lock()
	matched := false
	var torn *epochState
	if e, ok := c.epochs[apiKeyID]; ok && e.conn == conn {
		delete(c.epochs, apiKeyID)
		matched = true
		torn = e
	}
	c.epochMu.Unlock()
	if !matched {
		return
	}
	if torn != nil {
		c.stunMu.Lock()
		c.stunTeardownLocked(torn)
		c.stunMu.Unlock()
	}
	c.seqMu.Lock()
	delete(c.seq, apiKeyID)
	c.seqMu.Unlock()
	c.waiterMu.Lock()
	for nonce, w := range c.waiters {
		if w.apiKeyID == apiKeyID {
			delete(c.waiters, nonce)
		}
	}
	c.waiterMu.Unlock()
}

// CurrentDirectMatch evaluates the §10.3 live predicate against the CURRENT
// connection epoch's fresh observation for apiKeyID (single clock reading
// taken here). requiredIPs are the surfaces being gated (report_endpoint IP
// and/or open_ack public IP). This is the read Task 20's route selection will
// reuse; it NEVER consults the persisted direct_status diagnostics (§12,
// §15.7). When STUN scheduling is not wired there is no observation and the
// outcome is a closed fail (stun_timeout).
func (c *Controller) CurrentDirectMatch(apiKeyID string, requiredIPs ...string) DirectMatch {
	now := c.nowFn() // single clock reading for the freshness decision
	_, _, match := c.evaluateCurrentDirectMatch(apiKeyID, now, requiredIPs...)
	return match
}

// evaluateCurrentDirectMatch is CurrentDirectMatch with the caller-supplied
// single clock reading; it also returns the observation so the gated flow can
// persist the driving observation as diagnostics (§12).
func (c *Controller) evaluateCurrentDirectMatch(apiKeyID string, now time.Time, requiredIPs ...string) (STUNObservation, bool, DirectMatch) {
	observation, fresh := c.CurrentSTUNObservation(apiKeyID, now)
	return observation, fresh, evaluateDirectSTUNMatch(observation, fresh, requiredIPs...)
}

// EnableRelayPresence was the Task 15 setter; Task 20 (carry-forward c)
// removes it in favor of constructor-time injection via Config.RelayPresence.
// The unsynchronized setter allowed installing a presence view after the
// controller was already serving, violating the documented before-serving
// precondition; Config wiring makes the precondition structural. Tests that
// need to seed controller internals use newTestController / Config directly.

// InstallReadyEpochForTest is a TEST-ONLY seam (unreachable from production
// paths, which earn readiness exclusively through the enrollment flow):
// it installs a ready connection epoch and hub presence for apiKeyID so
// route-selection tests in packages that cannot reach controller internals
// (cmd/server) can seed the WS-state matrix dimension without replaying a
// full WebSocket enrollment.
func (c *Controller) InstallReadyEpochForTest(apiKeyID, namespace string) {
	c.epochMu.Lock()
	c.epochs[apiKeyID] = &epochState{agentID: "agent-test", namespace: namespace, ready: true}
	c.epochMu.Unlock()
	c.hub.RegisterAgent(apiKeyID, nil)
}

// RelayAvailable is the read-only §7.1 relay-eligibility presence term:
// "relay eligible = baseline ready + active gateway tunnel-presence lease".
// It delegates to the presence view's Available predicate, which joins the
// live (agent, port, generation) lease against the caller's route revision
// and the given instant. With no view installed it is always false (fail
// closed). It never reads the persisted relay_last_seen_at diagnostic and
// never consults agent telemetry (§12, §15.7).
func (c *Controller) RelayAvailable(agentID string, relayPort int, generation uint64, routeRevision uint64, now time.Time) bool {
	if c.relayPresence == nil {
		return false
	}
	return c.relayPresence.Available(agentID, relayPort, generation, routeRevision, now)
}
