# Secure Relay Plan B: Browser SecureRelayChannel + Connection Orchestration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the browser-side secure relay transport: a `SecureRelayChannel` over `/ws/relay`, a transport-agnostic `connectTransferChannel(...)` orchestrator that chooses direct vs relay within server policy, and a browser app integration that keeps the existing file-transfer protocol unchanged.

**Architecture:** Introduce a small `signaling-server/web/src/` module layout instead of extending the monolithic browser `app.js` directly. Wrap both WebRTC DataChannel and relay WebSocket + Noise into the same channel shape, make `connectTransferChannel(...)` own direct-first vs relay-only vs fallback logic, and keep the transfer UI speaking the same JSON-text plus binary-chunk protocol it already uses today.

**Tech Stack:** Browser JavaScript, Web Crypto, WebRTC, WebSocket, `noise-p256`, `node:test`, ES modules

---

## File Structure

### New files

- `signaling-server/web/package.json`
  - Declares browser-side ESM test scripts so `node --test` can run the new `src/*.test.js` files.
- `signaling-server/web/src/frame.js`
  - Shared relay frame constants plus encoder/decoder for handshake (`0x00`), text (`0x01`), and binary (`0x02`) frames.
- `signaling-server/web/src/frame.test.js`
  - Validates frame encoding, streaming decode, unknown-kind rejection, and handshake/data size caps.
- `signaling-server/web/src/directChannel.js`
  - Wraps `RTCDataChannel` in the shared browser-side channel shape used by the transfer layer.
- `signaling-server/web/src/directChannel.test.js`
  - Verifies event dispatch, send helpers, and buffered-amount behavior for the direct adapter.
- `signaling-server/web/src/secureRelayChannel.js`
  - Opens `/ws/relay`, performs the browser-side Noise initiator handshake, pins `remoteStaticPub`, then encrypts/decrypts framed text and binary messages.
- `signaling-server/web/src/secureRelayChannel.test.js`
  - Tests successful handshake, static-key mismatch, decrypt failure, unknown frame rejection, and framed send/receive behavior.
- `signaling-server/web/src/connectTransferChannel.js`
  - Owns direct-vs-relay selection, direct timeout, relay-only behavior, STUN-only direct ICE filtering, and status callbacks.
- `signaling-server/web/src/connectTransferChannel.test.js`
  - Verifies direct success, relay-only fast path, direct-timeout fallback, relay-disallowed failures, and mode/status reporting.
- `signaling-server/web/src/app.js`
  - Browser entry point that keeps the current UI and file-transfer protocol but delegates transport setup to `connectTransferChannel(...)`.

### Modified files

- `signaling-server/web/index.html`
  - Load the new module entrypoint instead of the legacy top-level script and keep the current DOM structure intact.

### Deleted files

- `signaling-server/web/app.js`
  - Replaced by `signaling-server/web/src/app.js`.

## Task 1: Introduce Browser Module/Test Scaffold, Shared Relay Framing, And The DirectChannel Adapter

**Files:**
- Create: `signaling-server/web/package.json`
- Create: `signaling-server/web/src/frame.js`
- Create: `signaling-server/web/src/frame.test.js`
- Create: `signaling-server/web/src/directChannel.js`
- Create: `signaling-server/web/src/directChannel.test.js`
- Modify: `signaling-server/web/index.html`

- [ ] **Step 1: Write the failing frame and direct-channel tests**

```js
// signaling-server/web/src/frame.test.js
import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  FRAME_HANDSHAKE,
  FRAME_TEXT,
  FRAME_BINARY,
  MAX_FRAME_PAYLOAD,
  MAX_HANDSHAKE_PAYLOAD,
  writeFrame,
  FrameDecoder,
} from './frame.js'

test('encodes and decodes handshake and data frames', () => {
  const decoder = new FrameDecoder()
  const encoded = writeFrame(FRAME_TEXT, new TextEncoder().encode('hello'))
  const [frame] = [...decoder.push(encoded)]
  assert.equal(frame.kind, FRAME_TEXT)
  assert.equal(new TextDecoder().decode(frame.payload), 'hello')
})

test('rejects unknown frame kinds', () => {
  const decoder = new FrameDecoder()
  assert.throws(() => [...decoder.push(Uint8Array.from([0xff, 0, 0, 0, 0]))], /unknown frame kind/i)
})

test('enforces handshake and payload caps independently', () => {
  assert.throws(
    () => writeFrame(FRAME_HANDSHAKE, new Uint8Array(MAX_HANDSHAKE_PAYLOAD + 1)),
    /handshake frame too large/i
  )
  assert.throws(
    () => writeFrame(FRAME_BINARY, new Uint8Array(MAX_FRAME_PAYLOAD + 1)),
    /frame too large/i
  )
})
```

```js
// signaling-server/web/src/directChannel.test.js
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { DirectChannel } from './directChannel.js'

function createMockRTCDataChannel() {
  return {
    bufferedAmount: 17,
    readyState: 'connecting',
    binaryType: 'blob',
    sent: [],
    onopen: null,
    onmessage: null,
    onclose: null,
    send(payload) {
      this.sent.push(payload)
    },
    closeCalled: false,
    close() {
      this.closeCalled = true
      this.readyState = 'closed'
    },
  }
}

test('DirectChannel normalizes RTCDataChannel into the shared channel contract', () => {
  const rtc = createMockRTCDataChannel()
  const channel = new DirectChannel(rtc)
  const seen = []

  channel.onmessage = (event) => seen.push(event.data)
  channel.onopen = () => seen.push('open')
  channel.onclose = () => seen.push('close')

  rtc.readyState = 'open'
  rtc.onopen()
  rtc.onmessage({ data: 'hi' })
  rtc.onmessage({ data: new ArrayBuffer(3) })
  rtc.onclose()

  channel.send(JSON.stringify({ type: 'ping' }))
  channel.sendBinary(new Uint8Array([1, 2, 3]))
  channel.close()

  assert.deepEqual(seen, ['open', 'hi', new ArrayBuffer(3), 'close'])
  assert.equal(rtc.binaryType, 'arraybuffer')
  assert.equal(channel.bufferedAmount, 17)
  assert.equal(rtc.sent.length, 2)
  assert.equal(rtc.closeCalled, true)
})
```

- [ ] **Step 2: Run the tests to verify they fail**

Run:

```bash
cd signaling-server/web && node --test src/frame.test.js src/directChannel.test.js
```

Expected:

- FAIL because `src/frame.js` and `src/directChannel.js` do not exist yet

- [ ] **Step 3: Write the minimal scaffold, frame codec, and direct adapter**

```json
// signaling-server/web/package.json
{
  "name": "sharebridge-web",
  "private": true,
  "type": "module",
  "scripts": {
    "test": "node --test src/*.test.js"
  }
}
```

```js
// signaling-server/web/src/frame.js
export const FRAME_HANDSHAKE = 0x00
export const FRAME_TEXT = 0x01
export const FRAME_BINARY = 0x02

export const MAX_HANDSHAKE_PAYLOAD = 4 * 1024
export const MAX_FRAME_PAYLOAD = 8 * 1024 * 1024

function getCapForKind(kind) {
  if (kind === FRAME_HANDSHAKE) return MAX_HANDSHAKE_PAYLOAD
  if (kind === FRAME_TEXT || kind === FRAME_BINARY) return MAX_FRAME_PAYLOAD
  throw new Error(`unknown frame kind: 0x${kind.toString(16)}`)
}

export function writeFrame(kind, payload) {
  const cap = getCapForKind(kind)
  if (payload.length > cap) {
    if (kind === FRAME_HANDSHAKE) {
      throw new Error(`handshake frame too large: ${payload.length} bytes`)
    }
    throw new Error(`frame too large: ${payload.length} bytes`)
  }

  const out = new Uint8Array(5 + payload.length)
  out[0] = kind
  out[1] = (payload.length >>> 24) & 0xff
  out[2] = (payload.length >>> 16) & 0xff
  out[3] = (payload.length >>> 8) & 0xff
  out[4] = payload.length & 0xff
  out.set(payload, 5)
  return out
}

export class FrameDecoder {
  constructor() {
    this.buffer = new Uint8Array(0)
  }

  *push(chunk) {
    const next = new Uint8Array(this.buffer.length + chunk.length)
    next.set(this.buffer, 0)
    next.set(chunk, this.buffer.length)
    this.buffer = next

    while (this.buffer.length >= 5) {
      const kind = this.buffer[0]
      const cap = getCapForKind(kind)
      const view = new DataView(this.buffer.buffer, this.buffer.byteOffset, this.buffer.byteLength)
      const length = view.getUint32(1, false)
      if (length > cap) {
        if (kind === FRAME_HANDSHAKE) {
          throw new Error(`handshake frame too large: ${length} bytes`)
        }
        throw new Error(`frame too large: ${length} bytes`)
      }
      if (this.buffer.length < 5 + length) break
      const payload = this.buffer.slice(5, 5 + length)
      this.buffer = this.buffer.slice(5 + length)
      yield { kind, payload }
    }
  }
}
```

```js
// signaling-server/web/src/directChannel.js
export class DirectChannel {
  constructor(rtcDataChannel) {
    this._dc = rtcDataChannel
    this.onopen = null
    this.onmessage = null
    this.onclose = null

    this._dc.binaryType = 'arraybuffer'
    this._dc.onopen = () => { if (this.onopen) this.onopen() }
    this._dc.onmessage = (event) => { if (this.onmessage) this.onmessage(event) }
    this._dc.onclose = () => { if (this.onclose) this.onclose() }
  }

  get readyState() {
    return this._dc.readyState
  }

  get bufferedAmount() {
    return this._dc.bufferedAmount
  }

  send(text) {
    this._dc.send(text)
  }

  sendBinary(bytes) {
    this._dc.send(bytes)
  }

  close() {
    this._dc.close()
  }
}
```

```html
<!-- signaling-server/web/index.html -->
  <script type="module" src="/src/app.js"></script>
```

- [ ] **Step 4: Run the tests to verify they pass**

Run:

```bash
cd signaling-server/web && node --test src/frame.test.js src/directChannel.test.js
```

Expected:

- PASS for both test files

- [ ] **Step 5: Commit**

```bash
git add signaling-server/web/package.json signaling-server/web/index.html signaling-server/web/src/frame.js signaling-server/web/src/frame.test.js signaling-server/web/src/directChannel.js signaling-server/web/src/directChannel.test.js
git commit -m "feat(web): add browser relay frame codec and direct channel adapter"
```

## Task 2: Implement SecureRelayChannel Over `/ws/relay` Using `noise-p256`

**Files:**
- Create: `signaling-server/web/src/secureRelayChannel.js`
- Create: `signaling-server/web/src/secureRelayChannel.test.js`
- Modify: `signaling-server/web/src/frame.js`

- [ ] **Step 1: Write the failing SecureRelayChannel tests**

```js
// signaling-server/web/src/secureRelayChannel.test.js
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { NoiseXX } from '../noise-p256/index.js'
import { FRAME_HANDSHAKE, FRAME_TEXT, FRAME_BINARY, writeFrame, FrameDecoder } from './frame.js'
import { SecureRelayChannel } from './secureRelayChannel.js'

function createMockRelaySocket() {
  const sent = []
  const listeners = { open: [], message: [], close: [], error: [] }
  const sendWaiters = []
  return {
    sent,
    readyState: 0,
    addEventListener(type, fn) { listeners[type].push(fn) },
    removeEventListener(type, fn) {
      listeners[type] = listeners[type].filter((candidate) => candidate !== fn)
    },
    send(bytes) {
      sent.push(bytes)
      while (sendWaiters.length > 0 && sent.length >= sendWaiters[0].count) {
        sendWaiters.shift().resolve()
      }
    },
    close() {
      this.readyState = 3
      for (const fn of listeners.close) fn()
    },
    open() {
      this.readyState = 1
      for (const fn of listeners.open) fn()
    },
    pushMessage(bytes) {
      for (const fn of listeners.message) fn({ data: bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength) })
    },
    fail(err) {
      for (const fn of listeners.error) fn(err)
    },
    waitForSentCount(count) {
      if (sent.length >= count) return Promise.resolve()
      return new Promise((resolve) => sendWaiters.push({ count, resolve }))
    },
  }
}

test('SecureRelayChannel performs Noise handshake, pins static key, and exchanges framed messages', async () => {
  const socket = createMockRelaySocket()
  const responderStatic = await (await import('../noise-p256/keys.js')).generateKeypair()
  const responder = await NoiseXX.createResponder(responderStatic.privateKey, responderStatic.publicKeyBytes)
  const seen = []

  const channel = new SecureRelayChannel({
    relayURL: 'ws://relay.test/ws/relay',
    relayToken: 'browser-jwt',
    expectedStaticPub: responderStatic.publicKeyBytes,
    websocketFactory: () => socket,
  })

  channel.onopen = () => seen.push('open')
  channel.onmessage = (event) => seen.push(event.data)

  socket.readyState = 1
  const started = channel.start()
  await socket.waitForSentCount(2)

  // Browser sent hello JSON, then msg1 as a handshake frame.
  const hello = new TextDecoder().decode(socket.sent[0])
  assert.match(hello, /browser-jwt/)
  const decoder = new FrameDecoder()
  const [{ kind: kind1, payload: msg1 }] = [...decoder.push(socket.sent[1])]
  assert.equal(kind1, FRAME_HANDSHAKE)

  await responder.readMessage1(msg1)
  const msg2 = await responder.writeMessage2()
  socket.pushMessage(writeFrame(FRAME_HANDSHAKE, msg2))
  await socket.waitForSentCount(3)

  const [{ payload: msg3 }] = [...decoder.push(socket.sent[2])]
  await responder.readMessage3(msg3)
  const [, rRecv] = await responder.split()

  await started
  await channel.send(JSON.stringify({ type: 'ping' }))
  const [{ kind: frameKind, payload: ciphertext }] = [...decoder.push(socket.sent[3])]
  assert.equal(frameKind, FRAME_TEXT)
  const plaintext = await rRecv.decrypt(new Uint8Array(0), ciphertext)
  assert.equal(new TextDecoder().decode(plaintext), '{"type":"ping"}')
  assert.deepEqual(seen, ['open'])
})

test('SecureRelayChannel fails closed on responder static-key mismatch', async () => {
  const socket = createMockRelaySocket()
  const responderStatic = await (await import('../noise-p256/keys.js')).generateKeypair()
  const wrongStatic = await (await import('../noise-p256/keys.js')).generateKeypair()
  const responder = await NoiseXX.createResponder(responderStatic.privateKey, responderStatic.publicKeyBytes)
  const channel = new SecureRelayChannel({
    relayURL: 'ws://relay.test/ws/relay',
    relayToken: 'browser-jwt',
    expectedStaticPub: wrongStatic.publicKeyBytes,
    websocketFactory: () => socket,
  })

  socket.readyState = 1
  const started = channel.start()
  await socket.waitForSentCount(2)

  const decoder = new FrameDecoder()
  const [{ payload: msg1 }] = [...decoder.push(socket.sent[1])]
  await responder.readMessage1(msg1)
  socket.pushMessage(writeFrame(FRAME_HANDSHAKE, await responder.writeMessage2()))

  await assert.rejects(() => started, /unexpected agent static key/i)
})
```

- [ ] **Step 2: Run the tests to verify they fail**

Run:

```bash
cd signaling-server/web && node --test src/secureRelayChannel.test.js
```

Expected:

- FAIL because `src/secureRelayChannel.js` does not exist

- [ ] **Step 3: Write the minimal SecureRelayChannel implementation**

```js
// signaling-server/web/src/secureRelayChannel.js
import { NoiseXX } from '../noise-p256/index.js'
import { FRAME_HANDSHAKE, FRAME_TEXT, FRAME_BINARY, FrameDecoder, writeFrame } from './frame.js'

export class SecureRelayChannel {
  constructor({ relayURL, relayToken, expectedStaticPub, websocketFactory = (url) => new WebSocket(url) }) {
    this._relayURL = relayURL
    this._relayToken = relayToken
    this._expectedStaticPub = expectedStaticPub
    this._websocketFactory = websocketFactory
    this._socket = null
    this._decoder = new FrameDecoder()
    this._noise = null
    this._sendCipher = null
    this._recvCipher = null
    this.readyState = 'connecting'
    this.bufferedAmount = 0
    this.onopen = null
    this.onmessage = null
    this.onclose = null
  }

  async start() {
    this._noise = await NoiseXX.createInitiator()
    this._socket = this._websocketFactory(this._relayURL)

    await new Promise((resolve, reject) => {
      const handleOpen = async () => {
        try {
          this._socket.send(new TextEncoder().encode(JSON.stringify({ token: this._relayToken })))
          this._socket.addEventListener('message', this._handleMessage)
          const msg1 = await this._noise.writeMessage1()
          this._socket.send(writeFrame(FRAME_HANDSHAKE, msg1))
          resolve()
        } catch (err) {
          reject(err)
        }
      }
      this._socket.addEventListener('open', handleOpen, { once: true })
      this._socket.addEventListener('error', reject, { once: true })
      this._socket.addEventListener('close', () => {
        this.readyState = 'closed'
        if (this.onclose) this.onclose()
      }, { once: true })
      if (this._socket.readyState === 1) {
        handleOpen()
      }
    })

    await new Promise((resolve, reject) => {
      this._handshakeResolve = resolve
      this._handshakeReject = reject
    })
  }

  _handleMessage = async (event) => {
    const chunk = new Uint8Array(event.data)
    for (const frame of this._decoder.push(chunk)) {
      if (frame.kind === FRAME_HANDSHAKE) {
        try {
          await this._handleHandshakeFrame(frame.payload)
        } catch (err) {
          this.close()
          this._handshakeReject?.(err)
        }
        continue
      }

      try {
        const plaintext = await this._recvCipher.decrypt(new Uint8Array(0), frame.payload)
        if (frame.kind === FRAME_TEXT) {
          this.onmessage?.({ data: new TextDecoder().decode(plaintext) })
        } else if (frame.kind === FRAME_BINARY) {
          this.onmessage?.({ data: plaintext.buffer.slice(plaintext.byteOffset, plaintext.byteOffset + plaintext.byteLength) })
        } else {
          throw new Error(`unknown frame kind: 0x${frame.kind.toString(16)}`)
        }
      } catch (err) {
        this.close()
      }
    }
  }

  async _handleHandshakeFrame(msg2) {
    await this._noise.readMessage2(msg2)
    if (!equalBytes(this._noise.remoteStaticPub, this._expectedStaticPub)) {
      throw new Error('unexpected agent static key')
    }
    const msg3 = await this._noise.writeMessage3()
    this._socket.send(writeFrame(FRAME_HANDSHAKE, msg3))
    ;[this._sendCipher, this._recvCipher] = await this._noise.split()
    this.readyState = 'open'
    this._handshakeResolve?.()
    this.onopen?.()
  }

  async send(text) {
    const plaintext = new TextEncoder().encode(text)
    const ciphertext = await this._sendCipher.encrypt(new Uint8Array(0), plaintext)
    this._socket.send(writeFrame(FRAME_TEXT, ciphertext))
  }

  async sendBinary(bytes) {
    const ciphertext = await this._sendCipher.encrypt(new Uint8Array(0), bytes)
    this._socket.send(writeFrame(FRAME_BINARY, ciphertext))
  }

  close() {
    this.readyState = 'closing'
    this._socket?.close()
  }
}

function equalBytes(left, right) {
  if (!left || !right || left.length !== right.length) return false
  for (let i = 0; i < left.length; i += 1) {
    if (left[i] !== right[i]) return false
  }
  return true
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run:

```bash
cd signaling-server/web && node --test src/frame.test.js src/secureRelayChannel.test.js
```

Expected:

- PASS for the frame and secure-relay tests

- [ ] **Step 5: Commit**

```bash
git add signaling-server/web/src/frame.js signaling-server/web/src/secureRelayChannel.js signaling-server/web/src/secureRelayChannel.test.js
git commit -m "feat(web): add browser secure relay channel"
```

## Task 3: Implement `connectTransferChannel(...)` With Direct-First, Relay-Only, And Fallback Logic

**Files:**
- Create: `signaling-server/web/src/connectTransferChannel.js`
- Create: `signaling-server/web/src/connectTransferChannel.test.js`
- Modify: `signaling-server/web/src/directChannel.js`

- [ ] **Step 1: Write the failing connection-orchestration tests**

```js
// signaling-server/web/src/connectTransferChannel.test.js
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { connectTransferChannel, decodeRelayPolicyToken } from './connectTransferChannel.js'

function createDeferred() {
  let resolve
  let reject
  const promise = new Promise((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

test('returns direct channel when direct opens before timeout', async () => {
  const statuses = []
  const directReady = createDeferred()
  const directChannel = { kind: 'direct', readyState: 'open', close() {} }

  const resultPromise = connectTransferChannel({
    relayPolicy: { relayAllowed: true, relayOnly: false },
    directConnect: async () => {
      statuses.push('direct-attempted')
      await directReady.promise
      return directChannel
    },
    relayConnect: async () => {
      throw new Error('relay should not be used')
    },
    directTimeoutMs: 5000,
    onStatusChange: (status) => statuses.push(status),
  })

  directReady.resolve()
  const connected = await resultPromise
  assert.equal(connected.mode, 'direct')
  assert.equal(connected.channel, directChannel)
  assert.deepEqual(statuses.slice(0, 2), ['connecting-direct', 'direct-attempted'])
})

test('falls back to relay when direct times out and relay is allowed', async () => {
  const statuses = []
  const relayChannel = { kind: 'relay', readyState: 'open', close() {} }

  const connected = await connectTransferChannel({
    relayPolicy: { relayAllowed: true, relayOnly: false },
    directConnect: async () => new Promise(() => {}),
    relayConnect: async () => relayChannel,
    directTimeoutMs: 5,
    onStatusChange: (status) => statuses.push(status),
  })

  assert.equal(connected.mode, 'relay')
  assert.equal(connected.channel, relayChannel)
  assert.deepEqual(statuses, ['connecting-direct', 'falling-back-to-relay', 'connected-relay'])
})

test('skips direct when relay_only is true', async () => {
  let directCalled = false
  const relayChannel = { kind: 'relay', readyState: 'open', close() {} }

  const connected = await connectTransferChannel({
    relayPolicy: { relayAllowed: true, relayOnly: true },
    directConnect: async () => {
      directCalled = true
      return null
    },
    relayConnect: async () => relayChannel,
    onStatusChange: () => {},
  })

  assert.equal(directCalled, false)
  assert.equal(connected.mode, 'relay')
})

test('fails closed when direct fails and relay is disallowed', async () => {
  await assert.rejects(() => connectTransferChannel({
    relayPolicy: { relayAllowed: false, relayOnly: false },
    directConnect: async () => { throw new Error('direct failed') },
    relayConnect: async () => ({ kind: 'relay' }),
    directTimeoutMs: 5,
    onStatusChange: () => {},
  }), /direct failed/i)
})

test('decodeRelayPolicyToken extracts the expected static key from the JWT payload', () => {
  const payload = Buffer.from(JSON.stringify({
    expected_agent_static_pub: '04abcd',
    relay_allowed: true,
    relay_only: false,
  })).toString('base64url')
  const token = ['header', payload, 'sig'].join('.')

  assert.deepEqual(decodeRelayPolicyToken(token), {
    expectedStaticPubHex: '04abcd',
    relayAllowed: true,
    relayOnly: false,
  })
})
```

- [ ] **Step 2: Run the tests to verify they fail**

Run:

```bash
cd signaling-server/web && node --test src/connectTransferChannel.test.js
```

Expected:

- FAIL because `src/connectTransferChannel.js` does not exist

- [ ] **Step 3: Write the minimal orchestration implementation**

```js
// signaling-server/web/src/connectTransferChannel.js
const DEFAULT_DIRECT_TIMEOUT_MS = 5000

export async function connectTransferChannel({
  relayPolicy,
  directConnect,
  relayConnect,
  directTimeoutMs = DEFAULT_DIRECT_TIMEOUT_MS,
  onStatusChange = () => {},
}) {
  if (relayPolicy.relayOnly) {
    onStatusChange('connecting-relay')
    const relayChannel = await relayConnect()
    onStatusChange('connected-relay')
    return { channel: relayChannel, mode: 'relay' }
  }

  onStatusChange('connecting-direct')

  try {
    const directChannel = await withTimeout(directConnect(), directTimeoutMs)
    onStatusChange('connected-direct')
    return { channel: directChannel, mode: 'direct' }
  } catch (err) {
    if (!relayPolicy.relayAllowed) {
      onStatusChange('failed')
      throw err
    }
    onStatusChange('falling-back-to-relay')
    const relayChannel = await relayConnect()
    onStatusChange('connected-relay')
    return { channel: relayChannel, mode: 'relay' }
  }
}

export function buildDirectIceServers(iceServers) {
  return iceServers
    .map((entry) => {
      const urls = Array.isArray(entry.urls) ? entry.urls : [entry.urls]
      const stunOnly = urls.filter((url) => typeof url === 'string' && url.startsWith('stun:'))
      return stunOnly.length > 0 ? { urls: stunOnly } : null
    })
    .filter(Boolean)
}

export function decodeRelayPolicyToken(token) {
  const [, payload] = token.split('.')
  const json = JSON.parse(new TextDecoder().decode(base64UrlToBytes(payload)))
  return {
    expectedStaticPubHex: json.expected_agent_static_pub,
    relayAllowed: Boolean(json.relay_allowed),
    relayOnly: Boolean(json.relay_only),
  }
}

function withTimeout(promise, timeoutMs) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error(`direct connect timeout after ${timeoutMs}ms`)), timeoutMs)
    promise.then(
      (value) => {
        clearTimeout(timer)
        resolve(value)
      },
      (err) => {
        clearTimeout(timer)
        reject(err)
      },
    )
  })
}

function base64UrlToBytes(base64Url) {
  const base64 = base64Url.replace(/-/g, '+').replace(/_/g, '/')
  const padded = base64 + '='.repeat((4 - (base64.length % 4)) % 4)
  const binary = atob(padded)
  return Uint8Array.from(binary, (char) => char.charCodeAt(0))
}
```

```js
// signaling-server/web/src/directChannel.js
// extend the adapter with a helper used by app.js and tests
export function waitForDirectChannelOpen(channel) {
  if (channel.readyState === 'open') return Promise.resolve(channel)
  return new Promise((resolve, reject) => {
    channel.onopen = () => resolve(channel)
    channel.onclose = () => reject(new Error('direct channel closed before opening'))
  })
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run:

```bash
cd signaling-server/web && node --test src/directChannel.test.js src/connectTransferChannel.test.js
```

Expected:

- PASS for both test files

- [ ] **Step 5: Commit**

```bash
git add signaling-server/web/src/directChannel.js signaling-server/web/src/connectTransferChannel.js signaling-server/web/src/connectTransferChannel.test.js
git commit -m "feat(web): add browser transport selection and fallback logic"
```

## Task 4: Refactor The Browser App To Use `connectTransferChannel(...)` Without Changing The Transfer Protocol

**Files:**
- Create: `signaling-server/web/src/app.js`
- Create: `signaling-server/web/src/app.test.js`
- Modify: `signaling-server/web/index.html`
- Delete: `signaling-server/web/app.js`

- [ ] **Step 1: Write the failing browser-app integration test**

```js
// signaling-server/web/src/app.test.js
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { installSessionMessageHandler, publishGlobalActions, applyConnectionBadge } from './app.js'

test('publishGlobalActions preserves inline button handlers after the move to an ES module', () => {
  const globals = {}
  const join = () => {}
  const submitPassword = () => {}

  publishGlobalActions(globals, { join, submitPassword })

  assert.equal(globals.join, join)
  assert.equal(globals.submitPassword, submitPassword)
})

test('applyConnectionBadge preserves the colored direct/relay badge', () => {
  const statusContainer = {
    classList: {
      removed: [],
      remove(...names) { this.removed.push(...names) },
    },
  }
  const badge = {
    textContent: '',
    classList: {
      removed: [],
      added: [],
      remove(...names) { this.removed.push(...names) },
      add(...names) { this.added.push(...names) },
    },
  }

  applyConnectionBadge({ statusContainer, badge, mode: 'relay' })

  assert.equal(badge.textContent, '● Connected (Relay)')
  assert.deepEqual(statusContainer.classList.removed, ['hidden'])
  assert.deepEqual(badge.classList.removed, ['connection-direct', 'connection-relay'])
  assert.deepEqual(badge.classList.added, ['connection-relay'])
})

test('relay_policy drives connection badge and transfer channel setup without changing file protocol handlers', async () => {
  const statuses = []
  const sends = []
  const fakeChannel = {
    readyState: 'open',
    bufferedAmount: 0,
    onopen: null,
    onmessage: null,
    onclose: null,
    send(text) { sends.push(text) },
    sendBinary() {},
    close() {},
  }

  const controller = installSessionMessageHandler({
    status: (msg) => statuses.push(msg),
    connectTransferChannel: async () => ({ channel: fakeChannel, mode: 'relay' }),
    requestFileList: (channel, path) => channel.send(JSON.stringify({ type: 'list_request', path })),
    applyConnectionBadge: ({ mode }) => statuses.push(`badge:${mode}`),
  })

  await controller.handleMessage({ type: 'relay_policy', token: 'jwt', relay_allowed: true, relay_only: true })
  fakeChannel.onopen()

  assert.equal(statuses.at(-1), 'badge:relay')
  assert.equal(sends[0], JSON.stringify({ type: 'list_request', path: '' }))
})
```

- [ ] **Step 2: Run the test to verify it fails**

Run:

```bash
cd signaling-server/web && node --test src/app.test.js
```

Expected:

- FAIL because `src/app.js` does not exist

- [ ] **Step 3: Copy the current `web/app.js` into `web/src/app.js`, preserve the existing UI logic, and refactor only the connection-specific functions**

```js
// signaling-server/web/src/app.js
import { DirectChannel, waitForDirectChannelOpen } from './directChannel.js'
import { SecureRelayChannel } from './secureRelayChannel.js'
import { connectTransferChannel, buildDirectIceServers, decodeRelayPolicyToken } from './connectTransferChannel.js'

// Start from the current signaling-server/web/app.js file and move its full contents
// into this module. Keep the existing UI and transfer helpers intact:
// - renderFileList
// - renderBreadcrumb
// - requestFile
// - startDownload
// - appendChunk
// - completeDownload
// - markFileDone
// - handleError
// - resetUI
// - formatBytes / formatSpeed / escapeHtml / initFromURL
// Only the connection layer should be rewritten to use DirectChannel /
// SecureRelayChannel / connectTransferChannel.

let pc, ws, transferChannel, currentDirectChannel
let pendingCandidates = []
let remoteDescSet = false
let relayPolicy = null
let pendingNonce = null
let relayQuotaExceeded = false
let quotaPeriodEnd = null
let sessionPassword = ''

export function installSessionMessageHandler({
  status,
  connectTransferChannel: connectFn = connectTransferChannel,
  requestFileList = (channel, path) => channel.send(JSON.stringify({ type: 'list_request', path })),
  applyConnectionBadge: applyBadge = applyConnectionBadge,
} = {}) {
  return {
    async handleMessage(msg) {
      switch (msg.type) {
        case 'ice_config':
          pc = new RTCPeerConnection({ iceServers: buildDirectIceServers(msg.ice_servers ?? []) })
          relayQuotaExceeded = Boolean(msg.relay_quota_exceeded)
          quotaPeriodEnd = msg.quota_period_end ?? null
          pc.onicecandidate = (event) => {
            if (event.candidate) {
              ws.send(JSON.stringify({ type: 'ice_candidate', candidate: event.candidate.toJSON() }))
            }
          }
          pc.ondatachannel = (event) => {
            currentDirectChannel = new DirectChannel(event.channel)
            currentDirectChannel.onmessage = handleTransferMessage
            currentDirectChannel.onclose = resetUI
          }
          pc.onconnectionstatechange = () => {
            if (pc.connectionState === 'failed' || pc.connectionState === 'closed') {
              if (relayQuotaExceeded) {
                const periodEnd = quotaPeriodEnd ? new Date(quotaPeriodEnd).toLocaleDateString() : 'soon'
                status(`Connection failed: Direct unavailable, relay blocked (quota exceeded). Resets ${periodEnd}.`)
              } else {
                status('Connection lost')
              }
              resetUI()
            }
          }
          ws.send(JSON.stringify({ type: 'knock' }))
          break

        case 'nonce':
          pendingNonce = msg.value
          if (msg.has_password && !sessionPassword) {
            showSection('password-section')
            document.getElementById('password-input').focus()
          } else {
            sendJoin()
          }
          break

        case 'auth_failed':
          {
            const errorDiv = document.getElementById('password-error')
            const attemptsRemaining = msg.attempts_remaining || 0
            if (attemptsRemaining <= 0) {
              errorDiv.textContent = 'Too many incorrect attempts. Connection closed.'
              document.getElementById('password-input').disabled = true
              document.querySelector('#password-section button').disabled = true
            } else {
              errorDiv.textContent = `Incorrect password. ${attemptsRemaining} attempt${attemptsRemaining === 1 ? '' : 's'} remaining.`
              document.getElementById('password-input').value = ''
              document.getElementById('password-input').focus()
              ws.send(JSON.stringify({ type: 'knock' }))
            }
          }
          break

        case 'relay_policy':
          const decodedRelayPolicy = decodeRelayPolicyToken(msg.token)
          relayPolicy = {
            token: msg.token,
            relayAllowed: msg.relay_allowed,
            relayOnly: msg.relay_only,
            expectedStaticPubHex: decodedRelayPolicy.expectedStaticPubHex,
          }
          const { channel, mode } = await connectFn({
            relayPolicy,
            directConnect: async () => {
              if (!currentDirectChannel) {
                await new Promise((resolve, reject) => {
                  const interval = setInterval(() => {
                    if (currentDirectChannel) {
                      clearInterval(interval)
                      resolve()
                    }
                  }, 10)
                  setTimeout(() => {
                    clearInterval(interval)
                    reject(new Error('direct data channel did not arrive'))
                  }, 5000)
                })
              }
              return waitForDirectChannelOpen(currentDirectChannel)
            },
            relayConnect: async () => {
              const relay = new SecureRelayChannel({
                relayURL: `${location.protocol === 'https:' ? 'wss:' : 'ws:'}//${location.host}/ws/relay`,
                relayToken: relayPolicy.token,
                expectedStaticPub: hexToBytes(relayPolicy.expectedStaticPubHex),
              })
              relay.onmessage = handleTransferMessage
              relay.onclose = resetUI
              await relay.start()
              return relay
            },
            onStatusChange: (next) => {
              if (next === 'connecting-direct') status('Connecting directly...')
              if (next === 'falling-back-to-relay') status('Direct unavailable, falling back to relay...')
              if (next === 'connected-direct') {
                status('Connected (Direct)')
                applyBadge({
                  statusContainer: document.getElementById('connection-status'),
                  badge: document.getElementById('connection-type'),
                  mode: 'direct',
                })
              }
              if (next === 'connecting-relay') status('Connecting via relay...')
              if (next === 'connected-relay') {
                status('Connected (Relay)')
                applyBadge({
                  statusContainer: document.getElementById('connection-status'),
                  badge: document.getElementById('connection-type'),
                  mode: 'relay',
                })
              }
            },
          })

          transferChannel = channel
          transferChannel.onopen = () => {
            hideSection('join-section')
            hideSection('password-section')
            requestFileList(transferChannel, '')
          }
          if (mode === 'relay' || transferChannel.readyState === 'open') {
            transferChannel.onopen?.()
          }
          break

        case 'offer':
          await pc.setRemoteDescription({ type: 'offer', sdp: msg.sdp })
          remoteDescSet = true
          for (const candidate of pendingCandidates) {
            await pc.addIceCandidate(candidate)
          }
          pendingCandidates = []
          const answer = await pc.createAnswer()
          await pc.setLocalDescription(answer)
          ws.send(JSON.stringify({ type: 'answer', sdp: answer.sdp }))
          break

        case 'ice_candidate':
          if (!remoteDescSet) {
            pendingCandidates.push(msg.candidate)
          } else {
            await pc.addIceCandidate(msg.candidate)
          }
          break
      }
    },
  }
}

export function publishGlobalActions(target, actions) {
  Object.assign(target, actions)
}

export function applyConnectionBadge({ statusContainer, badge, mode }) {
  statusContainer.classList.remove('hidden')
  badge.classList.remove('connection-direct', 'connection-relay')
  if (mode === 'direct') {
    badge.textContent = '● Connected (Direct)'
    badge.classList.add('connection-direct')
    return
  }
  badge.textContent = '● Connected (Relay)'
  badge.classList.add('connection-relay')
}

function join() {
  const code = document.getElementById('code').value.trim()
  if (!code) return
  status('Connecting...')

  relayQuotaExceeded = false
  quotaPeriodEnd = null
  currentDirectChannel = null
  transferChannel = null
  relayPolicy = null

  const protocol = location.protocol === 'https:' ? 'wss:' : 'ws:'
  ws = new WebSocket(`${protocol}//${location.host}/ws/client?session=${code}`)

  const controller = installSessionMessageHandler({ status })
  ws.onmessage = async (event) => {
    const msg = JSON.parse(event.data)
    if (msg.type === 'error') {
      status('Error: ' + msg.message)
      return
    }
    await controller.handleMessage(msg)
  }
  ws.onerror = () => {
    if (relayQuotaExceeded) {
      const periodEnd = quotaPeriodEnd ? new Date(quotaPeriodEnd).toLocaleDateString() : 'soon'
      status(`Connection failed: Direct unavailable, relay blocked (quota exceeded). Resets ${periodEnd}.`)
    } else {
      status('WebSocket error')
    }
  }
  ws.onclose = () => {
    if (pc) pc.close()
    resetUI()
  }
}

async function sendJoin() {
  if (!pendingNonce) return
  const hmac = sessionPassword ? await computeHMAC(sessionPassword, pendingNonce) : ''
  ws.send(JSON.stringify({ type: 'join', hmac }))
  pendingNonce = null
}

function submitPassword() {
  sessionPassword = document.getElementById('password-input').value
  sendJoin()
}

function hexToBytes(hex) {
  const out = new Uint8Array(hex.length / 2)
  for (let i = 0; i < out.length; i += 1) {
    out[i] = Number.parseInt(hex.slice(i * 2, i * 2 + 2), 16)
  }
  return out
}

function requestFile(name) {
  if (isDownloading) {
    status('Download in progress, please wait')
    return
  }
  const fullPath = [...currentPath, name].join('/')
  transferChannel.send(JSON.stringify({ type: 'file_request', path: fullPath }))
}

function requestFileList(subpath) {
  transferChannel.send(JSON.stringify({ type: 'list_request', path: subpath }))
}

function handleTransferMessage(event) {
  if (event.data instanceof ArrayBuffer) {
    appendChunk(new Uint8Array(event.data))
    return
  }

  const msg = JSON.parse(event.data)
  switch (msg.type) {
    case 'file_list':
      renderFileList(msg.files)
      break
    case 'file_header':
      startDownload(msg)
      break
    case 'chunk_end':
      completeDownload()
      break
    case 'error':
      handleError(msg)
      break
  }
}

document.addEventListener('DOMContentLoaded', () => {
  initFromURL()
  publishGlobalActions(window, { join, submitPassword, navigateTo })
  if (document.getElementById('code').value) {
    join()
  }
})
```

```html
<!-- signaling-server/web/index.html -->
  <script type="module" src="/src/app.js"></script>
```

- [ ] **Step 4: Run the focused browser app tests**

Run:

```bash
cd signaling-server/web && node --test src/frame.test.js src/directChannel.test.js src/secureRelayChannel.test.js src/connectTransferChannel.test.js src/app.test.js
```

Expected:

- PASS for the new browser-side unit and integration tests

- [ ] **Step 5: Commit**

```bash
git add signaling-server/web/index.html signaling-server/web/src/app.js signaling-server/web/src/app.test.js
git rm signaling-server/web/app.js
git commit -m "feat(web): route browser transfers through direct or secure relay channels"
```

## Task 5: Add Browser-Side Regression Coverage For The Actual Secure-Relay Policy Matrix

**Files:**
- Modify: `signaling-server/web/src/connectTransferChannel.test.js`
- Modify: `signaling-server/web/src/app.test.js`

- [ ] **Step 1: Write the final regression tests for the secure-relay policy matrix**

```js
// signaling-server/web/src/connectTransferChannel.test.js
test('relay_only ignores direct timeout entirely and reports relay mode immediately', async () => {
  const statuses = []
  const connected = await connectTransferChannel({
    relayPolicy: { relayAllowed: true, relayOnly: true },
    directConnect: async () => { throw new Error('direct should be skipped') },
    relayConnect: async () => ({ readyState: 'open', close() {} }),
    onStatusChange: (status) => statuses.push(status),
  })

  assert.equal(connected.mode, 'relay')
  assert.deepEqual(statuses, ['connecting-relay', 'connected-relay'])
})

test('relay_allowed false surfaces a quota-style failure instead of silently trying relay', async () => {
  await assert.rejects(() => connectTransferChannel({
    relayPolicy: { relayAllowed: false, relayOnly: false },
    directConnect: async () => { throw new Error('direct connect timeout after 5000ms') },
    relayConnect: async () => ({ readyState: 'open', close() {} }),
    onStatusChange: () => {},
  }), /direct connect timeout/i)
})
```

```js
// signaling-server/web/src/app.test.js
test('relay_policy with relay_allowed false preserves the quota-exceeded user message on direct failure', async () => {
  const statuses = []
  const controller = installSessionMessageHandler({
    status: (msg) => statuses.push(msg),
    connectTransferChannel: async () => {
      throw new Error('Direct unavailable, relay blocked (quota exceeded).')
    },
  })

  await assert.rejects(() => controller.handleMessage({
    type: 'relay_policy',
    token: 'jwt',
    relay_allowed: false,
    relay_only: false,
  }))

  assert.match(statuses.at(-1), /quota exceeded/i)
})
```

- [ ] **Step 2: Run the full browser test suite to verify the regressions fail first**

Run:

```bash
cd signaling-server/web && node --test src/*.test.js
```

Expected:

- FAIL until the app and connector surface the right status and policy behavior

- [ ] **Step 3: Tighten the implementation until the full browser suite passes**

```js
// signaling-server/web/src/app.js
// In the relay_policy branch, surface the existing quota-style failure copy
// when connectTransferChannel throws while relay is disallowed.
try {
  const connected = await connectFn(/* existing args */)
  transferChannel = connected.channel
} catch (err) {
  if (relayPolicy && relayPolicy.relayAllowed === false) {
    status('Connection failed: Direct unavailable, relay blocked (quota exceeded).')
  } else {
    status(err.message || 'Connection failed')
  }
  throw err
}
```

```js
// signaling-server/web/src/connectTransferChannel.js
// Keep relay_only and relay_allowed behavior explicit and side-effect free.
if (relayPolicy.relayOnly) {
  onStatusChange('connecting-relay')
  const relayChannel = await relayConnect()
  onStatusChange('connected-relay')
  return { channel: relayChannel, mode: 'relay' }
}
```

- [ ] **Step 4: Run the full browser suite to verify everything passes**

Run:

```bash
cd signaling-server/web && node --test src/*.test.js
```

Expected:

- PASS for all browser-side tests

- [ ] **Step 5: Commit**

```bash
git add signaling-server/web/src/connectTransferChannel.test.js signaling-server/web/src/app.test.js signaling-server/web/src/connectTransferChannel.js signaling-server/web/src/app.js
git commit -m "test(web): lock down secure relay browser policy behavior"
```

## Self-Review Coverage

- Secure relay framing, handshake kind `0x00`, and fatal unknown-frame handling are covered in Tasks 1 and 2.
- Browser `SecureRelayChannel`, expected agent static-key pinning, and fail-closed decrypt behavior are covered in Task 2.
- Browser-side `connectTransferChannel(...)`, direct-first / relay-only / relay fallback policy, and the `mode` return shape are covered in Task 3.
- Preserving the existing transfer protocol above the transport boundary is covered in Task 4 by moving the current UI/transfer flow onto the shared channel contract instead of changing message shapes.
- Relay quota / `relay_allowed` / `relay_only` browser behavior and user-visible failure messaging are locked down in Task 5.
