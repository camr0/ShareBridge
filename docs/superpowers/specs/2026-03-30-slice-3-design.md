# Slice 3 — CLI + Password + Max Downloads + Reconnect

**Date**: 2026-03-30
**Status**: Draft
**Reference**: [MVP Design](2026-03-30-mvp-design.md)

## Goal

Production-quality agent invocation: a real CLI, password-protected shares, download limits, and automatic reconnect so the agent survives transient network failures without manual restart.

## Decisions Summary

| Decision | Choice | Rationale |
|----------|--------|-----------|
| CLI library | `cobra` | Standard Go CLI library, extensible for future subcommands |
| TTL flag | None | OpenCloud share expiry is source of truth |
| Password validation | Agent only | Server is untrusted — never sees password content |
| Auth failure signaling | Generic event to server (no password) | Server can rate-limit without learning credentials |
| Agent-side auth banning | 3 failures → close DataChannel permanently | Limits brute force per connection, independent of server-side limiting |
| Password UX | `hello` message with `password_required` flag | Browser shows password field only when needed, no awkward failed first attempt |
| Max downloads | In-memory counter (resets on restart) | Simple, sufficient for MVP |
| Downloads counted | Successful only (after chunk_end sent) | Don't penalize users for connection drops |
| Persistence | None (deferred to Slice 4) | Fresh session code on each restart |
| Reconnect backoff | Exponential: 1s→2s→4s→8s→16s→30s cap, ±20% jitter | Standard approach, prevents thundering herd |

## CLI Design

```
opencloudshare share <url> [--password secret] [--max-downloads 10]
```

**Examples:**
```bash
opencloudshare share https://cloud.example.com/s/AbCdEfGh
opencloudshare share https://cloud.example.com/s/AbCdEfGh --password hunter2
opencloudshare share https://cloud.example.com/s/AbCdEfGh --max-downloads 5
```

**Output on start:**
```
Session ready — code: daxziivs
Open browser: http://localhost:8080  then enter code: daxziivs
Waiting for connections (Ctrl-C to stop)...
```

Flags:
- `--password` / `-p`: Optional. If set, browser must supply correct password before receiving file list.
- `--max-downloads` / `-n`: Optional. If set, share is disabled after N completed downloads (chunk_end sent). Default: unlimited.

## Component Design

### 1. CLI Entry Point (`agent/cmd/agent/`)

Replace `main.go` env-var-only setup with cobra command structure:

```go
// cmd/agent/main.go — cobra root + share subcommand
var rootCmd = &cobra.Command{Use: "opencloudshare"}

var shareCmd = &cobra.Command{
    Use:   "share <url>",
    Short: "Share an OpenCloud public share",
    Args:  cobra.ExactArgs(1),
    RunE:  runShare,
}
```

`runShare` reads flags, validates inputs, creates WebDAV client, starts reconnect loop.

Config struct gains `Password` and `MaxDownloads` fields — populated from flags, not env vars (env vars remain for backwards compat during development).

### 2. Password Authentication

**Handshake** — when the DataChannel opens, agent sends a `hello` message before any file list request:

```json
{"type": "hello", "password_required": true}
```

Browser shows a password input immediately if `password_required` is true; skips it if false. No failed first attempt, no guessing.

**Protocol** — `list_request` carries an optional password field:

```json
{"type": "list_request", "password": "hunter2"}
```

**Agent behavior:**
1. On DataChannel open → send `hello` with `password_required`.
2. If `--password` was set and `list_request.password` matches → proceed normally.
3. If `--password` was set and password is wrong or missing → increment failure counter, send `error: "incorrect password"` to browser, send `auth_failed` to server.
4. After 3 failures on the same connection → close the DataChannel permanently (no further retries accepted).
5. If no `--password` configured → ignore the field, proceed normally.

**Auth failure signal to server:**

Agent sends over the signaling WebSocket:
```json
{"type": "auth_failed", "session_id": "daxziivs"}
```

No password content. Server logs the event; rate limiting deferred to Slice 5.

### 3. Max Downloads (`agent/internal/transfer/`)

Counter on the `Manager`:

```go
type Manager struct {
    dc           DataChannel
    client       *opencloud.Client
    transfer     atomic.Bool
    maxDownloads int  // 0 = unlimited
    downloads    atomic.Int32
}
```

After `chunk_end` is sent, `downloads` is incremented. On next `list_request` or `file_request`, if `downloads >= maxDownloads`, send:
```json
{"type": "error", "message": "share has reached its download limit"}
```

Agent also signals the server to expire the session:
```json
{"type": "session_expired", "session_id": "daxziivs"}
```

Server marks session as expired; further join attempts get an error.

### 4. Reconnect Loop (`agent/internal/signaling/`)

New `Backoff` type for pure backoff calculation (unit-testable):

```go
type Backoff struct {
    initial time.Duration // 1s
    max     time.Duration // 30s
    factor  float64       // 2.0
    jitter  float64       // 0.2 (±20%)
    attempt int
}

func (b *Backoff) Next() time.Duration
func (b *Backoff) Reset()
```

`Next()` returns `min(initial * factor^attempt, max) * (1 ± jitter)`.

The reconnect loop in `main.go`:

```go
func reconnectLoop(ctx context.Context, cfg *Config, webdavClient *opencloud.Client) {
    b := signaling.NewBackoff()
    for {
        if err := runAgent(ctx, cfg, webdavClient); err != nil {
            log.Printf("disconnected: %v — retrying in %s", err, b.Next())
            select {
            case <-time.After(b.Next()):
            case <-ctx.Done():
                return
            }
        } else {
            b.Reset()
        }
    }
}
```

On each reconnect: full connect → register → create/re-register session → listen. No session code persistence (Slice 4).

## Protocol Changes

### New Browser → Agent messages

```json
// list_request with optional password
{"type": "list_request", "password": "hunter2"}
```

### New Agent → Server messages (signaling WebSocket)

```json
// Auth failure notification (no password content)
{"type": "auth_failed", "session_id": "daxziivs"}

// Session expired due to max downloads
{"type": "session_expired", "session_id": "daxziivs"}
```

### New Agent → Browser messages

```json
// Sent immediately when DataChannel opens
{"type": "hello", "password_required": true}

// Wrong password (up to 3 attempts, then DataChannel closed)
{"type": "error", "message": "incorrect password"}

// Max downloads reached
{"type": "error", "message": "share has reached its download limit"}
```

## Browser UI Changes

Password prompt flow:
1. DataChannel opens → agent sends `hello` with `password_required`
2. If `password_required: false` → browser sends `list_request` immediately (no password field shown)
3. If `password_required: true` → browser shows password input + submit button before sending `list_request`
4. User submits password → browser sends `list_request` with password
5. If agent responds `error: "incorrect password"` → show error, allow retry (up to 3 attempts)
6. After 3 failures → agent closes DataChannel, browser shows "too many incorrect attempts"
7. On success → show file list as normal

## Error Handling

| Scenario | Agent Behavior | Browser Behavior |
|----------|---------------|------------------|
| Wrong password (attempt 1-2) | Send `error: "incorrect password"`, signal server `auth_failed` | Show error, allow retry |
| Wrong password (attempt 3) | Send `error: "incorrect password"`, signal server `auth_failed`, close DataChannel | Show "too many incorrect attempts" |
| Max downloads reached | Send `error: "share has reached its download limit"`, signal server `session_expired` | Show message, no retry |
| Signaling disconnect | Log error, reconnect with backoff | No change — WebRTC DataChannel is independent of signaling; if DataChannel stays up the user sees nothing. If DataChannel also closes, existing `dc.onclose` handler shows "Connection closed" (Slice 2). |
| Reconnect fails repeatedly | Keep retrying up to 30s interval | No change — same as above; browser is unaware of signaling state |
| Ctrl-C | Graceful shutdown, close peer connections | Connection lost |

## Testing Strategy

**Unit Tests:**
- `signaling/backoff_test.go`: Next() values, cap enforcement, Reset(), jitter bounds
- `transfer/manager_test.go`: max-downloads counter increment and rejection (extend existing mock)

**Manual E2E Tests:**
1. `opencloudshare share <url>` → no password field shown, file list appears immediately
2. `opencloudshare share <url> --password secret` → password field shown, correct password shows file list
3. `opencloudshare share <url> --password secret` → 3 wrong passwords → DataChannel closed, browser shows "too many incorrect attempts"
4. `opencloudshare share <url> --max-downloads 2` → third download attempt rejected
3. Kill signaling server mid-session → agent reconnects automatically, prints new session code
4. Ctrl-C → clean shutdown

## File Structure

**New files:**
- `agent/internal/signaling/backoff.go` — Backoff type
- `agent/internal/signaling/backoff_test.go` — unit tests

**Modified files:**
- `agent/cmd/agent/main.go` — cobra CLI, reconnect loop
- `agent/internal/config/config.go` — Password, MaxDownloads fields
- `agent/internal/transfer/manager.go` — max-downloads counter, password check
- `signaling-server/internal/hub/hub.go` — log auth_failed and session_expired events (rate limiting deferred to Slice 5)
- `signaling-server/web/app.js` — password prompt UI
