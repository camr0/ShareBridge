# Nextcloud Extension (Slice 12) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a native Nextcloud app that adds a ShareBridge panel to the Files sidebar and a Personal Settings section for per-user agent configuration.

**Architecture:** A PHP backend (`SettingsController`) stores per-user settings and the `code→ncShareId` mapping via Nextcloud's `IConfig` API (no DB migrations). A JS frontend (Vue 3 + Pinia + `@nextcloud/vue`) registers a Files sidebar tab via `registerTab` and a settings section via `registerPersonalSettings`. On share creation the extension calls the Nextcloud OCS API to create a public link, then the ShareBridge agent to create a code. On revoke it deletes both in parallel.

**Tech Stack:** PHP 8.1, PHPUnit 10, `nextcloud/ocp` (for NC interfaces in tests); TypeScript, Vue 3, Pinia, `@nextcloud/files`, `@nextcloud/settings`, `@nextcloud/vue`, `@nextcloud/axios`, `@nextcloud/router`, `@nextcloud/l10n`, `@nextcloud/webpack-vue-config`, Vitest, `@vue/test-utils`, `happy-dom`.

---

## File Map

**New files — PHP:**
- `extensions/nextcloud/appinfo/info.xml` — app metadata, min NC 27, declares `files` dependency
- `extensions/nextcloud/appinfo/routes.php` — declares 4 API routes
- `extensions/nextcloud/lib/AppInfo/Application.php` — IBootstrap, loads JS bundle on every page
- `extensions/nextcloud/lib/Controller/SettingsController.php` — 4 endpoints: GET/PUT settings, PUT/GET ncShareId
- `extensions/nextcloud/composer.json` — phpunit + nextcloud/ocp as dev deps
- `extensions/nextcloud/tests/Controller/SettingsControllerTest.php` — PHPUnit unit tests

**New files — JS/TS:**
- `extensions/nextcloud/package.json` — scripts + devDependencies
- `extensions/nextcloud/webpack.config.js` — `@nextcloud/webpack-vue-config` for production build
- `extensions/nextcloud/vite.config.ts` — Vitest config + NC package aliases for tests
- `extensions/nextcloud/tsconfig.json`
- `extensions/nextcloud/.gitignore`
- `extensions/nextcloud/src/test/setup.ts` — happy-dom environment setup
- `extensions/nextcloud/src/test/mocks/nextcloud-axios.ts` — mock for `@nextcloud/axios`
- `extensions/nextcloud/src/test/mocks/nextcloud-router.ts` — mock for `@nextcloud/router`
- `extensions/nextcloud/src/test/mocks/nextcloud-l10n.ts` — mock for `@nextcloud/l10n`
- `extensions/nextcloud/src/test/mocks/nextcloud-vue.ts` — stub components for `@nextcloud/vue`
- `extensions/nextcloud/src/test/mocks/nextcloud-files.ts` — mock for `@nextcloud/files`
- `extensions/nextcloud/src/test/mocks/nextcloud-settings.ts` — mock for `@nextcloud/settings`
- `extensions/nextcloud/src/types.ts` — copied unchanged from `extensions/opencloud/src/types.ts`
- `extensions/nextcloud/src/composables/useAgentClient.ts` — copied unchanged from opencloud
- `extensions/nextcloud/src/composables/useAgentClient.test.ts` — adapted from opencloud
- `extensions/nextcloud/src/composables/useNextcloudOCS.ts` — OCS share create/delete + ncShareId CRUD
- `extensions/nextcloud/src/composables/useNextcloudOCS.test.ts`
- `extensions/nextcloud/src/stores/settings.ts` — async Pinia store backed by PHP endpoint
- `extensions/nextcloud/src/stores/settings.test.ts`
- `extensions/nextcloud/src/components/ShareCard.vue` — rewritten with `@nextcloud/vue`
- `extensions/nextcloud/src/components/ShareCard.test.ts`
- `extensions/nextcloud/src/components/PersonalSettings.vue` — settings form in NC Personal Settings
- `extensions/nextcloud/src/components/PersonalSettings.test.ts`
- `extensions/nextcloud/src/components/CreateShareModal.vue` — share creation form
- `extensions/nextcloud/src/components/CreateShareModal.test.ts`
- `extensions/nextcloud/src/components/ShareBridgeTab.vue` — sidebar tab root
- `extensions/nextcloud/src/components/ShareBridgeTab.test.ts`
- `extensions/nextcloud/src/main.ts` — entry: `registerTab` + `registerPersonalSettings`

---

## Task 1: Scaffold — config files and directory structure

**Files:**
- Create: `extensions/nextcloud/appinfo/info.xml`
- Create: `extensions/nextcloud/appinfo/routes.php`
- Create: `extensions/nextcloud/lib/AppInfo/Application.php`
- Create: `extensions/nextcloud/composer.json`
- Create: `extensions/nextcloud/package.json`
- Create: `extensions/nextcloud/webpack.config.js`
- Create: `extensions/nextcloud/vite.config.ts`
- Create: `extensions/nextcloud/tsconfig.json`
- Create: `extensions/nextcloud/.gitignore`

- [ ] **Step 1: Create directory structure**

```bash
mkdir -p extensions/nextcloud/appinfo
mkdir -p extensions/nextcloud/lib/AppInfo
mkdir -p extensions/nextcloud/lib/Controller
mkdir -p extensions/nextcloud/src/components
mkdir -p extensions/nextcloud/src/composables
mkdir -p extensions/nextcloud/src/stores
mkdir -p extensions/nextcloud/src/test/mocks
mkdir -p extensions/nextcloud/tests/Controller
mkdir -p extensions/nextcloud/js
```

- [ ] **Step 2: Create `appinfo/info.xml`**

```xml
<?xml version="1.0"?>
<info xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
      xsi:noNamespaceSchemaLocation="https://apps.nextcloud.com/schema/apps/info.xsd">
    <id>sharebridge</id>
    <name>ShareBridge</name>
    <summary>Share files externally via ShareBridge</summary>
    <description>Native Nextcloud Files sidebar integration for one-click ShareBridge sharing.</description>
    <version>0.1.0</version>
    <licence>AGPL</licence>
    <author>ShareBridge</author>
    <namespace>ShareBridge</namespace>
    <dependencies>
        <nextcloud min-version="27"/>
        <app>files</app>
    </dependencies>
</info>
```

- [ ] **Step 3: Create `appinfo/routes.php`**

```php
<?php
return [
    'routes' => [
        ['name' => 'settings#get',          'url' => '/api/settings',                    'verb' => 'GET'],
        ['name' => 'settings#update',       'url' => '/api/settings',                    'verb' => 'PUT'],
        ['name' => 'settings#saveNcShareId',   'url' => '/api/shares/{code}/nc-share-id',   'verb' => 'PUT'],
        ['name' => 'settings#getNcShareId',    'url' => '/api/shares/{code}/nc-share-id',   'verb' => 'GET'],
        ['name' => 'settings#deleteNcShareId', 'url' => '/api/shares/{code}/nc-share-id',   'verb' => 'DELETE'],
    ],
];
```

- [ ] **Step 4: Create `lib/AppInfo/Application.php`**

This loads `js/sharebridge-main.js` on every page. `registerTab` and `registerPersonalSettings` are no-ops on pages where their containers don't exist, so loading everywhere is safe.

```php
<?php
namespace OCA\ShareBridge\AppInfo;

use OCP\AppFramework\App;
use OCP\AppFramework\Bootstrap\IBootContext;
use OCP\AppFramework\Bootstrap\IBootstrap;
use OCP\AppFramework\Bootstrap\IRegistrationContext;
use OCP\Util;

class Application extends App implements IBootstrap {
    public const APP_ID = 'sharebridge';

    public function __construct() {
        parent::__construct(self::APP_ID);
    }

    public function register(IRegistrationContext $context): void {}

    public function boot(IBootContext $context): void {
        Util::addScript(self::APP_ID, 'sharebridge-main');
    }
}
```

- [ ] **Step 5: Create `composer.json`**

```json
{
    "name": "sharebridge/nextcloud-app",
    "description": "ShareBridge Nextcloud app",
    "require": {},
    "require-dev": {
        "nextcloud/ocp": "dev-stable27",
        "phpunit/phpunit": "^10.0"
    },
    "autoload": {
        "psr-4": {
            "OCA\\ShareBridge\\": "lib/"
        }
    },
    "autoload-dev": {
        "psr-4": {
            "OCA\\ShareBridge\\Tests\\": "tests/"
        }
    },
    "repositories": [
        {
            "type": "vcs",
            "url": "https://github.com/nextcloud/ocp"
        }
    ]
}
```

- [ ] **Step 6: Create `package.json`**

```json
{
    "name": "nextcloud-app-sharebridge",
    "version": "0.1.0",
    "private": true,
    "scripts": {
        "build": "webpack --node-env production",
        "dev": "webpack --node-env development --watch",
        "test": "vitest run",
        "test:watch": "vitest",
        "typecheck": "vue-tsc --noEmit"
    },
    "devDependencies": {
        "@nextcloud/axios": "^2.5.0",
        "@nextcloud/files": "^3.0.0",
        "@nextcloud/l10n": "^3.1.0",
        "@nextcloud/router": "^3.0.0",
        "@nextcloud/settings": "^1.0.0",
        "@nextcloud/vue": "^8.23.0",
        "@nextcloud/webpack-vue-config": "^6.1.0",
        "@vitejs/plugin-vue": "^5.0.0",
        "@vue/test-utils": "^2.4.0",
        "happy-dom": "^14.0.0",
        "pinia": "^2.1.0",
        "typescript": "^5.4.0",
        "vite": "^5.2.0",
        "vitest": "^1.6.0",
        "vue": "^3.4.0",
        "vue-tsc": "^2.0.0"
    }
}
```

- [ ] **Step 7: Create `webpack.config.js`**

```js
const path = require('path')
const webpackConfig = require('@nextcloud/webpack-vue-config')

module.exports = {
    ...webpackConfig,
    entry: {
        'sharebridge-main': path.join(__dirname, 'src', 'main.ts'),
    },
}
```

- [ ] **Step 8: Create `vite.config.ts`** (used only by Vitest — build uses webpack)

```typescript
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import path from 'path'

export default defineConfig({
    plugins: [vue()],
    resolve: {
        alias: {
            '@nextcloud/axios':    path.resolve(__dirname, 'src/test/mocks/nextcloud-axios.ts'),
            '@nextcloud/router':   path.resolve(__dirname, 'src/test/mocks/nextcloud-router.ts'),
            '@nextcloud/l10n':     path.resolve(__dirname, 'src/test/mocks/nextcloud-l10n.ts'),
            '@nextcloud/vue':      path.resolve(__dirname, 'src/test/mocks/nextcloud-vue.ts'),
            '@nextcloud/files':    path.resolve(__dirname, 'src/test/mocks/nextcloud-files.ts'),
            '@nextcloud/settings': path.resolve(__dirname, 'src/test/mocks/nextcloud-settings.ts'),
        },
    },
    test: {
        environment: 'happy-dom',
        globals: true,
        setupFiles: ['./src/test/setup.ts'],
    },
})
```

- [ ] **Step 9: Create `tsconfig.json`**

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
        "noUnusedParameters": true
    },
    "include": ["src/**/*.ts", "src/**/*.d.ts", "src/**/*.tsx", "src/**/*.vue"]
}
```

- [ ] **Step 10: Create `.gitignore`**

```
node_modules/
vendor/
js/
*.js.map
```

- [ ] **Step 11: Install dependencies**

```bash
cd extensions/nextcloud
composer install
npm install
```

Expected: both complete without errors. (Note: `nextcloud/ocp dev-stable27` requires the repository entry in composer.json to pull from GitHub.)

- [ ] **Step 12: Commit**

```bash
cd extensions/nextcloud
git add appinfo/ lib/AppInfo/ composer.json composer.lock package.json package-lock.json webpack.config.js vite.config.ts tsconfig.json .gitignore
git commit -m "feat(slice-12): scaffold Nextcloud extension"
```

---

## Task 2: PHP SettingsController (TDD)

**Files:**
- Create: `extensions/nextcloud/tests/Controller/SettingsControllerTest.php`
- Create: `extensions/nextcloud/lib/Controller/SettingsController.php`

- [ ] **Step 1: Write `tests/Controller/SettingsControllerTest.php`**

```php
<?php
namespace OCA\ShareBridge\Tests\Controller;

use OCA\ShareBridge\Controller\SettingsController;
use OCP\AppFramework\Http\JSONResponse;
use OCP\IConfig;
use OCP\IRequest;
use PHPUnit\Framework\MockObject\MockObject;
use PHPUnit\Framework\TestCase;

class SettingsControllerTest extends TestCase {
    private IConfig&MockObject $config;
    private SettingsController $controller;

    protected function setUp(): void {
        $this->config = $this->createMock(IConfig::class);
        $this->controller = new SettingsController(
            'sharebridge',
            $this->createMock(IRequest::class),
            $this->config,
            'testuser'
        );
    }

    public function testGetReturnsEmptyStringsForNewUser(): void {
        $this->config->method('getUserValue')
            ->willReturnMap([
                ['testuser', 'sharebridge', 'agent_url', '', ''],
                ['testuser', 'sharebridge', 'api_key',   '', ''],
            ]);

        $response = $this->controller->get();

        $this->assertInstanceOf(JSONResponse::class, $response);
        $this->assertEquals(['agent_url' => '', 'api_key' => ''], $response->getData());
    }

    public function testGetReturnsStoredValues(): void {
        $this->config->method('getUserValue')
            ->willReturnMap([
                ['testuser', 'sharebridge', 'agent_url', '', 'http://localhost:7878'],
                ['testuser', 'sharebridge', 'api_key',   '', 'sb_agent_abc123'],
            ]);

        $response = $this->controller->get();

        $this->assertEquals([
            'agent_url' => 'http://localhost:7878',
            'api_key'   => 'sb_agent_abc123',
        ], $response->getData());
    }

    public function testUpdateSavesValues(): void {
        $this->config->expects($this->exactly(2))
            ->method('setUserValue')
            ->willReturnCallback(function (string $uid, string $app, string $key, string $value): void {
                // just verify it's called — no return needed
            });

        $response = $this->controller->update('http://localhost:7878', 'sb_agent_abc123');

        $this->assertEquals(['status' => 'ok'], $response->getData());
    }

    public function testUpdateSavesCorrectKeys(): void {
        $calls = [];
        $this->config->method('setUserValue')
            ->willReturnCallback(function (string $uid, string $app, string $key, string $value) use (&$calls): void {
                $calls[] = [$uid, $app, $key, $value];
            });

        $this->controller->update('http://localhost:7878', 'sb_agent_abc123');

        $this->assertContains(['testuser', 'sharebridge', 'agent_url', 'http://localhost:7878'], $calls);
        $this->assertContains(['testuser', 'sharebridge', 'api_key',   'sb_agent_abc123'], $calls);
    }

    public function testSettingsArePerUser(): void {
        $otherController = new SettingsController(
            'sharebridge',
            $this->createMock(IRequest::class),
            $this->config,
            'otheruser'
        );

        // testuser has a value, otheruser has empty
        $this->config->method('getUserValue')
            ->willReturnMap([
                ['testuser',  'sharebridge', 'agent_url', '', 'http://localhost:7878'],
                ['testuser',  'sharebridge', 'api_key',   '', 'sb_key1'],
                ['otheruser', 'sharebridge', 'agent_url', '', ''],
                ['otheruser', 'sharebridge', 'api_key',   '', ''],
            ]);

        $testUserResponse  = $this->controller->get();
        $otherUserResponse = $otherController->get();

        $this->assertEquals('http://localhost:7878', $testUserResponse->getData()['agent_url']);
        $this->assertEquals('', $otherUserResponse->getData()['agent_url']);
    }

    public function testSaveNcShareIdStoresMapping(): void {
        $this->config->method('getUserValue')
            ->with('testuser', 'sharebridge', 'nc_share_ids', '{}')
            ->willReturn('{}');

        $saved = null;
        $this->config->expects($this->once())
            ->method('setUserValue')
            ->willReturnCallback(function (string $uid, string $app, string $key, string $value) use (&$saved): void {
                $saved = json_decode($value, true);
            });

        $this->controller->saveNcShareId('ABC123', '42');

        $this->assertEquals(['ABC123' => '42'], $saved);
    }

    public function testSaveNcShareIdPreservesExistingMappings(): void {
        $this->config->method('getUserValue')
            ->willReturn(json_encode(['XYZ789' => '10']));

        $saved = null;
        $this->config->method('setUserValue')
            ->willReturnCallback(function (string $uid, string $app, string $key, string $value) use (&$saved): void {
                $saved = json_decode($value, true);
            });

        $this->controller->saveNcShareId('ABC123', '42');

        $this->assertEquals(['XYZ789' => '10', 'ABC123' => '42'], $saved);
    }

    public function testGetNcShareIdReturnsStoredId(): void {
        $this->config->method('getUserValue')
            ->willReturn(json_encode(['ABC123' => '42']));

        $response = $this->controller->getNcShareId('ABC123');

        $this->assertEquals(200, $response->getStatus());
        $this->assertEquals(['nc_share_id' => '42'], $response->getData());
    }

    public function testGetNcShareIdReturns404ForUnknownCode(): void {
        $this->config->method('getUserValue')
            ->willReturn('{}');

        $response = $this->controller->getNcShareId('UNKNOWN');

        $this->assertEquals(404, $response->getStatus());
    }

    public function testDeleteNcShareIdRemovesEntry(): void {
        $this->config->method('getUserValue')
            ->willReturn(json_encode(['ABC123' => '42', 'XYZ789' => '10']));

        $saved = null;
        $this->config->expects($this->once())
            ->method('setUserValue')
            ->willReturnCallback(function (string $uid, string $app, string $key, string $value) use (&$saved): void {
                $saved = json_decode($value, true);
            });

        $this->controller->deleteNcShareId('ABC123');

        $this->assertArrayNotHasKey('ABC123', $saved);
        $this->assertArrayHasKey('XYZ789', $saved);
    }

    public function testDeleteNcShareIdOnUnknownCodeIsNoop(): void {
        $this->config->method('getUserValue')
            ->willReturn(json_encode(['XYZ789' => '10']));

        $this->config->expects($this->once())->method('setUserValue');

        $response = $this->controller->deleteNcShareId('UNKNOWN');

        $this->assertEquals(200, $response->getStatus());
    }
}
```

- [ ] **Step 2: Run tests to see them fail**

```bash
cd extensions/nextcloud
./vendor/bin/phpunit tests/
```

Expected: `Error: Class "OCA\ShareBridge\Controller\SettingsController" not found`

- [ ] **Step 3: Write `lib/Controller/SettingsController.php`**

```php
<?php
namespace OCA\ShareBridge\Controller;

use OCP\AppFramework\Controller;
use OCP\AppFramework\Http\JSONResponse;
use OCP\IConfig;
use OCP\IRequest;

class SettingsController extends Controller {
    public function __construct(
        string $appName,
        IRequest $request,
        private IConfig $config,
        private string $userId,
    ) {
        parent::__construct($appName, $request);
    }

    public function get(): JSONResponse {
        return new JSONResponse([
            'agent_url' => $this->config->getUserValue($this->userId, 'sharebridge', 'agent_url', ''),
            'api_key'   => $this->config->getUserValue($this->userId, 'sharebridge', 'api_key',   ''),
        ]);
    }

    public function update(string $agent_url, string $api_key): JSONResponse {
        $this->config->setUserValue($this->userId, 'sharebridge', 'agent_url', $agent_url);
        $this->config->setUserValue($this->userId, 'sharebridge', 'api_key',   $api_key);
        return new JSONResponse(['status' => 'ok']);
    }

    public function saveNcShareId(string $code, string $nc_share_id): JSONResponse {
        $blob    = $this->config->getUserValue($this->userId, 'sharebridge', 'nc_share_ids', '{}');
        $mapping = json_decode($blob, true) ?: [];
        $mapping[$code] = $nc_share_id;
        $this->config->setUserValue($this->userId, 'sharebridge', 'nc_share_ids', json_encode($mapping));
        return new JSONResponse(['status' => 'ok']);
    }

    public function getNcShareId(string $code): JSONResponse {
        $blob    = $this->config->getUserValue($this->userId, 'sharebridge', 'nc_share_ids', '{}');
        $mapping = json_decode($blob, true) ?: [];
        if (!array_key_exists($code, $mapping)) {
            return new JSONResponse(['message' => 'not found'], 404);
        }
        return new JSONResponse(['nc_share_id' => $mapping[$code]]);
    }

    public function deleteNcShareId(string $code): JSONResponse {
        $blob    = $this->config->getUserValue($this->userId, 'sharebridge', 'nc_share_ids', '{}');
        $mapping = json_decode($blob, true) ?: [];
        unset($mapping[$code]);
        $this->config->setUserValue($this->userId, 'sharebridge', 'nc_share_ids', json_encode($mapping));
        return new JSONResponse(['status' => 'ok']);
    }
}
```

- [ ] **Step 4: Run tests to see them pass**

```bash
cd extensions/nextcloud
./vendor/bin/phpunit tests/
```

Expected: `OK (8 tests, N assertions)`

- [ ] **Step 5: Commit**

```bash
git add lib/Controller/SettingsController.php tests/Controller/SettingsControllerTest.php
git commit -m "feat(slice-12): PHP SettingsController with IConfig-backed settings and ncShareId CRUD"
```

---

## Task 3: JS test infrastructure + shared source files

**Files:**
- Create: `extensions/nextcloud/src/test/setup.ts`
- Create: `extensions/nextcloud/src/test/mocks/nextcloud-axios.ts`
- Create: `extensions/nextcloud/src/test/mocks/nextcloud-router.ts`
- Create: `extensions/nextcloud/src/test/mocks/nextcloud-l10n.ts`
- Create: `extensions/nextcloud/src/test/mocks/nextcloud-vue.ts`
- Create: `extensions/nextcloud/src/test/mocks/nextcloud-files.ts`
- Create: `extensions/nextcloud/src/test/mocks/nextcloud-settings.ts`
- Create: `extensions/nextcloud/src/types.ts` (copy from opencloud)
- Create: `extensions/nextcloud/src/composables/useAgentClient.ts` (copy from opencloud)
- Create: `extensions/nextcloud/src/composables/useAgentClient.test.ts` (adapted from opencloud)

- [ ] **Step 1: Create `src/test/setup.ts`**

No localStorage mock needed (NC extension doesn't use localStorage).

```typescript
// Vitest happy-dom setup — no additional globals needed for this extension
```

- [ ] **Step 2: Create `src/test/mocks/nextcloud-axios.ts`**

Returns a shared vi.fn() object so tests can call `mockResolvedValue()` on it.

```typescript
import { vi } from 'vitest'

const axios = {
    get:    vi.fn(),
    post:   vi.fn(),
    put:    vi.fn(),
    delete: vi.fn(),
}

export default axios
```

- [ ] **Step 3: Create `src/test/mocks/nextcloud-router.ts`**

```typescript
import { vi } from 'vitest'

export const generateUrl = vi.fn((path: string): string => path)
```

- [ ] **Step 4: Create `src/test/mocks/nextcloud-l10n.ts`**

```typescript
export const t = (_app: string, text: string): string => text
```

- [ ] **Step 5: Create `src/test/mocks/nextcloud-vue.ts`**

Simple template stubs — just enough for tests to mount and interact.

```typescript
export const NcButton = {
    template: '<button @click="$emit(\'click\')"><slot /></button>',
    emits: ['click'],
}

export const NcModal = {
    props: ['name', 'show'],
    emits: ['close'],
    template: '<div v-if="show !== false" class="nc-modal"><slot /></div>',
}

export const NcTextField = {
    props: ['modelValue', 'label'],
    emits: ['update:modelValue'],
    template: '<input :value="modelValue" @input="$emit(\'update:modelValue\', ($event.target as HTMLInputElement).value)" />',
}

export const NcLoadingIcon = {
    template: '<div class="nc-loading-icon" />',
}

export const NcEmptyContent = {
    props: ['name', 'description'],
    template: '<div class="nc-empty-content"><slot /></div>',
}

export const NcSettingsSection = {
    props: ['name', 'description'],
    template: '<section><slot /></section>',
}

export const NcCheckboxRadioSwitch = {
    props: ['checked'],
    emits: ['update:checked'],
    template: '<input type="checkbox" :checked="checked" @change="$emit(\'update:checked\', !checked)" />',
}

export const NcBadge = {
    template: '<span class="nc-badge"><slot /></span>',
}

export const NcSelect = {
    props: ['modelValue', 'options'],
    emits: ['update:modelValue'],
    template: `
        <select :value="modelValue" @change="$emit('update:modelValue', ($event.target as HTMLSelectElement).value)">
            <option v-for="opt in options" :key="opt.value" :value="opt.value">{{ opt.label }}</option>
        </select>
    `,
}
```

- [ ] **Step 6: Create `src/test/mocks/nextcloud-files.ts`**

```typescript
import { vi } from 'vitest'

export const registerTab = vi.fn()
```

- [ ] **Step 7: Create `src/test/mocks/nextcloud-settings.ts`**

```typescript
import { vi } from 'vitest'

export const registerPersonalSettings = vi.fn()
```

- [ ] **Step 8: Copy `src/types.ts` from opencloud**

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

- [ ] **Step 9: Create temporary `src/stores/settings.ts` stub**

This stub satisfies the import in `useAgentClient.ts` and its tests. It is fully replaced in Task 4.

```typescript
// src/stores/settings.ts — temporary stub, replaced in Task 4
import { defineStore } from 'pinia'
export const useSettingsStore = defineStore('sharebridge-settings', {
    state: () => ({ agentUrl: '', apiKey: '', loading: false, loaded: false }),
    getters: { isConfigured: (s) => s.agentUrl !== '' && s.apiKey !== '' },
    actions: {
        async fetchSettings() {},
        async saveSettings()  {},
    },
})
```

- [ ] **Step 11: Copy `src/composables/useAgentClient.ts` from opencloud**

This file is identical to the opencloud version. The settings store exposes the same `agentUrl` and `apiKey` state properties (read synchronously), so the composable works unchanged.

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

- [ ] **Step 12: Create `src/composables/useAgentClient.test.ts`**

Adapted from opencloud: no localStorage, store state set directly via `$patch`.

```typescript
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { setActivePinia, createPinia } from 'pinia'
import { useAgentClient } from './useAgentClient'
import { useSettingsStore } from '../stores/settings'

describe('useAgentClient', () => {
    let mockFetch: ReturnType<typeof vi.fn>

    beforeEach(() => {
        setActivePinia(createPinia())
        mockFetch = vi.fn()
        vi.stubGlobal('fetch', mockFetch)
    })

    afterEach(() => {
        vi.unstubAllGlobals()
    })

    const setupStore = (url = 'http://localhost:7878', key = 'sb_agent_testkey') => {
        const store = useSettingsStore()
        store.$patch({ agentUrl: url, apiKey: key })
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
        await listShares('12345')

        expect(mockFetch).toHaveBeenCalledWith(
            'http://localhost:7878/api/v1/shares?file_id=12345',
            expect.anything()
        )
    })

    it('createShare POSTs JSON to /api/v1/shares', async () => {
        setupStore()
        const mockResult = { code: 'abc123', public_url: 'https://share.example.com/s/abc123', expires_at: '2026-04-15T00:00:00Z' }
        mockFetch.mockResolvedValue({ json: () => Promise.resolve(mockResult) })

        const { createShare } = useAgentClient()
        const result = await createShare({
            share_url: 'https://nextcloud.example.com/s/XYZ789',
            expiry_hours: 24,
            max_downloads: 0,
            relay_only: false,
        })

        expect(mockFetch).toHaveBeenCalledWith(
            'http://localhost:7878/api/v1/shares',
            expect.objectContaining({ method: 'POST' })
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

    it('getSettings fetches /api/v1/settings with X-API-Key', async () => {
        setupStore()
        const mockSettings = { default_expiry_hours: 24, default_max_downloads: 0, default_relay_only: false, turn_available: true }
        mockFetch.mockResolvedValue({ json: () => Promise.resolve(mockSettings) })

        const { getSettings } = useAgentClient()
        const result = await getSettings()

        expect(mockFetch).toHaveBeenCalledWith(
            'http://localhost:7878/api/v1/settings',
            expect.objectContaining({ headers: expect.objectContaining({ 'X-API-Key': 'sb_agent_testkey' }) })
        )
        expect(result).toEqual(mockSettings)
    })
})
```

- [ ] **Step 13: Run tests**

```bash
cd extensions/nextcloud
npm test
```

Expected: 5 passing tests in `useAgentClient.test.ts`.

- [ ] **Step 14: Commit**

```bash
git add src/test/ src/types.ts src/stores/settings.ts src/composables/useAgentClient.ts src/composables/useAgentClient.test.ts
git commit -m "feat(slice-12): JS test infrastructure, NC mocks, shared types and agent client"
```

---

## Task 4: Settings store — async IConfig-backed Pinia store (TDD)

**Files:**
- Create: `extensions/nextcloud/src/stores/settings.test.ts`
- Create/Replace: `extensions/nextcloud/src/stores/settings.ts`

- [ ] **Step 1: Write `src/stores/settings.test.ts`**

```typescript
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { setActivePinia, createPinia } from 'pinia'
import axios from '@nextcloud/axios'
import { useSettingsStore } from './settings'

describe('useSettingsStore', () => {
    beforeEach(() => {
        setActivePinia(createPinia())
        vi.mocked(axios.get).mockReset()
        vi.mocked(axios.put).mockReset()
    })

    it('starts in loading=false, loaded=false, empty strings', () => {
        const store = useSettingsStore()
        expect(store.agentUrl).toBe('')
        expect(store.apiKey).toBe('')
        expect(store.loading).toBe(false)
        expect(store.loaded).toBe(false)
    })

    it('isConfigured is false when not yet loaded', () => {
        const store = useSettingsStore()
        expect(store.isConfigured).toBe(false)
    })

    it('fetchSettings: sets loading=true during fetch, then false', async () => {
        let resolveGet!: (v: unknown) => void
        vi.mocked(axios.get).mockReturnValue(new Promise(r => { resolveGet = r }) as never)

        const store = useSettingsStore()
        const fetchPromise = store.fetchSettings()

        expect(store.loading).toBe(true)

        resolveGet({ data: { agent_url: 'http://localhost:7878', api_key: 'sb_key' } })
        await fetchPromise

        expect(store.loading).toBe(false)
    })

    it('fetchSettings: populates agentUrl and apiKey from response', async () => {
        vi.mocked(axios.get).mockResolvedValue({
            data: { agent_url: 'http://localhost:7878', api_key: 'sb_agent_abc' },
        } as never)

        const store = useSettingsStore()
        await store.fetchSettings()

        expect(store.agentUrl).toBe('http://localhost:7878')
        expect(store.apiKey).toBe('sb_agent_abc')
        expect(store.loaded).toBe(true)
    })

    it('fetchSettings: isConfigured is true after fetching non-empty values', async () => {
        vi.mocked(axios.get).mockResolvedValue({
            data: { agent_url: 'http://localhost:7878', api_key: 'sb_agent_abc' },
        } as never)

        const store = useSettingsStore()
        await store.fetchSettings()

        expect(store.isConfigured).toBe(true)
    })

    it('fetchSettings: isConfigured stays false when values are empty', async () => {
        vi.mocked(axios.get).mockResolvedValue({
            data: { agent_url: '', api_key: '' },
        } as never)

        const store = useSettingsStore()
        await store.fetchSettings()

        expect(store.isConfigured).toBe(false)
    })

    it('fetchSettings: on error, stays unconfigured, loading=false, loaded=true', async () => {
        vi.mocked(axios.get).mockRejectedValue(new Error('Network error'))

        const store = useSettingsStore()
        await store.fetchSettings()

        expect(store.agentUrl).toBe('')
        expect(store.apiKey).toBe('')
        expect(store.loading).toBe(false)
        expect(store.loaded).toBe(true)
    })

    it('fetchSettings: does not re-fetch if already loaded', async () => {
        vi.mocked(axios.get).mockResolvedValue({
            data: { agent_url: 'http://localhost:7878', api_key: 'sb_key' },
        } as never)

        const store = useSettingsStore()
        await store.fetchSettings()
        await store.fetchSettings() // second call

        expect(vi.mocked(axios.get)).toHaveBeenCalledTimes(1)
    })

    it('saveSettings: PUTs current agentUrl and apiKey to the endpoint', async () => {
        vi.mocked(axios.put).mockResolvedValue({ data: { status: 'ok' } } as never)

        const store = useSettingsStore()
        store.$patch({ agentUrl: 'http://localhost:7878', apiKey: 'sb_key' })
        await store.saveSettings()

        expect(vi.mocked(axios.put)).toHaveBeenCalledWith(
            '/apps/sharebridge/api/settings',
            { agent_url: 'http://localhost:7878', api_key: 'sb_key' }
        )
    })
})
```

- [ ] **Step 2: Run tests to see them fail**

```bash
cd extensions/nextcloud
npm test -- src/stores/settings.test.ts
```

Expected: failures because `stores/settings.ts` is the stub from Task 3.

- [ ] **Step 3: Write `src/stores/settings.ts`**

```typescript
import { defineStore } from 'pinia'
import axios from '@nextcloud/axios'
import { generateUrl } from '@nextcloud/router'

export const useSettingsStore = defineStore('sharebridge-settings', {
    state: () => ({
        agentUrl: '' as string,
        apiKey:   '' as string,
        loading:  false,
        loaded:   false,
    }),
    getters: {
        isConfigured: (state) =>
            state.loaded && !state.loading && state.agentUrl !== '' && state.apiKey !== '',
    },
    actions: {
        async fetchSettings(): Promise<void> {
            if (this.loaded) return
            this.loading = true
            try {
                const response = await axios.get(generateUrl('/apps/sharebridge/api/settings'))
                this.agentUrl = response.data.agent_url ?? ''
                this.apiKey   = response.data.api_key   ?? ''
            } catch {
                // treat as unconfigured — show "Configure in Personal Settings" prompt
            } finally {
                this.loading = false
                this.loaded  = true
            }
        },
        async saveSettings(): Promise<void> {
            await axios.put(generateUrl('/apps/sharebridge/api/settings'), {
                agent_url: this.agentUrl,
                api_key:   this.apiKey,
            })
        },
    },
})
```

- [ ] **Step 4: Run tests to see them pass**

```bash
cd extensions/nextcloud
npm test
```

Expected: all tests in `settings.test.ts` and `useAgentClient.test.ts` pass.

- [ ] **Step 5: Commit**

```bash
git add src/stores/settings.ts src/stores/settings.test.ts
git commit -m "feat(slice-12): async settings store backed by IConfig PHP endpoint"
```

---

## Task 5: useNextcloudOCS — OCS share creation and ncShareId CRUD (TDD)

**Files:**
- Create: `extensions/nextcloud/src/composables/useNextcloudOCS.test.ts`
- Create: `extensions/nextcloud/src/composables/useNextcloudOCS.ts`

- [ ] **Step 1: Write `src/composables/useNextcloudOCS.test.ts`**

```typescript
import { describe, it, expect, vi, beforeEach } from 'vitest'
import axios from '@nextcloud/axios'
import { useNextcloudOCS } from './useNextcloudOCS'

describe('useNextcloudOCS', () => {
    beforeEach(() => {
        vi.mocked(axios.post).mockReset()
        vi.mocked(axios.delete).mockReset()
        vi.mocked(axios.put).mockReset()
        vi.mocked(axios.get).mockReset()
    })

    describe('createOCSShare', () => {
        it('POSTs to OCS shares endpoint with shareType=3 and path', async () => {
            vi.mocked(axios.post).mockResolvedValue({
                data: { ocs: { meta: { statuscode: 200 }, data: { id: 42, url: 'https://nc.example.com/s/abc' } } },
            } as never)

            const { createOCSShare } = useNextcloudOCS()
            await createOCSShare('/Documents/report.pdf', 24)

            expect(vi.mocked(axios.post)).toHaveBeenCalledWith(
                '/ocs/v2.php/apps/files_sharing/api/v1/shares',
                expect.stringContaining('shareType=3'),
                expect.objectContaining({ headers: { 'Content-Type': 'application/x-www-form-urlencoded' } })
            )
        })

        it('includes the file path in POST body', async () => {
            vi.mocked(axios.post).mockResolvedValue({
                data: { ocs: { meta: { statuscode: 200 }, data: { id: 42, url: 'https://nc.example.com/s/abc' } } },
            } as never)

            const { createOCSShare } = useNextcloudOCS()
            await createOCSShare('/Documents/report.pdf', 24)

            const body = vi.mocked(axios.post).mock.calls[0][1] as string
            expect(body).toContain('path=%2FDocuments%2Freport.pdf')
        })

        it('returns shareUrl and shareId from OCS response', async () => {
            vi.mocked(axios.post).mockResolvedValue({
                data: { ocs: { meta: { statuscode: 200 }, data: { id: 42, url: 'https://nc.example.com/s/abc' } } },
            } as never)

            const { createOCSShare } = useNextcloudOCS()
            const result = await createOCSShare('/Documents/report.pdf', 24)

            expect(result.shareUrl).toBe('https://nc.example.com/s/abc')
            expect(result.shareId).toBe('42')
        })

        it('sets expireDate to a YYYY-MM-DD string in the future', async () => {
            vi.mocked(axios.post).mockResolvedValue({
                data: { ocs: { meta: { statuscode: 200 }, data: { id: 1, url: 'https://nc.example.com/s/x' } } },
            } as never)

            const { createOCSShare } = useNextcloudOCS()
            await createOCSShare('/file.pdf', 1) // 1 hour expiry

            const body = vi.mocked(axios.post).mock.calls[0][1] as string
            const match = body.match(/expireDate=(\d{4}-\d{2}-\d{2})/)
            expect(match).not.toBeNull()
            // The date must be today or later
            const expireDate = new Date(match![1])
            expect(expireDate.getTime()).toBeGreaterThanOrEqual(Date.now() - 86400000)
        })

        it('throws PASSWORD_REQUIRED when OCS meta indicates password enforcement', async () => {
            vi.mocked(axios.post).mockResolvedValue({
                data: { ocs: { meta: { statuscode: 403, message: 'Password protection is enforced' }, data: null } },
            } as never)

            const { createOCSShare } = useNextcloudOCS()
            await expect(createOCSShare('/file.pdf', 24)).rejects.toThrow('PASSWORD_REQUIRED')
        })

        it('throws OCS_SHARE_FAILED when OCS response has no url', async () => {
            vi.mocked(axios.post).mockResolvedValue({
                data: { ocs: { meta: { statuscode: 500, message: 'Internal error' }, data: null } },
            } as never)

            const { createOCSShare } = useNextcloudOCS()
            await expect(createOCSShare('/file.pdf', 24)).rejects.toThrow('OCS_SHARE_FAILED')
        })
    })

    describe('deleteOCSShare', () => {
        it('DELETEs the OCS share by ID', async () => {
            vi.mocked(axios.delete).mockResolvedValue({} as never)

            const { deleteOCSShare } = useNextcloudOCS()
            await deleteOCSShare('42')

            expect(vi.mocked(axios.delete)).toHaveBeenCalledWith(
                '/ocs/v2.php/apps/files_sharing/api/v1/shares/42',
                expect.anything()
            )
        })
    })

    describe('saveNcShareId', () => {
        it('PUTs code→shareId mapping to the PHP endpoint', async () => {
            vi.mocked(axios.put).mockResolvedValue({ data: { status: 'ok' } } as never)

            const { saveNcShareId } = useNextcloudOCS()
            await saveNcShareId('ABC123', '42')

            expect(vi.mocked(axios.put)).toHaveBeenCalledWith(
                '/apps/sharebridge/api/shares/ABC123/nc-share-id',
                { nc_share_id: '42' }
            )
        })
    })

    describe('getNcShareId', () => {
        it('GETs the stored shareId for a code', async () => {
            vi.mocked(axios.get).mockResolvedValue({
                data: { nc_share_id: '42' },
            } as never)

            const { getNcShareId } = useNextcloudOCS()
            const result = await getNcShareId('ABC123')

            expect(vi.mocked(axios.get)).toHaveBeenCalledWith(
                '/apps/sharebridge/api/shares/ABC123/nc-share-id'
            )
            expect(result).toBe('42')
        })
    })

    describe('deleteNcShareId', () => {
        it('DELETEs the mapping entry for a code', async () => {
            vi.mocked(axios.delete).mockResolvedValue({} as never)

            const { deleteNcShareId } = useNextcloudOCS()
            await deleteNcShareId('ABC123')

            expect(vi.mocked(axios.delete)).toHaveBeenCalledWith(
                '/apps/sharebridge/api/shares/ABC123/nc-share-id'
            )
        })
    })
})
```

- [ ] **Step 2: Run tests to see them fail**

```bash
cd extensions/nextcloud
npm test -- src/composables/useNextcloudOCS.test.ts
```

Expected: `Cannot find module './useNextcloudOCS'`

- [ ] **Step 3: Write `src/composables/useNextcloudOCS.ts`**

```typescript
import axios from '@nextcloud/axios'
import { generateUrl } from '@nextcloud/router'

export interface OCSShareResult {
    shareUrl: string
    shareId: string
}

// Compute expireDate for OCS: use the UTC calendar date of the expiry.
// OCS accepts YYYY-MM-DD only — sub-day expiry is not supported.
// The NC public link may outlive the ShareBridge share by up to ~24h, which is
// acceptable because ShareBridge is the access gatekeeper.
const ocsExpireDate = (expiryHours: number): string => {
    const expiry = new Date(Date.now() + expiryHours * 3600 * 1000)
    return expiry.toISOString().slice(0, 10) // YYYY-MM-DD
}

export const useNextcloudOCS = () => {
    const createOCSShare = async (filePath: string, expiryHours: number): Promise<OCSShareResult> => {
        const params = new URLSearchParams({
            shareType:  '3',
            path:       filePath,
            expireDate: ocsExpireDate(expiryHours),
        })

        const response = await axios.post(
            '/ocs/v2.php/apps/files_sharing/api/v1/shares',
            params.toString(),
            {
                headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
                params:  { format: 'json' },
            }
        )

        const ocsData = response.data?.ocs?.data
        if (!ocsData?.url) {
            const message: string = response.data?.ocs?.meta?.message ?? ''
            if (message.toLowerCase().includes('password')) {
                throw new Error('PASSWORD_REQUIRED')
            }
            throw new Error('OCS_SHARE_FAILED')
        }

        return {
            shareUrl: ocsData.url,
            shareId:  String(ocsData.id),
        }
    }

    const deleteOCSShare = async (shareId: string): Promise<void> => {
        await axios.delete(
            `/ocs/v2.php/apps/files_sharing/api/v1/shares/${shareId}`,
            { params: { format: 'json' } }
        )
    }

    const saveNcShareId = async (code: string, ncShareId: string): Promise<void> => {
        await axios.put(
            generateUrl(`/apps/sharebridge/api/shares/${code}/nc-share-id`),
            { nc_share_id: ncShareId }
        )
    }

    const getNcShareId = async (code: string): Promise<string> => {
        const response = await axios.get(
            generateUrl(`/apps/sharebridge/api/shares/${code}/nc-share-id`)
        )
        return response.data.nc_share_id
    }

    const deleteNcShareId = async (code: string): Promise<void> => {
        await axios.delete(generateUrl(`/apps/sharebridge/api/shares/${code}/nc-share-id`))
    }

    return { createOCSShare, deleteOCSShare, saveNcShareId, getNcShareId, deleteNcShareId }
}
```

- [ ] **Step 4: Run all tests**

```bash
cd extensions/nextcloud
npm test
```

Expected: all tests pass.

- [ ] **Step 5: Commit**

```bash
git add src/composables/useNextcloudOCS.ts src/composables/useNextcloudOCS.test.ts
git commit -m "feat(slice-12): useNextcloudOCS composable — OCS share create/delete and ncShareId CRUD"
```

---

## Task 6: ShareCard.vue (TDD)

**Files:**
- Create: `extensions/nextcloud/src/components/ShareCard.test.ts`
- Create: `extensions/nextcloud/src/components/ShareCard.vue`

- [ ] **Step 1: Write `src/components/ShareCard.test.ts`**

```typescript
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount } from '@vue/test-utils'
import ShareCard from './ShareCard.vue'
import type { Share } from '../types'

const makeShare = (overrides: Partial<Share> = {}): Share => ({
    code:          'ABC123',
    public_url:    'https://share.example.com/s/ABC123',
    share_url:     'https://nc.example.com/s/XYZ',
    file_id:       '12345',
    downloads:     2,
    max_downloads: 5,
    relay_only:    false,
    expires_at:    new Date(Date.now() + 3600 * 1000 * 48).toISOString(), // 2 days from now
    created_at:    new Date().toISOString(),
    ...overrides,
})

describe('ShareCard', () => {
    let mockWriteText: ReturnType<typeof vi.fn>

    beforeEach(() => {
        mockWriteText = vi.fn().mockResolvedValue(undefined)
        vi.stubGlobal('navigator', { clipboard: { writeText: mockWriteText } })
    })

    it('renders share code', () => {
        const wrapper = mount(ShareCard, { props: { share: makeShare() } })
        expect(wrapper.text()).toContain('ABC123')
    })

    it('renders download count', () => {
        const wrapper = mount(ShareCard, { props: { share: makeShare({ downloads: 2, max_downloads: 5 }) } })
        expect(wrapper.text()).toContain('2')
        expect(wrapper.text()).toContain('5')
    })

    it('shows ∞ when max_downloads is 0', () => {
        const wrapper = mount(ShareCard, { props: { share: makeShare({ max_downloads: 0 }) } })
        expect(wrapper.text()).toContain('∞')
    })

    it('shows "2 days" for a share expiring in ~48 hours', () => {
        const wrapper = mount(ShareCard, { props: { share: makeShare() } })
        expect(wrapper.text()).toContain('2 days')
    })

    it('shows "Expired" for a share past its expiry', () => {
        const wrapper = mount(ShareCard, {
            props: { share: makeShare({ expires_at: new Date(Date.now() - 1000).toISOString() }) },
        })
        expect(wrapper.text()).toContain('Expired')
    })

    it('emits revoke event with code when Revoke is clicked', async () => {
        const wrapper = mount(ShareCard, { props: { share: makeShare() } })
        await wrapper.find('[data-testid="revoke-btn"]').trigger('click')
        expect(wrapper.emitted('revoke')).toBeTruthy()
        expect(wrapper.emitted('revoke')![0]).toEqual(['ABC123'])
    })

    it('copies public_url to clipboard when Copy Link is clicked', async () => {
        const wrapper = mount(ShareCard, { props: { share: makeShare() } })
        await wrapper.find('[data-testid="copy-btn"]').trigger('click')
        expect(mockWriteText).toHaveBeenCalledWith('https://share.example.com/s/ABC123')
    })
})
```

- [ ] **Step 2: Run tests to see them fail**

```bash
cd extensions/nextcloud
npm test -- src/components/ShareCard.test.ts
```

Expected: `Cannot find module './ShareCard.vue'`

- [ ] **Step 3: Write `src/components/ShareCard.vue`**

```vue
<template>
    <div class="sb-share-card">
        <div class="sb-share-info">
            <span class="sb-share-code" data-testid="share-code">{{ share.code }}</span>
            <span class="sb-share-downloads">
                {{ share.downloads }} / {{ share.max_downloads === 0 ? '∞' : share.max_downloads }} downloads
            </span>
            <span class="sb-share-expiry">Expires: {{ formattedExpiry }}</span>
        </div>
        <div class="sb-share-actions">
            <NcButton data-testid="copy-btn" @click="copyLink">
                {{ copied ? 'Copied!' : 'Copy Link' }}
            </NcButton>
            <NcButton data-testid="revoke-btn" @click="emit('revoke', share.code)">
                Revoke
            </NcButton>
        </div>
    </div>
</template>

<script setup lang="ts">
import { ref, computed, onUnmounted } from 'vue'
import { NcButton } from '@nextcloud/vue'
import type { Share } from '../types'

const props = defineProps<{ share: Share }>()
const emit  = defineEmits<{ revoke: [code: string] }>()

const copied = ref(false)
let copyTimer: ReturnType<typeof setTimeout> | null = null

const formattedExpiry = computed(() => {
    const expires = new Date(props.share.expires_at)
    const now     = new Date()
    if (expires <= now) return 'Expired'
    const hours = Math.floor((expires.getTime() - now.getTime()) / 3_600_000)
    if (hours >= 24) {
        const days = Math.floor(hours / 24)
        return days === 1 ? '1 day' : `${days} days`
    }
    if (hours >= 1) return hours === 1 ? '1 hour' : `${hours} hours`
    const minutes = Math.floor((expires.getTime() - now.getTime()) / 60_000)
    return minutes <= 1 ? '<1 min' : `${minutes} min`
})

const copyLink = async () => {
    await navigator.clipboard.writeText(props.share.public_url)
    copied.value = true
    if (copyTimer) clearTimeout(copyTimer)
    copyTimer = setTimeout(() => { copied.value = false }, 2000)
}

onUnmounted(() => { if (copyTimer) clearTimeout(copyTimer) })
</script>
```

- [ ] **Step 4: Run all tests**

```bash
cd extensions/nextcloud
npm test
```

Expected: all tests pass.

- [ ] **Step 5: Commit**

```bash
git add src/components/ShareCard.vue src/components/ShareCard.test.ts
git commit -m "feat(slice-12): ShareCard component using @nextcloud/vue NcButton"
```

---

## Task 7: PersonalSettings.vue (TDD)

**Files:**
- Create: `extensions/nextcloud/src/components/PersonalSettings.test.ts`
- Create: `extensions/nextcloud/src/components/PersonalSettings.vue`

- [ ] **Step 1: Write `src/components/PersonalSettings.test.ts`**

```typescript
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { setActivePinia, createPinia } from 'pinia'
import { useSettingsStore } from '../stores/settings'
import PersonalSettings from './PersonalSettings.vue'

describe('PersonalSettings', () => {
    beforeEach(() => {
        setActivePinia(createPinia())
        // Pre-populate store so fetchSettings no-ops
        const store = useSettingsStore()
        store.$patch({ loaded: true, agentUrl: 'http://localhost:7878', apiKey: 'sb_key' })
    })

    it('shows loading indicator while settings are being fetched', async () => {
        const store = useSettingsStore()
        store.$patch({ loaded: false, loading: true })

        const wrapper = mount(PersonalSettings)
        expect(wrapper.find('.nc-loading-icon').exists()).toBe(true)
    })

    it('renders agentUrl and apiKey fields when loaded', () => {
        const wrapper = mount(PersonalSettings)
        const inputs = wrapper.findAll('input')
        expect(inputs.length).toBeGreaterThanOrEqual(2)
    })

    it('shows current agentUrl in the agent URL field', () => {
        const wrapper = mount(PersonalSettings)
        const agentUrlInput = wrapper.find('[data-testid="agent-url-input"]')
        expect((agentUrlInput.element as HTMLInputElement).value).toBe('http://localhost:7878')
    })

    it('shows current apiKey in the API key field', () => {
        const wrapper = mount(PersonalSettings)
        const apiKeyInput = wrapper.find('[data-testid="api-key-input"]')
        expect((apiKeyInput.element as HTMLInputElement).value).toBe('sb_key')
    })

    it('Save button calls saveSettings with updated values', async () => {
        const store = useSettingsStore()
        const saveSpy = vi.spyOn(store, 'saveSettings').mockResolvedValue(undefined)

        const wrapper = mount(PersonalSettings)
        const agentInput = wrapper.find('[data-testid="agent-url-input"]')
        await agentInput.setValue('http://newhost:7878')

        await wrapper.find('[data-testid="save-btn"]').trigger('click')
        await flushPromises()

        expect(saveSpy).toHaveBeenCalledOnce()
        expect(store.agentUrl).toBe('http://newhost:7878')
    })

    it('shows a success message after saving', async () => {
        const store = useSettingsStore()
        vi.spyOn(store, 'saveSettings').mockResolvedValue(undefined)

        const wrapper = mount(PersonalSettings)
        await wrapper.find('[data-testid="save-btn"]').trigger('click')
        await flushPromises()

        expect(wrapper.text()).toContain('Saved')
    })
})
```

- [ ] **Step 2: Run tests to see them fail**

```bash
cd extensions/nextcloud
npm test -- src/components/PersonalSettings.test.ts
```

Expected: `Cannot find module './PersonalSettings.vue'`

- [ ] **Step 3: Write `src/components/PersonalSettings.vue`**

```vue
<template>
    <NcSettingsSection name="ShareBridge" description="Configure your ShareBridge agent connection.">
        <div v-if="settings.loading" class="sb-settings-loading">
            <NcLoadingIcon />
        </div>
        <template v-else>
            <NcTextField
                data-testid="agent-url-input"
                :value="settings.agentUrl"
                label="Agent URL"
                placeholder="http://localhost:7878"
                @update:model-value="settings.agentUrl = $event"
            />
            <NcTextField
                data-testid="api-key-input"
                :value="settings.apiKey"
                label="API Key"
                type="password"
                placeholder="sb_agent_..."
                @update:model-value="settings.apiKey = $event"
            />
            <div class="sb-settings-actions">
                <NcButton data-testid="save-btn" @click="save" :disabled="saving">
                    {{ saving ? 'Saving…' : 'Save' }}
                </NcButton>
                <span v-if="saved" class="sb-saved-msg">Saved</span>
            </div>
            <p class="sb-settings-hint">
                Find the Agent URL and API key in your ShareBridge agent settings page.
            </p>
        </template>
    </NcSettingsSection>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { NcSettingsSection, NcTextField, NcButton, NcLoadingIcon } from '@nextcloud/vue'
import { useSettingsStore } from '../stores/settings'

const settings = useSettingsStore()
const saving   = ref(false)
const saved    = ref(false)

const save = async () => {
    saving.value = true
    saved.value  = false
    try {
        await settings.saveSettings()
        saved.value = true
        setTimeout(() => { saved.value = false }, 3000)
    } finally {
        saving.value = false
    }
}

onMounted(() => settings.fetchSettings())
</script>
```

- [ ] **Step 4: Run all tests**

```bash
cd extensions/nextcloud
npm test
```

Expected: all tests pass.

- [ ] **Step 5: Commit**

```bash
git add src/components/PersonalSettings.vue src/components/PersonalSettings.test.ts
git commit -m "feat(slice-12): PersonalSettings component in NC Personal Settings section"
```

---

## Task 8: CreateShareModal.vue (TDD)

**Files:**
- Create: `extensions/nextcloud/src/components/CreateShareModal.test.ts`
- Create: `extensions/nextcloud/src/components/CreateShareModal.vue`

- [ ] **Step 1: Write `src/components/CreateShareModal.test.ts`**

```typescript
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { setActivePinia, createPinia } from 'pinia'
import { useSettingsStore } from '../stores/settings'
import CreateShareModal from './CreateShareModal.vue'

// Mock composables so tests don't hit real network
vi.mock('../composables/useNextcloudOCS', () => ({
    useNextcloudOCS: () => ({
        createOCSShare:  vi.fn().mockResolvedValue({ shareUrl: 'https://nc.example.com/s/XYZ', shareId: '42' }),
        saveNcShareId:   vi.fn().mockResolvedValue(undefined),
    }),
}))
vi.mock('../composables/useAgentClient', () => ({
    useAgentClient: () => ({
        createShare: vi.fn().mockResolvedValue({ code: 'ABC123', public_url: 'https://share.example.com/s/ABC123', expires_at: '2026-04-15T00:00:00Z' }),
    }),
}))

describe('CreateShareModal', () => {
    beforeEach(() => {
        setActivePinia(createPinia())
        useSettingsStore().$patch({ loaded: true, agentUrl: 'http://localhost:7878', apiKey: 'sb_key' })
    })

    const mountModal = (props = {}) => mount(CreateShareModal, {
        props: { filePath: '/Documents/report.pdf', turnAvailable: true, ...props },
    })

    it('renders expiry select with preset options', () => {
        const wrapper = mountModal()
        const select = wrapper.find('[data-testid="expiry-select"]')
        expect(select.exists()).toBe(true)
        const options = select.findAll('option')
        expect(options.length).toBeGreaterThan(0)
    })

    it('renders password input', () => {
        const wrapper = mountModal()
        expect(wrapper.find('[data-testid="password-input"]').exists()).toBe(true)
    })

    it('renders max-downloads input', () => {
        const wrapper = mountModal()
        expect(wrapper.find('[data-testid="max-downloads-input"]').exists()).toBe(true)
    })

    it('renders relay-only checkbox', () => {
        const wrapper = mountModal()
        expect(wrapper.find('[data-testid="relay-only-input"]').exists()).toBe(true)
    })

    it('shows TURN warning when relay is checked and TURN is not available', async () => {
        const wrapper = mountModal({ turnAvailable: false })
        const relayCheckbox = wrapper.find('[data-testid="relay-only-input"]')
        await relayCheckbox.trigger('change')
        await wrapper.vm.$nextTick()
        expect(wrapper.find('[data-testid="turn-warning"]').exists()).toBe(true)
    })

    it('does not show TURN warning when relay is checked but TURN is available', async () => {
        const wrapper = mountModal({ turnAvailable: true })
        const relayCheckbox = wrapper.find('[data-testid="relay-only-input"]')
        await relayCheckbox.trigger('change')
        await wrapper.vm.$nextTick()
        expect(wrapper.find('[data-testid="turn-warning"]').exists()).toBe(false)
    })

    it('emits close when Cancel is clicked', async () => {
        const wrapper = mountModal()
        await wrapper.find('[data-testid="cancel-btn"]').trigger('click')
        expect(wrapper.emitted('close')).toBeTruthy()
    })

    it('on submit: calls createOCSShare, createShare, saveNcShareId, then emits created', async () => {
        const { useNextcloudOCS } = await import('../composables/useNextcloudOCS')
        const { useAgentClient }  = await import('../composables/useAgentClient')
        const { createOCSShare, saveNcShareId } = useNextcloudOCS()
        const { createShare }                   = useAgentClient()

        const wrapper = mountModal()
        await wrapper.find('[data-testid="create-btn"]').trigger('click')
        await flushPromises()

        expect(createOCSShare).toHaveBeenCalledWith('/Documents/report.pdf', expect.any(Number))
        expect(createShare).toHaveBeenCalledWith(expect.objectContaining({
            share_url: 'https://nc.example.com/s/XYZ',
        }))
        expect(saveNcShareId).toHaveBeenCalledWith('ABC123', '42')
        expect(wrapper.emitted('created')).toBeTruthy()
    })

    it('shows error when OCS share creation fails with PASSWORD_REQUIRED', async () => {
        const { useNextcloudOCS } = await import('../composables/useNextcloudOCS')
        vi.mocked(useNextcloudOCS().createOCSShare).mockRejectedValueOnce(new Error('PASSWORD_REQUIRED'))

        const wrapper = mountModal()
        await wrapper.find('[data-testid="create-btn"]').trigger('click')
        await flushPromises()

        expect(wrapper.find('[data-testid="error-msg"]').text()).toContain('password')
    })
})
```

- [ ] **Step 2: Run tests to see them fail**

```bash
cd extensions/nextcloud
npm test -- src/components/CreateShareModal.test.ts
```

Expected: `Cannot find module './CreateShareModal.vue'`

- [ ] **Step 3: Write `src/components/CreateShareModal.vue`**

```vue
<template>
    <NcModal name="Create ShareBridge Share" @close="emit('close')">
        <div class="sb-modal-body">
            <h2>Create ShareBridge Share</h2>

            <label>
                Expiry
                <select data-testid="expiry-select" v-model.number="form.expiryHours">
                    <option v-for="opt in expiryOptions" :key="opt.value" :value="opt.value">
                        {{ opt.label }}
                    </option>
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
                <input
                    data-testid="relay-only-input"
                    type="checkbox"
                    :checked="form.relayOnly"
                    @change="form.relayOnly = !form.relayOnly"
                />
                Relay mode only
            </label>

            <div v-if="form.relayOnly && !turnAvailable" data-testid="turn-warning" class="sb-warning">
                Warning: Relay mode requires a TURN server. Shares may not connect without one.
            </div>

            <div v-if="error" data-testid="error-msg" class="sb-error">{{ error }}</div>

            <div class="sb-modal-actions">
                <NcButton data-testid="cancel-btn" @click="emit('close')" :disabled="loading">
                    Cancel
                </NcButton>
                <NcButton data-testid="create-btn" @click="submit" :disabled="loading">
                    {{ loading ? 'Creating…' : 'Create Share' }}
                </NcButton>
            </div>
        </div>
    </NcModal>
</template>

<script setup lang="ts">
import { ref, reactive, computed } from 'vue'
import { NcModal, NcButton } from '@nextcloud/vue'
import { useNextcloudOCS } from '../composables/useNextcloudOCS'
import { useAgentClient } from '../composables/useAgentClient'
import type { CreateShareResult } from '../types'

const PRESET_EXPIRY_HOURS = [1, 6, 12, 24, 72, 168, 720]

const props = defineProps<{
    filePath: string           // node.path from @nextcloud/files Node
    turnAvailable?: boolean
    defaultExpiryHours?: number
    defaultMaxDownloads?: number
    defaultRelayOnly?: boolean
}>()
const emit = defineEmits<{
    close:   []
    created: [result: CreateShareResult]
}>()

const { createOCSShare, saveNcShareId } = useNextcloudOCS()
const { createShare }                   = useAgentClient()

const loading = ref(false)
const error   = ref('')
const form    = reactive({
    expiryHours: props.defaultExpiryHours  ?? 24,
    password:    '',
    maxDownloads: props.defaultMaxDownloads ?? 0,
    relayOnly:   props.defaultRelayOnly    ?? false,
})

const expiryOptions = computed(() => {
    const opts = PRESET_EXPIRY_HOURS.map(h => ({ value: h, label: formatExpiry(h) }))
    if (props.defaultExpiryHours && !PRESET_EXPIRY_HOURS.includes(props.defaultExpiryHours)) {
        opts.push({ value: props.defaultExpiryHours, label: `${props.defaultExpiryHours} hours` })
    }
    return opts.sort((a, b) => a.value - b.value)
})

const formatExpiry = (hours: number): string => {
    if (hours < 24) return hours === 1 ? '1 hour' : `${hours} hours`
    const days = hours / 24
    return days === 1 ? '1 day' : `${days} days`
}

const submit = async () => {
    loading.value = true
    error.value   = ''

    try {
        // Step 1: Create Nextcloud public share via OCS (no password needed)
        const { shareUrl, shareId } = await createOCSShare(props.filePath, form.expiryHours)

        // Step 2: Register the share with the agent
        const result = await createShare({
            share_url:     shareUrl,
            password:      form.password || undefined,
            expiry_hours:  form.expiryHours,
            max_downloads: form.maxDownloads,
            relay_only:    form.relayOnly,
        })

        // Step 3: Save the code→ncShareId mapping for future revocation
        await saveNcShareId(result.code, shareId)

        emit('created', result)
    } catch (err: unknown) {
        const message = err instanceof Error ? err.message : ''
        if (message === 'PASSWORD_REQUIRED') {
            error.value = 'Your Nextcloud requires passwords on public links. Please contact your admin.'
        } else if (message === 'OCS_SHARE_FAILED') {
            error.value = 'Failed to create Nextcloud share. Please try again.'
        } else {
            error.value = 'Failed to create ShareBridge share. Please try again.'
        }
    } finally {
        loading.value = false
    }
}
</script>
```

- [ ] **Step 4: Run all tests**

```bash
cd extensions/nextcloud
npm test
```

Expected: all tests pass.

- [ ] **Step 5: Commit**

```bash
git add src/components/CreateShareModal.vue src/components/CreateShareModal.test.ts
git commit -m "feat(slice-12): CreateShareModal — OCS share creation + agent registration"
```

---

## ⚠️ STOP — Live Nextcloud Instance Required

**Do not proceed to Task 9 without a running Nextcloud 27+ instance.**

Tasks 1–8 are fully unit-testable against mocks. Task 9 onwards requires verifying two things against a live NC instance before writing code that depends on them.

### Step A: Spin up Nextcloud

```bash
docker run -d \
  -p 8080:80 \
  --name nextcloud-dev \
  nextcloud:27
```

Go to `http://localhost:8080` and complete the setup wizard (SQLite is fine for dev).

### Step B: Verify the `registerTab` prop name

Build the extension and install it:

```bash
cd extensions/nextcloud
npm run build
# Copy the built app into the NC container
docker cp . nextcloud-dev:/var/www/html/apps/sharebridge
docker exec nextcloud-dev chown -R www-data:www-data /var/www/html/apps/sharebridge
```

Enable the app in NC admin → Apps, then temporarily replace `ShareBridgeTab.vue` with a
stub that logs its props:

```vue
<template><div>check console</div></template>
<script setup lang="ts">
const props = defineProps<Record<string, unknown>>()
console.log('[ShareBridge] tab props:', JSON.stringify(Object.keys(props)))
</script>
```

Open the Files app, select a file, open the sidebar ShareBridge tab, and check the
browser console. **Record the prop name here before continuing.**

Expected in NC 27+ (`@nextcloud/files` v3): `files` (array of `Node`)
May differ in older NC 27 builds: `node` (single `Node`) or `activeFiles`

**Update the `defineProps` in Task 9's `ShareBridgeTab.vue` to match what you observe.**

### Step C: Verify OCS `expireDate` behaviour

In the NC browser console (or via curl with a session cookie), create a test share with today's date as `expireDate` and confirm whether the link is immediately expired or valid until end of day:

```bash
curl -u admin:password \
  -X POST "http://localhost:8080/ocs/v2.php/apps/files_sharing/api/v1/shares?format=json" \
  -H "OCS-APIRequest: true" \
  -d "shareType=3&path=/welcome.txt&expireDate=$(date +%Y-%m-%d)"
```

Check the returned share URL in a browser. **If it's already expired, update `ocsExpireDate()` in `useNextcloudOCS.ts` (Task 5) to add 1 day to the expiry date.**

### Step D: Confirm `registerPersonalSettings` section appears

With the app enabled, go to user avatar → Settings → Personal and confirm a ShareBridge section appears. If it does not, the script may need to be loaded only on the personal settings page — investigate `Util::addScript` with a page check in `Application.php`.

---

## Task 9: ShareBridgeTab.vue (TDD)

**Files:**
- Create: `extensions/nextcloud/src/components/ShareBridgeTab.test.ts`
- Create: `extensions/nextcloud/src/components/ShareBridgeTab.vue`

- [ ] **Step 1: Write `src/components/ShareBridgeTab.test.ts`**

> **Note on the `files` prop:** Nextcloud 27+ injects a `files: Node[]` prop into registered tab components (verified against `@nextcloud/files` v3 source). If running on NC 27 (older v2 API), the prop name may be `node: Node`. Verify against your live instance and adjust the prop name in `ShareBridgeTab.vue` if needed.

```typescript
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { setActivePinia, createPinia } from 'pinia'
import { useSettingsStore } from '../stores/settings'
import ShareBridgeTab from './ShareBridgeTab.vue'
import type { Share } from '../types'

// Mock composables
vi.mock('../composables/useAgentClient', () => ({
    useAgentClient: () => ({
        listShares:  vi.fn().mockResolvedValue([]),
        revokeShare: vi.fn().mockResolvedValue(undefined),
        getSettings: vi.fn().mockResolvedValue({ default_expiry_hours: 24, default_max_downloads: 0, default_relay_only: false, turn_available: false }),
    }),
}))
vi.mock('../composables/useNextcloudOCS', () => ({
    useNextcloudOCS: () => ({
        getNcShareId:    vi.fn().mockResolvedValue('42'),
        deleteOCSShare:  vi.fn().mockResolvedValue(undefined),
        deleteNcShareId: vi.fn().mockResolvedValue(undefined),
    }),
}))

const makeNode = (fileid = 12345, path = '/Documents/report.pdf') => ({ fileid, path })

const makeShare = (code = 'ABC123'): Share => ({
    code,
    public_url:    `https://share.example.com/s/${code}`,
    share_url:     'https://nc.example.com/s/XYZ',
    file_id:       '12345',
    downloads:     0,
    max_downloads: 0,
    relay_only:    false,
    expires_at:    new Date(Date.now() + 86_400_000).toISOString(),
    created_at:    new Date().toISOString(),
})

describe('ShareBridgeTab', () => {
    beforeEach(() => {
        setActivePinia(createPinia())
    })

    it('shows "Configure in Personal Settings" prompt when not configured', async () => {
        useSettingsStore().$patch({ loaded: true, agentUrl: '', apiKey: '' })

        const wrapper = mount(ShareBridgeTab, { props: { files: [makeNode()] } })
        await flushPromises()

        expect(wrapper.text()).toContain('Personal Settings')
        expect(wrapper.find('[data-testid="configure-prompt"]').exists()).toBe(true)
    })

    it('shows loading icon while settings are being fetched', () => {
        useSettingsStore().$patch({ loaded: false, loading: true })

        const wrapper = mount(ShareBridgeTab, { props: { files: [makeNode()] } })
        expect(wrapper.find('.nc-loading-icon').exists()).toBe(true)
    })

    it('calls listShares with node fileid when configured', async () => {
        useSettingsStore().$patch({ loaded: true, agentUrl: 'http://localhost:7878', apiKey: 'sb_key' })

        const { useAgentClient } = await import('../composables/useAgentClient')
        const { listShares }     = useAgentClient()

        mount(ShareBridgeTab, { props: { files: [makeNode(99999)] } })
        await flushPromises()

        expect(vi.mocked(listShares)).toHaveBeenCalledWith('99999')
    })

    it('shows empty state when no shares exist for this file', async () => {
        useSettingsStore().$patch({ loaded: true, agentUrl: 'http://localhost:7878', apiKey: 'sb_key' })

        const wrapper = mount(ShareBridgeTab, { props: { files: [makeNode()] } })
        await flushPromises()

        expect(wrapper.find('[data-testid="empty-state"]').exists()).toBe(true)
    })

    it('renders a ShareCard for each share', async () => {
        useSettingsStore().$patch({ loaded: true, agentUrl: 'http://localhost:7878', apiKey: 'sb_key' })

        const { useAgentClient } = await import('../composables/useAgentClient')
        vi.mocked(useAgentClient().listShares).mockResolvedValue([makeShare('ABC'), makeShare('XYZ')])

        const wrapper = mount(ShareBridgeTab, { props: { files: [makeNode()] } })
        await flushPromises()

        expect(wrapper.findAll('[data-testid="share-card"]').length).toBe(2)
    })

    it('shows Create button when configured', async () => {
        useSettingsStore().$patch({ loaded: true, agentUrl: 'http://localhost:7878', apiKey: 'sb_key' })

        const wrapper = mount(ShareBridgeTab, { props: { files: [makeNode()] } })
        await flushPromises()

        expect(wrapper.find('[data-testid="create-share-btn"]').exists()).toBe(true)
    })

    it('shows connection error when listShares throws', async () => {
        useSettingsStore().$patch({ loaded: true, agentUrl: 'http://localhost:7878', apiKey: 'sb_key' })

        const { useAgentClient } = await import('../composables/useAgentClient')
        vi.mocked(useAgentClient().listShares).mockRejectedValueOnce(new Error('Network error'))

        const wrapper = mount(ShareBridgeTab, { props: { files: [makeNode()] } })
        await flushPromises()

        expect(wrapper.find('[data-testid="error-msg"]').exists()).toBe(true)
        expect(wrapper.find('[data-testid="error-msg"]').text()).toContain('Cannot connect')
    })

    it('shows error when revoke fails', async () => {
        useSettingsStore().$patch({ loaded: true, agentUrl: 'http://localhost:7878', apiKey: 'sb_key' })

        const { useAgentClient }  = await import('../composables/useAgentClient')
        const { useNextcloudOCS } = await import('../composables/useNextcloudOCS')
        vi.mocked(useAgentClient().listShares).mockResolvedValue([makeShare('ABC123')])
        vi.mocked(useNextcloudOCS().deleteOCSShare).mockRejectedValueOnce(new Error('OCS error'))

        const wrapper = mount(ShareBridgeTab, { props: { files: [makeNode()] } })
        await flushPromises()

        wrapper.findComponent({ name: 'ShareCard' }).vm.$emit('revoke', 'ABC123')
        await flushPromises()

        expect(wrapper.find('[data-testid="error-msg"]').exists()).toBe(true)
        expect(wrapper.find('[data-testid="error-msg"]').text()).toContain('Failed to fully revoke')
    })

    it('on revoke: calls getNcShareId, revokeShare, deleteOCSShare, then reloads shares', async () => {
        useSettingsStore().$patch({ loaded: true, agentUrl: 'http://localhost:7878', apiKey: 'sb_key' })

        const { useAgentClient }  = await import('../composables/useAgentClient')
        const { useNextcloudOCS } = await import('../composables/useNextcloudOCS')
        vi.mocked(useAgentClient().listShares).mockResolvedValue([makeShare('ABC123')])

        const { getNcShareId, deleteOCSShare, deleteNcShareId } = useNextcloudOCS()
        const { revokeShare, listShares }                       = useAgentClient()

        const wrapper = mount(ShareBridgeTab, { props: { files: [makeNode()] } })
        await flushPromises()

        // Trigger revoke from the first ShareCard
        wrapper.findComponent({ name: 'ShareCard' }).vm.$emit('revoke', 'ABC123')
        await flushPromises()

        expect(getNcShareId).toHaveBeenCalledWith('ABC123')
        expect(revokeShare).toHaveBeenCalledWith('ABC123')
        expect(deleteOCSShare).toHaveBeenCalledWith('42')
        expect(deleteNcShareId).toHaveBeenCalledWith('ABC123')
        // listShares called again after revoke
        expect(vi.mocked(listShares)).toHaveBeenCalledTimes(2)
    })
})
```

- [ ] **Step 2: Run tests to see them fail**

```bash
cd extensions/nextcloud
npm test -- src/components/ShareBridgeTab.test.ts
```

Expected: `Cannot find module './ShareBridgeTab.vue'`

- [ ] **Step 3: Write `src/components/ShareBridgeTab.vue`**

```vue
<template>
    <div class="sb-tab">
        <!-- Fetching settings -->
        <div v-if="settings.loading" class="sb-loading">
            <NcLoadingIcon />
        </div>

        <!-- Not configured -->
        <div v-else-if="!settings.isConfigured" data-testid="configure-prompt" class="sb-configure-prompt">
            <p>
                Configure ShareBridge in your
                <strong>Personal Settings</strong>
                to get started.
            </p>
        </div>

        <!-- Configured -->
        <template v-else>
            <div v-if="error" data-testid="error-msg" class="sb-error">{{ error }}</div>

            <div v-else-if="loadingShares" class="sb-loading">
                <NcLoadingIcon />
            </div>

            <template v-else>
                <div v-if="shares.length === 0" data-testid="empty-state" class="sb-empty">
                    <NcEmptyContent name="No shares" description="No ShareBridge shares for this file." />
                </div>

                <div v-else data-testid="share-list">
                    <ShareCard
                        v-for="share in shares"
                        :key="share.code"
                        :share="share"
                        data-testid="share-card"
                        @revoke="handleRevoke"
                    />
                </div>

                <NcButton data-testid="create-share-btn" @click="showModal = true">
                    Create ShareBridge Share
                </NcButton>
            </template>

            <CreateShareModal
                v-if="showModal"
                :file-path="node.path"
                :turn-available="turnAvailable"
                :default-expiry-hours="defaultExpiryHours"
                :default-max-downloads="defaultMaxDownloads"
                :default-relay-only="defaultRelayOnly"
                @close="showModal = false"
                @created="handleCreated"
            />
        </template>
    </div>
</template>

<script setup lang="ts">
import { ref, computed, watch, onMounted } from 'vue'
import { NcButton, NcLoadingIcon, NcEmptyContent } from '@nextcloud/vue'
import { useSettingsStore } from '../stores/settings'
import { useAgentClient } from '../composables/useAgentClient'
import { useNextcloudOCS } from '../composables/useNextcloudOCS'
import ShareCard from './ShareCard.vue'
import CreateShareModal from './CreateShareModal.vue'
import type { Share, CreateShareResult } from '../types'

// Props injected by registerTab.
// @nextcloud/files v3: prop name is `files` (Node[]).
// Verify against your live NC instance — older versions may use `node: Node`.
const props = defineProps<{
    files: Array<{ fileid: number; path: string }>
}>()

const node = computed(() => props.files[0])

const settings     = useSettingsStore()
const { listShares, revokeShare, getSettings } = useAgentClient()
const { getNcShareId, deleteOCSShare, deleteNcShareId } = useNextcloudOCS()

const shares         = ref<Share[]>([])
const loadingShares  = ref(false)
const error          = ref('')
const showModal      = ref(false)
const turnAvailable  = ref(false)
const defaultExpiryHours   = ref(24)
const defaultMaxDownloads  = ref(0)
const defaultRelayOnly     = ref(false)

const loadShares = async () => {
    loadingShares.value = true
    error.value         = ''
    try {
        shares.value = await listShares(String(node.value.fileid))
    } catch {
        error.value = "Cannot connect to ShareBridge agent. Check the agent URL and ensure it's running."
    } finally {
        loadingShares.value = false
    }
}

const applyAgentSettings = async () => {
    try {
        const s = await getSettings()
        turnAvailable.value        = s.turn_available
        defaultExpiryHours.value   = s.default_expiry_hours
        defaultMaxDownloads.value  = s.default_max_downloads
        defaultRelayOnly.value     = s.default_relay_only
    } catch {
        // leave defaults
    }
}

const handleRevoke = async (code: string) => {
    error.value = ''
    try {
        const ncShareId = await getNcShareId(code)
        await Promise.all([revokeShare(code), deleteOCSShare(ncShareId), deleteNcShareId(code)])
    } catch {
        error.value = 'Failed to fully revoke share. The Nextcloud link may still be accessible.'
    }
    await loadShares()
}

const handleCreated = async (_result: CreateShareResult) => {
    showModal.value = false
    await loadShares()
}

// Load shares whenever the file or configured state changes.
// Use != null (not truthiness) so fileid=0 is handled correctly.
watch(
    [() => node.value?.fileid, () => settings.isConfigured],
    ([fileid, isConfigured]) => {
        if (fileid != null && isConfigured) {
            loadShares()
            applyAgentSettings()
        }
    },
    { immediate: true }
)

onMounted(() => settings.fetchSettings())
</script>
```

- [ ] **Step 4: Run all tests**

```bash
cd extensions/nextcloud
npm test
```

Expected: all tests pass.

- [ ] **Step 5: Commit**

```bash
git add src/components/ShareBridgeTab.vue src/components/ShareBridgeTab.test.ts
git commit -m "feat(slice-12): ShareBridgeTab — Files sidebar tab with share list and revoke"
```

---

## Task 10: main.ts + build verification

**Files:**
- Create: `extensions/nextcloud/src/main.ts`

- [ ] **Step 1: Write `src/main.ts`**

```typescript
import { registerTab } from '@nextcloud/files'
import { registerPersonalSettings } from '@nextcloud/settings'
import { t } from '@nextcloud/l10n'
import ShareBridgeTab from './components/ShareBridgeTab.vue'
import PersonalSettings from './components/PersonalSettings.vue'

// Register sidebar tab in Files app.
// Tab is hidden for multi-file selections.
registerTab({
    id:        'sharebridge',
    name:      t('sharebridge', 'ShareBridge'),
    component: ShareBridgeTab,
    // `nodes` is Node[] from @nextcloud/files
    enabled:   (nodes: unknown[]) => nodes.length === 1,
})

// Register settings section under user avatar → Settings → Personal → ShareBridge
registerPersonalSettings({
    id:        'sharebridge',
    section:   'sharebridge',
    name:      t('sharebridge', 'ShareBridge'),
    component: PersonalSettings,
})
```

- [ ] **Step 2: Run typecheck**

```bash
cd extensions/nextcloud
npm run typecheck
```

Expected: no TypeScript errors.

- [ ] **Step 3: Run production build**

```bash
cd extensions/nextcloud
npm run build
```

Expected: `js/sharebridge-main.js` is created.

- [ ] **Step 4: Run all tests one final time**

```bash
cd extensions/nextcloud
npm test
```

Expected: all tests pass.

- [ ] **Step 5: Commit**

```bash
git add src/main.ts js/sharebridge-main.js
git commit -m "feat(slice-12): entry point main.ts — registerTab + registerPersonalSettings"
```

---

## Spec Coverage Check

| Spec requirement | Covered by |
|---|---|
| `registerTab` + `registerPersonalSettings` in `main.ts` | Task 10 |
| Tab hidden for multi-file selection (`enabled`) | Task 10 `main.ts` |
| PHP GET/PUT settings via `IConfig` | Task 2 |
| PHP PUT/GET `nc_share_ids` mapping | Task 2 |
| `stores/settings.ts` async fetch from PHP endpoint | Task 4 |
| `stores/settings.ts` `isConfigured` computed | Task 4 |
| `stores/settings.ts` `saveSettings()` PUT | Task 4 |
| OCS `POST /ocs/v2.php/.../shares` with `shareType=3` | Task 5 |
| OCS expiry as `YYYY-MM-DD` | Task 5 |
| Password enforcement → `PASSWORD_REQUIRED` error | Task 5 |
| `saveNcShareId` / `getNcShareId` / `deleteNcShareId` via PHP endpoint | Task 5 |
| `deleteOCSShare` via OCS DELETE | Task 5 |
| `ShareCard` with copy + revoke | Task 6 |
| `PersonalSettings` form in NC Personal Settings | Task 7 |
| `CreateShareModal` — OCS → agent → saveNcShareId flow | Task 8 |
| `ShareBridgeTab` — settings loading, configure prompt | Task 9 |
| `ShareBridgeTab` — `listShares(node.fileid)` | Task 9 |
| Revoke — `getNcShareId` + agent DELETE + OCS DELETE + mapping DELETE in parallel | Task 9 |
| Error messages from spec error table | Tasks 8, 9 |
| Build to `js/sharebridge-main.js` | Task 10 |
| Agent PROPFIND path not bypassed (no `file_id` in CreateShareParams) | Task 8 (confirmed: `file_id` not passed) |

**Unverifiable without live NC instance (must test manually):**
- Tab appears in Files sidebar
- Personal Settings section appears under user avatar → Settings → Personal
- Settings persist across browser refresh and across devices
- OCS share creation succeeds end-to-end
- Revoke removes NC public link

---

> **Prop name verification reminder:** The `files` prop name in `ShareBridgeTab.vue` must be verified against your live NC 27+ instance. Check with: `registerTab({ ..., component: { setup(props) { console.log(props) } } })` and open the Files sidebar.
