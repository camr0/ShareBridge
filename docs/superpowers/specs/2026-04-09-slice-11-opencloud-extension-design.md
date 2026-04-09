---
name: Slice 11 - OpenCloud Extension
description: Native OpenCloud UI integration for one-click ShareBridge sharing
type: spec
---

# Slice 11 - OpenCloud Extension

Build a native OpenCloud web extension that adds a ShareBridge sidebar panel for one-click file sharing.

## Overview

| Sub-slice | Scope | Dependencies |
|-----------|-------|--------------|
| **11a** | Agent JSON API auth + CORS | None |
| **11b** | JSON API endpoints + fileID storage | 11a |
| **11c** | OpenCloud web extension | 11b |

## Goals

- One-click sharing: user selects file → clicks "Share via ShareBridge" → gets share link
- File-specific tracking: panel shows only shares for the selected file/folder
- Simple setup: user configures agent URL + API key in extension settings
- Security: agent local API protected by simple API key

## Non-Goals

- Multiple file/folder selection (single only for MVP)
- Agent creates OpenCloud shares (extension does this via OCS API)
- Cross-device settings sync (localStorage is sufficient)

---

## Slice 11a - Agent JSON API Auth

### Problem

The agent's web UI uses HTMX + form posts with CSRF protection. The OpenCloud extension needs JSON endpoints with CORS support. We need auth for these new endpoints without breaking the existing HTMX UI.

### Solution

Add a simple API key auth mechanism for JSON endpoints, separate from the existing HTMX CSRF middleware.

### API Key Management

**Generation:**
- On first run, agent generates a random 32-character API key
- Stored in `~/.opencloudshare/config.json` (or via `SHAREBRIDGE_AGENT_API_KEY` env var)
- Displayed in Agent UI settings page for user to copy

**Format:**
```
sb_agent_xK9mN2pL4qR7sT1uV3wY5zA8bC0dE6fG
```

Prefix `sb_agent_` makes it identifiable as a ShareBridge agent key.

### Authentication Flow

```
Extension                           Agent
    │                                │
    │  GET /api/v1/shares            │
    │  X-API-Key: sb_agent_xxx       │
    │  Origin: https://opencloud.foo │
    │───────────────────────────────►│
    │                                │
    │  200 OK + CORS headers         │
    │  [{code, public_url, ...}]     │
    │◄───────────────────────────────│
    │                                │
```

### New Endpoints

| Endpoint | Auth | CORS | Description |
|----------|------|------|-------------|
| `GET /api/v1/shares` | API Key | Yes | List shares (optionally filter by `file_id`) |
| `POST /api/v1/shares` | API Key | Yes | Create new share |
| `DELETE /api/v1/shares/{code}` | API Key | Yes | Revoke share |
| `GET /api/v1/settings` | API Key | Yes | Get default settings (TTL, max downloads, etc.) |

Existing HTMX endpoints (`/api/shares`, `/api/share-form`, etc.) remain unchanged with CSRF middleware.

### CORS Configuration

Allow all origins for flexibility (agent is localhost-only in most cases):

```go
w.Header().Set("Access-Control-Allow-Origin", "*")
w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key")
```

Handle OPTIONS preflight requests.

### Error Responses

JSON error format:

```json
{
  "error": "Invalid API key",
  "code": "UNAUTHORIZED"
}
```

HTTP status codes:
- `401 Unauthorized` - Missing or invalid API key
- `400 Bad Request` - Invalid request body
- `404 Not Found` - Share not found
- `500 Internal Server Error` - Server error

### Changes to Agent

**Files to modify:**
- `agent/internal/web/server.go` - Add new routes, CORS middleware
- `agent/internal/web/api.go` - Add JSON handlers
- `agent/internal/config/config.go` - Add `AgentAPIKey` field
- `agent/internal/store/store.go` - Add `agent_api_key` to config JSON

**New files:**
- `agent/internal/web/api_v1.go` - JSON API handlers

### Agent UI Changes

Settings page shows the API key with copy button:

```
┌─────────────────────────────────────────┐
│ Settings                                │
├─────────────────────────────────────────┤
│ Extension Configuration                 │
│                                         │
│ Agent URL:  http://localhost:7878       │
│ API Key:    sb_agent_xK9mN2... [Copy]   │
│                                         │
│ Copy both values to configure the       │
│ ShareBridge OpenCloud extension.        │
└─────────────────────────────────────────┘
```

---

## Slice 11b - JSON API + FileID Storage

### Problem

The extension needs to:
1. Create ShareBridge shares via JSON API
2. List existing shares for a specific file/folder

We need to track which OpenCloud file each share belongs to.

### Solution

Agent extracts `oc:fileid` from OpenCloud WebDAV PROPFIND on the share root and stores it with each session.

### FileID Tracking

**OpenCloud fileID format:**
```
storage-users-1$some-admin-user-id-0000-000000000000!d7f8a9b2-c3e4-5f6a-7b8c-9d0e1f2a3b4c
```

This is a globally unique identifier for each file/folder.

**How the agent extracts it:**
1. When creating a session, the agent does a PROPFIND on the share root
2. The response includes `oc:fileid` for the shared item
3. Agent extracts and stores it with the session

**Why this approach:**
- Works for both Agent UI (manual share URL entry) and extension
- User never needs to know or enter fileID
- Single source of truth (the PROPFIND response)

**PROPFIND update:**

The agent's PROPFIND body needs to request `oc:fileid`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<D:propfind xmlns:D="DAV:" xmlns:oc="http://owncloud.org/ns">
  <D:prop>
    <D:getcontentlength/>
    <D:getcontenttype/>
    <D:resourcetype/>
    <oc:checksums/>
    <oc:fileid/>
  </D:prop>
</D:propfind>
```

**Graceful degradation:**
If `oc:fileid` is not returned (e.g., older OpenCloud versions), the agent stores an empty `file_id`. Shares without fileID won't be filtered per-file, but will still appear in the "all shares" list.

### API Endpoints

#### GET /api/v1/shares

Query shares, optionally filtered by file_id.

**Request:**
```
GET /api/v1/shares?file_id=storage-users-1%24...
X-API-Key: sb_agent_xxx
```

**Response:**
```json
[
  {
    "code": "abc123",
    "public_url": "https://share.example.com/s/abc123",
    "share_url": "https://opencloud.example.com/s/XYZ789",
    "file_id": "storage-users-1$...",
    "downloads": 3,
    "max_downloads": 10,
    "relay_only": false,
    "expires_at": "2026-04-10T12:00:00Z",
    "created_at": "2026-04-09T12:00:00Z"
  }
]
```

If `file_id` is omitted, returns all active shares.

#### POST /api/v1/shares

Create a new ShareBridge share. The agent extracts `file_id` from the share URL via WebDAV PROPFIND.

**Request:**
```json
{
  "share_url": "https://opencloud.example.com/s/XYZ789",
  "password": "optional",
  "expiry_hours": 24,
  "max_downloads": 10,
  "relay_only": false
}
```

**Response:**
```json
{
  "code": "abc123",
  "public_url": "https://share.example.com/s/abc123",
  "expires_at": "2026-04-10T12:00:00Z"
}
```

#### DELETE /api/v1/shares/{code}

Revoke a share.

**Request:**
```
DELETE /api/v1/shares/abc123
X-API-Key: sb_agent_xxx
```

**Response:**
```
204 No Content
```

#### GET /api/v1/settings

Get default settings for the share form.

**Response:**
```json
{
  "default_expiry_hours": 24,
  "default_max_downloads": 10,
  "default_relay_only": false,
  "turn_available": true
}
```

### Session Storage Changes

Add `file_id` to session struct:

```go
type Session struct {
    Code          string        `json:"code"`
    ShareURL      string        `json:"share_url"`
    FileID        string        `json:"file_id"`
    Password      string        `json:"password,omitempty"`
    Downloads     int           `json:"downloads"`
    MaxDownloads  int           `json:"max_downloads"`
    RelayOnly     bool          `json:"relay_only"`
    ExpiresAt     time.Time     `json:"expires_at"`
    CreatedAt     time.Time     `json:"created_at"`
}
```

Note: `file_name` is not stored — it's derived dynamically from the share when needed.

### Changes to Agent

**Files to modify:**
- `agent/internal/opencloud/client.go` - Update PROPFIND body to include `oc:fileid`, parse and return FileID from response
- `agent/internal/store/store.go` - Add `FileID` to Session struct
- `agent/internal/daemon/daemon.go` - Extract FileID from PROPFIND response, store with session
- `agent/internal/web/api_v1.go` - Implement JSON handlers

---

## Slice 11c - OpenCloud Extension

### Problem

Users want to share files via ShareBridge directly from the OpenCloud UI without switching contexts.

### Solution

Build an OpenCloud web extension that adds a ShareBridge panel to the right sidebar.

### User Flow

**Viewing existing shares:**
1. User selects a file in OpenCloud Files app
2. Right sidebar shows ShareBridge panel
3. Panel lists existing ShareBridge shares for this file
4. Each share shows: code, link, expiry, download count

**Creating a new share:**
1. User clicks "Create ShareBridge Share" button
2. Modal opens with options (TTL, password, max downloads, relay mode)
3. User clicks "Share"
4. Extension creates OpenCloud internal share via OCS API
5. Extension calls agent to create ShareBridge share
6. Panel shows new share with copyable link

### Repository Structure

ShareBridge is a monorepo. The OpenCloud extension lives in a dedicated `extensions/` directory:

```
ShareBridge/
├── agent/                    # Go agent (existing)
├── signaling-server/         # Go signaling server (existing)
├── extensions/
│   └── opencloud/           # OpenCloud web extension (Slice 11c)
│       ├── src/
│       │   ├── index.ts              # App registration
│       │   ├── App.vue               # Main component
│       │   ├── components/
│       │   │   ├── ShareBridgePanel.vue    # Sidebar panel
│       │   │   ├── ShareCard.vue           # Individual share display
│       │   │   └── CreateShareModal.vue    # Share creation form
│       │   ├── composables/
│       │   │   ├── useAgentClient.ts       # Agent API client
│       │   │   ├── useOpenCloudAPI.ts      # OCS Share API client
│       │   │   └── useShareBridgeExtension.ts # Main extension logic
│       │   └── stores/
│       │       └── settings.ts             # Agent URL + API key (localStorage)
│       ├── l10n/
│       │   └── translations.json
│       ├── package.json
│       ├── vite.config.ts
│       └── manifest.json
└── docs/
```

Future extensions (e.g., Nextcloud, Immich) would go in `extensions/nextcloud/`, `extensions/immich/`, etc.

### Extension Registration

```typescript
// src/index.ts
import { defineWebApplication } from '@opencloud-eu/web-pkg'
import { useShareBridgeExtension } from './composables/useShareBridgeExtension'

export default defineWebApplication({
  setup() {
    const { extension } = useShareBridgeExtension()

    return {
      appInfo: {
        name: 'ShareBridge',
        id: 'web-app-sharebridge',
      },
      extensions: computed(() => [unref(extension)]),
    }
  }
})
```

### Sidebar Panel Extension

```typescript
// composables/useShareBridgeExtension.ts
export const useShareBridgeExtension = () => {
  const { $gettext } = useGettext()

  const extension = computed<SidebarPanelExtension>(() => ({
    id: 'com.sharebridge.sidebar-panel',
    type: 'sidebarPanel',
    extensionPointIds: ['global.files.sidebar'],
    panel: {
      name: 'sharebridge',
      icon: 'share',
      title: () => $gettext('ShareBridge'),
      component: ShareBridgePanel,
      isRoot: () => true,
      isVisible: ({ items }) => {
        // Only show for single file/folder selection
        return items?.length === 1
      },
    },
  }))

  return { extension }
}
```

### Agent Client

```typescript
// composables/useAgentClient.ts
export const useAgentClient = () => {
  const settings = useSettingsStore()

  const headers = {
    'Content-Type': 'application/json',
    'X-API-Key': settings.apiKey,
  }

  const listShares = async (fileId?: string) => {
    const url = new URL(`${settings.agentUrl}/api/v1/shares`)
    if (fileId) url.searchParams.set('file_id', fileId)
    const response = await fetch(url.toString(), { headers })
    return response.json()
  }

  const createShare = async (params: CreateShareParams) => {
    const response = await fetch(`${settings.agentUrl}/api/v1/shares`, {
      method: 'POST',
      headers,
      body: JSON.stringify(params),
    })
    return response.json()
  }

  const revokeShare = async (code: string) => {
    await fetch(`${settings.agentUrl}/api/v1/shares/${code}`, {
      method: 'DELETE',
      headers,
    })
  }

  const getSettings = async () => {
    const response = await fetch(`${settings.agentUrl}/api/v1/settings`, { headers })
    return response.json()
  }

  return { listShares, createShare, revokeShare, getSettings }
}
```

### OpenCloud OCS API Integration

The extension uses OpenCloud's internal OCS Share API to create public shares.

```typescript
// composables/useOpenCloudAPI.ts
export const useOpenCloudAPI = () => {
  const clientService = useClientService()

  const createPublicShare = async (
    fileId: string,
    options: { password?: string; expireDate?: string } = {}
  ): Promise<string> => {
    // Use OpenCloud's Graph API or OCS Share API
    // Returns the public share URL
  }

  return { createPublicShare }
}
```

**Note:** OpenCloud extensions have access to the authenticated client service, so they can call internal APIs without separate auth.

### Settings Storage

```typescript
// stores/settings.ts
import { defineStore } from 'pinia'

export const useSettingsStore = defineStore('sharebridge-settings', {
  state: () => ({
    agentUrl: localStorage.getItem('sharebridge_agent_url') || '',
    apiKey: localStorage.getItem('sharebridge_api_key') || '',
  }),
  actions: {
    setAgentUrl(url: string) {
      this.agentUrl = url
      localStorage.setItem('sharebridge_agent_url', url)
    },
    setApiKey(key: string) {
      this.apiKey = key
      localStorage.setItem('sharebridge_api_key', key)
    },
  },
})
```

### UI Components

**ShareBridgePanel.vue:**
- Shows "Configure" button if agent URL/key not set
- Shows list of existing shares for selected file
- Shows "Create Share" button
- Shows loading/error states

**ShareCard.vue:**
- Displays: share code, public URL, expiry, download count
- Copy link button
- Revoke button

**CreateShareModal.vue:**
- TTL selector (dropdown: 1h, 24h, 7d, 30d)
- Password field (optional)
- Max downloads field
- Relay mode checkbox (with quota warning)
- Create/Cancel buttons

### Error Handling

| Scenario | User Message |
|----------|--------------|
| Agent unreachable | "Cannot connect to ShareBridge agent. Check the agent URL and ensure it's running." |
| Invalid API key | "Invalid API key. Check the key in your ShareBridge agent settings." |
| OpenCloud share creation failed | "Failed to create OpenCloud share. Please try again." |
| Agent share creation failed | "Failed to create ShareBridge share. Please try again." |

### Build Configuration

**vite.config.ts:**
- Output AMD module format (required by OpenCloud)
- External Vue and OpenCloud SDK dependencies

**manifest.json:**
```json
{
  "id": "web-app-sharebridge",
  "name": "ShareBridge",
  "version": "1.0.0"
}
```

---

## Testing Plan

### Slice 11a Tests

- [ ] API key validation works (valid key accepted)
- [ ] API key validation rejects invalid/missing key
- [ ] CORS headers present on JSON endpoints
- [ ] OPTIONS preflight handled correctly
- [ ] Existing HTMX endpoints still work (CSRF unchanged)

### Slice 11b Tests

- [ ] POST /api/v1/shares creates session with file_id
- [ ] GET /api/v1/shares returns all shares
- [ ] GET /api/v1/shares?file_id=xxx filters correctly
- [ ] DELETE /api/v1/shares/{code} revokes share
- [ ] GET /api/v1/settings returns defaults
- [ ] file_id persisted in sessions.json

### Slice 11c Tests

- [ ] Extension installs in OpenCloud
- [ ] Sidebar panel appears for single file selection
- [ ] Panel hidden for multiple file selection
- [ ] Settings stored in localStorage
- [ ] Agent client calls work with CORS
- [ ] OCS share creation works
- [ ] End-to-end: create share → see it in panel → copy link

---

## Deployment

1. Agent update (11a + 11b): Deploy new agent binary, users get API key from settings
2. Extension (11c): Package as `web-app-sharebridge`, distribute via OpenCloud App Store or direct install

---

## Future Enhancements

- QR code for mobile sharing
- Batch share creation (multiple files)
- Share link preview in OpenCloud
- Activity log in panel
- Custom share codes (pro tier)