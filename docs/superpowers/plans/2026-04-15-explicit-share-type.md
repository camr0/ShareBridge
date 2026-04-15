# Explicit Share Type Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Require explicit `share_type` for all new shares, persist it with sessions, remove WebDAV fallback/caching behavior, and update both extensions plus the agent web UI to supply the correct type.

**Architecture:** The agent becomes the source of truth for share backend type by validating and persisting `share_type` at session creation time. The WebDAV layer exposes explicit backend-specific client construction instead of probing both paths. OpenCloud and Nextcloud extensions send a fixed type automatically, while the generic agent UI asks the human to choose.

**Tech Stack:** Go, HTMX form handlers, embedded HTML templates, JSON session persistence, TypeScript/Vue in `extensions/opencloud`, TypeScript/Vue/PHP in `extensions/nextcloud`.

---

## File Map

**Agent / main branch**
- Modify: `agent/internal/cloudwebdav/client.go` — replace fallback/caching client with explicit backend selection
- Modify: `agent/internal/cloudwebdav/client_test.go` — cover backend-specific constructors and remove fallback expectations
- Modify: `agent/internal/daemon/daemon.go` — require `share_type`, persist it, reject legacy sessions without it
- Modify: `agent/internal/daemon/daemon_test.go` — validate create/load behavior around `share_type`
- Modify: `agent/internal/store/store.go` — add `ShareType` to persisted sessions
- Modify: `agent/internal/store/store_test.go` — cover round-trip persistence of `ShareType`
- Modify: `agent/internal/web/api_v1.go` — require `share_type` in JSON API
- Modify: `agent/internal/web/api_v1_test.go` — reject missing/invalid `share_type`
- Modify: `agent/internal/web/api.go` — parse `share_type` from the web form
- Modify: `agent/internal/web/templates/share-form.html` — add explicit selector to create-share UI
- Modify: `agent/cmd/agent/main.go` — pass `share_type` in any direct create-session path that still exists

**OpenCloud extension / main branch**
- Modify: `extensions/opencloud/src/composables/useAgentClient.ts` — include `share_type: "opencloud"` in create-share requests
- Modify: `extensions/opencloud/src/composables/useAgentClient.test.ts` — assert fixed OpenCloud type is sent
- Modify: `extensions/opencloud/src/components/CreateShareModal.vue` — no new UI; ensure payload wiring stays correct if params are built locally
- Modify: `extensions/opencloud/src/components/CreateShareModal.test.ts` — cover the outgoing payload if needed

**Nextcloud extension / slice-12 worktree**
- Modify: `.worktrees/slice-12/extensions/nextcloud/src/composables/useAgentClient.ts` — include `share_type: "nextcloud"` in create-share requests
- Modify: `.worktrees/slice-12/extensions/nextcloud/src/composables/useAgentClient.test.ts` — assert fixed Nextcloud type is sent
- Modify: `.worktrees/slice-12/extensions/nextcloud/src/components/CreateShareModal.vue` — no user-facing selector; ensure payload wiring stays correct if params are built locally
- Modify: `.worktrees/slice-12/extensions/nextcloud/src/components/CreateShareModal.test.ts` — cover the outgoing payload if needed

## Task 1: Add `share_type` to agent persistence and runtime session state

**Files:**
- Modify: `agent/internal/store/store.go`
- Modify: `agent/internal/store/store_test.go`
- Modify: `agent/internal/daemon/daemon.go`
- Modify: `agent/internal/daemon/daemon_test.go`

- [ ] **Step 1: Write the failing store test for `ShareType` round-trip**

Add a persisted field expectation in `agent/internal/store/store_test.go` using the existing save/load pattern:

```go
entry := SessionEntry{
	Code:      "abc123",
	ShareURL:  "https://cloud.example.com/s/token",
	ShareType: "opencloud",
	ExpiresAt: time.Now().Add(time.Hour),
	CreatedAt: time.Now(),
}
if err := st.SaveSession(entry); err != nil {
	t.Fatalf("SaveSession() error: %v", err)
}

got := st.GetSession("abc123")
if got == nil {
	t.Fatal("expected session")
}
if got.ShareType != "opencloud" {
	t.Fatalf("ShareType = %q, want opencloud", got.ShareType)
}
```

- [ ] **Step 2: Run the store test to verify it fails**

Run: `go test ./internal/store -run ShareType -count=1`
Expected: FAIL because `SessionEntry` does not yet contain `ShareType`.

- [ ] **Step 3: Add `ShareType` to the persisted session model**

Update `agent/internal/store/store.go`:

```go
type SessionEntry struct {
	Code         string    `json:"code"`
	ShareURL     string    `json:"share_url"`
	ShareType    string    `json:"share_type"`
	FileID       string    `json:"file_id,omitempty"`
	Password     string    `json:"password,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	MaxDownloads int       `json:"max_downloads,omitempty"`
	Downloads    int       `json:"downloads"`
	RelayOnly    bool      `json:"relay_only"`
	CreatedAt    time.Time `json:"created_at"`
}
```

- [ ] **Step 4: Run the store test to verify it passes**

Run: `go test ./internal/store -run ShareType -count=1`
Expected: PASS

- [ ] **Step 5: Write the failing daemon test for legacy sessions without `share_type`**

Add a load-path test in `agent/internal/daemon/daemon_test.go`:

```go
persisted := store.SessionEntry{
	Code:      "legacy123",
	ShareURL:  "https://cloud.example.com/s/token",
	ShareType: "",
	ExpiresAt: time.Now().Add(time.Hour),
	CreatedAt: time.Now(),
}
// Save in mock store, start daemon load, assert session is skipped
```

Expected assertions:
- no active session registered in `d.sessions`
- no `RegisterShare` call for that code

- [ ] **Step 6: Run the daemon test to verify it fails**

Run: `go test ./internal/daemon -run Legacy -count=1`
Expected: FAIL because load currently accepts sessions without `share_type`.

- [ ] **Step 7: Persist and enforce `ShareType` in daemon session state**

Update `agent/internal/daemon/daemon.go`:

```go
type Session struct {
	Code         string
	ShareURL     string
	ShareType    string
	FileID       string
	Password     string
	ExpiresAt    time.Time
	MaxDownloads int
	Downloads    int
	RelayOnly    bool
	CreatedAt    time.Time
	webdavClient *cloudwebdav.Client
	peers        map[string]*peer.Peer
	mu           sync.Mutex
}
```

And when loading persisted sessions:

```go
if entry.ShareType == "" {
	log.Printf("warning: skipping legacy session %s: missing share_type", entry.Code)
	continue
}
```

- [ ] **Step 8: Run the daemon test to verify it passes**

Run: `go test ./internal/daemon -run Legacy -count=1`
Expected: PASS

- [ ] **Step 9: Commit**

```bash
git add agent/internal/store/store.go agent/internal/store/store_test.go agent/internal/daemon/daemon.go agent/internal/daemon/daemon_test.go
git commit -m "Persist explicit share type in sessions"
```

## Task 2: Replace WebDAV fallback/caching with explicit backend constructors

**Files:**
- Modify: `agent/internal/cloudwebdav/client.go`
- Modify: `agent/internal/cloudwebdav/client_test.go`
- Modify: `agent/internal/daemon/daemon.go`

- [ ] **Step 1: Write the failing client tests for explicit backend construction**

Add constructor-level tests in `agent/internal/cloudwebdav/client_test.go`:

```go
func TestNewOpenCloud_UsesPublicFilesEndpoint(t *testing.T) {
	c, err := New("opencloud", "https://cloud.example.com/s/token", []string{"cloud.example.com"}, "")
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if got := c.baseURL(); got != "https://cloud.example.com/remote.php/dav/public-files/token" {
		t.Fatalf("baseURL = %q", got)
	}
}

func TestNewNextcloud_UsesPublicDAVEndpoint(t *testing.T) {
	c, err := New("nextcloud", "https://nc.example.com/s/token", []string{"nc.example.com"}, "")
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if got := c.baseURL(); got != "https://nc.example.com/public.php/dav/files/token" {
		t.Fatalf("baseURL = %q", got)
	}
}
```

- [ ] **Step 2: Run the client tests to verify they fail**

Run: `go test ./internal/cloudwebdav -run 'TestNew(OpenCloud|Nextcloud)' -count=1`
Expected: FAIL because the constructor does not accept explicit backend type yet.

- [ ] **Step 3: Refactor `cloudwebdav.Client` to use one backend-specific endpoint**

Replace endpoint list/fallback state with one explicit endpoint:

```go
type Backend string

const (
	BackendOpenCloud Backend = "opencloud"
	BackendNextcloud Backend = "nextcloud"
)

type Client struct {
	baseURL    string
	selfPath   string
	token      string
	password   string
	httpClient *http.Client
}
```

Constructor shape:

```go
func New(shareType, shareURL string, allowedHosts []string, password string) (*Client, error) {
	// validate host and token
	switch Backend(shareType) {
	case BackendOpenCloud:
		baseURL = fmt.Sprintf("https://%s/remote.php/dav/public-files/%s", u.Host, token)
		selfPath = "/remote.php/dav/public-files/" + token
	case BackendNextcloud:
		baseURL = fmt.Sprintf("https://%s/public.php/dav/files/%s", u.Host, token)
		selfPath = "/public.php/dav/files/" + token
	default:
		return nil, fmt.Errorf("unsupported share type %q", shareType)
	}
}
```

Remove:
- endpoint slice
- fallback request logic
- preferred endpoint cache

- [ ] **Step 4: Run the constructor tests to verify they pass**

Run: `go test ./internal/cloudwebdav -run 'TestNew(OpenCloud|Nextcloud)' -count=1`
Expected: PASS

- [ ] **Step 5: Update daemon call sites to pass the explicit type**

In `agent/internal/daemon/daemon.go`, construct clients with:

```go
webdavClient, err := cloudwebdav.New(shareType, shareURL, allowedHosts, password)
```

Use `entry.ShareType` on reload and the validated request field for new sessions.

- [ ] **Step 6: Run the full client and daemon suites**

Run: `go test ./internal/cloudwebdav ./internal/daemon -count=1`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add agent/internal/cloudwebdav/client.go agent/internal/cloudwebdav/client_test.go agent/internal/daemon/daemon.go
git commit -m "Use explicit WebDAV backend types"
```

## Task 3: Require `share_type` in agent APIs and web UI

**Files:**
- Modify: `agent/internal/web/api_v1.go`
- Modify: `agent/internal/web/api_v1_test.go`
- Modify: `agent/internal/web/api.go`
- Modify: `agent/internal/web/templates/share-form.html`
- Modify: `agent/cmd/agent/main.go`

- [ ] **Step 1: Write the failing API test for missing `share_type`**

Add to `agent/internal/web/api_v1_test.go`:

```go
body := `{"share_url":"https://oc.example.com/s/xyz"}`
req := httptest.NewRequest("POST", "/api/v1/shares", bytes.NewBufferString(body))
req.Header.Set("Content-Type", "application/json")
rec := httptest.NewRecorder()

ws.handleCreateShareV1(rec, req)

if rec.Code != http.StatusBadRequest {
	t.Fatalf("status = %d, want 400", rec.Code)
}
```

- [ ] **Step 2: Run the API test to verify it fails**

Run: `go test ./internal/web -run ShareType -count=1`
Expected: FAIL because `share_type` is not required yet.

- [ ] **Step 3: Add `share_type` validation to JSON and form handlers**

In `agent/internal/web/api_v1.go` request struct:

```go
type createShareRequest struct {
	ShareURL     string `json:"share_url"`
	ShareType    string `json:"share_type"`
	Password     string `json:"password"`
	ExpiryHours  int    `json:"expiry_hours"`
	MaxDownloads int    `json:"max_downloads"`
	RelayOnly    bool   `json:"relay_only"`
}
```

Validation:

```go
if req.ShareType != "opencloud" && req.ShareType != "nextcloud" {
	writeValidationError(...)
	return
}
```

In `agent/internal/web/api.go` form parsing:

```go
shareType := r.FormValue("share_type")
```

In `agent/internal/web/templates/share-form.html`:

```html
<label for="share-type">Share Type</label>
<select id="share-type" name="share_type" required>
  <option value="opencloud" selected>OpenCloud</option>
  <option value="nextcloud">Nextcloud</option>
</select>
```

- [ ] **Step 4: Run the web suite to verify it passes**

Run: `go test ./internal/web -count=1`
Expected: PASS

- [ ] **Step 5: Update any direct create-session path in `agent/cmd/agent/main.go`**

Where the CLI or daemon delegation constructs create-share requests, pass `share_type` explicitly or fail early if the path cannot provide one. Do not leave an implicit default in API-facing code.

- [ ] **Step 6: Commit**

```bash
git add agent/internal/web/api_v1.go agent/internal/web/api_v1_test.go agent/internal/web/api.go agent/internal/web/templates/share-form.html agent/cmd/agent/main.go
git commit -m "Require explicit share type in agent create flows"
```

## Task 4: Update OpenCloud extension to send `opencloud`

**Files:**
- Modify: `extensions/opencloud/src/composables/useAgentClient.ts`
- Modify: `extensions/opencloud/src/composables/useAgentClient.test.ts`
- Modify: `extensions/opencloud/src/components/CreateShareModal.vue`
- Modify: `extensions/opencloud/src/components/CreateShareModal.test.ts`

- [ ] **Step 1: Write the failing extension test**

In `extensions/opencloud/src/composables/useAgentClient.test.ts`, assert:

```ts
expect(axios.post).toHaveBeenCalledWith(
	expect.any(String),
	expect.objectContaining({
		share_type: 'opencloud',
	}),
)
```

- [ ] **Step 2: Run the targeted test to verify it fails**

Run: `npm test -- useAgentClient.test.ts`
Expected: FAIL because `share_type` is absent.

- [ ] **Step 3: Add the fixed OpenCloud type to create-share payloads**

Update `extensions/opencloud/src/composables/useAgentClient.ts`:

```ts
const response = await axios.post(url, {
	...params,
	share_type: 'opencloud',
})
```

Keep the extension UI unchanged.

- [ ] **Step 4: Run extension tests to verify they pass**

Run: `npm test -- useAgentClient.test.ts CreateShareModal.test.ts`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add extensions/opencloud/src/composables/useAgentClient.ts extensions/opencloud/src/composables/useAgentClient.test.ts extensions/opencloud/src/components/CreateShareModal.vue extensions/opencloud/src/components/CreateShareModal.test.ts
git commit -m "Send explicit opencloud share type"
```

## Task 5: Update Slice 12 Nextcloud extension to send `nextcloud`

**Files:**
- Modify: `.worktrees/slice-12/extensions/nextcloud/src/composables/useAgentClient.ts`
- Modify: `.worktrees/slice-12/extensions/nextcloud/src/composables/useAgentClient.test.ts`
- Modify: `.worktrees/slice-12/extensions/nextcloud/src/components/CreateShareModal.vue`
- Modify: `.worktrees/slice-12/extensions/nextcloud/src/components/CreateShareModal.test.ts`

- [ ] **Step 1: Write the failing Nextcloud extension test**

In `.worktrees/slice-12/extensions/nextcloud/src/composables/useAgentClient.test.ts`, assert:

```ts
expect(axios.post).toHaveBeenCalledWith(
	expect.any(String),
	expect.objectContaining({
		share_type: 'nextcloud',
	}),
)
```

- [ ] **Step 2: Run the targeted Nextcloud test to verify it fails**

Run: `npm test -- useAgentClient.test.ts`
Expected: FAIL because `share_type` is absent.

- [ ] **Step 3: Add the fixed Nextcloud type to create-share payloads**

Update `.worktrees/slice-12/extensions/nextcloud/src/composables/useAgentClient.ts`:

```ts
const response = await axios.post(url, {
	...params,
	share_type: 'nextcloud',
})
```

Keep the Nextcloud extension UI unchanged.

- [ ] **Step 4: Run Nextcloud verification**

Run:
- `npm test`
- `npm run typecheck`
- `npm run build`

Expected: PASS (build may keep existing asset-size warnings only)

- [ ] **Step 5: Commit in the slice worktree**

```bash
git -C .worktrees/slice-12/extensions/nextcloud add src/composables/useAgentClient.ts src/composables/useAgentClient.test.ts src/components/CreateShareModal.vue src/components/CreateShareModal.test.ts
git -C .worktrees/slice-12/extensions/nextcloud commit -m "Send explicit nextcloud share type"
```

## Task 6: Final verification and cleanup

**Files:**
- Modify: any touched files from Tasks 1-5

- [ ] **Step 1: Run full agent verification on `main`**

Run: `go test ./... -count=1`
Expected: PASS

- [ ] **Step 2: Manual agent web UI check**

Verify:
- create OpenCloud share via web UI selector
- create Nextcloud share via web UI selector
- missing `share_type` cannot be submitted through the form

- [ ] **Step 3: Manual extension checks**

Verify:
- OpenCloud extension creates a share successfully
- Nextcloud extension creates a share successfully
- daemon restart skips legacy sessions without `share_type`

- [ ] **Step 4: Review leftover fallback code**

Confirm these are gone:
- endpoint list probing
- preferred endpoint cache
- any tests that expect automatic fallback during normal operation

- [ ] **Step 5: Commit any final cleanup**

```bash
git status --short
```

If any cleanup edits were needed, commit them with a focused message.
