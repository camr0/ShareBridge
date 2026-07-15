# Immich Exact-Range Seek Design

## Problem

After a video is played, left for neighboring gallery items, and revisited, an out-of-buffer seek can destroy the player. The reproduced 3:02 video failure showed Chrome requesting `Range: bytes=22970368-` while the page's time-based estimate asked the agent to begin at byte `23406117`. The Service Worker declared the Chrome-requested offset in `Content-Range` but delivered a body beginning at the later agent offset. Chrome aborted the inconsistent response and lightGallery replaced the video with its media-error wrapper.

The agent completed the requested range successfully, so the fix belongs in the browser-to-Service-Worker seek protocol.

## Design

The Service Worker will make Chrome's actual Range request authoritative.

When a media fetch for the current seek generation includes a nonzero Range start, the Service Worker will post a `media_range_request` message to the requesting page containing the media ID, generation, and exact byte offset. The page will use that offset for `asset_preview_seek` instead of its `currentTime / duration * totalSize` estimate.

The existing `seeking` handler will still reset the Service Worker generation immediately, because Chrome may issue its Range request synchronously. It will retain the time-based offset only as a short fallback for browsers that reuse the existing fetch and therefore produce no new Range request. Whichever path sends the seek first cancels the other, and repeated Range notifications for the same generation are ignored.

The agent protocol and transfer format remain unchanged.

## State and Ordering

Each active video preview will track whether the current generation's agent seek has been sent. A seek follows this order:

1. The video emits `seeking` for a target outside its buffered ranges.
2. The page increments the generation and immediately posts the Service Worker reset.
3. The page schedules the estimated-offset fallback.
4. Chrome requests a byte range from the Service Worker.
5. The Service Worker reports that exact Range start to the page.
6. The page cancels the fallback and sends one `asset_preview_seek` using the exact Range start.
7. The agent header rebases the parked Service Worker stream at the same offset, keeping the response headers and body aligned.

Stale messages, messages for another media ID, and messages for an older generation are ignored.

## Error Handling

If Chrome does not create a new Range request, the fallback sends the existing time-based estimate so current browser behavior does not regress. If the transfer channel is unavailable, neither path sends a seek. Existing generation checks continue to reject stale agent headers and chunks.

This change does not suppress lightGallery media errors. The player should stop producing the corrupt response that causes the error.

## Testing

- App test: an out-of-buffer seek resets immediately but waits for the Service Worker Range message before sending the agent seek.
- App test: the exact Service Worker Range offset wins when it is lower than the time-based estimate.
- App test: only one seek is sent per generation and stale Range messages are ignored.
- App test: the time-based fallback still sends when no Range message arrives.
- Service Worker test: a nonzero Range fetch reports `media_range_request` with the current media ID, generation, and requested offset.
- Existing Service Worker streaming, generation-rebase, gallery lifecycle, and full web suites must remain green.
- Live verification repeats the reproduced 3:02-video sequence and confirms playback survives an out-of-buffer seek without lightGallery's error wrapper.
