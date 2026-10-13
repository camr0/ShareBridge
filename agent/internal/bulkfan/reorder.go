package bulkfan

import (
	"errors"
	"fmt"
)

// DefaultReorderLimit bounds how many out-of-order chunks may be buffered
// before the stream is considered unrecoverable. At the agent's 64 KiB chunk
// size this is 1 GiB of stalled payload — far beyond any plausible striping
// window, so exceeding it means a lost or misrouted stream, not jitter.
const DefaultReorderLimit = 1 << 14

// ErrReorderOverflow is returned when the buffer exceeds its configured limit.
var ErrReorderOverflow = errors.New("bulkfan: reorder buffer overflow")

// ReorderBuffer reassembles a chunk stream that was striped across N sinks back
// into global sequence order. It is the reference implementation for the
// browser-side reorder in signaling-server/web/src/bulkReorder.js and is used
// directly by Go receivers (cmd/fieldrecv).
//
// One buffer serves one operation. The agent numbers chunks from 0, so the
// buffer always expects 0 first and holds anything later until the gap closes.
type ReorderBuffer struct {
	next    uint64
	limit   int
	pending map[uint64][]byte
	dropped uint64
}

// NewReorderBuffer returns a buffer that accepts the default number of
// out-of-order chunks.
func NewReorderBuffer() *ReorderBuffer { return NewReorderBufferWithLimit(DefaultReorderLimit) }

// NewReorderBufferWithLimit returns a buffer with an explicit pending limit.
// A limit <= 0 uses DefaultReorderLimit.
func NewReorderBufferWithLimit(limit int) *ReorderBuffer {
	if limit <= 0 {
		limit = DefaultReorderLimit
	}
	return &ReorderBuffer{limit: limit, pending: make(map[uint64][]byte)}
}

// Push records payload under seq and returns every payload that has become
// consecutive, in sequence order. Duplicates and sequences already emitted are
// discarded. A gap leaves payload buffered until the missing sequence arrives.
//
// Push returns ErrReorderOverflow (and drops the payload) when the buffer would
// exceed its limit.
func (r *ReorderBuffer) Push(seq uint64, payload []byte) ([][]byte, error) {
	if seq < r.next {
		r.dropped++
		return nil, nil
	}
	if seq > r.next {
		if _, exists := r.pending[seq]; exists {
			// Duplicate out-of-order copy; keep the first.
			r.dropped++
			return nil, nil
		}
		if len(r.pending) >= r.limit {
			return nil, fmt.Errorf("%w: %d chunks pending, next=%d", ErrReorderOverflow, len(r.pending), r.next)
		}
		r.pending[seq] = append([]byte(nil), payload...)
		return nil, nil
	}

	out := make([][]byte, 0, 1)
	out = append(out, payload)
	r.next++
	for {
		buffered, ok := r.pending[r.next]
		if !ok {
			break
		}
		delete(r.pending, r.next)
		out = append(out, buffered)
		r.next++
	}
	return out, nil
}

// Next reports the next sequence the buffer expects.
func (r *ReorderBuffer) Next() uint64 { return r.next }

// Pending reports how many out-of-order chunks are buffered.
func (r *ReorderBuffer) Pending() int { return len(r.pending) }

// Dropped reports how many duplicate/late chunks were discarded.
func (r *ReorderBuffer) Dropped() uint64 { return r.dropped }

// Reset clears all buffered state.
func (r *ReorderBuffer) Reset() {
	r.next = 0
	r.dropped = 0
	r.pending = make(map[uint64][]byte)
}
