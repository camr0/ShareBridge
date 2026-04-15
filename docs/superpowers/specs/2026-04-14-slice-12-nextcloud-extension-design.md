---
name: Slice 12 - Nextcloud Extension
description: Native Nextcloud Files sidebar integration for one-click ShareBridge sharing
type: spec
---

# Slice 12 — Nextcloud Extension

Build a native Nextcloud app that adds a ShareBridge panel to the Files sidebar for one-click sharing.

## Overview

Nextcloud's app framework is more mature than OpenCloud's — `registerTab`, `@nextcloud/vue`, and `@nextcloud/axios` are stable, well-documented APIs. This extension is meaningfully simpler than the OpenCloud extension (11c):

- No password workaround (Nextcloud allows passwordless public shares by default)
- No PROPFIND needed for file ID (`node.fileid` is provided directly by the framework)
- No AMD module format or CSS injection hacks
- Server-side per-user settings via `IConfig` (configure once, works on all devices)

## Target Environment

- Nextcloud 27+ (uses `registerTab` from `@nextcloud/files`, available 27+)
- No legacy support for older versions

## Repository Structure

```
extensions/nextcloud/
├── appinfo/
│   ├── info.xml                    # App metadata (id, name, version, min/max NC version)
│   └── routes.php                  # Declares settings API routes
├── lib/
│   ├── Controller/
│   │   └── SettingsController.php  # GET/PUT per-user settings via IConfig
│   └── Settings/
│       └── PersonalSection.php     # PHP ISettings impl — registers NC Personal Settings section
├── templates/
│   └── personal_settings.php       # Renders <div id="sharebridge-personal-settings">
├── src/
│   ├── main.ts                     # Entry: registerTab + registerPersonalSettings
│   ├── components/
│   │   ├── ShareBridgeTab.vue      # Sidebar tab root
│   │   ├── ShareCard.vue           # Individual share display
│   │   ├── CreateShareModal.vue    # Share creation form
│   │   └── PersonalSettings.vue   # Settings form in NC Personal Settings
│   ├── composables/
│   │   ├── useAgentClient.ts       # Copied from OpenCloud (unchanged)
│   │   └── useNextcloudOCS.ts      # OCS share creation via @nextcloud/axios
│   └── types.ts                    # Copied from OpenCloud (unchanged)
├── js/                             # webpack output (gitignored except dist)
├── package.json
└── webpack.config.js               # @nextcloud/webpack-vue-config
```

Files **copied unchanged** from `extensions/opencloud/src/`:
- `composables/useAgentClient.ts`
- `types.ts`

No shared package needed — copy is appropriate at this scale. Revisit if a third extension (Immich) shares enough logic to justify extraction.

---

## Settings

### Storage

Per-user settings stored server-side via Nextcloud's `IConfig` API — stored in the `oc_preferences` table under app `sharebridge`. Users configure once and settings persist across all devices/browsers.

### PHP Controller

`lib/Controller/SettingsController.php` exposes four endpoints:

| Method | Route | Description |
|--------|-------|-------------|
| `GET` | `/apps/sharebridge/api/settings` | Returns `{ agent_url, api_key }` for the current user |
| `PUT` | `/apps/sharebridge/api/settings` | Saves `agent_url` and `api_key` for the current user |
| `PUT` | `/apps/sharebridge/api/shares/{code}/nc-share-id` | Stores the Nextcloud OCS share ID for a given ShareBridge code |
| `GET` | `/apps/sharebridge/api/shares/{code}/nc-share-id` | Returns the stored Nextcloud OCS share ID for a given code |
| `DELETE` | `/apps/sharebridge/api/shares/{code}/nc-share-id` | Removes the mapping entry for a given code (called on revoke) |

Uses `IConfig::getUserValue()` / `setUserValue()` throughout. The `{code → ncShareId}` mapping is stored as a JSON blob under key `nc_share_ids`. No database tables or migrations required.

### JS Settings Store

`stores/settings.ts` — rewritten (not copied from OpenCloud):
- On mount (of any component): `GET /apps/sharebridge/api/settings` via `@nextcloud/axios`
- On save: `PUT /apps/sharebridge/api/settings`
- `isConfigured` computed: both `agentUrl` and `apiKey` are non-empty
- Loading state while initial fetch resolves (settings fetch is async, unlike localStorage)
- Shared Pinia store — sidebar tab and personal settings component read/write the same state

### Routing Declaration

`appinfo/routes.php`:
```php
return [
  'routes' => [
    ['name' => 'settings#get', 'url' => '/api/settings', 'verb' => 'GET'],
    ['name' => 'settings#update', 'url' => '/api/settings', 'verb' => 'PUT'],
    ['name' => 'settings#saveNcShareId',   'url' => '/api/shares/{code}/nc-share-id', 'verb' => 'PUT'],
    ['name' => 'settings#getNcShareId',    'url' => '/api/shares/{code}/nc-share-id', 'verb' => 'GET'],
    ['name' => 'settings#deleteNcShareId', 'url' => '/api/shares/{code}/nc-share-id', 'verb' => 'DELETE'],
  ]
];
```

---

## Sidebar Registration & Personal Settings

### Personal Settings — PHP registration

`registerPersonalSettings` **does not exist** as a JS API. Personal Settings sections must be registered via PHP's `OCP\Settings\ISettings` interface.

`lib/Settings/PersonalSection.php` implements `ISettings`:
- `getForm()` → returns `TemplateResponse('sharebridge', 'personal_settings', [], 'blank')`
- `getSection()` → `'personal'`
- `getPriority()` → `50`

`templates/personal_settings.php` renders a mount point:
```html
<div id="sharebridge-personal-settings"></div>
```

`lib/AppInfo/Application.php` registers it in `register()`:
```php
$context->registerSetting(PersonalSection::class);
```

`main.ts` mounts the Vue component when the mount point exists (Settings page only):
```typescript
document.addEventListener('DOMContentLoaded', () => {
    const el = document.getElementById('sharebridge-personal-settings')
    if (el) createApp(PersonalSettings).use(pinia).mount(el)
})
```

### Sidebar Tab — hybrid v3/v4

`@nextcloud/files` v3 (NC 26-32) and v4 (NC 33+) use different APIs. NC removed `OCA.Files.Sidebar` global in NC 33 and replaced it with `getSidebar()` from `@nextcloud/files` v4 with web components.

`main.ts` detects which API is available at runtime:

```typescript
import { createApp, defineCustomElement, h, ref } from 'vue'
import { createPinia, setActivePinia } from 'pinia'
import { getSidebar } from '@nextcloud/files'
import type { ISidebarTab } from '@nextcloud/files'

const pinia = createPinia()
setActivePinia(pinia) // makes pinia available inside defineCustomElement-created apps

if (window.OCA?.Files?.Sidebar) {
  // NC 26-32: legacy OCA.Files.Sidebar global
  // fileInfo.id is the fileid (number)
  const currentNode = ref({ fileid: 0, path: '' })
  let app: ReturnType<typeof createApp> | null = null

  window.OCA.Files.Sidebar.registerTab(new window.OCA.Files.Sidebar.Tab({
    id: 'sharebridge',
    name: t('sharebridge', 'ShareBridge'),
    iconSvgInline: ICON_SVG,
    mount(el, fileInfo) {
      currentNode.value = { fileid: fileInfo.id, path: fileInfo.path }
      app = createApp({ render: () => h(ShareBridgeTab, { node: currentNode.value }) })
      app.use(pinia).mount(el)
    },
    update(fileInfo) { currentNode.value = { fileid: fileInfo.id, path: fileInfo.path } },
    destroy() { app?.unmount(); app = null },
  }))
} else {
  // NC 33+: @nextcloud/files v4 getSidebar() + web component
  const tab: ISidebarTab = {
    id: 'sharebridge',
    displayName: t('sharebridge', 'ShareBridge'),
    iconSvgInline: ICON_SVG,
    order: 50,
    tagName: 'sharebridge-files-sidebar-tab',
    enabled({ node }) { return true },
    async onInit() {
      const el = defineCustomElement(ShareBridgeTab, { shadowRoot: false })
      customElements.define('sharebridge-files-sidebar-tab', el)
    },
  }
  getSidebar().registerTab(tab)
}
```

- `shadowRoot: false` allows NC global CSS (theming) to apply to the web component
- `setActivePinia(pinia)` makes the shared store available inside custom elements without `app.use(pinia)` in each element app
- `package.json` targets `@nextcloud/files@^4.0.0`; on NC 26-32 the `getSidebar()` branch is never entered so the bundled v4 code is unused there
- Tab is enabled for all single-file selections; multi-file is handled by NC's sidebar which won't open the tab for multiple selections

---

## File ID

Nextcloud passes the selected file as a `Node` object from `@nextcloud/files`. The file ID is available directly as `node.fileid` (a plain integer) — no PROPFIND required.

`ShareBridgeTab` receives the node as a prop and passes `node.fileid.toString()` to `listShares()`. The agent API call is unchanged.

**Props**: `ShareBridgeTab.vue` declares `node: { fileid: number; path: string }`. Both code paths provide this:
- v3 (manual `createApp`): `{ fileid: fileInfo.id, path: fileInfo.path }`
- v4 (custom element): `INode` from `@nextcloud/files` which exposes `fileid` and `path`

**Agent PROPFIND unchanged**: The agent always does PROPFIND when creating a share (to extract file ID from the share URL). We do NOT pass `file_id` in `CreateShareParams` even though we have it — keeping the agent's PROPFIND path always exercised avoids hiding potential breakage in that code path.

---

## OCS Share Creation

`composables/useNextcloudOCS.ts` replaces `useOpenCloudAPI.ts`.

Nextcloud allows passwordless public shares by default (no enforcement). A single `POST` creates the share:

```
POST /ocs/v2.php/apps/files_sharing/api/v1/shares
Content-Type: application/x-www-form-urlencoded

shareType=3&path=/Documents/report.pdf&expireDate=2026-04-21
```

**No password workaround needed.** The OpenCloud extension required a create-then-remove-password hack because OpenCloud enforces passwords on public shares. Nextcloud does not.

**Expiry handling**: The OCS API only accepts `expireDate` as `YYYY-MM-DD` — no time component. ShareBridge supports sub-day expiry (e.g., 1 hour). Solution: always round the ShareBridge expiry up to the next calendar day for the OCS `expireDate`. The Nextcloud public link may outlive the ShareBridge share by up to 23h 59m, but ShareBridge is the access gatekeeper — once the ShareBridge code expires the share is unreachable regardless.

Uses `@nextcloud/axios` which automatically handles:
- CSRF token injection (`requesttoken` header)
- Session authentication (same-origin, always authenticated)

Edge case: if the Nextcloud admin has enabled "Enforce password protection for public links", the OCS call will fail. Surface this as a clear error: *"Your Nextcloud requires passwords on public links. Please contact your admin."*

---

## Vue Components

All three components rewritten using `@nextcloud/vue` for native look and feel. The sidebar panel sits inside Nextcloud's UI, so native components matter.

| Component | `@nextcloud/vue` components used | Notes |
|-----------|----------------------------------|-------|
| `ShareBridgeTab.vue` | `NcLoadingIcon`, `NcEmptyContent`, `NcButton` | Receives `node: { fileid, path }` prop; shows prompt to visit Settings if not configured |
| `ShareCard.vue` | `NcButton`, `NcBadge` | Same logic as OpenCloud version |
| `CreateShareModal.vue` | `NcModal`, `NcSelect`, `NcTextField`, `NcCheckboxRadioSwitch` | Same fields as OpenCloud version |
| `PersonalSettings.vue` | `NcSettingsSection`, `NcTextField`, `NcButton` | Agent URL + API key form; always accessible for updates |

`ShareBridgeTab` no longer contains the config form. If `!isConfigured`, it shows a simple prompt: *"Configure ShareBridge in your Personal Settings to get started."* This keeps the sidebar focused on sharing, not configuration.

### ShareBridgeTab.vue behaviour

1. Mount → fetch settings from PHP endpoint (via `settings.fetchSettings()`)
   - v3 path: triggered by `onMounted` in the manually-mounted Vue app
   - v4 path: triggered by `onMounted` in the custom element
2. If not configured → show prompt: *"Configure ShareBridge in your Personal Settings to get started."*
3. If configured → load shares for `node.fileid` from agent
4. Shows `NcLoadingIcon` while settings or shares are fetching
5. Shows `NcEmptyContent` when no shares exist for this file
6. "Create ShareBridge Share" button opens `CreateShareModal`

---

## User Flow

**First use:**
1. User goes to avatar → Settings → Personal → ShareBridge
2. Enters agent URL + API key (found in ShareBridge agent settings page) → Save
3. Settings stored server-side — works immediately on all devices
4. User selects a file → ShareBridge tab is ready to use

**Creating a share:**
1. User selects file → ShareBridge tab shows existing shares (or empty state)
2. User clicks "Create ShareBridge Share"
3. Modal opens: TTL, password (optional), max downloads, relay mode
4. User clicks Create:
   - Extension calls Nextcloud OCS API → creates public share → gets `shareUrl` + `ncShareId`
   - Extension calls agent `POST /api/v1/shares` with `shareUrl` → gets back ShareBridge `code`
   - Extension saves `{code → ncShareId}` via `PUT /api/shares/{code}/nc-share-id`
5. Panel refreshes, new share appears in list

**Revoking a share:**
1. User clicks Revoke on a ShareCard
2. Extension fetches `ncShareId` via `GET /api/shares/{code}/nc-share-id`
3. Extension calls agent `DELETE /api/v1/shares/{code}` + OCS share delete + `DELETE /api/shares/{code}/nc-share-id` in parallel
4. Panel refreshes — no orphaned Nextcloud public links or stale mapping entries left behind

---

## Error Handling

| Scenario | Message |
|----------|---------|
| Agent unreachable | "Cannot connect to ShareBridge agent. Check the agent URL and ensure it's running." |
| Invalid API key | "Invalid API key. Check the key in your ShareBridge agent settings." |
| OCS share creation failed | "Failed to create Nextcloud share. Please try again." |
| OCS password enforcement | "Your Nextcloud requires passwords on public links. Please contact your admin." |
| Agent share creation failed | "Failed to create ShareBridge share. Please try again." |
| Settings fetch failed | Show "Configure ShareBridge in your Personal Settings to get started." prompt (treat as unconfigured) |

---

## Build & Deployment

**Build tooling**: `@nextcloud/webpack-vue-config` (standard Nextcloud app build). Outputs `js/sharebridge-main.js`.

**`appinfo/info.xml`** declares:
- App ID: `sharebridge`
- Min Nextcloud version: 27
- Max Nextcloud version: omitted (track latest)
- Dependencies: `files` app (for sidebar integration)

**Installation (self-hosted):**
1. Drop `extensions/nextcloud/` (built) into Nextcloud's `apps/sharebridge/` directory
2. Enable in Nextcloud admin → Apps

**Future**: publish to Nextcloud App Store for in-admin-UI install button.

---

## What's Not In This Slice

- QR codes — deferred to Slice 16 (added everywhere at once: agent UI, share page, both extensions)
- Admin-level shared settings — deferred; target user is personal/small setup, per-user is sufficient
- Multi-user / shared agent — requires significant agent architecture changes, deferred
- App Store submission — after the extension is stable in the wild
- OpenCloud OCS/Graph share cleanup on revoke — no server-side storage available without agent changes; deferred to Future/TBD

---

## Testing Plan

### PHP
- [ ] `GET /api/settings` returns empty strings for new user
- [ ] `PUT /api/settings` saves and `GET` returns saved values
- [ ] Settings are per-user (user A's settings don't affect user B)
- [ ] `PUT /api/shares/{code}/nc-share-id` stores mapping
- [ ] `GET /api/shares/{code}/nc-share-id` returns stored ID
- [ ] Unknown code returns 404
- [ ] `DELETE /api/shares/{code}/nc-share-id` removes the entry from the blob

### JS / Vue
- [ ] Tab appears for single file selection
- [ ] Tab hidden for multi-file selection
- [ ] Tab shows "go to Personal Settings" prompt when not configured
- [ ] Personal Settings section appears under user avatar → Settings → Personal
- [ ] Personal Settings saves via PHP endpoint (not localStorage)
- [ ] Updating settings in Personal Settings reflects immediately in sidebar tab
- [ ] Settings load on mount (async spinner shown)
- [ ] `listShares` called with `node.fileid`
- [ ] OCS share created without password field
- [ ] Agent `POST /api/v1/shares` called with OCS share URL
- [ ] `ncShareId` saved after share creation
- [ ] Share list refreshes after create/revoke
- [ ] Revoke calls OCS delete + agent delete in parallel
- [ ] Password enforcement error surfaces correct message

### End-to-end (requires live Nextcloud instance)
- [ ] App installs and enables in Nextcloud 27+
- [ ] Tab appears in Files sidebar
- [ ] Configure with agent URL + API key — persists across browser refresh
- [ ] Configure on one device — works immediately on second device
- [ ] Create share end-to-end → share link works in browser
- [ ] Revoke share → removed from panel
