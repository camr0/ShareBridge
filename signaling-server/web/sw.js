// Service Worker for streaming video through the DataChannel.
// Intercepts /media/{id} fetches and returns a Response backed by a
// ReadableStream whose chunks arrive via postMessage from the page.
//
// Responds immediately — no minimum-buffer gate. Chrome falls through
// to the network if event.respondWith() takes too long, so we must
// return the Response synchronously. The ReadableStream's pull() handles
// backpressure naturally: it drains available chunks and waits when
// none are ready.

self.addEventListener('install', () => {
  self.skipWaiting()
})

self.addEventListener('activate', event => {
  event.waitUntil(self.clients.claim())
})

// Map of mediaId -> { streams: Set<stream>, chunks: [], isDone }
//
// chunks is an append-only history array. Each stream tracks its own
// readCursor into this array so multiple concurrent fetches each see
// every byte from the beginning — no shared destructive shift().
const entries = new Map()

function ensureEntry(mediaId) {
  let entry = entries.get(mediaId)
  if (!entry) {
    entry = {
      streams: new Set(),
      chunks: [],        // append-only (push, never shift)
      isDone: false,
    }
    entries.set(mediaId, entry)
  }
  return entry
}

function createStreamForEntry(entry, mediaId, startOffset) {
  let ctrl
  let readCursor = 0        // per-stream position into entry.chunks
  let skipBytes = startOffset || 0  // bytes to skip within the first chunk

  // If starting mid-stream, find the right chunk and byte offset.
  if (startOffset > 0) {
    let bytesSeen = 0
    for (let i = 0; i < entry.chunks.length; i++) {
      const chunk = entry.chunks[i]
      if (chunk === null) break
      if (bytesSeen + chunk.byteLength > startOffset) {
        readCursor = i
        skipBytes = startOffset - bytesSeen
        break
      }
      bytesSeen += chunk.byteLength
      readCursor = i + 1
    }
  }

  const readable = new ReadableStream({
    start(controller) { ctrl = controller },
    pull(controller) {
      // Drain every chunk available from this stream's cursor position.
      while (readCursor < entry.chunks.length) {
        let chunk = entry.chunks[readCursor++]
        if (chunk === null) {
          entry.streams.delete(readable)
          if (entry.streams.size === 0) entries.delete(mediaId)
          try { controller.close() } catch (_) {}
          return
        }
        // Slice the first chunk if we need to skip leading bytes.
        if (skipBytes > 0) {
          if (skipBytes >= chunk.byteLength) { skipBytes -= chunk.byteLength; continue }
          chunk = chunk.slice(skipBytes)
          skipBytes = 0
        }
        controller.enqueue(chunk)
      }
      if (entry.isDone) {
        entry.streams.delete(readable)
        if (entry.streams.size === 0) entries.delete(mediaId)
        try { controller.close() } catch (_) {}
        return
      }
      return new Promise(resolve => { stream.resolvePull = resolve })
    },
    cancel() {
      entry.streams.delete(readable)
      if (entry.streams.size === 0) entries.delete(mediaId)
    },
  })

  const stream = {
    controller: ctrl,
    readable,
    resolvePull: null,   // per-stream pull-wait promise
  }
  entry.streams.add(stream)
  return stream
}

function wakeAllStreams(entry) {
  for (const stream of entry.streams) {
    if (stream.resolvePull) {
      const r = stream.resolvePull
      stream.resolvePull = null
      r()
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

  // Parse Range header for seeking.  Only return 206 for actual seeks
  // (startOffset > 0); initial loads get 200 so Chrome doesn't lock onto
  // a Content-Length we can't guarantee.
  let startOffset = 0
  let status = 200
  const rangeHeader = event.request.headers.get('range')
  if (rangeHeader) {
    const m = rangeHeader.match(/bytes=(\d+)-/)
    if (m) {
      startOffset = parseInt(m[1], 10) || 0
      if (startOffset > 0) status = 206
    }
  }

  const stream = createStreamForEntry(entry, mediaId, startOffset)

  const headers = {
    'Content-Type': 'video/mp4',
    'Cache-Control': 'no-store',
  }
  // NOTE: Don't set Accept-Ranges.  Without a known Content-Length, Chrome
  // will cancel the initial 200 and issue Range:bytes=X- to probe the file
  // end, but we can't serve that offset until all data arrives.  Seeking
  // (range support) will be added once we track the true transcoded size.

  event.respondWith(new Response(stream.readable, { status, headers }))
})

self.addEventListener('message', event => {
  if (event.data === 'ping') return
  const { mediaId, chunk } = event.data
  if (!mediaId) return
  const entry = ensureEntry(mediaId)

  if (chunk === null) {
    // Signal end of stream. Push null as EOF marker so every stream's
    // pull() loop can see it at their own cursor position.
    entry.isDone = true
    entry.chunks.push(null)
    wakeAllStreams(entry)
    return
  }

  // Append to shared history. Never shift — each stream drains
  // independently from its own readCursor.
  entry.chunks.push(chunk)

  // Wake all waiting pull() loops so they drain from their cursors.
  wakeAllStreams(entry)
})
