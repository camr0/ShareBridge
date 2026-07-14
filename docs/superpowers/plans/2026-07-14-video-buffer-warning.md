# Video Buffering Warning Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Warn a recipient in the active ShareBridge video lightbox after repeated post-start buffering, without making a claim about the root cause.

**Architecture:** Add a small DOM-independent playback-health helper that owns `playing`, `waiting`, and `stalled` listeners for one video element. The existing video-preview lifecycle in `app.js` creates the helper after lightGallery creates the video element and destroys it during preview cleanup; lightweight rendering functions own the transient banner DOM.

**Tech Stack:** Browser ES modules, Node.js built-in `node:test`, existing lightGallery video lightbox, inline CSS in `signaling-server/web/index.html`.

---

## File Structure

- Create: `signaling-server/web/src/videoBufferWarning.js` — event-window state machine and listener cleanup for one HTMLVideoElement-like target.
- Create: `signaling-server/web/src/videoBufferWarning.test.js` — deterministic fake-video tests for the helper’s warning threshold and cleanup.
- Modify: `signaling-server/web/src/app.js` — create the monitor when the streamed lightbox video exists, render/hide the banner, and dispose it with the existing seek listener.
- Modify: `signaling-server/web/index.html` — CSS for the subtle in-lightbox warning banner.

### Task 1: Add the event-based playback-health helper

**Files:**
- Create: `signaling-server/web/src/videoBufferWarning.js`
- Test: `signaling-server/web/src/videoBufferWarning.test.js`

- [ ] **Step 1: Write the failing tests for startup buffering, repeated post-start buffering, recovery, and cleanup**

```js
// signaling-server/web/src/videoBufferWarning.test.js
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { createVideoBufferWarningMonitor } from './videoBufferWarning.js'

function createFakeVideo() {
  const listeners = new Map()
  return {
    addEventListener(type, listener) {
      listeners.set(type, listener)
    },
    removeEventListener(type, listener) {
      if (listeners.get(type) === listener) listeners.delete(type)
    },
    emit(type) {
      listeners.get(type)?.()
    },
    listenerCount() {
      return listeners.size
    },
  }
}

test('ignores startup buffering before playback begins', () => {
  const video = createFakeVideo()
  let warnings = 0
  const monitor = createVideoBufferWarningMonitor({
    video,
    showWarning: () => { warnings += 1 },
    hideWarning: () => {},
    now: () => 0,
  })
  video.emit('waiting')
  video.emit('stalled')
  assert.equal(warnings, 0)
  monitor.destroy()
})

test('warns once after two post-start buffer events within twenty seconds', () => {
  const video = createFakeVideo()
  let now = 0
  let warnings = 0
  const monitor = createVideoBufferWarningMonitor({
    video,
    showWarning: () => { warnings += 1 },
    hideWarning: () => {},
    now: () => now,
  })
  video.emit('playing')
  video.emit('waiting')
  now = 19_000
  video.emit('stalled')
  video.emit('waiting')
  assert.equal(warnings, 1)
  monitor.destroy()
})

test('does not combine buffer events outside the rolling window', () => {
  const video = createFakeVideo()
  let now = 0
  let warnings = 0
  const monitor = createVideoBufferWarningMonitor({
    video,
    showWarning: () => { warnings += 1 },
    hideWarning: () => {},
    now: () => now,
  })
  video.emit('playing')
  video.emit('waiting')
  now = 20_001
  video.emit('stalled')
  assert.equal(warnings, 0)
  monitor.destroy()
})

test('playing hides a shown warning without allowing another warning', () => {
  const video = createFakeVideo()
  let now = 0
  let warnings = 0
  let hides = 0
  const monitor = createVideoBufferWarningMonitor({
    video,
    showWarning: () => { warnings += 1 },
    hideWarning: () => { hides += 1 },
    now: () => now,
  })
  video.emit('playing')
  video.emit('waiting')
  now = 1
  video.emit('stalled')
  video.emit('playing')
  now = 2
  video.emit('waiting')
  assert.deepEqual({ warnings, hides }, { warnings: 1, hides: 1 })
  monitor.destroy()
})

test('destroy detaches listeners and prevents later UI changes', () => {
  const video = createFakeVideo()
  let warnings = 0
  const monitor = createVideoBufferWarningMonitor({
    video,
    showWarning: () => { warnings += 1 },
    hideWarning: () => {},
    now: () => 0,
  })
  monitor.destroy()
  video.emit('playing')
  video.emit('waiting')
  video.emit('stalled')
  assert.deepEqual({ warnings, listeners: video.listenerCount() }, { warnings: 0, listeners: 0 })
})
```

- [ ] **Step 2: Run the helper test and verify it fails because the module does not exist**

Run: `node --test signaling-server/web/src/videoBufferWarning.test.js`

Expected: failure resolving `./videoBufferWarning.js`.

- [ ] **Step 3: Implement the minimal event monitor**

```js
// signaling-server/web/src/videoBufferWarning.js
const BUFFER_WINDOW_MS = 20_000
const BUFFER_EVENT_THRESHOLD = 2

export function createVideoBufferWarningMonitor({
  video,
  showWarning,
  hideWarning,
  now = Date.now,
}) {
  let playbackStarted = false
  let warningShown = false
  let bufferEvents = []

  const onPlaying = () => {
    playbackStarted = true
    if (warningShown) hideWarning()
  }

  const onBuffer = () => {
    if (!playbackStarted || warningShown) return
    const timestamp = now()
    bufferEvents = bufferEvents.filter((time) => timestamp - time <= BUFFER_WINDOW_MS)
    bufferEvents.push(timestamp)
    if (bufferEvents.length >= BUFFER_EVENT_THRESHOLD) {
      warningShown = true
      showWarning()
    }
  }

  video.addEventListener('playing', onPlaying)
  video.addEventListener('waiting', onBuffer)
  video.addEventListener('stalled', onBuffer)

  return {
    destroy() {
      video.removeEventListener('playing', onPlaying)
      video.removeEventListener('waiting', onBuffer)
      video.removeEventListener('stalled', onBuffer)
    },
  }
}
```

- [ ] **Step 4: Run the helper test and verify it passes**

Run: `node --test signaling-server/web/src/videoBufferWarning.test.js`

Expected: five passing tests.

### Task 2: Connect the helper to streamed lightbox playback

**Files:**
- Modify: `signaling-server/web/src/app.js:1-30` (import)
- Modify: `signaling-server/web/src/app.js:1160-1185` (`cleanupCurrentVideoPreview`)
- Modify: `signaling-server/web/src/app.js:1428-1436` (`startVideoPreview` delayed video setup)
- Modify: `signaling-server/web/index.html:271` (inline lightbox warning CSS)
- Test: `signaling-server/web/src/app.test.js`

- [ ] **Step 1: Add a failing integration-level lifecycle test**

Add a test using a fake video and fake warning callbacks through an exported test-only wrapper. It must assert that replacing/cleaning a video preview invokes the helper’s `destroy()` exactly once and removes an already-visible banner. Keep the helper behavior tests in Task 1; this test covers the `app.js` lifecycle boundary.

```js
test('cleanupCurrentVideoPreview disposes its buffering monitor and hides its banner', () => {
  let disposed = 0
  let hidden = 0
  __test.setCurrentVideoPreview({
    mediaId: 'video-1',
    seekTimer: null,
    resumePlaybackTimer: null,
    seekListener: null,
    bufferWarningMonitor: { destroy() { disposed += 1 } },
    hideBufferWarning: () => { hidden += 1 },
  })
  __test.cleanupCurrentVideoPreview({ postMessage: () => {} })
  assert.deepEqual({ disposed, hidden }, { disposed: 1, hidden: 1 })
})
```

- [ ] **Step 2: Run the app test and verify the new lifecycle test fails because the test helper does not exist**

Run: `node --test signaling-server/web/src/app.test.js`

Expected: failure because `__test.setCurrentVideoPreview` and/or `__test.cleanupCurrentVideoPreview` is missing.

- [ ] **Step 3: Add banner rendering, monitor creation, and teardown**

1. Import `createVideoBufferWarningMonitor` in `app.js`.
2. Add `showVideoBufferWarning(video)` that appends one element with class `sharebridge-video-buffer-warning`, `role="status"`, and this exact text to the video’s lightbox container:

   `This video is buffering on the current connection. A lower-bitrate Immich transcode may improve playback.`

3. Add `hideVideoBufferWarning(video)` that removes that element from the active video container if present.
4. In `startVideoPreview`, after `const video = document.querySelector('video.lg-video')`, create and store:

```js
vp.hideBufferWarning = () => hideVideoBufferWarning(video)
vp.bufferWarningMonitor = createVideoBufferWarningMonitor({
  video,
  showWarning: () => showVideoBufferWarning(video),
  hideWarning: vp.hideBufferWarning,
})
```

5. In `cleanupCurrentVideoPreview`, call `currentVideoPreview.bufferWarningMonitor?.destroy()` and `currentVideoPreview.hideBufferWarning?.()` before nulling preview state.
6. Extend `__test` only with setters/cleanup dependency injection needed for the lifecycle test; do not expose production behavior through globals.

- [ ] **Step 4: Add the subtle banner CSS**

Add inline styles near the existing gallery CSS in `index.html`:

```css
.sharebridge-video-buffer-warning {
  position: absolute;
  left: 50%;
  bottom: 1rem;
  transform: translateX(-50%);
  z-index: 3;
  max-width: min(34rem, calc(100% - 2rem));
  padding: 0.55rem 0.75rem;
  border: 1px solid #f9e2af;
  border-radius: 0.5rem;
  background: rgba(30, 30, 46, 0.92);
  color: #f9e2af;
  font-size: 0.82rem;
  line-height: 1.35;
  text-align: center;
}
```

Ensure the selected lightbox container is `position: relative` before appending the banner; if it is not, add that class-local style rather than changing global lightGallery layout.

- [ ] **Step 5: Run all browser unit tests**

Run: `node --test signaling-server/web/src/*.test.js`

Expected: all existing browser tests and the new helper/lifecycle tests pass.

- [ ] **Step 6: Manually verify the live behavior after deployment**

1. Open an Immich ShareBridge video.
2. Let initial playback begin; confirm no banner appears for startup buffering.
3. Throttle the browser network or reproduce repeated post-start buffering.
4. Confirm the banner appears only on the second `waiting`/`stalled` event in 20 seconds.
5. Confirm a `playing` recovery hides it, and a second warning does not appear until the video is closed and reopened.

- [ ] **Step 7: Commit the implementation**

```bash
git add signaling-server/web/src/videoBufferWarning.js \
  signaling-server/web/src/videoBufferWarning.test.js \
  signaling-server/web/src/app.js \
  signaling-server/web/src/app.test.js \
  signaling-server/web/index.html
git commit -m "feat(web): warn on repeated video buffering"
```
