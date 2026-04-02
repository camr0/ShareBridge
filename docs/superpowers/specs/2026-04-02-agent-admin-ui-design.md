# Agent Admin UI Design

> **For agentic workers:** This is a design spec. Use `superpowers:writing-plans` to create an implementation plan.

**Date:** 2026-04-02
**Slice:** 8 — Daemon + Multi-share + Agent UI
**Status:** Design approved

---

## Overview

A local web UI for the OpenCloudShare agent, served at `localhost:7878`. Allows users to manage shares, configure the agent, and view download history. Built with minimal dependencies: PicoCSS for styling, HTMX for interactivity, and Go's `html/template` for server-side rendering.

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
- Table refresh: `hx-get="/api/shares" hx-trigger="load, every 10s"`
- Revoke: `hx-delete="/api/shares/{code}" hx-target="closest .card" hx-swap="outerHTML swap:0.5s"`
- New Share button: opens modal via `hx-get="/api/share-form" hx-target="#modal"`

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

### Session Storage

Add `relay_only` boolean to session struct:

```go
type Session struct {
    Code        string
    ShareURL    string
    Password    string
    ExpiresAt   time.Time
    MaxDownloads int
    Downloads   int
    RelayOnly   bool    // NEW: true = force relay mode
}
```

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

---

## File Structure

```
agent/
├── cmd/agent/
│   └── main.go              # Entry point, starts daemon + web server
├── internal/
│   ├── web/
│   │   ├── server.go        # HTTP server setup, routes
│   │   ├── handlers.go      # Page handlers (Dashboard, Settings, etc.)
│   │   └── api.go           # HTMX API endpoints
│   └── store/
│       └── store.go         # Session persistence (add RelayOnly field)
└── templates/
    ├── layout.html          # Base layout (header, footer, nav)
    ├── dashboard.html       # Dashboard page content
    ├── settings.html        # Settings page content
    ├── history.html         # History page content (placeholder)
    ├── share-form.html      # New Share modal content
    └── share-card.html      # Session card fragment (for HTMX)
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
| DELETE | `/api/shares/{code}` | Revoke share | 200 OK (card removed via swap) |
| GET | `/api/share-form` | Get new share form | HTML fragment (modal content) |
| POST | `/api/shares` | Create new share | Redirect to dashboard or error |
| PUT | `/api/settings` | Save settings | 200 OK or error |

---

## CSS Details

**PicoCSS CDN:**
```html
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@picocss/pico@2/css/pico.min.css">
```

**Custom CSS (minimal):**
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

2. **Optional UI password:** If `AGENT_UI_PASSWORD` env var is set, require basic auth for all UI routes.

3. **SSRF protection:** Settings page configures `allowed_opencloud_host`. Share creation validates URL hostname matches.

4. **API key storage:** Stored in config file with user-only permissions (0600). UI shows masked value with Show/Hide toggle.

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