# Slice 4 — Persistence + Stable Session Codes

**Date**: 2026-03-30
**Status**: Draft
**Reference**: [MVP Design](2026-03-30-mvp-design.md)

## Goal

Session codes survive agent restarts. If a share link has been bookmarked or shared with someone, it keeps working after the agent is restarted — even days or weeks later.

## Problem

Currently every agent restart generates a new random session code. Any share links distributed while the agent was previously running break immediately. For a share that may be active for weeks or months, agent restarts (reboots, updates, crashes) are routine and must not invalidate existing links.

## Decisions Summary

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Persistence format | JSON file at `~/.opencloudshare/sessions.json` | Simple, human-readable, no dependency |
| What is persisted | `shareURL → { sessionCode, downloadCount }` | Code for link stability; download count so max-downloads limit survives restarts |
| Stable AgentID | Not needed | Auth token already serves as stable agent identity |
| Code re-registration | Agent sends `preferred_code` to server; server honors it if available | Server restarts lose in-memory sessions; re-requesting recreates them with same code |
| Code conflict | Server falls back to new random code if preferred code is taken | Shouldn't happen in practice; agent updates its store with whatever code is returned |
| TTL on restored sessions | Unchanged — server default, not touched here | TTL sync with OpenCloud share expiry is deferred to a later slice |
| File permissions | `0600` | Session codes are somewhat sensitive (control access to shares) |
| Data directory | `OPENCLOUDSHARE_DATA_DIR` env var, fallback to `~/.opencloudshare` | Allows Docker users to mount a host volume; no new CLI flag needed |

## Architecture

### Agent-side persistence

New package `agent/internal/store`:

```
$OPENCLOUDSHARE_DATA_DIR/   # defaults to ~/.opencloudshare
  sessions.json             # shareURL → sessionCode map
```

`sessions.json` format:
```json
{
  "sessions": {
    "https://cloud.example.com/s/AbCdEfGh": { "code": "xk92mf1a", "download_count": 3 },
    "https://cloud.example.com/s/OtherTok": { "code": "zp44qr7b", "download_count": 0 }
  }
}
```

### Flow on agent start

```
1. Load sessions.json (or empty map if not found)
2. Look up shareURL → get stored code (may be empty on first run)
3. Call CreateSession(ctx, shareURL, storedCode)
4. Server returns code (same as requested, or new code if preferred was taken)
5. Save returned code to sessions.json for this shareURL
6. Display code to user
```

### Server-side change

`POST /api/v1/sessions` accepts an optional `preferred_code` field:

```json
{
  "share_url": "https://cloud.example.com/s/AbCdEfGh",
  "preferred_code": "xk92mf1a"
}
```

Session manager `Create` logic:
- If `preferred_code` is non-empty and not currently in use → create session with that exact code
- If `preferred_code` is in use by a different token → generate new random code
- If `preferred_code` is empty → generate new random code (existing behaviour)

Response is unchanged: `{"code": "xk92mf1a", "expires_at": "..."}`.

## Agent Changes

### New: `agent/internal/store/store.go`

```go
type Store struct {
    path string
}

func New() (*Store, error)                                      // resolves data dir (OPENCLOUDSHARE_DATA_DIR or ~/.opencloudshare), creates it if needed
func (s *Store) GetCode(shareURL string) string                 // returns "" if not found
func (s *Store) SetCode(shareURL string, code string) error
func (s *Store) GetDownloadCount(shareURL string) int               // returns 0 if not found
func (s *Store) IncrementDownloadCount(shareURL string) (int, error) // increments and persists, returns new count
```

- Read/write `sessions.json` on every call (no in-memory caching — file is tiny, simplifies concurrency)
- Create file with `0600` permissions
- If file is malformed JSON, return a fatal error — don't silently overwrite corrupted data; user should fix or delete the file manually

### Modified: `agent/internal/transfer/manager.go`

Add `OnDownloadComplete` callback, called at the end of `streamFile` where the download counter is incremented:

```go
OnDownloadComplete func() // called after each successful download (after chunk_end sent)
```

`main.go` wires this to `st.IncrementDownloadCount` so the count is persisted to disk.

### Modified: `agent/internal/signaling/client.go`

`CreateSession` signature change:

```go
// Before
func (s *Signaling) CreateSession(ctx context.Context, shareURL string, _ string) (string, error)

// After
func (s *Signaling) CreateSession(ctx context.Context, shareURL string, preferredCode string) (string, error)
```

The second parameter was already there (placeholder `""`). Now it's wired: if non-empty, included as `preferred_code` in the request body.

### Modified: `agent/cmd/agent/main.go`

```go
st, err := store.New()
if err != nil {
    return fmt.Errorf("session store: %w", err)
}

// In runShare, before runSession loop:
preferredCode := st.GetCode(shareURL)
// Restore persisted download count into transfer manager
mgr.downloads.Store(int32(st.GetDownloadCount(shareURL)))

// In runSession (or passed in):
code, err := sig.CreateSession(ctx, shareURL, preferredCode)
// After code returned:
if err := st.SetCode(shareURL, code); err != nil {
    return fmt.Errorf("save session code: %w", err)
}

// Wire download persistence: called by transfer.Manager after each successful download
tm.OnDownloadComplete = func() {
    if _, err := st.IncrementDownloadCount(shareURL); err != nil {
        log.Printf("warning: could not persist download count: %v", err)
    }
}
```

Store errors are fatal — agent exits with a clear error message rather than running without persistence. A user who doesn't check logs would incorrectly assume their share links are stable.

## Server Changes

### Modified: `signaling-server/internal/session/manager.go`

`Create` signature change:

```go
// Before
func (m *Manager) Create(token, shareURL string, ttl time.Duration) (*Session, error)

// After
func (m *Manager) Create(token, shareURL, preferredCode string, ttl time.Duration) (*Session, error)
```

Logic:
```go
code := preferredCode
if code == "" || m.isCodeTaken(code, token) {
    if code != "" {
        log.Printf("warning: requested code %s is already taken, assigning new code", code)
    }
    var err error
    code, err = generateCode()
    if err != nil { return nil, err }
}
```

`isCodeTaken(code, token string) bool`: returns true if a session with that code exists AND its token differs from the requesting agent's token. (Same agent re-registering the same code is fine.)

### Modified: `signaling-server/internal/handler/rest.go`

Request struct gains optional field:

```go
type createSessionRequest struct {
    ShareURL      string `json:"share_url"`
    TTL           int    `json:"ttl"`
    PreferredCode string `json:"preferred_code"` // optional
}
```

Pass `req.PreferredCode` to `sessions.Create(...)`.

## Known Limitations

**File locking is not implemented.** `sync.Mutex` protects against concurrent access within the agent process, but does not prevent the user from editing `sessions.json` while the agent is running. The atomic write (write to `.tmp`, then rename) prevents file corruption, but a manual edit made between two agent writes will be silently overwritten. Full file locking (`flock`) is deferred to the daemon slice, where a management UI and the agent process will both write to the same file.

**Code ownership is not durable.** The server's session store is in-memory. If agent A is offline and its session has expired (or the server restarted), that code is gone from memory. A malicious agent B who knows the code could request it as `preferred_code` and claim it, since the server sees it as unclaimed.

In practice this is hard to exploit accidentally (8 random chars ≈ 2.8 trillion combinations), but it is a real gap. The fix requires durable code ownership — a SQLite store that remembers "code X belongs to API key Y" even after expiry — which is scoped to Slice 6 (multi-tenancy + API keys).

## What Doesn't Change

- Session codes are still 8 random chars — no format change
- Server session TTL is unchanged (server default; TTL sync with OpenCloud is deferred)
- Max downloads counter is now persisted — this was explicitly deferred from Slice 3 until persistence was available
- No migration needed — first run after update simply generates new code and persists it

## Test Plan

### Unit tests

**`store` package:**
- `GetCode` on missing file returns `""`
- `SetCode` creates file with correct content and `0600` permissions
- `SetCode` then `GetCode` round-trips correctly
- Multiple shareURLs stored independently
- Malformed JSON file handled gracefully (returns `""`, no panic)

**`session.Manager`:**
- `Create` with empty `preferredCode` generates random code (existing behaviour)
- `Create` with `preferredCode` that is not taken uses that code
- `Create` with `preferredCode` taken by same token reuses it
- `Create` with `preferredCode` taken by different token falls back to random

### Integration smoke test

1. Start agent with a share URL → note code `XXXX`
2. Kill agent (Ctrl-C)
3. Restart agent with same share URL → code should still be `XXXX`
4. Browser navigates to share with code `XXXX` → file list loads

**After server restart:**

5. Kill signaling server
6. Restart signaling server
7. Kill and restart agent → code should still be `XXXX`
8. Browser confirms the code still works
