# Phase 3 — Content Serving (Direct HTTP) + `control/` Rename — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the Phase-2 placeholder with real Immich gallery content serving over the agent's direct HTTPS path, move the recipient UI to the agent, cut the canonical route over to direct, rename `signaling-server/` → `control/`, and (gated behind automated parity) delete the v1 WebRTC/relay/multilane transport.

**Architecture:** The agent's `direct.DirectServer` grows an HTTP content layer backed by a narrow `ContentBackend` interface (implemented over the frozen `immich.Client`), resolving share codes through an immutable `ContentSession` snapshot (gallery DTO + membership index + generation) and a per-share state-lock-guarded accounting ledger for download limits. The recipient UI (`index.html` + `gallery.js` + lightGallery) is embedded in the agent and served under `/s/{code}/static/…`; its data layer is rewritten from the binary WebRTC/multilane transport to plain `fetch`/`<img>`/`<video>`.

**Tech Stack:** Go (pure-Go sqlite, no cgo), `net/http` (+`http.ServeMux` with method patterns), `go:embed`, vanilla JS + lightGallery (no build step), PocketBase migrations.

## Global Constraints

- Every task MUST end with `go build ./...` green in the relevant module (agent: `agent/`, control: `signaling-server/`), so the branch compiles in isolation. Run it from the worktree root for the agent module, and from `signaling-server/` for the control module.
- Model policy: `deepseek/deepseek-v4-pro` for all implementation subagents; `openai-codex/gpt-5.6-sol` reserved for whole-branch review (per user direction).
- TDD throughout: write the failing test, watch it fail, implement, watch it pass, commit.
- The `immich.Client` wire protocol (endpoints, params, auth, request/response shapes) is **frozen** — §3.1 changes are additive Go helpers only.
- v1 transport stays wired-but-unused until the gated Task 11 (deletion) — do not delete it early.
- Direct-only: no relay fallback this milestone. `DefaultRelayOnly` is forced `false`.
- Never read/cat `.env` secrets; live tests use `deploy-testing.sh` which sources `.env.testing` without echoing it.

---

## File Structure Map

**Agent (module `sharebridge/agent`):**
- `agent/internal/immich/client.go` — MODIFY: typed errors + metadata helpers (additive).
- `agent/internal/immich/errors.go` — CREATE: typed error types.
- `agent/internal/direct/content.go` — CREATE: `ContentBackend`, `ContentSession`, resolver seam, accounting ledger.
- `agent/internal/direct/server.go` — MODIFY: route dispatch to content handlers; wire the resolver + idle hold.
- `agent/internal/direct/handlers.go` — CREATE: thumb/preview/asset/playback/archive/items HTTP handlers.
- `agent/internal/direct/range.go` — CREATE: Range parsing + `206`/`416` logic.
- `agent/internal/direct/embed.go` — CREATE: `go:embed` the recipient UI assets.
- `agent/internal/daemon/daemon.go` — MODIFY: snapshot hydration, membership, protected/unsupported rejection, `deregister`/`unregister` wiring, config plumbing.
- `agent/internal/web/static/` — CREATE: the recipient UI (copied from `signaling-server/web/`, rewritten data layer).

**Control (module `sharebridge/server` → `sharebridge/control`):**
- `signaling-server/migrations/8_sessions_inactive_reason.go` — CREATE: `inactive_reason` field + backfill.
- `signaling-server/internal/handler/agent_ws.go` — MODIFY: `deregister` handling; route tombstone resolver.
- `signaling-server/cmd/server/main.go` — MODIFY: `/share` + `/s` tombstone resolver; config.
- `signaling-server/internal/directctl/redirect.go` — MODIFY: shared tombstone-aware resolver.

---

## Milestone 3a — Content Serving

### Task 1: Immich client — typed errors + metadata helpers

**Files:**
- Create: `agent/internal/immich/errors.go`
- Modify: `agent/internal/immich/client.go`
- Test: `agent/internal/immich/errors_test.go`, `agent/internal/immich/metadata_test.go`

**Interfaces:**
- Consumes: the existing `immich.Client` HTTP paths (`getAsset`, `doJSON`, `doJSONStatus`, `HeadVideoPlayback`, `GetAlbumDownloadInfo`, `DownloadArchive`, thumbnail/preview fetches) — see `agent/internal/immich/client.go:550-670`.
- Produces:
  - `immich.NotFoundError`, `immich.AuthError`, `immich.UpstreamError{Status int}` (all implement `error`, exposed via `errors.As`).
  - `immich.ThumbnailInfo(ctx, id) (contentType string, length int64, known bool, err error)`
  - `immich.PreviewInfo(ctx, id) (contentType string, length int64, known bool, err error)`
  - `immich.PlaybackInfo(ctx, id) (length int64, known bool, err error)`

- [ ] **Step 1: Write the failing tests**

Create `agent/internal/immich/errors_test.go` with an `httptest.Server` returning a chosen status, assert `errors.As` classification. Create `metadata_test.go` asserting `ThumbnailInfo` returns the upstream `Content-Type` and `known=true` when `Content-Length` is present, `known=false` when absent, and `PlaybackInfo` returns `known=false` for a missing/zero length.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd agent && go test ./internal/immich/ -run 'Error|Info' -v`
Expected: FAIL (undefined types/functions).

- [ ] **Step 3: Implement**

In `errors.go` define the three error types with `Error()` methods. In `client.go`, replace the flattened `responseStatusError` returns with `&UpstreamError{Status}`/`NotFoundError`/`AuthError` (map 401/403→`AuthError`, 404→`NotFoundError`, other→`UpstreamError`); add `ThumbnailInfo`/`PreviewInfo`/`PlaybackInfo` using a HEAD request (or reading the response headers before `io.Copy`) to extract `Content-Type` + `Content-Length`, returning `known=false` when the header is absent.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd agent && go test ./internal/immich/ -run 'Error|Info' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/immich/errors.go agent/internal/immich/client.go agent/internal/immich/errors_test.go agent/internal/immich/metadata_test.go
git commit -m "feat(immich): typed errors + pre-stream metadata helpers (wire protocol unchanged)"
```

---

### Task 2: Content backend interface + resolver seam + accounting ledger

**Files:**
- Create: `agent/internal/direct/content.go`
- Test: `agent/internal/direct/content_test.go`

**Interfaces:**
- Consumes: `immich.Client` (§3 methods), `daemon.Daemon`'s `sessions map[string]*Session` (existing).
- Produces (exact — later tasks rely on these names):
  - `type ContentBackend interface { ListGallery(ctx) (immich.Gallery, error); GetThumbnail(ctx, id string, w io.Writer) (int64, error); GetPreview(ctx, id string, w io.Writer) (int64, error); GetAssetInfo(ctx, id string) (immich.Asset, error); GetFile(ctx, id string, w io.Writer) (int64, error); GetVideoPlayback(ctx, id string, w io.Writer) (int64, error); GetVideoPlaybackRange(ctx, id string, startOffset int64, w io.Writer) (int64, error); HeadVideoPlayback(ctx, id string) (int64, error); GetAlbumDownloadInfo(ctx) (immich.AlbumDownload, error); DownloadArchive(ctx, assetIDs []string, w io.Writer) (int64, error); ThumbnailInfo(ctx, id) (string, int64, bool, error); PreviewInfo(ctx, id) (string, int64, bool, error); PlaybackInfo(ctx, id) (int64, bool, error) }`
  - `type ContentSession struct { Backend ContentBackend; Gallery immich.Gallery; Membership map[string]struct{}; Generation uint64; MaxDownloads int; Ledger *Ledger }`
  - `type Ledger struct { mu sync.Mutex; downloads int; reservations int }`
  - `func (l *Ledger) TryReserve() bool` — `downloads+reservations < MaxDownloads` (or always true when `MaxDownloads <= 0`), increments `reservations`.
  - `func (l *Ledger) Commit() int` / `func (l *Ledger) Release()`
  - `type Resolver interface { Resolve(code string) (*ContentSession, error) }` with sentinel errors `ErrUnknown`, `ErrForbidden`, `ErrUnready`.
  - `type SnapshotManager struct { mu sync.Mutex /* per-share state lock */; session *ContentSession; lastRefresh time.Time; lastErr time.Time }` — implements `Resolver`. `Build(ctx, backend)` fetches `ListGallery` **outside** the lock, builds the DTO + membership index, then atomically swaps under `mu` (singleflight'd). `Resolve` returns `ErrUnready` (→503) before the first successful hydration, `ErrUnready` (→503, fail-closed) when refresh has failed for > 2× the poll interval, else the snapshot.

- [ ] **Step 1: Write the failing tests**

`content_test.go`: a fake `ContentBackend`; assert `Ledger.TryReserve` enforces the limit, `MaxDownloads<=0` is unlimited, `Commit` increments `downloads`, `Release` decrements `reservations`, and two concurrent `TryReserve` at `downloads==MaxDownloads-1` cannot both succeed (use a goroutine race test). Also assert `SnapshotManager.Resolve` returns `ErrUnready` before the first `Build`, returns the snapshot after `Build`, and returns `ErrUnready` (fail-closed) when `Build` keeps failing past 2× the poll interval.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd agent && go test ./internal/direct/ -run 'Ledger|Resolve' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Write `ContentBackend`, `ContentSession`, `Ledger`, `Resolver`, `SnapshotManager`, and the sentinel errors in `content.go`. The `Ledger` methods hold `l.mu`. `TryReserve` returns false when the limit is finite and reached. `SnapshotManager.Build` fetches outside the lock and swaps under it; `Resolve` implements the unready/fail-closed policy (§5.2).

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd agent && go test ./internal/direct/ -run 'Ledger|Resolve|Snapshot' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/direct/content.go agent/internal/direct/content_test.go
git commit -m "feat(direct): ContentBackend interface, ContentSession, download Ledger"
```

---

### Task 3: DirectServer routing + thumb/preview/asset handlers + HEAD + headers

**Files:**
- Create: `agent/internal/direct/handlers.go`, `agent/internal/direct/range.go`
- Modify: `agent/internal/direct/server.go` (route dispatch)
- Test: `agent/internal/direct/handlers_test.go`, `agent/internal/direct/range_test.go`

**Interfaces:**
- Consumes: `ContentSession`, `Resolver`, `ContentBackend` (Task 2); `DirectServer.route` (existing `server.go:95`).
- Produces: handler funcs `(s *DirectServer) handleThumb`, `handlePreview`, `handleAsset`, `handlePlayback`, `handleItems`; `ParseRange(h string, total int64, known bool) (start, end int64, partial bool, ok bool)`.

- [ ] **Step 1: Write the failing tests**

`range_test.go`: table-driven — `bytes=0-99` → `(0,99,true,true)`; `bytes=100-` → `(100,total-1,true,true)`; suffix/multiple/non-numeric → `ok=false`; `known=false` → `ok=false` (ignore); `known=true,total=0` → `ok=false` (→416); `start>=total` → `ok=false`.
`handlers_test.go`: using a fake `ContentBackend` + a `httptest` recorder over `DirectServer.Handler()`, assert: `/s/{code}/thumb/{id}` returns 200 + correct `Content-Type` + `X-Content-Type-Options: nosniff` + `Cache-Control: no-store`; unknown asset → 404; HEAD returns status+headers with no body; `Referrer-Policy: no-referrer`; `Content-Disposition: attachment` on `/asset/{id}` with a sanitized filename.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd agent && go test ./internal/direct/ -run 'Range|Handler' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

In `range.go`, implement `ParseRange`. In `handlers.go`, implement the handlers: each resolves the session (via the injected `Resolver`), membership-checks `{id}` against `ContentSession.Membership`, preflights metadata, then streams the body with a delayed-status writer (buffer `WriteHeader` until first body write). In `server.go`, change `route()` to dispatch `/items`, `/thumb/{id}`, `/preview/{id}`, `/asset/{id}` to the new handlers.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd agent && go test ./internal/direct/ -run 'Range|Handler' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/direct/handlers.go agent/internal/direct/range.go agent/internal/direct/server.go agent/internal/direct/handlers_test.go agent/internal/direct/range_test.go
git commit -m "feat(direct): thumb/preview/asset handlers + Range parsing + security headers"
```

---

### Task 4: Video playback (Range) handlers

**Files:**
- Modify: `agent/internal/direct/handlers.go`
- Test: `agent/internal/direct/playback_test.go`

**Interfaces:**
- Consumes: `ContentBackend.GetVideoPlaybackRange`, `HeadVideoPlayback`, `PlaybackInfo`, `ParseRange` (Tasks 1–3).
- Produces: `(s *DirectServer) handlePlayback` (wired at `/s/{code}/asset/{id}/playback`).

- [ ] **Step 1: Write the failing tests**

`playback_test.go` (fake backend): no Range → 200 `video/mp4` full body; `bytes=0-99` → `206` + `Content-Range: bytes 0-99/1000` + `Accept-Ranges: bytes`; `known=false` → ignore Range, 200, no `Accept-Ranges`; `known=true,total=0` → 416 `Content-Range: bytes */0`; `start>=total` → 416; a bounded writer that stops at `end` does not surface the bound-sentinel as an error.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd agent && go test ./internal/direct/ -run Playback -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Implement `handlePlayback`: `PlaybackInfo` → route on `known`/`total`; valid range → `GetVideoPlaybackRange` through a bounded writer (stop at `end`, suppress the sentinel); else `GetVideoPlayback`. Emit `206`/`Content-Range`/`Accept-Ranges` per §4.2.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd agent && go test ./internal/direct/ -run Playback -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/direct/handlers.go agent/internal/direct/playback_test.go
git commit -m "feat(direct): transcoded-video playback with byte-range (206/416)"
```

---

### Task 5: Album archive — manifest + transaction + accounting commit

**Files:**
- Modify: `agent/internal/direct/handlers.go`, `agent/internal/direct/content.go`
- Test: `agent/internal/direct/archive_test.go`

**Interfaces:**
- Consumes: `GetAlbumDownloadInfo`, `DownloadArchive`, `ContentSession.Membership`, `Ledger` (Tasks 1–2).
- Produces:
  - `type ArchiveTransaction struct { Token string; Parts []ArchivePart; Generation uint64; mu sync.Mutex; done map[int]bool; state string /* open|committed|released */ }`
  - `type ArchivePart struct { Index int; Name string; AssetIDs []string }`
  - `(s *DirectServer) handleArchiveManifest`, `(s *DirectServer) handleArchivePart`.

- [ ] **Step 1: Write the failing tests**

`archive_test.go` (fake backend): manifest returns a token + parts with every `assetIds` in `Membership`; a part outside membership → 403 on the whole manifest; zero archives → 404; `DownloadArchive` streams a part with `Content-Disposition: attachment`; duplicate part fetch is idempotent (no double commit); `Ledger.Commit` fires exactly once when the last part completes; an abandoned transaction (TTL) releases with no commit; an active part pins the transaction (TTL doesn't fire mid-stream).

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd agent && go test ./internal/direct/ -run Archive -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Implement the `ArchiveTransaction` state machine (§4.6) under the per-share state lock, `handleArchiveManifest` (singleflight the upstream `GetAlbumDownloadInfo` into a template keyed by generation, re-check generation, mint a fresh token + reservation), and `handleArchivePart` (revalidate asset IDs against the current snapshot generation before streaming; pin the transaction; commit once on last part).

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd agent && go test ./internal/direct/ -run Archive -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/direct/handlers.go agent/internal/direct/content.go agent/internal/direct/archive_test.go
git commit -m "feat(direct): transactional multi-part album archive with download accounting"
```

---

### Task 6: In-flight hold vs idle-close (long transfers)

**Files:**
- Modify: `agent/internal/direct/server.go`, `agent/internal/direct/ondemand.go`
- Test: `agent/internal/direct/ondemand_hold_test.go`

**Interfaces:**
- Consumes: `SessionTracker` / `OnDemandPort` close loop (`ondemand.go` `idleAt` evaluation), `activity()` (`server.go:173`).
- Produces: `type ActiveTransferHold interface { Begin(); End() }` (or a pause flag on the port), wired so the close loop does not close while any hold is active.

- [ ] **Step 1: Write the failing test**

`ondemand_hold_test.go`: start an `OnDemandPort` with a short idle deadline, take a hold, sleep past the deadline, assert the port is still open; release the hold, assert it closes after the deadline. Mirror the existing on-demand tests' harness.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd agent && go test ./internal/direct/ -run Hold -v`
Expected: FAIL (port closes despite hold).

- [ ] **Step 3: Implement**

Add an in-flight counter to `OnDemandPort` (or a pause flag) and have the close loop skip closing while `inFlight > 0`. In `server.go`, wrap each streaming response's body writer to `Begin()` on first write and `End()` on `Close`/completion (releases on client disconnect).

- [ ] **Step 4: Run test to verify it passes**

Run: `cd agent && go test ./internal/direct/ -run Hold -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/direct/server.go agent/internal/direct/ondemand.go agent/internal/direct/ondemand_hold_test.go
git commit -m "fix(direct): hold on-demand port open during long streaming responses"
```

---

### Task 7: Recipient UI — move, rewrite data layer, delete transport JS

**Files:**
- Create: `agent/internal/direct/embed.go`, `agent/internal/web/static/` (the UI)
- Modify: `agent/internal/direct/server.go` (serve `/s/{code}/static/…` + gallery HTML)
- Delete: the transport JS (see §6 delete list) from the agent's copy

**Interfaces:**
- Consumes: the existing UI at `signaling-server/web/` (`index.html`, `src/gallery.js`, `src/videoBufferWarning.js`, `src/vendor/lightgallery/*`).
- Produces: `agent/internal/web/static/` (embedded), `gallery.js` with URL factories `thumbUrl(id)`, `previewUrl(id)`, `assetUrl(id)`, `playbackUrl(id)`, `archiveManifestUrl()`, `archivePartUrl(token, index)`.

- [ ] **Step 1: Write the failing test**

A Go test asserting the embedded FS contains the gallery HTML + static assets, and that `/s/{code}/static/gallery.js` (or equivalent) is served with `Content-Type: text/javascript` under the code-prefixed path (Binder-admitted).

- [ ] **Step 2: Run test to verify it fails**

Run: `cd agent && go test ./internal/direct/ -run Static -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Copy `index.html`, `gallery.js`, `videoBufferWarning.js`, lightGallery vendor into `agent/internal/web/static/`; rewrite `app.js` to `fetch` the §4.1 endpoints and hand URL factories to `gallery.js`; replace binary thumbnail/preview/video handling with `<img src loading="lazy">` / `<video src>`; extract inline `<style>`/`onclick` to static files; delete the transport modules + both service workers + streamsaver + `noise-p256/`. Serve under `/s/{code}/static/…` with the CSP from §4.8.

- [ ] **Step 4: Run test to verify it passes**

Run: `cd agent && go test ./internal/direct/ -run Static -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/direct/embed.go agent/internal/direct/server.go agent/internal/web/static/
git commit -m "feat(agent): embed recipient UI, rewrite gallery data layer to HTTP, drop transport JS"
```

---

### Task 8: Canonical route cutover + `inactive_reason` migration + config/enforcement

**Files:**
- Create: `signaling-server/migrations/8_sessions_inactive_reason.go`
- Modify: `signaling-server/cmd/server/main.go`, `signaling-server/internal/handler/agent_ws.go`, `signaling-server/internal/directctl/redirect.go`
- Modify: `agent/internal/daemon/daemon.go`, `agent/internal/config/config.go`
- Test: `signaling-server/internal/handler/route_test.go`, `signaling-server/migrations/8_*_test.go`, `agent/internal/daemon/enforce_test.go`

**Interfaces:**
- Consumes: `serveShareRedirect`, `handleUnregisterShare`, `RegisterShare`, `registerImmichShare`, `createManualImmichSession`, the session code regexes (`agent_ws.go:97-98`).
- Produces: `inactive_reason` field + a shared `resolveForRedirect(code) (session, status)` used by both `/share/{code}` and `/s/{code}`.

- [ ] **Step 1: Write the failing tests**

Migration test: `inactive_reason` column exists + backfill ordering (unsupported first, then expired/revoked). Route tests: `/share/{code}` and `/s/{code}` → active gallery 302; relay-only/WebDAV/protected/expired → 410; revoked/unknown → 404; invalid/missing `inactive_reason` → 404. Enforcement tests: poller/manual/restore reject protected + relay-only + WebDAV shares; config forces `DefaultRelayOnly=false`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd signaling-server && go test ./... -run 'Route|Migration' -v` and `cd agent && go test ./internal/daemon/ -run Enforce -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Add the migration (field + backfill per §10 transitions). Add `inactive_reason` writes at every mutation site; add `deregister` handling in `agent_ws.go` (or route agent `RevokeSession`/`pruneExpiredSessions` through `UnregisterShare`). Make `/share/{code}` and control `/s/{code}` share the tombstone-aware resolver. Enforce unsupported/protected rejection at all entry points; force `DefaultRelayOnly=false` in config migration. Plumb `maxDownloads`/expiry through the manual Immich path; assign `config.DefaultMaxDownloads` to discovered shares.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd signaling-server && go test ./... -run 'Route|Migration' -v` and `cd agent && go test ./internal/daemon/ -run Enforce -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add signaling-server/migrations/ signaling-server/cmd/server/main.go signaling-server/internal/handler/agent_ws.go signaling-server/internal/directctl/redirect.go agent/internal/daemon/daemon.go agent/internal/config/config.go
git commit -m "feat(control): tombstone-aware route cutover, inactive_reason migration, unsupported-share enforcement"
```

---

### Task 9: Parity suite (browser-level, gates milestone 3c)

**Files:**
- Create: `agent/internal/direct/parity_test.go` (or a new `agent/internal/parity/` package)
- Modify: nothing else (read-only against Tasks 1–8).

**Interfaces:**
- Consumes: the full 3a stack (cert.Manager + Binder + DirectServer + OnDemandPort + fake `ContentBackend` + test CA), mirroring the Phase-2 end-to-end agent test `6233e10`.

- [ ] **Step 1: Write the failing tests**

An end-to-end test that, over the direct path with a test CA: enroll → cert → register gallery share → open → probe → redirect → `/s/{code}` (HTML) → `/items` → `/thumb/{id}` → `/preview/{id}` → `/asset/{id}` → `/asset/{id}/playback` (multiple `206` seeks) → `/archive` (manifest) → every `/archive/{token}/{part}` — and asserts the unsupported-share migration outcomes (`410`, not `500`). Add a browser test (headless) asserting zero CSP violations across gallery load, lightbox open, video slide, zoom, navigation.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd agent && go test ./internal/direct/ -run Parity -v`
Expected: FAIL (until Tasks 1–8 are all in place; this task is the integration gate).

- [ ] **Step 3: Implement / wire**

Implement the test harness (fake backend with deterministic assets + a multi-part archive + a video that honors byte ranges). No production code changes expected — if the suite reveals a gap, fix it in the owning task's file and note it.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd agent && go test ./internal/direct/ -run Parity -v`
Expected: PASS — this is the gate that unblocks milestone 3c.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/direct/parity_test.go
git commit -m "test(direct): browser-level parity suite gating v1 deletion"
```

---

## Milestone 3b — `control/` Rename

### Task 10: Rename `signaling-server/` → `control/`

**Files:**
- Modify: `signaling-server/go.mod` (module path), every `sharebridge/server` import, `deploy-testing.sh` → `control/deploy-testing.sh` (reduce `scp web/` to the four retained pages), `deploy.sh`, `signaling-server/docs/testing-server.md` → `control/docs/`, CI workflows, README.

**Interfaces:**
- Consumes: the module path `sharebridge/server` (`signaling-server/go.mod:1`) and all its importers.
- Produces: module `sharebridge/control`, dir `control/`.

- [ ] **Step 1: Apply the mechanical rename**

```bash
cd /Users/ali/Git/ShareBridge/.worktrees/phase-3
git mv signaling-server control
sed -i '' 's#sharebridge/server#sharebridge/control#g' $(grep -rl 'sharebridge/server' control --include='*.go' --include='go.mod')
```

Update `deploy-testing.sh` (keep copying only `home.html login.html register.html account.html`), `deploy.sh`, docs, CI, README references to the old path.

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

> **Gate:** Do not start Task 11 until Task 9's parity suite passes.

### Task 11: Delete the v1 transport

**Files:**
- Delete (agent): `internal/transfer`, `internal/multilane`, `internal/relaychannel`, `internal/peer`, `internal/noise`.
- Delete (control): `internal/relay`, `internal/handler/relay_ws.go`, `internal/handler/browser_ws.go`, TURN/ICE wiring, `/ws/client` + `/ws/relay` + `/i/{key}` + `/join` + `/sessions/{code}` + web-asset routes, relay runtime config fields, and the §6 client-JS source + client-UI source rows of the §10 table.
- Delete (repo): `extensions/`, `scripts/update_streamsaver_vendor.sh`.
- Modify: `control/cmd/server/main.go` (route removal), `agent/internal/daemon/daemon.go` (remove peer/multilane/relay wiring + transport message handling), `agent/internal/signaling/client.go` (remove transport message handling).

**Interfaces:**
- Consumes: nothing (pure removal).
- Produces: a v1-free tree that still compiles.

- [ ] **Step 1: Enumerate the wiring before deleting**

Run `grep -rn 'transfer\|multilane\|relaychannel\|internal/peer\|internal/noise\|internal/relay\|browser_ws\|TURN\|ICE' agent/cmd agent/internal/daemon control/cmd control/internal/handler --include='*.go' | grep -v _test.go` and record every reference (routes, handlers, config fields, WS message types, daemon fields/functions, tests). This is the deletion inventory.

- [ ] **Step 2: Delete the listed trees + files**

Remove the enumerated files and the recorded references. Remove transport message handling from the kept signaling client. Keep `immich`, `cloudwebdav`, `signaling`, `direct`, `cert`, `daemon` (minus v1 wiring), `store`, `web`, `config`.

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

- **Spec coverage:** §3.1→Task 1; §3/§4.1–4.5→Tasks 2–4; §4.6→Task 5; §4.7→Task 6; §4.8/§6→Task 7; §8/§10/§13→Task 8; §12.2→Task 9; §9→Task 10; §10→Task 11. §5.2 snapshot hydration + §11.1 download accounting are folded into Tasks 2/5/8 — confirm no gap.
- **Placeholder scan:** no TBD/TODO; every code step has actual code or an exact command.
- **Type consistency:** `ContentBackend`, `ContentSession`, `Ledger`, `Resolver`, `ArchiveTransaction`, `ParseRange` are defined once (Task 2/3/5) and referenced by name in later tasks.
