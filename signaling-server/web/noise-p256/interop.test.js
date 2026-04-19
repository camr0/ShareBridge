// signaling-server/web/noise-p256/interop.test.js
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'

const __dirname = dirname(fileURLToPath(import.meta.url))
const vectorPath = join(__dirname, '../../../agent/internal/noise/testdata/interop_vector.json')
const vec = JSON.parse(readFileSync(vectorPath, 'utf8'))

function fromHex(hex) {
  const bytes = new Uint8Array(hex.length / 2)
  for (let i = 0; i < bytes.length; i++) bytes[i] = parseInt(hex.slice(i*2, i*2+2), 16)
  return bytes
}

import { CipherState } from './cipher.js'

test('interop: JS can decrypt Go-generated ciphertext (initiator->responder)', async () => {
  // k1 is initiator->responder key; responder uses k1 to decrypt
  const k1 = fromHex(vec.k1)
  const rRecv = new CipherState(k1)

  const ct = fromHex(vec.sample_ct_initiator)
  const pt = await rRecv.decryptWithAd(new Uint8Array(0), ct)
  assert.strictEqual(new TextDecoder().decode(pt), vec.sample_plaintext)
})

test('interop: JS can decrypt Go-generated ciphertext (responder->initiator)', async () => {
  // k2 is responder->initiator key; initiator uses k2 to decrypt
  const k2 = fromHex(vec.k2)
  const iRecv = new CipherState(k2)

  const ct = fromHex(vec.sample_ct_responder)
  const pt = await iRecv.decryptWithAd(new Uint8Array(0), ct)
  assert.strictEqual(new TextDecoder().decode(pt), vec.sample_plaintext)
})

test('interop: JS derives same k1/k2 from same fixed keys (PRIMARY)', async () => {
  // This is the primary interop gate. JS must reproduce the full deterministic
  // transcript from the Go-generated vector, not just decrypt ciphertext with
  // already-derived keys.
  const { importPrivateKeyFromScalar } = await import('./keys.js')
  const { NoiseXX } = await import('./index.js')

  const iEPriv = await importPrivateKeyFromScalar(fromHex(vec.initiator_ephemeral_priv))
  const rEPriv = await importPrivateKeyFromScalar(fromHex(vec.responder_ephemeral_priv))
  const iSPriv = await importPrivateKeyFromScalar(fromHex(vec.initiator_static_priv))
  const rSPriv = await importPrivateKeyFromScalar(fromHex(vec.responder_static_priv))

  const initiator = await NoiseXX.createInitiator({ staticKeypair: iSPriv, ephemeralKeypair: iEPriv })
  const responder = await NoiseXX.createResponder(rSPriv.privateKey, rSPriv.publicKeyBytes, { ephemeralKeypair: rEPriv })

  // Message 1: initiator -> responder (65 bytes)
  const msg1 = await initiator.writeMessage1()
  assert.strictEqual(Buffer.from(msg1).toString('hex'), vec.msg1, 'msg1 transcript mismatch')
  await responder.readMessage1(msg1)

  // Message 2: responder -> initiator (162 bytes)
  const msg2 = await responder.writeMessage2()
  assert.strictEqual(Buffer.from(msg2).toString('hex'), vec.msg2, 'msg2 transcript mismatch')
  await initiator.readMessage2(msg2)

  // Message 3: initiator -> responder (97 bytes)
  const msg3 = await initiator.writeMessage3()
  assert.strictEqual(Buffer.from(msg3).toString('hex'), vec.msg3, 'msg3 transcript mismatch')
  await responder.readMessage3(msg3)

  // Split and verify derived keys match Go exactly
  const [iSend, iRecv] = await initiator.split()
  const [rSend, rRecv] = await responder.split()

  // initiator sends on k1, receives on k2
  assert.strictEqual(Buffer.from(iSend.keyBytes).toString('hex'), vec.k1, 'k1 mismatch (initiator send)')
  assert.strictEqual(Buffer.from(iRecv.keyBytes).toString('hex'), vec.k2, 'k2 mismatch (initiator recv)')

  // responder sends on k2, receives on k1
  assert.strictEqual(Buffer.from(rRecv.keyBytes).toString('hex'), vec.k1, 'k1 mismatch (responder recv)')
  assert.strictEqual(Buffer.from(rSend.keyBytes).toString('hex'), vec.k2, 'k2 mismatch (responder send)')
})