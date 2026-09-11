// Package gateway is the public Layer 4 acceptor (spec §4.1, §8, §16.1): it
// accepts browser TCP connections, extracts the exact SNI from the
// ClientHello without terminating TLS, routes by exact hostname against the
// in-memory route table, and splices the browser stream onto the agent's
// assigned loopback FRP port. It never parses HTTP, never calls a TLS server
// stack, never injects bytes into the stream, and never queries the database
// on a public connection — routing joins entirely from synchronized
// in-memory state. Every rejection is a generic TCP close; the reasons exist
// only in logs and metrics.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"sharebridge/relay/internal/clienthello"
	"sharebridge/relay/internal/limits"
	"sharebridge/relay/internal/metrics"
	"sharebridge/relay/internal/routes"
)

// ConnectTimeout is the spec §14 budget for connecting to the agent's
// loopback FRP proxy port. It is the same value the limiter carries as its
// configurable default.
const ConnectTimeout = limits.DefaultDialTimeout

// loopbackDialHost is the only host the gateway ever dials (spec §8, §16.2):
// frps binds agent proxy ports on 127.0.0.1 and they are unreachable from
// the public network. The dial target is built from this literal and the
// route's assigned port — never from any other route or agent state.
const loopbackDialHost = "127.0.0.1"

// streamCopyBufferSize bounds the payload buffer each copy direction holds
// while splicing a stream. Buffers are pooled; the gateway never buffers a
// complete response or file (spec §14).
const streamCopyBufferSize = 32 << 10

var streamCopyBufferPool = sync.Pool{
	New: func() any {
		buffer := make([]byte, streamCopyBufferSize)
		return &buffer
	},
}

// errRouteSuperseded reports that the exact route changed between the
// initial lookup and stream registration — revoked, dropped, or re-pointed
// at a different agent/port/generation join. The distinction is log-only;
// the browser still sees a generic close.
var errRouteSuperseded = errors.New("gateway: route changed between lookup and registration")

// helloParseFunc extracts the ClientHello SNI and owned replay prefix from a
// public connection within the parser's own bounds (64 KiB inspected, five
// seconds). Production uses clienthello.Peek; tests substitute shorter
// budgets.
type helloParseFunc func(conn net.Conn) (*clienthello.Hello, error)

// dialFunc opens the agent-side connection. Production uses a plain
// net.Dialer under the two-second connect budget; tests substitute a
// recorder to observe the exact loopback target and budget.
type dialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Server is the public L4 acceptor. It owns no listener of its own: Serve
// takes one, Close stops it, and Wait drains the accepted connections. Safe
// for concurrent use.
type Server struct {
	routes       *routes.Table
	streams      *Streams
	limits       *limits.Limiter
	limitsConfig *limits.Config
	parseHello   helloParseFunc
	dial         dialFunc
	logger       *slog.Logger
	metrics      *metrics.Registry

	mu       sync.Mutex
	listener net.Listener
	handlers sync.WaitGroup
}

// Option configures an optional Server collaborator.
type Option func(*Server)

// WithLogger overrides the structured logger (default slog.Default).
func WithLogger(logger *slog.Logger) Option {
	return func(server *Server) { server.logger = logger }
}

// WithHelloParser overrides ClientHello extraction (default: the configured
// §14 bounds via clienthello.PeekWithBounds). Tests use it to substitute
// shorter budgets; production code should not.
func WithHelloParser(parse helloParseFunc) Option {
	return func(server *Server) { server.parseHello = parse }
}

// WithDialer overrides the agent dialer. Tests use it to observe the exact
// loopback target and connect budget; production code should not.
func WithDialer(dial dialFunc) Option {
	return func(server *Server) { server.dial = dial }
}

// WithLimiter installs the §14 resource limiter (default: DefaultConfig with
// a logger-backed saturation alert). Tests install small ceilings to prove
// enforcement deterministically; an explicit limiter wins over
// WithLimitsConfig.
func WithLimiter(limiter *limits.Limiter) Option {
	return func(server *Server) {
		if limiter != nil {
			server.limits = limiter
		}
	}
}

// WithLimitsConfig installs the §14 resource bounds from the operator's
// production configuration (env and/or flags, resolved by the caller).
// NewServer builds the limiter and the ClientHello parser bounds from this
// single config, so a configured hello byte ceiling or read deadline actually
// drives clienthello.PeekWithBounds and a lowered global/FD-budget ceiling is
// enforced. An explicit WithLimiter takes precedence.
func WithLimitsConfig(config limits.Config) Option {
	return func(server *Server) { server.limitsConfig = &config }
}

// WithMetrics installs the §17.3 metadata-only registry. The default is a
// fresh relay registry, so every gateway exposes the bounded signal set; tests
// install their own to assert emissions.
func WithMetrics(registry *metrics.Registry) Option {
	return func(server *Server) {
		if registry != nil {
			server.metrics = registry
		}
	}
}

// Health is the truthful gateway health surface (spec §17.1). It tracks two
// INDEPENDENT truths that must never be conflated:
//
//   - RouteReady is true only after a route snapshot has been applied AND the
//     control sync is healthy. A gateway that has not synchronized has no
//     routes and must not pass health, even when its frps process is up.
//   - FRPSProcessHealthy reflects the FRP transport process only. A healthy
//     frps is not a routable gateway, and a routable gateway can outlive an
//     frps restart (its routes stay leased) — neither implies the other.
//
// Safe for concurrent use; the datapath setters are called from the sync loop
// and the process supervisor.
type Health struct {
	mu            sync.Mutex
	snapshotReady bool
	controlSynced bool
	frpsHealthy   bool
}

// NewHealth returns a health surface that reports not-ready until each truth
// is explicitly established. Unknown is reported as not-ready (fail closed).
func NewHealth() *Health { return &Health{} }

// SetSnapshotReady records that a full route snapshot has been applied.
func (health *Health) SetSnapshotReady(ready bool) {
	health.mu.Lock()
	defer health.mu.Unlock()
	health.snapshotReady = ready
}

// SetControlSynced records the control-sync liveness truth.
func (health *Health) SetControlSynced(synced bool) {
	health.mu.Lock()
	defer health.mu.Unlock()
	health.controlSynced = synced
}

// SetFRPSHealthy records the separate frps process truth.
func (health *Health) SetFRPSHealthy(healthy bool) {
	health.mu.Lock()
	defer health.mu.Unlock()
	health.frpsHealthy = healthy
}

// RouteReady reports the gateway's ability to route: snapshot AND control
// sync, never frps process health.
func (health *Health) RouteReady() bool {
	health.mu.Lock()
	defer health.mu.Unlock()
	return health.snapshotReady && health.controlSynced
}

// FRPSProcessHealthy reports the independent frps process truth.
func (health *Health) FRPSProcessHealthy() bool {
	health.mu.Lock()
	defer health.mu.Unlock()
	return health.frpsHealthy
}

// NewServer wires the public acceptor to the in-memory route and stream
// state. The server touches only those two synchronized structures plus the
// limits budget on a public connection — never a database (spec §8).
func NewServer(routeTable *routes.Table, streams *Streams, options ...Option) *Server {
	dialer := &net.Dialer{}
	server := &Server{
		routes:  routeTable,
		streams: streams,
		dial:    dialer.DialContext,
		logger:  slog.Default(),
	}
	for _, option := range options {
		option(server)
	}
	if server.limits == nil {
		config := limits.DefaultConfig()
		if server.limitsConfig != nil {
			config = *server.limitsConfig
		}
		config.OnSaturation = func(saturation limits.Saturation) {
			server.logger.Warn("gateway: resource saturation",
				"kind", saturation.Kind,
				"key", saturation.Key,
				"active", saturation.Active,
				"limit", saturation.Limit,
				"bytes", saturation.Bytes,
				"threshold", saturation.Threshold)
		}
		server.limits = limits.NewLimiter(config)
	}
	// The default parser is bound to the limiter's effective §14 hello limits,
	// never the compile-time constants: lowering the configured hello budget
	// must actually tighten the parser (audit A2). The closure reads the
	// installed limiter's effective config at parse time, so it stays correct
	// for a limiter supplied via WithLimitsConfig or an explicit WithLimiter.
	if server.parseHello == nil {
		server.parseHello = func(conn net.Conn) (*clienthello.Hello, error) {
			bounds := server.limits.Config()
			return clienthello.PeekWithBounds(conn, bounds.MaxHelloBytes, bounds.HelloTimeout)
		}
	}
	if server.metrics == nil {
		server.metrics = metrics.NewRegistry(metrics.Relay)
	}
	// Concurrent-connection scopes are scraped from the limiter at render
	// time. They are bounded aggregate dimensions, never per-IP/per-hostname
	// label values (spec §17.3; cardinality and end-user privacy).
	server.metrics.SetFunc("sharebridge_relay_concurrent_connections",
		func() int64 { return int64(server.limits.ActiveConnections()) }, metrics.ScopeGlobal)
	server.metrics.SetFunc("sharebridge_relay_concurrent_connections",
		func() int64 { return int64(server.limits.TrackedSourceIPs()) }, metrics.ScopeSourceIP)
	server.metrics.SetFunc("sharebridge_relay_concurrent_connections",
		func() int64 { return int64(server.limits.TrackedOrigins()) }, metrics.ScopeOrigin)
	server.metrics.SetFunc("sharebridge_relay_concurrent_connections",
		func() int64 { return int64(server.limits.TrackedAgents()) }, metrics.ScopeAgent)
	return server
}

// Serve accepts public connections on listener until it closes, handling
// each on its own goroutine. The §14 global and per-source-IP slots are
// acquired in the accept loop BEFORE the handler goroutine is spawned and
// before the 64 KiB ClientHello parse runs (audit I7), so an over-limit peer
// costs one generic close and nothing else. It returns nil after Close (or
// any direct listener close) and the acceptance error otherwise.
func (server *Server) Serve(listener net.Listener) error {
	server.mu.Lock()
	if server.listener != nil {
		server.mu.Unlock()
		return errors.New("gateway: server is already serving")
	}
	server.listener = listener
	server.mu.Unlock()

	for {
		publicConn, acceptErr := listener.Accept()
		if acceptErr != nil {
			if errors.Is(acceptErr, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("gateway: accept: %w", acceptErr)
		}
		connLease, admitErr := server.limits.AdmitConnection(publicConn.RemoteAddr())
		if admitErr != nil {
			server.recordRejection(metrics.ReasonLimits)
			server.logRejection(publicConn, "limits", admitErr, "")
			publicConn.Close()
			continue
		}
		server.handlers.Add(1)
		go func() {
			defer server.handlers.Done()
			server.handleConnection(publicConn, connLease)
		}()
	}
}

// Close stops the acceptor. It is safe to call more than once and
// concurrently with Serve; connections already accepted are unaffected —
// Wait drains them.
func (server *Server) Close() {
	server.mu.Lock()
	listener := server.listener
	server.mu.Unlock()
	if listener != nil {
		listener.Close()
	}
}

// Wait blocks until every accepted connection has finished.
func (server *Server) Wait() {
	server.handlers.Wait()
}

// handleConnection runs the §8 forwarding conditions for one public
// connection. Every exit path closes the browser connection generically —
// the browser never receives a byte on rejection — and releases every
// counter and registration taken along the way, in reverse acquisition order
// (origin, agent, source IP, global).
func (server *Server) handleConnection(publicConn net.Conn, connLease *limits.ConnectionLease) {
	defer publicConn.Close()
	defer connLease.Release()

	hello, err := server.parseHello(publicConn)
	if err != nil {
		server.recordClientHello(err)
		server.recordRejection(metrics.ReasonClientHello)
		server.logRejection(publicConn, "clienthello", err, "")
		return
	}

	route, err := server.routes.Lookup(hello.SNI)
	if err != nil {
		server.recordRejection(metrics.ReasonRoute)
		server.logRejection(publicConn, "route-lookup", err, hello.SNI)
		return
	}

	// Acquire the agent and exact-origin slots before the dial, atomically.
	streamLease, err := server.limits.AdmitStream(connLease, limits.StreamRequest{
		Hostname:            route.Hostname,
		AgentRecordID:       route.AgentRecordID,
		MaxStreamsPerOrigin: route.Limits.MaxStreamsPerOrigin,
		MaxStreamsPerAgent:  route.Limits.MaxStreamsPerAgent,
	})
	if err != nil {
		server.recordRejection(metrics.ReasonLimits)
		server.logRejection(publicConn, "limits", err, hello.SNI)
		return
	}
	defer streamLease.Release()

	agentConn, err := server.dialAgent(route)
	if err != nil {
		server.metrics.Inc("sharebridge_relay_frp_connect_failures_total")
		server.recordRejection(metrics.ReasonDial)
		server.logRejection(publicConn, "dial", err, hello.SNI)
		return
	}
	defer agentConn.Close()

	stream, admitErr := server.streams.RegisterAdmitted(hello.SNI, route.AgentRecordID, publicConn, server.admitStream(route))
	if stream == nil {
		// The route was revoked or superseded inside the lookup→register
		// window; the stream is fenced and never indexed.
		server.recordRejection(metrics.ReasonAdmission)
		server.logRejection(publicConn, "admission", admitErr, hello.SNI)
		return
	}
	defer stream.Close()

	// Replay the inspected ClientHello bytes byte-for-byte, exactly once,
	// before splicing the remaining stream (spec §8, §16.1: the bytes
	// delivered to the agent's TLS listener are identical to the browser
	// TLS stream).
	if _, err := agentConn.Write(hello.Prefix); err != nil {
		server.recordRejection(metrics.ReasonReplay)
		server.logRejection(publicConn, "replay", err, hello.SNI)
		return
	}
	streamLease.AddBytes(len(hello.Prefix))
	server.recordBytes(len(hello.Prefix))

	// The connection is fully admitted to the relay at this point: the
	// ClientHello was parsed, the exact route resolved, every §14 slot
	// acquired, the agent dialed, and the inspected prefix replayed. Only now
	// is it "accepted"; every earlier generic close is a rejection.
	server.metrics.Inc("sharebridge_relay_public_connections_total", metrics.OutcomeAccepted)

	server.pump(publicConn, agentConn, streamLease)
}

// admitStream re-verifies — at registration time, under the stream
// registry's coordination — that the route the connection was admitted
// against is still the live route for the hostname. It closes the
// lookup→register window: a route revoked, dropped, or re-pointed at a
// different agent/port/generation join after the initial lookup fences the
// not-yet-registered stream instead of letting it outlive the revoke that
// preceded its registration (spec §8 conditions 2–3).
//
// That guarantee depends on the control-side ordering invariant documented
// on Streams.RegisterAdmitted: control paths must mutate table/presence
// state (Revoke/Apply, presence expiry) before calling CloseRoute/CloseAgent
// — the drain follows, never precedes, the mutation.
func (server *Server) admitStream(dialed routes.Route) func() error {
	return func() error {
		current, err := server.routes.Lookup(dialed.Hostname)
		if err != nil {
			return err
		}
		if current.AgentRecordID != dialed.AgentRecordID ||
			current.RelayPort != dialed.RelayPort ||
			current.Generation != dialed.Generation {
			return fmt.Errorf("gateway: route for %q now joins agent %q port %d generation %d: %w",
				dialed.Hostname, current.AgentRecordID, current.RelayPort, current.Generation, errRouteSuperseded)
		}
		return nil
	}
}

// dialAgent connects to the agent's assigned FRP proxy port. The target is
// always loopback with the route's assigned relay port — never any other
// address from route or agent state — under the spec §14 two-second connect
// budget. The connect latency and failure are §17.3 signals.
func (server *Server) dialAgent(route routes.Route) (net.Conn, error) {
	address := net.JoinHostPort(loopbackDialHost, strconv.Itoa(route.RelayPort))
	dialContext, cancel := context.WithTimeout(context.Background(), server.limits.DialTimeout())
	defer cancel()
	started := time.Now()
	agentConn, err := server.dial(dialContext, "tcp", address)
	server.metrics.Observe("sharebridge_relay_frp_connect_seconds", time.Since(started).Seconds())
	if err != nil {
		return nil, fmt.Errorf("gateway: dial agent loopback proxy %s: %w", address, err)
	}
	return agentConn, nil
}

// recordRejection counts one generic public-connection close under a bounded
// reason class. The browser learns nothing (spec §8); the class is metadata
// only.
func (server *Server) recordRejection(reason string) {
	server.metrics.Inc("sharebridge_relay_public_connections_total", metrics.OutcomeRejected)
	server.metrics.Inc("sharebridge_relay_public_connection_rejections_total", reason)
}

// recordClientHello counts a ClientHello parse failure under the bounded
// timeout/oversize/malformed class.
func (server *Server) recordClientHello(err error) {
	switch {
	case errors.Is(err, clienthello.ErrTimeout):
		server.metrics.Inc("sharebridge_relay_clienthello_total", metrics.HelloTimeout)
	case errors.Is(err, clienthello.ErrTooLarge):
		server.metrics.Inc("sharebridge_relay_clienthello_total", metrics.HelloOversize)
	default:
		server.metrics.Inc("sharebridge_relay_clienthello_total", metrics.HelloMalformed)
	}
}

// recordBytes counts relayed payload bytes under the bounded global/agent/
// origin scopes. The scopes are aggregation dimensions, not identifiers: the
// gateway never labels a metric with a hostname or agent record (spec §17.3).
func (server *Server) recordBytes(count int) {
	if count <= 0 {
		return
	}
	server.metrics.Add("sharebridge_relay_relayed_bytes_total", int64(count), metrics.ScopeGlobal)
	server.metrics.Add("sharebridge_relay_relayed_bytes_total", int64(count), metrics.ScopeAgent)
	server.metrics.Add("sharebridge_relay_relayed_bytes_total", int64(count), metrics.ScopeOrigin)
}

// pump splices the two connections until either direction ends. The first
// direction to finish closes both connections so the pending copy in the
// other direction unblocks immediately: a TLS session ends as a unit and
// the MVP data plane has no TCP half-close. Neither side's bytes are
// interpreted, buffered beyond the copy window, or transformed.
func (server *Server) pump(publicConn, agentConn net.Conn, streamLease *limits.StreamLease) {
	clock := newStreamClock(server.limits.IdleTimeout(), server.limits.AbsoluteLifetime())
	var waitGroup sync.WaitGroup
	waitGroup.Add(2)
	go func() {
		defer waitGroup.Done()
		server.copyStream(agentConn, publicConn, clock, streamLease)
		agentConn.Close()
		publicConn.Close()
	}()
	go func() {
		defer waitGroup.Done()
		server.copyStream(publicConn, agentConn, clock, streamLease)
		agentConn.Close()
		publicConn.Close()
	}()
	waitGroup.Wait()
}

// streamClock tracks one stream's shared activity across both copy
// directions. The no-byte idle timeout resets on a byte in either direction;
// the absolute lifetime never resets. Methods are safe for the two copy
// goroutines to share.
type streamClock struct {
	lastActivity atomic.Int64
	absolute     time.Time
	idle         time.Duration
}

func newStreamClock(idle, lifetime time.Duration) *streamClock {
	clock := &streamClock{absolute: time.Now().Add(lifetime), idle: idle}
	clock.touch()
	return clock
}

func (clock *streamClock) touch() {
	clock.lastActivity.Store(time.Now().UnixNano())
}

func (clock *streamClock) lastTime() time.Time {
	return time.Unix(0, clock.lastActivity.Load())
}

// readDeadline returns the next read deadline: the earlier of the idle
// deadline (activity + idle) and the absolute lifetime. A false second
// result means the idle deadline already passed.
func (clock *streamClock) readDeadline(now time.Time) (time.Time, bool) {
	idleDeadline := clock.lastTime().Add(clock.idle)
	if !now.Before(idleDeadline) {
		return time.Time{}, false
	}
	if clock.absolute.Before(idleDeadline) {
		return clock.absolute, true
	}
	return idleDeadline, true
}

// expired reports whether a read timeout means the stream must close: either
// the hard absolute lifetime elapsed or no byte moved for the whole idle
// timeout. Activity in the other direction after this direction's deadline
// was set is what makes the second check necessary.
func (clock *streamClock) expired(now time.Time) bool {
	return !now.Before(clock.absolute) || !now.Before(clock.lastTime().Add(clock.idle))
}

// copyStream copies one direction with bounded pooled buffers and enforces
// the shared idle/lifetime clock (spec §14: activity is tracked without
// buffering payload; the gateway never buffers a complete response or file).
func (server *Server) copyStream(dst, src net.Conn, clock *streamClock, streamLease *limits.StreamLease) {
	bufferPointer := streamCopyBufferPool.Get().(*[]byte)
	defer streamCopyBufferPool.Put(bufferPointer)
	buffer := *bufferPointer

	for {
		deadline, withinBounds := clock.readDeadline(time.Now())
		if !withinBounds {
			return
		}
		if err := src.SetReadDeadline(deadline); err != nil {
			return
		}
		read, readErr := src.Read(buffer)
		if read > 0 {
			clock.touch()
			if _, writeErr := dst.Write(buffer[:read]); writeErr != nil {
				return
			}
			streamLease.AddBytes(read)
			server.recordBytes(read)
		}
		if readErr == nil {
			continue
		}
		if !errors.Is(readErr, os.ErrDeadlineExceeded) {
			return
		}
		// The deadline covers both bounds; re-check which one tripped before
		// closing, because the other direction may have moved the activity
		// clock while this read was blocked.
		if clock.expired(time.Now()) {
			return
		}
	}
}

// logRejection records why a public connection was closed generically. The
// browser never learns the reason (spec §8); logged fields are metadata only
// — never ClientHello bytes, credentials, headers, or payload (spec §16.6).
func (server *Server) logRejection(publicConn net.Conn, reason string, err error, sni string) {
	fields := []any{"reason", reason, "remote", publicConn.RemoteAddr()}
	if sni != "" {
		fields = append(fields, "sni", sni)
	}
	if err != nil {
		fields = append(fields, "error", err)
	}
	server.logger.Info("gateway: closed public connection", fields...)
}
