# Experiment 32 — does v1 direct's throughput survive realistic latency? (CLIENT-WEST, ~71 ms) — 2026-09-19

**Status:** COMPLETE — written incrementally after every cell (RUNBOOK §1.13).

**Verdict: NO — v1 direct does not survive realistic latency.** With the identical E29-fixed client, same payload,
same hour and asserted direct mode, CLIENT-EAST (~10.9 ms) delivered **137.71 Mbps = 17.21 MB/s (n=2)** while
CLIENT-WEST (~70.4 ms) delivered **74.71 Mbps = 9.34 MB/s (n=3)** — a **1.84×**
penalty, non-overlapping ranges (136.8–138.6 vs 71.6–77.4). The ~15–19 MB/s figure therefore holds **only** at
~12 ms; the right expectation for a distant user on v1 direct is **~9 MB/s**. The cause is **not** the app or the
sink: replacing the entire receive path with E28's bare byte counter left WEST at **77.36 Mbps (n=1)** —
statistically the same as the fixed client (1.04×) on **3.1× less renderer CPU** (0.337 vs 0.623 cores) — so the
residual ~75 Mbps is the **browser's DataChannel/SCTP receive path at high RTT**, not CPU exhaustion (the client
never exceeded 1.92 of 4 cores in any WEST arm). E10b's 60.24 Mbps was **almost entirely the latency, not the
client**: today's unfixed WEST control is **64.84 Mbps (n=2) = 1.08× E10b**, and the E29 fix buys only **1.15×**
at 70 ms (vs 1.10–1.27× at 12 ms) plus the tail (22.5 s → 0.6 s). The path is not the limiter (VERSA→WEST UDP
249 Mbps, TCP 4-flow 245 Mbps, single-flow TCP 111–163 Mbps; VERSA→EAST 245/249). **Deciding comparison:**
EAST(fixed) 137.71 vs WEST(fixed) 74.71 = **1.84×**, while E31's relay on the same client loses only ~3 % from
12 ms (228.6) to 71 ms (221.0) — so at ~70 ms the **relay is ~3× faster than direct (221.0 vs 74.7)** and is the
path for distant users. **Expectation:** ~17 MB/s direct only for near users; ~9 MB/s direct or ~28 MB/s relay
for distant ones.

**Question.** Every v1-direct measurement in this campaign so far comes from CLIENT-EAST at ~12 ms (E29 146.54,
E30 119.17 n=4 / 107.5–130.9, E31 E1 149.09) — a best-case path. Real users are on mobile and distant
networks. Does the ~15–19 MB/s (119–149 Mbps) figure survive ~71 ms?

**Design (per task):** normal direct shares (`relay_only=false`), same 754 MiB source
(**790,626,304 B = 6,325.010432 Mb**), one tab per cell, clients clean before every cell.
- **EAST, direct + fixed client** — n=2 (same-session ~12 ms reference).
- **WEST, direct + fixed client** — n=2. **The cell that answers the question.**
- **WEST, direct + unfixed client** — n=2 (control vs E10b's 60.24 Mbps).
- **WEST, direct + bare counting handler** — n=1 if time allows.
- **Path controls** — `iperf3` VERSA → each client (TCP single flow + UDP single flow), no cell running.
- **Per cell:** measured RTT (5-ping client → VERSA), agent-side Mbps (`DataChannel lanes ready` →
  `download complete`), tail, user-visible time, renderer cores. Reported in Mbps **and** MB/s.

## Part 0 — rig state

### Rig state change at 18:57Z (same v2 collision as E31; stop/start only)

The unshelve brought up the **phase-4a/v2 stack on TESTBOX** (`sharebridge-relay-gateway` and
`sharebridge.service` `active`+`enabled`), which makes the v1 rig impossible. Verified before acting:

- `sharebridge-relay-gateway` **active/enabled**, owns `*:443` (pid `gateway`) → Caddy cannot serve the v1
  share page or the agent's `wss://` endpoint.
- `sharebridge.service` **active/enabled**, owns `*:8080` (pid `server`) → the v1 signalling server's port.
- `sharebridge-test` and `caddy` both **inactive/disabled**; `curl https://TESTBOX/` = **000**;
  `http://127.0.0.1:8080/` = 200 is the **v2 control plane**, not the v1 server.

Action taken (no `systemctl disable`, no file edits, no `git` — stop/start only):

```
systemctl stop  sharebridge-relay-gateway sharebridge-relay-frps sharebridge.service
systemctl start sharebridge-test caddy
```

Verification after the action (18:58Z): `sharebridge-relay-gateway` / `sharebridge-relay-frps` / `sharebridge.service`
**inactive (still enabled)**, `sharebridge-test` + `caddy` **active**; `:443`/`:80` owned by `caddy`, `:8080` by
the v1 `server`; public origin = **200**, `/src/app.js` = **200**. The three v2 units are enabled but stopped —
the orchestrator can restart them for phase-4a work (`systemctl start sharebridge-relay-gateway
sharebridge-relay-frps sharebridge.service`). No `disable`, no file edit, no `git`, no instance state change.

### Agent (VERSA) — 18:57Z

`sb-run` was `Exited (2)` from the previous boot; started with the canonical
`cd ~/sharebridge-test && HOME=/home/ali bash run-variant.sh c` → `variant=c img=pristine ca=32768 auth=1
sessions=1`, `running`, `NanoCpus=0`, `Restart=unless-stopped`, `UI_PORT=7879`, `UI_ADDR=127.0.0.1`,
`SB_SCTP_CA_STEP=32768`, no `SB_SCTP_MIN_CWND`. Connected to the v1 signalling server
(`connected to signaling server at wss://TESTBOX`, `agent authenticated`, `session reconnected` ×2,
`loaded 2 sessions`), API **200** with the key. Production `sharebridge-agent` (7878) and every other VERSA
container untouched; `sharebridge-agent-test` left stopped.

### Payload and shares (reused, no new share)

- **direct share** = `relay_only=false`, downloads **107** at start, expires 2026-09-20T04:49:25Z — the same
  E28/E29/E30/E31-D share (`public_url https://TESTBOX/s/<direct code>`).
- relay share exists (`relay_only=true`, downloads 11) and is **not** used in any cell of this experiment.
- One 754 MiB source: **790,626,304 B = 6,325.010432 Mb** per download.

### Metric, tail, driver

- **Agent-side metric (E12/E30/E31 verbatim):** `docker logs --timestamps sb-run`, window = first
  `DataChannel lanes ready` → last `download complete`; Mbps = 6,325.010432 ÷ window_s. **Never** the
  Playwright Download API.
- **Tail:** client terminal UI moment (`.file-item.verified` + `✓ intact`, or the arm's own terminal) minus
  the agent's `download complete` timestamp. CLIENT-EAST↔TESTBOX skew ≈1 s (E29, cross-night); the
  CLIENT-WEST↔TESTBOX **clock** skew was **not** calibrated today, so sub-second tails are ±1 s and only the
  unfixed arms' 21–24 s tails are asserted tightly (the WEST *network* RTT was measured — 70.4 ms to VERSA,
  62.2 ms to TESTBOX).
- **Renderer cores:** 5 s `/proc/<pid>/stat` delta sampler (`exp32-sampler.py`, E28/E29 lineage), role-classified
  from cmdline; a Chrome Web Worker runs in the renderer process, so `renderer` includes the SHA-1 worker.
- **Driver:** `exp32-drive.js` (click-only; derived from `exp29-drive_verify.js` + `drive_bare.js`) — asserts the
  **expected** badge **before** the click (`MODE_EXPECT=direct`, prints `MODE-CONFIRMED`, exits 3 with
  `MODE-ABORT` on a mismatch); `ARM=fix` reads the worker SHA-1 (`globalThis.__exp29`, `FIX-NOT-ENGAGED` /
  `HASH-DISAGREE` guards); `ARM=discard` reads the discard sink's byte count (`globalThis.__exp31`);
  `ARM=bare` reads the EXP28 counter (`globalThis.__bare`) and asserts `wire − 14×frames = 790,626,304`.
- **Mode assertion per cell:** `DataChannel lanes ready` present, `MODE-CONFIRMED direct` badge, and **no**
  relay handshake in the window. A silent relay fallback reads ~44.7 Mbps and looks valid (E31 caught one).

### Client arming (TESTBOX test web root only)

Pre-change web root manifest: 140 files (`/tmp/exp32-web-manifest-pre.txt`), `src/app.js` =
`407ee7032f90e4298ee6b0416ed69dbf94147d0c6ff0933e7c6803f129bdf3c1` (91,308 B), `src/downloadSinks.js` =
`5c7ff32294ab617e4cd9d37d7406ef10362b167422d3471dd406a12b4edfc3da`, `src/hashWorker.js` **absent** —
byte-identical to the E15/E25/E27/E28/E29/E30/E31 restored state and to the repo copy. Full web-root tarball
backup `/tmp/exp32-webroot.tgz` + `backup/src/` (verified byte-identical by `cmp`). Switch with
`/tmp/exp32/set-arm.sh <unfixed|fix|discard|bare>`.

| arm | deployed `app.js` sha256 (size) | `downloadSinks.js` | `hashWorker.js` |
|---|---|---|---|
| unfixed | `407ee703…` (91,308 B) | `5c7ff322…` | absent |
| fix | `e650a95097fd29e5c1fd030b27aed50a2e529886280889bcab14cc50b9929c61` (91,784 B) | `72f28ed00a8b9f6fed04102b8a23c4895509a6691a0f0e627cfca858962fe105` (10,919 B) | `77a105958f4f8a2df0254ab12a95e686c9f6389c433c1e0fabb3e16894ba98b9` (672 B) |
| discard | `329551ce0ffb6aeca1f1e065370cf1b86084bb057dbdf78d7d0d82f501a10e64` (92,231 B) | `5c7ff322…` | absent |
| bare | `b7a18b9a24dd022d74b4c5658734a6c914b471f6303ad084522938fe126c795c` (92,372 B) | `5c7ff322…` | absent |

Reconstruction provenance (reported honestly): `downloadSinks.fixed.js` and `hashWorker.js` are **byte-identical**
to E31's deployed arm (`72f28ed0…`, `77a10595…` — E31's own hashes). The **discard** `app.js` is **byte-identical**
to E31's deployed discard arm (`329551ce…`; E31 recorded 92,207 B, my file is 92,231 B — the hash, not the size,
is authoritative and it matches exactly). The **fix** `app.js` is a fresh reconstruction from E29's documented
three edits; it is the same **size** as E31's (91,784 B) but the hash **differs** (`e650a950…` vs E31's
`ca4fdb51…`), so E31's exact byte layout was not reproduced — behaviour is gated in-cell instead
(`FIX-NOT-ENGAGED` / `HASH-DISAGREE` / `worker_sha1 = ui_sha1`). The **bare** `app.js` is a fresh reconstruction
of E28's documented three edits: E28's own patched file was 93,008 B while this reconstruction is 92,372 B
(637 B smaller), so E28's file contained further bytes its abridged diff did not show; the reconstruction is
behaviour-equivalent by construction and is gated in-cell by `NO-BARE-COUNTER` + `BARE-ACCOUNTING match=true`.
All four arms pass `node --check`.

Served-from-CLIENT-EAST verification with the fix armed: `app.js` → `e650a950…`, `downloadSinks.js` →
`72f28ed0…`, `hashWorker.js` → `77a10595…` (all HTTP 200 **through the real origin**), i.e. the browser really
exercises the patched bytes.

### Measured RTT (5-ping average, client → VERSA; not assumed)

| client | → VERSA (avg of 5, 0 % loss) | → TESTBOX (avg of 5, 0 % loss) |
|---|---:|---:|
| CLIENT-EAST | **10.945 ms** (min 9.868, max 12.803) | 0.603 ms |
| CLIENT-WEST | **70.385 ms** (min 68.832, max 71.905) | 62.239 ms |

### Path controls — `iperf3` VERSA → each client, **no cell running** (19:00–19:02Z)

| client | TCP 1 flow | TCP 4 flows (aggregate) | UDP offered 300 Mbps | UDP received | UDP loss |
|---|---:|---:|---:|---:|---:|
| CLIENT-EAST | **245 Mbps** (0 retr) | — (E30: 242) | 300 Mbps | **249 Mbps** | 17 % (0/258,979 sent-lost) |
| CLIENT-WEST | **163 Mbps** (39 retr); repeat **111 Mbps** | **245 Mbps** (362 retr) | 300 Mbps | **249 Mbps** | 17 % |

**Reading:** both paths can carry **~245–249 Mbps in aggregate** (WEST's 4-flow TCP and UDP both reach the
same 245–249 Mbps cap as EAST), so at the aggregate level WEST's ~70 ms path is **not** the limiter. But a
**single TCP flow** on WEST only achieves **111–163 Mbps** (variable across two runs) versus 245 on EAST — a
per-flow limit at 62–70 ms RTT. The v1 bulk lane is a single SCTP flow, so this per-flow limit is directly
relevant to the interpretation of the WEST cells below.

## Cells

_Appended after every cell. `win` = `DataChannel lanes ready` → `download complete` (UTC). Agent Mbps =
6,325.010432 ÷ win_s. MB/s = Mbps ÷ 8._

| # | arm | client | mode gate | RTT ms | win s | **agent Mbps** | **MB/s** | tail s | user-visible s | renderer cores (mean/peak) | n |
|---|---|---|---|---|---|---|---|---|---|---|---|
| **E1** | direct + **fix** | CLIENT-EAST | `MODE-CONFIRMED expected=direct` "● Connected (Direct)" 19:02:11.380; 0 relay bytes | 10.9 | 45.640 | **138.583** | **17.323** | 0.498 | 45.53 | 1.041 / 1.316 (n=9) | 1 |
| **W1** | direct + **fix** | CLIENT-WEST | `MODE-CONFIRMED expected=direct` 19:03:37.548; 0 relay bytes | 70.4 | 88.307 | **71.625** | **8.953** | 0.434 | 88.34 | 0.618 / 0.783 (n=17) | 1 |
| **E2** | direct + **fix** | CLIENT-EAST | `MODE-CONFIRMED expected=direct` 19:06:02.7; 0 relay bytes | 10.9 | 46.221 | **136.840** | **17.105** | 0.428 | 46.08 | 0.977 / 1.300 (n=9) | 1 |
| **W2** | direct + **fix** | CLIENT-WEST | `MODE-CONFIRMED expected=direct`; 0 relay bytes | 70.4 | 81.717 | **77.401** | **9.675** | 0.983 | 82.24 | 0.652 / 0.789 (n=16) | 1 |
| **W3** | direct + **unfixed** | CLIENT-WEST | `MODE-CONFIRMED expected=direct` 19:09:33.038; 0 relay bytes | 70.4 | 97.171 | **65.092** | **8.136** | **23.627** | 120.37 | 1.941 / 2.018 (n=19) | 1 |
| **W4** | direct + **unfixed** | CLIENT-WEST | `MODE-CONFIRMED expected=direct` 19:13:13.3; 0 relay bytes | 70.4 | 97.915 | **64.597** | **8.075** | **21.323** | 118.84 | 1.896 / 2.054 (n=19) | 1 |
| **W5** | direct + **bare counter** | CLIENT-WEST | `MODE-CONFIRMED expected=direct`; 0 relay bytes; `BARE-ACCOUNTING match=true` | 70.4 | 81.758 | **77.363** | **9.670** | 1.435 (see note) | 82.81 | 0.337 / 0.477 (n=16) | 1 |
| **W6** | direct + **fix** (3rd cell, + agent CPU) | CLIENT-WEST | `MODE-CONFIRMED expected=direct`; 0 relay bytes | 70.4 | 84.229 | **75.093** | **9.387** | 0.381 | 84.18 | 0.600 / 0.831 (n=16) | 1 |

### Cell E1 detail (appended immediately after the cell)

- Pre-cell: both clients `node=0 chrome=0 Xvfb=0`; agent log quiet since boot; direct share counter 107; TESTBOX
  armed `fix` (served hashes verified from CLIENT-EAST).
- Driver: `MODE-CONFIRMED E1 expected=direct badge="● Connected (Direct)"` at 19:02:11.380, clicked 19:02:11.4Z,
  terminal `✓ intact` at 19:02:56.914Z with `ui_sha1 = worker_sha1 = a7e0206e573edbef0c4d8107a151271fbeccf2fe`
  (fix engaged, no `FIX-NOT-ENGAGED`, no `HASH-DISAGREE`).
- Agent: **`DataChannel lanes ready`** 19:02:10.775868314 → `download complete (count: 108)` 19:02:56.416309787
  → window **45.640 s** → **138.583 Mbps = 17.323 MB/s**. The relay standby was connected at join and torn down
  **unused** at 19:02:55.107 (`pending wait window exceeded`, no relay bytes) → genuinely direct.
- UI showed a sustained **↓ 17.0 MB/s**; client-visible total (click → terminal) **45.53 s**.
- Renderer 1.041 / 1.316 cores (n=9 rows, the sampler's pre-transfer warm-up row excluded); client total
  1.686 / 2.326.
- **E1 reproduces E30's fixed-client EAST arm (119.17 n=4, range 107.5–130.9) at the top of its range and
  E31's E1 (149.09) just below it** — i.e. a normal ~12 ms fixed-client night.

### Cell W1 detail (appended immediately after the cell) — **the cell that answers the question**

- Pre-cell: both clients clean; TESTBOX still armed `fix`; share counter 108.
- Driver: `MODE-CONFIRMED W1 expected=direct badge="● Connected (Direct)"` at 19:03:37.548, clicked 19:03:37.6Z,
  terminal `✓ intact` at 19:05:05.890Z with `ui_sha1 = worker_sha1 = a7e0206e…` — the **same fixed client code
  as E1**, verified by the same worker digest.
- Agent: **`DataChannel lanes ready`** 19:03:37.149305320 → `download complete (count: 109)` 19:05:05.456233879
  → window **88.307 s** → **71.625 Mbps = 8.953 MB/s**. Relay standby connected at join, torn down **unused**
  at 19:04:21.083 (`pending wait window exceeded`) → genuinely direct.
- UI showed a sustained **↓ 8.5 MB/s**; client-visible total **88.34 s**.
- Renderer 0.618 / 0.783 cores (n=17) — **less than half the client CPU** E1 used, at half the rate.
- **First observation: WEST(fixed) 71.625 vs EAST(fixed) 138.583 = 1.94× slower at ~70 ms vs ~11 ms**, with the
  identical client build, the identical payload and the same hour.

### Cell E2 detail (appended immediately after the cell)

- Second EAST fixed cell, same arm/served hashes as E1. `MODE-CONFIRMED expected=direct`; agent
  **`DataChannel lanes ready`** 19:06:02.591924367 → `download complete (count: 110)` 19:06:48.812899752 →
  window **46.221 s** → **136.840 Mbps = 17.105 MB/s**; tail 0.428 s; user-visible **46.08 s**; relay standby
  torn down unused 19:06:46.903. `ui_sha1 = worker_sha1 = a7e0206e…` (fix engaged).
- **EAST(fixed) n=2 = 138.583, 136.840 → mean 137.71 Mbps (17.21 MB/s)**, spread 1.3 % — a tight same-session
  reference at ~11 ms.

### Cell W2 detail (appended immediately after the cell)

- Second WEST fixed cell. `MODE-CONFIRMED expected=direct` at 19:07:18.0; agent **`lanes ready`**
  19:07:18.026931656 → `download complete (count: 111)` 19:08:39.743882478 → window **81.717 s** →
  **77.401 Mbps = 9.675 MB/s**; tail **0.983 s**; user-visible **82.24 s**; relay standby unused
  (19:08:02.044). `ui_sha1 = worker_sha1 = a7e0206e…`.
- **WEST(fixed) n=2 = 71.625, 77.401 → mean 74.51 Mbps (9.31 MB/s)**, spread 1.08×.
- **Deciding comparison (n=2 vs n=2, same session, same arm, same payload):**
  **EAST(fixed) 137.71 Mbps (17.21 MB/s) vs WEST(fixed) 74.51 Mbps (9.31 MB/s) = 1.85×** — the ranges do not
  overlap (EAST 136.8–138.6 > WEST 71.6–77.4). (This is the n=2-vs-n=2 snapshot taken at the time of W2;
  W6 added a third WEST cell at 75.093, giving the final **WEST(fixed) n=3 mean 74.71 Mbps = 9.34 MB/s** and
  **1.84×** used in the verdict and arm summary. The two framings agree to 0.01×.)
- **Client CPU separates the two arms cleanly:** WEST(fixed) used **0.62–0.65 renderer cores** (1.21 client
  total) at 9.3 MB/s while EAST(fixed) used **0.98–1.04 renderer cores** (1.69 client total) at 17.2 MB/s. The
  WEST client is doing *less* work per second, not more — so the 1.85× gap is **not** client CPU exhaustion.

### Cell W3 detail (appended immediately after the cell) — unfixed control

- Arm switched to **unfixed** (`set-arm.sh unfixed`; served `app.js` re-verified **from CLIENT-WEST** as
  `407ee703…`, `hashWorker.js` absent). `MODE-CONFIRMED expected=direct`.
- **First W3 attempt (19:09:09Z) was aborted deliberately**, not a measurement: the driver was launched with
  the default `STALL_MS=90000`, which is shorter than the unfixed client's expected post-transfer tail, so it
  was killed ~40 s in (0.5 s of transfer, agent had logged `lanes ready` 19:09:13.594; no `download complete`
  — share counter unchanged at 111). Relaunched at 19:09:29Z with `STALL_MS=300000 MAX_MS=480000`.
- Relaunch: agent **`lanes ready`** 19:09:32.613973116 → `download complete (count: 112)` 19:11:09.785007368 →
  window **97.171 s** → **65.092 Mbps = 8.136 MB/s**; terminal `✓ intact` at **19:11:33.412** → tail
  **23.627 s**; user-visible total **120.37 s**. `worker_sha1=null` — the positive control that the *unfixed*
  client was measured.
- **The unfixed client froze the DOM as predicted:** after the 0.5 s progress line the driver logged nothing for
  120 s (the page could not answer a DOM query while the sink ran behind the DataChannel), then read the
  terminal state 23.6 s after the agent finished. CLIENT-WEST during the tail: renderer **1.69–1.90 cores**,
  client total **2.1–2.4**, `vmstat` `us 23 / sy 33 / id 44`, load 2.60. So the unfixed client's direct-mode
  cost shows up as **CPU + tail**, exactly as E31's D1 (EAST) showed.
- Renderer over the transfer window: **1.941 mean / 2.018 peak** (n=19) versus WEST(fixed)'s 0.62–0.65 — the
  unfixed client burns **3× the renderer CPU** to deliver **14 % fewer bytes per second** (65.09 vs 74.51 arm
  mean) plus a 23.6 s tail.
- **W3 vs E10b's 60.24 Mbps:** 65.092 is **1.08×** E10b — the unfixed WEST direct figure reproduces.

### Cell W4 detail (appended immediately after the cell)

- Second WEST unfixed cell (relaunched with `STALL_MS=300000`). `MODE-CONFIRMED expected=direct`; agent
  **`lanes ready`** 19:13:12.388890655 → `download complete (count: 113)` 19:14:50.303462985 → window
  **97.915 s** → **64.597 Mbps = 8.075 MB/s**; terminal `✓ intact` 19:15:11.626 → tail **21.323 s**;
  user-visible **118.84 s**; `worker_sha1=null` (unfixed control).
- **WEST(unfixed) n=2 = 65.092, 64.597 → mean 64.84 Mbps (8.11 MB/s)**, spread 1.01× — very reproducible.

### Cell W5 detail (appended immediately after the cell) — bare counter (E28 ablation)

- Arm switched to **bare** (`set-arm.sh bare`; served `app.js` re-verified **from CLIENT-WEST** as
  `b7a18b9a…`), URL `?bare=1`. Driver printed `bare-counter-present` before the click and
  `MODE-CONFIRMED expected=direct`.
- Agent **`lanes ready`** 19:16:15.582472890 → `download complete (count: 114)` 19:17:37.340419370 → window
  **81.758 s** → **77.363 Mbps = 9.670 MB/s**. Relay standby torn down unused (19:16:59.554).
- **Byte-accounting gate passed exactly:** `BARE-ACCOUNTING W5 payload=790626304 frames=48258
  wire=791301916 expected=790626304 match=true` (wire = payload + 14 × 48,258 envelope bytes). The arm is
  valid, not merely fast.
- "Tail" for this arm is the bare counter reaching the exact payload (19:17:38.775) minus the agent's
  `download complete` = **1.435 s**, but the bare driver polls at 1.5 s, so this is ±1.5 s, not the sub-second
  resolution of the sink arms. Renderer **0.337 / 0.477** cores (n=16), client total 0.597.
- **Decisive mechanism result: removing the app's entire receive path (no envelope decode, no chunk assembly,
  no sink, no SHA-1) leaves WEST at 77.363 Mbps — statistically the same as the fixed client's 74.71 Mbps arm
  mean (1.04×).** The ~75 Mbps ceiling at 70 ms is therefore **not** the app and **not** the sink; it is the
  browser's own DataChannel/SCTP receive path (or the transport) at that RTT.

### Cell W6 detail (appended immediately after the cell) — 3rd WEST fixed cell + sender CPU

- Second re-arm to `fix` (served hashes re-verified from CLIENT-WEST as `e650a950…` / `72f28ed0…` /
  `77a10595…`). `MODE-CONFIRMED expected=direct`; agent **`lanes ready`** 19:18:33.582845289 →
  `download complete (count: 115)` 19:19:57.811497867 → window **84.229 s** → **75.093 Mbps = 9.387 MB/s**;
  tail **0.381 s**; user-visible **84.18 s**; `ui_sha1 = worker_sha1 = a7e0206e…`.
- **Sender CPU sampled with `docker stats --no-stream` every 5 s (21 rows, 2 pre-cell + 14 in-window +
  5 post-cell): in-window mean 100.24 % (≈1.00 core), peak 192.28 % (≈1.92 cores), memory 65–68 MiB.** The
  agent is **not** saturated (`NanoCpus=0` on a multi-core host) and is delivering 75 Mbps at ~1 core. For
  comparison E30 measured the sender at **0.65 core for 119 Mbps (167–179 Mbps/core)** on the EAST fixed arm;
  at 70 ms the sender spends **~2.3× more CPU per delivered Mbps** (≈75 Mbps/core) — the signature of a flow
  parked in congestion control / retransmission behind a slow receiver, not of a CPU-bound sender.
- **WEST(fixed) n=3 = 71.625, 77.401, 75.093 → mean 74.71 Mbps (9.34 MB/s)**, min 71.63, max 77.40, spread 1.08×.

## Arm summary (same session, E12/E30 metric)

| arm | cells | agent Mbps | mean | min–max | mean tail s | mean renderer cores |
|---|---|---|---|---|---|---|
| **EAST, direct + fix** (12 ms ref) | E1, E2 | 138.583, 136.840 | **137.71** (n=2) | 136.84–138.58 | **0.463** | 1.009 |
| **WEST, direct + fix** (the question) | W1, W2, W6 | 71.625, 77.401, 75.093 | **74.71** (n=3) | 71.63–77.40 | **0.599** | 0.623 |
| **WEST, direct + unfixed** (E10b control) | W3, W4 | 65.092, 64.597 | **64.84** (n=2) | 64.60–65.09 | **22.475** | 1.919 |
| **WEST, direct + bare counter** | W5 | 77.363 | **77.36** (n=1, single observation) | — | ~1.4 (±1.5) | 0.337 |
| path control VERSA→CLIENT-EAST (no cell) | — | TCP 1 flow **245** | UDP 300→**249** (17 % loss) | — | — | — |
| path control VERSA→CLIENT-WEST (no cell) | — | TCP 1 flow **163**, repeat **111**; TCP 4 flows **245** | UDP 300→**249** (17 % loss) | — | — | — |
| E31 same-rig relay + fix, CLIENT-WEST (cross-experiment) | B2 | — | **221.003** | — | 0.627 | 1.365 |

## Interpretation

### 1. The deciding comparison — and the answer is NO, direct does not survive latency

| | agent Mbps | MB/s | client renderer cores |
|---|---:|---:|---|
| EAST(fixed), ~11 ms (n=2) | **137.71** | **17.21** | 1.009 |
| WEST(fixed), ~70 ms (n=3) | **74.71** | **9.34** | 0.623 |
| ratio | **1.84×** | 1.84× | 0.62× |

The two ranges do **not** overlap (EAST 136.8–138.6; WEST 71.6–77.4), the arms ran interleaved in the same hour
on the identical payload and the identical client build, and both were mode-asserted direct with the relay
standby unused. **WEST(fixed) is 1.84× slower than EAST(fixed), so the 15–19 MB/s figure does *not* transfer to
~70 ms users: the right expectation for a distant user on v1 direct is ~9 MB/s (≈75 Mbps), i.e. about half.**

### 2. It is not the client's CPU, and it is not the app or the sink — it is the browser's receive path at high RTT

The three WEST arms separate the possible causes:

| WEST arm (all ~70 ms, all direct) | agent Mbps | renderer cores | what it removes |
|---|---:|---:|---|
| unfixed sink | 64.84 (n=2) | 1.919 | — |
| E29 fixed sink (worker SHA-1, no re-concat) | 74.71 (n=3) | 0.623 | 53 % of the app's per-byte cost |
| **bare counter** (no sink, no pipeline, no SHA-1) | **77.36** (n=1) | **0.337** | **everything above the DataChannel** |

Removing the entire app receive path moves WEST from 74.71 to 77.36 Mbps — **1.04×** — while cutting renderer
CPU 3.1× (0.623 → 0.337 cores). At 70 ms the app is therefore **no longer the constraint**: the residual ~75 Mbps
is below it, in the browser's DataChannel/SCTP receive path. The client is also never CPU-saturated (0.34–1.92
of 4 cores across all three arms), so this is a **latency/concurrency limit in the receive pipeline**, not CPU
throughput — the same signature E30 inferred at 4 vCPU, now shown to be strongly RTT-dependent.

### 3. How much of E10b's 60.24 Mbps was the client rather than the latency?

**Almost none of it.** WEST(unfixed) today = 64.84 Mbps (n=2, spread 1.01×) versus E10b's 60.24 — **1.08×**, i.e.
E10b's figure essentially reproduces. Applying E29's fix at 70 ms buys **74.71 / 64.84 = 1.15×** (n=3 vs n=2,
ranges 71.6–77.4 vs 64.6–65.1 — non-overlapping, but only just) plus the tail (**22.5 s → 0.6 s**) and 3× less
renderer CPU. At ~12 ms the same fix buys 1.10–1.27× (E29/E30). So E10b's "direct halves with RTT" was
**substantially right about the halving** and the old suspicion that it merely measured a bad client is
**refuted**: at 70 ms the fix recovers only ~15 % of the gap, because the binding constraint has moved off the
app and onto the browser's high-RTT receive path.

### 4. Against the path — the ~75 Mbps ceiling is not the network

| control (no cell running) | result |
|---|---|
| VERSA → CLIENT-WEST, TCP 1 flow | **163 Mbps** (39 retr), repeat **111 Mbps** |
| VERSA → CLIENT-WEST, TCP 4 flows | **245 Mbps** aggregate |
| VERSA → CLIENT-WEST, UDP offered 300 Mbps | **249 Mbps** received, 17 % loss (the path policer) |
| VERSA → CLIENT-EAST, TCP 1 flow / UDP 300 | **245 Mbps** / **249 Mbps**, 17 % loss |

The WEST path carries **≥2.1× to 3.3×** what direct achieved, in aggregate and in a single TCP flow. So the
network is not the limiter; the SCTP/DataChannel receive path at 62–70 ms is. (Caveat: the WEST single-flow TCP
figure is variable — 163 then 111 Mbps — which by itself shows that a single-flow, high-RTT transport number on
this rig spans a wide band; the load-bearing comparison is the same-session WEST bare-vs-fixed equality above,
which needs no path control to interpret.)

### 5. Verdict on the relay for distant users: YES — the relay is the answer at ~70 ms

| v1 transport, CLIENT-WEST, ~70 ms | agent Mbps | MB/s | source |
|---|---:|---:|---|
| direct + unfixed client | 64.84 (n=2) | 8.11 | this experiment |
| direct + E29 fixed client | 74.71 (n=3) | 9.34 | this experiment |
| direct + bare counter | 77.36 (n=1) | 9.67 | this experiment |
| **relay + E29 fixed client** | **221.00** (n=1) | **27.63** | E31 B2, same rig, same client |
| relay + fixed client, CLIENT-EAST (12 ms) | 228.63 (n=1) | 28.58 | E31 B1 |
| relay + fixed client, CLIENT-WEST (71 ms) — E31 mean | 224.82 (n=2) | 28.10 | E31 |

E31's relay numbers (cross-experiment, single observations) show the relay losing only **~3 %** between 12 ms
(228.6) and 71 ms (221.0), while direct loses **46 %** in the same pairing. At ~70 ms the relay is therefore
**~3× faster than direct on the same client** (221.0 vs 74.7), the reverse of the 12 ms picture where E31
measured relay 228.6 > direct 149.1. The v1 relay — a TCP/WebSocket path whose receive side is cheap and whose
flow is RTT-tolerant — is the transport to use for distant users; direct is the fast option only for near ones.

### 6. What this changes for expectations

- **Near users (≈12 ms):** v1 direct with the fixed client ≈ **137 Mbps / 17.2 MB/s** (this session), consistent
  with E30's 119.17 (n=4) and E31's 149.09. The 15–19 MB/s figure is right **only** for this regime.
- **Distant users (≈70 ms):** v1 direct with the fixed client ≈ **75 Mbps / 9.3 MB/s** — half. Use the relay
  (~221 Mbps / 27.6 MB/s on the same client, E31).
- **Mobile users will be worse than 70 ms in the RTT dimension and worse again in CPU**, and this experiment
  shows the direct penalty is an RTT effect that the client fix does **not** remove; the relay result is the one
  to lean on for them (with the caveat that no mobile device or cellular path was measured here).

## Caveats

1. **n is small: EAST(fixed) n=2, WEST(fixed) n=3, WEST(unfixed) n=2, WEST(bare) n=1.** The WEST bare arm is a
   **single observation**; it is used only for the mechanism claim *together with* the n=2/n=3 sink arms it
   agrees with (1.04×), never as a standalone rate.
2. **One cell was deliberately aborted** (first W3, 19:09:09Z) because it was launched with a `STALL_MS` shorter
   than the unfixed client's tail; it is reported above and is not a measurement. Share counter moved 107 → 115.
3. **The E29 fix was reconstructed, not reused byte-for-byte** (see the arm table). `downloadSinks.js` and
   `hashWorker.js` are byte-identical to E31's; the fix `app.js` differs in hash from E31's (same size). Every
   fixed cell gated on `FIX-NOT-ENGAGED` / `HASH-DISAGREE` and reported `worker_sha1 = ui_sha1 = a7e0206e…`, so
   the measured behaviour is the fixed behaviour, but the byte-identity claim is weaker than E31's.
4. **The bare arm's `app.js` is a reconstruction of E28's abridged diff** (92,372 B vs E28's 93,008 B — E28's
   file held 637 B more than its published diff shows). The reconstruction is behaviour-equivalent by
   construction and gated by `NO-BARE-COUNTER` + exact byte accounting.
5. **The relay comparison is cross-experiment** (E31, ~10 h earlier) and n=1 per client there. E31's relay cells
   are the campaign's own measurement of the same rig and client; no relay cell was run today because this
   experiment's question was about direct. Absolute rates are known to drift between nights on this rig (E29
   146.54 vs E30 119.17), so the ~3× direct-vs-relay gap should be read as "large and in the relay's favour",
   not as a precise ratio.
6. **One direction only, one payload, one file size, one tab.** No concurrent sessions, no loss on the data
   path (RTT pings showed 0 % loss), no jitter or mobile-network emulation. The 176 / 247 / 101 ms RTT bands and
   real cellular paths are extrapolations.
7. **Renderer-core means are coarse for the short EAST cells** (n=9–10 rows) and the arm table's "mean renderer"
   is the mean of the per-cell means; the CPU claims are used qualitatively (3× differences), never as precise
   ratios.
8. **The tail is measured from two hosts' clocks.** CLIENT-EAST↔TESTBOX skew ≈1 s (E29); CLIENT-WEST↔TESTBOX was
   not separately calibrated, so the sub-second tails (0.38–0.98 s) are ±1 s and only the unfixed arms' 21–24 s
   tails are asserted tightly.
9. **Agent CPU was sampled in one cell only** (W6, `docker stats --no-stream` every 5 s, 14 in-window rows), so
   the "sender is not the constraint" claim rests on that cell plus E30's EAST measurements.
10. **The metric is agent-side and the sender does not log a byte count** (E12 limitation). Completeness is
    established by the client's own verification (`✓ intact` with the expected SHA-1) in every sink arm, by
    `worker_sha1 = ui_sha1` in the fixed arms, and by exact byte accounting in the bare arm (790,626,304 B).

## Restore and rig verification (19:21–19:23Z)

**TESTBOX web root restored byte-for-byte and checksum-verified.** `src/app.js` =
`407ee7032f90e4298ee6b0416ed69dbf94147d0c6ff0933e7c6803f129bdf3c1` (91,308 B), `src/downloadSinks.js` =
`5c7ff32294ab617e4cd9d37d7406ef10362b167422d3471dd406a12b4edfc3da`, `src/hashWorker.js` **absent**; both `cmp`
checks against `/tmp/exp32/backup/src/` returned byte-identical. A full `find . -type f -exec sha256sum {} \;`
manifest of the web root matches the pre-change manifest exactly — **140 files,
`WEBROOT-RESTORED-MANIFEST-IDENTICAL-140-FILES`** (the first `diff` looked non-identical only because the
pre-manifest used absolute paths and the post-manifest relative ones; both were normalised to the same form and
re-compared). A fresh HTTP retrieval **from CLIENT-WEST through the real origin** returned `app.js` =
`407ee703…` (200), `downloadSinks.js` = `5c7ff322…` (200) and `hashWorker.js` = **404**, so the served client
really is the original again.

- **TESTBOX services:** `sharebridge-test` and `caddy` **active** (as this experiment's Part 0 left them);
  `sharebridge`, `sharebridge-relay-gateway`, `sharebridge-relay-frps` remain **inactive but enabled** — they were
  stopped at 18:58Z to make the v1 rig possible (see "Rig state change"). Restart them
  (`systemctl start sharebridge-relay-gateway sharebridge-relay-frps sharebridge.service`) for phase-4a work.
- **VERSA:** `sb-run` left **running** (`sb-agent:pristine`, `NanoCpus=0`, `Restart=unless-stopped`, variant `c`,
  `StartedAt 18:56:57.680007125Z`) — the orchestrator owns teardown. Production `sharebridge-agent` (7878)
  **running and never touched** (`StartedAt 10:03:56Z`, unchanged); `sharebridge-agent-test` left **exited**; every
  other VERSA container untouched. Agent log quiet after `peer … closed` (19:20:13); share counters ended at
  direct 107 → **115** (8 cells; the one aborted attempt produced no completion) and relay 11 → 11 (**no relay
  cell was run**).
- **Clients clean:** CLIENT-EAST and CLIENT-WEST both report **0** `node`, **0** `chrome`, **0** `Xvfb`,
  **0** `iperf3`, **0** `exp32-sampler` processes (`ps -eo args | grep -c`, which avoids the `pgrep -fc`
  self-match that reads 2). One at a time, `pkill -x`/`-9 -x` only.
- **Untouched:** the v2 stack beyond the documented stop, `sharebridge-agent-test`, production
  `sharebridge.app`, DNS/ACME, all non-test VERSA containers and non-test Immich resources. **No instance was
  shelved, stopped or resized; no `git` command was run; no repo file other than this results file was written.**

## Artifacts

- Results file: this file (`.worktrees/benchdirect/docs/superpowers/spikes/results/`).
- TESTBOX: `/tmp/exp32/out/{app.fix,app.discard,app.bare,downloadSinks.fixed,hashWorker}.js`, `/tmp/exp32/set-arm.sh`,
  `/tmp/exp32/backup/src/` (originals), `/tmp/exp32-webroot.tgz`, `/tmp/exp32-web-manifest-{pre,post}.txt`,
  `/tmp/exp32-{pre,post}-norm.txt`.
- Clients: `~/sbtest/exp32-{drive.js,launch.sh,sampler.py}` (new test-only tooling on **both** clients —
  CLIENT-WEST is left with it deliberately so the arm can be re-run; nothing pre-existing was modified),
  per-cell `/tmp/exp32-<cell>.log` (driver: MODE/RESULT/BARE-ACCOUNTING/SINK-BYTES/hashes) and
  `/tmp/exp32-<cell>.proc` (5 s per-role CPU deltas).
- VERSA: `/tmp/exp32-W6.dockerstats` (sender CPU samples); agent windows via
  `docker logs --timestamps --since <RFC3339Z> sb-run` per cell (downloads 108 = E1 … 115 = W6).
