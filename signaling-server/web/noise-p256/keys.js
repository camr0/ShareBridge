// signaling-server/web/noise-p256/keys.js

export async function generateKeypair() {
  const pair = await crypto.subtle.generateKey(
    { name: 'ECDH', namedCurve: 'P-256' },
    true,
    ['deriveBits']
  )
  const rawPub = await crypto.subtle.exportKey('raw', pair.publicKey) // 65 bytes uncompressed
  return {
    privateKey: pair.privateKey,
    publicKey: pair.publicKey,
    publicKeyBytes: new Uint8Array(rawPub),
  }
}

// dh performs P-256 ECDH. publicKeyBytes is the 65-byte uncompressed form.
// Returns the 32-byte shared secret (x-coordinate).
export async function dh(privateKey, publicKeyBytes) {
  const pubKey = await importPublicKey(publicKeyBytes)
  const shared = await crypto.subtle.deriveBits(
    { name: 'ECDH', public: pubKey },
    privateKey,
    256
  )
  return new Uint8Array(shared)
}

export async function importPublicKey(rawBytes) {
  return crypto.subtle.importKey(
    'raw', rawBytes,
    { name: 'ECDH', namedCurve: 'P-256' },
    true,
    []
  )
}

export async function exportPublicKey(cryptoKey) {
  const raw = await crypto.subtle.exportKey('raw', cryptoKey)
  return new Uint8Array(raw)
}

// importPrivateKeyFromScalar imports a P-256 private key from a raw 32-byte scalar (big-endian).
// Uses PKCS8 import since Web Crypto requires full key format (not raw scalar).
export async function importPrivateKeyFromScalar(scalar32) {
  // Build PKCS8 DER structure wrapping ECPrivateKey SEC1
  const pkcs8 = buildP256PKCS8(scalar32)

  // Import as PKCS8 - Web Crypto derives public key from scalar
  const privateKey = await crypto.subtle.importKey(
    'pkcs8',
    pkcs8,
    { name: 'ECDH', namedCurve: 'P-256' },
    true,
    ['deriveBits']
  )

  // Export as JWK to get computed x,y, then construct public key
  const fullJwk = await crypto.subtle.exportKey('jwk', privateKey)
  const publicKey = await crypto.subtle.importKey(
    'jwk',
    { kty: 'EC', crv: 'P-256', x: fullJwk.x, y: fullJwk.y },
    { name: 'ECDH', namedCurve: 'P-256' },
    true,
    []
  )

  const publicKeyBytes = await exportPublicKey(publicKey)

  return { privateKey, publicKey, publicKeyBytes }
}

// buildP256PKCS8 constructs a minimal PKCS8 DER for P-256 with a given 32-byte scalar.
// PKCS8: SEQUENCE { version=0, AlgorithmIdentifier, privateKey OCTET STRING wrapping ECPrivateKey }
function buildP256PKCS8(scalar32) {
  // ECPrivateKey SEC1 structure (without publicKey [1] field)
  // SEQUENCE { version=1, privateKey OCTET STRING, parameters [0] OID }
  const ecPrivKey = new Uint8Array([
    0x30, 0x31,             // SEQUENCE, 49 bytes
      0x02, 0x01, 0x01,     // INTEGER version = 1
      0x04, 0x20,           // OCTET STRING, 32 bytes
      ...scalar32,          // the scalar
      0xa0, 0x0a,           // [0] EXPLICIT - curve parameters
        0x06, 0x08,         // OID, 8 bytes (secp256r1 = 1.2.840.10045.3.1.7)
        0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07
  ])

  // AlgorithmIdentifier for ecPublicKey + P-256
  // SEQUENCE { OID ecPublicKey, OID P-256 }
  const algId = new Uint8Array([
    0x30, 0x13,             // SEQUENCE, 19 bytes
      0x06, 0x07,           // OID ecPublicKey = 1.2.840.10045.2.1 (7 bytes)
      0x2a, 0x86, 0x48, 0xce, 0x3d, 0x02, 0x01,
      0x06, 0x08,           // OID secp256r1 = 1.2.840.10045.3.1.7 (8 bytes)
      0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07
  ])

  // PKCS8: SEQUENCE { version=0, algorithmIdentifier, privateKey OCTET STRING }
  const totalLen = 3 + algId.length + 2 + ecPrivKey.length
  const pkcs8 = new Uint8Array([
    0x30, totalLen & 0x7f,  // SEQUENCE (assuming length < 128)
      0x02, 0x01, 0x00,     // INTEGER version = 0
      ...algId,
      0x04, ecPrivKey.length, // OCTET STRING wrapping ECPrivateKey
      ...ecPrivKey
  ])

  return pkcs8
}