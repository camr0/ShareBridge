package daemon

import (
	"sync"
	"testing"
	"time"

	"sharebridge/agent/internal/config"
	"sharebridge/agent/internal/direct"
	"sharebridge/agent/internal/signaling"
)

// fencingMapper wraps recordingDirectMapper to (a) gate ExternalIP behind a
// channel — the slow public-IP lookup of audit Critical #2 — and (b) count
// AddPortMapping calls so a fenced open/renewal can be proven to have issued
// none. All mutable access is mutex-guarded so the open-signal goroutine and a
// concurrent Lockdown may race it cleanly under -race.
type fencingMapper struct {
	*recordingDirectMapper

	mu        sync.Mutex
	ipEntered chan struct{}
	ipRelease chan struct{}
	addCalls  int
}

func newFencingMapper() *fencingMapper {
	return &fencingMapper{recordingDirectMapper: &recordingDirectMapper{ip: "203.0.113.7"}}
}

// gate arms ExternalIP to block until release, signalling entry on entered.
// A nil entered leaves ExternalIP ungated.
func (m *fencingMapper) gate(entered, release chan struct{}) {
	m.mu.Lock()
	m.ipEntered, m.ipRelease = entered, release
	m.mu.Unlock()
}

func (m *fencingMapper) ExternalIP() (string, error) {
	m.mu.Lock()
	entered, release := m.ipEntered, m.ipRelease
	m.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
		<-release
	}
	return m.recordingDirectMapper.ip, nil
}

func (m *fencingMapper) AddPortMapping(ext, internal int, desc string, lease int) (int, error) {
	m.mu.Lock()
	m.addCalls++
	m.mu.Unlock()
	return m.recordingDirectMapper.AddPortMapping(ext, internal, desc, lease)
}

func (m *fencingMapper) addCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.addCalls
}

func (m *fencingMapper) mappingCount() int {
	mappings, _ := m.recordingDirectMapper.ListPortMappings()
	return len(mappings)
}

// newOpenFenceDaemon builds a ready direct state (real SignalGate + real
// OnDemandPort over mapper) with the in-memory signaling mock, so
// handleOpenSignal and Lockdown/Unlock run end-to-end in-process.
func newOpenFenceDaemon(t *testing.T, mapper direct.PortMapper) (*Daemon, *mockSignalingClient, *direct.OnDemandPort) {
	t.Helper()
	cfg := &config.Config{SignalingURL: "ws://localhost:8080", APIKey: "test-key"}
	st := newMockStore()
	sig := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())
	port := direct.NewOnDemandPortOwned(mapper, 443, 8443, time.Minute, "test", "192.168.1.20")
	d := &Daemon{
		store:     st,
		signaling: sig,
		direct: &directState{
			ready:  true,
			gate:   direct.NewSignalGate(st.GetAgentID(), func(string, direct.RouteKind) bool { return true }),
			port:   port,
			mapper: mapper,
		},
	}
	t.Cleanup(func() { _ = port.Close() })
	return d, sig, port
}

// openSignalMessageAt returns an admissible direct open signal with a distinct
// (nonce, seq) so the gate does not reject it as a replay.
func openSignalMessageAt(seq uint64, nonce string) signaling.Message {
	msg := openSignalMessage()
	msg.Seq = seq
	msg.Nonce = nonce
	return msg
}

func awaitRecv(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never arrived", what)
	}
}

func awaitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s", what)
	}
}

// assertFencedOpen is the shared post-condition of a fenced open: no mapping,
// no renewal, no OK open_ack, and the failure surfaced as an error ack.
func assertFencedOpen(t *testing.T, sig *mockSignalingClient, port *direct.OnDemandPort, mapper *fencingMapper) {
	t.Helper()
	if port.Open() {
		t.Errorf("mapping must not be open after a fenced open")
	}
	if n := mapper.mappingCount(); n != 0 {
		t.Errorf("router mappings after a fenced open = %d, want 0", n)
	}
	if n := mapper.addCount(); n != 0 {
		t.Errorf("AddPortMapping calls after a fenced open = %d, want 0", n)
	}
	if sig.hasSentMessage("open_ack", map[string]any{"status": "ok"}) {
		t.Errorf("no OK open_ack may be sent for a fenced open, got %#v", sig.messagesSnapshot())
	}
	if !sig.hasSentMessage("open_ack", map[string]any{"status": "error", "error": "superseded"}) {
		t.Errorf("expected a superseded error open_ack, got %#v", sig.messagesSnapshot())
	}
}

// TestHandleOpenSignalFencedByLockdownDuringExternalIP is the exact audit
// Critical #2 scenario: an open_signal is admitted, blocks in the slow
// ExternalIP lookup, lockdown runs (advancing the direct-state generation and
// closing the mapping), and then the open resumes. It must fail closed: no
// mapping created, no renewal, and no OK open_ack.
func TestHandleOpenSignalFencedByLockdownDuringExternalIP(t *testing.T) {
	mapper := newFencingMapper()
	d, sig, port := newOpenFenceDaemon(t, mapper)

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	mapper.gate(entered, release)

	done := make(chan struct{})
	go func() {
		defer close(done)
		d.handleOpenSignal(openSignalMessage())
	}()

	awaitRecv(t, entered, "handleOpenSignal entering ExternalIP")
	if err := d.Lockdown(); err != nil {
		t.Fatalf("lockdown: %v", err)
	}
	close(release)
	awaitClosed(t, done, "handleOpenSignal did not return after the fence released ExternalIP")

	assertFencedOpen(t, sig, port, mapper)
}

// TestHandleOpenSignalLockdownInterleavings exercises the two transition
// orderings named by the audit, repeatedly, under -race -count. Both must
// leave the port closed with a superseded error ack and no OK ack.
func TestHandleOpenSignalLockdownInterleavings(t *testing.T) {
	t.Run("open_then_lockdown", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			mapper := newFencingMapper()
			d, sig, port := newOpenFenceDaemon(t, mapper)

			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			mapper.gate(entered, release)

			done := make(chan struct{})
			go func() {
				defer close(done)
				d.handleOpenSignal(openSignalMessage())
			}()
			awaitRecv(t, entered, "handleOpenSignal entering ExternalIP")
			if err := d.Lockdown(); err != nil {
				t.Fatalf("lockdown: %v", err)
			}
			close(release)
			awaitClosed(t, done, "handleOpenSignal did not return")
			assertFencedOpen(t, sig, port, mapper)
		}
	})

	t.Run("unlock_then_lockdown", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			mapper := newFencingMapper()
			d, sig, port := newOpenFenceDaemon(t, mapper)

			// A full lock→unlock cycle advances the generation first.
			if err := d.Lockdown(); err != nil {
				t.Fatalf("lockdown: %v", err)
			}
			if err := d.Unlock(); err != nil {
				t.Fatalf("unlock: %v", err)
			}
			if d.IsLocked() {
				t.Fatalf("daemon must be unlocked after Unlock")
			}

			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			mapper.gate(entered, release)

			done := make(chan struct{})
			go func() {
				defer close(done)
				d.handleOpenSignal(openSignalMessage())
			}()
			awaitRecv(t, entered, "handleOpenSignal entering ExternalIP")
			if err := d.Lockdown(); err != nil {
				t.Fatalf("second lockdown: %v", err)
			}
			close(release)
			awaitClosed(t, done, "handleOpenSignal did not return")
			assertFencedOpen(t, sig, port, mapper)
			if !d.IsLocked() {
				t.Errorf("daemon must remain locked")
			}
		}
	})
}

// TestHandleOpenSignalLegitimateOpenAndRenewalUnfenced is the no-false-fencing
// control: with no generation change, a cold open and a subsequent renewal both
// succeed and are acked OK.
func TestHandleOpenSignalLegitimateOpenAndRenewalUnfenced(t *testing.T) {
	mapper := newFencingMapper()
	d, sig, port := newOpenFenceDaemon(t, mapper)

	d.handleOpenSignal(openSignalMessageAt(1, "nonce-cold"))
	if !port.Open() {
		t.Fatalf("legitimate open did not open the mapping")
	}
	if n := mapper.addCount(); n != 1 {
		t.Fatalf("cold open AddPortMapping calls = %d, want 1", n)
	}
	if !sig.hasSentMessage("open_ack", map[string]any{"status": "ok", "was_already_open": false}) {
		t.Fatalf("expected cold OK open_ack, got %#v", sig.messagesSnapshot())
	}

	// A second admitted signal while the mapping is open renews the lease (the
	// fresh 30s lease extends the deadline) and must not be fenced.
	d.handleOpenSignal(openSignalMessageAt(2, "nonce-renew"))
	if n := mapper.addCount(); n != 2 {
		t.Fatalf("renewal AddPortMapping calls = %d, want 2", n)
	}
	if !sig.hasSentMessage("open_ack", map[string]any{"status": "ok", "was_already_open": true}) {
		t.Fatalf("expected renewal OK open_ack, got %#v", sig.messagesSnapshot())
	}
	if !port.Open() {
		t.Fatalf("mapping must stay open after a legitimate renewal")
	}
}
