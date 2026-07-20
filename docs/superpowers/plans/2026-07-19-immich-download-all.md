# Immich Download All Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an Immich-style `Download All` action that streams an album's ordered ZIP parts through ShareBridge's serial bulk lane while media remains usable.

**Architecture:** The Immich client asks `/api/download/info` for the authoritative ordered archive partition, then streams each `/api/download/archive` response without buffering it on the agent. The transfer manager reserves the bulk lane for the complete batch, emits one correlated operation per ZIP, and waits for a browser sink acknowledgement before advancing. The browser reuses the existing download pipeline, acknowledges each finalized ZIP, and renders batch progress beside the album item count.

**Tech Stack:** Go 1.24, Immich shared-link HTTP API, ShareBridge transport-v2 control/bulk lanes, browser ES modules, Node's built-in test runner.

---

### Task 1: Add the Immich album archive client

**Files:**
- Modify: `agent/internal/immich/client.go`
- Test: `agent/internal/immich/client_test.go`

- [ ] **Step 1: Write failing client-contract tests**

Add tests proving that `GetAlbumDownloadInfo` first resolves the shared album, then posts `{"albumId":"album-1"}` to `/api/download/info` with the shared-link key and password, and preserves the returned archive order, asset IDs, sizes, album name, and total size. Add a second test proving `DownloadArchive` posts `{"assetIds":[...],"edited":true}` to `/api/download/archive`, requests `application/octet-stream`, carries the same shared-link parameters, and streams the response into the supplied writer. Cover non-2xx responses without copying an unbounded error body.

- [ ] **Step 2: Run the focused tests and verify red**

Run:

```bash
cd agent
go test ./internal/immich -run 'Test(GetAlbumDownloadInfo|DownloadArchive)' -count=1
```

Expected: FAIL because the archive types and methods do not exist.

- [ ] **Step 3: Implement the API adapter**

Add focused transport-neutral types:

```go
type DownloadArchive struct {
	AssetIDs []string `json:"assetIds"`
	Size     int64    `json:"size"`
}

type AlbumDownload struct {
	AlbumName string
	TotalSize int64             `json:"totalSize"`
	Archives  []DownloadArchive `json:"archives"`
}
```

Factor the existing `/api/shared-links/me` read into a private helper so `ListGallery` and `GetAlbumDownloadInfo` use the same album identity and shared-link authentication. Implement:

```go
func (c *Client) GetAlbumDownloadInfo(ctx context.Context) (AlbumDownload, error)
func (c *Client) DownloadArchive(ctx context.Context, assetIDs []string, w io.Writer) (int64, error)
```

`GetAlbumDownloadInfo` must reject non-album or missing-album shares, POST JSON to `/api/download/info`, leave `archiveSize` unset so Immich uses its configured/default split size, and preserve response order exactly. `DownloadArchive` must set `edited: true`, stream with `io.Copy`, enforce the existing redirect allow-list, and use the existing bounded HTTP-error convention.

- [ ] **Step 4: Run the Immich package tests and verify green**

Run:

```bash
cd agent
go test ./internal/immich -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit the Immich client boundary**

```bash
git add agent/internal/immich/client.go agent/internal/immich/client_test.go
git commit -m "feat: add Immich album archive client"
```

### Task 2: Add an acknowledged album batch to the agent bulk lane

**Files:**
- Modify: `agent/internal/daemon/daemon.go`
- Modify: `agent/internal/transfer/manager.go`
- Test: `agent/internal/daemon/daemon_test.go`
- Test: `agent/internal/transfer/manager_test.go`

- [ ] **Step 1: Write failing adapter and transfer tests**

Extend the daemon adapter test doubles and assert that the Immich archive methods map into transfer-layer types without leaking Immich DTOs. In transfer tests, use two archives and assert this ordered lifecycle:

```text
album_download_request
file_header(part 1, size 0, estimated_size, batch metadata)
bulk chunks(part 1)
chunk_end(part 1)
album_archive_ack(ok=true, part 1)
file_header(part 2, new operation ID)
bulk chunks(part 2)
chunk_end(part 2)
album_archive_ack(ok=true, part 2)
album_download_complete
```

Also prove that media requests can complete between bulk chunks, a second bulk request receives request-scoped `transfer in progress`, a failed/negative/mismatched/timed-out acknowledgement stops the batch, and only a fully acknowledged batch increments `downloads` and calls `OnDownloadComplete` once with the sum of actual ZIP bytes.

- [ ] **Step 2: Run focused transfer tests and verify red**

Run:

```bash
cd agent
go test ./internal/transfer ./internal/daemon -run 'Test.*AlbumDownload' -count=1
```

Expected: FAIL because the album batch protocol is absent.

- [ ] **Step 3: Extend the gallery backend boundary**

Add transfer-layer types and methods:

```go
type AlbumArchive struct {
	AssetIDs      []string
	EstimatedSize int64
}

type AlbumDownload struct {
	AlbumName string
	TotalSize int64
	Archives  []AlbumArchive
}

type GalleryBackend interface {
	// existing methods...
	GetAlbumDownload(ctx context.Context) (AlbumDownload, error)
	StreamAlbumArchive(ctx context.Context, assetIDs []string, w io.Writer) (int64, error)
}
```

Map these methods in `immichTransferAdapter` to the client methods from Task 1.

- [ ] **Step 4: Implement the reserved batch lifecycle**

Teach `HandleMessage` these control messages:

```json
{"type":"album_download_request","request_id":"bulk-1"}
{"type":"album_archive_ack","batch_id":"bulk-1","part_index":1,"operation_id":"42","ok":true}
```

Reserve `bulkTransfer` once for the complete batch. Give every ZIP part a fresh operation ID, use a buffered acknowledgement channel owned by the active batch, and accept only an acknowledgement matching its batch, one-based part index, and current operation. Wait at most 60 seconds after `chunk_end`; on timeout or `ok:false`, release the batch without incrementing the download count.

Emit archive headers shaped as:

```json
{
  "type":"file_header",
  "name":"Summer+1-20260719_214500.zip",
  "size":0,
  "estimated_size":4294967296,
  "mimeType":"application/zip",
  "scope":"bulk",
  "request_id":"bulk-1",
  "operation_id":"42",
  "batch_id":"bulk-1",
  "part_index":1,
  "part_count":2
}
```

Match Immich's naming rule exactly: base `${albumName}.zip`; append `+N` only for multipart downloads; append `-yyyyMMdd_HHmmss` immediately before `.zip`; use one-based part numbers. Stream each response directly into 64 KiB bulk envelopes. After the last positive acknowledgement, increment once, invoke `OnDownloadComplete(totalActualBytes)` once, and send `album_download_complete` containing the batch ID, part count, and actual byte total.

- [ ] **Step 5: Run the agent suites and verify green**

Run:

```bash
cd agent
go test ./internal/immich ./internal/transfer ./internal/daemon -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the agent batch protocol**

```bash
git add agent/internal/daemon/daemon.go agent/internal/daemon/daemon_test.go agent/internal/transfer/manager.go agent/internal/transfer/manager_test.go
git commit -m "feat: stream acknowledged Immich album batches"
```

### Task 3: Support exact completion sizes for streamed ZIPs

**Files:**
- Modify: `signaling-server/web/src/downloadPipeline.js`
- Test: `signaling-server/web/src/downloadPipeline.test.js`

- [ ] **Step 1: Write failing unknown-size tests**

Add a pipeline test with `header.size === 0` that appends bytes and calls `complete({ expectedSize: 3 })`; assert that the sink receives `expectedSize: 3` and succeeds. Add a mismatch case and preserve the existing known-size behavior.

- [ ] **Step 2: Run the focused browser tests and verify red**

Run:

```bash
cd signaling-server/web
node --test src/downloadPipeline.test.js
```

Expected: FAIL because `complete` cannot accept an exact completion size.

- [ ] **Step 3: Implement the minimal unknown-size extension**

Change the pipeline completion API to:

```js
async complete({ expectedSize = header.size } = {}) {
  // existing terminal logic
  return sink.finalize({
    expectedSha1: header.sha1 || null,
    expectedSize,
    receivedBytes,
  })
}
```

Keep strict byte validation: album ZIPs use the agent's correlated decimal `chunk_end.bytes_sent` as the exact final size rather than treating zero as “skip validation.” Never use the archive estimate for final integrity validation.

- [ ] **Step 4: Run the download module tests and verify green**

Run:

```bash
cd signaling-server/web
node --test src/downloadPipeline.test.js src/downloadSinks.test.js
```

Expected: PASS.

- [ ] **Step 5: Commit unknown-size streaming support**

```bash
git add signaling-server/web/src/downloadPipeline.js signaling-server/web/src/downloadPipeline.test.js
git commit -m "feat: validate streamed downloads at completion"
```

### Task 4: Add the Download All UI and browser batch acknowledgements

**Files:**
- Modify: `signaling-server/web/index.html`
- Modify: `signaling-server/web/src/gallery.js`
- Modify: `signaling-server/web/src/gallery.test.js`
- Modify: `signaling-server/web/src/app.js`
- Modify: `signaling-server/web/src/app.test.js`

- [ ] **Step 1: Write failing gallery UI tests**

Assert that `renderGalleryShell` places a labeled `Download All` button immediately after the item count, disables it for an empty album, and escapes album content. Exercise delegated click handling and assert it invokes `onAlbumDownloadRequest` without opening an item preview. Add controller-state tests for idle, `Downloading 1/2`, failed, and complete labels/disabled state.

- [ ] **Step 2: Run the gallery tests and verify red**

Run:

```bash
cd signaling-server/web
node --test src/gallery.test.js
```

Expected: FAIL because the album action and state API do not exist.

- [ ] **Step 3: Implement the header action**

Add `onAlbumDownloadRequest` to `createGalleryController`, render this structure on the right side of `.gallery-header`, and style it responsively in `index.html`:

```html
<div class="gallery-summary">
  <span>12 items</span>
  <button class="gallery-download-all" type="button">Download All</button>
</div>
```

Keep the button visually secondary to media content, preserve the approved placement after the count, and expose `setAlbumDownloadState({ phase, partIndex, partCount })` so `app.js` does not manipulate gallery internals.

- [ ] **Step 4: Write failing browser lifecycle tests**

Add tests proving that clicking `Download All` sends one `album_download_request`, disables the action, and rejects additional file/asset/album bulk requests locally. For a two-part batch, feed a header, chunks, and `chunk_end`; prove the browser calls `pipeline.complete({expectedSize: bytesSent})`, sends a positive `album_archive_ack` only after terminal sink success, accepts the next operation, and waits for `album_download_complete` before showing final success. Prove a sink failure sends `ok:false`, leaves the batch failed, and cannot acknowledge or start later parts. Preserve the existing media/bulk interleaving test during an album batch.

- [ ] **Step 5: Run the app tests and verify red**

Run:

```bash
cd signaling-server/web
node --test src/app.test.js
```

Expected: FAIL because app-level album state and acknowledgement handling are absent.

- [ ] **Step 6: Implement browser batch state and acknowledgement**

Wire the gallery callback to `requestAlbumDownload()`. Track one browser-side batch with `batchId`, `partIndex`, `partCount`, and terminal state; do not introduce the later general-purpose job queue. On `file_header`, retain its batch metadata in the existing bulk operation. On correlated `chunk_end`, pass the parsed `bytes_sent` into pipeline completion. In `onTerminalState`, send exactly one acknowledgement for an album part:

```json
{"type":"album_archive_ack","batch_id":"bulk-1","part_index":1,"operation_id":"42","ok":true}
```

Handle `album_download_complete` by restoring the button and showing success. Route request-scoped errors and disconnects to a failed album state without closing the independent media operation. Keep ordinary single-file behavior unchanged.

When selecting a sink and presenting progress for an album part, use `header.estimated_size ?? header.size`; the estimate is only for capability/UX decisions, while `chunk_end.bytes_sent` remains the integrity boundary.

- [ ] **Step 7: Run the complete web suite and verify green**

Run:

```bash
cd signaling-server/web
npm test
```

Expected: PASS.

- [ ] **Step 8: Commit the browser feature**

```bash
git add signaling-server/web/index.html signaling-server/web/src/gallery.js signaling-server/web/src/gallery.test.js signaling-server/web/src/app.js signaling-server/web/src/app.test.js
git commit -m "feat: add Immich Download All action"
```

### Task 5: Update roadmap and verify the integrated feature

**Files:**
- Modify: `TODO.md`
- Modify: `docs/superpowers/specs/2026-07-18-multilane-transport-design.md`

- [ ] **Step 1: Update the roadmap**

Mark Immich `Download All` complete while retaining the separate future items for the serial browser job queue, parallel bulk downloads, and WebTransport. Add a short implementation note to the multi-lane follow-on section pointing to the delivered protocol names and browser acknowledgement requirement.

- [ ] **Step 2: Run formatting and all automated suites**

Run:

```bash
gofmt -w agent/internal/immich/client.go agent/internal/immich/client_test.go agent/internal/daemon/daemon.go agent/internal/daemon/daemon_test.go agent/internal/transfer/manager.go agent/internal/transfer/manager_test.go
(cd agent && go test ./...)
(cd signaling-server && go test ./...)
(cd signaling-server/web && npm test)
```

Expected: all commands PASS.

- [ ] **Step 3: Perform a live direct/relay acceptance pass**

Using a fresh browser context and the ShareBridge Immich playback workflow, verify:

1. `Download All` appears after the item count.
2. A single-part album produces Immich's timestamped ZIP filename and increments the ShareBridge download counter once.
3. A forced multipart fixture downloads parts sequentially with `+1`, `+2`, ... filenames and still increments once.
4. Starting the album download while an image preview loads succeeds on both lanes.
5. Playing and seeking a video during the ZIP transfer remains usable.
6. Canceling/failing a part stops the batch and does not increment the counter.

Capture browser console plus agent/server logs, and redact share keys and API keys.

- [ ] **Step 4: Commit documentation**

```bash
git add TODO.md docs/superpowers/specs/2026-07-18-multilane-transport-design.md
git commit -m "docs: record Immich album download delivery"
```
