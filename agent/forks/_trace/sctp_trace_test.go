package sctp

import (
	"bytes"
	"encoding/json"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/logging"
)

// testAssociation builds a bare Association without starting readLoop/writeLoop,
// so a test can drive its send-side state directly.
func testAssociation(t *testing.T) *Association {
	t.Helper()
	return createAssociationFromConfigWithTsn(&Config{
		LoggerFactory: logging.NewDefaultLoggerFactory(),
	}, 1000)
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestTraceSnapshotReadsAssociationState pins the load-bearing fields: the
// snapshot must report cwnd/ssthresh/rwnd and the bytes sitting in the inflight
// queue.
func TestTraceSnapshotReadsAssociationState(t *testing.T) {
	a := testAssociation(t)

	a.lock.Lock()
	a.setCWND(131072)
	a.ssthresh = 262144
	a.setRWND(524288)
	a.inflightQueue.pushNoCheck(&chunkPayloadData{userData: make([]byte, 4096)})
	a.inflightQueue.pushNoCheck(&chunkPayloadData{userData: make([]byte, 8192)})
	a.lock.Unlock()

	snap := a.TraceSnapshot()
	if snap.CWND != 131072 {
		t.Fatalf("cwnd = %d, want 131072", snap.CWND)
	}
	if snap.SSTHRESH != 262144 {
		t.Fatalf("ssthresh = %d, want 262144", snap.SSTHRESH)
	}
	if snap.RWND != 524288 {
		t.Fatalf("rwnd = %d, want 524288", snap.RWND)
	}
	// rwnd is a remaining credit, so the peer's advertised window is the sum
	// with what is still outstanding, and the gating window is min(cwnd, rwnd).
	if want := uint32(524288 + 12288); snap.PeerRwnd != want {
		t.Fatalf("peerRwnd = %d, want %d", snap.PeerRwnd, want)
	}
	if snap.Effective != 131072 {
		t.Fatalf("effectiveWindow = %d, want 131072 (min of cwnd and rwnd)", snap.Effective)
	}
	if snap.Outstanding != 12288 {
		t.Fatalf("outstanding = %d, want 12288", snap.Outstanding)
	}
	if snap.OutstandingChunks != 2 {
		t.Fatalf("outstandingChunks = %d, want 2", snap.OutstandingChunks)
	}
	if snap.RetransTotal != snap.RetransRTO+snap.RetransFast {
		t.Fatalf("retransTotal %d != rto %d + fast %d",
			snap.RetransTotal, snap.RetransRTO, snap.RetransFast)
	}
}

// TestTraceInertWhenDisabled is the default-behaviour guarantee: with
// SB_SCTP_TRACE unset, startTrace must not write anything and must not leave a
// goroutine behind.
func TestTraceInertWhenDisabled(t *testing.T) {
	t.Setenv(traceEnableEnv, "")

	sink := &lockedBuffer{}
	traceSetWriter(sink)

	a := testAssociation(t)

	before := runtime.NumGoroutine()
	a.startTrace()
	time.Sleep(80 * time.Millisecond)

	if got := sink.String(); got != "" {
		t.Fatalf("trace wrote %q while disabled", got)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && runtime.NumGoroutine() != before {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got != before {
		t.Fatalf("goroutines %d -> %d: disabled trace started a goroutine", before, got)
	}
}

// TestTraceEmitsWhenEnabled proves the switch turns on a JSONL stream carrying
// the window state.
func TestTraceEmitsWhenEnabled(t *testing.T) {
	t.Setenv(traceEnableEnv, "1")
	t.Setenv(traceEveryEnv, "25")

	sink := &lockedBuffer{}
	traceSetWriter(sink)

	a := testAssociation(t) // constructor calls startTrace()
	defer close(a.readLoopCloseCh)

	deadline := time.Now().Add(2 * time.Second)
	var line string
	for time.Now().Before(deadline) {
		if s := sink.String(); s != "" {
			line = strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if line == "" {
		t.Fatal("no trace line emitted while enabled")
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(line), &decoded); err != nil {
		t.Fatalf("trace line is not JSON: %v (%q)", err, line)
	}
	for _, key := range []string{
		"ts", "name", "state", "cwnd", "ssthresh", "rwnd", "peerRwnd",
		"effectiveWindow", "outstandingBytes", "outstandingChunks",
		"retransRTO", "retransFast", "retransTotal", "srttMs", "cwndFill", "windowFill",
	} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("trace line missing %q: %s", key, line)
		}
	}
	if decoded["cwnd"].(float64) <= 0 {
		t.Fatalf("cwnd not reported: %s", line)
	}
}

// TestTraceIntervalFloors guards the sampling interval against a zero/absurd
// value that would spin the emitter.
func TestTraceIntervalFloors(t *testing.T) {
	t.Setenv(traceEveryEnv, "")
	if got := traceInterval(); got != traceDefaultEvery {
		t.Fatalf("empty interval = %v, want %v", got, traceDefaultEvery)
	}
	t.Setenv(traceEveryEnv, "0")
	if got := traceInterval(); got != traceDefaultEvery {
		t.Fatalf("zero interval = %v, want %v", got, traceDefaultEvery)
	}
	t.Setenv(traceEveryEnv, "1")
	if got := traceInterval(); got != traceMinEvery {
		t.Fatalf("1ms interval = %v, want floor %v", got, traceMinEvery)
	}
	t.Setenv(traceEveryEnv, "250")
	if got := traceInterval(); got != 250*time.Millisecond {
		t.Fatalf("250ms interval = %v", got)
	}
}
