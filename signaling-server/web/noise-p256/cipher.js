// signaling-server/web/noise-p256/cipher.js
import { hkdf2 } from './transcript.js'

export class CipherState {
  #key
  #n = 0n

  constructor(key) {
    this.#key = key
  }

  async encrypt(ad, plaintext) {
    return this.encryptWithAd(ad, plaintext)
  }

  async decrypt(ad, ciphertext) {
    return this.decryptWithAd(ad, ciphertext)
  }

  async encryptWithAd(ad, plaintext) {
    this.#assertInitializedKey()
    if (this.#n === (2n ** 64n) - 1n) {
      throw new Error('noise: nonce exhausted')
    }
    const k = await crypto.subtle.importKey('raw', this.#key, { name: 'AES-GCM' }, false, ['encrypt'])
    const iv = nonceBytes(this.#n)
    const params = { name: 'AES-GCM', iv, ...(ad && ad.length ? { additionalData: ad } : {}) }
    const ct = await crypto.subtle.encrypt(params, k, plaintext)
    this.#n += 1n
    return new Uint8Array(ct)
  }

  async decryptWithAd(ad, ciphertext) {
    this.#assertInitializedKey()
    if (this.#n === (2n ** 64n) - 1n) {
      throw new Error('noise: nonce exhausted')
    }
    const k = await crypto.subtle.importKey('raw', this.#key, { name: 'AES-GCM' }, false, ['decrypt'])
    const iv = nonceBytes(this.#n)
    const params = { name: 'AES-GCM', iv, ...(ad && ad.length ? { additionalData: ad } : {}) }
    try {
      const pt = await crypto.subtle.decrypt(params, k, ciphertext)
      this.#n += 1n
      return new Uint8Array(pt)
    } catch {
      throw new Error('noise: AEAD authentication failed')
    }
  }

  // Expose key for interop test vector generation only
  get keyBytes() { return this.#key }
  _setNonceForTest(value) { this.#n = value }

  #assertInitializedKey() {
    if (isZeroKey(this.#key)) {
      throw new Error('noise: cipher key not initialized')
    }
  }
}

// splitKeys: Noise Split using empty input (NOT zeros(32)).
export async function splitKeys(ck) {
  const [k1, k2] = await hkdf2(ck, new Uint8Array(0))
  return [k1, k2]
}

// nonceBytes: 4 zero bytes || n as uint64 big-endian = 12 bytes total.
function nonceBytes(n) {
  const buf = new Uint8Array(12)
  const view = new DataView(buf.buffer)
  view.setBigUint64(4, BigInt(n), false) // big-endian at offset 4
  return buf
}

function isZeroKey(key) {
  for (const byte of key) {
    if (byte !== 0) return false
  }
  return true
}
