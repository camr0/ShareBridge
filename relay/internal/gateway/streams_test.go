package gateway

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	streamRouteAlpha = "alpha.relay.ns1.sharebridgeusercontent.com"
	streamRouteBeta  = "beta.relay.ns1.sharebridgeusercontent.com"
	streamRouteGamma = "gamma.relay.ns1.sharebridgeusercontent.com"
	streamAgentAnn   = "agent-record-ann"
	streamAgentBob   = "agent-record-bob"
)

// fakeConn is a net.Conn whose close state is observable without blocking,
// so tests can assert exact close behavior race-free. Any second Close
// reports net.ErrClosed per the net.Conn convention, which proves the
// registry never double-closes when a test observes nil.
type fakeConn struct {
	closed atomic.Bool
}

func (conn *fakeConn) Read(buffer []byte) (int, error) {
	if conn.closed.Load() {
		return 0, net.ErrClosed
	}
	return 0, io.EOF
}

func (conn *fakeConn) Write(buffer []byte) (int, error) {
	if conn.closed.Load() {
		return 0, net.ErrClosed
	}
	return len(buffer), nil
}

func (conn *fakeConn) Close() error {
	if conn.closed.Swap(true) {
		return net.ErrClosed
	}
	return nil
}

func (conn *fakeConn) LocalAddr() net.Addr              { return nil }
func (conn *fakeConn) RemoteAddr() net.Addr             { return nil }
func (conn *fakeConn) SetDeadline(time.Time) error      { return nil }
func (conn *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (conn *fakeConn) SetWriteDeadline(time.Time) error { return nil }

func assertConnClosed(t *testing.T, label string, conn *fakeConn) {
	t.Helper()
	if !conn.closed.Load() {
		t.Fatalf("%s: expected the connection to be closed", label)
	}
}

func assertConnOpen(t *testing.T, label string, conn *fakeConn) {
	t.Helper()
	if conn.closed.Load() {
		t.Fatalf("%s: expected the connection to remain open", label)
	}
}

func TestRegisterAdmittedCommitsOnlyLiveRoutes(t *testing.T) {
	t.Run("admit failure leaves the registry untouched", func(t *testing.T) {
		registry := NewStreams()
		conn := &fakeConn{}

		stream, admitErr := registry.RegisterAdmitted(streamRouteAlpha, streamAgentAnn, conn, func() error {
			return errors.New("routes: route is inactive")
		})
		if stream != nil {
			t.Fatalf("RegisterAdmitted returned a stream despite admit failure")
		}
		if admitErr == nil {
			t.Fatalf("RegisterAdmitted returned no admit error")
		}
		if got := registry.Len(); got != 0 {
			t.Fatalf("registry size after rejected registration = %d, want 0", got)
		}
		if closed := registry.CloseRoute(streamRouteAlpha); closed != 0 {
			t.Fatalf("CloseRoute after rejected registration = %d, want 0", closed)
		}
		// The caller owns the generic close; the registry must not have
		// touched the connection.
		assertConnOpen(t, "connection after rejected registration", conn)
	})

	t.Run("admit success commits the stream to both indexes", func(t *testing.T) {
		registry := NewStreams()
		conn := &fakeConn{}

		stream, admitErr := registry.RegisterAdmitted(streamRouteAlpha, streamAgentAnn, conn, func() error {
			return nil
		})
		if stream == nil || admitErr != nil {
			t.Fatalf("RegisterAdmitted = (%v, %v), want a committed stream", stream, admitErr)
		}
		if got := registry.Len(); got != 1 {
			t.Fatalf("registry size after committed registration = %d, want 1", got)
		}
		if closed := registry.CloseRoute(streamRouteAlpha); closed != 1 {
			t.Fatalf("CloseRoute on a committed stream = %d, want 1", closed)
		}
		assertConnClosed(t, "committed stream connection", conn)
	})

	t.Run("a concurrent revoke waits for admission and then closes the committed stream", func(t *testing.T) {
		// This pins the coordination contract: admit runs while the registry
		// lock is held, so a CloseRoute started mid-admission can never drain
		// the indexes between indexing and the liveness re-check — it waits,
		// then closes the committed stream like any revoke after admission.
		registry := NewStreams()
		conn := &fakeConn{}

		admissionStarted := make(chan struct{})
		admissionRelease := make(chan struct{})
		revokeResult := make(chan int, 1)
		go func() {
			<-admissionStarted      // the revoke fires mid-admission...
			close(admissionRelease) // ...but may only proceed once admission completed
			revokeResult <- registry.CloseRoute(streamRouteAlpha)
		}()

		stream, admitErr := registry.RegisterAdmitted(streamRouteAlpha, streamAgentAnn, conn, func() error {
			close(admissionStarted)
			<-admissionRelease // hold admission open; the revoke must wait, not interleave
			return nil
		})
		if stream == nil || admitErr != nil {
			t.Fatalf("RegisterAdmitted = (%v, %v), want a committed stream", stream, admitErr)
		}
		if closed := <-revokeResult; closed != 1 {
			t.Fatalf("revoke waiting on admission closed %d streams, want 1", closed)
		}
		assertConnClosed(t, "stream closed by the waiting revoke", conn)
		if got := registry.Len(); got != 0 {
			t.Fatalf("registry size after the waiting revoke = %d, want 0", got)
		}
	})
}

func TestRevokeClosesOnlyIndexedRouteStreams(t *testing.T) {
	t.Run("route revoke closes every exact-route stream and none besides", func(t *testing.T) {
		registry := NewStreams()
		alphaFirst, alphaSecond, betaConn := &fakeConn{}, &fakeConn{}, &fakeConn{}
		registry.Register(streamRouteAlpha, streamAgentAnn, alphaFirst)
		registry.Register(streamRouteAlpha, streamAgentAnn, alphaSecond)
		registry.Register(streamRouteBeta, streamAgentBob, betaConn)

		if closed := registry.CloseRoute(streamRouteAlpha); closed != 2 {
			t.Fatalf("CloseRoute(%q) = %d, want 2", streamRouteAlpha, closed)
		}
		assertConnClosed(t, "first alpha stream", alphaFirst)
		assertConnClosed(t, "second alpha stream", alphaSecond)
		assertConnOpen(t, "beta stream", betaConn)
		if got := registry.Len(); got != 1 {
			t.Fatalf("registry size after revoke = %d, want 1", got)
		}

		// A repeated revoke is an idempotent no-op.
		if closed := registry.CloseRoute(streamRouteAlpha); closed != 0 {
			t.Fatalf("repeated CloseRoute(%q) = %d, want 0", streamRouteAlpha, closed)
		}
		assertConnOpen(t, "beta stream after repeat", betaConn)
	})

	t.Run("agent lockdown closes that agent's streams across routes only", func(t *testing.T) {
		registry := NewStreams()
		annOnAlpha, annOnGamma, bobOnBeta := &fakeConn{}, &fakeConn{}, &fakeConn{}
		registry.Register(streamRouteAlpha, streamAgentAnn, annOnAlpha)
		registry.Register(streamRouteGamma, streamAgentAnn, annOnGamma)
		registry.Register(streamRouteBeta, streamAgentBob, bobOnBeta)

		if closed := registry.CloseAgent(streamAgentAnn); closed != 2 {
			t.Fatalf("CloseAgent(%q) = %d, want 2", streamAgentAnn, closed)
		}
		assertConnClosed(t, "ann stream on alpha", annOnAlpha)
		assertConnClosed(t, "ann stream on gamma", annOnGamma)
		assertConnOpen(t, "bob stream", bobOnBeta)
		if got := registry.Len(); got != 1 {
			t.Fatalf("registry size after lockdown = %d, want 1", got)
		}

		if closed := registry.CloseAgent(streamAgentAnn); closed != 0 {
			t.Fatalf("repeated CloseAgent(%q) = %d, want 0", streamAgentAnn, closed)
		}
		assertConnOpen(t, "bob stream after repeat", bobOnBeta)
	})

	t.Run("double close and revoke after close are idempotent no-ops", func(t *testing.T) {
		registry := NewStreams()
		conn := &fakeConn{}
		stream := registry.Register(streamRouteAlpha, streamAgentAnn, conn)

		if err := stream.Close(); err != nil {
			t.Fatalf("first stream.Close() = %v, want nil", err)
		}
		assertConnClosed(t, "stream connection", conn)
		// fakeConn returns net.ErrClosed on any second Close; a nil result
		// proves the registry never closed the connection twice.
		if err := stream.Close(); err != nil {
			t.Fatalf("second stream.Close() = %v, want nil (closing twice must be a no-op)", err)
		}
		if closed := registry.CloseRoute(streamRouteAlpha); closed != 0 {
			t.Fatalf("CloseRoute after manual close = %d, want 0", closed)
		}
		if closed := registry.CloseAgent(streamAgentAnn); closed != 0 {
			t.Fatalf("CloseAgent after manual close = %d, want 0", closed)
		}
		assertConnClosed(t, "stream connection at end", conn)
	})

	t.Run("release on stream close removes it from both indexes", func(t *testing.T) {
		registry := NewStreams()
		alphaConn, betaConn := &fakeConn{}, &fakeConn{}
		alphaStream := registry.Register(streamRouteAlpha, streamAgentAnn, alphaConn)
		registry.Register(streamRouteBeta, streamAgentBob, betaConn)
		if got := registry.Len(); got != 2 {
			t.Fatalf("registry size after registrations = %d, want 2", got)
		}

		// Normal connection exit: one Close must deregister from both the
		// route and the agent index so nothing leaks (spec §14).
		if err := alphaStream.Close(); err != nil {
			t.Fatalf("alpha stream.Close() = %v, want nil", err)
		}
		if got := registry.Len(); got != 1 {
			t.Fatalf("registry size after release = %d, want 1", got)
		}
		if closed := registry.CloseRoute(streamRouteAlpha); closed != 0 {
			t.Fatalf("CloseRoute on released route = %d, want 0", closed)
		}
		if closed := registry.CloseAgent(streamAgentAnn); closed != 0 {
			t.Fatalf("CloseAgent on released agent = %d, want 0", closed)
		}
		assertConnOpen(t, "unrelated beta stream", betaConn)
	})

	t.Run("concurrent registration, release and revoke stay consistent", func(t *testing.T) {
		registry := NewStreams()
		const streamsPerRoute = 64
		// Every third stream exits on its own before the revoke, exercising
		// deregistration racing revocation.
		const earlyExitsPerRoute = (streamsPerRoute + 2) / 3
		const survivingPerRoute = streamsPerRoute - earlyExitsPerRoute

		var waitGroup sync.WaitGroup
		routes := []string{streamRouteAlpha, streamRouteBeta}
		agents := []string{streamAgentAnn, streamAgentBob}
		for routeIndex, routeHostname := range routes {
			for streamIndex := 0; streamIndex < streamsPerRoute; streamIndex++ {
				waitGroup.Add(1)
				go func(routeHostname, agentRecordID string, closeEarly bool) {
					defer waitGroup.Done()
					stream := registry.Register(routeHostname, agentRecordID, &fakeConn{})
					if closeEarly {
						_ = stream.Close()
					}
				}(routeHostname, agents[routeIndex], streamIndex%3 == 0)
			}
		}
		waitGroup.Wait()

		if got := registry.Len(); got != 2*survivingPerRoute {
			t.Fatalf("registry size before revoke = %d, want %d", got, 2*survivingPerRoute)
		}
		if closed := registry.CloseRoute(streamRouteAlpha) + registry.CloseRoute(streamRouteBeta); closed != 2*survivingPerRoute {
			t.Fatalf("revokes closed %d streams, want %d", closed, 2*survivingPerRoute)
		}
		if got := registry.Len(); got != 0 {
			t.Fatalf("registry size after full revoke = %d, want 0 (no unbounded growth)", got)
		}
	})
}
