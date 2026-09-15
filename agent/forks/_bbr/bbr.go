package sctp

// BBR-lite patch (spike).
//
// Stock pion initialises ssthresh to the peer's receiver window (~5MB from
// Chrome) and slow-starts toward it. On any bottleneck whose buffer is smaller
// than rwnd that over-drives the queue, tail-drops, and collapses -- measured at
// 14.5 Mbps (23%) on a 64 Mbps link where kernel TCP gets 86%.
//
// This patch instead slow-starts until the measured delivery rate stops growing,
// then pins cwnd to the estimated bandwidth-delay product. It makes ssthresh
// irrelevant, needs no probe, and cannot strangle a fast link because the rate
// estimate rises with observed delivery.

import (
	"sync"
	"time"
)

type bbrState struct {
	delivered   uint64
	roundEnd    time.Time
	decayAt     time.Time
	rate        float64 // max delivery rate seen, bytes/s (decayed periodically)
	minRTT      float64 // ms, windowed min
	noGrowth    int
	startupDone bool
}

// Keyed by *Association and never deleted. Acceptable for a benchmark spike; a
// production version would hang this off the Association and release it on close.
var bbrStates sync.Map

func bbrStateFor(a *Association) *bbrState {
	if v, ok := bbrStates.Load(a); ok {
		return v.(*bbrState)
	}
	st := &bbrState{}
	actual, _ := bbrStates.LoadOrStore(a, st)
	return actual.(*bbrState)
}

// getSRTT exposes the RTO manager's smoothed RTT (ms) to the BDP estimator.
func (m *rtoManager) getSRTT() float64 {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	return m.srtt
}

// bbrOnAck samples delivery rate and, once slow start has been exited, clamps
// cwnd to the estimated BDP. Caller holds the association lock.
//
// Rate is aggregated over whole rounds (one min-RTT each) rather than per-sample:
// a 20ms instantaneous sample is far too noisy to distinguish "still slow-starting"
// from "hit the buffer", and exiting startup early pins cwnd at a low estimate
// that then prevents the rate from ever growing.
func (a *Association) bbrOnAck(acked int) {
	if acked <= 0 {
		return
	}
	st := bbrStateFor(a)
	now := time.Now()
	if st.roundEnd.IsZero() {
		st.roundEnd, st.decayAt = now, now
	}
	st.delivered += uint64(acked)
	if srtt := a.rtoMgr.getSRTT(); srtt > 0 && (st.minRTT <= 0 || srtt < st.minRTT) {
		st.minRTT = srtt
	}

	roundMs := st.minRTT
	if roundMs <= 0 {
		roundMs = 100
	}
	sinceRoundMs := float64(now.Sub(st.roundEnd).Milliseconds())
	if sinceRoundMs < roundMs {
		return // not a full round yet
	}

	roundRate := float64(st.delivered) * 1000.0 / sinceRoundMs // bytes/s
	st.delivered = 0
	st.roundEnd = now

	prev := st.rate
	if roundRate > st.rate {
		st.rate = roundRate
	}

	if !st.startupDone {
		switch {
		case prev == 0 || roundRate >= prev*1.25:
			st.noGrowth = 0
		default:
			st.noGrowth++
			if st.noGrowth >= 3 {
				st.startupDone = true
			}
		}
	}

	// Let the estimate fall if the path changes underneath us.
	if now.Sub(st.decayAt) > 10*time.Second {
		st.rate *= 0.5
		st.decayAt = now
	}

	if !st.startupDone || st.rate <= 0 || st.minRTT <= 0 {
		return
	}
	bdp := uint32(st.rate * (st.minRTT / 1000.0))
	if floor := 4 * a.MTU(); bdp < floor {
		bdp = floor
	}
	if a.CWND() > bdp {
		a.setCWND(bdp)
	}
}
