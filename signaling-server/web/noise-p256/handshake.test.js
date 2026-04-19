// signaling-server/web/noise-p256/handshake.test.js
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { NoiseXX } from './index.js'

test('full Noise_XX handshake: mutual encryption works in both directions', async () => {
  const initiator = await NoiseXX.createInitiator()
  const responderStatic = await (await import('./keys.js')).generateKeypair()
  const responder = await NoiseXX.createResponder(responderStatic.privateKey, responderStatic.publicKeyBytes)

  // Message 1: 65 bytes
  const msg1 = await initiator.writeMessage1()
  assert.strictEqual(msg1.length, 65)
  await responder.readMessage1(msg1)

  // Message 2: 162 bytes
  const msg2 = await responder.writeMessage2()
  assert.strictEqual(msg2.length, 162)
  await initiator.readMessage2(msg2)

  // Initiator must see responder's static pub after msg2
  assert.deepStrictEqual(initiator.remoteStaticPub, responderStatic.publicKeyBytes)

  // Message 3: 97 bytes
  const msg3 = await initiator.writeMessage3()
  assert.strictEqual(msg3.length, 97)
  await responder.readMessage3(msg3)

  // Split into cipher states
  const [iSend, iRecv] = await initiator.split()
  const [rSend, rRecv] = await responder.split()

  const plaintext = new TextEncoder().encode('hello from browser')
  const ct = await iSend.encrypt(new Uint8Array(0), plaintext)
  const pt = await rRecv.decrypt(new Uint8Array(0), ct)
  assert.deepStrictEqual(pt, plaintext)

  const reply = new TextEncoder().encode('hello from agent')
  const ct2 = await rSend.encrypt(new Uint8Array(0), reply)
  const pt2 = await iRecv.decrypt(new Uint8Array(0), ct2)
  assert.deepStrictEqual(pt2, reply)
})

test('message lengths match spec', async () => {
  const initiator = await NoiseXX.createInitiator()
  const rs = await (await import('./keys.js')).generateKeypair()
  const responder = await NoiseXX.createResponder(rs.privateKey, rs.publicKeyBytes)

  const msg1 = await initiator.writeMessage1()
  await responder.readMessage1(msg1)
  const msg2 = await responder.writeMessage2()
  await initiator.readMessage2(msg2)
  const msg3 = await initiator.writeMessage3()

  assert.strictEqual(msg1.length, 65,  'msg1 must be 65 bytes')
  assert.strictEqual(msg2.length, 162, 'msg2 must be 162 bytes')
  assert.strictEqual(msg3.length, 97,  'msg3 must be 97 bytes')
})

test('out-of-order calls are rejected', async () => {
  const initiator = await NoiseXX.createInitiator()
  await assert.rejects(() => initiator.writeMessage3())

  const rs = await (await import('./keys.js')).generateKeypair()
  const responder = await NoiseXX.createResponder(rs.privateKey, rs.publicKeyBytes)
  await assert.rejects(() => responder.writeMessage2())
})
