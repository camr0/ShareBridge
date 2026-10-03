package sctp

// SCTP send-side instrumentation (E35 spike).
//
// WHY THIS EXISTS
//
// E32 showed v1-direct's throughput drops 137.7 Mbps (~11 ms) -> 74.7 Mbps
// (~70 ms) but could only *infer* a DataChannel/SCTP receive-path cause from
// rate plus CPU. No in-flight trace was ever captured, so three candidate fixes
// cannot be told apart:
//
//	A5  a cwnd floor sized to the 70 ms BDP  -> would show cwnd pinned below
//	    the bandwidth-delay product while outstanding stays flat/underfilled.
//	A1  N independent PeerConnections        -> per-association window is the
//	    unit; the trace must be per association (it is: one line per assoc).
//	A2/N5 head-of-line blocking              -> loss-correlated stalls, which
//	    need the RTO vs fast-retransmit split.
//
// This file adds exactly that view: a ~1 Hz JSONL line carrying cwnd, ssthresh,
// rwnd, bytes outstanding, and the retransmit counters split by cause.
//
// THIS IS INERT BY DEFAULT
//
// startTrace() is called once per association and returns after a single
// os.Getenv when SB_SCTP_TRACE is unset: no goroutine, no timer, no write, and
// nothing on the packet path. With the switch off, throughput is unchanged.
//
// Environment (read once per association, at creation):
//
//	SB_SCTP_TRACE=1            enable the emitter
//	SB_SCTP_TRACE_FILE=path    append JSONL to path instead of stderr
//	SB_SCTP_TRACE_EVERY_MS=N   sample interval (default 1000)
//
// Output: one JSON object per line, one line per association per tick. `name`
// identifies the association (pion defaults it to a hex pointer). The counter
// fields are cumulative; diff consecutive lines to get per-interval rates.
//
// WHAT IS REACHABLE
//
// cwnd / ssthresh / rwnd / outstanding are read straight off the Association
// under its read lock (payloadQueue keeps no lock of its own, so the caller
// must hold a.lock). Note pion's naming: `rwnd` is a remaining send credit
// (peer a_rwnd minus outstanding, recomputed per SACK), not the raw advertised
// window, so the snapshot also reports peerRwnd (rwnd + outstanding) and the
// window that actually gates sending, effectiveWindow = min(cwnd, rwnd)
// (association.go: `awnd := min32(a.CWND(), a.RWND())`).
//
// The RTO-vs-fast split is free: this fork's association_stats.go already
// increments nT3Timeouts on the T3 (RTO) path and nFastRetrans on the
// fast-retransmit path; we only surface the totals.
//
// Caveat on that split: nFastRetrans counts chunks actually retransmitted via
// the fast path, NOT "fast recovery entered". This fork halves ssthresh on a
// dup-ACK/RACK loss signal (`ssthresh = max(cwnd/2, 4*MTU)`) even when nothing
// is retransmitted, so diff ssthresh across samples as well: a drop with
// RetransFast unchanged is a loss signal that cost no retransmit, and a drop
// with RetransRTO advancing is a real stall.
//
// Limits: 1 Hz sampling can miss a sub-second burst (the cumulative counters
// still catch it in the diff), and this is the agent/sender's association only
// -- the browser's own SCTP state is not observable from here.

import (
	"encoding/json"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	traceEnableEnv = "SB_SCTP_TRACE"
	traceFileEnv   = "SB_SCTP_TRACE_FILE"
	traceEveryEnv  = "SB_SCTP_TRACE_EVERY_MS"

	traceDefaultEvery = time.Second
	traceMinEvery     = 10 * time.Millisecond
)

// TraceSnapshot is a point-in-time view of one association's send-side state.
// Every byte- and window-valued field is taken from the same critical section,
// so cwnd/ssthresh/rwnd/outstanding are mutually consistent within a line.
type TraceSnapshot struct {
	TS    time.Time `json:"ts"`
	PID   int       `json:"pid"`
	Name  string    `json:"name"`
	State string    `json:"state"`

	// Windows, in bytes.
	//
	// CWND is the sender's congestion window. RWND is NOT the peer's raw
	// advertised receive window: pion keeps it as a *remaining send credit*,
	// recomputed on every SACK as `a_rwnd - bytesOutstanding` (RFC 4960
	// 6.2.1 D.ii) and decremented as new data is sent. The peer's advertised
	// window is therefore PeerRwnd (RWND + outstanding, exact between SACKs).
	// The window that actually gates sending is min(CWND, RWND) -- Effective.
	CWND      uint32 `json:"cwnd"`
	SSTHRESH  uint32 `json:"ssthresh"`
	RWND      uint32 `json:"rwnd"`
	PeerRwnd  uint32 `json:"peerRwnd"`
	Effective uint32 `json:"effectiveWindow"`
	MTU       uint32 `json:"mtu"`

	// Send queue depth.
	Outstanding       int  `json:"outstandingBytes"` // inflightQueue: sent, unacked
	OutstandingChunks int  `json:"outstandingChunks"`
	PendingBytes      int  `json:"pendingBytes"` // queued, not yet handed to SCTP
	FastRecovery      bool `json:"inFastRecovery"`

	SRTTMs float64 `json:"srttMs"`

	// Cumulative byte and packet counters.
	SentBytes       uint64 `json:"sentBytes"`
	RecvBytes       uint64 `json:"recvBytes"`
	PacketsSent     uint64 `json:"packetsSent"`
	PacketsReceived uint64 `json:"packetsReceived"`
	DataChunks      uint64 `json:"dataChunks"`

	// Retransmission accounting, split by cause. RetransTotal is their sum;
	// RTO-driven means the T3 timer expired (a stall), fast-retransmit means
	// duplicate SACKs (loss detected while data still flows).
	RetransRTO    uint64 `json:"retransRTO"`
	RetransFast   uint64 `json:"retransFast"`
	RetransTotal  uint64 `json:"retransTotal"`
	AckTimeouts   uint64 `json:"ackTimeouts"`
	SACKsReceived uint64 `json:"sacksReceived"`
	SACKsSent     uint64 `json:"sacksSent"`

	// Bytes in flight relative to each window. CwndFill >= 1 means the sender
	// is cwnd limited; WindowFill >= 1 means it is limited by min(cwnd, rwnd).
	// Comparing the two says which window binds.
	CwndFill   float64 `json:"cwndFill"`
	WindowFill float64 `json:"windowFill"`
}

// TraceSnapshot returns the association's current send-side state. Safe to call
// at any time from any goroutine.
func (a *Association) TraceSnapshot() TraceSnapshot {
	a.lock.RLock()
	outstanding := a.inflightQueue.getNumBytes()
	outstandingChunks := a.inflightQueue.size()
	pending := a.pendingQueue.getNumBytes()
	ssthresh := a.ssthresh
	fastRecovery := a.inFastRecovery
	a.lock.RUnlock()

	cwnd := a.CWND()
	rwnd := a.RWND()
	fill := 0.0
	if cwnd > 0 {
		fill = float64(outstanding) / float64(cwnd)
	}
	effective := min32(cwnd, rwnd)
	windowFill := 0.0
	if effective > 0 {
		windowFill = float64(outstanding) / float64(effective)
	}

	rto := a.stats.getNumT3Timeouts()
	fast := a.stats.getNumFastRetrans()

	return TraceSnapshot{
		TS:                time.Now(),
		PID:               os.Getpid(),
		Name:              a.name,
		State:             getAssociationStateString(a.getState()),
		CWND:              cwnd,
		SSTHRESH:          ssthresh,
		RWND:              rwnd,
		PeerRwnd:          rwnd + uint32(outstanding), //nolint:gosec // G115
		Effective:         effective,
		MTU:               a.MTU(),
		Outstanding:       outstanding,
		OutstandingChunks: outstandingChunks,
		PendingBytes:      pending,
		FastRecovery:      fastRecovery,
		SRTTMs:            a.SRTT(),
		SentBytes:         a.BytesSent(),
		RecvBytes:         a.BytesReceived(),
		PacketsSent:       a.stats.getNumPacketsSent(),
		PacketsReceived:   a.stats.getNumPacketsReceived(),
		DataChunks:        a.stats.getNumDATAs(),
		RetransRTO:        rto,
		RetransFast:       fast,
		RetransTotal:      rto + fast,
		AckTimeouts:       a.stats.getNumAckTimeouts(),
		SACKsReceived:     a.stats.getNumSACKsReceived(),
		SACKsSent:         a.stats.getNumSACKsSent(),
		CwndFill:          fill,
		WindowFill:        windowFill,
	}
}

var (
	traceMu     sync.Mutex
	traceSink   io.Writer = os.Stderr
	traceSinkID sync.Once
)

// traceWriter resolves the destination once per process. A file is opened
// append-only so several agent restarts accumulate into one trace.
func traceWriter() io.Writer {
	traceSinkID.Do(func() {
		path := strings.TrimSpace(os.Getenv(traceFileEnv))
		if path == "" {
			return
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			// Fall back to stderr rather than losing the trace.
			return
		}
		traceSink = f
	})
	return traceSink
}

func traceWrite(line []byte) {
	traceMu.Lock()
	defer traceMu.Unlock()
	_, _ = traceWriter().Write(line)
}

// traceSetWriter overrides the destination. Test-only helper.
func traceSetWriter(w io.Writer) {
	traceSinkID.Do(func() {})
	traceMu.Lock()
	traceSink = w
	traceMu.Unlock()
}

func traceEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(traceEnableEnv))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func traceInterval() time.Duration {
	raw := strings.TrimSpace(os.Getenv(traceEveryEnv))
	if raw == "" {
		return traceDefaultEvery
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms <= 0 {
		return traceDefaultEvery
	}
	every := time.Duration(ms) * time.Millisecond
	if every < traceMinEvery {
		return traceMinEvery
	}
	return every
}

// startTrace launches the emitter when SB_SCTP_TRACE is set, and does nothing
// at all otherwise. Called once per association, from association creation.
func (a *Association) startTrace() {
	if !traceEnabled() {
		return
	}
	go a.traceLoop(traceInterval())
}

func (a *Association) traceLoop(every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		a.emitTrace()
		select {
		case <-a.readLoopCloseCh:
			return
		case <-ticker.C:
		}
	}
}

func (a *Association) emitTrace() {
	line, err := json.Marshal(a.TraceSnapshot())
	if err != nil {
		return
	}
	traceWrite(append(line, '\n'))
}
