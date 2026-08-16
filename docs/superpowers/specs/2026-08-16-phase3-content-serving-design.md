# Phase 3 — Content Serving (Direct HTTP) + `control/` Rename

**Date:** 2026-08-16
**Status:** Draft — revision 6 (incorporates design review round 5)
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
rejected at registration and return a clean "unsupported" response when served.
`DefaultRelayOnly` is **forced to `false`** in 3a (persisted `true` overridden). This is
intentional and asserted by the parity gate (§12.2).

**Out of scope (deferred):**
- WebDAV / file shares (drops in later on the same serving layer; the file-browser half of the UI stays as reference).
- **Password-protected Immich shares** (§7.2) and **private/authenticated shares** (GitHub OAuth). The auth seam (§5) is designed so both slot in later without rework.
- **Large-album pagination** (§4.3): `ListGallery` eagerly loads all pages; Phase 3 serves from a snapshot and documents the limit.
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
   the direct path; the v1 `/i/{key}`, `/join`, `/ws/client`, `/ws/relay`, `/sessions/{code}`,
   and web-asset routes are retired (in 3c). `DefaultRelayOnly` flips to `false`.
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
| `PollShares(ctx)` | discover shared links |

Package-level discovery helpers on the `SharedLink` value (not `Client` methods):
`SharedLink.IsPasswordProtected()`, and `Client.ValidatePassword(ctx, password)`.

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

1. **Typed error classification** — applied to **every** client HTTP path that currently
   flattens status into a string (`getAsset`, `doJSON`, `doJSONStatus`,
   `HeadVideoPlayback`, `GetAlbumDownloadInfo`, `DownloadArchive`, thumbnail/preview
   fetches): `NotFoundError`, `AuthError`, `UpstreamError{Status}`. Exposed via
   `errors.As`/`errors.Is`.
2. **Metadata access before streaming** — helpers returning an asset kind's
   `Content-Type` and authoritative length **when the upstream provides it** (HEAD
   request or response headers), e.g. `ThumbnailInfo(id)` / `PreviewInfo(id)` /
   `PlaybackInfo(id)`. Playback metadata returns `(length int64, known bool)` so
   "unknown" is distinguishable from an authoritative zero. Where Immich omits the
   length, `known=false` (never fabricate a length).

## 4. HTTP serving layer

### 4.1 Endpoints (all under `https://<origin>/s/{code}/…`)

| Method | Path | Serves | Backing |
|---|---|---|---|
| GET/HEAD | `/s/{code}` | gallery HTML (embedded UI) | — |
| GET/HEAD | `/s/{code}/items` | album + items (JSON, §4.3) | snapshot (§5.2) |
| GET/HEAD | `/s/{code}/thumb/{id}` | thumbnail | `GetThumbnail` |
| GET/HEAD | `/s/{code}/preview/{id}` | lightbox preview | `GetPreview` |
| GET/HEAD | `/s/{code}/asset/{id}` | original asset (download) | `GetFile` |
| GET/HEAD | `/s/{code}/asset/{id}/playback` | transcoded video (Range), `video/mp4` | `GetVideoPlayback`/`GetVideoPlaybackRange` + `HeadVideoPlayback` |
| GET/HEAD | `/s/{code}/archive` | archive manifest + transaction token (JSON, §4.6) | `GetAlbumDownloadInfo` |
| GET/HEAD | `/s/{code}/archive/{token}/{part}` | one archive ZIP (attachment) | `DownloadArchive` |

### 4.2 Range (transcoded video only)

- Parse a **single** `bytes=start-end` / `bytes=start-` range (case/whitespace-tolerant).
  Suffix ranges (`bytes=-N`), multiple ranges, non-numeric, or `start ≥ total` → `416
  Range Not Satisfiable` with `Content-Range: bytes */<total>`.
- **Unknown total** (`PlaybackInfo` → `known=false`): **ignore all `Range` headers** and
  serve `200` + full body via `GetVideoPlayback` (no `Accept-Ranges` advertised) — never
  fabricate a `206`, and never emit a `416` whose `*/<total>` can't be populated.
- **Authoritative zero** (`known=true, length=0`): `416`.
- Valid range → `206 Partial Content`, `Content-Range: bytes start-end/total`,
  `Accept-Ranges: bytes`. `end` is clamped to `total-1`; `start > end` after clamping and
  integer-overflow inputs → `416`.
- `start-end` reads use `GetVideoPlaybackRange` with a **bounded writer** that stops the
  upstream read at `end` (the "hit the bound" sentinel must not surface as an error).
  `start=0` sends no upstream Range header (client behavior) — a downstream `206` is
  synthesized deliberately.
- **Original assets and all non-video resources do not advertise `Accept-Ranges`.**

### 4.3 `items` JSON

`/items` returns the **snapshot's** gallery as a **normative lowerCamel wire schema**
(the agent marshals into an explicit DTO — `immich.Gallery`'s raw struct lacks JSON tags):

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

Empty album → `200` with `items: []`. `duration` is float seconds or `null`. **Large-
album pagination is deferred**; the plan documents the eager-load memory/latency
behavior and the UI's 120-item "load-more" windowing.

### 4.4 HEAD

HEAD returns **status + headers only, no body**, derived from metadata (§3.1) and
membership (§5) — **not** by invoking the streaming GET path. Where metadata is
unavailable (thumbnails/previews/archives), `Content-Length` is **omitted** and the
handler reports existence via `200`/`404` only. HEAD does **not** promise byte-identical
headers to GET for streaming endpoints, and **ignores `Range`** (returns `200` +
resource headers; no `206`/`Content-Range` even for playback). Upstream failure during
metadata lookup → `502`/`503` (§4.5).

### 4.5 Errors and response commitment

- **Preflight before streaming**: resolve + membership-check + metadata lookup first;
  only then stream. Unknown asset → `404` before any bytes are written.
- **Delayed status commit**: handlers buffer the status until the **first body write**.
  An error returned before any bytes are written (e.g. an upstream `404` on the actual
  fetch, discovered only after metadata) still sets the correct status — no
  already-committed `200`.
- **Typed error mapping** (§3.1), exact and deterministic: upstream `401`/`403` → `403`;
  `404` → `404`; any other upstream HTTP status → `502`; timeout / unreachable → `503`.
- **Mid-stream failure** (after the first body byte): log + abort via
  `panic(http.ErrAbortHandler)` — aborts the HTTP/2 stream and terminates the HTTP/1.1
  response (no hijacking). Truncation is only detectable where `Content-Length` was
  authoritative (transcoded video); thumbnails/previews/archives may have no
  authoritative length, so their truncation is not guaranteed-detectable — documented,
  not promised.
- Fallback MIME: `application/octet-stream` when metadata has no usable type.

### 4.6 Album archive (multi-part, transactional)

`GET /s/{code}/archive` creates an in-memory **album-download transaction** (opaque
random token, TTL — default 1h) and returns:

```json
{ "token": "<opaque>",
  "parts": [ { "index": 0, "name": "Summer-2025-part-1.zip",
               "estimatedSize": 500000000, "assetIds": ["…"] } ] }
```

- Zero archives → `404`. The archive info is **deep-copied** from `GetAlbumDownloadInfo`
  and **every `assetIds` element is verified against the membership snapshot** (§5.2);
  any mismatch → `403` (reject the whole manifest). Duplicate IDs → `403`.
- `GET /s/{code}/archive/{token}/{part}` → stream that part via `DownloadArchive` with
  `Content-Disposition: attachment` (filename from the manifest, sanitized). The token
  binds the part to its transaction. Missing part index → `404`; duplicate part fetch →
  idempotent (re-stream, no double-count).
- **Transaction state machine** (atomic, under the ledger lock): a transaction is
  `open` (holds its reservation) → `committed` (all parts done → one download) or
  `released` (no count). **TTL expiry targets only idle transactions** — an in-flight
  part **pins** its transaction (renews the TTL), and the pin is taken/released under
  the lock so expiry and streaming are mutually exclusive. A released reservation can
  never commit: if a transaction is released (expiry/invalidation) while a part is still
  streaming, that stream is aborted and its commit is a no-op. Transactions are
  in-memory — an agent restart drops uncommitted transactions (no count).
- **Generation binding**: each transaction is bound to its snapshot generation. Before
  streaming a part, revalidate the part's copied `assetIds` against the **current**
  snapshot; on a membership change (new generation), invalidate incomplete transactions —
  atomically cancel any active stream, release the reservation, and `403` further part
  fetches.
- Multiple parts are **not** concatenated; the UI presents each part as its own
  download. Archive streaming is exclusive per transaction (§11).

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
- **CSP**: `default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline';
  img-src 'self'; media-src 'self'; connect-src 'self'`. `script-src` stays strict (no
  inline scripts — move the inline service-worker script and `onclick` handlers into
  static files), but `style-src` allows inline style *attributes* because the reused
  lightGallery runtime and `gallery.js`'s progress bar set `style`/`style.cssText` at
  runtime (CSS cannot execute script; the residual CSS-injection risk requires a separate
  injection vulnerability and is accepted this phase). The page's large inline `<style>`
  block is still moved to a static file, and the `data:image/gif` placeholder is replaced
  with a code-prefixed embedded static asset. A browser test asserts zero CSP violations
  across gallery load, lightbox open, video slide, zoom, and navigation.
- `Content-Disposition: attachment` on `/asset/{id}` and `/archive/{token}/{part}`;
  filename via `mime.FormatMediaType` with an ASCII-safe fallback and path separators
  stripped.

## 5. Auth, resolver, membership, and snapshot

### 5.1 Resolver seam

```
resolve(code) → (ContentSession, error)   // error → 404 (unknown) / 403 (membership/limit/type) / 503 (not yet hydrated)
```

`ContentSession` is an **immutable per-share snapshot** constructed atomically under the
daemon lock, containing: the resolved `ContentBackend` (the §3 client subset bound to
the share's Immich key), the **complete gallery snapshot** (DTO + membership index +
generation/timestamp, §5.2), the download **limit** (immutable), and lifecycle state
(active, not revoked, type=gallery). The mutable download **count + active reservations
live in a separate, locked per-session accounting ledger** (§11.1) — not in this
immutable snapshot. The snapshot is a value the request holds for its lifetime — no
shared mutable client state (`ValidatePassword`'s mutation of the client is not used in
Phase 3; see §7.2).

### 5.2 Snapshot & membership (single source of truth)

- The snapshot contains the **complete gallery DTO + membership index** (the set of asset
  IDs) **+ a generation/timestamp**. `/items`, membership checks, and archive manifests
  all read the **same snapshot** — never a live, separate `ListGallery` re-run — so they
  are mutually consistent.
- **Refresh** is driven by the existing Immich poll cycle: rebuild the snapshot by
  fetching **outside** the lock (network I/O), then **atomically swap** it. Concurrent
  rebuilds are **singleflight**-deduplicated. **One per-share state lock** guards the
  snapshot generation, membership, manifest validation/recheck, transaction insertion,
  and refresh-driven invalidation — so the §11 singleflight recheck and refresh-driven
  invalidation are atomic with the swap (no split daemon/ledger locks).
- **Generation semantics**: a **refresh timestamp** advances every successful poll, but a
  **content/membership generation** advances only when the gallery DTO/membership
  actually changes. Archive transactions and the §11 singleflight cache invalidate on the
  **membership generation** only — a no-op refresh does not cancel long-running archives.
- **Stale / fail-closed policy**: on poll failure, keep serving the last snapshot; but if
  refresh fails for longer than a bound (2× the poll interval), **fail closed** — reject
  new content requests (`503`) rather than indefinitely authorizing a stale membership.
  Requests already holding a snapshot finish their in-flight streams.
- **Initial hydration**: snapshots are in-memory, not persisted. On new-share or restart,
  a share is **not content-ready** until its first snapshot succeeds; until then
  `resolve()` returns `503` (unready). The origin binding may exist first, but content
  requests fail `503` until hydration. The 2×poll stale timer starts at the first
  successful hydration. Cover new-registration, restart, and initial-fetch failure.

### 5.3 Path constraints

`{code}` matches the control plane's contract: external codes `[A-Za-z0-9_-]{8,128}`,
generated codes `[a-z0-9]{8}`. `{id}` is an Immich asset UUID, validated by strict regex
+ length cap; reject encoded slashes, dot segments, and multi-segment paths. Verify
`{id}` ∈ the membership snapshot before streaming. The archive `{token}` is opaque
random, validated by length/charset.

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
  `archivePartUrl(token, index)` — replacing binary-frame thumbnail batches, `/media/{id}`
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
make the same HTTP requests to Immich/WebDAV as today (§3.1 is additive Go helpers only).

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
`web/index.html`; `/join` serves it too; `/ws/client` + `/ws/relay` are the v1 sockets;
`/sessions/{code}` exposes session info.

Phase 3a changes this so **gallery shares reach the agent's direct server**:

- `/share/{code}` **and control-hosted `/s/{code}`** → resolve the session **including
  inactive tombstones** → `302` to canonical `/s/{code}` (direct) for **active gallery
  shares**. All other cases return a
  clean explicit status driven by a single lifecycle discriminator
  **`inactive_reason ∈ {expired, revoked, unsupported}`** (§10): `revoked` → `404`;
  `expired` → `410`; `unsupported` (relay-only / WebDAV/file / protected) → `410`;
  unknown code → `404`. The discriminator makes revoked-vs-expired unambiguous (both set
  `is_active=false` today). The Phase 2 runtime-flow error handling
  (`docs/superpowers/specs/2026-08-15-phase2-direct-mode-transport-design.md` §5) still
  governs the open-signal/redirect for the direct case.
- The **direct-origin** path is TLS-gated: only **active gallery shares** have a live
  origin binding + snapshot. Revoked/expired/unsupported bindings are removed, so those
  requests **fail at TLS admission** and never reach HTTP. The HTTP resolver's statuses
  (`404` unknown code / `403` membership-or-limit failure) therefore apply only to
  requests that *reach* the resolver.
- **Unsupported registration is rejected at every entry point** (poller, manual
  creation, persisted-session restoration, and control `register_share`): relay-only,
  WebDAV/file, and protected shares are all rejected. **Config migration** forces
  `DefaultRelayOnly` to `false` on upgrade (persisted `true` overridden); existing v1
  sessions of unsupported types become `inactive_reason=unsupported` tombstones.
- `/i/{key}`, `/join`, `/ws/client`, `/ws/relay`, `/sessions/{code}`, `/sw.js`,
  `/app.js`, `/src/{…}`, `/noise-p256/{…}` are retired in 3c.
- Route tests cover: active gallery (→ direct), relay-only (→ `410`), WebDAV/file
  (→ `410`), protected (→ `410`), expired (→ `410`), revoked (→ `404`).

## 9. `control/` rename (3b)

Mechanical, no functional change:
- Directory `signaling-server/` → `control/`; module `sharebridge/server` →
  `sharebridge/control` (`go.mod` + every import path).
- `deploy-testing.sh` moves to `control/deploy-testing.sh`. Its `scp web/` step is
  **reduced, not removed**: the control server still serves the **four retained
  control-hosted pages** (`home.html`, `login.html`, `register.html`, `account.html`)
  from `./web/` at runtime, so the script keeps copying **those pages only** (the
  recipient UI was **copied/adapted into the agent** in 3a; its control-tree source is
  deleted in 3c).
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
| control | `internal/relay`, `internal/handler/relay_ws.go`, `internal/handler/browser_ws.go`, TURN/ICE wiring, `/ws/client` + `/ws/relay` + `/i/{key}` + `/join` + `/sessions/{code}` + web-asset routes, relay *runtime* config fields |
| repo | `extensions/`, `scripts/update_streamsaver_vendor.sh` |
| client JS | §6 "Delete" list |

The deletion is not a simple tree removal — `cmd/server/main.go`, `agent_ws.go`, and
`daemon.go` deeply wire the v1 packages (peer/multilane/relay/TURN). The plan must
enumerate every route, handler, config field, WS message type, daemon field/function,
and test to remove, so each step keeps `go build ./...` + `go test ./...` green. Remove
transport message handling from the kept `agent/internal/signaling` client.

**Discriminator fields survive 3c.** The session lifecycle adds an **`inactive_reason`
field** (`expired | revoked | unsupported`) so the canonical route can distinguish
revoked (`404`) from expired/unsupported (`410`) — today both just set `is_active=false`.
The classifier fields (`relay_only`, `share_type`, `is_password_protected`, `expires_at`,
`is_active`) are **kept** through 3c for diagnostics; `inactive_reason` is authoritative
for the status.

**Transitions** (every mutation site writes the discriminator): a new or reactivated
session **clears** `inactive_reason`; expiry writes `expired`; explicit removal/
revocation writes `revoked`; unsupported migration writes `unsupported`. A missing or
unrecognized `inactive_reason` on an inactive row **fails safe as `404`**. Backfill
ordering: mark `unsupported` (relay-only/WebDAV/protected) first, then assign `expired`
(past `expires_at`) / `revoked` (remaining) to the rest.

Only the relay *runtime* (sockets, handler, TURN) is deleted.

**Kept (frozen):** `internal/immich`, `internal/cloudwebdav`, `internal/signaling`,
`internal/direct`, `internal/cert`, `internal/daemon` (minus the v1 wiring),
`internal/store`, `internal/web` (agent admin UI), `internal/config`.

## 11. Resource limits, concurrency, and download accounting

- `http.Server`: explicit `ReadHeaderTimeout`, `IdleTimeout`, `MaxHeaderBytes`.
- **Streaming responses** (assets, video, archive parts) are semaphore-limited per share
  + globally; over limit → `429` (transient) / `503` with a bounded-retry hint.
- **Non-streaming upstream work is bounded separately**: `/items` is served **from the
  snapshot** (no per-request upstream call); HEAD metadata probes run behind
  per-share/global semaphores. `/archive` singleflights **only** the upstream
  `GetAlbumDownloadInfo` fetch + validation into a shared immutable manifest template
  (keyed by share + snapshot generation); each GET then creates its own **fresh token +
  transaction + reservation** from that template (HEAD creates neither). **After the
  shared fetch returns, re-check the template's membership generation against the
  current snapshot under the per-share state lock; if it advanced, discard and retry
  against the new generation** (test: force a swap between comparison and transaction
  creation).
- Cancellation: propagate request `ctx` into every immich call; abort upstream on client
  disconnect (releases the §4.7 hold).

### 11.1 Download accounting (defined)

- **What counts** (committed downloads): a completed **original-asset download**
  (`/asset/{id}`) and a completed **album archive transaction** (all parts, once).
  Thumbnails, previews, and video playback do **not** count.
- **Model**: `MaxDownloads` is an **immutable per-share limit**; `Downloads` is the
  **persisted committed count** (`store.IncrementDownloads`, as in v1). A **locked
  per-session accounting ledger** (separate from the immutable gallery snapshot) tracks
  `Downloads` + active reservations.
- **Concurrency-safe**: an atomic `TryReserve` on the ledger enforces
  `Downloads + activeReservations < MaxDownloads` **when `MaxDownloads > 0`**.
  `MaxDownloads <= 0` means **unlimited** — admission is always granted, but the ledger
  still tracks reservations + idempotent commit for consistency. A finite limit
  exhausted by reservation → `403`. Assets reserve immediately before streaming; albums
  reserve after manifest validation but **before the token is returned**. Commit
  (increment `Downloads`) on successful completion; release on failure / cancel /
  TTL-expiry / invalidation. Commit is idempotent. Two concurrent requests resolving at
  `Downloads == MaxDownloads-1` cannot both pass. Tests: zero/unlimited, reservation
  denial, mixed concurrent asset/album at `MaxDownloads-1`.
- **Persistence**: the committed increment is durable; a persistence failure is logged
  and the in-memory count still enforces the limit for the session's lifetime
  (documented gap). **Byte-level reporting is out of scope** — v1's
  `DownloadComplete` byte report is already a no-op at the control plane (no handler);
  Phase 3 does not reintroduce it.
- **Source of the limit**: discovered Immich shares get an **explicit default
  `MaxDownloads` from config at registration** (today polled shares leave it unset);
  manual Immich creation must **plumb** its `maxDownloads` + expiry through the Immich
  path (today they are discarded — fix that).

## 12. Testing

### 12.1 Matrix

- **Unit** — Range parsing (single/suffix/multiple/invalid/`start≥total`/unknown-total/
  zero-length/overflow → `206`/`416`/`200`-ignore), HEAD (no body, omitted length),
  typed-error mapping (exact `403`/`404`/`502`/`503`), delayed status commit
  (pre-first-byte error sets the right status), path-traversal/encoded-separator
  rejection, `Content-Disposition` sanitization, headers (no-store/nosniff/CSP/
  Referrer-Policy), snapshot/membership lifecycle (immutability, atomic swap,
  singleflight, fail-closed after refresh failure, initial-hydration `503`), download
  accounting (reserve/commit/rollback, album-transaction once, duplicate-part
  idempotency, abandoned-transaction expiry, mixed concurrent asset/album at
  `MaxDownloads-1`, zero/unlimited limit, **TTL firing during an active part stream**),
  HEAD+Range (ignored), **manifest generation changing during the singleflight fetch**.
- **Integration** — real agent stack (cert.Manager + Binder + DirectServer +
  OnDemandPort + a fake `ContentBackend`, mirroring `6233e10`): SNI admission → gallery
  HTML → items (from snapshot) → thumb → preview → asset → playback (206) → archive
  manifest (token) → part. Plus: transfer-longer-than-idle-timeout does **not** close
  the mapping; concurrent recipients; cancellation; overload → `429`/`503`.
- **UI** — port `gallery.test.js` to the URL-factory data layer (item render, lightbox,
  download/album-download against a stubbed `fetch`, archive manifest + `archivePartUrl`),
  plus a browser test asserting **zero CSP violations** across gallery load, lightbox
  open, video slide, zoom, and navigation.
- **Route cutover** — `/share/{code}` **and** control `/s/{code}` (same resolver):
  active gallery → direct; relay-only/WebDAV/protected/expired → `410`; revoked → `404`
  (including inactive-tombstone lookup); lifecycle transitions (new→clear, expire,
  revoke, unsupported-migrate) and invalid/missing `inactive_reason` → `404`.
- **Unsupported-share enforcement** — protected, relay-only, and WebDAV shares rejected
  at all entry points (poller, manual, restore, control registration) + config migration
  forcing `DefaultRelayOnly=false`.

### 12.2 Parity gate (blocks 3c)

An automated end-to-end suite must pass before the v1 transport is deleted. It covers
the **full 3a test suite** plus a **browser-level test** that, over the direct path with
a test CA and a fake backend, actually: opens the gallery page, lazy-loads a thumbnail,
opens a lightbox preview, **seeks a video** (multiple `206` ranges), downloads an
original asset, and enumerates/downloads **every** archive part — and asserts the
**unsupported-share migration outcomes** (relay-only/WebDAV/protected → `410`, not a
`500`). This is the "recipient parity" evidence that gates milestone 3c.

## 13. Migration / rollout

- One forward migration in 3a: add `sessions.inactive_reason`
  (`expired|revoked|unsupported`) + backfill (§10). No other schema change. Manual
  Immich creation must start plumbing `maxDownloads`/expiry (a code fix, not a schema
  change).
- 3a is additive to Phase 2's transport; the v1 transport stays wired (unused) until the
  gated 3c cleanup, so every plan step keeps `go build ./...` green in isolation.
- Live e2e (optional, manual): `control/deploy-testing.sh --bootstrap` on a Hetzner box
  + agent against a real Immich instance; verify a real share serves thumbnails,
  preview, an original, and a transcoded video over the direct path.
