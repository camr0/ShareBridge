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

  const ct = await cs.encrypt(ad, plaintext)
  assert.strictEqual(ct.length, plaintext.length + 16)

  const cs2 = new CipherState(key)
  const pt = await cs2.decrypt(ad, ct)
  assert.deepStrictEqual(pt, plaintext)
})

test('CipherState: nonce increments — same plaintext gives different ciphertext', async () => {
  const key = new Uint8Array(32).fill(0x11)
  const cs = new CipherState(key)
  const msg = new TextEncoder().encode('msg')
  const ct1 = await cs.encrypt(new Uint8Array(0), msg)
  const ct2 = await cs.encrypt(new Uint8Array(0), msg)
  assert.notDeepStrictEqual(ct1, ct2)
})

test('CipherState: wrong AD fails decryption', async () => {
  const key = new Uint8Array(32).fill(0x33)
  const cs = new CipherState(key)
  const ct = await cs.encrypt(new TextEncoder().encode('correct'), new TextEncoder().encode('data'))
  const cs2 = new CipherState(key)
  await assert.rejects(() => cs2.decrypt(new TextEncoder().encode('wrong'), ct))
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
  await assert.rejects(() => cs.encrypt(new Uint8Array(0), new Uint8Array([0x01])))
})

test('CipherState: zero key is rejected as uninitialized', async () => {
  const cs = new CipherState(new Uint8Array(32))
  await assert.rejects(() => cs.encrypt(new Uint8Array(0), new Uint8Array([0x01])))
})

test('CipherState: concurrent decrypts are serialized to preserve nonce order', async () => {
  const key = new Uint8Array(32).fill(0x55)
  const sender = new CipherState(key)
  const receiver = new CipherState(key)
  const ad = new Uint8Array(0)
  const plaintext1 = new TextEncoder().encode('first')
  const plaintext2 = new TextEncoder().encode('second')
  const ct1 = await sender.encrypt(ad, plaintext1)
  const ct2 = await sender.encrypt(ad, plaintext2)

  const originalDecrypt = crypto.subtle.decrypt
  let decryptCalls = 0
  let releaseFirstDecrypt
  const firstDecryptBlocked = new Promise((resolve) => {
    releaseFirstDecrypt = resolve
  })

  crypto.subtle.decrypt = async function (...args) {
    decryptCalls += 1
    if (decryptCalls === 1) {
      await firstDecryptBlocked
    }
    return originalDecrypt.apply(this, args)
  }

  try {
    const p1 = receiver.decrypt(ad, ct1)
    const p2 = receiver.decrypt(ad, ct2)

    await new Promise((resolve) => setTimeout(resolve, 0))
    assert.equal(decryptCalls, 1, 'expected second decrypt to wait for the first nonce to finish')

    releaseFirstDecrypt()
    const [pt1, pt2] = await Promise.all([p1, p2])
    assert.deepEqual(pt1, plaintext1)
    assert.deepEqual(pt2, plaintext2)
  } finally {
    crypto.subtle.decrypt = originalDecrypt
  }
})
