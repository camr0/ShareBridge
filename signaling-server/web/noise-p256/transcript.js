// signaling-server/web/noise-p256/transcript.js

const PROTOCOL_NAME = 'Noise_XX_P256_AESGCM_SHA256' // 27 bytes

export function initialize() {
  const h = new Uint8Array(32)
  h.set(new TextEncoder().encode(PROTOCOL_NAME)) // zero-padded by typed array init
  const ck = h.slice()
  return { h, ck }
}

export async function mixHash(h, data) {
  const input = new Uint8Array(h.length + data.length)
  input.set(h)
  input.set(data, h.length)
  const digest = await crypto.subtle.digest('SHA-256', input)
  return new Uint8Array(digest)
}

// Noise HKDF (§4.2): returns [out1, out2], each 32 bytes.
export async function hkdf2(ck, ikm) {
  const tempKey = await hmacSha256(ck, ikm)
  const out1 = await hmacSha256(tempKey, new Uint8Array([0x01]))
  const out2 = await hmacSha256(tempKey, concat(out1, new Uint8Array([0x02])))
  return [out1, out2]
}

async function hmacSha256(key, data) {
  const k = await crypto.subtle.importKey(
    'raw', key,
    { name: 'HMAC', hash: 'SHA-256' },
    false, ['sign']
  )
  const sig = await crypto.subtle.sign('HMAC', k, data)
  return new Uint8Array(sig)
}

function concat(...arrays) {
  const total = arrays.reduce((n, a) => n + a.length, 0)
  const out = new Uint8Array(total)
  let offset = 0
  for (const arr of arrays) { out.set(arr, offset); offset += arr.length }
  return out
}

export { hmacSha256, concat }