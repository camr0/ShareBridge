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

test('forward seek fetch created before reset survives and rebases to the new generation', async () => {
  const sw = await loadServiceWorker()
  sw.dispatchMessage({ mediaId: 'video', size: 1000 })

  const response = await sw.dispatchFetch('/media/video', { range: 'bytes=100-' })
  const reader = response.body.getReader()

  sw.dispatchMessage({ mediaId: 'video', reset: 100, generation: 1 })
  sw.dispatchMessage({ mediaId: 'video', chunk: new Uint8Array([7, 8, 9]), generation: 1 })

  assert.deepEqual(await readChunk(reader), { done: false, value: [7, 8, 9] })
})

test('later seek generation wins over an earlier parked seek stream', async () => {
  const sw = await loadServiceWorker()
  sw.dispatchMessage({ mediaId: 'video', size: 2000 })

  const firstResponse = await sw.dispatchFetch('/media/video', { range: 'bytes=100-' })
  const firstReader = firstResponse.body.getReader()
  sw.dispatchMessage({ mediaId: 'video', reset: 100, generation: 1 })

  const secondResponse = await sw.dispatchFetch('/media/video', { range: 'bytes=120-' })
  const secondReader = secondResponse.body.getReader()
  sw.dispatchMessage({ mediaId: 'video', reset: 120, generation: 2 })

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

test('forward seek fetch prepends cached init segment before rebased seek data', async () => {
  const sw = await loadServiceWorker()
  sw.dispatchMessage({ mediaId: 'video', size: 1000 })

  const initSegment = new Uint8Array([
    0x00, 0x00, 0x00, 0x10, 0x66, 0x74, 0x79, 0x70, 1, 2, 3, 4, 5, 6, 7, 8,
    0x00, 0x00, 0x00, 0x0c, 0x6d, 0x6f, 0x6f, 0x76, 9, 10, 11, 12,
  ])
  sw.dispatchMessage({ mediaId: 'video', chunk: initSegment })

  const response = await sw.dispatchFetch('/media/video', { range: 'bytes=100-' })
  assert.equal(response.status, 200)

  const reader = response.body.getReader()
  sw.dispatchMessage({ mediaId: 'video', reset: 100, generation: 1 })
  sw.dispatchMessage({ mediaId: 'video', chunk: new Uint8Array([7, 8, 9]), generation: 1 })

  assert.deepEqual(await readChunk(reader), { done: false, value: [...initSegment] })
  assert.deepEqual(await readChunk(reader), { done: false, value: [7, 8, 9] })
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
