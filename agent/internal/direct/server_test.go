// agent/internal/direct/server_test.go
package direct

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type rotatableCerts struct{ c *tls.Certificate }

func (r *rotatableCerts) Certificate() (*tls.Certificate, error) { return r.c, nil }

// testServerCert builds a self-signed leaf valid for both the namespace and the
// base domain (plus the full demo origin), so it serves as the direct-path
// certificate without any external CA.
func testServerCert(t *testing.T, ns, base string) *tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: ns},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{ns, base, "demo." + ns + "." + base},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// admittedTLSClient returns a client that presents SNI and trusts no CA (the
// handshake proceeds because the server presents its own cert and the client
// skips verification).
func admittedTLSClient(sni string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{ServerName: sni, InsecureSkipVerify: true},
	}}
}

func TestServerServesAdmittedSNI(t *testing.T) {
	ns, base := "sbdeadbeef", "example.com"
	cert := testServerCert(t, ns, base) // self-signed leaf with the two SANs
	gate := NewSignalGate("a", func(string, RouteKind) bool { return true })
	srv := NewDirectServer(ns, base, nil, &rotatableCerts{cert}, gate, 1<<20)
	_ = srv.Binder().Allow("demo."+ns+"."+base, RouteDirect, "abc")
	srv.SetResolver(stubResolver{})

	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.TLS = srv.TLSConfig()
	ts.StartTLS()
	defer ts.Close()

	// Explicit SNI = origin, Host = origin, self-signed trust.
	client := ts.Client()
	client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{
		ServerName: "demo." + ns + "." + base, InsecureSkipVerify: true,
	}
	req, _ := http.NewRequest("GET", ts.URL+"/s/abc", nil)
	req.Host = "demo." + ns + "." + base
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("admitted SNI should succeed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestServerRejectsUnknownSNI(t *testing.T) {
	ns, base := "sbdeadbeef", "example.com"
	cert := testServerCert(t, ns, base)
	gate := NewSignalGate("a", func(string, RouteKind) bool { return true })
	srv := NewDirectServer(ns, base, nil, &rotatableCerts{cert}, gate, 1<<20)
	_ = srv.Binder().Allow("demo."+ns+"."+base, RouteDirect, "abc")

	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.TLS = srv.TLSConfig()
	ts.StartTLS()
	defer ts.Close()

	// An origin that is not admitted must fail the handshake before any HTTP
	// is read — the request returns a transport error, not a 4xx/5xx.
	client := admittedTLSClient("other." + ns + "." + base)
	req, _ := http.NewRequest("GET", ts.URL+"/s/abc", nil)
	req.Host = "other." + ns + "." + base
	if _, err := client.Do(req); err == nil {
		t.Fatalf("unknown SNI: want handshake error, got nil")
	}
}

func TestServerProbeEchoesNonce(t *testing.T) {
	ns, base := "sbdeadbeef", "example.com"
	cert := testServerCert(t, ns, base)
	gate := NewSignalGate("a", func(string, RouteKind) bool { return true })
	// Admit a nonce for the bound share code so the probe verifies it.
	if err := gate.Admit(OpenSignal{
		Version: signalVersion, AgentID: "a", ShareID: "abc", RouteKind: RouteDirect,
		Nonce: "nonce-123", Seq: 1, ExpiresAt: time.Now().Add(time.Minute), Lease: 30 * time.Second,
	}); err != nil {
		t.Fatalf("admit nonce: %v", err)
	}
	srv := NewDirectServer(ns, base, nil, &rotatableCerts{cert}, gate, 1<<20)
	_ = srv.Binder().Allow("demo."+ns+"."+base, RouteDirect, "abc")

	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.TLS = srv.TLSConfig()
	ts.StartTLS()
	defer ts.Close()

	client := ts.Client()
	client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{
		ServerName: "demo." + ns + "." + base, InsecureSkipVerify: true,
	}
	req, _ := http.NewRequest("GET", ts.URL+"/s/abc/probe?nonce=nonce-123", nil)
	req.Host = "demo." + ns + "." + base
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "nonce-123" {
		t.Fatalf("echo = %q, want nonce-123", body)
	}
}

func TestServerProbeRejectsUnknownNonce(t *testing.T) {
	ns, base := "sbdeadbeef", "example.com"
	cert := testServerCert(t, ns, base)
	gate := NewSignalGate("a", func(string, RouteKind) bool { return true })
	srv := NewDirectServer(ns, base, nil, &rotatableCerts{cert}, gate, 1<<20)
	_ = srv.Binder().Allow("demo."+ns+"."+base, RouteDirect, "abc")

	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.TLS = srv.TLSConfig()
	ts.StartTLS()
	defer ts.Close()

	client := ts.Client()
	client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{
		ServerName: "demo." + ns + "." + base, InsecureSkipVerify: true,
	}
	req, _ := http.NewRequest("GET", ts.URL+"/s/abc/probe?nonce=never-admitted", nil)
	req.Host = "demo." + ns + "." + base
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// recordingTracker implements SessionTracker, recording call order so the
// ConnState/ConnContext wiring can be asserted end-to-end.
type recordingTracker struct {
	mu     sync.Mutex
	begins []string
	acts   []string
	ends   []string
	next   int
}

func (r *recordingTracker) BeginSession(shareID string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.begins = append(r.begins, shareID)
	r.next++
	return fmt.Sprintf("sess-%d", r.next), nil
}

func (r *recordingTracker) Activity(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.acts = append(r.acts, sessionID)
}

func (r *recordingTracker) EndSession(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ends = append(r.ends, sessionID)
}

func (r *recordingTracker) snapshot() (begins, acts, ends []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.begins...),
		append([]string(nil), r.acts...),
		append([]string(nil), r.ends...)
}

// TestServerConnSessionTracking drives the full http.Server wiring (not
// httptest, which ignores ConnContext/ConnState) over a real connection and
// asserts first-request → BeginSession, second-request → Activity, close →
// EndSession.
func TestServerConnSessionTracking(t *testing.T) {
	ns, base := "sbdeadbeef", "example.com"
	cert := testServerCert(t, ns, base)
	gate := NewSignalGate("a", func(string, RouteKind) bool { return true })
	tr := &recordingTracker{}
	srv := NewDirectServer(ns, base, tr, &rotatableCerts{cert}, gate, 1<<20)
	_ = srv.Binder().Allow("demo."+ns+"."+base, RouteDirect, "abc")
	srv.SetResolver(stubResolver{})

	hs := srv.newHTTPServer()
	// Force HTTP/1.1 so the two requests deterministically share one conn.
	hs.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go hs.ServeTLS(ln, "", "")
	defer hs.Close()

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			ServerName:         "demo." + ns + "." + base,
			InsecureSkipVerify: true,
			NextProtos:         []string{"http/1.1"},
		},
		ForceAttemptHTTP2: false,
		// Dial the in-process listener regardless of the request host, so the
		// request URL can carry the real origin without a DNS lookup.
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, ln.Addr().String())
		},
	}
	client := &http.Client{Transport: transport}
	origin := "demo." + ns + "." + base

	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest("GET", "https://"+origin+"/s/abc", nil)
		req.Host = origin
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		// Drain to EOF so the transport keeps the connection alive for the
		// second request (a small buffered body would be closed, not reused).
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	// Two requests over one keep-alive connection → one Begin, one Activity.
	waitFor(t, func() bool {
		begins, acts, _ := tr.snapshot()
		return len(begins) == 1 && len(acts) == 1
	})

	begins, acts, _ := tr.snapshot()
	if len(begins) != 1 || begins[0] != "abc" {
		t.Fatalf("begins = %v, want [abc]", begins)
	}
	if len(acts) != 1 {
		t.Fatalf("acts = %v, want exactly one Activity", acts)
	}

	// Close the idle connection → StateClosed → EndSession.
	transport.CloseIdleConnections()
	waitFor(t, func() bool {
		_, _, ends := tr.snapshot()
		return len(ends) == 1
	})
	_, _, ends := tr.snapshot()
	if len(ends) != 1 {
		t.Fatalf("ends = %v, want exactly one EndSession", ends)
	}
}

// TestServerDownloadTracksActivityAndExactPath verifies the /download route
// participates in session activity tracking (I5) and that only the exact
// /download path matches (no prefix routing).
func TestServerDownloadTracksActivityAndExactPath(t *testing.T) {
	ns, base := "sbdeadbeef", "example.com"
	cert := testServerCert(t, ns, base)
	gate := NewSignalGate("a", func(string, RouteKind) bool { return true })
	tr := &recordingTracker{}
	srv := NewDirectServer(ns, base, tr, &rotatableCerts{cert}, gate, 1<<20)
	_ = srv.Binder().Allow("demo."+ns+"."+base, RouteDirect, "abc")
	srv.SetResolver(stubResolver{})

	hs := srv.newHTTPServer()
	hs.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go hs.ServeTLS(ln, "", "")
	defer hs.Close()

	origin := "demo." + ns + "." + base
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			ServerName:         origin,
			InsecureSkipVerify: true,
			NextProtos:         []string{"http/1.1"},
		},
		ForceAttemptHTTP2: false,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, ln.Addr().String())
		},
	}
	client := &http.Client{Transport: transport}

	download := func(path string) int {
		req, _ := http.NewRequest("GET", "https://"+origin+path, nil)
		req.Host = origin
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}

	if got := download("/s/abc/download"); got != http.StatusOK {
		t.Fatalf("download status = %d, want 200", got)
	}
	if got := download("/s/abc/downloadX"); got != http.StatusNotFound {
		t.Fatalf("downloadX status = %d, want 404 (prefix routing must not match)", got)
	}

	// The download request must have begun a session (activity tracked); the
	// 404 request must not add a second begin.
	begins, _, _ := tr.snapshot()
	if len(begins) != 1 || begins[0] != "abc" {
		t.Fatalf("begins = %v, want exactly [abc]", begins)
	}
}

func TestServerPageServesGalleryHTMLAndDownloadContentLength(t *testing.T) {
	ns, base := "sbdeadbeef", "example.com"
	cert := testServerCert(t, ns, base)
	gate := NewSignalGate("a", func(string, RouteKind) bool { return true })
	srv := NewDirectServer(ns, base, &recordingTracker{}, &rotatableCerts{cert}, gate, 1<<30)
	_ = srv.Binder().Allow("demo."+ns+"."+base, RouteDirect, "abc")
	srv.SetResolver(stubResolver{})

	hs := srv.newHTTPServer()
	hs.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go hs.ServeTLS(ln, "", "")
	defer hs.Close()

	origin := "demo." + ns + "." + base
	transport := &http.Transport{
		TLSClientConfig:     &tls.Config{ServerName: origin, InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}},
		ForceAttemptHTTP2:   false,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, ln.Addr().String())
		},
	}
	client := &http.Client{Transport: transport}
	get := func(path string) *http.Response {
		req, _ := http.NewRequest("GET", "https://"+origin+path, nil)
		req.Host = origin
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		return resp
	}

	// The page must be the embedded recipient gallery: code-substituted <base>,
	// the gallery root container, the §4.8 CSP, and no inline handlers.
	page := get("/s/abc")
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if ct := page.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("page Content-Type = %q, want text/html", ct)
	}
	if got := page.Header.Get("Content-Security-Policy"); !strings.Contains(got, "script-src 'self'") {
		t.Fatalf("page CSP = %q, want script-src 'self'", got)
	}
	if !strings.Contains(string(body), `id="gallery-root"`) {
		t.Fatalf("page missing gallery root:\n%s", body)
	}
	if !strings.Contains(string(body), `<base href="/s/abc/">`) {
		t.Fatalf("page missing code-substituted base:\n%s", body)
	}
	if strings.Contains(string(body), "onclick=") {
		t.Fatalf("page contains inline onclick (forbidden by CSP):\n%s", body)
	}

	// Download must set Content-Length to the exact requested size.
	dl := get("/s/abc/download?size=104857600")
	n, _ := io.Copy(io.Discard, dl.Body)
	dl.Body.Close()
	if n != 104857600 {
		t.Fatalf("downloaded %d bytes, want 104857600", n)
	}
	if cl := dl.Header.Get("Content-Length"); cl != "104857600" {
		t.Fatalf("Content-Length = %q, want 104857600", cl)
	}
}

// connectRecordingResolver counts Resolve invocations so the connect
// endpoint's zero-resolver-work guarantee (§9.3) can be asserted.
type connectRecordingResolver struct{ resolves atomic.Int32 }

func (r *connectRecordingResolver) Resolve(code string) (*ContentSession, error) {
	r.resolves.Add(1)
	return &ContentSession{Membership: map[string]struct{}{}}, nil
}

// startConnectServer builds a TLS direct server for share "abc" on the demo
// direct origin, backed by the given resolver and a recording session tracker
// (returned so no-accounting guarantees can be asserted). When withRelay is
// true the matching relay origin is bound too, so relay-binding rejections can
// be exercised against the same binder.
func startConnectServer(t *testing.T, r Resolver, withRelay bool) (*httptest.Server, *recordingTracker) {
	t.Helper()
	ns, base := "sbdeadbeef", "example.com"
	cert := testServerCert(t, ns, base)
	gate := NewSignalGate("a", func(string, RouteKind) bool { return true })
	tr := &recordingTracker{}
	srv := NewDirectServer(ns, base, tr, &rotatableCerts{cert}, gate, 1<<20)
	_ = srv.Binder().Allow("demo."+ns+"."+base, RouteDirect, "abc")
	if withRelay {
		_ = srv.Binder().Allow("demo.relay."+ns+"."+base, RouteRelay, "abc")
	}
	srv.SetResolver(r)
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.TLS = srv.TLSConfig()
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts, tr
}

// doConnect issues a request against the connect test server with explicit
// SNI, Host, method, path, and Origin header (an empty origin omits it), via
// the package's SNI-setting test client.
func doConnect(t *testing.T, ts *httptest.Server, sni, host, method, path, origin string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, nil)
	if err != nil {
		t.Fatalf("NewRequest %s %s: %v", method, path, err)
	}
	req.Host = host
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := tlsClient(sni).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

const (
	connectDirectOrigin = "demo.sbdeadbeef.example.com"
	connectRelayOrigin  = "demo.relay.sbdeadbeef.example.com"
)

// TestConnectReturns204NoStoreForAuthorizedDirectBinding verifies the §9.3
// success contract: after Binder SNI/Host/code authorization with the
// RouteDirect binding and the exact interstitial Origin, the connect check is
// an empty 204 with no-store and exactly one ACAO for sharebridge.app.
func TestConnectReturns204NoStoreForAuthorizedDirectBinding(t *testing.T) {
	ts, _ := startConnectServer(t, stubResolver{}, false)

	resp := doConnect(t, ts, connectDirectOrigin, connectDirectOrigin, http.MethodGet,
		"/s/abc/connect", "https://sharebridge.app")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	acao := resp.Header.Values("Access-Control-Allow-Origin")
	if len(acao) != 1 || acao[0] != "https://sharebridge.app" {
		t.Fatalf("Access-Control-Allow-Origin = %v, want exactly [https://sharebridge.app]", acao)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil || len(body) != 0 {
		t.Fatalf("body = %q (err %v), want empty 204 body", body, err)
	}
}

// TestConnectAllowsOnlyShareBridgeAppOrigin verifies the Origin gate: absent,
// foreign, scheme-downgraded, suffixed, null, and case-variant origins are
// refused with 403/404 and no ACAO (no origin reflection); only the exact
// https://sharebridge.app value is accepted.
func TestConnectAllowsOnlyShareBridgeAppOrigin(t *testing.T) {
	ts, _ := startConnectServer(t, stubResolver{}, false)

	for _, tc := range []struct{ name, origin string }{
		{"absent", ""},
		{"foreign https", "https://evil.example.com"},
		{"scheme downgrade", "http://sharebridge.app"},
		{"suffix host", "https://sharebridge.app.evil.com"},
		{"null", "null"},
		{"case variation", "https://SHAREBRIDGE.APP"},
	} {
		resp := doConnect(t, ts, connectDirectOrigin, connectDirectOrigin, http.MethodGet,
			"/s/abc/connect", tc.origin)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 403/404", tc.name, resp.StatusCode)
		}
		if got := resp.Header.Values("Access-Control-Allow-Origin"); len(got) != 0 {
			t.Fatalf("%s: Access-Control-Allow-Origin = %v, want none (no reflection)", tc.name, got)
		}
	}

	// The exact origin value is the only accepted one.
	resp := doConnect(t, ts, connectDirectOrigin, connectDirectOrigin, http.MethodGet,
		"/s/abc/connect", "https://sharebridge.app")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("exact origin: status = %d, want 204", resp.StatusCode)
	}
	if got := resp.Header.Values("Access-Control-Allow-Origin"); len(got) != 1 || got[0] != "https://sharebridge.app" {
		t.Fatalf("exact origin: Access-Control-Allow-Origin = %v, want exactly [https://sharebridge.app]", got)
	}
}

// TestConnectAllowedOriginConfigurable verifies the config-driven allowed
// origin: with CONNECT_ALLOWED_ORIGIN set to a valid absolute origin, exactly
// that origin is accepted (204 + exactly one ACAO echoing it) while every other
// value — including the production default — is refused with 403 and no ACAO.
func TestConnectAllowedOriginConfigurable(t *testing.T) {
	t.Setenv("CONNECT_ALLOWED_ORIGIN", "http://192.0.2.10:8080")
	ts, _ := startConnectServer(t, stubResolver{}, false)

	resp := doConnect(t, ts, connectDirectOrigin, connectDirectOrigin, http.MethodGet,
		"/s/abc/connect", "http://192.0.2.10:8080")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("configured origin: status = %d, want 204", resp.StatusCode)
	}
	acao := resp.Header.Values("Access-Control-Allow-Origin")
	if len(acao) != 1 || acao[0] != "http://192.0.2.10:8080" {
		t.Fatalf("configured origin: Access-Control-Allow-Origin = %v, want exactly [http://192.0.2.10:8080]", acao)
	}

	// Under the override the production origin is just a foreign origin:
	// refused, no reflection.
	resp = doConnect(t, ts, connectDirectOrigin, connectDirectOrigin, http.MethodGet,
		"/s/abc/connect", "https://sharebridge.app")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("production origin under override: status = %d, want 403", resp.StatusCode)
	}
	if got := resp.Header.Values("Access-Control-Allow-Origin"); len(got) != 0 {
		t.Fatalf("production origin under override: Access-Control-Allow-Origin = %v, want none", got)
	}
}

// TestConnectAllowedOriginMalformedFailsClosed verifies that a malformed
// CONNECT_ALLOWED_ORIGIN fails closed at server construction: the server keeps
// the production default origin, so the exact production origin is accepted and
// everything else is refused with 403 and no ACAO.
func TestConnectAllowedOriginMalformedFailsClosed(t *testing.T) {
	for _, v := range []string{
		"not-an-origin",
		"sharebridge.app",
		"ftp://sharebridge.app",
		"https://sharebridge.app/path",
		"https://sharebridge.app?probe=1",
		"https://user:pass@sharebridge.app",
		"https://",
		"null",
	} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("CONNECT_ALLOWED_ORIGIN", v)
			ts, _ := startConnectServer(t, stubResolver{}, false)

			resp := doConnect(t, ts, connectDirectOrigin, connectDirectOrigin, http.MethodGet,
				"/s/abc/connect", "https://sharebridge.app")
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("malformed env %q: production origin status = %d, want 204 (fail closed to default)", v, resp.StatusCode)
			}
			acao := resp.Header.Values("Access-Control-Allow-Origin")
			if len(acao) != 1 || acao[0] != "https://sharebridge.app" {
				t.Fatalf("malformed env %q: Access-Control-Allow-Origin = %v, want exactly [https://sharebridge.app]", v, acao)
			}

			resp = doConnect(t, ts, connectDirectOrigin, connectDirectOrigin, http.MethodGet,
				"/s/abc/connect", "http://192.0.2.10:8080")
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("malformed env %q: foreign origin status = %d, want 403", v, resp.StatusCode)
			}
			if got := resp.Header.Values("Access-Control-Allow-Origin"); len(got) != 0 {
				t.Fatalf("malformed env %q: Access-Control-Allow-Origin = %v, want none", v, got)
			}
		})
	}
}

// TestConnectSetsNoCookieAndTouchesNoBackend verifies the §18.3 "no content /
// cookie" property structurally: a successful connect check never sets a
// cookie, never consults the share resolver, and never runs session activity
// accounting.
func TestConnectSetsNoCookieAndTouchesNoBackend(t *testing.T) {
	res := &connectRecordingResolver{}
	ts, tr := startConnectServer(t, res, false)

	resp := doConnect(t, ts, connectDirectOrigin, connectDirectOrigin, http.MethodGet,
		"/s/abc/connect", "https://sharebridge.app")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if got := resp.Header.Values("Set-Cookie"); len(got) != 0 {
		t.Fatalf("Set-Cookie = %v, want none", got)
	}
	if resp.Cookies() != nil && len(resp.Cookies()) != 0 {
		t.Fatalf("parsed cookies = %v, want none", resp.Cookies())
	}
	if n := res.resolves.Load(); n != 0 {
		t.Fatalf("resolver.Resolve called %d times, want 0", n)
	}
	begins, acts, _ := tr.snapshot()
	if len(begins) != 0 || len(acts) != 0 {
		t.Fatalf("session accounting ran: begins=%v acts=%v, want none", begins, acts)
	}
}

// TestConnectRejectsRelayBindingWrongHostWrongCodeAndNonGET verifies every
// rejection axis: a relay-bound origin, a mismatched Host, a mismatched share
// code, non-GET methods, and paths without the /s/<code> prefix are refused
// with 403/404 and no ACAO.
func TestConnectRejectsRelayBindingWrongHostWrongCodeAndNonGET(t *testing.T) {
	ts, _ := startConnectServer(t, stubResolver{}, true)

	t.Run("relay binding", func(t *testing.T) {
		// The Binder authorizes the relay origin (Host/kind/code all match its
		// relay binding), so the endpoint itself must refuse the non-direct
		// binding and must not reveal a connect endpoint on relay origins.
		resp := doConnect(t, ts, connectRelayOrigin, connectRelayOrigin, http.MethodGet,
			"/s/abc/connect", "https://sharebridge.app")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
		if got := resp.Header.Values("Access-Control-Allow-Origin"); len(got) != 0 {
			t.Fatalf("Access-Control-Allow-Origin = %v, want none", got)
		}
	})

	t.Run("wrong Host", func(t *testing.T) {
		resp := doConnect(t, ts, connectDirectOrigin, "evil.example.com", http.MethodGet,
			"/s/abc/connect", "https://sharebridge.app")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", resp.StatusCode)
		}
		if got := resp.Header.Values("Access-Control-Allow-Origin"); len(got) != 0 {
			t.Fatalf("Access-Control-Allow-Origin = %v, want none", got)
		}
	})

	t.Run("wrong code", func(t *testing.T) {
		resp := doConnect(t, ts, connectDirectOrigin, connectDirectOrigin, http.MethodGet,
			"/s/other/connect", "https://sharebridge.app")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", resp.StatusCode)
		}
		if got := resp.Header.Values("Access-Control-Allow-Origin"); len(got) != 0 {
			t.Fatalf("Access-Control-Allow-Origin = %v, want none", got)
		}
	})

	t.Run("non-GET methods", func(t *testing.T) {
		for _, m := range []string{http.MethodPost, http.MethodHead, http.MethodOptions, http.MethodPut} {
			resp := doConnect(t, ts, connectDirectOrigin, connectDirectOrigin, m,
				"/s/abc/connect", "https://sharebridge.app")
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("%s: status = %d, want 404", m, resp.StatusCode)
			}
			if got := resp.Header.Values("Access-Control-Allow-Origin"); len(got) != 0 {
				t.Fatalf("%s: Access-Control-Allow-Origin = %v, want none", m, got)
			}
		}
	})

	t.Run("unauthenticated path", func(t *testing.T) {
		// Without the /s/<code> prefix the request never reaches the endpoint:
		// the Binder rejects the code-less path with 403 (ErrWrongCode).
		resp := doConnect(t, ts, connectDirectOrigin, connectDirectOrigin, http.MethodGet,
			"/connect", "https://sharebridge.app")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 403/404", resp.StatusCode)
		}
		if got := resp.Header.Values("Access-Control-Allow-Origin"); len(got) != 0 {
			t.Fatalf("Access-Control-Allow-Origin = %v, want none", got)
		}
	})
}

// TestConnectPreflightIsNotBroadened pins the preflight ruling: the Task 22
// interstitial issues a simple CORS GET (no custom headers), so browsers never
// preflight and the endpoint must not implement preflight handling. An OPTIONS
// request with preflight headers must fail as a plain error with none of the
// Access-Control-* response headers, so no CORS capability is broadened.
func TestConnectPreflightIsNotBroadened(t *testing.T) {
	ts, _ := startConnectServer(t, stubResolver{}, false)

	req, err := http.NewRequest(http.MethodOptions, ts.URL+"/s/abc/connect", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Host = connectDirectOrigin
	req.Header.Set("Origin", "https://sharebridge.app")
	req.Header.Set("Access-Control-Request-Method", http.MethodGet)
	req.Header.Set("Access-Control-Request-Headers", "cache-control")
	resp, err := tlsClient(connectDirectOrigin).Do(req)
	if err != nil {
		t.Fatalf("OPTIONS: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (preflight must not succeed)", resp.StatusCode)
	}
	for _, h := range []string{
		"Access-Control-Allow-Origin",
		"Access-Control-Allow-Methods",
		"Access-Control-Allow-Headers",
		"Access-Control-Max-Age",
		"Access-Control-Allow-Credentials",
	} {
		if got := resp.Header.Get(h); got != "" {
			t.Fatalf("%s = %q, want none (preflight must not be broadened)", h, got)
		}
	}
}
