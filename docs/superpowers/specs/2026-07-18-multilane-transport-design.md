# Multi-Lane Transport Design

**Date:** 2026-07-18
**Status:** Approved for implementation planning

## Problem

ShareBridge currently carries control messages, thumbnails, image previews, video
ranges, and downloads over one logical channel. The browser routes binary frames
according to whichever operation appears active, while the agent protects all
transfers with one global lock. As a result, a bulk download blocks new previews
and video ranges, and an active preview can block a download.

This is incompatible with the expected gallery experience. A recipient should be
able to browse photos or watch and seek within a video while a download continues
in the background.

## Goals

- Expose three stable application lanes: control, media, and bulk.
- Allow one active media transfer and one serial bulk download concurrently.
- Keep control messages responsive during both kinds of transfer.
- Give thumbnails first-paint treatment on the media lane.
- Preserve end-to-end Noise encryption and aggregate relay quota accounting.
- Present the same lane API to gallery and download code in direct and relay mode.
- Establish a transport boundary that can later map to native WebTransport streams.

## Non-Goals

- Parallel bulk downloads.
- A general user-request download queue.
- WebTransport or HTTP/3 deployment.
- Multiple WebRTC PeerConnections or multiple relay WebSockets.
- Album ZIP download itself; it follows after this prerequisite.
- Cancellation or retry UI redesign beyond lane-scoped media replacement.

## Lane Model

The transport exposes a `ChannelSet` with three required endpoints:

| ID | Lane | Traffic |
|---:|---|---|
| `0x00` | control | Requests, headers, errors, completion, acknowledgements |
| `0x01` | media | Thumbnails, image previews, video ranges |
| `0x02` | bulk | Original-file downloads and album ZIPs |

Lane IDs `0x03` through `0xff` are reserved for later protocol versions.

All three lanes must be ready before the browser reports the session as connected.
If any required lane closes, the complete session closes. This avoids a partially
working connection whose behavior depends on which lane survived.

## Direct Transport

Direct mode uses one WebRTC `PeerConnection` containing three labeled
`RTCDataChannel`s: `control`, `media`, and `bulk`. These are logical SCTP streams,
not separate peer connections. The direct adapter exposes them through the common
`ChannelSet` API and participates in the same scheduling and backpressure contract
as relay mode.

The agent creates all three data channels before generating its SDP offer. The
browser collects channels by label and considers direct transport ready only after
all three are open.

## Relay Transport

Relay mode retains one authenticated encrypted WebSocket in each direction. The
signaling server continues to pair the browser and agent, blindly forward encrypted
frames, and count aggregate forwarded bytes.

The sender selects a lane before encryption. After the Noise handshake, each
plaintext application frame begins with its lane ID:

```text
+---------+-------------------------+
| lane ID | existing frame payload  |
+---------+-------------------------+
  1 byte       variable length
```

The existing outer relay frame still distinguishes text and binary payloads. The
lane ID is inside the Noise ciphertext, so the relay cannot identify traffic
classes. The receiver decrypts the frame, removes the lane byte, and dispatches the
remaining payload to the matching endpoint.

A single outbound pump serializes Noise cipher use. Callers enqueue lane-tagged
writes rather than competing directly for the cipher mutex.

## Scheduling

Scheduling is event-driven. A round is a bookkeeping pass that runs whenever the
underlying transport can accept more data; it has no fixed time duration.

Control receives immediate priority between data frames. To prevent a faulty
control loop from starving payloads, no more than eight control messages are sent
as one burst when media or bulk work is waiting.

Media and bulk use byte-weighted deficit round robin with a base quantum of
64 KiB:

- media receives three quanta, or 192 KiB, per round;
- bulk receives one quantum, or 64 KiB, per round;
- transmitted bytes are subtracted from that lane's deficit;
- a lane whose next frame is larger than its current deficit carries the deficit
  into the next round;
- an empty lane is skipped, so the other lane can use the full available
  connection rather than being capped at its nominal share.

When both lanes remain continuously busy, the intended service order is
approximately:

```text
control, if pending
media
media
media
bulk
repeat
```

The scheduler never interrupts a frame already being written. Stream chunks remain
bounded so newly queued control or media work waits behind at most a small active
bulk write rather than a multi-megabyte application frame.

### Media sub-priority

The media lane has two bounded internal queues:

- interactive media: the active image preview or video range;
- thumbnails: gallery first-paint assets.

They use the same 3:1 byte-weighted policy. Interactive media receives three shares
for each thumbnail share while both are continuously queued. When no interactive
preview exists, thumbnails receive the full media allocation. When thumbnails are
complete, the active preview receives the full media allocation. This prioritizes
video responsiveness without allowing a long video stream to starve the gallery.

## Backpressure and Memory Bounds

Queues are bounded by bytes, not only by message count. A full queue pauses its
producer until the outbound pump creates capacity. Reliable frames are never
dropped to relieve pressure.

Initial application queue limits are:

- control: 8 MiB, matching the existing maximum control/gallery metadata frame;
- interactive media: 512 KiB;
- thumbnails: 2 MiB, accommodating the existing six-thumbnail fetch window;
- bulk: 256 KiB, or four 64 KiB stream chunks.

Stream producers use payloads no larger than 64 KiB. Existing thumbnail messages
remain whole because Immich thumbnails are already bounded derivative assets; their
separate media subqueue prevents them from hiding interactive media or bulk backlog.

The common adapter must account for both application queue depth and native
transport buffering. Direct mode stops dequeuing to a DataChannel while its native
buffer exceeds 256 KiB and resumes from the channel's low-buffer notification.
Relay mode relies on blocking WebSocket writes and the bounded application queues
rather than the current placeholder `BufferedAmount() == 0` behavior. All limits
are named constants covered by boundary tests.

## Application Routing and Concurrency

The browser installs independent handlers for each endpoint:

- control parses JSON and manages operation lifecycle;
- media routes thumbnail frames, image preview chunks, and generation-tagged video
  ranges;
- bulk routes original-file and future ZIP chunks into the existing download
  pipeline.

The current binary heuristic where an active download takes priority over preview
state is removed. Lane identity becomes the routing authority.

The transfer manager replaces its global lock with separate media and bulk state:

- media allows one active operation and supports replacement/cancellation for image
  previews and video seek generations;
- bulk allows one active serial download;
- a media operation and a bulk operation may coexist;
- a second bulk request is rejected until general serial user queuing is added.

Thumbnails use the media lane but do not consume the active interactive-media slot.
Their producer is bounded by the thumbnail subqueue and scheduler.

### OpenCloud and Nextcloud file shares

The multi-lane contract applies to every recipient session, not only Immich gallery
shares. OpenCloud and Nextcloud currently have no preview or video protocol, so
their routing is:

- control: `hello`, `list_request`, `file_list`, `file_request`, `file_header`,
  `chunk_end`, and scoped `error` messages;
- media: open and ready but unused;
- bulk: file-chunk frames for the active download.

There is no distinct browser-agent control channel today. These JSON messages and
binary file chunks currently share one WebRTC DataChannel or encrypted relay
WebSocket. The multi-lane change therefore moves existing file-share JSON traffic
onto the new control endpoint and file bytes onto bulk; it does not introduce a
second file-share protocol.

All three lanes remain required even when media is unused. A uniform readiness and
closure contract avoids mode-dependent transport negotiation and lets OpenCloud or
Nextcloud add previews later without another connection redesign. The OpenCloud and
Nextcloud extensions only create and manage shares through the agent API; their
recipient transfer behavior uses the common ShareBridge web page, so extension UI
and agent-API contracts remain unchanged.

## Protocol Version and Errors

The control lane begins with a protocol-version handshake. A single-lane peer and a
multi-lane peer must fail with a clear incompatible-version error instead of
misinterpreting frames.

Concurrent operations require scoped errors. Control errors include:

- scope: `media`, `bulk`, or `connection`;
- request ID when the error belongs to an operation;
- human-readable message.

A media failure or replacement clears only the matching media request. A bulk
failure aborts only the bulk pipeline. Connection errors close all lanes. Stale
request IDs and stale video generations are ignored rather than applied to a newer
operation.

## Security and Accounting

- The relay continues to see ciphertext and total frame sizes only.
- One Noise handshake authenticates the relay session; the single cipher sequence
  remains serialized by the outbound pump.
- Existing relay JWT pairing and replay protection remain unchanged.
- Relay quota accounting remains aggregate across control, media, and bulk bytes.
- Existing successful-download accounting remains attached to completed bulk user
  actions, not media traffic.

## Verification

### Scheduler tests

- control frames preempt waiting data between frame writes;
- the eight-message control burst cap guarantees data progress;
- continuously queued media and bulk converge on the 3:1 byte allocation;
- idle-lane capacity is consumed by the active lane;
- interactive media is preferred while thumbnails continue progressing;
- queue limits pause producers and never drop reliable frames;
- a write failure closes the channel set and unblocks waiting producers.

### Direct transport tests

- the agent creates exactly the three required labeled DataChannels;
- the browser maps channels by label independent of arrival order;
- readiness waits for all three channels;
- an unknown, duplicate, missing, or closed required channel fails the session;
- lane sends use the correct DataChannel and respect native buffer watermarks.

### Relay transport tests

- Go and JavaScript agree on encrypted lane envelopes;
- lane IDs remain encrypted and the relay forwards frames without parsing them;
- one Noise send sequence is preserved under concurrent media and bulk producers;
- received frames dispatch to the correct endpoint;
- aggregate forwarded-byte quota accounting is unchanged;
- reconnect and closure notify every lane exactly once.

### Application integration tests

- thumbnails load through media while control remains responsive;
- an image preview completes during an active bulk download;
- a video starts and seeks during an active bulk download;
- media replacement does not abort or corrupt bulk data;
- a bulk failure does not prevent a later preview;
- a second bulk request is rejected while the first remains active;
- media and bulk bytes never cross pipelines.
- OpenCloud and Nextcloud-style file sessions route list and lifecycle JSON over
  control and file bytes over bulk while leaving media idle;
- file-mode sessions still require all three lanes and close consistently if the
  unused media lane fails.

Run the complete browser, agent, and signaling-server suites. Then verify both
direct and relay sessions manually with throttling: start a large download, open an
image, play and seek a video, and confirm both media and bulk continue making
progress.

## Follow-On Work

### Immich album download

Delivered: the browser starts the ordered Immich ZIP flow with
`album_download_request`. The agent owns one ordered multipart batch on the serial
bulk lane and emits one operation per ZIP part, advancing only after the browser's
terminal sink acknowledgement, `album_archive_ack`. It sends
`album_download_complete` only after every part succeeds; the complete batch counts
as one ShareBridge download, while any failed batch counts as zero. Previews and
video remain on the media lane throughout the batch.

### Serial user-request queue

Introduce a browser-side job queue so a user can request file A, then B, then C
while A is still active. Each job may contain one file or a multi-part album batch.
The bulk scheduler remains concurrency-one and starts the next job only after the
current job reaches a terminal state.

### Parallel bulk downloads

Later, add transfer IDs to bulk framing and increase the bulk scheduler concurrency
limit. The serial job API and lane model remain valid, but parallel routing,
fairness, cancellation, and progress require their own design.

### WebTransport relay

When WebTransport support and deployment tooling are mature enough for ShareBridge's
browser baseline, replace the relay adapter behind `ChannelSet` with native streams.
Direct WebRTC remains necessary for peer-to-peer connections. The migration must
preserve end-to-end encryption, relay pairing, and quota accounting rather than
relying only on relay-terminated TLS.

The relevant standards and current browser context are the
[W3C WebTransport specification](https://www.w3.org/TR/webtransport/) and
[WebKit's Safari 26.4 WebTransport announcement](https://webkit.org/blog/17862/webkit-features-for-safari-26-4/).

## Reusable Diagram

The standalone diagram is saved at
[`docs/diagrams/multilane-transport.html`](../../diagrams/multilane-transport.html).
`TODO.md` records the follow-up to reuse it on the public ShareBridge website.
