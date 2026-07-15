# Gallery Background Video Pause Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Pause every outgoing LightGallery video synchronously so media cannot continue playing behind later gallery items.

**Architecture:** Keep the change inside the gallery controller. A small helper pauses all videos in the current slide and is called before slide and close transitions; existing preview, Service Worker, and seek lifecycles remain unchanged.

**Tech Stack:** Browser JavaScript, LightGallery lifecycle events, Node test runner.

---

### Task 1: Pause outgoing gallery videos

**Files:**
- Modify: `signaling-server/web/src/gallery.js`
- Test: `signaling-server/web/src/gallery.test.js`

- [ ] **Step 1: Write the failing tests**

Add tests that register `lgBeforeSlide` and `lgBeforeClose`, expose two video
objects through `.lg-current`, invoke each handler, and assert that both
`pause()` methods run once without changing either `currentTime`.

- [ ] **Step 2: Run the focused tests to verify they fail**

Run:

```bash
cd signaling-server/web
node --test --test-name-pattern='pauses every outgoing video' src/gallery.test.js
```

Expected: FAIL because the lifecycle handlers do not pause the video objects.

- [ ] **Step 3: Implement the minimal pause lifecycle**

Add a helper equivalent to:

```js
function pauseCurrentSlideVideos() {
  const videos = lightboxRoot?.querySelectorAll?.('.lg-current video') || []
  for (const video of videos) video.pause?.()
}
```

Call it synchronously from a dedicated `lgBeforeSlide` handler before
`requestPreviewByIndex`, and from `lgBeforeClose`. Register and unregister both
handlers with the other LightGallery lifecycle listeners.

- [ ] **Step 4: Run the focused tests to verify they pass**

Run the focused command from Step 2.

Expected: both slide and close cases PASS.

### Task 2: Verify and deploy

**Files:**
- Verify: `signaling-server/web/src/gallery.js`
- Verify: `signaling-server/web/src/gallery.test.js`

- [ ] **Step 1: Run the full web suite**

```bash
cd signaling-server/web
npm test
```

Expected: all tests PASS.

- [ ] **Step 2: Check the patch**

```bash
git diff --check
git status --short
```

Expected: no whitespace errors and only the planned files changed.

- [ ] **Step 3: Deploy the signaling server**

```bash
cd signaling-server
./redeploy.sh
```

Expected: the container rebuilds, starts, and reports the signaling server
listening on port 8080.

- [ ] **Step 4: Verify the live reproduction**

Open the 3:02 video, manually pause and play it, navigate away, and trace all
video elements for at least 15 seconds. The outgoing player's `paused` value
must become `true` before the transition and its `currentTime` must not advance.
Repeat after navigating back and playing again.

- [ ] **Step 5: Commit the verified fix**

```bash
git add signaling-server/web/src/gallery.js signaling-server/web/src/gallery.test.js
git commit -m "fix gallery background video playback"
```
