# Experiment 29 — how much of the client's app-side cost is recoverable while keeping end-to-end verification intact? (verification-preserving client fix) — 2026-09-18/19

**Verdict: the fix removes the entire app-side cost that real users feel, and only that. The ~85–226 s
post-transfer tail is 100 % app-caused and is gone (0.30/0.33 s unpinned, 1.34 s pinned — a 273×/98×
shortening), so **user-visible time falls 139.9 → 43.4 s unpinned (3.23×) and 299.2 → 169.6 s pinned to
1 vCPU (1.76×)** — while the checksum still verifies (`✓ intact`, worker digest = expected digest in every
fixed cell). Unpinned the fix also lifts the agent-side rate **115.36 → 146.54 Mbps (1.27×)** on **48 %
less renderer CPU** (2.07 → 1.07 cores). But at 1 vCPU it recovers **0 % of the 2.23× rate headroom**
(37.53 vs 37.53 Mbps — identical windows, 168.549 vs 168.529 s) **because the pinned client is not
CPU-bound at all**: the fixed arm used only **0.774 of the single pinned core** (control 0.992) yet hit the
same ceiling, so that ~37.5 Mbps pinned limit is not app CPU and cannot be bought back in app code; the
headroom the fix does not recover is the StreamSaver/service-worker write path and the browser's receive
path on one core (E28's sink-free 56.35 / bare 82.99 Mbps pinned remain unreached). Deciding numbers:
tail **85.2 s → 0.31 s** (4 vCPU) and **≤131.8 s → 1.34 s** (1 vCPU).

**Setup:** Field rig. Agent = VERSA container `sb-run` ONLY (`sb-agent:pristine`, host networking, `UI_PORT=7879`,
`UI_ADDR=127.0.0.1`, `SB_SCTP_CA_STEP=32768`, no `SB_SCTP_MIN_CWND`, no `--cpus`; `docker inspect`:
`NanoCpus=0`, `StartedAt 2026-09-18T22:39:06Z` — **not recreated by this experiment**).
Signalling = TESTBOX v1 (Caddy TLS) + **test-only** web root `/opt/sharebridge-test/web/`.
Client = CLIENT-EAST (4 vCPU, b2-15, ~12 ms path). Payload = one 754 MiB file = 790,626,304 B = 6,325.01 Mb per
download (share `a1k6q46n`, `relay_only=false`, agent downloads count 90 at start, expires 2026-09-20T04:49:25Z).
Agent-side metric: `docker logs --timestamps sb-run`, window = first `DataChannel lanes ready` → last
`download complete`; Mbps = 6,325.01 ÷ window_s. User-visible metric: the client's own terminal UI moment
(`.file-item` class `verified` + `.file-status` = `✓ intact`, observed by the driver) minus the agent's
`download complete` timestamp; both clocks were checked (CLIENT-EAST vs TESTBOX skew ≈ 1.0 s, TESTBOX vs the
agent host < 0.1 s), so tails of a few hundred ms are meaningful and tails of tens of seconds are unambiguous.
Renderer cores from a 5 s `/proc/<pid>/stat` delta sampler classified per PID by cmdline role
(renderer / network service / gpu / chrome-main / node / xvfb) — a Chrome Web Worker runs in the renderer
process, so `renderer=` includes the hashing worker.

## The two defects, and the minimal fix

Ranked per-byte costs came from the E-prior code reading of `signaling-server/web/src/` (unverified by
measurement). This experiment fixes the top-ranked one and the third:

- **(a) `downloadSinks.js:20-31` re-concatenated the whole 1 MiB verification tail on EVERY append.**
  With 64 KiB bulk frames that is `new Uint8Array(1 MiB + 64 KiB)` + a 1.06 MiB copy per chunk (~17×
  write amplification: 754 MiB of payload becomes ~12.8 GiB of allocation+copy) on the main thread, and the
  retained `subarray` keeps a full 1.06 MiB backing store alive per append.
- **(b) SHA-1 (`hash-wasm`) ran on the main thread**, serialised behind the same chain.
- (Untouched, for attribution honesty: `vendor/streamsaver.js postMessage(chunk)` service-worker hop per
  write, the 14-byte `data.slice(14)` envelope copy, the promise/timer/DOM overhead per chunk. The fix
  **also** cuts StreamSaver hops 16× as a side effect, because the fixed sink now writes 1 MiB blocks
  instead of 64 KiB blocks — so the measured win bundles (a), (b) and fewer writer hops. There is no arm
  that separates the writer-hop reduction from (a)+(b).)

### Exact diff (TESTBOX test web root only)

`src/downloadSinks.js` — appended, existing code untouched:

```js
// SHA-1 over the same byte stream, computed in a module Web Worker.
// crypto.subtle.digest has no incremental API (it cannot hash a 754 MiB stream
// chunk-by-chunk), so the worker runs the same incremental hash-wasm SHA-1.
export function createWorkerSha1({ workerUrl = new URL('./hashWorker.js', import.meta.url), maxOutstanding = 512 } = {}) {
  const worker = new Worker(workerUrl, { type: 'module' })
  const ackWaiters = []
  let outstanding = 0
  let seq = 0
  let digestWaiter = null
  let failure = null

  worker.onmessage = (event) => {
    const msg = event.data || {}
    if (msg.type === 'ack') {
      outstanding -= 1
      const release = ackWaiters.shift()
      if (release) release()
      return
    }
    if (msg.type === 'digest' && digestWaiter && msg.id === digestWaiter.id) {
      const { resolve } = digestWaiter
      digestWaiter = null
      try {
        globalThis.__exp29 = { ...(globalThis.__exp29 || {}), computedSha1: msg.hex, worker: true }
      } catch (_) {}
      resolve(msg.hex)
      return
    }
    if (msg.type === 'error') {
      failure = new Error('sha1 worker: ' + msg.message)
      if (digestWaiter) { const { reject } = digestWaiter; digestWaiter = null; reject(failure) }
    }
  }
  worker.onerror = (event) => { failure = new Error('sha1 worker failed: ' + (event.message || 'unknown')) }

  return {
    update(bytes) {
      if (failure) throw failure
      seq += 1
      // One copy per chunk (64 KiB) is the price of transferable, non-blocking
      // hashing; it replaces a ~1.06 MiB allocation+copy per chunk.
      const copy = new Uint8Array(bytes.length)
      copy.set(bytes)
      outstanding += 1
      worker.postMessage({ type: 'update', seq, buf: copy.buffer }, [copy.buffer])
      if (outstanding >= maxOutstanding) return new Promise((resolve) => ackWaiters.push(resolve))
      return undefined
    },
    digest() {
      const id = 'digest-' + seq
      return new Promise((resolve, reject) => {
        if (failure) { reject(failure); return }
        digestWaiter = { id, resolve, reject }
        worker.postMessage({ type: 'digest', id })
      })
    },
  }
}

// Fixed streaming sink: writes the byte stream through a single fixed
// tailBytes buffer instead of re-building concatBytes(tail, bytes) per append.
export async function createStreamingSinkFixed({
  fileName, mimeType, tailBytes, createWriter, createHasher,
  deferCloseSettlement = false, onDeferredCloseError,
}) {
  const writer = await createWriter(fileName, mimeType)
  const hasher = await createHasher()
  const tail = new Uint8Array(tailBytes)
  let tailLength = 0

  return {
    async append(bytes) {
      const updateResult = hasher.update(bytes)
      if (updateResult && typeof updateResult.then === 'function') await updateResult

      const chunk = bytes instanceof Uint8Array ? bytes : new Uint8Array(bytes)
      let offset = 0
      while (offset < chunk.length) {
        if (tailLength === tailBytes) { await writer.write(tail); tailLength = 0 }
        const take = Math.min(tailBytes - tailLength, chunk.length - offset)
        tail.set(chunk.subarray(offset, offset + take), tailLength)
        tailLength += take
        offset += take
      }
    },
    bufferedTailSize() { return tailLength },
    async finalize({ expectedSha1, expectedSize, receivedBytes }) {
      const normalizedExpectedSha1 = normalizeExpectedSha1(expectedSha1)
      const computedSha1 = normalizedExpectedSha1 ? await hasher.digest('hex') : null

      if (receivedBytes !== expectedSize) {
        await writer.abort('size-mismatch')
        return { ok: false, code: 'size-mismatch' }
      }
      if (normalizedExpectedSha1 && computedSha1 !== normalizedExpectedSha1) {
        await writer.abort('checksum-mismatch')
        return { ok: false, code: 'checksum-mismatch', computedSha1 }
      }
      if (tailLength > 0) await writer.write(tail.subarray(0, tailLength))
      const closeResult = writer.close()
      if (deferCloseSettlement) {
        void Promise.resolve(closeResult).then(undefined, (error) => {
          try { onDeferredCloseError?.(error) } catch (_callbackError) {}
        })
      } else {
        await closeResult
      }
      return { ok: true, code: normalizedExpectedSha1 ? 'intact' : 'done', computedSha1 }
    },
    async abort(reason) { await writer.abort(reason) },
  }
}
```

New file `src/hashWorker.js` (1069 B, root:root 644):

```js
import { createSHA1 } from './vendor/hash-wasm.js'
const hasherPromise = createSHA1()
self.onmessage = async (event) => {
  const msg = event.data || {}
  try {
    if (msg.type === 'update') {
      const hasher = await hasherPromise
      hasher.update(new Uint8Array(msg.buf))
      if (msg.seq) self.postMessage({ type: 'ack', seq: msg.seq })
      return
    }
    if (msg.type === 'digest') {
      const hasher = await hasherPromise
      self.postMessage({ type: 'digest', id: msg.id, hex: hasher.digest('hex') })
      return
    }
  } catch (error) {
    self.postMessage({ type: 'error', message: String(error && error.message ? error.message : error) })
  }
}
```

`src/app.js` — three edits, all inside the `?fix=1` gate; the `?fix` absent path is the current client:

```js
import { ..., createStreamingSinkFixed, createWorkerSha1 } from './downloadSinks.js'   // +2 names
const EXP29_FIX_MODE = typeof location !== 'undefined' &&
  new URLSearchParams(location.search).has('fix')
// in buildDownloadSink(), before the existing streaming branch:
  if (support.mode === 'streaming' && EXP29_FIX_MODE) {
    return createStreamingSinkFixed({
      fileName: header.name, mimeType: header.mimeType, tailBytes: 1024 * 1024,
      createWriter: createBrowserStreamWriter,
      createHasher: () => createWorkerSha1(),
      ...albumCloseOptions,
    })
  }
```

Patcher (idempotent, anchored, aborts if an anchor is not found exactly once):
`/tmp/exp29/exp29-patch.py`; outputs `app.js` 92,265 B (`cc4c944c…`), `downloadSinks.js` 11,679 B
(`db69d58d…`), `hashWorker.js` 1069 B (`10f9dc0e…`).

**Why not `crypto.subtle.digest`:** WebCrypto exposes only a one-shot `digest()` — there is no incremental
interface, so a 754 MiB stream cannot be hashed with it chunk-by-chunk. The worker therefore runs the same
incremental `hash-wasm` SHA-1 the client already uses, just off the main thread. (The repo's `createBlobSink`
already uses `crypto.subtle` for the non-streaming whole-blob path, where the bytes are in one buffer.)

### Correctness gates (run before any field cell)

1. **Byte-stream equivalence (logic test).** A 200-trial randomized harness compared the original
   `concatBytes` sink and the fixed sink (tail window 8 B vs 0–4 B chunks, ~6,800 appends):
   `LOGIC-OK: written + final tail bytes identical to the original sink; withheld window <= tailBytes`.
   The concatenation `written ++ final tail` is byte-identical, so the hashed stream and the written file
   are identical. **The one semantic delta** is the withheld window: the original always holds exactly
   `tailBytes` unwritten, the fixed sink flushes when its 1 MiB buffer fills, so it withholds 0–1 MiB.
   Success-path output is identical; on a checksum/size failure at most 1 MiB more may already have been
   written before `writer.abort()` (the original had already written everything but the last 1 MiB).
   The size and SHA-1 gate itself is unchanged code.
2. **Live checksum verification (field).** Each `?fix=1` cell must show `✓ intact`, and the driver compares
   the worker-computed 40-hex digest (`globalThis.__exp29.computedSha1`) against the SHA-1 the client
   displays for the *expected* file hash — a mismatch logs `HASH-DISAGREE` — and refuses to report a cell
   whose fix never engaged (`FIX-NOT-ENGAGED`).
3. Syntax check of all three deployed files (`node --check`) — OK.

## Part 0 — pre-change state (00:05–00:11Z, 2026-09-19)

- Rig: `sb-run` present, `sb-agent:pristine`, `NanoCpus=0`, `UI_PORT=7879`, `UI_ADDR=127.0.0.1`,
  `SB_SCTP_CA_STEP=32768`, no `SB_SCTP_MIN_CWND`, `StartedAt 2026-09-18T22:39:06Z`; agent log idle since
  00:04:46 (`download complete (count: 90)`), `pending wait window exceeded` relay standby teardown only.
- TESTBOX: Caddy **active**, `sharebridge-test` **active**, local `http://127.0.0.1:8080/` = 200.
- Test web root manifest taken **before** any change: `find . -type f -exec sha256sum {} \;` = 140 files
  (`/tmp/exp29-web-manifest-pre.txt` on TESTBOX). Originals backed up to `/tmp/exp29-backup/src/`:
  `app.js` = `407ee703…` 91,308 B 644, `downloadSinks.js` = `5c7ff322…` 644 — `app.js` byte-identical to the
  repo copy and to the E15/E25/E27/E28 restored state.
- CLIENT-EAST: `node`/`chrome`/`Xvfb` counts all **0**.
- Post-patch: TESTBOX `src/app.js` `cc4c944c…` 92,265 B, `src/downloadSinks.js` `db69d58d…` 11,679 B,
  `src/hashWorker.js` `10f9dc0e…` 1069 B, all root:root 644; a fresh HTTP retrieval **from CLIENT-EAST**
  through the real origin (`https://…/src/app.js`, `/src/hashWorker.js`) returns the same hashes as the local
  patched files — the client really is exercising the fixed code.
- Share page from CLIENT-EAST = **200** ×1/1.

## Raw cells

All cells CLIENT-EAST, one at a time, agent-side window (`DataChannel lanes ready` → `download complete`),
6,325.01 Mb per download. `finish` = agent `download complete` → client terminal UI (`✓ intact`).
`pin` = the `taskset` mask the driver (and therefore all of Chromium) ran under.

### Arm C — control (current client, no `?fix`), unpinned (4 vCPU)

| # | cell | pin | lanes ready → download complete (UTC) | agent window s | **agent Mbps** | agent finished | client terminal (UTC) | **finish tail s** | client-visible total s | renderer cores (mean/peak) | UI | direct? |
|---|---|---|---:|---:|---|---|---:|---:|---|---|---|
| 1 | **C1** | none | 00:12:08.258 → 00:13:03.209 | 54.951 | **115.10** | 00:13:03.209 | 00:14:29.121 | **85.912** | 140.76 | **2.059 / 2.244** (n=12) | `✓ intact` `a7e0206e…` | yes |
| 2 | **C2** | none | 00:16:40.269 → 00:17:34.974 | 54.705 | **115.62** | 00:17:34.974 | 00:18:59.408 | **84.434** | 138.97 | 2.077 / 2.154 (n=11) | `✓ intact` `a7e0206e…` | yes |

**Arm C unpinned mean = 115.36 Mbps** (n=2), mean window 54.828 s, mean finish tail **85.17 s**, mean
user-visible total **139.87 s**. Both cells direct, relay standby unused in both, **0 relay cells discarded**
(2 of 2 attempts valid).

C1 details: badge `● Connected (Direct)` read **before** the click (00:12:08.356), relay standby connected at
join and torn down at 00:12:52 with `pending wait window exceeded` carrying no data — **0 relay cells
discarded in this arm**. Driver exited cleanly, no `node`/`chrome`/`Xvfb` left. UI SHA-1 displayed
`a7e0206e573edbef0c4d8107a151271fbeccf2fe` (the share's expected hash — this is the client's own verification
verdict, not a byte count).

### Arm F — fixed sink + worker SHA-1 (`?fix=1`), unpinned (4 vCPU)

| # | cell | pin | lanes ready → download complete (UTC) | agent window s | **agent Mbps** | agent finished | client terminal (UTC) | **finish tail s** | client-visible total s | renderer cores (mean/peak) | UI | direct? |
|---|---|---|---:|---:|---|---|---:|---:|---|---|---|
| 1 | **F1** | none | 00:15:09.800 → 00:15:53.272 | 43.472 | **145.50** | 00:15:53.272 | 00:15:53.569 | **0.298** | 43.61 | **1.143 / 1.455** (n=8) | `✓ intact` `a7e0206e…` | yes |
| 2 | **F2** | none | 00:20:36.538 → 00:21:19.398 | 42.860 | **147.58** | 00:21:19.398 | 00:21:19.724 | **0.326** | 43.09 | 0.991 / 1.332 (n=10) | `✓ intact` `a7e0206e…` | yes |

**Arm F unpinned mean = 146.54 Mbps** (n=2; 145.50, 147.58), mean window 43.166 s, mean finish tail
**0.312 s**, mean user-visible total **43.35 s**. Both cells direct, relay standby unused, **0 relay cells
discarded**. Both cells verified: `worker_sha1 = ui_sha1 = a7e0206e573edbef0c4d8107a151271fbeccf2fe`
(identical to arm C's verified hash).

**Unpinned A/B (n=2 each, interleaved C1 → F1 → C2 → F2):** agent-side **115.36 → 146.54 Mbps (1.270×)**;
finish tail **85.17 s → 0.312 s**; user-visible total **139.87 → 43.35 s (3.23×)**; renderer cores
**2.068 → 1.067 mean (48 % less)** while moving 27 % more bytes per second, i.e. the app's own per-byte
cost is essentially removed at 4 vCPU and the client stops being the constraint.

### Arm Cp — control (current client), pinned to 1 vCPU (`taskset -c 0`)

| # | cell | pin | attempt | lanes ready → download complete (UTC) | agent window s | **agent Mbps** | agent finished | client terminal | **finish tail s** | client-visible total s | renderer cores (mean/peak) | UI | direct? |
|---|---|---|---|---:|---:|---|---|---|---:|---:|---|---|---|
| — | Cp1 | `-c 0` | 1st pinned | — | — | — | — | — | — | — | — | — | **no — relay (`● Connected (Relay)`, `RELAY-ABORT`)** |
| — | Cp2 | `-c 0` | 2nd pinned | — | — | — | — | — | — | — | — | — | **no — relay (`RELAY-ABORT`)** |
| 1 | **Cp3** | `-c 0` | 3rd pinned | 00:23:45.463 → 00:26:33.993 | 168.529 | **37.53** | 00:26:33.993 | 00:28:45.772 read (driver deadline) | **≤131.8 s (proved bound; core stayed 100 % busy for a further 226 s)** | **≥299.2** | **0.644 / 0.683** renderer, **0.992 total** (n=33) | `✓ intact` `a7e0206e…` | yes |

Cp3 details and why its tail is a **bound**, not an exact number: the pinned control saturates the single
core (`total=0.99` cores for the whole transfer and for **226 s afterwards**), so the driver's DOM
`page.evaluate` calls — the thing that observes the terminal UI — were starved: the last progress line it
managed was at 00:25:21 (21 %, running average 1.6 MB/s) and the loop's next read did not return until the
300 s deadline (00:28:45.772). That read already showed `cls="file-item verified"`, `status="✓ intact"`,
100 %, which **proves the client's terminal state was reached at or before 00:28:45.77, i.e. tail ≤ 131.8 s**
(the true tail could be shorter — the page was unanswerable, so we cannot observe it) and **user-visible
total ≥ 299.2 s measured from the click**; the pin was 100 % busy on client work until the 00:30:20–00:30:25
samples, i.e. up to **394 s** of user-unusable machine time after the click. The arm is scored at the
conservative lower bound (tail 131.8 s, total 299.2 s) — which *understates* the fix's pinned win. The freeze
itself is user-visible pain (the page cannot even answer a DOM query for ~3.4 min after the last byte).
Agent-side 37.53 Mbps reproduces E25's pinned real-client 37.19 (n=2) — the pinned control is CPU-bound at
the ~37.5 Mbps ceiling. Relay cells Cp1/Cp2 discarded (badge asserted `Relay` **before** the click; no agent
`lanes ready` for either).

### Arm Fp — fixed sink + worker SHA-1 (`?fix=1`), pinned to 1 vCPU (`taskset -c 0`)

| # | cell | pin | lanes ready → download complete (UTC) | agent window s | **agent Mbps** | agent finished | client terminal (UTC) | **finish tail s** | client-visible total s | renderer cores (mean/peak) | UI | direct? |
|---|---|---|---:|---:|---|---|---:|---:|---|---|---|
| 1 | **Fp1** | `-c 0` | 00:31:01.227 → 00:33:49.776 | 168.549 | **37.53** | 00:33:49.776 | 00:33:51.111 | **1.335** | 169.55 | **0.356 / 0.457** renderer, **0.774 total** (n=33) | `✓ intact` `a7e0206e…` | yes |

Fp1 details: direct badge before the click (00:31:01.560); relay standby torn down at 00:31:45 with
`pending wait window exceeded`, no relay data; **0 relay cells discarded in this arm**. The driver polled the
DOM every 500 ms without ever being starved, and read `worker_sha1 = a7e0206e573edbef0c4d8107a151271fbeccf2fe`
= `ui_sha1` — verification intact, no `FIX-NOT-ENGAGED`, no `HASH-DISAGREE`. **The fixed pinned client was NOT
CPU-bound**: 0.774 of the 1.0 pinned core was in use (0.23 cores idle) at the same 37.53 Mbps the saturated
control reached, which is the key pinned finding — see Interpretation.

## Interpretation

**1. The ~80 s tail is 100 % app-side, and it is now measured to be exactly that.** It is not transport, not
the agent, not StreamSaver's steady-state write rate: it is the append pipeline running behind the
DataChannel. With the 1.06 MiB concat per 64 KiB frame the client could not keep up with the wire, so when
the agent's last byte arrived the client still had most of the transfer queued (control C1/C2 finished at
115 Mbps *and* had 85 s of work left). Remove the amplification and the same client keeps up in real time:
tail 0.31 s. This converts the tail from a fixed ~1.4× tax on user-visible time (E15: 139 s versus a 60 s
transport window) into ~0.

**2. At 1 vCPU the fix buys no throughput at all, and that is the actionable negative result.** Fixed and
control pinned windows are 168.549 s and 168.529 s (37.53 Mbps both) — a 0.01 % difference, i.e. **0 % of
the modelled 2.23× pinned headroom** (E28: real client 37.19 → sink-free 56.35 → bare 82.99 Mbps). The CPU
evidence explains why and kills the optimistic reading of E28: the fixed pinned client used **0.774 of 1.0
core**, so at 37.5 Mbps it was *not* CPU-limited; the pinned ceiling is a latency/pacing artefact of the
single-core renderer + StreamSaver service-worker hop + browser receive path, not app throughput. No app-code
change can recover pinned throughput by deleting CPU work; only removing the sink at all (E28's sink-free
56.35) or bypassing WebRTC (v2's HTTP path) does that.

**3. Where the fix does pay is the metric that matters.** Pinned user-visible time 299.2 s → 169.55 s (1.76×)
with the residual being the 168.5 s pinned *transport* window + 1.3 s. Since the pinned transport window is
immovable by app code, the fix recovers **essentially 100 % of the app-recoverable user-visible time at
1 vCPU** — the tail. Unpinned it recovers everything: 3.23× user-visible, 1.27× agent-side, on 48 % less
renderer CPU, with the app no longer the constraint.

**4. Fraction of theoretical app-side headroom recovered.**

| case | modelled app-side headroom (E28) | recovered by this fix | left on the table |
|---|---|---|---|
| 4 vCPU, agent-side rate | 1.137× (108.76 → 123.68 bare) | **≥100 %** (115.36 → 146.54 = 1.270×) | none from app code; ~0 % of E28's bare-path gap because the fixed client already *exceeds* E28's bare 123.68 (day-to-day transport variance — see caveats) |
| 4 vCPU, user-visible time | not previously quantified | **3.23×** (139.87 → 43.35 s) | none (tail 0.31 s residual) |
| 1 vCPU, agent-side rate | 2.23× (37.19 → 82.99 bare) | **0 %** (37.53 → 37.53) | 100 % — but not app CPU (fixed arm had 23 % of the core idle); it is the StreamSaver/service-worker write path + browser receive path at 37.5 Mbps, and only removing the sink or leaving WebRTC moves it |
| 1 vCPU, user-visible time | E28 implied ≥2.23× (tail-dominated) | **1.76×** (299.2 → 169.55 s) | 0 % of the app-recoverable part; the residual 168.5 s is the pinned transport window itself |

**5. What this means for the real client (re-application recipe).** The change is three files and is
revertible with the patcher in the reverse direction: (i) keep `createStreamingSink` exactly as-is and add
`createStreamingSinkFixed` beside it; (ii) add `hashWorker.js` and `createWorkerSha1`; (iii) select the fixed
sink in `buildDownloadSink`'s streaming branch (`createHasher: () => createWorkerSha1()`). Nothing else in the
client, the server or the agent changes, and the size+SHA-1 gate is the same code. Expected user-visible win
in the field: the post-transfer tail disappears; on a busy/CPU-starved machine the win is the difference
between a UI that freezes for minutes and one that finishes when the bytes do.

## Caveats

- **n=2 unpinned, n=1 pinned per arm**; pinned cells are the expensive ones (relay losses). 2 pinned
  attempts were discarded (Cp1, Cp2 — `● Connected (Relay)` badge before the click, no `lanes ready`);
  unpinned 6 attempts → 6 direct, 0 discarded. Overall pinned relay loss 2 of 4 attempts, the same shape as
  E27 (4/8) and E28 (3/5).
- **The pinned control's tail is a bound (≤131.8 s, proven by the DOM read at the 300 s deadline), not an
  exact measurement**, because the saturated client could not serve a DOM query for 3.4 min; the pinned fixed
  tail (1.335 s) is exact. The pinned control is scored at its bound, which *understates* the fix's pinned win.
- **The fix bundles three effects** and no arm separates them: (a) the removed ~17× tail re-concatenation,
  (b) SHA-1 off the main thread, and (c) 16× fewer StreamSaver `postMessage` hops (the fixed sink writes
  1 MiB blocks instead of 64 KiB). (c) is a side effect of how the buffer is flushed, not a separate change.
- **One documented semantic delta**: the withheld (unwritten) window is 0–1 MiB instead of always exactly
  1 MiB. The written bytes plus the final tail are byte-identical (200/200 randomized logic trials) and the
  verification gate is unchanged; on a checksum/size failure at most 1 MiB more may already have been
  written before the writer is aborted. If exact withheld-window fidelity is required, keep two 1 MiB
  buffers and flush one behind the other (withheld 1–2 MiB) — same single-copy cost.
- **`crypto.subtle.digest` was NOT used**: WebCrypto has no incremental digest, so a 754 MiB stream cannot be
  hashed with it; the worker runs the client's existing incremental `hash-wasm` SHA-1 instead. The speedup
  comes from getting hashing off the main thread, not from a faster SHA-1.
- **Cross-experiment absolute rates are not comparable across days.** The fixed 4-vCPU client ran at
  146.54 Mbps — *above* E28's bare-receive 123.68 Mbps plateau on the same rig — so today's v1
  sender/transport was simply faster than E28's day. Only the within-experiment interleaved deltas
  (115.36 → 146.54; 85.17 s → 0.31 s) are load-bearing. Nothing here changes the architectural conclusion:
  v2's HTTP path remains the answer for throughput, since even the perfect app-side fix cannot move the
  1-vCPU 37.5 Mbps ceiling or the ~1 MiB-window-bound write path.
- The driver measured the *client-visible* tail using both hosts' clocks; CLIENT-EAST↔TESTBOX skew was ≈1.0 s
  (TESTBOX↔VERSA < 0.1 s), so the sub-second tails are reported to ±1 s and every tail claim of tens of
  seconds is unaffected.
- One benign `join() re-entered while browser signaling socket is still active` page error appeared in every
  cell (known since E15); it did not affect mode or transfer.

## Restore and rig verification (00:34–00:36Z)

- TESTBOX test web root restored from `/tmp/exp29-backup/src/`: `src/app.js` = `407ee703…` 91,308 B 644,
  `src/downloadSinks.js` = `5c7ff322…` 644, `src/hashWorker.js` **removed**. A full
  `find . -type f -exec sha256sum {}` manifest of the web root is **byte-identical to the pre-change
  manifest (140 files, `diff` clean)** — `/tmp/exp29-web-manifest-pre.txt` vs `…-post.txt`. Caddy and
  `sharebridge-test` both `active`, local `http://127.0.0.1:8080/` = 200.
- VERSA: `sb-run` = `sb-agent:pristine`, `NanoCpus=0`, `UI_PORT=7879`, `UI_ADDR=127.0.0.1`,
  `SB_SCTP_CA_STEP=32768`, no `SB_SCTP_MIN_CWND`, `Running=true`, `StartedAt 2026-09-18T22:39:06Z`
  (**never recreated** — read-only inspection only). Production `sharebridge-agent` (7878) was never
  touched, no container was stopped/shelved/resized/deleted, and the v2 stack was not used.
- CLIENT-EAST cleaned before and after every cell and left clean at the end:
  `node=0 chrome=0 Xvfb=0` and **0** `exp29-sampler.py` processes (`ps`-based check; the `pgrep -fc` reading of
  `2` earlier was the pattern matching its own command line).
- Agent log after the last cell: quiet after `download complete (count: 96)`.

## Artifacts

- Patcher (idempotent, anchor-checked): `/tmp/exp29/exp29-patch.py`; patched outputs
  `/tmp/exp29/out/{app.js,downloadSinks.js}` (`cc4c944c…`, `db69d58d…`) and `/tmp/exp29/hashWorker.js`
  (`10f9dc0e…`).
- Correctness harness: `/tmp/exp29/logic-test.mjs` (expects identity), `/tmp/exp29/logic-test2.mjs`
  (identity + withheld-window bound) — output `LOGIC-OK`.
- Driver/launcher/sampler: `/tmp/exp29/exp29-drive_verify.js`, `/tmp/exp29/exp29-launch.sh`,
  `/tmp/exp29/exp29-sampler.py` (copies also on CLIENT-EAST under `~/sbtest/`).
- Per-cell raw logs on CLIENT-EAST: `/tmp/exp29-{C1,C2,F1,F2,Cp1,Cp2,Cp3,Fp1}.log` (driver, DOM terminal
  moment + hashes) and `/tmp/exp29-<cell>.proc` (5 s per-PID CPU deltas, classified by role).
- TESTBOX: `/tmp/exp29-backup/src/` (originals), `/tmp/exp29-web-manifest-{pre,post}.txt`.
- Agent windows: `docker logs --timestamps sb-run` (downloads count 91 = C1 … 96 = Fp1).

F1 details: direct badge before the click (00:15:09.960); relay standby torn down at 00:15:54 with
`pending wait window exceeded`, no relay data; **0 relay cells discarded**. **Checksum verdict:** the driver
read `worker_sha1 = a7e0206e573edbef0c4d8107a151271fbeccf2fe` from the worker and the identical
`ui_sha1 = a7e0206e573edbef0c4d8107a151271fbeccf2fe` from the client's verified state — the fix hashed the
identical byte stream and the identical gate passed; no `FIX-NOT-ENGAGED`, no `HASH-DISAGREE`. Per-interval
renderer cost 0.745–1.455 cores while receiving ~17–18 MB/s, i.e. **more throughput on ~55 % of the control's
main-thread CPU**.
