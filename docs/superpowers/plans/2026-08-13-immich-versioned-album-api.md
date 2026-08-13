# Immich Versioned Album API Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Load complete Immich albums through the correct version-specific API, including albums larger than 1,000 assets.

**Architecture:** Cache server version detection on each Immich client and route album asset loading by major version. Implement v3 metadata-search pagination as a focused helper while retaining legacy and capability fallback paths.

**Tech Stack:** Go, `encoding/json`, `net/http/httptest`, Testify

---

### Task 1: Reproduce v3 Large-Album Failure

**Files:**
- Modify: `agent/internal/immich/client_test.go`

- [ ] Add an HTTP test where `/api/server/version` reports major 3, album details omit assets, and three `/api/search/metadata` pages return 1,000, 1,000, and 453 assets.
- [ ] Assert `ListGallery` returns all 2,453 items in order and the test fails before implementation.

### Task 2: Implement Cached Version Routing and Pagination

**Files:**
- Modify: `agent/internal/immich/client.go`
- Modify: `agent/internal/immich/client_test.go`

- [ ] Add client-scoped, concurrency-safe lazy version detection through `/api/server/version`.
- [ ] Route major version 3 and newer to paginated `/api/search/metadata` requests with `albumIds`, `page`, `size: 1000`, and `withExif: true`.
- [ ] Follow and validate `nextPage`; attach shared-link/password parameters to every page.
- [ ] Retain legacy album loading for earlier versions and legacy-then-search capability fallback when detection fails.
- [ ] Add explicit version, strategy, page-count, and total-count logs.
- [ ] Add tests for cache behavior, v2 selection, failed detection fallback, empty results, password propagation, and malformed/repeated pagination.
- [ ] Run `go test ./internal/immich -count=1` until green.

### Task 3: Verify and Publish

**Files:**
- Modify: `agent/internal/immich/client.go`
- Modify: `agent/internal/immich/client_test.go`
- Create: `docs/superpowers/specs/2026-08-13-immich-versioned-album-api-design.md`
- Create: `docs/superpowers/plans/2026-08-13-immich-versioned-album-api.md`

- [ ] Run `gofmt`, `git diff --check`, `go test ./... -count=1`, `go vet ./...`, and `go build ./cmd/agent`.
- [ ] Independently review the scoped diff.
- [ ] Commit only scoped files, push `main`, and confirm the Agent Container workflow publishes both `latest` and commit-specific GHCR tags.
