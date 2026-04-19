# Secure Relay + Transfer Channel Design

**Date:** 2026-04-18  
**Status:** Revised after review

## Problem

ShareBridge now has a standalone `noise-p256` library for authenticated E2E relay encryption, but the application still needs a clean system boundary above it.

Today, direct mode and relay mode are too transport-specific:

- direct mode is effectively shaped around WebRTC DataChannel behavior
- relay mode needs post-auth encrypted framing on top of a byte stream
- fallback behavior used to be mostly hidden inside WebRTC/TURN
- quota enforcement still assumes TURN/Prometheus-era plumbing

If we integrate relay encryption ad hoc, transport details will leak into the browser app and transfer layer. That would make fallback, quota enforcement, and E2E verification harder to reason about.

## Goals

- Introduce a post-auth `secure-relay` boundary for the relay path
- Keep the existing file transfer protocol unchanged above the transport boundary
- Preserve direct mode behavior from the transfer layer's point of view
- Hide direct-vs-relay selection behind one browser-side orchestration boundary
- Make relay policy server-authoritative and tamper-evident
- Replace TURN/Prometheus-era relay quota accounting with relay-native accounting
- Remove Coturn from the new architecture
- Specify a concrete relay authorization and routing protocol before implementation planning

## Non-Goals

- Rewriting the pre-auth `knock` / `nonce` / `join(hmac)` flow into `secure-relay`
- Changing the existing file transfer message shapes
- Supporting mid-transfer migration between direct and relay in v1
- Making the Go relay/agent package a public reusable external Go dependency

## High-Level Architecture

The system is split into five layers:

1. **Signaling/Auth Layer**
   - `knock`
   - `nonce`
   - `join(hmac)`
   - authorization outcome
   - signed connection policy issuance
   - relay session creation and agent-side relay pre-registration

2. **Connection Orchestration Layer**
   - browser-side `connectTransferChannel(...)`
   - decides direct vs relay within server-issued policy
   - handles fallback before transfer begins
   - updates connection status indicators

3. **Transport Adapter Layer**
   - `DirectChannel`
   - `SecureRelayChannel`

4. **Shared Transfer Protocol Layer**
   - `hello`
   - `list_request`
   - `file_request`
   - `file_list`
   - `file_header`
   - binary chunk messages
   - `chunk_end`
   - `error`

5. **Transfer/Application Logic**
   - browser UI
   - transfer manager
   - progress/completion logic

Flow:

```text
signaling/auth
    ->
connectTransferChannel(...)
    ->
[ DirectChannel | SecureRelayChannel ]
    ->
shared transfer protocol
    ->
transfer manager / browser app
```

## Module Boundaries

### `noise-p256`

Already implemented. This remains a pure crypto primitive:

- Noise XX handshake
- split cipher states
- no transport policy
- no app framing
- no session authorization logic

### `secure-relay`

`secure-relay` owns the entire **post-auth encrypted relay session**:

- Noise handshake orchestration
- expected agent static public key verification
- internal relay stream framing
- encrypted message send/receive
- fail-closed behavior on decrypt/protocol errors

It does **not** own:

- public signaling websocket flow
- `knock` / `nonce` / `join(hmac)`
- share lookup and authorization policy

### `connectTransferChannel(...)`

Browser-side orchestration boundary:

- consumes server-issued signed policy
- decides whether to attempt direct first or relay immediately
- handles direct-to-relay fallback
- returns exactly one connected transport adapter
- updates relay/direct connection status

This may begin life as a function plus helpers. Conceptually it is a small connection manager, but v1 does not require a heavy class abstraction.

## Ownership Rules

- **Signaling server owns authorization and policy**
- **Browser owns transport execution and fallback within policy**
- **Agent exposes transport endpoints and accepts whichever authorized path is established**
- **Relay backend owns authoritative relay byte measurement**

The browser is not trusted to define what paths are allowed. It only executes connection logic within server-issued constraints.

## Shared Channel Contract

The transport-facing contract to the transfer layer stays aligned with the existing direct-mode design:

- `SendText(text)` for JSON control messages
- `SendBinary(bytes)` for raw file chunks
- `BufferedAmount()` for backpressure
- `Close()`
- inbound delivery that preserves message boundaries
  - text arrives as text
  - binary arrives as binary

This preserves the existing transfer protocol and minimizes changes to the transfer manager and browser app logic.

### Why not `sendFrame()`?

The system could have been raised to a generic `sendFrame/readFrame` abstraction, but that would add migration churn without clear benefit right now. The existing text/binary split is simple, proven, and maps cleanly to both direct and relay adapters.

## Direct Channel

`DirectChannel` is the compatibility adapter over the existing direct transport:

- browser side: WebRTC DataChannel / DTLS
- agent side: existing direct transport implementation

Direct mode remains functionally unchanged from the transfer layer's point of view.

Direct mode still uses STUN for candidate gathering. In the new architecture:

- **direct mode uses STUN only**
- **TURN fallback is removed**
- **relay fallback is handled explicitly by orchestration, not automatically by WebRTC**

Cloudflare STUN is the intended default:

- `stun.cloudflare.com:3478`

## Secure Relay Channel

`SecureRelayChannel` is the relay adapter:

- runs only after signaling/auth succeeds
- uses `noise-p256` for the post-auth relay session
- verifies the responder static public key against the server-issued expected value
- exposes the same text/binary channel contract upward
- owns session phase handling across handshake and post-handshake traffic

It is implemented in:

- browser JS
- agent Go

The relay itself remains transport-forwarding infrastructure. It does not participate in the Noise session beyond enforcing relay authorization and forwarding ciphertext.

## Relay Transport and Routing Protocol

The relay backend is a signaling-server-side WebSocket service. Relay traffic does not continue to flow through the public signaling message channel after auth succeeds.

Transport:

- browser ↔ relay backend: dedicated `wss` relay endpoint
- agent ↔ relay backend: dedicated `wss` relay endpoint
- relay backend forwards opaque framed bytes between paired browser and agent sockets for one authorized session

The relay backend never decrypts Noise payloads. Its responsibilities are:

- verify relay authorization
- pair the correct browser and agent sockets for a session
- forward opaque framed bytes
- measure relay bytes for quota accounting

### Relay Session ID

Every post-auth secure relay attempt is bound to a server-issued opaque session ID:

- `sid`

`sid` binds together:

- the authorized share/session context
- the intended agent
- the browser relay authorization
- the agent-side relay pre-registration

The relay backend routes strictly by `sid`. No bytes are forwarded until both sides have attached to the same valid session.

### Agent Pre-Registration

After signaling/auth succeeds and before relay fallback can be used, the signaling server creates an in-memory relay session record and notifies the authenticated agent over the existing signaling channel.

That notification contains at minimum:

- `sid`
- share/session identifier
- session expiry

The agent then opens or attaches an authenticated relay socket for that `sid`.

Concrete agent-side auth:

- signaling server issues a short-lived server-signed agent relay token for `sid`
- agent presents that token as the first application message on its relay WebSocket
- relay backend verifies that token before binding the agent socket to `sid`

This means the relay backend never guesses which agent should receive a browser relay session. The server creates the binding up front, and the agent explicitly registers its side of the relay session before forwarding begins.

### Browser Relay Authorization

The browser receives a signed connection policy containing `sid` and relay permissions. If the browser needs relay, it opens the relay WebSocket and sends that signed policy as the first application message.

The relay backend verifies all of the following before forwarding any bytes:

- policy signature valid
- policy unexpired
- `relayAllowed = true`
- `sid` exists and is still pending
- token replay check passes
- the session is bound to a live authenticated agent-side relay socket

Only then does the relay backend mark the session active and start forwarding framed bytes between browser and agent.

## Relay Framing

WebRTC DataChannels already preserve message boundaries. Relay byte streams do not, so `SecureRelayChannel` must recreate them with internal framing.

Relay frame format:

- 1 byte frame kind
  - `0x00` = Noise handshake
  - `0x01` = text
  - `0x02` = binary
- 4 bytes big-endian payload length
- payload bytes

This is **internal transport framing**, not part of the file transfer protocol itself.

Handshake rule:

- during the pre-split phase, only `0x00` handshake frames are valid
- after successful Noise split, only `0x01` text and `0x02` binary frames are valid

Unknown frame kinds are fatal and close the relay session immediately.

### Frame Limits

- The 4-byte length field has a theoretical maximum of `2^32 - 1` bytes.
- The actual allowed maximum frame payload in ShareBridge is capped much lower.

Current hard limit:

- **8 MiB max frame payload**

This is more than sufficient for:

- JSON control messages
- current 64 KiB binary chunks
- modest future growth

Malformed, oversized, out-of-phase, or unknown-kind relay frames are fatal and close the relay session immediately.

## Signed Connection Policy

After signaling/auth succeeds, the signaling server issues a **signed connection policy**. This policy is authoritative and tamper-evident.

Concrete mechanism:

- format: JWT
- signature algorithm: **HMAC-SHA256 (HS256)**
- signing key: signaling server secret, stored server-side only

Who verifies it:

- browser: reads the policy fields and is constrained by them, but does not rely on local verification for trust
- relay backend: verifies the HS256 signature before allowing relay use

TLS protects the policy in transit. The JWT signature protects it from client-side mutation after receipt.

It carries:

- `sid`
- session/share identifier
- expiry
- `relayAllowed`
- `relayOnly`
- expected agent static public key
- `jti` anti-replay identifier

### Why this is separate from Noise

Noise provides:

- authenticated key exchange
- transcript integrity
- fresh session keys

Noise does **not** by itself tell the browser or relay:

- which agent static public key it was supposed to see
- whether relay is allowed for this session
- whether the authorization is expired
- whether a relay authorization blob is being replayed
- which relay session ID is authorized for routing

So the system uses both:

- **Noise** to authenticate the encrypted channel
- **signed policy** to authenticate the authorized session context and transport permissions

### Anti-Replay

Anti-replay is required in v1.

The signed policy includes:

- `jti` — single-use unique token identifier

Relay backend rule:

- first successful relay authorization for a given `jti` marks it spent
- any later reuse of the same `jti` is rejected

Expiry alone is not sufficient, because an intercepted policy blob would otherwise be replayable for the remainder of its validity window.

### Policy Shape

Because direct mode is always free and not quota-restricted in the current ShareBridge model, the simplest policy is:

- `relayAllowed`
- `relayOnly`

Derived behavior:

- `relayOnly = false`, `relayAllowed = true`  
  try direct first, fallback to relay

- `relayOnly = false`, `relayAllowed = false`  
  direct only

- `relayOnly = true`, `relayAllowed = true`  
  relay only

- `relayOnly = true`, `relayAllowed = false`  
  invalid, reject before connection setup

### Agent-Side Browser Authentication Model

The browser does not have a long-lived pinned identity in ShareBridge.

So the agent-side acceptance rule is not “pin a browser public key.” Instead:

- the relay backend verifies that the browser presented a valid, single-use server-issued policy for `sid`
- the relay backend pairs that browser with the intended authenticated agent-side relay socket for the same `sid`
- the agent then completes the Noise XX handshake with that initiator

The browser’s Noise static key remains per-session ephemeral. The agent authenticates the **authorized session**, not a reusable browser identity.

## Connection Flow

1. Signaling/auth completes as it does today.
2. Signaling server returns a signed connection policy.
3. Browser calls `connectTransferChannel(policy, ...)`.
4. Browser executes:
   - if `relayOnly`, connect relay immediately
   - otherwise try direct first
   - if direct succeeds, return `DirectChannel`
   - if direct fails and `relayAllowed`, connect `SecureRelayChannel`
   - otherwise fail
5. Transfer layer receives one connected channel and runs unchanged.

### Relay Path Detailed Flow

1. Signaling/auth completes.
2. Signaling server creates an in-memory relay session for `sid`.
3. Signaling server notifies the authenticated agent to pre-register relay for `sid`.
4. Signaling server returns a signed policy JWT to the browser containing:
   - `sid`
   - `relayAllowed`
   - `relayOnly`
   - expected agent static public key
   - `exp`
   - `jti`
5. Browser opens the relay WebSocket and sends the policy JWT as the first application message.
6. Relay backend verifies:
   - HS256 signature
   - `exp`
   - `relayAllowed`
   - `jti` unused
   - `sid` exists
   - matching authenticated agent relay socket is present for `sid`
7. Relay backend marks `jti` spent and begins opaque byte forwarding.
8. Browser and agent run the Noise XX handshake through the relay.
9. Browser verifies the agent static public key from the Noise handshake matches the expected key from the signed policy.
10. After handshake success, both sides switch to text/binary framed transfer traffic.

### First-Version Rules

- Transport is selected **before** transfer starts.
- There is **no mid-transfer migration** between direct and relay in v1.
- Fallback happens before the transfer layer sees a connected session.
- If both paths fail, the browser surfaces a connection error and transfer never starts.
- Direct attempt timeout is explicit and bounded.

### Direct Attempt Timeout

Direct mode is attempted with a caller-configurable timeout.

Default v1 value:

- **5 seconds**

This keeps fallback latency bounded while still allowing a reasonable ICE attempt window. If the direct path is not connected by then, orchestration treats that attempt as failed and proceeds according to policy.

## Connection Status Indicator

`connectTransferChannel(...)` also owns user-facing connection status updates. Expected states include:

- connecting direct
- falling back to relay
- connected direct
- connected relay
- failed

This keeps status logic close to the actual connection orchestration rather than scattering it through the UI.

### `connectTransferChannel(...)` Sketch

The exact implementation can stay lightweight, but the spec anchors it with a rough signature:

```ts
async function connectTransferChannel({
  policy,
  iceServers,
  directTimeoutMs = 5000,
  onStatusChange,
}): Promise<DirectChannel | SecureRelayChannel>
```

Where:

- `policy` is the server-issued signed connection policy
- `iceServers` configures direct mode
- `directTimeoutMs` bounds the direct attempt window
- `onStatusChange` receives states such as `connecting-direct`, `falling-back-to-relay`, `connected-direct`, `connected-relay`, `failed`

## Quota Accounting

TURN/Prometheus-era quota accounting is removed from this design.

New model:

- quota applies only to relay traffic
- direct traffic remains free/unmetered
- relay backend measures actual relay bytes server-side
- signaling/policy layer enforces quota by controlling `relayAllowed`

### Authoritative Measurement

Authoritative relay byte accounting happens on the signaling-server-side relay/backend. The browser is not trusted to report usage.

That means:

- no browser-side “usage declarations”
- no client-controlled quota accounting
- no funny business around understated relay usage

### Enforcement Split

- **Relay backend owns byte measurement**
- **Signaling/policy layer owns quota enforcement decisions**
- **Browser consumes the resulting signed policy**

## Infrastructure Changes

This design explicitly includes the following cleanup:

- remove Coturn from the new direct/relay architecture
- remove TURN-specific Prometheus quota plumbing
- replace it with relay-native byte accounting
- use Cloudflare STUN for direct ICE gathering

Result:

- **direct mode** = WebRTC/DTLS + STUN only
- **relay mode** = post-auth `SecureRelayChannel` + Noise

## Agent and Browser Responsibilities

### Browser

- receives signed connection policy
- runs `connectTransferChannel(...)`
- executes direct attempt / relay fallback
- validates expected agent static public key in relay mode
- exposes a connected channel to the transfer layer

### Agent

- keeps pre-auth signaling/HMAC flow unchanged
- accepts direct or relay path depending on what is successfully established
- exposes a channel-compatible transport endpoint upward
- participates in Noise handshake only for relay mode

## Error Handling

Relay mode is fail-closed:

- Noise auth failure closes the session
- decrypt failure closes the session
- malformed relay frame closes the session
- oversized frame closes the session
- protocol ordering violation closes the session

Direct-mode behavior remains whatever the direct transport currently guarantees.

The transfer layer should treat transport loss as fatal for that session.

## Rollout Plan

1. Introduce `DirectChannel` and `SecureRelayChannel` behind the shared contract.
2. Keep direct mode behavior unchanged.
3. Route relay mode through `SecureRelayChannel`.
4. Route browser connection setup through `connectTransferChannel(...)`.
5. Keep transfer logic transport-agnostic.

This is intentionally low-risk:

- direct path remains stable
- relay path gets hardened
- old legacy encrypted relay-channel logic collapses into one post-auth relay package instead of leaking into app code

## Testing Strategy

The spec expects:

- browser unit tests for `connectTransferChannel(...)` policy/fallback behavior
- JS `SecureRelayChannel` tests for frame encoding/decoding, corruption handling, and close-on-failure behavior
- Go `SecureRelayChannel` tests for the same
- end-to-end tests proving the exact same transfer protocol works over:
  - `DirectChannel`
  - `SecureRelayChannel`

## Summary

This design keeps the existing file transfer protocol and direct-mode behavior stable while introducing a hardened post-auth relay session boundary.

The key decisions are:

- `noise-p256` stays a pure crypto primitive
- `secure-relay` owns the entire post-auth encrypted relay session
- browser-side `connectTransferChannel(...)` owns fallback execution
- signed server policy constrains transport choices
- relay-native server-side accounting replaces TURN/Prometheus-era quota tracking
