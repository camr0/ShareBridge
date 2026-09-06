// Package presence keeps the gateway's authoritative tunnel presence state
// machine (spec §4.2, §7.3, §15.1–15.2). It consumes the credential-free
// facts the frpplugin emits on its bounded ordered dispatcher and decides,
// per agent/port/generation join key, whether the tunnel is online:
//
//	absent
//	  └─ valid Login + authorized NewProxy + probe-confirmed NewUserConn ─► online
//	online
//	  ├─ current Ping renews 45s lease ─► online
//	  ├─ CloseProxy / logout / lease expiry ─► absent
//	  ├─ frps reset (ClearAll) ─► absent
//	  └─ replacement generation ─► fenced, then new online
//
// Online requires the probe-confirmed readiness predicate (§4.2 condition 2,
// §23.1): an authorized NewProxy is pre-registration authorization in the
// pinned release, so the gateway closes the loop itself — after NewProxy it
// runs the bounded loopback readiness probe (§4.4: ≤5 attempts, ~500 ms
// backoff, 2.5 s hard deadline re-checked before each wait) and only a
// NewUserConn callback correlating ALL FOUR fields (proxy name, server-
// assigned run id, generation metadata, probe socket source address) for the
// exact current generation yields online. A registration failure therefore
// never becomes online while authenticated Pings continue, and stale
// listeners answering with old run ids/generations can never confirm.
//
// Every emitted event carries the gateway boot ID and a monotonically
// increasing revision (§7.3); control discards events from an older boot or
// revision. Routing joins presence against the route table's agent/port/
// generation (routes.Presence): a presence fact without a matching route is
// not routable, and a route without online presence is not routable.
package presence

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	"sharebridge/relay/internal/frpplugin"
	"sharebridge/relay/internal/routes"
)

const (
	// StateOnline and StateOffline are the §11.3 presence event wire values
	// (they match control's relayctl/controlsync PresenceState constants).
	StateOnline  = "online"
	StateOffline = "offline"

	// DefaultLeaseTTL is the §14 tunnel presence lease, renewed only by an
	// authenticated current-generation Ping. Four nominal 10-second Ping
	// opportunities leave one delayed or missed Ping without flapping.
	DefaultLeaseTTL = 45 * time.Second

	// §4.4/§14 readiness probe budget defaults: at most five loopback connect
	// attempts with ~500 ms backoff against a 2.5-second hard deadline.
	DefaultProbeMaxAttempts  = 5
	DefaultProbeBackoff      = 500 * time.Millisecond
	DefaultProbeHardDeadline = 2500 * time.Millisecond
	DefaultProbeDialTimeout  = 500 * time.Millisecond

	// maxBootIDBytes mirrors the §11.3 identifier bound.
	maxBootIDBytes = 128

	// maxBufferedUserConns caps the NewUserConn callbacks buffered while the
	// probe dial has not yet committed its source address. It is at least the
	// probe attempt count; each buffered entry is one bounded address string.
	maxBufferedUserConns = 8

	// maxRemoteAddrBytes bounds a correlation remote address ("127.0.0.1:port"
	// and the IPv6 loopback form both fit comfortably).
	maxRemoteAddrBytes = 64

	// maxFactFieldBytes bounds the per-field size of correlation data on an
	// admitted fact (proxy name, run id), matching frpplugin's identifier
	// bound.
	maxFactFieldBytes = 128
)

// Event is one gateway-authoritative tunnel presence transition (§7.3, §11.3).
type Event struct {
	BootID         string
	Revision       uint64
	AgentRecordID  string
	RelayPort      int
	Generation     uint64
	State          string
	LeaseExpiresAt time.Time
}

// Sink receives presence events in emission (revision) order. The registry
// calls it while holding its state lock — which the gateway stream-admission
// path holds in turn (gateway Streams.RegisterAdmitted) — so an implementation
// MUST NOT block: enqueue on a bounded queue and drop on overflow, exactly
// like frpplugin's dispatcher. Losing an event degrades fail-closed (a missed
// online delays availability; control's own lease expiry covers a missed
// offline), never fail-open.
type Sink interface {
	ObservePresenceEvent(Event)
}

// ProbeFunc performs one bounded loopback readiness probe against the agent's
// assigned loopback FRP port and reports the probe socket's local (source)
// address, which frps echoes back as the NewUserConn remote_addr correlation
// field. Implementations must respect ctx and write zero bytes.
type ProbeFunc func(ctx context.Context, relayPort int) (sourceAddress string, err error)

// AgentDrainer drains established public streams by agent record (satisfied
// by gateway.Streams). Only the §15.2 frps-reset path (ClearAll) drains: the
// tunnel's data plane dies with frps itself there, and §15.4 keeps other
// presence transitions from closing established streams.
type AgentDrainer interface {
	CloseAgent(agentRecordID string) int
}

type discardSink struct{}

func (discardSink) ObservePresenceEvent(Event) {}

// Config is the fail-closed construction contract for the registry.
type Config struct {
	// BootID is this gateway process's identity; a new boot replaces all
	// recorded presence at control (§15.1). Required.
	BootID string
	// LeaseTTL overrides the 45-second default lease. Must be positive.
	LeaseTTL time.Duration
	// Now is the clock seam; tests inject a fake clock.
	Now func() time.Time
	// Sink receives online/offline events. Optional; discards when nil.
	Sink Sink
	// Probe overrides the default loopback readiness probe. Optional.
	Probe ProbeFunc
	// Drainer drains established streams on the frps-reset path. Optional.
	Drainer AgentDrainer
	// Probe budget overrides; zero selects the §14 defaults.
	ProbeMaxAttempts  int
	ProbeBackoff      time.Duration
	ProbeHardDeadline time.Duration
	ProbeDialTimeout  time.Duration
}

// tunnelKey is the §8 presence join: one agent record, one relay port, one
// tunnel generation.
type tunnelKey struct {
	agentRecordID string
	relayPort     int
	generation    uint64
}

// probeState is one tunnel's in-flight readiness probe expectation.
type probeState struct {
	// sourceAddress is the probe socket's own address once a dial succeeds;
	// empty until then and when the probe failed outright.
	sourceAddress string
	// deadline is the probe's hard deadline in registry-clock time.
	deadline time.Time
	// buffered holds correlation addresses of NewUserConn callbacks that
	// arrived before the dial committed the source address (frps calls the
	// plugin inside the accept loop, so the fact can beat the dial return).
	buffered []string
	// finished records that the probe returned (successfully or not); after
	// a failed probe nothing may confirm.
	finished bool
}

type tunnelState struct {
	proxyName       string
	runID           string
	loginSeen       bool
	proxyAuthorized bool
	online          bool
	leaseExpiresAt  time.Time
	probe           *probeState
}

// Registry is the gateway's leased tunnel presence state machine. Safe for
// concurrent use by the plugin's event dispatcher (ObserveFRPEvent), every
// public stream admission (Online, under the streams lock), and the frps
// reset path (ClearAll).
type Registry struct {
	bootID            string
	leaseTTL          time.Duration
	now               func() time.Time
	sink              Sink
	probe             ProbeFunc
	drainer           AgentDrainer
	probeHardDeadline time.Duration

	mu       sync.Mutex
	revision uint64
	tunnels  map[tunnelKey]*tunnelState
	inflight sync.WaitGroup
}

// Registry satisfies the route table's presence join statically.
var _ routes.Presence = (*Registry)(nil)

// NewRegistry validates the configuration fail-closed and returns the registry.
func NewRegistry(config Config) (*Registry, error) {
	if len(config.BootID) == 0 || len(config.BootID) > maxBootIDBytes {
		return nil, fmt.Errorf("presence: gateway boot id must be 1..%d bytes", maxBootIDBytes)
	}
	for index := 0; index < len(config.BootID); index++ {
		if config.BootID[index] < 0x21 || config.BootID[index] > 0x7e {
			return nil, errors.New("presence: gateway boot id contains a forbidden byte")
		}
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	leaseTTL := config.LeaseTTL
	if leaseTTL == 0 {
		leaseTTL = DefaultLeaseTTL
	}
	if leaseTTL < 0 {
		return nil, errors.New("presence: negative lease TTL")
	}
	bounds, err := resolveProbeBounds(config)
	if err != nil {
		return nil, err
	}
	sink := config.Sink
	if sink == nil {
		sink = discardSink{}
	}
	probe := config.Probe
	if probe == nil {
		probe = newLoopbackProbe(bounds.maxAttempts, bounds.backoff, bounds.hardDeadline, bounds.dialTimeout)
	}
	return &Registry{
		bootID:            config.BootID,
		leaseTTL:          leaseTTL,
		now:               config.Now,
		sink:              sink,
		probe:             probe,
		drainer:           config.Drainer,
		probeHardDeadline: bounds.hardDeadline,
		tunnels:           make(map[tunnelKey]*tunnelState),
	}, nil
}

type loopbackBounds struct {
	maxAttempts  int
	backoff      time.Duration
	hardDeadline time.Duration
	dialTimeout  time.Duration
}

func resolveProbeBounds(config Config) (loopbackBounds, error) {
	bounds := loopbackBounds{
		maxAttempts:  DefaultProbeMaxAttempts,
		backoff:      DefaultProbeBackoff,
		hardDeadline: DefaultProbeHardDeadline,
		dialTimeout:  DefaultProbeDialTimeout,
	}
	if config.ProbeMaxAttempts != 0 {
		bounds.maxAttempts = config.ProbeMaxAttempts
	}
	if config.ProbeBackoff != 0 {
		bounds.backoff = config.ProbeBackoff
	}
	if config.ProbeHardDeadline != 0 {
		bounds.hardDeadline = config.ProbeHardDeadline
	}
	if config.ProbeDialTimeout != 0 {
		bounds.dialTimeout = config.ProbeDialTimeout
	}
	if bounds.maxAttempts < 1 || bounds.backoff < 0 || bounds.hardDeadline <= 0 ||
		bounds.dialTimeout <= 0 || bounds.dialTimeout > bounds.hardDeadline {
		return bounds, errors.New("presence: invalid readiness probe bounds")
	}
	return bounds, nil
}

// ObserveFRPEvent consumes one credential-free fact from frpplugin's bounded
// ordered dispatcher. Facts are processed strictly in authorization order, so
// a tunnel's Login, NewProxy, Ping, NewUserConn, and CloseProxy facts arrive
// in the order frps produced them.
func (registry *Registry) ObserveFRPEvent(fact frpplugin.PresenceFact) {
	// Range-check the Task 13 carry-forward edge at the fact boundary:
	// credential generations are non-negative ints and relay ports are TCP
	// ports. Facts outside that domain are dropped — the int -> uint64 key
	// conversion below is exact for every admitted value.
	if fact.AgentRecordID == "" || fact.RelayPort <= 0 || fact.RelayPort > 65535 || fact.Generation < 0 {
		return
	}
	if fact.ProxyName == "" || len(fact.ProxyName) > maxFactFieldBytes || len(fact.RunID) > maxFactFieldBytes {
		return
	}
	key := tunnelKey{
		agentRecordID: fact.AgentRecordID,
		relayPort:     fact.RelayPort,
		generation:    uint64(fact.Generation),
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()
	tunnel := registry.tunnels[key]
	if tunnel != nil {
		registry.expireIfDueLocked(key, tunnel)
	}
	switch fact.Operation {
	case frpplugin.OperationLogin:
		registry.observeLogin(key, fact)
	case frpplugin.OperationNewProxy:
		registry.observeNewProxy(key, fact, tunnel)
	case frpplugin.OperationPing:
		registry.observePing(tunnel)
	case frpplugin.OperationCloseProxy:
		registry.observeCloseProxy(key, fact, tunnel)
	case frpplugin.OperationNewUserConn:
		registry.observeUserConn(key, fact, tunnel)
	}
}

// observeLogin admits a new tunnel generation and fences every older
// generation of the same agent. The plugin only emits strictly newer
// generations (its issued-at/generation fences reject equal or older
// credentials), so a fact for an existing key is replayed history and is
// ignored fail-closed: live state can never regress.
func (registry *Registry) observeLogin(key tunnelKey, fact frpplugin.PresenceFact) {
	if registry.tunnels[key] != nil {
		return
	}
	for otherKey, other := range registry.tunnels {
		if otherKey.agentRecordID == key.agentRecordID && otherKey.generation < key.generation {
			registry.absentLocked(otherKey, other)
			delete(registry.tunnels, otherKey)
		}
	}
	registry.tunnels[key] = &tunnelState{
		proxyName: fact.ProxyName,
		runID:     fact.RunID,
		loginSeen: true,
	}
}

// observeNewProxy records the authorized NewProxy and evokes the bounded
// readiness probe. The tunnel is still absent: NewProxy is pre-registration
// authorization in the pinned release (frps binds the listener only if
// downstream registration succeeds, and never retries a failed NewProxy).
func (registry *Registry) observeNewProxy(key tunnelKey, fact frpplugin.PresenceFact, tunnel *tunnelState) {
	if tunnel == nil || !tunnel.loginSeen || fact.ProxyName != tunnel.proxyName {
		return
	}
	if fact.RunID == "" {
		return // the server-assigned run id is mandatory from NewProxy on
	}
	if tunnel.runID != "" && tunnel.runID != fact.RunID {
		return // run-id change mid-session: fail closed, never re-arm
	}
	tunnel.runID = fact.RunID
	if tunnel.proxyAuthorized {
		return // identical NewProxy retry; the plugin dedups, this is defense
	}
	tunnel.proxyAuthorized = true
	registry.startProbeLocked(key, tunnel)
}

// observePing renews the 45-second lease of an online tunnel. A Ping before
// confirmation creates nothing; a Ping after expiry does not resurrect —
// only the full probe-confirmed readiness path returns a tunnel to online.
func (registry *Registry) observePing(tunnel *tunnelState) {
	if tunnel == nil || !tunnel.online {
		return
	}
	tunnel.leaseExpiresAt = registry.now().Add(registry.leaseTTL)
}

// observeCloseProxy makes the tunnel absent. The entry stays as a tombstone:
// a late NewUserConn for a closed proxy must confirm nothing, and the plugin
// rejects any same-generation re-login.
func (registry *Registry) observeCloseProxy(key tunnelKey, fact frpplugin.PresenceFact, tunnel *tunnelState) {
	if tunnel == nil || fact.ProxyName != tunnel.proxyName {
		return
	}
	registry.absentLocked(key, tunnel)
}

// observeUserConn correlates one NewUserConn callback against the tunnel's
// pending probe expectation. Confirmation requires ALL FOUR correlation
// fields for the exact current generation: proxy name, server-assigned run
// id, generation (the join key), and the probe socket's own source address.
// Anything else — a stale listener answering with an old run id, another
// local process's connection to the loopback port — never confirms.
func (registry *Registry) observeUserConn(key tunnelKey, fact frpplugin.PresenceFact, tunnel *tunnelState) {
	if tunnel == nil || tunnel.probe == nil {
		return // readiness facts confirm nothing without our own probe
	}
	if fact.ProxyName != tunnel.proxyName || tunnel.runID == "" || fact.RunID != tunnel.runID {
		return
	}
	if fact.RemoteAddr == "" || len(fact.RemoteAddr) > maxRemoteAddrBytes {
		return
	}
	probe := tunnel.probe
	if registry.now().After(probe.deadline) {
		tunnel.probe = nil
		return
	}
	if probe.sourceAddress != "" {
		if fact.RemoteAddr == probe.sourceAddress {
			registry.confirmLocked(key, tunnel)
		}
		return
	}
	if probe.finished || len(probe.buffered) >= maxBufferedUserConns {
		return // the probe failed outright, or its race buffer is full
	}
	probe.buffered = append(probe.buffered, fact.RemoteAddr)
}

// startProbeLocked arms the probe expectation and runs the bounded loopback
// probe asynchronously — one goroutine per authorized NewProxy (the plugin
// emits at most one), hard-deadline capped, never per user connection. The
// NewUserConn handler side runs on frpplugin's existing bounded dispatcher
// with zero additional goroutines.
func (registry *Registry) startProbeLocked(key tunnelKey, tunnel *tunnelState) {
	tunnel.probe = &probeState{deadline: registry.now().Add(registry.probeHardDeadline)}
	hardDeadline := registry.probeHardDeadline
	registry.inflight.Add(1)
	go func() {
		defer registry.inflight.Done()
		ctx, cancel := context.WithTimeout(context.Background(), hardDeadline)
		defer cancel()
		sourceAddress, probeErr := registry.probe(ctx, key.relayPort)

		registry.mu.Lock()
		defer registry.mu.Unlock()
		tunnel := registry.tunnels[key]
		if tunnel == nil || tunnel.probe == nil {
			return // fenced, closed, or cleared while probing
		}
		probe := tunnel.probe
		probe.finished = true
		if probeErr != nil {
			return // fail closed: no source address, nothing ever confirms
		}
		probe.sourceAddress = sourceAddress
		for _, buffered := range probe.buffered {
			if buffered == sourceAddress {
				registry.confirmLocked(key, tunnel)
				return
			}
		}
	}()
}

// confirmLocked is the single absent -> online transition: probe-confirmed,
// fully correlated, current generation. The lease starts here.
func (registry *Registry) confirmLocked(key tunnelKey, tunnel *tunnelState) {
	tunnel.online = true
	tunnel.leaseExpiresAt = registry.now().Add(registry.leaseTTL)
	tunnel.probe = nil
	registry.emitLocked(key, tunnel, StateOnline)
}

// absentLocked makes a tunnel absent and emits the offline transition once.
func (registry *Registry) absentLocked(key tunnelKey, tunnel *tunnelState) {
	tunnel.probe = nil
	if !tunnel.online {
		return
	}
	tunnel.online = false
	registry.emitLocked(key, tunnel, StateOffline)
}

// expireIfDueLocked applies the lazy lease-expiry transition: the state
// change happens at the lease boundary logically; the offline event is
// emitted at the first observation past it. Control enforces the same lease
// on its own view (§7.1/Task 15), so a delayed observation never extends
// availability.
func (registry *Registry) expireIfDueLocked(key tunnelKey, tunnel *tunnelState) {
	if tunnel.online && !registry.now().Before(tunnel.leaseExpiresAt) {
		registry.absentLocked(key, tunnel)
	}
}

// emitLocked stamps and publishes one transition event. Called under
// registry.mu, so revisions are strictly increasing in emission order; the
// sink's non-blocking contract keeps this safe on the admission path.
func (registry *Registry) emitLocked(key tunnelKey, tunnel *tunnelState, state string) {
	registry.revision++
	registry.sink.ObservePresenceEvent(Event{
		BootID:         registry.bootID,
		Revision:       registry.revision,
		AgentRecordID:  key.agentRecordID,
		RelayPort:      key.relayPort,
		Generation:     key.generation,
		State:          state,
		LeaseExpiresAt: tunnel.leaseExpiresAt,
	})
}

// Online answers the route table's presence join (routes.Presence). It reads
// only in-memory leased state and never blocks on I/O or re-enters the
// streams registry — the gateway's RegisterAdmitted calls it under its own
// lock (Streams ordering invariant).
func (registry *Registry) Online(agentRecordID string, relayPort int, generation uint64) bool {
	// Out-of-domain joins fail closed: the registry only ever holds keys
	// range-checked into the non-negative int generation and TCP port domains,
	// so a route carrying a wider uint64 generation can never join.
	if relayPort <= 0 || relayPort > 65535 || generation > math.MaxInt64 {
		return false
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	key := tunnelKey{
		agentRecordID: agentRecordID,
		relayPort:     relayPort,
		generation:    generation,
	}
	tunnel := registry.tunnels[key]
	if tunnel == nil {
		return false
	}
	registry.expireIfDueLocked(key, tunnel)
	return tunnel.online
}

// ClearAll implements §15.2: an frps restart without a gateway restart clears
// all tunnel presence immediately. Established relayed streams terminate —
// AFTER the presence mutation commits, per the Streams.RegisterAdmitted
// mutate-before-drain ordering invariant. The registry lock is released
// before draining: CloseAgent takes the streams lock, and an admission path
// holding the streams lock calls back into Online, so the reverse order
// would deadlock.
func (registry *Registry) ClearAll() {
	registry.mu.Lock()
	agents := make(map[string]struct{}, len(registry.tunnels))
	for key, tunnel := range registry.tunnels {
		registry.absentLocked(key, tunnel)
		agents[key.agentRecordID] = struct{}{}
		delete(registry.tunnels, key)
	}
	registry.mu.Unlock()

	if registry.drainer == nil {
		return
	}
	ordered := make([]string, 0, len(agents))
	for agent := range agents {
		ordered = append(ordered, agent)
	}
	sort.Strings(ordered)
	for _, agent := range ordered {
		registry.drainer.CloseAgent(agent)
	}
}

// waitIdle blocks until all in-flight readiness probes have finished. Test
// seam only; production code never calls it.
func (registry *Registry) waitIdle() {
	registry.inflight.Wait()
}

// newLoopbackProbe returns the production §4.4 readiness probe: at most
// maxAttempts zero-byte TCP connects to 127.0.0.1:<port>, ~backoff apart,
// against a hard deadline that is re-checked before every wait and caps both
// every wait and every dial, so the true worst case stays inside the deadline
// (spike finding 3: an attempt must never start with the budget already
// spent). The socket writes and reads nothing and is closed immediately; in
// accept mode the agent target sees at most one harmless zero-byte
// connection (frp-readiness-spike-report.md E1/E5).
func newLoopbackProbe(maxAttempts int, backoff, hardDeadline, dialTimeout time.Duration) ProbeFunc {
	return func(ctx context.Context, relayPort int) (string, error) {
		address := net.JoinHostPort("127.0.0.1", strconv.Itoa(relayPort))
		deadline := time.Now().Add(hardDeadline)
		var lastErr error
		for attempt := 1; ; attempt++ {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return "", fmt.Errorf("presence: readiness probe hit its %s deadline after %d attempts: %w",
					hardDeadline, attempt-1, lastErr)
			}
			dialTimeoutBudget := dialTimeout
			if remaining < dialTimeoutBudget {
				dialTimeoutBudget = remaining
			}
			conn, err := net.DialTimeout("tcp", address, dialTimeoutBudget)
			if err == nil {
				sourceAddress := conn.LocalAddr().String()
				_ = conn.Close() // zero-byte connect, immediate close (§4.4)
				return sourceAddress, nil
			}
			lastErr = err
			if attempt >= maxAttempts {
				return "", fmt.Errorf("presence: readiness probe failed after %d attempts: %w", attempt, lastErr)
			}
			// Re-check the hard deadline BEFORE each wait (spike finding 3)
			// and cap the wait at the remaining budget.
			remaining = time.Until(deadline)
			if remaining <= 0 {
				return "", fmt.Errorf("presence: readiness probe hit its %s deadline after %d attempts: %w",
					hardDeadline, attempt, lastErr)
			}
			wait := backoff
			if remaining < wait {
				wait = remaining
			}
			select {
			case <-ctx.Done():
				return "", fmt.Errorf("presence: readiness probe canceled: %w", ctx.Err())
			case <-time.After(wait):
			}
		}
	}
}
