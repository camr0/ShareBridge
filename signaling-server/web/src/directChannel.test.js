import { test } from 'node:test'
import assert from 'node:assert/strict'
import { DirectChannel, createDirectChannelSet, waitForDirectChannelOpen } from './directChannel.js'
import { TRAFFIC_CLASS_BULK, TRAFFIC_CLASS_THUMBNAIL } from './multiLaneProtocol.js'

function createMockRTCDataChannel(label = 'control') {
  return {
    label,
    bufferedAmount: 17,
    readyState: 'connecting',
    binaryType: 'blob',
    sent: [],
    onopen: null,
    onmessage: null,
    onclose: null,
    send(payload) {
      this.sent.push(payload)
    },
    closeCalled: false,
    close() {
      this.closeCalled = true
      this.readyState = 'closed'
    },
  }
}

test('direct set accepts required channels in arbitrary arrival order and waits for handshake', async () => {
  const set = createDirectChannelSet({ handshakeTimeoutMs: 100 })
  const bulk = createMockRTCDataChannel('bulk')
  const media = createMockRTCDataChannel('media')
  const control = createMockRTCDataChannel('control')

  set.accept(bulk)
  set.accept(media)
  set.accept(control)
  for (const rtc of [media, control, bulk]) {
    rtc.readyState = 'open'
    rtc.onopen()
  }

  assert.deepEqual(JSON.parse(control.sent[0]), { type: 'transport_hello', version: 2 })
  assert.equal(set.readyState, 'handshaking')
  control.onmessage({ data: JSON.stringify({ type: 'transport_ready', version: 2 }) })

  assert.equal(await set.ready, set)
  assert.equal(set.readyState, 'open')
  assert.equal(set.control.lane, 0)
  assert.equal(set.media.lane, 1)
  assert.equal(set.bulk.lane, 2)
})

test('direct set rejects duplicate and unknown required labels', async () => {
  const duplicateSet = createDirectChannelSet({ handshakeTimeoutMs: 100 })
  duplicateSet.accept(createMockRTCDataChannel('control'))
  assert.throws(() => duplicateSet.accept(createMockRTCDataChannel('control')), /duplicate/i)
  await assert.rejects(duplicateSet.ready, /duplicate/i)

  const unknownSet = createDirectChannelSet({ handshakeTimeoutMs: 100 })
  assert.throws(() => unknownSet.accept(createMockRTCDataChannel('data')), /unknown/i)
  await assert.rejects(unknownSet.ready, /unknown/i)
})

test('direct set reports missing lanes and tears down when any required lane closes', async () => {
  const missing = createDirectChannelSet({ handshakeTimeoutMs: 1 })
  missing.accept(createMockRTCDataChannel('control'))
  await assert.rejects(missing.ready, /missing required lanes: media, bulk/i)

  const set = createDirectChannelSet({ handshakeTimeoutMs: 100 })
  const channels = ['control', 'media', 'bulk'].map(createMockRTCDataChannel)
  channels.forEach((channel) => set.accept(channel))
  channels.forEach((channel) => {
    channel.readyState = 'open'
    channel.onopen()
  })
  channels[0].onmessage({ data: JSON.stringify({ type: 'transport_ready', version: 2 }) })
  await set.ready

  let closed = 0
  set.onclose = () => { closed += 1 }
  channels[1].readyState = 'closed'
  channels[1].onclose()
  channels[2].onclose()
  assert.equal(set.readyState, 'closed')
  assert.equal(closed, 1)
  assert.ok(channels.every((channel) => channel.closeCalled))
})

test('DirectChannel normalizes RTCDataChannel into the shared channel contract', () => {
  const rtc = createMockRTCDataChannel()
  const channel = new DirectChannel(rtc)
  const seen = []

  channel.onmessage = (event) => seen.push(event.data)
  channel.onopen = () => seen.push('open')
  channel.onclose = () => seen.push('close')

  rtc.readyState = 'open'
  rtc.onopen()
  rtc.onmessage({ data: 'hi' })
  rtc.onmessage({ data: new ArrayBuffer(3) })
  rtc.onclose()

  channel.send(JSON.stringify({ type: 'ping' }))
  channel.sendBinary(new Uint8Array([1, 2, 3]))
  channel.close()

  assert.deepEqual(seen, ['open', 'hi', new ArrayBuffer(3), 'close'])
  assert.equal(rtc.binaryType, 'arraybuffer')
  assert.equal(channel.bufferedAmount, 17)
  assert.equal(rtc.sent.length, 2)
  assert.equal(rtc.closeCalled, true)
})

test('direct lane endpoint sends on its physical channel and rejects another lane class', () => {
  const rtc = createMockRTCDataChannel('media')
  const channel = new DirectChannel(rtc)

  channel.sendBinaryClass(TRAFFIC_CLASS_THUMBNAIL, new Uint8Array([4, 5]))
  assert.deepEqual([...rtc.sent[0]], [4, 5])
  assert.throws(
    () => channel.sendBinaryClass(TRAFFIC_CLASS_BULK, new Uint8Array([6])),
    /does not map to direct lane/i
  )
  assert.equal(rtc.sent.length, 1)
})

test('waitForDirectChannelOpen resolves when the RTC data channel opens later', async () => {
  const rtc = createMockRTCDataChannel()
  const channel = new DirectChannel(rtc)

  const pending = waitForDirectChannelOpen(channel)
  rtc.readyState = 'open'
  rtc.onopen()

  const opened = await pending
  assert.equal(opened, channel)
})
