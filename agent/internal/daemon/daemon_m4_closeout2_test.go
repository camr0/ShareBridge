// agent/internal/daemon/daemon_m4_closeout2_test.go
package daemon

// M4 closeout batch 2 — the post-remediation Sol audit's must-fix-before-M6
// composition gap, daemon half.
//
// After a lockdown whose mapping-close lever could not delete the owned router
// mapping (deletion exhausted: StateCloseFailed), `open=false` but the granted
// port and its router mapping linger. Unlock used to restore the listener and
// the Binder unconditionally, so a previous recipient could reuse the known
// direct origin over the lingering mapping with no new open signal. Unlock must
// now refuse to report success — and must not perform any unlocked-success
// lifecycle action — while the port reports an unresolved owned mapping, and a
// later successful release must let a retried Unlock complete.

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sharebridge/agent/internal/direct"
)

// failDeleteDirectMapper wraps the fixture's recording mapper with a togglable
// DeletePortMapping failure. While failing, DeleteOwnedMapping can never remove
// the owned mapping: the router-mapping lease outlives the port's logical open
// state (the close-failed / unresolved-delete state the unlock arm must refuse
// past).
type failDeleteDirectMapper struct {
	*recordingDirectMapper
	mu   sync.Mutex
	fail bool
}

func (m *failDeleteDirectMapper) DeletePortMapping(ext int) error {
	m.mu.Lock()
	fail := m.fail
	m.mu.Unlock()
	if fail {
		return errors.New("router refused the port-mapping delete")
	}
	return m.recordingDirectMapper.DeletePortMapping(ext)
}

func (m *failDeleteDirectMapper) setFail(v bool) {
	m.mu.Lock()
	m.fail = v
	m.mu.Unlock()
}

// waitForPortState waits (bounded, generously above the close-retry schedule)
// for the port to reach a lifecycle state. It is only used where the state is
// driven by the port's own real-clock retry timer.
func waitForPortState(t *testing.T, p *direct.OnDemandPort, want direct.PortState) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if p.State() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("port state = %v, want %v within 10s", p.State(), want)
}

// TestUnlockRefusedWhileOwnedMappingUnresolved proves the second half of the
// fix: while the port owns a router mapping whose deletion is unresolved,
// Unlock must fail closed — no listener restore, no unlocked-success lifecycle
// action, admission still fenced, direct serving not restored.
func TestUnlockRefusedWhileOwnedMappingUnresolved(t *testing.T) {
	rec := &recordingDirectMapper{ip: "203.0.113.7"}
	fm := &failDeleteDirectMapper{recordingDirectMapper: rec, fail: true}
	fx := newLockdownFixtureWithPortMapper(t, rec, fm)
	ds := fx.ds

	// Pre-lockdown: an open mapping, a live listener, one served connection.
	if err := ds.port.OpenFor(fx.code, time.Minute); err != nil {
		t.Fatalf("open mapping: %v", err)
	}
	fx.d.startDirectServer()
	waitDialable(t, ds.listenAddr)
	conn := openRouteConn(t, ds, fx.origin, fx.code)

	fx.d.mu.RLock()
	managerBefore := fx.d.tunnel
	fx.d.mu.RUnlock()

	require.NoError(t, fx.d.Lockdown())
	require.True(t, fx.d.IsLocked())
	assertConnClosed(t, conn)

	// The mapping-close lever's delete was refused: the owned mapping lingers
	// on the router and the port records the unresolved deletion.
	waitForCond(t, func() bool { return ds.port.CloseError() != nil })
	if ds.port.Open() {
		t.Fatalf("port must not be logically open in the unresolved-delete state")
	}
	if got := mappingCount(fx.mapper); got != 1 {
		t.Fatalf("lingering router mappings = %d, want 1", got)
	}

	// Unlock must refuse rather than restore service over the lingering mapping.
	err := fx.d.Unlock()
	require.Error(t, err, "unlock must refuse while an owned mapping is unresolved")

	if _, locked := ds.port.Generation(); !locked {
		t.Fatalf("the port generation stamp must keep its locked bit after the refused unlock")
	}
	fx.d.mu.RLock()
	managerAfter := fx.d.tunnel
	fx.d.mu.RUnlock()
	if managerAfter != managerBefore {
		t.Error("refused unlock rebuilt the tunnel manager")
	}
	if fx.sig.hasSentMessage("relay_credential_request", map[string]any{"reason": "restart"}) {
		t.Error("refused unlock requested a fresh relay credential")
	}
	if got := countSentMessages(fx.sig, "lockdown_status", map[string]any{"locked": false}); got != 0 {
		t.Errorf("refused unlock reported an unlocked status: %d report(s), want 0", got)
	}
	// Direct-open admission stays fenced.
	if err := ds.gate.Admit(validOpenSignal("test-agent-id", fx.code, "nonce-refused")); !errors.Is(err, direct.ErrSignalLockdown) {
		t.Errorf("gate admit after a refused unlock = %v, want ErrSignalLockdown", err)
	}
	// No direct serving restored: the previous listener stayed down.
	assertListenerClosed(t, ds.listenAddr)
}

// TestUnlockRecoversAfterLingeringMappingReleased proves recovery: once the
// router accepts the deletion (its mapping lease expires / the router
// recovers), a retried Unlock performs the release itself, completes, and
// normal direct serving resumes with no operator action.
func TestUnlockRecoversAfterLingeringMappingReleased(t *testing.T) {
	rec := &recordingDirectMapper{ip: "203.0.113.7"}
	fm := &failDeleteDirectMapper{recordingDirectMapper: rec, fail: true}
	fx := newLockdownFixtureWithPortMapper(t, rec, fm)
	ds := fx.ds

	if err := ds.port.OpenFor(fx.code, time.Minute); err != nil {
		t.Fatalf("open mapping: %v", err)
	}
	fx.d.startDirectServer()
	waitDialable(t, ds.listenAddr)
	_ = openRouteConn(t, ds, fx.origin, fx.code)

	require.NoError(t, fx.d.Lockdown())
	// Deletion is exhausted: the port is close-failed with the mapping live.
	waitForPortState(t, ds.port, direct.StateCloseFailed)
	require.Error(t, fx.d.Unlock(), "unlock must refuse while the owned mapping is unresolved")

	// The router recovers (the mapping's lease expires, or the delete is now
	// accepted). The refused unlock's release attempt re-entered the port's
	// bounded retry schedule, so the port clears the lingering mapping on its
	// own — with no operator action — and a retried unlock then completes.
	fm.setFail(false)
	waitForPortState(t, ds.port, direct.StateClosed)
	require.NoError(t, fx.d.Unlock(), "the retried unlock must complete once the owned mapping is released")
	require.False(t, fx.d.IsLocked())
	if _, locked := ds.port.Generation(); locked {
		t.Fatalf("the port generation stamp must be unlocked after the recovered unlock")
	}
	if got := mappingCount(fx.mapper); got != 0 {
		t.Fatalf("router mappings after the recovered unlock = %d, want 0", got)
	}

	// The rebuild replaced the server; re-wire the fixture's resolver onto it
	// so the recovered listener can serve content again.
	ds.mu.Lock()
	server := ds.server
	ds.mu.Unlock()
	server.SetResolver(stubResolver{})

	// Normal direct serving resumes with no operator action: a fresh open and
	// an authorized request succeed.
	require.NoError(t, ds.port.OpenFor(fx.code, time.Minute))
	require.True(t, ds.port.Open())
	conn := openRouteConn(t, ds, fx.origin, fx.code)
	if conn == nil {
		t.Fatalf("recovered direct serving returned no connection")
	}
}
