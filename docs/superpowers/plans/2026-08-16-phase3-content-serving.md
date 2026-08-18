# Phase 3 — Content Serving (Direct HTTP) + `control/` Rename — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the Phase-2 placeholder with real Immich gallery content serving over the agent's direct HTTPS path, move the recipient UI to the agent, cut the canonical route over to direct, rename `signaling-server/` → `control/`, and (gated behind automated parity) delete the v1 WebRTC/relay/multilane transport.

**Architecture:** The agent's `direct.DirectServer` grows an HTTP content layer backed by a narrow `ContentBackend` interface (implemented over the frozen `immich.Client`), resolving share codes through an immutable `ContentSession` snapshot (gallery DTO + membership index + generation) held by a `SnapshotManager` registry, plus a per-share state-lock-guarded accounting ledger for download limits. The recipient UI (`index.html` + `gallery.js` + lightGallery) is embedded in the agent and served under `/s/{code}/static/…`; its data layer is rewritten from the binary WebRTC/multilane transport to plain `fetch`/`<img>`/`<video>`.

**Tech Stack:** Go (pure-Go sqlite, no cgo), `net/http` (+`http.ServeMux` with method patterns), `go:embed`, vanilla JS + lightGallery (no build step), PocketBase migrations.

## Global Constraints

- Every task MUST end with `go build ./...` green in the relevant module. There is **no root `go.mod`/`go.work`** — the agent is its own module (`cd agent`), and the control plane is its own module (`cd signaling-server`, later `cd control`). Run the build in the module the task touched.
- Model policy: `deepseek/deepseek-v4-pro` for all implementation subagents; `openai-codex/gpt-5.6-sol` reserved for whole-branch review (per user direction).
- TDD throughout: write the failing test, watch it fail, implement, watch it pass, commit.
- The `immich.Client` wire protocol (endpoints, params, auth, request/response shapes) is **frozen** — Task 1 changes are additive Go helpers only.
- v1 transport stays wired-but-unused until the gated Task 15 (deletion) — do not delete it early.
- Direct-only: no relay fallback this milestone. `DefaultRelayOnly` is forced `false`.
- Never read/cat `.env` secrets; live tests use `deploy-testing.sh` which sources `.env.testing` without echoing it.

---

## File Structure Map

**Agent (module `sharebridge/agent`):**
- `agent/internal/immich/client.go` — MODIFY: typed errors + metadata helpers (additive).
- `agent/internal/immich/errors.go` — CREATE: typed error types.
- `agent/internal/direct/content.go` — CREATE: `ContentBackend`, `ContentSession`, `Ledger`, `Resolver`, `SnapshotManager`, `ResolverRegistry`, `/items` wire DTO.
- `agent/internal/direct/range.go` — CREATE: `ParseRange`.
- `agent/internal/direct/handlers.go` — CREATE: thumb/preview/asset/playback/archive/items handlers.
- `agent/internal/direct/server.go` — MODIFY: route dispatch; `SetResolver`; idle hold; server timeouts.
- `agent/internal/direct/embed.go` — CREATE: `go:embed` the recipient UI.
- `agent/internal/daemon/daemon.go` — MODIFY: `Session` → `*immich.Client`, registry wiring, snapshot hydration triggers, protected/unsupported rejection, `deregister`/`unregister` wiring, config plumbing.
- `agent/internal/web/static/` — CREATE: the recipient UI (copied from `signaling-server/web/`, rewritten data layer).

**Control (module `sharebridge/server` → `sharebridge/control`):**
- `signaling-server/migrations/8_sessions_inactive_reason.go` — CREATE.
- `signaling-server/internal/directctl/redirect.go` — MODIFY: the **single** tombstone-aware resolver (owner of the status logic).
- `signaling-server/internal/handler/agent_ws.go` — MODIFY: `deregister` handling; call the resolver.
- `signaling-server/cmd/server/main.go` — MODIFY: route `/share/{code}` and `/s/{code}` through the resolver; config.

---

## Milestone 3a — Content Serving

### Task 1: Immich client — typed errors + metadata helpers

**Files:**
- Create: `agent/internal/immich/errors.go`
- Modify: `agent/internal/immich/client.go`
- Test: `agent/internal/immich/errors_test.go`, `agent/internal/immich/metadata_test.go`

**Interfaces:**
- Consumes: every HTTP status-flattening site in `immich.Client`: `getAsset` (`client.go:613`), `doJSON` (`:637`), `doJSONStatus` (`:653`), `HeadVideoPlayback` (`:597`, plain `fmt.Errorf`), `GetAlbumDownloadInfo` (`:388`), `DownloadArchive` (`:418`), and the thumbnail/preview fetches (`GetThumbnail` `:550`, `GetPreview` `:554`).
- Produces:
  - `immich.NotFoundError`, `immich.AuthError`, `immich.UpstreamError{Status int}` (all implement `error`; classified via `errors.As`).
  - `immich.ThumbnailInfo(ctx, id) (contentType string, length int64, known bool, err error)`
  - `immich.PreviewInfo(ctx, id) (contentType string, length int64, known bool, err error)`
  - `immich.PlaybackInfo(ctx, id) (length int64, known bool, err error)` — `known=false` when length is missing/zero.

- [ ] **Step 1: Write the failing tests**

`errors_test.go`: an `httptest.Server` returning each status (401, 403, 404, 500); assert `errors.As` classifies `AuthError`/`NotFoundError`/`UpstreamError`. `metadata_test.go`: assert `ThumbnailInfo`/`PreviewInfo` return the upstream `Content-Type` + `known=true` when `Content-Length` is present, `known=false` when absent; `PlaybackInfo` `known=false` for a missing length.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd agent && go test ./internal/immich/ -run 'Error|Info' -v`
Expected: FAIL (undefined types/functions).

- [ ] **Step 3: Implement**

Define the three error types in `errors.go`. In `client.go`, replace **every** flattened status return with the typed error (map 401/403→`AuthError`, 404→`NotFoundError`, other HTTP→`UpstreamError{Status}`; 416/timeout → `UpstreamError`). Add `ThumbnailInfo`/`PreviewInfo`/`PlaybackInfo` via a HEAD request (or reading response headers before `io.Copy`), `known=false` when the header is absent.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd agent && go test ./internal/immich/ -run 'Error|Info' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/immich/errors.go agent/internal/immich/client.go agent/internal/immich/errors_test.go agent/internal/immich/metadata_test.go
git commit -m "feat(immich): typed errors + pre-stream metadata helpers (wire protocol unchanged)"
```

---

### Task 2: Content state layer — backend, session, ledger, snapshot, registry, DTO

**Files:**
- Create: `agent/internal/direct/content.go`
- Test: `agent/internal/direct/content_test.go`

**Interfaces:**
- Consumes: `immich.Client` (§3 methods).
- Produces (exact — later tasks rely on these names):
  - `type ContentBackend interface { ListGallery(ctx) (immich.Gallery, error); GetThumbnail(ctx, id string, w io.Writer) (int64, error); GetPreview(ctx, id string, w io.Writer) (int64, error); GetAssetInfo(ctx, id string) (immich.Asset, error); GetFile(ctx, id string, w io.Writer) (int64, error); GetVideoPlayback(ctx, id string, w io.Writer) (int64, error); GetVideoPlaybackRange(ctx, id string, startOffset int64, w io.Writer) (int64, error); HeadVideoPlayback(ctx, id string) (int64, error); GetAlbumDownloadInfo(ctx) (immich.AlbumDownload, error); DownloadArchive(ctx, assetIDs []string, w io.Writer) (int64, error); ThumbnailInfo(ctx, id) (string, int64, bool, error); PreviewInfo(ctx, id) (string, int64, bool, error); PlaybackInfo(ctx, id) (int64, bool, error) }`
  - `type ContentSession struct { Backend ContentBackend; Gallery immich.Gallery; Membership map[string]struct{}; ContentGen uint64; MaxDownloads int; Ledger *Ledger }` — the resolver's sentinel errors (below) encode the lifecycle ("active, gallery-type, not revoked").
  - `type Ledger struct { mu sync.Mutex; max int; downloads int; reservations int }` — `TryReserve() bool` = `downloads+reservations < max` (or true when `max <= 0`); `Commit() int`; `Release()`.
  - `type Resolver interface { Resolve(code string) (*ContentSession, error) }` + sentinels `ErrUnknown`, `ErrForbidden`, `ErrUnready`.
  - `type SnapshotManager struct { mu sync.Mutex; backend ContentBackend; session *ContentSession; contentGen uint64; refreshAt time.Time; lastErrAt time.Time }` — `Build(ctx)` fetches `ListGallery` **outside** the lock, detects content change (deep-compare DTO/membership → only then advance `contentGen`), and atomically swaps under `mu` (singleflight'd). `Resolve` → `ErrUnready` before first successful hydration and after refresh has failed > 2× the poll interval (fail-closed).
  - `type ResolverRegistry struct { mu sync.Mutex; byCode map[string]*SnapshotManager }` — `Get(code)`, `Put(code, *SnapshotManager)`, `Delete(code)`.
  - The `/items` wire DTO (lowerCamel, since `immich.Gallery` has **no JSON tags**): `type itemsResponse struct { AlbumName string \`json:"albumName"\`; AlbumDescription string \`json:"albumDescription"\`; Items []itemDTO \`json:"items"\` }` and `type itemDTO struct { ID, Name, MimeType string; Width, Height int; Size int64; Duration *float64; SHA1 string }` with the matching `json:"…"` tags.

- [ ] **Step 1: Write the failing tests**

`content_test.go` (fake `ContentBackend`): `Ledger.TryReserve` enforces the limit, `max<=0` is unlimited, `Commit` increments `downloads`, `Release` decrements `reservations`, and two concurrent `TryReserve` at `downloads==max-1` cannot both succeed. `SnapshotManager.Resolve` → `ErrUnready` before first `Build`, → snapshot after `Build`, → `ErrUnready` (fail-closed) after repeated `Build` failures past 2× poll. `Build` does **not** advance `contentGen` when the gallery is unchanged. `ResolverRegistry` `Get`/`Put`/`Delete` round-trip. `itemsResponse` marshals to lowerCamel keys.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd agent && go test ./internal/direct/ -run 'Ledger|Snapshot|Registry|Items' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Implement all the above types in `content.go`. `Ledger` holds `l.mu`; `SnapshotManager` holds `mu` (the per-share state lock); `ResolverRegistry` holds `mu`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd agent && go test ./internal/direct/ -run 'Ledger|Snapshot|Registry|Items' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/direct/content.go agent/internal/direct/content_test.go
git commit -m "feat(direct): content state layer — backend, session, ledger, snapshot manager, registry, DTO"
```

---

### Task 3: Resolver wiring — DirectServer setter + daemon session plumbing

**Files:**
- Modify: `agent/internal/direct/server.go` (`SetResolver`), `agent/internal/daemon/daemon.go` (`Session` field, registry, hydration triggers)
- Test: `agent/internal/direct/wiring_test.go`, `agent/internal/daemon/wiring_test.go`

**Interfaces:**
- Consumes: `ContentBackend`, `Resolver`, `ResolverRegistry`, `SnapshotManager` (Task 2); `DirectServer` (`server.go:37`), `daemon.Daemon.sessions map[string]*Session` + `syncDirectServe` + `syncImmichShares` + `registerImmichShare` + `loadSessionsFromStore`.
- Produces:
  - `func (s *DirectServer) SetResolver(r Resolver)` — a **setter** (NOT a constructor change), so `syncDirectServe`'s call to `NewDirectServerWithBinder` keeps compiling.
  - `daemon.Session` retains the concrete `*immich.Client` (the `immichGalleryBackend`/`ValidatePassword`-only interface is insufficient to satisfy `ContentBackend`).
  - Hydration triggers: `syncImmichShares`, `registerImmichShare`, and `loadSessionsFromStore` each ensure a `SnapshotManager` exists in the registry for their share and call `Build`.

- [ ] **Step 1: Write the failing tests**

`wiring_test.go`: after `SetResolver(fake)`, `DirectServer.Handler()` consults the fake (assert a resolved code reaches the handler; an unresolvable code → 404). `daemon/wiring_test.go`: registering a share creates a `SnapshotManager` in the registry and triggers `Build`; `loadSessionsFromStore` re-hydrates restored sessions.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd agent && go test ./internal/direct/ -run Wiring -v && go test ./internal/daemon/ -run Wiring -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Add `SetResolver` to `DirectServer` (store the `Resolver`; `route()` falls back to 404 when nil). In `daemon.go`, add the `*immich.Client` field to `Session`, create the `ResolverRegistry`, and call `SetResolver` after constructing the `DirectServer` in `syncDirectServe`. Add the three hydration triggers.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd agent && go test ./internal/direct/ -run Wiring -v && go test ./internal/daemon/ -run Wiring -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/direct/server.go agent/internal/daemon/daemon.go agent/internal/direct/wiring_test.go agent/internal/daemon/wiring_test.go
git commit -m "feat(direct): wire resolver into DirectServer + daemon session plumbing"
```

---

### Task 4: Range parsing

**Files:**
- Create: `agent/internal/direct/range.go`
- Test: `agent/internal/direct/range_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `func ParseRange(h string, total int64, known bool) (start, end int64, partial bool, ok bool)`.

- [ ] **Step 1: Write the failing tests**

Table-driven: `bytes=0-99` → `(0,99,true,true)`; `bytes=100-` → `(100,total-1,true,true)`; suffix/multiple/non-numeric → `ok=false`; `known=false` → `ok=false` (ignore); `known=true,total=0` → `ok=false` (→416); `start>=total` → `ok=false`; `end` clamped to `total-1`; overflow → `ok=false`.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd agent && go test ./internal/direct/ -run Range -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Implement `ParseRange` per §4.2.

- [ ] **Step 4: Run test to verify it passes**

Run: `cd agent && go test ./internal/direct/ -run Range -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/direct/range.go agent/internal/direct/range_test.go
git commit -m "feat(direct): byte-range parsing"
```

---

### Task 5: Route dispatch + thumb/preview/asset handlers + headers + errors

**Files:**
- Create: `agent/internal/direct/handlers.go`
- Modify: `agent/internal/direct/server.go` (route dispatch)
- Test: `agent/internal/direct/handlers_test.go`

**Interfaces:**
- Consumes: `ContentSession`, `Resolver` (Task 2), `ParseRange` (Task 4), `DirectServer.route` (`server.go:95`).
- Produces: `(s *DirectServer) handleThumb`, `handlePreview`, `handleAsset`; route dispatch for `/s/{code}/thumb/{id}`, `/preview/{id}`, `/asset/{id}`.

- [ ] **Step 1: Write the failing tests**

Fake `ContentBackend` + `httptest` over `DirectServer.Handler()`: `/thumb/{id}` and `/preview/{id}` → 200 + correct `Content-Type` + `X-Content-Type-Options: nosniff` + `Cache-Control: no-store` + `Referrer-Policy: no-referrer`; unknown asset → 404 **before** any body byte; unknown code → 404; membership failure → 403; HEAD → status+headers, no body; `/asset/{id}` → `Content-Disposition: attachment` with a sanitized filename (path separators stripped); upstream 500 → 502, timeout → 503 (typed-error mapping).

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd agent && go test ./internal/direct/ -run Handler -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Implement the handlers: resolve via the injected `Resolver`, membership-check `{id}` against `ContentSession.Membership`, preflight metadata, then stream with a **delayed-status writer** (buffer `WriteHeader` until the first body write) so a pre-first-byte error still sets the right status. Map typed errors (§4.5) exactly. **Mid-stream failure after the first byte**: log + `panic(http.ErrAbortHandler)`. In `server.go`, dispatch `/thumb/{id}`, `/preview/{id}`, `/asset/{id}` in `route()`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd agent && go test ./internal/direct/ -run Handler -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/direct/handlers.go agent/internal/direct/server.go agent/internal/direct/handlers_test.go
git commit -m "feat(direct): thumb/preview/asset handlers + security headers + typed-error mapping"
```

---

### Task 6: `/items` handler + wire DTO

**Files:**
- Modify: `agent/internal/direct/handlers.go`
- Test: `agent/internal/direct/items_test.go`

**Interfaces:**
- Consumes: `ContentSession.Gallery`, `itemsResponse`/`itemDTO` (Task 2).
- Produces: `(s *DirectServer) handleItems` (wired at `/s/{code}/items`).

- [ ] **Step 1: Write the failing tests**

Assert `/items` returns `200` with the lowerCamel JSON (`albumName`, `items[].id/name/mimeType/width/height/size/duration/sha1`), `duration` omitted-or-null per item, empty album → `items: []`, `no-store` header, HEAD no body.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd agent && go test ./internal/direct/ -run Items -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Implement `handleItems`: marshal the `ContentSession.Gallery` into `itemsResponse` (never `immich.Gallery` directly — it lacks JSON tags). Serve `200`, `no-store`.

- [ ] **Step 4: Run test to verify it passes**

Run: `cd agent && go test ./internal/direct/ -run Items -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/direct/handlers.go agent/internal/direct/items_test.go
git commit -m "feat(direct): /items endpoint with lowerCamel wire DTO"
```

---

### Task 7: Video playback (Range) handler

**Files:**
- Modify: `agent/internal/direct/handlers.go`
- Test: `agent/internal/direct/playback_test.go`

**Interfaces:**
- Consumes: `ContentBackend.GetVideoPlaybackRange`, `HeadVideoPlayback`, `PlaybackInfo`, `ParseRange`.
- Produces: `(s *DirectServer) handlePlayback` (wired at `/s/{code}/asset/{id}/playback`).

- [ ] **Step 1: Write the failing tests**

Fake backend: no Range → 200 `video/mp4` full body; `bytes=0-99` → `206` + `Content-Range: bytes 0-99/1000` + `Accept-Ranges: bytes`; `known=false` → ignore Range, 200, no `Accept-Ranges`; `known=true,total=0` → 416 `Content-Range: bytes */0`; `start>=total` → 416; bounded writer stops at `end` without surfacing the bound-sentinel as an error.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd agent && go test ./internal/direct/ -run Playback -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Implement `handlePlayback` per §4.2 (route on `known`/`total`; valid range → `GetVideoPlaybackRange` through a bounded writer; else `GetVideoPlayback`).

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd agent && go test ./internal/direct/ -run Playback -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/direct/handlers.go agent/internal/direct/playback_test.go
git commit -m "feat(direct): transcoded-video playback with byte-range (206/416)"
```

---

### Task 8: Album archive — manifest + transaction + accounting commit

**Files:**
- Modify: `agent/internal/direct/handlers.go`, `agent/internal/direct/content.go`
- Test: `agent/internal/direct/archive_test.go`

**Interfaces:**
- Consumes: `GetAlbumDownloadInfo`, `DownloadArchive`, `ContentSession.Membership`, `ContentSession.ContentGen`, `Ledger`.
- Produces:
  - `type ArchivePart struct { Index int; Name string; AssetIDs []string; EstimatedSize int64 }`
  - `type ArchiveTransaction struct { Token string; Parts []ArchivePart; ContentGen uint64; mu sync.Mutex; done map[int]bool; state string /* open|committed|released */ }`
  - `(s *DirectServer) handleArchiveManifest`, `(s *DirectServer) handleArchivePart`.

- [ ] **Step 1: Write the failing tests**

Fake backend: manifest returns a token + parts with every `assetIds` ∈ `Membership` and `EstimatedSize` populated; a part outside membership → 403 on the whole manifest; zero archives → 404; `DownloadArchive` streams a part with `Content-Disposition: attachment`; duplicate part fetch is idempotent (no double commit); `Ledger.Commit` fires exactly once when the last part completes; an abandoned transaction (TTL) releases with no commit; an active part pins the transaction (TTL doesn't fire mid-stream); a `ContentGen` change invalidates incomplete transactions (403 + release).

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd agent && go test ./internal/direct/ -run Archive -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Implement the `ArchiveTransaction` state machine (§4.6) under the per-share state lock; `handleArchiveManifest` (singleflight upstream `GetAlbumDownloadInfo` into a template keyed by `ContentGen`, re-check generation after the fetch, mint a fresh token + reservation); `handleArchivePart` (revalidate asset IDs against the current `ContentGen` before streaming, pin the transaction, commit once on last part).

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd agent && go test ./internal/direct/ -run Archive -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/direct/handlers.go agent/internal/direct/content.go agent/internal/direct/archive_test.go
git commit -m "feat(direct): transactional multi-part album archive with download accounting"
```

---

### Task 9: In-flight hold vs idle-close (long transfers)

**Files:**
- Modify: `agent/internal/direct/server.go`, `agent/internal/direct/ondemand.go`
- Test: `agent/internal/direct/ondemand_hold_test.go`

**Interfaces:**
- Consumes: `OnDemandPort` close loop (`ondemand.go` `idleAt` evaluation), `activity()` (`server.go:173`).
- Produces: an in-flight counter / pause flag on `OnDemandPort` so the close loop does not close while any hold is active.

- [ ] **Step 1: Write the failing test**

Start an `OnDemandPort` with a short idle deadline, take a hold, sleep past the deadline, assert still open; release, assert it closes. Mirror the existing on-demand test harness.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd agent && go test ./internal/direct/ -run Hold -v`
Expected: FAIL (port closes despite hold).

- [ ] **Step 3: Implement**

Add the in-flight counter/pause flag to `OnDemandPort`; skip closing while `inFlight > 0`. In `server.go`, wrap each streaming response writer to `Begin()` on first write and `End()` on close/completion (releases on client disconnect).

- [ ] **Step 4: Run test to verify it passes**

Run: `cd agent && go test ./internal/direct/ -run Hold -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/direct/server.go agent/internal/direct/ondemand.go agent/internal/direct/ondemand_hold_test.go
git commit -m "fix(direct): hold on-demand port open during long streaming responses"
```

---

### Task 10: Resource limits — server timeouts, semaphores, overload

**Files:**
- Modify: `agent/internal/direct/server.go` (`newHTTPServer`), `agent/internal/direct/handlers.go`
- Test: `agent/internal/direct/limits_test.go`

**Interfaces:**
- Consumes: `newHTTPServer` (`server.go:212`), the handlers (Tasks 5–8).
- Produces: `ReadHeaderTimeout`, `IdleTimeout`, `MaxHeaderBytes` on the server; per-share + global streaming semaphores; `429`/`503` overload responses; request-`ctx` propagation into every immich call (abort upstream on disconnect).

- [ ] **Step 1: Write the failing tests**

Assert `newHTTPServer` sets the three limits; a saturated per-share semaphore yields `429`/`503`; cancelling the request context aborts the upstream read.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd agent && go test ./internal/direct/ -run Limits -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Add the timeouts/limits to `newHTTPServer`. Add a per-share streaming semaphore (from §11) around the asset/video/archive streaming paths, returning `429` (transient) / `503` when saturated. Ensure handlers pass the request `ctx` into `ContentBackend` calls.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd agent && go test ./internal/direct/ -run Limits -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/direct/server.go agent/internal/direct/handlers.go agent/internal/direct/limits_test.go
git commit -m "feat(direct): server timeouts + per-share streaming semaphores + overload responses"
```

---

### Task 11: Recipient UI — move, rewrite data layer, delete transport JS

**Files:**
- Create: `agent/internal/direct/embed.go`, `agent/internal/web/static/` (the UI)
- Modify: `agent/internal/direct/server.go` (serve `/s/{code}/static/…` + gallery HTML)
- Delete: the transport JS (see §6 delete list) from the agent's copy

**Interfaces:**
- Consumes: `signaling-server/web/` (`index.html`, `src/gallery.js`, `src/videoBufferWarning.js`, `src/vendor/lightgallery/*`).
- Produces: `agent/internal/web/static/` (embedded), `gallery.js` with URL factories `thumbUrl(id)`, `previewUrl(id)`, `assetUrl(id)`, `playbackUrl(id)`, `archiveManifestUrl()`, `archivePartUrl(token, index)`.

- [ ] **Step 1: Write the failing test**

A Go test asserting the embedded FS contains the gallery HTML + static assets, and that `/s/{code}/static/gallery.js` is served with `Content-Type: text/javascript` under the code-prefixed path (Binder-admitted).

- [ ] **Step 2: Run test to verify it fails**

Run: `cd agent && go test ./internal/direct/ -run Static -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Copy `index.html`, `gallery.js`, `videoBufferWarning.js`, lightGallery vendor into `agent/internal/web/static/`; rewrite `app.js` to `fetch` the endpoints and hand URL factories to `gallery.js`; replace binary thumbnail/preview/video handling with `<img src loading="lazy">` / `<video src>`; extract inline `<style>`/`onclick` to static files; replace the `data:image/gif` placeholder with an embedded static asset; delete the transport modules + both service workers + streamsaver + `noise-p256/`. Serve under `/s/{code}/static/…` with the §4.8 CSP.

- [ ] **Step 4: Run test to verify it passes**

Run: `cd agent && go test ./internal/direct/ -run Static -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/direct/embed.go agent/internal/direct/server.go agent/internal/web/static/
git commit -m "feat(agent): embed recipient UI, rewrite gallery data layer to HTTP, drop transport JS"
```

---

### Task 12: Canonical route cutover + `inactive_reason` migration + config/enforcement

**Files:**
- Create: `signaling-server/migrations/8_sessions_inactive_reason.go`
- Modify: `signaling-server/internal/directctl/redirect.go` (the **single** tombstone-aware resolver), `signaling-server/cmd/server/main.go` (route both `/share/{code}` and `/s/{code}` through it), `signaling-server/internal/handler/agent_ws.go` (`deregister` handling)
- Modify: `agent/internal/daemon/daemon.go`, `agent/internal/config/config.go`
- Test: `signaling-server/internal/directctl/redirect_test.go`, `signaling-server/migrations/8_*_test.go`, `agent/internal/daemon/enforce_test.go`

**Interfaces:**
- Consumes: `serveShareRedirect`, `handleUnregisterShare`, the code regexes (`agent_ws.go:97-98`), `RegisterShare`, `registerImmichShare`, `createManualImmichSession`, `RevokeSession`, `pruneExpiredSessions`.
- Produces: `sessions.inactive_reason` field; `directctl.ResolveForRedirect(code) (session, status int)` — the **owner** of the status logic (used by both `/share/{code}` and `/s/{code}`, which 302 to the control `/s/{code}` direct path per §8).

- [ ] **Step 1: Write the failing tests**

Migration test: `inactive_reason` column + backfill ordering (unsupported first, then expired/revoked). Resolver tests: active gallery → 302; relay-only/WebDAV/protected/expired → 410; revoked/unknown → 404; invalid/missing `inactive_reason` → 404. Enforcement tests: poller/manual/restore reject protected + relay-only + WebDAV shares; config forces `DefaultRelayOnly=false`; manual Immich creation plumbs `maxDownloads`/expiry; discovered shares get `config.DefaultMaxDownloads`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd signaling-server && go test ./... -run 'Redirect|Migration' -v` and `cd agent && go test ./internal/daemon/ -run Enforce -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Add the migration (§10 transitions + backfill). Implement `directctl.ResolveForRedirect` and route both `/share/{code}` and `/s/{code}` through it. Add `inactive_reason` writes at every mutation site (§10), and add `deregister` handling in `agent_ws.go` (or route agent revocation/expiry through `UnregisterShare`). Enforce unsupported/protected rejection at all entry points; force `DefaultRelayOnly=false`; plumb `maxDownloads`/expiry.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd signaling-server && go test ./... -run 'Redirect|Migration' -v` and `cd agent && go test ./internal/daemon/ -run Enforce -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add signaling-server/migrations/ signaling-server/internal/directctl/redirect.go signaling-server/cmd/server/main.go signaling-server/internal/handler/agent_ws.go agent/internal/daemon/daemon.go agent/internal/config/config.go
git commit -m "feat(control): tombstone-aware route cutover, inactive_reason migration, unsupported-share enforcement"
```

---

### Task 13: Parity suite (full 3a, gates milestone 3c)

**Files:**
- Create: `agent/internal/direct/parity_test.go` (agent side)
- Modify: none (read-only against Tasks 1–12) — but the gate **runs both** the agent suite **and** the control route suite.

**Interfaces:**
- Consumes: the full 3a stack (cert.Manager + Binder + DirectServer + OnDemandPort + fake `ContentBackend` + test CA, mirroring `6233e10`) **and** Task 12's control route tests.

- [ ] **Step 1: Write the failing tests**

Agent-side end-to-end: enroll → cert → register gallery share → open → probe → redirect → `/s/{code}` (HTML) → `/items` → `/thumb/{id}` → `/preview/{id}` → `/asset/{id}` → `/asset/{id}/playback` (multiple `206` seeks) → `/archive` (manifest) → every `/archive/{token}/{part}`. A browser test (headless) asserts zero CSP violations across gallery load, lightbox open, video slide, zoom, navigation. The gate is `cd agent && go test ./... && cd ../signaling-server && go test ./...` (the control suite covers the `410`/`404` unsupported-share outcomes).

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd agent && go test ./internal/direct/ -run Parity -v`
Expected: FAIL (until Tasks 1–12 are all in place).

- [ ] **Step 3: Implement / wire**

Implement the fake backend (deterministic assets + a multi-part archive + a video honoring byte ranges). No production changes expected — if the suite reveals a gap, fix it in the owning task's file and note it.

- [ ] **Step 4: Run the full 3a gate**

Run: `cd agent && go test ./... && cd ../signaling-server && go test ./...`
Expected: PASS — this is the gate that unblocks milestone 3c.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/direct/parity_test.go
git commit -m "test(direct): browser-level parity suite gating v1 deletion"
```

---

## Milestone 3b — `control/` Rename

### Task 14: Rename `signaling-server/` → `control/`

**Files:**
- Modify: `signaling-server/go.mod` (module path), every `sharebridge/server` import, `deploy-testing.sh` → `control/deploy-testing.sh` (reduce `scp web/` to the four retained pages), `deploy.sh`, `signaling-server/docs/testing-server.md` → `control/docs/`, CI workflows, README.

**Interfaces:**
- Consumes: module path `sharebridge/server` (`signaling-server/go.mod:1`) — no importer exists outside `signaling-server/`, so an in-tree `sed` covers all.
- Produces: module `sharebridge/control`, dir `control/`.

- [ ] **Step 1: Apply the mechanical rename**

```bash
cd /Users/ali/Git/ShareBridge/.worktrees/phase-3
git mv signaling-server control
sed -i '' 's#sharebridge/server#sharebridge/control#g' $(grep -rl 'sharebridge/server' control --include='*.go' --include='go.mod')
```

Update `deploy-testing.sh` (keep copying only `home.html login.html register.html account.html`), `deploy.sh`, docs, CI, README references.

- [ ] **Step 2: Build**

Run: `cd control && CGO_ENABLED=0 go build ./...`
Expected: green.

- [ ] **Step 3: Commit**

```bash
git add -A control/ .github/ README.md
git commit -m "refactor: rename signaling-server -> control (module sharebridge/control)"
```

---

## Milestone 3c — v1 Transport Deletion (GATED)

> **Gate:** Do not start Task 15 until Task 13's parity gate passes **and** Task 14 (rename) is done.

### Task 15: Delete the v1 transport

**Files:**
- Delete (agent): `internal/transfer`, `internal/multilane`, `internal/relaychannel`, `internal/peer`, `internal/noise`.
- Delete (control): `internal/relay`, `internal/handler/relay_ws.go`, `internal/handler/browser_ws.go`, TURN/ICE wiring, `/ws/client` + `/ws/relay` + `/i/{key}` + `/join` + `/sessions/{code}` + web-asset routes, relay runtime config fields.
- Delete (control web tree, wholesale): `control/web/index.html`, `control/web/app.js`, `control/web/sw.js`, `control/web/src/` (entire), `control/web/noise-p256/`, `control/web/package*.json` — leaving only the four retained pages.
- Delete (repo): `extensions/`, `scripts/update_streamsaver_vendor.sh`.
- Modify: `control/cmd/server/main.go` (route removal), `agent/internal/daemon/daemon.go` (remove peer/multilane/relay wiring + transport message handling), `agent/internal/signaling/client.go` (remove transport message handling).

**Interfaces:**
- Consumes: nothing (pure removal).
- Produces: a v1-free tree that still compiles.

- [ ] **Step 1: Enumerate the wiring before deleting**

Run `grep -rn 'transfer\|multilane\|relaychannel\|internal/peer\|internal/noise\|internal/relay\|browser_ws\|TURN\|ICE' agent/cmd agent/internal/daemon control/cmd control/internal/handler --include='*.go' | grep -v _test.go` and record every reference. This is the deletion inventory.

- [ ] **Step 2: Delete the listed trees + files**

Remove the enumerated files and recorded references; remove `control/web/src`, `control/web/noise-p256`, `control/web/package*.json`, `control/web/index.html`, `control/web/app.js`, `control/web/sw.js` wholesale. Remove transport message handling from the kept signaling client. Keep `immich`, `cloudwebdav`, `signaling`, `direct`, `cert`, `daemon` (minus v1 wiring), `store`, `web`, `config`.

- [ ] **Step 3: Build both modules**

Run: `cd agent && go build ./... && cd ../control && go build ./...`
Expected: green.

- [ ] **Step 4: Run the full test suite**

Run: `cd agent && go test ./... && cd ../control && go test ./...`
Expected: PASS (parity suite included).

- [ ] **Step 5: Commit**

```bash
git add -A agent/ control/ extensions/ scripts/
git commit -m "chore: delete v1 WebRTC/relay/multilane transport (parity reached)"
```

---

## Self-Review Notes (run after writing)

- **Spec coverage:** §3.1→Task 1; §3/§5→Task 2; §5.1 wiring→Task 3; §4.2→Task 4; §4.1/4.4/4.5/4.8→Task 5; §4.3→Task 6; §4.2 playback→Task 7; §4.6→Task 8; §4.7→Task 9; §11 limits→Task 10; §6→Task 11; §8/§10/§13→Task 12; §12.2→Task 13; §9→Task 14; §10→Task 15.
- **Placeholder scan:** no TBD/TODO; every code step has actual code or an exact command.
- **Type consistency:** `ContentBackend`, `ContentSession`, `Ledger` (with `max`), `Resolver`, `SnapshotManager` (with `contentGen`), `ResolverRegistry`, `itemsResponse`/`itemDTO`, `ParseRange`, `ArchivePart` (with `EstimatedSize`), `ArchiveTransaction` are defined once and referenced by name.
