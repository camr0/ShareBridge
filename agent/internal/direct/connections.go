// agent/internal/direct/connections.go
package direct

import (
	"context"
	"net"
	"sync"
)

// connState is the per-connection accounting record (§13.2). It is created in
// ConnContext — which runs before the TLS handshake, and therefore before SNI
// admission — and gains its route information later, when the Binder has
// admitted the connection's SNI and authorized its first request. Until the
// binding is known the connection is treated as relay-like for port
// accounting (no session, no hold): only Binder-admitted direct connections
// may ever drive OnDemandPort session/hold accounting.
type connState struct {
	mu        sync.Mutex
	sessionID string    // OnDemandPort session token (direct connections only)
	route     RouteKind // Binder-admitted route kind ("" until first request)
	origin    string    // Binder-admitted origin
	share     string    // Binder-admitted native share code
}

// noteBinding records the Binder-admitted binding for this connection. The
// first authorized request wins: SNI is fixed for a connection's lifetime and
// re-admission of a changed origin is refused, so every authorized request on
// a connection carries the same binding.
func (cs *connState) noteBinding(bd Binding) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.route == "" {
		cs.route = bd.RouteKind
		cs.origin = bd.Origin
		cs.share = bd.ShareCode
	}
}

func (cs *connState) boundRoute() RouteKind {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.route
}

func (cs *connState) boundOrigin() string {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.origin
}

func (cs *connState) boundShare() string {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.share
}

// connStateKey is the context key the request context carries the connection's
// *connState under.
type connStateKey struct{}

// connStateFromContext returns the *connState recorded for the request's
// connection, or nil when the request never passed through ConnContext
// (handler-level test doubles).
func connStateFromContext(ctx context.Context) *connState {
	cs, _ := ctx.Value(connStateKey{}).(*connState)
	return cs
}

// registryEntry pairs a registered connection with its accounting record.
// closing marks a connection the registry has already closed, so repeated
// close-by-* calls never close (or count) a connection twice.
type registryEntry struct {
	cs      *connState
	closing bool
}

// connRegistry tracks every live connection together with its Binder-admitted
// route, origin, and share (§13.2). The same map backs the ConnContext /
// ConnState wiring and the close-by-route/share/all entry points that lockdown
// and revocation use. A registry close does NOT unregister the connection:
// the normal ConnState (StateClosed) path owns unregistration and session
// teardown, so a registry-initiated close tears a connection down through
// exactly the path a client disconnect would.
type connRegistry struct {
	mu    sync.Mutex
	conns map[net.Conn]*registryEntry
}

func newConnRegistry() connRegistry {
	return connRegistry{conns: map[net.Conn]*registryEntry{}}
}

// register records a newly accepted connection (called from ConnContext).
func (r *connRegistry) register(c net.Conn, cs *connState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.conns[c] = &registryEntry{cs: cs}
}

// unregister drops a closed connection and returns its accounting record
// (called from ConnState on StateClosed).
func (r *connRegistry) unregister(c net.Conn) (*connState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.conns[c]
	if !ok {
		return nil, false
	}
	delete(r.conns, c)
	return e.cs, true
}

// closeRoute closes every live connection admitted on the given route and
// returns how many were closed by this call. Connections whose route is not
// (yet) known are only closed by closeAll.
func (r *connRegistry) closeRoute(kind RouteKind) int {
	return r.closeWhere(func(cs *connState) bool { return cs.boundRoute() == kind })
}

// closeShare closes every live connection admitted for the given share code,
// on both routes together (§13.2: revocation closes both routes together).
func (r *connRegistry) closeShare(code string) int {
	return r.closeWhere(func(cs *connState) bool { return cs.boundShare() == code })
}

// closeAll closes every live connection on both routes.
func (r *connRegistry) closeAll() int {
	return r.closeWhere(func(*connState) bool { return true })
}

// closeState closes the specific connection whose accounting record is cs and
// marks it closing so repeated close calls skip it. It is the handler's
// fail-closed teardown for a request whose binding was revoked between
// authorization and registration (audit Critical #3): the revocation's
// close-by-share scan may have already run before the binding was recorded, so
// the handler must close its own connection rather than leave it usable. It
// returns whether a matching live connection was closed.
func (r *connRegistry) closeState(cs *connState) bool {
	r.mu.Lock()
	var target net.Conn
	for c, e := range r.conns {
		if e.cs == cs && !e.closing {
			e.closing = true
			target = c
			break
		}
	}
	r.mu.Unlock()
	if target == nil {
		return false
	}
	_ = target.Close()
	return true
}

// closeWhere closes the connections whose record matches. Matched entries are
// marked closing before Close runs (outside the registry lock, so the async
// ConnState teardown can take it) and are skipped by later close calls.
func (r *connRegistry) closeWhere(match func(*connState) bool) int {
	r.mu.Lock()
	var targets []net.Conn
	for c, e := range r.conns {
		if e.closing || !match(e.cs) {
			continue
		}
		e.closing = true
		targets = append(targets, c)
	}
	r.mu.Unlock()
	for _, c := range targets {
		_ = c.Close()
	}
	return len(targets)
}

// CloseRouteConns closes every established connection admitted on the given
// route (§13.2) and returns how many were closed. Lockdown uses it with
// RouteDirect and RouteRelay; connections whose route is not yet known (no
// authorized request served) are not matched — close-all or listener teardown
// covers them.
func (s *DirectServer) CloseRouteConns(route RouteKind) int {
	return s.conns.closeRoute(route)
}

// CloseShareConns closes every established connection admitted for the share
// code, on both routes together (revocation), and returns how many were
// closed.
func (s *DirectServer) CloseShareConns(code string) int {
	return s.conns.closeShare(code)
}

// CloseAllConns closes every established connection on both routes and
// returns how many were closed.
func (s *DirectServer) CloseAllConns() int {
	return s.conns.closeAll()
}

// CloseRecipientConns closes every established recipient connection admitted
// on EITHER route kind — the §13.4 step 5 lockdown close — and returns how
// many were closed. Connections whose route is not yet known (no authorized
// request served) are not matched; the lockdown listener teardown covers
// them.
func (s *DirectServer) CloseRecipientConns() int {
	return s.CloseRouteConns(RouteDirect) + s.CloseRouteConns(RouteRelay)
}
