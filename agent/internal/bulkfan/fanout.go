package bulkfan

import "sync/atomic"

// Sink is one bulk destination: a single DataChannel on some PeerConnection.
// The manager adapts a multilane.Endpoint to this narrow interface so the
// fan-out logic stays independent of the transport package.
type Sink interface {
	Send(data []byte) error
	BufferedAmount() uint64
}

// Fanout distributes bulk chunks round-robin across N sinks and hands out a
// strictly increasing global chunk sequence. Both A1 (N independent
// PeerConnections) and N5 (K ordered streams on one association) are this same
// distribution; they differ only in how the sinks were created.
//
// One Fanout serves one logical bulk transfer. Calls are expected from the
// single goroutine that reads the transfer source, but the counters are atomic
// so a stray concurrent caller cannot corrupt the assignment.
type Fanout struct {
	sinks []Sink
	next  atomic.Uint64
	seq   atomic.Uint64
}

// NewFanout builds a fan-out over the given sinks. It returns nil when fewer
// than two sinks are supplied so callers can treat nil as "single lane,
// untouched path".
func NewFanout(sinks []Sink) *Fanout {
	if len(sinks) < 2 {
		return nil
	}
	ordered := make([]Sink, len(sinks))
	copy(ordered, sinks)
	return &Fanout{sinks: ordered}
}

// Len reports the number of sinks.
func (f *Fanout) Len() int { return len(f.sinks) }

// Next returns the sink index and global chunk sequence for the next chunk.
// The first call returns (0, 0).
func (f *Fanout) Next() (index int, seq uint64) {
	index = int(f.next.Add(1)-1) % len(f.sinks)
	seq = f.seq.Add(1) - 1
	return index, seq
}

// SendOn sends data to the sink previously returned by Next.
func (f *Fanout) SendOn(index int, data []byte) error { return f.sinks[index].Send(data) }

// SinkAt exposes a sink for tests and for callers that need per-sink
// backpressure sampling.
func (f *Fanout) SinkAt(index int) Sink { return f.sinks[index] }

// BufferedAmount reports the deepest queue across sinks. The striper is
// throttled by whichever association is furthest behind, which is the correct
// bound for a shared source.
func (f *Fanout) BufferedAmount() uint64 {
	var max uint64
	for _, sink := range f.sinks {
		if amount := sink.BufferedAmount(); amount > max {
			max = amount
		}
	}
	return max
}
