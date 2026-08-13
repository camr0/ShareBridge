# Immich Duration Compatibility Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Allow ShareBridge to synchronize shared links from both legacy and current Immich versions when asset durations use different JSON representations.

**Architecture:** Add a package-local duration value that owns JSON compatibility and conversion to seconds. Keep `Asset`, `GalleryItem`, and downstream transfer behavior otherwise unchanged, and prove compatibility at the HTTP polling and gallery conversion boundaries.

**Tech Stack:** Go, `encoding/json`, `net/http/httptest`, Testify

---

### Task 1: Reproduce Current Immich Polling Failure

**Files:**
- Modify: `agent/internal/immich/client_test.go`

- [ ] **Step 1: Write the failing polling regression test**

Add `TestPollSharesAcceptsNumericDurationMilliseconds`, whose test server writes a raw `/api/shared-links` response containing `"duration":94500`, then assert that `PollShares` succeeds and retains a 94.5-second duration.

- [ ] **Step 2: Run the focused test and verify RED**

Run: `go test ./internal/immich -run TestPollSharesAcceptsNumericDurationMilliseconds -count=1`

Expected: FAIL because `encoding/json` cannot unmarshal a number into the current string field.

### Task 2: Add Dual-Format Duration Decoding

**Files:**
- Modify: `agent/internal/immich/client.go`
- Modify: `agent/internal/immich/client_test.go`

- [ ] **Step 1: Implement the minimal compatible type**

Define an `assetDuration` type with `UnmarshalJSON` support for `null`, JSON strings, and finite non-negative JSON numbers. Store legacy strings separately from numeric milliseconds and expose a `seconds() *float64` method that delegates strings to `parseDurationSeconds` and divides milliseconds by 1000.

- [ ] **Step 2: Use the type at the conversion boundary**

Change `Asset.Duration` to `assetDuration` and change gallery conversion from `parseDurationSeconds(asset.Duration)` to `asset.Duration.seconds()`.

- [ ] **Step 3: Run the focused test and verify GREEN**

Run: `go test ./internal/immich -run TestPollSharesAcceptsNumericDurationMilliseconds -count=1`

Expected: PASS.

- [ ] **Step 4: Add compatibility cases**

Update existing test fixtures to construct legacy values through a helper, and add table-driven JSON tests for a legacy timestamp string, integer milliseconds, `null`, and malformed input. Assert both accepted representations produce the expected seconds and malformed input returns an error.

- [ ] **Step 5: Run Immich package tests**

Run: `go test ./internal/immich -count=1`

Expected: PASS with zero failures.

### Task 3: Verify and Publish

**Files:**
- Modify: `agent/internal/immich/client.go`
- Modify: `agent/internal/immich/client_test.go`
- Create: `docs/superpowers/specs/2026-08-12-immich-duration-compatibility-design.md`
- Create: `docs/superpowers/plans/2026-08-12-immich-duration-compatibility.md`

- [ ] **Step 1: Format and inspect**

Run: `gofmt -w internal/immich/client.go internal/immich/client_test.go && git diff --check`

Expected: no output from `git diff --check`.

- [ ] **Step 2: Run the complete agent test suite**

Run: `go test ./... -count=1`

Expected: PASS with zero failing packages.

- [ ] **Step 3: Build the agent**

Run: `go build ./cmd/agent`

Expected: exit status 0.

- [ ] **Step 4: Commit only scoped files**

Run: `git add agent/internal/immich/client.go agent/internal/immich/client_test.go docs/superpowers/specs/2026-08-12-immich-duration-compatibility-design.md docs/superpowers/plans/2026-08-12-immich-duration-compatibility.md && git commit -m "fix: support numeric Immich asset durations"`

Expected: one commit containing only the compatibility fix, tests, design, and plan.

- [ ] **Step 5: Push the requested branch**

Run: `git push origin main`

Expected: `origin/main` advances and the repository's GHCR workflow is triggered.
