import { test } from 'node:test'
import assert from 'node:assert/strict'
import { NoiseXX } from '../noise-p256/index.js'
import { FRAME_HANDSHAKE, FRAME_TEXT, FRAME_BINARY, writeFrame, FrameDecoder } from './frame.js'
import { MAX_RELAY_PAYLOAD_BYTES, SecureRelayChannel } from './secureRelayChannel.js'
import { generateKeypair } from '../noise-p256/keys.js'
import {
  LANE_CONTROL,
  LANE_BULK,
  LANE_MEDIA,
  decodeLaneEnvelope,
  encodeLaneEnvelope,
} from './multiLaneProtocol.js'

function createMockRelaySocket() {
  const sent = []
  const listeners = { open: [], message: [], close: [], error: [] }
  const sendWaiters = []
  return {
    sent,
    readyState: 0,
    addEventListener(type, fn) { listeners[type].push(fn) },
    removeEventListener(type, fn) {
      listeners[type] = listeners[type].filter((candidate) => candidate !== fn)
    },
    send(bytes) {
      sent.push(bytes)
      while (sendWaiters.length > 0 && sent.length >= sendWaiters[0].count) {
        sendWaiters.shift().resolve()
      }
    },
    close() {
      this.readyState = 3
      for (const fn of listeners.close) fn()
    },
    open() {
      this.readyState = 1
      for (const fn of listeners.open) fn()
    },
    pushMessage(bytes) {
      for (const fn of listeners.message) fn({ data: bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength) })
    },
    fail(err) {
      for (const fn of listeners.error) fn(err)
    },
    waitForSentCount(count) {
      if (sent.length >= count) return Promise.resolve()
      return new Promise((resolve) => sendWaiters.push({ count, resolve }))
    },
  }
}

test('SecureRelayChannel performs Noise handshake, pins static key, and exchanges framed messages', async () => {
  const socket = createMockRelaySocket()
  const responderStatic = await generateKeypair()
  const responder = await NoiseXX.createResponder(responderStatic.privateKey, responderStatic.publicKeyBytes)
  const seen = []

  const channel = new SecureRelayChannel({
    relayURL: 'ws://relay.test/ws/relay',
    relayToken: 'browser-jwt',
    expectedStaticPub: responderStatic.publicKeyBytes,
    websocketFactory: () => socket,
  })
  channel.onopen = () => seen.push('open')
  channel.onmessage = (event) => seen.push(event.data)

  socket.readyState = 1
  const started = channel.start()
  await socket.waitForSentCount(2)
  assert.equal(socket.binaryType, 'arraybuffer')

  // Browser sent hello JSON, then msg1 as a handshake frame.
  const hello = new TextDecoder().decode(socket.sent[0])
  assert.match(hello, /browser-jwt/)
  const decoder = new FrameDecoder()
  const [{ kind: kind1, payload: msg1 }] = [...decoder.push(socket.sent[1])]
  assert.equal(kind1, FRAME_HANDSHAKE)

  await responder.readMessage1(msg1)
  const msg2 = await responder.writeMessage2()
  socket.pushMessage(writeFrame(FRAME_HANDSHAKE, msg2))
  await socket.waitForSentCount(3)

  const [{ payload: msg3 }] = [...decoder.push(socket.sent[2])]
  await responder.readMessage3(msg3)
  const [rSend, rRecv] = await responder.split()

  await socket.waitForSentCount(4)
  const [{ kind: helloKind, payload: helloCiphertext }] = [...decoder.push(socket.sent[3])]
  assert.equal(helloKind, FRAME_TEXT)
  const helloPlaintext = await rRecv.decrypt(new Uint8Array(0), helloCiphertext)
  const helloEnvelope = decodeLaneEnvelope(helloPlaintext)
  assert.equal(helloEnvelope.lane, LANE_CONTROL)
  assert.deepEqual(JSON.parse(new TextDecoder().decode(helloEnvelope.payload)), {
    type: 'transport_hello',
    version: 2,
  })
  const ready = encodeLaneEnvelope(
    LANE_CONTROL,
    new TextEncoder().encode(JSON.stringify({ type: 'transport_ready', version: 2 })),
  )
  socket.pushMessage(writeFrame(FRAME_TEXT, await rSend.encrypt(new Uint8Array(0), ready)))

  await started
  await channel.send(JSON.stringify({ type: 'ping' }))
  const [{ kind: frameKind, payload: ciphertext }] = [...decoder.push(socket.sent[4])]
  assert.equal(frameKind, FRAME_TEXT)
  const plaintext = await rRecv.decrypt(new Uint8Array(0), ciphertext)
  const envelope = decodeLaneEnvelope(plaintext)
  assert.equal(envelope.lane, LANE_CONTROL)
  assert.equal(new TextDecoder().decode(envelope.payload), '{"type":"ping"}')
  assert.deepEqual(seen, ['open'])
})

test('SecureRelayChannel exposes the required logical lane endpoints', () => {
  const socket = createMockRelaySocket()
  const channel = new SecureRelayChannel({
    relayURL: 'ws://relay.test/ws/relay',
    relayToken: 'browser-jwt',
    expectedStaticPub: new Uint8Array(65),
    websocketFactory: () => socket,
  })

  assert.ok(channel.control)
  assert.ok(channel.media)
  assert.ok(channel.bulk)
  assert.notEqual(channel.control, channel.media)
  assert.notEqual(channel.media, channel.bulk)
})

test('relay endpoints enforce plaintext cap before encryption and keep the session usable', async () => {
  const socket = createMockRelaySocket()
  socket.readyState = 1
  let encryptCalls = 0
  const channel = new SecureRelayChannel({
    relayURL: 'ws://relay.test', relayToken: 'token', expectedStaticPub: new Uint8Array(65), websocketFactory: () => socket,
  })
  channel._socket = socket
  channel._sendCipher = { async encrypt() { encryptCalls++; return new Uint8Array([1]) } }

  assert.throws(() => channel.control.send(new Uint8Array(MAX_RELAY_PAYLOAD_BYTES + 1)), /maximum/i)
  assert.equal(encryptCalls, 0)
  await channel.control.send(new Uint8Array(MAX_RELAY_PAYLOAD_BYTES))
  await channel.control.send(new Uint8Array([1]))
  assert.equal(encryptCalls, 2)
  assert.notEqual(channel.readyState, 'closed')
  channel.close()
})

test('media endpoint schedules interactive and thumbnail traffic three to one on the media lane', async () => {
  const socket = createMockRelaySocket()
  socket.readyState = 1
  const plaintexts = []
  const channel = new SecureRelayChannel({
    relayURL: 'ws://relay.test', relayToken: 'token', expectedStaticPub: new Uint8Array(65), websocketFactory: () => socket,
  })
  channel._socket = socket
  channel._sendCipher = { async encrypt(_ad, plaintext) { plaintexts.push(new Uint8Array(plaintext)); return new Uint8Array([1]) } }
  const interactive = Array.from({ length: 6 }, () => channel.media.send(new Uint8Array(64 * 1024).fill(0x10)))
  const thumbnails = Array.from({ length: 2 }, () => channel.media.sendThumbnail(new Uint8Array(64 * 1024).fill(0x20)))
  await Promise.all([...interactive, ...thumbnails])
  const decoded = plaintexts.map(decodeLaneEnvelope)
  assert.ok(decoded.every(({ lane }) => lane === LANE_MEDIA))
  assert.deepEqual(decoded.slice(0, 4).map(({ payload }) => payload[0]), [0x10, 0x10, 0x10, 0x20])
  assert.throws(() => channel.media.sendBinaryClass('bulk', new Uint8Array([1])), /does not map/i)
  channel.close()
})

test('relay adapter backpressure and writer failure reject every sender', async () => {
  const socket = createMockRelaySocket()
  socket.readyState = 1
  let rejectWriter
  const blocked = new Promise((_resolve, reject) => { rejectWriter = reject })
  let encryptCalls = 0
  const channel = new SecureRelayChannel({
    relayURL: 'ws://relay.test', relayToken: 'token', expectedStaticPub: new Uint8Array(65), websocketFactory: () => socket,
  })
  channel._socket = socket
  channel._sendCipher = { encrypt() { encryptCalls++; return blocked } }
  const sends = Array.from({ length: 5 }, () => channel.bulk.send(new Uint8Array(64 * 1024)))
  await new Promise((resolve) => setTimeout(resolve, 0))
  assert.equal(encryptCalls, 1)
  rejectWriter(new Error('writer failed'))
  const results = await Promise.allSettled(sends)
  assert.ok(results.every(({ status }) => status === 'rejected'))
  assert.equal(channel.readyState, 'closed')
})

test('start failure cleans socket, scheduler, timer, and close callback exactly once', async () => {
  let closes = 0
  const channel = new SecureRelayChannel({
    relayURL: 'ws://relay.test',
    relayToken: 'token',
    expectedStaticPub: new Uint8Array(65),
    websocketFactory: () => { throw new Error('factory failed') },
  })
  channel.onclose = () => { closes++ }
  await assert.rejects(() => channel.start(), /factory failed/i)
  channel.close()
  assert.equal(closes, 1)
  assert.equal(channel.readyState, 'closed')
  await assert.rejects(() => channel.control.send(new Uint8Array([1])), /closed/i)
})

test('SecureRelayChannel fails closed on responder static-key mismatch', async () => {
  const socket = createMockRelaySocket()
  const responderStatic = await generateKeypair()
  const wrongStatic = await generateKeypair()
  const responder = await NoiseXX.createResponder(responderStatic.privateKey, responderStatic.publicKeyBytes)
  const channel = new SecureRelayChannel({
    relayURL: 'ws://relay.test/ws/relay',
    relayToken: 'browser-jwt',
    expectedStaticPub: wrongStatic.publicKeyBytes,
    websocketFactory: () => socket,
  })
  let closeCalls = 0
  channel.onclose = () => { closeCalls++ }

  socket.readyState = 1
  const started = channel.start()
  await socket.waitForSentCount(2)

  const decoder = new FrameDecoder()
  const [{ payload: msg1 }] = [...decoder.push(socket.sent[1])]
  await responder.readMessage1(msg1)
  socket.pushMessage(writeFrame(FRAME_HANDSHAKE, await responder.writeMessage2()))

  await assert.rejects(() => started, /unexpected agent static key/i)
  channel.close()
  assert.equal(closeCalls, 1)
})

test('SecureRelayChannel reports one close after websocket opens', async () => {
  const socket = createMockRelaySocket()
  const responderStatic = await generateKeypair()
  const responder = await NoiseXX.createResponder(responderStatic.privateKey, responderStatic.publicKeyBytes)
  let closeCalls = 0
  const channel = new SecureRelayChannel({
    relayURL: 'ws://relay.test/ws/relay',
    relayToken: 'browser-jwt',
    expectedStaticPub: responderStatic.publicKeyBytes,
    websocketFactory: () => socket,
  })
  channel.onclose = () => {
    closeCalls += 1
  }

  socket.readyState = 1
  const started = channel.start()
  await socket.waitForSentCount(2)

  const decoder = new FrameDecoder()
  const [{ payload: msg1 }] = [...decoder.push(socket.sent[1])]
  await responder.readMessage1(msg1)
  socket.pushMessage(writeFrame(FRAME_HANDSHAKE, await responder.writeMessage2()))
  await socket.waitForSentCount(3)

  const [{ payload: msg3 }] = [...decoder.push(socket.sent[2])]
  await responder.readMessage3(msg3)
  const [rSend, rRecv] = await responder.split()
  await socket.waitForSentCount(4)
  const [{ payload: helloCiphertext }] = [...decoder.push(socket.sent[3])]
  await rRecv.decrypt(new Uint8Array(0), helloCiphertext)
  const ready = encodeLaneEnvelope(
    LANE_CONTROL,
    new TextEncoder().encode(JSON.stringify({ type: 'transport_ready', version: 2 })),
  )
  socket.pushMessage(writeFrame(FRAME_TEXT, await rSend.encrypt(new Uint8Array(0), ready)))
  await started

  socket.close()
  assert.equal(closeCalls, 1)
})

test('SecureRelayChannel encrypts lane ids and dispatches decrypted media', async () => {
  const socket = createMockRelaySocket()
  const responderStatic = await generateKeypair()
  const responder = await NoiseXX.createResponder(responderStatic.privateKey, responderStatic.publicKeyBytes)
  const channel = new SecureRelayChannel({
    relayURL: 'ws://relay.test/ws/relay',
    relayToken: 'browser-jwt',
    expectedStaticPub: responderStatic.publicKeyBytes,
    websocketFactory: () => socket,
  })
  socket.readyState = 1
  const started = channel.start()
  await socket.waitForSentCount(2)
  const decoder = new FrameDecoder()
  const [{ payload: msg1 }] = [...decoder.push(socket.sent[1])]
  await responder.readMessage1(msg1)
  socket.pushMessage(writeFrame(FRAME_HANDSHAKE, await responder.writeMessage2()))
  await socket.waitForSentCount(3)
  const [{ payload: msg3 }] = [...decoder.push(socket.sent[2])]
  await responder.readMessage3(msg3)
  const [rSend, rRecv] = await responder.split()
  await socket.waitForSentCount(4)
  const [{ payload: helloCiphertext }] = [...decoder.push(socket.sent[3])]
  await rRecv.decrypt(new Uint8Array(0), helloCiphertext)
  const ready = encodeLaneEnvelope(LANE_CONTROL, new TextEncoder().encode('{"type":"transport_ready","version":2}'))
  socket.pushMessage(writeFrame(FRAME_TEXT, await rSend.encrypt(new Uint8Array(0), ready)))
  await started

  const received = new Promise((resolve) => { channel.media.onmessage = (event) => resolve(new Uint8Array(event.data)) })
  const inbound = encodeLaneEnvelope(LANE_MEDIA, new Uint8Array([7, 8, 9]))
  socket.pushMessage(writeFrame(FRAME_BINARY, await rSend.encrypt(new Uint8Array(0), inbound)))
  assert.deepEqual(await received, new Uint8Array([7, 8, 9]))

  await channel.media.send(new Uint8Array([4, 5]))
  const [{ kind, payload: outboundCiphertext }] = [...decoder.push(socket.sent[4])]
  assert.equal(kind, FRAME_BINARY)
  assert.notEqual(outboundCiphertext[0], LANE_MEDIA)
  const outbound = decodeLaneEnvelope(await rRecv.decrypt(new Uint8Array(0), outboundCiphertext))
  assert.equal(outbound.lane, LANE_MEDIA)
  assert.deepEqual(outbound.payload, new Uint8Array([4, 5]))

  const firstConcurrentFrame = socket.sent.length
  const mediaSends = Array.from({ length: 6 }, (_, index) => {
    const bytes = new Uint8Array(64 * 1024)
    bytes[0] = index
    return channel.media.send(bytes)
  })
  const bulkSends = Array.from({ length: 2 }, (_, index) => {
    const bytes = new Uint8Array(64 * 1024)
    bytes[0] = index
    return channel.bulk.send(bytes)
  })
  await Promise.all([...mediaSends, ...bulkSends])
  const scheduledLanes = []
  for (const frameBytes of socket.sent.slice(firstConcurrentFrame)) {
    const [{ payload: scheduledCiphertext }] = [...decoder.push(frameBytes)]
    const scheduled = decodeLaneEnvelope(await rRecv.decrypt(new Uint8Array(0), scheduledCiphertext))
    scheduledLanes.push(scheduled.lane)
  }
  assert.deepEqual(scheduledLanes.slice(0, 4), [LANE_MEDIA, LANE_MEDIA, LANE_MEDIA, LANE_BULK])

  const closed = new Promise((resolve) => { channel.onclose = resolve })
  const invalid = await rSend.encrypt(new Uint8Array(0), new Uint8Array([0x03, 1]))
  socket.pushMessage(writeFrame(FRAME_BINARY, invalid))
  await closed
  assert.equal(channel.readyState, 'closed')
  assert.equal(channel.control.readyState, 'closed')
  assert.equal(channel.media.readyState, 'closed')
  assert.equal(channel.bulk.readyState, 'closed')
})
