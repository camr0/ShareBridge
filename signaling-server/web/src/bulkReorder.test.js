import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  ReorderBuffer,
  ReorderOverflowError,
  createOrderedSink,
} from './bulkReorder.js'

const utf8 = (text) => new TextEncoder().encode(text)

test('passes in-order chunks straight through', () => {
  const buffer = new ReorderBuffer()
  assert.deepEqual(buffer.push(0n, utf8('a')).map(String), [String(utf8('a'))])
  assert.deepEqual(buffer.push(1n, utf8('b')).map(String), [String(utf8('b'))])
  assert.equal(buffer.pending, 0)
  assert.equal(buffer.next, 2n)
})

test('buffers a gap and releases the run when it closes', () => {
  const buffer = new ReorderBuffer()
  assert.deepEqual(buffer.push(2n, utf8('c')), [])
  assert.deepEqual(buffer.push(1n, utf8('b')), [])
  assert.equal(buffer.pending, 2)
  assert.equal(buffer.next, 0n)

  const released = buffer.push(0n, utf8('a'))
  assert.equal(released.length, 3)
  assert.deepEqual(released.map((chunk) => chunk[0]), [97, 98, 99])
  assert.equal(buffer.pending, 0)
  assert.equal(buffer.next, 3n)
})

test('drops duplicates and late chunks', () => {
  const buffer = new ReorderBuffer()
  buffer.push(0n, utf8('a'))
  buffer.push(1n, utf8('b'))
  assert.deepEqual(buffer.push(1n, utf8('b-again')), [])
  buffer.push(3n, utf8('d'))
  assert.deepEqual(buffer.push(3n, utf8('d-again')), [])
  assert.equal(buffer.dropped, 2)

  const released = buffer.push(2n, utf8('c'))
  assert.equal(released.length, 2)
  assert.deepEqual(released.map((chunk) => chunk[0]), [99, 100])
})

test('requires sequence zero before releasing later chunks', () => {
  const buffer = new ReorderBuffer()
  assert.deepEqual(buffer.push(5n, utf8('f')), [])
  assert.equal(buffer.pending, 1)
  assert.deepEqual(buffer.push(0n, utf8('a')).map((chunk) => chunk[0]), [97])
  assert.equal(buffer.pending, 1, 'the 5 is still waiting for 1..4')
})

test('throws once the pending limit is exceeded', () => {
  const buffer = new ReorderBuffer(2)
  buffer.push(1n, utf8('b'))
  buffer.push(2n, utf8('c'))
  assert.throws(() => buffer.push(3n, utf8('d')), ReorderOverflowError)
  // The overflow must not corrupt the buffer.
  const released = buffer.push(0n, utf8('a'))
  assert.deepEqual(released.map((chunk) => chunk[0]), [97, 98, 99])
})

test('accepts decimal strings and numbers as sequences', () => {
  const buffer = new ReorderBuffer()
  assert.equal(buffer.push('0', utf8('a')).length, 1)
  assert.equal(buffer.push(1, utf8('b')).length, 1)
  assert.equal(buffer.next, 2n)
})

test('reset clears buffered state', () => {
  const buffer = new ReorderBuffer()
  buffer.push(3n, utf8('d'))
  assert.equal(buffer.pending, 1)
  buffer.reset()
  assert.equal(buffer.pending, 0)
  assert.equal(buffer.next, 0n)
  assert.equal(buffer.dropped, 0)
})

test('createOrderedSink reports every arrival but appends only in order', () => {
  const appended = []
  const arrivals = []
  const sink = createOrderedSink({
    onAppend: (chunk) => appended.push(chunk[0]),
    onArrival: (chunk) => arrivals.push(chunk[0]),
  })

  assert.equal(sink.push(0n, utf8('a')), 1)
  assert.equal(sink.push(2n, utf8('c')), 0)
  assert.equal(sink.push(1n, utf8('b')), 2)

  assert.deepEqual(appended, [97, 98, 99])
  assert.deepEqual(arrivals, [97, 99, 98], 'wire order is preserved for accounting')
})

test('createOrderedSink rejects a missing append callback', () => {
  assert.throws(() => createOrderedSink({}), /onAppend/)
})
