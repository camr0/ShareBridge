# Immich Exact-Range Seek Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make out-of-buffer Immich video seeks request the exact byte offset Chrome asks for, preventing `Content-Range` and response-body mismatches after gallery navigation.

**Architecture:** The Service Worker reports each nonzero seek Range to the page with the current media ID and generation. The page sends exactly one agent seek for that generation using Chrome's offset, while retaining the existing time-derived offset as a short fallback when Chrome reuses a fetch.

**Tech Stack:** Browser JavaScript ES modules, Service Worker Fetch API, Node.js built-in test runner, live ShareBridge relay and Immich agent.

---

### Task 1: Report Chrome's exact Range from the Service Worker

**Files:**
- Modify: `signaling-server/web/src/sw.test.js`
- Modify: `signaling-server/web/src/sw.js`
- Modify: `signaling-server/web/sw.js`

- [ ] **Step 1: Write the failing Service Worker test**

Extend `loadServiceWorker()` with a fake fetch client and captured `waitUntil` promises. Add a test that resets to generation 3, fetches `bytes=22970368-`, and expects:

```js
assert.deepEqual(sw.clientMessages, [{
  type: 'media_range_request',
  mediaId: 'video',
  generation: 3,
  startOffset: 22970368,
}])
```

- [ ] **Step 2: Run the focused test and verify RED**

Run:

```bash
node --test --test-name-pattern="reports Chrome's exact nonzero Range" src/sw.test.js
```

Expected: FAIL because no `media_range_request` message is posted.

- [ ] **Step 3: Implement the minimal Service Worker message**

Add a production message helper that uses `event.clientId`, `event.waitUntil()`, and `self.clients.get()`:

```js
function reportMediaRangeRequest(event, entry, mediaId, startOffset) {
  if (!event.clientId || !event.waitUntil || !self.clients?.get) return
  event.waitUntil(
    self.clients.get(event.clientId).then((client) => {
      client?.postMessage({
        type: 'media_range_request',
        mediaId,
        generation: entry.generation,
        startOffset,
      })
    }).catch(() => {}),
  )
}
```

Call it for parsed nonzero media ranges when `entry.generation > 0`. Copy the tested worker byte-for-byte to `signaling-server/web/sw.js`.

- [ ] **Step 4: Run the focused Service Worker suite and verify GREEN**

Run:

```bash
node --test src/sw.test.js
```

Expected: all Service Worker tests pass, including served-worker synchronization.

### Task 2: Make the exact Range authoritative in the page

**Files:**
- Modify: `signaling-server/web/src/app.test.js`
- Modify: `signaling-server/web/src/app.js`

- [ ] **Step 1: Write failing page tests**

Add tests proving:

```js
__test.handleMediaRangeRequest({
  type: 'media_range_request',
  mediaId: 'video-1',
  generation: 4,
  startOffset: 22970368,
})
```

sends exactly one `asset_preview_seek` at `22970368`, cancels the pending estimated-offset fallback, and ignores duplicate, stale-generation, and wrong-media messages. Add a second test that manually fires the captured fallback timer and verifies the time-derived offset is still sent when no Range message arrives.

- [ ] **Step 2: Run the focused page tests and verify RED**

Run:

```bash
node --test --test-name-pattern="exact Service Worker Range|seek fallback" src/app.test.js
```

Expected: FAIL because `handleMediaRangeRequest` and per-generation send deduplication do not exist.

- [ ] **Step 3: Implement one-send-per-generation seek routing**

Register one Service Worker message handler in all builds. Keep debug telemetry conditional, but route `media_range_request` to a new `handleMediaRangeRequest()` function. Add a shared sender:

```js
function sendVideoSeek(vp, startOffset, generation = vp?.generation) {
  if (!vp || currentVideoPreview !== vp || generation !== vp.generation) return false
  if (!transferChannel || vp.seekSentGeneration === generation) return false
  vp.seekSentGeneration = generation
  vp.seeking = true
  transferChannel.send(JSON.stringify({
    type: 'asset_preview_seek',
    id: vp.id,
    quality: 'video',
    start_offset: startOffset,
    generation,
  }))
  return true
}
```

`createSeekHandler()` must reset `seekSentGeneration`, schedule the existing time-derived offset as a fallback, and let `handleMediaRangeRequest()` cancel that timer and call `sendVideoSeek()` with Chrome's exact offset. Export the new handlers through `__test`.

- [ ] **Step 4: Run focused and full web suites**

Run:

```bash
node --test src/app.test.js src/sw.test.js
npm test
git diff --check
```

Expected: all tests pass, the served Service Worker matches its source, and `git diff --check` prints nothing.

### Task 3: Deploy locally and verify the original failure three times

**Files:**
- Verify: `signaling-server/web/src/app.js`
- Verify: `signaling-server/web/src/sw.js`
- Verify: `signaling-server/web/sw.js`

- [ ] **Step 1: Restart or refresh the local ShareBridge components using the repository's existing workflow**

Confirm the live page is serving the changed application and Service Worker before testing. Keep the existing Immich agent session and share URL.

- [ ] **Step 2: Run live reproduction pass 1**

Use `DSCF3646.MOV` (3:02): play it, navigate forward across `DSCF3645.JPG` and `DSCF3644.JPG`, return to the video, play, then set `currentTime` beyond `buffered.end()` through the writable Browser MCP console. Confirm time resumes, no `<video>` error occurs, and lightGallery does not show “Oops... Failed to load content...”.

- [ ] **Step 3: Run live reproduction passes 2 and 3 from fresh preview sessions**

Close and reopen the preview between passes. Repeat the same two-image round trip and out-of-buffer console seek twice more. Record each pre-seek buffer end, target, post-seek time progression, and agent seek offset/generation.

- [ ] **Step 4: Verify Range/body alignment from network and agent evidence**

For each pass, confirm Chrome's Range start matches the `asset_preview_seek start_offset` logged by the agent. Confirm all three agent ranges complete successfully and no media request ends in the gallery error wrapper.

- [ ] **Step 5: Commit the tested fix**

```bash
git add signaling-server/web/src/app.js signaling-server/web/src/app.test.js \
  signaling-server/web/src/sw.js signaling-server/web/src/sw.test.js signaling-server/web/sw.js
git commit -m "fix exact byte ranges for Immich video seeks"
```
