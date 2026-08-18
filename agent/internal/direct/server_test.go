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
