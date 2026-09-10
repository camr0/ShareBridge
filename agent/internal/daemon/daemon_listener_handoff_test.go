package daemon

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sharebridge/agent/internal/direct"
)

// These tests pin the audit-C1 ordered listener handoff: a rebuild must drain
// the old server's connection registry, cancel its listener, and wait
// (bounded) for the old listener goroutine to release its socket BEFORE the
// replacement server is published and bound. They use the startListenerFn seam
// so the old listener's exit is gated behind a channel, making the race
// deterministic rather than scheduling-dependent.

// listenerGates is the shared test harness for the injected listener: the first
// (old) listener is a raw TCP bind whose exit the test controls; later
// (replacement) listeners use the real server.
type listenerGates struct {
	mu                 sync.Mutex
	calls              int
	firstBound         chan struct{} // old listener holds its socket
	cancelSeen         chan struct{} // old listener observed the handoff cancel
	releaseFirst       chan struct{} // test releases the old socket
	oldExited          chan struct{} // old listener released its socket
	replacementEntered chan struct{} // a replacement Start was entered

	// serveRealFirst makes the old listener serve the REAL server on an inner
	// context (so the handoff's cancel cannot tear it down): only the handoff's
	// registry drain can close its connections. Used by the registry-drain test.
	serveRealFirst bool
}

func newListenerGates() *listenerGates {
	return &listenerGates{
		firstBound:         make(chan struct{}),
		cancelSeen:         make(chan struct{}),
		releaseFirst:       make(chan struct{}),
		oldExited:          make(chan struct{}),
		replacementEntered: make(chan struct{}),
	}
}

func (g *listenerGates) next() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	return g.calls
}

func (g *listenerGates) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// start is the injected startListenerFn.
func (g *listenerGates) start(ctx context.Context, server *direct.DirectServer, addr string) error {
	if g.next() > 1 {
		select {
		case <-g.replacementEntered:
		default:
			close(g.replacementEntered)
		}
		return server.Start(ctx, addr)
	}
	if g.serveRealFirst {
		innerCtx, innerCancel := context.WithCancel(context.Background())
		defer innerCancel()
		errCh := make(chan error, 1)
		go func() { errCh <- server.Start(innerCtx, addr) }()
		close(g.firstBound)
		<-ctx.Done()
		close(g.cancelSeen)
		<-g.releaseFirst
		innerCancel()
		return <-errCh
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	close(g.firstBound)
	<-ctx.Done()
	close(g.cancelSeen)
	<-g.releaseFirst
	ln.Close()
	close(g.oldExited)
	return nil
}

// TestListenerHandoffWaitsForOldSocketRelease forces the C1 race: the old
// listener's socket is held open past the handoff's cancel, so the rebuild must
// not return or enter the replacement listener until the old listener exits. At
// HEAD the handoff returned immediately and the replacement either bound over a
// held socket (bind failure) or raced it; with the fix it blocks on the exit
// signal.
func TestListenerHandoffWaitsForOldSocketRelease(t *testing.T) {
	fx := newLockdownFixture(t)
	ds := fx.ds
	g := newListenerGates()
	ds.startListenerFn = g.start

	fx.d.startDirectServer()
	select {
	case <-g.firstBound:
	case <-time.After(5 * time.Second):
		t.Fatal("old listener never bound")
	}
	waitDialable(t, ds.listenAddr)

	// Trigger an ordered rebuild exactly like Unlock does (force serveNS stale,
	// rebuild, then (re)start), but hold the old socket until we say so.
	handoffDone := make(chan struct{})
	go func() {
		ds.mu.Lock()
		ds.serveNS = ""
		ds.mu.Unlock()
		fx.d.syncDirectServe()
		fx.d.startDirectServer()
		close(handoffDone)
	}()

	select {
	case <-g.cancelSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("old listener never observed the handoff cancel")
	}

	// The old socket is still held (releaseFirst is still open). At HEAD the
	// handoff proceeds anyway; with the fix it must still be blocked.
	select {
	case <-handoffDone:
		t.Fatal("listener handoff returned before the old listener released its socket")
	case <-g.replacementEntered:
		t.Fatal("replacement listener started before the old listener released its socket")
	case <-time.After(250 * time.Millisecond):
		// Still correctly blocked: assert the failure mode is impossible by
		// construction rather than by timing below.
	}

	close(g.releaseFirst)
	select {
	case <-g.oldExited:
	case <-time.After(5 * time.Second):
		t.Fatal("old listener never released its socket after release")
	}
	select {
	case <-handoffDone:
	case <-time.After(5 * time.Second):
		t.Fatal("handoff never completed after the old socket was released")
	}
	if err := fx.d.directHandoffErr(); err != nil {
		t.Fatalf("handoff failed closed unexpectedly: %v", err)
	}
	waitDialable(t, ds.listenAddr)
}

// TestListenerHandoffFailsClosedWhenOldListenerStuck pins the fail-closed
// branch: if the old listener does not release its socket within the bound, the
// handoff must surface an error, must NOT publish the replacement server, and
// must keep the start guard latched so the replacement cannot bind. A later
// handoff that completes the wait clears the state and recovers.
func TestListenerHandoffFailsClosedWhenOldListenerStuck(t *testing.T) {
	fx := newLockdownFixture(t)
	ds := fx.ds
	ds.handoffTimeout = 100 * time.Millisecond

	g := newListenerGates()
	// This variant never closes releaseFirst until the test says so, so the old
	// listener overruns the bound.
	releaseOnce := sync.Once{}
	release := func() { releaseOnce.Do(func() { close(g.releaseFirst) }) }
	defer release()
	ds.startListenerFn = g.start

	fx.d.startDirectServer()
	select {
	case <-g.firstBound:
	case <-time.After(5 * time.Second):
		t.Fatal("old listener never bound")
	}
	oldServer := ds.server

	ds.mu.Lock()
	ds.serveNS = ""
	ds.mu.Unlock()
	fx.d.syncDirectServe() // blocks for handoffTimeout, then fails closed

	if err := fx.d.directHandoffErr(); err == nil {
		t.Fatal("handoff must fail closed when the old listener does not exit within the bound")
	}
	if got := ds.server; got != oldServer {
		t.Fatal("replacement server was published despite the fail-closed handoff")
	}
	ds.mu.Lock()
	latched := ds.started
	ds.mu.Unlock()
	if !latched {
		t.Fatal("start guard must stay latched after a fail-closed handoff")
	}
	select {
	case <-g.replacementEntered:
		t.Fatal("replacement listener was started despite the fail-closed handoff")
	default:
	}

	// Recovery: once the old listener exits, a fresh handoff completes the wait,
	// clears the error, and the replacement binds.
	release()
	ds.mu.Lock()
	ds.serveNS = ""
	ds.mu.Unlock()
	fx.d.syncDirectServe()
	if err := fx.d.directHandoffErr(); err != nil {
		t.Fatalf("a completed handoff must clear the fail-closed error: %v", err)
	}
	fx.d.startDirectServer()
	waitForCond(t, func() bool { return g.callCount() >= 2 })
	waitDialable(t, ds.listenAddr)
}

// TestListenerHandoffDrainsOldServerRegistry pins that the ordered handoff
// closes the old server's active connections through its own T29 registry
// (before cancelling the listener), so the closers are never orphaned. The old
// listener serves the REAL server on an inner context, so only the handoff's
// registry drain can close the connection.
func TestListenerHandoffDrainsOldServerRegistry(t *testing.T) {
	fx := newLockdownFixture(t)
	ds := fx.ds
	g := newListenerGates()
	g.serveRealFirst = true
	ds.startListenerFn = g.start

	fx.d.startDirectServer()
	select {
	case <-g.firstBound:
	case <-time.After(5 * time.Second):
		t.Fatal("old listener never bound")
	}
	waitDialable(t, ds.listenAddr)
	oldServer := ds.server
	conn := openRouteConn(t, ds, fx.origin, fx.code)

	handoffDone := make(chan struct{})
	go func() {
		ds.mu.Lock()
		ds.serveNS = ""
		ds.mu.Unlock()
		fx.d.syncDirectServe()
		fx.d.startDirectServer()
		close(handoffDone)
	}()

	select {
	case <-g.cancelSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("old listener never observed the handoff cancel")
	}

	// The old listener's inner context is still live, so the connection can only
	// have been closed by the handoff's registry drain. CloseAllConns is
	// idempotent and reports only connections it actually closed, so 0 here
	// proves the drain already ran.
	if n := oldServer.CloseAllConns(); n != 0 {
		t.Fatalf("handoff did not drain the old server's registry: CloseAllConns closed %d connection(s)", n)
	}
	assertConnClosed(t, conn)

	close(g.releaseFirst)
	select {
	case <-handoffDone:
	case <-time.After(5 * time.Second):
		t.Fatal("handoff never completed")
	}
	// The drained connection stays closed after the handoff.
	assertConnClosed(t, conn)
	waitDialable(t, ds.listenAddr)
}

// TestListenerHandoffSurvivesRapidLockUnlock runs repeated lockdown/unlock
// cycles over the same listen address under -race and -count>=3. Before the
// ordered handoff this tripped "bind: address already in use" because Unlock
// bound the replacement while the lockdown-cancelled listener was still
// releasing its socket.
func TestListenerHandoffSurvivesRapidLockUnlock(t *testing.T) {
	fx := newLockdownFixture(t)
	ds := fx.ds
	fx.d.startDirectServer()
	waitDialable(t, ds.listenAddr)

	// Tight rebuild cycles first: each reuses the same address with no
	// lock/unlock overhead, maximally shrinking the cancel->bind gap. Before
	// the ordered handoff this raced the old listener's socket release
	// ("bind: address already in use") and the listener then never became
	// dialable.
	for i := 0; i < 25; i++ {
		ds.mu.Lock()
		ds.serveNS = ""
		ds.mu.Unlock()
		fx.d.syncDirectServe()
		fx.d.startDirectServer()
		waitDialable(t, ds.listenAddr)
		if err := fx.d.directHandoffErr(); err != nil {
			t.Fatalf("rebuild %d failed closed: %v", i, err)
		}
	}

	// Then the T30 lockdown/unlock rebuild interaction.
	for i := 0; i < 5; i++ {
		require.NoError(t, fx.d.Lockdown())
		require.True(t, fx.d.IsLocked())
		require.NoError(t, fx.d.Unlock())
		require.False(t, fx.d.IsLocked())
		waitDialable(t, ds.listenAddr)
		if err := fx.d.directHandoffErr(); err != nil {
			t.Fatalf("handoff failed closed on lock/unlock cycle %d: %v", i, err)
		}
	}
}
