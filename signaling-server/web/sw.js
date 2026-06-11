// Service Worker for streaming video through the DataChannel.
// Intercepts /media/{id} fetches and returns a Response backed by a
// ReadableStream whose chunks arrive via postMessage from the page.

self.addEventListener('install', () => {
  self.skipWaiting()
})

self.addEventListener('activate', event => {
  event.waitUntil(self.clients.claim())
})

// Buffer initial chunks before responding so Chrome's MP4 demuxer has
// enough data to parse the moov atom.  Without this, Chrome's
// ReadableStream read timeout fires before chunks arrive via the relay.
// Chrome's *fetch* timeout is much longer, so delaying the Response is
// the correct strategy.
const MIN_INITIAL_CHUNKS = 6

// Map of mediaId -> { streams: Set<stream>, pendingChunks: [], resolvePull, chunkCount }
// Each fetch creates a dedicated ReadableStream stored in `streams`.
// Chunks are broadcast to every active stream.
const entries = new Map()

function ensureEntry(mediaId) {
  let entry = entries.get(mediaId)
  if (!entry) {
    entry = {
      streams: new Set(),
      pendingChunks: [],
      resolvePull: null,
      chunkCount: 0,
    }
    entries.set(mediaId, entry)
  }
  return entry
}

function createStreamForEntry(entry, mediaId) {
  let ctrl
  // Per-stream ready promise: resolves when global chunkCount >= MIN_INITIAL_CHUNKS.
  // Each stream has its own ready so concurrent fetches don't share a locked stream.
  let resolveReady
  let readyTimer = null
  const ready = new Promise(r => { resolveReady = r })

  if (entry.chunkCount >= MIN_INITIAL_CHUNKS) {
    resolveReady()
    resolveReady = null
  }

  const readable = new ReadableStream({
    start(controller) { ctrl = controller },
    pull(controller) {
      while (entry.pendingChunks.length > 0) {
        const chunk = entry.pendingChunks.shift()
        if (chunk === null) { drainNull(entry, mediaId); return }
        controller.enqueue(chunk)
      }
      return new Promise(resolve => { entry.resolvePull = resolve })
    },
    cancel() {
      clearTimeout(readyTimer)
      entry.streams.delete(readable)
      if (entry.streams.size === 0) entries.delete(mediaId)
    },
  })

  const stream = { controller: ctrl, readable, resolveReady, readyTimer }
  entry.streams.add(stream)
  return stream
}

function drainNull(entry, mediaId) {
  for (const stream of entry.streams) {
    try { stream.controller.close() } catch (_) {}
  }
  entry.streams.clear()
  entries.delete(mediaId)
}

function broadcastChunk(entry, chunk, mediaId) {
  for (const stream of entry.streams) {
    try { stream.controller.enqueue(chunk) } catch (_) {}
  }
  if (entry.resolvePull) {
    const r = entry.resolvePull; entry.resolvePull = null; r()
  }
}

function resolveAllReady(entry) {
  for (const stream of entry.streams) {
    if (stream.resolveReady) {
      clearTimeout(stream.readyTimer)
      stream.readyTimer = null
      stream.resolveReady()
      stream.resolveReady = null
    }
  }
}

// --- event listeners -------------------------------------------------

self.addEventListener('fetch', event => {
  const url = new URL(event.request.url)
  const match = url.pathname.match(/^\/media\/([^/]+)$/)
  if (!match) return
  const mediaId = match[1]
  const entry = ensureEntry(mediaId)
  const stream = createStreamForEntry(entry, mediaId)

  event.respondWith(
    stream.ready.then(() => new Response(stream.readable, {
      status: 200,
      headers: { 'Content-Type': 'video/mp4', 'Cache-Control': 'no-store' }
    }))
  )
})

self.addEventListener('message', event => {
  if (event.data === 'ping') return
  const { mediaId, chunk } = event.data
  if (!mediaId) return
  const entry = ensureEntry(mediaId)

  if (chunk === null) {
    resolveAllReady(entry)
    if (entry.streams.size > 0) drainNull(entry, mediaId)
    else entry.pendingChunks.push(null)
    entries.delete(mediaId)
    return
  }

  entry.pendingChunks.push(chunk)
  entry.chunkCount++

  if (entry.chunkCount >= MIN_INITIAL_CHUNKS) resolveAllReady(entry)

  if (entry.streams.size > 0) broadcastChunk(entry, chunk, mediaId)
})
