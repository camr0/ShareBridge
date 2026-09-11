// agent/internal/direct/port_gate_test.go
package direct

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

// M4 closeout batch 2 — the post-remediation Sol audit's must-fix-before-M6
// composition gap: direct content must be gated on the on-demand port being
// LOGICALLY open.
//
// After deletion exhaustion the port is `open=false` but the router mapping and
// granted port linger (StateCloseFailed). The router keeps forwarding that
// mapping to the agent's listener, and the DirectServer used to serve content
// regardless — it drove session accounting but IGNORED BeginSession's error, so
// a previously-authorized recipient could reuse a known direct origin over the
// lingering mapping (including on a keep-alive connection) with no new open
// signal. The fix makes the port's logical open state the authoritative gate
// for every direct content request: BeginSession must succeed, and an existing
// session must still be live, or the request fails closed.

// directTLSServer serves srv.Handler() on a real TLS listener and returns a
// client that dials it with the given SNI, forcing HTTP/1.1 and a single
// connection so the keep-alive reuse case is exercised deterministically.
func directTLSServer(t *testing.T, srv *DirectServer, sni string) (*http.Client, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	hs := srv.newHTTPServer()
	hs.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
	go hs.ServeTLS(ln, "", "")
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			ServerName: sni, InsecureSkipVerify: true, NextProtos: []string{"http/1.1"},
		},
		ForceAttemptHTTP2:   false,
		MaxConnsPerHost:     1,
		MaxIdleConnsPerHost: 1,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, ln.Addr().String())
		},
	}}
	return client, func() { hs.Close() }
}

// getDirect issues one authorized direct GET (URL origin == Host == SNI).
func getDirect(t *testing.T, client *http.Client, origin, path string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "https://"+origin+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = origin
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, body
}

// TestServerRefusesDirectContentWhenPortNotLogicallyOpen is the lingering-
// mapping reuse proof: a recipient that was already served over a known direct
// origin must not be served again once the port is no longer logically open,
// even though the router mapping (and its lease) still forwards to the
// listener. At the base revision the port's accounting did not gate the
// request, so this served content (200); it must now fail closed.
func TestServerRefusesDirectContentWhenPortNotLogicallyOpen(t *testing.T) {
	ns, base := "sbdeadbeef", "example.com"
	const code = "abc"
	origin := "demo." + ns + "." + base

	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	rm := &recordingMapper{alwaysFailDel: true}
	p := newTestPort(fc, rm, time.Minute)
	t.Cleanup(func() {
		rm.mu.Lock()
		rm.alwaysFailDel = false
		rm.mu.Unlock()
		_ = p.Close()
	})

	if err := p.OpenFor(code, time.Minute); err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	cert := testServerCert(t, ns, base)
	gate := NewSignalGate("a", func(string, RouteKind) bool { return true })
	srv := NewDirectServer(ns, base, p, &rotatableCerts{cert}, gate, 1<<20)
	if err := srv.Binder().Allow(origin, RouteDirect, code); err != nil {
		t.Fatalf("allow origin: %v", err)
	}
	srv.SetResolver(stubResolver{})

	client, closeServer := directTLSServer(t, srv, origin)
	defer closeServer()

	// The recipient is served once: this is the "previously-authorized
	// recipient" whose keep-alive connection is then reused.
	resp, _ := getDirect(t, client, origin, "/s/"+code+"/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first direct request: status = %d, want 200", resp.StatusCode)
	}

	// Drive the mapping into the close-failed, lingering state: the router
	// delete is refused on every retry, so the mapping (and its forwarded port)
	// stays live on the router while the port is no longer logically open.
	if err := p.Close(); !errors.Is(err, ErrDeleteRetry) {
		t.Fatalf("Close: want ErrDeleteRetry, got %v", err)
	}
	escalateCloseFailure(t, fc, stateSignal(p, StateCloseFailed))
	if got := p.State(); got != StateCloseFailed {
		t.Fatalf("port state = %v, want StateCloseFailed", got)
	}
	if p.Open() {
		t.Fatalf("port must not be logically open in the close-failed state")
	}

	// Reuse the recipient's known direct origin over the lingering mapping.
	resp, body := getDirect(t, client, origin, "/s/"+code+"/")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("direct request over a lingering mapping: status = %d, want 503 (content not served); body = %q",
			resp.StatusCode, body)
	}
}

// failingBeginSessionTracker is a SessionTracker whose BeginSession always
// fails, standing in for a port that is not logically open. The DirectServer
// must honor that error and serve nothing (the base revision ignored it).
type failingBeginSessionTracker struct {
	mu     sync.Mutex
	begins int
}

func (f *failingBeginSessionTracker) BeginSession(string) (string, error) {
	f.mu.Lock()
	f.begins++
	f.mu.Unlock()
	return "", errors.New("direct: port is not logically open")
}

func (f *failingBeginSessionTracker) Activity(string) {}

func (f *failingBeginSessionTracker) EndSession(string) {}

func (f *failingBeginSessionTracker) beginCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.begins
}

// TestServerHonorsBeginSessionFailure proves the BeginSession error is no
// longer ignored: a content request whose session cannot be begun fails
// closed, and the failure is surfaced (the tracker is consulted at all).
func TestServerHonorsBeginSessionFailure(t *testing.T) {
	ns, base := "sbdeadbeef", "example.com"
	const code = "abc"
	origin := "demo." + ns + "." + base

	tracker := &failingBeginSessionTracker{}
	cert := testServerCert(t, ns, base)
	gate := NewSignalGate("a", func(string, RouteKind) bool { return true })
	srv := NewDirectServer(ns, base, tracker, &rotatableCerts{cert}, gate, 1<<20)
	if err := srv.Binder().Allow(origin, RouteDirect, code); err != nil {
		t.Fatalf("allow origin: %v", err)
	}
	srv.SetResolver(stubResolver{})

	client, closeServer := directTLSServer(t, srv, origin)
	defer closeServer()

	resp, body := getDirect(t, client, origin, "/s/"+code+"/")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("BeginSession failure must fail closed: status = %d, want 503; body = %q",
			resp.StatusCode, body)
	}
	if tracker.beginCount() == 0 {
		t.Fatalf("the server never consulted the session tracker")
	}
}

// TestServerServesDirectContentWhilePortLogicallyOpen is the no-false-blocking
// control: a fresh open, a keep-alive follow-up (activity), and a renewed open
// all serve normally.
func TestServerServesDirectContentWhilePortLogicallyOpen(t *testing.T) {
	ns, base := "sbdeadbeef", "example.com"
	const code = "abc"
	origin := "demo." + ns + "." + base

	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	p := newTestPort(fc, &recordingMapper{}, time.Minute)
	t.Cleanup(func() { _ = p.Close() })
	if err := p.OpenFor(code, time.Minute); err != nil {
		t.Fatalf("OpenFor: %v", err)
	}

	cert := testServerCert(t, ns, base)
	gate := NewSignalGate("a", func(string, RouteKind) bool { return true })
	srv := NewDirectServer(ns, base, p, &rotatableCerts{cert}, gate, 1<<20)
	if err := srv.Binder().Allow(origin, RouteDirect, code); err != nil {
		t.Fatalf("allow origin: %v", err)
	}
	srv.SetResolver(stubResolver{})

	client, closeServer := directTLSServer(t, srv, origin)
	defer closeServer()

	for i, what := range []string{"fresh open", "keep-alive follow-up"} {
		resp, body := getDirect(t, client, origin, "/s/"+code+"/")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s request %d: status = %d, want 200; body = %q", what, i, resp.StatusCode, body)
		}
	}

	// A renewal (a second, still-admitted open signal) must not block serving.
	if err := p.OpenFor(code, 2*time.Minute); err != nil {
		t.Fatalf("renew OpenFor: %v", err)
	}
	resp, body := getDirect(t, client, origin, "/s/"+code+"/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("renewed request: status = %d, want 200; body = %q", resp.StatusCode, body)
	}
}

// TestServerServesRelayRouteWhenPortClosed is the T26/T27/T30 control: relay
// connections never drive the on-demand port, so the new direct gate must not
// block them even with the port closed.
func TestServerServesRelayRouteWhenPortClosed(t *testing.T) {
	ns, base := "sbdeadbeef", "example.com"
	const code = "abc"
	relayOrigin := "demo.relay." + ns + "." + base

	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	p := newTestPort(fc, &recordingMapper{}, time.Minute)
	t.Cleanup(func() { _ = p.Close() })
	if p.Open() {
		t.Fatalf("precondition: the port must be closed")
	}

	cert := testServerCert(t, ns, base)
	gate := NewSignalGate("a", func(string, RouteKind) bool { return true })
	srv := NewDirectServer(ns, base, p, &rotatableCerts{cert}, gate, 1<<20)
	if err := srv.Binder().Allow(relayOrigin, RouteRelay, code); err != nil {
		t.Fatalf("allow relay origin: %v", err)
	}
	srv.SetResolver(stubResolver{})

	client, closeServer := directTLSServer(t, srv, relayOrigin)
	defer closeServer()

	resp, body := getDirect(t, client, relayOrigin, "/s/"+code+"/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("relay request with the port closed: status = %d, want 200; body = %q",
			resp.StatusCode, body)
	}
}
