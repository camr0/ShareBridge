# Phase 3 — Content Serving (Direct HTTP) + `control/` Rename

**Date:** 2026-08-16
**Status:** Draft — revision 3 (incorporates design review round 2)
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

**Compatibility policy (explicit breaking change):** v2 has no legacy compatibility
(pre-release, zero users). Removing the v1 transport **retires** relay-only shares,
WebDAV/file shares, and password-protected shares — they are **not migrated**; they are
rejected at registration and return a clean "unsupported" response when served. The
default `DefaultRelayOnly` flips to `false` in 3a so new registrations are direct. This
is intentional and asserted by the parity gate (§12.2).

**Out of scope (deferred):**
- WebDAV / file shares (drops in later on the same serving layer; the file-browser half of the UI stays as reference).
- **Password-protected Immich shares** (§7.2) and **private/authenticated shares** (GitHub OAuth). The auth seam (§5) is designed so both slot in later without rework.
- **Large-album pagination** (§4.3): the frozen `ListGallery` eagerly loads all pages; Phase 3 keeps v1 parity and documents the limit.
- Relay fallback / FRP tunnel + measure-and-prefer → **Phase 4**.
- Any UI framework rewrite — the vanilla-JS lightGallery UI is kept (lean, no build step).

## 2. Architecture

Three changes, in gated order: the agent's `DirectServer` grows an HTTP content layer
(3a); the control plane tree is renamed (3b); the v1 transport is deleted (3c). The
share→content binding is **frozen** at the wire level (§7).

### Agent (3a)

1. **`direct.DirectServer` content layer** — replaces the placeholder `handlePage`/
   `handleDownload`. `/s/{code}` dispatches to content handlers backed by a narrow
   `ContentBackend` interface (§5), implemented over the existing `immich.Client`.
2. **Embedded recipient UI** — `index.html` + `gallery.js` + lightGallery vendor +
   `videoBufferWarning.js`, served under a code-prefixed static namespace (§6).
3. **In-flight-aware idle handling** — the on-demand port must not close while a
   response is streaming (§4.7).

### Control plane (3a–3b)

4. **Canonical-route cutover** (§8): `/share/{code}` resolves Immich gallery shares to
   the direct path; the v1 `/i/{key}`, `/join`, `/ws/client`, `/ws/relay`, and web-asset
   routes are retired (in 3c). `DefaultRelayOnly` flips to `false`.
5. **Rename** (3b): mechanical, no functional change (§9).

### Deleted (3c, gated)

6. The v1 transport stack (§10): agent `transfer`/`multilane`/`relaychannel`/`peer`/
   `noise`, server `internal/relay` + `relay_ws.go` + `browser_ws.go` + TURN/ICE + the
   v1 routes, `extensions/`, and the client JS transport + both service workers.

## 3. Immich backend — frozen wire protocol, minimal additive client

The `transfer.GalleryBackend` names (`GetAsset`, `GetAssetRange`, `GetAlbumDownload`,
`StreamAlbumArchive`) are **adapter** methods in `daemon.go` — **not** methods on
`immich.Client`. The client's actual methods:

| `immich.Client` method | Purpose |
|---|---|
| `ListGallery(ctx)` | album + items (eager, all pages) |
| `GetThumbnail(ctx, id, w)` | thumbnail image |
| `GetPreview(ctx, id, w)` | medium preview image (lightbox) |
| `GetAssetInfo(ctx, id)` | original `Asset` (name, size, mimeType) |
| `GetFile(ctx, id, w)` | **original** asset (full body, **no Range**) |
| `GetVideoPlayback(ctx, id, w)` | transcoded video (progressive MP4, full) |
| `GetVideoPlaybackRange(ctx, id, startOffset, w)` | transcoded video, byte-offset read |
| `HeadVideoPlayback(ctx, id)` | transcoded video `Content-Length` (may be unknown/non-positive) |
| `GetAlbumDownloadInfo(ctx)` | `AlbumDownload` (may hold **multiple** `Archives`) |
| `DownloadArchive(ctx, assetIDs, w)` | stream one archive (ZIP) |
| `PollShares`, `ValidatePassword`, `IsPasswordProtected` | discovery / password |

**Consequences:**
- **Range is supported only for transcoded video** (`GetVideoPlaybackRange` +
  `HeadVideoPlayback`). **Original assets (`GetFile`) are full-body only** — no Range,
  no HLS. Original video is a *download*, not a playback stream.
- **No HLS.** The client speaks progressive-MP4 byte-ranges; there is no manifest/segment
  route or upstream-header passthrough.
- **Multi-part archives** are a first-class case (§4.6).

### 3.1 Minimal additive client changes (wire protocol unchanged)

The **request/response shapes and endpoints to Immich are unchanged**; the Go client
gains two additive capabilities the serving layer needs (no behavior change to existing
methods):

1. **Typed error classification** — `getAsset`/`doJSON` currently flatten HTTP status
   into a formatted string. Add typed errors (e.g. `NotFoundError`, `AuthError`,
   `UpstreamError{Status}`) so the serving layer can map `404` vs `403` vs `5xx` vs
   transport failure (§4.5). Exposed via `errors.As`.
2. **Metadata access before streaming** — helpers that return an asset kind's
   `Content-Type` and authoritative `Content-Length` **when the upstream provides it**
   (via a HEAD request or response headers), e.g. `ThumbnailInfo(id)` / `PreviewInfo(id)`
   / `PlaybackInfo(id)`. Where Immich omits the length (thumbnails/previews/archive
   estimates), the helper reports `ok=false` rather than fabricating a length.

## 4. HTTP serving layer

### 4.1 Endpoints (all under `https://<origin>/s/{code}/…`)

| Method | Path | Serves | Backing call |
|---|---|---|---|
| GET/HEAD | `/s/{code}` | gallery HTML (embedded UI) | — |
| GET/HEAD | `/s/{code}/items` | album + item metadata (JSON, §4.3) | `ListGallery` |
| GET/HEAD | `/s/{code}/thumb/{id}` | thumbnail | `GetThumbnail` |
| GET/HEAD | `/s/{code}/preview/{id}` | lightbox preview | `GetPreview` |
| GET/HEAD | `/s/{code}/asset/{id}` | original asset (download) | `GetFile` |
| GET/HEAD | `/s/{code}/asset/{id}/playback` | transcoded video (Range), `video/mp4` | `GetVideoPlayback` / `GetVideoPlaybackRange` + `HeadVideoPlayback` |
| GET/HEAD | `/s/{code}/archive` | archive manifest (JSON, §4.6) | `GetAlbumDownloadInfo` |
| GET/HEAD | `/s/{code}/archive/{part}` | one archive ZIP (attachment) | `DownloadArchive` |

### 4.2 Range (transcoded video only)

- Parse a **single** `bytes=start-end` / `bytes=start-` range (case/whitespace-tolerant).
  Suffix ranges (`bytes=-N`), multiple ranges, non-numeric, or `start ≥ total` → `416
  Range Not Satisfiable` with `Content-Range: bytes */<total>`.
- **Unknown/non-positive total** (`HeadVideoPlayback` ≤ 0): serve `200` + full body via
  `GetVideoPlayback` (no `Accept-Ranges` advertised). Do not fabricate a `206`.
- Valid range → `206 Partial Content`, `Content-Range: bytes start-end/total`,
  `Accept-Ranges: bytes`. `end` is clamped to `total-1`; `start > end` after clamping and
  integer-overflow inputs → `416`. Zero-length total → `416`.
- `start-end` reads use `GetVideoPlaybackRange` with a **bounded writer** that stops the
  upstream read at `end` (the "did we hit the bound" sentinel must not surface as an
  error). `start=0` sends no upstream Range header (client behavior) — a downstream
  `206` is synthesized deliberately.
- **Original assets and all non-video resources do not advertise `Accept-Ranges`.**

### 4.3 `items` JSON

`/items` returns a **normative lowerCamel wire schema** (the agent marshals
`immich.Gallery` — whose raw struct lacks JSON tags — into an explicit DTO):

```json
{
  "albumName": "Summer 2025",
  "albumDescription": "…",
  "items": [
    { "id": "<uuid>", "name": "IMG_0001.jpg", "mimeType": "image/jpeg",
      "width": 4032, "height": 3024, "size": 4200000, "duration": null, "sha1": "…" }
  ]
}
```

Empty album → `200` with `items: []`. `duration` is a float seconds or `null`. **Large-
album pagination is deferred**; the plan documents the eager-load memory/latency
behavior and the UI's existing 120-item "load-more" windowing.

### 4.4 HEAD

HEAD returns **status + headers only, no body**, derived from metadata (§3.1) and
membership (§5) — **not** by invoking the streaming GET path. Where metadata is
unavailable (thumbnails/previews/archives), `Content-Length` is **omitted** and the
handler reports existence via `200`/`404` only. HEAD does **not** promise byte-identical
headers to GET for streaming endpoints. Upstream failure during metadata lookup →
`502`/`503` (§4.5).

### 4.5 Errors and response commitment

- **Preflight before streaming**: resolve + membership-check + metadata lookup first;
  only then stream. Unknown asset → `404` before any bytes are written.
- **Typed error mapping** (§3.1): `NotFoundError` → `404`; `AuthError` → `403`
  (invalid/revoked share); other `UpstreamError` / transport failure → `502`/`503`.
- **Mid-stream upstream failure** (after `200` is committed): log + close the
  connection. **Truncation is only detectable where `Content-Length` was authoritative**
  (transcoded video via `HeadVideoPlayback`); thumbnails/previews/archives may have no
  authoritative length, so their truncation is not guaranteed-detectable — document
  this, don't promise it.
- Fallback MIME: `application/octet-stream` when metadata has no usable type.

### 4.6 Album archive (multi-part)

- `GET /s/{code}/archive` → manifest:
  ```json
  { "parts": [ { "index": 0, "name": "Summer-2025-part-1.zip",
                 "estimatedSize": 500000000, "assetIds": ["…"] } ] }
  ```
  Zero archives → `404`. `assetIds` come from the **same authorized snapshot** as §5.
- `GET /s/{code}/archive/{part}` → stream that part via `DownloadArchive(assetIDs, w)`,
  `Content-Disposition: attachment` (filename from the manifest, sanitized).
- Multiple parts are **not** concatenated; the UI presents each part as its own
  download. Archive streaming is exclusive per share (one at a time, §11).

### 4.7 Idle vs. long transfers

The on-demand port closes on an idle deadline (`idleAt`), while `DirectServer` currently
records activity only at request start. Change: each streaming response takes a
**begin/end active-transfer hold** on the port (a per-share in-flight counter). The
idle deadline is **not evaluated while any hold is active** — a large
asset/archive/video lasting past the idle timeout does not get its mapping closed. Each
HTTP/2 stream holds independently; client disconnect releases the hold. An integration
test transfers longer than the idle timeout (§12.1).

### 4.8 Headers & CSP

- **`Cache-Control: no-store` on everything** (page, items, thumb, preview, asset,
  playback, archive). The share is ephemeral and revocable; no caching is the only
  behavior consistent with revocation.
- `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`.
- **CSP**: `default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self';
  media-src 'self'; connect-src 'self'`. The moved page's **inline `<style>`, inline
  service-worker script, and inline `onclick` must be extracted into static files** so
  no `unsafe-inline` is required.
- `Content-Disposition: attachment` on `/asset/{id}` and `/archive/{part}`; filename via
  `mime.FormatMediaType` with an ASCII-safe fallback and path separators stripped.

## 5. Auth, resolver, and membership

### 5.1 Resolver seam

```
resolve(code) → (ContentSession, error)   // error → 404 (unknown) / 403 (revoked/unsupported)
```

`ContentSession` is an **immutable per-share snapshot** constructed under the daemon
lock, containing: the resolved `ContentBackend` (the §3 client subset, bound to the
share's Immich key), the share's **membership index** (§5.2), download limits
(§11.2), and lifecycle state (active, not revoked, type=gallery). It is a value the
daemon builds atomically and the request holds for its lifetime — no shared mutable
client state (`ValidatePassword`'s mutation of the client is not used in Phase 3; see
§7.2).

### 5.2 Membership index

`{id}` authorization must not re-run `ListGallery` per thumbnail. Each `ContentSession`
carries an **immutable membership snapshot** — the set of asset IDs in the resolved
gallery — with a refresh TTL (or rebuilt on poll). Atomic replacement on rebuild;
requests hold their snapshot for the request lifetime (an asset removed from the share
mid-request may still complete its in-flight stream, but new requests see the new set).
Archive `assetIds` are taken from the same snapshot, so archive and membership are
consistent.

### 5.3 Path constraints

`{code}` matches the control plane's contract: external codes `[A-Za-z0-9_-]{8,128}`,
generated codes `[a-z0-9]{8}`. `{id}` is an Immich asset UUID, validated by strict
regex + length cap; reject encoded slashes, dot segments, and multi-segment paths.
Verify `{id}` ∈ the membership snapshot before streaming.

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
- **Rewrite:** `app.js` (fetch the §4.1 endpoints) and `gallery.js`'s data layer. The
  revised gallery API receives **URL factories directly**: `thumbUrl(id)`,
  `previewUrl(id)`, `assetUrl(id)`, `playbackUrl(id)`, `archiveManifestUrl()`,
  `archivePartUrl(index)` — replacing binary-frame thumbnail batches, `/media/{id}`
  video, and binary preview payloads. Thumbnails become `<img src="…/thumb/{id}"
  loading="lazy">`. Inline style/handlers are moved to static files (§4.8 CSP).
- **Delete (transport):** `directChannel.js`, `secureRelayChannel.js`,
  `connectTransferChannel.js`, `binaryEnvelope.js`, `frame.js`, `channelSet.js`,
  `laneScheduler.js`, `multiLaneProtocol.js`, `downloadPipeline.js`, `downloadSinks.js`,
  `downloadCapabilities.js`, `sw.js` (media-bridge SW), streamsaver vendor
  (`streamsaver*.js`, `*-mitm.html`), `noise-p256/`.

**Stays with the control plane:** the four retained control-hosted pages — `home.html`
(marketing), `login.html`, `register.html`, `account.html` — see §9 for the deploy change.

## 7. Share → content binding (frozen) + protected shares

### 7.1 Frozen discovery/registration

Unchanged from v1: the agent polls Immich shared-links (`PollShares`), discovers shares,
and registers a matching ShareBridge share with the control plane (`RegisterShare` →
code + control-allocated origin). The `code → Immich shareKey` mapping stays
agent-local, maintained by the daemon's polling/registration flow. **Untouched wire
protocol:** `agent/internal/immich/client.go` and `agent/internal/cloudwebdav/client.go`
make the same HTTP requests to Immich/WebDAV as today (§3.1 is additive Go helpers
only).

### 7.2 Password-protected shares — deferred (explicit, all entry points)

Phase 3 **does not register or serve password-protected shares**. This must be enforced
at **all three entry points**, not just the poller: (1) the Immich poller, (2) manual
`immich://` share creation, and (3) persisted-session restoration on restart — reject/
remove protected sessions at each, and clean up any already-persisted protected
sessions and their control registrations. The future direct flow (password submission
endpoint, rate-limited attempts, an HttpOnly/Secure/SameSite scoped session cookie,
per-auth-session backend credentials) reuses the §5 seam; it is **not** implemented now.

## 8. Canonical-route cutover (3a)

Today the control plane routes Immich links through the v1 path: `/share/{code}` →
`serveShareRedirect` → `/i/{code}` (Immich) or `/s/{code}` (other); `/i/{key}` serves
`web/index.html`; `/join` serves it too; `/ws/client` + `/ws/relay` are the v1 sockets.

Phase 3a changes this so **gallery shares reach the agent's direct server**:

- `/share/{code}` → resolve the session → `302` to canonical `/s/{code}` (direct) for
  **active gallery shares**. Non-gallery (WebDAV/file), relay-only, expired, revoked, or
  protected sessions → a **clean explicit response** (not a silent redirect into a
  resolver that rejects them): relay-only/protected/WebDAV → `410 Gone` (unsupported);
  expired → `410`; revoked/unknown → `404`. The Phase 2 runtime-flow error handling
  (`docs/superpowers/specs/2026-08-15-phase2-direct-mode-transport-design.md` §5) still
  governs the open-signal/redirect for the direct case.
- `DefaultRelayOnly` flips to `false` so new registrations are direct by default.
- `/i/{key}`, `/join`, `/ws/client`, `/ws/relay`, `/sw.js`, `/app.js`, `/src/{…}`,
  `/noise-p256/{…}` are retired in 3c.
- Route tests cover: active gallery (→ direct), relay-only (→ 410), WebDAV/file (→ 410),
  protected (→ 410), expired (→ 410), revoked (→ 404).

## 9. `control/` rename (3b)

Mechanical, no functional change:
- Directory `signaling-server/` → `control/`; module `sharebridge/server` →
  `sharebridge/control` (`go.mod` + every import path).
- `deploy-testing.sh` moves to `control/deploy-testing.sh`. Its `scp web/` step is
  **reduced, not removed**: the control server still serves the **four retained
  control-hosted pages** (`home.html`, `login.html`, `register.html`, `account.html`)
  from `./web/` at runtime, so the script keeps copying **those pages only** (the
  recipient UI + transport assets left the tree in 3a).
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

## 11. Resource limits, concurrency, and download accounting

- `http.Server`: explicit `ReadHeaderTimeout`, `IdleTimeout`, `MaxHeaderBytes`.
- Per-share concurrency: a semaphore limiting simultaneous streaming responses (assets,
  video, archives) per share; global cap across shares. Over limit → `429` (transient)
  or `503` with a bounded-retry hint. Archive streaming is **exclusive** per share.
- Cancellation: propagate request `ctx` into every immich call; abort upstream on client
  disconnect (releases the §4.7 hold).

### 11.1 Download accounting (defined)

- **What counts:** a completed **original-asset download** (`/asset/{id}`) and a
  completed **album archive** (the whole multi-part album, once). Thumbnails, previews,
  and video playback do **not** count.
- **Multi-part album = one download**, counted only when the **last** part completes.
  The agent tracks per-share part-completion state (which parts of the current archive
  have been fetched) to infer "all parts done"; a manifest refetch or share re-open
  resets the window.
- **Concurrency-safe:** atomic reserve at request start, commit on successful
  completion, rollback on failure/cancel. A check-then-stream must not let two
  concurrent requests both pass the limit — reserve the slot before streaming.
- **Persistence:** mirror v1 (`MaxDownloads` decremented, `DownloadComplete` byte
  report). When the limit reaches zero, further downloads → `403`/`410`.
- **Source of the limit:** discovered Immich shares must get an **explicit default
  `MaxDownloads` at registration** (today polled sessions leave it unset); manual
  creation keeps its existing `maxDownloads` parameter.

## 12. Testing

### 12.1 Matrix

- **Unit** — Range parsing (single/suffix/multiple/invalid/`start≥total`/unknown-total/
  zero-length/overflow → `206`/`416`/`200`), HEAD (no body, omitted length), typed-error
  mapping (`404`/`403`/`502`/`503`), path-traversal/encoded-separator rejection,
  `Content-Disposition` sanitization, headers (no-store/nosniff/CSP/Referrer-Policy),
  resolver/membership lifecycle (snapshot immutability, TTL rebuild, concurrent
  polling), download accounting (reserve/commit/rollback, multi-part-once).
- **Integration** — real agent stack (cert.Manager + Binder + DirectServer +
  OnDemandPort + a fake `ContentBackend`, mirroring `6233e10`): SNI admission → gallery
  HTML → items → thumb → preview → asset → playback (206) → archive manifest → part.
  Plus: transfer-longer-than-idle-timeout does **not** close the mapping; concurrent
  recipients; cancellation; overload → `429`/`503`.
- **UI** — port `gallery.test.js` to the URL-factory data layer (item render, lightbox,
  download/album-download against a stubbed `fetch`, archive manifest + part wiring).
- **Route cutover** — `/share/{code}`: active gallery → direct; relay-only/WebDAV/
  protected/expired → `410`; revoked → `404`.
- **Protected-share enforcement** — all three entry points (poller, manual, restore).

### 12.2 Parity gate (blocks 3c)

An automated end-to-end suite must pass before the v1 transport is deleted. It covers
the **full 3a test suite** plus a **browser-level test** that, over the direct path with
a test CA and a fake backend, actually: opens the gallery page, lazy-loads a thumbnail,
opens a lightbox preview, **seeks a video** (multiple `206` ranges), downloads an
original asset, and enumerates/downloads **every** archive part — and asserts the
**unsupported-share migration outcomes** (relay-only/WebDAV/protected → `410`, not a
`500`). This is the "recipient parity" evidence that gates milestone 3c.

## 13. Migration / rollout

- No schema migration (content serving is agent-local; no new collections).
- 3a is additive to Phase 2's transport; the v1 transport stays wired (unused) until the
  gated 3c cleanup, so every plan step keeps `go build ./...` green in isolation.
- Live e2e (optional, manual): `control/deploy-testing.sh --bootstrap` on a Hetzner box
  + agent against a real Immich instance; verify a real share serves thumbnails,
  preview, an original, and a transcoded video over the direct path.
