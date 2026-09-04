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
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"sharebridge/relay/internal/clienthello"
	"sharebridge/relay/internal/routes"
)

// ConnectTimeout is the spec §14 budget for connecting to the agent's
// loopback FRP proxy port.
const ConnectTimeout = 2 * time.Second

// loopbackDialHost is the only host the gateway ever dials (spec §8, §16.2):
// frps binds agent proxy ports on 127.0.0.1 and they are unreachable from
// the public network. The dial target is built from this literal and the
// route's assigned port — never from any other route or agent state.
const loopbackDialHost = "127.0.0.1"

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

// StreamLimiter is the §14 concurrency-admission seam. Task 32 replaces the
// placeholder below with the enforcing limiter — global → source IP → agent
// → origin, acquired before the agent dial and released on every exit. The
// interface fixes the server's call sites today so enforcement lands without
// touching the data plane again.
type StreamLimiter interface {
	AcquireStream(admission StreamAdmission) (*StreamLease, error)
}

// StreamAdmission describes one connection asking for a stream slot.
type StreamAdmission struct {
	// Hostname is the exact route hostname the connection resolved to.
	Hostname string
	// AgentRecordID is the agent record serving the route.
	AgentRecordID string
	// RemoteAddr is the public peer address, for the future per-source-IP
	// ceiling (spec §14).
	RemoteAddr net.Addr
}

// StreamLease is one held stream slot. Release must run on every exit path
// (spec §14: counters are decremented on all close/error paths).
type StreamLease struct{}

// Release releases the slot. The Task 4 placeholder holds nothing; the
// enforcing Task 32 limiter returns leases that decrement real counters.
func (lease *StreamLease) Release() {}

// unboundedLimiter is the Task 4 placeholder limiter: every stream is
// admitted and no counter is held. Task 32 swaps in the enforcing
// implementation through WithStreamLimiter.
type unboundedLimiter struct{}

func (unboundedLimiter) AcquireStream(admission StreamAdmission) (*StreamLease, error) {
	return &StreamLease{}, nil
}

// Server is the public L4 acceptor. It owns no listener of its own: Serve
// takes one, Close stops it, and Wait drains the accepted connections. Safe
// for concurrent use.
type Server struct {
	routes     *routes.Table
	streams    *Streams
	limits     StreamLimiter
	parseHello helloParseFunc
	dial       dialFunc
	logger     *slog.Logger

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

// WithHelloParser overrides ClientHello extraction (default clienthello.Peek).
// Tests use it to shorten the read budget; production code should not.
func WithHelloParser(parse helloParseFunc) Option {
	return func(server *Server) { server.parseHello = parse }
}

// WithDialer overrides the agent dialer. Tests use it to observe the exact
// loopback target and connect budget; production code should not.
func WithDialer(dial dialFunc) Option {
	return func(server *Server) { server.dial = dial }
}

// WithStreamLimiter overrides the §14 admission seam (default: the
// unbounded placeholder). Task 32 installs the enforcing limiter here.
func WithStreamLimiter(limiter StreamLimiter) Option {
	return func(server *Server) { server.limits = limiter }
}

// NewServer wires the public acceptor to the in-memory route and stream
// state. The server touches only those two synchronized structures plus the
// limits seam on a public connection — never a database (spec §8).
func NewServer(routeTable *routes.Table, streams *Streams, options ...Option) *Server {
	dialer := &net.Dialer{}
	server := &Server{
		routes:     routeTable,
		streams:    streams,
		limits:     unboundedLimiter{},
		parseHello: clienthello.Peek,
		dial:       dialer.DialContext,
		logger:     slog.Default(),
	}
	for _, option := range options {
		option(server)
	}
	return server
}

// Serve accepts public connections on listener until it closes, handling
// each on its own goroutine. It returns nil after Close (or any direct
// listener close) and the acceptance error otherwise.
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
		server.handlers.Add(1)
		go func() {
			defer server.handlers.Done()
			server.handleConnection(publicConn)
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
// counter and registration taken along the way.
func (server *Server) handleConnection(publicConn net.Conn) {
	defer publicConn.Close()

	hello, err := server.parseHello(publicConn)
	if err != nil {
		server.logRejection(publicConn, "clienthello", err, "")
		return
	}

	route, err := server.routes.Lookup(hello.SNI)
	if err != nil {
		server.logRejection(publicConn, "route-lookup", err, hello.SNI)
		return
	}

	lease, err := server.limits.AcquireStream(StreamAdmission{
		Hostname:      hello.SNI,
		AgentRecordID: route.AgentRecordID,
		RemoteAddr:    publicConn.RemoteAddr(),
	})
	if err != nil {
		server.logRejection(publicConn, "limits", err, hello.SNI)
		return
	}
	defer lease.Release()

	agentConn, err := server.dialAgent(route)
	if err != nil {
		server.logRejection(publicConn, "dial", err, hello.SNI)
		return
	}
	defer agentConn.Close()

	stream, admitErr := server.streams.RegisterAdmitted(hello.SNI, route.AgentRecordID, publicConn, server.admitStream(route))
	if stream == nil {
		// The route was revoked or superseded inside the lookup→register
		// window; the stream is fenced and never indexed.
		server.logRejection(publicConn, "admission", admitErr, hello.SNI)
		return
	}
	defer stream.Close()

	// Replay the inspected ClientHello bytes byte-for-byte, exactly once,
	// before splicing the remaining stream (spec §8, §16.1: the bytes
	// delivered to the agent's TLS listener are identical to the browser
	// TLS stream).
	if _, err := agentConn.Write(hello.Prefix); err != nil {
		server.logRejection(publicConn, "replay", err, hello.SNI)
		return
	}

	server.pump(publicConn, agentConn)
}

// admitStream re-verifies — at registration time, under the stream
// registry's coordination — that the route the connection was admitted
// against is still the live route for the hostname. It closes the
// lookup→register window: a route revoked, dropped, or re-pointed at a
// different agent/port/generation join after the initial lookup fences the
// not-yet-registered stream instead of letting it outlive the revoke that
// preceded its registration (spec §8 conditions 2–3).
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
// budget.
func (server *Server) dialAgent(route routes.Route) (net.Conn, error) {
	address := net.JoinHostPort(loopbackDialHost, strconv.Itoa(route.RelayPort))
	dialContext, cancel := context.WithTimeout(context.Background(), ConnectTimeout)
	defer cancel()
	agentConn, err := server.dial(dialContext, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("gateway: dial agent loopback proxy %s: %w", address, err)
	}
	return agentConn, nil
}

// pump splices the two connections until either direction ends. The first
// direction to finish closes both connections so the pending copy in the
// other direction unblocks immediately: a TLS session ends as a unit and
// the MVP data plane has no TCP half-close. Neither side's bytes are
// interpreted, buffered beyond the copy window, or transformed.
func (server *Server) pump(publicConn, agentConn net.Conn) {
	var waitGroup sync.WaitGroup
	waitGroup.Add(2)
	go func() {
		defer waitGroup.Done()
		_, _ = io.Copy(agentConn, publicConn)
		agentConn.Close()
		publicConn.Close()
	}()
	go func() {
		defer waitGroup.Done()
		_, _ = io.Copy(publicConn, agentConn)
		agentConn.Close()
		publicConn.Close()
	}()
	waitGroup.Wait()
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
