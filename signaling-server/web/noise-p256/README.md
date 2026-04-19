# Noise XX P-256 Library

Small `Noise_XX_P256_AESGCM_SHA256` implementations for ShareBridge in:

- JS/Web Crypto: `signaling-server/web/noise-p256/`
- Go: `agent/internal/noise/`

This library gives you a compact Noise XX handshake plus post-handshake AES-256-GCM cipher states. It is intended for app-layer end-to-end encryption over an existing byte stream such as WebSocket relay framing.

## What It Implements

- Handshake pattern: `Noise_XX`
- DH: P-256
- Cipher: AES-256-GCM
- Hash/HKDF: SHA-256 / Noise HKDF
- Roles: initiator and responder
- Post-handshake API: caller-perspective `send` / `recv` cipher states

The handshake message sizes are fixed:

- `msg1`: 65 bytes
- `msg2`: 162 bytes
- `msg3`: 97 bytes

## What It Does Not Do

- transport framing
- session management
- password auth
- certificate handling
- automatic agent static-key pinning

Callers still need to:

- frame handshake and ciphertext messages on the wire
- verify the responder static public key against the expected value
- close the session on decrypt/authentication failures
- wrap this behind a higher-level app boundary so the rest of the app does not touch raw handshake steps directly

## JS Usage

Entry point:

```js
import { NoiseXX } from './index.js'
```

Example:

```js
const initiator = await NoiseXX.createInitiator()

// Responder side needs its long-lived static keypair.
const responder = await NoiseXX.createResponder(
  responderStatic.privateKey,
  responderStatic.publicKeyBytes
)

const msg1 = await initiator.writeMessage1()
await responder.readMessage1(msg1)

const msg2 = await responder.writeMessage2()
await initiator.readMessage2(msg2)

// Pin this to the expected agent static key before continuing.
const responderStaticPub = initiator.remoteStaticPub

const msg3 = await initiator.writeMessage3()
await responder.readMessage3(msg3)

const [send, recv] = await initiator.split()
const ciphertext = await send.encrypt(new Uint8Array(0), plaintext)
const plaintextAgain = await recv.decrypt(new Uint8Array(0), ciphertextFromPeer)
```

Public JS API:

- `await NoiseXX.createInitiator()`
- `await NoiseXX.createResponder(staticPrivKey, staticPubKeyBytes)`
- `await noise.writeMessage1()`
- `await noise.readMessage1(msg1)`
- `await noise.writeMessage2()`
- `await noise.readMessage2(msg2)`
- `await noise.writeMessage3()`
- `await noise.readMessage3(msg3)`
- `await noise.split()` returns `[send, recv]`
- `noise.remoteStaticPub`

`CipherState` methods:

- `await send.encrypt(ad, plaintext)`
- `await recv.decrypt(ad, ciphertext)`

For normal ShareBridge use, `ad` should usually be an empty byte string.

## Go Usage

Go package path:

```go
import "sharebridge/agent/internal/noise"
```

Note: this is an `internal` package today, so it is reusable inside this Go module, not as a public external Go dependency.

Example:

```go
initiator, err := noise.NewInitiator()
if err != nil {
    return err
}

responder, err := noise.NewResponder(responderStaticPriv)
if err != nil {
    return err
}

msg1, err := initiator.WriteMessage1()
if err != nil {
    return err
}
if err := responder.ReadMessage1(msg1); err != nil {
    return err
}

msg2, err := responder.WriteMessage2()
if err != nil {
    return err
}
if err := initiator.ReadMessage2(msg2); err != nil {
    return err
}

// Pin this to the expected agent static key before continuing.
responderStaticPub := initiator.RemoteStaticPub()

msg3, err := initiator.WriteMessage3()
if err != nil {
    return err
}
if err := responder.ReadMessage3(msg3); err != nil {
    return err
}

send, recv := initiator.Split()
ct, err := send.Encrypt(nil, plaintext)
pt, err := recv.Decrypt(nil, ciphertextFromPeer)
```

Public Go API:

- `noise.NewInitiator()`
- `noise.NewResponder(staticPriv)`
- `(*NoiseXX).WriteMessage1()`
- `(*NoiseXX).ReadMessage1(msg1)`
- `(*NoiseXX).WriteMessage2()`
- `(*NoiseXX).ReadMessage2(msg2)`
- `(*NoiseXX).WriteMessage3()`
- `(*NoiseXX).ReadMessage3(msg3)`
- `(*NoiseXX).RemoteStaticPub()`
- `(*NoiseXX).Split()` returns `(send, recv)`

`CipherState` methods:

- `Encrypt(ad, plaintext)`
- `Decrypt(ad, ciphertext)`

## Split Semantics

`split()` / `Split()` always returns cipher states from the caller's perspective:

- initiator: `(send=i->r, recv=r->i)`
- responder: `(send=r->i, recv=i->r)`

That means both sides can use the same mental model after handshake completion:

- write with `send`
- read with `recv`

## Failure Behavior

The library fails closed on common misuse and crypto errors:

- out-of-order handshake calls error
- wrong message lengths error
- AEAD authentication failures error
- zero/uninitialized cipher keys error
- nonce exhaustion error

Callers should treat handshake/decrypt failures as fatal for the session.

## Verification

The implementation is tested three ways:

1. Self-written unit and integration tests for transcript, cipher, key ops, and handshake flow.
2. Cross-language deterministic Go/JS interop tests for `msg1`, `msg2`, `msg3`, `k1`, and `k2`.
3. Independent official vectors:
   - NIST P-256 CDH vectors
   - official Noise `cacophony` vector replay for `Noise_XX_25519_AESGCM_SHA256`

Helpful commands:

```bash
cd agent
go test ./internal/noise/...
```

```bash
cd /Users/ali/Git/ShareBridge/.worktrees/noise-p256
node --test signaling-server/web/noise-p256/*.test.js
```

## Recommended Integration Pattern

Use this library as a crypto primitive, not as your whole session API.

The safest app shape is a thin wrapper that:

- performs the full handshake
- pins `remoteStaticPub` against the expected agent key
- owns framing
- exposes only `encryptFrame()` / `decryptFrame()` style methods to the rest of the app

That keeps the Noise state machine in one place and makes misuse much less likely.
