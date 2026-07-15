# Gallery Background Video Pause Design

## Problem

LightGallery can retain more than one video element for a video slide. When the
viewer navigates to another item, LightGallery may eventually pause one player,
but another player can continue producing background audio for many seconds.
The outgoing video remains resumable when the viewer navigates back.

## Behavior

- Before any slide transition begins, pause every video element in the outgoing
  `.lg-current` slide.
- Apply the same rule before the gallery closes.
- Preserve `currentTime`; returning to the video should show the paused player at
  its previous position rather than restarting it.
- Do not cancel the preview transfer, reset the Service Worker stream, or change
  the existing video seek lifecycle.

## Implementation

Add a focused gallery-controller helper that finds all `video` descendants of
the current slide and calls `pause()` on each. Invoke it from `lgBeforeSlide`
before updating `activePreviewID`, and from `lgBeforeClose` before LightGallery
removes or hides its media elements.

Keep `lgAfterSlide` as the existing preview-loading backstop. The new behavior
belongs in `gallery.js`; it does not require changes to `app.js`, the Service
Worker, the agent protocol, or Immich requests.

## Verification

Add regression tests with two video elements in the outgoing current slide.
Both must be paused synchronously on `lgBeforeSlide` and `lgBeforeClose`, while
their `currentTime` values remain unchanged. Run the complete web test suite,
deploy with `signaling-server/redeploy.sh`, then verify live by playing a video,
navigating two images forward, waiting at least 15 seconds, and confirming that
no video time advances and no audio continues.
