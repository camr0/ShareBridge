// agent/internal/direct/server.go
package direct

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
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

	// globalStreams bounds the total number of simultaneous streaming
	// responses (asset/video/archive parts) across all shares (§11). A nil
	// gate means unlimited.
	globalStreams *streamGate

	// connStates maps each raw net.Conn to its per-connection *connState.
	// The key is the exact conn handed to ConnContext and later to ConnState
	// on StateClosed — the same object for the connection's entire lifetime
	// (with ServeTLS the listener is tls-wrapped at Accept, so c.rwc is never
	// reassigned). A field (rather than a package-level map) keeps state per
	// server instance.
	connStates sync.Map // net.Conn -> *connState
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
		globalStreams: newStreamGate(globalStreamLimit),
	}
}

func (s *DirectServer) Binder() *Binder { return s.binder }

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
			return &tls.Config{Certificates: []tls.Certificate{*cert}}, nil
		},
	}
}

// Handler returns the direct path wrapped in the binder's HTTP authorization.
// The binder re-derives the admitted binding from r.TLS.ServerName, so the
// handler is only reached for SNI-admitted, Host- and code-authorized requests.
func (s *DirectServer) Handler() http.Handler { return s.binder.Handler(http.HandlerFunc(s.route)) }

func (s *DirectServer) route(w http.ResponseWriter, r *http.Request) {
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
	dst, release := s.holdStream(w)
	defer release()
	io.CopyN(dst, zeroReader{}, size)
}

// holdStream wraps dst so the on-demand port pauses its idle close for the
// duration of a long streaming response: Begin fires on the first write and
// End (via the returned release func, which the caller must defer) releases it
// on completion, error, or the panic that aborts a mid-stream failure —
// including a client disconnect, which surfaces as a write error and unwinds
// the same defer. When the port does not support holds, dst is returned
// unchanged and release is a no-op.
func (s *DirectServer) holdStream(dst io.Writer) (io.Writer, func()) {
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
// is driven by ConnState (StateClosed). The conn's mutex guards sessionID so a
// connection (an HTTP/2 connection with concurrent streams) can't double-begin
// or race a close.
func (s *DirectServer) activity(w http.ResponseWriter, r *http.Request, code string) {
	if s.port == nil {
		return
	}
	cs, _ := r.Context().Value(connStateKey{}).(*connState)
	if cs == nil {
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

// connState carries the per-connection session token. It is created in
// ConnContext (which runs before ConnState's StateNew, so StateNew cannot
// supply it) and stashed in both the request context and the server's
// connStates map.
type connState struct {
	mu        sync.Mutex
	sessionID string
}

type connStateKey struct{}

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
// ConnContext/ConnState that connect each net.Conn to its *connState.
//
// Wiring (ordering matters): Go's Serve calls ConnContext BEFORE firing
// ConnState(StateNew), so the *connState must be created and stashed in
// ConnContext — not in StateNew. ConnContext stores it under the raw net.Conn
// in connStates and returns a context carrying it; the handler's activity
// reads it from r.Context(); ConnState(StateClosed) deletes it from connStates
// and ends the session.
func (s *DirectServer) newHTTPServer() *http.Server {
	return &http.Server{
		Handler:           s.Handler(),
		TLSConfig:         s.TLSConfig(),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			cs := &connState{}
			s.connStates.Store(c, cs)
			return context.WithValue(ctx, connStateKey{}, cs)
		},
		ConnState: func(c net.Conn, st http.ConnState) {
			if st != http.StateClosed {
				return
			}
			v, ok := s.connStates.LoadAndDelete(c)
			if !ok {
				return
			}
			cs := v.(*connState)
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
