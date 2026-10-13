# exp48 — landing the E29 client fix in `benchdirect` (1 MiB tail buffer + SHA-1 module worker) — 2026-10-13

**Verdict: the E29 fix is applied to the client source in `.worktrees/benchdirect` and re-proven byte-identical,
and it is now the DEFAULT for the generic browser streaming path (`mode === 'streaming'`, i.e. every non-mobile-Safari
browser including the field rig's Chromium).** Nothing about the *design* was improvised: `createStreamingSinkFixed`
and `createWorkerSha1` are transcribed verbatim from the diff in
`2026-09-18-exp29-verification-preserving-client-fix.md`, and the new `src/hashWorker.js` reproduces, byte for byte,
the file that exp31 independently reconstructed from the same document (`77a10595…`, 672 B) — two independent
transcriptions of one spec agreeing exactly.

This is a **local, node-only** experiment: no field cell, no rig, no client VM, no container, no `docker build`,
**no `git` command of any kind**. Every number below was produced on this Mac (`darwin/arm64`, node v26.4.0) and is
labelled either **NEWLY MEASURED** or **INHERITED FROM E29**.

- **Equivalence: 200/200 randomized trials byte-identical, 0 mismatches**, 8,558 appends (the E29 harness used ~6,800);
  plus a production-shape run (4,096 × 16 KiB frames, 1 MiB tail) whose SHA-1 equals the reference in both sinks.
- **Client suite: 311 tests, 311 pass, 0 fail** (305 before this change; the 6 new ones are the EXP48 equivalence
  and worker-protocol tests). Adjacent `bareReceive`/`bulkReorder`/N5 paths untouched and green.
- **NEWLY MEASURED (node, not the browser):** over the real 754 MiB / 48,258 × 16 KiB frame shape, the old append
  path burns **1,930 ms** of main-thread time vs **16 ms** for the fixed one (118×), the amplification is **65×**
  (47.9 GiB allocated+copied for 0.74 GiB of payload — E29's document said ~17× because it assumed 64 KiB frames),
  and the SHA-1 the worker removes from the main thread costs **894 ms** (845 MiB/s).
- **INHERITED FROM E29, NOT re-measured here:** 3.23× user-visible, −48 % renderer CPU, 4.36× on the relay,
  85.2 s → 0.31 s post-transfer tail. **I did not measure client CPU, user-visible time, or any throughput.**

## 0. Scope compliance

- Machine: this Mac only. Worktree: `/Users/ali/Git/ShareBridge/.worktrees/benchdirect`.
- **Zero `git` commands.** Files are written for the orchestrator to commit.
- Files written: `signaling-server/web/src/{app.js,downloadSinks.js,hashWorker.js,downloadSinksFixed.test.js,downloadSinksFixed.bench.mjs}`
  and this results file. No `agent/` Go code, no `agent/deploy_bare_client.sh`, no other results file, no VERSA,
  no client VM, no rig, no other agent's files.
- One writer per file: respected. No field experiment was started, so rule 11 (one field cell at a time) is not engaged.

## 1. What changed, exactly

### 1.1 `signaling-server/web/src/downloadSinks.js` — 2 exports appended, existing code untouched

`createStreamingSink` (the `concatBytes(tail, bytes)` sink) is **kept exactly as it was**; the fix is added
beside it. Both new functions are copied verbatim from the E29 results file (§"Exact diff"):

- `createWorkerSha1({ workerUrl = new URL('./hashWorker.js', import.meta.url), maxOutstanding = 512 })` — module
  `Worker`, `ack`/`digest`/`error` message handling, one 16 KiB (or 64 KiB) copy per frame so the buffer can be
  transferred, backpressure after `maxOutstanding` unacked updates, and a `digest()` that resolves the worker's hex.
- `createStreamingSinkFixed({ fileName, mimeType, tailBytes, createWriter, createHasher, deferCloseSettlement, onDeferredCloseError })` —
  a single pre-allocated `new Uint8Array(tailBytes)` filled in place; when it is full it is written and `tailLength`
  resets. `finalize()` is the **same** size + SHA-1 gate as the original, with the same `deferCloseSettlement` /
  `onDeferredCloseError` album semantics.

**The one semantic delta is unchanged from E29 and re-proven here:** the withheld (not-yet-written) window is
`0…tailBytes` instead of always exactly `tailBytes`. Written bytes ++ final tail are identical.

New/changed byte counts and SHA-256:

| file | size | sha256 |
|---|---:|---|
| `src/downloadSinks.js` | 10,920 B | `2f47dc95147c082fb46cdf3d6b0255d3ba2a99aed257c7c52bcce81da3f49682` |
| `src/hashWorker.js` | 672 B | `77a105958f4f8a2df0254ab12a95e686c9f6389c433c1e0fabb3e16894ba98b9` |
| `src/app.js` | 94,867 B | `502263c2b50963cd7511fa370bd16f46d20962877d664e20dd36d365d1b4dd76` |
| `src/downloadSinksFixed.test.js` | 15,369 B | `aa073e5e85d8d1a5bf70c371878b77df018f4de641d7bdab3c696e4f7de3b7fa` |
| `src/downloadSinksFixed.bench.mjs` | 8,144 B | `5c01e5ca374c1ba91b85c05d4d4986f0918fa37bb722edc3a166da454cd14c74` |

**Provenance / fidelity, reported honestly.** E29 deployed `downloadSinks.js` at 11,679 B and `hashWorker.js` at
1,069 B. The results file's diff is **abridged** (its `hashWorker.js` block is 673 B including the trailing newline),
so a transcription from the document cannot be byte-identical to E29's deployed artifact — this is exactly what
exp31 already found ("the E29 result file reproduces the diff in abridged form … reconstructed from E29's documented
diff and is behaviour-equivalent, not byte-identical"). What I can show is stronger than "behaviour-equivalent":

- My `hashWorker.js` (672 B) hashes **`77a10595…` — the same SHA-256 as exp31's independent reconstruction** of the
  same block (exp31's table: `hashWorker.js` = `77a105958f4f8a2df0254ab12a95e686c9f6389c433c1e0fabb3e16894ba98b9`,
  672 B). Two independent transcriptions of the E29 document produce the identical file. The 396 B difference from
  E29's deployed 1,069 B is comment/header text the document does not reproduce; no executable line differs.
- My `downloadSinks.js` addition is 10,920 B total vs E29's 11,679 B deployed — the same ~760 B of omitted comments.
  Function bodies are verbatim.

### 1.2 `signaling-server/web/src/hashWorker.js` — new file

Verbatim from the E29 document. Imports `createSHA1` from `./vendor/hash-wasm.js` (resolves correctly from `src/`),
memoises the hasher promise, handles `update` (with `ack`) and `digest`, and reports any throw as `{type:'error'}`.
`crypto.subtle.digest` is deliberately **not** used — WebCrypto has no incremental digest and a 754 MiB stream cannot
be hashed one frame at a time with it.

### 1.3 `signaling-server/web/src/app.js` — 2 edits

1. Import list gains `createStreamingSinkFixed` and `createWorkerSha1` (both still exported from `downloadSinks.js`).
2. `buildDownloadSink()`, the `support.mode === 'streaming'` branch:

```js
  if (support.mode === 'streaming') {
    return createStreamingSinkFixed({
      fileName: header.name,
      mimeType: header.mimeType,
      tailBytes: 1024 * 1024,
      createWriter: createBrowserStreamWriter,
      createHasher: () => createWorkerSha1(),
      ...albumCloseOptions,
    })
  }
```

(was `createStreamingSink({ …, createHasher: createIncrementalSha1, … })`). `createIncrementalSha1` is still
imported and still used by the untouched `experimental-streaming` branch. Net size change: **+56 B**.

### 1.4 Three deliberate deviations from E29's deployed artifact — read these

1. **The `?fix=1` gate is NOT carried over; the fix is unconditional on `mode === 'streaming'`.** E29's gate existed
   to run an A/B against the unfixed client in one web root. Keeping it would mean the fix is still *not landed*,
   which is the whole point of this task. E29's own "re-application recipe" (§Interpretation 5) says exactly this:
   "select the fixed sink in `buildDownloadSink`'s streaming branch". If a field A/B is ever wanted again, re-add
   the two-line gate (`new URLSearchParams(location.search).has('fix')`) and put the original `createStreamingSink`
   call back in the fallthrough — see §7.
2. **`experimental-streaming` (mobile Safari, ≥100 MiB) is left on the original `createStreamingSink`.** E29 measured
   only the `streaming` branch, and its diff was inserted *before* that branch and *after* the Safari one. Extending
   the fix to Safari would be an unmeasured behaviour change on a browser nothing here has tested. **Consequence,
   stated plainly: the 65× amplification bug still exists on the mobile-Safari path.** `detectDownloadCapabilities`
   confirms the field rig's Chromium takes `streaming` (mobile Safari is the only route to `experimental-streaming`),
   so the landed fix does cover every path this campaign has measured.
3. **The `globalThis.__exp29 = { computedSha1, worker: true }` debug hook is kept verbatim.** It is inherited
   experimental instrumentation, not part of the algorithm. It is kept deliberately because it is how every field
   driver since E29 proves the fix actually engaged (`FIX-NOT-ENGAGED` / `HASH-DISAGREE` guards) — but note that
   because the fix is now unconditional, `worker_sha1` will be non-null in *every* cell, so "`worker_sha1=null`"
   can no longer be used as the unfixed positive control. A rename (e.g. `__sha1Worker`) or removal is a one-line
   follow-up; it is left as-is here to stay faithful to the measured artifact.

## 2. Equivalence proof — real numbers

Harness: `src/downloadSinksFixed.test.js`, run with `node --test src/downloadSinksFixed.test.js`.
Both sinks are driven over the identical chunk sequence with a recording writer (which copies, exactly as
StreamSaver's `WritableStream` sink does) and compared as `written bytes ++ withheld final tail`.

```
EXP48 LOGIC: 200/200 trials byte-identical, 8558 appends, max fixed withheld window 8/8 B, mismatches=0
✔ EXP48 equivalence: 200 randomized trials are byte-identical to the original sink (25.98ms)
EXP48 PRODUCTION-SHAPE: 4096 x 16 KiB frames, sha1=403054c2e08cfd87826fa4e0da34ef4c140a2338 (reference 403054c2e08cfd87826fa4e0da34ef4c140a2338); write calls original=4033 fixed=64; withheld original=1048576 fixed=1048576
✔ EXP48 equivalence: production shape (1 MiB tail, 16 KiB frames) is byte- and SHA-1-identical (654.24ms)
✔ EXP48 fixed sink: size mismatch and checksum mismatch still abort (0.91ms)
✔ EXP48 fixed sink: album deferred-close options behave like the original (9.06ms)
✔ EXP48 createWorkerSha1: marshalling protocol, transferables, backpressure, digest (2.79ms)
✔ EXP48 createWorkerSha1: worker error surfaces on update and digest (0.62ms)
ℹ tests 6  ℹ pass 6  ℹ fail 0  ℹ duration_ms 743.4
```

| gate | result |
|---|---|
| 200 randomized trials, `written ++ final tail` byte-identical to the original `concatBytes` sink | **200/200 pass, 0 mismatches** |
| same 200 trials: the stream hashed by the sink equals the input stream, for both sinks | 200/200 |
| fixed withheld window `≤ tailBytes` | 200/200, max 8/8 B with `tailBytes=8` |
| total appends exercised | **8,558** (E29's harness: ~6,800) |
| production shape 4,096 × 16 KiB, 1 MiB tail: written bytes identical, SHA-1 identical to `node:crypto` reference | pass (`403054c2…`) |
| fixed-sink writer calls vs original at production shape | 64 vs 4,033 (63×) |
| size-mismatch / checksum-mismatch abort reasons unchanged | pass |
| album `deferCloseSettlement` + `onDeferredCloseError` semantics preserved | pass |
| `createWorkerSha1`: transfer list is exactly `[copy.buffer]`, caller's buffer never detached, `maxOutstanding` backpressure honoured, digest == `node:crypto` SHA-1, `__exp29` hook populated, worker `error` surfaces on both `update` and `digest` | pass |

**The E29 claim reproduces.** 200/200 randomized equivalence trials byte-identical — confirmed here on a fresh,
independent transcription, with a larger append count than E29 used.

Two implementation invariants the tests pin down, worth stating because they are new relative to the old sink:

- The fixed sink **reuses one `Uint8Array`** and passes the *same object* to `writer.write()` on every flush. This is
  safe only because the write is awaited before the buffer is refilled (standard `WritableStreamDefaultWriter`
  discipline, and what StreamSaver's `postMessage` sink does). A writer that retained the reference past the awaited
  write would corrupt data. The original sink passed fresh `subarray` views, so it had no such requirement.
- `createWorkerSha1.update()` **copies** each frame before transferring it. That copy is the price of
  non-blocking hashing and is counted in the benchmark below.

## 3. Existing client test suite

```
$ cd signaling-server/web && node --test src/*.test.js
ℹ tests 311   ℹ pass 311   ℹ fail 0   ℹ cancelled 0   ℹ skipped 0   ℹ todo 0
```

Baseline with the new test file excluded (`ls src/*.test.js | grep -v downloadSinksFixed`): **305 tests, 305 pass,
0 fail.** So the change adds 6 tests and breaks none. 311 − 305 = 6, matching the new file exactly; no pre-existing
test changed status.

Adjacent paths (exp44's `bareReceive.js` ablation and `bulkReorder.js` N5 wiring) were run explicitly:

```
$ node --test src/bareReceive.test.js src/bareReceiveMode.test.js src/bulkReorder.test.js src/n5ClientWiring.test.js src/app.test.js
ℹ tests 122   ℹ pass 122   ℹ fail 0
```

`app.js`'s bare/N5 wiring is untouched by this change — the edits are confined to the import list and
`buildDownloadSink`. `installBareReceive()` (app.js:60), the `bareReceive.record()` bulk-lane short-circuit
(app.js:499), the `bareReceive.markHeader()` control hook (app.js:852), the `ReorderBuffer` ingest path
(app.js:65–81) and `__bare` publication (app.js:2706) are all outside the diff. The N5 path also keeps its own
sink-independent behaviour: the reorder buffer releases consecutive frames to `routeOrBufferFrame`, and the sink
selection happens later, in `buildDownloadSink` — so striped bulk ordering is unaffected by which sink is chosen.

## 4. Micro-benchmark — the JS-side cost of old vs new, over the real shape

Script: `src/downloadSinksFixed.bench.mjs` (`node --expose-gc src/downloadSinksFixed.bench.mjs`).
Shape: **754.0 MiB = 790,659,072 B as 48,258 frames of 16,384 B**, tail 1 MiB. The 16 KiB frame size is the real
field granularity (exp44 §gates: "uniform 16 KiB chunking … 48,255 full 16 KiB payloads + 8 × 2 KiB tails"), *not*
the 64 KiB the E29 document assumed.

Three full runs (ms):

| arm | run 1 | run 2 | run 3 | **median** |
|---|---:|---:|---:|---:|
| old (`concatBytes`) — counting writer | 1,857.4 | 1,956.5 | 1,930.2 | **1,930.2** |
| fixed (1 MiB buffer) — counting writer | 15.8 | 16.3 | 16.6 | **16.3** |
| old — copying writer | 1,936.6 | 1,863.4 | 1,939.0 | **1,936.6** |
| fixed — copying writer | 29.1 | 29.2 | 29.2 | **29.2** |
| old + hash-wasm SHA-1 on the main thread | 2,838.7 | 2,834.7 | 2,867.9 | **2,838.7** |
| fixed + hash-wasm SHA-1 on the main thread | 906.4 | 909.6 | 909.2 | **909.2** |
| **fixed + worker-copy hasher (the LANDED configuration: no main-thread SHA-1)** | 67.4 | 69.0 | 69.7 | **69.0** |
| hash-wasm SHA-1 alone | 892.1 | 894.0 | 903.3 | **894.0** |

Derived:

- **Old per-append amplification: (1 MiB + 16 KiB) / 16 KiB = 65.0×** → **47.9 GiB allocated and copied** to deliver
  0.74 GiB. (E29's document quotes ~17×; that is the same formula with 64 KiB frames. At the real 16 KiB frame size
  the waste is **3.8× larger** than the document implies — the fix is worth *more*, not less, than E29 stated.)
- **Append-path main-thread cost removed: 1,914 ms** (counting writer) / **1,907 ms** (copying writer) per 754 MiB.
  Ratio 1,930.2 / 16.3 = **118×**.
- **Writer calls: 48,195 → 755 (63.8× fewer)** — the StreamSaver `postMessage` service-worker hop drops by the same
  factor, which is E29's bundled side effect (c) now quantified in call count.
- **SHA-1 moved off the main thread: 894 ms** at 845 MiB/s. That is main-thread time the landed fix no longer pays
  (it is paid on a worker thread instead).
- **Whole-path main-thread cost, landed vs old: 2,838.7 ms → 69.0 ms = 41.1×.** The fixed+worker arm's 69 ms is
  almost entirely the 16 KiB transfer copy `createWorkerSha1.update()` makes per frame.

**What this benchmark is not.** Node is a single JS thread: it cannot model the renderer/worker process split, the
GPU/network-service processes, the StreamSaver service-worker hop, Chromium's DataChannel receive path, or the
1-vCPU pinned ceiling. It measures *JS main-thread work per byte*, nothing else. Absolute wall times on an arm64 Mac
are not transferable to a 4-vCPU OVH client.

## 5. NEWLY MEASURED here vs INHERITED from E29

| claim | status |
|---|---|
| 200/200 randomized equivalence trials byte-identical | **re-measured: 200/200, 8,558 appends, 0 mismatches** |
| withheld window ≤ `tailBytes` (the one semantic delta) | **re-measured** (200/200; max 8/8 B at `tailBytes=8`) |
| SHA-1 gate unchanged and intact/checksum/size paths still abort | **re-measured** |
| album `deferCloseSettlement` semantics preserved | **re-measured** |
| `createWorkerSha1` worker protocol (transfer, backpressure, digest, error) | **re-measured** against a mock `Worker` |
| client suite green | **re-measured: 305 → 311 tests, 0 fail** |
| `bareReceive` / `bulkReorder` / N5 adjacent paths intact | **re-measured: 122/122 pass** |
| per-append amplification is 65× and ≈1.9 s / 754 MiB of main-thread JS | **NEWLY MEASURED (node, this Mac)** |
| SHA-1 costs 894 ms / 754 MiB of main-thread JS (845 MiB/s) | **NEWLY MEASURED (node, this Mac)** |
| writer hops fall 63.8× (48,195 → 755) | **NEWLY MEASURED (node, this Mac)** |
| landed main-thread path is 41× cheaper than old (2,838.7 → 69.0 ms) | **NEWLY MEASURED (node, this Mac)** |
| **3.23× user-visible time (139.87 → 43.35 s unpinned)** | **INHERITED FROM E29 — not re-measured** |
| **post-transfer tail 85.17 s → 0.312 s (273×)** | **INHERITED FROM E29 — not re-measured** |
| **−48 % renderer CPU (2.068 → 1.067 cores) and 1.27× agent-side rate** | **INHERITED FROM E29 — not re-measured** |
| **4.36× on the relay (exp31)** | **INHERITED FROM exp31/E29 — not re-measured** |
| **1.76× user-visible pinned to 1 vCPU (299.2 → 169.55 s)** | **INHERITED FROM E29 — not re-measured** |
| **0 % of the pinned 2.23× throughput headroom recovered** | **INHERITED FROM E29 — not re-measured** |

**I am not restating E29's −48 % as if I measured it.** The −48 % is renderer *cores* in a browser under a pinned
field cell. Nothing here measured CPU in a browser, and the node numbers above are a different quantity (main-thread
JS wall time per byte). They are consistent with the mechanism — the old path's serialised per-byte work is real and
large — but they do not confirm E29's magnitude, and they cannot.

## 6. Honest limits

- No field cell, no browser, no client VM: the only thing proven end-to-end here is **byte-for-byte stream
  equivalence and test-suite health**. Every throughput/latency/CPU claim is inherited from E29/exp31/exp30.
- The node benchmark overstates the browser's *relative* win in one way and understates it in another: it includes no
  service-worker hop cost (understates), but it also runs without Chromium's DataChannel receive path (overstates the
  share of total time that the append path occupies).
- `hashWorker.js` has never been executed by a real browser in this worktree. It is exercised in node only through a
  mock `Worker` that mimics the message protocol; the real `Worker` + `hash-wasm` + transferable combination is
  verified by E29/E30/E31/E32 field cells, not here.
- The `experimental-streaming` (mobile Safari) path still has the old 65× amplification (§1.4 item 2).
- The `__exp29` debug global now fires on every production streaming download (§1.4 item 3).
- No pre-change `app.js` blob hash is available because no `git` command was run. Reverting the two textual edits
  must produce a 94,811 B `app.js` hashing to
  `10b53ec90e2a3728556e5edce06130b95fd7d93582ef814e7414549983987706` — verified by reversing the edits on a copy
  (size delta +56 B, all of it accounted for: +48 B imports, +5 B `createStreamingSink` → `createStreamingSinkFixed`,
  +3 B hasher argument).

## 7. How to revert this

Everything is three textual edits in two files plus one file to delete. No schema, protocol, server or agent change
is involved, and the size+SHA-1 gate is the same code either way.

1. `signaling-server/web/src/app.js`
   - delete the two import names `createStreamingSinkFixed` and `createWorkerSha1` from the `./downloadSinks.js`
     import block (leave the rest of the block, including `createStreamingSink` and `createIncrementalSha1`);
   - in `buildDownloadSink()`, change the `support.mode === 'streaming'` branch back to:
     ```js
     return createStreamingSink({
       fileName: header.name,
       mimeType: header.mimeType,
       tailBytes: 1024 * 1024,
       createWriter: createBrowserStreamWriter,
       createHasher: createIncrementalSha1,
       ...albumCloseOptions,
     })
     ```
   - result must be a 94,811 B file with SHA-256
     `10b53ec90e2a3728556e5edce06130b95fd7d93582ef814e7414549983987706`.
2. `signaling-server/web/src/downloadSinks.js` — delete the `createWorkerSha1` and `createStreamingSinkFixed`
   functions (everything between `export async function createStreamingSink(...)`'s closing brace and
   `export async function createBlobSink(`). `createStreamingSink` itself is untouched and needs no edit; the
   original file was 6,547 B.
3. Delete `signaling-server/web/src/hashWorker.js`. (`src/downloadSinksFixed.test.js` and
   `src/downloadSinksFixed.bench.mjs` can stay — the test file will fail to import the deleted exports, so delete
   or skip it too if you revert.)
4. Re-run `node --test src/*.test.js` — expect 305 tests / 0 fail after removing the EXP48 test file.
5. To keep the fix but re-enable an A/B, add back the E29 gate instead of reverting:
   `const EXP29_FIX_MODE = new URLSearchParams(location.search).has('fix')` and `&& EXP29_FIX_MODE` on the fixed
   branch, with the original `createStreamingSink` call in the fallthrough.

## 8. Artifacts

- `signaling-server/web/src/downloadSinks.js` — `createStreamingSinkFixed`, `createWorkerSha1` (verbatim from E29).
- `signaling-server/web/src/hashWorker.js` — new, `77a10595…`, byte-identical to exp31's reconstruction.
- `signaling-server/web/src/app.js` — import + `streaming`-branch selection.
- `signaling-server/web/src/downloadSinksFixed.test.js` — the standing equivalence + protocol gate (6 tests).
- `signaling-server/web/src/downloadSinksFixed.bench.mjs` — the micro-benchmark, `EXP48-BENCH-JSON` line at the end.
- This file. Files are uncommitted by design; the orchestrator commits.
