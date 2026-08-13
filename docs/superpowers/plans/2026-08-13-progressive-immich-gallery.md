# Progressive Immich Gallery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep multi-thousand-item Immich albums responsive by rendering and transferring thumbnails in demand-driven 120-item windows.

**Architecture:** Retain complete gallery metadata but add a ranged thumbnail request protocol. The browser progressively appends grid windows and uses dynamic LightGallery metadata for full-album navigation independently of initially rendered grid DOM size.

**Tech Stack:** Go, WebRTC control/media lanes, vanilla JavaScript, IntersectionObserver, LightGallery, Go tests, Node test runner

---

### Task 1: Compatible Demand-Driven Thumbnail Protocol

**Files:**
- Modify: `agent/internal/transfer/manager.go`
- Modify: `agent/internal/transfer/manager_test.go`

- [ ] Add failing tests asserting gallery open advertises `thumbnailMode: "pull-v1"`, does not immediately fetch thumbnails, and falls back to the legacy eager stream plus `thumbnail_complete` after a configurable five-second grace period.
- [ ] Add a failing test sending `{"type":"thumbnail_batch_request","request_id":"thumb-2","start":120,"count":120}` and asserting only indices 120–239 are fetched and framed, followed by `{"type":"thumbnail_batch_complete","request_id":"thumb-2","start":120,"count":120,"sent":120,"failed":0}`.
- [ ] Add tests proving synchronized `waiting -> pull` or `waiting -> legacy` transitions: the first valid request wins and cancels fallback, invalid requests do not, and requests at/after legacy start are rejected without overlapping streams.
- [ ] Add validation tests for missing request IDs, negative starts, zero/counts above 120, ranges beyond the album, and a second request while a batch is active; assert final ranges clamp to the advertised album length.
- [ ] Add a 65,536-item boundary test proving metadata is consistently capped before uint16 thumbnail framing.
- [ ] Store gallery items and synchronized per-connection thumbnail mode/batch-active state on the manager. Implement bounded asynchronous batch delivery using the existing workers, count fetch and send failures, and keep the grace duration injectable in tests.
- [ ] Run `cd agent && gofmt -w internal/transfer/manager.go internal/transfer/manager_test.go && go test ./internal/transfer -count=1 && go test -race ./internal/transfer -count=1`.

### Task 2: Progressive Browser Grid and Dynamic Lightbox

**Files:**
- Modify: `signaling-server/web/src/gallery.js`
- Modify: `signaling-server/web/src/gallery.test.js`
- Modify: `signaling-server/web/src/app.js`
- Modify: `signaling-server/web/src/app.test.js`
- Modify: `signaling-server/web/index.html`

- [ ] Add failing controller tests proving a 2,453-item `pull-v1` list initially renders and requests only 120 items, while a legacy list sends no ranged request and still accepts eager thumbnail frames and `thumbnail_complete`.
- [ ] Add failing tests for sentinel/Load More expansion, request deduplication, and batch completion progress.
- [ ] Add a failing partial-failure test proving explicit `failed_indices` become terminal unavailable tiles without automatic retry; accept successful media frames that arrive after their correlated completion while ignoring failed, duplicate, and stale frames.
- [ ] Add failing tests proving clicking global item 130 opens LightGallery index 130 even when later windows are not yet rendered.
- [ ] Implement the `onThumbnailBatchRequest` callback in app control messaging.
- [ ] Implement append-only 120-item windows, observer setup/cleanup, Load More fallback, and queued/in-flight/complete range state. Do not start a second request until the current completion arrives. Reset all state on a new list or destroy.
- [ ] Initialize LightGallery on the stable container with `{dynamic:true, dynamicEl:[...]}`. Open clicked tiles with `openGallery(globalIndex)`; update `galleryItems[globalIndex]` directly for thumbnails/previews without `refresh()`.
- [ ] Keep `thumb:<id>` and `preview:<id>` URL ownership separate. Ensure thumbnail-to-preview upgrades preserve grid thumbnails; final partial windows clamp correctly; late/out-of-order completions do not advance state; first-pull legacy fallback frames survive cross-lane overtaking; observer absence leaves Load More usable; and replacement/destroy tears down LightGallery before revoking every URL exactly once.
- [ ] Run `cd signaling-server/web && npm test`.

### Task 3: Verification and Publication

**Files:**
- All files from Tasks 1–2
- Create: `docs/superpowers/specs/2026-08-13-progressive-immich-gallery-design.md`
- Create: `docs/superpowers/plans/2026-08-13-progressive-immich-gallery.md`

- [ ] Run formatting, complete Go tests, Go race tests for transfer, vet, agent build, and complete signaling-server tests.
- [ ] Independently review protocol correctness, mixed-version compatibility, bounds, cleanup, and large-album behavior.
- [ ] Commit scoped files, push `main`, confirm applicable GitHub workflows and GHCR publication, then verify the live 2,453-item share renders an initial bounded grid and progressively expands.
