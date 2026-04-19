// signaling-server/web/noise-p256/keys.test.js
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { generateKeypair, dh, importPublicKey, exportPublicKey } from './keys.js'

test('generateKeypair: public key is 65-byte uncompressed P-256', async () => {
  const kp = await generateKeypair()
  assert.strictEqual(kp.publicKeyBytes.length, 65)
  assert.strictEqual(kp.publicKeyBytes[0], 0x04)
})

test('dh: shared secret is 32 bytes and commutative', async () => {
  const alice = await generateKeypair()
  const bob = await generateKeypair()

  const sharedAB = await dh(alice.privateKey, bob.publicKeyBytes)
  const sharedBA = await dh(bob.privateKey, alice.publicKeyBytes)

  assert.strictEqual(sharedAB.length, 32)
  assert.deepStrictEqual(sharedAB, sharedBA)
})

test('importPublicKey: round-trips through exportPublicKey', async () => {
  const kp = await generateKeypair()
  const imported = await importPublicKey(kp.publicKeyBytes)
  const exported = await exportPublicKey(imported)
  assert.deepStrictEqual(exported, kp.publicKeyBytes)
})

test('importPrivateKeyFromScalar: fixed scalar gives deterministic public key', async () => {
  // Use a known-safe P-256 scalar
  const scalar = new Uint8Array(32)
  scalar[31] = 0x01 // scalar = 1 — valid but weak (test only)
  // Actually scalar=1 gives the generator point G; valid for import
  const { importPrivateKeyFromScalar } = await import('./keys.js')
  const priv1 = await importPrivateKeyFromScalar(scalar)
  const priv2 = await importPrivateKeyFromScalar(scalar)
  const pub1 = await exportPublicKey(priv1.publicKey)
  const pub2 = await exportPublicKey(priv2.publicKey)
  assert.deepStrictEqual(pub1, pub2)
})