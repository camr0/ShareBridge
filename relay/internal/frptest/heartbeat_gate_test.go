package frptest

import (
	"net"
	"testing"
	"time"

	"sharebridge/relay/internal/frpplugin"
)

// §23.7 — the pinned release honors the explicit 10-second authenticated
// Ping interval and sends its first Ping immediately after login, leaving
// four nominal opportunities inside the 45-second presence lease.
func TestPinnedFRPPingIntervalAndLeaseMargin(t *testing.T) {
	fixture := newPinnedFRPFixture(t, gateFixtureOptions{})
	fixture.startClient(gateClientOptions{Label: "cadence"})

	logins := fixture.callsOf(frpplugin.OperationLogin)
	if len(logins) == 0 {
		logins = fixture.plugin.waitFor(fixture.t, frpplugin.OperationLogin, 1, 5*time.Second)
	}
	loginAt := logins[0].At
	pings := fixture.waitForPings(3, 35*time.Second)

	firstDelay := pings[0].At.Sub(loginAt)
	if firstDelay <= 0 || firstDelay > 2500*time.Millisecond {
		t.Fatalf("first authenticated Ping arrived %s after Login; want immediate (<2.5s)", firstDelay)
	}
	var maxInterval time.Duration
	for i := 1; i < len(pings); i++ {
		interval := pings[i].At.Sub(pings[i-1].At)
		if interval < 8*time.Second || interval > 12*time.Second {
			t.Fatalf("authenticated Ping interval %d was %s; want about 10s", i, interval)
		}
		if interval > maxInterval {
			maxInterval = interval
		}
	}
	if 4*maxInterval >= 45*time.Second {
		t.Fatalf("Ping interval %s leaves no four-Ping margin inside the 45s lease", maxInterval)
	}
	t.Logf("pingCadence: firstPingAfterLogin=%s intervals=%s,%s margin45s>=%s",
		firstDelay.Round(time.Millisecond),
		pings[1].At.Sub(pings[0].At).Round(time.Millisecond),
		pings[2].At.Sub(pings[1].At).Round(time.Millisecond),
		(45*time.Second - 4*maxInterval).Round(time.Millisecond))
}

// §23.7 — one delayed or missed Ping must not tear the tunnel down: after a
// suspension longer than one full heartbeat interval, the next authenticated
// Ping still arrives far inside the 45-second lease and the registered proxy
// keeps serving.
func TestPinnedFRPDelayedPingToleratedUnderLease(t *testing.T) {
	fixture := newPinnedFRPFixture(t, gateFixtureOptions{})
	fixture.startClient(gateClientOptions{Label: "delayed"})
	pings := fixture.waitForPings(2, 25*time.Second)
	lastBefore := pings[len(pings)-1]

	// Freeze the real client past one full heartbeat interval so at least one
	// scheduled Ping is genuinely missed. Cleanup thaws the process first so
	// fixture shutdown can always deliver its graceful close.
	fixture.t.Cleanup(func() { fixture.thawClient("delayed") })
	fixture.freezeClient("delayed")
	time.Sleep(14 * time.Second)
	fixture.thawClient("delayed")

	resumed := fixture.waitForPingAfter(lastBefore.At, 20*time.Second)
	gap := resumed.At.Sub(lastBefore.At)
	if gap <= 12*time.Second {
		t.Fatalf("observed Ping gap %s missed no 10s beat; the tolerance proof would be vacuous", gap)
	}
	if gap >= 40*time.Second {
		t.Fatalf("observed Ping gap %s would breach the 45s lease; want tolerance only below it", gap)
	}

	// Cadence resumes at the normal interval and the tunnel still serves.
	fixture.waitForPings(len(pings)+2, 25*time.Second)
	after := fixture.pings()
	resumeInterval := after[len(after)-1].At.Sub(resumed.At)
	if resumeInterval < 8*time.Second || resumeInterval > 12*time.Second {
		t.Fatalf("post-delay Ping interval %s; want about 10s", resumeInterval)
	}
	readProxyGreeting(t, fixture.proxyAddr())
	t.Logf("delayedPing: gap=%s (>1 missed 10s beat) tolerated; lease margin after gap=%s",
		gap.Round(time.Millisecond), (45*time.Second - gap).Round(time.Millisecond))
}

// §23.7 — true lease expiry: with the client frozen and no Ping arriving at
// all, the tunnel stays present past two full missed beats and then
// transitions offline at the configured 45-second heartbeat timeout.
func TestPinnedFRPTrueLeaseExpiryTransitionsOffline(t *testing.T) {
	fixture := newPinnedFRPFixture(t, gateFixtureOptions{})
	fixture.startClient(gateClientOptions{Label: "expiring"})
	pings := fixture.waitForPings(2, 25*time.Second)
	last := pings[len(pings)-1]

	fixture.t.Cleanup(func() { fixture.thawClient("expiring") })
	fixture.freezeClient("expiring")

	// Presence must survive two full missed beats: at last+20s the listener
	// must still accept.
	sleepUntil(t, last.At.Add(20*time.Second))
	if connection, err := net.DialTimeout("tcp", fixture.proxyAddr(), 500*time.Millisecond); err != nil {
		t.Fatalf("proxy listener vanished after only 20s without a Ping (gap must tolerate missed beats): %v", err)
	} else {
		_ = connection.Close()
	}

	// True expiry: frps tears the session and its listener down at about the
	// configured 45-second heartbeat timeout measured from the last Ping.
	refusedAt, closed := fixture.waitTCPClosed(fixture.proxyAddr(), time.Until(last.At.Add(75*time.Second)))
	if !closed {
		t.Fatalf("proxy listener still present 75s after the last authenticated Ping; true lease expiry did not transition offline")
	}
	elapsed := refusedAt.Sub(last.At)
	if elapsed < 42*time.Second || elapsed > 70*time.Second {
		t.Fatalf("lease-expiry teardown %s after the last Ping; want about the 45s heartbeat timeout", elapsed)
	}
	for _, call := range fixture.pings() {
		if call.At.After(last.At) {
			t.Fatal("authenticated Ping observed after the client was frozen")
		}
	}
	t.Logf("leaseExpiry: lastPingToOffline=%s (heartbeatTimeout=45s); present at +20s with two missed beats", elapsed.Round(time.Millisecond))
}
