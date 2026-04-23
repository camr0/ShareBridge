import { test } from 'node:test'
import assert from 'node:assert/strict'
import { createDownloadPipeline } from './downloadPipeline.js'

test('small files that fit entirely inside the tail are not written before validation', async () => {
  const events = []
  const pipeline = await createDownloadPipeline({
    header: { name: 'tiny.txt', size: 3, mimeType: 'text/plain', sha1: 'a9993e364706816aba3e25717850c26c9cd0d89d' },
    sinkFactory: async () => ({
      append(bytes) {
        events.push({ type: 'buffer', size: bytes.length })
      },
      finalize: async () => {
        events.push({ type: 'flush' })
        return { ok: true, code: 'intact' }
      },
      abort() {},
    }),
    now: () => 1000,
    scheduleTimeout: () => 1,
    clearScheduledTimeout: () => {},
  })

  await pipeline.append(new Uint8Array([97, 98, 99]))
  assert.deepEqual(events, [{ type: 'buffer', size: 3 }])
  const result = await pipeline.complete()

  assert.equal(result.code, 'intact')
  assert.deepEqual(events, [{ type: 'buffer', size: 3 }, { type: 'flush' }])
})

test('checksum mismatch fails closed', async () => {
  const pipeline = await createDownloadPipeline({
    header: { name: 'bad.bin', size: 4, mimeType: 'application/octet-stream', sha1: 'deadbeef' },
    sinkFactory: async () => ({
      append() {},
      finalize: async () => ({ ok: false, code: 'checksum-mismatch', computedSha1: 'cafebabe' }),
      abort() {},
    }),
    now: () => 1000,
    scheduleTimeout: () => 1,
    clearScheduledTimeout: () => {},
  })

  await pipeline.append(new Uint8Array([1, 2, 3, 4]))
  const result = await pipeline.complete()

  assert.equal(result.statusClass, 'corrupted')
})

test('missing checksum still resolves as done when byte count matches', async () => {
  const pipeline = await createDownloadPipeline({
    header: { name: 'plain.bin', size: 2, mimeType: 'application/octet-stream', sha1: '' },
    sinkFactory: async () => ({
      append() {},
      finalize: async () => ({ ok: true, code: 'done', computedSha1: null }),
      abort() {},
    }),
    now: () => 1000,
    scheduleTimeout: () => 1,
    clearScheduledTimeout: () => {},
  })

  await pipeline.append(new Uint8Array([9, 9]))
  const result = await pipeline.complete()

  assert.equal(result.statusText, '✓ done')
})

test('stalled transfers fail after 60 seconds without a new chunk', async () => {
  const timers = []
  let terminal = null
  const pipeline = await createDownloadPipeline({
    header: { name: 'movie.mp4', size: 8, mimeType: 'video/mp4', sha1: '' },
    sinkFactory: async () => ({ append() {}, finalize() {}, abort() {} }),
    now: () => 1000,
    scheduleTimeout: (fn, ms) => {
      const timer = {
        ms,
        active: true,
        async fire() {
          if (!this.active) {
            return
          }
          await fn()
        },
      }
      timers.push(timer)
      return timer
    },
    clearScheduledTimeout: (timer) => {
      timer.active = false
    },
    onTerminalState: (result) => {
      terminal = result
    },
  })

  assert.equal(timers[0].ms, 60_000)
  await pipeline.append(new Uint8Array([1, 2, 3, 4]))
  assert.equal(timers.length, 2)
  await timers[0].fire()
  assert.equal(terminal, null)
  await timers[1].fire()
  assert.equal(terminal.code, 'timeout')
})

test('slow append cannot re-arm the timeout or emit downloading after timeout failure', async () => {
  const timers = []
  const states = []
  let resolveAppend = null
  const appendPromiseSignal = new Promise((resolve) => {
    resolveAppend = resolve
  })

  const pipeline = await createDownloadPipeline({
    header: { name: 'movie.mp4', size: 8, mimeType: 'video/mp4', sha1: '' },
    sinkFactory: async () => ({
      append: async () => appendPromiseSignal,
      finalize() {},
      abort() {},
    }),
    now: () => 1000,
    scheduleTimeout: (fn, ms) => {
      const timer = {
        ms,
        active: true,
        async fire() {
          if (!this.active) {
            return
          }
          await fn()
        },
      }
      timers.push(timer)
      return timer
    },
    clearScheduledTimeout: (timer) => {
      timer.active = false
    },
    onStateChange: (state) => {
      states.push(state)
    },
  })

  const appendPromise = pipeline.append(new Uint8Array([1, 2, 3, 4]))
  assert.equal(timers.length, 2)
  assert.equal(timers[0].active, false)

  await timers[1].fire()
  resolveAppend()
  await appendPromise

  assert.equal(timers.length, 2)
  assert.deepEqual(states.map((state) => state.phase), ['downloading'])
})

test('append rejection becomes a terminal failure', async () => {
  let terminal = null
  let abortReason = null
  const pipeline = await createDownloadPipeline({
    header: { name: 'bad.bin', size: 4, mimeType: 'application/octet-stream', sha1: '' },
    sinkFactory: async () => ({
      async append() {
        throw new Error('writer exploded')
      },
      finalize() {},
      abort(reason) {
        abortReason = reason
      },
    }),
    now: () => 1000,
    scheduleTimeout: () => 1,
    clearScheduledTimeout: () => {},
    onTerminalState: (result) => {
      terminal = result
    },
  })

  const result = await pipeline.append(new Uint8Array([1, 2, 3, 4]))

  assert.equal(abortReason, 'append-failed')
  assert.deepEqual(result, terminal)
  assert.equal(result.code, 'append-failed')
  assert.equal(result.statusClass, 'failed')
})

test('finalize rejection becomes a terminal failure', async () => {
  let terminal = null
  let abortReason = null
  const pipeline = await createDownloadPipeline({
    header: { name: 'bad.bin', size: 4, mimeType: 'application/octet-stream', sha1: '' },
    sinkFactory: async () => ({
      append() {},
      async finalize() {
        throw new Error('finalize exploded')
      },
      abort(reason) {
        abortReason = reason
      },
    }),
    now: () => 1000,
    scheduleTimeout: () => 1,
    clearScheduledTimeout: () => {},
    onTerminalState: (result) => {
      terminal = result
    },
  })

  await pipeline.append(new Uint8Array([1, 2, 3, 4]))
  const result = await pipeline.complete()

  assert.equal(abortReason, 'finalize-failed')
  assert.deepEqual(result, terminal)
  assert.equal(result.code, 'finalize-failed')
  assert.equal(result.statusClass, 'failed')
})

test('connection closure fails the pipeline before completion', async () => {
  let abortReason = null
  let terminal = null
  const timestamps = [1000, 3000]
  const pipeline = await createDownloadPipeline({
    header: { name: 'movie.mp4', size: 8, mimeType: 'video/mp4', sha1: '' },
    sinkFactory: async () => ({
      append() {},
      finalize() {},
      abort(reason) {
        abortReason = reason
      },
    }),
    now: () => timestamps.shift() ?? 3000,
    scheduleTimeout: () => 1,
    clearScheduledTimeout: () => {},
    onTerminalState: (result) => {
      terminal = result
    },
  })

  await pipeline.append(new Uint8Array([1, 2, 3, 4]))
  const result = await pipeline.failForDisconnect()

  assert.equal(abortReason, 'disconnected')
  assert.equal(result.code, 'disconnected')
  assert.equal(result.avgBytesPerSecond, 2)
  assert.equal(terminal.code, 'disconnected')
})
