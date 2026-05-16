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

New package `agent/internal/immich/` implementing the `openCloudClient` interface:

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

**Password handling**: Unlike OC/NC (where the agent owns and verifies the password via HMAC), the Immich agent is a blind relay. It does not know the password. When a recipient submits a password, the agent forwards it to the Immich API as an `X-Immich-Shared-Link-Password` header. Immich's own auth gates the share and returns accept/reject. The agent sees only the yes/no result. Whether a share requires a password is discovered from the share info response on first access.

## Protocol Changes

Three new DataChannel message types:

### `thumbnail_list` (agent → browser, JSON)
```json
{
  "type": "thumbnail_list",
  "items": [
    {
      "id": "abc123",
      "name": "photo.jpg",
      "mimeType": "image/jpeg",
      "width": 4000,
      "height": 3000,
      "size": 5242880,
      "thumbSize": 18432
    }
  ]
}
```

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
- "Download All" button
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
- For each **new** key not yet registered: send `registershare` to signaling server with the Immich key as the code
- For each **removed** key (share expired/deleted in Immich): send `unregistershare`
- On agent startup: poll immediately, register all current keys
- On agent reconnect: same flow as OC/NC — re-register all persisted sessions (including Immich)

The signaling server stores Immich shares in the same sessions table. The code column holds the Immich key. All existing session infrastructure works unchanged: persistence, bandwidth tracking, download counting, expiry, reclaim on reconnect.

### Signaling Server

- `GET /i/KEY` → lookup in sessions table (same as `/s/CODE`)
- Key miss → 404
- No separate capability map — the sessions table IS the source of truth

A newly-created Immich share may not appear for up to 30 seconds (until the next poll). Acceptable.

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

`SessionEntry.ShareType` already stored as `"opencloud"` or `"nextcloud"`. Adding `"immich"` requires no schema migration. On agent restart, `ShareType` is used to reconstruct the correct backend client.

## Files Changed

**New:**
- `agent/internal/immich/client.go` — Immich API client
- `agent/internal/immich/client_test.go` — tests with mocked Immich API
- `signaling-server/web/src/gallery.js` — gallery grid + lightGallery.js init

**Modified:**
- `agent/internal/daemon/daemon.go` — accept "immich" share_type, construct ImmichClient
- `agent/internal/web/api_v1.go` — validate "immich"
- `agent/internal/web/api.go` — validate "immich"
- `agent/internal/config/config.go` — add ImmichURL, ImmichAllowedHost, ImmichAPIKey
- `agent/internal/transfer/manager.go` — thumbnail pass before serving file list
- `signaling-server/web/src/app.js` — detect `/i/` path, route to gallery mode
- `signaling-server/web/src/messages.js` — handle thumbnail_list, thumbnail_data, asset_request
- Signaling server: `/i/` route handler, `registershare`/`unregistershare` for external codes

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
