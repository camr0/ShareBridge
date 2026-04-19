# Noise Independent Vectors Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add independently sourced Noise and NIST vector tests that block both the Go and JS Noise implementations separately before shipping.

**Architecture:** Vendor the official `cacophony` Noise vectors and NIST ECC CDH vectors into a shared repo-level `testdata/external/` directory. Add tiny local parsers in Go and JS test files so each runtime replays the same external fixtures independently, while keeping the existing Go↔JS parity tests as a third layer rather than the primary oracle.

**Tech Stack:** Go testing, Node test runner, Web Crypto, `crypto/ecdh`, vendored JSON/text fixtures from Noise-C and NIST CAVP

---

### Task 1: Vendor Official Fixtures

**Files:**
- Create: `testdata/external/cacophony.txt`
- Create: `testdata/external/KAS_ECC_CDH_PrimitiveTest.txt`

- [ ] **Step 1: Add failing tests that expect the shared fixture files**

Create tests in:
- `agent/internal/noise/official_vectors_test.go`
- `signaling-server/web/noise-p256/official-vectors.test.js`

The first assertion in each file should load:
- `../../../testdata/external/cacophony.txt`
- `../../../testdata/external/KAS_ECC_CDH_PrimitiveTest.txt`

Expected red state: tests fail with file-not-found before any parsing happens.

- [ ] **Step 2: Vendor the exact upstream files**

Sources:
- `https://raw.githubusercontent.com/rweather/noise-c/master/tests/vector/cacophony.txt`
- `https://csrc.nist.gov/CSRC/media/Projects/Cryptographic-Algorithm-Validation-Program/documents/components/ecccdhtestvectors.zip`

Store the unmodified `cacophony.txt` JSON file and the unzipped `KAS_ECC_CDH_PrimitiveTest.txt` text file under `testdata/external/`.

- [ ] **Step 3: Re-run the new tests**

Run:
```bash
go test ./internal/noise/... -run 'Official|NIST'
node --test signaling-server/web/noise-p256/official-vectors.test.js
```

Expected: parsing/behavior failures, not missing-file failures.

### Task 2: Go Independent Vector Tests

**Files:**
- Create: `agent/internal/noise/official_vectors_test.go`

- [ ] **Step 1: Write failing Go tests for NIST P-256 vectors**

Add a parser that reads the `[P-256]` section and verifies `dhP256()` against multiple `ZIUT` entries.

- [ ] **Step 2: Run Go test to verify the red state**

Run:
```bash
go test ./internal/noise/... -run TestNISTP256Vectors
```

Expected: FAIL until the parser and fixtures are wired correctly.

- [ ] **Step 3: Add minimal Go parser/harness code**

In the same test file:
- parse the NIST text format into cases
- import `dIUT` via `ecdh.P256().NewPrivateKey`
- import `QCAVSx/QCAVSy` as uncompressed public keys
- compare `dhP256()` output to `ZIUT`

Also add a test that loads the official `Noise_XX_25519_AESGCM_SHA256` vector from `cacophony.txt` and replays all six messages using:
- `crypto/ecdh.X25519()` for DH
- local test-only symmetric state driven by `mixHash`, `hkdf2`, `splitKeys`, and `CipherState`

- [ ] **Step 4: Run Go tests to green**

Run:
```bash
go test ./internal/noise/... -run 'TestNISTP256Vectors|TestOfficialNoiseVector'
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add testdata/external/cacophony.txt testdata/external/KAS_ECC_CDH_PrimitiveTest.txt agent/internal/noise/official_vectors_test.go
git commit -m "test(noise): add Go independent vector verification"
```

### Task 3: JS Independent Vector Tests

**Files:**
- Create: `signaling-server/web/noise-p256/official-vectors.test.js`

- [ ] **Step 1: Write failing JS tests for the same fixture files**

Add tests that:
- parse the shared NIST `[P-256]` section
- verify `dh()` against `ZIUT`
- parse the official `Noise_XX_25519_AESGCM_SHA256` vector
- replay the full vector with built-in X25519 import/derive support plus existing transcript/cipher helpers

- [ ] **Step 2: Run JS test to verify the red state**

Run:
```bash
node --test signaling-server/web/noise-p256/official-vectors.test.js
```

Expected: FAIL until the harness is complete.

- [ ] **Step 3: Add minimal JS parser/harness code**

In the same test file:
- load shared fixtures via `node:fs/promises`
- import P-256 private keys with `importPrivateKeyFromScalar()`
- import NIST public keys with `importPublicKey()`
- verify `dh()` output equals `ZIUT`
- build test-only X25519 PKCS8/SPKI wrappers for cacophony raw keys
- replay all six official messages and compare ciphertext/plaintext exactly

- [ ] **Step 4: Run JS tests to green**

Run:
```bash
node --test signaling-server/web/noise-p256/official-vectors.test.js
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add signaling-server/web/noise-p256/official-vectors.test.js
git commit -m "test(noise): add JS independent vector verification"
```

### Task 4: Full Verification

**Files:**
- Verify only

- [ ] **Step 1: Run full Go suite**

```bash
go test ./internal/noise/...
```

- [ ] **Step 2: Run full JS suite**

```bash
node --test signaling-server/web/noise-p256/*.test.js
```

- [ ] **Step 3: Run whitespace check**

```bash
git diff --check
```

- [ ] **Step 4: Commit final integration pass**

```bash
git add docs/superpowers/plans/2026-04-18-noise-independent-vectors.md
git commit -m "docs(noise): plan independent vector verification"
```
