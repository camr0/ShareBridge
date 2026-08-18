// agent/internal/direct/ondemand_hold_test.go
package direct

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestOnDemandPort_HoldPausesIdleClose verifies the in-flight hold: while a
// hold is active the idle close must not fire (a long streaming response must
// outlive the idle deadline), and once the hold is released the port closes.
func TestOnDemandPort_HoldPausesIdleClose(t *testing.T) {
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	rm := &recordingMapper{}
	p := newTestPort(fc, rm, 20*time.Second) // idle timeout 20s
	defer p.Close()

	if err := p.OpenFor("share-1", time.Minute); err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if _, err := p.BeginSession("share-1"); err != nil {
		t.Fatalf("BeginSession: %v", err)
	}

	tok := p.Begin() // take the in-flight hold

	// Sleep past the idle deadline: the hold must keep the mapping open.
	fc.advance(21 * time.Second)
	if !p.Open() {
		t.Fatalf("port must stay open while a hold is active")
	}

	// Release the hold: the idle deadline has passed, so the port closes.
	p.End(tok)
	waitFor(t, func() bool { return !p.Open() })
	fc.advance(closeRetryDelay + time.Millisecond)
	waitFor(t, func() bool { _, c, _, _ := rm.snapshot(); return c >= 1 })
}

// TestOnDemandPort_HoldDoesNotPreventRenewal verifies that a hold pauses only
// the idle close, not the lease renewal: with an active session the mapping is
// still renewed before the lease expires while held.
func TestOnDemandPort_HoldDoesNotPreventRenewal(t *testing.T) {
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	rm := &recordingMapper{}
	p := newTestPort(fc, rm, 20*time.Second)
	defer p.Close()

	if err := p.OpenFor("share-1", 30*time.Second); err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if _, err := p.BeginSession("share-1"); err != nil {
		t.Fatalf("BeginSession: %v", err)
	}
	_ = p.Begin()

	// The lease (30s) is longer than the idle timeout (20s). Advance past the
	// idle timeout but before the renewal point: the hold keeps it open.
	fc.advance(21 * time.Second)
	if !p.Open() {
		t.Fatalf("port must stay open while held past the idle deadline")
	}

	opened, _, _, _ := rm.snapshot()
	// Advance to just past the renewal point (lease - renewWindow = 28s). The
	// session is still active, so the mapping is renewed despite the hold.
	fc.advance(8 * time.Second) // 29s total
	waitFor(t, func() bool { o, _, _, _ := rm.snapshot(); return o > opened })
	if !p.Open() {
		t.Fatalf("port must stay open and renew while held with an active session")
	}
}

// TestOnDemandPort_StaleEndAfterReopenIsIgnored reproduces the
// cross-generation corruption: a stream is force-closed mid-flight (Close), the
// port is reopened for a different stream, and then the old stream's deferred
// release runs. The stale End token must not decrement the new epoch's
// in-flight count, or it would re-enable the idle close this hold exists to
// prevent.
func TestOnDemandPort_StaleEndAfterReopenIsIgnored(t *testing.T) {
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	rm := &recordingMapper{}
	p := newTestPort(fc, rm, 20*time.Second)
	defer p.Close()

	if err := p.OpenFor("share-1", time.Minute); err != nil {
		t.Fatalf("OpenFor share-1: %v", err)
	}
	if _, err := p.BeginSession("share-1"); err != nil {
		t.Fatalf("BeginSession share-1: %v", err)
	}
	stale := p.Begin() // hold bound to the first open epoch

	// Force-close mid-flight (lockdown / lease-expiry), resetting the hold.
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitFor(t, func() bool { return !p.Open() })

	// Reopen for a different stream; this bumps the open epoch.
	if err := p.OpenFor("share-2", time.Minute); err != nil {
		t.Fatalf("OpenFor share-2: %v", err)
	}
	if _, err := p.BeginSession("share-2"); err != nil {
		t.Fatalf("BeginSession share-2: %v", err)
	}
	fresh := p.Begin() // hold bound to the new epoch
	if fresh == stale {
		t.Fatalf("reopen must issue a fresh epoch token: got %d == stale %d", fresh, stale)
	}

	// The old stream's deferred release runs now, after the reopen. It must be
	// ignored, leaving the new hold (inFlight == 1) intact.
	p.End(stale)

	// Sleep past the idle deadline: the new hold must keep the mapping open.
	fc.advance(21 * time.Second)
	if !p.Open() {
		t.Fatalf("stale End must not release the new hold")
	}

	// Releasing the new hold with its own token closes the port.
	p.End(fresh)
	waitFor(t, func() bool { return !p.Open() })
}

// TestContentRoutesRecordActivity verifies the carried requirement: the content
// routes (/items, /thumb, /preview, /asset, /asset/{id}/playback, /archive)
// record connection activity so browsing arms and refreshes the idle deadline.
// A single connection's first request begins a session and each subsequent
// request records Activity.
func TestContentRoutesRecordActivity(t *testing.T) {
	backend := &handlerBackend{}
	session := &ContentSession{Backend: backend, Membership: map[string]struct{}{"asset-1": {}}}

	ns, base := "sbdeadbeef", "example.com"
	cert := testServerCert(t, ns, base)
	gate := NewSignalGate("a", func(string, RouteKind) bool { return true })
	tr := &recordingTracker{}
	srv := NewDirectServer(ns, base, tr, &rotatableCerts{cert}, gate, 1<<20)
	_ = srv.Binder().Allow(testOrigin, RouteDirect, "abc")
	srv.SetResolver(&fakeResolver{sessions: map[string]*ContentSession{"abc": session}})
	handler := srv.Handler()

	paths := []string{
		"/s/abc/items",
		"/s/abc/thumb/asset-1",
		"/s/abc/preview/asset-1",
		"/s/abc/asset/asset-1",
		"/s/abc/asset/asset-1/playback",
		"/s/abc/archive",
	}

	cs := &connState{}
	for _, path := range paths {
		rr := httptest.NewRecorder()
		req := admittedRequest(http.MethodGet, path, testOrigin)
		req = req.WithContext(context.WithValue(req.Context(), connStateKey{}, cs))
		handler.ServeHTTP(rr, req)
	}

	begins, acts, _ := tr.snapshot()
	if len(begins) != 1 || begins[0] != "abc" {
		t.Fatalf("begins = %v, want exactly [abc]", begins)
	}
	if len(acts) != len(paths)-1 {
		t.Fatalf("acts = %d, want %d (one Activity per subsequent request)", len(acts), len(paths)-1)
	}
}
