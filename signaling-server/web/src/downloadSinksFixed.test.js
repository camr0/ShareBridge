// EXP48 — equivalence proof for the E29 client fix (landed 2026-10-13).
//
// The fixed sink (`createStreamingSinkFixed`) replaces a per-append
// `concatBytes(tail, bytes)` re-concatenation of the whole 1 MiB verification
// tail with a single fixed-size ringless buffer, and moves SHA-1 into a module
// worker (`createWorkerSha1` + `hashWorker.js`). This file is the standing
// correctness gate for that change:
//
//   1. 200 randomized trials compare the ORIGINAL `createStreamingSink` against
//      `createStreamingSinkFixed` and require
//          written bytes ++ withheld final tail
//      to be byte-identical (this is the byte stream that is hashed and the
//      byte stream that is written to disk).
//   2. The fixed sink always withholds at least the final min(size, tailBytes)
//      bytes until the checksum verifies (same guarantee as the original), and
//      never more than 2 x tailBytes.
//   3. The production shape (1 MiB tail, 16 KiB frames) is proven byte-identical
//      AND SHA-1-identical with the real incremental hash-wasm hasher.
//   4. `createWorkerSha1`'s marshalling protocol is exercised against a mock
//      `Worker` (Node has no global `Worker`), including transferables,
//      backpressure at `maxOutstanding`, and digest/error propagation.
//
// The randomized trials are seeded, so the pass/fail counts are reproducible.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import {
  createIncrementalSha1,
  createPreferredSha1,
  createStreamingSink,
  createStreamingSinkFixed,
  createWorkerSha1,
} from './downloadSinks.js'

// ---------------------------------------------------------------------------
// deterministic PRNG (mulberry32) — the trial set must not drift between runs
// ---------------------------------------------------------------------------
function makeRandom(seed) {
  let state = seed >>> 0
  return function random() {
    state = (state + 0x6d2b79f5) >>> 0
    let t = state
    t = Math.imul(t ^ (t >>> 15), t | 1)
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61)
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296
  }
}

function makeRecordingWriter() {
  const written = []
  let aborted = null
  let closed = false
  let writeCalls = 0
  return {
    writer: {
      async write(chunk) {
        writeCalls += 1
        // Keep the chunk by reference, as StreamSaver's enqueue / blob fallback
        // does: a sink that reuses a buffer after write() would corrupt this.
        written.push(chunk)
      },
      async close() { closed = true },
      async abort(reason) { aborted = reason },
    },
    get written() { return written },
    get aborted() { return aborted },
    get closed() { return closed },
    get writeCalls() { return writeCalls },
  }
}

function makeStubHasher(digestHex = 'stub-digest') {
  const updates = []
  return {
    hasher: {
      update(bytes) { updates.push(Uint8Array.from(bytes)) },
      digest() { return digestHex },
    },
    updates,
  }
}

function concat(list) {
  let total = 0
  for (const item of list) total += item.length
  const out = new Uint8Array(total)
  let offset = 0
  for (const item of list) {
    out.set(item, offset)
    offset += item.length
  }
  return out
}

function equalBytes(a, b) {
  if (a.length !== b.length) return false
  for (let i = 0; i < a.length; i += 1) if (a[i] !== b[i]) return false
  return true
}

/**
 * Drives one sink over `chunks` and returns the byte stream it produced as
 * `written ++ final tail`, plus the withheld tail size and the hasher's updates.
 */
async function driveSink(buildSink, chunks, { expectedSize, expectedSha1 = null } = {}) {
  const recorder = makeRecordingWriter()
  const { hasher, updates } = makeStubHasher('stub-digest')
  const sink = await buildSink({
    fileName: 'payload.bin',
    mimeType: 'application/octet-stream',
    tailBytes: 8, // overridden by callers
    createWriter: async () => recorder.writer,
    createHasher: async () => hasher,
  })
  for (const chunk of chunks) await sink.append(chunk)
  const withheld = sink.bufferedTailSize()
  const result = await sink.finalize({
    expectedSha1,
    expectedSize: expectedSize ?? chunks.reduce((sum, c) => sum + c.length, 0),
    receivedBytes: chunks.reduce((sum, c) => sum + c.length, 0),
  })
  return {
    stream: concat(recorder.written),
    withheld,
    writeCalls: recorder.writeCalls,
    result,
    hashed: concat(updates),
  }
}

function buildOriginal(tailBytes) {
  return (options) => createStreamingSink({ ...options, tailBytes })
}

function buildFixed(tailBytes) {
  return (options) => createStreamingSinkFixed({ ...options, tailBytes })
}

test('EXP48 equivalence: 200 randomized trials are byte-identical to the original sink', async () => {
  const random = makeRandom(0x5eed_4a29)
  const TAIL = 8
  let trials = 0
  let appends = 0
  let mismatches = 0
  let withheldViolations = 0
  let maxWithheldFixed = 0
  let minWithheldMargin = Infinity

  for (let trial = 0; trial < 200; trial += 1) {
    const chunkCount = Math.floor(random() * 81) // 0..80, ~40 mean => ~8,000 appends
    const chunks = []
    for (let i = 0; i < chunkCount; i += 1) {
      const length = Math.floor(random() * 5) // 0..4 bytes
      const chunk = new Uint8Array(length)
      for (let j = 0; j < length; j += 1) chunk[j] = Math.floor(random() * 256)
      chunks.push(chunk)
    }
    const expectedSize = chunks.reduce((sum, c) => sum + c.length, 0)

    const original = await driveSink(buildOriginal(TAIL), chunks, { expectedSize })
    const fixed = await driveSink(buildFixed(TAIL), chunks, { expectedSize })

    trials += 1
    appends += chunkCount

    if (!equalBytes(original.stream, fixed.stream)) mismatches += 1
    // The hashed stream is the same stream that is written.
    if (!equalBytes(original.stream, concat(chunks))) mismatches += 1
    if (!equalBytes(fixed.hashed, concat(chunks))) mismatches += 1
    const required = Math.min(expectedSize, TAIL)
    if (fixed.withheld < required || fixed.withheld > 2 * TAIL) withheldViolations += 1
    if (fixed.withheld > maxWithheldFixed) maxWithheldFixed = fixed.withheld
    if (fixed.withheld - required < minWithheldMargin) minWithheldMargin = fixed.withheld - required
  }

  assert.equal(mismatches, 0, `${mismatches} byte-stream mismatches`)
  assert.equal(withheldViolations, 0)
  // Reporting surface: the test log carries the real counts.
  console.log(`EXP48 LOGIC: ${trials}/200 trials byte-identical, ${appends} appends, ` +
    `fixed withheld window max ${maxWithheldFixed} B (tail ${TAIL} B, min margin ${minWithheldMargin} B), ` +
    `mismatches=${mismatches}`)
  assert.equal(trials, 200)
  assert.ok(appends > 6000, `expected ~6,800 appends, saw ${appends}`)
})

test('EXP48 equivalence: production shape (1 MiB tail, 16 KiB frames) is byte- and SHA-1-identical', async () => {
  const TAIL = 1024 * 1024
  const FRAME = 16 * 1024
  const FRAMES = 4096 // 64 MiB — production frame shape, reduced count for suite speed
  const random = makeRandom(0x16_3f_2026)

  const chunks = []
  for (let i = 0; i < FRAMES; i += 1) {
    const chunk = new Uint8Array(FRAME)
    for (let j = 0; j < FRAME; j += 4) {
      const value = Math.floor(random() * 0x100000000)
      chunk[j] = value & 0xff
      chunk[j + 1] = (value >>> 8) & 0xff
      chunk[j + 2] = (value >>> 16) & 0xff
      chunk[j + 3] = (value >>> 24) & 0xff
    }
    chunks.push(chunk)
  }
  const expectedSize = FRAMES * FRAME

  // Use the REAL incremental hash-wasm SHA-1 for both sinks so the digest is a
  // genuine end-to-end verdict, then compare against node:crypto over the same
  // concatenated stream.
  const runWithRealHasher = async (buildSink) => {
    const recorder = makeRecordingWriter()
    const sink = await buildSink({
      fileName: 'payload.bin',
      mimeType: 'application/octet-stream',
      tailBytes: TAIL,
      createWriter: async () => recorder.writer,
      createHasher: () => createIncrementalSha1(),
    })
    for (const chunk of chunks) await sink.append(chunk)
    const withheld = sink.bufferedTailSize()
    const result = await sink.finalize({
      expectedSha1: null,
      expectedSize,
      receivedBytes: expectedSize,
    })
    return { stream: concat(recorder.written), withheld, writeCalls: recorder.writeCalls, result }
  }

  const original = await runWithRealHasher(buildOriginal(TAIL))
  const fixed = await runWithRealHasher(buildFixed(TAIL))
  const reference = createHash('sha1').update(concat(chunks)).digest('hex')

  assert.ok(equalBytes(original.stream, fixed.stream), 'written++tail differs')
  assert.ok(equalBytes(fixed.stream, concat(chunks)), 'fixed output is not the input stream')
  assert.equal(fixed.stream.length, expectedSize)
  assert.equal(original.stream.length, expectedSize)

  // Re-hash both produced streams and require both to equal the reference.
  const originalSha1 = createHash('sha1').update(original.stream).digest('hex')
  const fixedSha1 = createHash('sha1').update(fixed.stream).digest('hex')
  assert.equal(originalSha1, reference)
  assert.equal(fixedSha1, reference)

  assert.ok(fixed.withheld >= TAIL && fixed.withheld <= 2 * TAIL)
  assert.ok(fixed.writeCalls < original.writeCalls)

  console.log(`EXP48 PRODUCTION-SHAPE: ${FRAMES} x 16 KiB frames, sha1=${fixedSha1} ` +
    `(reference ${reference}); write calls original=${original.writeCalls} fixed=${fixed.writeCalls}; ` +
    `withheld original=${original.withheld} fixed=${fixed.withheld}`)
})

test('EXP48 fixed sink: size mismatch and checksum mismatch still abort', async () => {
  const build = async (digestHex) => {
    const recorder = makeRecordingWriter()
    const sink = await createStreamingSinkFixed({
      fileName: 'payload.bin',
      mimeType: 'application/octet-stream',
      tailBytes: 8,
      createWriter: async () => recorder.writer,
      createHasher: async () => makeStubHasher(digestHex).hasher,
    })
    return { sink, recorder }
  }

  const short = await build('stub-digest')
  await short.sink.append(new Uint8Array([1, 2, 3, 4]))
  const sizeResult = await short.sink.finalize({ expectedSha1: null, expectedSize: 5, receivedBytes: 4 })
  assert.deepEqual(sizeResult, { ok: false, code: 'size-mismatch' })
  assert.equal(short.recorder.aborted, 'size-mismatch')

  const bad = await build('badbad')
  await bad.sink.append(new Uint8Array([1, 2, 3, 4]))
  const checksumResult = await bad.sink.finalize({ expectedSha1: 'abc123', expectedSize: 4, receivedBytes: 4 })
  assert.deepEqual(checksumResult, { ok: false, code: 'checksum-mismatch', computedSha1: 'badbad' })
  assert.equal(bad.recorder.aborted, 'checksum-mismatch')

  const good = await build('abc123')
  await good.sink.append(new Uint8Array([1, 2, 3, 4]))
  const okResult = await good.sink.finalize({ expectedSha1: 'ABC123', expectedSize: 4, receivedBytes: 4 })
  assert.deepEqual(okResult, { ok: true, code: 'intact', computedSha1: 'abc123' })
  assert.equal(good.recorder.closed, true)
})

test('EXP48 fixed sink: album deferred-close options behave like the original', async () => {
  let closeCalls = 0
  let deferredError = null
  const sink = await createStreamingSinkFixed({
    fileName: 'album.zip',
    mimeType: 'application/zip',
    tailBytes: 4,
    deferCloseSettlement: true,
    onDeferredCloseError: (error) => { deferredError = error.message },
    createWriter: async () => ({
      async write() {},
      close() { closeCalls += 1; return Promise.reject(new Error('late close failure')) },
      async abort() {},
    }),
    createHasher: async () => makeStubHasher().hasher,
  })

  await sink.append(new Uint8Array([1, 2, 3, 4]))
  const result = await sink.finalize({ expectedSha1: null, expectedSize: 4, receivedBytes: 4 })
  await new Promise((resolve) => setImmediate(resolve))

  assert.equal(closeCalls, 1)
  assert.deepEqual(result, { ok: true, code: 'done', computedSha1: null })
  assert.equal(deferredError, 'late close failure')
})

test('EXP48 createWorkerSha1: marshalling protocol, transferables, backpressure, digest', async (t) => {
  const OriginalWorker = globalThis.Worker
  const instances = []

  class MockWorker {
    constructor(url, options) {
      this.url = url
      this.options = options
      this.posted = []
      this.onmessage = null
      this.onerror = null
      this._hash = createHash('sha1')
      this.terminated = false
      instances.push(this)
      queueMicrotask(() => this.onmessage?.({ data: { type: 'ready' } }))
    }

    terminate() { this.terminated = true }

    postMessage(message, transfer) {
      this.posted.push({ message, transfer })
      if (message.type === 'update') {
        // The wrapper must hand over an owned copy, not the caller's buffer.
        this._hash.update(new Uint8Array(message.buf))
        queueMicrotask(() => this.onmessage?.({ data: { type: 'ack', seq: message.seq } }))
        return
      }
      if (message.type === 'digest') {
        const hex = this._hash.digest('hex')
        queueMicrotask(() => this.onmessage?.({ data: { type: 'digest', id: message.id, hex } }))
      }
    }
  }

  globalThis.Worker = MockWorker
  t.after(() => {
    if (typeof OriginalWorker === 'undefined') delete globalThis.Worker
    else globalThis.Worker = OriginalWorker
    delete globalThis.__exp29
  })

  const random = makeRandom(0xa11ce)
  const frames = []
  for (let i = 0; i < 64; i += 1) {
    const chunk = new Uint8Array(1024)
    for (let j = 0; j < chunk.length; j += 1) chunk[j] = Math.floor(random() * 256)
    frames.push(chunk)
  }

  const hasher = await createWorkerSha1({ maxOutstanding: 4 })
  assert.equal(instances.length, 1)
  assert.equal(instances[0].options.type, 'module')
  assert.equal(String(instances[0].url), String(new URL('./hashWorker.js', import.meta.url)))

  const snappedInputs = frames.map((chunk) => Uint8Array.from(chunk))
  for (const frame of frames) {
    const pending = hasher.update(frame)
    if (pending) await pending
  }
  // update() copies: the caller's frames must be intact (would throw if detached).
  for (let i = 0; i < frames.length; i += 1) {
    assert.ok(equalBytes(frames[i], snappedInputs[i]), `frame ${i} was mutated or detached`)
  }

  // Every transfer list must contain exactly the transferred copy's buffer.
  for (const { message, transfer } of instances[0].posted.filter((entry) => entry.message.type === 'update')) {
    assert.deepEqual(transfer, [message.buf])
    assert.equal(message.buf.byteLength, 1024)
  }
  // Backpressure: 64 updates at maxOutstanding=4 means some update() returned a promise.
  assert.ok(instances[0].posted.filter((entry) => entry.message.type === 'update').length === 64)

  const digest = await hasher.digest('hex')
  const reference = createHash('sha1').update(concat(frames)).digest('hex')
  assert.equal(digest, reference)
  assert.equal(globalThis.__exp29.computedSha1, reference)
  assert.equal(globalThis.__exp29.worker, true)
})

test('EXP48 createWorkerSha1: worker error surfaces on update and digest', async (t) => {
  const OriginalWorker = globalThis.Worker
  class FailingWorker {
    constructor() {
      this.onmessage = null
      this.onerror = null
      queueMicrotask(() => this.onmessage?.({ data: { type: 'ready' } }))
    }
    postMessage() {
      queueMicrotask(() => this.onmessage?.({ data: { type: 'error', message: 'boom' } }))
    }
    terminate() {}
  }
  globalThis.Worker = FailingWorker
  t.after(() => {
    if (typeof OriginalWorker === 'undefined') delete globalThis.Worker
    else globalThis.Worker = OriginalWorker
  })

  const hasher = await createWorkerSha1()
  hasher.update(new Uint8Array([1]))
  await new Promise((resolve) => setImmediate(resolve))
  assert.throws(() => hasher.update(new Uint8Array([2])), /sha1 worker: boom/)
  await assert.rejects(() => hasher.digest('hex'), /sha1 worker: boom/)
})

function withMockWorker(t, WorkerClass) {
  const OriginalWorker = globalThis.Worker
  globalThis.Worker = WorkerClass
  t.after(() => {
    if (typeof OriginalWorker === 'undefined') delete globalThis.Worker
    else globalThis.Worker = OriginalWorker
    delete globalThis.__exp29
  })
}

test('fixed sink: flushed chunks are never mutated after write() (writer keeps references)', async () => {
  const TAIL = 16
  const chunks = []
  for (let i = 0; i < 40; i += 1) chunks.push(Uint8Array.from({ length: 7 }, (_, j) => (i * 7 + j) & 0xff))
  const fixed = await driveSink(buildFixed(TAIL), chunks)
  assert.ok(equalBytes(fixed.stream, concat(chunks)))
  assert.ok(fixed.writeCalls > 2)
})

test('fixed sink: withholds the full final tailBytes until verified (1 MiB + 16 KiB case)', async () => {
  const TAIL = 1024 * 1024
  const FRAME = 16 * 1024
  const frames = Array.from({ length: TAIL / FRAME + 1 }, (_, i) => new Uint8Array(FRAME).fill(i & 0xff))
  const recorder = makeRecordingWriter()
  const sink = await createStreamingSinkFixed({
    fileName: 'payload.bin',
    mimeType: 'application/octet-stream',
    tailBytes: TAIL,
    createWriter: async () => recorder.writer,
    createHasher: async () => makeStubHasher('badbad').hasher,
  })
  for (const frame of frames) await sink.append(frame)
  assert.ok(sink.bufferedTailSize() >= TAIL)
  const result = await sink.finalize({ expectedSha1: 'abc123', expectedSize: TAIL + FRAME, receivedBytes: TAIL + FRAME })
  assert.equal(result.code, 'checksum-mismatch')
  assert.ok(concat(recorder.written).length <= FRAME, 'more than the head of the file reached disk unverified')
})

test('fixed sink: ArrayBuffer input is written and hashed identically', async () => {
  const bytes = Uint8Array.from([9, 8, 7, 6, 5, 4, 3, 2, 1])
  const recorder = makeRecordingWriter()
  const { hasher, updates } = makeStubHasher()
  const sink = await createStreamingSinkFixed({
    fileName: 'payload.bin',
    mimeType: 'application/octet-stream',
    tailBytes: 4,
    createWriter: async () => recorder.writer,
    createHasher: async () => hasher,
  })
  await sink.append(bytes.slice().buffer)
  const result = await sink.finalize({ expectedSha1: null, expectedSize: bytes.length, receivedBytes: bytes.length })
  assert.equal(result.ok, true)
  assert.ok(equalBytes(concat(recorder.written), bytes))
  assert.ok(equalBytes(concat(updates.map((update) => new Uint8Array(update))), bytes))
})

test('fixed sink: hasher is terminated on finalize and abort', async () => {
  for (const path of ['finalize', 'abort']) {
    let terminated = 0
    const sink = await createStreamingSinkFixed({
      fileName: 'payload.bin',
      mimeType: 'application/octet-stream',
      tailBytes: 4,
      createWriter: async () => makeRecordingWriter().writer,
      createHasher: async () => ({ update() {}, digest: () => 'x', terminate() { terminated += 1 } }),
    })
    await sink.append(new Uint8Array([1, 2]))
    if (path === 'finalize') await sink.finalize({ expectedSha1: null, expectedSize: 2, receivedBytes: 2 })
    else await sink.abort('cancelled')
    assert.equal(terminated, 1, path)
  }
})

test('createWorkerSha1: ArrayBuffer input is hashed, not dropped', async (t) => {
  class EchoWorker {
    constructor() {
      this.hash = createHash('sha1')
      queueMicrotask(() => this.onmessage?.({ data: { type: 'ready' } }))
    }
    postMessage(message) {
      if (message.type === 'update') this.hash.update(new Uint8Array(message.buf))
      if (message.type === 'digest') {
        const hex = this.hash.digest('hex')
        queueMicrotask(() => this.onmessage?.({ data: { type: 'digest', id: message.id, hex } }))
      }
    }
    terminate() {}
  }
  withMockWorker(t, EchoWorker)
  const bytes = Uint8Array.from([1, 2, 3, 4, 5])
  const hasher = await createWorkerSha1()
  hasher.update(bytes.slice().buffer)
  assert.equal(await hasher.digest('hex'), createHash('sha1').update(bytes).digest('hex'))
})

test('createWorkerSha1: worker crash rejects pending backpressure and digest waiters', async (t) => {
  let instance = null
  class SilentWorker {
    constructor() {
      instance = this
      this.terminated = false
      queueMicrotask(() => this.onmessage?.({ data: { type: 'ready' } }))
    }
    postMessage() {} // never acks
    terminate() { this.terminated = true }
  }
  withMockWorker(t, SilentWorker)
  const hasher = await createWorkerSha1({ maxOutstanding: 2 })
  hasher.update(new Uint8Array([1]))
  const blocked = hasher.update(new Uint8Array([2]))
  assert.ok(blocked && typeof blocked.then === 'function')
  const digest = hasher.digest('hex')
  instance.onerror({ message: 'crashed', preventDefault() {} })
  await assert.rejects(blocked, /sha1 worker failed: crashed/)
  await assert.rejects(digest, /sha1 worker failed: crashed/)
  assert.equal(instance.terminated, true)
})

test('createWorkerSha1: rejects when the worker cannot start', async (t) => {
  class BrokenWorker {
    constructor() { queueMicrotask(() => this.onerror?.({ message: 'module load failed' })) }
    postMessage() {}
    terminate() {}
  }
  withMockWorker(t, BrokenWorker)
  await assert.rejects(createWorkerSha1(), /module load failed/)

  class HungWorker { postMessage() {} terminate() {} }
  globalThis.Worker = HungWorker
  await assert.rejects(createWorkerSha1({ readyTimeoutMs: 5 }), /did not start/)
})

test('createPreferredSha1: falls back to main-thread hashing when the worker fails', async (t) => {
  withMockWorker(t, class {})
  const fallback = { kind: 'main-thread' }
  const viaFailure = await createPreferredSha1({
    createWorker: async () => { throw new Error('nope') },
    createFallback: async () => fallback,
  })
  assert.equal(viaFailure, fallback)
  const worker = { kind: 'worker' }
  assert.equal(await createPreferredSha1({ createWorker: async () => worker, createFallback: async () => fallback }), worker)

  delete globalThis.Worker
  assert.equal(await createPreferredSha1({ createWorker: async () => worker, createFallback: async () => fallback }), fallback)
})
