// signaling-server/web/noise-p256/transcript.test.js
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { initialize, mixHash, hkdf2 } from './transcript.js'

test('initialize: h and ck are 27-byte protocol name zero-padded to 32', async () => {
  const { h, ck } = initialize()
  assert.strictEqual(h.length, 32)
  assert.strictEqual(ck.length, 32)

  const name = new TextEncoder().encode('Noise_XX_P256_AESGCM_SHA256')
  const expected = new Uint8Array(32)
  expected.set(name)

  assert.deepStrictEqual(h, expected)
  assert.deepStrictEqual(ck, expected)
})

test('initialize: h and ck are equal but independent copies', () => {
  const { h, ck } = initialize()
  assert.deepStrictEqual(h, ck)
  h[0] = 0xff // mutate h
  assert.notStrictEqual(h[0], ck[0]) // ck must not be affected
})

test('mixHash: deterministic and changes h', async () => {
  const { h } = initialize()
  const h2 = await mixHash(h, new Uint8Array(0))
  assert.strictEqual(h2.length, 32)
  assert.notDeepStrictEqual(h2, h)

  const h3 = await mixHash(h, new Uint8Array(0))
  assert.deepStrictEqual(h2, h3) // deterministic
})

test('mixHash: different data produces different output', async () => {
  const { h } = initialize()
  const h1 = await mixHash(h, new Uint8Array([0x01]))
  const h2 = await mixHash(h, new Uint8Array([0x02]))
  assert.notDeepStrictEqual(h1, h2)
})

test('hkdf2: returns two distinct 32-byte outputs', async () => {
  const { ck } = initialize()
  const ikm = new Uint8Array(32).fill(0xab)
  const [out1, out2] = await hkdf2(ck, ikm)
  assert.strictEqual(out1.length, 32)
  assert.strictEqual(out2.length, 32)
  assert.notDeepStrictEqual(out1, out2)
  assert.notDeepStrictEqual(out1, ck)
})

test('hkdf2: deterministic', async () => {
  const { ck } = initialize()
  const ikm = new Uint8Array(32).fill(0xcd)
  const [a1, a2] = await hkdf2(ck, ikm)
  const [b1, b2] = await hkdf2(ck, ikm)
  assert.deepStrictEqual(a1, b1)
  assert.deepStrictEqual(a2, b2)
})