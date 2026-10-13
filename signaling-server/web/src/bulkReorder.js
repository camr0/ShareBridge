// Reorder buffer mirroring agent/internal/bulkfan/reorder.go.
//
// A1 (N independent PeerConnections) and N5 (K ordered streams on one
// association) both stripe one bulk operation across several lanes and stamp a
// global chunk sequence in the v2 chunk envelope. Each lane preserves its own
// order, so the receiver buffers any chunk whose sequence is ahead of the next
// expected one and releases runs of consecutive chunks as the gaps close.
//
// One buffer serves one operation. Sequences are BigInt because the agent's
// counter is a uint64.

export const DEFAULT_REORDER_LIMIT = 1 << 14

export class ReorderOverflowError extends Error {
  constructor(pending, next) {
    super(`bulk reorder buffer overflow: ${pending} chunks pending, next=${next}`)
    this.name = 'ReorderOverflowError'
    this.pending = pending
    this.next = next
  }
}

export class ReorderBuffer {
  constructor(limit = DEFAULT_REORDER_LIMIT) {
    if (!Number.isInteger(limit) || limit <= 0) {
      throw new RangeError('reorder limit must be a positive integer')
    }
    this.limit = limit
    this.reset()
  }

  reset() {
    this.next = 0n
    this.buffered = new Map()
    this.dropped = 0
  }

  get pending() {
    return this.buffered.size
  }

  /**
   * Records a chunk and returns every payload that became consecutive, in
   * sequence order. Duplicates and already-emitted sequences are dropped.
   * Throws ReorderOverflowError when the buffer would exceed its limit.
   */
  push(seq, payload) {
    const sequence = typeof seq === 'bigint' ? seq : BigInt(seq)
    if (sequence < this.next) {
      this.dropped += 1
      return []
    }
    if (sequence > this.next) {
      if (this.buffered.has(sequence)) {
        this.dropped += 1
        return []
      }
      if (this.buffered.size >= this.limit) {
        throw new ReorderOverflowError(this.buffered.size, this.next)
      }
      this.buffered.set(sequence, payload)
      return []
    }

    const out = [payload]
    this.next += 1n
    while (this.buffered.has(this.next)) {
      out.push(this.buffered.get(this.next))
      this.buffered.delete(this.next)
      this.next += 1n
    }
    return out
  }
}

/**
 * Wraps a ReorderBuffer so a caller can feed every arriving chunk and invoke an
 * append callback only for in-order runs. `onArrival` is called for every chunk
 * (including reordered ones) so byte accounting stays exact.
 */
export function createOrderedSink({ onAppend, onArrival, limit } = {}) {
  if (typeof onAppend !== 'function') throw new TypeError('onAppend must be a function')
  const buffer = new ReorderBuffer(limit)
  return {
    buffer,
    push(seq, payload) {
      onArrival?.(payload)
      const ordered = buffer.push(seq, payload)
      for (const chunk of ordered) onAppend(chunk)
      return ordered.length
    },
    reset() {
      buffer.reset()
    },
  }
}
