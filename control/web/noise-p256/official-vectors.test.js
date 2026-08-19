import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { CipherState, splitKeys } from './cipher.js'
import { dh, importPublicKey, importPrivateKeyFromScalar } from './keys.js'
import { hkdf2, mixHash } from './transcript.js'

const EMPTY = new Uint8Array(0)
const encoder = new TextEncoder()

test('NIST P-256 vectors: dh matches official ZIUT outputs', async () => {
  const cases = parseNistP256Cases(
    await readFixtureText('KAS_ECC_CDH_PrimitiveTest.txt')
  )
  assert.ok(cases.length > 0)

  for (const tc of cases) {
    const priv = await importPrivateKeyFromScalar(hexToBytes(tc.dIUT))
    const expectedPub = concat(
      new Uint8Array([0x04]),
      hexToBytes(tc.QIUTx),
      hexToBytes(tc.QIUTy)
    )
    assert.deepStrictEqual(priv.publicKeyBytes, expectedPub, `COUNT ${tc.COUNT}: public key`)

    const peerPub = concat(
      new Uint8Array([0x04]),
      hexToBytes(tc.QCAVSx),
      hexToBytes(tc.QCAVSy)
    )
    const shared = await dh(priv.privateKey, peerPub)
    assert.deepStrictEqual(shared, hexToBytes(tc.ZIUT), `COUNT ${tc.COUNT}: shared secret`)
  }
})

test('official Noise XX 25519 AESGCM SHA256 vector replays exactly', async () => {
  const vector = await loadOfficialNoiseVector()

  const initStatic = hexToBytes(vector.init_static)
  const initEphemeral = hexToBytes(vector.init_ephemeral)
  const respStatic = hexToBytes(vector.resp_static)
  const respEphemeral = hexToBytes(vector.resp_ephemeral)
  const initStaticPub = await x25519PublicKey(initStatic)
  const initEphemeralPub = await x25519PublicKey(initEphemeral)
  const respStaticPub = await x25519PublicKey(respStatic)
  const respEphemeralPub = await x25519PublicKey(respEphemeral)

  const initState = new OfficialSymmetricState('Noise_XX_25519_AESGCM_SHA256')
  const respState = new OfficialSymmetricState('Noise_XX_25519_AESGCM_SHA256')

  const prologue = hexToBytes(vector.init_prologue)
  assert.deepStrictEqual(prologue, hexToBytes(vector.resp_prologue))
  await initState.mixHash(prologue)
  await respState.mixHash(prologue)

  const msg1Payload = hexToBytes(vector.messages[0].payload)
  let msg1 = concat(initEphemeralPub)
  await initState.mixHash(initEphemeralPub)
  msg1 = concat(msg1, await initState.encryptAndHash(msg1Payload))
  assert.deepStrictEqual(msg1, hexToBytes(vector.messages[0].ciphertext))

  await respState.mixHash(msg1.subarray(0, 32))
  assert.deepStrictEqual(await respState.decryptAndHash(msg1.subarray(32)), msg1Payload)

  const msg2Payload = hexToBytes(vector.messages[1].payload)
  let msg2 = concat(respEphemeralPub)
  await respState.mixHash(respEphemeralPub)
  await respState.mixKey(await x25519(respEphemeral, initEphemeralPub))
  msg2 = concat(msg2, await respState.encryptAndHash(respStaticPub))
  await respState.mixKey(await x25519(respStatic, initEphemeralPub))
  msg2 = concat(msg2, await respState.encryptAndHash(msg2Payload))
  assert.deepStrictEqual(msg2, hexToBytes(vector.messages[1].ciphertext))

  const msg2ResponderEphemeral = msg2.subarray(0, 32)
  await initState.mixHash(msg2ResponderEphemeral)
  await initState.mixKey(await x25519(initEphemeral, respEphemeralPub))
  const msg2ResponderStatic = await initState.decryptAndHash(msg2.subarray(32, 80))
  assert.deepStrictEqual(msg2ResponderStatic, respStaticPub)
  await initState.mixKey(await x25519(initEphemeral, msg2ResponderStatic))
  assert.deepStrictEqual(await initState.decryptAndHash(msg2.subarray(80)), msg2Payload)

  const msg3Payload = hexToBytes(vector.messages[2].payload)
  let msg3 = await initState.encryptAndHash(initStaticPub)
  await initState.mixKey(await x25519(initStatic, respEphemeralPub))
  msg3 = concat(msg3, await initState.encryptAndHash(msg3Payload))
  assert.deepStrictEqual(msg3, hexToBytes(vector.messages[2].ciphertext))

  const msg3InitiatorStatic = await respState.decryptAndHash(msg3.subarray(0, 48))
  assert.deepStrictEqual(msg3InitiatorStatic, initStaticPub)
  await respState.mixKey(await x25519(respEphemeral, msg3InitiatorStatic))
  assert.deepStrictEqual(await respState.decryptAndHash(msg3.subarray(48)), msg3Payload)

  const [iC1, iC2] = await initState.split()
  const [rC1, rC2] = await respState.split()

  const transportPayload1 = hexToBytes(vector.messages[3].payload)
  const transportCiphertext1 = await rC2.encrypt(EMPTY, transportPayload1)
  assert.deepStrictEqual(transportCiphertext1, hexToBytes(vector.messages[3].ciphertext))
  assert.deepStrictEqual(await iC2.decrypt(EMPTY, transportCiphertext1), transportPayload1)

  const transportPayload2 = hexToBytes(vector.messages[4].payload)
  const transportCiphertext2 = await iC1.encrypt(EMPTY, transportPayload2)
  assert.deepStrictEqual(transportCiphertext2, hexToBytes(vector.messages[4].ciphertext))
  assert.deepStrictEqual(await rC1.decrypt(EMPTY, transportCiphertext2), transportPayload2)

  const transportPayload3 = hexToBytes(vector.messages[5].payload)
  const transportCiphertext3 = await rC2.encrypt(EMPTY, transportPayload3)
  assert.deepStrictEqual(transportCiphertext3, hexToBytes(vector.messages[5].ciphertext))
  assert.deepStrictEqual(await iC2.decrypt(EMPTY, transportCiphertext3), transportPayload3)
})

class OfficialSymmetricState {
  constructor(protocolName) {
    const { h, ck } = initializeWithProtocolName(protocolName)
    this.h = h
    this.ck = ck
    this.cs = null
  }

  async mixHash(data) {
    this.h = await mixHash(this.h, data)
  }

  async mixKey(ikm) {
    const [ck, key] = await hkdf2(this.ck, ikm)
    this.ck = ck
    this.cs = new CipherState(key)
  }

  async encryptAndHash(plaintext) {
    if (this.cs == null) {
      const out = new Uint8Array(plaintext)
      await this.mixHash(out)
      return out
    }
    const ciphertext = await this.cs.encrypt(this.h, plaintext)
    await this.mixHash(ciphertext)
    return ciphertext
  }

  async decryptAndHash(ciphertext) {
    if (this.cs == null) {
      const out = new Uint8Array(ciphertext)
      await this.mixHash(out)
      return out
    }
    const plaintext = await this.cs.decrypt(this.h, ciphertext)
    await this.mixHash(ciphertext)
    return plaintext
  }

  async split() {
    const [k1, k2] = await splitKeys(this.ck)
    return [new CipherState(k1), new CipherState(k2)]
  }
}

function initializeWithProtocolName(protocolName) {
  const nameBytes = encoder.encode(protocolName)
  if (nameBytes.length <= 32) {
    const h = new Uint8Array(32)
    h.set(nameBytes)
    return { h, ck: h.slice() }
  }
  throw new Error('long protocol names are not needed in this test harness')
}

async function loadOfficialNoiseVector() {
  const raw = await readFixtureText('cacophony.txt')
  const doc = JSON.parse(raw)
  const vector = doc.vectors.find((candidate) => candidate.name === 'Noise_XX_25519_AESGCM_SHA256')
  assert.ok(vector, 'Noise_XX_25519_AESGCM_SHA256 not found in cacophony.txt')
  return vector
}

function parseNistP256Cases(text) {
  const lines = text.split(/\r?\n/)
  const cases = []
  let inSection = false
  let current = null

  const flush = () => {
    if (current != null) {
      cases.push(current)
      current = null
    }
  }

  for (const rawLine of lines) {
    const line = rawLine.trim()
    if (line === '[P-256]') {
      inSection = true
      continue
    }
    if (inSection && line.startsWith('[') && line !== '[P-256]') {
      flush()
      break
    }
    if (!inSection || line === '' || line.startsWith('#')) {
      continue
    }
    if (line.startsWith('COUNT = ')) {
      flush()
      current = { COUNT: Number.parseInt(line.slice('COUNT = '.length), 10) }
      continue
    }
    const parts = line.split(' = ')
    if (parts.length !== 2 || current == null) {
      continue
    }
    current[parts[0]] = parts[1]
  }

  flush()
  return cases
}

async function x25519(privateKeyRaw, publicKeyRaw) {
  const privateKey = await crypto.subtle.importKey(
    'pkcs8',
    buildX25519Pkcs8(privateKeyRaw),
    { name: 'X25519' },
    false,
    ['deriveBits']
  )
  const publicKey = await crypto.subtle.importKey(
    'spki',
    buildX25519Spki(publicKeyRaw),
    { name: 'X25519' },
    false,
    []
  )
  const shared = await crypto.subtle.deriveBits(
    { name: 'X25519', public: publicKey },
    privateKey,
    256
  )
  return new Uint8Array(shared)
}

async function x25519PublicKey(privateKeyRaw) {
  const privateKey = await crypto.subtle.importKey(
    'pkcs8',
    buildX25519Pkcs8(privateKeyRaw),
    { name: 'X25519' },
    true,
    ['deriveBits']
  )
  const jwk = await crypto.subtle.exportKey('jwk', privateKey)
  return base64UrlToBytes(jwk.x)
}

function buildX25519Pkcs8(privateKeyRaw) {
  return concat(
    hexToBytes('302e020100300506032b656e04220420'),
    privateKeyRaw
  )
}

function buildX25519Spki(publicKeyRaw) {
  return concat(
    hexToBytes('302a300506032b656e032100'),
    publicKeyRaw
  )
}

async function readFixtureText(name) {
  return readFile(new URL(`../../../testdata/external/${name}`, import.meta.url), 'utf8')
}

function hexToBytes(hex) {
  return new Uint8Array(Buffer.from(hex, 'hex'))
}

function base64UrlToBytes(value) {
  const base64 = value.replace(/-/g, '+').replace(/_/g, '/')
  const padded = base64 + '='.repeat((4 - (base64.length % 4)) % 4)
  return new Uint8Array(Buffer.from(padded, 'base64'))
}

function concat(...arrays) {
  const total = arrays.reduce((sum, array) => sum + array.length, 0)
  const out = new Uint8Array(total)
  let offset = 0
  for (const array of arrays) {
    out.set(array, offset)
    offset += array.length
  }
  return out
}
