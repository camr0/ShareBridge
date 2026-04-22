# Slice 16a Large File Streaming Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the browser's full-buffer download flow with a streaming-capable save pipeline that verifies byte count and SHA-1 before surfacing success, while preserving Blob fallback for unsupported browsers and adding a Mobile Safari large-file experimental streaming path.

**Architecture:** Keep the transfer protocol unchanged and move the complexity into three browser-local units: capability detection, save sinks, and a single-download pipeline controller. `app.js` remains the message entrypoint, but delegates download behavior to focused helpers so we can test held-back-tail logic, timeout handling, fallback warnings, Mobile Safari threshold routing, and validated UI states without wiring every case through DOM-heavy integration tests.

**Tech Stack:** Plain browser ES modules, vendored StreamSaver assets, vendored incremental SHA-1 implementation (`hash-wasm` or equivalent browser-ready build), existing `node --test` browser unit tests in `signaling-server/web/src/*.test.js`

---

## File Structure

**New files:**
- `scripts/update_streamsaver_vendor.sh` — refreshes vendored StreamSaver assets from pinned upstream and fork commits without using a git submodule
- `signaling-server/web/src/downloadCapabilities.js` — runtime capability detection and fallback warning copy
- `signaling-server/web/src/downloadCapabilities.test.js` — unit tests for capability classification and warning severity
- `signaling-server/web/src/downloadSinks.js` — streaming sink, Blob sink, and incremental SHA-1 helpers behind one browser-friendly interface
- `signaling-server/web/src/downloadSinks.test.js` — unit tests for sink behavior with fakes
- `signaling-server/web/src/downloadPipeline.js` — single active-download state machine: byte counting, held-back tail, checksum finalize, timeout, connection-close failure
- `signaling-server/web/src/downloadPipeline.test.js` — unit tests for pipeline success and failure cases
- `signaling-server/web/src/vendor/streamsaver.js` — vendored patched StreamSaver browser module
- `signaling-server/web/src/vendor/streamsaver-sw.js` — vendored StreamSaver service worker asset
- `signaling-server/web/src/vendor/streamsaver-mitm.html` — vendored StreamSaver MITM page if required by the chosen upstream build
- `signaling-server/web/src/vendor/streamsaver-safari.js` — vendored Mobile Safari StreamSaver build from the Safari-capable fork
- `signaling-server/web/src/vendor/streamsaver-safari-sw.js` — vendored Mobile Safari service worker asset from the Safari-capable fork
- `signaling-server/web/src/vendor/streamsaver-safari-mitm.html` — vendored Mobile Safari MITM page from the Safari-capable fork
- `signaling-server/web/src/vendor/hash-wasm.js` — vendored browser-ready incremental hash module exposing SHA-1 `init/update/digest`

**Modified files:**
- `signaling-server/web/src/app.js` — replace `fileChunks[]` download flow with the pipeline controller and explicit fallback warning handling
- `signaling-server/web/src/app.test.js` — add light integration coverage for the UI state mapping and active-download cleanup hooks
- `signaling-server/web/index.html` — add fallback warning styling/container for the join page

**No server route changes expected:**
- `signaling-server/cmd/server/main.go` already serves `/src/{path...}` from `./web/src`, so new helper modules and vendored assets should be reachable without Go changes.

## Notes for the implementing engineer

- This client is served as plain source files, not through a bundler. Prefer vendored browser-ready modules under `signaling-server/web/src/` over npm-only packages that assume a build step.
- Keep the StreamSaver browser assets as normal tracked files in ShareBridge. Do not use a git submodule for the Safari fork; refresh the vendored files from pinned commits via `scripts/update_streamsaver_vendor.sh`.
- The current `node --test src/*.test.js` script only picks up root-level tests inside `signaling-server/web/src`. Keep new test files directly in `src/`, even if helper modules remain there too.
- Slice 16a intentionally keeps one active download at a time. Do not design for concurrent transfers here.
- `intact` is the validated success state when SHA-1 is present and matches. `done` is only for successful transfers with no checksum present. Any mismatch must fail.
- Failed streaming downloads may leave a visibly partial file on disk. Treat that as accepted behavior and make sure the UI failure path is explicit.
- Mobile Safari support is file-size sensitive in this slice. Do not cache one global support decision for the whole session; evaluate support per file header so the `< 100 MiB` vs `>= 100 MiB` split stays correct.

## Task 1: Add capability detection and fallback warning helpers

**Files:**
- Create: `signaling-server/web/src/downloadCapabilities.js`
- Create: `signaling-server/web/src/downloadCapabilities.test.js`

- [ ] **Step 1: Write the failing tests for capability classification**

```js
// signaling-server/web/src/downloadCapabilities.test.js
import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  MOBILE_SAFARI_EXPERIMENT_BYTES,
  detectDownloadSupport,
  buildFallbackWarning,
  buildExperimentalWarning,
} from './downloadCapabilities.js'

test('detectDownloadSupport chooses streaming when runtime primitives are present', async () => {
  const result = await detectDownloadSupport({
    hasWritableStream: true,
    hasServiceWorker: true,
    registerServiceWorker: async () => ({ scope: '/src/vendor/' }),
  })

  assert.equal(result.mode, 'streaming')
  assert.equal(result.warning, null)
})

test('detectDownloadSupport chooses experimental streaming for large Mobile Safari downloads', async () => {
  const result = await detectDownloadSupport({
    fileSize: 150 * 1024 * 1024,
    userAgent: 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1',
    hasWritableStream: true,
    hasServiceWorker: true,
    registerServiceWorker: async () => ({ scope: '/src/vendor/' }),
    registerExperimentalServiceWorker: async () => ({ scope: '/src/vendor/' }),
  })

  assert.equal(result.mode, 'experimental-streaming')
  assert.equal(result.warning.level, 'strong')
  assert.match(result.warning.message, /experimental streaming path/i)
})

test('detectDownloadSupport keeps small Mobile Safari downloads on blob fallback', async () => {
  const result = await detectDownloadSupport({
    fileSize: 10 * 1024 * 1024,
    userAgent: 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1',
    hasWritableStream: true,
    hasServiceWorker: true,
    registerServiceWorker: async () => ({ scope: '/src/vendor/' }),
  })

  assert.equal(result.mode, 'blob')
  assert.match(result.reason, /below 100 mib/i)
})

test('detectDownloadSupport falls back when service worker registration fails', async () => {
  const result = await detectDownloadSupport({
    fileSize: 600 * 1024 * 1024,
    hasWritableStream: true,
    hasServiceWorker: true,
    registerServiceWorker: async () => { throw new Error('register failed') },
  })

  assert.equal(result.mode, 'blob')
  assert.match(result.reason, /register failed/i)
})

test('detectDownloadSupport fails large Mobile Safari downloads when experimental init fails', async () => {
  const result = await detectDownloadSupport({
    fileSize: 150 * 1024 * 1024,
    userAgent: 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1',
    hasWritableStream: true,
    hasServiceWorker: true,
    registerExperimentalServiceWorker: async () => {
      throw new Error('register failed')
    },
  })

  assert.equal(result.mode, 'fail')
  assert.match(result.reason, /register failed/i)
  assert.match(result.warning.message, /experimental streaming path/i)
})

test('buildFallbackWarning strengthens copy for large files', () => {
  const small = buildFallbackWarning({ fileSize: 10 * 1024 * 1024, reason: 'unsupported browser' })
  const large = buildFallbackWarning({ fileSize: 600 * 1024 * 1024, reason: 'unsupported browser' })
  const mobile = buildExperimentalWarning()

  assert.match(small.message, /in-memory download path/i)
  assert.match(large.message, /large downloads may fail/i)
  assert.match(mobile.message, /experimental streaming path/i)
})
```

Run: `cd signaling-server/web && node --test src/downloadCapabilities.test.js -v`
Expected: FAIL because `downloadCapabilities.js` does not exist yet.

- [ ] **Step 2: Implement capability detection with runtime-first gating**

```js
// signaling-server/web/src/downloadCapabilities.js
const LARGE_FILE_WARNING_BYTES = 500 * 1024 * 1024
export const MOBILE_SAFARI_EXPERIMENT_BYTES = 100 * 1024 * 1024

export async function detectDownloadSupport({
  fileSize,
  userAgent = typeof navigator !== 'undefined' ? navigator.userAgent : '',
  hasWritableStream = typeof WritableStream !== 'undefined',
  hasServiceWorker = typeof navigator !== 'undefined' && !!navigator.serviceWorker,
  registerServiceWorker,
  registerExperimentalServiceWorker = registerServiceWorker,
}) {
  if (isMobileSafari(userAgent)) {
    if (typeof fileSize === 'number' && fileSize >= MOBILE_SAFARI_EXPERIMENT_BYTES) {
      if (!hasWritableStream || !hasServiceWorker) {
        return failSupport('Experimental iPhone/iPad streaming unavailable', fileSize)
      }
      try {
        await registerExperimentalServiceWorker()
        return {
          mode: 'experimental-streaming',
          reason: null,
          warning: buildExperimentalWarning(),
        }
      } catch (err) {
        return failSupport(`Experimental iPhone/iPad streaming unavailable: ${err.message}`, fileSize)
      }
    }
    return blobFallback('Mobile Safari stays on the in-memory path below 100 MiB', fileSize)
  }

  if (!hasWritableStream) {
    return blobFallback('WritableStream unavailable', fileSize)
  }
  if (!hasServiceWorker) {
    return blobFallback('Service worker unavailable', fileSize)
  }

  try {
    await registerServiceWorker()
    return { mode: 'streaming', warning: null }
  } catch (err) {
    return blobFallback(`Streaming initialization failed: ${err.message}`, fileSize)
  }
}

export function buildFallbackWarning({ fileSize, reason }) {
  const large = typeof fileSize === 'number' && fileSize >= LARGE_FILE_WARNING_BYTES
  return {
    level: large ? 'strong' : 'normal',
    message: large
      ? `This browser is using an in-memory download path (${reason}). Large downloads may fail.`
      : `This browser is using an in-memory download path (${reason}).`,
  }
}

export function buildExperimentalWarning() {
  return {
    level: 'strong',
    message: 'This iPhone/iPad download is using an experimental streaming path to avoid memory limits. It usually works, but may stall or fail and require retrying later or using another device/browser.',
  }
}

function failSupport(reason) {
  return {
    mode: 'fail',
    reason,
    warning: buildExperimentalWarning(),
  }
}

function blobFallback(reason, fileSize) {
  return {
    mode: 'blob',
    reason,
    warning: buildFallbackWarning({ fileSize, reason }),
  }
}

function isMobileSafari(userAgent) {
  return /iP(hone|ad|od)/.test(userAgent) && /Safari/i.test(userAgent) && !/CriOS|FxiOS|EdgiOS/i.test(userAgent)
}
```

- [ ] **Step 3: Run the tests and make them pass**

Run: `cd signaling-server/web && node --test src/downloadCapabilities.test.js -v`
Expected: PASS with 6 passing tests.

- [ ] **Step 4: Commit**

```bash
git add signaling-server/web/src/downloadCapabilities.js signaling-server/web/src/downloadCapabilities.test.js
git commit -m "feat(web): add download capability detection"
```

## Task 2: Vendor streaming assets and build the save-sink layer

**Files:**
- Create: `scripts/update_streamsaver_vendor.sh`
- Create: `signaling-server/web/src/downloadSinks.js`
- Create: `signaling-server/web/src/downloadSinks.test.js`
- Create: `signaling-server/web/src/vendor/streamsaver.js`
- Create: `signaling-server/web/src/vendor/streamsaver-sw.js`
- Create: `signaling-server/web/src/vendor/streamsaver-mitm.html`
- Create: `signaling-server/web/src/vendor/streamsaver-safari.js`
- Create: `signaling-server/web/src/vendor/streamsaver-safari-sw.js`
- Create: `signaling-server/web/src/vendor/streamsaver-safari-mitm.html`
- Create: `signaling-server/web/src/vendor/hash-wasm.js`

- [ ] **Step 1: Add the vendor sync script**

Create a small shell script that refreshes the tracked browser assets from pinned commits instead of using a git submodule.

```bash
# scripts/update_streamsaver_vendor.sh
#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VENDOR_DIR="$ROOT_DIR/signaling-server/web/src/vendor"

UPSTREAM_REPO="${UPSTREAM_REPO:-https://raw.githubusercontent.com/jimmywarting/StreamSaver.js}"
UPSTREAM_SHA="${UPSTREAM_SHA:-REPLACE_UPSTREAM_SHA}"
SAFARI_REPO="${SAFARI_REPO:-https://raw.githubusercontent.com/camr0/StreamSaver.js}"
SAFARI_SHA="${SAFARI_SHA:-REPLACE_SAFARI_SHA}"

mkdir -p "$VENDOR_DIR"

fetch() {
  curl -fsSL "$1/$2/$3" -o "$4"
}

patch_generic_streamsaver() {
  perl -0pi -e 's/\|\| !!global\.safari \|\| !!global\.WebKitPoint//g' "$1"
}

prepend_header() {
  local source_repo="$1"
  local source_sha="$2"
  local source_path="$3"
  local target_path="$4"
  {
    printf '/* Vendored from %s at %s (%s). Updated via scripts/update_streamsaver_vendor.sh. */\n' "$source_repo" "$source_sha" "$source_path"
    cat "$target_path"
  } > "$target_path.tmp"
  mv "$target_path.tmp" "$target_path"
}

fetch "$UPSTREAM_REPO" "$UPSTREAM_SHA" "StreamSaver.js" "$VENDOR_DIR/streamsaver.js"
fetch "$UPSTREAM_REPO" "$UPSTREAM_SHA" "sw.js" "$VENDOR_DIR/streamsaver-sw.js"
fetch "$UPSTREAM_REPO" "$UPSTREAM_SHA" "mitm.html" "$VENDOR_DIR/streamsaver-mitm.html"
fetch "$SAFARI_REPO" "$SAFARI_SHA" "StreamSaver.js" "$VENDOR_DIR/streamsaver-safari.js"
fetch "$SAFARI_REPO" "$SAFARI_SHA" "sw.js" "$VENDOR_DIR/streamsaver-safari-sw.js"
fetch "$SAFARI_REPO" "$SAFARI_SHA" "mitm.html" "$VENDOR_DIR/streamsaver-safari-mitm.html"

patch_generic_streamsaver "$VENDOR_DIR/streamsaver.js"

prepend_header "$UPSTREAM_REPO" "$UPSTREAM_SHA" "StreamSaver.js" "$VENDOR_DIR/streamsaver.js"
prepend_header "$SAFARI_REPO" "$SAFARI_SHA" "StreamSaver.js" "$VENDOR_DIR/streamsaver-safari.js"
```

- [ ] **Step 2: Run the sync script once and verify it populates the vendor directory**

Run: `UPSTREAM_SHA=<pinned-upstream-sha> SAFARI_SHA=<pinned-safari-sha> ./scripts/update_streamsaver_vendor.sh`
Expected: the six StreamSaver vendor files exist under `signaling-server/web/src/vendor/` as normal tracked files with source headers, the generic `streamsaver.js` has the Safari hard-fallback removed, and no git submodule is introduced.

- [ ] **Step 3: Verify the synced vendor assets**

Add the patched third-party files under `signaling-server/web/src/vendor/`:

- upstream StreamSaver browser module
- upstream StreamSaver service worker
- upstream MITM page if required by the selected build
- Safari-capable StreamSaver fork browser module from `camr0/StreamSaver.js`
- Safari-capable service worker and MITM page from the same fork
- browser-ready incremental SHA-1 module

The sync script should already apply the generic Safari patch while refreshing the upstream file:

```js
// signaling-server/web/src/vendor/streamsaver.js
// BEFORE
let useBlobFallback = /constructor/i.test(global.HTMLElement) || !!global.safari || !!global.WebKitPoint

// AFTER
let useBlobFallback = /constructor/i.test(global.HTMLElement)
```

Expected: `git diff -- signaling-server/web/src/vendor/streamsaver.js` shows the Safari-specific fallback removal even after rerunning the sync script, and the Safari-specific vendored files match the chosen `camr0/StreamSaver.js` commit except for source headers.

- [ ] **Step 4: Write failing sink tests with fakes**

```js
// signaling-server/web/src/downloadSinks.test.js
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { createBlobSink, createStreamingSink } from './downloadSinks.js'

test('createStreamingSink writes all but the held-back tail before finalize', async () => {
  const writes = []
  const sink = await createStreamingSink({
    fileName: 'video.mov',
    mimeType: 'video/quicktime',
    tailBytes: 4,
    createWriter: async () => ({
      async write(chunk) { writes.push([...chunk]) },
      async close() {},
      async abort() {},
    }),
    createHasher: async () => ({
      update() {},
      digest() { return 'abc123' },
    }),
  })

  await sink.append(new Uint8Array([1, 2, 3]))
  await sink.append(new Uint8Array([4, 5, 6, 7]))

  assert.deepEqual(writes, [[1, 2, 3]])
  assert.equal(sink.bufferedTailSize(), 4)
})

test('createStreamingSink finalizes as intact when SHA-1 matches', async () => {
  const writes = []
  let closed = false
  const sink = await createStreamingSink({
    fileName: 'video.mov',
    mimeType: 'video/quicktime',
    tailBytes: 4,
    createWriter: async () => ({
      async write(chunk) { writes.push([...chunk]) },
      async close() { closed = true },
      async abort() {},
    }),
    createHasher: async () => ({
      update() {},
      digest() { return 'abc123' },
    }),
  })

  await sink.append(new Uint8Array([1, 2, 3]))
  await sink.append(new Uint8Array([4, 5, 6, 7]))
  const result = await sink.finalize({ expectedSha1: 'abc123', expectedSize: 7, receivedBytes: 7 })

  assert.deepEqual(writes, [[1, 2, 3], [4, 5, 6, 7]])
  assert.equal(closed, true)
  assert.deepEqual(result, { ok: true, code: 'intact', computedSha1: 'abc123' })
})

test('createStreamingSink aborts when SHA-1 validation fails', async () => {
  let abortReason = null
  const sink = await createStreamingSink({
    fileName: 'video.mov',
    mimeType: 'video/quicktime',
    tailBytes: 4,
    createWriter: async () => ({
      async write() {},
      async close() {},
      async abort(reason) { abortReason = reason },
    }),
    createHasher: async () => ({
      update() {},
      digest() { return 'badbad' },
    }),
  })

  await sink.append(new Uint8Array([1, 2, 3, 4]))
  const result = await sink.finalize({ expectedSha1: 'abc123', expectedSize: 4, receivedBytes: 4 })

  assert.equal(abortReason, 'checksum-mismatch')
  assert.deepEqual(result, { ok: false, code: 'checksum-mismatch', computedSha1: 'badbad' })
})

test('createBlobSink does not call subtle.digest when no sha1 is expected', async () => {
  let digestCalls = 0
  const sink = await createBlobSink({
    fileName: 'archive.bin',
    mimeType: 'application/octet-stream',
    subtleDigest: async () => {
      digestCalls += 1
      return new Uint8Array([0xaa]).buffer
    },
    triggerBrowserSave: () => {},
  })

  await sink.append(new Uint8Array([1, 2, 3]))
  await sink.finalize({ expectedSha1: null, expectedSize: 3, receivedBytes: 3 })

  assert.equal(digestCalls, 0)
})

test('createBlobSink fails when SHA-1 validation does not match', async () => {
  let saveCalls = 0
  const sink = await createBlobSink({
    fileName: 'archive.bin',
    mimeType: 'application/octet-stream',
    subtleDigest: async () => new Uint8Array([0xaa]).buffer,
    triggerBrowserSave: () => {
      saveCalls += 1
    },
  })

  await sink.append(new Uint8Array([1, 2, 3]))
  const result = await sink.finalize({ expectedSha1: 'ffffffff', expectedSize: 3, receivedBytes: 3 })

  assert.equal(saveCalls, 0)
  assert.equal(result.ok, false)
  assert.equal(result.code, 'checksum-mismatch')
})

test('createBlobSink fails on size mismatch before saving', async () => {
  let saveCalls = 0
  const sink = await createBlobSink({
    fileName: 'archive.bin',
    mimeType: 'application/octet-stream',
    subtleDigest: async () => new Uint8Array([0xaa]).buffer,
    triggerBrowserSave: () => {
      saveCalls += 1
    },
  })

  await sink.append(new Uint8Array([1, 2, 3]))
  const result = await sink.finalize({ expectedSha1: null, expectedSize: 4, receivedBytes: 3 })

  assert.equal(saveCalls, 0)
  assert.deepEqual(result, { ok: false, code: 'size-mismatch' })
})
```

Run: `cd signaling-server/web && node --test src/downloadSinks.test.js -v`
Expected: FAIL because `downloadSinks.js` does not exist yet.

- [ ] **Step 5: Implement sink helpers behind a shared interface**

```js
// signaling-server/web/src/downloadSinks.js
import streamSaver from './vendor/streamsaver.js'
import safariStreamSaver from './vendor/streamsaver-safari.js'
import { createSHA1 } from './vendor/hash-wasm.js'

streamSaver.mitm = '/src/vendor/streamsaver-mitm.html'
safariStreamSaver.mitm = '/src/vendor/streamsaver-safari-mitm.html'

export async function createStreamingSink({ fileName, mimeType, tailBytes, createWriter, createHasher }) {
  const writer = await createWriter(fileName, mimeType)
  const hasher = await createHasher()
  const tail = []
  let tailSize = 0

  return {
    async append(bytes) {
      hasher.update(bytes)
      tail.push(bytes)
      tailSize += bytes.length

      while (tailSize > tailBytes) {
        const next = tail.shift()
        tailSize -= next.length
        await writer.write(next)
      }
    },
    bufferedTailSize() {
      return tailSize
    },
    async finalize({ expectedSha1, expectedSize, receivedBytes }) {
      const computedSha1 = expectedSha1 ? hasher.digest('hex') : null
      if (receivedBytes !== expectedSize) {
        await writer.abort('size-mismatch')
        return { ok: false, code: 'size-mismatch' }
      }
      if (expectedSha1 && computedSha1 !== expectedSha1.toLowerCase()) {
        await writer.abort('checksum-mismatch')
        return { ok: false, code: 'checksum-mismatch', computedSha1 }
      }
      for (const chunk of tail) {
        await writer.write(chunk)
      }
      await writer.close()
      return { ok: true, code: expectedSha1 ? 'intact' : 'done', computedSha1 }
    },
    async abort(reason) {
      await writer.abort(reason)
    },
  }
}

export async function createBlobSink({ fileName, mimeType, subtleDigest, triggerBrowserSave }) {
  const chunks = []
  return {
    async append(bytes) {
      chunks.push(bytes)
    },
    async finalize({ expectedSha1, expectedSize, receivedBytes }) {
      const combined = combineChunks(chunks)
      if (receivedBytes !== expectedSize) {
        return { ok: false, code: 'size-mismatch' }
      }
      let computedSha1 = null
      if (expectedSha1) {
        computedSha1 = await sha1FromSubtle(subtleDigest, combined)
        if (computedSha1 !== expectedSha1.toLowerCase()) {
          return { ok: false, code: 'checksum-mismatch', computedSha1 }
        }
      }
      await triggerBrowserSave(
        new Blob([combined], { type: mimeType || 'application/octet-stream' }),
        fileName,
      )
      return { ok: true, code: expectedSha1 ? 'intact' : 'done', computedSha1 }
    },
    async abort() {},
  }
}

export async function createBrowserStreamWriter(fileName, mimeType) {
  const fileStream = streamSaver.createWriteStream(fileName, {
    size: undefined,
    mimeType,
    writableStrategy: undefined,
  })
  return fileStream.getWriter()
}

export async function createSafariBrowserStreamWriter(fileName, mimeType) {
  const fileStream = safariStreamSaver.createWriteStream(fileName, {
    size: undefined,
    mimeType,
    writableStrategy: undefined,
  })
  return fileStream.getWriter()
}

export async function createIncrementalSha1() {
  return createSHA1()
}

function combineChunks(chunks) {
  const totalLength = chunks.reduce((sum, chunk) => sum + chunk.length, 0)
  const combined = new Uint8Array(totalLength)
  let offset = 0
  for (const chunk of chunks) {
    combined.set(chunk, offset)
    offset += chunk.length
  }
  return combined
}

async function sha1FromSubtle(subtleDigest, bytes) {
  const hashBuffer = await subtleDigest('SHA-1', bytes.buffer)
  return Array.from(new Uint8Array(hashBuffer))
    .map((b) => b.toString(16).padStart(2, '0'))
    .join('')
}
```

- [ ] **Step 6: Run the sink tests**

Run: `cd signaling-server/web && node --test src/downloadSinks.test.js -v`
Expected: PASS with sink behavior covered by fakes.

- [ ] **Step 7: Commit**

```bash
git add scripts/update_streamsaver_vendor.sh signaling-server/web/src/downloadSinks.js signaling-server/web/src/downloadSinks.test.js signaling-server/web/src/vendor
git commit -m "feat(web): add streaming and blob download sinks"
```

## Task 3: Build the active download pipeline controller

**Files:**
- Create: `signaling-server/web/src/downloadPipeline.js`
- Create: `signaling-server/web/src/downloadPipeline.test.js`

- [ ] **Step 1: Write failing pipeline tests for the required edge cases**

```js
// signaling-server/web/src/downloadPipeline.test.js
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { createDownloadPipeline } from './downloadPipeline.js'

test('small files that fit entirely inside the tail are not written before validation', async () => {
  let flushed = false
  const pipeline = await createDownloadPipeline({
    header: { name: 'tiny.txt', size: 3, mimeType: 'text/plain', sha1: 'a9993e364706816aba3e25717850c26c9cd0d89d' },
    sinkFactory: async () => ({
      append() {},
      finalize: async () => {
        flushed = true
        return { ok: true, code: 'intact' }
      },
      abort() {},
    }),
    now: () => 1000,
    scheduleTimeout: () => 1,
    clearScheduledTimeout: () => {},
  })

  await pipeline.append(new Uint8Array([97, 98, 99]))
  assert.equal(flushed, false)
  const result = await pipeline.complete()

  assert.equal(result.code, 'intact')
  assert.equal(flushed, true)
})

test('checksum mismatch fails closed', async () => {
  const pipeline = await createDownloadPipeline({
    header: { name: 'bad.bin', size: 4, mimeType: 'application/octet-stream', sha1: 'deadbeef' },
    sinkFactory: async () => ({
      append() {},
      finalize: async () => ({ ok: false, code: 'checksum-mismatch', computedSha1: 'cafebabe' }),
      abort() {},
    }),
    now: () => 1000,
    scheduleTimeout: () => 1,
    clearScheduledTimeout: () => {},
  })

  await pipeline.append(new Uint8Array([1, 2, 3, 4]))
  const result = await pipeline.complete()

  assert.equal(result.statusClass, 'corrupted')
})

test('missing checksum still resolves as done when byte count matches', async () => {
  const pipeline = await createDownloadPipeline({
    header: { name: 'plain.bin', size: 2, mimeType: 'application/octet-stream', sha1: '' },
    sinkFactory: async () => ({
      append() {},
      finalize: async () => ({ ok: true, code: 'done', computedSha1: null }),
      abort() {},
    }),
    now: () => 1000,
    scheduleTimeout: () => 1,
    clearScheduledTimeout: () => {},
  })

  await pipeline.append(new Uint8Array([9, 9]))
  const result = await pipeline.complete()

  assert.equal(result.statusText, '✓ done')
})

test('stalled transfers fail after 60 seconds without a new chunk', async () => {
  let timeoutMs = 0
  let timeoutFn = null
  let terminal = null
  const pipeline = await createDownloadPipeline({
    header: { name: 'movie.mp4', size: 8, mimeType: 'video/mp4', sha1: '' },
    sinkFactory: async () => ({ append() {}, finalize() {}, abort() {} }),
    now: () => 1000,
    scheduleTimeout: (fn, ms) => {
      timeoutMs = ms
      timeoutFn = fn
      return fn
    },
    clearScheduledTimeout: () => {},
    onTerminalState: (result) => {
      terminal = result
    },
  })

  assert.equal(timeoutMs, 60_000)
  await timeoutFn()
  assert.equal(terminal.code, 'timeout')
})
```

Run: `cd signaling-server/web && node --test src/downloadPipeline.test.js -v`
Expected: FAIL because `downloadPipeline.js` does not exist yet.

- [ ] **Step 2: Implement the pipeline controller**

```js
// signaling-server/web/src/downloadPipeline.js
const STALL_TIMEOUT_MS = 60_000

export async function createDownloadPipeline({
  header,
  sinkFactory,
  now = () => Date.now(),
  scheduleTimeout = (fn, ms) => setTimeout(fn, ms),
  clearScheduledTimeout = (id) => clearTimeout(id),
  onStateChange = () => {},
  onTerminalState = () => {},
}) {
  const sink = await sinkFactory(header)
  let receivedBytes = 0
  let receivedChunkCount = 0
  let timeoutId = null
  let finished = false
  const startedAt = now()

  const bumpTimeout = () => {
    if (timeoutId) clearScheduledTimeout(timeoutId)
    timeoutId = scheduleTimeout(() => {
      void fail('timeout', 'Transfer stalled before completion')
    }, STALL_TIMEOUT_MS)
  }

  bumpTimeout()
  onStateChange({ phase: 'downloading', receivedBytes, expectedBytes: header.size })

  return {
    async append(bytes) {
      if (finished) return
      receivedBytes += bytes.length
      receivedChunkCount += 1
      await sink.append(bytes)
      bumpTimeout()
      onStateChange({ phase: 'downloading', receivedBytes, expectedBytes: header.size, receivedChunkCount })
    },
    async complete() {
      if (finished) return { ok: false, code: 'already-finished' }
      finished = true
      if (timeoutId) clearScheduledTimeout(timeoutId)
      onStateChange({ phase: 'verifying', receivedBytes, expectedBytes: header.size })
      const result = await sink.finalize({
        expectedSha1: header.sha1 || null,
        expectedSize: header.size,
        receivedBytes,
      })
      const terminal = mapFinalizeResult({ header, result, receivedBytes, startedAt, finishedAt: now() })
      onTerminalState(terminal)
      return terminal
    },
    async failForDisconnect() {
      return fail('disconnected', 'Connection closed before completion')
    },
  }

  async function fail(code, message) {
    if (finished) return { ok: false, code: 'already-finished' }
    finished = true
    if (timeoutId) clearScheduledTimeout(timeoutId)
    await sink.abort(code)
    const terminal = { ok: false, code, statusClass: 'failed', statusText: `✗ ${message}` }
    onTerminalState(terminal)
    return terminal
  }
}

function mapFinalizeResult({ header, result, receivedBytes, startedAt, finishedAt }) {
  const elapsedSeconds = Math.max(1, (finishedAt - startedAt) / 1000)
  const avgBytesPerSecond = receivedBytes / elapsedSeconds
  if (!result.ok) {
    return {
      ok: false,
      code: result.code,
      statusClass: result.code === 'checksum-mismatch' ? 'corrupted' : 'failed',
      statusText: result.code === 'checksum-mismatch' ? '✗ corrupted' : '✗ failed',
      avgBytesPerSecond,
      computedSha1: result.computedSha1 || null,
    }
  }
  return {
    ok: true,
    code: result.code,
    statusClass: result.code === 'intact' ? 'verified' : 'done',
    statusText: result.code === 'intact' ? '✓ intact' : '✓ done',
    avgBytesPerSecond,
    computedSha1: result.computedSha1 || null,
  }
}
```

- [ ] **Step 3: Run the pipeline tests**

Run: `cd signaling-server/web && node --test src/downloadPipeline.test.js -v`
Expected: PASS with the small-file, checksum, no-checksum, and timeout cases covered.

- [ ] **Step 4: Commit**

```bash
git add signaling-server/web/src/downloadPipeline.js signaling-server/web/src/downloadPipeline.test.js
git commit -m "feat(web): add download pipeline controller"
```

## Task 4: Integrate the pipeline into `app.js` and surface warning/status UI

**Files:**
- Modify: `signaling-server/web/src/app.js`
- Modify: `signaling-server/web/src/app.test.js`
- Modify: `signaling-server/web/index.html`

- [ ] **Step 1: Add the join-page warning container and styles**

```html
<!-- signaling-server/web/index.html -->
<style>
  .download-warning {
    margin: 12px 0;
    padding: 10px 12px;
    border-radius: 8px;
    font-size: 0.9rem;
    background: rgba(245, 158, 11, 0.14);
    border: 1px solid rgba(245, 158, 11, 0.35);
    color: #f9d18b;
  }
  .download-warning.hidden {
    display: none;
  }
  .download-warning-strong {
    background: rgba(243, 139, 168, 0.14);
    border-color: rgba(243, 139, 168, 0.35);
    color: #f5b3c7;
  }
</style>

<div id="download-warning" class="download-warning hidden" role="status" aria-live="polite"></div>
```

- [ ] **Step 2: Replace the in-memory download flow in `app.js`**

```js
// signaling-server/web/src/app.js
import { detectDownloadSupport } from './downloadCapabilities.js'
import {
  createBlobSink,
  createBrowserStreamWriter,
  createIncrementalSha1,
  createSafariBrowserStreamWriter,
  createStreamingSink,
} from './downloadSinks.js'
import { createDownloadPipeline } from './downloadPipeline.js'

let activeDownload = null

async function getDownloadSupport(header) {
  return detectDownloadSupport({
    fileSize: header.size,
    userAgent: navigator.userAgent,
    registerServiceWorker: () => navigator.serviceWorker.register('/src/vendor/streamsaver-sw.js'),
    registerExperimentalServiceWorker: () => navigator.serviceWorker.register('/src/vendor/streamsaver-safari-sw.js'),
  })
}

async function startDownload(header) {
  const support = await getDownloadSupport(header)
  renderDownloadWarning(support.warning)

  if (support.mode === 'fail') {
    finalizeDownloadUI(header, {
      ok: false,
      code: 'unsupported',
      statusClass: 'failed',
      statusText: '✗ experimental download unavailable',
      avgBytesPerSecond: 0,
      computedSha1: null,
    })
    return
  }

  activeDownload = await createDownloadPipeline({
    header,
    sinkFactory: async () => {
      if (support.mode === 'experimental-streaming') {
        return createStreamingSink({
          fileName: header.name,
          mimeType: header.mimeType,
          tailBytes: 1024 * 1024,
          createWriter: createSafariBrowserStreamWriter,
          createHasher: createIncrementalSha1,
        })
      }
      if (support.mode === 'streaming') {
        return createStreamingSink({
          fileName: header.name,
          mimeType: header.mimeType,
          tailBytes: 1024 * 1024,
          createWriter: createBrowserStreamWriter,
          createHasher: createIncrementalSha1,
        })
      }
      return createBlobSink({
        fileName: header.name,
        mimeType: header.mimeType,
        subtleDigest: (algorithm, bytes) => crypto.subtle.digest(algorithm, bytes),
        triggerBrowserSave: saveBlobToDisk,
      })
    },
    onStateChange: (state) => updateDownloadUI(header, state),
    onTerminalState: (result) => {
      finalizeDownloadUI(header, result)
      activeDownload = null
    },
  })
}

async function appendChunk(bytes) {
  if (!activeDownload) return
  await activeDownload.append(bytes)
}

async function completeDownload() {
  if (!activeDownload) return
  await activeDownload.complete()
}

function renderDownloadWarning(warning) {
  const el = document.getElementById('download-warning')
  if (!el) return
  if (!warning) {
    el.textContent = ''
    el.className = 'download-warning hidden'
    return
  }
  el.textContent = warning.message
  el.className = 'download-warning' + (warning.level === 'strong' ? ' download-warning-strong' : '')
}

function saveBlobToDisk(blob, fileName = currentFile?.name || 'download') {
  const objectUrl = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = objectUrl
  a.download = fileName
  document.body.appendChild(a)
  a.click()
  document.body.removeChild(a)
  URL.revokeObjectURL(objectUrl)
}

function updateDownloadUI(file, state) {
  const fileItem = getFileItem(file.name)
  if (!fileItem) return
  if (state.phase === 'verifying') {
    fileItem.querySelector('.file-status').textContent = '… verifying'
    fileItem.querySelector('.file-status').className = 'file-status speed'
    return
  }
  if (state.phase === 'downloading') {
    const pct = file.size > 0 ? Math.round((state.receivedBytes / file.size) * 100) : 0
    fileItem.querySelector('.file-progress-fill').style.width = pct + '%'
    fileItem.querySelector('.file-progress-text').textContent = `${pct}% — ${formatBytes(state.receivedBytes)} of ${formatBytes(file.size)}`
  }
}

function finalizeDownloadUI(file, result) {
  const fileItem = getFileItem(file.name)
  applyFinalDownloadState(fileItem, file, result)
}

function applyFinalDownloadState(fileItem, file, result) {
  if (!fileItem) return
  fileItem.classList.remove('downloading')
  fileItem.classList.add(result.statusClass)
  fileItem.querySelector('.file-progress').classList.add('hidden')
  fileItem.querySelector('.file-status').textContent = result.statusText
  fileItem.querySelector('.file-status').className = 'file-status ' + (result.ok ? 'ok' : 'fail')
  fileItem.querySelector('.file-size').textContent = formatBytes(file.size) + ' · avg ' + formatSpeed(result.avgBytesPerSecond || 0)
  if (file.sha1 && result.ok) {
    fileItem.querySelector('.file-hash').textContent = 'SHA-1: ' + file.sha1.toLowerCase()
  } else if (result.computedSha1 && file.sha1) {
    fileItem.querySelector('.file-hash').textContent = 'expected ' + file.sha1.slice(0, 8) + '… got ' + result.computedSha1.slice(0, 8) + '…'
  }
}
```

- [ ] **Step 3: Handle disconnect/error cleanup and add focused app tests**

```js
// signaling-server/web/src/app.test.js
function fakeFileItem() {
  const parts = {
    status: { textContent: '', className: '' },
    size: { textContent: '' },
    hash: { textContent: '' },
    progress: { classList: { add() {} } },
  }
  return {
    classList: {
      added: [],
      removed: [],
      add(name) { this.added.push(name) },
      remove(...names) { this.removed.push(...names) },
    },
    querySelector(selector) {
      if (selector === '.file-status') return parts.status
      if (selector === '.file-size') return parts.size
      if (selector === '.file-hash') return parts.hash
      if (selector === '.file-progress') return parts.progress
      throw new Error('unexpected selector: ' + selector)
    },
  }
}

test('applyFinalDownloadState maps checksum-backed success to intact', () => {
  const fileItem = fakeFileItem()
  __test.applyFinalDownloadState(fileItem, {
    name: 'report.pdf',
    size: 1024,
    sha1: 'abc123',
  }, {
    ok: true,
    code: 'intact',
    statusClass: 'verified',
    statusText: '✓ intact',
    avgBytesPerSecond: 2048,
  })

  assert.equal(fileItem.querySelector('.file-status').textContent, '✓ intact')
  assert.equal(fileItem.classList.added.at(-1), 'verified')
})

test('handleTransferClosure fails an active download before chunk_end', async () => {
  let failed = false
  __test.setActiveDownload({
    failForDisconnect: async () => {
      failed = true
      return { ok: false, code: 'disconnected', statusText: '✗ Connection closed before completion' }
    },
  })

  await __test.handleTransferClosure()
  assert.equal(failed, true)
})
```

Also extend the existing `__test` export in `signaling-server/web/src/app.js`:

```js
export const __test = {
  setQuotaState({ exceeded, periodEnd }) {
    relayQuotaExceeded = exceeded
    quotaPeriodEnd = periodEnd
  },
  setActiveDownload(download) {
    activeDownload = download
  },
  handleTransferClosure() {
    return activeDownload?.failForDisconnect?.()
  },
  applyFinalDownloadState,
}
```

Run: `cd signaling-server/web && node --test src/app.test.js src/downloadCapabilities.test.js src/downloadSinks.test.js src/downloadPipeline.test.js -v`
Expected: PASS with the new integration points covered.

- [ ] **Step 4: Commit**

```bash
git add signaling-server/web/index.html signaling-server/web/src/app.js signaling-server/web/src/app.test.js
git commit -m "feat(web): integrate streaming download pipeline"
```

## Task 5: Run full verification and capture manual checks

**Files:**
- Modify: `docs/superpowers/specs/2026-04-21-slice-16a-large-file-streaming-design.md` (only if implementation revealed a real spec correction)

- [ ] **Step 1: Run the automated web test suite**

Run: `cd signaling-server/web && npm test`
Expected: PASS with all existing and new `node --test` cases green.

- [ ] **Step 2: Do focused browser verification on one streaming-capable browser and one fallback browser**

Manual checklist:

- Chrome or Firefox desktop:
  - request a large file
  - confirm no full-buffer pause at `chunk_end`
  - confirm success shows `✓ intact` when SHA-1 exists
- Safari macOS 16.6+:
  - request a large file
  - confirm the patched desktop Safari StreamSaver path works without the Blob warning
  - confirm success still shows `✓ intact` when SHA-1 exists
- Safari iOS/iPadOS below 100 MiB:
  - request a small file
  - confirm the in-memory warning banner appears before completion
  - confirm success shows `✓ done` when no checksum exists
- Safari iOS/iPadOS at or above 100 MiB:
  - request a large file
  - confirm the experimental warning appears before streaming begins
  - confirm the Safari-specific StreamSaver assets are used
  - if the transfer fails, confirm the UI ends in `✗ failed` without retrying through Blob
- Corruption / truncation simulation:
  - force a size mismatch or bad checksum
  - confirm the file row ends in `✗ failed` or `✗ corrupted`
  - confirm the UI does not claim success

Expected: streaming path feels continuous, fallback path is explicit, the Mobile Safari threshold policy behaves as designed, and mismatches fail closed.

- [ ] **Step 3: If the implementation changed the chosen design, update the spec**

Only edit the spec if implementation surfaced a real mismatch such as:

- StreamSaver requiring a different vendored asset layout than planned
- a proven runtime capability check that is more reliable than the original wording
- a timeout or UI-state decision that had to change for correctness

Otherwise, leave the spec unchanged.

- [ ] **Step 4: Commit the final implementation or doc adjustment**

```bash
git add signaling-server/web docs/superpowers/specs/2026-04-21-slice-16a-large-file-streaming-design.md
git commit -m "test(web): verify slice 16a streaming downloads"
```
