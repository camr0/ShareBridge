# Experiment 45 — C1 client scaling (8-vCPU browser) and C2 browser identity (Firefox) — 2026-10-13

**Status: COMPLETE. 18 completed transfer cells + 4 Firefox relay aborts, written incrementally after every
cell.** 8 browser cells on the 8-vCPU client (C1-a/b/c/d), 2 same-host CPU-pin controls, 2 same-session
4-vCPU real-host controls, 4 bare-receiver cells (2 Firefox + 2 Chromium) and 3 failed deploy attempts before
`DEPLOY-OK`.
**Zero relay fallbacks on Chromium (0 of 16 Chromium cells); 1 C1 cell flagged DIRTY; 2 blocking defects found
in EXP44's deploy script.**

**Verdict:**
* **C1 — the client IS the cap. "A bigger client VM won't help" is FALSIFIED.** On the **same host and the
  same path**, pinning the 8-vCPU client down to 4 vCPU moves the browser from **179.554 → 121.680 Mbps**
  (**1.48×**) at 12 ms — and 121.680 reproduces exp43's real 4-vCPU CLIENT-EAST number (117.799) to
  **+3.3 %**. Against exp43's 4-vCPU arms the 8-vCPU client scales **1.52× / 1.55× / 1.63× / 2.99×**
  (12 ms ON / 12 ms OFF / 72 ms ON / 72 ms OFF). The 4-vCPU browser ceiling is therefore a **client-side**
  ceiling, not a host, NIC or path artefact — which the pin control proves, because CLIENT-8's path carries
  **1.56× CLIENT-EAST's** TCP capacity in the same session yet its *pinned* browser lands on the same number.
* **C1, production relevance:** the scale-up is **largest on the knob-OFF (production) 72 ms arm, 2.99×**,
  versus 1.63× on the knob-ON arm. A bigger client disproportionately rescues the configuration production
  actually ships.
* **C2 — Firefox CAN be driven this way and delivers a byte-exact 754 MiB transfer** (E36's "writes no
  download artifact" is circumvented by the EXP28 bare counter), **but it is unreliable**: **4 of 6 attempts
  fell back to relay** and only 2 produced a number. On the like-for-like bare path, same hour, same client,
  **Firefox = 0.525× Chromium** (112.077 vs 213.469 Mbps) — a receive-path (engine) difference.
* It is **not** raw CPU exhaustion: at 12 ms the 8-vCPU client uses ~3.2–4.0 of 8 cores and the 4-vCPU-pinned
  client uses 2.62 of its 4. The binding constraint is the receive pipeline's **serialization/queueing**, which
  extra cores relieve — E28's framing survives; what dies is "4 vCPU is enough".

## Questions

exp43 measured the real browser on a **4-vCPU** client: **95.757** (knob unset) / **117.799** (knob 32768) at
~12 ms and **13.405** / **59.326** at ~70 ms. E30's CPU-pin gradient (39.0 / 68.3 / 119.2 Mbps at
1 / 2 / 4 vCPU) was **still rising at 4 cores**, so "a bigger client VM won't help" was inference, not
measurement.

**C1:** does the browser scale with client vCPU? Per the brief's rule — *if the 8-vCPU client lands ~the same,
the client is NOT the cap; if it scales up substantially, the client CPU IS the cap and the verdict reopens*.
**It scales up substantially → the client CPU is the cap → the verdict reopens.**

**C2:** is the receive path browser-identity-dependent? All 32 campaign experiments ran one Chromium (dcSCTP);
Firefox is usrsctp with a ~1 MiB rwnd. E36's Firefox attempt failed on the app's save path — which is exactly
why the EXP28 bare byte-counting receiver (made permanent by EXP44 behind `?bare=1`) exists.

## Setup

- Agent host **VERSA**; experiment container **`sb-run` ONLY**, `sb-agent:pristine`, host networking,
  UI **7879**. Production `sharebridge-agent` (7878) and every non-ShareBridge container untouched —
  verified at the end: **49 containers up**, `sharebridge-agent` `running=true`, started
  `2026-10-12T19:54:16Z` (predates this run, never restarted for it).
- Recreated between arms with the rig's own `run-variant.sh` (**executed, never modified**):
  `u` = pristine, no `SB_SCTP_CA_STEP`; `c` = pristine + `SB_SCTP_CA_STEP=32768`. **`run-variant.sh` was
  never edited and no `docker build` was run.** Arms A/A2/A3/C and the whole C2 phase ran on the container
  handed over from exp43 (`SB_SCTP_CA_STEP=32768`, started 05:00:47Z); the OFF arms ran on a container
  recreated at **05:41:07Z** with `run-variant.sh u`, whose `docker inspect` env shows **no `SB_SCTP_*` at
  all**.
- Signalling origin **TESTBOX**, `https://TESTBOX/` = **200** (5/5 from CLIENT-8; one transient `000` on the
  very first probe, a DNS hiccup on that probe only).
- Share: the orchestrator's direct share, `relay_only=false`, 754 MiB = 790,626,304 B = **6,325.010432 Mb**
  per download, 30/500 used at start → **48/500** at the end (18 completed transfers), expires 2026-10-15.
- **Clients:** **CLIENT-8** (8 vCPU / 28 GiB / 500 Mbps NIC) for every C1/C2 cell; **CLIENT-EAST** (4 vCPU)
  for the same-session 4-vCPU control only. CLIENT-WEST was **never driven and never unshelved**.
- **Metric: agent-side window only, for every cell.** First `DataChannel lanes ready` → last
  `download complete` from `docker logs --timestamps sb-run`; `Mbps = 6,325.010432 / window_s`.
  Bare cells additionally carry the driver's own `BARE-ACCOUNTING` line as a cross-check.
- **Mode assertion:** a valid cell needs `DataChannel lanes ready` **and** the relay standby expiring unused
  (`relay: pending wait window exceeded`, never `relay channel started successfully`). Every valid cell below
  shows that shape.
- **Playwright's Download API was never called** in any cell.

### RTT measured this session (ICMP, n≥8, avg/min/max)

| pair | state | avg | min | max | mdev |
|---|---|---|---|---|---|
| CLIENT-8 → VERSA | native | **13.474 ms** | 8.912 | 22.788 | 4.330 |
| CLIENT-8 → VERSA | netem `30ms` both ways | **71.958 ms** | 69.138 | 79.859 | 2.954 |
| CLIENT-8 → VERSA | netem removed | **11.312 ms** | 9.203 | 13.073 | 1.218 |
| CLIENT-EAST → VERSA | native | **12.063 ms** | 10.360 | 14.181 | 1.264 |
| CLIENT-WEST → VERSA | *(exp43 reference, not re-measured)* | 69.954 ms | 68.481 | 71.582 | 1.105 |

**The netem number is the measured one, not the intended one**, and the mandated correction was applied:
`tc qdisc add dev ens3 root netem delay …` is **egress-only** (it delays the ACK path once and yields only
~+30 ms RTT); both directions were delayed — `root` on `ens3` **and** `ifb0` fed by an ingress `mirred`
redirect. Verified live: `netem 8004: root … delay 30ms` + `ingress ffff:` on `ens3`, `netem 8005: root …
delay 30ms` on `ifb0`. 30 ms each way was chosen over exp41's 28 ms sample because 28 ms measured **66.924 ms**
(too low); 30 ms measures **71.958 ms**, which brackets exp43's 69.954 ms.

### Path controls (in-session, VERSA → client, TCP 1-flow, 10 s)

| pair | UTC | Mbps | retransmits / 10 s | exp43 | exp40 |
|---|---|---|---|---|---|
| VERSA → CLIENT-EAST | ~05:15 | **234.58** | **107** | P1/P2 = 210 / 244 with 125 / 147 retrans | 244.52 with 24 retrans |
| VERSA → CLIENT-8 | ~05:16 | **364.91** | **2696** | — | — |

Today's paths are **as lossy as exp43's session** (107 retransmits vs exp43's 125/147) and **not** exp40's
clean 24. CLIENT-8's path carries **1.56× CLIENT-EAST's TCP capacity** in the same session — which is exactly
why the `taskset` pin (arm A2) was necessary: the 8-vCPU-vs-4-vCPU-host comparison confounds cores with host
capacity, and the pin removes that confound on one host and one path.

### Method note M1 — `pkill -x node` does not kill the driver; SIGKILL is required

The runbook's cleanup (`pkill -x node; pkill -x chrome; pkill -x Xvfb`) **silently failed to kill the
driver's `node`** during the first arm-A cleanup: after `pkill` plus 2 s the same PID (`node drive_click.js`,
the just-finished cell's driver) was **still alive**, while its `chrome` and `Xvfb` descendants were already
gone. A later `kill -TERM <that pid>` killed it, and a later `pkill -x node -e` killed a subsequent driver
normally.

**Mechanism (identified in the later cells): the driver's `node` catches SIGTERM.** Playwright installs a
SIGTERM/SIGINT handler that awaits a graceful browser close; immediately after a 754 MiB service-worker-backed
download there is heavy pending work, so that close can take an arbitrary time and `kill -TERM` *appears* to
do nothing. Every subsequent cell on the verified runner shows the same shape — `TERM name=node …` followed
3 s later by `KILL name=node …` — and then a clean gate. **SIGKILL is required; SIGTERM alone is not
sufficient.** This is a live hazard for any future field agent that trusts the runbook's cleanup line.

Consequence: cell **C1a-2 ran with cell 1's idle, already-completed browser still resident**. It is flagged
DIRTY and not tidied away; its rate (195.093) is +0.7 % over C1a-1 (193.719), so the contamination is not
visible in the number.

A per-cell runner (`CLIENT-8:/tmp/sbcell.sh`, `CLIENT-EAST:/tmp/sbcell.sh`) now (i) TERMs, (ii) waits 3 s,
(iii) KILLs, and (iv) **asserts `node=0 chrome=0 Xvfb=0 firefox=0` and prints `GATE-CLEAN` before launching** —
a dirty gate aborts the cell (exit 9) and is never retried through. It also records NIC `rx_bytes` before
launch and starts a bounded `vmstat 1 500`. Every cell below records its gate line.

## Raw cells — C1

Agent-side windows; `Mbps = 6,325.010432 / window_s`. All cells CLIENT-8 unless stated. Every valid cell's
mode was direct (relay standby expired unused at the timestamp shown in the notes).

### Arm A (C1-a) — CLIENT-8, 8 vCPU, native RTT, knob **ON** (32768)

| # | UTC lanes ready | UTC download complete | window s | **Mbps** | count | gate | notes |
|---|---|---|---|---|---|---|---|
| C1a-1 | 05:09:01.540908294 | 05:09:34.191413820 | 32.650506 | **193.719** | 31 | clean (manual pre-runner check: all 0) | first cell; driver log clean apart from the pre-existing harmless `join() re-entered` app warning; standby expired unused 05:09:45 |
| C1a-2 | 05:10:31.783871871 | 05:11:04.204358360 | 32.420486 | **195.093** | 32 | **DIRTY** — cell 1's idle `node` survived `pkill` (M1) | ran alongside an idle, already-completed browser; flagged, not tidied away |
| C1a-3 | 05:12:33.407397130 | 05:13:15.616128195 | 42.208731 | **149.851** | 33 | **GATE-CLEAN** | first cell on the verified runner; NIC RX delta 3,155,204,112 → 4,035,849,362 = **880,645,250 B** for a 791,301,804 B DataChannel payload, so the volume is real (+11 % UDP/SCTP wire overhead), not a short transfer |

**Arm A: n=3, 193.719 / 195.093 / 149.851 — mean 179.554, range 149.9–195.1 (spread 30 %).**
Client CPU in C1a-3 (`vmstat 1`, 8 vCPUs, `us+sy` over the 42.2 s window): **3.2–4.0 cores busy, peak 4.0** —
the 8-vCPU client is **not saturated** at 150–195 Mbps. (This build's `vmstat` repeats its header every ~22
rows; a naive column read picks the header up as `id=0` and fabricates an 8-core peak. These numbers are real
data rows only.)

### Arm A2 (control) — CLIENT-8 **pinned to 4 vCPU** (`taskset -c 0-3`), native RTT, knob **ON**

Same host, same path, same session, same container as arm A — **only the CPU budget changes**. Pin verified
live: `taskset -cp` on the `node` driver and on every `chrome` process returned `0-3`.

| # | UTC lanes ready | UTC download complete | window s | **Mbps** | count | gate | notes |
|---|---|---|---|---|---|---|---|
| C1a-pin4-1 | 05:16:19.624931722 | 05:17:11.197259887 | 51.572328 | **122.643** | 34 | GATE-CLEAN | 32.7 % of 8 vCPUs busy = **2.62 cores of the 4 pinned** |
| C1a-pin4-2 | 05:17:32.944715833 | 05:18:25.340584484 | 52.395869 | **120.716** | 35 | GATE-CLEAN | pin re-verified mid-flight on node + 4 chrome PIDs |

**Arm A2: n=2, 122.643 / 120.716 — mean 121.680, range 120.7–122.6.**

### Arm A3 (control) — CLIENT-EAST, real 4 vCPU, ~12.1 ms, knob **ON**

| # | UTC lanes ready | UTC download complete | window s | **Mbps** | count | gate | notes |
|---|---|---|---|---|---|---|---|
| CE-a-1 | 05:19:06.355469138 | 05:20:00.408352587 | 54.052883 | **117.015** | 36 | GATE-CLEAN | RTT 12.063 ms this session |
| CE-a-2 | 05:20:36.775560072 | 05:21:37.847304258 | 61.071744 | **103.567** | 37 | GATE-CLEAN | |

**Arm A3: n=2, 117.015 / 103.567 — mean 110.291, range 103.6–117.0** vs exp43's same config
**117.799 (n=3, 107.9–132.9)** → **−6.4 %**, and this arm's best cell is inside exp43's range. That is what
licenses the cross-session comparison to exp43's numbers below.

### Arm C (C1-c) — CLIENT-8, 8 vCPU, netem **71.958 ms**, knob **ON**

| # | UTC lanes ready | UTC download complete | window s | **Mbps** | count | gate | notes |
|---|---|---|---|---|---|---|---|
| C1c-1 | 05:22:40.405491555 | 05:23:51.708017698 | 71.302526 | **88.707** | 38 | GATE-CLEAN | direct, standby expired unused 05:23:24 |
| C1c-2 | 05:24:25.416178311 | 05:25:25.817275503 | 60.401097 | **104.717** | 39 | GATE-CLEAN | direct, standby expired unused 05:25:09 |

**Arm C: n=2, 88.707 / 104.717 — mean 96.712, range 88.7–104.7.**

### Arm B (C1-b) — CLIENT-8, 8 vCPU, native RTT, knob **UNSET** (production config)

Container recreated 05:41:07Z with `run-variant.sh u`; `docker inspect` shows **no `SB_SCTP_*`**.

| # | UTC lanes ready | UTC download complete | window s | **Mbps** | count | gate | notes |
|---|---|---|---|---|---|---|---|
| C1b-1 | 05:41:31.348465048 | 05:42:12.434713372 | 41.086248 | **153.945** | 44 | GATE-CLEAN | direct, standby expired unused 05:42:15 |
| C1b-2 | 05:42:45.704570030 | 05:43:31.402648349 | 45.698078 | **138.409** | 45 | GATE-CLEAN | direct, standby expired unused 05:43:30 |
| C1b-3 | 05:43:58.770675530 | 05:44:40.247093358 | 41.476418 | **152.497** | 46 | GATE-CLEAN | direct, standby expired unused 05:44:43 |

**Arm B: n=3, 153.945 / 138.409 / 152.497 — mean 148.283, range 138.4–153.9.**

### Arm D (C1-d) — CLIENT-8, 8 vCPU, netem ~71.5 ms, knob **UNSET**

RTT re-verified before this arm: **min 69.703 / avg 71.521 / max 74.018 / mdev 1.360 ms**.

| # | UTC lanes ready | UTC download complete | window s | **Mbps** | count | gate | notes |
|---|---|---|---|---|---|---|---|
| C1d-1 | 05:46:02.827786061 | 05:48:45.864972175 | 163.037186 | **38.795** | 47 | GATE-CLEAN | direct, standby expired unused 05:46:46 |
| C1d-2 | 05:49:23.809625007 | 05:51:56.791897940 | 152.982273 | **41.345** | 48 | GATE-CLEAN | direct, standby expired unused 05:50:07 |

**Arm D: n=2, 38.795 / 41.345 — mean 40.070, range 38.8–41.3.**

## The C1 answer table — 8 vCPU vs exp43's 4 vCPU

| arm | client / RTT | knob | n | cells (Mbps) | **mean** | range | exp43 4-vCPU | **8-vCPU ÷ 4-vCPU** |
|---|---|---|---|---|---|---|---|---|
| **C1-a** | CLIENT-8 8 vCPU, 13.474 ms | **32768** | 3 | 193.719 / 195.093 / 149.851 | **179.554** | 149.9–195.1 | 117.799 | **1.52×** |
| **C1-b** | CLIENT-8 8 vCPU, 13.474 ms | **unset** (production) | 3 | 153.945 / 138.409 / 152.497 | **148.283** | 138.4–153.9 | 95.757 | **1.55×** |
| **C1-c** | CLIENT-8 8 vCPU, 71.958 ms (netem) | **32768** | 2 | 88.707 / 104.717 | **96.712** | 88.7–104.7 | 59.326 | **1.63×** |
| **C1-d** | CLIENT-8 8 vCPU, 71.958 ms (netem) | **unset** (production) | 2 | 38.795 / 41.345 | **40.070** | 38.8–41.3 | 13.405 | **2.99×** |
| — | **A2 control:** CLIENT-8 *pinned* 4 vCPU, 13.474 ms | 32768 | 2 | 122.643 / 120.716 | **121.680** | 120.7–122.6 | 117.799 | **1.03×** |
| — | **A3 control:** CLIENT-EAST 4 vCPU, 12.063 ms | 32768 | 2 | 117.015 / 103.567 | **110.291** | 103.6–117.0 | 117.799 | 0.94× |

**Two independent readings of the same result:**

1. **Different hosts:** 8 vCPU beats exp43's 4 vCPU by **1.52× to 2.99×** in all four configurations.
2. **Same host, same path, same container** (`taskset` pin): 179.554 → **121.680 = 1.48×**, and 121.680 lands
   **+3.3 %** on exp43's real 4-vCPU CLIENT-EAST (117.799) and **+10.3 %** on this session's real 4-vCPU
   CLIENT-EAST (110.291) — despite CLIENT-8's TCP path carrying **1.56× more** than CLIENT-EAST's.

Reading 2 is the decisive one: doubling the CPU budget on **one machine** reproduces the whole
4-vCPU-vs-8-vCPU-host difference. The 4-vCPU browser ceiling is **not** the host, the NIC cap or the path.

### The knob's effect on an 8-vCPU client (same client, same session, one env var)

| RTT | knob OFF | knob ON | effect | exp43's same comparison on 4 vCPU |
|---|---|---|---|---|
| ~13.5 ms | 148.283 | 179.554 | **+21.1 %** | +23.0 % |
| ~72 ms | 40.070 | 96.712 | **+141.4 % (2.41×)** | **4.43×** |

The knob still helps at both RTTs and still helps more at 70 ms, but **the relative benefit at 70 ms collapses
from 4.43× to 2.41×** because a bigger client improves the *knob-OFF* arm far more (2.99×) than the knob-ON arm
(1.63×). **The production 70 ms number is the most CPU-limited arm in the whole matrix.**

## C2 — browser identity (Firefox, bare receiver)

### The deploy, and two blocking defects in EXP44's script

The served client at the start of this run was the pristine 91,308 B `app.js` (`407ee703…`);
`grep -c BARE_RECEIVE_MODE` = **0**. The bare receiver was deployed to the TESTBOX test web root with EXP44's
`agent/deploy_bare_client.sh deploy bare`.

**⚠ `deploy_bare_client.sh` does not work over ssh as committed.** EXP44's own caveat 2 named the unexercised
parts ("`ssh`/`scp` transport, `sudo install -o root -g root`, and the HTTP served-bytes check") — those are
exactly the parts that are broken. Both defects reproduced on first real use:

1. **`remote_script` cannot pass a multi-file fileset over ssh.** It runs
   `ssh "${SSH_OPTS[@]}" "$SB_TESTBOX_HOST" bash -s -- "$@"`. The `bare` fileset is a newline-joined 3-file
   string, so sshd's login shell **re-splits it into three commands**:
   ```
   bash: line 2: src/bareReceive.js: No such file or directory
   bash: line 3: src/bulkReorder.js: No such file or directory
   ```
   and the manifest comes back with **one file only**, exit 127. It works for a 1-line fileset and it works
   under `SB_LOCAL_SIM=1` — which is precisely how the EXP44 self-test passed and why the defect shipped.
2. **`cmd_remote_backup` creates `$dir` as root, then the local side `scp`s `manifest.before` into it** as the
   unprivileged ssh user → `scp: dest open "…/manifest.before": Permission denied`.

Both were worked around by running a **copy** of the script with two minimal fixes; **the repo file was not
modified** (the copy is at `/tmp/dbc/agent/deploy_bare_client.sh`, with `REPO_ROOT` reached through
`/tmp/dbc/signaling-server` → symlink to the repo, so `LOCAL_SRC` resolves to the same bytes):
(i) `remote_script` builds one single-quoted command string so embedded newlines survive as a single argv
element; (ii) the backup dir is `chmod 777` after creation. `diff` against the committed file shows only those
two hunks (12 lines). **Every assertion the script makes still ran.**

The failure sequence: one attempt on the **committed** script died even earlier on
`SSH_OPTS[@]: unbound variable` (macOS bash 3.2 + `set -u` + an empty array — worked around by setting
`SB_SSH_OPTS`, never by editing the file); the next two hit defect 1 and then defect 2; a fourth on the
partially-patched copy hit defect 2 again; the fifth reached `DEPLOY-OK`. None of the failures wrote a
web-root file (the install step runs last), and the partial `/tmp/sb-bare-backup/<session>` they left was
removed with `sudo rm -rf` before the successful deploy.

**These two defects are themselves a deliverable of this experiment — they invalidate EXP44's operational
claim that the script is ready to run against TESTBOX, and they show that `SB_LOCAL_SIM=1`-only proof (plus
`bash -n`) does not exercise 'the only unexercised parts' it claimed to leave — it exercises none of them.**

**`DEPLOY-OK`** — all six assertions passed (**repo == deployed == served**):

| file | sha256 | size | before deploy |
|---|---|---|---|
| `src/app.js` | `10b53ec90e2a3728556e5edce06130b95fd7d93582ef814e7414549983987706` | 94,811 | `407ee703…` (91,308 B) |
| `src/bareReceive.js` | `83cb881e733a8ec3159adfd1151f16fcc4bea4dd07e9e2dee182a4e4bd25cf52` | 4,291 | **ABSENT** |
| `src/bulkReorder.js` | `97eae95694676a2ddafaf589d83f764709d4a77ae5e8de112f47253b94e95a84` | 2,879 | **ABSENT** |

`app.js` and `bareReceive.js` hashes are byte-identical to EXP44 §1.2. Session id
`20261013T052808Z-bare`.

### Firefox bare cells — CLIENT-8, ~13.5 ms, knob ON, `?bare=1`, Playwright Firefox (Gecko)

Driver `~/sbtest/drive_bare_ff.js`; **never touches the Playwright Download API**. Byte gate:
`wire − 14 × frames == 790,626,304` exactly.

| # | UTC lanes ready | UTC download complete | agent window s | **agent Mbps** | driver `BARE-ACCOUNTING` | frames | gate | mode |
|---|---|---|---|---|---|---|---|---|
| C2-ff-1 | — | — | — | **RELAY-ABORT** | — | — | GATE-CLEAN | relay (`relay channel started successfully` 05:28:44.476, **no `lanes ready` ever**) |
| C2-ff-2 | 05:29:56.422195619 | 05:30:52.477434394 | 56.055239 | **112.835** | `payload=790626304 frames=48267 wire=791302042 match=true window_s=56.24 mbps=112.47` | 48,267 | GATE-CLEAN | **direct** (badge `● Connected (Direct)`, standby expired unused 05:30:40) |
| C2-ff-3 | — | — | — | **RELAY-ABORT** | — | — | GATE-CLEAN | relay (`relay channel started successfully` 05:31:58) |
| C2-ff-4 | — | — | — | **RELAY-ABORT** | — | — | GATE-CLEAN | relay |
| C2-ff-5 | — | — | — | **RELAY-ABORT** | — | — | GATE-CLEAN | relay |
| C2-ff-6 | 05:36:53.284848444 | 05:37:50.103720218 | 56.818872 | **111.319** | `payload=790626304 frames=48259 wire=791301930 match=true window_s=58.26 mbps=108.57` | 48,259 | GATE-CLEAN | **direct** (standby expired unused 05:37:37) |

**Firefox bare, n=2 valid of 6 attempts: 112.835 / 111.319 — mean 112.077, range 111.3–112.8 (spread 1.3 %).**
Both valid cells are **byte-exact** — every byte of the 754 MiB file accounted for.

UA reported by the successful cells:
`Mozilla/5.0 (X11; Linux x86_64; rv:155.0) Gecko/20100101 Firefox/155.0` — Playwright's **bundled juggler
build** (`firefox-1543`), **not** the apt Firefox 157. The claim C2 supports is therefore "a different browser
engine (Gecko/usrsctp)", not "stock Firefox 157".

### Chromium bare cells — like-for-like, same client, same hour, same container

| # | UTC lanes ready | UTC download complete | agent window s | **agent Mbps** | driver `BARE-ACCOUNTING` | frames | gate |
|---|---|---|---|---|---|---|---|
| C2-cr-1 | 05:38:36.305726302 | 05:39:09.541660133 | 33.235934 | **190.306** | `payload=790626304 frames=48262 wire=791301972 match=true window_s=34.16 mbps=185.16` | 48,262 | GATE-CLEAN |
| C2-cr-2 | 05:39:46.415934374 | 05:40:13.145264816 | 26.729330 | **236.632** | `payload=790626304 frames=48261 wire=791301958 match=true window_s=28.13 mbps=224.86` | 48,261 | GATE-CLEAN |

**Chromium bare, n=2: 190.306 / 236.632 — mean 213.469, range 190.3–236.6 (spread 24 %).**

### The C2 answer table

| comparison | value |
|---|---|
| **bare-vs-bare, same hour** | Firefox **0.525×** Chromium (112.077 / 213.469). Every Firefox cell is ≥ 40 % below every Chromium cell — the arms do not overlap. |
| **bare-vs-app** | Chromium bare 213.469 vs Chromium **app** (arm A) 179.554 → bare is **1.189×** the app, i.e. the app sink costs **~16 %** on this 8-vCPU client. E28's 4-vCPU bare-vs-real was 1.137×, i.e. the same story at a smaller magnitude. |
| **Firefox establishment** | **4 of 6 attempts relay-fell-back (67 %)**; 2/6 produced a number. |

**Firefox / Chromium = 0.525× is the headline: this is a *receive-path engine* difference.** Same client, same
path, same hour, same agent container, same share, and in both cases the app's entire receive pipeline
(envelope decode, chunk assembly, SHA-1, sink, DOM) is removed — only the browser's own DataChannel receive
remains. So the difference is `usrsctp` + ~1 MiB rwnd vs `dcSCTP`, not the sink, host, path or app JS.

**Firefox establishment reliability — reported as an observation, not a finding.** Both successes were the two
`DEBUG_CONSOLE=1` runs and all four non-DEBUG runs relayed (**2/2 vs 0/4**, Fisher exact p ≈ 0.067). With n=6
this may be coincidence. A plausible mechanism exists — driver-side logging changes the timing of
`page.click('#join-section button')` relative to the app's own `join()`, and both browsers log the pre-existing
`join() re-entered while browser signaling socket is still active` error — but it was **not established**.
Chromium never relayed in this experiment: **0 of 16** Chromium cells (14 on CLIENT-8 — 12 app-path + the 2
bare cells above — plus the 2 arm-A3 controls on CLIENT-EAST).

**The practical C2 conclusion:** E36's verdict ("Firefox cannot be driven this way") is **wrong as stated** —
Firefox can be driven and delivers a byte-exact transfer — but it is **right in effect for measurement
purposes**, because its *direct* establishment here is unreliable enough (2/6) that a Firefox number must
always carry its attempt count and can never be read as a like-for-like single-cell comparison. Anyone
re-measuring browser identity must budget for ~3 attempts per valid cell.

## Interpretation

**1. The answer to the brief's question.** The 8-vCPU client lands **nowhere near** the 4-vCPU client: +52 %,
+55 %, +63 % and +199 % across the four configurations. **The client CPU is the cap and the v1-direct verdict
reopens.** The pin control (arm A2) makes this a same-host, same-path, same-container result rather than a
cross-host inference, and it lands on exp43's real 4-vCPU number to within 3 %.

**2. But it is not raw CPU exhaustion — it is serialization/queueing.** Neither arm is saturated: at 12 ms the
unpinned 8-vCPU client uses **3.2–4.0 of 8** cores, and the pinned 4-vCPU client uses **2.62 of its 4**. Yet
halving the CPU budget halves the throughput. The browser receive pipeline (network-service SCTP/DTLS →
renderer `onmessage` → sink) has a critical path that cannot be parallelized further, and extra cores relieve
the *contention and queueing* around it, not a saturating loop. This is exactly the mechanism E28 flagged and
could not establish ("the mechanism by which ~0.2 core of app JS costs 1.47× at a pinned client is not
established here") — **C1 reproduces that shape at 4 vCPU on a different host, with the pin as the only
variable.** E28's "serialized receive path" framing **survives**; what dies is the inference that 4 vCPU is
enough to be out of its way.

**3. The production configuration is the most damaged and benefits most.** Production ships **no
`SB_SCTP_*`** (pion defaults). On an 8-vCPU client the knob-off 72 ms arm is **40.070 Mbps** vs 13.405 on
4 vCPU — a **2.99×** rescue, the largest ratio in the matrix, against 1.63× for the knob-ON arm. So the
already-alarming exp43 finding ("production does ~13 Mbps at 70 ms, not the 64.5 the campaign quotes") was
partly a **client-hardware** effect: on a 500 Mbps 8-vCPU client production does ~40 Mbps at 70 ms. It is
still 2.4× worse than the knob-ON configuration, so the fix still matters — but the absolute production number
is client-dependent, which exp43 could not see.

**4. Browser identity is a real, first-order receive-path variable (C2).** With the app pipeline removed
entirely, Gecko delivers **0.525×** what Blink delivers on the same client in the same hour. Everything the
campaign has measured is Chromium/dcSCTP; a Firefox user on the same path would see roughly half the rate, and
that is not a sink, host or app-code effect. It is worth stating explicitly because "the browser receive path
is the cap" (E25/E27/E28) was always measured on one engine.

**5. The knob's 12 ms benefit transfers to the bigger client; its 70 ms benefit shrinks relatively.** +21.1 %
at 12 ms on 8 vCPU vs exp43's +23.0 % on 4 vCPU — essentially unchanged, so exp43's headline 12 ms knob effect
is **not** a client-hardware artefact. At 70 ms it falls from 4.43× to 2.41× because the bigger client improves
the knob-OFF arm more. The knob's *direction* is reproduced on a different client both times.

**6. Lab/field asymmetry, and what this does NOT fix.** All of this is one client shape (b2-30, 8 vCPU,
500 Mbps, US-EAST-VA-1) on one day with paths that are as lossy as exp43's (107 retransmits / 10 s vs exp43's
125/147, not exp40's clean 24). The scaling is measured only at **4 and 8 vCPU (and only unpinned)**, so there
is still no gradient between 4 and 8 and no confirmation that 8 vCPU is the top. Nothing here addresses the
~234 Mbps payload ceiling or explains why TCP gets 244–365 Mbps while SCTP stops lower.

## Caveats

- **Small n.** n=3 / 3 / 2 / 2 for the four C1 arms, n=2 for each control arm, n=2 for Firefox, n=2 for
  Chromium bare, **of 6 Firefox attempts**. The arm *separations* are far outside the within-arm spread in
  every case, but the means carry that spread.
- **Arm A's three cells span 30 % (149.9–195.1)** — by far the widest spread here, and wider than exp43's
  4-vCPU arm B (107.9–132.9). The 8-vCPU plateau may be unstable, or CLIENT-8's noisier path (RTT mdev
  4.33 ms at native, vs CLIENT-EAST's 1.26) may be responsible. The pin control's own spread is only 1.6 %,
  so the *ratio* in arm A2 is solid, but arm A's mean should be read with that caveat.
- **The 8-vCPU-vs-exp43 comparison is cross-session.** It is mitigated, not eliminated, by (i) the same-host
  pin control, (ii) the same-session real-4-vCPU arm A3 reproducing exp43 to −6.4 %, and (iii) in-session TCP
  controls on both hosts. The pin control is the claim to trust; the cross-session ratios are support.
- **The CPU pin is not a 4-vCPU box.** `taskset -c 0-3` on an 8-vCPU host shares cache/memory bandwidth and
  the same physical socket(s); a real b2-15 differs. Arm A2's agreement with real 4-vCPU hosts (exp43's
  CLIENT-EAST and A3) is evidence that the pin is a good proxy, not proof that it is identical.
- **CPU accounting is aggregate `vmstat`.** Idle/`us+sy` percentages across vCPUs cannot exclude one
  saturated thread inside a lower aggregate — the same caveat exp43 and exp28 carried. The pinned client's
  **2.62 of 4 cores** is therefore an *aggregate* that does not prove no single thread was pegged.
- **The Firefox/Chromium ratio rests on n=2 vs n=2, and the Chromium bare spread is 24 %** (190.3–236.6). The
  arms do not overlap (Firefox max 112.8 < Chromium min 190.3) so the *direction* is safe, but the 0.525×
  magnitude carries that spread.
- **Firefox is Playwright's bundled Gecko (rv:155.0), not stock Firefox 157.** C2 supports "a different
  browser engine", not "stock Firefox". No geckodriver-based driver was written.
- **The 2/2-vs-0/4 DEBUG correlation is an observation, not a finding.** It is reported because it is the only
  difference between the direct and relayed attempts that was recorded, and because a future agent should know
  to retry rather than conclude Firefox "cannot" do direct.
- **One cell is DIRTY** (C1a-2, M1) and is retained as measured. C1a-1 predates the automated runner, so its
  gate line is from a manual pre-launch check (all counts 0), not from `sbcell.sh`.
- **The agent-side window includes session setup.** `lanes ready` precedes the click by ≤0.09 s in every cell —
  ≤0.3 % of a 32 s window. Browser windows are second-resolution in the agent log, so ±1 s ⇒ ≤3 % at 32 s.
- **`iperf3` was installed on CLIENT-8** (it was absent; CLIENT-EAST already had it) to obtain the TCP path
  control. It is **left installed** so that CLIENT-8's toolset matches CLIENT-EAST's. Flagged as a change.
- **Not run (budget):** a CPU-pin gradient on CLIENT-8 (2 vCPU, 6 vCPU), a native `fieldrecv` arm on CLIENT-8
  to give the 8-vCPU client a non-browser reference point, and any repeat of the knob alternation at 8 vCPU.

## Restoration — verified

**CLIENT-8 (final verification output, all confirmed):**
- netem **removed and confirmed**: `tc qdisc show dev ens3` = stock `qdisc fq_codel 0: root …`;
  `ip link show ifb0` = `Device "ifb0" does not exist`.
- **RTT restored**: `min 9.203 / avg 11.312 / max 13.073 / mdev 1.218 ms` (native baseline was 13.474 avg).
- **`rmem` sysctls: `net.core.rmem_default = 212992`, `net.core.rmem_max = 212992`** — never changed by this
  experiment, verified unchanged at the end.
- **All processes zero**: `node=0 chrome=0 Xvfb=0 firefox=0 vmstat=0 iperf3=0`.
- `iperf3` installed (see caveat) — the only persistent change to the VM.

**CLIENT-EAST**: `node=0 chrome=0 Xvfb=0 firefox=0 vmstat=0 iperf3=0`, stock `fq_codel`,
`rmem 212992 / 212992`. Only driven for the two arm-A3 control cells.

**CLIENT-WEST: never driven, never unshelved, never contacted.**

**TESTBOX — web root byte-identical, `REVERT-OK`.** `deploy_bare_client.sh revert 20261013T052808Z-bare`
printed `REVERT-OK — post-revert manifest is identical to the recorded pre-deploy manifest`, and was then
verified independently:

| check | result |
|---|---|
| `sha256sum /opt/sharebridge-test/web/src/app.js` | `407ee7032f90e4298ee6b0416ed69dbf94147d0c6ff0933e7c6803f129bdf3c1` (the exact pre-change hash) |
| size / mode / owner | **91,308 B**, `644`, `root:root` |
| `src/bareReceive.js`, `src/bulkReorder.js` | **removed** (both were ABSENT before the deploy) |
| `grep -c BARE_RECEIVE_MODE app.js` | **0** |
| fresh HTTP `curl http://127.0.0.1:8080/src/app.js \| sha256sum` | `407ee703…` — **served bytes identical** |
| `systemctl is-active sharebridge-test caddy` | **active / active** |
| `/tmp/sb-bare-backup`, `/tmp/sb-bare-stage` | removed |

**VERSA — production untouched.** **49 containers up** (same count as exp43's hand-back);
`sharebridge-agent` (7878) `running=true`, `StartedAt 2026-10-12T19:54:16Z` (predates this run);
both `127.0.0.1:7878` and `127.0.0.1:7879` listening. The only container acted on was `sb-run`, recreated once
via `run-variant.sh u`. `run-variant.sh` was **executed, never modified**; **no `docker build`** was run.

**Rig state on hand-back:** `sb-run` = `sb-agent:pristine`, **`SB_SCTP_CA_STEP` unset** (knob OFF), `UI_PORT=7879`,
running and authenticated, with the campaign share's session reconnected. **No instance was shelved, stopped, created,
resized or deleted** — the campaign is still in flight (flagged for the operator, as exp43 did).

**No `git` command of any kind was run.** The only file written inside the repo is this results file.

## Artifacts

- This file: `docs/superpowers/spikes/results/2026-10-13-exp45-client-scaling-and-firefox.md`
- **Agent-side logs are the metric and live only in `docker logs sb-run` on VERSA.** The container was
  recreated once (knob ON → OFF), so the ON-arm raw log was dumped before that: **`VERSA:/tmp/exp45-sb-run-knob-on.log`**
  (264 lines, covers arms A/A2/A3/C and all of C2). Every window in this file is transcribed above with
  nanosecond timestamps as `docker logs --timestamps` printed them.
- **`CLIENT-8:/tmp/sbcell.sh`** — the verified per-cell runner (gate + NIC counters + `vmstat`). A copy is on
  **`CLIENT-EAST:/tmp/sbcell.sh`**.
- **`/tmp/dbc/agent/deploy_bare_client.sh`** (this Mac) — the minimally-patched copy of
  `agent/deploy_bare_client.sh` that actually works over ssh, with the two fixes and the `diff` evidence in
  the C2 section above. `/tmp/dbc/signaling-server` is a symlink into the repo so `LOCAL_SRC` resolves.
- Driver logs on CLIENT-8: `/tmp/cell-C1a-{1,2,3}.log`, `/tmp/cell-C1a-pin4-{1,2}.log`,
  `/tmp/cell-C1c-{1,2}.log`, `/tmp/cell-C1b-{1,2,3}.log`, `/tmp/cell-C1d-{1,2}.log`,
  `/tmp/cell-C2-ff-{1..6}.log`, `/tmp/cell-C2-cr-{1,2}.log`; `vmstat` streams `/tmp/<label>.vmstat`.
  Driver logs on CLIENT-EAST: `/tmp/cell-CE-a-{1,2}.log`, `/tmp/CE-a-{1,2}.vmstat`.
- Driver logs from the two failed deploy attempts: `/tmp/deploy-trace.out` / `/tmp/deploy-trace.err` (this Mac).
- Share consumption: **30 → 48 downloads** (18 completed transfers; 4 Firefox relay aborts consumed none).
