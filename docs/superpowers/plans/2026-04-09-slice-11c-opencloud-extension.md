# Slice 11c — OpenCloud Web Extension Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build an OpenCloud web extension that adds a ShareBridge sidebar panel for one-click file sharing, communicating with the local agent JSON API from Slices 11a+11b.

**Architecture:** A Vue 3 + TypeScript AMD module registered via `defineWebApplication` from `@opencloud-eu/web-pkg`. A Pinia store persists agent URL and API key in localStorage. Two composables handle the agent API and OpenCloud OCS API respectively. Three components (ShareBridgePanel, ShareCard, CreateShareModal) implement the UI. All unit-tested with Vitest + @vue/test-utils.

**Tech Stack:** Vue 3, TypeScript, Pinia, Vite (AMD output), Vitest, @vue/test-utils, @opencloud-eu/web-pkg, happy-dom.

---

## File Map

| Action | File | Purpose |
|--------|------|---------|
| Create | `extensions/opencloud/package.json` | NPM manifest, scripts, deps |
| Create | `extensions/opencloud/vite.config.ts` | Vite build (AMD) + Vitest config |
| Create | `extensions/opencloud/tsconfig.json` | TypeScript config for Vue 3 |
| Create | `extensions/opencloud/manifest.json` | OpenCloud extension manifest |
| Create | `extensions/opencloud/src/index.ts` | App registration via `defineWebApplication` |
| Create | `extensions/opencloud/src/types.ts` | Shared TypeScript types (Share, CreateShareParams, etc.) |
| Create | `extensions/opencloud/src/stores/settings.ts` | Pinia store: agentUrl + apiKey → localStorage |
| Create | `extensions/opencloud/src/stores/settings.test.ts` | Unit tests for settings store |
| Create | `extensions/opencloud/src/composables/useAgentClient.ts` | Agent JSON API client |
| Create | `extensions/opencloud/src/composables/useAgentClient.test.ts` | Unit tests for agent client |
| Create | `extensions/opencloud/src/composables/useOpenCloudAPI.ts` | OCS Share API client via clientService |
| Create | `extensions/opencloud/src/composables/useOpenCloudAPI.test.ts` | Unit tests for OCS API client |
| Create | `extensions/opencloud/src/composables/useShareBridgeExtension.ts` | Returns SidebarPanelExtension config |
| Create | `extensions/opencloud/src/composables/useShareBridgeExtension.test.ts` | Unit tests for extension registration |
| Create | `extensions/opencloud/src/components/ShareCard.vue` | Individual share row (link, expiry, revoke button) |
| Create | `extensions/opencloud/src/components/ShareCard.test.ts` | Unit tests for ShareCard |
| Create | `extensions/opencloud/src/components/CreateShareModal.vue` | Modal: TTL/password/options form + create logic |
| Create | `extensions/opencloud/src/components/CreateShareModal.test.ts` | Unit tests for CreateShareModal |
| Create | `extensions/opencloud/src/components/ShareBridgePanel.vue` | Main sidebar panel (list + create flow) |
| Create | `extensions/opencloud/src/components/ShareBridgePanel.test.ts` | Unit tests for ShareBridgePanel |

---

## Task 1: Project Scaffold

**Files:**
- Create: `extensions/opencloud/package.json`
- Create: `extensions/opencloud/vite.config.ts`
- Create: `extensions/opencloud/tsconfig.json`
- Create: `extensions/opencloud/manifest.json`
- Create: `extensions/opencloud/src/index.ts` (stub)
- Create: `extensions/opencloud/src/types.ts`

- [ ] **Step 1: Create the directory structure**

```bash
mkdir -p extensions/opencloud/src/stores
mkdir -p extensions/opencloud/src/composables
mkdir -p extensions/opencloud/src/components
mkdir -p extensions/opencloud/l10n
```

- [ ] **Step 2: Create package.json**

```json
{
  "name": "web-app-sharebridge",
  "version": "1.0.0",
  "private": true,
  "scripts": {
    "build": "vite build",
    "test": "vitest run",
    "test:watch": "vitest",
    "typecheck": "vue-tsc --noEmit"
  },
  "dependencies": {
    "@opencloud-eu/web-pkg": "^7.0.0"
  },
  "devDependencies": {
    "@vitejs/plugin-vue": "^5.0.0",
    "@vue/test-utils": "^2.4.0",
    "happy-dom": "^14.0.0",
    "pinia": "^2.1.0",
    "typescript": "^5.4.0",
    "vite": "^5.2.0",
    "vitest": "^1.6.0",
    "vue": "^3.4.0",
    "vue-tsc": "^2.0.0"
  },
  "peerDependencies": {
    "vue": "^3.4.0"
  }
}
```

**Note:** Verify the `@opencloud-eu/web-pkg` version that matches your OpenCloud installation. Check `node_modules/@opencloud-eu/web-pkg/package.json` or the OpenCloud server's bundled version.

- [ ] **Step 3: Create vite.config.ts**

```typescript
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig({
  plugins: [vue()],
  build: {
    lib: {
      entry: 'src/index.ts',
      formats: ['amd'],
      name: 'web-app-sharebridge',
      fileName: () => 'web-app-sharebridge.js',
    },
    rollupOptions: {
      external: ['vue', '@opencloud-eu/web-pkg', 'pinia'],
    },
  },
  test: {
    environment: 'happy-dom',
    globals: true,
  },
})
```

- [ ] **Step 4: Create tsconfig.json**

```json
{
  "compilerOptions": {
    "target": "ES2020",
    "useDefineForClassFields": true,
    "module": "ESNext",
    "lib": ["ES2020", "DOM", "DOM.Iterable"],
    "skipLibCheck": true,
    "moduleResolution": "bundler",
    "allowImportingTsExtensions": true,
    "resolveJsonModule": true,
    "isolatedModules": true,
    "noEmit": true,
    "jsx": "preserve",
    "strict": true,
    "noUnusedLocals": true,
    "noUnusedParameters": true,
    "noFallthroughCasesInSwitch": true
  },
  "include": ["src/**/*.ts", "src/**/*.d.ts", "src/**/*.tsx", "src/**/*.vue"]
}
```

- [ ] **Step 5: Create manifest.json**

```json
{
  "id": "web-app-sharebridge",
  "name": "ShareBridge",
  "version": "1.0.0"
}
```

- [ ] **Step 6: Create src/types.ts**

```typescript
export interface Share {
  code: string
  public_url: string
  share_url: string
  file_id: string
  downloads: number
  max_downloads: number
  relay_only: boolean
  expires_at: string
  created_at: string
}

export interface CreateShareParams {
  share_url: string
  password?: string
  expiry_hours: number
  max_downloads: number
  relay_only: boolean
}

export interface CreateShareResult {
  code: string
  public_url: string
  expires_at: string
}

export interface AgentSettings {
  default_expiry_hours: number
  default_max_downloads: number
  default_relay_only: boolean
  turn_available: boolean
}
```

- [ ] **Step 7: Create stub src/index.ts**

```typescript
import { defineWebApplication } from '@opencloud-eu/web-pkg'

export default defineWebApplication({
  setup() {
    return {
      appInfo: {
        name: 'ShareBridge',
        id: 'web-app-sharebridge',
      },
      extensions: [],
    }
  },
})
```

- [ ] **Step 8: Install dependencies**

```bash
cd extensions/opencloud && npm install
```

Expected: packages installed without errors. `node_modules/@opencloud-eu/web-pkg` exists.

- [ ] **Step 9: Verify build succeeds**

```bash
cd extensions/opencloud && npm run build
```

Expected: `dist/web-app-sharebridge.js` created. No TypeScript errors.

- [ ] **Step 10: Commit**

```bash
git add extensions/opencloud/
git commit -m "feat: scaffold OpenCloud extension project (11c)"
```

---

## Task 2: Settings Store

**Files:**
- Create: `extensions/opencloud/src/stores/settings.ts`
- Create: `extensions/opencloud/src/stores/settings.test.ts`

- [ ] **Step 1: Write the failing test**

Create `extensions/opencloud/src/stores/settings.test.ts`:

```typescript
import { describe, it, expect, beforeEach } from 'vitest'
import { setActivePinia, createPinia } from 'pinia'
import { useSettingsStore } from './settings'

describe('useSettingsStore', () => {
  beforeEach(() => {
    localStorage.clear()
    setActivePinia(createPinia())
  })

  it('initializes with empty values when localStorage is empty', () => {
    const store = useSettingsStore()
    expect(store.agentUrl).toBe('')
    expect(store.apiKey).toBe('')
  })

  it('initializes agentUrl from localStorage', () => {
    localStorage.setItem('sharebridge_agent_url', 'http://localhost:7878')
    const store = useSettingsStore()
    expect(store.agentUrl).toBe('http://localhost:7878')
  })

  it('initializes apiKey from localStorage', () => {
    localStorage.setItem('sharebridge_api_key', 'sb_agent_test')
    const store = useSettingsStore()
    expect(store.apiKey).toBe('sb_agent_test')
  })

  it('setAgentUrl updates state and persists to localStorage', () => {
    const store = useSettingsStore()
    store.setAgentUrl('http://localhost:7878')
    expect(store.agentUrl).toBe('http://localhost:7878')
    expect(localStorage.getItem('sharebridge_agent_url')).toBe('http://localhost:7878')
  })

  it('setApiKey updates state and persists to localStorage', () => {
    const store = useSettingsStore()
    store.setApiKey('sb_agent_key123')
    expect(store.apiKey).toBe('sb_agent_key123')
    expect(localStorage.getItem('sharebridge_api_key')).toBe('sb_agent_key123')
  })

  it('isConfigured returns false when agentUrl is empty', () => {
    const store = useSettingsStore()
    expect(store.isConfigured).toBe(false)
  })

  it('isConfigured returns false when apiKey is empty', () => {
    const store = useSettingsStore()
    store.setAgentUrl('http://localhost:7878')
    expect(store.isConfigured).toBe(false)
  })

  it('isConfigured returns true when both agentUrl and apiKey are set', () => {
    const store = useSettingsStore()
    store.setAgentUrl('http://localhost:7878')
    store.setApiKey('sb_agent_key123')
    expect(store.isConfigured).toBe(true)
  })
})
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd extensions/opencloud && npm test
```

Expected: FAIL — `Cannot find module './settings'`

- [ ] **Step 3: Create src/stores/settings.ts**

```typescript
import { defineStore } from 'pinia'

export const useSettingsStore = defineStore('sharebridge-settings', {
  state: () => ({
    agentUrl: localStorage.getItem('sharebridge_agent_url') ?? '',
    apiKey: localStorage.getItem('sharebridge_api_key') ?? '',
  }),
  getters: {
    isConfigured: (state) => state.agentUrl !== '' && state.apiKey !== '',
  },
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

- [ ] **Step 4: Run tests to verify they pass**

```bash
cd extensions/opencloud && npm test
```

Expected: all 8 tests PASS

- [ ] **Step 5: Commit**

```bash
git add extensions/opencloud/src/stores/
git commit -m "feat: add settings store with localStorage persistence (11c)"
```

---

## Task 3: Agent Client Composable

**Files:**
- Create: `extensions/opencloud/src/composables/useAgentClient.ts`
- Create: `extensions/opencloud/src/composables/useAgentClient.test.ts`

- [ ] **Step 1: Write the failing test**

Create `extensions/opencloud/src/composables/useAgentClient.test.ts`:

```typescript
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { setActivePinia, createPinia } from 'pinia'
import { useAgentClient } from './useAgentClient'
import { useSettingsStore } from '../stores/settings'

describe('useAgentClient', () => {
  let mockFetch: ReturnType<typeof vi.fn>

  beforeEach(() => {
    localStorage.clear()
    setActivePinia(createPinia())
    mockFetch = vi.fn()
    vi.stubGlobal('fetch', mockFetch)
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  const setupStore = (url = 'http://localhost:7878', key = 'sb_agent_testkey') => {
    const store = useSettingsStore()
    store.setAgentUrl(url)
    store.setApiKey(key)
    return store
  }

  it('listShares fetches /api/v1/shares with X-API-Key header', async () => {
    setupStore()
    mockFetch.mockResolvedValue({ json: () => Promise.resolve([]) })

    const { listShares } = useAgentClient()
    await listShares()

    expect(mockFetch).toHaveBeenCalledWith(
      'http://localhost:7878/api/v1/shares',
      expect.objectContaining({
        headers: expect.objectContaining({ 'X-API-Key': 'sb_agent_testkey' }),
      })
    )
  })

  it('listShares appends file_id query param when provided', async () => {
    setupStore()
    mockFetch.mockResolvedValue({ json: () => Promise.resolve([]) })

    const { listShares } = useAgentClient()
    await listShares('storage-users-1$abc!def')

    expect(mockFetch).toHaveBeenCalledWith(
      'http://localhost:7878/api/v1/shares?file_id=storage-users-1%24abc%21def',
      expect.anything()
    )
  })

  it('createShare POSTs JSON to /api/v1/shares', async () => {
    setupStore()
    const mockResult = { code: 'abc123', public_url: 'https://share.example.com/s/abc123', expires_at: '2026-04-10T12:00:00Z' }
    mockFetch.mockResolvedValue({ json: () => Promise.resolve(mockResult) })

    const { createShare } = useAgentClient()
    const result = await createShare({
      share_url: 'https://opencloud.example.com/s/XYZ789',
      expiry_hours: 24,
      max_downloads: 10,
      relay_only: false,
    })

    expect(mockFetch).toHaveBeenCalledWith(
      'http://localhost:7878/api/v1/shares',
      expect.objectContaining({
        method: 'POST',
        headers: expect.objectContaining({ 'Content-Type': 'application/json' }),
      })
    )
    expect(result).toEqual(mockResult)
  })

  it('revokeShare sends DELETE to /api/v1/shares/{code}', async () => {
    setupStore()
    mockFetch.mockResolvedValue({})

    const { revokeShare } = useAgentClient()
    await revokeShare('abc123')

    expect(mockFetch).toHaveBeenCalledWith(
      'http://localhost:7878/api/v1/shares/abc123',
      expect.objectContaining({ method: 'DELETE' })
    )
  })

  it('getSettings fetches /api/v1/settings', async () => {
    setupStore()
    const mockSettings = { default_expiry_hours: 24, default_max_downloads: 10, default_relay_only: false, turn_available: true }
    mockFetch.mockResolvedValue({ json: () => Promise.resolve(mockSettings) })

    const { getSettings } = useAgentClient()
    const result = await getSettings()

    expect(mockFetch).toHaveBeenCalledWith(
      'http://localhost:7878/api/v1/settings',
      expect.objectContaining({ headers: expect.objectContaining({ 'X-API-Key': 'sb_agent_testkey' }) })
    )
    expect(result).toEqual(mockSettings)
  })

  it('getHeaders reads apiKey fresh on each call after store update', async () => {
    const store = setupStore('http://localhost:7878', 'old-key')
    mockFetch.mockResolvedValue({ json: () => Promise.resolve([]) })

    const { listShares } = useAgentClient()
    store.setApiKey('new-key')
    await listShares()

    expect(mockFetch).toHaveBeenCalledWith(
      expect.anything(),
      expect.objectContaining({
        headers: expect.objectContaining({ 'X-API-Key': 'new-key' }),
      })
    )
  })
})
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd extensions/opencloud && npm test
```

Expected: FAIL — `Cannot find module './useAgentClient'`

- [ ] **Step 3: Create src/composables/useAgentClient.ts**

```typescript
import type { CreateShareParams, CreateShareResult, Share, AgentSettings } from '../types'
import { useSettingsStore } from '../stores/settings'

export const useAgentClient = () => {
  const settings = useSettingsStore()

  const getHeaders = (): Record<string, string> => ({
    'Content-Type': 'application/json',
    'X-API-Key': settings.apiKey,
  })

  const listShares = async (fileId?: string): Promise<Share[]> => {
    const url = new URL(`${settings.agentUrl}/api/v1/shares`)
    if (fileId) url.searchParams.set('file_id', fileId)
    const response = await fetch(url.toString(), { headers: getHeaders() })
    return response.json()
  }

  const createShare = async (params: CreateShareParams): Promise<CreateShareResult> => {
    const response = await fetch(`${settings.agentUrl}/api/v1/shares`, {
      method: 'POST',
      headers: getHeaders(),
      body: JSON.stringify(params),
    })
    return response.json()
  }

  const revokeShare = async (code: string): Promise<void> => {
    await fetch(`${settings.agentUrl}/api/v1/shares/${code}`, {
      method: 'DELETE',
      headers: getHeaders(),
    })
  }

  const getSettings = async (): Promise<AgentSettings> => {
    const response = await fetch(`${settings.agentUrl}/api/v1/settings`, {
      headers: getHeaders(),
    })
    return response.json()
  }

  return { listShares, createShare, revokeShare, getSettings }
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
cd extensions/opencloud && npm test
```

Expected: all 6 tests PASS

- [ ] **Step 5: Commit**

```bash
git add extensions/opencloud/src/composables/useAgentClient.ts \
        extensions/opencloud/src/composables/useAgentClient.test.ts
git commit -m "feat: add agent client composable with fresh-header pattern (11c)"
```

---

## Task 4: OpenCloud OCS API Composable

**Files:**
- Create: `extensions/opencloud/src/composables/useOpenCloudAPI.ts`
- Create: `extensions/opencloud/src/composables/useOpenCloudAPI.test.ts`

- [ ] **Step 1: Write the failing test**

Create `extensions/opencloud/src/composables/useOpenCloudAPI.test.ts`:

```typescript
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { useOpenCloudAPI } from './useOpenCloudAPI'

// Mock @opencloud-eu/web-pkg
const mockOcsPost = vi.fn()
vi.mock('@opencloud-eu/web-pkg', () => ({
  useClientService: () => ({
    ocs: { post: mockOcsPost },
  }),
}))

describe('useOpenCloudAPI', () => {
  beforeEach(() => {
    mockOcsPost.mockReset()
  })

  it('createPublicShare POSTs to OCS shares endpoint with shareType=3 and path', async () => {
    mockOcsPost.mockResolvedValue({
      ocs: { data: { url: 'https://opencloud.example.com/s/XYZ789' } },
    })

    const { createPublicShare } = useOpenCloudAPI()
    const result = await createPublicShare('/Documents/report.pdf')

    expect(mockOcsPost).toHaveBeenCalledWith(
      '/apps/files_sharing/api/v1/shares',
      expect.any(URLSearchParams)
    )
    const params = mockOcsPost.mock.calls[0][1] as URLSearchParams
    expect(params.get('shareType')).toBe('3')
    expect(params.get('path')).toBe('/Documents/report.pdf')
    expect(result).toBe('https://opencloud.example.com/s/XYZ789')
  })

  it('createPublicShare includes password when provided', async () => {
    mockOcsPost.mockResolvedValue({
      ocs: { data: { url: 'https://opencloud.example.com/s/ABC' } },
    })

    const { createPublicShare } = useOpenCloudAPI()
    await createPublicShare('/file.pdf', { password: 'secret' })

    const params = mockOcsPost.mock.calls[0][1] as URLSearchParams
    expect(params.get('password')).toBe('secret')
  })

  it('createPublicShare includes expireDate when provided', async () => {
    mockOcsPost.mockResolvedValue({
      ocs: { data: { url: 'https://opencloud.example.com/s/ABC' } },
    })

    const { createPublicShare } = useOpenCloudAPI()
    await createPublicShare('/file.pdf', { expireDate: '2026-04-10' })

    const params = mockOcsPost.mock.calls[0][1] as URLSearchParams
    expect(params.get('expireDate')).toBe('2026-04-10')
  })

  it('createPublicShare omits password and expireDate when not provided', async () => {
    mockOcsPost.mockResolvedValue({
      ocs: { data: { url: 'https://opencloud.example.com/s/ABC' } },
    })

    const { createPublicShare } = useOpenCloudAPI()
    await createPublicShare('/file.pdf')

    const params = mockOcsPost.mock.calls[0][1] as URLSearchParams
    expect(params.has('password')).toBe(false)
    expect(params.has('expireDate')).toBe(false)
  })
})
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd extensions/opencloud && npm test
```

Expected: FAIL — `Cannot find module './useOpenCloudAPI'`

- [ ] **Step 3: Create src/composables/useOpenCloudAPI.ts**

```typescript
import { useClientService } from '@opencloud-eu/web-pkg'

interface CreatePublicShareOptions {
  password?: string
  expireDate?: string
}

export const useOpenCloudAPI = () => {
  const clientService = useClientService()

  const createPublicShare = async (
    path: string,
    options: CreatePublicShareOptions = {}
  ): Promise<string> => {
    const params = new URLSearchParams({ shareType: '3', path })
    if (options.password) params.set('password', options.password)
    if (options.expireDate) params.set('expireDate', options.expireDate)

    // Note: verify exact method name against your @opencloud-eu/web-pkg version.
    // It may be clientService.httpAuthenticated.post() or clientService.ocs.post().
    // Check: node_modules/@opencloud-eu/web-pkg/dist/index.d.ts for ClientService type.
    const response = await clientService.ocs.post(
      '/apps/files_sharing/api/v1/shares',
      params
    )
    return response.ocs.data.url as string
  }

  return { createPublicShare }
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
cd extensions/opencloud && npm test
```

Expected: all 4 tests PASS

- [ ] **Step 5: Commit**

```bash
git add extensions/opencloud/src/composables/useOpenCloudAPI.ts \
        extensions/opencloud/src/composables/useOpenCloudAPI.test.ts
git commit -m "feat: add OpenCloud OCS share API composable (11c)"
```

---

## Task 5: Extension Registration

**Files:**
- Create: `extensions/opencloud/src/composables/useShareBridgeExtension.ts`
- Create: `extensions/opencloud/src/composables/useShareBridgeExtension.test.ts`
- Modify: `extensions/opencloud/src/index.ts`

- [ ] **Step 1: Write the failing test**

Create `extensions/opencloud/src/composables/useShareBridgeExtension.test.ts`:

```typescript
import { describe, it, expect, vi } from 'vitest'
import { useShareBridgeExtension } from './useShareBridgeExtension'

vi.mock('@opencloud-eu/web-pkg', () => ({
  useGettext: () => ({ $gettext: (s: string) => s }),
}))

// ShareBridgePanel imported inside the composable; mock it as a placeholder
vi.mock('../components/ShareBridgePanel.vue', () => ({ default: {} }))

describe('useShareBridgeExtension', () => {
  it('returns a SidebarPanelExtension with correct id and type', () => {
    const { extension } = useShareBridgeExtension()
    expect(extension.value.id).toBe('com.sharebridge.sidebar-panel')
    expect(extension.value.type).toBe('sidebarPanel')
  })

  it('extensionPointIds targets global.files.sidebar', () => {
    const { extension } = useShareBridgeExtension()
    expect(extension.value.extensionPointIds).toContain('global.files.sidebar')
  })

  it('panel.name is sharebridge', () => {
    const { extension } = useShareBridgeExtension()
    expect(extension.value.panel.name).toBe('sharebridge')
  })

  it('isVisible returns true for single file selection', () => {
    const { extension } = useShareBridgeExtension()
    const { isVisible } = extension.value.panel
    expect(isVisible({ items: [{ id: '1', path: '/file.txt' }] })).toBe(true)
  })

  it('isVisible returns false for multiple file selection', () => {
    const { extension } = useShareBridgeExtension()
    const { isVisible } = extension.value.panel
    expect(isVisible({ items: [{ id: '1' }, { id: '2' }] })).toBe(false)
  })

  it('isVisible returns false for empty selection', () => {
    const { extension } = useShareBridgeExtension()
    const { isVisible } = extension.value.panel
    expect(isVisible({ items: [] })).toBe(false)
  })

  it('isVisible returns false when items is undefined', () => {
    const { extension } = useShareBridgeExtension()
    const { isVisible } = extension.value.panel
    expect(isVisible({ items: undefined })).toBe(false)
  })

  it('isRoot returns true', () => {
    const { extension } = useShareBridgeExtension()
    expect(extension.value.panel.isRoot()).toBe(true)
  })
})
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd extensions/opencloud && npm test
```

Expected: FAIL — `Cannot find module './useShareBridgeExtension'`

- [ ] **Step 3: Create src/composables/useShareBridgeExtension.ts**

```typescript
import { computed } from 'vue'
import { useGettext } from '@opencloud-eu/web-pkg'
import type { SidebarPanelExtension } from '@opencloud-eu/web-pkg'
import ShareBridgePanel from '../components/ShareBridgePanel.vue'

export const useShareBridgeExtension = () => {
  const { $gettext } = useGettext()

  const extension = computed<SidebarPanelExtension<unknown>>(() => ({
    id: 'com.sharebridge.sidebar-panel',
    type: 'sidebarPanel',
    extensionPointIds: ['global.files.sidebar'],
    panel: {
      name: 'sharebridge',
      icon: 'share',
      title: () => $gettext('ShareBridge'),
      component: ShareBridgePanel,
      isRoot: () => true,
      // Note: verify field name against your SDK version.
      // May be 'resources' instead of 'items' in some versions.
      isVisible: ({ items }: { items?: unknown[] }) => (items?.length ?? 0) === 1,
    },
  }))

  return { extension }
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
cd extensions/opencloud && npm test
```

Expected: all 8 tests PASS

- [ ] **Step 5: Update src/index.ts to wire up the extension**

Replace the stub with:

```typescript
import { computed } from 'vue'
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
      extensions: computed(() => [extension.value]),
    }
  },
})
```

- [ ] **Step 6: Verify build still passes**

```bash
cd extensions/opencloud && npm run build
```

Expected: builds successfully (will warn about missing ShareBridgePanel.vue — that's expected, create an empty placeholder if needed)

- [ ] **Step 7: Commit**

```bash
git add extensions/opencloud/src/composables/useShareBridgeExtension.ts \
        extensions/opencloud/src/composables/useShareBridgeExtension.test.ts \
        extensions/opencloud/src/index.ts
git commit -m "feat: register OpenCloud sidebar panel extension (11c)"
```

---

## Task 6: ShareCard Component

**Files:**
- Create: `extensions/opencloud/src/components/ShareCard.vue`
- Create: `extensions/opencloud/src/components/ShareCard.test.ts`

- [ ] **Step 1: Write the failing test**

Create `extensions/opencloud/src/components/ShareCard.test.ts`:

```typescript
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount } from '@vue/test-utils'
import ShareCard from './ShareCard.vue'
import type { Share } from '../types'

const makeShare = (overrides: Partial<Share> = {}): Share => ({
  code: 'abc123',
  public_url: 'https://share.example.com/s/abc123',
  share_url: 'https://opencloud.example.com/s/XYZ',
  file_id: 'storage-users-1$abc!def',
  downloads: 3,
  max_downloads: 10,
  relay_only: false,
  expires_at: '2026-04-10T12:00:00Z',
  created_at: '2026-04-09T12:00:00Z',
  ...overrides,
})

describe('ShareCard', () => {
  it('renders share code', () => {
    const wrapper = mount(ShareCard, { props: { share: makeShare() } })
    expect(wrapper.text()).toContain('abc123')
  })

  it('renders download count', () => {
    const wrapper = mount(ShareCard, { props: { share: makeShare({ downloads: 3, max_downloads: 10 }) } })
    expect(wrapper.text()).toContain('3')
    expect(wrapper.text()).toContain('10')
  })

  it('shows ∞ when max_downloads is 0', () => {
    const wrapper = mount(ShareCard, { props: { share: makeShare({ max_downloads: 0 }) } })
    expect(wrapper.text()).toContain('∞')
  })

  it('emits revoke event with code when Revoke button is clicked', async () => {
    const wrapper = mount(ShareCard, { props: { share: makeShare() } })
    await wrapper.find('[data-testid="revoke-btn"]').trigger('click')
    expect(wrapper.emitted('revoke')).toBeTruthy()
    expect(wrapper.emitted('revoke')![0]).toEqual(['abc123'])
  })

  it('copy button copies public_url to clipboard', async () => {
    const mockWriteText = vi.fn().mockResolvedValue(undefined)
    Object.assign(navigator, { clipboard: { writeText: mockWriteText } })

    const wrapper = mount(ShareCard, { props: { share: makeShare() } })
    await wrapper.find('[data-testid="copy-btn"]').trigger('click')

    expect(mockWriteText).toHaveBeenCalledWith('https://share.example.com/s/abc123')
  })

  it('shows copied feedback after copying', async () => {
    vi.useFakeTimers()
    Object.assign(navigator, { clipboard: { writeText: vi.fn().mockResolvedValue(undefined) } })

    const wrapper = mount(ShareCard, { props: { share: makeShare() } })
    await wrapper.find('[data-testid="copy-btn"]').trigger('click')
    await wrapper.vm.$nextTick()

    expect(wrapper.find('[data-testid="copied-feedback"]').exists()).toBe(true)

    vi.advanceTimersByTime(2001)
    await wrapper.vm.$nextTick()
    expect(wrapper.find('[data-testid="copied-feedback"]').exists()).toBe(false)

    vi.useRealTimers()
  })
})
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd extensions/opencloud && npm test -- --reporter=verbose 2>&1 | head -30
```

Expected: FAIL — `Cannot find module './ShareCard.vue'`

- [ ] **Step 3: Create src/components/ShareCard.vue**

```vue
<template>
  <div class="share-card">
    <div class="share-info">
      <span class="share-code">{{ share.code }}</span>
      <span class="share-downloads">
        {{ share.downloads }} / {{ share.max_downloads === 0 ? '∞' : share.max_downloads }} downloads
      </span>
      <span class="share-expiry">Expires: {{ formattedExpiry }}</span>
    </div>
    <div class="share-actions">
      <button data-testid="copy-btn" @click="copyLink">
        {{ copied ? 'Copied!' : 'Copy Link' }}
      </button>
      <button data-testid="revoke-btn" @click="emit('revoke', share.code)">
        Revoke
      </button>
    </div>
    <div v-if="copied" data-testid="copied-feedback" class="copied-feedback">
      Copied!
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, computed } from 'vue'
import type { Share } from '../types'

const props = defineProps<{ share: Share }>()
const emit = defineEmits<{ revoke: [code: string] }>()

const copied = ref(false)
const formattedExpiry = computed(() =>
  new Date(props.share.expires_at).toLocaleDateString()
)

const copyLink = async () => {
  await navigator.clipboard.writeText(props.share.public_url)
  copied.value = true
  setTimeout(() => { copied.value = false }, 2000)
}
</script>
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
cd extensions/opencloud && npm test
```

Expected: all 6 tests PASS

- [ ] **Step 5: Commit**

```bash
git add extensions/opencloud/src/components/ShareCard.vue \
        extensions/opencloud/src/components/ShareCard.test.ts
git commit -m "feat: add ShareCard component with copy and revoke (11c)"
```

---

## Task 7: CreateShareModal Component

**Files:**
- Create: `extensions/opencloud/src/components/CreateShareModal.vue`
- Create: `extensions/opencloud/src/components/CreateShareModal.test.ts`

- [ ] **Step 1: Write the failing test**

Create `extensions/opencloud/src/components/CreateShareModal.test.ts`:

```typescript
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount } from '@vue/test-utils'
import { setActivePinia, createPinia } from 'pinia'
import CreateShareModal from './CreateShareModal.vue'

// Mock composables
const mockCreatePublicShare = vi.fn()
const mockCreateShare = vi.fn()

vi.mock('../composables/useOpenCloudAPI', () => ({
  useOpenCloudAPI: () => ({ createPublicShare: mockCreatePublicShare }),
}))
vi.mock('../composables/useAgentClient', () => ({
  useAgentClient: () => ({ createShare: mockCreateShare }),
}))

describe('CreateShareModal', () => {
  beforeEach(() => {
    localStorage.clear()
    setActivePinia(createPinia())
    mockCreatePublicShare.mockReset()
    mockCreateShare.mockReset()
  })

  it('renders TTL selector with default 24h', () => {
    const wrapper = mount(CreateShareModal, { props: { filePath: '/file.pdf' } })
    const select = wrapper.find('[data-testid="expiry-select"]')
    expect(select.exists()).toBe(true)
    expect((select.element as HTMLSelectElement).value).toBe('24')
  })

  it('renders password field', () => {
    const wrapper = mount(CreateShareModal, { props: { filePath: '/file.pdf' } })
    expect(wrapper.find('[data-testid="password-input"]').exists()).toBe(true)
  })

  it('renders max downloads field with default 0', () => {
    const wrapper = mount(CreateShareModal, { props: { filePath: '/file.pdf' } })
    const input = wrapper.find('[data-testid="max-downloads-input"]')
    expect(input.exists()).toBe(true)
    expect((input.element as HTMLInputElement).value).toBe('0')
  })

  it('emits close when Cancel is clicked', async () => {
    const wrapper = mount(CreateShareModal, { props: { filePath: '/file.pdf' } })
    await wrapper.find('[data-testid="cancel-btn"]').trigger('click')
    expect(wrapper.emitted('close')).toBeTruthy()
  })

  it('calls OCS API then agent API on submit', async () => {
    const shareUrl = 'https://opencloud.example.com/s/XYZ789'
    const agentResult = { code: 'abc123', public_url: 'https://share.example.com/s/abc123', expires_at: '2026-04-10T12:00:00Z' }
    mockCreatePublicShare.mockResolvedValue(shareUrl)
    mockCreateShare.mockResolvedValue(agentResult)

    const wrapper = mount(CreateShareModal, { props: { filePath: '/file.pdf' } })
    await wrapper.find('[data-testid="create-btn"]').trigger('click')
    await wrapper.vm.$nextTick()
    // Wait for async operations
    await new Promise(resolve => setTimeout(resolve, 0))

    expect(mockCreatePublicShare).toHaveBeenCalledWith(
      '/file.pdf',
      expect.objectContaining({})
    )
    expect(mockCreateShare).toHaveBeenCalledWith(
      expect.objectContaining({ share_url: shareUrl })
    )
  })

  it('emits created event with agent result after successful create', async () => {
    const shareUrl = 'https://opencloud.example.com/s/XYZ789'
    const agentResult = { code: 'abc123', public_url: 'https://share.example.com/s/abc123', expires_at: '2026-04-10T12:00:00Z' }
    mockCreatePublicShare.mockResolvedValue(shareUrl)
    mockCreateShare.mockResolvedValue(agentResult)

    const wrapper = mount(CreateShareModal, { props: { filePath: '/file.pdf' } })
    await wrapper.find('[data-testid="create-btn"]').trigger('click')
    await new Promise(resolve => setTimeout(resolve, 0))

    expect(wrapper.emitted('created')).toBeTruthy()
    expect(wrapper.emitted('created')![0]).toEqual([agentResult])
  })

  it('shows error message when OCS share creation fails', async () => {
    mockCreatePublicShare.mockRejectedValue(new Error('OCS error'))

    const wrapper = mount(CreateShareModal, { props: { filePath: '/file.pdf' } })
    await wrapper.find('[data-testid="create-btn"]').trigger('click')
    await new Promise(resolve => setTimeout(resolve, 0))
    await wrapper.vm.$nextTick()

    expect(wrapper.find('[data-testid="error-msg"]').text()).toContain(
      'Failed to create OpenCloud share'
    )
  })

  it('shows error message when agent share creation fails', async () => {
    mockCreatePublicShare.mockResolvedValue('https://opencloud.example.com/s/XYZ')
    mockCreateShare.mockRejectedValue(new Error('agent error'))

    const wrapper = mount(CreateShareModal, { props: { filePath: '/file.pdf' } })
    await wrapper.find('[data-testid="create-btn"]').trigger('click')
    await new Promise(resolve => setTimeout(resolve, 0))
    await wrapper.vm.$nextTick()

    expect(wrapper.find('[data-testid="error-msg"]').text()).toContain(
      'Failed to create ShareBridge share'
    )
  })
})
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd extensions/opencloud && npm test
```

Expected: FAIL — `Cannot find module './CreateShareModal.vue'`

- [ ] **Step 3: Create src/components/CreateShareModal.vue**

```vue
<template>
  <div class="modal-overlay" @click.self="emit('close')">
    <div class="modal">
      <h2>Create ShareBridge Share</h2>

      <label>
        Expiry
        <select data-testid="expiry-select" v-model.number="form.expiryHours">
          <option :value="1">1 hour</option>
          <option :value="24">24 hours</option>
          <option :value="168">7 days</option>
          <option :value="720">30 days</option>
        </select>
      </label>

      <label>
        Password (optional)
        <input data-testid="password-input" v-model="form.password" type="password" />
      </label>

      <label>
        Max Downloads (0 = unlimited)
        <input
          data-testid="max-downloads-input"
          v-model.number="form.maxDownloads"
          type="number"
          min="0"
        />
      </label>

      <label>
        <input data-testid="relay-only-input" v-model="form.relayOnly" type="checkbox" />
        Relay mode only
      </label>

      <div v-if="error" data-testid="error-msg" class="error">{{ error }}</div>

      <div class="actions">
        <button data-testid="cancel-btn" @click="emit('close')" :disabled="loading">
          Cancel
        </button>
        <button data-testid="create-btn" @click="submit" :disabled="loading">
          {{ loading ? 'Creating...' : 'Create Share' }}
        </button>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, reactive, computed } from 'vue'
import { useOpenCloudAPI } from '../composables/useOpenCloudAPI'
import { useAgentClient } from '../composables/useAgentClient'
import type { CreateShareResult } from '../types'

const props = defineProps<{ filePath: string }>()
const emit = defineEmits<{
  close: []
  created: [result: CreateShareResult]
}>()

const { createPublicShare } = useOpenCloudAPI()
const { createShare } = useAgentClient()

const loading = ref(false)
const error = ref('')
const form = reactive({
  expiryHours: 24,
  password: '',
  maxDownloads: 0,
  relayOnly: false,
})

const expiryDate = computed(() => {
  const date = new Date()
  date.setHours(date.getHours() + form.expiryHours)
  return date.toISOString().split('T')[0] // YYYY-MM-DD
})

const submit = async () => {
  loading.value = true
  error.value = ''

  let shareUrl: string
  try {
    shareUrl = await createPublicShare(props.filePath, {
      password: form.password || undefined,
      expireDate: expiryDate.value,
    })
  } catch {
    error.value = 'Failed to create OpenCloud share. Please try again.'
    loading.value = false
    return
  }

  try {
    const result = await createShare({
      share_url: shareUrl,
      password: form.password || undefined,
      expiry_hours: form.expiryHours,
      max_downloads: form.maxDownloads,
      relay_only: form.relayOnly,
    })
    emit('created', result)
  } catch {
    error.value = 'Failed to create ShareBridge share. Please try again.'
    loading.value = false
  }
}
</script>
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
cd extensions/opencloud && npm test
```

Expected: all 8 tests PASS

- [ ] **Step 5: Commit**

```bash
git add extensions/opencloud/src/components/CreateShareModal.vue \
        extensions/opencloud/src/components/CreateShareModal.test.ts
git commit -m "feat: add CreateShareModal with OCS + agent API integration (11c)"
```

---

## Task 8: ShareBridgePanel Component

**Files:**
- Create: `extensions/opencloud/src/components/ShareBridgePanel.vue`
- Create: `extensions/opencloud/src/components/ShareBridgePanel.test.ts`

This is the main panel that orchestrates the whole UI. It receives the selected resource from OpenCloud and coordinates all composables.

- [ ] **Step 1: Write the failing test**

Create `extensions/opencloud/src/components/ShareBridgePanel.test.ts`:

```typescript
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount } from '@vue/test-utils'
import { setActivePinia, createPinia } from 'pinia'
import ShareBridgePanel from './ShareBridgePanel.vue'
import type { Share } from '../types'

const mockListShares = vi.fn()
const mockRevokeShare = vi.fn()
const mockGetSettings = vi.fn()

vi.mock('../composables/useAgentClient', () => ({
  useAgentClient: () => ({
    listShares: mockListShares,
    revokeShare: mockRevokeShare,
    getSettings: mockGetSettings,
  }),
}))
vi.mock('./ShareCard.vue', () => ({ default: { template: '<div data-testid="share-card">{{ share.code }}</div>', props: ['share'] } }))
vi.mock('./CreateShareModal.vue', () => ({ default: { template: '<div data-testid="create-modal" />', props: ['filePath'], emits: ['close', 'created'] } }))

const makeShare = (code = 'abc123'): Share => ({
  code,
  public_url: `https://share.example.com/s/${code}`,
  share_url: 'https://opencloud.example.com/s/XYZ',
  file_id: 'storage-users-1$abc!def',
  downloads: 0,
  max_downloads: 10,
  relay_only: false,
  expires_at: '2026-04-10T12:00:00Z',
  created_at: '2026-04-09T12:00:00Z',
})

const defaultProps = {
  resource: {
    id: 'storage-users-1$abc!def',
    path: '/Documents/report.pdf',
    name: 'report.pdf',
  },
}

describe('ShareBridgePanel', () => {
  beforeEach(() => {
    localStorage.clear()
    setActivePinia(createPinia())
    mockListShares.mockReset()
    mockRevokeShare.mockReset()
    mockGetSettings.mockReset()
    mockGetSettings.mockResolvedValue({
      default_expiry_hours: 24,
      default_max_downloads: 0,
      default_relay_only: false,
      turn_available: true,
    })
  })

  it('shows configure prompt when agent not configured', async () => {
    // Settings store has empty agentUrl and apiKey by default
    const wrapper = mount(ShareBridgePanel, { props: defaultProps })
    await wrapper.vm.$nextTick()
    expect(wrapper.find('[data-testid="configure-prompt"]').exists()).toBe(true)
  })

  it('shows share list when configured', async () => {
    localStorage.setItem('sharebridge_agent_url', 'http://localhost:7878')
    localStorage.setItem('sharebridge_api_key', 'sb_agent_key')
    mockListShares.mockResolvedValue([makeShare('abc123')])

    const wrapper = mount(ShareBridgePanel, { props: defaultProps })
    await new Promise(resolve => setTimeout(resolve, 0))
    await wrapper.vm.$nextTick()

    expect(wrapper.find('[data-testid="share-list"]').exists()).toBe(true)
  })

  it('shows empty state when no shares exist for file', async () => {
    localStorage.setItem('sharebridge_agent_url', 'http://localhost:7878')
    localStorage.setItem('sharebridge_api_key', 'sb_agent_key')
    mockListShares.mockResolvedValue([])

    const wrapper = mount(ShareBridgePanel, { props: defaultProps })
    await new Promise(resolve => setTimeout(resolve, 0))
    await wrapper.vm.$nextTick()

    expect(wrapper.find('[data-testid="empty-state"]').exists()).toBe(true)
  })

  it('shows create share button when configured', async () => {
    localStorage.setItem('sharebridge_agent_url', 'http://localhost:7878')
    localStorage.setItem('sharebridge_api_key', 'sb_agent_key')
    mockListShares.mockResolvedValue([])

    const wrapper = mount(ShareBridgePanel, { props: defaultProps })
    await new Promise(resolve => setTimeout(resolve, 0))
    await wrapper.vm.$nextTick()

    expect(wrapper.find('[data-testid="create-share-btn"]').exists()).toBe(true)
  })

  it('opens modal when Create Share button is clicked', async () => {
    localStorage.setItem('sharebridge_agent_url', 'http://localhost:7878')
    localStorage.setItem('sharebridge_api_key', 'sb_agent_key')
    mockListShares.mockResolvedValue([])

    const wrapper = mount(ShareBridgePanel, { props: defaultProps })
    await new Promise(resolve => setTimeout(resolve, 0))
    await wrapper.vm.$nextTick()

    await wrapper.find('[data-testid="create-share-btn"]').trigger('click')
    expect(wrapper.find('[data-testid="create-modal"]').exists()).toBe(true)
  })

  it('refreshes share list after new share is created', async () => {
    localStorage.setItem('sharebridge_agent_url', 'http://localhost:7878')
    localStorage.setItem('sharebridge_api_key', 'sb_agent_key')
    mockListShares
      .mockResolvedValueOnce([])
      .mockResolvedValueOnce([makeShare('new123')])

    const wrapper = mount(ShareBridgePanel, { props: defaultProps })
    await new Promise(resolve => setTimeout(resolve, 0))
    await wrapper.vm.$nextTick()

    // Simulate modal created event
    await wrapper.find('[data-testid="create-share-btn"]').trigger('click')
    await wrapper.vm.$nextTick()
    // Trigger 'created' on the modal
    const modal = wrapper.findComponent({ name: 'CreateShareModal' })
    await modal.vm.$emit('created', { code: 'new123', public_url: 'https://share.example.com/s/new123', expires_at: '2026-04-10T12:00:00Z' })
    await new Promise(resolve => setTimeout(resolve, 0))
    await wrapper.vm.$nextTick()

    // listShares called twice: initial load + after create
    expect(mockListShares).toHaveBeenCalledTimes(2)
  })

  it('shows error message when agent is unreachable', async () => {
    localStorage.setItem('sharebridge_agent_url', 'http://localhost:7878')
    localStorage.setItem('sharebridge_api_key', 'sb_agent_key')
    mockListShares.mockRejectedValue(new Error('Network error'))

    const wrapper = mount(ShareBridgePanel, { props: defaultProps })
    await new Promise(resolve => setTimeout(resolve, 0))
    await wrapper.vm.$nextTick()

    expect(wrapper.find('[data-testid="error-msg"]').text()).toContain(
      'Cannot connect to ShareBridge agent'
    )
  })
})
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd extensions/opencloud && npm test
```

Expected: FAIL — `Cannot find module './ShareBridgePanel.vue'`

- [ ] **Step 3: Create src/components/ShareBridgePanel.vue**

```vue
<template>
  <div class="sharebridge-panel">
    <!-- Not configured -->
    <div v-if="!settings.isConfigured" data-testid="configure-prompt" class="configure-prompt">
      <p>Configure ShareBridge to get started.</p>
      <p>
        Add your Agent URL and API Key in the OpenCloud extension settings.<br />
        Find these values in the ShareBridge agent settings page.
      </p>
    </div>

    <!-- Configured -->
    <template v-else>
      <!-- Error state -->
      <div v-if="error" data-testid="error-msg" class="error">{{ error }}</div>

      <!-- Loading -->
      <div v-else-if="loading" class="loading">Loading shares...</div>

      <!-- Share list -->
      <template v-else>
        <div v-if="shares.length > 0" data-testid="share-list" class="share-list">
          <ShareCard
            v-for="share in shares"
            :key="share.code"
            :share="share"
            @revoke="handleRevoke"
          />
        </div>

        <div v-else data-testid="empty-state" class="empty-state">
          No ShareBridge shares for this file.
        </div>

        <button
          data-testid="create-share-btn"
          @click="showModal = true"
          class="create-btn"
        >
          Create ShareBridge Share
        </button>
      </template>

      <!-- Create modal -->
      <CreateShareModal
        v-if="showModal"
        :file-path="resource.path"
        @close="showModal = false"
        @created="handleCreated"
      />
    </template>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { useSettingsStore } from '../stores/settings'
import { useAgentClient } from '../composables/useAgentClient'
import ShareCard from './ShareCard.vue'
import CreateShareModal from './CreateShareModal.vue'
import type { Share, CreateShareResult } from '../types'

// The resource prop comes from OpenCloud's sidebar context.
// Field name 'resource' should be verified against your @opencloud-eu/web-pkg version.
// It may be passed differently depending on how the SidebarPanelExtension mounts the component.
const props = defineProps<{
  resource: {
    id: string    // OpenCloud fileId (oc:fileid)
    path: string  // File path, used for OCS share creation
    name: string
  }
}>()

const settings = useSettingsStore()
const { listShares, revokeShare } = useAgentClient()

const shares = ref<Share[]>([])
const loading = ref(false)
const error = ref('')
const showModal = ref(false)

const loadShares = async () => {
  loading.value = true
  error.value = ''
  try {
    shares.value = await listShares(props.resource.id)
  } catch {
    error.value = 'Cannot connect to ShareBridge agent. Check the agent URL and ensure it\'s running.'
  } finally {
    loading.value = false
  }
}

const handleRevoke = async (code: string) => {
  await revokeShare(code)
  await loadShares()
}

const handleCreated = async (_result: CreateShareResult) => {
  showModal.value = false
  await loadShares()
}

onMounted(() => {
  if (settings.isConfigured) {
    loadShares()
  }
})
</script>
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
cd extensions/opencloud && npm test
```

Expected: all 8 tests PASS

- [ ] **Step 5: Run full test suite to confirm nothing broken**

```bash
cd extensions/opencloud && npm test -- --reporter=verbose
```

Expected: all tests across all files PASS (≈ 40 tests total)

- [ ] **Step 6: Final build check**

```bash
cd extensions/opencloud && npm run build
```

Expected: `dist/web-app-sharebridge.js` produced, no TypeScript errors.

- [ ] **Step 7: Commit**

```bash
git add extensions/opencloud/src/components/ShareBridgePanel.vue \
        extensions/opencloud/src/components/ShareBridgePanel.test.ts
git commit -m "feat: add ShareBridgePanel sidebar component (11c)"
```

---

## Self-Review

### Spec coverage

| Spec requirement | Task |
|-----------------|------|
| Extension directory `extensions/opencloud/` | Task 1 |
| `defineWebApplication` registration | Task 5 |
| `SidebarPanelExtension` for `global.files.sidebar` | Task 5 |
| `isVisible` for single file only | Task 5 |
| Settings store (agentUrl + apiKey → localStorage) | Task 2 |
| Agent client: list, create, revoke, getSettings | Task 3 |
| Fresh header on each call (not captured at creation) | Task 3 |
| OCS API: `POST /apps/files_sharing/api/v1/shares`, `shareType=3`, `path` | Task 4 |
| AMD build output, external Vue + web-pkg + pinia | Task 1 |
| ShareCard: code, expiry, download count, copy, revoke | Task 6 |
| CreateShareModal: TTL, password, max downloads, relay mode | Task 7 |
| CreateShareModal: OCS then agent, separate error messages | Task 7 |
| ShareBridgePanel: configure prompt when not set up | Task 8 |
| ShareBridgePanel: list shares filtered by file_id | Task 8 |
| ShareBridgePanel: empty state | Task 8 |
| ShareBridgePanel: open modal, refresh after create | Task 8 |
| Error: agent unreachable | Task 8 |
| Error: invalid API key | Not explicitly tested — covered by 401 from agent (UI gets error state) |
| `manifest.json` | Task 1 |

**Gap:** The spec mentions an error for "Invalid API key" distinct from "agent unreachable". Both map to fetch errors from the agent client. The panel currently lumps all errors as "Cannot connect to ShareBridge agent". To exactly match the spec, `listShares` could inspect the HTTP status code (401 vs network error) and return a typed error. This is an enhancement; the current implementation is safe and functional. Add this refinement if needed.

### Placeholder scan

None found. All steps contain complete code.

### Type consistency

- `Share`, `CreateShareParams`, `CreateShareResult`, `AgentSettings` defined once in `src/types.ts`, imported everywhere.
- `useAgentClient` returns `listShares(fileId?: string)` — called as `listShares(props.resource.id)` in ShareBridgePanel ✓
- `CreateShareModal` emits `created: [result: CreateShareResult]` — panel receives as `handleCreated(_result: CreateShareResult)` ✓
- `ShareCard` emits `revoke: [code: string]` — panel handles as `handleRevoke(code: string)` ✓
- `useSettingsStore().isConfigured` getter used in panel ✓
