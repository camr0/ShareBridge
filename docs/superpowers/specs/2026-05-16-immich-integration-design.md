# Immich Integration — Design Spec

**Slice 14** (moved up from Slice 17, 2026-05-16)
**Status:** Designing
**Inspiration:** Immich Public Proxy ([alangrainger/immich-public-proxy](https://github.com/alangrainger/immich-public-proxy))

## Overview

Add Immich as a third storage backend (alongside OpenCloud and Nextcloud). Recipients get a photo/video gallery view instead of the file-tree-and-download UI. Share creation is zero-touch: create an album share in Immich, send the link, done. No extension UI needed.

## Architecture

```
Immich (LAN) ←→ Agent (LAN) ←→ Signaling Server (public) ←→ Recipient Browser
                  ↑                 ↑
            API key:              /i/KEY in
            sharedLink.read       sessions table
```

- **Discovery**: Agent polls `GET /shared-links` every 30s with a `sharedLink.read`-scoped API key. Auto-registers each discovered key as a session via the existing `registershare` mechanism. The auto-discovery polling is new (Immich-specific); the session registration path is shared with OC/NC.
- **Asset serving**: All file/thumbnail access uses the Immich share key as auth (no API key). Same pattern as immich-public-proxy.
- **Signaling server**: Stores Immich shares in the standard sessions table. `/i/KEY` is a session lookup (same as `/s/CODE`). No separate routing infrastructure.

## Agent: Immich API Client

New package `agent/internal/immich/` implementing the `StorageBackend` interface (renamed from `openCloudClient` to reflect multi-backend support):

```go
type Client struct {
    baseURL  string  // e.g. http://immich.lan:2283
    shareKey string  // extracted from /share/{key}
    password string  // optional, sent as X-Immich-Shared-Link-Password header
}
```

**Endpoints called:**

| Method | Immich API | Auth |
|--------|-----------|------|
| `ListFiles` | `GET /api/shared-links/my-share?key={key}` | share key |
| `GetThumbnail` | `GET /api/assets/{id}/thumbnail?key={key}` | share key |
| `GetFile` | `GET /api/assets/{id}/original?key={key}` | share key |
| `PollShares` | `GET /shared-links` | API key (sharedLink.read) |

**Password handling**: Immich uses a different auth model than OC/NC — there is no HMAC pre-challenge. The agent cannot verify passwords locally because Immich stores them as a hash. Instead:

1. Share info (fetched during session registration) includes `isPasswordProtected: true`
2. Recipient visits `/i/KEY` → signaling server responds with `{"type": "password_required"}`
3. Browser prompts for password, sends `{"type": "password_submit", "password": "..."}` over the signaling WebSocket
4. Signaling server relays to agent (password transits the signaling server)
5. Agent calls `GET /api/shared-links/my-share?key={key}` with `X-Immich-Shared-Link-Password` header
6. Immich accepts/rejects → agent tells signaling server `auth_ok` or `auth_fail`
7. On success: agent creates WebRTC peer. On failure: no peer created

The agent never knows the password — it sees only Immich's yes/no response. The raw password transits the signaling server, but this is acceptable: the signaling server has no Immich or LAN access, and a malicious signaling server can already swap the browser JS to capture passwords (the HMAC model assumes a trusted server serving untampered JS). This provides the same resource-exhaustion protection as HMAC (no peer created before auth) via a different mechanism (Immich API validation instead of local HMAC verify).

**Trust model note:** This is a deliberate exception to the server trust model for Immich shares. The OC/NC HMAC pre-challenge prevents the signaling server from seeing the raw password; the Immich flow does not. Future readers should treat this as a known design decision, not an oversight. The Immich architecture (password hash stored on Immich, not shared with the agent) makes HMAC impossible without out-of-band password exchange.

### Session Registration: isPasswordProtected

When the agent calls `registershare` for an Immich key, it fetches the share info from Immich and stores `isPasswordProtected` in the sessions table alongside the code. The signaling server reads this field when a browser connects: if true, it sends `password_required` and enters the auth relay flow. If false, it sends `ice_config` immediately. The browser sends a standard `join` message; the agent skips HMAC verification (the share has no password to prove). The `join` triggers peer creation and offer generation.

### Hub Auth Relay

The signaling server's hub already routes WebRTC signaling messages (offer/answer/ICE) between browser and agent WebSocket connections using the session code as the routing key. The password auth flow reuses these same routing primitives:

```
Browser WS                  Hub                     Agent WS
   |                         |                         |
   |-- password_submit ----->|                         |
   |  {code, password}      |                         |
   |                         |-- password_submit ----->|
   |                         |  {code, password}      |
   |                         |                         |-- Immich API call
   |                         |                         |   (with password)
   |                         |                     <---|
   |                         |                         |
   |                         |<-- auth_ok / auth_fail -|
   |<-- auth_ok / auth_fail -|                         |
   |                         |                         |
   |--- (on auth_ok) WebRTC negotiation begins ---->---|
```

The hub's `ForwardToAgent` and `ForwardToBrowser` methods handle the routing. No new hub infrastructure is needed — this is the same pattern as WebRTC offer/answer relaying. The hub adds three new behaviors: (1) send `ice_config` or `password_required` unilaterally on browser connect based on `isPasswordProtected`, (2) validate `password_submit.code` matches the WS session code, and (3) count password attempts, close browser WS after 5 consecutive `auth_fail` responses.

## Signaling Protocol Changes

### Password Auth (Immich-specific, replaces HMAC pre-challenge)

**`password_required`** (server → browser, JSON)
```json
{"type": "password_required"}
```
Sent when the session is password-protected and the browser must submit a password before the agent creates a peer.

**`password_submit`** (browser → server → agent, JSON)
```json
{"type": "password_submit", "code": "ffSw63qn...", "password": "hunter2"}
```
Browser sends password over signaling WebSocket. Server validates that `code` matches the session code on this WS connection (reject with `auth_fail` if mismatched). Then relays to agent. Agent tests against Immich API. On success: `auth_ok` + peer creation. On failure: `auth_fail`, no peer created.

**Brute-force protection:** The hub limits password attempts to 5 per browser WS connection. On the 5th `auth_fail`, the hub closes the browser WebSocket. A new WS connection resets the counter (but WS connect itself is rate-limited at the knock endpoint). This replaces the DataChannel-based "3 strikes per session" lockout used for OC/NC.

**Unprotected share flow:** When `isPasswordProtected` is false, the hub sends `ice_config` immediately on browser WS connect and the browser sends a standard `join` message. The agent sees `ShareType == "immich"` with `isPasswordProtected == false` and skips HMAC verification — there's no password to prove. The `join` triggers peer creation and offer generation as usual. Reusing `join` (rather than a new message) keeps the agent-side peer creation trigger path unified.

## DataChannel Protocol Changes

Four new DataChannel message types:

### `thumbnail_list` (agent → browser, JSON)
```json
{
  "type": "thumbnail_list",
  "albumName": "Summer Vacation 2025",
  "albumDescription": "Beach trip photos",
  "items": [
    {
      "id": "abc123",
      "name": "photo.jpg",
      "mimeType": "image/jpeg",
      "width": 4000,
      "height": 3000,
      "size": 5242880,
      "thumbSize": 18432,
      "duration": null
    },
    {
      "id": "vid456",
      "name": "sunset.mp4",
      "mimeType": "video/mp4",
      "width": 3840,
      "height": 2160,
      "size": 524288000,
      "thumbSize": 24576,
      "duration": 94.5
    }
  ]
}
```
`albumName` and `albumDescription` come from the Immich share info response. `albumDescription` may be empty. `duration` is a float in seconds for video assets, `null` for images — sourced from the Immich asset metadata (`exifInfo.duration`).

### `thumbnail_data` (agent → browser, binary)
2-byte asset index (into thumbnail_list array) + JPEG bytes. One per asset.

### `asset_request` (browser → agent, JSON)
```json
{"type": "asset_request", "id": "abc123", "quality": "original"}
```

Full asset transfer reuses existing `file_header` / `chunk` / `chunk_end` protocol unchanged.

## Browser: Gallery Mode

The recipient browser app gains a gallery mode alongside the existing file tree mode. Mode is selected based on the URL path (`/i/` = gallery, `/s/` = file tree).

**Gallery UI:**
- 3-column responsive thumbnail grid (2-col on mobile)
- Video thumbnails with duration badge overlay
- Album title + item count in header bar
- "Download All" button: deferred to Slice 17b (requires parallel transfers for good UX).
- **lightGallery.js** for lightbox: zoom, swipe, keyboard nav, video playback, per-item download
- ShareBridge header bar preserved (logo, connection badge, session code)
- PicoCSS styling

**Thumbnail flow:**
1. DataChannel opens → agent sends `thumbnail_list` → browser renders grid shells
2. Agent streams `thumbnail_data` frames → browser populates grid progressively
3. User taps thumbnail → browser sends `asset_request` → agent streams full file via chunk protocol
4. Full asset rendered in lightbox (image) or video player (video, full download required before playback)

**Video note**: Full download before playback (no progressive streaming through DataChannel). Future optimization: MediaSource Extensions with fragmented MP4 repackaging.

## Session Registration + Routing

Immich shares use the same session infrastructure as OC/NC. No separate routing layer.

### Agent: Auto-Registration

- Every 30s: poll `GET /shared-links` → get active share keys
- For each **new** key not yet registered: send `registershare` with `"code": "ffSw63qn..."` — the server uses the provided code instead of generating one
- For each **removed** key (share expired/deleted in Immich): send `unregistershare`
- On agent startup: poll immediately, register all current keys
- On agent reconnect: same flow as OC/NC — re-register all persisted sessions (including Immich)

**External code protocol note:** The existing `register_share` message gains two new fields: `code` (optional, string) and `is_password_protected` (optional, boolean). When `code` is present and non-empty, the server skips code generation and uses the provided value. The server stores `is_password_protected` in the sessions table alongside the code. The server still validates code uniqueness — if the Immich key collides with an existing code (vanishingly unlikely), it returns an error and the agent skips that share. Immich keys are long random strings with sufficient entropy to share the same namespace as 8-char server-generated codes.

The signaling server stores Immich shares in the same sessions table. The code column holds the Immich key. All existing session infrastructure works unchanged: persistence, bandwidth tracking, download counting, expiry, reclaim on reconnect.

### Signaling Server

- `GET /i/KEY` → lookup in sessions table (same as `/s/CODE`)
- Key miss → 404
- No separate capability map — the sessions table IS the source of truth

A newly-created Immich share may not appear for up to 30 seconds (until the next poll). Acceptable.

**Share deletion during active transfer:** If the 30s poll detects a key removal (share deleted/expired in Immich) while a recipient has an active download, the agent terminates the transfer with an `{"type": "error", "message": "share has been removed"}` message and closes the peer connection.

### Immich API Paths (verify during implementation)

The spec uses these paths based on the documented Immich API. Exact paths must be verified against the running Immich instance during implementation — the API docs pagination prevented full verification.

| Purpose | Expected path | Auth |
|---------|--------------|------|
| List all shares | `GET /shared-links` | API key |
| Get share by key | `GET /shared-links/my-share?key={key}` | share key |
| Thumbnail | `GET /assets/{id}/thumbnail?key={key}` | share key |
| Original | `GET /assets/{id}/original?key={key}` | share key |

## URL Structure

| Path | Source | Example |
|------|--------|---------|
| `/s/CODE` | Server-generated 6-char code | `sharebridge.app/s/ABC123` |
| `/i/KEY` | Immich-generated share key | `sharebridge.app/i/ffSw63qnIYMt...` |

Both use the same sessions table. Immich keys are just longer strings. Code generation is skipped when the agent provides an external code.

## Share Creation Flow

Zero-touch. No extension, no admin UI, no userscript required.

1. In Immich: create a shared album (sets password, expiry, etc. in Immich)
2. Agent's next 30s poll discovers the new share key
3. Agent calls `registershare` with the Immich key → signaling server stores it in sessions table
4. Share link (`sharebridge.app/i/KEY`) is immediately live for recipients

If you need the link before the 30s window, the agent admin UI can trigger an immediate poll (manual refresh button). Not required for normal use.

## Config

```bash
IMMICH_URL=http://immich.lan:2283
IMMICH_ALLOWED_HOST=immich.lan       # SSRF protection
IMMICH_API_KEY=sb_immich_xxx         # scoped to sharedLink.read only
```

Optional: `IMMICH_POLL_INTERVAL=30` (seconds, default 30).

## Agent API Changes

**`POST /api/v1/shares` (JSON API):** Accept `"immich"` as valid `share_type`.
**`POST /api/shares` (form API):** Accept `"immich"` as valid `share_type`.

No new API endpoints needed.

## Persistence

`SessionEntry.ShareType` already stored as `"opencloud"` or `"nextcloud"`. Adding `"immich"` requires one new boolean column in the sessions table: `is_password_protected` (default false, no migration needed for existing OC/NC rows). On agent restart, `ShareType` is used to reconstruct the correct backend client. Agent-side session persistence (`sessions.json`) also gains the `is_password_protected` field.

## Files Changed

**New:**
- `agent/internal/immich/client.go` — Immich API client
- `agent/internal/immich/client_test.go` — tests with mocked Immich API
- `signaling-server/web/src/gallery.js` — gallery grid + lightGallery.js init

**Modified:**
- `agent/internal/transfer/manager.go` — rename `openCloudClient` to `StorageBackend`, add thumbnail pass
- `agent/internal/cloudwebdav/client.go` — rename interface name in doc comment
- `agent/internal/daemon/daemon.go` — accept "immich" share_type, construct ImmichClient
- `agent/internal/web/api_v1.go` — validate "immich"
- `agent/internal/web/api.go` — validate "immich"
- `agent/internal/config/config.go` — add ImmichURL, ImmichAllowedHost, ImmichAPIKey
- `agent/internal/signaling/client.go` — accept optional code in `RegisterShare`
- `signaling-server/web/src/app.js` — detect `/i/` path, route to gallery mode
- `signaling-server/web/src/messages.js` — handle thumbnail_list, thumbnail_data, asset_request
- Signaling server schema: add `is_password_protected` column to sessions table
- Signaling server: `/i/` route handler, accept `code` and `is_password_protected` in `register_share`

**Dependency added:**
- `lightGallery.js` (npm, ~50KB gzipped)

## Security

- **Signaling server never accesses Immich** — agent is the only thing that calls Immich API
- **Share key gates asset access** — no API key used for serving content
- **API key is read-only, single-scope** (`sharedLink.read`) — cannot modify, upload, or delete
- **Password validated by Immich** — agent is a blind relay, never knows the password (unlike OC/NC where agent owns it)
- **No IP leak** — all recipient traffic goes through signaling server WebSocket
- **Key enumeration protection** — rate limiting on `/i/KEY` endpoint, keys are high-entropy random strings
- **No broadcast** — sessions table ensures O(1) routing, no cross-agent information leak

## Credit

Immich integration design heavily inspired by [Immich Public Proxy](https://github.com/alangrainger/immich-public-proxy) by Al Angrainger — which proved the share-key-as-auth pattern.
