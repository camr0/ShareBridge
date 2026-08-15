// agent/internal/direct/sni.go
package direct

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
)

// RouteKind distinguishes the two DNS namespaces (spec §7): the clean
// <origin>.<ns>.sharebridgeusercontent.com namespace (direct) and the
// <origin>.relay.<ns>.sharebridgeusercontent.com namespace (relay).
type RouteKind string

const (
	RouteDirect RouteKind = "direct"
	RouteRelay  RouteKind = "relay"
)

// Binding is an active origin→share authorization: an exact, normalized origin
// hostname bound to its route kind and the native share code that authorizes
// content for it (spec §11).
type Binding struct {
	Origin    string
	RouteKind RouteKind
	ShareCode string
}

var (
	ErrNoSNI          = errors.New("direct: empty or invalid SNI")
	ErrUnknownOrigin  = errors.New("direct: unknown or revoked origin")
	ErrHostMismatch   = errors.New("direct: Host does not match the admitted origin")
	ErrBadHost        = errors.New("direct: malformed or duplicate Host header")
	ErrWrongRouteKind = errors.New("direct: route kind mismatch")
	ErrWrongCode      = errors.New("direct: share code mismatch")
)

// Binder enforces strict origin→share binding at the agent. TLS admission
// (SNI) is separate from HTTP authorization (Host + route kind + share code):
// an unknown SNI is rejected during the handshake, before any HTTP is read;
// a known SNI is then authorized per-request against Host, route kind, and the
// native share code.
type Binder struct {
	mu           sync.RWMutex
	namespace    string
	directSuffix string
	relaySuffix  string
	active       map[string]Binding
}

func NewBinder(namespace, baseDomain string) *Binder {
	return &Binder{
		namespace:    namespace,
		directSuffix: "." + namespace + "." + baseDomain,
		relaySuffix:  ".relay." + namespace + "." + baseDomain,
		active:       map[string]Binding{},
	}
}

// routeKindFor classifies a normalized origin into its namespace route kind.
func (b *Binder) routeKindFor(host string) (RouteKind, bool) {
	switch {
	case strings.HasSuffix(host, b.relaySuffix):
		return RouteRelay, true
	case strings.HasSuffix(host, b.directSuffix):
		return RouteDirect, true
	default:
		return "", false
	}
}

// Allow registers an origin→share binding. It refuses origins outside the
// agent's two namespaces and refuses a route kind that does not match the
// origin's namespace (belt-and-suspenders; the request path re-checks too).
func (b *Binder) Allow(origin string, kind RouteKind, shareCode string) error {
	host := normalizeHost(origin)
	if !validHostname(host) {
		return fmt.Errorf("direct: invalid origin %q", origin)
	}
	k, ok := b.routeKindFor(host)
	if !ok {
		return fmt.Errorf("direct: origin %q is not in the agent namespace %q", origin, b.namespace)
	}
	if k != kind {
		return fmt.Errorf("%w: %q is %s, not %s", ErrWrongRouteKind, host, k, kind)
	}
	b.mu.Lock()
	b.active[host] = Binding{Origin: host, RouteKind: kind, ShareCode: shareCode}
	b.mu.Unlock()
	return nil
}

func (b *Binder) Revoke(origin string) {
	host := normalizeHost(origin)
	b.mu.Lock()
	delete(b.active, host)
	b.mu.Unlock()
}

// AdmitSNI is TLS admission: the ClientHello SNI must be a non-empty, exact,
// active origin. Rejecting here fails the handshake before any HTTP is read.
func (b *Binder) AdmitSNI(serverName string) (Binding, error) {
	host := normalizeHost(serverName)
	if host == "" || !validHostname(host) {
		return Binding{}, ErrNoSNI
	}
	b.mu.RLock()
	bd, ok := b.active[host]
	b.mu.RUnlock()
	if !ok {
		return Binding{}, fmt.Errorf("%w: %q", ErrUnknownOrigin, serverName)
	}
	return bd, nil
}

// Authorize performs HTTP authorization for a connection already admitted by
// AdmitSNI: Host must equal the admitted origin (ignoring case, trailing dot,
// and port), the route kind must match the Host's namespace, and the request
// path must carry the binding's native share code.
func (b *Binder) Authorize(bd Binding, r *http.Request) error {
	host, ok := normalizeRequestHost(r)
	if !ok {
		return ErrBadHost
	}
	if host != bd.Origin {
		return fmt.Errorf("%w: Host %q, admitted origin %q", ErrHostMismatch, host, bd.Origin)
	}
	if k, _ := b.routeKindFor(host); k != bd.RouteKind {
		return fmt.Errorf("%w: %q is %s, binding is %s", ErrWrongRouteKind, host, k, bd.RouteKind)
	}
	code, ok := shareCodeFromPath(r)
	if !ok || code != bd.ShareCode {
		return ErrWrongCode
	}
	return nil
}

// TLSConfig returns a tls.Config whose GetConfigForClient performs SNI
// admission. Unknown/empty SNI aborts the handshake; known SNI proceeds.
func (b *Binder) TLSConfig() *tls.Config {
	return &tls.Config{
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			if _, err := b.AdmitSNI(hello.ServerName); err != nil {
				return nil, err
			}
			return nil, nil
		},
	}
}

// Handler wraps next with HTTP authorization, re-deriving the admitted binding
// from the connection's SNI (r.TLS.ServerName). Wire it as:
//
//	server := &http.Server{Handler: binder.Handler(actualHandler)}
//	server.TLSConfig = binder.TLSConfig()
func (b *Binder) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sni := ""
		if r.TLS != nil {
			sni = r.TLS.ServerName
		}
		bd, err := b.AdmitSNI(sni)
		if err != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if err := b.Authorize(bd, r); err != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// normalizeHost lowercases, strips a trailing dot, and strips an optional
// ":port" so Host and SNI compare on the bare hostname.
func normalizeHost(h string) string {
	h = strings.TrimSpace(h)
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

// validHostname accepts only lowercase DNS labels (letters, digits, hyphen,
// dot). This rejects whitespace, underscores, and other malformed Host values.
func validHostname(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '.':
		default:
			return false
		}
	}
	return true
}

// normalizeRequestHost extracts a single, well-formed Host value from the
// request, falling back to r.Host (HTTP/2 :authority) when no Host header is
// present, and rejecting duplicate Host headers.
func normalizeRequestHost(r *http.Request) (string, bool) {
	vals := r.Header.Values("Host")
	switch len(vals) {
	case 0:
		if r.Host == "" {
			return "", false
		}
		h := normalizeHost(r.Host)
		if !validHostname(h) {
			return "", false
		}
		return h, true
	case 1:
		h := normalizeHost(vals[0])
		if !validHostname(h) {
			return "", false
		}
		return h, true
	default:
		return "", false
	}
}

// shareCodeFromPath extracts the native share code from a /s/<code> path.
func shareCodeFromPath(r *http.Request) (string, bool) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) >= 2 && parts[0] == "s" && parts[1] != "" {
		return parts[1], true
	}
	return "", false
}
