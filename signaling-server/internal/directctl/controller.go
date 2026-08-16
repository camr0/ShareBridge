package directctl

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/pocketbase/pocketbase/core"
	"sharebridge/server/internal/certcoordinator"
	"sharebridge/server/internal/ddns"
	"sharebridge/server/internal/hub"
)

// Config holds controller configuration.
type Config struct {
	BaseDomain string
}

// epochState captures the readiness of a single agent WebSocket connection.
// Readiness is connection-epoch-local: a persisted cert_status=ready does NOT
// authorize readiness alone — the current connection must complete
// enrolled → tls_ready (+ DDNS) → enrollment_ready itself.
type epochState struct {
	agentID   string
	namespace string
	conn      *websocket.Conn
	tlsReady  bool
	ddnsReady bool
	ready     bool
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
	sendToAgentFn func(ctx context.Context, apiKeyID string, msg any) error
	emitOpenFn    func(ctx context.Context, apiKeyID, shareID, origin string, lease time.Duration) (OpenAck, error)
	probeFn       func(ctx context.Context, origin, code, apiKeyID string, ack OpenAck) error

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

	ackTimeout time.Duration
}

// NewController constructs a Controller with the default send/ddns/sendToAgent
// wiring.
func NewController(app core.App, h *hub.Hub, coord *certcoordinator.Coordinator, dnsClient *ddns.Cloudflare, cfg Config) *Controller {
	c := &Controller{
		app: app, hub: h, coord: coord, ddns: dnsClient, cfg: cfg,
		epochs:     map[string]*epochState{},
		waiters:    map[string]*openWaiter{},
		seq:        map[string]uint64{},
		verified:   map[string]string{},
		ackTimeout: 3 * time.Second,
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
	c.sendToAgentFn = func(ctx context.Context, apiKeyID string, msg any) error {
		return h.SendToAgent(ctx, apiKeyID, msg)
	}
	c.emitOpenFn = c.EmitOpen
	c.probeFn = c.Probe
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

// AgentDisconnected drops the epoch for apiKeyID only if conn is still the
// connection that owns it (a stale old-socket disconnect must not disrupt a
// replacement socket). It also drops that epoch's waiters WITHOUT closing
// their channels (closing would make a later EmitOpen read a zero-ack and
// treat it as success).
func (c *Controller) AgentDisconnected(apiKeyID string, conn *websocket.Conn) {
	c.epochMu.Lock()
	matched := false
	if e, ok := c.epochs[apiKeyID]; ok && e.conn == conn {
		delete(c.epochs, apiKeyID)
		matched = true
	}
	c.epochMu.Unlock()
	if !matched {
		return
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
