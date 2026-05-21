// Service Worker for streaming video through the DataChannel.
// Intercepts /media/{id} fetches and returns a Response backed by a
// ReadableStream whose chunks arrive via postMessage from the page.

self.addEventListener('install', () => {
  self.skipWaiting()
})

self.addEventListener('activate', event => {
  event.waitUntil(self.clients.claim())
})

// Wait for several chunks before starting the stream so Chrome's MP4 demuxer
// has enough data to parse the full moov atom (including the AAC audio decoder
// config in the esds box). 6 × 64KB = 384KB covers the moov for even large videos.
const MIN_INITIAL_CHUNKS = 6

// Map of mediaId -> { controller, pendingChunks, resolvePull, resolveReady, ready, chunkCount }
const streams = new Map()

self.addEventListener('fetch', event => {
  const url = new URL(event.request.url)
  const match = url.pathname.match(/^\/media\/([^/]+)$/)
  if (!match) return

  const mediaId = match[1]
  let entry = streams.get(mediaId)
  if (!entry) {
    let ctrl
    let resolveReady
    const ready = new Promise(r => { resolveReady = r })

    const readable = new ReadableStream({
      start(controller) {
        ctrl = controller
      },
      pull(controller) {
        // Serve pending chunks if available
        while (entry.pendingChunks.length > 0) {
          const chunk = entry.pendingChunks.shift()
          if (chunk === null) {
            controller.close()
            streams.delete(mediaId)
            return
          }
          controller.enqueue(chunk)
        }
        // Return a promise that resolves when next chunk arrives.
        // This tells the browser to wait instead of erroring.
        return new Promise(resolve => {
          entry.resolvePull = resolve
        })
      },
      cancel() {
        streams.delete(mediaId)
      }
    })

    const pendingChunks = []
    entry = { controller: ctrl, readable, pendingChunks, resolvePull: null, resolveReady, ready, chunkCount: 0 }
    streams.set(mediaId, entry)
  }

  // Wait for initial chunks before responding, so Chrome's MP4 demuxer
  // has data to probe immediately.
  event.respondWith(
    entry.ready.then(() => new Response(entry.readable, {
      status: 200,
      headers: {
        'Content-Type': 'video/mp4',
        'Accept-Ranges': 'none',
        'Cache-Control': 'no-store',
      }
    }))
  )
})

self.addEventListener('message', event => {
  if (event.data === 'ping') return

  const { mediaId, chunk } = event.data
  if (!mediaId) return

  let entry = streams.get(mediaId)
  if (!entry) {
    entry = {
      controller: null,
      readable: null,
      pendingChunks: [],
      resolvePull: null,
      resolveReady: null,
      ready: Promise.resolve(),
      chunkCount: 0,
    }
    streams.set(mediaId, entry)
  }

  // Signal end-of-stream
  if (chunk === null) {
    // Resolve ready immediately so the stream can close
    if (entry.resolveReady) {
      entry.resolveReady()
      entry.resolveReady = null
    }
    if (entry.controller) {
      if (entry.resolvePull) {
        entry.resolvePull()
        entry.resolvePull = null
      }
      entry.controller.close()
    } else {
      entry.pendingChunks.push(null)
    }
    streams.delete(mediaId)
    return
  }

  // Buffer chunk
  entry.pendingChunks.push(chunk)
  entry.chunkCount++

  // Resolve the ready promise once enough initial chunks are buffered
  if (entry.resolveReady && entry.chunkCount >= MIN_INITIAL_CHUNKS) {
    entry.resolveReady()
    entry.resolveReady = null
  }

  // Wake up the pull() promise if consumer is waiting
  if (entry.resolvePull) {
    const resolve = entry.resolvePull
    entry.resolvePull = null
    resolve()
  }
})
