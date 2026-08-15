// agent/internal/direct/server.go
package direct

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
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

	// connStates maps each raw net.Conn to its per-connection *connState.
	// The key is the exact conn handed to ConnContext and later to ConnState
	// on StateClosed — the same object for the connection's entire lifetime
	// (with ServeTLS the listener is tls-wrapped at Accept, so c.rwc is never
	// reassigned). A field (rather than a package-level map) keeps state per
	// server instance.
	connStates sync.Map // net.Conn -> *connState
}

func NewDirectServer(namespace, baseDomain string, port SessionTracker, certs CertProvider, gate *SignalGate, maxContentBytes int64) *DirectServer {
	return &DirectServer{
		namespace: namespace, baseDomain: baseDomain, port: port, certs: certs, gate: gate,
		maxContentBytes: maxContentBytes, binder: NewBinder(namespace, baseDomain),
	}
}

func (s *DirectServer) Binder() *Binder { return s.binder }

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
	case rest == "/probe" || strings.HasPrefix(rest, "/probe?"):
		s.handleProbe(w, r, code)
	case rest == "/" || rest == "":
		s.handlePage(w, r, code)
	case strings.HasPrefix(rest, "/download"):
		s.handleDownload(w, r)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
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
	s.activity(w, r, code)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, "<html><body><h1>ShareBridge direct</h1><p>serving %s P2P over direct HTTPS</p></body></html>", code)
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
	io.CopyN(w, zeroReader{}, size)
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
		Handler:   s.Handler(),
		TLSConfig: s.TLSConfig(),
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
