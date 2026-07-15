import { test } from 'node:test'
import assert from 'node:assert/strict'
import vm from 'node:vm'
import { readFile } from 'node:fs/promises'

async function loadServiceWorker() {
  const listeners = new Map()
  const self = {
    skipWaiting() {},
    clients: { claim: async () => {} },
    addEventListener(type, handler) {
      listeners.set(type, handler)
    },
  }

  const source = await readFile(new URL('./sw.js', import.meta.url), 'utf8')
  vm.runInNewContext(source, {
    self,
    URL,
    Request,
    Response,
    ReadableStream,
    Map,
    Set,
    Promise,
    console,
  })

  return {
    async dispatchFetch(path, headers = {}) {
      const request = new Request(`https://example.test${path}`, { headers })
      let responsePromise
      listeners.get('fetch')({
        request,
        respondWith(response) {
          responsePromise = Promise.resolve(response)
        },
      })
      return responsePromise
    },
    dispatchMessage(data) {
      listeners.get('message')({ data })
    },
  }
}

async function readChunk(reader) {
  const { value, done } = await reader.read()
  return {
    done,
    value: value ? [...value] : value,
  }
}

test('served root Service Worker stays in sync with the tested source worker', async () => {
  const servedWorker = await readFile(new URL('../sw.js', import.meta.url), 'utf8')
  const sourceWorker = await readFile(new URL('./sw.js', import.meta.url), 'utf8')
  assert.equal(servedWorker, sourceWorker)
})

test('forward seek fetch created before reset survives and rebases to the new generation', async () => {
  const sw = await loadServiceWorker()
  sw.dispatchMessage({ mediaId: 'video', size: 1000 })

  const response = await sw.dispatchFetch('/media/video', { range: 'bytes=100-' })
  const reader = response.body.getReader()

  sw.dispatchMessage({ mediaId: 'video', reset: true, generation: 1 })
  sw.dispatchMessage({ mediaId: 'video', startOffset: 100, generation: 1 })
  sw.dispatchMessage({ mediaId: 'video', chunk: new Uint8Array([7, 8, 9]), generation: 1 })

  assert.deepEqual(await readChunk(reader), { done: false, value: [7, 8, 9] })
})

test('one parked playback stream survives repeated seek generation rebases', async () => {
  const sw = await loadServiceWorker()
  sw.dispatchMessage({ mediaId: 'video', size: 1000 })

  const response = await sw.dispatchFetch('/media/video', { range: 'bytes=100-' })
  const reader = response.body.getReader()

  sw.dispatchMessage({ mediaId: 'video', reset: true, generation: 1 })
  sw.dispatchMessage({ mediaId: 'video', startOffset: 100, generation: 1 })
  sw.dispatchMessage({ mediaId: 'video', chunk: new Uint8Array([1]), generation: 1 })
  assert.deepEqual(await readChunk(reader), { done: false, value: [1] })

  sw.dispatchMessage({ mediaId: 'video', reset: true, generation: 2 })
  sw.dispatchMessage({ mediaId: 'video', startOffset: 200, generation: 2 })
  sw.dispatchMessage({ mediaId: 'video', chunk: new Uint8Array([2]), generation: 2 })
  assert.deepEqual(await readChunk(reader), { done: false, value: [2] })
})

test('later seek generation wins over an earlier parked seek stream', async () => {
  const sw = await loadServiceWorker()
  sw.dispatchMessage({ mediaId: 'video', size: 2000 })

  const firstResponse = await sw.dispatchFetch('/media/video', { range: 'bytes=100-' })
  const firstReader = firstResponse.body.getReader()
  sw.dispatchMessage({ mediaId: 'video', reset: true, generation: 1 })
  sw.dispatchMessage({ mediaId: 'video', startOffset: 100, generation: 1 })

  const secondResponse = await sw.dispatchFetch('/media/video', { range: 'bytes=120-' })
  const secondReader = secondResponse.body.getReader()
  sw.dispatchMessage({ mediaId: 'video', reset: true, generation: 2 })
  sw.dispatchMessage({ mediaId: 'video', startOffset: 120, generation: 2 })

  sw.dispatchMessage({ mediaId: 'video', chunk: new Uint8Array([1, 2, 3]), generation: 1 })
  sw.dispatchMessage({ mediaId: 'video', chunk: new Uint8Array([4, 5, 6]), generation: 2 })

  assert.deepEqual(await readChunk(firstReader), { done: true, value: undefined })
  assert.deepEqual(await readChunk(secondReader), { done: false, value: [4, 5, 6] })
})

test('backward seek within buffered range is served from buffered chunks without a reset', async () => {
  const sw = await loadServiceWorker()
  sw.dispatchMessage({ mediaId: 'video', size: 1000 })

  const response = await sw.dispatchFetch('/media/video')
  const reader = response.body.getReader()
  sw.dispatchMessage({ mediaId: 'video', chunk: new Uint8Array([10, 11, 12, 13, 14]) })

  assert.deepEqual(await readChunk(reader), { done: false, value: [10, 11, 12, 13, 14] })

  const seekResponse = await sw.dispatchFetch('/media/video', { range: 'bytes=2-' })
  const seekReader = seekResponse.body.getReader()

  assert.deepEqual(await readChunk(seekReader), { done: false, value: [12, 13, 14] })
})

test('seek init probe returns the cached init segment for the new generation', async () => {
  const sw = await loadServiceWorker()
  sw.dispatchMessage({ mediaId: 'video', size: 1000 })

  const initSegment = new Uint8Array([
    0x00, 0x00, 0x00, 0x10, 0x66, 0x74, 0x79, 0x70, 1, 2, 3, 4, 5, 6, 7, 8,
    0x00, 0x00, 0x00, 0x0c, 0x6d, 0x6f, 0x6f, 0x76, 9, 10, 11, 12,
  ])
  sw.dispatchMessage({ mediaId: 'video', chunk: initSegment })

  sw.dispatchMessage({ mediaId: 'video', reset: true, generation: 1 })

  const response = await sw.dispatchFetch('/media/video', { range: 'bytes=0-' })
  assert.equal(response.status, 206)

  const body = new Uint8Array(await response.arrayBuffer())
  assert.deepEqual([...body], [...initSegment])
})

test('seek stream slices from Chrome range relative to the agent-reported stream start', async () => {
  const sw = await loadServiceWorker()
  sw.dispatchMessage({ mediaId: 'video', size: 1000 })

  const response = await sw.dispatchFetch('/media/video', { range: 'bytes=210-' })
  const reader = response.body.getReader()
  const chunk = Uint8Array.from(Array.from({ length: 80 }, (_, i) => i))

  sw.dispatchMessage({ mediaId: 'video', reset: true, generation: 1 })
  sw.dispatchMessage({ mediaId: 'video', startOffset: 200, generation: 1 })
  sw.dispatchMessage({ mediaId: 'video', chunk, generation: 1 })

  assert.deepEqual(await readChunk(reader), { done: false, value: [...chunk.slice(10)] })
})

test('normal sequential playback streams chunks in order until EOF', async () => {
  const sw = await loadServiceWorker()
  sw.dispatchMessage({ mediaId: 'video', size: 1000 })

  const response = await sw.dispatchFetch('/media/video')
  const reader = response.body.getReader()

  sw.dispatchMessage({ mediaId: 'video', chunk: new Uint8Array([1, 2]) })
  sw.dispatchMessage({ mediaId: 'video', chunk: new Uint8Array([3, 4]) })
  sw.dispatchMessage({ mediaId: 'video', chunk: null })

  assert.deepEqual(await readChunk(reader), { done: false, value: [1, 2] })
  assert.deepEqual(await readChunk(reader), { done: false, value: [3, 4] })
  assert.deepEqual(await readChunk(reader), { done: true, value: undefined })
})

test('a completed initial stream retains bytes for Chrome\'s later range request', async () => {
  const sw = await loadServiceWorker()
  sw.dispatchMessage({ mediaId: 'video', size: 4 })

  const initialResponse = await sw.dispatchFetch('/media/video')
  const initialReader = initialResponse.body.getReader()
  sw.dispatchMessage({ mediaId: 'video', chunk: new Uint8Array([1, 2]) })
  sw.dispatchMessage({ mediaId: 'video', chunk: new Uint8Array([3, 4]) })
  sw.dispatchMessage({ mediaId: 'video', chunk: null })

  assert.deepEqual(await readChunk(initialReader), { done: false, value: [1, 2] })
  assert.deepEqual(await readChunk(initialReader), { done: false, value: [3, 4] })
  assert.deepEqual(await readChunk(initialReader), { done: true, value: undefined })

  const continuationResponse = await sw.dispatchFetch('/media/video', { range: 'bytes=2-' })
  const continuationReader = continuationResponse.body.getReader()

  // If EOF deleted the completed entry, this replacement byte is all the new
  // stream can see. A retained entry replays the requested historical bytes.
  sw.dispatchMessage({ mediaId: 'video', chunk: new Uint8Array([9]) })
  const continuation = await Promise.race([
    readChunk(continuationReader),
    new Promise(resolve => setTimeout(() => resolve({ timedOut: true }), 50)),
  ])
  assert.deepEqual(continuation, { done: false, value: [3, 4] })
})

test('a fresh preview clears a completed Service Worker entry from a previous page load', async () => {
  const sw = await loadServiceWorker()
  sw.dispatchMessage({ mediaId: 'video', size: 1000 })

  const firstResponse = await sw.dispatchFetch('/media/video')
  const firstReader = firstResponse.body.getReader()
  sw.dispatchMessage({ mediaId: 'video', chunk: new Uint8Array([1, 2]) })
  assert.deepEqual(await readChunk(firstReader), { done: false, value: [1, 2] })
  await firstReader.cancel()

  // The abandoned stream is gone before EOF, so the completed entry remains
  // in the long-lived worker with stale chunks and no reader to delete it.
  sw.dispatchMessage({ mediaId: 'video', chunk: null })

  sw.dispatchMessage({ mediaId: 'video', freshPreview: true, generation: 0, startOffset: 0, size: 1000 })
  const secondResponse = await sw.dispatchFetch('/media/video')
  const secondReader = secondResponse.body.getReader()
  sw.dispatchMessage({ mediaId: 'video', chunk: new Uint8Array([9, 8]) })

  assert.deepEqual(await readChunk(secondReader), { done: false, value: [9, 8] })
})

test('a reopened preview can freshly reset the worker with a nonzero generation', async () => {
  const sw = await loadServiceWorker()
  sw.dispatchMessage({ mediaId: 'video', freshPreview: true, generation: 0, startOffset: 0, size: 1000 })
  sw.dispatchMessage({ mediaId: 'video', chunk: new Uint8Array([1, 2]) })
  sw.dispatchMessage({ mediaId: 'video', chunk: null })

  sw.dispatchMessage({ mediaId: 'video', freshPreview: true, generation: 4, startOffset: 0, size: 1000 })
  const response = await sw.dispatchFetch('/media/video?play=2')
  const reader = response.body.getReader()
  sw.dispatchMessage({ mediaId: 'video', generation: 4, chunk: new Uint8Array([9, 8]) })

  assert.deepEqual(await readChunk(reader), { done: false, value: [9, 8] })
})
