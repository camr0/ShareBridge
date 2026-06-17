// Service Worker for streaming video through the DataChannel.
// Intercepts /media/{id} fetches and returns a Response backed by a
// ReadableStream whose chunks arrive via postMessage from the page.
//
// Responds immediately — no minimum-buffer gate. Chrome falls through
// to the network if event.respondWith() takes too long, so we must
// return the Response synchronously. The ReadableStream's pull() handles
// backpressure naturally: it drains available chunks and waits when
// none are ready.
//
// Seeking: when totalSize is known (forwarded from the agent via
// postMessage), we set Content-Length + Accept-Ranges so Chrome's
// <video> element can scrub. Range requests are served as 206 with
// Content-Range. Backward seeks are instant (data already buffered in
// the append-only chunks array). Forward seeks wait for the streaming
// position to catch up.

self.addEventListener('install', () => {
  self.skipWaiting()
})

self.addEventListener('activate', event => {
  event.waitUntil(self.clients.claim())
})

// Map of mediaId -> { streams: Set<stream>, chunks: [], isDone, totalSize }
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
      totalSize: 0,      // Content-Length of the full transcoded video
    }
    entries.set(mediaId, entry)
  }
  return entry
}

function createStreamForEntry(entry, mediaId, startOffset) {
  let ctrl
  let readCursor = 0        // per-stream position into entry.chunks
  let skipBytes = startOffset || 0  // bytes to skip before enqueuing

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
    // If we exhausted all available chunks without reaching startOffset,
    // deduct what we've already scanned past so pull() only skips the
    // remaining bytes when new chunks arrive.
    if (readCursor >= entry.chunks.length) {
      skipBytes = startOffset - bytesSeen
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
        // Trim leading bytes when seeking to a mid-chunk position.
        if (skipBytes > 0) {
          if (skipBytes >= chunk.byteLength) {
            skipBytes -= chunk.byteLength
            continue
          }
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

  // Parse Range header for seeking.
  let startOffset = 0
  let status = 200
  let contentRange = null
  const rangeHeader = event.request.headers.get('range')
  if (rangeHeader) {
    const m = rangeHeader.match(/bytes=(\d+)-/)
    if (m) {
      startOffset = parseInt(m[1], 10) || 0
      // Only use 206 for actual seeks (startOffset > 0).  Chrome's video
      // element sends Range: bytes=0- as a probe but needs a 200 with the
      // full MP4 (including ftyp/moov atoms) to initialize the demuxer.
      if (startOffset > 0 && entry.totalSize > 0) {
        status = 206
        contentRange = `bytes ${startOffset}-${entry.totalSize - 1}/${entry.totalSize}`
      }
    }
  }

  const stream = createStreamForEntry(entry, mediaId, startOffset)

  const headers = {
    'Content-Type': 'video/mp4',
    'Cache-Control': 'no-store',
  }

  // Advertise seeking when we know the total size.  Without Content-Length
  // Chrome treats the stream as live/unseekable even though the duration is
  // parsed from the moov atom.
  if (entry.totalSize > 0) {
    headers['Accept-Ranges'] = 'bytes'
    // For 200, include Content-Length so Chrome can map time→byte offsets.
    // For 206, Content-Range provides the total size instead.
    if (status === 200) {
      headers['Content-Length'] = String(entry.totalSize)
    }
  }
  if (contentRange) {
    headers['Content-Range'] = contentRange
  }

  event.respondWith(new Response(stream.readable, { status, headers }))
})

self.addEventListener('message', event => {
  if (event.data === 'ping') return

  const { mediaId, chunk, size } = event.data
  if (!mediaId) return

  const entry = ensureEntry(mediaId)

  // Store the total content size for Content-Length / Accept-Ranges.
  // Sent by the page before the <video> element starts fetching.
  if (size > 0 && !entry.totalSize) {
    entry.totalSize = size
    return
  }

  if (chunk === null) {
    // Signal end of stream. Push null as EOF marker so every stream's
    // pull() loop can see it at their own cursor position.
    entry.isDone = true
    entry.chunks.push(null)
    // Compute total size from the accumulated chunks if the agent
    // didn't send one (e.g. HEAD /video/playback didn't report size).
    // This lets rewind seeks work after the video finishes playing.
    if (!entry.totalSize) {
      let total = 0
      for (const c of entry.chunks) {
        if (c === null) break
        total += c.byteLength
      }
      if (total > 0) entry.totalSize = total
    }
    wakeAllStreams(entry)
    return
  }

  // Append to shared history. Never shift — each stream drains
  // independently from its own readCursor.
  entry.chunks.push(chunk)

  // Wake all waiting pull() loops so they drain from their cursors.
  wakeAllStreams(entry)
})
