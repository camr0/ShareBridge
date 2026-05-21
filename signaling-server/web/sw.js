// Service Worker for streaming video through the DataChannel.
// Intercepts /media/{id} fetches and returns a Response backed by a
// ReadableStream whose chunks arrive via postMessage from the page.

self.addEventListener('install', () => {
  self.skipWaiting()
})

self.addEventListener('activate', event => {
  event.waitUntil(self.clients.claim())
})

// Map of mediaId -> { controller, readable, pendingChunks, resolveReady, ready }
const streams = new Map()

self.addEventListener('fetch', event => {
  const url = new URL(event.request.url)
  const match = url.pathname.match(/^\/media\/([^/]+)$/)
  if (!match) return

  const mediaId = match[1]
  let entry = streams.get(mediaId)
  if (!entry) {
    let controller
    let resolveReady
    const ready = new Promise(r => { resolveReady = r })

    const readable = new ReadableStream({
      start(c) { controller = c },
      cancel() { streams.delete(mediaId) }
    })

    const pendingChunks = []
    entry = { controller, readable, pendingChunks, resolveReady, ready }
    streams.set(mediaId, entry)
  }

  // Wait for at least one chunk before responding, so Chrome's MP4 demuxer
  // has data to probe immediately instead of failing on an empty stream.
  event.respondWith(
    entry.ready.then(() => {
      // Flush any chunks that arrived while waiting
      while (entry.pendingChunks.length > 0) {
        const chunk = entry.pendingChunks.shift()
        if (chunk === null) {
          try { entry.controller.close() } catch (_) {}
        } else {
          entry.controller.enqueue(chunk)
        }
      }
      return new Response(entry.readable, {
        status: 200,
        headers: {
          'Content-Type': 'video/mp4',
          'Accept-Ranges': 'none',
          'Cache-Control': 'no-store',
        }
      })
    })
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
      resolveReady: null,
      ready: Promise.resolve(),
    }
    streams.set(mediaId, entry)
  }

  if (chunk === null) {
    if (entry.controller) {
      try { entry.controller.close() } catch (_) {}
    } else {
      entry.pendingChunks.push(null)
    }
    streams.delete(mediaId)
    return
  }

  // Signal that at least one chunk is available
  if (entry.resolveReady) {
    entry.resolveReady()
    entry.resolveReady = null
  }

  if (entry.controller) {
    entry.controller.enqueue(chunk)
  } else {
    entry.pendingChunks.push(chunk)
  }
})
