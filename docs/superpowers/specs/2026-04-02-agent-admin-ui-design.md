# Agent Admin UI Design

> **For agentic workers:** This is a design spec. Use `superpowers:writing-plans` to create an implementation plan.

**Date:** 2026-04-02
**Slice:** 8 — Daemon + Multi-share + Agent UI
**Status:** Design approved

---

## Overview

A local web UI for the OpenCloudShare agent, served at `localhost:7878`. Allows users to manage shares, configure the agent, and view download history. Built with minimal dependencies: PicoCSS for styling, HTMX for interactivity, and Go's `html/template` for server-side rendering.

---

## Daemon Architecture

### Current State (Pre-Slice 8)

The current `main.go` is a single-share CLI command:
```
opencloudshare share <url> [--password x] [--max-downloads 10]
```

One execution = one share = one signaling connection. The process exits when the share expires or is revoked.

### New Daemon Model

**Command:**
```
opencloudshare daemon                    # Start daemon (foreground)
opencloudshare daemon --foreground       # Explicit foreground
opencloudshare share <url>               # If daemon running, add share to it; else error
```

**Process:**
1. Load config from `~/.opencloudshare/config.json`
2. Load sessions from `~/.opencloudshare/sessions.json`
3. Connect to signaling server (single WebSocket connection)
4. Start web UI server on `127.0.0.1:7878`
5. Re-register all non-expired sessions with signaling server
6. Wait for:
   - New share requests (via web UI or CLI client)
   - Incoming browser connections (dispatched to correct session handler)
   - Session expiry/revocation

**Multiple Concurrent Shares:**

The daemon maintains a single WebSocket to the signaling server. Each session has:
- Its own `map[string]*Peer` for active browser connections
- Its own WebDAV client for the share URL
- Shared signaling connection (multiplexed by session code)

```go
type Daemon struct {
    signaling      *signaling.Client     // Single WebSocket connection
    sessions       map[string]*Session   // code -> session
    peers          map[string]map[string]*Peer  // code -> peerID -> Peer
    store          *Store
    config         *Config
    webServer      *http.Server
}

type Session struct {
    Code         string
    ShareURL     string
    Password     string
    ExpiresAt    time.Time
    MaxDownloads int
    Downloads    int
    RelayOnly    bool
    CreatedAt    time.Time
}
```

**Signaling Message Dispatch:**

When a `join` message arrives from signaling server, the daemon looks up the session by code and spawns a new peer connection:

```go
func (d *Daemon) handleSignalingMessage(msg Message) {
    switch msg.Type {
    case "join":
        session, ok := d.sessions[msg.SessionID]
        if !ok {
            // Session not found
            return
        }
        go d.handleBrowserJoin(session, msg)
    case "ice_candidate":
        // Route to correct peer
    }
}
```

### CLI Client Mode

When daemon is running, `opencloudshare share <url>` becomes a thin client:

```go
func main() {
    // Check if daemon is running (try connecting to localhost:7878)
    if daemonRunning() {
        // Client mode: POST to daemon's API
        resp, err := http.Post("http://127.0.0.1:7878/api/shares", ...)
        // Print resulting code
        return
    }
    // Fallback: run in single-session mode (current behavior)
    // This preserves backward compatibility for users who don't run the daemon
    runSingleSessionMode(url, password, maxDownloads)
}
```

**Note:** This preserves backward compatibility. Users can continue using `opencloudshare share <url>` without running a daemon — it will work exactly as before (single share, process exits when share expires).

### Process Management

**macOS:** `launchd` plist in `~/Library/LaunchAgents/`
**Linux:** `systemd` service file
**Windows:** Windows service (future)

The daemon should handle SIGTERM/SIGINT gracefully:
1. Stop accepting new connections
2. Close active peer connections
3. Persist any dirty state
4. Exit

---

## Session Model

### In-Memory Representation

The daemon maintains sessions as a map keyed by session code for O(1) lookup:

```go
type Daemon struct {
    // ...
    sessions map[string]*SessionEntry  // code -> session
    // ...
}
```

### Persisted Session (sessions.json)

Sessions are stored as an array on disk (preserves order, simpler JSON):

```go
type SessionEntry struct {
    Code         string    `json:"code"`
    ShareURL     string    `json:"share_url"`
    Password     string    `json:"password,omitempty"`
    ExpiresAt    time.Time `json:"expires_at"`
    MaxDownloads int       `json:"max_downloads,omitempty"`  // 0 = unlimited
    Downloads    int       `json:"downloads"`
    RelayOnly    bool      `json:"relay_only"`
    CreatedAt    time.Time `json:"created_at"`
}
```

**Loading:** On startup, read array from JSON, build in-memory map keyed by `code`.

**Saving:** On any change, rebuild array from map values, write to JSON with atomic rename.

**Breaking change from current store:** The current `sessions.json` only stores `code`, `download_count`, and `api_key_id`. Migration: on first run with new version, existing sessions are invalid (agent re-registers with fresh codes).

### Session Expiry Pruning

The daemon runs a background goroutine that prunes expired sessions:

```go
func (d *Daemon) startExpiryPruner() {
    ticker := time.NewTicker(1 * time.Minute)
    go func() {
        for range ticker.C {
            d.pruneExpiredSessions()
        }
    }()
}

func (d *Daemon) pruneExpiredSessions() {
    now := time.Now()
    for code, session := range d.sessions {
        if session.ExpiresAt.Before(now) {
            // Deregister from signaling server
            d.signaling.DeregisterSession(code)
            // Remove from memory
            delete(d.sessions, code)
            // Persist changes
            d.store.Save(d.sessions)
        }
    }
}
```

This ensures expired sessions are cleaned up even if no user interaction occurs.

### Store File Structure

```json
{
  "agent_id": "e3b0c442-98fc-1c14-9afb-f4c8996fb924",
  "sessions": [
    {
      "code": "abc123",
      "share_url": "https://opencloud.example.com/s/xyz789",
      "password": "",
      "expires_at": "2024-01-20T15:00:00Z",
      "max_downloads": 10,
      "downloads": 3,
      "relay_only": false,
      "created_at": "2024-01-19T15:00:00Z"
    }
  ]
}
```

---

## Configuration

### Config File (config.json)

Location: `~/.opencloudshare/config.json`

```go
type Config struct {
    SignalingURL      string `json:"signaling_url"`
    APIKey            string `json:"api_key"`
    AllowedHost       string `json:"allowed_host"`
    DefaultExpiry     int    `json:"default_expiry_hours"`  // Default: 24
    DefaultMaxDownloads int  `json:"default_max_downloads"` // Default: 10, 0 = unlimited
    DefaultRelayOnly  bool   `json:"default_relay_only"`
    UIPort            int    `json:"ui_port"`               // Default: 7878
    UIPassword        string `json:"ui_password,omitempty"` // Optional
}
```

### Priority Order

1. `config.json` values (persisted from Settings UI)
2. Environment variables (fallback, useful for Docker)
3. Built-in defaults

### Environment Variables (Backward Compatibility)

```
SIGNALING_URL=wss://share.example.com
API_KEY=ocs_abc123...
ALLOWED_HOST=opencloud.example.com
UI_PORT=7878
UI_PASSWORD=secret
```

### Settings Save Behavior

When user clicks "Save Settings":
1. Validate signaling URL (must be `wss://`)
2. Validate API key format (must start with `ocs_`)
3. Write to `config.json` with 0600 permissions
4. If signaling URL or API key changed, reconnect to signaling server
5. Show success toast

---

## Tech Stack

| Component | Choice | Rationale |
|-----------|--------|-----------|
| CSS Framework | PicoCSS (~10KB) | Classless, semantic HTML, zero build step |
| Interactivity | HTMX (~14KB) | Server-driven UI, no client-side state management |
| Templating | Go `html/template` | Stdlib, HTML-safe, template inheritance |
| Build Step | None | Single binary serves everything |

**Why not a SPA framework:**
- The UI is simple CRUD (list/create/revoke shares, edit settings)
- No need for React/Vue complexity
- Matches the project's "keep it simple" philosophy
- Browser UI already uses vanilla JS — consistency

**Embedded Assets (no CDN dependency):**

PicoCSS and HTMX are embedded into the binary via `go:embed`:

```go
// In internal/web/server.go:
//go:embed static/* templates/*
var embeddedFS embed.FS

// In server setup:
http.Handle("/static/", http.FileServer(http.FS(embeddedFS)))
```

Directory structure (inside `internal/web/`):
```
agent/internal/web/
├── static/
│   ├── pico.min.css      # Downloaded from PicoCSS release
│   ├── htmx.min.js       # Downloaded from HTMX release
│   └── custom.css        # Mode badges, warning boxes
└── templates/
    ├── layout.html
    └── ...
```

This ensures the UI works offline (private server without internet access).

---

## Pages

### 1. Dashboard (`/`)

**Purpose:** View and manage active shares.

**Layout:**
```
┌─────────────────────────────────────────────────────────┐
│ Header: App name | Dashboard History Settings | Status  │
├─────────────────────────────────────────────────────────┤
│ Active Shares                              [+ New Share]│
│                                                         │
│ ┌─────────────────────────────────────────────────────┐ │
│ │ abc123                                    [Direct]  │ │
│ │ https://opencloud.example.com/s/xyz789              │ │
│ │ Downloads: 3/10  |  Expires: 2024-01-20             │ │
│ │ [Revoke]                                            │ │
│ └─────────────────────────────────────────────────────┘ │
│                                                         │
│ ┌─────────────────────────────────────────────────────┐ │
│ │ def456                                     [Relay]  │ │
│ │ https://opencloud.example.com/s/aaa111              │ │
│ │ Downloads: 1/∞   |  Expires: 2024-01-25             │ │
│ │ [Revoke]                                            │ │
│ └─────────────────────────────────────────────────────┘ │
├─────────────────────────────────────────────────────────┤
│ Footer: v1.0.0 · Uptime: 3d 14h | GitHub Docs Issues  │
└─────────────────────────────────────────────────────────┘
```

**Session Card Fields:**
- Code (heading)
- Share URL (secondary text)
- Mode badge: Direct (blue) or Relay (amber)
- Downloads: current/max (∞ if unlimited)
- Expires: date
- Revoke button

**HTMX Interactions:**
- Share list refresh: `hx-get="/api/shares" hx-trigger="load, every 10s"`
- Revoke: `hx-delete="/api/shares/{code}" hx-target="closest .card" hx-swap="outerHTML swap:0.5s"`
- New Share button: opens modal via `hx-get="/api/share-form" hx-target="#modal-container"`
- Connection status: `hx-get="/api/status" hx-trigger="load, every 5s" hx-target="#conn-status"`

---

### 2. New Share Modal

**Triggered by:** "New Share" button on Dashboard.

**Form Fields:**

| Field | Type | Required | Notes |
|-------|------|----------|-------|
| OpenCloud Share URL | URL input | Yes | Validates against allowed host |
| Password | Password input | No | Leave empty if share has no password |
| Connection Mode | Radio buttons | Yes | Direct (default) or Relay |
| Expires After | Number + dropdown | Yes | Number + hours/days, shows calculated date |
| Max Downloads | Number input | No | Empty = unlimited |

**Connection Mode Options:**

**Direct (recommended):**
- Description: "Fast, free, uses TURN only as fallback"
- Privacy warning (amber): "Your IP address will be visible to recipients if direct connection succeeds"
- Selected by default

**Relay (hide IP):**
- Description: "All traffic via TURN server, IP never exposed"
- TURN warning (red): "Requires TURN server. Connection will fail if TURN is not configured."
- Sets `iceTransportPolicy: relay` on the peer connection

**Expiry Behavior:**
- Required field — no "never" option
- Default: 24 hours
- Shows calculated expiry date below input (e.g., "Share expires 2024-01-21 at 3:00 PM")

**Max Downloads:**
- Helper text: "💡 Recommended: limit downloads to prevent link abuse"
- Default: 10

**Buttons:**
- Create Share (primary)
- Cancel (secondary)

**On Success:**
- Modal closes
- New session card appears in dashboard
- Code is briefly highlighted

**Modal Mechanics (PicoCSS v2):**

PicoCSS v2 uses the native `<dialog>` element. The flow:

1. Server returns HTML fragment with `<dialog open>` and form content
2. HTMX swaps it into `#modal-container`
3. Small JS snippet focuses the dialog:
   ```html
   <dialog open hx-on::after-swap="this.showModal()">
     <!-- form content -->
   </dialog>
   ```
4. Close on Cancel or success: `hx-on::click="this.closest('dialog').close()"`

**Modal Container (in layout.html):**
```html
<div id="modal-container"></div>
```

---

### 3. History (`/history`)

**Purpose:** View download history (future feature).

**MVP State:** Placeholder page with TODO notes.

**Placeholder Content:**
```
📋 Download history coming soon

Track what files were downloaded and when.
```

**TODO Implementation Notes (for future):**
- Store: session_code, file_name, size, timestamp, recipient_ip (optional)
- Display: table with filters (by session, date range)
- Export: CSV download option
- Retention: last 30 days or 1000 entries

---

### 4. Settings (`/settings`)

**Purpose:** Configure agent connection and defaults.

**Sections:**

#### Signaling Server
| Field | Type | Notes |
|-------|------|-------|
| Server URL | URL input | `wss://` endpoint |
| API Key | Password input | With Show/Hide toggle |
| — | Status indicator | "● Connected to signaling server" |

#### OpenCloud
| Field | Type | Notes |
|-------|------|-------|
| Allowed Host | Text input | SSRF protection — only shares from this host are allowed |

#### Default Share Settings
| Field | Type | Notes |
|-------|------|-------|
| Default Expiry | Number + dropdown | Pre-fills New Share modal |
| Default Max Downloads | Number input | Pre-fills New Share modal |
| Default Mode | Select | Direct or Relay — pre-fills New Share modal |

**Buttons:**
- Save Settings (primary)
- Reset to Defaults (secondary)

#### About Footer
```
About
OpenCloudShare Agent v1.0.0
Uptime: 3 days, 14 hours

GitHub · Documentation · Report Issue
```

---

## Navigation Structure

**Header (all pages):**
```
[OpenCloudShare] [Dashboard] [History] [Settings]    ● Connected
```

- App name links to Dashboard (`/`)
- Active tab has background highlight
- Connection status: green dot + "Connected" or red dot + "Disconnected"

**Footer (all pages):**
```
OpenCloudShare Agent v1.0.0 · Uptime: 3d 14h    GitHub · Docs · Report Issue
```

---

## Direct vs Relay Mode

### Technical Behavior

**Direct Mode:**
- Normal ICE negotiation: host → srflx (STUN) → relay (TURN)
- TURN used only as fallback if direct P2P fails
- IP visible to recipient if direct connection succeeds
- Works without TURN server

**Relay Mode:**
- Forces `iceTransportPolicy: relay` on peer connection
- All traffic goes through TURN server
- IP never exposed to recipient
- **Fails if TURN server not configured**

### Peer Connection Change

Update `peer.New()` to accept relay flag:

```go
func New(iceServers []webrtc.ICEServer, relayOnly bool) (*Peer, error) {
    config := webrtc.Configuration{
        ICEServers: iceServers,
    }
    if relayOnly {
        config.ICETransportPolicy = webrtc.ICETransportPolicyRelay
    }
    // ... rest of function
}
```

### UI Warning Logic

When user selects Relay mode:
1. Check if TURN is configured (server provided TURN credentials in ICE config)
2. If no TURN: show red warning "Requires TURN server. Connection will fail."
3. If TURN available: show no warning (Relay is a valid choice)

**Note:** The `relay_only` field is stored in the `Session` struct (see Session Model section).

---

## File Structure

```
agent/
├── cmd/agent/
│   └── main.go              # Entry point, starts daemon + web server
├── internal/
│   ├── web/
│   │   ├── server.go        # HTTP server setup, routes, go:embed directives
│   │   ├── handlers.go      # Page handlers (Dashboard, Settings, etc.)
│   │   ├── api.go           # HTMX API endpoints
│   │   ├── static/          # Embedded static files (go:embed)
│   │   │   ├── pico.min.css
│   │   │   ├── htmx.min.js
│   │   │   └── custom.css
│   │   └── templates/       # Embedded templates (go:embed)
│   │       ├── layout.html
│   │       ├── dashboard.html
│   │       ├── settings.html
│   │       ├── history.html
│   │       ├── share-form.html
│   │       └── share-card.html
│   └── store/
│       └── store.go         # Session persistence
```

**Note:** `static/` and `templates/` are inside `internal/web/` because `go:embed` can only reference paths relative to the file containing the directive. The embed directive in `server.go`:

```go
//go:embed static/* templates/*
var embeddedFS embed.FS
```

---

## API Endpoints

### Page Routes
| Method | Path | Purpose |
|--------|------|---------|
| GET | `/` | Dashboard page |
| GET | `/settings` | Settings page |
| GET | `/history` | History page |

### HTMX API Routes
| Method | Path | Purpose | Response |
|--------|------|---------|----------|
| GET | `/api/shares` | List all shares | HTML fragment (share cards) |
| POST | `/api/shares` | Create new share | HTML fragment (new card) or error |
| DELETE | `/api/shares/{code}` | Revoke share | 200 OK (card removed via swap) |
| GET | `/api/share-form` | Get new share form | HTML fragment (modal content) |
| GET | `/api/status` | Connection status | HTML fragment (status indicator) |
| PUT | `/api/settings` | Save settings | 200 OK or error |

---

## CSS Details

**Embedded Stylesheets:**

```html
<!-- In layout.html <head> -->
<link rel="stylesheet" href="/static/pico.min.css">
<link rel="stylesheet" href="/static/custom.css">
<script src="/static/htmx.min.js" defer></script>
```

Files are embedded via `go:embed` (see Tech Stack section) and served from `/static/`.

**Custom CSS (custom.css):**
```css
/* Mode badges */
.badge-direct { background: #dbeafe; color: #1d4ed8; }
.badge-relay { background: #fef3c7; color: #b45309; }

/* Warning boxes */
.warning-amber { background: #fef3c7; color: #b45309; }
.warning-red { background: #fee2e2; color: #dc2626; }

/* Revoke button */
.btn-revoke { background: #fee2e2; color: #dc2626; }
```

---

## Security Considerations

1. **Local-only binding:** Server binds to `127.0.0.1:7878` by default. No external access without explicit configuration.

2. **Optional UI password:** If `UI_PASSWORD` is set in config or env, require basic auth for all UI routes.

3. **SSRF protection:** Settings page configures `allowed_host`. Share creation validates URL hostname matches.

4. **API key storage:** Stored in `config.json` with user-only permissions (0600). UI shows masked value with Show/Hide toggle.

5. **Plaintext passwords in sessions.json:** Session passwords are stored in plaintext because they're needed to re-register sessions after daemon restart. Mitigation: `sessions.json` has 0600 permissions (user-only read/write). The threat model assumes an attacker with file system access already controls the machine.

6. **CSRF protection:** All state-modifying endpoints (POST, DELETE, PUT) verify the `X-Requested-With: XMLHttpRequest` header, which HTMX sends by default. Reject requests missing this header with 403 Forbidden.

   ```go
   func csrfMiddleware(next http.Handler) http.Handler {
       return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
           if r.Method != "GET" && r.Method != "HEAD" {
               if r.Header.Get("X-Requested-With") != "XMLHttpRequest" {
                   http.Error(w, "Forbidden", http.StatusForbidden)
                   return
               }
           }
           next.ServeHTTP(w, r)
       })
   }
   ```

   This is sufficient for local-only binding. If remote access is enabled in the future, a proper CSRF token mechanism would be required.

---

## Out of Scope (Post-MVP)

- Download history implementation (History page is placeholder)
- QR code generation for shares
- Bandwidth quota display (Slice 10)
- Third-party TURN provider configuration in UI
- Mobile-specific optimizations

---

## References

- PicoCSS: https://picocss.com/
- HTMX: https://htmx.org/
- Go `html/template`: https://pkg.go.dev/html/template