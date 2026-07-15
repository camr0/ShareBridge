# Lightbox Download Button Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the lightbox download action render as a correctly positioned native LightGallery toolbar icon without a duplicate blank glyph.

**Architecture:** Keep button creation in the gallery controller and reuse LightGallery's existing `lg-icon` and `lg-download` CSS contract. Test the emitted DOM attributes directly, then verify computed styles and accessibility in the deployed preview.

**Tech Stack:** Browser JavaScript, LightGallery CSS, Node test runner.

---

### Task 1: Correct the toolbar button contract

**Files:**
- Modify: `signaling-server/web/src/gallery.js`
- Test: `signaling-server/web/src/gallery.test.js`

- [ ] **Step 1: Write the failing regression test**

Extend the lightbox download-button test to assert:

```js
assert.equal(downloadButton.className, 'lg-icon lg-download sharebridge-lightbox-download')
assert.equal(downloadButton.textContent, '')
assert.equal(downloadButton.getAttribute('aria-label'), 'Download')
```

- [ ] **Step 2: Run the focused test and confirm RED**

```bash
cd signaling-server/web
node --test --test-name-pattern='lightbox download button' src/gallery.test.js
```

Expected: FAIL because the current button lacks `lg-icon` and contains `↓`.

- [ ] **Step 3: Implement the native toolbar markup**

Change button creation to:

```js
button.className = 'lg-icon lg-download sharebridge-lightbox-download'
button.textContent = ''
```

Keep its type, title, accessible label, and click handler unchanged.

- [ ] **Step 4: Run the focused test and confirm GREEN**

Run the focused command from Step 2.

Expected: the lightbox download-button test passes.

### Task 2: Verify, deploy, and commit

**Files:**
- Verify: `signaling-server/web/src/gallery.js`
- Verify: `signaling-server/web/src/gallery.test.js`

- [ ] **Step 1: Run the complete web suite**

```bash
cd signaling-server/web
npm test
```

Expected: all tests pass.

- [ ] **Step 2: Check the patch**

```bash
git diff --check
git status --short
```

Expected: no whitespace errors and only the planned source and test files are modified.

- [ ] **Step 3: Deploy the signaling server**

```bash
cd signaling-server
./redeploy.sh
```

Expected: the VPS container rebuilds and reports the signaling server listening on port 8080.

- [ ] **Step 4: Verify the live toolbar**

Open both an image and video preview. Confirm the control has the native icon
font, a transparent toolbar background, 50 by 47 pixel dimensions, and sits
immediately beside Close. Confirm its accessible text is `Download` without an
additional private-use character.

- [ ] **Step 5: Commit the verified fix**

```bash
git add signaling-server/web/src/gallery.js signaling-server/web/src/gallery.test.js
git commit -m "fix lightbox download button styling"
```
