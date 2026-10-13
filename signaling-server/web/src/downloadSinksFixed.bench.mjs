// EXP48 micro-benchmark — JS-side cost of the OLD append/verify path vs the E29
// fixed path, over the real production shape.
//
//   node src/downloadSinksFixed.bench.mjs
//
// Shape used by every October field cell: one 754 MiB file = 790,626,304 B,
// delivered as ~48,258 DataChannel frames of 16 KiB (790,626,304 / 16,384 =
// 48,256 exactly; the brief's "~48,258" is the same 16 KiB frame size rounded
// from a slightly larger file). This script uses 48,258 x 16,384 B =
// 790,659,072 B to match the brief, and reports the frame math it used.
//
// Measured, all in Node on this Mac (NOT the browser, NOT the field rig):
//   1. append-only, counting writer  — isolates the tail-buffer logic
//   2. append-only, copying writer   — adds a realistic 754 MiB writer memcpy
//   3. append + real incremental hash-wasm SHA-1 on the calling thread
//   4. SHA-1 alone                   — the cost the E29 fix moves to a worker
//
// Node is a single-threaded JS runtime: (3) shows the main-thread serialisation
// that the browser loses by moving SHA-1 to a module worker, but Node cannot
// reproduce the browser's worker-thread hop, the renderer process split, or the
// StreamSaver/service-worker write path. Those parts of E29's -48 % renderer
// CPU / 3.23x user-visible numbers are INHERITED from E29, not re-measured here.

import { createIncrementalSha1, createStreamingSink, createStreamingSinkFixed } from './downloadSinks.js'

const FRAME = 16 * 1024
const FRAMES = 48_258
const TOTAL = FRAMES * FRAME
const TAIL = 1024 * 1024

const gc = typeof globalThis.gc === 'function' ? globalThis.gc : () => {}

/** Deterministic pool of distinct 16 KiB frames, cycled to avoid holding 754 MiB. */
function makeFramePool(count = 64) {
  const pool = []
  let state = 0x12345678
  for (let f = 0; f < count; f += 1) {
    const frame = new Uint8Array(FRAME)
    for (let i = 0; i < FRAME; i += 4) {
      state = (state * 1664525 + 1013904223) >>> 0
      frame[i] = state & 0xff
      frame[i + 1] = (state >>> 8) & 0xff
      frame[i + 2] = (state >>> 16) & 0xff
      frame[i + 3] = (state >>> 24) & 0xff
    }
    pool.push(frame)
  }
  return pool
}

function countingWriter() {
  let calls = 0
  let bytes = 0
  return {
    writer: {
      async write(chunk) { calls += 1; bytes += chunk.length },
      async close() {},
      async abort() {},
    },
    stats: () => ({ calls, bytes }),
  }
}

function copyingWriter() {
  const scratch = new Uint8Array(TAIL)
  let calls = 0
  let bytes = 0
  return {
    writer: {
      async write(chunk) { calls += 1; bytes += chunk.length; scratch.set(chunk.subarray(0, chunk.length)) },
      async close() {},
      async abort() {},
    },
    stats: () => ({ calls, bytes }),
  }
}

const noopHasher = {
  update() {},
  digest() { return 'noop' },
}

// What the LANDED fix actually asks of the main thread: createWorkerSha1.update
// copies each 16 KiB frame once so it can be transferred to the worker. This arm
// includes that copy and nothing else (no hashing on the main thread).
const workerCopyHasher = {
  update(bytes) {
    const copy = new Uint8Array(bytes.length)
    copy.set(bytes)
    return undefined
  },
  digest() { return 'worker' },
}

async function bench(name, run) {
  gc()
  const rssBefore = process.memoryUsage().rss
  const start = process.hrtime.bigint()
  const out = await run()
  const ms = Number(process.hrtime.bigint() - start) / 1e6
  gc()
  const rssPeak = Math.max(rssBefore, process.memoryUsage().rss) / (1024 * 1024)
  console.log(
    `${name.padEnd(34)} ${ms.toFixed(1).padStart(9)} ms   ${(TOTAL / 1048576 / (ms / 1000)).toFixed(0).padStart(6)} MiB/s   ` +
    `writes=${String(out.calls).padStart(6)}   rss~${rssPeak.toFixed(0)} MiB`,
  )
  return { ms, ...out }
}

async function driveSink(createSink, makeWriter, makeHasher) {
  const { writer, stats } = makeWriter()
  const sink = await createSink({
    fileName: 'payload.bin',
    mimeType: 'application/octet-stream',
    tailBytes: TAIL,
    createWriter: async () => writer,
    createHasher: async () => makeHasher(),
  })
  const pool = makeFramePool()
  for (let i = 0; i < FRAMES; i += 1) {
    await sink.append(pool[i % pool.length])
  }
  const withheld = sink.bufferedTailSize()
  await sink.finalize({ expectedSha1: null, expectedSize: TOTAL, receivedBytes: TOTAL })
  return { ...stats(), withheld }
}

const createOld = (options) => createStreamingSink(options)
const createFixed = (options) => createStreamingSinkFixed(options)

console.log(`EXP48 bench — node ${process.version} on ${process.platform}/${process.arch}`)
console.log(`shape: ${FRAMES} frames x ${FRAME} B = ${TOTAL} B = ${(TOTAL / 1048576).toFixed(1)} MiB, tail=${TAIL} B\n`)

const results = {}
results.oldCounting = await bench('old  (concatBytes) counting writer', () => driveSink(createOld, countingWriter, () => noopHasher))
results.fixedCounting = await bench('fixed (1 MiB buffer) counting writer', () => driveSink(createFixed, countingWriter, () => noopHasher))
results.oldCopying = await bench('old  (concatBytes) copying writer', () => driveSink(createOld, copyingWriter, () => noopHasher))
results.fixedCopying = await bench('fixed (1 MiB buffer) copying writer', () => driveSink(createFixed, copyingWriter, () => noopHasher))
results.oldSha1 = await bench('old  + hash-wasm SHA-1 (main thr)', () => driveSink(createOld, countingWriter, () => createIncrementalSha1()))
results.fixedSha1 = await bench('fixed + hash-wasm SHA-1 (main thr)', () => driveSink(createFixed, countingWriter, () => createIncrementalSha1()))
// The landed configuration: fixed sink + worker (main thread only pays the
// per-frame copy made by createWorkerSha1.update).
results.fixedWorker = await bench('fixed + worker copy (LANDED)     ', () => driveSink(createFixed, countingWriter, () => workerCopyHasher))

// SHA-1 alone: the piece the worker removes from the main thread.
results.sha1Only = await bench('hash-wasm SHA-1 alone', async () => {
  const hasher = await createIncrementalSha1()
  const pool = makeFramePool()
  for (let i = 0; i < FRAMES; i += 1) hasher.update(pool[i % pool.length])
  hasher.digest('hex')
  return { calls: FRAMES }
})

console.log('\n--- derived ---')
const amp = (TAIL + FRAME) / FRAME
console.log(`old per-append amplification: (${TAIL} + ${FRAME}) / ${FRAME} = ${amp.toFixed(2)}x  ` +
  `=> ${(TOTAL * amp / 1024 ** 3).toFixed(1)} GiB allocated+copied for ${(TOTAL / 1024 ** 3).toFixed(2)} GiB payload`)
console.log(`fixed bytes copied into the tail buffer: ${(TOTAL / 1024 ** 3).toFixed(2)} GiB (1x)`)
console.log(`append-only cost removed: ${(results.oldCounting.ms - results.fixedCounting.ms).toFixed(1)} ms ` +
  `(counting writer), ${(results.oldCopying.ms - results.fixedCopying.ms).toFixed(1)} ms (copying writer)`)
console.log(`LANDED main-thread cost (fixed sink + worker copy, no main-thread SHA-1): ` +
  `${results.fixedWorker.ms.toFixed(1)} ms vs old main-thread path ` +
  `${results.oldSha1.ms.toFixed(1)} ms = ${(results.oldSha1.ms / results.fixedWorker.ms).toFixed(1)}x`)
console.log(`SHA-1 moved off the main thread: ${results.sha1Only.ms.toFixed(1)} ms ` +
  `(${(TOTAL / 1048576 / (results.sha1Only.ms / 1000)).toFixed(0)} MiB/s) of serial-ish main-thread work`)
console.log(`writer calls: old=${results.oldCounting.calls} fixed=${results.fixedCounting.calls} ` +
  `(${(results.oldCounting.calls / results.fixedCounting.calls).toFixed(1)}x fewer)`)

// Machine-readable tail of the output for the results file.
console.log('\nEXP48-BENCH-JSON ' + JSON.stringify({
  frames: FRAMES,
  frameBytes: FRAME,
  totalBytes: TOTAL,
  tailBytes: TAIL,
  ms: {
    oldCounting: results.oldCounting.ms,
    fixedCounting: results.fixedCounting.ms,
    oldCopying: results.oldCopying.ms,
    fixedCopying: results.fixedCopying.ms,
    oldSha1: results.oldSha1.ms,
    fixedSha1: results.fixedSha1.ms,
    fixedWorker: results.fixedWorker.ms,
    sha1Only: results.sha1Only.ms,
  },
  writes: { old: results.oldCounting.calls, fixed: results.fixedCounting.calls },
  withheld: { old: results.oldCounting.withheld, fixed: results.fixedCounting.withheld },
}))
