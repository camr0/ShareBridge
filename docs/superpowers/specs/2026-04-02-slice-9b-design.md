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

#### conn_id

The signaling server generates a UUID per browser WebSocket connection on connect. This is included in all knock/nonce/join messages so the agent can correlate concurrent connection attempts to the same session code (multiple browsers may attempt to connect to the same code simultaneously).

#### Nonce lifecycle

- Agent generates a cryptographically random 32-byte hex nonce on receiving `knock`
- Stored in-memory: `map[connID]→{nonce, expiresAt}`
- Expires after 60 seconds — stale entries cleaned up periodically
- Consumed (deleted) on first use, whether HMAC passes or fails

#### HMAC computation

```
HMAC-SHA256(key=password, data=nonce)
```

- Password is the UTF-8 encoded share password string
- Nonce is the hex string as received from the agent
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

- On `knock`: generate nonce, store with 60s TTL, send `nonce` back
- On `join`: look up stored nonce by `conn_id`, verify `HMAC-SHA256(key=storedPassword, data=nonce)`
  - Pass: delete nonce, create WebRTC peer, proceed with offer
  - Fail: delete nonce, send `auth_failed` to signaling server, do nothing else
- Password-less shares: skip HMAC verification, proceed directly to peer creation

---

### Part 2: Direct Links

#### URL format

```
/s/ABC123             — code only (recipient types password if required)
/s/ABC123#mypassword  — magic link (auto-submits, no typing required)
```

#### Signaling server route

Add `GET /s/:code` route that serves the existing `index.html`. No server-side logic — the page loads identically regardless of code or hash.

#### Browser JS behaviour on load

1. Read code from `window.location.pathname` (e.g. `/s/ABC123` → `ABC123`)
2. If code found: pre-fill the session code input field
3. If `window.location.hash` is non-empty: extract as password, immediately clear from URL bar via `history.replaceState(null, '', window.location.pathname)`, store in memory, auto-submit
4. If hash is empty and code is pre-filled: knock to get nonce, check `has_password` in nonce response — if false, auto-submit; if true, show password input field and wait for user

#### `nonce` response includes `has_password`

The agent includes `has_password: bool` in the `nonce` response. The browser uses this to decide whether to show the password field or auto-submit immediately. The signaling server learns whether a session has a password — acceptable, it is low-sensitivity metadata, not the password itself.

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
| `signaling-server/web/app.js` | Read code from URL path; read+clear password from hash; auto-submit logic |

### Agent

| File | Change |
|------|--------|
| `agent/internal/signaling/client.go` | Handle `knock` message: generate nonce, store, send back; handle `join` with HMAC: verify before peer creation |
| `agent/internal/daemon/daemon.go` | Pass password to signaling client for HMAC verification; nonce store + cleanup goroutine |

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

**`transfer/Manager` changes:** The `authenticated` atomic bool initialises as `true` for all peers (HMAC already proved auth before peer creation). The `authFailures` counter and DataChannel-level lockout are removed — the 3-strike limit now lives on the signaling server counting `auth_failed` messages. The gate `if msg.Type != "list_request" && !authenticated` is removed entirely.

**Side effect:** With the `authenticated` gate gone, a browser could send `file_request` before `list_request`. This is not a security issue — the browser is already proven — but it is a protocol assumption change. An invalid path on `file_request` already returns an error gracefully, so no special handling is needed.

---

## Out of Scope

- Rate limiting by IP (separate hardening concern, Slice 12)
- HMAC for agent→server communication (agent already authenticates via API key)
- Custom session codes
