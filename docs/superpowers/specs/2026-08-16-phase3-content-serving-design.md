# Phase 3 — Content Serving (Direct HTTP) + `control/` Rename

**Date:** 2026-08-16
**Status:** Draft (pending review)
**Companion to:** `docs/superpowers/specs/2026-08-15-phase2-direct-mode-transport-design.md` (the transport this phase builds on)
**Forks off:** `v2`

## 1. Summary

Phase 2 proved the direct transport end-to-end (native HTTPS terminating at the
agent, real cert, on-demand port) but served only a **placeholder** page. Phase 3
replaces the placeholder with **real content serving**: the agent's direct server
serves Immich shares over native HTTP — a gallery page, thumbnails, full assets
(original + transcoded video), and album downloads — reusing the recipient UI that
the signaling server already serves today. It also renames `signaling-server/` →
`control/` and deletes the v1 WebRTC/relay/multilane transport once it is superseded.

**In scope:**
- Direct-HTTP content serving for Immich (gallery) shares on the agent.
- Move the recipient UI (`signaling-server/web/` gallery half) to the agent, embedded.
- The `control/` rename (`signaling-server/` → `control/`, module `sharebridge/server` → `sharebridge/control`).
- Delete the v1 transport (agent + server + extensions + client JS), after content serving reaches parity.

**Out of scope (deferred):**
- WebDAV / file shares (drops in later on the same serving layer; the file-browser half of the UI stays as reference).
- Private / authenticated shares (e.g. GitHub OAuth → short-lived bearer token). The direct server's auth is kept a single seam so this slots in later (§3.4).
- Relay fallback / FRP tunnel + measure-and-prefer → **Phase 4**.
- Any UI framework rewrite — the existing vanilla-JS lightGallery UI is kept (lean, no build step).

## 2. Architecture & Components

Three changes: the agent's `DirectServer` grows an HTTP content layer; the recipient
UI relocates from the control plane to the agent; and the v1 transport is deleted.
The share→content binding is **frozen** (§5).

### Agent

1. **`direct.DirectServer` content layer** — replaces the placeholder `handlePage`/
   `handleDownload`. The `/s/{code}` route dispatches to content handlers that call
   the existing `immich.Client` directly (§3).
2. **Embedded recipient UI** — `index.html` + `gallery.js` + lightGallery vendor +
   `videoBufferWarning.js`, served at `/s/{code}` (§4).
3. **Share resolver** — a provider interface the DirectServer uses to map a code to
   the corresponding Immich gallery backend (the auth seam, §3.4).

### Control plane

4. **Rename only** — `signaling-server/` → `control/`; no functional change (§6).
   The recipient UI and the v1 relay handler leave this tree.

### Deleted (end of phase)

5. The v1 transport stack (§7): agent `transfer`/`multilane`/`relaychannel`/`peer`/
   `noise`, server `internal/relay` + `relay_ws.go`, `extensions/`, and the client JS
   transport (`*Protocol.js`, `frame.js`, `channelSet.js`, `laneScheduler.js`,
   `directChannel.js`, `secureRelayChannel.js`, `connectTransferChannel.js`,
   `binaryEnvelope.js`, `downloadPipeline.js`, `downloadSinks.js`,
   `downloadCapabilities.js`, `sw.js`, streamsaver vendor, `noise-p256/`).

## 3. HTTP serving layer

### 3.1 Endpoints (all under the direct origin, `https://<origin>/s/{code}/…`)

| Method | Path | Serves | Immich client method |
|---|---|---|---|
| GET | `/s/{code}` | gallery HTML (embedded UI) | — |
| GET | `/s/{code}/items` | album + item metadata (JSON) | `ListGallery` |
| GET | `/s/{code}/thumb/{id}` | thumbnail image | `GetThumbnail` |
| GET | `/s/{code}/asset/{id}` | original asset (image/video), Range | `GetAsset` / `GetAssetRange` |
| GET | `/s/{code}/asset/{id}/playback` | transcoded video, Range/HLS | `HeadVideoPlayback` / `GetAssetRange` |
| GET | `/s/{code}/archive` | album zip (attachment) | `GetAlbumDownload` / `StreamAlbumArchive` |

`GetAssetInfo` supplies each asset's name/size/mimeType for `Content-Type` and
`Content-Length`. The immich client's methods already write to an `io.Writer`, so
handlers stream straight into the `http.ResponseWriter` — no buffering.

### 3.2 Range & streaming

- `GET /s/{code}/asset/{id}` and `/asset/{id}/playback` honor the `Range` header:
  parse `bytes=start-`/`bytes=start-end`, respond `206 Partial Content` with
  `Content-Range: bytes start-end/total` and `Accept-Ranges: bytes`; `200` + full
  body when no Range. `GetAssetRange(ctx, id, quality, startOffset, w)` is the
  offset-based read; `HeadVideoPlayback` provides the transcoded total length.
- HLS (when Immich returns it) is passed through as-is: `.m3u8` and segment fetches
  are just proxied responses.

### 3.3 Headers

- Gallery page + `/items`: `Cache-Control: no-store` (ephemeral share).
- Thumbnails/assets: correct `Content-Type` (from `GetAssetInfo`/thumbnail), `private`
  caching; `Content-Disposition: attachment` on `/archive` and on per-item downloads.

### 3.4 Auth seam

The DirectServer resolves a code through a single seam:

```
resolve(request) → (share, galleryBackend, err)
```

Today the code in the URL satisfies it; the backend is the agent-local `immich.Client`
for that share. Unknown / revoked / non-gallery codes → `404`/`403`. This seam is the
one extension point for **private shares** later (a bearer token minted by the control
plane after OAuth would resolve the same way). No per-request token is added now — the
on-demand port is the ephemerality, the share code is the credential.

## 4. Recipient UI — move, reuse, rewrite, delete

The recipient experience today is `signaling-server/web/index.html` + `src/app.js` +
`src/gallery.js` (+ lightGallery vendor + `sw.js` + the transport modules). It relocates
to the agent, embedded via `go:embed` and served by `DirectServer`.

**Move + reuse (as-is or lightly edited):**
- `index.html` — the gallery + file-browser DOM/CSS (file-browser half is reference for the WebDAV phase).
- `src/gallery.js` — the lightGallery controller (lightbox, zoom/swipe, video playback, per-item download, album-download, load-more). Decoupled from transport via injected callbacks (`onThumbnailBatchRequest`, `onPreviewRequest`, `onDownloadRequest`, `onAlbumDownloadRequest`, `onPreviewClose`) and zero imports — its presentation logic is reused; only its thumbnail *push-model* internals are simplified below.
- `src/videoBufferWarning.js`.
- `src/vendor/lightgallery/*` (js + css).

**Rewrite (smaller):**
- `src/app.js` — instead of wiring WebRTC/relay/multilane, it `fetch()`es the §3.1
  endpoints and hands URLs to the gallery.
- `gallery.js`'s thumbnail data layer — binary-frame decode → blob-URL becomes a plain
  `<img src="/s/{code}/thumb/{id}" loading="lazy">`; the batching/generation/retry
  machinery is removed (native caching + lazy-loading replace it).

**Delete (transport):** `directChannel.js`, `secureRelayChannel.js`,
`connectTransferChannel.js`, `binaryEnvelope.js`, `frame.js`, `channelSet.js`,
`laneScheduler.js`, `multiLaneProtocol.js`, `downloadPipeline.js`, `downloadSinks.js`,
`downloadCapabilities.js`, `sw.js` (media-bridge SW), streamsaver vendor
(`streamsaver*.js`, `*-mitm.html` — the download SW), `noise-p256/`.

**Stays with the control plane:** `account.html`, `home.html`, `login.html`,
`register.html` (the account-management UI, server-rendered by the control plane).

## 5. Share → content binding (frozen)

Unchanged from v1: the agent polls Immich shared-links (`immich.Client.PollShares`),
discovers shares, and registers a matching ShareBridge share with the control plane
(`RegisterShare` → code + control-allocated origin). The `code → Immich shareKey`
mapping stays agent-local, maintained by the daemon's polling/registration flow.

**Untouched backends:** `agent/internal/immich/client.go` and
`agent/internal/cloudwebdav/client.go` — all agent ↔ Immich/WebDAV HTTP communication
is frozen. Phase 3 only changes *how the bytes reach the recipient's browser*
(multilane binary frames → native HTTP), not how the agent talks to Immich.

## 6. `control/` rename

Mechanical, no functional change:
- Directory `signaling-server/` → `control/`.
- Module `sharebridge/server` → `sharebridge/control` (`go.mod` + every import path).
- Update references: `deploy-testing.sh` (now at `control/deploy-testing.sh`; its
  `scp web/` step is removed since the recipient UI moved to the agent),
  `signaling-server/docs/testing-server.md` (moves/updates), CI workflows, README.
- The v1 relay (`internal/relay`, `handler/relay_ws.go`) is deleted *during* the rename
  (it becomes `control/internal/relay/` and is then removed) — see §7.

## 7. v1 transport deletion (after content parity)

Once the direct-HTTP serving reaches parity, the v1 transport is deleted in one scoped
cleanup. Git history is the reference (the pre-deletion tip is recorded in the plan;
`v2` retains the full v1 stack). No `relay-old`/`relay-v1` copies — FRP (Phase 4) is
architecturally unrelated to the Noise/WebRTC relay, so there is nothing to port.

| Tree | Delete |
|---|---|
| agent | `internal/transfer`, `internal/multilane`, `internal/relaychannel`, `internal/peer`, `internal/noise` |
| control | `internal/relay`, `internal/handler/relay_ws.go` |
| repo | `extensions/` (nextcloud, opencloud) |
| client JS | §4 "Delete" list |

**Kept (frozen):** `internal/immich`, `internal/cloudwebdav` (backends),
`internal/signaling`, `internal/direct`, `internal/cert`, `internal/daemon`,
`internal/store`, `internal/web` (agent admin UI), `internal/config`.

## 8. Testing

- **Unit** — HTTP handlers: Range parsing (`206`/`Content-Range`), content-type,
  `404`/`403` on unknown/revoked codes, `Content-Disposition` on archive, no-store
  headers; the share resolver seam.
- **Integration** — a real agent stack (cert.Manager + Binder + DirectServer +
  OnDemandPort + a fake `immich.Client` backend, mirroring Phase 2's end-to-end agent
  test `6233e10`): SNI admission → gallery HTML → thumbnail → asset (Range) → archive.
- **UI** — port `gallery.test.js` to the HTTP data layer (item render, lightbox,
  download/album-download callbacks against a stubbed `fetch`).
- **Live e2e (optional, manual)** — `control/deploy-testing.sh --bootstrap` on a
  Hetzner box + agent against a real Immich instance; verify a real share serves
  thumbnails, an original, and a transcoded video over the direct path.

## 9. Migration / rollout

- New forward migration: none required (no schema change; content serving is agent-local).
- The recipient UI and content serving are additive to Phase 2's transport; the v1
  transport remains wired (but unused) until the §7 cleanup, so each plan step keeps
  `go build ./...` green in isolation.
