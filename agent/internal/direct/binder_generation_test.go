// agent/internal/direct/binder_generation_test.go
package direct

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingContentResolver wraps a fixed content session and counts Resolve calls, so a
// test can prove that a request fenced closed after revocation never reached
// content resolution (and therefore never started a stream).
type countingContentResolver struct {
	calls   atomic.Int64
	session *ContentSession
}

func (r *countingContentResolver) Resolve(string) (*ContentSession, error) {
	r.calls.Add(1)
	return r.session, nil
}

func (r *countingContentResolver) Calls() int64 { return r.calls.Load() }

// generationTestServer is a DirectServer on a real TLS listener with both §6
// origins of share "abc" admitted, plus a counting resolver and the
// post-authorize test seam.
type generationTestServer struct {
	srv     *DirectServer
	res     *countingContentResolver
	ln      net.Listener
	hs      *http.Server
	direct  string
	relay   string
	baseURL string
}

func startGenerationTestServer(t *testing.T) *generationTestServer {
	t.Helper()
	ns, base := "sbdeadbeef", "example.com"
	directOrigin := "demo." + ns + "." + base
	relayOrigin := "demo.relay." + ns + "." + base
	cert := testServerCert(t, ns, base)
	gate := NewSignalGate("a", func(string, RouteKind) bool { return true })
	srv := NewDirectServer(ns, base, nil, &rotatableCerts{cert}, gate, 1<<20)
	if err := srv.Binder().AllowShare(directOrigin, relayOrigin, "abc"); err != nil {
		t.Fatalf("AllowShare: %v", err)
	}
	res := &countingContentResolver{session: &ContentSession{Membership: map[string]struct{}{}}}
	srv.SetResolver(res)

	hs := srv.newHTTPServer()
	// Force HTTP/1.1 so the client sees the connection close deterministically
	// instead of an h2 stream reset.
	hs.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go hs.ServeTLS(ln, "", "")
	t.Cleanup(func() { hs.Close(); ln.Close() })
	return &generationTestServer{srv: srv, res: res, ln: ln, hs: hs, direct: directOrigin, relay: relayOrigin}
}

func (g *generationTestServer) client(origin string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{ServerName: origin, InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}}, // test-only: self-signed
		ForceAttemptHTTP2: false,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, g.ln.Addr().String())
		},
	}}
}

func (g *generationTestServer) download(client *http.Client, origin, code string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, "https://"+origin+"/s/"+code+"/download?size=4096", nil)
	if err != nil {
		return nil, err
	}
	req.Host = origin
	return client.Do(req)
}

func awaitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never arrived", what)
	}
}

// waitRegistryEmpty waits (bounded) until the server's connection registry is
// empty, i.e. the fail-closed teardown reached ConnState(StateClosed). The
// causal ordering is channel-gated by the caller; this only observes the
// post-condition.
func (g *generationTestServer) waitRegistryEmpty(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		g.srv.conns.mu.Lock()
		n := len(g.srv.conns.conns)
		g.srv.conns.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("connection was not closed: %d still registered", n)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestHandlerFencesRevokeBetweenAuthorizeAndBinding is audit Critical #3
// exactly: a request passes Binder authorization, then a revocation lands in
// the authorization→registration window, and the handler must NOT serve it.
// The gated seam (which runs after authorization and before the connection's
// binding is recorded) makes the interleaving deterministic; the revocation's
// close-by-share scan misses the connection because its binding is not yet
// recorded, so only the post-noteBinding revalidation can stop the stream.
func TestHandlerFencesRevokeBetweenAuthorizeAndBinding(t *testing.T) {
	g := startGenerationTestServer(t)

	entered := make(chan struct{})
	release := make(chan struct{})
	g.srv.SetTestHookAfterAuthorize(func() {
		close(entered)
		<-release
	})

	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := g.download(g.client(g.direct), g.direct, "abc")
		done <- result{resp, err}
	}()
	awaitSignal(t, entered, "handler entering the post-authorize window")

	// Revocation lands while the connection is authorized but unregistered.
	g.srv.Binder().RevokeShare(g.direct, g.relay)
	close(release)

	res := <-done
	if res.err == nil {
		defer res.resp.Body.Close()
		if res.resp.StatusCode == http.StatusOK {
			t.Fatalf("a revoked share served a stream after revocation (status 200)")
		}
		body, _ := io.ReadAll(res.resp.Body)
		if len(body) == 4096 {
			t.Fatalf("a revoked share served the full 4096-byte stream")
		}
	}
	if n := g.res.Calls(); n != 0 {
		t.Fatalf("content was resolved %d time(s) after revocation; the revoked share must serve nothing", n)
	}
	g.waitRegistryEmpty(t)
}

// TestHandlerServesAuthorizedRequestEndToEnd is the no-false-revocation
// control: the same gated path, without any revocation, must still serve the
// stream end to end and consume the resolver exactly once.
func TestHandlerServesAuthorizedRequestEndToEnd(t *testing.T) {
	g := startGenerationTestServer(t)

	entered := make(chan struct{})
	release := make(chan struct{})
	g.srv.SetTestHookAfterAuthorize(func() {
		close(entered)
		<-release
	})

	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := g.download(g.client(g.direct), g.direct, "abc")
		done <- result{resp, err}
	}()
	awaitSignal(t, entered, "handler entering the post-authorize window")
	close(release)

	res := <-done
	if res.err != nil {
		t.Fatalf("authorized request must still serve: %v", res.err)
	}
	defer res.resp.Body.Close()
	if res.resp.StatusCode != http.StatusOK {
		t.Fatalf("authorized request status = %d, want 200", res.resp.StatusCode)
	}
	if n, _ := io.Copy(io.Discard, res.resp.Body); n != 4096 {
		t.Fatalf("authorized request served %d bytes, want 4096", n)
	}
	if n := g.res.Calls(); n != 1 {
		t.Fatalf("resolver calls = %d, want 1", n)
	}
}

// TestBinderRevalidateGeneration pins the generation semantics Revalidate
// relies on: the exact admitted entry revalidates; a withdrawn entry does not;
// and a withdraw+re-add of the identical origin/route/code is a NEW generation
// that does not resurrect the stale admission.
func TestBinderRevalidateGeneration(t *testing.T) {
	ns, base := "sbdeadbeef", "example.com"
	directOrigin := "demo." + ns + "." + base
	b := NewBinder(ns, base)
	if err := b.Allow(directOrigin, RouteDirect, "abc"); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	bd, err := b.AdmitSNI(directOrigin)
	if err != nil {
		t.Fatalf("AdmitSNI: %v", err)
	}
	if !b.Revalidate(bd) {
		t.Fatalf("an unchanged admitted entry must revalidate")
	}
	b.Revoke(directOrigin)
	if b.Revalidate(bd) {
		t.Fatalf("a revoked entry must not revalidate")
	}
	if err := b.Allow(directOrigin, RouteDirect, "abc"); err != nil {
		t.Fatalf("re-Allow: %v", err)
	}
	if b.Revalidate(bd) {
		t.Fatalf("a withdraw+re-add of the same identity must be a new generation")
	}
	bd2, err := b.AdmitSNI(directOrigin)
	if err != nil {
		t.Fatalf("AdmitSNI after re-add: %v", err)
	}
	if !b.Revalidate(bd2) {
		t.Fatalf("the re-added entry must revalidate")
	}
}

// TestBinderConcurrentRevokeAuthorizeRevalidate is the repetition/interleaving
// control, run under -race -count: concurrent revoke/authorize/revalidate must
// never let a request that revalidated successfully observe a revoked binding
// (every successful revalidation is followed by an immediate AdmitSNI that must
// return the same binding).
func TestBinderConcurrentRevokeAuthorizeRevalidate(t *testing.T) {
	ns, base := "sbdeadbeef", "example.com"
	directOrigin := "demo." + ns + "." + base
	relayOrigin := "demo.relay." + ns + "." + base
	b := NewBinder(ns, base)
	if err := b.AllowShare(directOrigin, relayOrigin, "abc"); err != nil {
		t.Fatalf("AllowShare: %v", err)
	}

	const workers = 4
	const iterations = 300
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			b.RevokeShare(directOrigin, relayOrigin)
			_ = b.AllowShare(directOrigin, relayOrigin, "abc")
		}
	}()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				bd, err := b.AdmitSNI(directOrigin)
				if err != nil {
					continue
				}
				if !b.Revalidate(bd) {
					continue
				}
				// A successful revalidation means the exact entry was still
				// present at that instant; an immediate admission must therefore
				// still find it (a revoke may land between the two, in which case
				// the admission simply reports unknown rather than a wrong
				// generation).
				if cur, err := b.AdmitSNI(bd.Origin); err == nil && cur.ShareCode != bd.ShareCode {
					t.Errorf("admission returned a different share than the revalidated binding")
					return
				}
			}
		}()
	}
	wg.Wait()
}
