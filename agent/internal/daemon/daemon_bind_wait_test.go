package daemon

// M4 closeout batch 1c, finding A1: the listener start's bind-confirmation wait
// must NOT be performed while holding ds.syncMu (or any lock a rebuild needs).
// At the base revision startDirectServer held ds.syncMu for the whole wait, so a
// concurrent syncDirectServe rebuild (and the Unlock / enrollment paths that
// drive it) serialized behind the wait for up to defaultDirectListenStartTimeout
// (2s) — and behind a stalled bind indefinitely up to its bound.
//
// The fix coordinates the start and the rebuild through a single-flight
// in-progress state (observed under ds.mu) instead of a lock held across the
// wait: a rebuild observes the in-flight start, cancels it, and proceeds to its
// OWN bounded listener-exit wait; the superseded start re-evaluates and starts
// the server the rebuild published (or reports the rebuild's fail-closed
// error). These tests are channel-gated, so the ordering is deterministic
// rather than scheduling-dependent.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sharebridge/agent/internal/direct"
)

// stallBind installs a startListenerFn whose first bind confirmation is held
// until release. It deliberately ignores ctx, modelling a listener whose bind
// has not confirmed yet so the stall outlives every daemon bound. Later calls
// report success immediately (so a test can drive recovery).
func stallBind(ds *directState) (entered chan struct{}, release chan struct{}) {
	entered = make(chan struct{})
	release = make(chan struct{})
	var n int32
	ds.startListenerFn = func(ctx context.Context, server *direct.DirectServer, addr string, ready func(error)) error {
		if atomic.AddInt32(&n, 1) == 1 {
			close(entered)
			<-release
		}
		ready(nil)
		return nil
	}
	return entered, release
}

// TestStartBindWaitDoesNotBlockRebuild is the finding-A1 proof. The listener's
// bind confirmation is stalled; a concurrent ordered rebuild must not serialize
// behind that wait. With ds.syncMu no longer held across the bind wait, the
// rebuild acquires it immediately, cancels the stalled start, and fails closed
// on its OWN bounded listener-exit wait (handoffTimeout). At the base revision
// syncDirectServe blocks on ds.syncMu for the whole stalled bind wait, so this
// test times out.
//
// It also pins the recovery contract: the superseded start must NOT report a
// false success for the server the rebuild discarded. It re-evaluates and
// returns the rebuild's fail-closed error; once the stalled listener really
// exits, a fresh rebuild clears the latch and the replacement listener binds
// (B1's ordering and the fence are preserved).
func TestStartBindWaitDoesNotBlockRebuild(t *testing.T) {
	fx := newLockdownFixture(t)
	ds := fx.ds

	ds.mu.Lock()
	ds.handoffTimeout = 100 * time.Millisecond
	ds.listenStartTimeout = 30 * time.Second // the stalled start cannot resolve on its own
	ds.serveNS = ""                          // force the next rebuild to hand off
	ds.mu.Unlock()

	entered, release := stallBind(ds)
	defer func() {
		// Unblock the stalled start on every exit path so the base RED run does
		// not leak the goroutine holding ds.syncMu.
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	startErr := make(chan error, 1)
	go func() { startErr <- fx.d.startDirectServer() }()
	awaitRecv(t, entered, "the listener start never reached its bind wait")

	// The rebuild must not serialize behind the stalled bind wait: it proceeds,
	// cancels the stalled start, and fails closed on its own handoff bound.
	rebuildErr := make(chan error, 1)
	go func() { rebuildErr <- fx.d.syncDirectServe() }()
	select {
	case err := <-rebuildErr:
		if err == nil {
			t.Fatal("the rebuild must fail closed while the old listener does not release its socket")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("syncDirectServe was blocked by the stalled bind wait (ds.syncMu held across it)")
	}

	// B1's fail-closed latch is intact.
	ds.mu.Lock()
	handoffErr := ds.handoffErr
	latched := ds.started
	ds.mu.Unlock()
	require.Error(t, handoffErr, "a fail-closed handoff must stay latched")
	require.True(t, latched, "the start guard must stay latched while the old listener may still serve")

	// The superseded start must report the fail-closed handoff, never a false
	// success: the server it was binding was discarded by the rebuild.
	close(release)
	select {
	case err := <-startErr:
		require.Error(t, err, "a start superseded by a failed handoff must not report success")
	case <-time.After(5 * time.Second):
		t.Fatal("the superseded start never resolved")
	}

	// Recovery: the old listener has exited; a fresh rebuild clears the latch and
	// the replacement (real) listener binds.
	ds.startListenerFn = nil
	ds.mu.Lock()
	ds.serveNS = ""
	ds.mu.Unlock()
	require.NoError(t, fx.d.syncDirectServe(), "the completed handoff must recover")
	require.NoError(t, fx.d.startDirectServer(), "the recovered listener must bind")
	waitDialable(t, ds.listenAddr)
}

// TestConcurrentStartsWaitThenRetry pins the single-flight gate for concurrent
// starts: while the first start's bind is unresolved a second start must NOT
// bind (no two live servers), and when the first bind fails the second waits
// for it and retries, so neither caller observes a false success. This is the
// wait-then-retry serialization ds.syncMu used to provide, now performed
// outside the wait.
func TestConcurrentStartsWaitThenRetry(t *testing.T) {
	fx := newLockdownFixture(t)
	ds := fx.ds
	ds.mu.Lock()
	ds.listenStartTimeout = 10 * time.Second
	ds.mu.Unlock()

	var n int32
	entered := make(chan struct{})
	release := make(chan struct{})
	ds.startListenerFn = func(ctx context.Context, server *direct.DirectServer, addr string, ready func(error)) error {
		if atomic.AddInt32(&n, 1) == 1 {
			close(entered)
			<-release
			ready(errors.New("first bind failed"))
			return errors.New("first bind failed")
		}
		ready(nil)
		return nil
	}

	first := make(chan error, 1)
	go func() { first <- fx.d.startDirectServer() }()
	awaitRecv(t, entered, "the first start never reached its bind wait")

	second := make(chan error, 1)
	go func() { second <- fx.d.startDirectServer() }()
	select {
	case <-second:
		t.Fatal("the second start resolved while the first bind was unresolved")
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	require.Error(t, <-first, "the first bind failure must be reported")
	require.NoError(t, <-second, "the second start must retry and bind")
	if got := atomic.LoadInt32(&n); got != 2 {
		t.Fatalf("listener start calls = %d, want 2 (first failed, second retried)", got)
	}
	fx.d.stopDirectServer()
}
