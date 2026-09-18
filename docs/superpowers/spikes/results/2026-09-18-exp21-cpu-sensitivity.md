# Experiment 21 — is the v1-direct field cap CPU-bound on the sender? — 2026-09-18

**Verdict:** **Part 1/3 are decisive; Part 2 is directional.** The **host is NOT the explanation**:
during both an unconstrained and a 1-core-capped transfer the sender host ran **~75 % idle with
`steal` = exactly 0 jiffies on all six CPUs**, no core above 28 % busy, load ≤ 1.06, run queue mostly
0–4. Throughput *does* track the CPU allowance below ~2 cores (0.5 → 38.31; 1.0 → 89.2 mean of n=2;
2.0 → 124.17; uncapped → 113.9 mean of n=3) and **plateaus at ~2 cores**, which equals the uncapped
baseline and the field's ~122 Mbps. So: **a quieter/faster host buys nothing** (nothing is competing —
the agent never asks for more than ~2 of 6 vCPU), while **the sender's own per-byte cost is the only
CPU-side candidate left**. This experiment *removes host contention* as the cause; it does **not** by
itself prove the ~120 Mbps plateau is CPU-set rather than path-set (see Caveats §4).

**Question:** v1 direct (WebRTC DataChannel) delivers ~122 Mbps agent-side to CLIENT-EAST (~12 ms) and
~68 Mbps to CLIENT-WEST (~71 ms) in the field, while the same pion code (webrtc v4.2.11 / sctp v1.9.4)
does ~225 Mbps in the lab harness, and the path carries ~246 Mbps. Two candidate explanations with
opposite remedies:
(a) **per-byte send-path cost** (userspace DTLS + SCTP chunking, per-packet syscalls/copies) → a
CPU-efficiency fork of pion would pay off;
(b) **the sender host** (VERSA also runs a GPU LLM server, Jellyfin, Immich, Prometheus, …; the agent
burns ~0.9–1.6 cores with no pinned thread) → a quieter/faster box would pay off and a fork would not.
Discriminated here by measuring **throughput versus available CPU**.

**Setup:** Field rig. Agent = VERSA container `sb-run` ONLY (`sb-agent:pristine`, host networking,
`UI_PORT=7879`, `UI_ADDR=127.0.0.1`, `SB_SCTP_CA_STEP=32768`, **no** `SB_SCTP_MIN_CWND`) — the
documented variant-`c` state, changed only by the `--cpus` quota under test. Signalling TESTBOX (v1,
Caddy TLS, HTTP 200). Payload = one 754 MiB file = 790,626,304 B = 6,325.01 Mb per download (the same
exp20 share; the mount points at the same `file_id`). Driver: `drive_click.js` on CLIENT-EAST, exactly
one click-only tab, **never** the Playwright Download API. Metric: agent-side
`docker logs --timestamps sb-run`, window = first `DataChannel lanes ready` → `download complete`;
Mbps = 6,325.01 ÷ window_s. Host telemetry on VERSA: `uptime`/`/proc/loadavg`, interval `vmstat 1 3 |
tail -1`, `/proc/stat` aggregate **and per-CPU** jiffies (differenced to get per-CPU busy/idle/steal),
and `docker stats --no-stream sb-run` CPU %. `mpstat` is NOT installed on VERSA. VERSA = 6-vCPU
i5-9500, local clock UTC-4. All times UTC.

**Mode assertion per cell (non-negotiable):** a measured session must show `DataChannel lanes ready`
**and** an unused relay standby (relay channel closed by the relay at +44 s with
`pending wait window exceeded`, no relay data). **Two of eight cells (25 %) silently fell back to the
relay** and were rejected — the rate matches E17's warning. Rejected cells are kept in the table.

**Quota injection (hand-run for every constrained cell; `run-variant.sh` cannot express it):**
```
docker rm -f sb-run
docker run -d --name sb-run --restart unless-stopped --network host --cpus=<V> \
  -v sb-run-data:/root/.sharebridge --env-file /home/ali/sharebridge-test/env-run sb-agent:pristine
```
`env-run` was **never modified** by this experiment (it already carried `SB_SCTP_CA_STEP=32768` and no
`SB_SCTP_MIN_CWND`), so no env backup was needed — only the container was recreated. Verified per cell
via `docker inspect`: `NanoCpus=500000000` / `1000000000` / `2000000000` respectively, and `NanoCpus=0`
for the uncapped cells. Quota is applied to the whole container (agent's own cores), not a sidecar.

## Pre-state (recorded 2026-09-18T21:41Z)

- `sb-run` up, image `sb-agent:pristine`, `NanoCpus=0`, env verified `UI_PORT=7879`,
  `UI_ADDR=127.0.0.1`, `SB_SCTP_CA_STEP=32768`, no `SB_SCTP_MIN_CWND`; `docker stats` 3.62 % when idle.
- VERSA load average **0.66 / 0.90 / 1.02** (6 vCPU). Agent log quiet since 20:49:40Z (previous
  experiment's last peer close), i.e. >50 min before the first cell — no competing field agent.
- TESTBOX: `caddy` + `sharebridge-test` active, v1 signalling HTTP **200**.
- CLIENT-EAST, CLIENT-WEST reachable; `node`/`chrome`/`Xvfb` counts **0** on both.
- Production `sharebridge-agent`, the v2 stack, and all non-ShareBridge containers untouched. No git
  command was run at any point.

## Raw cells

`agent CPU` = `docker stats --no-stream sb-run` samples taken during the transfer window (valid
interval measure). `st`/`r`/host CPU come from corrected interval `vmstat` (`vmstat 1 3 | tail -1`) +
`/proc/stat` diffs, available only for the last two cells (see Method note 1).

| # | cell | `--cpus` | window (UTC, lanes ready → complete) | window s | Mbps | direct? | agent CPU mean/max | host busy/idle | steal `st` | runq `r` | load |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | EAST solo (unmonitored) | none | 21:42:00.162 → 21:42:50.486 | 50.323 | **125.69** | yes | — | — | — | — | — |
| — | EAST (REJECTED, relay) | none | click 22:13:19.555 → 22:15:47.644 | — | relay ≈42.7 | **NO — relay** | — | — | — | — | — |
| 2 | unlimB | none | 22:23:45.730 → 22:24:40.622 | 54.892 | **115.23** | yes | 92.3 / 178.5 % | — | boot-avg only | boot-avg only | 0.31–1.27 |
| 3 | c05 | 0.5 | 22:25:21.395 → 22:28:06.500 | 165.105 | **38.31** | yes | 50.0 / 52.4 % (pinned) | — | boot-avg only | boot-avg only | 0.83–1.97 |
| — | c1 (REJECTED, relay) | 1 | peer closed 22:28:50.713, no lanes | — | relay ≈39.6 | **NO — relay** | — | — | — | — | — |
| 4 | c2 | 2 | 22:31:49.801 → 22:32:40.739 | 50.938 | **124.17** | yes | 69.7 / 101.5 % | — | boot-avg only | boot-avg only | 0.68–1.50 |
| 5 | c1b | 1 | 22:33:13.292 → 22:34:25.596 | 72.303 | **87.48** | yes | 78.5 / 104.5 % | — | boot-avg only | boot-avg only | 0.91–1.64 |
| 6 | unlimC | none | 22:36:01.316 → 22:37:04.168 | 62.851 | **100.64** | yes | 83.2 / 142.0 % | 24.2 / 75.6 % | **0.000 % (0 j total, all 6 CPUs)** | 0–17 (mostly 0–4) | 0.59–1.06 |
| 7 | c1c | 1 | 22:37:45.802 → 22:38:55.327 | 69.525 | **90.98** | yes | 77.0 / 96.1 % | 24.8 / 75.1 % | **0.000 % (0 j total, all 6 CPUs)** | 0–9 | 0.49–1.04 |

### Per-quota summary

| `--cpus` | n (measured) | Mbps cells | mean | min | max | agent CPU mean / max |
|---|---|---|---|---|---|---|
| 0.5 | 1 | 38.31 | 38.31 | 38.31 | 38.31 | 50.0 / 52.4 % — CFS-throttled at the cap the whole transfer |
| 1 | 2 | 87.48, 90.98 | **89.23** | 87.48 | 90.98 | 77.0–78.5 / 96.1–104.5 % — pinned at ~1 core |
| 2 | 1 | 124.17 | 124.17 | 124.17 | 124.17 | 69.7 / 101.5 % |
| none (control) | 3 | 125.69, 115.23, 100.64 | **113.85** | 100.64 | 125.69 | 83.2–92.3 / 142.0–178.5 % |

### Cell details

- **Cell 1** — direct asserted (relay standby closed unused 21:42:44), count 67. No host telemetry
  (the transfer ran before the corrected sampler existed).
- **Rejected relay cell at 22:13** — `peer 579ebba4… closed` at 22:13:19.059 (10 s after join, **no**
  `lanes ready` line for it), then `relaychannel: handshake complete` at 22:13:19.092; `download
  complete` 22:15:47.644 (count 68). Relay, not direct → excluded. Same signature as E17's `EC0`.
- **Cell 2 (unlimB)** — lanes 22:23:45.730193602, complete 22:24:40.622349453 (count 69); relay standby
  closed unused 22:24:30. Agent CPU samples (18, mid-transfer): 45.07, 113.62, 78.44, 106.01, 107.42,
  63.47, 96.91, **178.45**, 51.19, 92.59, 53.61, 93.53, 111.84, 92.04, 77.67, 79.32, 94.11, 126.60 % →
  mean **92.3 %**, peak **178.5 %** (i.e. the uncapped agent briefly uses ~1.8 cores).
- **Cell 3 (c05, `--cpus=0.5`)** — `NanoCpus=500000000`; lanes 22:25:21.395496153, complete
  22:28:06.500058300 (count 70); relay standby closed unused 22:26:05. Agent CPU samples 47.1–52.4 %
  (mean ≈ 50 %) → the container was **against its quota for the entire 165 s window**.
- **Rejected relay cell at 22:28 (`--cpus=1`)** — `peer 0f3a7770… closed` 22:28:50.713, no lanes-ready,
  `relaychannel: handshake complete` 22:28:50.752, complete 22:31:20.642 (count 71). Excluded; retried.
- **Cell 4 (c2, `--cpus=2`)** — `NanoCpus=2000000000`; lanes 22:31:49.801040662, complete
  22:32:40.739239491 (count 72); relay standby closed unused 22:32:34. Agent CPU mean 69.7 %, max
  101.5 % — **never quota-clipped**, and the rate equals the uncapped baseline.
- **Cell 5 (c1b, `--cpus=1`)** — `NanoCpus=1000000000`; lanes 22:33:13.292389799, complete
  22:34:25.595881863 (count 73); relay standby closed unused 22:33:57. Agent CPU mean 78.5 %, max
  104.5 % (clipped at the 1-core cap; contrast the 178.5 % peak when uncapped).
- **Cell 6 (unlimC, uncapped, full corrected telemetry)** — `NanoCpus=0`; lanes 22:36:01.316472438,
  complete 22:37:04.167800401 (count 74); relay standby closed unused 22:36:45. Over the 12
  consecutive 1-s interval samples inside the transfer: host aggregate **busy 24.2 %, idle 75.6 %,
  iowait 0.17 %, steal 0.000 % (0 jiffies)**; per-CPU busy 23.0 / 26.3 / 24.4 / 23.2 / 24.3 / 24.6 %
  with **steal = 0 j on every one of the six CPUs**; run queue `r` = 1,0,2,2,4,1,12,0,3,17,1,0;
  load 0.59–1.06; `docker stats` CPU mean 83.2 %, peak 142.0 %.
- **Cell 7 (c1c, `--cpus=1`, full corrected telemetry)** — `NanoCpus=1000000000`; lanes
  22:37:45.802080525, complete 22:38:55.326720795 (count 75); relay standby closed unused 22:38:30.
  Over the 13 interval samples inside the transfer: host aggregate **busy 24.8 %, idle 75.1 %,
  iowait 0.13 %, steal 0.000 % (0 jiffies)**; per-CPU busy 22.9 / 28.0 / 24.1 / 25.0 / 24.8 / 24.3 %
  with **steal = 0 j on all six**; `r` = 3,0,0,3,9,1,2,2,0,5,7,2,2 (max 9); load 0.49–1.04;
  `docker stats` CPU mean 77.0 %, peak 96.1 % (pinned at the cap).

## Interpretation

1. **Host contention is falsified as the explanation.** With the corrected interval sampler the two
   cells that have it — one uncapped (100.64 Mbps) and one capped at 1 core (90.98 Mbps) — both show
   the sender host **75 % idle, `steal` = 0 jiffies on all six CPUs, no core above 28 % busy, load
   ≤ 1.06**. There is no steal, no sustained run queue, and no pinned/saturated thread. A transfer on
   this host has ~4.5 idle vCPUs behind it. So remedy (b) — move the agent to a quieter/faster box —
   cannot buy throughput: nothing is competing for the agent's CPU during a cell, and the host-side
   evidence for "the agent burns 0.9–1.6 cores while the box runs an LLM server and Jellyfin" does not
   reproduce as contention (the other workloads were idle during my cells; load stayed ≤ 1.06).
2. **Throughput tracks the CPU allowance up to ~2 cores, then plateaus at the uncapped field rate.**
   0.5 → **38.31**, 1.0 → **89.23** (n=2: 87.48 / 90.98), 2.0 → **124.17**, uncapped → **113.85**
   (n=3: 125.69 / 115.23 / 100.64). The 0.5→1 step is 2.33× for 2× CPU; the 1→2 step is 1.39×; the
   2→uncapped step is **−8 %**, i.e. **the 2-core quota already reaches the uncapped ceiling** (124.17
   is inside the uncapped range and equals its maximum). The **number that decides it** is that the
   uncapped agent self-limits at the same ~120 Mbps as a 2-core quota while the host offers 6 cores and
   uses ~0.8–1.4 mean / 1.8 peak. The agent's demand saturates; the machine's capacity does not.
3. **The CPU cost per unit of goodput is large and the demand is bursty.** At the uncapped field rate
   the agent consumed ~0.83–0.92 **cores on average** but **peaked at 1.42–1.79 cores**, i.e. ~60–90
   Mbps per mean core with instantaneous demand near 2 cores. Clipping that demand to 1.0 core
   (cells 5, 7) costs 21–29 % of throughput (89.2 vs 113.9 mean); clipping it to 0.5 core costs 66 %
   (38.3). That shape — high cycles/byte with bursty multi-goroutine demand — is exactly the
   per-byte-send-path-cost signature (userspace DTLS record protection + SCTP chunking + per-packet
   syscalls/copies) named in hypothesis (a), and it is the *only* candidate left once the host is
   excluded.
4. **Consequence for the pion-fork business case.** The premise "the field host is busy and the agent
   has no pinned thread, so a quieter box would pay off" is **not supported**: steal is 0 and the host
   is 75 % idle during the measurement, and the agent never wants more than ~2 cores. Conversely,
   throughput per core is poor (~60–90 Mbps per mean core with 1.8-core peaks), so **reducing CPU
   cycles per byte is the only CPU-side lever that can move the v1 field cap** — a CPU-efficiency fork
   has a real (if bounded) case. Note the bound: the plateau at ~2 cores means *adding* cores or
   threads will not help; only cutting the cost per byte will.
5. **What this does not settle — see Caveats §4.** The sweep proves the *host* is not the limit and
   that throughput is CPU-allocation-sensitive; it does not prove the ~120 Mbps plateau is CPU-set
   rather than path-set. A path-side mechanism (loss/RTO/cwnd, E19's loss-independent ceiling) could
   equally hold the rate at ~120 Mbps, with the agent's ~0.8–1.4 mean cores a *consequence* of the
   achieved rate rather than its cause. Distinguishing those two needs an A/B on the send path's cost
   (e.g. the fork, or a lab cell at fixed RTT where per-core goodput is measured directly) — not a
   quota sweep. What the quota sweep uniquely eliminates is remedy (b).

## Caveats

1. **`vmstat 1 1` was the wrong invocation for the first five cells.** A bare `vmstat 1 1` row is the
   **since-boot average**, not an interval sample; the identical `us 9 / sy 5 / id 83 / st 0` row in
   cells 2–5 is boot-average residue and must **not** be read as per-cell idle/steal. I caught this and
   re-ran with `vmstat 1 3 | tail -1` plus per-CPU `/proc/stat` diffs for the last two cells (6, 7),
   which are the only cells whose `st`/`r`/host-CPU figures are valid. The `docker stats` CPU % column
   is valid in every cell; that is what the CPU-usage claims rest on.
2. **One unmonitored uncapped cell (cell 1, 125.69) has no CPU telemetry**, and the rejected relay
   cells have none either. Unconstrained n=3 spread is **100.64–125.69 (±11 %)**, so deltas inside
   ~11 % are not resolvable here. The load-bearing deltas are comfortably outside that: 0.5 vs 1
   (38.3 vs 89.2 = 2.33×) and 1 vs 2 (89.2 vs 124.2 = 1.39×). The 2.0 vs uncapped comparison (124.17
   vs 113.85) is *inside* the noise band and is only claimed as "no further gain".
3. **CFS quota throttling is harsher than a fair share of N cores.** `--cpus=N` caps the container at N
   cores *total* with burst penalties, whereas an idle 6-core host would give a bursty process more.
   So the 0.5/1.0 Mbps figures are a **lower bound** on what that much CPU could deliver in a
   fair-share regime, and they should not be read as a calibrated "Mbps per core" curve. The
   observation that does not depend on the quota method is the pairing in Interpretation §2: uncapped
   ≈ 2-core quota ≈ 120 Mbps **while the host is 75 % idle with zero steal**.
4. **Not established: that the ~120 Mbps plateau is CPU-set rather than path-set.** Also not
   established: (a) whether the same holds on CLIENT-WEST (~71 ms; its ~68 Mbps cap was not tested and
   is plausibly ACK/RTT-limited, not CPU-limited); (b) whether the uncapped peak of 178 % CPU is a pion
   thread-pool artefact or genuine useful work; (c) any lab comparison — the lab 225 Mbps figure was
   not re-measured and no lab per-core goodput was taken, so the "same code does 225 Mbps in the lab"
   contrast is quoted from the existing record, not reproduced here.
5. n=1 for the 0.5 and 2.0 cells and n=2 for the 1.0 cell; n=3 for uncapped. Single share, single
   754 MiB file, CLIENT-EAST only, single sequential driver. **Two of eight cells (25 %) silently fell
   back to the relay** and were rejected — a reminder that any field cell without the mode assertion
   is worthless (relayed cells read ≈39–45 Mbps, superficially plausible).
6. `docker stats --no-stream` CPU % is a ~1 s average sampled every ~4 s, so mean/peak here
   under-sample the true instantaneous envelope; the 178.5 % peak is a floor on the real peak.

## Restoration (verified 22:39Z)

- `sb-run` recreated via the canonical variant-`c` path (`run-variant.sh c`, reported
  `variant=c img=pristine ca=32768 auth=1 sessions=1`). Verified by `docker inspect`:
  image `sb-agent:pristine`, **`NanoCpus=0` / no `--cpus`**, `Restart=unless-stopped`, host networking,
  `UI_PORT=7879`, `UI_ADDR=127.0.0.1`, `SB_SCTP_CA_STEP=32768`, **no** `SB_SCTP_MIN_CWND`
  (`MIN_CWND` env matches = 0); log shows `loaded 2 sessions from store` and the web UI on
  `127.0.0.1:7879`. `env-run` was never modified.
- TESTBOX `caddy` + `sharebridge-test` active, v1 signalling HTTP **200**.
- CLIENT-EAST and CLIENT-WEST: `node`/`chrome`/`Xvfb` counts all **0** (my own drivers were cleaned up
  on CLIENT-EAST at the end of every cell; CLIENT-WEST was never driven).
- Production `sharebridge-agent`, the v2 stack, DNS/ACME, and non-test resources untouched; no instance
  state changed; no git command run.
- **Flag for the operator (not caused by this experiment):** `sharebridge-agent`'s
  `.State.StartedAt` read `2026-09-18T21:55:45Z` at verification time versus a running-container
  reading earlier in the session; I issued **no** command against that container at any point. Worth a
  glance in case something restarted production.

## Artifacts

- Results file: this file.
- Host samplers (VERSA `/tmp/`): `exp21-samp-{unlimB,c05,c1,c1b,c2,unlimC,c1c}.txt`; local copies of
  the corrected ones in `/tmp/samp-{unlimB,c05,c1b,c2,unlimC,c1c}.full`.
- Agent log windows: `docker logs --since <cell start> --timestamps sb-run` (containers were recreated
  per cell, so logs of removed boots are gone; the quoted lines were captured live per cell).
- Driver logs on CLIENT-EAST: `/tmp/cell-{unlimB,c05,c1b,c2,c1c}.log` (plus the rejected cells'
  logs from the first script run).
- Share download counter advanced 66 → 75 across the cells (limit 500); every measured cell = 1 count.
