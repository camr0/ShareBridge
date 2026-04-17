# Slice 13c — Browser Transport Replacement Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace `RTCPeerConnection`/`RTCDataChannel` in the browser with a js-libp2p client that connects to the 13a relay over WebSocket Secure, validates its single-use JWT, dials the agent via circuit relay, and streams files using the 5-byte framing + open-envelope contract defined in 13b.

**Architecture:** The browser generates an ephemeral Ed25519 libp2p identity per page load, sends its peer ID with the `join` message so the signaling server can bind it into the JWT (13a R4), receives `{ relay_multiaddr, agent_peer_id, jwt }` after HMAC success, connects to the relay, authenticates via `/sharebridge/relay/1.0.0` (single JWT text frame, ack = one text frame), then dials the agent through the circuit and opens `/sharebridge/file/1.0.0`. The first frame on that stream is the open envelope. All subsequent text/binary messages go through the same 5-byte framing codec defined in 13b. A minimal esbuild step produces a single bundled `app.bundle.js` served as a static asset — there is no package manager for the site today and we keep the surface area small.

**Tech Stack:** js-libp2p, `@libp2p/websockets`, `@libp2p/webrtc` (DCUtR direct path), `@chainsafe/libp2p-noise`, `@chainsafe/libp2p-yamux`, `@libp2p/circuit-relay-v2`, `@multiformats/multiaddr`, esbuild (bundler, dev-dep only).

**Prerequisites:**
- 13a merged (relay listening; JWT issuance wired into `browser_ws.go`; welcome carries `relay_multiaddr`; `/sharebridge/relay/1.0.0` implemented and validates a single-use JWT; agent welcome includes agent peer ID or it is propagated with the JWT message to the browser).
- 13b merged (agent opens inbound file streams via circuit, reads the open envelope, uses the frame codec).
- 13a R4 (signaling server accepts `browser_peer_id` on the `join` message and binds it into the JWT). If 13a left this as TODO, do Task 1 of this plan before anything else.

---

## Scope Check

This plan covers only the browser client (`signaling-server/web/app.js` and supporting build step). No Go changes expected beyond the 13a R4 follow-through called out explicitly in Task 1. No test files are added to the Go side — E2E validation runs in a real Chromium in Task 11.

---

## File Structure

**New:**
- `signaling-server/web/src/frame.js` — 5-byte framing codec (`writeFrame`, `readFrameStream`). Mirrors `agent/internal/transport/frame.go` byte-for-byte.
- `signaling-server/web/src/libp2pClient.js` — js-libp2p node factory, relay dial, JWT handshake, file stream open. One responsibility: transport lifecycle.
- `signaling-server/web/src/dataChannelAdapter.js` — wraps a `libp2p` `Stream` to expose the same `.send(text)`, `.sendBinary(bytes)`, `onmessage`, `onclose` surface the old `RTCDataChannel` code in `app.js` relies on, so the non-transport code in `app.js` barely changes.
- `signaling-server/web/src/app.js` — rewritten top-level app. Replaces `signaling-server/web/app.js`.
- `signaling-server/web/build.mjs` — esbuild script producing `signaling-server/web/app.bundle.js`.
- `signaling-server/web/package.json` — dev-dep manifest.
- `signaling-server/web/.gitignore` — excludes `node_modules/` and the generated bundle (bundle is built on deploy).
- `signaling-server/web/README.md` — one-paragraph note: “how to build the web bundle”.

**Modified:**
- `signaling-server/web/index.html` — swap `<script src="app.js">` for `<script src="app.bundle.js">`.
- `signaling-server/Makefile` or `scripts/deploy.sh` (whichever currently ships static assets) — add a `web-build` step before the Go embed / copy.
- `signaling-server/handler/browser_ws.go` — **only if** 13a R4 wasn’t done: accept `browser_peer_id` on the join payload and pass it into the JWT claim builder.

**Deleted:**
- `signaling-server/web/app.js` (replaced by `web/src/app.js` + bundle output).

---

## Task 1: Confirm signaling server accepts browser_peer_id on join

This is the 13a R4 follow-through. If 13a already merged it, verify and skip.

**Files:**
- Read-only: `signaling-server/handler/browser_ws.go`
- Modify (if needed): `signaling-server/handler/browser_ws.go`, `signaling-server/internal/relay/token.go` (or wherever JWT is minted)

- [ ] **Step 1: Audit**

Run: `rg -n "browser_peer_id|BrowserPeerID" signaling-server/`

Expected: appears in at least three places — (a) the struct for the inbound `join` message, (b) the JWT claim payload, (c) the code that forwards `auth_ok` triggers to JWT minting.

- [ ] **Step 2: If missing, add the field**

In the join struct (inbound from browser WS):

```go
type joinMsg struct {
    Type          string `json:"type"`
    HMAC          string `json:"hmac,omitempty"`
    BrowserPeerID string `json:"browser_peer_id,omitempty"`
}
```

When minting the JWT after `auth_ok` from the agent, include:

```go
claims := relay.TokenClaims{
    JTI:           uuid.NewString(),
    ShareCode:     shareCode,
    BrowserPeerID: join.BrowserPeerID, // propagated from inbound browser message
    RelayAllowed:  relayAllowed,
    DCUtRAllowed:  dcutrAllowed,
    Exp:           time.Now().Add(5 * time.Minute).Unix(),
}
```

And include `agent_peer_id` in the outbound message to the browser so it can address the agent through the circuit:

```go
type relayInfoMsg struct {
    Type           string `json:"type"`             // "relay_info"
    RelayMultiaddr string `json:"relay_multiaddr"`
    AgentPeerID    string `json:"agent_peer_id"`
    JWT            string `json:"jwt"`
}
```

The agent peer ID is already known to the signaling server from the agent’s welcome message; plumb it through.

- [ ] **Step 3: Test**

Run: `cd signaling-server && go test ./handler/... ./internal/relay/...`
Expected: PASS. Add a minimal test asserting that `browser_peer_id` in join is propagated into minted claims and the `relay_info` message to the browser if one doesn’t already exist.

- [ ] **Step 4: Commit**

```bash
git add signaling-server/
git commit -m "feat(signaling): bind browser_peer_id into JWT (13a R4 follow-through)"
```

---

## Task 2: esbuild bundler scaffolding

**Files:**
- Create: `signaling-server/web/package.json`
- Create: `signaling-server/web/build.mjs`
- Create: `signaling-server/web/.gitignore`
- Create: `signaling-server/web/README.md`

- [ ] **Step 1: Write package.json**

```json
{
  "name": "sharebridge-web",
  "private": true,
  "type": "module",
  "scripts": {
    "build": "node build.mjs",
    "watch": "node build.mjs --watch"
  },
  "devDependencies": {
    "esbuild": "^0.25.0"
  },
  "dependencies": {
    "libp2p": "^2.4.0",
    "@libp2p/websockets": "^9.1.0",
    "@libp2p/webrtc": "^5.1.0",
    "@libp2p/circuit-relay-v2": "^3.1.0",
    "@libp2p/identify": "^3.1.0",
    "@chainsafe/libp2p-noise": "^16.0.0",
    "@chainsafe/libp2p-yamux": "^7.0.0",
    "@multiformats/multiaddr": "^12.3.0",
    "it-pipe": "^3.0.0",
    "it-length-prefixed": "^9.1.0"
  }
}
```

Version pins are approximate: bump to current `@latest` at `npm install` time and commit the resulting `package-lock.json`. Any future libp2p major bump is a deliberate upgrade, not an implicit one.

- [ ] **Step 2: Write build.mjs**

```js
import { build, context } from 'esbuild';

const opts = {
  entryPoints: ['src/app.js'],
  outfile: 'app.bundle.js',
  bundle: true,
  format: 'esm',
  target: ['es2022'],
  platform: 'browser',
  sourcemap: true,
  minify: process.env.NODE_ENV === 'production',
  loader: { '.js': 'js' },
};

if (process.argv.includes('--watch')) {
  const ctx = await context(opts);
  await ctx.watch();
  console.log('watching...');
} else {
  await build(opts);
}
```

- [ ] **Step 3: Write .gitignore**

```
node_modules/
app.bundle.js
app.bundle.js.map
```

- [ ] **Step 4: Write README.md**

```markdown
# ShareBridge web frontend

Single-page app. Bundled with esbuild because we ship js-libp2p which is ESM-only.

## Build

    cd signaling-server/web
    npm install
    npm run build       # produces app.bundle.js
    npm run watch       # rebuild on change

The generated `app.bundle.js` is not checked in — it is produced by `make web-build` during deploy and consumed by the Go embed / static-file handler.
```

- [ ] **Step 5: Install and smoke-test build**

```bash
cd signaling-server/web
npm install
mkdir -p src
echo "console.log('bundle works');" > src/app.js
npm run build
ls app.bundle.js
```

Expected: `app.bundle.js` exists and runs `console.log` when loaded.

- [ ] **Step 6: Commit**

```bash
git add signaling-server/web/package.json signaling-server/web/package-lock.json signaling-server/web/build.mjs signaling-server/web/.gitignore signaling-server/web/README.md
git commit -m "build(web): esbuild scaffolding for js-libp2p bundle"
```

---

## Task 3: Hook the bundle into the deploy path

**Files:**
- Modify: `signaling-server/Makefile` (or the equivalent deploy script; grep for `web` first)

- [ ] **Step 1: Find how static assets ship today**

Run:

```bash
rg -n "web/" signaling-server/Makefile signaling-server/*.sh 2>/dev/null
rg -n "embed" signaling-server/ --glob '*.go'
```

Two possibilities: (a) Go `embed.FS` pulls `web/*` at compile time, or (b) a deploy script copies `web/*` into the server image. Either way, the new artifact is `web/app.bundle.js` (and `web/app.bundle.js.map`).

- [ ] **Step 2: Add web-build target**

If Makefile-based:

```makefile
.PHONY: web-build
web-build:
	cd web && npm ci && npm run build

build: web-build
	go build ./...
```

If a shell deploy script, add `cd web && npm ci && npm run build` before the Go build line. Ensure CI runs Node 22+.

- [ ] **Step 3: Verify `go build ./...` still succeeds**

```bash
cd signaling-server && make web-build && go build ./...
```

Expected: bundle built, Go build succeeds, server binary embeds the bundle if using `embed.FS`.

- [ ] **Step 4: Commit**

```bash
git add signaling-server/Makefile
git commit -m "build(signaling): run web-build before go build"
```

---

## Task 4: Frame codec (JS mirror of 13b frame.go)

Byte-for-byte compatible with `agent/internal/transport/frame.go`: 1-byte kind (0x01 text, 0x02 binary) + 4-byte BE uint32 length + payload. Max payload 8 MiB.

**Files:**
- Create: `signaling-server/web/src/frame.js`
- Create: `signaling-server/web/src/frame.test.mjs`
- Modify: `signaling-server/web/package.json` (add `test` script)

- [ ] **Step 1: Write the failing test**

```js
// src/frame.test.mjs — run with `node --test src/frame.test.mjs`
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { FRAME_TEXT, FRAME_BINARY, writeFrame, readFrameFromChunks } from './frame.js';

test('round-trip text and binary', () => {
  const textFrame = writeFrame(FRAME_TEXT, new TextEncoder().encode('hello'));
  const binFrame = writeFrame(FRAME_BINARY, new Uint8Array([0xde, 0xad, 0xbe, 0xef]));
  const concat = new Uint8Array(textFrame.length + binFrame.length);
  concat.set(textFrame, 0);
  concat.set(binFrame, textFrame.length);

  const reader = readFrameFromChunks([concat]);
  const first = reader.next();
  assert.equal(first.value.kind, FRAME_TEXT);
  assert.equal(new TextDecoder().decode(first.value.payload), 'hello');

  const second = reader.next();
  assert.equal(second.value.kind, FRAME_BINARY);
  assert.deepEqual(Array.from(second.value.payload), [0xde, 0xad, 0xbe, 0xef]);

  assert.ok(reader.next().done);
});

test('rejects oversized frame', () => {
  // kind=binary, length = 0xFFFFFFFF
  const bad = new Uint8Array([0x02, 0xff, 0xff, 0xff, 0xff]);
  assert.throws(() => readFrameFromChunks([bad]).next(), /too large/);
});

test('rejects unknown kind', () => {
  const bad = new Uint8Array([0x99, 0, 0, 0, 0]);
  assert.throws(() => readFrameFromChunks([bad]).next(), /unknown frame kind/);
});

test('handles chunk boundary mid-header', () => {
  const frame = writeFrame(FRAME_TEXT, new TextEncoder().encode('xy'));
  const chunk1 = frame.slice(0, 2);
  const chunk2 = frame.slice(2);
  const reader = readFrameFromChunks([chunk1, chunk2]);
  const { value } = reader.next();
  assert.equal(value.kind, FRAME_TEXT);
  assert.equal(new TextDecoder().decode(value.payload), 'xy');
});
```

Add `"test": "node --test src/*.test.mjs"` to `package.json` scripts.

- [ ] **Step 2: Run test, expect failure**

```bash
cd signaling-server/web && npm test
```

Expected: FAIL — `frame.js` doesn’t exist yet.

- [ ] **Step 3: Implement frame.js**

```js
// src/frame.js
export const FRAME_TEXT = 0x01;
export const FRAME_BINARY = 0x02;
export const MAX_FRAME_PAYLOAD = 8 * 1024 * 1024;

// writeFrame: returns a single Uint8Array containing the 5-byte header + payload.
export function writeFrame(kind, payload) {
  if (kind !== FRAME_TEXT && kind !== FRAME_BINARY) {
    throw new Error(`unknown frame kind: 0x${kind.toString(16)}`);
  }
  if (payload.length > MAX_FRAME_PAYLOAD) {
    throw new Error(`frame too large: ${payload.length} bytes`);
  }
  const out = new Uint8Array(5 + payload.length);
  out[0] = kind;
  out[1] = (payload.length >>> 24) & 0xff;
  out[2] = (payload.length >>> 16) & 0xff;
  out[3] = (payload.length >>> 8) & 0xff;
  out[4] = payload.length & 0xff;
  out.set(payload, 5);
  return out;
}

// readFrameFromChunks: iterator-style decoder over a sequence of Uint8Array
// chunks. Yields { kind, payload } for each complete frame. Used in tests; in
// production we feed chunks from a libp2p stream (see libp2pClient.js).
export function* readFrameFromChunks(chunks) {
  const it = new FrameDecoder();
  for (const chunk of chunks) {
    for (const frame of it.push(chunk)) yield frame;
  }
}

// FrameDecoder: streaming decoder. Call push(chunk) to get an iterable of
// complete frames extracted so far. Holds partial data internally.
export class FrameDecoder {
  constructor() {
    this._buf = new Uint8Array(0);
  }

  *push(chunk) {
    if (chunk.length === 0) return;
    const combined = new Uint8Array(this._buf.length + chunk.length);
    combined.set(this._buf, 0);
    combined.set(chunk, this._buf.length);
    this._buf = combined;

    while (this._buf.length >= 5) {
      const kind = this._buf[0];
      if (kind !== FRAME_TEXT && kind !== FRAME_BINARY) {
        throw new Error(`unknown frame kind: 0x${kind.toString(16)}`);
      }
      const length =
        (this._buf[1] << 24) >>> 0 |
        (this._buf[2] << 16) |
        (this._buf[3] << 8) |
        this._buf[4];
      if (length > MAX_FRAME_PAYLOAD) {
        throw new Error(`frame too large: ${length} bytes`);
      }
      if (this._buf.length < 5 + length) break;
      const payload = this._buf.slice(5, 5 + length);
      this._buf = this._buf.slice(5 + length);
      yield { kind, payload };
    }
  }
}
```

- [ ] **Step 4: Run test, expect pass**

```bash
cd signaling-server/web && npm test
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add signaling-server/web/src/frame.js signaling-server/web/src/frame.test.mjs signaling-server/web/package.json
git commit -m "feat(web): 5-byte framing codec matching agent/internal/transport"
```

---

## Task 5: DataChannel adapter over a libp2p stream

Expose a shim with the same surface the old `RTCDataChannel` consumer code uses (`.send`, `.binaryType`, `.onopen`, `.onmessage`, `.onclose`) so the download / file-list logic in `app.js` can stay byte-identical.

**Files:**
- Create: `signaling-server/web/src/dataChannelAdapter.js`

- [ ] **Step 1: Implement**

```js
// src/dataChannelAdapter.js
import { FRAME_TEXT, FRAME_BINARY, writeFrame, FrameDecoder } from './frame.js';

// LibP2PDataChannel wraps a libp2p stream to look like an RTCDataChannel to
// the rest of app.js: assign .onopen, .onmessage, .onclose; call .send() /
// .sendBinary() to write.
//
// .readyState mirrors the browser DataChannel enum ('connecting' | 'open' |
// 'closing' | 'closed').
//
// binaryType is fixed to 'arraybuffer' to match the existing app.js
// assumption (app.js:184).
export class LibP2PDataChannel {
  constructor(stream) {
    this._stream = stream;
    this._decoder = new FrameDecoder();
    this.readyState = 'connecting';
    this.binaryType = 'arraybuffer';
    this.onopen = null;
    this.onmessage = null;
    this.onclose = null;
  }

  // start: begin the read pump. Call once from libp2pClient after the first
  // non-handshake frame is expected. Resolves when the stream ends.
  async start() {
    this.readyState = 'open';
    queueMicrotask(() => { if (this.onopen) this.onopen(); });
    try {
      for await (const chunk of this._stream.source) {
        const bytes = chunk.subarray ? chunk.subarray() : chunk;
        for (const frame of this._decoder.push(bytes)) {
          this._dispatch(frame);
        }
      }
    } catch (err) {
      console.error('libp2p stream read error:', err);
    } finally {
      this.readyState = 'closed';
      if (this.onclose) this.onclose();
    }
  }

  _dispatch({ kind, payload }) {
    if (!this.onmessage) return;
    if (kind === FRAME_TEXT) {
      this.onmessage({ data: new TextDecoder().decode(payload) });
    } else {
      // binaryType arraybuffer → deliver ArrayBuffer, not Uint8Array
      this.onmessage({ data: payload.buffer.slice(payload.byteOffset, payload.byteOffset + payload.byteLength) });
    }
  }

  // send: JSON string messages (control plane — same shape as old
  // dc.send(JSON.stringify(...))). Assumes the caller has already stringified.
  async send(text) {
    if (this.readyState !== 'open') throw new Error('data channel not open');
    const payload = new TextEncoder().encode(text);
    await this._stream.sink([writeFrame(FRAME_TEXT, payload)]);
  }

  // sendBinary: bytes message (unused by the current browser but kept for
  // parity). Present so the shim is symmetric with the agent-side adapter.
  async sendBinary(bytes) {
    if (this.readyState !== 'open') throw new Error('data channel not open');
    await this._stream.sink([writeFrame(FRAME_BINARY, bytes)]);
  }

  close() {
    this.readyState = 'closing';
    this._stream.close().catch(() => {});
  }
}
```

Note: js-libp2p streams use `it-pipe`-style `.source`/`.sink` duplex iterables. The sink normally consumes a source — sending one message per sink call is inefficient at the protocol level but simple. Task 10 is an optional follow-up to use a real sink pump via `it-pushable` if profiling shows head-of-line stalls. For now: correctness over throughput on the browser-to-agent path (upstream bytes are tiny — control messages only).

- [ ] **Step 2: Commit (no test yet — exercised end-to-end in Task 11)**

```bash
git add signaling-server/web/src/dataChannelAdapter.js
git commit -m "feat(web): RTCDataChannel-shaped adapter over libp2p stream"
```

---

## Task 6: libp2p client — node, relay dial, JWT handshake, file stream

This is the biggest file. Single entry point `connect()` that returns a `LibP2PDataChannel` once the file stream is open and the `open` envelope has been sent.

**Files:**
- Create: `signaling-server/web/src/libp2pClient.js`

- [ ] **Step 1: Implement**

```js
// src/libp2pClient.js
import { createLibp2p } from 'libp2p';
import { webSockets } from '@libp2p/websockets';
import { webRTC } from '@libp2p/webrtc';
import { noise } from '@chainsafe/libp2p-noise';
import { yamux } from '@chainsafe/libp2p-yamux';
import { circuitRelayTransport } from '@libp2p/circuit-relay-v2';
import { identify } from '@libp2p/identify';
import { multiaddr } from '@multiformats/multiaddr';
import { writeFrame, FRAME_TEXT, FrameDecoder } from './frame.js';
import { LibP2PDataChannel } from './dataChannelAdapter.js';

const RELAY_AUTH_PROTOCOL = '/sharebridge/relay/1.0.0';
const FILE_PROTOCOL = '/sharebridge/file/1.0.0';

// createNode: ephemeral libp2p node. We do NOT persist the peer identity —
// the browser mints a fresh Ed25519 keypair every page load.
export async function createNode() {
  return await createLibp2p({
    transports: [
      webSockets(),                  // outbound connection to relay
      circuitRelayTransport(),       // dial agent through relay
      webRTC(),                      // DCUtR direct-path upgrade
    ],
    connectionEncrypters: [noise()],
    streamMuxers: [yamux()],
    services: { identify: identify() },
  });
}

// getLocalPeerId: call before knock/join so the browser can advertise its
// peer ID to the signaling server (for JWT binding).
export function getLocalPeerId(node) {
  return node.peerId.toString();
}

// connect: given a relay multiaddr string, an agent peer ID string, and a
// JWT, perform:
//   1. dial relay
//   2. open /sharebridge/relay/1.0.0, send JWT frame, await ack
//   3. dial agent via the circuit
//   4. open /sharebridge/file/1.0.0 on the agent
//   5. send the open envelope as the first text frame
//   6. return a LibP2PDataChannel wrapping the stream (readyState='open')
//
// Throws on any failure. The caller handles UI feedback.
export async function connect(node, { relayMultiaddr, agentPeerId, jwt, shareCode, connId }) {
  const relayAddr = multiaddr(relayMultiaddr);
  await node.dial(relayAddr);

  // Step 2: JWT handshake on the relay.
  const authStream = await node.dialProtocol(relayAddr, RELAY_AUTH_PROTOCOL);
  const jwtBytes = new TextEncoder().encode(JSON.stringify({ type: 'jwt', token: jwt }));
  await authStream.sink([writeFrame(FRAME_TEXT, jwtBytes)]);
  await expectAck(authStream);
  // leave authStream open — closing it would tear the circuit on some relay
  // implementations. Relay will close it on token expiry or session end.

  // Step 3+4: dial agent through the circuit by composing a p2p-circuit addr.
  const circuitAddr = multiaddr(
    `${relayMultiaddr}/p2p-circuit/p2p/${agentPeerId}`
  );
  const fileStream = await node.dialProtocol(circuitAddr, FILE_PROTOCOL);

  // Step 5: open envelope.
  const envelope = JSON.stringify({ type: 'open', share_code: shareCode, conn_id: connId });
  await fileStream.sink([writeFrame(FRAME_TEXT, new TextEncoder().encode(envelope))]);

  // Step 6: wrap and return. Caller starts the read pump.
  const channel = new LibP2PDataChannel(fileStream);
  // Intentionally no await — the pump runs for the lifetime of the channel.
  channel.start();
  return channel;
}

async function expectAck(stream) {
  const decoder = new FrameDecoder();
  for await (const chunk of stream.source) {
    const bytes = chunk.subarray ? chunk.subarray() : chunk;
    for (const frame of decoder.push(bytes)) {
      if (frame.kind !== FRAME_TEXT) throw new Error('relay ack not text frame');
      const msg = JSON.parse(new TextDecoder().decode(frame.payload));
      if (msg.type === 'auth_ok') return;
      if (msg.type === 'error') throw new Error(`relay auth rejected: ${msg.message || 'unknown'}`);
      throw new Error(`unexpected relay message: ${msg.type}`);
    }
  }
  throw new Error('relay closed stream before ack');
}
```

Protocol contracts introduced here that 13a must honour — cross-check before shipping:
- `/sharebridge/relay/1.0.0` framing is the same 5-byte codec as `/sharebridge/file/1.0.0`.
- First frame from browser is `{"type":"jwt","token":"..."}` as `FRAME_TEXT`.
- Relay replies with one `FRAME_TEXT`: either `{"type":"auth_ok"}` or `{"type":"error","message":"..."}`.
- Relay does not close the auth stream proactively; client leaves it open for the session lifetime.

If 13a’s relay used a different shape (e.g. raw JWT bytes without the JSON envelope), align the relay to this contract — the JSON envelope is trivial and keeps the framing consistent across all `/sharebridge/*` protocols.

- [ ] **Step 2: Commit**

```bash
git add signaling-server/web/src/libp2pClient.js
git commit -m "feat(web): js-libp2p client with relay JWT handshake and file stream open"
```

---

## Task 7: Rewrite app.js — replace WebRTC with libp2p, keep download logic

The existing `signaling-server/web/app.js` is 584 lines. Only the transport and signaling sections change (roughly lines 1–159 and 504–563); everything from `setupDataChannel` downward stays structurally the same because of the adapter in Task 5.

**Files:**
- Create: `signaling-server/web/src/app.js`
- Delete: `signaling-server/web/app.js` (after Task 7 is fully verified)

- [ ] **Step 1: Create the new entry point**

```js
// src/app.js
import { createNode, getLocalPeerId, connect } from './libp2pClient.js';

let ws;
let dc;                 // LibP2PDataChannel (RTCDataChannel-shaped)
let node;               // js-libp2p node
let relayQuotaExceeded = false;
let quotaPeriodEnd = null;

// Download state
let currentFile = null;
let receivedBytes = 0;
let fileChunks = [];
let isDownloading = false;
let transferStartTime = 0;

// Navigation state
let currentPath = [];
let sessionPassword = '';

// HMAC pre-challenge state
let pendingNonce = null;

// Connection info received from the signaling server after auth_ok.
let pendingConnInfo = null; // { relay_multiaddr, agent_peer_id, jwt, conn_id, share_code }

function status(msg) {
  document.getElementById('status').textContent = msg;
}

function showSection(id) {
  document.getElementById(id).classList.remove('hidden');
}

function hideSection(id) {
  document.getElementById(id).classList.add('hidden');
}

async function join() {
  const code = document.getElementById('code').value.trim();
  if (!code) return;
  status('Starting libp2p...');

  relayQuotaExceeded = false;
  quotaPeriodEnd = null;

  // 1. Start the libp2p node so we have a peer ID before the knock.
  if (!node) {
    node = await createNode();
  }
  const browserPeerId = getLocalPeerId(node);

  // 2. Open the signaling WebSocket.
  const protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
  ws = new WebSocket(`${protocol}//${location.host}/ws/client?session=${code}`);

  ws.onopen = () => {
    status('Connecting...');
    // Send knock with our browser peer id up-front so the server has it
    // available when it later mints the JWT.
    ws.send(JSON.stringify({ type: 'knock', browser_peer_id: browserPeerId }));
  };

  ws.onmessage = async (event) => {
    const msg = JSON.parse(event.data);
    switch (msg.type) {
      case 'quota_status':
        if (msg.relay_quota_exceeded) {
          relayQuotaExceeded = true;
          quotaPeriodEnd = msg.quota_period_end;
        }
        break;

      case 'nonce':
        pendingNonce = msg.value;
        if (msg.has_password && !sessionPassword) {
          showSection('password-section');
          document.getElementById('password-input').focus();
        } else {
          await sendJoin(browserPeerId, code);
        }
        break;

      case 'auth_failed': {
        const errorDiv = document.getElementById('password-error');
        const attemptsRemaining = msg.attempts_remaining || 0;
        if (attemptsRemaining <= 0) {
          errorDiv.textContent = 'Too many incorrect attempts. Connection closed.';
          document.getElementById('password-input').disabled = true;
          document.querySelector('#password-section button').disabled = true;
        } else {
          errorDiv.textContent = `Incorrect password. ${attemptsRemaining} attempt${attemptsRemaining === 1 ? '' : 's'} remaining.`;
          document.getElementById('password-input').value = '';
          document.getElementById('password-input').focus();
          ws.send(JSON.stringify({ type: 'knock', browser_peer_id: browserPeerId }));
        }
        break;
      }

      case 'relay_info':
        // Server has confirmed HMAC success and minted the JWT.
        pendingConnInfo = {
          relayMultiaddr: msg.relay_multiaddr,
          agentPeerId: msg.agent_peer_id,
          jwt: msg.jwt,
          shareCode: code,
          connId: msg.conn_id,
        };
        await startTransport();
        break;

      case 'error':
        status('Error: ' + msg.message);
        break;
    }
  };

  ws.onerror = () => {
    if (relayQuotaExceeded) {
      const periodEnd = quotaPeriodEnd ? new Date(quotaPeriodEnd).toLocaleDateString() : 'soon';
      status(`Connection failed: Direct unavailable, relay blocked (quota exceeded). Resets ${periodEnd}.`);
    } else {
      status('WebSocket error');
    }
  };

  ws.onclose = () => {
    // Keep libp2p node alive across signaling disconnects so reconnect is fast.
    if (dc) dc.close();
    resetUI();
  };
}

async function startTransport() {
  try {
    status('Connecting to relay...');
    dc = await connect(node, pendingConnInfo);
    setupDataChannel();
  } catch (err) {
    console.error(err);
    status('Transport error: ' + (err.message || err));
  }
}

async function computeHMAC(password, nonce) {
  const encoder = new TextEncoder();
  const key = await crypto.subtle.importKey(
    'raw',
    encoder.encode(password),
    { name: 'HMAC', hash: 'SHA-256' },
    false,
    ['sign']
  );
  const signature = await crypto.subtle.sign('HMAC', key, encoder.encode(nonce));
  return Array.from(new Uint8Array(signature))
    .map(b => b.toString(16).padStart(2, '0'))
    .join('');
}

async function sendJoin(browserPeerId, shareCode) {
  if (!pendingNonce) return;
  const hmac = sessionPassword ? await computeHMAC(sessionPassword, pendingNonce) : '';
  ws.send(JSON.stringify({
    type: 'join',
    hmac,
    browser_peer_id: browserPeerId,
  }));
  pendingNonce = null;
}

function setupDataChannel() {
  dc.onopen = () => {
    status('Connection open');
    hideSection('join-section');
    hideSection('password-section');
    setTimeout(updateConnectionStatus, 500);
    requestFileList('');
  };

  dc.onmessage = (event) => {
    if (event.data instanceof ArrayBuffer) {
      const bytes = new Uint8Array(event.data);
      appendChunk(bytes);
      return;
    }
    const msg = JSON.parse(event.data);
    switch (msg.type) {
      case 'file_list':   renderFileList(msg.files); break;
      case 'file_header': startDownload(msg); break;
      case 'chunk_end':   completeDownload(); break;
      case 'error':       handleError(msg); break;
    }
  };

  dc.onclose = () => {
    status('Connection closed');
    resetUI();
  };
}

// renderFileList, getFileItem, requestFile, startDownload, appendChunk,
// completeDownload, markFileDone, renderBreadcrumb, navigateTo, openFolder,
// submitPassword, requestFileList, handleError, resetUI, escapeHtml,
// formatBytes, formatSpeed — COPY VERBATIM from the old app.js.
// Replace `dc.send(...)` call sites (no change needed — send takes a string).
// Replace `pc.close()` in onclose handlers with `dc.close()`.

// ... (copy those functions unchanged — they do not touch the transport) ...

async function updateConnectionStatus() {
  // libp2p gives us explicit connection type instead of inferring from ICE.
  const statusEl = document.getElementById('connection-status');
  const typeEl = document.getElementById('connection-type');
  statusEl.classList.remove('hidden');
  typeEl.classList.remove('connection-direct', 'connection-relay');

  const type = detectConnectionType();
  if (type === 'direct') {
    typeEl.textContent = '● Connected (Direct)';
    typeEl.classList.add('connection-direct');
  } else {
    typeEl.textContent = '● Connected (Relay)';
    typeEl.classList.add('connection-relay');
  }
}

function detectConnectionType() {
  // If any open connection to the agent peer uses the webrtc transport, we
  // hole-punched. Otherwise we are on the circuit relay.
  if (!node || !pendingConnInfo) return 'relay';
  const agentId = pendingConnInfo.agentPeerId;
  const conns = node.getConnections().filter(c => c.remotePeer.toString() === agentId);
  for (const c of conns) {
    const addr = c.remoteAddr.toString();
    // p2p-circuit in the addr → relay; webrtc transport component → direct.
    if (addr.includes('/webrtc')) return 'direct';
  }
  return 'relay';
}

// Periodically re-check in case DCUtR upgrades the connection mid-session.
setInterval(() => {
  if (dc && dc.readyState === 'open') updateConnectionStatus();
}, 5000);

function initFromURL() {
  const parts = window.location.pathname.split('/');
  if (parts[1] === 's' && parts[2]) {
    document.getElementById('code').value = parts[2];
  }
  if (window.location.hash) {
    sessionPassword = decodeURIComponent(window.location.hash.slice(1));
    history.replaceState(null, '', window.location.pathname);
  }
}

// Expose functions used by inline HTML onclick handlers.
window.join = join;
window.submitPassword = submitPassword;
window.navigateTo = navigateTo;

document.addEventListener('DOMContentLoaded', () => {
  initFromURL();
  if (document.getElementById('code').value) {
    join();
  }
});
```

For the block marked `// ... (copy those functions unchanged — they do not touch the transport) ...`, copy the following functions **exactly** from `signaling-server/web/app.js` into this file, adjusting only the two notes above:

- `renderFileList` (lines 225–278)
- `getFileItem` (lines 280–282)
- `requestFile` (lines 284–291)
- `startDownload` (lines 293–308)
- `appendChunk` (lines 310–328)
- `completeDownload` (lines 330–397)
- `markFileDone` (lines 399–410)
- `renderBreadcrumb` (lines 412–430)
- `navigateTo` (lines 432–436)
- `openFolder` (lines 438–445)
- `submitPassword` (lines 447–450)
- `requestFileList` (lines 452–454)
- `handleError` (lines 456–460)
- `resetUI` (lines 462–482)
- `escapeHtml` (lines 484–488)
- `formatBytes` (lines 490–496)
- `formatSpeed` (lines 498–502)

- [ ] **Step 2: Rebuild bundle**

```bash
cd signaling-server/web && npm run build
```

Expected: `app.bundle.js` rebuilds clean. Bundle size will be on the order of 1–2 MB before gzip; don’t panic, browsers cache it.

- [ ] **Step 3: Update index.html**

Edit `signaling-server/web/index.html`: replace

```html
<script src="app.js"></script>
```

with

```html
<script type="module" src="app.bundle.js"></script>
```

- [ ] **Step 4: Delete the old app.js**

```bash
rm signaling-server/web/app.js
```

- [ ] **Step 5: Commit**

```bash
git add signaling-server/web/src/app.js signaling-server/web/index.html signaling-server/web/app.js
git commit -m "feat(web): replace WebRTC with js-libp2p transport"
```

---

## Task 8: Signaling server — send relay_info instead of ice_config

**Files:**
- Modify: `signaling-server/handler/browser_ws.go`

This depends on 13a’s structure. If 13a already sends `relay_info` in this shape, verify and skip.

- [ ] **Step 1: Find the ice_config / ICE message path**

Run: `rg -n '"ice_config"|ice_servers|ICEServers' signaling-server/`

- [ ] **Step 2: Replace ice_config with relay_info**

Anywhere the server writes `{"type":"ice_config","ice_servers":[...]}` to the browser WS, replace with:

```go
resp := map[string]any{
    "type":            "relay_info",
    "relay_multiaddr": srv.Config.RelayPublicMultiaddr, // e.g. "/dns4/relay.sharebridge.app/tcp/443/wss/p2p/<relay-peer-id>"
    "agent_peer_id":   session.AgentPeerID,
    "jwt":             token,
    "conn_id":         connID,
}
if err := ws.WriteJSON(resp); err != nil { ... }
```

Delete the `"offer"` and `"ice_candidate"` browser-WS branches entirely — the browser no longer exchanges SDP.

Update the `knock` inbound struct to accept `browser_peer_id`:

```go
type knockMsg struct {
    Type          string `json:"type"`
    BrowserPeerID string `json:"browser_peer_id,omitempty"`
}
```

Store the browser peer ID on the session state so `auth_ok` from the agent can trigger a `relay_info` with the correct peer ID bound into the JWT.

- [ ] **Step 3: Build and test**

```bash
cd signaling-server && go build ./... && go test ./...
```

Expected: PASS. Any handler tests that referenced `ice_config` / `offer` / `ice_candidate` must be updated or deleted.

- [ ] **Step 4: Commit**

```bash
git add signaling-server/
git commit -m "feat(signaling): send relay_info to browser after auth_ok"
```

---

## Task 9: Drop coturn / TURN credential code

This was earmarked for 13a but trivial leftovers often survive. Sweep the repo.

**Files:**
- Various, discovered below.

- [ ] **Step 1: Audit**

```bash
rg -n "coturn|turn_secret|TURN_|turnserver" --glob '!docs/' --glob '!memory/'
```

Anything left in production code (not docs, not memory) should be removed.

- [ ] **Step 2: Remove and commit each hit**

For each hit in Go or JS source, delete the code and adjacent config keys. Run `go build ./...` and `cd signaling-server/web && npm run build` after each removal batch to catch typos.

- [ ] **Step 3: Commit**

```bash
git add -A
git commit -m "chore: remove leftover TURN/coturn references"
```

(Skip this task entirely if Step 1 produced no hits.)

---

## Task 10: Ensure backpressure on the browser-to-agent text path

The browser sends only tiny control messages (`list_request`, `file_request`) — the adapter’s one-shot `sink([frame])` pattern is fine at this volume. But a future parallel-downloads slice may pressure this path. Add a simple guard so the read loop cannot OOM the browser if the agent misbehaves.

**Files:**
- Modify: `signaling-server/web/src/dataChannelAdapter.js`

- [ ] **Step 1: Cap in-flight received bytes**

In `LibP2PDataChannel.start()`, add a soft limit:

```js
const MAX_INFLIGHT_BYTES = 32 * 1024 * 1024; // 32 MiB
let inflight = 0;
// ...
for await (const chunk of this._stream.source) {
  const bytes = chunk.subarray ? chunk.subarray() : chunk;
  inflight += bytes.length;
  if (inflight > MAX_INFLIGHT_BYTES) {
    console.warn('inflight bytes exceeded cap; aborting');
    this.close();
    break;
  }
  for (const frame of this._decoder.push(bytes)) {
    this._dispatch(frame);
    inflight -= (5 + frame.payload.length);
  }
}
```

This is belt-and-braces; yamux-level credits should keep this from firing in practice.

- [ ] **Step 2: Rebuild**

```bash
cd signaling-server/web && npm run build
```

- [ ] **Step 3: Commit**

```bash
git add signaling-server/web/src/dataChannelAdapter.js
git commit -m "feat(web): guard against unbounded in-flight receive"
```

---

## Task 11: End-to-end browser verification + manual checks

**Files:** none.

- [ ] **Step 1: Full local stack**

```bash
# Terminal 1: signaling server + relay
cd signaling-server && make web-build && go run ./cmd/server

# Terminal 2: agent (13b)
cd agent && SIGNALING_SERVER=ws://127.0.0.1:8080 \
    SHAREBRIDGE_API_KEY=<key> \
    go run ./cmd/agent

# Terminal 3: Chromium
# Navigate to http://127.0.0.1:8080/s/<code> (or with #password if protected)
```

- [ ] **Step 2: Verify the golden path**

- Browser loads, libp2p node initialises (check devtools console for "created node with peer id ...").
- Knock → nonce → join → `relay_info` received.
- Relay dial succeeds, JWT accepted, file stream opens.
- File list renders.
- Download a small file (< 10 MiB). SHA-1 verification passes.
- Connection indicator reads **Relay** initially; if DCUtR succeeds, flips to **Direct** within 10s.

- [ ] **Step 3: Verify the error paths**

- Wrong password → three attempts → WS closed (unchanged from today).
- Relay-only share (`relay_only=true` in agent config) → indicator stays on **Relay** even with DCUtR capable.
- Expired JWT (shorten TTL in 13a for this test) → browser receives relay error, status updates, retry requires a fresh knock.
- Server restart during session → channel closes; `resetUI()` fires.

- [ ] **Step 4: Verify large file**

Download a 500 MB file over the relay. Throughput should exceed the Chrome SCTP ceiling of ~5 MB/s on 50 ms latency links (that was the reason for this whole slice). Record observed MB/s in the PR description.

- [ ] **Step 5: Verify cross-browser**

Run the golden path on: Chromium, Firefox, Safari (≥17), plus one Chromium on a mobile device. Any failures are blockers for ship — note in PR and fix before merge.

- [ ] **Step 6: Commit (if any adjustments made)**

```bash
git add -A
git commit -m "fix(web): cross-browser adjustments after E2E verification"
```

(Skip if Steps 2–5 passed without changes.)

- [ ] **Step 7: Push branch and open PR**

```bash
git push -u origin slice-13c-browser
```

PR description must include:
- observed throughput numbers on relay vs direct for a large file,
- cross-browser matrix with pass/fail for each,
- bundle size (gzipped) and a note that the bundle is rebuilt on every deploy,
- explicit confirmation that the framing + open-envelope + relay-auth contracts match 13b’s Go implementation (grep for `0x01`/`0x02` constants in both trees to prove equality).

---

## Self-Review Notes

**Spec coverage (from `docs/superpowers/specs/2026-04-16-slice13-libp2p-transport-design.md` — Browser bullet and Connection Protocol section):**
- ✅ `RTCPeerConnection`, `RTCDataChannel`, ICE, SDP removed (Task 7).
- ✅ js-libp2p node with `@libp2p/websockets` + `@libp2p/webrtc` (Task 6 Step 1).
- ✅ Noise + yamux configured (Task 6 Step 1).
- ✅ Connection-type indicator driven by explicit libp2p state (Task 7 `detectConnectionType`).
- ✅ Receive `{ relay_multiaddr, jwt }` from signaling WS — extended to also include `agent_peer_id` and `conn_id` (Tasks 1, 6, 8).
- ✅ Attempt DCUtR unless `relay_only` — implicit via `@libp2p/webrtc` transport being enabled; the 13a relay enforces `dcutr_allowed=false` on relay-only shares by refusing to forward DCUtR coordination frames (Task 11 Step 3 verifies).

**Cross-slice contracts locked in this plan (must match 13b exactly):**
- Frame layout: 1-byte kind ∈ {0x01, 0x02}, 4-byte BE uint32 length, payload, 8 MiB cap — `frame.js` and `frame.go` are identical bit-for-bit.
- Open envelope: first `FRAME_TEXT` frame on `/sharebridge/file/1.0.0` is `{"type":"open","share_code":"<code>","conn_id":"<id>"}`.
- Relay auth envelope: first `FRAME_TEXT` frame on `/sharebridge/relay/1.0.0` is `{"type":"jwt","token":"<jwt>"}`, reply is `{"type":"auth_ok"}` or `{"type":"error","message":"..."}`.

**Known follow-ups / risks:**
- Bundle size: js-libp2p + transports + noise is heavy. Gzipped should land under ~500 KB; if it exceeds that, try tree-shaking with `minify: true` and consider the `@libp2p/*/min` exports if they exist at install time.
- Safari WebRTC support for DCUtR in `@libp2p/webrtc` was still flaky as of the last check; if Safari can’t hole-punch it falls back to relay (correct) but the indicator stays on "Relay" — which is fine, just document in the PR.
- Reconnect on signaling WS drop: the libp2p node outlives the WS (Task 7 Step 1 intentionally keeps `node` alive). If the WS reconnects during an open session, the existing `dc` still works — session continuity is free.
- 13a R4 (browser_peer_id binding) is a hard prerequisite. If the 13a merge left it as a TODO, Task 1 is the first real commit in this slice, not a verification.
