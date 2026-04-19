// signaling-server/web/noise-p256/cipher.test.js
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { CipherState, splitKeys } from './cipher.js'
import { initialize } from './transcript.js'

test('CipherState: round-trip encrypt/decrypt', async () => {
  const key = new Uint8Array(32).fill(0x42)
  const cs = new CipherState(key)
  const plaintext = new TextEncoder().encode('hello sharebridge')
  const ad = new TextEncoder().encode('associated-data')

  const ct = await cs.encryptWithAd(ad, plaintext)
  assert.strictEqual(ct.length, plaintext.length + 16)

  const cs2 = new CipherState(key)
  const pt = await cs2.decryptWithAd(ad, ct)
  assert.deepStrictEqual(pt, plaintext)
})

test('CipherState: nonce increments — same plaintext gives different ciphertext', async () => {
  const key = new Uint8Array(32).fill(0x11)
  const cs = new CipherState(key)
  const msg = new TextEncoder().encode('msg')
  const ct1 = await cs.encryptWithAd(null, msg)
  const ct2 = await cs.encryptWithAd(null, msg)
  assert.notDeepStrictEqual(ct1, ct2)
})

test('CipherState: wrong AD fails decryption', async () => {
  const key = new Uint8Array(32).fill(0x33)
  const cs = new CipherState(key)
  const ct = await cs.encryptWithAd(new TextEncoder().encode('correct'), new TextEncoder().encode('data'))
  const cs2 = new CipherState(key)
  await assert.rejects(() => cs2.decryptWithAd(new TextEncoder().encode('wrong'), ct))
})

test('splitKeys: returns two distinct 32-byte keys, deterministic', async () => {
  const { ck } = initialize()
  const [k1, k2] = await splitKeys(ck)
  assert.strictEqual(k1.length, 32)
  assert.strictEqual(k2.length, 32)
  assert.notDeepStrictEqual(k1, k2)

  const [k1b, k2b] = await splitKeys(ck)
  assert.deepStrictEqual(k1, k1b)
  assert.deepStrictEqual(k2, k2b)
})

test('CipherState: nonce exhaustion throws before wraparound', async () => {
  const key = new Uint8Array(32).fill(0x44)
  const cs = new CipherState(key)
  cs._setNonceForTest?.((2n ** 64n) - 1n)
  await assert.rejects(() => cs.encryptWithAd(new Uint8Array(0), new Uint8Array([0x01])))
})