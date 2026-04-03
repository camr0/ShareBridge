# Slice 9b — HMAC Pre-challenge + Direct Links

## Goal

Prevent unauthenticated browser connections from causing the agent to create WebRTC peers (and expose the agent's public IP via ICE candidates) before auth is verified. Add direct-link support so recipients can click a URL instead of typing a session code.

---

## Problem Statement

### HMAC Pre-challenge

In the current flow, the signaling server sends a `join` to the agent the moment a browser WebSocket connects with a valid session code. The agent then creates a WebRTC peer connection immediately — which generates ICE candidates including the agent's STUN-reflexive (public IP) address — before any password check has occurred.

This has two consequences:

1. **IP leak**: Any browser that knows a session code (even without the password) can learn the agent's public IP address via ICE candidates forwarded through the signaling server.
2. **Resource exhaustion**: A malicious or compromised signaling server can flood the agent with `join` messages for valid codes, causing unbounded peer connection creation.

### Direct Links

Users currently must navigate to the signaling server's web UI and manually type a session code. There is no shareable URL that goes directly to a specific share. This creates friction for recipients.

---

## Design

### Part 1: HMAC Pre-challenge

#### Protocol

The browser must prove it knows the password **before** the agent creates a WebRTC peer. The signaling server remains blind to the password throughout.

```
Browser              Signaling Server                Agent
  |-- WS connect ------->|                             |
  |                       | pair conn (don't notify)   |
  |-- knock(conn_id) ----->|-- knock(conn_id) ---------->|
  |<-- nonce(conn_id) ----|<-- nonce(conn_id, value) ---|  agent stores {conn_id → nonce, expires}
  |                       |                             |
  | compute HMAC-SHA256(key=password, data=nonce)       |
  |-- join(conn_id, hmac) ->|-- join(conn_id, hmac) ---->|
  |                       |              verify HMAC    |
  |                       |              create peer    |
  |<-- offer -------------|<-- offer -------------------|
```

#### Message formats

```json
// Browser → Signaling → Agent
{"type": "knock", "conn_id": "uuid", "code": "ABC123"}

// Agent → Signaling → Browser
{"type": "nonce", "conn_id": "uuid", "value": "hex...", "has_password": true}

// Browser → Signaling → Agent
{"type": "join", "conn_id": "uuid", "code": "ABC123", "hmac": "hex..."}
```

#### conn_id

The signaling server generates a UUID per browser WebSocket connection on connect. This is included in all knock/nonce/join messages so the agent can correlate concurrent connection attempts to the same session code (multiple browsers may attempt to connect to the same code simultaneously).

#### Nonce lifecycle

- Agent generates a cryptographically random 32-byte hex nonce on receiving `knock`
- Stored in-memory: `map[connID]→{nonce, expiresAt}` protected by a mutex
- Expires after 60 seconds — cleaned up on each `knock` handler invocation (sweep expired entries inline, no background goroutine)
- **Atomic delete-and-verify**: on `join`, the agent deletes the nonce entry under the mutex before checking the HMAC. This prevents a race where two concurrent `join` messages for the same `conn_id` both read the nonce before either deletes it.

#### HMAC computation

```
HMAC-SHA256(key=password, data=nonce)
```

- Password is the UTF-8 encoded share password string
- Nonce is the hex string as received from the agent (the `value` field)
- Result is hex-encoded before transmission
- Password-less shares: `key=""` (empty string) — agent skips HMAC verification entirely and accepts any join that completed the knock flow

#### Auth failure handling

- Agent sends `auth_failed(conn_id)` to signaling server on bad HMAC
- Signaling server tracks failure count per `conn_id`
- After 3 failures, signaling server closes the browser WebSocket
- Signaling server never learns why auth failed — it just counts agent-reported failures

#### Signaling server responsibilities

The signaling server:
- Generates and attaches `conn_id` to each browser WS connection
- On browser WS connect: looks up session (code valid? agent connected?), pairs for routing — does **not** notify agent
- Routes `knock` → agent, `nonce` → browser, `join(hmac)` → agent blindly
- Tracks `auth_failed` count per `conn_id`, closes WS after 3 failures
- Existing `auth_failed` message type (currently used post-DataChannel) is reused here

#### Agent responsibilities

- On `knock`: sweep expired nonces, generate nonce, store with 60s TTL, send `nonce` back
- On `join`: **atomically delete** nonce entry under mutex, then verify `HMAC-SHA256(key=storedPassword, data=nonce)`
  - Pass: create WebRTC peer, proceed with offer
  - Fail: send `auth_failed` to signaling server, do nothing else
- Password-less shares: skip HMAC verification, proceed directly to peer creation

#### Deployment

This is a **coordinated deploy** — signaling server and agent must be updated together. No backward compatibility with old agents receiving `knock`, or old browsers connecting to an updated signaling server. The protocol version field in the `hello` message (added in Slice 8) handles this: agent advertises its version on connect; signaling server can reject mismatched agents with a clear error.

---

### Part 2: Direct Links

#### URL format

```
/s/ABC123             — code only (recipient types password if required)
/s/ABC123#mypassword  — magic link (auto-submits, no typing required)
```

Passwords containing special characters must be percent-encoded when constructing the URL (e.g. via `encodeURIComponent`). The browser reads the hash with `decodeURIComponent` before use.

#### Signaling server route

Add `GET /s/:code` route that serves the existing `index.html`. No server-side logic — the page loads identically regardless of code or hash.

#### Browser JS behaviour on load

1. Read code from `window.location.pathname` (e.g. `/s/ABC123` → `ABC123`)
2. If code found: pre-fill the session code input field
3. If `window.location.hash` is non-empty: extract and `decodeURIComponent`, immediately clear from URL bar via `history.replaceState(null, '', window.location.pathname)`, store in memory, auto-submit
4. If hash is empty and code is pre-filled: knock to get nonce, check `has_password` in nonce response — if false, auto-submit with `HMAC(key="", data=nonce)`; if true, show password input field and wait for user

#### `nonce` response includes `has_password`

The agent includes `has_password: bool` in the `nonce` response. The browser uses this to decide whether to show the password field or auto-submit immediately.

The signaling server learns whether a session has a password — this is low-sensitivity metadata (not the password), but it does enable session enumeration: an attacker could probe `/s/:code` values to discover which codes exist and whether they have passwords. This is acceptable for now; rate limiting on the knock endpoint (Slice 12) mitigates enumeration at scale.

#### Password in URL — security note

The URL hash (`#`) is never sent to the server. It lives only in the browser. Risks: browser history, screenshots. This is the same pattern used by Bitwarden Send and accepted as a reasonable tradeoff for magic-link UX. Use is opt-in — users who share code and password separately are unaffected.

---

## Future Consideration

Password-less shares exist primarily for convenience ("anyone with the code can access"). With magic links, a password-protected share + magic link (`/s/ABC123#password`) provides identical recipient UX while keeping the signaling server blind to the content. Consider deprecating passwordless in a future slice in favour of auto-generated passwords always embedded in magic links.

---

## Files Changed

### Signaling Server

| File | Change |
|------|--------|
| `signaling-server/internal/handler/browser_ws.go` | Generate `conn_id` on connect; delay agent notification; route knock/nonce/join; track auth failures per conn_id |
| `signaling-server/internal/hub/hub.go` | Store `conn_id` in session pair; add routes for knock/nonce forwarding |
| `signaling-server/internal/handler/routes.go` | Add `GET /s/:code` route serving index.html |
| `signaling-server/web/app.js` | Read code from URL path; read+clear password from hash; `encodeURIComponent`/`decodeURIComponent` for hash; auto-submit logic |

### Agent

| File | Change |
|------|--------|
| `agent/internal/signaling/client.go` | Handle `knock` message: generate nonce, store, send back; handle `join` with HMAC: atomic delete-then-verify before peer creation |
| `agent/internal/daemon/daemon.go` | Pass password to signaling client for HMAC verification; mutex-protected nonce store |

---

## What Does Not Change

- WebRTC peer creation, ICE negotiation, DataChannel — unchanged
- Post-connection file transfer protocol — unchanged
- API key authentication for agents — unchanged

## Breaking Changes to Existing Protocol

| Field | Location | Change |
|-------|----------|--------|
| `password` | `list_request` DataChannel message (browser→agent) | **Removed** — auth is proven via HMAC before WebRTC; re-checking on `list_request` is redundant |
| `password_required` | `hello` DataChannel message (agent→browser) | **Removed** — browser now learns `has_password` from the `nonce` response before WebRTC starts |

> **Context:** Password was previously piggybacked on `list_request` as a dual-purpose auth+navigation message. A comment in `agent/internal/transfer/manager.go` already flagged this as a known wart deferred from Slice 3. This slice resolves it.

**`transfer/Manager` changes:** The `authenticated` atomic bool and `authFailures` counter are removed entirely — `authenticated` was only used for the DataChannel gate and its associated test (`TestHandleFileRequest_UnauthenticatedBlocked`), neither of which exist in other code paths (no logging or metrics). The gate `if msg.Type != "list_request" && !authenticated` is removed. All peers start fully authenticated since HMAC already proved auth before peer creation.

**Side effect:** With the `authenticated` gate gone, a browser could send `file_request` before `list_request`. This is not a security issue — the browser is already proven — but it is a protocol assumption change. An invalid path on `file_request` already returns an error gracefully, so no special handling is needed.

---

## Out of Scope

- Rate limiting by IP (separate hardening concern, Slice 12)
- HMAC for agent→server communication (agent already authenticates via API key)
- Custom session codes
