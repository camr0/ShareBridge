# Slice 13 — libp2p Transport Overhaul

**Date:** 2026-04-16
**Status:** Approved for implementation

## Problem

WebRTC TURN relay throughput is capped by the SCTP receive window (browser-dependent: Chrome 256 KiB, Firefox 1 MiB, pion default 1 MiB but agent-side is untuned). Throughput = window / RTT, so at 50ms latency the Chrome ceiling is ~5 MB/s regardless of available bandwidth. The fix requires replacing the relay transport with something that uses TCP window scaling.

---

## Goals

- Replace the WebRTC + coturn stack with libp2p as the underlying transport
- Fix relay throughput (TCP window scaling eliminates the SCTP ceiling)
- Gain E2E encryption via Noise protocol (battle-tested, same as WireGuard/Signal) — nothing to implement
- Gain stream multiplexing via yamux on the relay path (parallel downloads in a future slice nearly free on relay — direct mode still shares one SCTP window across streams, so parallel benefit there would require multiple PeerConnections; see Future)
- Establish a clear upgrade path to WebTransport/QUIC via a dependency bump when Safari adoption is solid
- Change nothing above the transport layer (file protocol, auth, quota, UI, extensions)

---

## Approach

Three-phase clean cutover: 13a (relay server), 13b (agent), 13c (browser). No backward compatibility with WebRTC — service is not public, clean break is correct. Each phase has a testable end state before the next begins. All three phases ship as a single release when 13c is complete.

---

## Architecture Overview

```
Current:
  Browser <—WebRTC DataChannel (TURN relay or direct ICE)—> Agent
  Signaling server: SDP/ICE forwarding + coturn HMAC credentials

New:
  Browser <—libp2p stream (Noise encrypted)—> Agent
           (circuit relay or DCUtR direct)
  Signaling server: token issuance + relay multiaddr distribution
  Relay: custom validating libp2p Host embedded in signaling server binary
```

The relay is a go-libp2p Host running as goroutines inside the existing signaling server binary. It is **not** stock circuit relay v2 — it implements a custom protocol `/sharebridge/relay/1.0.0` that validates a signed JWT as the first step of every circuit establishment. No valid token → stream closed, no circuit, no bytes forwarded.

The signaling server (PocketBase HTTP on `:8080`) and relay (libp2p Host on `127.0.0.1:9001`) are the same OS process, communicating via in-process function calls and shared memory. No IPC overhead.

coturn is removed entirely. STUN is still needed for DCUtR ICE gathering — use `stun.cloudflare.com:3478`.

---

## Deployment Changes

### Relay endpoint

- Relay binds to `127.0.0.1:9001` (loopback only — unreachable from internet directly)
- Firewall blocks port 9001 from external access (belt and suspenders)
- New subdomain: `relay.sharebridge.app`
  - Cloudflare DNS-only (grey cloud) — file transfer data must not flow through Cloudflare proxy (ToS violation for large transfers; same situation as coturn today)
  - Caddy handles TLS termination for `relay.sharebridge.app` (Let's Encrypt, same as today)
  - Caddy proxies `relay.sharebridge.app → localhost:9001`
  - **The Caddy route for `relay.sharebridge.app` must NOT include the Cloudflare IP allowlist** that protects the main domain routes — connections arrive from real client IPs (browsers and agents worldwide). This is a manual Caddy config change required before 13a goes live.

### Keepalive

The existing 30s keepalive ping in `signaling/client.go` (for the signaling server WebSocket) stays — `sharebridge.app` is behind Cloudflare which has a 100s idle WebSocket timeout. The relay connection (`relay.sharebridge.app`) bypasses Cloudflare (DNS-only), so no keepalive is needed there — Caddy's default WebSocket idle timeout is 30 minutes.

### coturn

Decommissioned as part of 13a.

---

## Components

### What changes

**Signaling server (`signaling-server/`)**

- **New: `internal/relay/`** — go-libp2p Host, custom `/sharebridge/relay/1.0.0` protocol handler, JWT validation, in-memory JTI store (5min TTL, cleared on expiry), byte counting per circuit for quota accounting, DCUtR coordination, `relay_allowed` / `dcutr_allowed` enforcement
- **Changed: `handler/browser_ws.go`** — after share code validation (and HMAC pre-challenge for password shares), issue a signed JWT; send `{ relay_multiaddr, jwt }` to browser instead of ICE config; remove SDP/ICE forwarding; keep knock/nonce/join/auth_failed flow unchanged
- **Changed: `handler/agent_ws.go`** — add relay multiaddr to welcome message; remove `offer` and `ice_candidate` forwarding (those move to libp2p streams); keep: `hello`, `register_share`, `download_complete`, `auth_failed`
- **Removed: `internal/turn/`** — HMAC credential generation and ICE config building gone entirely

**Agent (`agent/`)**

- **Removed: `internal/peer/`** — WebRTC PeerConnection and DataChannel gone
- **New: `internal/transport/`** — go-libp2p Host, outbound-only connection to relay (`wss://relay.sharebridge.app`), stream handler that wires into `transfer.Manager` via the existing `DataChannel` interface (thin adapter)
- **Changed: `internal/signaling/client.go`** — remove ICE server handling; receive relay multiaddr on welcome message
- **Unchanged: `internal/transfer/manager.go`** — zero logic changes; gets a libp2p stream adapter that satisfies the existing `DataChannel` interface

**Browser (`signaling-server/web/app.js`)**

- **Removed:** `RTCPeerConnection`, `RTCDataChannel`, ICE candidate handling, SDP exchange, ICE config handling
- **New:** js-libp2p node with `@libp2p/websockets` (relay path) and `@libp2p/webrtc` (DCUtR direct path); Noise encryption automatic
- **Changed:** connection flow — receive `{ relay_multiaddr, jwt }` from signaling WS, connect via js-libp2p; attempt DCUtR upgrade unless `relay_only`
- **Changed:** connection type indicator — replace ICE candidate type inspection with explicit libp2p connection type tracking (relay vs direct)

### What does not change

Share codes, HMAC pre-challenge flow (knock/nonce/join/auth_failed), quota tracking concept, download count tracking, file chunking protocol, backpressure, binary message format, bandwidth accounting, admin UI, OpenCloud extension, Nextcloud extension, PocketBase schema (except dropping unused TURN-related fields).

---

## Connection Protocol

### Relay path

```
1. Agent starts up (private server)
   → connects outbound to wss://relay.sharebridge.app (Cloudflare DNS-only → Caddy → localhost:9001)
   → connects with API key in query param (same as signaling WS: ?api_key=...)
   → relay validates API key against PocketBase (in-process call), registers agent peer ID → account ID mapping

2. Browser navigates to share link
   → WS connect to signaling server (sharebridge.app, behind Cloudflare)
   → sends knock (share code)
   → [password share only] signaling server forwards knock to agent
     → agent sends nonce → browser sends join(HMAC) → agent validates
     → agent signals OK to signaling server
   → signaling server issues JWT:
       { jti, share_code, browser_peer_id, relay_allowed: true, dcutr_allowed: true/false, exp: now+5min }
       signed with server secret
   → signaling server sends browser: { relay_multiaddr, jwt }

3. Browser → relay (wss://relay.sharebridge.app, token=JWT)
   → Noise handshake (key exchange only)
   → browser opens /sharebridge/relay/1.0.0 stream, sends JWT
   → relay validates: signed by us? not expired? jti not used before?
     → valid: jti recorded in-memory, circuit established to agent, Noise runs browser↔agent
     → invalid: stream closed, connection logged, agent sees nothing

4. File transfer over libp2p stream (same binary protocol as today)
   → relay counts bytes for quota accounting
```

### Direct path (DCUtR upgrade)

```
Steps 1–3 same as relay path.

4. DCUtR runs through the established relay circuit
   → browser and agent exchange hole-punch coordination messages via relay
   → if hole punch succeeds → direct WebRTC connection established
   → relay circuit torn down, relay stops counting bytes, direct is quota-free

relay_only=true (dcutr_allowed=false in JWT):
   → relay refuses to forward DCUtR messages → step 4 skipped → traffic stays on relay
   → agent IP never revealed to browser (agent connects outbound only, browser sees relay IP)

Quota exhausted (direct-capable share):
   → signaling server issues JWT with relay_allowed=false, dcutr_allowed=true
   → relay allows DCUtR coordination only, refuses to forward data bytes
   → if hole punch succeeds → direct connection works fine, no relay bandwidth consumed
   → if hole punch fails → connection fails, no relay fallback
   → consistent with Slice 10c: direct mode is always free

Quota exhausted + relay_only=true (complete block):
   → both relay_allowed and dcutr_allowed would be false — no JWT issued
   → signaling server rejects immediately with error: "file host's relay quota exceeded - this share requires relay which is unavailable"
   → same behaviour as current code in browser_ws.go
```

---

## JWT Structure

```json
{
  "jti": "<uuid>",
  "share_code": "abc12345",
  "browser_peer_id": "<libp2p peer ID>",
  "relay_allowed": true,
  "dcutr_allowed": true,
  "exp": 1234567890
}
```

- Signed with signaling server secret (HS256 or similar)
- 5-minute TTL
- Single-use: `jti` recorded in-memory on first use; second use rejected within TTL window
- In-memory JTI store is sufficient: a restart clears it, but the 5-min TTL means the replay window is at most 5 minutes post-restart — acceptable given the attacker would also need to intercept a valid token

---

## Security Model

| Threat | Mitigation |
|---|---|
| Past user with cached relay multiaddr dials agent | JWT required for every circuit — no token, stream closed |
| Attacker forges JWT | JWT signed with server secret — any field modification invalidates signature |
| Replayed valid JWT | `jti` recorded in-memory on first use — second use rejected within TTL |
| Wrong password on password share | HMAC pre-challenge before JWT issued — 3 failures = signaling server closes browser WS |
| Quota exhaustion breaking direct connections | `relay_allowed:false` JWT still allows DCUtR coordination — direct works, relay data refused |
| `relay_only` bypass (browser attempts DCUtR anyway) | `dcutr_allowed:false` in JWT — relay refuses DCUtR messages |
| Arbitrary libp2p peers discovering/dialing agent | DHT + mDNS disabled — agent only knows relay (outbound) and token-validated browsers |
| Agent IP exposure in relay mode | Agent connects outbound only — browser sees relay IP, never agent IP |
| Direct access to relay bypassing Caddy | Relay binds to 127.0.0.1 + firewall blocks :9001 externally |
| Using relay for free bandwidth | Custom protocol requires JWT before any circuit — no token, no forwarded bytes |

---

## Phase Breakdown

### 13a — Relay + Token Issuance

Deliverable: working libp2p relay embedded in signaling server, JWT issuance and validation, coturn removed. Testable with standalone go-libp2p test clients — no agent or browser changes needed.

- New `internal/relay/` package
- Update `handler/browser_ws.go` (JWT issuance, remove ICE config)
- Update `handler/agent_ws.go` (relay multiaddr in welcome, remove SDP/ICE forwarding)
- Remove `internal/turn/`
- Caddy config: add `relay.sharebridge.app` route (manual — remove Cloudflare IP restriction)
- Firewall: block port 9001
- coturn decommissioned

### 13b — Agent Transport Replacement

Deliverable: agent connects to relay via go-libp2p, serves files over libp2p streams. Testable end-to-end with a go-libp2p test client in place of a real browser.

- Remove `internal/peer/`
- New `internal/transport/` package
- Update `internal/signaling/client.go`
- Thin `DataChannel` interface adapter over libp2p stream (no changes to `transfer/manager.go`)
- Remove `github.com/pion/webrtc/v4` dependency

### 13c — Browser Transport Replacement

Deliverable: full browser→relay→agent file transfer over libp2p. DCUtR direct upgrade working. Ready to ship.

- Replace `RTCPeerConnection`/DataChannel in `app.js` with js-libp2p
- Add `@libp2p/websockets`, `@libp2p/webrtc`, Noise
- Update connection type indicator
- End-to-end relay and direct connection testing

---

## Future

When WebTransport has broad browser support (Safari 18.4 just landed it but rollout takes time, and server-side QUIC setup adds complexity to 13a), configure the relay to also listen on WebTransport and js-libp2p will automatically prefer it. Essentially a dependency bump and Caddy config addition — no protocol changes.

yamux stream multiplexing is included automatically with go-libp2p / js-libp2p. Parallel downloads (Slice 15) become a protocol-level feature with no transport work required **on the relay path**. On the direct path (DCUtR → WebRTC), all yamux streams share a single SCTP association and thus one receive window — parallel streams won't multiply throughput. Addressing direct-mode throughput is tracked separately: (1) agent-side pion SCTP buffer tuning via `SettingEngine.SetSCTPMaxReceiveBufferSize` (quick win, verify go-libp2p passthrough), then (2) multiple independent PeerConnections with JWT `max_uses` if tuning is insufficient. See `memory/future_parallel_webrtc_channels.md`.
