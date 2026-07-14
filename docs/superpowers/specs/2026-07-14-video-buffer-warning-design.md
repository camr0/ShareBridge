# Video Buffering Warning Design

**Status:** Approved behavior; awaiting implementation-plan review
**Date:** 2026-07-14

## Goal

Show a subtle, temporary warning in the ShareBridge video lightbox only when
the recipient experiences repeated playback buffering.

## Scope

The feature is browser-only. It does not query Immich for bitrate metadata and
does not change the agent, relay protocol, Service Worker, or transfer logic.

## Behavior

1. When a streamed lightbox video reaches its first `playing` event, begin
   observing playback-health events for that opened asset.
2. Ignore `waiting` and `stalled` events before the first `playing` event so
   normal initial buffering never produces a warning.
3. After playback begins, record `waiting` and `stalled` events in a rolling
   20-second window.
4. On the second recorded event in that window, show one subtle banner in the
   active lightbox:

   > This video is buffering on the current connection. A lower-bitrate Immich
   > transcode may improve playback.

5. Do not show the banner more than once for an open video asset. A later
   `playing` event hides the banner, but does not reset the one-warning limit.
6. Opening a different video creates fresh buffering state; closing the
   lightbox clears all listeners and warning state.

## Rationale

The browser cannot reliably attribute a playback problem to source bitrate:
relay capacity, the recipient network, and upstream availability can produce
the same symptom. Repeated post-start buffering is therefore the right
user-visible trigger. The recommendation is framed as an optional mitigation,
not a diagnosis.

## Implementation Shape

Add a small playback-health helper in the web client. It receives a video
element, owns the `playing`, `waiting`, and `stalled` listeners, and returns a
cleanup function. The existing lightbox/video-preview teardown calls that
cleanup alongside its seek listener cleanup.

The helper exposes no global state. The lightbox owns rendering and removal of
the banner, allowing the behavior to be unit-tested without lightGallery.

## Tests

- Startup `waiting` before first `playing` never warns.
- One post-start buffer event never warns.
- Two post-start buffer events within 20 seconds warn once.
- Events outside the rolling window do not combine into a warning.
- A later `playing` hides the banner without permitting another warning for
  the same open asset.
- Cleanup detaches all listeners and prevents later events from changing UI.

## Non-goals

- No bitrate badge or pre-play warning.
- No agent-side bitrate probing or Immich API changes.
- No automatic transcode request, settings deep link, or telemetry.
