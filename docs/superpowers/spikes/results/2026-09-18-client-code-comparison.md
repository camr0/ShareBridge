# v1 direct client vs the "v2 browser-sink" arm — code comparison of the download path — 2026-09-18

**Method:** read-only source comparison. No field rig, no VM, no containers, no `git`. The only
throughput numbers quoted below are imported from E10c / E27 / E15 as *given field measurements*;
nothing here derives a rate from source reading.

**Central answer — short form:** **yes, but not in the way the question assumes.** The v2 arm does
materially less work per byte because it does **no page-JavaScript work per byte at all** — it is not a
faster implementation of a byte-level client, it is the *absence* of one. Chrome performs an ordinary
native HTTP download of a `Content-Disposition: attachment` response (`/s/{code}/asset/{id}`). There is
no DataChannel, no chunk assembly, no SHA-1, and no StreamSaver/service-worker hop. Consequently the
239.054 Mbps E10c number is evidence about **Chrome's native HTTP download path**, and is **not**
evidence about any DataChannel-based receive path, fast or slow. It is a template for v2's
*architecture*, not for v1's receive *code*.

---

## 1. Locating the two clients

### v1 direct client (the measured ~110–120 Mbps stack)

Worktree `.worktrees/e2e-v1/`, browser app under `signaling-server/web/` — page `/s/{code}` serves
`web/index.html`, which loads `/src/app.js`. The bulk receive path is:

| role | file |
|---|---|
| lane fan-in / per-chunk orchestration | `signaling-server/web/src/app.js` (2 640 lines) |
| WebRTC lane adapter | `signaling-server/web/src/directChannel.js` |
| wire envelope decode | `signaling-server/web/src/binaryEnvelope.js` |
| per-chunk pipeline | `signaling-server/web/src/downloadPipeline.js` |
| **the sink** (SHA-1 + StreamSaver) | `signaling-server/web/src/downloadSinks.js` |
| vendor StreamSaver writer | `signaling-server/web/src/vendor/streamsaver.js` |
| vendor StreamSaver service worker | `signaling-server/web/src/vendor/streamsaver-sw.js` |
| vendor SHA-1 | `signaling-server/web/src/vendor/hash-wasm.js` |

Confirmed to be the field stack: `app.js:1330` registers exactly `'/src/vendor/streamsaver-sw.js'` as
the streaming sink's service worker, and `app.js:1588-1605` builds
`createStreamingSink({ tailBytes: 1024*1024, createWriter: createBrowserStreamWriter, createHasher:
createIncrementalSha1 })` — i.e. hash-wasm SHA-1 + StreamSaver, matching E15/E27's description of the
measured sink byte-for-byte.

### The "v2 / phase-4a browser test client"

Worktree `.worktrees/phase-4a/`. It has **no byte-level client to compare**. The page is
`agent/internal/direct/static/app.js` (108 lines) + `gallery.js` (484 lines), and its own header
comment is the whole story:

```js
// agent/internal/direct/static/app.js:1-7
// ShareBridge recipient entry script.
// ...
// hands them to the (transport-agnostic) gallery presentation layer. Downloads
// are native browser navigations to /asset/{id} and /archive/{token}/{part}
// (the server sets Content-Disposition: attachment); video playback streams
// from /asset/{id}/playback.
```

`gallery.js:157` is the download trigger — `anchor.href = url`, an ordinary navigation. The only
`fetch()` in the client is `app.js:78` (`GET ${base}/items` → JSON). A repo-wide search of
`.worktrees/phase-4a` for `showSaveFilePicker`, `FileSystemWritableFileStream`, `hash-wasm`,
`crypto.subtle`, `createDataChannel` and `ondatachannel` in `.js/.mjs/.html/.ts` returns **zero hits**
(the only `crypto/subtle` hits are Go `crypto/subtle` imports in `server.go` files).

The server side is a plain Go `http` handler that streams to the response writer:

```go
// agent/internal/direct/handlers.go:347 handleAsset
w.Header().Set("Content-Disposition", contentDisposition(sanitizeFilename(asset.OriginalFileName)))
if size := asset.FileSize(); size > 0 {
    w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
}
...
s.streamBodyLimited(r, session, w, http.StatusOK, func(dst io.Writer) error {
    streamed, err := session.Backend.GetFile(r.Context(), id, dst)
    ...
})
```

The bench endpoint is even barer — `agent/internal/direct/server.go:470 handleDownload` sets
`Content-Length` and does `io.CopyN(dst, zeroReader{}, size)`.

This matches the E10c cell-B protocol exactly: *"navigated to the relay origin asset URL and the
browser's own download sink wrote the asset to disk. No `download.path()` / `download.saveAs()` was
called (only the `download` event …)"*. E10c's "browser sink" arm is therefore **Chrome downloading an
HTTP attachment**, not an app client.

---

## 2. Per-chunk receive path, v1 direct (Chrome desktop, `support.mode === 'streaming'`)

Chunk size is fixed at 64 KiB on the agent (`e2e-v1/agent/internal/transfer/manager.go:24`
`chunkSize = 64 * 1024`), so at 110 Mbps the client processes **~210 chunks/s**; at 239 Mbps it would
be ~455/s. Every item below is per chunk.

**Step 1 — `onmessage` (lane fan-in).** `directChannel.js:24` sets `binaryType = 'arraybuffer'`, so
`event.data` is an `ArrayBuffer`. `app.js:461-464`:

```js
channel.bulk.onmessage = (event) => {
  if (!isCurrentSession()) return
  bulkChain = bulkChain.then(() => isCurrentSession() && handleBulkMessage(event, { isCurrentSession })).catch(handleLaneError)
}
```
→ one promise-chain link + closure per chunk; all chunks are strictly serialised through `bulkChain`.

**Step 2 — envelope decode = copy #1.** `handleBulkMessage` (`app.js:887`) calls
`decodeBinaryEnvelope(event.data)`; `binaryEnvelope.js:47-58`:

```js
const data = input instanceof Uint8Array ? input : new Uint8Array(input)   // view, free
...
payload: data.slice(CHUNK_ENVELOPE_BYTES)                                  // 14-byte header stripped
```
`Uint8Array.prototype.slice` **copies the whole 64 KiB payload** to drop a 14-byte header.

**Step 3 — routing, wire accounting, timer churn.** `routeOrBufferFrame` (`app.js:989-1013`) does
BigInt arithmetic (`operation.wireBytes + BigInt(frame.payload.byteLength)`) and calls
`refreshCompletionTimeout('bulk', …)` → `deferOperationEnd` (`app.js:1098-1110`), which does
`clearTimeout` + `setTimeout` **per chunk**:

```js
function deferOperationEnd(lane, msg) {
  clearCompletionTimeout(lane)
  ...
  const timer = setTimeout(() => { ... }, completionTimeoutMs)
```
Then `await append` (one more await hop).

**Step 4 — ingest chain.** `appendChunk` → `enqueueBulkIngest` (`app.js:1478-1491`):

```js
const run = operation.ingestChain.then(async () => {
  await operation.initPromise
  if (!isCurrentBulkOperation(operation) || !operation.pipeline) return
  await action(operation.pipeline)
})
```
→ a second promise chain and two more awaits per chunk. Together with steps 1 and 3 that is ~4
serialised microtask turns per chunk with several closures allocated per chunk.

**Step 5 — pipeline.** `downloadPipeline.js:39-51`: counters, `bumpTimeout()` (a **second**
`clearTimeout` + `setTimeout` pair per chunk), then `await sink.append(bytes)`.

**Step 6 — the sink: SHA-1 on the main thread, and ~17× copy amplification.**
`downloadSinks.js:19-31`:

```js
async append(bytes) {
  hasher.update(bytes)                          // SHA-1, hash-wasm WASM, MAIN THREAD
  const combined = concatBytes(tail, bytes)     // <- allocates ~1.06 MiB, copies ~1.06 MiB
  if (combined.length <= tailBytes) { tail = combined; return }
  const flushLength = combined.length - tailBytes
  await writer.write(combined.subarray(0, flushLength))   // flush 64 KiB
  tail = combined.subarray(flushLength)                   // retain last 1 MiB
}
```
with `tailBytes = 1024 * 1024` (`app.js:1591`) and `concatBytes` (`downloadSinks.js:209-217`):

```js
function concatBytes(left, right) {
  if (left.length === 0) return right
  if (right.length === 0) return left
  const combined = new Uint8Array(left.length + right.length)
  combined.set(left, 0)
  combined.set(right, left.length)
  return combined
}
```
In steady state `tail` is the full 1 MiB, so **every 64 KiB chunk allocates a ~1.06 MiB `Uint8Array`
and memcpys ~1.06 MiB** (two `.set()` calls) in order to write 64 KiB. That is **~17 bytes
allocated+copied per byte delivered**, on the renderer main thread, ~210×/s at the measured v1 rate.
`hash-wasm` has **no `Worker` and no `postMessage`/`onmessage` anywhere** in the vendored file — the
SHA-1 is synchronous WASM on the main thread; there is **no `new Worker` anywhere in `web/src`**.
(The 1 MiB tail exists only to make the final SHA-1/size check possible; a ring buffer or a list of
retained slices would cost O(1).)

**Step 7 — StreamSaver writer = copy #3 across an IPC hop.** `downloadSinks.js:104-113` returns
`streamSaver.createWriteStream(...).getWriter()`. `vendor/streamsaver.js:262-288`:

```js
write (chunk) {
  if (!(chunk instanceof Uint8Array)) { throw new TypeError('Can only write Uint8Arrays') }
  ...
  channel.port1.postMessage(chunk)      // no transfer list -> STRUCTURED CLONE = full 64 KiB copy
  bytesWritten += chunk.length
```
The chunk is **structured-cloned** (not transferred) over a `MessageChannel` into
`vendor/streamsaver-sw.js`, which enqueues it into a `ReadableStream` (`streamsaver-sw.js:49-70`) and
serves it as a synthetic download response (`streamsaver-sw.js:74 self.onfetch`). The bytes then
re-enter Chrome's download loader and are written to disk. The `await writer.write(...)` is awaited
**per chunk**, so the flush is serialised with each `postMessage` round trip.

**Step 8 — per-chunk DOM work.** `downloadPipeline.js:48` calls `onStateChange` on every append, which
`app.js:1413` wires to `updateDownloadUI`; `app.js:1642-1675` runs, per chunk:

```js
fileItem.querySelector('.file-progress-fill').style.width = pct + '%'
fileItem.querySelector('.file-progress-text').textContent =
  `${pct}% — ${formatBytes(state.receivedBytes)} of ${formatBytes(file.size)}`
const statusEl = fileItem.querySelector('.file-status')
statusEl.textContent = '↓ ' + formatSpeed(speedBps)
```
Three `querySelector` DOM lookups + two `textContent` writes + a `style.width` write + `Math.log`-based
`formatBytes` string building, per chunk — style/layout invalidation at ~210×/s in the renderer.

### v1 direct per-chunk cost ranking (by construction)

Ranked by bytes and allocations moved, then by per-chunk fixed overhead:

1. **1 MiB validation tail in `createStreamingSink.append`** — ~1.06 MiB allocated + memcpy'd per
   64 KiB flushed (~17× amplification), main thread. Largest *constructed* cost. `downloadSinks.js:22`,
   `:1591`.
2. **StreamSaver `postMessage` structured clone + service-worker hop** — 1 full 64 KiB copy + a
   renderer→SW IPC + re-entry into the download loader, awaited per chunk. `vendor/streamsaver.js:287`.
3. **hash-wasm SHA-1 on the main thread** — 1 pass over 64 KiB, synchronous, no worker.
   `downloadSinks.js:21`, `vendor/hash-wasm.js:1385`.
4. **`decodeBinaryEnvelope` `data.slice(14)`** — 1 full 64 KiB copy per chunk. `binaryEnvelope.js:58`.
5. **Per-chunk fixed overhead** — ~4 serialised promise-chain links + several awaits, 2 timer
   cancel/reschedule pairs, ~6 closures, 3 `querySelector`s + 3 DOM/style writes + 2 string formats per
   chunk. `app.js:461`, `:1005`, `:1481`, `downloadPipeline.js:36`, `app.js:1670-1675`.
6. **Browser-internal DataChannel receive (SCTP/DTLS → `ArrayBuffer`)** — unknown from source. Cannot
   be sized by reading code (see §4).

Note `laneScheduler.js` (324 lines, a write-side scheduler) is referenced **only by its own test file**
— it is not in the production receive path.

---

## 3. Per-chunk receive path, v2 / phase-4a

Per byte, in the page: **nothing.** There is no `onmessage`, no per-chunk handler, no buffer, no copy,
no hash, no writer. The page's only involvement is `<a href="/s/{code}/asset/{id}">`
(`gallery.js:157`); from there:

1. Chrome network service performs the request through the relay/direct origin.
2. `handleAsset` (`handlers.go:347`) sets `Content-Type`, `Content-Disposition: attachment`,
   `Content-Length`, and calls `session.Backend.GetFile(ctx, id, dst)` writing **straight into the
   `http.ResponseWriter`** — no application-layer chunking, no hashing, no JS-side assembly.
3. Chrome's **native download machinery** writes the response body to disk out of process, off the
   page's JavaScript thread.

The only shared property with v1 is the HTTP/TCP transport underneath; nothing in the *client
receive code* is shared, because on the v2 path there is no client receive code to share.

---

## 4. Does v2 do materially less per-byte work? — what this does and does not prove

**Proven by code (traceable):** the two paths are not comparable implementations. v1 pays, per 64 KiB
chunk, a ~1.06 MiB copy (item 1), a 64 KiB structured clone + SW IPC (item 2), a main-thread SHA-1 pass
(item 3), a 64 KiB header-strip copy (item 4), plus ~4 serialised promise hops, 2 timer pairs and 3
DOM writes (item 5). v2 pays **none** of those. So yes: v2 does materially less work per byte — the
degenerate case of "less".

**Proven by field measurement (imported, not derived here):** E10c cell B put a real Chromium on the
relay origin asset URL and Chrome's own download sink wrote 1 445 795 840 B in 48.384 s = **239.054
Mbps** — above the v2 `curl` cells (233.5), with the same client VM and the same relay. E27 established
that the v1 sink is **34 %** of the client's per-byte cost at 1 vCPU / **47.7 %** at 4 vCPU, and that
the remaining receive path is the other 66 % / 52.3 %, with the cost in the **chrome renderer**
(1.95–2.01 cores with the sink on vs 0.66–0.86 sink-free at 4 vCPU; network service flat ~0.4–0.5).
E15/E27 established the sink's work as *"incremental SHA-1 (hash-wasm) and then awaits StreamSaver's
writer after holding a 1 MiB validation tail"* — which is precisely items 1–3 above, so the code read
and the measured 34 % agree on *what* the sink costs.

**What cannot be concluded from code — explicitly:**

- The **browser-internal DataChannel receive cost is not measurable by reading source.** No source is
  available for Chrome's SCTP/DTLS receive path. E10c's 239 Mbps says nothing about it, because that
  arm never opened a DataChannel.
- Therefore **this comparison cannot tell you whether the residual 66 % is dominated by Chrome's
  DataChannel receive or by v1's own `onmessage`/assembly/timer/DOM JavaScript.** The v2 arm removes
  both at once, so it is uninformative on the split. Isolating it needs a measurement this task cannot
  make: e.g. an arm whose `onmessage` only counts `byteLength` and touches nothing else (the lab rig's
  `benchdirect/web/bench.js:41-50 attachChannel` is already exactly that shape, but it is a lab arm,
  not a field arm).
- **No rate in this document is derived from source.** The tail-copy amplification is a *number of
  bytes copied*, not a throughput claim; whether removing it is worth 1.05× or 1.4× at 1 vCPU is an
  empirical question for the existing sink-bypass A/B harness.
- **ASCII: everything above is main-thread discussion.** Because there is no `Worker` in the v1 client,
  SHA-1, copying and DOM work all contend for the same renderer main thread as the DataChannel message
  dispatch — which is consistent with E27's finding that 1 vCPU fails *specifically* while 4 vCPU
  (same code) largely does not.

---

## 5. The single first change I would make

**Stop putting bulk asset bytes on the DataChannel; serve them over plain HTTP — i.e. adopt phase-4a's
`anchor.href = /s/{code}/asset/{id}` + `Content-Disposition: attachment` path for ordinary file
downloads in the client.**

Reasoning:

1. It is the only change in this comparison that is **demonstrated on identical hardware** to reach a
   materially higher rate (239.054 Mbps, E10c), and it does so by deleting the entire per-byte path
   rather than by making part of it cheaper.
2. It deletes **both** measured buckets at once — the sink's 34 %/47.7 % (items 1–4) and whatever share
   of the 66 %/52.3 % receive path is app JS (item 5) — instead of trading a bounded win against the
   unknown browser-internal DataChannel floor.
3. The receiving half is already written, deployed and exercised in `.worktrees/phase-4a`
   (`handleAsset`, `handleArchivePart`, `gallery.js:157`), and E10c cell B already drove it from a real
   browser. This is a port, not a design.
4. It is the only option that also fixes the RTT sensitivity E10c measured: v1 degrades 111 → 60 Mbps
   as RTT goes 12 → 71 ms, while the HTTP path barely moves (233.6 → 221.6). No micro-optimisation of
   the v1 sink shortens that.

Fallback, if the DataChannel must carry bulk for now: the single highest-value **contained** change is
to remove the 1 MiB tail copy in `downloadSinks.js:20-31` (keep the verification tail as a ring buffer
or a retained list of slices instead of re-concatenating it on every append). It is ~15 lines in one
function at the largest constructed per-byte cost, and it is directly A/B-testable with the existing
`?sink=discard`-style isolation used in E15/E27 — but it cannot bridge 110 → 239 and should not be sold
as if it could.

---

## 6. Files read (all read-only)

`.worktrees/e2e-v1/signaling-server/web/src/`: `app.js`, `downloadPipeline.js`, `downloadSinks.js`,
`directChannel.js`, `binaryEnvelope.js`, `downloadCapabilities.js`, `secureRelayChannel.js`, `frame.js`,
`sw.js`, `vendor/streamsaver.js`, `vendor/streamsaver-sw.js`, `vendor/hash-wasm.js`.
`.worktrees/e2e-v1/agent/internal/transfer/manager.go` (chunk size only).
`.worktrees/phase-4a/`: `agent/internal/direct/static/app.js`, `.../gallery.js`,
`agent/internal/direct/handlers.go`, `agent/internal/direct/server.go`,
`e2e/browser/relay-route.spec.js`, `e2e/browser/PHASE-B-SAFARI.md`.
`.worktrees/benchdirect/`: `agent/cmd/benchdirect/web/bench.js`,
`docs/superpowers/spikes/results/2026-09-18-exp10c-v2-arms.md`,
`2026-09-18-exp27-client-cost-split.md`, `2026-09-18-exp15-app-sink-isolation.md`,
`2026-09-18-exp23-cost-attribution.md`.
