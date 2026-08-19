// signaling-server/web/noise-p256/handshake.js
import { initialize, mixHash, hkdf2 } from './transcript.js'
import { CipherState, splitKeys } from './cipher.js'
import { generateKeypair, dh, importPublicKey, exportPublicKey } from './keys.js'

export class NoiseXX {
  #role  // 'initiator' | 'responder'
  #phase // 'ready' | 'msg1-sent' | 'msg1-received' | 'msg2-sent' | 'msg2-received' | 'done'
  #h
  #ck
  #k = new Uint8Array(32)  // zero until first mixKey
  #hasK = false

  #ePriv = null   // ephemeral private key (CryptoKey)
  #ePub = null    // ephemeral public key bytes (65 bytes)
  #sPriv = null   // static private key (CryptoKey)
  #sPub = null    // static public key bytes (65 bytes)
  #rEPub = null   // remote ephemeral pub bytes
  #rSPub = null   // remote static pub bytes

  constructor(role, sPriv, sPub) {
    this.#role = role
    this.#phase = 'ready'
    const { h, ck } = initialize()
    this.#h = h
    this.#ck = ck
    this.#sPriv = sPriv
    this.#sPub = sPub
  }

  static async createInitiator(opts = {}) {
    const kp = opts.staticKeypair ?? await generateKeypair()
    const noise = new NoiseXX('initiator', kp.privateKey, kp.publicKeyBytes)
    noise._testEphemeralKeypair = opts.ephemeralKeypair ?? null
    noise.#h = await mixHash(noise.#h, new Uint8Array(0)) // MixHash(prologue)
    return noise
  }

  static async createResponder(staticPrivKey, staticPubKeyBytes, opts = {}) {
    const noise = new NoiseXX('responder', staticPrivKey, staticPubKeyBytes)
    noise._testEphemeralKeypair = opts.ephemeralKeypair ?? null
    noise.#h = await mixHash(noise.#h, new Uint8Array(0)) // MixHash(prologue)
    return noise
  }

  get remoteStaticPub() { return this.#rSPub }

  // writeMessage1 — initiator: e (65 bytes)
  async writeMessage1() {
    this._assertRole('initiator', 'writeMessage1')
    this._assertPhase('ready', 'writeMessage1')
    const kp = this._testEphemeralKeypair ?? await generateKeypair()
    this._testEphemeralKeypair = null
    this.#ePriv = kp.privateKey
    this.#ePub = kp.publicKeyBytes
    this.#h = await mixHash(this.#h, this.#ePub)
    this.#phase = 'msg1-sent'
    return this.#ePub.slice()
  }

  // readMessage1 — responder: e
  async readMessage1(msg) {
    this._assertRole('responder', 'readMessage1')
    this._assertPhase('ready', 'readMessage1')
    this._assertLen(msg, 65, 'readMessage1')
    this.#rEPub = msg.slice(0, 65)
    this.#h = await mixHash(this.#h, this.#rEPub)
    this.#phase = 'msg1-received'
  }

  // writeMessage2 — responder: e, ee, s, es (162 bytes)
  async writeMessage2() {
    this._assertRole('responder', 'writeMessage2')
    this._assertPhase('msg1-received', 'writeMessage2')
    const kp = this._testEphemeralKeypair ?? await generateKeypair()
    this._testEphemeralKeypair = null
    this.#ePriv = kp.privateKey
    this.#ePub = kp.publicKeyBytes

    // token: e
    this.#h = await mixHash(this.#h, this.#ePub)
    const buf = [this.#ePub]

    // token: ee
    const ee = await dh(this.#ePriv, this.#rEPub)
    await this.#mixKey(ee)

    // token: s — EncryptAndHash(rs_pub)
    const encS = await this.#encryptAndHash(this.#sPub)
    buf.push(encS)

    // token: es — DH(rs, ie)
    const es = await dh(this.#sPriv, this.#rEPub)
    await this.#mixKey(es)

    // empty payload
    const tag = await this.#encryptAndHash(new Uint8Array(0))
    buf.push(tag)
    this.#phase = 'msg2-sent'
    return concat(...buf)
  }

  // readMessage2 — initiator: e, ee, s, es (162 bytes)
  async readMessage2(msg) {
    this._assertRole('initiator', 'readMessage2')
    this._assertPhase('msg1-sent', 'readMessage2')
    this._assertLen(msg, 162, 'readMessage2')

    // token: e
    this.#rEPub = msg.slice(0, 65)
    this.#h = await mixHash(this.#h, this.#rEPub)

    // token: ee
    const ee = await dh(this.#ePriv, this.#rEPub)
    await this.#mixKey(ee)

    // token: s — DecryptAndHash(encrypted_rs_pub)
    this.#rSPub = await this.#decryptAndHash(msg.slice(65, 146))

    // token: es — DH(ie, rs)
    const es = await dh(this.#ePriv, this.#rSPub)
    await this.#mixKey(es)

    // verify empty payload
    await this.#decryptAndHash(msg.slice(146, 162))
    this.#phase = 'msg2-received'
  }

  // writeMessage3 — initiator: s, se (97 bytes)
  async writeMessage3() {
    this._assertRole('initiator', 'writeMessage3')
    this._assertPhase('msg2-received', 'writeMessage3')
    const buf = []

    // token: s — EncryptAndHash(is_pub)
    const encS = await this.#encryptAndHash(this.#sPub)
    buf.push(encS)

    // token: se — DH(is, re)
    const se = await dh(this.#sPriv, this.#rEPub)
    await this.#mixKey(se)

    // empty payload
    const tag = await this.#encryptAndHash(new Uint8Array(0))
    buf.push(tag)
    this.#phase = 'done'
    return concat(...buf)
  }

  // readMessage3 — responder: s, se (97 bytes)
  async readMessage3(msg) {
    this._assertRole('responder', 'readMessage3')
    this._assertPhase('msg2-sent', 'readMessage3')
    this._assertLen(msg, 97, 'readMessage3')

    // token: s — DecryptAndHash(encrypted_is_pub)
    this.#rSPub = await this.#decryptAndHash(msg.slice(0, 81))

    // token: se — DH(re, is)
    const se = await dh(this.#ePriv, this.#rSPub)
    await this.#mixKey(se)

    // verify empty payload
    await this.#decryptAndHash(msg.slice(81, 97))
    this.#phase = 'done'
  }

  // split returns [send, recv] from the caller's perspective.
  async split() {
    this._assertPhase('done', 'split')
    const [k1, k2] = await splitKeys(this.#ck)
    const cs1 = new CipherState(k1)
    const cs2 = new CipherState(k2)
    return this.#role === 'initiator' ? [cs1, cs2] : [cs2, cs1]
  }

  async #mixKey(ikm) {
    const [newCK, newK] = await hkdf2(this.#ck, ikm)
    this.#ck = newCK
    this.#k = newK
    this.#hasK = true
  }

  async #encryptAndHash(plaintext) {
    if (!this.#hasK) throw new Error('noise: cipher key not initialized')
    const cs = new CipherState(this.#k)
    const ct = await cs.encryptWithAd(this.#h, plaintext)
    this.#h = await mixHash(this.#h, ct)
    return ct
  }

  async #decryptAndHash(ciphertext) {
    if (!this.#hasK) throw new Error('noise: cipher key not initialized')
    const cs = new CipherState(this.#k)
    const pt = await cs.decryptWithAd(this.#h, ciphertext)
    this.#h = await mixHash(this.#h, ciphertext)
    return pt
  }

  _assertRole(expected, method) {
    if (this.#role !== expected) throw new Error(`noise: ${method} called on ${this.#role}`)
  }

  _assertPhase(expected, method) {
    if (this.#phase !== expected) throw new Error(`noise: ${method} invalid in phase ${this.#phase}`)
  }

  _assertLen(buf, expected, method) {
    if (buf.length !== expected) throw new Error(`noise: ${method}: expected ${expected} bytes, got ${buf.length}`)
  }
}

function concat(...arrays) {
  const total = arrays.reduce((n, a) => n + a.length, 0)
  const out = new Uint8Array(total)
  let offset = 0
  for (const arr of arrays) { out.set(arr, offset); offset += arr.length }
  return out
}