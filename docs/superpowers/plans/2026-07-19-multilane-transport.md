# Multi-Lane Transport Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace ShareBridge's single browser-agent transfer channel with control, media, and bulk lanes so previews and video ranges can run during one serial download.

**Architecture:** Add a transport-neutral lane protocol and weighted scheduler, then adapt relay and direct transports behind a common channel-set contract. Route transfer-manager and browser operations by lane, with one media operation and one bulk operation allowed concurrently while OpenCloud/Nextcloud leave media idle.

**Tech Stack:** Go 1.26.1, Pion WebRTC, coder/websocket, Noise P-256, browser JavaScript modules, WebRTC DataChannels, Node's built-in test runner.

**Execution prerequisite:** Use `superpowers:using-git-worktrees` to create an isolated `codex/` worktree before Task 1. Preserve the user's untracked `.claude/` and `.codex/` directories in the main workspace.

---

## File Structure

### New files

- `agent/internal/multilane/lane.go` — stable lane IDs, traffic classes, relay lane envelopes, and endpoint/channel-set interfaces.
- `agent/internal/multilane/lane_test.go` — lane validation and envelope tests.
- `agent/internal/multilane/scheduler.go` — bounded Go outbound queues and byte-weighted deficit-round-robin pump.
- `agent/internal/multilane/scheduler_test.go` — deterministic priority, weighting, bounds, and failure tests.
- `agent/internal/peer/peer_test.go` — direct three-DataChannel contract and readiness tests.
- `signaling-server/web/src/multiLaneProtocol.js` — browser lane IDs, envelope helpers, and traffic-class constants.
- `signaling-server/web/src/multiLaneProtocol.test.js` — browser protocol parity tests.
- `signaling-server/web/src/laneScheduler.js` — browser bounded weighted scheduler.
- `signaling-server/web/src/laneScheduler.test.js` — browser scheduler tests.
- `signaling-server/web/src/channelSet.js` — browser lane endpoint/channel-set lifecycle and transport-version handshake.
- `signaling-server/web/src/channelSet.test.js` — readiness, handshake, dispatch, and closure tests.

### Modified files

- `agent/internal/relaychannel/channel.go` — expose a three-lane set, encrypt lane IDs, and use the scheduler.
- `agent/internal/relaychannel/channel_test.go` — Go relay lane and handshake coverage.
- `agent/internal/peer/peer.go` — create three labeled DataChannels and expose lane endpoints.
- `agent/internal/daemon/daemon.go` — pass direct/relay channel sets into transfer managers.
- `agent/internal/daemon/daemon_test.go` — update relay/direct mocks and assert all lanes are wired.
- `agent/internal/transfer/manager.go` — route control/media/bulk independently and split media/bulk locks.
- `agent/internal/transfer/manager_test.go` — lane routing, concurrent media/bulk, file-mode idle-media, and scoped-error tests.
- `signaling-server/web/src/secureRelayChannel.js` — decrypt/encrypt lane envelopes and expose a channel set.
- `signaling-server/web/src/secureRelayChannel.test.js` — relay lane interoperability and scheduler behavior.
- `signaling-server/web/src/directChannel.js` — collect three labeled DataChannels into a channel set.
- `signaling-server/web/src/directChannel.test.js` — label-independent readiness and failure tests.
- `signaling-server/web/src/app.js` — install per-lane handlers and remove active-operation binary heuristics.
- `signaling-server/web/src/app.test.js` — browser lane routing and concurrent media/bulk tests.
- `signaling-server/web/src/connectTransferChannel.test.js` — assert connection selection returns a ready channel set.

## Task 1: Define the cross-language lane protocol

**Files:**
- Create: `agent/internal/multilane/lane.go`
- Create: `agent/internal/multilane/lane_test.go`
- Create: `signaling-server/web/src/multiLaneProtocol.js`
- Create: `signaling-server/web/src/multiLaneProtocol.test.js`

- [ ] **Step 1: Write failing Go protocol tests**

Create tests that require stable IDs, reject reserved IDs, and round-trip the encrypted relay plaintext envelope:

```go
func TestRelayEnvelopeRoundTrip(t *testing.T) {
    payload := []byte("file header")
    encoded, err := EncodeEnvelope(LaneControl, payload)
    require.NoError(t, err)
    require.Equal(t, byte(0x00), encoded[0])

    lane, decoded, err := DecodeEnvelope(encoded)
    require.NoError(t, err)
    require.Equal(t, LaneControl, lane)
    require.Equal(t, payload, decoded)
}

func TestDecodeEnvelopeRejectsReservedLane(t *testing.T) {
    _, _, err := DecodeEnvelope([]byte{0x03, 0x01})
    require.ErrorContains(t, err, "unknown lane")
}
```

- [ ] **Step 2: Run Go tests and confirm RED**

Run:

```bash
cd agent
go test ./internal/multilane -run 'TestRelayEnvelope|TestDecodeEnvelope' -v
```

Expected: FAIL because `agent/internal/multilane` does not exist.

- [ ] **Step 3: Implement Go lane constants and envelopes**

Implement the exact public contract:

```go
package multilane

type Lane byte

const (
    ProtocolVersion = 2
    LaneControl Lane = 0x00
    LaneMedia   Lane = 0x01
    LaneBulk    Lane = 0x02
)

func (l Lane) Valid() bool {
    return l == LaneControl || l == LaneMedia || l == LaneBulk
}

func EncodeEnvelope(lane Lane, payload []byte) ([]byte, error) {
    if !lane.Valid() { return nil, fmt.Errorf("unknown lane: 0x%02x", byte(lane)) }
    encoded := make([]byte, 1+len(payload))
    encoded[0] = byte(lane)
    copy(encoded[1:], payload)
    return encoded, nil
}

func DecodeEnvelope(encoded []byte) (Lane, []byte, error) {
    if len(encoded) == 0 { return 0, nil, errors.New("missing lane envelope") }
    lane := Lane(encoded[0])
    if !lane.Valid() { return 0, nil, fmt.Errorf("unknown lane: 0x%02x", encoded[0]) }
    return lane, append([]byte(nil), encoded[1:]...), nil
}
```

Also define `KindText Kind = 0x01`, `KindBinary Kind = 0x02`,
`ClassControl`, `ClassInteractiveMedia`, `ClassThumbnail`, and `ClassBulk`
without assigning extra wire lane IDs. Add the explicit scheduler mapping:

```go
func LaneForClass(class TrafficClass) (Lane, error) {
    switch class {
    case ClassControl:
        return LaneControl, nil
    case ClassInteractiveMedia, ClassThumbnail:
        return LaneMedia, nil
    case ClassBulk:
        return LaneBulk, nil
    default:
        return 0, fmt.Errorf("unknown traffic class: %d", class)
    }
}
```

- [ ] **Step 4: Write failing browser protocol tests**

```js
test('relay lane envelopes match the Go wire contract', () => {
  const encoded = encodeLaneEnvelope(LANE_MEDIA, new Uint8Array([4, 5]))
  assert.deepEqual([...encoded], [0x01, 4, 5])
  assert.deepEqual(decodeLaneEnvelope(encoded), {
    lane: LANE_MEDIA,
    payload: new Uint8Array([4, 5]),
  })
})

test('reserved relay lane ids fail closed', () => {
  assert.throws(() => decodeLaneEnvelope(new Uint8Array([0x03])), /unknown lane/i)
})
```

- [ ] **Step 5: Run browser tests and confirm RED**

```bash
cd signaling-server/web
node --test src/multiLaneProtocol.test.js
```

Expected: FAIL because the module is missing.

- [ ] **Step 6: Implement browser protocol parity**

Export `PROTOCOL_VERSION = 2`, `LANE_CONTROL = 0x00`, `LANE_MEDIA = 0x01`, `LANE_BULK = 0x02`, traffic-class strings, and `encodeLaneEnvelope`/`decodeLaneEnvelope` matching Go byte-for-byte.

- [ ] **Step 7: Run both focused suites and commit**

```bash
cd agent && go test ./internal/multilane -v
cd ../signaling-server/web && node --test src/multiLaneProtocol.test.js
git add agent/internal/multilane signaling-server/web/src/multiLaneProtocol.js signaling-server/web/src/multiLaneProtocol.test.js
git commit -m "feat: define multi-lane transport protocol"
```

Expected: both suites PASS.

## Task 2: Implement deterministic weighted schedulers

**Files:**
- Create: `agent/internal/multilane/scheduler.go`
- Create: `agent/internal/multilane/scheduler_test.go`
- Create: `signaling-server/web/src/laneScheduler.js`
- Create: `signaling-server/web/src/laneScheduler.test.js`

- [ ] **Step 1: Write failing Go scheduler tests**

Use an injected blocking writer and assert the actual send sequence:

```go
func TestSchedulerWeightsMediaThreeToBulkOne(t *testing.T) {
    writer := newRecordingWriter()
    scheduler := NewScheduler(writer.Write)
    t.Cleanup(scheduler.Close)

    for i := 0; i < 6; i++ {
        go scheduler.Send(context.Background(), ClassInteractiveMedia, KindBinary, bytes.Repeat([]byte{byte(i)}, 64*1024))
    }
    for i := 0; i < 2; i++ {
        go scheduler.Send(context.Background(), ClassBulk, KindBinary, bytes.Repeat([]byte{byte(i)}, 64*1024))
    }

    require.Eventually(t, func() bool { return writer.Count() == 8 }, time.Second, time.Millisecond)
    require.Equal(t,
        []TrafficClass{ClassInteractiveMedia, ClassInteractiveMedia, ClassInteractiveMedia, ClassBulk},
        writer.Classes()[:4],
    )
}

func TestSchedulerControlBurstCannotStarveData(t *testing.T) {
    // Queue 16 control writes and one bulk write; assert bulk appears no later
    // than position 9 because the control burst cap is eight.
}

func TestSchedulerBlocksProducerAtByteLimit(t *testing.T) {
    // Hold the writer, fill the 256 KiB bulk queue, and assert a fifth 64 KiB
    // Send blocks until one queued frame is released.
}
```

Add tests for idle-lane borrowing, 3:1 interactive-media/thumbnail progress, context cancellation, and writer failure unblocking every sender.

- [ ] **Step 2: Run Go scheduler tests and confirm RED**

```bash
cd agent
go test ./internal/multilane -run Scheduler -v
```

Expected: FAIL because `NewScheduler` is undefined.

- [ ] **Step 3: Implement the Go scheduler**

Use these constants and synchronous producer semantics:

```go
const (
    BaseQuantumBytes       = 64 * 1024
    ControlBurstLimit      = 8
    ControlQueueBytes      = 8 * 1024 * 1024
    InteractiveQueueBytes  = 512 * 1024
    ThumbnailQueueBytes    = 2 * 1024 * 1024
    BulkQueueBytes         = 256 * 1024
    NativeBufferHighWater  = 256 * 1024
)

type WriteFunc func(class TrafficClass, kind Kind, payload []byte) error

type Scheduler struct {
    // mutex + condition variable
    // byte-counted FIFO per traffic class
    // media and bulk deficits
    // terminal writer error
}

func (s *Scheduler) Send(ctx context.Context, class TrafficClass, kind Kind, payload []byte) error
func (s *Scheduler) Close() error
```

`Send` copies the payload, waits for queue capacity, enqueues a request containing a result channel, and returns only after the writer accepts that request or the scheduler fails. The pump drains at most eight control messages, applies outer media:bulk deficits of 192 KiB:64 KiB, and applies inner interactive:thumbnail deficits of 3:1 when choosing a media request.

- [ ] **Step 4: Write failing JavaScript scheduler tests**

Mirror the Go sequence with an injected async `write` function:

```js
test('scheduler gives media three byte quanta for each bulk quantum', async () => {
  const writes = []
  const scheduler = new LaneScheduler({
    write: async (request) => writes.push(request.className),
  })
  await Promise.all([
    ...Array.from({ length: 6 }, () => scheduler.send(interactiveFrame(64 * 1024))),
    ...Array.from({ length: 2 }, () => scheduler.send(bulkFrame(64 * 1024))),
  ])
  assert.deepEqual(writes.slice(0, 4), ['interactive-media', 'interactive-media', 'interactive-media', 'bulk'])
})
```

Add browser parity tests for control burst, thumbnails, idle borrowing, queue bounds, and terminal failure.

- [ ] **Step 5: Implement and verify the browser scheduler**

Implement the same constants and algorithm. `send()` returns a Promise that resolves after the injected transport writer accepts the frame, not merely after enqueue.

```bash
cd signaling-server/web
node --test src/laneScheduler.test.js
cd ../../agent
go test ./internal/multilane -run Scheduler -v
```

Expected: both scheduler suites PASS with identical ordering assertions.

- [ ] **Step 6: Commit**

```bash
git add agent/internal/multilane/scheduler.go agent/internal/multilane/scheduler_test.go signaling-server/web/src/laneScheduler.js signaling-server/web/src/laneScheduler.test.js
git commit -m "feat: add weighted lane scheduler"
```

## Task 3: Add channel-set lifecycle and protocol handshake

**Files:**
- Modify: `agent/internal/multilane/lane.go`
- Modify: `agent/internal/multilane/lane_test.go`
- Create: `signaling-server/web/src/channelSet.js`
- Create: `signaling-server/web/src/channelSet.test.js`

- [ ] **Step 1: Write failing lifecycle tests**

Go must expose a transport-neutral set:

```go
type Endpoint interface {
    SendText(string) error
    SendBinary([]byte) error
    BufferedAmount() uint64
    SetOnMessage(func([]byte))
}

type ChannelSet interface {
    Endpoint(Lane) Endpoint
    SetOnOpen(func())
    SetOnClose(func())
    Close() error
}
```

Browser tests must require all lanes and the version exchange:

```js
test('channel set becomes ready only after three lanes and version acknowledgement', async () => {
  const set = createChannelSet()
  set.attach('bulk', fakeChannel('open'))
  set.attach('control', fakeChannel('open'))
  set.attach('media', fakeChannel('open'))
  assert.equal(set.readyState, 'handshaking')
  set.control.deliver(JSON.stringify({ type: 'transport_ready', version: 2 }))
  await set.ready
  assert.equal(set.readyState, 'open')
})
```

Add unknown-label, duplicate-label, missing-lane timeout, version mismatch, and close-on-any-lane tests.

- [ ] **Step 2: Confirm RED**

```bash
cd signaling-server/web
node --test src/channelSet.test.js
```

Expected: FAIL because `channelSet.js` is missing.

- [ ] **Step 3: Implement the browser channel set**

`ChannelSet.attach(label, channel)` validates `control`, `media`, or `bulk`; installs one close fan-in; sends `{type:'transport_hello', version:2}` over control after all lanes open; and resolves `ready` only after `{type:'transport_ready', version:2}`. It buffers no application messages before readiness.

- [ ] **Step 4: Implement the Go responder helper**

Add `InstallHandshakeResponder(set ChannelSet, onReady func())`. It intercepts the first control message, rejects anything other than `transport_hello` version 2, sends `transport_ready`, then installs the transfer manager's control handler and invokes `onReady`.

- [ ] **Step 5: Verify and commit**

```bash
cd signaling-server/web && node --test src/channelSet.test.js
cd ../../agent && go test ./internal/multilane -v
git add agent/internal/multilane signaling-server/web/src/channelSet.js signaling-server/web/src/channelSet.test.js
git commit -m "feat: add channel set handshake"
```

## Task 4: Convert secure relay to encrypted logical lanes

**Files:**
- Modify: `agent/internal/relaychannel/channel.go`
- Modify: `agent/internal/relaychannel/channel_test.go`
- Modify: `signaling-server/web/src/secureRelayChannel.js`
- Modify: `signaling-server/web/src/secureRelayChannel.test.js`

- [ ] **Step 1: Write failing Go relay tests**

Extend the relay test server to decrypt browser/agent traffic and assert the lane byte is inside ciphertext:

```go
func TestSecureRelayChannelDispatchesEncryptedLanes(t *testing.T) {
    channel := startTestChannel(t)
    seen := make(chan []byte, 1)
    channel.Endpoint(multilane.LaneMedia).SetOnMessage(func(data []byte) { seen <- data })

    encoded, err := multilane.EncodeEnvelope(multilane.LaneMedia, []byte("preview"))
    require.NoError(t, err)
    testRelay.SendEncrypted(encoded)
    require.Equal(t, []byte("preview"), <-seen)
}
```

Add a concurrent media/bulk send test that confirms one Noise cipher sequence and scheduler order. Replace the obsolete `BufferedAmount always returns 0` test with bounded-queue/backpressure assertions.

- [ ] **Step 2: Run focused Go tests and confirm RED**

```bash
cd agent
go test ./internal/relaychannel -run 'Lane|Scheduler|Buffered' -v
```

Expected: FAIL because `SecureRelayChannel` has no endpoints.

- [ ] **Step 3: Implement the Go relay channel set**

Keep one WebSocket, handshake, send cipher, and receive cipher. Replace direct `sendFrame` calls with scheduler requests. The scheduler writer must:

```go
func (c *SecureRelayChannel) writeScheduled(class multilane.TrafficClass, kind multilane.Kind, payload []byte) error {
    lane, err := multilane.LaneForClass(class)
    if err != nil { return err }
    plaintext, err := multilane.EncodeEnvelope(lane, payload)
    if err != nil { return err }
    ciphertext, err := c.send.Encrypt(nil, plaintext)
    if err != nil { return err }
    frame, err := WriteFrame(byte(kind), ciphertext)
    if err != nil { return err }
    return c.conn.Write(context.Background(), websocket.MessageBinary, frame)
}
```

The read loop decrypts, decodes the lane envelope, and dispatches through that endpoint. The signaling relay remains untouched.

- [ ] **Step 4: Write failing JavaScript relay tests**

Require `channel.control`, `channel.media`, and `channel.bulk`; verify a media payload's lane byte appears only after Noise decryption; and assert invalid lane IDs close the complete set.

- [ ] **Step 5: Implement browser relay lanes**

After Noise split, create the common `ChannelSet`, install the version initiator, and route scheduler writes through `encodeLaneEnvelope()` before encryption. Preserve the existing outer text/binary frame kinds.

- [ ] **Step 6: Verify relay suites and Go/JS fixtures**

```bash
cd agent && go test ./internal/relaychannel -v
cd ../signaling-server/web && node --test src/secureRelayChannel.test.js src/channelSet.test.js src/laneScheduler.test.js
```

Expected: all focused tests PASS; no signaling-server relay source changes.

- [ ] **Step 7: Commit**

```bash
git add agent/internal/relaychannel signaling-server/web/src/secureRelayChannel.js signaling-server/web/src/secureRelayChannel.test.js
git commit -m "feat: multiplex secure relay lanes"
```

## Task 5: Create three direct WebRTC DataChannels

**Files:**
- Modify: `agent/internal/peer/peer.go`
- Create: `agent/internal/peer/peer_test.go`
- Modify: `signaling-server/web/src/directChannel.js`
- Modify: `signaling-server/web/src/directChannel.test.js`
- Modify: `signaling-server/web/src/app.js`
- Modify: `signaling-server/web/src/app.test.js`

- [ ] **Step 1: Write failing Go peer tests**

Inject a PeerConnection factory or inspect SDP/datachannel state through a focused test seam:

```go
func TestCreateOfferCreatesRequiredLaneLabels(t *testing.T) {
    peer := newTestPeer(t)
    _, err := peer.CreateOffer()
    require.NoError(t, err)
    require.ElementsMatch(t, []string{"control", "media", "bulk"}, peer.LaneLabelsForTest())
}
```

Add readiness-on-all-three, duplicate open notification, any-lane close, and endpoint routing tests.

- [ ] **Step 2: Confirm RED**

```bash
cd agent
go test ./internal/peer -v
```

Expected: FAIL because the peer creates only `data`.

- [ ] **Step 3: Implement agent direct lanes**

Replace `dc *webrtc.DataChannel` with a map keyed by `multilane.Lane`. Create labels in stable order before the SDP offer. Expose `Endpoint(lane)`, fire readiness once after all channels open and the browser version hello succeeds, and close the PeerConnection if any required DataChannel closes.

- [ ] **Step 4: Write failing browser direct tests**

```js
test('direct set accepts required channels in arbitrary arrival order', async () => {
  const set = createDirectChannelSet()
  set.accept(fakeRTCDataChannel('bulk'))
  set.accept(fakeRTCDataChannel('media'))
  set.accept(fakeRTCDataChannel('control'))
  deliverTransportReady(set.control)
  await set.ready
  assert.equal(set.readyState, 'open')
})
```

Add duplicate, unknown, missing, and close tests.

- [ ] **Step 5: Implement browser direct collection**

Update `initializeIceConfigTransport()` so every `ondatachannel` event is passed to one `DirectChannelSet`; call the direct-connect resolver only after `set.ready`, not after the first DataChannel opens.

- [ ] **Step 6: Verify and commit**

```bash
cd agent && go test ./internal/peer -v
cd ../signaling-server/web && node --test src/directChannel.test.js src/app.test.js
git add agent/internal/peer signaling-server/web/src/directChannel.js signaling-server/web/src/directChannel.test.js signaling-server/web/src/app.js signaling-server/web/src/app.test.js
git commit -m "feat: add direct WebRTC lanes"
```

## Task 6: Route the Go transfer manager by lane

**Files:**
- Modify: `agent/internal/transfer/manager.go`
- Modify: `agent/internal/transfer/manager_test.go`

- [ ] **Step 1: Replace the test mock with a channel-set mock**

Create independent recording endpoints:

```go
type mockChannelSet struct {
    control *mockEndpoint
    media   *mockEndpoint
    bulk    *mockEndpoint
}

func (m *mockChannelSet) Endpoint(lane multilane.Lane) multilane.Endpoint {
    return map[multilane.Lane]*mockEndpoint{
        multilane.LaneControl: m.control,
        multilane.LaneMedia: m.media,
        multilane.LaneBulk: m.bulk,
    }[lane]
}
```

- [ ] **Step 2: Write failing routing tests**

Add exact assertions:

```go
func TestFileShareRoutesControlAndBulkWhileMediaStaysIdle(t *testing.T) {
    channels := newMockChannelSet()
    manager := NewManager(channels, storage, 0)
    manager.HandleOpen()
    channels.control.DeliverJSON(map[string]any{"type":"file_request", "path":"report.pdf", "request_id":"bulk-1"})

    require.Eventually(t, func() bool { return channels.bulk.BinaryCount() > 0 }, time.Second, time.Millisecond)
    require.True(t, channels.control.HasTextType("file_header"))
    require.True(t, channels.control.HasTextType("chunk_end"))
    require.Zero(t, channels.media.BinaryCount())
}

func TestMediaPreviewAndBulkDownloadRunConcurrently(t *testing.T) {
    // Block both backend readers after their first chunks, start preview and
    // asset_request, and assert each lane receives bytes before either releases.
}
```

Add thumbnail-on-media, image/video-on-media, original-on-bulk, second-bulk rejection, media replacement without bulk cancellation, and scoped-error request-ID tests.

- [ ] **Step 3: Run transfer tests and confirm RED**

```bash
cd agent
go test ./internal/transfer -run 'Lane|Routes|Concurrently|Scoped|FileShare' -v
```

Expected: FAIL because `Manager` still accepts one `DataChannel` and one global lock.

- [ ] **Step 4: Implement lane routing**

Change constructors to accept `multilane.ChannelSet`. Install `HandleMessage` only on control. Replace `transfer atomic.Bool` with:

```go
mediaTransfer atomic.Bool
bulkTransfer  atomic.Bool
mediaCancel   context.CancelFunc
```

Use control for all JSON; media for thumbnail and preview binary frames; bulk for file/original binary frames. Parameterize `sendWithBackpressure(ctx, endpoint, data)` instead of reading one global channel. Include `scope` and `request_id` in errors and lifecycle headers.

- [ ] **Step 5: Verify the complete transfer package**

```bash
cd agent
go test ./internal/transfer -count=1 -v
```

Expected: all legacy and new tests PASS under the channel-set mock.

- [ ] **Step 6: Commit**

```bash
git add agent/internal/transfer/manager.go agent/internal/transfer/manager_test.go
git commit -m "feat: route transfers across lanes"
```

## Task 7: Wire channel sets through the daemon

**Files:**
- Modify: `agent/internal/daemon/daemon.go`
- Modify: `agent/internal/daemon/daemon_test.go`

- [ ] **Step 1: Write failing daemon wiring tests**

Update `mockRelayChannel` to expose three endpoints. Add assertions that both direct and relay paths create a transfer manager only after the channel-set handshake and that one lane closure tears down the session once.

- [ ] **Step 2: Confirm RED**

```bash
cd agent
go test ./internal/daemon -run 'Relay.*Lane|Direct.*Lane|ChannelSet' -v
```

Expected: FAIL because daemon interfaces still model one transfer channel.

- [ ] **Step 3: Update daemon interfaces and construction**

Replace `relayTransferChannel` with a lifecycle interface embedding `multilane.ChannelSet`. Construct `transfer.NewManager(channelSet, ...)` or `NewGalleryManager(channelSet, ...)` in both direct and relay paths. Install transfer handlers after the version responder succeeds; preserve download persistence and signaling callbacks unchanged.

- [ ] **Step 4: Verify daemon and agent suites**

```bash
cd agent
go test ./internal/daemon -count=1
go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/daemon/daemon.go agent/internal/daemon/daemon_test.go
git commit -m "feat: wire multi-lane agent sessions"
```

## Task 8: Route browser application traffic by lane

**Files:**
- Modify: `signaling-server/web/src/app.js`
- Modify: `signaling-server/web/src/app.test.js`
- Modify: `signaling-server/web/src/connectTransferChannel.test.js`

- [ ] **Step 1: Write failing browser routing tests**

Refactor test helpers to install a fake channel set. Add:

```js
test('file mode sends lifecycle JSON on control and consumes bytes from bulk', async () => {
  const channels = fakeChannelSet()
  __test.setTransferSession({ channels, mode: 'relay' })

  __test.requestFile('report.pdf')
  assert.equal(JSON.parse(channels.control.sentText[0]).type, 'file_request')

  await channels.control.deliverJSON({ type: 'file_header', name: 'report.pdf', size: 3, binary_envelope: true })
  await channels.bulk.deliverBinary(fileChunk([1, 2, 3]))
  await channels.control.deliverJSON({ type: 'chunk_end' })
  assert.equal(channels.media.deliveries, 0)
})

test('video seek and bulk download route concurrently', async () => {
  // Start a download, issue a video seek, feed media and bulk frames in an
  // interleaved order, and assert each destination receives only its bytes.
})
```

Add thumbnail-media routing, preview-media routing, scoped error handling, stale request IDs, second bulk rejection, and any-lane closure tests.

- [ ] **Step 2: Run focused browser tests and confirm RED**

```bash
cd signaling-server/web
node --test --test-name-pattern='lane|control|bulk|concurrently|file mode' src/app.test.js src/connectTransferChannel.test.js
```

Expected: FAIL because `app.js` still has one `transferChannel` and one binary handler.

- [ ] **Step 3: Split browser handlers**

Replace `transferChannel` with `transferChannels`. Install:

```js
transferChannels.control.onmessage = handleControlMessage
transferChannels.media.onmessage = handleMediaMessage
transferChannels.bulk.onmessage = handleBulkMessage
transferChannels.onclose = handleTransferClosure
```

Move JSON switch logic into `handleControlMessage`. Media decodes only thumbnails and preview/video file frames. Bulk decodes only download file frames. Remove the `active download takes priority` routing heuristic and remove the `if (activeDownload) return` guard from `requestGalleryPreview`. Send every request over control and attach request IDs/scopes.

Export `requestFile`, `handleControlMessage`, `handleMediaMessage`, and
`handleBulkMessage` through `__test` for focused routing tests.

- [ ] **Step 4: Preserve file-share behavior**

Ensure `requestFileList`, breadcrumb navigation, OpenCloud/Nextcloud file requests, file headers, chunk completion, max-download errors, and disconnect cleanup all use control plus bulk while media remains idle.

- [ ] **Step 5: Verify browser suites**

```bash
cd signaling-server/web
node --test src/app.test.js src/connectTransferChannel.test.js src/directChannel.test.js src/secureRelayChannel.test.js src/channelSet.test.js src/laneScheduler.test.js
npm test
```

Expected: all browser tests PASS with no unhandled promise rejections.

- [ ] **Step 6: Commit**

```bash
git add signaling-server/web/src/app.js signaling-server/web/src/app.test.js signaling-server/web/src/connectTransferChannel.test.js
git commit -m "feat: route browser traffic by lane"
```

## Task 9: Cross-stack verification and handoff

**Files:**
- Verify: `agent/internal/multilane/`
- Verify: `agent/internal/peer/`
- Verify: `agent/internal/relaychannel/`
- Verify: `agent/internal/transfer/`
- Verify: `agent/internal/daemon/`
- Verify: `signaling-server/web/src/`
- Verify: `signaling-server/internal/handler/relay_ws.go`

- [ ] **Step 1: Run formatting and static checks**

```bash
gofmt -w agent/internal/multilane/*.go agent/internal/peer/*.go agent/internal/relaychannel/*.go agent/internal/transfer/*.go agent/internal/daemon/*.go
cd agent && go vet ./...
cd ../signaling-server && go vet ./...
cd .. && git diff --check
```

Expected: no output from `git diff --check`; both `go vet` commands exit 0.

- [ ] **Step 2: Run complete automated suites**

```bash
cd agent && go test ./... -count=1
cd ../signaling-server && go test ./... -count=1
cd web && npm test
cd ../../extensions/opencloud && npm test
cd ../nextcloud && npm test
```

Expected: every suite PASS. Extension suites verify that share-management contracts remain unchanged.

- [ ] **Step 3: Run relay race-sensitive tests repeatedly**

```bash
cd agent
go test -race ./internal/multilane ./internal/relaychannel ./internal/transfer -count=10
```

Expected: PASS with no race reports, deadlocks, or goroutine leaks.

- [ ] **Step 4: Verify relay opacity and accounting**

```bash
cd signaling-server
go test ./internal/handler ./internal/relay -run 'Relay|Accounting' -count=1 -v
```

Expected: relay forwarding and aggregate accounting tests PASS without teaching the signaling server about lane IDs.

- [ ] **Step 5: Perform live direct-mode concurrency check**

Start a large original-file download, open an image, play a video, and seek outside its buffered range. Confirm:

- all three DataChannels are open inside one PeerConnection;
- the image and seek complete before the bulk download finishes;
- bulk progress continues during media activity;
- a second bulk request is rejected;
- no payload corruption or unexpected connection reset occurs.

- [ ] **Step 6: Perform live relay-mode concurrency check**

Repeat Step 5 with relay-only mode and browser network throttling. Confirm control remains responsive, thumbnails progress, video is favored, bulk continues, and quota accounting includes all forwarded bytes.

- [ ] **Step 7: Review the implementation against the spec**

Check every requirement in `docs/superpowers/specs/2026-07-18-multilane-transport-design.md`, especially OpenCloud/Nextcloud control+bulk routing, encrypted lane IDs, the eight-message control cap, 3:1 weights, and idle-lane borrowing.

- [ ] **Step 8: Confirm the verified worktree is clean**

```bash
git status --short
```

Expected: no uncommitted implementation changes. If verification exposed a defect,
return to the task that owns that behavior, add a failing regression test, fix it,
rerun that task's focused and full verification, and commit the tested correction
before repeating this step.
