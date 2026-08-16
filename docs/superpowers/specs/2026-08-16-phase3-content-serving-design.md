# Phase 3 — Content Serving (Direct HTTP) + `control/` Rename

**Date:** 2026-08-16
**Status:** Draft — revision 2 (incorporates design review)
**Companion to:** `docs/superpowers/specs/2026-08-15-phase2-direct-mode-transport-design.md` (the transport this phase builds on)
**Forks off:** `v2`

## 1. Summary

Phase 2 proved the direct transport end-to-end (native HTTPS terminating at the agent,
real cert, on-demand port) but served only a **placeholder** page. Phase 3 replaces the
placeholder with **real content serving**: the agent's direct server serves Immich
shares over native HTTP — a gallery page, thumbnails, previews, full assets, transcoded
video, and album downloads — reusing the recipient UI the signaling server already
serves. It also cuts the canonical route over to direct, renames `signaling-server/` →
`control/`, and (gated behind automated parity) deletes the v1 WebRTC/relay/multilane
transport.

**In scope (three gated milestones):**
- **3a — Content serving**: direct-HTTP serving for Immich gallery shares + the
  recipient UI moved to the agent + the canonical-route cutover (Immich links go direct).
- **3b — `control/` rename**: `signaling-server/` → `control/`, module
  `sharebridge/server` → `sharebridge/control`.
- **3c — v1 transport deletion**: agent + server + extensions + client JS, **only after
  3a's automated parity tests pass** (§12.2).

**Out of scope (deferred):**
- WebDAV / file shares (drops in later on the same serving layer; the file-browser half of the UI stays as reference).
- **Password-protected Immich shares** (§7.2) and **private/authenticated shares** (GitHub OAuth). The auth seam (§5) is designed so both slot in later without rework.
- **Large-album pagination** (§4.3): the frozen `ListGallery` eagerly loads all pages; Phase 3 keeps v1 parity and documents the limit.
- Relay fallback / FRP tunnel + measure-and-prefer → **Phase 4**.
- Any UI framework rewrite — the vanilla-JS lightGallery UI is kept (lean, no build step).

## 2. Architecture

Three changes, in gated order: the agent's `DirectServer` grows an HTTP content layer
(3a); the control plane tree is renamed (3b); the v1 transport is deleted (3c). The
share→content binding is **frozen** (§7).

### Agent (3a)

1. **`direct.DirectServer` content layer** — replaces the placeholder `handlePage`/
   `handleDownload`. `/s/{code}` dispatches to content handlers backed by a narrow
   `ContentBackend` interface (§5), implemented by the frozen `immich.Client`.
2. **Embedded recipient UI** — `index.html` + `gallery.js` + lightGallery vendor +
   `videoBufferWarning.js`, served under a code-prefixed static namespace (§6).
3. **In-flight-aware idle handling** — the on-demand port must not close while a
   response is streaming (§4.7).

### Control plane (3a–3b)

4. **Canonical-route cutover** (§8): `/share/{code}` resolves Immich shares to the
   direct path; the v1 `/i/{key}`, `/join`, `/ws/client`, `/ws/relay`, and web-asset
   routes are retired (in 3c).
5. **Rename** (3b): mechanical, no functional change (§9).

### Deleted (3c, gated)

6. The v1 transport stack (§10): agent `transfer`/`multilane`/`relaychannel`/`peer`/
   `noise`, server `internal/relay` + `relay_ws.go` + `browser_ws.go` + TURN/ICE + the
   v1 routes, `extensions/`, and the client JS transport + both service workers.

## 3. Corrected Immich API mapping (frozen backend)

The `transfer.GalleryBackend` names (`GetAsset`, `GetAssetRange`, `GetAlbumDownload`,
`StreamAlbumArchive`) are **adapter** methods in `daemon.go` — **not** methods on
`immich.Client`. The frozen client's actual methods are:

| `immich.Client` method | Purpose |
|---|---|
| `ListGallery(ctx)` | album + items (eager, all pages) |
| `GetThumbnail(ctx, id, w)` | thumbnail image |
| `GetPreview(ctx, id, w)` | medium preview image (lightbox) |
| `GetAssetInfo(ctx, id)` | `Asset` (name, size, mimeType) |
| `GetFile(ctx, id, w)` | **original** asset (full body, **no Range**) |
| `GetVideoPlayback(ctx, id, w)` | transcoded video (progressive MP4, full) |
| `GetVideoPlaybackRange(ctx, id, startOffset, w)` | transcoded video, byte-offset read |
| `HeadVideoPlayback(ctx, id)` | transcoded video `Content-Length` |
| `GetAlbumDownloadInfo(ctx)` | `AlbumDownload` (may hold **multiple** `Archives`) |
| `DownloadArchive(ctx, assetIDs, w)` | stream one archive (ZIP) |
| `PollShares`, `ValidatePassword`, `IsPasswordProtected` | discovery / password (see §7.2) |

**Consequences, stated explicitly so the plan is implementable without unfreezing `client.go`:**
- **Range is supported only for transcoded video** (`GetVideoPlaybackRange` +
  `HeadVideoPlayback`). **Original assets (`GetFile`) are full-body only** — no Range,
  no HLS. Original video is a *download*, not a playback stream.
- **No HLS.** The client speaks progressive-MP4 byte-ranges; there is no manifest/segment
  route or upstream-header passthrough. HLS is not in scope.
- **Multi-part archives** are a first-class case (§4.6) — `GetAlbumDownloadInfo` returns
  an array of archives, and each is streamed separately via `DownloadArchive`.

## 4. HTTP serving layer

### 4.1 Endpoints (all under `https://<origin>/s/{code}/…`)

| Method | Path | Serves | Backing call |
|---|---|---|---|
| GET/HEAD | `/s/{code}` | gallery HTML (embedded UI) | — |
| GET/HEAD | `/s/{code}/items` | album + item metadata (JSON) | `ListGallery` |
| GET/HEAD | `/s/{code}/thumb/{id}` | thumbnail | `GetThumbnail` |
| GET/HEAD | `/s/{code}/preview/{id}` | lightbox preview | `GetPreview` |
| GET/HEAD | `/s/{code}/asset/{id}` | original asset (download) | `GetFile` |
| GET/HEAD | `/s/{code}/asset/{id}/playback` | transcoded video (Range) | `GetVideoPlayback` / `GetVideoPlaybackRange` + `HeadVideoPlayback` |
| GET/HEAD | `/s/{code}/archive` | archive manifest (JSON) | `GetAlbumDownloadInfo` |
| GET/HEAD | `/s/{code}/archive/{part}` | one archive ZIP (attachment) | `DownloadArchive` |

`GetAssetInfo` supplies name/size/mimeType for `Content-Type` and (when authoritative)
`Content-Length`. Writer-based methods stream straight into the `ResponseWriter`.

### 4.2 Range (transcoded video only)

- Accept a **single** `bytes=start-end` or `bytes=start-` range. Suffix ranges
  (`bytes=-N`), multiple ranges, non-numeric, or `start ≥ total` → `416 Range Not
  Satisfiable` with `Content-Range: bytes */<total>`.
- Valid range → `206 Partial Content`, `Content-Range: bytes start-end/total`,
  `Accept-Ranges: bytes`; no Range header → `200` + full body.
- `total` comes from `HeadVideoPlayback`; `start-end` reads use `GetVideoPlaybackRange`
  and must stop the upstream read at `end` (bounded read, not a full re-stream).
- **Original assets and all non-video resources do not advertise `Accept-Ranges`.**

### 4.3 `items` and album size

`/items` returns the full `ListGallery` result (v1 parity: the gallery already loads
all pages eagerly). Empty album → `200` with `items: []`. **Large-album pagination is
deferred**; the plan documents the eager-load memory/latency behavior and the UI's
existing 120-item "load-more" windowing. No album-size cap is imposed, but this is an
explicit, documented known-limit.

### 4.4 HEAD

Every content endpoint serves HEAD: identical status + headers to GET, **no body
transfer** (handlers must not invoke the writer-based GET path for HEAD — derive
status/length from `GetAssetInfo`/`HeadVideoPlayback`/metadata, not from streaming).
`Content-Length` is omitted when the length is not authoritative.

### 4.5 Errors and response commitment

- **Preflight before streaming**: call `GetAssetInfo` (or `HeadVideoPlayback`) first to
  validate the asset exists and to obtain length/type; only then stream. Unknown asset
  → `404` before any bytes are written.
- **Mid-stream upstream failure** (Immich errors after `200` is committed): log, close
  the connection; the client detects truncation via the `Content-Length` mismatch. No
  status change is possible post-commit — document this.
- Fallback MIME: `application/octet-stream` when `GetAssetInfo` has no usable type.

### 4.6 Album archive (multi-part)

- `GET /s/{code}/archive` → JSON manifest: `{ parts: [{ index, name, estimatedSize,
  assetIds }] }`. `GetAlbumDownloadInfo` returning **zero** archives → `404`.
- `GET /s/{code}/archive/{part}` → stream that part via `DownloadArchive(assetIDs, w)`,
  `Content-Disposition: attachment`, filename `part-<index>.zip` (or the album name +
  index, sanitized).
- Multiple parts are **not** concatenated; the UI presents each part as its own
  download link. Archive streaming is exclusive per share (one at a time, §11).

### 4.7 Idle vs. long transfers

`DirectServer` tracks in-flight responses (atomic counter / counting writer). The
on-demand port's idle-close must treat an **in-progress response** as activity, so a
large asset/archive/video lasting beyond the idle timeout does not get its mapping
closed mid-stream. An integration test transfers longer than the idle timeout (§12.1).

### 4.8 Headers & caching

- **`Cache-Control: no-store` on everything** (page, items, thumb, preview, asset,
  playback, archive). The share is ephemeral and revocable; no caching is the only
  behavior consistent with revocation.
- `X-Content-Type-Options: nosniff`, a restrictive `Content-Security-Policy`, and
  `Referrer-Policy: no-referrer` on the gallery page.
- `Content-Disposition: attachment` on `/asset/{id}` (original download) and
  `/archive/{part}`; filename derived from `GetAssetInfo` name via
  `mime.FormatMediaType`, with an ASCII-safe fallback and path separators stripped.

## 5. Auth & resolver seam

The DirectServer resolves a code through a single seam:

```
resolve(code) → (ContentBackend, error)   // error → 404 (unknown) / 403 (revoked)
```

- `ContentBackend` is a **narrow, immutable** interface (the §3 subset of
  `immich.Client`), returned as a **snapshot** the daemon constructs under its lock so
  concurrent polling can't mutate it mid-request.
- Request-time checks in the seam: share exists, session active, share type is
  **gallery**, not revoked, and download limit not exceeded (§11.2). The origin binding
  (Phase 2 SNI admission) remains the outer gate; this seam adds content-level
  authorization.
- This is the **single extension point** for private shares and password-protected
  shares later (a bearer token or session cookie would resolve through the same seam).
  No token/cookie is added now.

## 6. Recipient UI — move, reuse, rewrite, delete

The recipient UI (`index.html` + `app.js` + `gallery.js` + lightGallery + transport
modules + `sw.js`) relocates to the agent, embedded via `go:embed`, served by
`DirectServer` **under `/s/{code}/static/…`**. The existing page's absolute `/src/…`,
`/app.js`, `/sw.js`, and `/noise-p256/…` references must all be rewritten to the
code-prefixed namespace (the Phase-2 `Binder` rejects any path without the bound
`/s/{code}` prefix), and service-worker registration removed.

- **Move + reuse:** `index.html` (gallery + file-browser DOM/CSS; file-browser half is
  reference for the WebDAV phase), `src/gallery.js` (presentation: lightbox,
  zoom/swipe, video playback, per-item download, album-download, load-more — decoupled
  via injected callbacks, zero imports), `src/videoBufferWarning.js`, `src/vendor/lightgallery/*`.
- **Rewrite:** `app.js` (fetch the §4.1 endpoints; hand URLs to the gallery) and
  `gallery.js`'s data layer. The revised gallery API receives **URLs directly**:
  `thumbUrl(id)`, `previewUrl(id)`, `assetUrl(id)`, `playbackUrl(id)`, `archiveUrl()` —
  replacing binary-frame thumbnail batches, `/media/{id}` video, and binary preview
  payloads. Thumbnails become `<img src="…/thumb/{id}" loading="lazy">`.
- **Delete (transport):** `directChannel.js`, `secureRelayChannel.js`,
  `connectTransferChannel.js`, `binaryEnvelope.js`, `frame.js`, `channelSet.js`,
  `laneScheduler.js`, `multiLaneProtocol.js`, `downloadPipeline.js`, `downloadSinks.js`,
  `downloadCapabilities.js`, `sw.js` (media-bridge SW), streamsaver vendor
  (`streamsaver*.js`, `*-mitm.html`), `noise-p256/`.

**Stays with the control plane:** `account.html`, `home.html`, `login.html`,
`register.html` (server-rendered account UI) — see §9 for the deploy change.

## 7. Share → content binding (frozen) + protected shares

### 7.1 Frozen discovery/registration

Unchanged from v1: the agent polls Immich shared-links (`PollShares`), discovers shares,
and registers a matching ShareBridge share with the control plane (`RegisterShare` →
code + control-allocated origin). The `code → Immich shareKey` mapping stays
agent-local, maintained by the daemon's polling/registration flow. **Untouched
backends:** `agent/internal/immich/client.go` and `agent/internal/cloudwebdav/client.go`.

### 7.2 Password-protected shares — deferred (explicit)

Immich shared-link passwords are an **existing** feature (`IsPasswordProtected`,
`ValidatePassword`, a password form in `index.html`) that the v1 browser-signaling flow
handles. Removing that flow breaks it. Phase 3 **does not register or serve
password-protected shares** — the poller skips them. The future direct flow (password
submission endpoint, rate-limited attempts, an HttpOnly/Secure/SameSite scoped session
cookie, per-auth-session backend credentials) is specified to reuse the §5 seam; it is
**not** implemented now.

## 8. Canonical-route cutover (3a)

Today the control plane routes Immich links through the v1 path:

- `/share/{code}` → `serveShareRedirect` → `/i/{code}` (Immich) or `/s/{code}` (other).
- `/i/{key}` serves `web/index.html` (v1 UI); `/join` serves it too.
- `/ws/client`, `/ws/relay` are the v1 browser/relay sockets.

Phase 3a changes this so **Immich links reach the agent's direct server**:

- `/share/{code}` → resolve the session → `302` to the canonical `/s/{code}` direct
  path (for gallery shares), regardless of Immich vs. non-Immich. The existing
  `relay_only` / offline / expired handling is preserved (§ of the Phase 2 spec).
- `/i/{key}`, `/join`, `/ws/client`, `/ws/relay`, `/sw.js`, `/app.js`, `/src/{…}`,
  `/noise-p256/{…}` are retired in 3c (removed once the v1 transport is deleted).
- Route tests cover: active gallery share (→ direct), relay-only, expired, revoked,
  offline, protected (→ not served).

## 9. `control/` rename (3b)

Mechanical, no functional change:
- Directory `signaling-server/` → `control/`; module `sharebridge/server` →
  `sharebridge/control` (`go.mod` + every import path).
- `deploy-testing.sh` moves to `control/deploy-testing.sh`. Its `scp web/` step is
  **reduced, not removed**: the control server still serves `home.html`, `login.html`,
  `register.html`, `account.html` from `./web/` at runtime, so the script keeps copying
  the **account pages only** (the recipient UI + transport assets left the tree in 3a).
- Update: `deploy.sh` (image/install paths), `signaling-server/docs/testing-server.md`
  (moves to `control/docs/`), CI workflows, README, `scripts/update_streamsaver_vendor.sh`
  (deleted with StreamSaver in 3c).

## 10. v1 transport deletion (3c, gated)

After 3a's automated parity tests pass (§12.2), delete the v1 transport in one scoped
cleanup. Git history is the reference (record the pre-deletion commit hash in the
plan); no `relay-old`/`relay-v1` copies.

| Tree | Delete |
|---|---|
| agent | `internal/transfer`, `internal/multilane`, `internal/relaychannel`, `internal/peer`, `internal/noise` |
| control | `internal/relay`, `internal/handler/relay_ws.go`, `internal/handler/browser_ws.go`, TURN/ICE wiring, `/ws/client` + `/ws/relay` + `/i/{key}` + `/join` + web-asset routes, relay config/registration fields |
| repo | `extensions/`, `scripts/update_streamsaver_vendor.sh` |
| client JS | §6 "Delete" list |

The deletion is not a simple tree removal — `cmd/server/main.go`, `agent_ws.go`, and
`daemon.go` deeply wire the v1 packages (peer/multilane/relay/TURN). The plan must
enumerate every route, handler, config field, WS message type, daemon field/function,
and test to remove, so each step keeps `go build ./...` + `go test ./...` green. Remove
transport message handling from the kept `agent/internal/signaling` client.

**Kept (frozen):** `internal/immich`, `internal/cloudwebdav`, `internal/signaling`,
`internal/direct`, `internal/cert`, `internal/daemon` (minus the v1 wiring),
`internal/store`, `internal/web` (agent admin UI), `internal/config`.

## 11. Resource limits & concurrency

- `http.Server`: explicit `ReadHeaderTimeout`, `IdleTimeout`, `MaxHeaderBytes`.
- Per-share concurrency: a semaphore limiting simultaneous streaming responses (assets,
  video, archives) per share; global cap across shares. Over limit → `429` (transient)
  or `503` with a bounded-retry hint.
- Archive streaming is **exclusive** per share (one at a time).
- Cancellation: propagate request `ctx` into every immich call; abort upstream on client
  disconnect.
- Path constraints: `{code}` matches the existing code charset/`[A-Za-z0-9_-]{8,64}`;
  `{id}` is an Immich asset UUID, validated by strict regex, with length caps; reject
  encoded slashes, dot segments, and multi-segment paths. Verify `{id}` ∈ the resolved
  gallery's items (membership check) before streaming.

## 12. Testing

### 12.1 Matrix

- **Unit** — Range parsing (single/suffix/multiple/invalid/`start≥total` → `206`/`416`
  + `Content-Range: bytes */total`), HEAD (no body), content-type, `404`/`403` on
  unknown/revoked codes, path-traversal/encoded-separator rejection, `Content-Disposition`
  filename sanitization, no-store/nosniff/CSP/Referrer-Policy headers, resolver
  lifecycle (snapshot immutability, concurrent polling).
- **Integration** — real agent stack (cert.Manager + Binder + DirectServer +
  OnDemandPort + a fake `ContentBackend`, mirroring `6233e10`): SNI admission → gallery
  HTML → items → thumb → preview → asset → playback (206) → archive manifest → part.
  Plus: transfer-longer-than-idle-timeout does **not** close the mapping; concurrent
  recipients; cancellation; overload → `429`/`503`.
- **UI** — port `gallery.test.js` to the URL-based data layer (item render, lightbox,
  download/album-download against a stubbed `fetch`).
- **Route cutover** — `/share/{code}` → direct for gallery; relay-only/expired/revoked/
  offline/protected cases.

### 12.2 Parity gate (blocks 3c)

An automated end-to-end test must pass before the v1 transport is deleted: enroll → cert
→ register gallery share → open → probe → redirect → `/s/{code}` (HTML) → `/items` →
`/thumb/{id}` → `/preview/{id}` → `/asset/{id}` → `/asset/{id}/playback` (Range `206`)
→ `/archive` → `/archive/{part}`, all over the direct path with a test CA. This is the
"content parity" evidence that gates milestone 3c.

## 13. Migration / rollout

- No schema migration (content serving is agent-local; no new collections).
- 3a is additive to Phase 2's transport; the v1 transport stays wired (unused) until the
  gated 3c cleanup, so every plan step keeps `go build ./...` green in isolation.
- Live e2e (optional, manual): `control/deploy-testing.sh --bootstrap` on a Hetzner box
  + agent against a real Immich instance; verify a real share serves thumbnails,
  preview, an original, and a transcoded video over the direct path.
