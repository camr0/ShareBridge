# Experiment 30 — v1-direct with E29's client fix re-applied: what binds at the new ceiling? — 2026-09-19

**Verdict:** the achievable v1-direct rate with E29's fixed client is **119.2 Mbps today (n=4,
107.5–130.9)** — only **1.10×** the unfixed client measured in the same block (108.1, n=2) — and **what
binds is the client's own receive side, not the sender**: capping the sender at 2 cores changed nothing
(115.2 vs 119.2) while it burned only **0.65 of one core**, whereas halving the client's cores cut the
rate 1.74× (119.2 → 68.3) and quartering them cut it 3.05× (→ 39.0), even though the client never
saturated its pinned allowance (0.755/1.0 core at 39.0; 1.140/2.0 at 68.3; 1.79/4.0 at 119.2) — and the
raw path carried **246 Mbps UDP / 241–242 Mbps TCP**, i.e. **2.1× headroom** over what v1-direct
achieved. Since the fix removes **53 % of the client's renderer CPU at unchanged rate** (2.06 → 0.98
cores), the binding client-side cost is **not in the app**: it is the browser's DataChannel receive path
plus the StreamSaver/service-worker write path, exactly the residue E28 isolated. **E29's 146.54 was not
reproduced** (4 cells, max 130.9; E29's own unpinned control that night was 115.36). **Deciding
comparison:** the client-CPU pin gradient (39.0 / 68.3 / 119.2 Mbps at 1/2/4 vCPU, 1.75×/1.74× per
doubling) moves the rate while the sender-CPU cap does not (115.2 vs 119.2) and the path has 2.1× spare.

**Question.** E29 measured the verification-preserving client fix at **146.54 Mbps**, which *exceeds*
E21's old-client sender plateau (**124.17** at `--cpus=2`, **113.85** uncapped mean), so E21's plateau was
client-limited, not a sender property. Re-apply the fix and find the **new binding constraint**.

**Arms** (per task): **F** fixed client, unpinned 4 vCPU, sender stock/uncapped (n=2 requested → n=4 run);
**S** fixed client + sender `--cpus=2` (n=1); **P2** fixed client pinned to 2 vCPU; **P1** fixed client
pinned to 1 vCPU (n=1 each). One **extra control arm C** (unfixed client, unpinned) was added after arm F
came in at ~109 instead of the expected ~146: without a same-block control the whole question "is the
client still the limit?" is unanswerable. C is reported separately and labelled as an addition.

## Setup

- **Agent:** VERSA container `sb-run` ONLY. The container was **recreated once at the start** through the
  canonical `run-variant.sh c` (image `sb-agent:pristine`, host networking, `UI_PORT=7879`,
  `UI_ADDR=127.0.0.1`, `SB_SCTP_CA_STEP=32768`, **no** `SB_SCTP_MIN_CWND`) because E29 left it carrying
  state; `docker inspect` after recreate: `NanoCpus=0`, `StartedAt 2026-09-19T00:39:00.961083408Z`,
  `variant=c img=pristine ca=32768 auth=1 sessions=1`, API **200**.
  **Sender CPU quota was injected with `docker update --cpus=2`** (no recreate) → `NanoCpus=2000000000`;
  it was reset by recreating the container again at 00:43:39 (`run-variant.sh c`, `NanoCpus=0`).
  `docker update --cpus=0` is a **no-op** (verified: `NanoCpus` stayed `2000000000`), so it was not used.
- **Signalling:** TESTBOX v1 (Caddy TLS) + **test-only** web root `/opt/sharebridge-test/web/`;
  `caddy`+`sharebridge-test` active, local `http://127.0.0.1:8080/` = 200, origin = 200.
- **Client:** CLIENT-EAST (4 vCPU, b2-15, ~12 ms path), `node`/`chrome`/`Xvfb` counts **0** before start.
- **Payload:** one 754 MiB file = 790,626,304 B = **6,325.01 Mb** per download — the same E28/E29 share
  (`relay_only=false`, expires 2026-09-20T04:49:25Z; agent download count 96 → 105 across this run).
- **Agent-side metric:** `docker logs --timestamps sb-run`, window = first `DataChannel lanes ready` →
  last `download complete`; Mbps = 6,325.01 ÷ window_s.
- **Tail:** the client's own terminal UI moment (`.file-item` class `verified` + `.file-status` = `✓ intact`,
  read from the DOM by the driver) minus the agent's `download complete` timestamp. CLIENT-EAST↔TESTBOX
  skew ≈ 1 s (E29), so sub-second tails are ±1 s and every tens-of-seconds claim is unaffected.
- **Renderer cores:** 5 s `/proc/<pid>/stat` delta sampler, per-PID role classified from cmdline
  (renderer / network service / gpu / chrome-main / node / xvfb). A Chrome Web Worker runs in the renderer
  process, so `renderer=` includes the SHA-1 worker. `client total` = every sampled process.
- **Driver:** `exp29-drive_verify.js` (click-only; the Playwright Download API is **never** called),
  asserting a `● Connected (Direct)` badge **before** the click and aborting with `RELAY-ABORT` on a Relay
  badge; `?fix=1` cells additionally assert the worker SHA-1 engaged (`FIX-NOT-ENGAGED`,
  `HASH-DISAGREE` guards).
- **Client pinning:** the driver is launched under `taskset`, so Chromium and every descendant inherit the
  mask. `none` = unpinned (4 vCPU), `pin2` = `taskset -c 0,1`, `pin1` = `taskset -c 0`.
- **Host state:** pre-state load 0.70, `vmstat 1 3` last row `id 97`, `st 0`, `sb-run` idle at 1.46 % CPU.
  No other field agent; one cell at a time.

## E29 client fix re-applied (TESTBOX test web root only)

Restored from E29's saved artifacts in TESTBOX `/tmp/exp29/`, installed `root:root 644`. The hashes are
the **exact** E29 outputs, and a fresh HTTP retrieval **from CLIENT-EAST through the real origin** returned
the same three hashes — so the client really exercised the fixed code:

| file | sha256 | size |
|---|---|---:|
| `src/app.js` | `cc4c944c1cbd081e9a70441a92c491f658f47f0eb9eaeb7b13621d6b42186542` | 92,265 B |
| `src/downloadSinks.js` | `db69d58da7b530fcc9019f475fedc543f2a4afe1d1b15cb90e61e98a3b134f8b` | 11,679 B |
| `src/hashWorker.js` | `10f9dc0e21c9aa443c20a57b54cb193b3dfda2af198ebe03d38293870f6f67cb` | 1,069 B |

The fix was applied for cells F1, S2, F2, P2, P1, F3, F4 (all `?fix=1`, driver `FIX_EXPECTED=1`, and every
one of them reported `worker_sha1 = ui_sha1 = a7e0206e…`), and reverted for the control cells C1, C2
(no `?fix`; both reported `worker_sha1=null`, which is the positive control that the *unfixed* client was
really being measured).

**One process error, reported for honesty:** the first command that ran on TESTBOX also copied the three
*patched* files over `/tmp/exp29-backup/src/`, destroying E29's saved originals there. The originals were
recovered byte-for-byte from the repo worktree copy (`signaling-server/web/src/{app.js,downloadSinks.js}` =
`407ee703…` / `5c7ff322…`, hash-matching E29's recorded originals) and the final manifest proves the
restore is byte-identical. No other file was affected; no repo file was modified.

## Raw cells

All cells CLIENT-EAST, one at a time, agent-side window (`DataChannel lanes ready` → `download complete`),
6,325.01 Mb per download. `tail` = agent `download complete` → client terminal UI (`✓ intact`).
`sender quota` = `sb-run` `NanoCpus` during the cell. `docker stats` = `docker stats --no-stream` CPU %
samples inside the transfer window (~5 s apart, so mean/peak under-sample the instantaneous envelope).

### Arm F — fixed client, unpinned (4 vCPU), sender stock/uncapped — the baseline for everything

| # | cell | client pin | sender quota | lanes ready → download complete (UTC) | agent window s | **agent Mbps** | agent finished | client terminal (UTC) | **tail s** | client-visible total s | renderer cores mean/peak (n) | client total cores mean/peak | `docker stats sb-run` mean/peak | UI | direct? |
|---|---|---|---|---|---|---|---:|---:|---|---:|---:|---|---|---|---|---|
| 1 | **F1** | none | uncapped (`NanoCpus=0`) | 00:39:31.860 → 00:40:30.692 | 58.832 | **107.51** | 00:40:30.692 | 00:40:31.354 | **0.662** | 58.88 | **0.928 / 1.333** (n=11) | 1.713 / 2.386 | — (poller added later) | `✓ intact` `a7e0206e…` | yes |
| 2 | **F2** | none | uncapped (`NanoCpus=0`) | 00:44:06.395 → 00:45:03.479 | 57.084 | **110.80** | 00:45:03.479 | 00:45:03.763 | **0.284** | 57.26 | **0.920 / 1.170** (n=11) | 1.693 / 2.098 | **66.2 / 116.7** (n=12) | `✓ intact` `a7e0206e…` | yes |
| 3 | **F3** | none | uncapped | 00:56:04.348 → 00:56:53.968 | 49.620 | **127.47** | 00:56:53.968 | 00:56:54.698 | **0.730** | 50.25 | **1.023 / 1.304** (n=8) | 1.877 / 2.320 | **91.0 / 127.6** (n=10) | `✓ intact` `a7e0206e…` | yes |
| 4 | **F4** | none | uncapped | 00:57:30.555 → 00:58:18.875 | 48.320 | **130.90** | 00:58:18.875 | 00:58:19.329 | **0.454** | 48.66 | **1.031 / 1.352** (n=8) | 1.889 / 2.436 | **77.3 / 125.2** (n=10) | `✓ intact` `a7e0206e…` | yes |

**Arm F mean = 119.17 Mbps (n=4; 107.51, 110.80, 127.47, 130.90)** — min 107.51, max 130.90, spread
1.218× — mean window 53.464 s, mean tail **0.533 s**, mean user-visible total 53.76 s, mean renderer
**0.976 cores**, mean client total **1.793 cores**. All four direct, fix engaged in all four,
**0 relay cells discarded**.

### Arm S — fixed client unpinned + sender `--cpus=2`

| # | cell | client pin | sender quota | lanes ready → download complete (UTC) | agent window s | **agent Mbps** | agent finished | client terminal (UTC) | **tail s** | client-visible total s | renderer cores mean/peak (n) | client total cores mean/peak | `docker stats sb-run` mean/peak | UI | direct? |
|---|---|---|---|---|---|---:|---:|---|---:|---:|---|---|---|---|---|
| 5 | **S2** | none | **`--cpus=2`** (`NanoCpus=2000000000`) | 00:42:14.670 → 00:43:09.583 | 54.913 | **115.18** | 00:43:09.583 | 00:43:10.166 | **0.583** | 55.39 | **0.977 / 1.307** (n=11) | 1.798 / 2.343 | **64.5 / 110.7** (n=11) | `✓ intact` `a7e0206e…` | yes |

**S2 is the deciding cell for the sender question.** Capping the sender at **2 cores** left the rate
**statistically unchanged — in fact above the uncapped arms** (115.18 vs arm-F mean 119.17; vs the two
temporally adjacent uncapped cells F2 = 110.80 and F3 = 127.47, which bracket it). The sender meanwhile
consumed **64.5 % mean / 110.7 % peak of one core — i.e. half of its 2-core allowance**. The sender's CPU
quota is **not** the binding constraint at this rate, and the sender does not even want 2 cores.
Interleaving: F1 → S2 → F2 → F3 → F4.

### Arm P — fixed client pinned: the client's remaining headroom (sender uncapped throughout)

| # | cell | client pin | sender quota | lanes ready → download complete (UTC) | agent window s | **agent Mbps** | agent finished | client terminal (UTC) | **tail s** | client-visible total s | renderer cores mean/peak (n) | client total cores mean/peak | `docker stats sb-run` mean/peak | UI | direct? |
|---|---|---|---|---|---|---|---:|---:|---|---:|---:|---|---|---|---|---|
| 6 | **P2** | `taskset -c 0,1` (2 vCPU) | uncapped | 00:45:37.606 → 00:47:10.156 | 92.550 | **68.34** | 00:47:10.156 | 00:47:10.952 | **0.796** | 93.20 | **0.567 / 0.693** (n=18) | **1.140 / 1.376** | **98.4 / 166.6** (n=16) | `✓ intact` `a7e0206e…` | yes |
| 7 | **P1** | `taskset -c 0` (1 vCPU) | uncapped | 00:48:20.769 → 00:51:02.877 | 162.108 | **39.02** | 00:51:02.877 | 00:51:04.695 | **1.818** | 163.73 | **0.347 / 0.418** (n=32) | **0.755 / 0.901** | **86.2 / 174.5** (n=29) | `✓ intact` `a7e0206e…` | yes |

**The client-CPU gradient is the decisive lever** (all three cells fixed client, sender uncapped, same
share, same driver):

| client vCPU | agent Mbps | client total cores used / allowance | renderer cores | agent Mbps per client core used |
|---|---:|---|---:|---:|
| 1 (P1) | **39.02** | 0.755 / 1.0 (76 %) | 0.347 | 51.7 |
| 2 (P2) | **68.34** | 1.140 / 2.0 (57 %) | 0.567 | 59.9 |
| 4 (F1–F4) | **119.17** | 1.793 / 4.0 (45 %) | 0.976 | 66.5 |

1 → 2 vCPU = **1.75×**; 2 → 4 vCPU = **1.74×**; 1 → 4 vCPU = **3.05×**. **The client never saturates the
core allowance it is given** (76 % / 57 % / 45 %), yet every extra core buys throughput almost
proportionally. P1's 39.02 Mbps reproduces E29's pinned fixed arm (37.53) and E25's pinned real client
(37.19) — the ~37–39 Mbps single-core client ceiling is confirmed on a third night.

### Arm C — control: **unfixed** client, unpinned (4 vCPU), sender uncapped (extra arm, not in the task)

| # | cell | client pin | sender quota | lanes ready → download complete (UTC) | agent window s | **agent Mbps** | agent finished | client terminal (UTC) | **tail s** | client-visible total s | renderer cores mean/peak (n) | client total cores mean/peak | `docker stats sb-run` mean/peak | UI | direct? |
|---|---|---|---|---|---|---|---:|---:|---|---:|---:|---|---|---|---|---|
| 8 | **C1** | none | uncapped | 00:52:03.681 → 00:53:03.294 | 59.613 | **106.10** | 00:53:03.294 | 00:54:33.109 | **89.815** | 149.29 | **2.041 / 2.163** (n=11) | 2.739 / 2.970 | **75.3 / 145.3** (n=13) | `✓ intact` `a7e0206e…` (`worker_sha1=null`) | yes |
| 9 | **C2** | none | uncapped | 00:58:56.756 → 00:59:54.248 | 57.492 | **110.01** | 00:59:54.248 | 01:01:19.312 | **85.064** | 142.46 | **2.077 / 2.223** (n=10) | 2.782 / 3.070 | **75.2 / 107.5** (n=10) | `✓ intact` `a7e0206e…` (`worker_sha1=null`) | yes |

**Arm C mean = 108.06 Mbps (n=2)**, mean window 58.553 s, mean tail **87.44 s**, mean renderer **2.059
cores**, mean client total **2.761 cores**. Both direct, both verified, both with `worker_sha1=null`
(proof the unfixed client was running), **0 relay cells discarded**.

**Fixed vs unfixed, same block, both unpinned 4 vCPU, sender uncapped:**

| metric | unfixed (C, n=2) | fixed (F, n=4) | ratio |
|---|---:|---:|---|
| agent-side rate | **108.06 Mbps** | **119.17 Mbps** | **1.10×** |
| renderer cores (mean) | **2.059** | **0.976** | 0.47× (**53 % less**) |
| client total cores (mean) | **2.761** | **1.793** | 0.65× (35 % less) |
| tail (mean) | **87.44 s** | **0.533 s** | **164× shorter** |
| user-visible total (mean) | 145.9 s | 53.8 s | **2.72×** |

### Path control (no cell running, ~00:55Z) — `iperf3` VERSA → CLIENT-EAST

| test | offered | received | loss |
|---|---:|---:|---:|
| UDP | 300 Mbps | **246 Mbps** | 18 % (the path policer) |
| TCP, 1 flow | — | **241 Mbps** | 0 |
| TCP, 4 flows | — | **242 Mbps** (aggregate) | 0 |

The path carries **≥2.1× the v1-direct rate achieved** in every cell of this experiment.

**Relay cells:** **0 discarded of 9 attempts** — every cell asserted `● Connected (Direct)` before the
click and had its relay standby torn down with `pending wait window exceeded` carrying no data. Unlike
E27 (4 of 8 pinned losses) and E29 (2 pinned cells lost), **both pinned cells (P2, P1) stayed direct on
the first attempt**, so no retries were needed.

## Interpretation

**1. The sender is not the constraint — the deciding comparison.** Capping the sender at 2 cores left the
rate unchanged (115.18 vs 119.17 mean; it even sits between the two adjacent uncapped cells 110.80 and
127.47), and the sender used **0.65 core mean / 1.11 core peak** — half its quota. E21's "plateau at ~2
cores" therefore does not survive as a *sender* property, which confirms E29's reading; but the ceiling it
was hiding is not a higher sender ceiling either, because the sender never needed those 2 cores.

**2. The client's own receive side is the constraint, and it is not CPU-throughput exhaustion.** The only
lever that moves the rate materially is the client's core allowance: 39.02 / 68.34 / 119.17 Mbps at
1 / 2 / 4 vCPU (1.75× per doubling), while the client is **never loaded to its allowance** — 76 %, 57 %,
45 % of the pinned cores are busy at those three rates. That is the signature of a *latency/concurrency*
limit inside the client's receive pipeline (a small number of threads or in-flight operations), not of a
saturated CPU: giving the client more cores lets the existing work finish faster, but the client always
finishes early on the cores it has. P1 (39.02) reproduces E29's fixed pinned arm (37.53, 0.774 core used)
and E25's pinned real client (37.19), so the ~37–39 Mbps single-core client ceiling is reproducible on a
third night with a different client build.

**3. The app is not what is binding at 4 vCPU.** Today the fixed client is only **1.10×** the unfixed
client (119.17 vs 108.06; the arm ranges overlap: F1/F2 = 107.5/110.8 sit *inside* C1/C2 = 106.1/110.0),
yet it uses **53 % less renderer CPU** and the tail collapses **87.4 s → 0.53 s**. Removing the app's
per-byte cost therefore removes CPU (and all of the user-visible tail) **without moving the rate**, which
means the binding per-byte cost lives *below* the app: the browser's DataChannel receive path plus the
StreamSaver/service-worker write path — the same split E28 measured (bare receiver 56.35 vs sink-free
56.35 → 82.99 pinned; sink-free 123.68 vs real client 119.45 unpinned).

**4. What the sender's CPU cost actually is (side finding that corrects E21).** Measured *through* a
client that keeps up, the agent delivers **~167–179 Mbps per core** (F2 110.80 / 0.662 = 167; F4 130.90 /
0.773 = 169; S2 115.18 / 0.645 = 179). When the client throttles the flow (P1) the sender still burns
0.862 core for 39.02 Mbps — **45 Mbps per core**. E21's "~85–100 Mbps per core / ~37 CPU-s per GB" was
therefore partly a measurement of the *client's* ceiling: a client-bound flow makes the sender's CPU look
2–4× more expensive per delivered byte than it is. **The pion-fork business case is therefore weaker than
E21 assumed**, because the honest per-byte cost is ~2× better than that experiment's central estimate,
and because the binding constraint at 4 vCPU is on the client, not the sender.

**5. Cross-run instability is now itself a finding.** E29 measured this same fix on this same rig at
**146.54 Mbps (n=2)**; today, four cells give **107.51–130.90, mean 119.17**. E29's fixed-client arm
*exceeded* its own unfixed control by 1.27×; today the same comparison gives 1.10× with overlapping
ranges. The rate ceiling of v1-direct on this rig is therefore **not stable between runs at the ~1.2–1.5×
level**, and the ~110–146 Mbps band is best read as "client-side ceiling with a run-to-run window of
±20 %", not as a single reproducible number. What *is* reproducible across E29 and E30: the fix's CPU win
(≈50 % renderer) and the tail win (85–90 s → 0.3–0.8 s). Both nights agree that the **rate** at 4 vCPU is
set by something outside the app.

**6. One sentence for the product decision.** *With E29's client fix applied, v1-direct delivers ~119 Mbps
today (n=4: 107.5–130.9) and what binds is the client's own browser receive/write path — not the sender's
CPU (a 2-core cap costs nothing, the sender uses 0.65 core), not the app (removing the app's per-byte cost
halves client CPU and kills the 87 s tail but moves the rate only 1.10×), and not the path (iperf3 today
carries 241–246 Mbps, 2.1× the achieved rate).*

## Caveats

1. **E29's 146.54 Mbps was not reproduced.** Four fixed-client uncapped cells today: 107.51, 110.80,
   127.47, 130.90 (mean 119.17). Cross-run absolute rates on this rig are not comparable (E29 said so
   itself), and E29's fixed arm also exceeded E28's bare-receiver unpinned plateau (123.68) — an
   internally odd number for the same night. The load-bearing results here are within-block comparisons;
   the 146.54→119.17 discrepancy is reported, not explained.
2. **The control arm C is an addition to the requested design** (the task asked only for arms F, S, P2,
   P1), added because arm F landed ~20 % below expectation and the question "is the client still the
   limit?" needs a same-block unfixed reference. It is n=2 and interleaved (C1 at 00:52 sits between F2
   and F3; C2 at 00:59 after F4).
3. **The client's core allowance is the only lever tested from below, never from above.** The client VM
   cannot be resized (safety rule), so "would more client cores lift the rate?" is **not** answered. The
   extrapolation the gradient suggests (≈ +1.75× per doubling, or a hard browser-path ceiling ~124 Mbps
   from E28) is speculation, and the two readings are not distinguished here.
4. **"Browser receive path" vs "v1 transport" is not fully separated.** A `?bare=1` bare-receiver cell
   (E28's patch) would have separated them directly, but E28's patched `app.js` did not survive on TESTBOX
   and re-authoring the patch against the E29 client was out of the time box. The evidence that the
   client-side limit is *outside the app* is the fixed-vs-unfixed equality (arm F ≈ arm C at the same
   block) plus the E28 split, not a fresh bare-receiver measurement.
5. **n is small and the rate drifts within the run.** Arm F spans 107.5–130.9 (1.22×) over 19 minutes with
   no monotone trend (F3/F4 were the fastest cells, but C2 immediately after them fell back to 110.01), so
   the 1.10× fixed-vs-unfixed difference is **inside** the arm's own spread. The claim made is therefore
   "the fix does not move the rate beyond noise today", **not** "the fix cannot move the rate".
6. **`docker stats --no-stream` is a ~1 s average sampled every ~5 s**, so the sender's mean/peak CPU
   under-samples its instantaneous envelope. F1's sender CPU was not sampled at all (the poller was added
   from S2 onward) and is reported as `—`, not interpolated.
7. One benign `join() re-entered while browser signaling socket is still active` page error appears in
   every cell (known since E15); it affected neither mode nor transfer in any cell.
8. The tail is measured from two hosts' clocks (CLIENT-EAST↔TESTBOX skew ≈1 s), so the sub-second fixed
   tails (0.284–0.796 s) are ±1 s accurate as *absolute* values; their 100×+ contrast with the unfixed
   85–90 s tails is unaffected.

## Restore and rig verification (01:02–01:05Z)

- **Web root restored byte-for-byte.** `src/app.js` = `407ee7032f90e4298ee6b0416ed69dbf94147d0c6ff0933e7c6803f129bdf3c1`,
  `src/downloadSinks.js` = `5c7ff32294ab617e4cd9d37d7406ef10362b167422d3471dd406a12b4edfc3da` (E29's exact
  originals), `src/hashWorker.js` **absent**. A full `find . -type f -exec sha256sum {} \;` manifest of the
  web root is **identical to E29's pre-change manifest** — 140 files, `diff` clean:
  `WEBROOT-RESTORED-MANIFEST-IDENTICAL (140 files)`. `caddy` and `sharebridge-test` both `active`;
  local `http://127.0.0.1:8080/` = 200 and the public origin = 200.
- **Rig restored.** `sb-run`: image `sb-agent:pristine`, **`NanoCpus=0`**, `Restart=unless-stopped`,
  `Running=true`, `StartedAt 2026-09-19T00:43:39.949383258Z` (the `--cpus=2` quota was cleared by
  recreating through the canonical `run-variant.sh c`), env `UI_PORT=7879`, `UI_ADDR=127.0.0.1`,
  `SB_SCTP_CA_STEP=32768`, **no** `SB_SCTP_MIN_CWND` (env grep count 0), agent API **200**. Agent log quiet
  after `download complete (count: 105)`.
- **Clients clean.** CLIENT-EAST after the last cell: `node=0 chrome=0 Xvfb=0 iperf3=0` (the temporary
  `iperf3 -s` used for the path control was killed with `pkill -x iperf3`, `-9 -x` fallback). CLIENT-WEST
  was never driven. `pkill -x`/`-9 -x` only, and only while it was certain that no other field agent was
  running (verified by process counts on both clients before every cell).
- **Untouched:** production `sharebridge-agent` (UI 7878), the v2 stack, `sharebridge-agent-test`,
  production `sharebridge.app`, DNS/ACME, all non-test containers — none was started, stopped, exec'd or
  inspected beyond `sb-run`. No container was shelved/resized/deleted; no instance state changed; **no
  `git` command was run** and no repo file was written.

## Artifacts

- Results file: this file.
- TESTBOX (`/tmp/`): `/tmp/exp29/{app.js,downloadSinks.js,hashWorker.js}` (E29's patched sources),
  `/tmp/exp30-orig-{app,downloadSinks}.js` (recovered originals), `/tmp/exp29-web-manifest-pre.txt`
  (E29's true 140-file original manifest), `/tmp/exp30-web-manifest-{pre,post,final}.txt`,
  `/tmp/exp29-web-manifest-post.txt`.
- VERSA (`/tmp/`): `/tmp/exp30-{S2,F2,P2,P1,C1,F3,F4,C2}.dockerstats` (+ `.tsv` pairings) — `docker stats`
  CPU % samples; agent log windows via `docker logs --since <RFC3339 Z> --timestamps sb-run`.
- CLIENT-EAST: `~/sbtest/exp29-drive_verify.js`, `~/sbtest/exp30-launch.sh`, `~/sbtest/exp29-sampler.py`;
  per-cell `/tmp/exp30-<cell>.log` (driver; MODE/RESULT/hashes) and `/tmp/exp30-<cell>.proc` (5 s per-PID
  CPU deltas, role-classified).
- Agent download counter advanced 96 → 105 (one per cell; limit 500).
