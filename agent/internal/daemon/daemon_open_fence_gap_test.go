package daemon

import (
	"sync"
	"testing"
	"time"

	"sharebridge/agent/internal/direct"
	"sharebridge/agent/internal/signaling"
)

// gapMapper is the fix-round-2 fixture: it gates AddPortMapping behind a
// channel so a test can land the §13.4 generation transition in the exact gap
// the reviewer named — after the open/renew predicate has passed and while the
// mapping write is in flight — and it counts AddPortMapping calls. Everything
// else is the round-1 fencingMapper.
type gapMapper struct {
	*fencingMapper

	mu         sync.Mutex
	addEntered chan struct{}
	addRelease chan struct{}
}

func newGapMapper() *gapMapper {
	return &gapMapper{fencingMapper: newFencingMapper()}
}

// gateAdd arms AddPortMapping to signal entry on entered and block until
// release. A nil entered leaves AddPortMapping ungated.
func (m *gapMapper) gateAdd(entered, release chan struct{}) {
	m.mu.Lock()
	m.addEntered, m.addRelease = entered, release
	m.mu.Unlock()
}

func (m *gapMapper) AddPortMapping(ext, internal int, desc string, lease int) (int, error) {
	m.mu.Lock()
	entered, release := m.addEntered, m.addRelease
	m.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
		<-release
	}
	return m.fencingMapper.AddPortMapping(ext, internal, desc, lease)
}

// openSignalMessageLease returns an admissible open signal with an explicit
// lease, so a test can drive the autonomous renewal timer promptly.
func openSignalMessageLease(seq uint64, nonce string, leaseSeconds int) signaling.Message {
	msg := openSignalMessageAt(seq, nonce)
	msg.LeaseSeconds = leaseSeconds
	return msg
}

// waitForLocked polls the daemon's local lockdown flag. The interleave itself
// is channel-gated; this only observes that the transition has been published.
func waitForLocked(t *testing.T, d *Daemon) {
	t.Helper()
	waitForCond(t, d.IsLocked)
}

// stateRecorder records every state transition the port publishes.
type stateRecorder struct {
	mu     sync.Mutex
	states []direct.PortState
}

func (r *stateRecorder) record(_, next direct.PortState, _ int) {
	r.mu.Lock()
	r.states = append(r.states, next)
	r.mu.Unlock()
}

func (r *stateRecorder) sawOpen() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.states {
		if s == direct.StateOpen {
			return true
		}
	}
	return false
}

func (r *stateRecorder) snapshot() []direct.PortState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]direct.PortState(nil), r.states...)
}

// assertNoOKAck fails when any OK open_ack was sent.
func assertNoOKAck(t *testing.T, sig *mockSignalingClient) {
	t.Helper()
	if sig.hasSentMessage("open_ack", map[string]any{"status": "ok"}) {
		t.Errorf("no OK open_ack may be sent for a fenced open, got %#v", sig.messagesSnapshot())
	}
}

// assertSupersededAck fails when the fenced path did not surface the
// superseded error ack.
func assertSupersededAck(t *testing.T, sig *mockSignalingClient) {
	t.Helper()
	if !sig.hasSentMessage("open_ack", map[string]any{"status": "error", "error": "superseded"}) {
		t.Errorf("expected a superseded error open_ack, got %#v", sig.messagesSnapshot())
	}
}

// TestHandleOpenSignalFencedByLockdownDuringAddPortMapping is the reviewer's
// exact round-2 gap for the cold open: the fence predicate has already passed
// and the state loop is inside AddPortMapping when Lockdown performs the
// generation transition. The open must not be published, must not be acked OK,
// and must not leave a mapping behind.
//
// Lockdown runs in a goroutine because its mapping-close lever queues behind
// the in-progress state-loop step; waitForLocked observes the transition
// itself, which is what the fence must react to.
func TestHandleOpenSignalFencedByLockdownDuringAddPortMapping(t *testing.T) {
	scenarios := []struct {
		name  string
		prep  func(t *testing.T, d *Daemon)
		index int
	}{
		{
			name: "open_then_lockdown",
			prep: func(*testing.T, *Daemon) {},
		},
		{
			name: "unlock_then_lockdown",
			prep: func(t *testing.T, d *Daemon) {
				if err := d.Lockdown(); err != nil {
					t.Fatalf("prep lockdown: %v", err)
				}
				if err := d.Unlock(); err != nil {
					t.Fatalf("prep unlock: %v", err)
				}
				if d.IsLocked() {
					t.Fatalf("daemon must be unlocked after Unlock")
				}
			},
		},
	}

	for _, sc := range scenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			for i := 0; i < 3; i++ {
				mapper := newGapMapper()
				d, sig, port := newOpenFenceDaemon(t, mapper)
				sc.prep(t, d)

				rec := &stateRecorder{}
				port.SetTransitionCallback(rec.record)

				entered := make(chan struct{}, 1)
				release := make(chan struct{})
				mapper.gateAdd(entered, release)

				done := make(chan struct{})
				go func() {
					defer close(done)
					d.handleOpenSignal(openSignalMessageAt(uint64(i+1), "nonce-cold"))
				}()
				awaitRecv(t, entered, "handleOpenSignal entering AddPortMapping")

				lockdownDone := make(chan struct{})
				go func() {
					defer close(lockdownDone)
					_ = d.Lockdown()
				}()
				waitForLocked(t, d)
				close(release)
				awaitClosed(t, done, "handleOpenSignal did not return")
				awaitClosed(t, lockdownDone, "Lockdown did not return")

				assertSupersededAck(t, sig)
				assertNoOKAck(t, sig)
				if rec.sawOpen() {
					t.Errorf("the port must never transition to open when fenced during AddPortMapping: %v", rec.snapshot())
				}
				waitForCond(t, func() bool { return mapper.mappingCount() == 0 })

				// A post-lockdown open must still fail closed.
				before := mapper.addCount()
				d.handleOpenSignal(openSignalMessageAt(uint64(100+i), "nonce-after"))
				if n := mapper.addCount(); n != before {
					t.Errorf("a post-lockdown open created a mapping: add calls %d → %d", before, n)
				}
				if d.IsLocked() != true {
					t.Errorf("daemon must remain locked")
				}
			}
		})
	}
}

// TestHandleOpenSignalFencedByLockdownDuringRenewal is the reviewer's round-2
// gap for the renewal fast path: the mapping is already open, the renewal
// predicate has passed, and the generation transition lands while the renewal
// write is in flight. The renewal must not be published and must not be acked
// OK.
func TestHandleOpenSignalFencedByLockdownDuringRenewal(t *testing.T) {
	mapper := newGapMapper()
	d, sig, port := newOpenFenceDaemon(t, mapper)

	// A legitimate cold open establishes the mapping.
	d.handleOpenSignal(openSignalMessageAt(1, "nonce-cold"))
	if !port.Open() {
		t.Fatalf("legitimate cold open did not open the mapping")
	}

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	mapper.gateAdd(entered, release)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// A longer lease guarantees the fast path takes the renewal branch.
		d.handleOpenSignal(openSignalMessageLease(2, "nonce-renew", 600))
	}()
	awaitRecv(t, entered, "renewal entering AddPortMapping")

	lockdownDone := make(chan struct{})
	go func() {
		defer close(lockdownDone)
		_ = d.Lockdown()
	}()
	waitForLocked(t, d)
	close(release)
	awaitClosed(t, done, "the fenced renewal did not return")
	awaitClosed(t, lockdownDone, "Lockdown did not return")

	if sig.hasSentMessage("open_ack", map[string]any{"status": "ok", "was_already_open": true}) {
		t.Errorf("a renewal fenced during AddPortMapping must not be acked OK, got %#v", sig.messagesSnapshot())
	}
	assertSupersededAck(t, sig)
	waitForCond(t, func() bool { return mapper.mappingCount() == 0 })
}

// TestHandleOpenSignalFencedByLockdownDuringTimerRenewal proves the autonomous
// timer-driven renewal (the `timerC` branch) is fenced too. The lockdown
// mapping-close lever is held so that the ONLY actor that could remove the
// mapping is the port's own generation check; at the unfenced HEAD the timer
// renews and the mapping survives.
func TestHandleOpenSignalFencedByLockdownDuringTimerRenewal(t *testing.T) {
	mapper := newGapMapper()
	d, _, port := newOpenFenceDaemon(t, mapper)

	// Hold the close lever past the transition so it cannot mask an unfenced
	// renewal. The test unblocks it in cleanup, after its assertions.
	closeGate := make(chan struct{})
	d.lockdownLeverGate = func(name string) {
		if name == "close on-demand mapping" {
			<-closeGate
		}
	}
	d.lockdownLeverTimeout = 200 * time.Millisecond
	t.Cleanup(func() { close(closeGate) })

	// minValidLease is 5s and renewWindow is 2s, so the autonomous renewal is
	// scheduled ~3s out. A session keeps the renewable branch armed.
	d.handleOpenSignal(openSignalMessageLease(1, "nonce-cold", 5))
	if !port.Open() {
		t.Fatalf("legitimate cold open did not open the mapping")
	}
	if _, err := port.BeginSession("SHARE123"); err != nil {
		t.Fatalf("BeginSession: %v", err)
	}

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	mapper.gateAdd(entered, release)

	// The scheduled renewal fires and blocks inside AddPortMapping.
	awaitRecv(t, entered, "the timer renewal entering AddPortMapping")

	lockdownDone := make(chan struct{})
	go func() {
		defer close(lockdownDone)
		_ = d.Lockdown()
	}()
	waitForLocked(t, d)
	close(release)
	awaitClosed(t, lockdownDone, "Lockdown did not return")

	waitForCond(t, func() bool { return mapper.mappingCount() == 0 })
	if port.Open() {
		t.Errorf("a timer renewal fenced by lockdown must not leave the port open")
	}
}
