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
//
// Generation protocol: every preview (initial or seek) gets a unique
// generation number. Chunks carry generation in the postMessage, and
// the SW drops stale chunks. Streams are stamped with the generation
// at creation time and close themselves when it changes.

self.addEventListener('install', () => {
  self.skipWaiting()
})

self.addEventListener('activate', event => {
  event.waitUntil(self.clients.claim())
})

// Map of mediaId -> {
//   streams: Set<stream>,
//   chunks: [],        // append-only (push, never shift)
//   isDone: false,
//   totalSize: 0,      // Content-Length of the full transcoded video
//   streamStartOffset: 0|null, // actual byte offset where the current generation starts
//   generation: 0,     // monotonic generation counter
//   totalBufferedBytes: 0,  // total bytes buffered for current generation
//   initSegment: Uint8Array|null,  // cached MP4 init segment (ftyp+...+moov)
//   initCapture: Uint8Array|null,  // temporary bytes while finding moov
// }
const entries = new Map()

function ensureEntry(mediaId) {
  let entry = entries.get(mediaId)
  if (!entry) {
    entry = {
      streams: new Set(),
      chunks: [],        // append-only (push, never shift)
      isDone: false,
      totalSize: 0,      // Content-Length of the full transcoded video
      streamStartOffset: 0,
      generation: 0,
      totalBufferedBytes: 0,
      initSegment: null,
      initCapture: null,
      debug: false,
    }
    entries.set(mediaId, entry)
  }
  return entry
}

function mediaDebugPayload(stage, entry, extra = {}) {
  return {
    type: 'media_debug',
    stage,
    generation: entry.generation,
    isDone: entry.isDone,
    streamStartOffset: entry.streamStartOffset,
    totalBufferedBytes: entry.totalBufferedBytes,
    streamCount: entry.streams.size,
    ...extra,
  }
}

function reportMessageDebug(event, stage, entry, extra) {
  if (!entry.debug || !event.source?.postMessage) return
  event.source.postMessage(mediaDebugPayload(stage, entry, extra))
}

function reportFetchDebug(event, stage, entry, extra) {
  if (!entry.debug || !event.clientId || !event.waitUntil || !self.clients?.get) return
  event.waitUntil(
    self.clients.get(event.clientId).then((client) => {
      client?.postMessage(mediaDebugPayload(stage, entry, extra))
    }).catch(() => {}),
  )
}

function concatUint8Arrays(parts) {
  let total = 0
  for (const part of parts) total += part.byteLength
  const merged = new Uint8Array(total)
  let offset = 0
  for (const part of parts) {
    merged.set(part, offset)
    offset += part.byteLength
  }
  return merged
}

function readBoxSize(bytes, offset) {
  return (
    (bytes[offset] * 0x1000000) +
    (bytes[offset + 1] << 16) +
    (bytes[offset + 2] << 8) +
    bytes[offset + 3]
  )
}

function captureInitSegment(entry, chunk) {
  if (entry.initSegment || entry.streamStartOffset !== 0 || entry.generation !== 0 || !chunk?.byteLength) return

  entry.initCapture = entry.initCapture
    ? concatUint8Arrays([entry.initCapture, chunk])
    : chunk.slice()

  const bytes = entry.initCapture
  let offset = 0
  while (offset + 8 <= bytes.byteLength) {
    const size = readBoxSize(bytes, offset)
    if (size < 8) return
    if (offset + size > bytes.byteLength) return

    const type = String.fromCharCode(
      bytes[offset + 4],
      bytes[offset + 5],
      bytes[offset + 6],
      bytes[offset + 7],
    )
    offset += size
    if (type === 'moov') {
      entry.initSegment = bytes.slice(0, offset)
      entry.initCapture = null
      return
    }
  }
}

function parseRangeHeader(rangeHeader) {
  if (!rangeHeader) return null
  const match = rangeHeader.match(/bytes=(\d+)-(\d+)?/)
  if (!match) return null

  return {
    start: parseInt(match[1], 10) || 0,
    end: match[2] !== undefined ? (parseInt(match[2], 10) || 0) : null,
  }
}

function computeStreamPosition(entry, startOffset) {
  let readCursor = 0
  let skipBytes = 0

  // Compute effective offset within the current generation's data.
  const streamStartOffset = entry.streamStartOffset ?? 0
  let effectiveStart = startOffset - streamStartOffset
  if (effectiveStart < 0) {
    // Seek is before the start of our buffered data for this generation.
    // Chrome will try a few offsets; the page will trigger a new agent seek
    // if needed. Clamp and let what data we have be served.
    effectiveStart = 0
  }

  // If starting mid-stream, find the right chunk and byte offset.
  if (effectiveStart > 0) {
    let bytesSeen = 0
    for (let i = 0; i < entry.chunks.length; i++) {
      const chunk = entry.chunks[i]
      if (chunk === null) break
      if (bytesSeen + chunk.byteLength > effectiveStart) {
        readCursor = i
        skipBytes = effectiveStart - bytesSeen
        break
      }
      bytesSeen += chunk.byteLength
      readCursor = i + 1
    }
    // If we exhausted all available chunks without reaching effectiveStart,
    // deduct what we've already scanned past so pull() only skips the
    // remaining bytes when new chunks arrive.
    if (readCursor >= entry.chunks.length) {
      skipBytes = effectiveStart - bytesSeen
    }
  }

  return { readCursor, skipBytes }
}

function rebaseStream(stream, entry, generation) {
  const position = computeStreamPosition(entry, stream.startOffset)
  stream.readCursor = position.readCursor
  stream.skipBytes = position.skipBytes
  stream.generation = generation
}

function createStreamForEntry(entry, mediaId, startOffset) {
  let ctrl
  const stream = {
    controller: null,
    readable: null,
    resolvePull: null,   // per-stream pull-wait promise
    startOffset,
    createdGeneration: entry.generation,
    generation: entry.generation,
    readCursor: 0,
    skipBytes: 0,
    initSegment: null,
    initQueued: false,
  }
  rebaseStream(stream, entry, entry.generation)

  const readable = new ReadableStream({
    start(controller) {
      ctrl = controller
      stream.controller = controller
    },
    pull(controller) {
      // If generation changed, this stream is stale — close it.
      if (entry.generation !== stream.generation) {
        entry.streams.delete(stream)
        if (entry.streams.size === 0) entries.delete(mediaId)
        try { controller.close() } catch (_) {}
        return
      }
      if (stream.initSegment && !stream.initQueued) {
        stream.initQueued = true
        controller.enqueue(stream.initSegment)
        return
      }
      // Drain every chunk available from this stream's cursor position.
      while (stream.readCursor < entry.chunks.length) {
        let chunk = entry.chunks[stream.readCursor++]
        if (chunk === null) {
          entry.streams.delete(stream)
          // Keep the completed byte history. Chrome can finish this response
          // after decoding only part of the MP4, then issue another Range
          // request when playback reaches that buffer edge.
          try { controller.close() } catch (_) {}
          return
        }
        // Trim leading bytes when seeking to a mid-chunk position.
        if (stream.skipBytes > 0) {
          if (stream.skipBytes >= chunk.byteLength) {
            stream.skipBytes -= chunk.byteLength
            continue
          }
          chunk = chunk.slice(stream.skipBytes)
          stream.skipBytes = 0
        }
        controller.enqueue(chunk)
      }
      if (entry.isDone) {
        entry.streams.delete(stream)
        // A later Range request must still be able to replay completed bytes.
        // The next generation-0 freshPreview message resets this entry.
        try { controller.close() } catch (_) {}
        return
      }
      return new Promise(resolve => { stream.resolvePull = resolve })
    },
    cancel() {
      entry.streams.delete(stream)
      // Don't delete the entry when all streams cancel — Chrome
      // cancels the old fetch before issuing a new Range request.
      // Deleting would drop all buffered chunks and force the new
      // stream to start from scratch.  Only clean up on EOF (null
      // chunk) when isDone and no streams remain.
    },
  })

  stream.readable = readable
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
  const rangeHeader = event.request.headers.get('range')
  const parsedRange = parseRangeHeader(rangeHeader)

  reportFetchDebug(event, 'fetch', entry, { range: rangeHeader, requestStart: parsedRange?.start ?? 0 })

  if (
    parsedRange &&
    parsedRange.start === 0 &&
    entry.generation > 0 &&
    entry.initSegment
  ) {
    const initSegmentLength = entry.initSegment.byteLength
    const endOffset = Math.min(
      parsedRange.end ?? (initSegmentLength - 1),
      initSegmentLength - 1,
    )
    const contentLength = Math.max(0, endOffset + 1)
    const totalSize = entry.totalSize || initSegmentLength

    event.respondWith(new Response(entry.initSegment.slice(0, contentLength), {
      status: 206,
      headers: {
        'Content-Type': 'video/mp4',
        'Cache-Control': 'no-store',
        'Accept-Ranges': 'bytes',
        'Content-Length': String(contentLength),
        'Content-Range': `bytes 0-${endOffset}/${totalSize}`,
      },
    }))
    return
  }

  // Parse Range header for seeking.
  let startOffset = 0
  let status = 200
  let contentRange = null
  if (parsedRange) {
    startOffset = parsedRange.start
      // Return 206 for all Range requests when totalSize is known so
      // Chrome detects Range support on its initial probe and uses
      // Range requests for seeking.  bytes=0- with 206 + Content-Range
      // is semantically identical to 200 + Content-Length.
    if (entry.totalSize > 0) {
      status = 206
      contentRange = startOffset === 0
        ? `bytes 0-${entry.totalSize - 1}/${entry.totalSize}`
        : `bytes ${startOffset}-${entry.totalSize - 1}/${entry.totalSize}`
    }
  }

  const stream = createStreamForEntry(entry, mediaId, startOffset)
  reportFetchDebug(event, 'stream-created', entry, {
    requestStart: startOffset,
    streamStart: stream.startOffset,
    streamGeneration: stream.generation,
  })

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

  const { mediaId, chunk, size, generation, reset, startOffset, freshPreview, debug } = event.data
  if (!mediaId) return

  const entry = ensureEntry(mediaId)
  entry.debug = entry.debug || debug === true

  // A Service Worker can outlive the page that abandoned this media stream.
  // Start an initial generation from a clean entry so stale chunks/EOF cannot
  // complete the replacement response before its new transfer arrives.
  if (freshPreview === true && Number.isInteger(generation) && generation >= 0) {
    for (const stream of entry.streams) {
      try { stream.controller?.close() } catch (_) {}
    }
    entry.streams.clear()
    entry.chunks = []
    entry.isDone = false
    entry.totalSize = size > 0 ? size : 0
    entry.streamStartOffset = startOffset ?? 0
    entry.generation = generation
    entry.totalBufferedBytes = 0
    entry.initSegment = null
    entry.initCapture = null
    reportMessageDebug(event, 'fresh-preview', entry)
    return
  }

  // --- Reset message (seek start) ---
  // {mediaId, reset: true, generation: N}
  if (reset !== undefined && reset !== null) {
    // Guard against stale resets: only apply if the generation is newer.
    // generation 0 is the initial state; every real generation is >= 1.
    if (generation <= entry.generation) return
    const previousGeneration = entry.generation
    // Reset entry state for the new generation.
    entry.chunks = []
    entry.isDone = false
    entry.streamStartOffset = null
    entry.generation = generation
    entry.totalBufferedBytes = 0
    entry.initCapture = null
    // Chrome may create the seek fetch before the page's seeked handler sends
    // this reset. Prefer streams created during the previous generation when
    // they exist; otherwise preserve the one parked stream and rebase it
    // again. The latter happens when Chrome reuses the original fetch across
    // consecutive seeks. Closing that stream on the second reset leaves the
    // player waiting forever even though the new generation's chunks arrive.
    const previousStreams = [...entry.streams].filter((s) => s.generation === previousGeneration)
    const freshPreviousStreams = previousStreams.filter((s) => s.createdGeneration === previousGeneration)
    const streamsToRebase = freshPreviousStreams.length > 0 ? freshPreviousStreams : previousStreams

    for (const s of [...entry.streams]) {
      if (!streamsToRebase.includes(s)) {
        entry.streams.delete(s)
        try { s.controller?.close() } catch (_) {}
        continue
      }
      rebaseStream(s, entry, generation)
    }
    wakeAllStreams(entry)
    reportMessageDebug(event, 'reset', entry, { previousGeneration })
    return
  }

  // Store the actual byte offset where the agent's current seek stream starts.
  // This is the reference point we use when slicing Chrome's Range requests.
  if (startOffset !== undefined && startOffset !== null) {
    if (generation !== undefined && generation !== entry.generation) return
    entry.streamStartOffset = startOffset
    for (const s of entry.streams) {
      if (s.generation === entry.generation) {
        rebaseStream(s, entry, entry.generation)
      }
    }
    reportMessageDebug(event, 'stream-offset', entry, { messageGeneration: generation })
    return
  }

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
    reportMessageDebug(event, 'range-ended', entry)
    return
  }

  // --- Chunk message ---
  // {mediaId, chunk, generation}
  // Drop stale chunks from cancelled streams.
  if (generation !== undefined && generation !== entry.generation) {
    return
  }

  // Append to shared history. Never shift — each stream drains
  // independently from its own readCursor.
  captureInitSegment(entry, chunk)
  entry.chunks.push(chunk)
  entry.totalBufferedBytes += chunk.byteLength

  // Wake all waiting pull() loops so they drain from their cursors.
  wakeAllStreams(entry)
})
