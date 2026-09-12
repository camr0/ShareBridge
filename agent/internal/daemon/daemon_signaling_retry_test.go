package daemon

import (
	"bytes"
	"context"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sharebridge/agent/internal/config"
	"sharebridge/agent/internal/signaling"
	"sharebridge/agent/internal/tunnel"
)

// retrySignalingStub is a signaling client whose Connect fails a fixed number
// of times before succeeding. The attempt counter lets the tests observe
// retries deterministically (they block on the channel rather than sleeping).
// It embeds the package mock for the rest of the interface.
type retrySignalingStub struct {
	*mockSignalingClient
	connectCalls atomic.Int32
	failCount    int32
	attempts     chan int
	connected    chan struct{}
	connectedOne sync.Once
}

func newRetrySignalingStub(failCount int32) *retrySignalingStub {
	return &retrySignalingStub{
		mockSignalingClient: newMockSignalingClient("ws://signaling.invalid:8080", "test-api-key", "test-agent-id"),
		failCount:           failCount,
		attempts:            make(chan int, 16),
		connected:           make(chan struct{}),
	}
}

func (s *retrySignalingStub) Connect(ctx context.Context) error {
	attempt := int(s.connectCalls.Add(1))
	select {
	case s.attempts <- attempt:
	default:
	}
	if attempt <= int(s.failCount) {
		return errors.New("dial signaling server: connection refused")
	}
	s.connectedOne.Do(func() { close(s.connected) })
	return nil
}

func (s *retrySignalingStub) Listen(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// waitForAttempts blocks until n connect attempts have been observed.
func waitForAttempts(t *testing.T, stub *retrySignalingStub, n int) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-stub.attempts:
		case <-deadline:
			t.Fatalf("timed out waiting for connect attempt %d", i+1)
		}
	}
}

// failingWebServer fails immediately, modelling a local admin surface that
// cannot bind — the runtime class that must still terminate the daemon.
type failingWebServer struct{ err error }

func (w *failingWebServer) Start(ctx context.Context) error { return w.err }
func (w *failingWebServer) Stop() error                     { return nil }
func (w *failingWebServer) SetDaemon(d *Daemon)             {}

func newRetryTestDaemon(t *testing.T, stub SignalingClientInterface) *Daemon {
	t.Helper()
	cfg := &config.Config{
		SignalingURL: "ws://signaling.invalid:8080",
		APIKey:       "test-api-key",
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	d, err := NewWithSignaling(cfgMgr, st, stub)
	require.NoError(t, err)
	return d
}

// TestDaemonStartRetriesInitialConnectInsteadOfExiting is the true-base
// behavioural pin for the carry-forward bug: at the parent the first connect
// error is pushed onto errChan (which cmd/agent/main.go treats as fatal), so
// the daemon exits. After the fix the daemon must still be running after a
// retry and errChan must carry nothing.
func TestDaemonStartRetriesInitialConnectInsteadOfExiting(t *testing.T) {
	stub := newRetrySignalingStub(1 << 30) // never succeeds: every Connect fails
	d := newRetryTestDaemon(t, stub)
	d.SetWebServer(&mockWebServer{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errChan := d.Start(ctx)

	// Wait for the second attempt. By then the parent has pushed the first
	// connect error onto errChan; after the fix nothing has been pushed.
	waitForAttempts(t, stub, 2)

	select {
	case err := <-errChan:
		t.Fatalf("initial connect failure must not be fatal, got errChan=%v", err)
	default:
	}

	cancel()
	_ = d.Stop()
}

// TestDaemonStartSurfacesFatalWebServerError pins the other half of the
// contract: a genuinely fatal local failure still reaches errChan so the
// process can exit.
func TestDaemonStartSurfacesFatalWebServerError(t *testing.T) {
	stub := newRetrySignalingStub(0) // connect succeeds
	d := newRetryTestDaemon(t, stub)
	d.SetWebServer(&failingWebServer{err: errors.New("admin bind failed")})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errChan := d.Start(ctx)

	select {
	case err := <-errChan:
		require.ErrorContains(t, err, "web server")
		require.ErrorContains(t, err, "admin bind failed")
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the fatal web server error")
	}

	cancel()
	_ = d.Stop()
}

// TestTunnelStatusEmissionWithoutConnectionLogsError proves the tunnel
// telemetry path is safe when no WebSocket is established: the real signaling
// client's nil-connection guard returns a logged error, never a panic and
// never a fatal errChan exit.
func TestTunnelStatusEmissionWithoutConnectionLogsError(t *testing.T) {
	cfg := &config.Config{
		SignalingURL: "ws://signaling.invalid:8080",
		APIKey:       "test-api-key",
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	client := signaling.New(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())
	d, err := NewWithSignaling(cfgMgr, st, client)
	require.NoError(t, err)

	var logBuf bytes.Buffer
	originalOutput := log.Writer()
	originalFlags := log.Flags()
	log.SetOutput(&logBuf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(originalOutput)
		log.SetFlags(originalFlags)
	})

	require.NotPanics(t, func() {
		d.handleTunnelStatus(tunnel.StatusReport{
			Generation: 1,
			Status:     tunnel.StatusError,
			Reason:     "tunnel child exited",
		})
	})

	logged := logBuf.String()
	require.Contains(t, logged, "send relay_client_state")
	require.Contains(t, logged, "signaling connection not established")
}

// TestRunSignalingLoopBackoffScheduleDeterministic pins the retry schedule
// through the injected sleep seam: the delay passed for each retry is the
// existing signaling.Backoff value (1s,2s,4s,8s,16s, then the 30s cap, all
// ±20% jitter) — not a new, second backoff.
func TestRunSignalingLoopBackoffScheduleDeterministic(t *testing.T) {
	stub := newRetrySignalingStub(1 << 30) // never succeeds
	d := newRetryTestDaemon(t, stub)

	const samples = 6
	var mu sync.Mutex
	var delays []time.Duration
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.reconnectSleep = func(ctx context.Context, delay time.Duration) bool {
		mu.Lock()
		delays = append(delays, delay)
		n := len(delays)
		mu.Unlock()
		if n >= samples {
			cancel()
			return false
		}
		return true
	}

	done := make(chan struct{})
	go func() {
		d.runSignalingLoop(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runSignalingLoop did not return after the sample bound")
	}

	mu.Lock()
	got := append([]time.Duration(nil), delays...)
	mu.Unlock()
	require.Len(t, got, samples)
	for i, delay := range got {
		base := 30 * time.Second
		if i < 5 { // 1s,2s,4s,8s,16s before the 30s cap
			base = time.Second * time.Duration(1<<i)
		}
		min := time.Duration(float64(base) * 0.8)
		max := time.Duration(float64(base) * 1.2)
		require.GreaterOrEqual(t, delay, min, "sample %d out of band", i)
		require.LessOrEqual(t, delay, max, "sample %d out of band", i)
	}
	require.Equal(t, int32(samples), stub.connectCalls.Load())
}

// TestRunSignalingLoopConnectsWhenEndpointBecomesReachable proves the loop
// recovers in-process: after two failed attempts the third succeeds and the
// listener runs. The two retry delays are the initial backoff values.
func TestRunSignalingLoopConnectsWhenEndpointBecomesReachable(t *testing.T) {
	stub := newRetrySignalingStub(2)
	d := newRetryTestDaemon(t, stub)

	var mu sync.Mutex
	var delays []time.Duration
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.reconnectSleep = func(ctx context.Context, delay time.Duration) bool {
		mu.Lock()
		delays = append(delays, delay)
		mu.Unlock()
		return true
	}

	done := make(chan struct{})
	go func() {
		d.runSignalingLoop(ctx)
		close(done)
	}()

	select {
	case <-stub.connected:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a successful connect")
	}
	require.Equal(t, int32(3), stub.connectCalls.Load())

	mu.Lock()
	got := append([]time.Duration(nil), delays...)
	mu.Unlock()
	require.Len(t, got, 2)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runSignalingLoop did not stop after cancel")
	}
}

// TestRepeatLogLimiterRateLimits proves the outage diagnostic is rate-limited
// but never fully suppressed: the first line logs, repeats inside the
// interval are dropped, and a new outage logs immediately again.
func TestRepeatLogLimiterRateLimits(t *testing.T) {
	limiter := newRepeatLogLimiter(30 * time.Second)
	now := time.Unix(1_700_000_000, 0)
	limiter.now = func() time.Time { return now }

	require.True(t, limiter.shouldLog(), "first outage line logs immediately")
	require.False(t, limiter.shouldLog(), "repeat inside the interval is suppressed")
	now = now.Add(29 * time.Second)
	require.False(t, limiter.shouldLog(), "still suppressed just under the interval")
	now = now.Add(2 * time.Second)
	require.True(t, limiter.shouldLog(), "a line is allowed once the interval passes")

	limiter.reset()
	require.True(t, limiter.shouldLog(), "reset makes the next outage log immediately")
}
