import { test } from 'node:test'
import assert from 'node:assert/strict'
import { createDownloadPipeline } from './downloadPipeline.js'

test('small files that fit entirely inside the tail are not written before validation', async () => {
  let flushed = false
  const pipeline = await createDownloadPipeline({
    header: { name: 'tiny.txt', size: 3, mimeType: 'text/plain', sha1: 'a9993e364706816aba3e25717850c26c9cd0d89d' },
    sinkFactory: async () => ({
      append() {},
      finalize: async () => {
        flushed = true
        return { ok: true, code: 'intact' }
      },
      abort() {},
    }),
    now: () => 1000,
    scheduleTimeout: () => 1,
    clearScheduledTimeout: () => {},
  })

  await pipeline.append(new Uint8Array([97, 98, 99]))
  assert.equal(flushed, false)
  const result = await pipeline.complete()

  assert.equal(result.code, 'intact')
  assert.equal(flushed, true)
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
  let timeoutMs = 0
  let timeoutFn = null
  let terminal = null
  const pipeline = await createDownloadPipeline({
    header: { name: 'movie.mp4', size: 8, mimeType: 'video/mp4', sha1: '' },
    sinkFactory: async () => ({ append() {}, finalize() {}, abort() {} }),
    now: () => 1000,
    scheduleTimeout: (fn, ms) => {
      timeoutMs = ms
      timeoutFn = fn
      return fn
    },
    clearScheduledTimeout: () => {},
    onTerminalState: (result) => {
      terminal = result
    },
  })

  assert.equal(timeoutMs, 60_000)
  await timeoutFn()
  assert.equal(terminal.code, 'timeout')
})

test('connection closure fails the pipeline before completion', async () => {
  let abortReason = null
  let terminal = null
  const pipeline = await createDownloadPipeline({
    header: { name: 'movie.mp4', size: 8, mimeType: 'video/mp4', sha1: '' },
    sinkFactory: async () => ({
      append() {},
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

  const result = await pipeline.failForDisconnect()

  assert.equal(abortReason, 'disconnected')
  assert.equal(result.code, 'disconnected')
  assert.equal(terminal.code, 'disconnected')
})
