// agent/internal/direct/limits_test.go
package direct

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"sharebridge/agent/internal/immich"
)

// newLimitsServer builds a DirectServer wired with the given sessions and
// returns both the server (so tests can override its global gate) and its
// handler. The single origin testOrigin is bound to code "abc".
func newLimitsServer(t *testing.T, sessions map[string]*ContentSession) (*DirectServer, http.Handler) {
	t.Helper()
	ns, base := "sbdeadbeef", "example.com"
	cert := testServerCert(t, ns, base)
	gate := NewSignalGate("a", func(string, RouteKind) bool { return true })
	srv := NewDirectServer(ns, base, nil, &rotatableCerts{cert}, gate, 1<<20)
	_ = srv.Binder().Allow(testOrigin, RouteDirect, "abc")
	srv.SetResolver(&fakeResolver{sessions: sessions})
	return srv, srv.Handler()
}

// blockingAssetBackend returns a handlerBackend whose GetFile blocks until
// release is closed, signalling entered once it has been invoked.
func blockingAssetBackend(entered, release chan struct{}) *handlerBackend {
	return &handlerBackend{
		assetInfo: func(context.Context, string) (immich.Asset, error) {
			return immich.Asset{OriginalFileName: "x.bin", OriginalMimeType: "application/octet-stream"}, nil
		},
		file: func(_ context.Context, _ string, w io.Writer) (int64, error) {
			close(entered)
			<-release
			n, err := io.WriteString(w, "DATA")
			return int64(n), err
		},
	}
}

func TestLimitsNewHTTPServerSetsTimeouts(t *testing.T) {
	srv := NewDirectServer("sbdeadbeef", "example.com", nil, nil, nil, 1<<20)
	hs := srv.newHTTPServer()
	if hs.ReadHeaderTimeout != readHeaderTimeout {
		t.Fatalf("ReadHeaderTimeout = %v, want %v", hs.ReadHeaderTimeout, readHeaderTimeout)
	}
	if hs.IdleTimeout != idleTimeout {
		t.Fatalf("IdleTimeout = %v, want %v", hs.IdleTimeout, idleTimeout)
	}
	if hs.MaxHeaderBytes != maxHeaderBytes {
		t.Fatalf("MaxHeaderBytes = %d, want %d", hs.MaxHeaderBytes, maxHeaderBytes)
	}
}

func TestLimitsPerShareSaturatedReturns429(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	session := &ContentSession{
		Backend:    blockingAssetBackend(entered, release),
		Membership: map[string]struct{}{"asset-1": {}},
		Streams:    newStreamGate(1),
	}
	_, handler := newLimitsServer(t, map[string]*ContentSession{"abc": session})

	rr1 := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(rr1, admittedRequest(http.MethodGet, "/s/abc/asset/asset-1", testOrigin))
	}()
	<-entered

	rr2 := doRequest(handler, http.MethodGet, "/s/abc/asset/asset-1")
	if rr2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429", rr2.Code)
	}
	if rr2.Header().Get("Retry-After") == "" {
		t.Fatal("429 overload response must carry a Retry-After bounded-retry hint")
	}

	close(release)
	<-done
	if rr1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", rr1.Code)
	}
}

func TestLimitsGlobalSaturatedReturns503(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	// Per-share gate has ample room; the global gate is the constraint.
	session := &ContentSession{
		Backend:    blockingAssetBackend(entered, release),
		Membership: map[string]struct{}{"asset-1": {}},
		Streams:    newStreamGate(4),
	}
	srv, handler := newLimitsServer(t, map[string]*ContentSession{"abc": session})
	srv.globalStreams = newStreamGate(1)

	rr1 := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(rr1, admittedRequest(http.MethodGet, "/s/abc/asset/asset-1", testOrigin))
	}()
	<-entered

	rr2 := doRequest(handler, http.MethodGet, "/s/abc/asset/asset-1")
	if rr2.Code != http.StatusServiceUnavailable {
		t.Fatalf("second request status = %d, want 503", rr2.Code)
	}
	if rr2.Header().Get("Retry-After") == "" {
		t.Fatal("503 overload response must carry a Retry-After bounded-retry hint")
	}

	close(release)
	<-done
	if rr1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", rr1.Code)
	}
}

func TestLimitsPerShareGateScopedPerShare(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	blocking := blockingAssetBackend(entered, release)
	quick := &handlerBackend{
		assetInfo: func(context.Context, string) (immich.Asset, error) {
			return immich.Asset{OriginalFileName: "b.bin", OriginalMimeType: "application/octet-stream"}, nil
		},
		file: func(_ context.Context, _ string, w io.Writer) (int64, error) {
			n, err := io.WriteString(w, "B")
			return int64(n), err
		},
	}

	originA := "a.sbdeadbeef.example.com"
	originB := "b.sbdeadbeef.example.com"
	ns, base := "sbdeadbeef", "example.com"
	cert := testServerCert(t, ns, base)
	gate := NewSignalGate("a", func(string, RouteKind) bool { return true })
	srv := NewDirectServer(ns, base, nil, &rotatableCerts{cert}, gate, 1<<20)
	_ = srv.Binder().Allow(originA, RouteDirect, "aaa")
	_ = srv.Binder().Allow(originB, RouteDirect, "bbb")
	srv.SetResolver(&fakeResolver{sessions: map[string]*ContentSession{
		"aaa": {Backend: blocking, Membership: map[string]struct{}{"asset-1": {}}, Streams: newStreamGate(1)},
		"bbb": {Backend: quick, Membership: map[string]struct{}{"asset-1": {}}, Streams: newStreamGate(1)},
	}})
	handler := srv.Handler()

	rrA := httptest.NewRecorder()
	doneA := make(chan struct{})
	go func() {
		defer close(doneA)
		handler.ServeHTTP(rrA, admittedRequest(http.MethodGet, "/s/aaa/asset/asset-1", originA))
	}()
	<-entered

	// Share aaa holds its own per-share slot; share bbb has a separate gate
	// and must stream without being rejected by aaa's saturation.
	rrB := httptest.NewRecorder()
	handler.ServeHTTP(rrB, admittedRequest(http.MethodGet, "/s/bbb/asset/asset-1", originB))
	if rrB.Code != http.StatusOK {
		t.Fatalf("share bbb status = %d, want 200 (per-share gate must not be shared across shares)", rrB.Code)
	}

	close(release)
	<-doneA
	if rrA.Code != http.StatusOK {
		t.Fatalf("share aaa status = %d, want 200", rrA.Code)
	}
}

func TestLimitsContextCancelAbortsUpstreamRead(t *testing.T) {
	started := make(chan struct{})
	aborted := make(chan struct{})
	backend := &handlerBackend{
		assetInfo: func(context.Context, string) (immich.Asset, error) {
			return immich.Asset{OriginalFileName: "x.bin", OriginalMimeType: "application/octet-stream"}, nil
		},
		file: func(ctx context.Context, _ string, w io.Writer) (int64, error) {
			close(started)
			<-ctx.Done()
			close(aborted)
			return 0, ctx.Err()
		},
	}
	_, handler := newLimitsServer(t, map[string]*ContentSession{
		"abc": {Backend: backend, Membership: map[string]struct{}{"asset-1": {}}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := admittedRequest(http.MethodGet, "/s/abc/asset/asset-1", testOrigin).WithContext(ctx)

	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(rr, req)
	}()
	<-started
	cancel()

	select {
	case <-aborted:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream read was not aborted after request context cancellation")
	}
	<-done
}
