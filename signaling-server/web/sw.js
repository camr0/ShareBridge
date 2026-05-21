// Service Worker for streaming video through the DataChannel.
// Intercepts /media/{id} fetches and returns a Response backed by a
// ReadableStream whose chunks arrive via postMessage from the page.

self.addEventListener('install', () => {
  self.skipWaiting()
})

self.addEventListener('activate', event => {
  event.waitUntil(self.clients.claim())
})

// Map of mediaId -> { controller, chunks }
const streams = new Map()

self.addEventListener('fetch', event => {
  const url = new URL(event.request.url)
  const match = url.pathname.match(/^\/media\/([^/]+)$/)
  if (!match) return

  const mediaId = match[1]
  let entry = streams.get(mediaId)
  if (!entry) {
    // Create a new stream entry. Chunks will be enqueued via postMessage.
    let controller
    const readable = new ReadableStream({
      start(c) { controller = c },
      cancel() { streams.delete(mediaId) }
    })

    const pendingChunks = []
    entry = { controller, readable, pendingChunks }
    streams.set(mediaId, entry)

    // Flush any chunks that arrived before the fetch
    while (entry.pendingChunks.length > 0) {
      const chunk = entry.pendingChunks.shift()
      if (chunk === null) {
        try { controller.close() } catch (_) {}
      } else {
        controller.enqueue(chunk)
      }
    }
  }

  event.respondWith(
    new Response(entry.readable, {
      status: 200,
      headers: {
        'Content-Type': 'video/mp4',
        'Accept-Ranges': 'none',
        'Cache-Control': 'no-store',
      }
    })
  )
})

self.addEventListener('message', event => {
  if (event.data === 'ping') return

  const { mediaId, chunk } = event.data
  if (!mediaId) return

  let entry = streams.get(mediaId)
  if (!entry) {
    // Stream hasn't been fetched yet — buffer chunks
    entry = {
      controller: null,
      readable: null,
      pendingChunks: [],
    }
    streams.set(mediaId, entry)
  }

  if (chunk === null) {
    // null signals end-of-stream
    if (entry.controller) {
      try { entry.controller.close() } catch (_) {}
    } else {
      entry.pendingChunks.push(null)
    }
    streams.delete(mediaId)
    return
  }

  if (entry.controller) {
    entry.controller.enqueue(chunk)
  } else {
    entry.pendingChunks.push(chunk)
  }
})
