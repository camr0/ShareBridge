# Slice 16a - Large File Streaming Design

**Date:** 2026-04-21  
**Status:** Draft

## Problem

The current recipient download path in `signaling-server/web/src/app.js` accumulates every binary chunk in memory, assembles one large `Uint8Array`, triggers a Blob download, and only then computes SHA-1 with `crypto.subtle.digest()`.

That has three problems:

- large files can exhaust browser memory, especially on Firefox, Safari, and mobile browsers
- the user can be handed a file before integrity verification completes
- the implementation couples download completion to full-buffer assembly instead of streaming

ShareBridge already has the right transfer protocol shape for a simpler fix:

- `file_header` provides `size`, `mimeType`, and optional `sha1`
- binary chunk messages carry the file payload
- `chunk_end` marks sender completion

For Slice 16a, the bottleneck is the browser save pipeline, not the transport protocol.

## Goals

- Stream downloads to disk on capable browsers instead of buffering the full file in memory
- Preserve the existing `file_header` -> binary chunks -> `chunk_end` transfer protocol
- Prevent "complete" UI states until byte-count validation and SHA-1 validation pass when SHA-1 is available
- Keep compatibility on non-streaming browsers via the existing Blob-style path
- Make the fallback path explicit to the user when it is memory-backed and therefore more failure-prone for large files
- Add an experimental large-file streaming path for Mobile Safari when in-memory Blob downloads are more likely to fail than help
- Keep direct mode and relay mode identical above the transport boundary

## Non-Goals

- Parallel downloads, cancel controls, or downloads-tray redesign
- Resumable downloads or partial-file persistence
- Changing the Go transfer manager's message shapes
- Adding protocol metadata to `chunk_end` unless implementation reveals a concrete need
- Solving background-download limitations on mobile browsers

## Current State

Today the browser download flow is:

1. Receive `file_header`
2. Push each binary chunk into `fileChunks[]`
3. On `chunk_end`, concatenate the full file into one `Uint8Array`
4. Trigger a Blob download immediately
5. Compute SHA-1 afterward with `crypto.subtle.digest()`
6. Update the UI to `intact` or `corrupted`

This means the peak memory cost is effectively the whole file size plus intermediate copies, and integrity is verified too late to guard the save itself.

## Design Summary

Slice 16a introduces two browser-side save paths selected up front through capability detection.

### Streaming Path

On browsers that support StreamSaver-style streaming downloads:

- register the StreamSaver service worker
- create a streaming file writer for the destination file
- compute SHA-1 incrementally as chunks arrive using an incremental hash library such as `hash-wasm`
- write chunks to disk as they arrive, except for a small held-back tail buffer
- when `chunk_end` arrives:
  - verify `receivedBytes === file_header.size`
  - finalize the incremental SHA-1 if `file_header.sha1` is present
  - if validation succeeds, write the held-back tail and close the writer
  - if validation fails, abort the writer and mark the download as failed

### Experimental Mobile Safari Path

On Mobile Safari, Slice 16a should use a size-based policy instead of pretending that one path fits all downloads:

- downloads smaller than 100 MiB stay on the Blob-style in-memory path
- downloads at or above 100 MiB may use an experimental StreamSaver-based path built from the Safari-capable fork
- the page should show a strong warning before the large-download streaming path starts
- once the user has tapped download, the experimental path should start automatically without an extra confirmation step
- if that experimental streaming path fails, the transfer should fail outright rather than silently falling back to Blob for a large iOS download that is likely to run out of memory anyway

This is intentionally a product-policy exception, not a claim that Mobile Safari is fully solved.

### Fallback Path

On browsers that cannot use the streaming path:

- keep the existing Blob-style in-memory accumulation path
- show a visible warning before download starts that this browser is using memory-backed download mode
- strengthen that warning when file size is known and large
- continue to validate byte count and SHA-1 after the in-memory buffer is assembled

This keeps compatibility without pretending that all browsers have the same reliability profile.

## Browser Capability Model

The browser should classify the download path before the first chunk is written.

Implementation should prefer runtime feature detection and initialization success over browser-sniffing. The practical checks are:

- can the StreamSaver service worker register successfully
- are the required stream primitives available
- can the appropriate streaming writer actually be initialized for this browser bucket

The browser/version groupings below are the intended Slice 16a policy:

- **Streaming-capable desktop browsers**
  - Chrome, Edge, Brave
  - Firefox desktop
  - Safari macOS 16.6+ after the StreamSaver Safari fallback patch
- **Experimental large-file Mobile Safari**
  - Safari iOS/iPadOS on Mobile Safari
  - files below 100 MiB stay on Blob fallback
  - files at or above 100 MiB use the experimental streaming path with a strong warning
- **Non-streaming fallback browsers**
  - Safari macOS below 16.6
  - any browser where the StreamSaver path cannot be initialized successfully
- **Uncertain/mobile browsers**
  - browsers that appear compatible but are known to have memory or lifecycle limits
  - these may still use the fallback path, but the UI should make the risk visible

Slice 16a should still keep browser-specific exceptions narrow and explicit:

- the vendored general StreamSaver build should remove the hardcoded Safari Blob-fallback check so Safari macOS 16.6+ can attempt normal streaming
- the vendored Mobile Safari experiment should use the Safari-capable forked StreamSaver build rather than the generic build
- the Mobile Safari threshold rule is the one intentional place where UA bucketing is acceptable, because the behavior is a product-policy decision tied to known iOS memory constraints, not just a support-detection shortcut
- the Safari fork should be consumed as vendored static assets in the ShareBridge repo, refreshed from a pinned fork commit by a small sync script rather than a git submodule

## Integrity Model

Slice 16a keeps protocol validation simple:

- `file_header.size` is the expected byte count
- local `receivedBytes` is the actual byte count
- `file_header.sha1` is the expected checksum when available
- `chunk_end` remains the sender-complete signal

### Byte Count Validation

Every completed download must validate:

```text
receivedBytes === file_header.size
```

If the counts do not match, the browser must treat the transfer as failed even if `chunk_end` arrived.

### SHA-1 Validation

When `file_header.sha1` is present:

- the streaming path uses an incremental hasher
- the Blob fallback path may keep using `crypto.subtle.digest()` because the whole buffer is already in memory

When `file_header.sha1` is absent:

- byte-count validation is still required
- completion may still be shown as `done`
- the UI should avoid implying that checksum verification ran when no checksum was available

This matches current reality, where SHA-1 is available on OpenCloud and only sometimes available on other sources.

### Why Incremental Hashing

`SubtleCrypto.digest()` does not support streaming input, so it cannot be used for the new streaming path without reintroducing full-file buffering. The streaming path therefore needs an incremental hashing implementation such as `hash-wasm` or an equivalent library with `init/update/digest` semantics.

Slice 16a only needs SHA-1 because that is the checksum the current protocol already carries. The browser-side hasher wrapper may be written so a future slice can swap algorithms without rewriting the pipeline, but this slice only guarantees SHA-1 behavior end to end.

## Streaming Save Pipeline

The streaming pipeline should work like this:

1. Receive `file_header`
2. Detect that streaming is available
3. Initialize the streaming writer and incremental hasher
4. For each chunk:
   - increment `receivedBytes`
   - update the hasher
   - write all but the held-back tail buffer to the writer
   - update progress UI
5. On `chunk_end`:
   - confirm byte count
   - finalize SHA-1 if expected
   - if validation passes:
     - flush the held-back tail
     - close the writer
     - mark success
   - if validation fails:
     - abort the writer
     - mark failure

The held-back tail does not need to be large. It only needs to be large enough that a failed validation never leaves the user with a convincingly complete file on disk, while still being negligible RAM cost compared with the full file. Roughly 1 MiB is a reasonable starting point for implementation planning.

If `file_header.size` is less than or equal to the held-back tail size, the whole file may remain in memory until `chunk_end`. That is acceptable and should stay on the streaming code path rather than switching storage strategies mid-transfer. In that case, no bytes are written to disk until validation passes, which is actually the safest outcome for small files.

## Warning UX

If the browser is forced onto the Blob path, the page should say so before download starts.

The warning should communicate:

- this browser is using an in-memory download path
- large downloads may fail
- desktop Chrome, Firefox, Edge, and Safari 16.6+ use the safer streaming path
- large iPhone/iPad downloads may use an experimental streaming path instead of the memory-backed one

When file size is known, the message can be more specific, for example:

- normal fallback warning for small files
- stronger warning for large files such as 500 MiB or larger

For Mobile Safari files at or above 100 MiB, the page should show a different strong warning that communicates:

- this download is using an experimental streaming path on iPhone/iPad
- the path is meant to avoid iOS memory limits on large files
- it usually works, but may still stall or fail and require retrying later or using another device/browser
- the transfer will start automatically because the user already chose to download the file

The goal is honesty, not gating. Slice 16a keeps the fallback path available rather than refusing the download.

## UI States

Slice 16a keeps the current single-download interaction model, but adds clearer state transitions.

Suggested user-visible states:

- `downloading`
- `verifying`
- `intact`
- `done`
- `failed`
- `corrupted`
- `memory-backed download` warning on fallback browsers
- `experimental iPhone/iPad download` warning on large Mobile Safari transfers

The UI should not mark a transfer as complete until validation is done. That is the main semantic change from the current implementation. `intact` means byte-count validation passed and checksum validation passed when a checksum was provided. If no checksum was available, the transfer may still complete as `done` under the weaker byte-count-only validation model already used by current non-OpenCloud sources.

## Failure Handling

The browser must fail closed on the streaming path.

### Streaming Path Failures

Abort the writer and show a failed state when any of the following occur:

- the streaming writer cannot be initialized
- service worker registration fails and no streaming writer can be created
- no chunk activity arrives for 60 seconds after the most recent received chunk and before `chunk_end`
- the peer connection or data channel closes before `chunk_end`
- byte count does not match `file_header.size`
- SHA-1 does not match `file_header.sha1`
- the writer reports an error while writing or closing

If streaming initialization fails before any bytes are written, the browser may fall back to the Blob path if that behavior is explicit and predictable. If bytes have already begun streaming, the browser should fail the transfer instead of silently switching storage strategies mid-file.

For the Mobile Safari experimental path, the rule is stricter:

- if the large-file experimental path cannot initialize before the transfer starts, fail the transfer instead of silently downgrading to large in-memory Blob mode
- if the experimental path fails mid-transfer, fail the transfer outright
- do not offer an automatic Blob retry for those large iPhone/iPad downloads

Aborting the streaming writer does not guarantee cleanup of bytes already written to disk. Slice 16a should explicitly accept that a failed streaming download may leave behind a visibly partial file. The implementation should not chase a fake cleanup mechanism that StreamSaver does not provide. The UX should instead make the failure clear so the user understands the leftover file is incomplete and can be deleted manually if desired.

### Fallback Path Failures

The fallback path remains best-effort and memory-backed. Failures should still be surfaced clearly:

- interrupted transfer
- size mismatch
- checksum mismatch
- browser memory limitation or Blob creation failure

## Implementation Shape

This slice should stay mostly browser-local.

Expected implementation areas:

- `signaling-server/web/src/app.js`
  - replace the current full-buffer completion flow
  - integrate capability detection and save-path selection
- new browser-side helpers as needed, for example:
  - download capability detection
  - streaming writer wrapper
  - incremental hash wrapper
  - fallback warning helpers
- vendored StreamSaver assets under the web app
  - include the Safari patch in the vendored copy by removing the hardcoded Safari Blob-fallback check from the upstream build
  - vendor the Safari-capable fork as a separate Mobile Safari-specific build so the experimental iPhone/iPad path is isolated from the default desktop/general build
  - refresh both vendored builds from pinned upstream/fork commits via a small repo-local sync script so deployment stays self-contained and the browser assets remain normal tracked files
  - the sync script should also reapply the generic upstream Safari patch when refreshing `streamsaver.js`, so rerunning the script cannot silently restore the hardcoded Blob fallback
  - record the source repo URL and pinned commit SHA in a short header comment or adjacent metadata note when refreshing the vendored files
- service worker asset registration for the streaming path

The Go agent transfer manager should remain unchanged for Slice 16a unless implementation uncovers a concrete gap.

## Testing Strategy

### Automated Tests

Add browser-side tests for:

- capability detection
- streaming-path chunk handling
- held-back-tail behavior
- small-file behavior when the whole file fits inside the held-back tail
- byte-count validation
- checksum validation
- transfer timeout and connection-close failure handling
- fallback warning conditions
- "no SHA-1 available" completion behavior

The existing direct and relay transport tests should continue to pass unchanged because the transfer protocol is not being redesigned.

### Manual Verification

Manually verify:

- Chrome desktop streaming path
- Firefox desktop streaming path
- Safari macOS 16.6+ streaming path with the patched StreamSaver build
- Safari iOS/iPadOS below 100 MiB on the Blob fallback path
- Safari iOS/iPadOS at or above 100 MiB on the experimental streaming path

Manual verification should confirm:

- large files no longer require full-buffer memory on streaming-capable browsers
- fallback browsers show an explicit warning before download
- large iPhone/iPad downloads show the experimental warning before streaming begins
- corrupted or truncated transfers do not land as successful completed files
- failed streaming downloads are clearly presented as failed even if a partial file is left behind on disk
- failed large iPhone/iPad downloads do not silently retry through the Blob path
- direct and relay modes behave the same from the recipient UI's point of view

### Performance Check

Run a light benchmark on representative large-file transfers to confirm:

- memory usage drops substantially on the streaming path
- incremental SHA-1 hashing does not obviously cap normal desktop transfer throughput

This does not need to be a formal benchmark suite in Slice 16a; it is a sanity check before claiming the new path is production-ready.

## Decision Summary

- Keep the existing transfer protocol unchanged
- Add streaming-to-disk on capable browsers
- Use incremental hashing for the streaming path
- Keep Blob fallback for incompatible browsers
- Warn explicitly when the browser is using the memory-backed fallback path
- Add a Mobile Safari threshold policy: Blob below 100 MiB, experimental streaming at or above 100 MiB, fail outright if that experimental path fails
- Defer parallel downloads, resumable downloads, and broader download-manager redesign to later slices
