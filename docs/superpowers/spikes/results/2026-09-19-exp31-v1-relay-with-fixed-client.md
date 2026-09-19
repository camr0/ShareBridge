# Experiment 31 — v1 relay with a fixed client: is 42/55 Mbps the relay path or the client sink? — 2026-09-19

**Verdict:** **The v1 relay's 42/55 Mbps was the client sink, not the relay path.** With the sink replaced by a byte
counter but the relay channel and its per-frame JS Noise decryption **fully intact**, the same relay share on the
same two clients delivered **229.93 Mbps mean (n=2: 227.958 EAST, 231.896 WEST)** versus **51.58 Mbps mean (n=3:
50.684, 49.517 EAST, 54.550 WEST)** for the unfixed client — **Arm C / Arm A = 4.46×**, above the ≥2× bar the task
set for "the client's sink". E29's recommended fix delivers almost all of it to real relay users:
**224.82 Mbps mean (n=2: 228.631 EAST, 221.003 WEST)**, a **4.36×** uplift on the agent-side E12 metric, with the
post-transfer tail collapsing **3.86 s → 0.60 s** and renderer cost falling (1.67 → 1.40 cores) while moving 4.4×
more bytes per second. The relay is therefore **not** the weakest v1 path: with the fix it runs at **~96–99 % of the
same-session 235–239 Mbps TCP capacity measured to CLIENT-EAST**, i.e. essentially at the client VM's ingress
ceiling, and it is **1.53× faster than v1 direct with the identical fixed client** (E1: 149.090 Mbps). F22's
*diagnosis* (the unfixed client pays ~65× memory amplification + ~48 k main-thread service-worker hops) is
confirmed as causal; F22's *prediction* that the relay would stay slow because of its own Noise cost is refuted.

**Question.** E12 measured the v1 relay at **41.989 Mbps (CLIENT-EAST, n=2)** and **55.306 (CLIENT-WEST, n=2)** with the
*unfixed* client, i.e. through the client that F22 shows pays ~65× memory amplification, 48,260 StreamSaver
service-worker hops and main-thread SHA-1 — and the relay additionally decrypts every frame in JavaScript Noise
(`secureRelayChannel.js` → JS `noise-p256`). E29 then produced a verification-preserving client fix (fixed 1 MiB tail
buffer instead of per-append re-concatenation + SHA-1 in a module worker; StreamSaver kept) and E15/E14 produced a
**discard-sink** technique that removes the sink entirely. This experiment re-measures the E12 relay metric with
(A) the unfixed client, (B) the E29 fix, (C) the discard sink **with the relay channel and its JS Noise decryption
intact**, and (D) a same-session direct reference.

## Setup

- **Agent:** VERSA container `sb-run` ONLY — `./run-variant.sh c` (image `sb-agent:pristine`, host networking,
  `UI_PORT=7879`, `UI_ADDR=127.0.0.1`, `SB_SCTP_CA_STEP=32768`, no `SB_SCTP_MIN_CWND`), started 07:16Z via the
  canonical script; `docker inspect` → `NanoCpus=0`. Production `sharebridge-agent` (7878) and every other VERSA
  container untouched; `sharebridge-agent-test` left stopped; the v2 stack was never used.
- **Signalling:** TESTBOX v1 rig (`/opt/sharebridge-test/server` on :8080 fronted by Caddy TLS on :443) + the
  **test-only** web root `/opt/sharebridge-test/web/`. Caddy and `sharebridge-test` both `active`; local
  `http://127.0.0.1:8080/` = 200 and `https://…/src/app.js` = 200.
- **Clients:** CLIENT-EAST (4 vCPU, b2-15, ~12 ms path) and CLIENT-WEST (4 vCPU, b2-15, ~71 ms path); `node`,
  `chrome`, `Xvfb` counts all 0 before every cell.
- **Payload/shares (reused verbatim from E12/E29/E30, no new share created):**
  - relay cells: `relay_only=true`, exactly the E12 relay share (public_url `https://TESTBOX/s/<relay code>`), agent
    download counter 4 → n at start, expires 2026-09-20T16:54Z — the same share E12 measured.
  - direct cell: `relay_only=false`, the E29/E30 share (public_url `https://TESTBOX/s/<direct code>`), counter 105 at start.
  - One 754 MiB source for all: **790,626,304 B = 6,325.010432 Mb** per download.
- **Metric (E12's, verbatim):** agent-side `relaychannel: handshake complete` → agent-side `download complete`
  (`docker logs --timestamps sb-run`); Mbps = 6,325.010432 ÷ window_s. For the direct cell the window is
  `DataChannel lanes ready` → `download complete`. **Never** the Playwright Download API.
- **Tail:** client terminal UI moment (`.file-item` class `verified`+`✓ intact`, or `done`+`✓ done` for the discard
  arm, read from the DOM by the driver) minus the agent's `download complete` timestamp. Cross-host skew
  CLIENT-EAST↔TESTBOX ≈ 1 s (E29), so sub-second tails are ±1 s and tens-of-seconds claims are unaffected.
- **Renderer cores:** 5 s `/proc/<pid>/stat` delta sampler (`exp29-sampler.py`), role-classified from cmdline; a Chrome
  Web Worker runs in the renderer process, so `renderer` includes the hashing worker.
- **Driver:** `exp31-drive.js` (derived from `exp29-drive_verify.js`; click-only, no Download API) — asserts the
  **expected** transport badge **before** the click (`MODE_EXPECT=relay|direct`, exits 3 on a mismatch) and prints
  `MODE-CONFIRMED`; for `?fix=1` cells reads the worker SHA-1 (`globalThis.__exp29`) and prints
  `FIX-NOT-ENGAGED`/`HASH-DISAGREE` guards; for `?sink=discard` cells reads the discard sink's exact byte count
  (`globalThis.__exp31`).
- **Mode assertions per cell (E12's rule):** relay cells must show the relay handshake **and the absence of
  `DataChannel lanes ready`** in the agent log; the direct cell must show `DataChannel lanes ready`. A silent mode
  fallback reads ~44.7 Mbps and looks valid.
- **Rig-state deviation (reported loudly, see "Rig state change" below).**

### Rig state change at 07:19Z (unavoidable, documented for the orchestrator)

The unshelve at 07:11Z brought up the **phase-4a/v2 stack on TESTBOX**, which had been deployed during the previous
boot (Caddy and `sharebridge-test` were stopped and the v2 units enabled at 06:20:57Z). That state makes the v1 rig
impossible: `sharebridge-relay-gateway` owns :443 (by design — "public 443 belongs only to
sharebridge-relay-gateway"), so Caddy cannot serve the v1 share page or the agent's `wss://` endpoint, and
`sharebridge.service` (v2 control plane) owns :8080, the port the v1 signalling server needs. Verified before acting:
the agent crash-looped with `dial signaling server: … connection refused`/`EOF` and `curl https://TESTBOX/` = 000.

Action taken (no `systemctl disable`, no file edits — stop/start only):

```
systemctl stop  sharebridge-relay-gateway sharebridge-relay-frps sharebridge.service
systemctl start sharebridge-test caddy
```

After that: `sharebridge-test` + `caddy` `active`, :8080 = v1 server, :443/:80 = caddy; the agent connected
(`session reconnected`, `loaded 2 sessions`). The three v2 units remain **enabled but stopped** — the orchestrator
should restart them if the phase-4a work needs them (`systemctl start sharebridge-relay-gateway
sharebridge-relay-frps sharebridge.service`). The only other unit state change is on TESTBOX test infra; no VERSA
container, no instance, no DNS/ACME, and no repo file was touched, and no `git` command was run.

### Arming the client (TESTBOX test web root only)

Originals backed up to `/tmp/exp31/backup/src/` and verified **byte-identical** to the pre-change web root
(`/tmp/exp31-web-manifest-pre.txt`, 140 files):
`app.js` = `407ee7032f90e4298ee6b0416ed69dbf94147d0c6ff0933e7c6803f129bdf3c1` (91,308 B),
`downloadSinks.js` = `5c7ff32294ab617e4cd9d37d7406ef10362b167422d3471dd406a12b4edfc3da` (6,547 B) — the same
originals E29/E30 record. Switch with `/tmp/exp31/set-arm.sh unfixed|fix|discard`.

**E29's exact patched bytes were lost** when TESTBOX rebooted at 07:11Z (it wipes `/tmp`, including E29's
`/tmp/exp29/` outputs), and the E29 result file reproduces the diff in abridged form (its displayed
`hashWorker.js` block is 672 B vs the deployed 1,069 B), so the fix was **reconstructed from E29's documented
diff** and is behaviour-equivalent, not byte-identical. Deployed hashes are recorded per arm below.

| arm | deployed `app.js` sha256 | `downloadSinks.js` sha256 | `hashWorker.js` |
|---|---|---|---|
| unfixed (A) | 407ee703… (91,308 B) | 5c7ff322… | absent |
| fix (B) | ca4fdb51a6e75cb3d28104645322be3540493d3866b8e82259b77ebe9bdc7047 (91,784 B) | 72f28ed00a8b9f6fed04102b8a23c4895509a6691a0f0e627cfca858962fe105 (10,919 B) | 77a105958f4f8a2df0254ab12a95e686c9f6389c433c1e0fabb3e16894ba98b9 (672 B) |
| discard (C) | 329551ce0ffb6aeca1f1e065370cf1b86084bb057dbdf78d7d0d82f501a10e64 (92,207 B) | 5c7ff322… | absent |

The **discard arm (C)** is the only genuinely new code: a `?sink=discard` gate at the top of `buildDownloadSink`
that returns a sink whose `append` only accumulates `bytes.length`, whose `finalize` checks the byte count against
`expectedSize` and emits `{ok:true, code:'done'}`, with `bufferedTailSize()=0` and a no-op `abort`. No
re-concatenation, no StreamSaver writer, no hashing — but the relay channel, its WebSocket framing and its
per-frame JS Noise decryption are untouched. Exact diff:

```diff
--- a/src/app.js
+++ b/src/app.js
@@ async function buildDownloadSink(header, support) {
+  // EXP31 test-only discard sink: counts bytes and validates only the byte
+  // count. No tail re-concatenation, no StreamSaver writer, no hashing.
+  // Enabled only by ?sink=discard on the TESTBOX test web root.
+  if (typeof location !== 'undefined' && new URLSearchParams(location.search).get('sink') === 'discard') {
+    let received = 0
+    return {
+      async append(bytes) {
+        received += bytes && typeof bytes.length === 'number' ? bytes.length : 0
+      },
+      bufferedTailSize() { return 0 },
+      async finalize({ expectedSize }) {
+        try {
+          globalThis.__exp31 = { receivedBytes: received, expectedSize: expectedSize }
+        } catch (_) {}
+        if (typeof expectedSize === 'number' && received !== expectedSize) {
+          return { ok: false, code: 'size-mismatch' }
+        }
+        return { ok: true, code: 'done', computedSha1: null }
+      },
+      async abort() {},
+    }
+  }
   const albumCloseOptions = header.batch_id
```

## Cells

_Appended after each cell. `win` = the E12-metric window (UTC); agent Mbps = 6,325.010432 ÷ win_s._

| # | arm | client | mode gate | `win` start → end (UTC) | win s | **agent Mbps** | agent finished | client terminal | **tail s** | renderer cores mean/peak (n) | sink bytes | UI |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| **A1** | A: relay + unfixed | CLIENT-EAST | `MODE-CONFIRMED relay` badge `● Connected (Relay)` at 07:19:19.355 | 07:19:18.759479 → 07:21:23.551874 | 124.792 | **50.684** | 07:21:23.551874 | 07:21:28.501 | **4.949** | **1.659 / 1.691** (n=25) | n/a | `✓ intact` `a7e0206e…`, `worker_sha1=null` |
| **A2** | A: relay + unfixed | CLIENT-EAST | `MODE-CONFIRMED relay` badge `● Connected (Relay)` | 07:24:45.494255 → 07:26:53.228120 | 127.734 | **49.517** | 07:26:53.228120 | 07:26:56.645 | **3.417** | **1.653 / 1.676** (n=25) | n/a | `✓ intact` `a7e0206e…`, `worker_sha1=null` |
| **B1** | B: relay + E29 fix (`?fix=1`) | CLIENT-EAST | `MODE-CONFIRMED relay` badge `● Connected (Relay)` at 07:22:00.470 | 07:22:00.312240 → 07:22:27.976948 | 27.665 | **228.631** | 07:22:27.976948 | 07:22:28.545 | **0.568** | **1.429 / 1.447** (n=5) | n/a | `✓ intact` `a7e0206e…`, `worker_sha1=a7e0206e…` |
| **C1** | C: relay + discard sink (`?sink=discard`) | CLIENT-EAST | `MODE-CONFIRMED relay` badge `● Connected (Relay)` | 07:23:41.504042 → 07:24:09.250457 | 27.746 | **227.958** | 07:24:09.250457 | 07:24:09.468 | **0.218** | **1.093 / 1.122** (n=5) | **790,626,304 / 790,626,304** | `✓ done` |
| **D1** | D: **direct** + unfixed (reference) | CLIENT-EAST | `MODE-CONFIRMED direct` badge `● Connected (Direct)` at 07:27:33 | 07:27:32.746923 → 07:28:25.842728 | 53.096 | **119.124** | 07:28:25.842728 | 07:29:32.451 | **66.608** | **1.877 / 2.243** (n=23) | n/a | `✓ intact` `a7e0206e…`, `worker_sha1=null` |
| **A3** | A: relay + unfixed | CLIENT-WEST | `MODE-CONFIRMED relay` badge `● Connected (Relay)` | 07:30:01.556443 → 07:31:57.506382 | 115.950 | **54.550** | 07:31:57.506382 | 07:32:00.728 | **3.222** | **1.686 / 1.726** (n=23) | n/a | `✓ intact` `a7e0206e…`, `worker_sha1=null` |
| **B2** | B: relay + E29 fix (`?fix=1`) | CLIENT-WEST | `MODE-CONFIRMED relay` badge `● Connected (Relay)` | 07:32:34.230776 → 07:33:02.850369 | 28.620 | **221.003** | 07:33:02.850369 | 07:33:03.477 | **0.627** | **1.365 / 1.447** (n=5) | n/a | `✓ intact` `a7e0206e…`, `worker_sha1=a7e0206e…` |
| **C2** | C: relay + discard sink (`?sink=discard`) | CLIENT-WEST | `MODE-CONFIRMED relay` badge `● Connected (Relay)` | 07:33:41.369001 → 07:34:08.644163 | 27.275 | **231.896** | 07:34:08.644163 | 07:34:08.921 | **0.277** | **1.027 / 1.056** (n=5) | **790,626,304 / 790,626,304** | `✓ done` |
| **E1** | **E (extra): direct + E29 fix** | CLIENT-EAST | `MODE-CONFIRMED direct` badge `● Connected (Direct)` (attempt 1 aborted, see below) | 07:35:30.737030 → 07:36:13.161264 | 42.424 | **149.090** | 07:36:13.161264 | 07:36:13.905 | **0.744** | **1.035 / 1.273** (n=16) | n/a | `✓ intact` `a7e0206e…`, `worker_sha1=a7e0206e…` |

### Cell A3 detail (appended immediately after the cell)

- First CLIENT-WEST cell (~62 ms RTT to TESTBOX, ~71 ms to VERSA). `MODE-CONFIRMED relay`, click 07:30:01.8Z,
  terminal `✓ intact` 07:32:00.728Z, `worker_sha1=null`.
- Agent: `relaychannel: handshake complete` 07:30:01.556443 → `download complete (count: 9)` 07:31:57.506382;
  **0 `DataChannel lanes ready`**. UI was a flat **↓ 6.1 MB/s**.
- **54.550 Mbps reproduces E12's WEST relay mean (55.306) to within 1.4 %** — the only arm whose absolute number
  today matches E12, which is itself evidence for a fixed client-side sink ceiling rather than a path property:
  EAST (124.8 s, 50.7) and WEST (115.9 s, 54.5) differ by 7 % despite a 50 ms RTT difference, because both are
  pinned to the same ~5.9–6.1 MB/s client drain rate.

### Cell A1 detail (appended immediately after the first cell)

- Pre-cell: both clients `node=0 chrome=0 Xvfb=0`; agent log quiet since 07:16:43; shares `relay_only=true` counter 4
  (relay) / 105 (direct); v1 rig `https`=200.
- Driver: joined, `MODE OK want=relay badge="● Connected (Relay)"`, clicked 07:19:19.389Z, terminal `✓ intact` at
  07:21:28.501Z, `ui_sha1=a7e0206e573edbef0c4d8107a151271fbeccf2fe` (the share's expected hash), `worker_sha1=null` —
  the positive control that the **unfixed** client was the one measured.
- Agent log in the window: `relay_prepare received`, `relaychannel: connected to relay`, `handshake complete`
  (07:19:18.759479), `relay_prepare: relay channel started successfully`, then `download complete for session
  <relay> (count: 5)`. **Zero `DataChannel lanes ready`** → mode is genuinely relay, exactly E12's metric and
  E12's window boundaries. Mid-transfer: VERSA `docker stats sb-run` = **20.41 % CPU**, 28.31 MiB; CLIENT-EAST
  `vmstat 1 2` last row `us 25 / sy 30 / id 43 / wa 2`. Client-visible total (click→terminal) **129.14 s**.

### Cell A2 detail (appended immediately after the cell)

- Identical arm to A1 (unfixed client, relay share, one tab): clicked ~07:24:45.6Z, terminal `✓ intact`
  07:26:56.645Z, `worker_sha1=null` again (positive control).
- Agent: `relaychannel: handshake complete` 07:24:45.494255 → `download complete (count: 8)` 07:26:53.228120;
  **0 `DataChannel lanes ready`**. UI progress was a flat **↓ 5.8 MB/s** for the whole transfer, exactly as in A1.
- **EAST Arm A (n=2) = 50.684, 49.517 → mean 50.100 Mbps** (mean window 126.263 s, mean tail 4.18 s, mean
  renderer 1.656 cores). E12's EAST relay mean was 41.989; today's unfixed relay is **1.19×** that — cross-run
  drift, not a rig difference (same share, same client, same metric).

### Cell B1 detail (appended immediately after the cell)

- Pre-cell: clients clean, agent quiet, relay share counter 5. TESTBOX armed `fix` (hashes above), `http_app`=200.
- Driver: `MODE-CONFIRMED B1 expected=relay badge="● Connected (Relay)"`, clicked 07:22:00.497Z, terminal `✓ intact`
  07:22:28.545Z with `ui_sha1 = worker_sha1 = a7e0206e573edbef0c4d8107a151271fbeccf2fe` — the fix engaged **and** the
  worker hashed the identical byte stream the UI verified (no `FIX-NOT-ENGAGED`, no `HASH-DISAGREE`).
- Agent log: `relay-only session … skipping direct WebRTC peer`, `relay_prepare`, `relaychannel: connected`,
  `handshake complete` (07:22:00.312240), `relay channel started successfully`, `download complete (count: 6)` at
  07:22:27.976948. **Zero `DataChannel lanes ready`** in the window (verified by `grep -ci` = 0).
- Mid-cell: UI progress showed a sustained **↓ 22–27 MB/s** (agent-side 28.58 MB/s). Client-visible total 28.07 s.

### Cell C1 detail (appended immediately after the cell)

- TESTBOX armed `discard` (`app.js` = `329551ce…`; `downloadSinks.js` back to `5c7ff322…`; `hashWorker.js` absent).
- Driver: `MODE-CONFIRMED relay`, clicked 07:23:41.691Z, terminal `✓ done` at 07:24:09.468Z with the discard sink's
  independent byte count `sink_bytes=790626304` = `sink_expected=790626304` — **every byte accounted for**, exactly the
  E12 numerator. No SHA-1 is computed in this arm by construction, so `✓ done` (not `✓ intact`) is the correct terminal.
- Agent log: `relaychannel: handshake complete` 07:23:41.504042 → `download complete (count: 7)` 07:24:09.250457;
  **zero `DataChannel lanes ready`**.
- **This is the deciding cell.** C1 = **227.958 Mbps** vs A1 = **50.684 Mbps** on the same share, same client, same
  hour, both asserted relay — **4.50×**.

### Cell D1 detail (appended immediately after the cell)

- Arm D is the **direct** reference (normal share, no `relay_only`): driver `MODE-CONFIRMED D1 expected=direct
  badge="● Connected (Direct)"`, clicked 07:27:33.845Z, terminal `✓ intact` 07:29:32.451Z.
- Mode assertion inverted as required: the agent log shows **`DataChannel lanes ready for peer …`
  07:27:32.746923** (and **no** `relaychannel: handshake complete` in the window) → this cell is genuinely direct.
- Agent window 53.096 s → **119.124 Mbps**, with a **66.608 s** client tail — i.e. the same unfixed client that
  consumed the relay at ~5.9 MB/s kept receiving into SCTP/DataChannel buffers fast (13.4 MB/s) and then needed a
  further 67 s to drain its own sink.
- **The D1-vs-A comparison is the mechanism in one line:** the unfixed client's ceiling is ~5.9 MB/s (47 Mbps) of
  *sink* throughput. In **relay** that back-pressure throttles the sender for the whole transfer (49.5–50.7 Mbps,
  ~4 s tail); in **direct** the transport buffers let the sender run ahead (119 Mbps) and the same client work shows
  up as a **67 s tail** instead. Same client cost, two different symptoms — exactly what E29/E30 saw on this client.

### Cell B2 detail (appended immediately after the cell)

- TESTBOX re-armed `fix` (hashes re-verified: `ca4fdb51…` / `72f28ed0…` / `77a10595…`). CLIENT-WEST, relay share,
  `?fix=1`, `FIX_EXPECTED=1`. Driver `MODE-CONFIRMED relay`, terminal `✓ intact` 07:33:03.477Z with
  `ui_sha1 = worker_sha1 = a7e0206e573edbef0c4d8107a151271fbeccf2fe` (fix engaged, no hash disagreement).
- Agent: `relaychannel: handshake complete` 07:32:34.230776 → `download complete (count: 10)` 07:33:02.850369;
  **0 `DataChannel lanes ready`**. Client-visible total 28.94 s.

### Cell C2 detail (appended immediately after the cell)

- TESTBOX armed `discard` (`app.js` = `329551ce…`, `hashWorker.js` absent). CLIENT-WEST, relay share, `?sink=discard`.
- Driver `MODE-CONFIRMED relay`, terminal `✓ done` 07:34:08.921Z, `sink_bytes=790626304` = `sink_expected=790626304`
  — every byte accounted for. Agent: `handshake complete` 07:33:41.369001 → `download complete (count: 11)`
  07:34:08.644163; **0 `DataChannel lanes ready`**.

### Cell E1 detail (extra arm, appended after the cell)

- **Attempt 1 (07:34:35Z) was aborted, not a measurement.** The *direct* share silently fell back to relay after
  join: `MODE-ABORT E1 wanted direct got badge="● Connected (Relay)"`; the agent logged
  `relaychannel: handshake complete` at 07:34:47.705 and **no `DataChannel lanes ready`**. The driver exited 3
  before clicking, no bytes moved, and the direct share's counter stayed at 106. This is the documented silent
  fallback that reads ~44.7 Mbps and looks valid — the mode gate caught it. Retry at 07:35:28Z was clean.
- Retry: `MODE-CONFIRMED direct`, clicked 07:35:30.9Z, terminal `✓ intact` 07:36:13.905Z,
  `ui_sha1 = worker_sha1 = a7e0206e…`. Agent: `DataChannel lanes ready` 07:35:30.737030 → `download complete
  (count: 107)` 07:36:13.161264; **no relay handshake in the window**. Window 42.424 s → **149.090 Mbps**.
- Extra arm, not requested: it provides the same-session fixed-client **direct** reference that makes
  "relay-with-fix is faster than direct-with-fix" a same-hour comparison rather than a cross-experiment one.

### Arm summaries (same-session, E12 metric)

| arm | cells | agent Mbps | mean | min–max | mean tail s | mean renderer cores |
|---|---|---|---|---|---|---|
| **A** relay + unfixed | A1,A2 (EAST), A3 (WEST) | 50.684, 49.517, 54.550 | **51.58** | 49.52–54.55 | 3.86 | 1.666 |
| **B** relay + E29 fix | B1 (EAST), B2 (WEST) | 228.631, 221.003 | **224.82** | 221.00–228.63 | 0.598 | 1.397 |
| **C** relay + discard sink | C1 (EAST), C2 (WEST) | 227.958, 231.896 | **229.93** | 227.96–231.90 | 0.248 | 1.060 |
| **D** direct + unfixed (reference) | D1 (EAST) | 119.124 | 119.12 (n=1) | — | 66.61 | 1.877 |
| **E** direct + E29 fix (extra) | E1 (EAST) | 149.090 | 149.09 (n=1) | — | 0.744 | 1.035 |
| path control (`iperf3` VERSA→EAST TCP, no cell) | — | 235–239 | — | — | — | — |

**Deciding number: Arm C / Arm A = 229.93 / 51.58 = 4.46×** (EAST-only pairing C1/A1 = 4.50×; the arms were
interleaved within each client). Arm C kept the relay channel, its WebSocket framing and its per-frame JS Noise
decryption fully intact — only the sink was replaced by a byte counter.

## Interpretation

### 1. The deciding comparison — Arm C vs Arm A

| pairing | unfixed (A) | sink bypassed, Noise intact (C) | ratio |
|---|---:|---:|---:|
| CLIENT-EAST, same share, same hour (A1 → C1) | 50.684 Mbps | **227.958 Mbps** | **4.50×** |
| CLIENT-WEST (A3 → C2) | 54.550 Mbps | **231.896 Mbps** | **4.25×** |
| arm means | **51.58** (n=3) | **229.93** (n=2) | **4.46×** |

Arm C is the cleanest possible isolation of the *sink* from the *relay path*: the browser still joins the relay,
still runs the Noise XX handshake, still decrypts every frame in JavaScript through `secureRelayChannel.js`, still
frames/unframes each WebSocket message, still drives the pipeline, still updates the UI — and it still accounted for
every byte (`sink_bytes = sink_expected = 790,626,304` in both cells). Only the concat/StreamSaver/SHA-1 work was
removed. Removing it multiplies the relay's agent-side rate **4.46×**. The relay's own per-frame JavaScript cost is
thus nowhere near the 42/55 ceiling; the sink was.

### 2. Why E12's 42/55 looked latency-independent — it is a client constant

The unfixed client drains its sink at **~5.8 MB/s on CLIENT-EAST and ~6.1 MB/s on CLIENT-WEST** in *relay* mode,
and at ~5.9 MB/s in *direct* mode (D1's UI rate). E12's two relay numbers (41.989 EAST / 55.306 WEST) are 5.25 and
6.91 MB/s — the same band. Today the two clients differ by only **7 %** despite a ~50 ms RTT difference. That is the
signature of a fixed client-side throughput ceiling, not of a path or of relay design: F22's "latency-independent"
observation is right, but the cause is the **main-thread sink**, not the Noise decryption alongside it.

The direct-mode control shows the same client cost wearing a different mask: with the same unfixed client, **direct**
ran at 119.124 Mbps agent-side with a **66.6 s** tail (D1) while **relay** ran at ~50 Mbps with a ~4 s tail (A1/A2).
SCTP/DataChannel buffering lets the sender run ahead of the sink, so in direct mode the client's ~5.9 MB/s ceiling
turns into a tail; in relay mode the WebSocket flow-control feeds that ceiling straight back to the agent. Same
ceiling, two symptoms — exactly E29/E30's reading of this client.

### 3. What the recommended fix actually delivers for relay users

| | unfixed (A) | E29 fix (B) | gain |
|---|---:|---:|---:|
| agent-side Mbps (mean) | **51.58** (n=3) | **224.82** (n=2) | **4.36×** |
| mean tail | **3.86 s** | **0.598 s** | **6.5× shorter** |
| mean renderer cores | 1.666 | 1.397 | 16 % less CPU for 4.4× the rate |
| mean client-visible total | ~126 s | ~28.5 s | **4.4×** |

For a relay user the fix is not a marginal CPU win: it is the difference between the slowest v1 transport and the
fastest one. Both B cells verified `✓ intact` with `worker_sha1 = ui_sha1` (the worker hashed the identical stream),
so the 4.36× costs nothing in verification fidelity. Arm B (224.82) sits just below Arm C (229.93): **the fix recovers
~98 % of the sink-free ceiling**, the residual being the SHA-1 worker plus the fixed sink's 1 MiB flush path.

### 4. Where the relay actually now sits: at the machine's ceiling, and above v1 direct

- Same-session path control (no cell running): `iperf3` VERSA → CLIENT-EAST TCP = **235–239 Mbps** (E30 measured
  241–246 on the same path). Arm C (231.9 WEST) and Arm B (228.6 EAST) therefore run at **~96–99 % of the measured
  client-ingress capacity**; the client VMs' ingress cap is documented as ~240 Mbps.
- Arm E (extra, not requested): **direct + the same fix, CLIENT-EAST, 149.090 Mbps** (`DataChannel lanes ready`, zero
  relay handshake; tail 0.744 s, renderer 1.035 cores). It reproduces E29's 146.5 Mbps well.
- So **relay + fix (228.6) = 1.53× direct + fix (149.1)** on the same client, same hour, same payload, with modes
  asserted both ways. That is consistent with E30's finding that direct mode is bound by the browser's
  DataChannel/SCTP receive path: the relay delivers over a WebSocket, which is a cheap receive path, and its
  per-frame JS Noise decrypt is much cheaper than the SCTP receive cost it avoids.

### 5. Verdict on the user's belief

**The user's belief is right: v1 relay runs at essentially line speed once the client is fixed.** The 42/55 Mbps in
E12 (and reproduced today as 49.5–54.6 Mbps) is a *measurement through a throttled client*, not a property of the
relay transport. This directly resolves the campaign's 2×2 matrix correction in F22 Consequence 3: v1 relay
"measured but client-throttled (42.0/55.3)" should now read **measured, client-throttled, and ~230 Mbps
(~4.5×) once the client's sink is fixed** — i.e. comparable to v2 relay (233 EAST / 215–228 WEST) rather than to
nothing. The remaining v1-vs-v2 question is no longer "is the relay slow?" but "does 230 Mbps, bounded by each
client's ingress cap, suffice?".

## Caveats

1. **Small n, not randomized.** A n=3 (A1,A2 EAST + A3 WEST), B n=2 (one per client), C n=2 (one per client),
   D n=1, E n=1. Within each client the arms ran in the order A → B → C (→ D/E), so the EAST sequence is
   A1,B1,C1,A2,D1 (unfixed/fix/discard/unfixed/unfixed) and the WEST sequence A3,B2,C2 — interleaved but
   monotonically drifting in time, and the 4.46× is far larger than the within-arm spread (A: 1.10×,
   B: 1.03×, C: 1.02×), so drift cannot explain it.
2. **Arm C is a diagnostic, not a shippable fix.** It removes the concatenation, SHA-1 and the StreamSaver writer,
   and validates **only the byte count** of 790,626,304 B; the terminal state is `✓ done`, not `✓ intact`. It
   isolates the sink's cost; it does not establish that a checksum-free client is acceptable. It also still runs the
   receive pipeline, the relay channel, the Noise decryption, the per-frame dispatch and the UI updates.
3. **E29's exact bytes could not be reused.** TESTBOX rebooted at 07:11Z (it wipes `/tmp`, including E29's
   `/tmp/exp29/` outputs), and the E29 results file reproduces its diff in abridged form (its `hashWorker.js` block
   is 672 B where the deployed file was 1,069 B). Arm B is therefore a **reconstruction from E29's documented
   diff**, hash-recorded above (`ca4fdb51…` / `72f28ed0…` / `77a10595…`) but **not byte-identical** to E29's
   `cc4c944c…` / `db69d58d…` / `10f9dc0e…`. Equivalence evidence: the same three edits, `node --check` clean,
   every `?fix=1` cell reported `FIX_EXPECTED`, no `FIX-NOT-ENGAGED`, no `HASH-DISAGREE`, and
   `worker_sha1 = ui_sha1 = a7e0206e…` in B1, B2 and E1.
4. **Absolute rates are not comparable across nights** (E30's own finding). Today's Arm A means are 50.100
   Mbps EAST (E12: 41.989, **+19 %**) and 54.550 WEST (E12: 55.306, **−1.4 %**). Every load-bearing comparison in
   this file is **within-session**; the E12 comparison is descriptive.
5. **The path control is a separate probe, not a simultaneous protocol-identical control**, and only one direction
   was measured (VERSA → CLIENT-EAST TCP, `iperf3`, no cell running). TESTBOX has no `iperf3` installed, so the
   relay's second hop (TESTBOX → client) was not measured directly. The ~240 Mbps figure quoted for the client VMs'
   ingress cap comes from the infra notes, not from today's measurement.
6. **Renderer-core means are coarse where the cell is short.** B1/B2/C1/C2/E1 are ~28–43 s transfers, so their
   sampler means rest on n=5 (B/C) and n=16 (E) 5 s deltas; A1/A2/A3/D1 rest on n=23–25. The cores claim is used
   only qualitatively ("more bytes/sec on fewer cores"), never as a precise ratio.
7. **The agent log's `download complete` does not print a byte count** (an E12 limitation, unchanged). The
   numerator is the known source size. Arm C supplies the independent client-side byte count (790,626,304 exactly);
   arms A/B/D/E rely on the client's own `✓ intact` SHA-1 verification for completeness (A/B/D/E all showed the
   expected hash, so all 790,626,304 bytes were received and hashed).
8. **The relay measured here is v1's WebSocket/JS-Noise relay only.** Nothing in this experiment measures, changes
   or interprets the v2 relay, which is a different transport (HTTP/FRP).
9. **Arm E1 attempt 1 exposed the known silent-fallback trap** (direct share joined as Relay; aborted by the mode
   gate, no bytes moved, share counter unchanged at 106) — reported rather than discarded, and it is why every cell
   in this file carries an asserted mode.

## Restore

**TESTBOX web root restored byte-for-byte and checksum-verified at 07:38Z.**

- `src/app.js` back to `407ee7032f90e4298ee6b0416ed69dbf94147d0c6ff0933e7c6803f129bdf3c1` (91,308 B, root:root 0644),
  `src/downloadSinks.js` back to `5c7ff32294ab617e4cd9d37d7406ef10362b167422d3471dd406a12b4edfc3da`, and
  `src/hashWorker.js` **absent**.
- Both `cmp` checks against `/tmp/exp31/backup/src/` returned byte-identical, and a full
  `find . -type f -exec sha256sum {} \; | sort` manifest of the web root is **identical to the pre-change manifest**
  (140 files): `WEBROOT-RESTORED-MANIFEST-IDENTICAL`.
- A fresh HTTP retrieval **from CLIENT-EAST through the real origin** returned `app.js` = `407ee703…` and
  `hashWorker.js` = **404** (`app=200`), so the served client really is the original again.
- **Clients clean:** `pkill -x`/`-9 -x` only, one at a time. CLIENT-EAST and CLIENT-WEST both report
  `node=0 chrome=0 Xvfb=0 iperf3=0`; the temporary `~/sbtest/exp31-drive.js`, `~/sbtest/exp31-launch.sh` and the
  sampler copy placed on CLIENT-WEST were removed (EAST's pre-existing `exp29-*` lab files were left as found).
- **TESTBOX services:** `sharebridge-test` and `caddy` `active`, `http://127.0.0.1:8080/` = 200 and the public
  origin = 200. `sharebridge`, `sharebridge-relay-gateway` and `sharebridge-relay-frps` remain **stopped** (they were
  stopped at 07:19Z to make the v1 rig possible — see "Rig state change"; restart them for phase-4a work).
- **VERSA:** `sb-run` left **running** (`sb-agent:pristine`, `NanoCpus=0`, variant `c`) — the orchestrator owns
  teardown; production `sharebridge-agent` (7878), every other VERSA container and `sharebridge-agent-test` were never
  touched. Agent log quiet after `download complete (count: 107)`; share counters ended at relay 4 → 11 and direct
  105 → 107.
- **No `git` command was run, no instance was shelved/stopped/resized, and no repo file other than this results file
  was written.**

## Artifacts

- TESTBOX: `/tmp/exp31/` (`out/{app.fix,app.discard,downloadSinks.fixed,hashWorker}.js`, `set-arm.sh`,
  `backup/src/`), `/tmp/exp31-web-manifest-{pre,post}.txt`.
- Clients: `~/sbtest/exp31-{drive,launch}.js|sh`, per-cell `/tmp/exp31-<cell>.log` and `/tmp/exp31-<cell>.proc`.
- VERSA: timestamped `docker logs sb-run` windows (`--since` per cell). Share codes are omitted from this file.
