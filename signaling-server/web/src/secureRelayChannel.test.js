import { test } from 'node:test'
import assert from 'node:assert/strict'
import { NoiseXX } from '../noise-p256/index.js'
import { FRAME_HANDSHAKE, FRAME_TEXT, FRAME_BINARY, writeFrame, FrameDecoder } from './frame.js'
import { SecureRelayChannel } from './secureRelayChannel.js'
import { generateKeypair } from '../noise-p256/keys.js'

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
  const [, rRecv] = await responder.split()

  await started
  await channel.send(JSON.stringify({ type: 'ping' }))
  const [{ kind: frameKind, payload: ciphertext }] = [...decoder.push(socket.sent[3])]
  assert.equal(frameKind, FRAME_TEXT)
  const plaintext = await rRecv.decrypt(new Uint8Array(0), ciphertext)
  assert.equal(new TextDecoder().decode(plaintext), '{"type":"ping"}')
  assert.deepEqual(seen, ['open'])
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

  socket.readyState = 1
  const started = channel.start()
  await socket.waitForSentCount(2)

  const decoder = new FrameDecoder()
  const [{ payload: msg1 }] = [...decoder.push(socket.sent[1])]
  await responder.readMessage1(msg1)
  socket.pushMessage(writeFrame(FRAME_HANDSHAKE, await responder.writeMessage2()))

  await assert.rejects(() => started, /unexpected agent static key/i)
})

test('SecureRelayChannel removes first-phase close handler after websocket opens', async () => {
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
  await started

  socket.close()
  assert.equal(closeCalls, 0)
})
