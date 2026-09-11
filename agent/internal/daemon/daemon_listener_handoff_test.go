package daemon

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sharebridge/agent/internal/direct"
	"sharebridge/agent/internal/signaling"
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
func (g *listenerGates) start(ctx context.Context, server *direct.DirectServer, addr string, ready func(error)) error {
	if g.next() > 1 {
		select {
		case <-g.replacementEntered:
		default:
			close(g.replacementEntered)
		}
		return server.StartWithReady(ctx, addr, ready)
	}
	if g.serveRealFirst {
		innerCtx, innerCancel := context.WithCancel(context.Background())
		defer innerCancel()
		errCh := make(chan error, 1)
		go func() { errCh <- server.StartWithReady(innerCtx, addr, ready) }()
		close(g.firstBound)
		<-ctx.Done()
		close(g.cancelSeen)
		<-g.releaseFirst
		innerCancel()
		return <-errCh
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		ready(err)
		return err
	}
	ready(nil)
	close(g.firstBound)
	<-ctx.Done()
	close(g.cancelSeen)
	<-g.releaseFirst
	ln.Close()
	close(g.oldExited)
	return nil
}

// TestListenerHandoffWaitsForOldSocketRelease forces the C1 race: the old
// listener's socket is held open past the handoff's cancel, so the replacement
// listener must not be entered until the old listener has released its socket.
//
// The ordering is asserted WITHOUT a timing window. The replacement's entry is
// the only point where a premature bind can happen, so the injected listener
// checks the old listener's exit signal (oldExited) exactly there: a correct
// ordered handoff returns its wait only on that signal, so oldExited is closed
// whenever the replacement is entered, while any handoff that skipped the wait
// enters the replacement with oldExited still open. The check is a plain state
// read, and the test joins the handoff goroutine before asserting, so a
// violation can never be recorded after the assertion. (The previous version
// used a 250ms negative assertion; that timing window is gone.)
func TestListenerHandoffWaitsForOldSocketRelease(t *testing.T) {
	fx := newLockdownFixture(t)
	ds := fx.ds
	// A generous handoff bound: while the test holds the old socket the only way
	// the ordered wait can end is by the old listener's exit, never the timeout.
	ds.handoffTimeout = 30 * time.Second

	g := newListenerGates()
	orderViolated := make(chan struct{})
	var violationOnce sync.Once
	ds.startListenerFn = func(ctx context.Context, server *direct.DirectServer, addr string, ready func(error)) error {
		if g.callCount() >= 1 { // the old listener already bound: this is the replacement
			select {
			case <-g.oldExited:
			default:
				violationOnce.Do(func() { close(orderViolated) })
			}
		}
		return g.start(ctx, server, addr, ready)
	}

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
	var handoffSyncErr, handoffStartErr error
	go func() {
		ds.mu.Lock()
		ds.serveNS = ""
		ds.mu.Unlock()
		handoffSyncErr = fx.d.syncDirectServe()
		if handoffSyncErr == nil {
			handoffStartErr = fx.d.startDirectServer()
		}
		close(handoffDone)
	}()

	select {
	case <-g.cancelSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("old listener never observed the handoff cancel")
	}

	// Release the old socket; only now may the replacement be entered.
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
	require.NoError(t, handoffSyncErr, "handoff failed closed unexpectedly")
	require.NoError(t, handoffStartErr, "replacement listener failed to start")
	// The replacement listener is started on its own goroutine, so wait for it
	// to enter; the ordering check runs inside that entry, before this returns.
	waitForCond(t, func() bool { return g.callCount() >= 2 })
	select {
	case <-orderViolated:
		t.Fatal("replacement listener was entered before the old listener released its socket")
	default:
	}
	if got := g.callCount(); got != 2 {
		t.Fatalf("listener start calls = %d, want 2 (the replacement must bind)", got)
	}
	waitDialable(t, ds.listenAddr)
}

// countSentMessages counts the messages of one type whose fields match, so a
// transition test can assert absence (0) as precisely as presence.
func countSentMessages(sig *mockSignalingClient, msgType string, fields map[string]any) int {
	count := 0
	for _, msg := range sig.messagesSnapshot() {
		if hasSentMessage([]map[string]any{msg}, msgType, fields) {
			count++
		}
	}
	return count
}

// TestUnlockHandoffTimeoutFailsClosedWithoutLifecycleActions pins the
// operational contract of a failed ordered handoff: when Unlock cannot prove
// the old listener released its socket, it must report the failure to its
// caller and must NOT emit any of the unlocked-success lifecycle actions — no
// tunnel restart, no fresh credential request, no unlocked status report, no
// replacement listener — while the daemon stays fail-closed. A later rebuild
// that completes the handoff recovers service, and the retried Unlock then
// completes the owed relay restore.
func TestUnlockHandoffTimeoutFailsClosedWithoutLifecycleActions(t *testing.T) {
	fx := newLockdownFixture(t)
	ds := fx.ds
	ds.handoffTimeout = 100 * time.Millisecond
	g := newListenerGates()
	ds.startListenerFn = g.start

	// The listener that lockdown will stop holds its socket past every bound.
	fx.d.startDirectServer()
	select {
	case <-g.firstBound:
	case <-time.After(5 * time.Second):
		t.Fatal("listener never bound")
	}

	fx.d.handleSignalingMessage(relayConfigMessage(t, 1, "credential-before-lockdown"))
	waitForCond(t, func() bool { return fx.starter.startCount() == 1 })

	require.NoError(t, fx.d.Lockdown())
	require.True(t, fx.d.IsLocked())
	select {
	case <-g.cancelSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("lockdown never cancelled the listener")
	}

	// (a) Unlock reports the handoff failure instead of success.
	fx.d.mu.RLock()
	managerBefore := fx.d.tunnel
	fx.d.mu.RUnlock()
	err := fx.d.Unlock()
	if err == nil {
		t.Error("unlock must report the listener-handoff failure, not success")
	} else if !strings.Contains(err.Error(), "did not release its socket") {
		t.Errorf("unlock error = %v, want the ordered-handoff failure", err)
	}

	// (b) No unlocked-success lifecycle action ran. The tunnel manager identity
	// is the deterministic signal: the success path replaces the stopped
	// manager (restartTunnelManager), the failure path must leave it alone.
	fx.d.mu.RLock()
	managerAfterFailure := fx.d.tunnel
	fx.d.mu.RUnlock()
	if managerAfterFailure != managerBefore {
		t.Error("failed unlock rebuilt the tunnel manager")
	}
	if got := fx.starter.startCount(); got != 1 {
		t.Errorf("failed unlock restarted the tunnel: %d start(s), want 1", got)
	}
	if fx.sig.hasSentMessage("relay_credential_request", map[string]any{"reason": "restart"}) {
		t.Error("failed unlock requested a fresh relay credential")
	}
	if got := countSentMessages(fx.sig, "lockdown_status", map[string]any{"locked": false}); got != 0 {
		t.Errorf("failed unlock reported an unlocked status: %d report(s), want 0", got)
	}

	// (c) No replacement listener was bound.
	select {
	case <-g.replacementEntered:
		t.Error("failed unlock bound a replacement listener")
	default:
	}
	if got := g.callCount(); got != 1 {
		t.Errorf("listener start calls = %d, want 1 (no replacement)", got)
	}

	// (d) The daemon is still fail-closed: the handoff latch is held, the start
	// guard stays latched, the start path reports the failure, and the binder no
	// longer admits the share. (The start check runs before the sync latch check
	// below so one RED/GREEN run observes both.)
	startErr := fx.d.startDirectServer()
	ds.mu.Lock()
	handoffErr := ds.handoffErr
	latched := ds.started
	ds.mu.Unlock()
	if startErr == nil {
		t.Error("startDirectServer must report the fail-closed handoff, not success")
	}
	if handoffErr == nil {
		t.Error("a fail-closed handoff must stay latched, not be cleared")
	}
	if !latched {
		t.Error("the start guard must stay latched while the old listener may still serve")
	}
	if _, admitErr := ds.binder.AdmitSNI(fx.origin); admitErr == nil {
		t.Error("a failed unlock must not leave the direct origin admitted")
	}

	// (e) Recovery: once the old listener releases its socket, the rebuild
	// completes and restores the listener and its source-verified admissions.
	// The bound is widened first: joining a real event, not racing a timeout.
	ds.mu.Lock()
	ds.handoffTimeout = 10 * time.Second
	ds.mu.Unlock()
	close(g.releaseFirst)
	select {
	case <-g.oldExited:
	case <-time.After(5 * time.Second):
		t.Fatal("old listener never released its socket")
	}
	ds.mu.Lock()
	ds.serveNS = ""
	ds.mu.Unlock()
	require.NoError(t, fx.d.syncDirectServe(), "a completed handoff must recover")
	require.NoError(t, fx.d.startDirectServer(), "the recovered listener must bind")
	waitDialable(t, ds.listenAddr)
	waitForCond(t, func() bool {
		_, err := ds.binder.AdmitSNI(fx.origin)
		return err == nil
	})

	// The owed relay restore still runs on the retried unlock: the stopped
	// manager is replaced and a fresh credential is requested, exactly once.
	require.NoError(t, fx.d.Unlock())
	require.False(t, fx.d.IsLocked())
	fx.d.mu.RLock()
	managerAfterRetry := fx.d.tunnel
	fx.d.mu.RUnlock()
	if managerAfterRetry == nil || managerAfterRetry == managerBefore {
		t.Error("the retried unlock must rebuild the stopped tunnel manager")
	}
	if got := countSentMessages(fx.sig, "relay_credential_request", map[string]any{"reason": "restart"}); got != 1 {
		t.Errorf("relay credential requests = %d, want exactly 1", got)
	}
}

// TestDirectServeRebuildAndStartPropagateFailClosedHandoff pins error
// propagation for the operational callers other than Unlock: the rebuild
// itself (syncDirectServe), the listener start (startDirectServer), and the two
// signaling-loop handlers that drive them (handleEnrolled, handleEnrollmentReady)
// must all surface the fail-closed handoff instead of reporting success, while
// the §7.1 baseline work they own still runs and no replacement listener binds.
func TestDirectServeRebuildAndStartPropagateFailClosedHandoff(t *testing.T) {
	fx := newLockdownFixture(t)
	ds := fx.ds
	ds.handoffTimeout = 100 * time.Millisecond
	g := newListenerGates()
	ds.startListenerFn = g.start

	require.NoError(t, fx.d.startDirectServer())
	select {
	case <-g.firstBound:
	case <-time.After(5 * time.Second):
		t.Fatal("listener never bound")
	}

	// The rebuild returns the fail-closed handoff error instead of stashing it.
	ds.mu.Lock()
	ds.serveNS = ""
	ds.mu.Unlock()
	require.Error(t, fx.d.syncDirectServe(), "the rebuild must return the fail-closed error")

	// The start path reports the latched failure, not success.
	require.Error(t, fx.d.startDirectServer(), "startDirectServer must report the fail-closed handoff")

	// The signaling-loop rebuild path surfaces it too, while still running the
	// §7.1 certificate reconciliation (whichever branch the fixture's leaf is in):
	// a direct-path failure must not gate baseline enrollment.
	require.Error(t, fx.d.handleEnrolled(signaling.Message{Type: "enrolled", Namespace: testDirectNS}),
		"handleEnrolled must surface the rebuild failure")
	require.True(t,
		fx.sig.hasSentMessage("csr_submit", nil) || fx.sig.hasSentMessage("tls_ready", nil),
		"the §7.1 certificate reconciliation must still run when the direct path failed")

	// The enrollment_ready/start path surfaces it as well, and baseline
	// readiness is not rolled back by the optional direct-path failure.
	require.Error(t, fx.d.handleEnrollmentReady(signaling.Message{Type: "enrollment_ready"}),
		"handleEnrollmentReady must surface the listener start failure")
	ds.mu.Lock()
	ready := ds.ready
	ds.mu.Unlock()
	require.True(t, ready, "baseline readiness must not be rolled back by a direct-path failure")

	// None of them bound a replacement listener behind the stuck one.
	select {
	case <-g.replacementEntered:
		t.Fatal("a replacement listener was bound despite the fail-closed handoff")
	default:
	}
	if got := g.callCount(); got != 1 {
		t.Fatalf("listener start calls = %d, want 1 (no replacement)", got)
	}
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
	if err := fx.d.syncDirectServe(); err == nil {
		t.Fatal("handoff must return the fail-closed error when the old listener does not exit within the bound")
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
	if err := fx.d.startDirectServer(); err == nil {
		t.Fatal("startDirectServer must report the latched fail-closed handoff")
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
	require.NoError(t, fx.d.syncDirectServe(), "a completed handoff must clear the fail-closed error")
	require.NoError(t, fx.d.startDirectServer())
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
	var handoffErr error
	go func() {
		ds.mu.Lock()
		ds.serveNS = ""
		ds.mu.Unlock()
		handoffErr = fx.d.syncDirectServe()
		if handoffErr == nil {
			handoffErr = fx.d.startDirectServer()
		}
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
	require.NoError(t, handoffErr)
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
		require.NoErrorf(t, fx.d.syncDirectServe(), "rebuild %d failed closed", i)
		require.NoErrorf(t, fx.d.startDirectServer(), "rebuild %d listener did not start", i)
		waitDialable(t, ds.listenAddr)
	}

	// Then the T30 lockdown/unlock rebuild interaction.
	for i := 0; i < 5; i++ {
		require.NoError(t, fx.d.Lockdown())
		require.True(t, fx.d.IsLocked())
		require.NoError(t, fx.d.Unlock())
		require.False(t, fx.d.IsLocked())
		waitDialable(t, ds.listenAddr)
	}
}
