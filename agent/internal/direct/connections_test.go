// agent/internal/direct/connections_test.go
package direct

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"sharebridge/agent/internal/immich"
)

// --- test doubles ---

// recordingHoldTracker extends recordingTracker with the HoldTracker half of
// OnDemandPort, recording hold begin/end calls so tests can assert exactly
// which connections drive the port's session and hold accounting (§13.2).
type recordingHoldTracker struct {
	recordingTracker

	mu     sync.Mutex
	begins int
	ends   []uint64
	next   uint64
}

func newRecordingHoldTracker() *recordingHoldTracker { return &recordingHoldTracker{} }

func (h *recordingHoldTracker) Begin() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.begins++
	h.next++
	return h.next
}

func (h *recordingHoldTracker) End(token uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ends = append(h.ends, token)
}

func (h *recordingHoldTracker) holds() (begins, ends int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.begins, len(h.ends)
}

// streamCoord lets a test park a streaming handler inside its backend, so a
// connection is deterministically mid-stream while the test observes or
// closes things.
type streamCoord struct {
	mu      sync.Mutex
	entered map[string]chan struct{}
	release chan struct{}
}

func newStreamCoord() *streamCoord {
	return &streamCoord{entered: map[string]chan struct{}{}, release: make(chan struct{})}
}

// waitEntered blocks until the handler for id has entered its backend.
func (s *streamCoord) waitEntered(t *testing.T, id string) {
	t.Helper()
	s.mu.Lock()
	ch, ok := s.entered[id]
	if !ok {
		ch = make(chan struct{})
		s.entered[id] = ch
	}
	s.mu.Unlock()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("stream %q never entered the backend", id)
	}
}

// block is a ContentBackend stream body: it signals that the handler for id
// reached the backend, then parks until Release or request-context
// cancellation (a registry-initiated connection close surfaces as the latter).
func (s *streamCoord) block(ctx context.Context, id string, w io.Writer) (int64, error) {
	s.mu.Lock()
	ch, ok := s.entered[id]
	if !ok {
		ch = make(chan struct{})
		s.entered[id] = ch
	}
	s.mu.Unlock()
	ch <- struct{}{}
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-s.release:
		n, err := io.WriteString(w, "DATA")
		return int64(n), err
	}
}

// Release unblocks every parked handler. One-shot per streamCoord.
func (s *streamCoord) Release() { close(s.release) }

// assetStreamingBackend returns a ContentBackend whose original-asset streams
// park in the per-id coord, so tests can hold streams mid-flight. With no
// coords the stream completes immediately ("DATA").
func assetStreamingBackend(coords ...*streamCoord) *handlerBackend {
	return &handlerBackend{
		assetInfo: func(context.Context, string) (immich.Asset, error) {
			return immich.Asset{OriginalFileName: "x.bin", OriginalMimeType: "application/octet-stream"}, nil
		},
		file: func(ctx context.Context, id string, w io.Writer) (int64, error) {
			if len(coords) == 0 {
				n, err := io.WriteString(w, "DATA")
				return int64(n), err
			}
			// "-1" ids park in the first coord, "-2" ids in the second (when
			// present); a lone coord parks everything.
			if len(coords) > 1 && strings.HasSuffix(id, "2") {
				return coords[1].block(ctx, id, w)
			}
			return coords[0].block(ctx, id, w)
		},
	}
}

// --- shared wiring ---

// connTestServer is a DirectServer on a real TLS listener with both §6 origins
// of share "abc" admitted, so tests can drive true connections (ConnContext /
// ConnState wiring) over each route.
type connTestServer struct {
	t       *testing.T
	srv     *DirectServer
	tracker *recordingHoldTracker
	ln      net.Listener
	direct  string
	relay   string
}

// startConnTestServer builds the server. h2 keeps the default HTTP/2 support;
// otherwise HTTP/1.1 is forced (TLSNextProto disabled). A nil session installs
// an empty resolver.
func startConnTestServer(t *testing.T, tr *recordingHoldTracker, session *ContentSession, h2 bool) *connTestServer {
	t.Helper()
	ns, base := "sbdeadbeef", "example.com"
	directOrigin := "demo." + ns + "." + base
	relayOrigin := "demo.relay." + ns + "." + base
	cert := testServerCert(t, ns, base)
	gate := NewSignalGate("a", func(string, RouteKind) bool { return true })
	srv := NewDirectServer(ns, base, tr, &rotatableCerts{cert}, gate, 1<<20)
	if err := srv.Binder().AllowShare(directOrigin, relayOrigin, "abc"); err != nil {
		t.Fatalf("AllowShare: %v", err)
	}
	if session == nil {
		session = &ContentSession{Membership: map[string]struct{}{}}
	}
	srv.SetResolver(&fakeResolver{sessions: map[string]*ContentSession{"abc": session}})

	hs := srv.newHTTPServer()
	if !h2 {
		hs.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go hs.ServeTLS(ln, "", "")
	t.Cleanup(func() { hs.Close(); ln.Close() })
	return &connTestServer{t: t, srv: srv, tracker: tr, ln: ln, direct: directOrigin, relay: relayOrigin}
}

// client returns an SNI-pinned client dialing the in-process listener. h2
// negotiates HTTP/2 (ForceAttemptHTTP2); otherwise HTTP/1.1 is forced.
func (c *connTestServer) client(origin string, h2 bool) *http.Client {
	tcfg := &tls.Config{ServerName: origin, InsecureSkipVerify: true} // test only: self-signed cert
	tr := &http.Transport{
		TLSClientConfig: tcfg,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, c.ln.Addr().String())
		},
	}
	if h2 {
		tr.ForceAttemptHTTP2 = true
	} else {
		tcfg.NextProtos = []string{"http/1.1"}
	}
	return &http.Client{Transport: tr}
}

func (c *connTestServer) get(client *http.Client, origin, path string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, "https://"+origin+path, nil)
	if err != nil {
		return nil, err
	}
	req.Host = origin
	return client.Do(req)
}

// bindings snapshots the live connection registry (route, origin, share per
// connection).
func (c *connTestServer) bindings() []connBinding {
	return registryBindings(&c.srv.conns)
}

type connBinding struct {
	route  RouteKind
	origin string
	share  string
}

// registryBindings snapshots the registry's live connections with their noted
// bindings. Same-package access to the registry map, under its lock.
func registryBindings(r *connRegistry) []connBinding {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]connBinding, 0, len(r.conns))
	for _, e := range r.conns {
		out = append(out, connBinding{
			route:  e.cs.boundRoute(),
			origin: e.cs.boundOrigin(),
			share:  e.cs.boundShare(),
		})
	}
	return out
}

// --- §13.2 tests ---

// TestDirectConnectionUsesOnDemandSessionAndHold pins §13.2 for the direct
// route: a direct connection's first request begins an OnDemandPort session,
// later requests record activity, a streaming response holds the port for the
// stream's lifetime, the admitted binding (route, origin, share) is tracked on
// the connection, and closing the connection ends the session.
func TestDirectConnectionUsesOnDemandSessionAndHold(t *testing.T) {
	tr := newRecordingHoldTracker()
	cs := startConnTestServer(t, tr, &ContentSession{Membership: map[string]struct{}{}}, false)

	client := cs.client(cs.direct, false)

	// A streaming download (port hold taken on the first body write), then the
	// gallery page (Activity), over one keep-alive connection.
	resp, err := cs.get(client, cs.direct, "/s/abc/download?size=4096")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d, want 200", resp.StatusCode)
	}
	page, err := cs.get(client, cs.direct, "/s/abc")
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	io.Copy(io.Discard, page.Body)
	page.Body.Close()
	if page.StatusCode != http.StatusOK {
		t.Fatalf("page status = %d, want 200", page.StatusCode)
	}

	// The live connection is registered with its Binder-admitted binding.
	waitFor(t, func() bool { return len(cs.bindings()) == 1 })
	if b := cs.bindings()[0]; b.route != RouteDirect || b.origin != cs.direct || b.share != "abc" {
		t.Fatalf("connection binding = %+v, want direct/%s/abc", b, cs.direct)
	}

	begins, acts, _ := tr.snapshot()
	if len(begins) != 1 || begins[0] != "abc" {
		t.Fatalf("begins = %v, want exactly [abc]", begins)
	}
	if len(acts) != 1 {
		t.Fatalf("acts = %d, want exactly one Activity (the second request)", len(acts))
	}
	if hb, he := tr.holds(); hb != 1 || he != 1 {
		t.Fatalf("holds = (%d, %d), want (1, 1): the direct stream must hold the port", hb, he)
	}

	// Closing the keep-alive connection ends the port session through the
	// normal ConnState path.
	client.Transport.(*http.Transport).CloseIdleConnections()
	waitFor(t, func() bool {
		_, _, ends := tr.snapshot()
		return len(ends) == 1
	})
	if b := cs.bindings(); len(b) != 0 {
		t.Fatalf("registry = %v after close, want empty", b)
	}
}

// TestRelayConnectionNeverBeginsRenewsOrHoldsPort pins the other half of
// §13.2: relay traffic must not open, renew, or hold the home port mapping —
// no session begin, activity, hold, or end may ever reach the port from a
// relay-routed connection, while the same content routes keep serving.
func TestRelayConnectionNeverBeginsRenewsOrHoldsPort(t *testing.T) {
	tr := newRecordingHoldTracker()
	cs := startConnTestServer(t, tr, &ContentSession{Membership: map[string]struct{}{}}, false)

	client := cs.client(cs.relay, false)
	for _, path := range []string{"/s/abc/download?size=4096", "/s/abc", "/s/abc/items"} {
		resp, err := cs.get(client, cs.relay, path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status = %d, want 200", path, resp.StatusCode)
		}
	}

	// The connection is registered — as a relay connection with its binding.
	waitFor(t, func() bool { return len(cs.bindings()) == 1 })
	if b := cs.bindings()[0]; b.route != RouteRelay || b.origin != cs.relay || b.share != "abc" {
		t.Fatalf("connection binding = %+v, want relay/%s/abc", b, cs.relay)
	}

	// Nothing reached the port: no session, no activity, no holds.
	begins, acts, _ := tr.snapshot()
	if len(begins) != 0 || len(acts) != 0 {
		t.Fatalf("relay connection touched port session accounting: begins=%v acts=%v", begins, acts)
	}
	if hb, he := tr.holds(); hb != 0 || he != 0 {
		t.Fatalf("relay connection took streaming holds: begins=%d ends=%d", hb, he)
	}

	// Closing the relay connection must not produce an EndSession either.
	client.Transport.(*http.Transport).CloseIdleConnections()
	waitFor(t, func() bool { return len(cs.bindings()) == 0 })
	_, _, ends := tr.snapshot()
	if len(ends) != 0 {
		t.Fatalf("relay close produced EndSession %v; relay must never end port sessions", ends)
	}
}

// countingResolver counts Resolve invocations so sharing of the single
// resolver across both routes can be asserted.
type countingResolver struct {
	inner Resolver
	mu    sync.Mutex
	n     int
}

func (r *countingResolver) Resolve(code string) (*ContentSession, error) {
	r.mu.Lock()
	r.n++
	r.mu.Unlock()
	return r.inner.Resolve(code)
}

func (r *countingResolver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

// TestBothRoutesShareContentSemaphoresLedgerAndResolver pins §13.2/§9.4: direct
// and relay connections serve through the SAME content machinery — one
// resolver, one per-share streaming semaphore, one download ledger. A relay
// connection must observe the saturation and the download limits a direct
// connection produced.
func TestBothRoutesShareContentSemaphoresLedgerAndResolver(t *testing.T) {
	ns, base := "sbdeadbeef", "example.com"
	directOrigin := "demo." + ns + "." + base
	relayOrigin := "demo.relay." + ns + "." + base
	cert := testServerCert(t, ns, base)
	gate := NewSignalGate("a", func(string, RouteKind) bool { return true })
	srv := NewDirectServer(ns, base, &recordingTracker{}, &rotatableCerts{cert}, gate, 1<<20)
	if err := srv.Binder().AllowShare(directOrigin, relayOrigin, "abc"); err != nil {
		t.Fatalf("AllowShare: %v", err)
	}

	// The resolver resolves to whichever session is installed, so each phase
	// can pin exactly the sharing property it asserts while the resolver
	// instance — and its count — stays common to both routes.
	sessions := map[string]*ContentSession{}
	res := &countingResolver{inner: &fakeResolver{sessions: sessions}}
	srv.SetResolver(res)
	handler := srv.Handler()

	// --- Phase 1: shared per-share streaming semaphore ---
	// A direct connection parks mid-download holding the per-share stream
	// semaphore (capacity 1); a relay connection must be refused by the SAME
	// semaphore (429), before its own backend call could start.
	coord := newStreamCoord()
	sessions["abc"] = &ContentSession{
		Backend:    assetStreamingBackend(coord),
		Membership: map[string]struct{}{"asset-1": {}},
		Streams:    newStreamGate(1),
	}
	directDone := make(chan struct{})
	var directCode int
	go func() {
		defer close(directDone)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, admittedRequest(http.MethodGet, "/s/abc/asset/asset-1", directOrigin))
		directCode = rr.Code
	}()
	coord.waitEntered(t, "asset-1")

	relayRR := httptest.NewRecorder()
	handler.ServeHTTP(relayRR, admittedRequest(http.MethodGet, "/s/abc/asset/asset-1", relayOrigin))
	if relayRR.Code != http.StatusTooManyRequests {
		t.Fatalf("relay stream status = %d, want 429 (shared per-share semaphore)", relayRR.Code)
	}

	coord.Release()
	<-directDone
	if directCode != http.StatusOK {
		t.Fatalf("direct stream status = %d, want 200", directCode)
	}

	// --- Phase 2: shared download ledger ---
	// The direct route's completed download exhausts the finite ledger; the
	// relay route must then be refused (403) by the SAME ledger.
	ledger := NewLedger(1)
	sessions["abc"] = &ContentSession{
		Backend:    assetStreamingBackend(),
		Membership: map[string]struct{}{"asset-1": {}},
		Ledger:     ledger,
		Streams:    newStreamGate(1),
	}

	directRR := httptest.NewRecorder()
	handler.ServeHTTP(directRR, admittedRequest(http.MethodGet, "/s/abc/asset/asset-1", directOrigin))
	if directRR.Code != http.StatusOK {
		t.Fatalf("direct download status = %d, want 200", directRR.Code)
	}
	if got := ledger.Downloads(); got != 1 {
		t.Fatalf("downloads = %d, want 1 (committed through the shared ledger)", got)
	}

	relayRR = httptest.NewRecorder()
	handler.ServeHTTP(relayRR, admittedRequest(http.MethodGet, "/s/abc/asset/asset-1", relayOrigin))
	if relayRR.Code != http.StatusForbidden {
		t.Fatalf("relay download after limit = %d, want 403 (shared download ledger)", relayRR.Code)
	}

	// Both routes resolved through the same resolver instance: the direct
	// stream (1), the relay 429 attempt (2), the direct download (3), the
	// relay limited download (4).
	if got := res.count(); got != 4 {
		t.Fatalf("resolver.Resolve count = %d, want 4", got)
	}
}

// TestCloseByRouteAndShare pins the §13.2 connection registry: connections can
// be closed by share (both routes together, as revocation does), by route (as
// lockdown closes one path), and all — while a closed connection tears down
// through the normal path (its port session ends) and unmatched connections
// are undisturbed.
func TestCloseByRouteAndShare(t *testing.T) {
	tr := newRecordingHoldTracker()
	phaseA, phaseB := newStreamCoord(), newStreamCoord()
	cs := startConnTestServer(t, tr, &ContentSession{
		Backend: assetStreamingBackend(phaseA, phaseB),
		Membership: map[string]struct{}{
			"asset-d1": {}, "asset-r1": {}, // phase A (close-by-share)
			"asset-d2": {}, "asset-r2": {}, // phase B (close-by-route/all)
		},
	}, false)

	stream := func(client *http.Client, origin, id string, done chan struct{}, errp *error, codep *int) {
		go func() {
			defer close(done)
			resp, err := cs.get(client, origin, "/s/abc/asset/"+id)
			if err != nil {
				*errp = err
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			*codep = resp.StatusCode
		}()
	}

	// Phase A: one direct and one relay stream mid-flight for share "abc".
	directA, relayA := cs.client(cs.direct, false), cs.client(cs.relay, false)
	directADone, relayADone := make(chan struct{}), make(chan struct{})
	var directAErr, relayAErr error
	stream(directA, cs.direct, "asset-d1", directADone, &directAErr, nil)
	stream(relayA, cs.relay, "asset-r1", relayADone, &relayAErr, nil)
	phaseA.waitEntered(t, "asset-d1")
	phaseA.waitEntered(t, "asset-r1")
	waitFor(t, func() bool { return len(cs.bindings()) == 2 })

	// close-by-share closes BOTH routes' connections for the share, each
	// exactly once (revocation semantics, §13.2).
	if n := cs.srv.CloseShareConns("abc"); n != 2 {
		t.Fatalf("CloseShareConns(abc) = %d, want 2", n)
	}
	if n := cs.srv.CloseShareConns("abc"); n != 0 {
		t.Fatalf("repeat CloseShareConns(abc) = %d, want 0 (each connection closed once)", n)
	}
	if n := cs.srv.CloseShareConns("other"); n != 0 {
		t.Fatalf("CloseShareConns(other) = %d, want 0", n)
	}
	<-directADone
	<-relayADone
	if directAErr == nil || relayAErr == nil {
		t.Fatalf("share close must abort in-flight streams on both routes: direct=%v relay=%v", directAErr, relayAErr)
	}
	// The direct session ended through the normal ConnState teardown path.
	waitFor(t, func() bool {
		_, _, ends := tr.snapshot()
		return len(ends) == 1
	})
	waitFor(t, func() bool { return len(cs.bindings()) == 0 })

	// Phase B: close-by-route closes only the direct connection (lockdown
	// semantics) — the relay stream is undisturbed and completes.
	directB, relayB := cs.client(cs.direct, false), cs.client(cs.relay, false)
	directBDone, relayBDone := make(chan struct{}), make(chan struct{})
	var directBErr, relayBErr error
	var relayBCode int
	stream(directB, cs.direct, "asset-d2", directBDone, &directBErr, nil)
	stream(relayB, cs.relay, "asset-r2", relayBDone, &relayBErr, &relayBCode)
	phaseB.waitEntered(t, "asset-d2")
	phaseB.waitEntered(t, "asset-r2")

	if n := cs.srv.CloseRouteConns(RouteDirect); n != 1 {
		t.Fatalf("CloseRouteConns(direct) = %d, want 1", n)
	}
	<-directBDone
	if directBErr == nil {
		t.Fatal("direct stream must abort on a direct route close")
	}
	waitFor(t, func() bool {
		_, _, ends := tr.snapshot()
		return len(ends) == 2 // both direct sessions ended, relay never had one
	})

	phaseB.Release()
	<-relayBDone
	if relayBErr != nil {
		t.Fatalf("relay stream must survive a direct route close: %v", relayBErr)
	}
	if relayBCode != http.StatusOK {
		t.Fatalf("relay stream status = %d, want 200", relayBCode)
	}

	// The idle relay connection is still registered; close-all closes it.
	waitFor(t, func() bool { return len(cs.bindings()) == 1 })
	if n := cs.srv.CloseAllConns(); n != 1 {
		t.Fatalf("CloseAllConns = %d, want 1", n)
	}
	waitFor(t, func() bool { return len(cs.bindings()) == 0 })

	// With the registry empty, every close form is an idempotent zero.
	if n := cs.srv.CloseRouteConns(RouteDirect); n != 0 {
		t.Fatalf("CloseRouteConns(direct) on empty registry = %d, want 0", n)
	}
	if n := cs.srv.CloseShareConns("abc"); n != 0 {
		t.Fatalf("CloseShareConns(abc) on empty registry = %d, want 0", n)
	}
	if n := cs.srv.CloseAllConns(); n != 0 {
		t.Fatalf("CloseAllConns on empty registry = %d, want 0", n)
	}
}

// TestDirectAndRelayStreamsCanCoexist pins §9.4: a live direct stream and a
// live relay stream for the same share coexist — as concurrent HTTP/2 streams
// — the direct connection keeps exactly ONE port session across its
// multiplexed streams, and the relay streams never touch the port.
func TestDirectAndRelayStreamsCanCoexist(t *testing.T) {
	tr := newRecordingHoldTracker()
	coord := newStreamCoord()
	cs := startConnTestServer(t, tr, &ContentSession{
		Backend:    assetStreamingBackend(coord),
		Membership: map[string]struct{}{"asset-d1": {}, "asset-d2": {}, "asset-r1": {}},
	}, true) // HTTP/2 enabled

	direct := cs.client(cs.direct, true)
	relay := cs.client(cs.relay, true)

	type streamResult struct {
		proto string
		err   error
	}
	run := func(client *http.Client, origin, path string) <-chan streamResult {
		ch := make(chan streamResult, 1)
		go func() {
			resp, err := cs.get(client, origin, path)
			if err != nil {
				ch <- streamResult{err: err}
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			ch <- streamResult{proto: resp.Proto}
		}()
		return ch
	}

	// Two concurrent HTTP/2 streams on the direct connection, one on the
	// relay connection — all three live at the same time.
	d1 := run(direct, cs.direct, "/s/abc/asset/asset-d1")
	d2 := run(direct, cs.direct, "/s/abc/asset/asset-d2")
	r1 := run(relay, cs.relay, "/s/abc/asset/asset-r1")
	coord.waitEntered(t, "asset-d1")
	coord.waitEntered(t, "asset-d2")
	coord.waitEntered(t, "asset-r1")

	// The direct connection holds ONE port session across both of its streams
	// (h2 multiplexing); the relay connection holds none.
	begins, acts, _ := tr.snapshot()
	if len(begins) != 1 || begins[0] != "abc" {
		t.Fatalf("begins = %v, want exactly one direct session [abc]", begins)
	}
	if len(acts) != 1 {
		t.Fatalf("acts = %d, want exactly one Activity (the direct conn's second stream)", len(acts))
	}
	if hb, he := tr.holds(); hb != 0 || he != 0 {
		t.Fatalf("holds before first byte = (%d, %d), want (0, 0)", hb, he)
	}

	coord.Release()
	for name, res := range map[string]streamResult{"direct-1": <-d1, "direct-2": <-d2, "relay": <-r1} {
		if res.err != nil {
			t.Fatalf("%s stream: %v", name, res.err)
		}
		if res.proto != "HTTP/2.0" {
			t.Fatalf("%s stream: proto = %q, want HTTP/2.0 (coexistence must hold on h2 streams)", name, res.proto)
		}
	}

	// The two direct streams each held the port, start to finish; the relay
	// stream never did.
	if hb, he := tr.holds(); hb != 2 || he != 2 {
		t.Fatalf("holds = (%d, %d), want (2, 2): direct streams only", hb, he)
	}
	if _, _, ends := tr.snapshot(); len(ends) != 0 {
		t.Fatalf("ends = %v before connection close; live sessions must persist", ends)
	}

	// Closing both connections ends exactly the direct session.
	direct.Transport.(*http.Transport).CloseIdleConnections()
	relay.Transport.(*http.Transport).CloseIdleConnections()
	waitFor(t, func() bool {
		_, _, ends := tr.snapshot()
		return len(ends) == 1
	})
}
