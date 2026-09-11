package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"sharebridge/agent/internal/direct"
	"sharebridge/agent/internal/signaling"
	"sharebridge/agent/internal/tunnel"
)

// TestDaemonShutdownTunnelLeverBoundIsSafetyNet pins that the daemon's bounded
// tunnel-stop lever is an INDEPENDENT safety net: even when the tunnel manager
// itself has not finished stopping (here it is parked in its graceful-stop
// grace, which is longer than the daemon's lever bound), Daemon.Stop must stop
// waiting at the lever bound, surface the timeout, and run the remaining
// teardown.
func TestDaemonShutdownTunnelLeverBoundIsSafetyNet(t *testing.T) {
	const leverBound = 60 * time.Millisecond
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
	d.shutdownLeverTimeout = leverBound

	starter := &recordingTunnelStarter{stopsOnGraceful: false, ignoresKill: true}
	d.startTunnelManager(context.Background(),
		tunnel.WithProcessStarter(starter.start),
		tunnel.WithBackoffBase(time.Millisecond),
		// Longer than the daemon's lever bound: the manager cannot possibly
		// finish before the daemon gives up waiting for it.
		tunnel.WithKillGracePeriod(2*time.Second),
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
	if err == nil {
		t.Fatal("Daemon.Stop() = nil; the tunnel-stop timeout must be surfaced")
	}
	if !strings.Contains(err.Error(), "did not complete") {
		t.Fatalf("Daemon.Stop() error = %q, want the tunnel lever bound timeout", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Daemon.Stop() waited %v; the %v lever bound must cap the wait", elapsed, leverBound)
	}

	// The remaining levers ran even though the manager was still stopping.
	assertShutdownListenerClosed(t, addr)

	// Release the still-parked manager so its goroutines finish before the test
	// ends; the cleanup Stop joins them.
	starter.recordAt(0).child.signalExit(nil)
}
