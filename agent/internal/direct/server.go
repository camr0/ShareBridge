// agent/internal/direct/server.go
package direct

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"sharebridge/agent/internal/config"
)

// CertProvider supplies the current serving certificate. The cert Manager
// (see manager) implements this; a non-nil error aborts the TLS handshake.
type CertProvider interface {
	Certificate() (*tls.Certificate, error)
}

// SessionTracker is the connection-level session accounting the DirectServer
// drives: BeginSession on the first request of a connection, Activity on each
// subsequent request, and EndSession when the connection closes. OnDemandPort
// implements it (the idle-close deadline is owned by active sessions).
type SessionTracker interface {
	BeginSession(shareID string) (string, error)
	Activity(sessionID string)
	EndSession(sessionID string)
}

// HoldTracker optionally extends SessionTracker with an in-flight hold that
// pauses the on-demand port's idle close for the lifetime of a long streaming
// response. Begin is taken on the first body write and returns a token bound to
// the current open epoch; End(token) is released when the stream completes or
// the client disconnects, and is ignored if the token is stale (from a prior
// open epoch). A SessionTracker that does not implement HoldTracker simply
// never pauses (the streaming wrapper is skipped). OnDemandPort implements
// both.
type HoldTracker interface {
	Begin() uint64
	End(token uint64)
}

// DirectServer serves the native-HTTPS "direct" data path (placeholder content
// plus a reachability probe) on an on-demand port. It composes the Binder (SNI
// admission + per-request authorization), a CertProvider (the current serving
// certificate), a SessionTracker (per-connection session accounting), and a
// SignalGate (probe nonce verification).
type DirectServer struct {
	namespace       string
	baseDomain      string
	port            SessionTracker
	certs           CertProvider
	gate            *SignalGate
	maxContentBytes int64
	binder          *Binder
	resolver        Resolver

	// connectOrigin is the single cross-origin caller of the direct path's
	// /connect reachability check (§9.3): the control-hosted interstitial
	// origin. The endpoint never reflects a request-supplied Origin; the
	// value is resolved once at construction from CONNECT_ALLOWED_ORIGIN via
	// config.ResolveConnectAllowedOrigin, failing closed to the production
	// default (https://sharebridge.app) when unset or malformed.
	connectOrigin string

	// globalStreams bounds the total number of simultaneous streaming
	// responses (asset/video/archive parts) across all shares (§11). A nil
	// gate means unlimited.
	globalStreams *streamGate

	// testHookAfterAuthorize is a test seam, nil in production: when set the
	// Handler runs it after Binder authorization and before the admitted
	// binding is committed to the connection. Tests use it to land a
	// revocation exactly in the authorization→registration window (audit
	// Critical #3) deterministically, without sleeping. It is claimed (and
	// cleared) by the first authorized request, so it is genuinely one-shot.
	testHookAfterAuthorize func()
	testHookMu             sync.Mutex

	// conns is the connection registry: each raw net.Conn mapped to its
	// per-connection *connState carrying the Binder-admitted route/origin/share
	// and the port session token. The key is the exact conn handed to
	// ConnContext and later to ConnState on StateClosed — the same object for
	// the connection's entire lifetime (with ServeTLS the listener is
	// tls-wrapped at Accept, so c.rwc is never reassigned). The registry's
	// close-by-route/share/all entry points (lockdown/revocation, §13.2) are
	// exposed on DirectServer below; a field (rather than a package-level map)
	// keeps state per server instance.
	conns connRegistry
}

func NewDirectServer(namespace, baseDomain string, port SessionTracker, certs CertProvider, gate *SignalGate, maxContentBytes int64) *DirectServer {
	return NewDirectServerWithBinder(namespace, baseDomain, port, certs, gate, maxContentBytes, NewBinder(namespace, baseDomain))
}

// NewDirectServerWithBinder is NewDirectServer with an explicit, SHARED binder.
// The daemon uses it so the binder it populates via bindOrigin is the same
// binder the server consults for SNI admission and HTTP authorization; a
// split-binder would reject every direct handshake as an unknown origin.
func NewDirectServerWithBinder(namespace, baseDomain string, port SessionTracker, certs CertProvider, gate *SignalGate, maxContentBytes int64, binder *Binder) *DirectServer {
	return &DirectServer{
		namespace: namespace, baseDomain: baseDomain, port: port, certs: certs, gate: gate,
		maxContentBytes: maxContentBytes, binder: binder,
		connectOrigin: config.ResolveConnectAllowedOrigin(),
		globalStreams: newStreamGate(globalStreamLimit),
		conns:         newConnRegistry(),
	}
}

func (s *DirectServer) Binder() *Binder { return s.binder }

// SetTestHookAfterAuthorize installs a ONE-SHOT test seam invoked after Binder
// authorization and before the connection's binding is recorded. It exists so
// cross-package tests can land a revocation in the authorization→registration
// window; production never sets it. The first authorized request claims and
// clears the seam (see claimTestHookAfterAuthorize), so later requests do not
// run it.
func (s *DirectServer) SetTestHookAfterAuthorize(fn func()) {
	s.testHookMu.Lock()
	s.testHookAfterAuthorize = fn
	s.testHookMu.Unlock()
}

// claimTestHookAfterAuthorize returns the installed seam and clears it in one
// step, making the seam genuinely one-shot: only the request that claims it
// runs it, even when several requests are in flight.
func (s *DirectServer) claimTestHookAfterAuthorize() func() {
	s.testHookMu.Lock()
	fn := s.testHookAfterAuthorize
	s.testHookAfterAuthorize = nil
	s.testHookMu.Unlock()
	return fn
}

// SetConnectAllowedOrigin overrides the construction-time resolved allowed
// origin. The daemon calls it when building the server so the resolved
// config.json connect_allowed_origin reaches the §9.3 connect check —
// construction-time resolution alone only sees the environment variable. The
// value must be a well-formed origin (the same validation the config loader
// applies); anything malformed is rejected and the previous origin is kept,
// failing closed.
func (s *DirectServer) SetConnectAllowedOrigin(origin string) {
	if err := config.ValidateConnectOrigin(origin); err != nil {
		log.Printf("direct: reject invalid connect allowed origin: %v", err)
		return
	}
	s.connectOrigin = origin
}

// SetResolver installs the share-code resolver consulted for content routes.
// It is a setter (not a constructor argument) so the daemon can wire the
// registry after the server is built. When no resolver is set, content routes
// fail closed with 404.
func (s *DirectServer) SetResolver(r Resolver) {
	s.resolver = r
}

// TLSConfig returns a tls.Config whose GetConfigForClient performs SNI
// admission and, on success, returns the current serving certificate. An
// unknown/empty SNI aborts the handshake before any HTTP is read.
func (s *DirectServer) TLSConfig() *tls.Config {
	return &tls.Config{
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			if _, err := s.binder.AdmitSNI(hello.ServerName); err != nil {
				return nil, err
			}
			cert, err := s.certs.Certificate()
			if err != nil {
				return nil, fmt.Errorf("no certificate: %w", err)
			}
			return &tls.Config{
				Certificates: []tls.Certificate{*cert},
				// The config returned from GetConfigForClient governs the
				// whole handshake, ALPN included: without NextProtos every
				// connection would be silently downgraded to HTTP/1.1 and no
				// HTTP/2 streams could be served (§9.4 direct/relay
				// coexistence runs on h2 streams too).
				NextProtos: alpnProtos(hello.SupportedProtos),
			}, nil
		},
	}
}

// alpnPreference is the server's ALPN preference order; the offered protocols
// are served in this order, so an h2-capable client always negotiates h2.
var alpnPreference = []string{"h2", "http/1.1"}

// alpnProtos intersects the client's offered ALPN protocols with the server's
// preference, for the per-handshake config GetConfigForClient returns.
func alpnProtos(offered []string) []string {
	out := make([]string, 0, len(alpnPreference))
	for _, want := range alpnPreference {
		if slices.Contains(offered, want) {
			out = append(out, want)
		}
	}
	return out
}

// Handler returns the direct path wrapped in the binder's HTTP authorization
// plus the §13.2 route note: after SNI admission and HTTP authorization
// succeed, the admitted binding (route kind, origin, share) is recorded on
// the connection's accounting state, before any handler (and therefore any
// port session/hold accounting) runs. A connection that never serves an
// authorized request keeps an unknown route and is treated as relay-like for
// port accounting.
func (s *DirectServer) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sni := ""
		if r.TLS != nil {
			sni = r.TLS.ServerName
		}
		bd, err := s.binder.AdmitSNI(sni)
		if err != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if err := s.binder.Authorize(bd, r); err != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		cs := connStateFromContext(r.Context())
		if fn := s.claimTestHookAfterAuthorize(); fn != nil {
			fn()
		}
		// Commit the admitted binding to the connection FIRST, so the T29
		// close-by-share scan can find a connection whose request passed
		// authorization. Then revalidate that this exact Binder entry is still
		// active at the same generation before any dispatch.
		//
		// Linearization (audit Critical #3): revocation mutates the Binder and
		// only then closes the share's connections. noteBinding happens-before
		// the revalidation here. Therefore either the revocation's Binder
		// mutation precedes revalidation (revalidation fails, we close the
		// connection and serve nothing), or it follows it, in which case it also
		// follows noteBinding and the subsequent close-by-share scan is
		// guaranteed to match this connection. A revoked share can never start
		// serving after revocation.
		if cs != nil {
			cs.noteBinding(bd)
		}
		if !s.binder.Revalidate(bd) {
			if cs != nil {
				s.conns.closeState(cs)
			}
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		s.route(w, r)
	})
}

func (s *DirectServer) route(w http.ResponseWriter, r *http.Request) {
	// The interstitial's CORS reachability check (§9.3) is dispatched before
	// the shared GET/HEAD gate because it owns its method policy (GET only ⇒
	// 404): a 405 would be neither 403 nor 404 and would leak the endpoint's
	// existence on non-GET verbs.
	if code, ok := shareCodeFromPath(r); ok && strings.TrimPrefix(r.URL.Path, "/s/"+code) == "/connect" {
		s.handleConnect(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	code, ok := shareCodeFromPath(r)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/s/"+code)
	switch {
	case rest == "/static/" || strings.HasPrefix(rest, "/static/"):
		// Code-scoped static UI assets. These are the same for every share and
		// do not require content resolution (the Binder has already authorized
		// the code against the admitted origin).
		s.handleStatic(w, r, rest)
		return
	case rest == "/probe" || strings.HasPrefix(rest, "/probe?"):
		// The reachability probe is a control-plane liveness check, not a
		// recipient session, so it must not participate in activity tracking
		// or content resolution.
		s.handleProbe(w, r, code)
		return
	case rest == "/" || rest == "":
		if _, ok := s.resolveContent(w, code); !ok {
			return
		}
		s.activity(w, r, code)
		s.handlePage(w, r, code)
	case rest == "/items":
		s.handleItems(w, r, code)
		return
	case rest == "/download":
		if _, ok := s.resolveContent(w, code); !ok {
			return
		}
		s.activity(w, r, code)
		s.handleDownload(w, r)
	case strings.HasPrefix(rest, "/thumb/"):
		if id, ok := contentID(rest, "/thumb/"); ok {
			s.handleThumb(w, r, code, id)
		} else {
			http.Error(w, "not found", http.StatusNotFound)
		}
		return
	case strings.HasPrefix(rest, "/preview/"):
		if id, ok := contentID(rest, "/preview/"); ok {
			s.handlePreview(w, r, code, id)
		} else {
			http.Error(w, "not found", http.StatusNotFound)
		}
		return
	case strings.HasPrefix(rest, "/asset/"):
		assetRest := strings.TrimPrefix(rest, "/asset/")
		if id, found := strings.CutSuffix(assetRest, "/playback"); found {
			if id == "" || id == "." || id == ".." || strings.Contains(id, "/") {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			s.handlePlayback(w, r, code, id)
			return
		}
		if id, ok := contentID(assetRest, ""); ok {
			s.handleAsset(w, r, code, id)
		} else {
			http.Error(w, "not found", http.StatusNotFound)
		}
		return
	case rest == "/archive":
		s.handleArchiveManifest(w, r, code)
		return
	case strings.HasPrefix(rest, "/archive/"):
		seg := strings.TrimPrefix(rest, "/archive/")
		token, partStr, found := strings.Cut(seg, "/")
		if !found || token == "" || partStr == "" || len(token) > 128 {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		part, err := strconv.Atoi(partStr)
		if err != nil || part < 0 {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		s.handleArchivePart(w, r, code, token, part)
		return
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// resolveContent resolves the share code to its content session, writing the
// mapped error response (404 unknown / 403 forbidden / 503 unready) and
// returning ok=false when the request must not proceed. A nil resolver fails
// closed with 404.
func (s *DirectServer) resolveContent(w http.ResponseWriter, code string) (*ContentSession, bool) {
	if s.resolver == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return nil, false
	}
	session, err := s.resolver.Resolve(code)
	if err != nil {
		switch {
		case errors.Is(err, ErrUnknown):
			http.Error(w, "not found", http.StatusNotFound)
		case errors.Is(err, ErrForbidden):
			http.Error(w, "forbidden", http.StatusForbidden)
		case errors.Is(err, ErrUnready):
			http.Error(w, "content not ready", http.StatusServiceUnavailable)
		default:
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
		return nil, false
	}
	if session == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return nil, false
	}
	return session, true
}

// handleProbe serves the reachability probe. It verifies the nonce was
// recently admitted for this share and echoes it on success, else 403.
func (s *DirectServer) handleProbe(w http.ResponseWriter, r *http.Request, code string) {
	nonce := r.URL.Query().Get("nonce")
	if nonce == "" || s.gate == nil || !s.gate.VerifyNonce(nonce, code) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	io.WriteString(w, nonce)
}

func (s *DirectServer) handlePage(w http.ResponseWriter, r *http.Request, code string) {
	page, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	// The embedded page uses a <base href="/s/__CODE__/"> placeholder so its
	// relative asset references resolve under the code-prefixed namespace.
	_, _ = w.Write(bytes.ReplaceAll(page, []byte("__CODE__"), []byte(code)))
}

// handleStatic serves one embedded static UI asset under /s/{code}/static/….
// The name is strictly a single relative path under the embedded static/ tree;
// dot segments and absolute paths are rejected so a traversal never escapes
// the embed.FS.
func (s *DirectServer) handleStatic(w http.ResponseWriter, r *http.Request, rest string) {
	name := strings.TrimPrefix(rest, "/static/")
	if name == "" || name == "." || name == ".." || strings.Contains(name, "..") ||
		strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	f, err := staticFS.Open("static/" + name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	setSecurityHeaders(w)
	http.ServeContent(w, r, name, info.ModTime(), rs)
}

func (s *DirectServer) handleDownload(w http.ResponseWriter, r *http.Request) {
	size := int64(1 << 20)
	if v := r.URL.Query().Get("size"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 && n <= s.maxContentBytes {
			size = n
		}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="sharebridge.bin"`)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	dst, release := s.holdStream(r, w)
	defer release()
	io.CopyN(dst, zeroReader{}, size)
}

// holdStream wraps dst so the on-demand port pauses its idle close for the
// duration of a long streaming response on a DIRECT connection: Begin fires on
// the first write and End (via the returned release func, which the caller
// must defer) releases it on completion, error, or the panic that aborts a
// mid-stream failure — including a client disconnect, which surfaces as a
// write error and unwinds the same defer. Relay-routed connections (and
// connections of unknown route) never hold the port: relay traffic must not
// keep the home mapping open (§13.2). When the connection is not direct, or
// the port does not support holds, dst is returned unchanged and release is a
// no-op.
func (s *DirectServer) holdStream(r *http.Request, dst io.Writer) (io.Writer, func()) {
	cs := connStateFromContext(r.Context())
	if cs == nil || cs.boundRoute() != RouteDirect {
		return dst, func() {}
	}
	if h, ok := s.port.(HoldTracker); ok {
		hw := &holdWriter{Writer: dst, h: h}
		return hw, func() { _ = hw.Close() }
	}
	return dst, func() {}
}

// holdWriter wraps a streaming response body so a long transfer holds the
// on-demand port open from the first byte until it is closed. Begin and End
// are each applied at most once regardless of the write/close pattern, and the
// Begin token is captured so a deferred End after a port reopen is ignored.
type holdWriter struct {
	io.Writer
	beginOnce sync.Once
	endOnce   sync.Once
	token     atomic.Uint64
	h         HoldTracker
}

func (hw *holdWriter) Write(p []byte) (int, error) {
	hw.beginOnce.Do(func() { hw.token.Store(hw.h.Begin()) })
	return hw.Writer.Write(p)
}

func (hw *holdWriter) Close() error {
	hw.endOnce.Do(func() { hw.h.End(hw.token.Load()) })
	return nil
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// activity ties each request to a connection-scoped session: BeginSession on
// the first request of a connection, Activity on subsequent ones. EndSession
// is driven by ConnState (StateClosed). Only DIRECT connections drive the
// port's session accounting (§13.2): relay connections must never begin,
// renew, or hold the home mapping, so their requests skip the tracker. The
// conn's mutex guards sessionID so a connection (an HTTP/2 connection with
// concurrent streams) can't double-begin or race a close.
func (s *DirectServer) activity(w http.ResponseWriter, r *http.Request, code string) {
	if s.port == nil {
		return
	}
	cs := connStateFromContext(r.Context())
	if cs == nil || cs.boundRoute() != RouteDirect {
		return
	}
	cs.mu.Lock()
	if cs.sessionID == "" {
		if sid, err := s.port.BeginSession(code); err == nil {
			cs.sessionID = sid
		}
	} else {
		s.port.Activity(cs.sessionID)
	}
	cs.mu.Unlock()
}

// Server resource limits (§11): explicit header/keep-alive timeouts and the
// streaming semaphore capacities.
const (
	// readHeaderTimeout bounds how long a client may take to send request
	// headers (slowloris protection).
	readHeaderTimeout = 10 * time.Second
	// idleTimeout bounds keep-alive connections between requests. The
	// on-demand port's own idle close (§4.7 hold) governs the mapping; this
	// only bounds the HTTP keep-alive.
	idleTimeout = 120 * time.Second
	// maxHeaderBytes bounds the total size of request headers.
	maxHeaderBytes = 1 << 20

	// perShareStreamLimit bounds simultaneous streaming responses per share.
	perShareStreamLimit = 4
	// globalStreamLimit bounds simultaneous streaming responses across shares.
	globalStreamLimit = 32
)

// streamGate is a counting semaphore that admits at most n simultaneous
// streaming responses and fails fast (rather than blocking) when saturated. A
// nil gate (or one built with a non-positive limit) is unlimited: tryAcquire
// always succeeds and release is a no-op.
type streamGate struct {
	slots chan struct{}
}

// newStreamGate returns a gate with capacity n. A non-positive n yields a nil
// (unlimited) gate.
func newStreamGate(n int) *streamGate {
	if n <= 0 {
		return nil
	}
	return &streamGate{slots: make(chan struct{}, n)}
}

// tryAcquire attempts to take a slot without blocking. A nil receiver is
// unlimited and always succeeds.
func (g *streamGate) tryAcquire() bool {
	if g == nil {
		return true
	}
	select {
	case g.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// release returns a slot. It is a no-op on a nil receiver.
func (g *streamGate) release() {
	if g == nil {
		return
	}
	<-g.slots
}

// newHTTPServer builds the fully-wired http.Server: Handler and TLSConfig, plus
// ConnContext/ConnState that connect each net.Conn to its *connState in the
// connection registry.
//
// Wiring (ordering matters): Go's Serve calls ConnContext BEFORE the TLS
// handshake (and before ConnState's StateNew), so the *connState must be
// created and registered in ConnContext — not in StateNew — and it cannot
// carry the SNI-derived route yet (the route is noted by Handler once the
// first request is authorized). ConnContext registers it under the raw
// net.Conn in the registry and returns a context carrying it; the handler's
// activity reads it from r.Context(); ConnState(StateClosed) unregisters it
// from the registry and ends the port session. HTTP/2 connections run through
// the same hooks (ConnContext supplies their base context; the h2 server
// leaves StateNew/StateClosed to net/http), so one registry serves both
// protocols and both routes.
func (s *DirectServer) newHTTPServer() *http.Server {
	return &http.Server{
		Handler:           s.Handler(),
		TLSConfig:         s.TLSConfig(),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			cs := &connState{}
			s.conns.register(c, cs)
			return context.WithValue(ctx, connStateKey{}, cs)
		},
		ConnState: func(c net.Conn, st http.ConnState) {
			if st != http.StateClosed {
				return
			}
			cs, ok := s.conns.unregister(c)
			if !ok {
				return
			}
			cs.mu.Lock()
			sid := cs.sessionID
			cs.sessionID = ""
			cs.mu.Unlock()
			if sid != "" && s.port != nil {
				s.port.EndSession(sid)
			}
		},
	}
}

// Start listens on listenAddr and serves the direct HTTPS path until ctx is
// cancelled. The certificate is supplied per-handshake by TLSConfig's
// GetConfigForClient, so no cert/key files are passed to ServeTLS.
func (s *DirectServer) Start(ctx context.Context, listenAddr string) error {
	srv := s.newHTTPServer()
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	return srv.ServeTLS(ln, "", "")
}
