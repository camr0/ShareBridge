import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  createBlobSink,
  createBrowserStreamWriter,
  createSafariBrowserStreamWriter,
  createStreamingSink,
} from './downloadSinks.js'

test('createStreamingSink writes all but the held-back tail before finalize', async () => {
  const writes = []
  const sink = await createStreamingSink({
    fileName: 'video.mov',
    mimeType: 'video/quicktime',
    tailBytes: 4,
    createWriter: async () => ({
      async write(chunk) { writes.push([...chunk]) },
      async close() {},
      async abort() {},
    }),
    createHasher: async () => ({
      update() {},
      digest() { return 'abc123' },
    }),
  })

  await sink.append(new Uint8Array([1, 2, 3]))
  await sink.append(new Uint8Array([4, 5, 6, 7]))

  assert.deepEqual(writes, [[1, 2, 3]])
  assert.equal(sink.bufferedTailSize(), 4)
})

test('createStreamingSink retains the last tailBytes when a single append exceeds the tail window', async () => {
  const writes = []
  const sink = await createStreamingSink({
    fileName: 'video.mov',
    mimeType: 'video/quicktime',
    tailBytes: 4,
    createWriter: async () => ({
      async write(chunk) { writes.push([...chunk]) },
      async close() {},
      async abort() {},
    }),
    createHasher: async () => ({
      update() {},
      digest() { return 'abc123' },
    }),
  })

  await sink.append(new Uint8Array([1, 2, 3, 4, 5, 6]))

  assert.deepEqual(writes, [[1, 2]])
  assert.equal(sink.bufferedTailSize(), 4)
})

test('createStreamingSink finalizes as intact when SHA-1 matches', async () => {
  const writes = []
  let closed = false
  const sink = await createStreamingSink({
    fileName: 'video.mov',
    mimeType: 'video/quicktime',
    tailBytes: 4,
    createWriter: async () => ({
      async write(chunk) { writes.push([...chunk]) },
      async close() { closed = true },
      async abort() {},
    }),
    createHasher: async () => ({
      update() {},
      digest() { return 'abc123' },
    }),
  })

  await sink.append(new Uint8Array([1, 2, 3]))
  await sink.append(new Uint8Array([4, 5, 6, 7]))
  const result = await sink.finalize({ expectedSha1: 'abc123', expectedSize: 7, receivedBytes: 7 })

  assert.deepEqual(writes, [[1, 2, 3], [4, 5, 6, 7]])
  assert.equal(closed, true)
  assert.deepEqual(result, { ok: true, code: 'intact', computedSha1: 'abc123' })
})

test('createStreamingSink aborts when SHA-1 validation fails', async () => {
  let abortReason = null
  const sink = await createStreamingSink({
    fileName: 'video.mov',
    mimeType: 'video/quicktime',
    tailBytes: 4,
    createWriter: async () => ({
      async write() {},
      async close() {},
      async abort(reason) { abortReason = reason },
    }),
    createHasher: async () => ({
      update() {},
      digest() { return 'badbad' },
    }),
  })

  await sink.append(new Uint8Array([1, 2, 3, 4]))
  const result = await sink.finalize({ expectedSha1: 'abc123', expectedSize: 4, receivedBytes: 4 })

  assert.equal(abortReason, 'checksum-mismatch')
  assert.deepEqual(result, { ok: false, code: 'checksum-mismatch', computedSha1: 'badbad' })
})

test('createBlobSink does not call subtle.digest when no sha1 is expected', async () => {
  let digestCalls = 0
  const sink = await createBlobSink({
    fileName: 'archive.bin',
    mimeType: 'application/octet-stream',
    subtleDigest: async () => {
      digestCalls += 1
      return new Uint8Array([0xaa]).buffer
    },
    triggerBrowserSave: () => {},
  })

  await sink.append(new Uint8Array([1, 2, 3]))
  await sink.finalize({ expectedSha1: null, expectedSize: 3, receivedBytes: 3 })

  assert.equal(digestCalls, 0)
})

test('createBlobSink fails when SHA-1 validation does not match', async () => {
  let saveCalls = 0
  const sink = await createBlobSink({
    fileName: 'archive.bin',
    mimeType: 'application/octet-stream',
    subtleDigest: async () => new Uint8Array([0xaa]).buffer,
    triggerBrowserSave: () => {
      saveCalls += 1
    },
  })

  await sink.append(new Uint8Array([1, 2, 3]))
  const result = await sink.finalize({ expectedSha1: 'ffffffff', expectedSize: 3, receivedBytes: 3 })

  assert.equal(saveCalls, 0)
  assert.equal(result.ok, false)
  assert.equal(result.code, 'checksum-mismatch')
})

test('createBlobSink fails on size mismatch before saving', async () => {
  let saveCalls = 0
  const sink = await createBlobSink({
    fileName: 'archive.bin',
    mimeType: 'application/octet-stream',
    subtleDigest: async () => new Uint8Array([0xaa]).buffer,
    triggerBrowserSave: () => {
      saveCalls += 1
    },
  })

  await sink.append(new Uint8Array([1, 2, 3]))
  const result = await sink.finalize({ expectedSha1: null, expectedSize: 4, receivedBytes: 3 })

  assert.equal(saveCalls, 0)
  assert.deepEqual(result, { ok: false, code: 'size-mismatch' })
})

test('createBrowserStreamWriter validates the imported StreamSaver global and forwards mimeType', async (t) => {
  const originalStreamSaver = globalThis.streamSaver
  const writer = { label: 'writer' }
  let captured = null

  t.after(() => {
    if (typeof originalStreamSaver === 'undefined') {
      delete globalThis.streamSaver
      return
    }

    globalThis.streamSaver = originalStreamSaver
  })

  const result = await createBrowserStreamWriter('archive.bin', 'application/octet-stream', {
    importModule: async () => {
      globalThis.streamSaver = {
        mitm: null,
        createWriteStream(fileName, options) {
          captured = { fileName, options, mitm: this.mitm }
          return {
            getWriter() {
              return writer
            },
          }
        },
      }
    },
  })

  assert.equal(result, writer)
  assert.deepEqual(captured, {
    fileName: 'archive.bin',
    options: {
      size: undefined,
      mimeType: 'application/octet-stream',
      writableStrategy: undefined,
    },
    mitm: '/src/vendor/streamsaver-mitm.html',
  })
})

test('createSafariBrowserStreamWriter uses the Safari MITM path', async (t) => {
  const originalStreamSaver = globalThis.streamSaver
  const writer = { label: 'safari-writer' }
  let capturedMitm = null

  t.after(() => {
    if (typeof originalStreamSaver === 'undefined') {
      delete globalThis.streamSaver
      return
    }

    globalThis.streamSaver = originalStreamSaver
  })

  const result = await createSafariBrowserStreamWriter('archive.bin', 'application/octet-stream', {
    importModule: async () => {
      globalThis.streamSaver = {
        mitm: null,
        createWriteStream() {
          capturedMitm = this.mitm
          return {
            getWriter() {
              return writer
            },
          }
        },
      }
    },
  })

  assert.equal(result, writer)
  assert.equal(capturedMitm, '/src/vendor/streamsaver-safari-mitm.html')
})

test('createBrowserStreamWriter fails when the vendor module does not expose streamSaver', async (t) => {
  const originalStreamSaver = globalThis.streamSaver
  delete globalThis.streamSaver

  t.after(() => {
    if (typeof originalStreamSaver === 'undefined') {
      delete globalThis.streamSaver
      return
    }

    globalThis.streamSaver = originalStreamSaver
  })

  await assert.rejects(
    () => createBrowserStreamWriter('archive.bin', 'application/octet-stream', {
      importModule: async () => ({}),
    }),
    /StreamSaver failed to load/,
  )
})
