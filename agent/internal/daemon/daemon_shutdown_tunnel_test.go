package daemon

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"sharebridge/agent/internal/direct"
	"sharebridge/agent/internal/signaling"
	"sharebridge/agent/internal/tunnel"
)

// Round D (post-remediation audit I4 / M5 / T33): Daemon.Stop used to stop the
// tunnel synchronously before closing the HTTPS listener, so an unkillable
// frpc child stranded the whole remaining shutdown. These tests pin the new
// ordering contract: the tunnel-stop lever is bounded, the remaining teardown
// levers (HTTPS listener, established connections, web/admin surface, direct
// state) always execute, and the tunnel failure is surfaced to the caller —
// never swallowed.

// TestDaemonShutdownNotBlockedByUnkillableTunnel is the ordering regression:
// with a tunnel child that ignores both graceful stop and kill, Daemon.Stop
// must still return (within the tunnel bound), surface the tunnel error, and
// have executed every remaining shutdown lever.
func TestDaemonShutdownNotBlockedByUnkillableTunnel(t *testing.T) {
	const (
		killGrace = 10 * time.Millisecond
		killWait  = 60 * time.Millisecond
	)
	addr := reserveLoopbackAddr(t)
	cfg := tunnelTestConfig(t)
	st := newMockStore()
	sig := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())

	gate := direct.NewSignalGate(st.GetAgentID(), func(string, direct.RouteKind) bool { return true })
	d := &Daemon{config: cfg, configMgr: &mockConfigManager{cfg: cfg}, store: st, signaling: sig}
	d.direct = &directState{
		ready:      true,
		gate:       gate,
		baseDomain: testDirectBase,
		namespace:  testDirectNS,
		server:     direct.NewDirectServer(testDirectNS, testDirectBase, nil, nil, gate, 1<<20),
		origins:    map[string]originPair{},
		listenAddr: addr,
	}
	web := &mockWebServer{}
	d.SetWebServer(web)

	starter := &recordingTunnelStarter{stopsOnGraceful: false, ignoresKill: true}
	d.startTunnelManager(context.Background(),
		tunnel.WithProcessStarter(starter.start),
		tunnel.WithBackoffBase(time.Millisecond),
		tunnel.WithKillGracePeriod(killGrace),
		tunnel.WithKillWaitTimeout(killWait),
		tunnel.WithCredentialWaitTimeout(time.Hour),
		tunnel.WithRunningStabilityWindow(time.Hour),
	)
	if d.tunnel == nil {
		t.Fatal("tunnel manager must be constructed when tunnel settings are configured")
	}
	t.Cleanup(func() { _ = d.stopTunnelManager() })

	d.handleSignalingMessage(relayConfigMessage(t, 1, "credential-generation-one"))
	waitForCond(t, func() bool { return starter.startCount() == 1 })
	d.handleSignalingMessage(signaling.Message{Type: "enrollment_ready"})
	waitDialable(t, addr)

	// A real accepted connection proves the teardown closes established
	// traffic, not merely stops accepting new connections.
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial HTTPS listener: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	stopReturned := make(chan error, 1)
	go func() { stopReturned <- d.Stop() }()

	select {
	case err := <-stopReturned:
		if err == nil {
			t.Fatal("Daemon.Stop() = nil; the tunnel stop failure must be surfaced, not swallowed")
		}
		if !strings.Contains(err.Error(), "tunnel") {
			t.Fatalf("Daemon.Stop() error = %q, want it to name the tunnel stop failure", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Daemon.Stop() did not return: an unkillable tunnel child stranded shutdown")
	}

	// Every remaining lever ran despite the tunnel failure.
	assertShutdownListenerClosed(t, addr)
	assertShutdownConnClosed(t, conn)
	if !web.stopped {
		t.Fatal("the web/admin surface must be stopped even when the tunnel stop fails")
	}
	ds := d.direct
	ds.mu.Lock()
	started, cancelSet := ds.started, ds.cancel != nil
	ds.mu.Unlock()
	if started || cancelSet {
		t.Fatalf("direct serving state must be reset at shutdown (started=%v cancelSet=%v)", started, cancelSet)
	}
}

// assertShutdownListenerClosed polls until the loopback listener refuses
// connections, failing if it is still accepting after the deadline.
func assertShutdownListenerClosed(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			return // the listener socket is gone
		}
		_ = conn.Close()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("HTTPS listener still accepting connections after daemon shutdown")
}

// assertShutdownConnClosed proves an established connection was closed by the
// shutdown teardown rather than left open until its server-side read timeout.
func assertShutdownConnClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, readErr := conn.Read(make([]byte, 1))
	if readErr == nil {
		t.Fatal("established direct connection survived daemon shutdown")
	}
	var netErr net.Error
	if errors.As(readErr, &netErr) && netErr.Timeout() {
		t.Fatal("established direct connection was not closed within the shutdown deadline")
	}
}

// TestDaemonShutdownNotBlockedByWedgedRelayStateSender is the fix-round
// regression for the audit's exact trigger: an unkillable tunnel child AND a
// relay_client_state send that ignores its (cooperative) context timeout.
// Round D bounded the wait for the child, but shutdownChild still emitted the
// final statuses synchronously, so the wedged send parked the manager run
// goroutine: Manager.Stop never returned and Daemon.Stop only got past its
// tunnel lever once the outer lever bound fired — with the wrong error.
func TestDaemonShutdownNotBlockedByWedgedRelayStateSender(t *testing.T) {
	const leverBound = 3 * time.Second
	addr := reserveLoopbackAddr(t)
	cfg := tunnelTestConfig(t)
	st := newMockStore()
	sig := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())
	relayBlock := make(chan struct{})
	var releaseOnce sync.Once
	releaseSender := func() { releaseOnce.Do(func() { close(relayBlock) }) }
	sig.relayStateBlock = relayBlock
	t.Cleanup(releaseSender)

	gate := direct.NewSignalGate(st.GetAgentID(), func(string, direct.RouteKind) bool { return true })
	d := &Daemon{config: cfg, configMgr: &mockConfigManager{cfg: cfg}, store: st, signaling: sig}
	d.direct = &directState{
		ready:      true,
		gate:       gate,
		baseDomain: testDirectBase,
		namespace:  testDirectNS,
		server:     direct.NewDirectServer(testDirectNS, testDirectBase, nil, nil, gate, 1<<20),
		origins:    map[string]originPair{},
		listenAddr: addr,
	}
	// Smaller than the killeable-child bound so the assertion is about the
	// manager's own bound, not the daemon's independent safety net.
	d.shutdownLeverTimeout = leverBound

	starter := &recordingTunnelStarter{stopsOnGraceful: false, ignoresKill: true}
	d.startTunnelManager(context.Background(),
		tunnel.WithProcessStarter(starter.start),
		tunnel.WithBackoffBase(time.Millisecond),
		tunnel.WithKillGracePeriod(10*time.Millisecond),
		tunnel.WithKillWaitTimeout(60*time.Millisecond),
		tunnel.WithStatusDrainTimeout(50*time.Millisecond),
		tunnel.WithCredentialWaitTimeout(time.Hour),
		tunnel.WithRunningStabilityWindow(time.Hour),
	)
	t.Cleanup(func() { _ = d.stopTunnelManager() })

	d.handleSignalingMessage(relayConfigMessage(t, 1, "credential-generation-one"))
	waitForCond(t, func() bool { return starter.startCount() == 1 })
	d.handleSignalingMessage(signaling.Message{Type: "enrollment_ready"})
	waitDialable(t, addr)

	started := time.Now()
	err := d.Stop()
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("Daemon.Stop() = nil; the tunnel stop failure must be surfaced")
	}
	if !errors.Is(err, tunnel.ErrChildKillTimeout) {
		t.Fatalf("Daemon.Stop() error = %q, want the manager's precise ErrChildKillTimeout", err)
	}
	if elapsed >= leverBound {
		t.Fatalf("Daemon.Stop() waited %v; a wedged relay_client_state send must not push it to the %v lever bound", elapsed, leverBound)
	}

	// The remaining levers still ran while the sender stayed wedged.
	assertShutdownListenerClosed(t, addr)

	releaseSender()
	starter.recordAt(0).child.signalExit(nil)
}
