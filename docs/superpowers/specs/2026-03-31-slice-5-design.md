# Slice 5 — Download Quality: Speed Display + Checksum Verification

**Date**: 2026-03-31
**Status**: Draft
**Reference**: [MVP Design](2026-03-30-mvp-design.md)

## Goal

Give users confidence that downloaded files arrived intact, and show live transfer speed during downloads.

## Problem

Currently there is no way to know if a file was silently corrupted in transit (e.g. the Slice 3 bug where a global HTTP timeout truncated large files), and no feedback on how fast a download is progressing beyond a progress bar percentage.

## Decisions Summary

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Hash algorithm | SHA-1 | Provided natively by OpenCloud PROPFIND; supported by Web Crypto API (SubtleCrypto); fast enough on mobile |
| Who verifies | Browser | The DataChannel transport is what we're validating — the browser computes SHA-1 of received bytes and compares against the expected value from the agent |
| Who computes expected hash | Agent (from PROPFIND) | Agent fetches `<oc:checksums>` from OpenCloud and passes the SHA-1 value to the browser in `file_header` |
| Protocol change | Extend `file_header` with optional `sha1` field | No new message types; `sha1` is omitted if OpenCloud didn't return a checksum |
| Speed tracking | Browser-side, live rolling average | Browser already receives all chunks; no protocol changes needed; measures actual received bytes/sec |
| Speed display | Live "↓ X MB/s" during transfer, "avg X MB/s" after completion | Most useful feedback; rolling 1-second window for live, total bytes / elapsed for average |
| Hash display | Shown under file size after completion | Full SHA-1 on pass; truncated expected vs actual on fail |
| Missing checksum | Silently omit indicator | Some files may not have checksums in PROPFIND; no indicator shown rather than showing a false result |

## Architecture

### Data flow

```
OpenCloud PROPFIND → agent (SHA-1 in oc:checksums)
                          │
                          ▼
                    file_header {sha1: "..."}
                          │
                    DataChannel
                          │
                          ▼
              browser (accumulates chunks)
                          │
                    chunk_end received
                          │
              crypto.subtle.digest('SHA-1', buffer)
                          │
                    compare → ✓ / ✗
```

### Speed tracking

The browser tracks speed independently of checksums:
- On first binary chunk: record `startTime`, `startBytes = 0`
- On each chunk: record `lastChunkTime`, update `totalBytes`
- Live display: rolling 1-second window — bytes received in the last second, updated on each chunk
- After `chunk_end`: display `totalBytes / (endTime - startTime)` as average

## Protocol Changes

Only `file_header` changes. Gains one optional field:

```json
{
  "type": "file_header",
  "name": "video.mp4",
  "size": 2147483648,
  "mimeType": "video/mp4",
  "sha1": "a7e0206e573edbef0c4d8107a151271fbeccf2fe"
}
```

`sha1` is omitted if OpenCloud's PROPFIND did not return a checksum for that file. All other protocol messages are unchanged.

## Agent Changes

### `agent/internal/opencloud/client.go`

**PROPFIND request**: Add `<oc:checksums>` to the requested properties:

```xml
<D:propfind xmlns:D="DAV:" xmlns:oc="http://owncloud.org/ns">
  <D:prop>
    <D:getcontentlength/>
    <D:getcontenttype/>
    <D:getlastmodified/>
    <oc:checksums/>
  </D:prop>
</D:propfind>
```

**Parsing**: Extract the `oc:checksum` text content (e.g. `"SHA1:a7e020… MD5:f77a74… ADLER32:23026e"`), split on spaces, find the token starting with `"SHA1:"`, strip the prefix.

**`FileInfo` struct**: Add `SHA1 string` field. Empty string means no checksum was returned.

### `agent/internal/transfer/manager.go`

When marshaling `file_header`, include `sha1` if `fileInfo.SHA1` is non-empty:

```go
type fileHeaderMsg struct {
    Type     string `json:"type"`
    Name     string `json:"name"`
    Size     int64  `json:"size"`
    MimeType string `json:"mimeType"`
    SHA1     string `json:"sha1,omitempty"`
}
```

No other changes to the transfer manager.

## Browser Changes

### `signaling-server/web/app.js`

**On `file_header`**: Store `expectedSHA1 = msg.sha1 || null` for the current transfer.

**During binary chunks**: Accumulate chunks as today (no change). Update live speed display:
- Track `chunkTimestamps` (ring buffer of recent chunk arrival times and sizes)
- Update `↓ X MB/s` display using bytes received in the last 1 second

**On `chunk_end`**:
1. Compute average speed: `totalBytes / (Date.now() - transferStartTime)`
2. If `expectedSHA1` is set:
   - Call `crypto.subtle.digest('SHA-1', assembledBuffer)`
   - Convert result to hex string
   - Compare with `expectedSHA1`
   - Update UI: ✓ intact (green) or ✗ corrupted (red)
   - Display full SHA-1 on pass; `expected <first 8 chars>… got <first 8 chars>…` on fail
3. Trigger file download (unchanged)

### `signaling-server/web/index.html`

No structural changes needed — speed and checksum are rendered dynamically into the existing file list rows by `app.js`.

## UI States

| State | Left border | Right label | Below size |
|-------|-------------|-------------|------------|
| Waiting | grey | — | — |
| Downloading | blue | ↓ 8.4 MB/s | X% — Y MB of Z MB |
| Complete, verified | green | ✓ intact | avg 7.9 MB/s · SHA-1: a7e020… |
| Complete, corrupted | red | ✗ corrupted | avg 5.2 MB/s · expected a7e020… got 3fc91b… |
| Complete, no checksum | grey/white | ✓ done | avg 6.1 MB/s |

## Edge Cases

- **No checksum in PROPFIND**: `sha1` omitted from `file_header`; browser skips verification and shows "✓ done" with speed only
- **`crypto.subtle` unavailable**: catch the error, fall back to "✓ done" — verification is best-effort
- **Multiple files**: each file tracks its own `expectedSHA1`, speed, and bytes independently; state is reset on each `file_header`
- **Transfer cancelled mid-way**: no `chunk_end` is received, so no checksum or speed is shown for that file

## What Doesn't Change

- Session codes, agent reconnect, persistence — all unchanged
- WebRTC DataChannel setup — unchanged
- File list display and download trigger — unchanged
- Agent does not compute any hash

## Test Plan

### Unit / integration

- `TestListFiles_ParsesChecksums` — PROPFIND response with `<oc:checksums>` → `FileInfo.SHA1` populated correctly
- `TestListFiles_MissingChecksums` — PROPFIND response without `<oc:checksums>` → `FileInfo.SHA1` is `""`
- `TestFileHeader_IncludesSHA1` — `file_header` JSON includes `sha1` when `FileInfo.SHA1` non-empty
- `TestFileHeader_OmitsSHA1` — `file_header` JSON omits `sha1` when `FileInfo.SHA1` is `""`

### Smoke test

1. Start agent with a share containing at least one file
2. Open browser, request a file download
3. Confirm live speed display updates during transfer
4. Confirm ✓ intact + SHA-1 shown after download completes
5. (Optional) Corrupt a file on disk after PROPFIND but before download — confirm ✗ corrupted shown
