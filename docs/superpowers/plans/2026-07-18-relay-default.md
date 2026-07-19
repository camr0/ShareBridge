# Relay Default Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Relay the first, recommended, and default connection mode across the agent admin UI, OpenCloud, and Nextcloud while replacing obsolete TURN warnings with accurate Direct-mode privacy and performance guidance.

**Architecture:** Keep the existing `relay_only` boolean and transport behavior unchanged. Set configuration and component fallbacks to `true`, render both modes explicitly in recommended-first order, and preserve explicit saved Direct preferences. Test rendered behavior at the Go handler and Vue component boundaries.

**Tech Stack:** Go `html/template` and `net/http/httptest`; Vue 3; TypeScript; Vitest; Vue Test Utils.

---

## File Map

- Modify `agent/internal/config/config.go` and `config_test.go` for the new-install default.
- Modify `agent/internal/web/api.go`, `api_test.go`, `templates/share-form.html`, and `templates/settings.html` for the agent UI.
- Modify each extension's `CreateShareModal.vue` and `CreateShareModal.test.ts` for explicit recommended-first mode choices.
- Do not change transport logic, API field names, quota behavior, or the existing `turn_available` compatibility field.

### Task 1: Default New Agent Configurations to Relay

**Files:**
- Modify: `agent/internal/config/config_test.go`
- Modify: `agent/internal/config/config.go`

- [ ] **Step 1: Write the failing configuration test**

Add to `TestNewManager_CreatesConfigDir`:

```go
if !cfg.DefaultRelayOnly {
	t.Error("DefaultRelayOnly = false, want true")
}
```

- [ ] **Step 2: Run the focused test and verify RED**

Run:

```bash
cd agent && go test ./internal/config -run '^TestNewManager_CreatesConfigDir$' -count=1
```

Expected: FAIL with `DefaultRelayOnly = false, want true`.

- [ ] **Step 3: Set the minimal configuration default**

Change the default literal in `Manager.load` to:

```go
cfg := &Config{
	DefaultExpiry:       24,
	DefaultMaxDownloads: 10,
	DefaultRelayOnly:    true,
	// UIPort: leave as 0, use env fallback with default
}
```

- [ ] **Step 4: Verify GREEN and commit**

Run:

```bash
cd agent && go test ./internal/config -count=1
git add agent/internal/config/config.go agent/internal/config/config_test.go
git commit -m "feat: default new shares to relay"
```

Expected: tests PASS, then the commit succeeds.

### Task 2: Update the Agent Admin UI

**Files:**
- Modify: `agent/internal/web/api_test.go`
- Modify: `agent/internal/web/api.go`
- Modify: `agent/internal/web/templates/share-form.html`
- Modify: `agent/internal/web/templates/settings.html`

- [ ] **Step 1: Write failing share-form tests**

Add to `agent/internal/web/api_test.go`:

```go
func TestShareForm_RendersRelayFirstRecommendedAndSelected(t *testing.T) {
	cfg := &config.Config{DefaultRelayOnly: true}
	ws, _ := newV1TestServer(cfg)
	req := httptest.NewRequest(http.MethodGet, "/api/share-form", nil)
	rec := httptest.NewRecorder()

	ws.shareFormHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	relayAt := strings.Index(body, "Relay (recommended)")
	directAt := strings.Index(body, ">Direct<")
	if relayAt < 0 || directAt < 0 || relayAt >= directAt {
		t.Fatalf("Relay should appear before Direct: relay=%d direct=%d", relayAt, directAt)
	}
	for _, want := range []string{
		`id="mode-relay" name="relay_only" value="true" checked`,
		"End-to-end encrypted, hides your IP, and provides consistent performance",
		"Peer-to-peer, quota-free",
		"Direct transfers expose your IP address and may be slower due to browser protocol limitations. Use Relay for more consistent performance.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	for _, obsolete := range []string{"TURN", "Fast, free", "Requires TURN server"} {
		if strings.Contains(body, obsolete) {
			t.Errorf("body contains obsolete copy %q", obsolete)
		}
	}
}

func TestShareForm_HonorsSavedDirectDefault(t *testing.T) {
	cfg := &config.Config{DefaultRelayOnly: false}
	ws, _ := newV1TestServer(cfg)
	req := httptest.NewRequest(http.MethodGet, "/api/share-form", nil)
	rec := httptest.NewRecorder()

	ws.shareFormHandler(rec, req)

	if !strings.Contains(rec.Body.String(), `id="mode-direct" name="relay_only" value="false" checked`) {
		t.Fatal("Direct radio is not checked for saved Direct default")
	}
}
```

- [ ] **Step 2: Run the focused tests and verify RED**

```bash
cd agent && go test ./internal/web -run '^TestShareForm_' -count=1
```

Expected: FAIL because Relay is second, Direct is checked, old copy remains, and the handler ignores the saved default.

- [ ] **Step 3: Pass saved mode data into the template**

Add near `shareFormHandler`:

```go
type shareFormData struct {
	DefaultRelayOnly bool
}
```

Before rendering, derive the fallback and saved preference:

```go
data := shareFormData{DefaultRelayOnly: true}
if ws.daemon != nil && ws.daemon.GetConfig() != nil {
	data.DefaultRelayOnly = ws.daemon.GetConfig().DefaultRelayOnly
}
```

Pass `data` instead of `nil` to `ExecuteTemplate`.

- [ ] **Step 4: Render Relay first in the agent form**

Replace both mode cards in `share-form.html` with this structure:

```html
<div class="mode-option{{if .DefaultRelayOnly}} selected{{end}}" id="relay-option">
    <div class="mode-option-header">
        <input type="radio" id="mode-relay" name="relay_only" value="true"{{if .DefaultRelayOnly}} checked{{end}}>
        <div>
            <div class="mode-option-title">Relay (recommended)</div>
            <div class="mode-option-desc">End-to-end encrypted, hides your IP, and provides consistent performance</div>
        </div>
    </div>
    <div id="quota-container" hx-get="/api/quota-inline" hx-trigger="load"
         hx-target="this" hx-swap="innerHTML"></div>
</div>

<div class="mode-option{{if not .DefaultRelayOnly}} selected{{end}}" id="direct-option">
    <div class="mode-option-header">
        <input type="radio" id="mode-direct" name="relay_only" value="false"{{if not .DefaultRelayOnly}} checked{{end}}>
        <div>
            <div class="mode-option-title">Direct</div>
            <div class="mode-option-desc">Peer-to-peer, quota-free</div>
        </div>
    </div>
    <div class="mode-option-warning amber">
        Direct transfers expose your IP address and may be slower due to browser protocol limitations. Use Relay for more consistent performance.
    </div>
</div>
```

Keep the existing IDs and JavaScript selection behavior unchanged.

- [ ] **Step 5: Reorder and relabel the saved-setting selector**

Use these options in `settings.html`:

```html
<option value="true"  {{if .Config.DefaultRelayOnly}}selected{{end}}>Relay (recommended)</option>
<option value="false" {{if not .Config.DefaultRelayOnly}}selected{{end}}>Direct</option>
```

- [ ] **Step 6: Verify GREEN and commit**

```bash
cd agent && go test ./internal/web -count=1
git add agent/internal/web/api.go agent/internal/web/api_test.go agent/internal/web/templates/share-form.html agent/internal/web/templates/settings.html
git commit -m "feat: recommend relay in agent UI"
```

Expected: tests PASS and the commit succeeds.

### Task 3: Update the OpenCloud Share Form

**Files:**
- Modify: `extensions/opencloud/src/components/CreateShareModal.test.ts`
- Modify: `extensions/opencloud/src/components/CreateShareModal.vue`

- [ ] **Step 1: Replace TURN-warning tests with failing mode tests**

Remove the TURN-warning tests and replace the unchecked fallback test with:

```ts
it('lists Relay first and selects it by default', () => {
  const wrapper = mountModal()
  const choices = Array.from(document.querySelectorAll('[data-testid$="-option"]'))
  expect(choices.map(choice => choice.getAttribute('data-testid'))).toEqual([
    'relay-option',
    'direct-option',
  ])
  expect((findEl('mode-relay') as HTMLInputElement).checked).toBe(true)
  expect(document.body.textContent).toContain('Relay (recommended)')
  expect(document.body.textContent).toContain(
    'End-to-end encrypted, hides your IP, and provides consistent performance',
  )
  expect(document.body.textContent).not.toContain('TURN')
})

it('shows the Direct advisory when Direct is selected', async () => {
  const wrapper = mountModal()
  ;(findEl('mode-direct') as HTMLInputElement).click()
  await wrapper.vm.$nextTick()
  expect(findEl('direct-advisory')?.textContent).toContain(
    'Direct transfers expose your IP address and may be slower due to browser protocol limitations. Use Relay for more consistent performance.',
  )
})

it('honors a Direct default from props', () => {
  const wrapper = mountModal({ defaultRelayOnly: false })
  expect((findEl('mode-direct') as HTMLInputElement).checked).toBe(true)
})
```

Extend the existing submit assertion with `relay_only: true`.

- [ ] **Step 2: Run the component test and verify RED**

```bash
cd extensions/opencloud && npm test -- src/components/CreateShareModal.test.ts
```

Expected: FAIL because the component is checkbox-based, defaults to Direct, and contains TURN copy.

- [ ] **Step 3: Implement explicit Relay and Direct choices**

Replace the checkbox and TURN warning with:

```html
<fieldset class="sb-mode-options">
  <legend>Connection Mode</legend>
  <label data-testid="relay-option" class="sb-mode-option">
    <input data-testid="mode-relay" v-model="form.relayOnly" type="radio" :value="true" />
    <span><strong>Relay (recommended)</strong>
      <small>End-to-end encrypted, hides your IP, and provides consistent performance</small>
    </span>
  </label>
  <label data-testid="direct-option" class="sb-mode-option">
    <input data-testid="mode-direct" v-model="form.relayOnly" type="radio" :value="false" />
    <span><strong>Direct</strong><small>Peer-to-peer, quota-free</small></span>
  </label>
  <div v-if="!form.relayOnly" data-testid="direct-advisory" class="sb-warning">
    Direct transfers expose your IP address and may be slower due to browser protocol limitations. Use Relay for more consistent performance.
  </div>
</fieldset>
```

Change the fallback to `relayOnly: props.defaultRelayOnly ?? true`. Add focused flex-column styles for `.sb-mode-options`, `.sb-mode-option`, and its copy span; reuse `.sb-warning`.

- [ ] **Step 4: Verify GREEN and commit**

```bash
cd extensions/opencloud
npm test -- src/components/CreateShareModal.test.ts
npm run typecheck
git add src/components/CreateShareModal.vue src/components/CreateShareModal.test.ts
git commit -m "feat: recommend relay in OpenCloud"
```

Expected: tests and type checking PASS, then the commit succeeds.

### Task 4: Update the Nextcloud Share Form

**Files:**
- Modify: `extensions/nextcloud/src/components/CreateShareModal.test.ts`
- Modify: `extensions/nextcloud/src/components/CreateShareModal.vue`

- [ ] **Step 1: Replace checkbox/TURN tests with failing mode tests**

Replace those tests with:

```ts
it('lists Relay first and selects it by default', () => {
    const wrapper = mountModal()
    expect(wrapper.findAll('[data-testid$="-option"]').map(
        choice => choice.attributes('data-testid'),
    )).toEqual(['relay-option', 'direct-option'])
    expect((wrapper.find('[data-testid="mode-relay"] input').element as HTMLInputElement).checked).toBe(true)
    expect(wrapper.text()).toContain('Relay (recommended)')
    expect(wrapper.text()).toContain(
        'End-to-end encrypted, hides your IP, and provides consistent performance',
    )
    expect(wrapper.text()).not.toContain('TURN')
})

it('shows the Direct advisory when Direct is selected', async () => {
    const wrapper = mountModal()
    await wrapper.find('[data-testid="mode-direct"] input').setValue(true)
    expect(wrapper.find('[data-testid="direct-advisory"]').text()).toContain(
        'Direct transfers expose your IP address and may be slower due to browser protocol limitations. Use Relay for more consistent performance.',
    )
})

it('honors a Direct default from props', () => {
    const wrapper = mountModal({ defaultRelayOnly: false })
    expect((wrapper.find('[data-testid="mode-direct"] input').element as HTMLInputElement).checked).toBe(true)
})
```

Extend the submit assertion with `relay_only: true`.

- [ ] **Step 2: Run the component test and verify RED**

```bash
cd extensions/nextcloud && npm test -- src/components/CreateShareModal.test.ts
```

Expected: FAIL because the component has one checkbox, defaults to Direct, and contains TURN copy.

- [ ] **Step 3: Implement explicit recommended-first choices**

Use two `NcCheckboxRadioSwitch` components with `type="radio"`, boolean values, and test IDs `mode-relay` and `mode-direct`. Put copy spans with IDs `relay-option` and `direct-option` inside them, in that order. Use the same approved labels/descriptions as OpenCloud, followed by:

```html
<NcNoteCard v-if="!form.relayOnly" data-testid="direct-advisory" type="warning">
    Direct transfers expose your IP address and may be slower due to browser protocol limitations. Use Relay for more consistent performance.
</NcNoteCard>
```

Change the fallback to `relayOnly: props.defaultRelayOnly ?? true`. Keep `turnAvailable` as an accepted compatibility prop, but do not use it for warnings. Add focused flex-column styles for the fieldset and copy spans.

- [ ] **Step 4: Verify GREEN and commit**

```bash
cd extensions/nextcloud
npm test -- src/components/CreateShareModal.test.ts
npm run typecheck
git add src/components/CreateShareModal.vue src/components/CreateShareModal.test.ts
git commit -m "feat: recommend relay in Nextcloud"
```

Expected: tests and type checking PASS, then the commit succeeds.

### Task 5: Full Relevant Verification

**Files:**
- Verify only; no planned source changes.

- [ ] **Step 1: Run all agent tests**

```bash
cd agent && go test ./... -count=1
```

Expected: PASS with zero failures.

- [ ] **Step 2: Run complete OpenCloud validation**

```bash
cd extensions/opencloud && npm test && npm run typecheck && npm run build
```

Expected: tests, type checking, and production build PASS.

- [ ] **Step 3: Run complete Nextcloud validation**

```bash
cd extensions/nextcloud && npm test && npm run typecheck && npm run build
```

Expected: tests, type checking, and production build PASS.

- [ ] **Step 4: Verify obsolete UI copy is gone**

```bash
rg -n "Requires TURN server|Relay mode requires a TURN server|Fast, free, uses TURN|All traffic via TURN" agent/internal/web extensions/opencloud/src extensions/nextcloud/src
```

Expected: no matches.

- [ ] **Step 5: Inspect the final diff and workspace**

```bash
git diff --check
git status --short
```

Expected: no whitespace errors; the user's pre-existing untracked `.claude/` and `.codex/` directories remain untouched.

