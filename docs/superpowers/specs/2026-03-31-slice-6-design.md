# Slice 6 — Nested Folder Support Design

**Date:** 2026-03-31

---

## Goal

Allow users to browse into subdirectories of an OpenCloud share via a breadcrumb navigation UI, downloading individual files at any depth.

## Architecture

Folder navigation is **lazy** — the browser fetches one directory at a time (Depth:1 PROPFIND) when the user clicks into it. The browser owns the current-path state as an array of path segments. The agent is stateless with respect to navigation — it processes each `list_request` independently.

The signaling server never sees file paths or folder names. The `path` field in protocol messages travels over the WebRTC DataChannel (peer-to-peer), and WebDAV calls go directly from the agent to OpenCloud.

---

## Protocol Changes

All changes are additive. No protocol version bump for this slice (versioning deferred to the daemon slice, when agent and browser may be deployed independently).

### `list_request` (browser → agent)

Gains optional `path` field (default `""`= share root):

```json
{ "type": "list_request", "password": "secret", "path": "docs/reports" }
```

### `file_list` (agent → browser)

`FileInfo` entries gain `isDir` field. Folders have no `size` or `mimeType`:

```json
{
  "type": "file_list",
  "files": [
    { "name": "docs", "size": 0, "mimeType": "", "isDir": true },
    { "name": "README.txt", "size": 1024, "mimeType": "text/plain", "isDir": false }
  ]
}
```

`SHA1` remains `json:"-"` — never sent in `file_list`.

### `file_request` (browser → agent)

Gains `path` field — full path from share root:

```json
{ "type": "file_request", "path": "docs/reports/Q1.pdf" }
```

The existing `name` field is dropped from `file_request` (name is derived from `path.Base(path)`).

### `file_header` (agent → browser)

Unchanged — `name` is always the basename (what the browser saves the download as):

```json
{ "type": "file_header", "name": "Q1.pdf", "size": 2097152, "mimeType": "application/pdf", "sha1": "abc123..." }
```

---

## Agent: `opencloud/client.go`

### `FileInfo`

```go
type FileInfo struct {
    Name        string `json:"name"`
    Size        int64  `json:"size"`
    ContentType string `json:"mimeType"`
    IsDir       bool   `json:"isDir"`
    SHA1        string `json:"-"`
}
```

### `ListFiles(path string)`

- Builds request URL: `c.baseURL` if `path == ""`, else `c.baseURL + "/" + path`
- Sends Depth:1 PROPFIND with existing `propfindBody`
- Identifies the "self" entry (the directory being listed) by comparing each response entry's cleaned href to the expected self-path: `"/remote.php/dav/public-files/" + token` for root, or `"/remote.php/dav/public-files/" + token + "/" + path` for subpaths
- Self-entry (collection whose href matches the self-path): skipped
- Other collections: returned as `FileInfo{Name: basename, IsDir: true}`
- Files: returned as before, with `IsDir: false`

### `GetFile(filePath string, w io.Writer)`

Signature unchanged in behavior — `filePath` is now the full relative path (e.g. `"docs/reports/Q1.pdf"`). URL = `c.baseURL + "/" + filePath`. No code change needed; manager passes the full path.

---

## Agent: `transfer/manager.go`

### Message parsing

`HandleMessage` struct gains `Path string \`json:"path"\``.

### Authentication state

Add `authenticated atomic.Bool` to `Manager`. It starts `true` for password-free shares (set in `NewManager` when `password == ""`), and is set to `true` on the first successful `list_request` auth.

The auth check is centralized in `HandleMessage` — before the `switch` statement, any message type other than `list_request` is rejected if `!authenticated.Load()`. This means new message types added in future slices are automatically protected without needing per-handler checks.

```go
// Gate everything except list_request behind authentication.
// Note: list_request is now dual-purpose (auth + folder navigation), which is
// a known awkwardness. A dedicated auth message on hello would be cleaner —
// deferred to the daemon slice when the protocol gets a version field anyway.
if msg.Type != "list_request" && !m.authenticated.Load() {
    m.sendError("authentication required")
    return
}
```

### `handleListRequest(password, path string)`

- If `m.password != ""`: validate password on every call; set `authenticated = true` on success
- Passes `path` to `client.ListFiles(path)`

### `handleFileRequest(filePath string)`

1. Reject if `strings.Contains(filePath, "..")` — returns `error` message (defense-in-depth; OpenCloud's own auth is the real protection)
2. Derive `dir = path.Dir(filePath)`, `name = path.Base(filePath)` (using Go's `path` package, not `filepath` — share paths use forward slashes on all platforms)
3. Call `client.ListFiles(dir)` to validate file exists
4. Send `file_header` with `Name: name` (basename only)
5. Stream with `client.GetFile(filePath)`

---

## Browser: `app.js`

### New state

```js
let currentPath = [];    // e.g. [] = root, ["docs", "reports"] = two levels deep
let sessionPassword = ''; // cached after user submits; cleared on resetUI
```

### `requestFileList(path)`

```js
function requestFileList(path) {
  dc.send(JSON.stringify({ type: 'list_request', password: sessionPassword, path }));
}
```

Called on initial auth (sets `sessionPassword` first) and on every folder navigation.

### `submitPassword()`

```js
function submitPassword() {
  sessionPassword = document.getElementById('password-input').value;
  requestFileList('');
}
```

For unprotected shares, `sessionPassword` stays `''` and is sent as-is.

`handleHello` (for unprotected shares) calls `requestFileList('')` — the `''` now means path=root (not password), since `sessionPassword` is sourced from the module-level variable.

### `renderFileList(files)`

- Sorts: folders first (by `isDir`), then alphabetically within each group
- Folder items: `📁` prefix, clicking pushes segment to `currentPath`, calls `requestFileList(currentPath.join("/"))`, re-renders breadcrumb
- File items: unchanged click handler, but `file_request` now sends `path: [...currentPath, file.name].join("/")`
- Renders breadcrumb before the list (see below)

### Breadcrumb

```js
function renderBreadcrumb() {
  const breadcrumb = document.getElementById('breadcrumb');
  if (currentPath.length === 0) {
    breadcrumb.classList.add('hidden');
    return;
  }
  breadcrumb.classList.remove('hidden');
  const parts = [
    { label: 'Share root', index: -1 },
    ...currentPath.map((seg, i) => ({ label: seg, index: i })),
  ];
  breadcrumb.innerHTML = parts.map((p, i) => {
    const isLast = i === parts.length - 1;
    if (isLast) return `<span class="breadcrumb-current">${escapeHtml(p.label)}</span>`;
    return `<span class="breadcrumb-link" onclick="navigateTo(${p.index})">${escapeHtml(p.label)}</span>`;
  }).join('<span class="breadcrumb-sep">›</span>');
}

function navigateTo(index) {
  // index -1 = root, 0 = first segment, etc.
  currentPath = index === -1 ? [] : currentPath.slice(0, index + 1);
  requestFileList(currentPath.join('/'));
}
```

### `requestFile(name)` → path-aware

```js
function requestFile(name) {
  if (isDownloading) { status('Download in progress, please wait'); return; }
  const fullPath = [...currentPath, name].join('/');
  dc.send(JSON.stringify({ type: 'file_request', path: fullPath }));
}
```

### `resetUI()`

Adds:
```js
currentPath = [];
sessionPassword = '';
```

---

## Browser: `index.html`

Add `#breadcrumb` div above `#file-list`:

```html
<div id="breadcrumb" class="hidden"></div>
<div id="file-list" class="hidden"></div>
```

CSS (Catppuccin Mocha, consistent with existing theme):

```css
#breadcrumb {
  margin-bottom: 8px;
  font-size: 0.85rem;
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 4px;
}
.breadcrumb-link {
  color: #89b4fa;
  cursor: pointer;
}
.breadcrumb-link:hover { text-decoration: underline; }
.breadcrumb-current { color: #cdd6f4; }
.breadcrumb-sep { color: #45475a; }
```

---

## Error Handling

| Scenario | Behaviour |
|---|---|
| `path` contains `..` | Agent returns `error` message; browser shows it in `#status` |
| File not found at path | Agent returns `error` message |
| Empty folder | `file_list` returns `[]`; browser shows "No files in share" with breadcrumb still visible |
| Folder clicked during active download | `isDownloading` guard — shows "Download in progress, please wait" |
| Connection lost mid-navigation | `resetUI()` clears `currentPath` and `sessionPassword`; user re-enters session code |

---

## Testing

- `TestListFiles_WithSubdirectories` — PROPFIND response containing both files and a subdirectory; assert `IsDir: true` on folder, `IsDir: false` on file, self-entry excluded
- `TestListFiles_EmptyFolder` — PROPFIND with only self-entry; assert empty slice returned
- `TestHandleFileRequest_NestedPath` — `file_request` with `path: "docs/file.txt"`; assert `file_header` name is `"file.txt"`, `GetFile` called with full path
- `TestHandleFileRequest_PathTraversal` — `path: "../escape"`; assert error returned, no `ListFiles` call made
- `TestHandleListRequest_Subpath` — `list_request` with `path: "docs"`; assert `ListFiles("docs")` called
- `TestHandleFileRequest_UnauthenticatedBlocked` — `file_request` sent before any `list_request` on a password-protected share; assert `error` returned, no `ListFiles` or `GetFile` call made

---

## Out of Scope (this slice)

- Concurrent downloads / downloads tray UI — deferred; noted in parallel downloads memory
- Protocol version field in `hello` — deferred to daemon slice
- Folder download (zip entire directory) — post-MVP
