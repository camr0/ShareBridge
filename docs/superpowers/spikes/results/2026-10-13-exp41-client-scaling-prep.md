# Experiment 41 (prep) — 8-vCPU client VM (C1) + Firefox bare driver (C2)

**Date:** 2026-10-13 · **Status:** prep COMPLETE (no field cell run) · **Agent:** EXP41-prep
**Scope:** infrastructure + driver development ONLY. No transfer cell was run against the live field
rig — another agent held it. Nothing on VERSA / TESTBOX / CLIENT-EAST / CLIENT-WEST was started,
stopped or modified; CLIENT-EAST was read only (`scp` pulls of `~/sbtest`).

## 0. Why

The v1-direct verdict's last unverified assumption is *"a bigger client VM won't help"* — that is
inference, not measurement. E30 measured a CPU-pin gradient of **39.0 / 68.3 / 119.2 Mbps at
1 / 2 / 4 vCPU**, still rising at 4 cores. **C1** needs an 8-vCPU client to falsify it. **C2**
(browser identity) needs a working **Firefox** driver: E36 failed because Playwright Firefox wrote no
download artifact and the app's save path treats Firefox as harmful. Fix = drive the **bare
byte-counting handler** (E28) instead of the app's save path.

Two things were produced:
1. a new 8-vCPU client (placeholder **`CLIENT-EAST-8`**) with the full `~/sbtest` rig + Firefox;
2. **`drive_bare_ff.js`**, a Firefox version of `drive_bare.js`, smoke-tested on the new VM.

## 1. The new instance

Real name / instance id / public IP / SSH user are recorded in **`.worktrees/e2e-v1/INFRA.local.md`
(gitignored)** under `sb-client-8` — runbook rule 5 keeps them out of this file.

| property | value |
|---|---|
| placeholder | **`CLIENT-EAST-8`** |
| region | US-EAST-VA-1 |
| flavor | **b2-30** — 8 vCPU / 30 GB / 200 GB disk, **500 Mbps inbound** (`inboundBandwidth`) |
| flavor id | `0f197347-66f9-4316-af5b-2d9ef8bb7d76` |
| image | Ubuntu 24.04, id `40c82cd6-c9d0-466a-a1f8-21d0057a9afd` — **the same image as the other two VA clients** (verified against the running id of CLIENT-EAST) |
| ssh key | `M4 Air` (the same key the other clients use) |
| status | **ACTIVE, not shelved** — operator shelves at campaign end |
| `nproc` | **8** |
| RAM | 28 GiB visible (`free -h`: 28Gi total, 25Gi free), **no swap** |
| kernel / OS | `6.8.0-106-generic` · Ubuntu 24.04.4 LTS |
| NIC | `ens3` (also the default route dev) |
| CPU model | `Intel Core Processor (Haswell, no TSX)`, 8 sockets × 1 core × 1 thread |

Created **without `--wait`** (known gotcha) and polled with `ovhcloud cloud instance list`:
`BUILD` at t+0, `ACTIVE` + public IP at **t+50 s**; sshd answered ~90 s after `ACTIVE`.

```bash
ovhcloud cloud instance create US-EAST-VA-1 --name sb-client-8 \
  --flavor 0f197347-66f9-4316-af5b-2d9ef8bb7d76 \
  --boot-from.image 40c82cd6-c9d0-466a-a1f8-21d0057a9afd \
  --network.public --ssh-key.name "M4 Air" --billing-period hourly
# then: ovhcloud cloud instance list -o json  (poll, no --wait)
```

**Why b2-30 also removes a confound for C1:** b2-15 caps ingress at **250 Mbps** (the runbook's
"~240 Mbps ingress cap"; also E39's ~245 Mbps path ceiling), while b2-30 caps at **500 Mbps**. A C1
cell on b2-30 therefore cannot be explained away as "it just hit the same NIC cap" — 4/5/6/8-vCPU
arms up to ~250 Mbps are directly comparable with E30's 4-vCPU point.

### Verified identical to CLIENT-EAST

| component | CLIENT-EAST | CLIENT-EAST-8 |
|---|---|---|
| Node | v22.23.2 | v22.23.3 |
| playwright | **1.63.0** | **1.63.0** (installed via `npm ci` from the copied lockfile) |
| Chromium | chromium-1243 | **chromium-1243** |
| Playwright Firefox | firefox-1543 | **firefox-1543** |
| ffmpeg (playwright) | ffmpeg-1011 | ffmpeg-1011 |
| system Firefox | 157.0 (packages.mozilla.org) | 157.0.1 (same repo + pin) |
| geckodriver | 0.36.0 | 0.36.0 |
| Xvfb | /usr/bin/Xvfb, xvfb-run | same |

Note: `npx playwright install` with no lockfile first pulled **chromium-1248 / firefox-1555** (the
then-latest 1.63.x → a newer browser revision). That was reverted with `npm ci` (lockfile pins
playwright **1.63.0**) and a re-`install`, so the two rigs are on the **same browser revisions**.
Do not run a bare `npx playwright install` on this box without the lockfile present.

## 2. Rig installed on `CLIENT-EAST-8`

`~/sbtest` is a mirror of CLIENT-EAST. Copied with `scp` **from this Mac** (originals pulled from
CLIENT-EAST read-only):
`drive_click.js`, `drive.js`, `drive_bare.js`, `run_cell.sh`, `analyze.py`, `package.json`,
`package-lock.json`, plus the rest of the rig for completeness: `drive_click_ff.js`, `drive_keep.js`,
`drive_saveas.js`, `browser_sink.js`, `ffprobe.js`, `ffdiag.js`, `exp28-sampler.py`, `exp29-launch.sh`,
`exp29-drive_verify.js`, `exp30-launch.sh`, `exp32-launch.sh`, `exp32-drive.js`, `exp32-sampler.py`.
Only `run_cell.sh` exists on CLIENT-EAST (there is no `run_cell2.sh`); the runbook's `run_cell*.sh`
reference resolves to that single file.

Also installed: Node 22 (NodeSource), Xvfb, **Firefox 157.0.1** from `packages.mozilla.org` (same repo
+ same `/etc/apt/preferences.d/mozilla` pin as CLIENT-EAST), **geckodriver 0.36.0** in
`/usr/local/bin`, and Playwright's Chromium + Firefox browsers.

## 3. New file: `drive_bare_ff.js` (Firefox bare driver) — `CLIENT-EAST-8:~/sbtest/drive_bare_ff.js`

A Firefox port of `drive_bare.js`. It keeps the Chromium arm's contract exactly — same
`#join-section button` → `#file-list .file-item` → click sequence, same `window.__bare` counter read,
same accounting gate `payload = wire_bytes − 14 × frames`, same machine-greppable
`BARE-ACCOUNTING` / `TERMINAL` lines — and adds the Firefox-specific items from `drive_click_ff.js`
(headed under Xvfb, badge asserted **before** the click with `RELAY-ABORT` exit 3, `visibilityState` /
`hasFocus` / UA reported, `FF_NO_THROTTLE=1` opt-in for background-timer throttling).

It **never touches the Playwright Download API** (the failure mode that killed E36): with the bare
handler there is no save path to break, so the Firefox "no download artifact" problem cannot occur by
construction.

Differences from `drive_bare.js`, all deliberate:
- `EXPECTED_PAYLOAD` / `ENVELOPE_BYTES` env overrides (defaults **790626304 / 14**, i.e. the field
  behaviour is byte-identical) so the same file can be smoke-tested against a small payload;
- aborts with exit **5** on a badge that never shows Direct/Relay (`ALLOW_MODE_UNKNOWN=1` restores
  `drive_bare.js`'s proceed-anyway behaviour) — the E28 mode assertion is non-negotiable;
- `FF_EXECUTABLE` passthrough (documented as experimental: stock Firefox cannot be launched by
  Playwright because it lacks the juggler protocol).

Exit codes: `0` exact accounting · `2` accounting mismatch/stall · `3` relay abort · `4` no bare
counter · `5` badge never resolved · `1` fatal.

## 4. Smoke tests — **both passed** (on `CLIENT-EAST-8`, never against the field rig)

Served a local mimic of the patched app (`~/sbtest/smoke/index.html`, `python3 -m http.server 8899`,
`127.0.0.1` only). The page mirrors the EXP28 gate exactly: `window.__bare` is defined **only** when
the URL carries `?bare=1`, it has the same `#join-section` / `#file-list .file-item` /
`#connection-type` structure, and clicking the file item pushes 1000 real `ArrayBuffer`s of 1014 wire
bytes → `payload = 1,014,000 − 14×1000 = 1,000,000`. `EXPECTED_PAYLOAD=1000000` makes the same
accounting gate tight.

**(a) Firefox bare counter — PASS**
```
2026-10-13T02:35:27Z [smoke-ff] FF-LAUNCH version=155.0 no_throttle=false executable=(bundled)
2026-10-13T02:35:28Z [smoke-ff] joined
2026-10-13T02:35:28Z [smoke-ff] bare-counter-present
2026-10-13T02:35:28Z [smoke-ff] MODE direct badge="● Connected (Direct)" vis=visible focus=true
2026-10-13T02:35:28Z [smoke-ff] clicked
2026-10-13T02:35:30Z [smoke-ff] t=2.1s payload=1000000 frames=1000 wire=1014000 cum_mbps=3.83
2026-10-13T02:35:30Z [smoke-ff] ACCOUNTING-EXACT
2026-10-13T02:35:30Z [smoke-ff] post-click state vis=visible focus=true ua="... X11; Linux x86_64; rv:155.0 Gecko/20100101 Firefox/155.0"
BARE-ACCOUNTING smoke-ff payload=1000000 frames=1000 wire=1014000 expected=1000000 match=true window_s=2.09 mbps=3.83
TERMINAL smoke-ff cls=bare-exact match=true      (exit 0)
```

**(b) Chromium bare counter (`drive_bare.js`, unmodified) — PASS** (same page, `EXPECTED_PAYLOAD`
patched only inside a throwaway copy because the field file hardcodes 790626304)
```
2026-10-13T02:35:55Z [smoke-cr] joined / bare-counter-present
2026-10-13T02:35:55Z [smoke-cr] MODE direct badge="● Connected (Direct)"
2026-10-13T02:35:57Z [smoke-cr] t=2.0s payload=1000000 frames=1000 wire=1014000 cum_mbps=3.90
BARE-ACCOUNTING smoke-cr payload=1000000 frames=1000 wire=1014000 expected=1000000 match=true window_s=2.05 mbps=3.90
TERMINAL smoke-cr cls=bare-exact match=true      (exit 0)
```

**(c) Negative control — PASS** (no `?bare=1`; the gate must refuse, not invent a number)
```
NO-BARE-COUNTER nb1 — the served app.js is not the EXP28 patched copy (or ?bare=1 missing)   exit 4
NO-BARE-COUNTER nb2 — ...                                                                     exit 4
```

Not a measurement: ~3.8 Mbps is the timer-driven smoke page, not a transfer. What is proven is launch
(real headed Firefox under Xvfb), navigation, join, badge gate, click, live `window.__bare` reads, the
byte-count arithmetic, and the failure modes.

### Display / exit-code caveat (found during the smoke)
`xvfb-run -a` occasionally fails its own cleanup ("problem while cleaning up temporary directory") and
can then leave a **stale `Xvfb` holding the display and return a wrong exit code** — one negative-control
run returned `5` (MODE-UNKNOWN) instead of `4`. Mitigations, both verified working:
- treat the **log lines** (`RELAY-ABORT` / `NO-BARE-COUNTER` / `MODE-UNKNOWN` / `TERMINAL`) as the
  authoritative classification, not the exit code alone; and/or
- run a **persistent display** instead of `xvfb-run` (verified on this VM):
  `Xvfb :99 -screen 0 1920x1080x24 -nolisten tcp -ac &` then `DISPLAY=:99 node drive_bare_ff.js`
  (a stale-lock error `Server is already active for display 99` means a previous `xvfb-run` leaked —
  `pkill -x Xvfb; rm -f /tmp/.X99-lock` first).

## 5. Exact command lines for the C1 / C2 arms

Metric stays **agent-side** (runbook §4): `docker logs --timestamps sb-run` window first
`DataChannel lanes ready` → last `download complete`; each file is 754 MiB = 6,325.01 Mb. The bare
arms produce **no client-side artifact**, so `run_cell.sh`'s `/tmp/playwright-artifacts-*` sampler
does **not** apply — use the driver's own `BARE-ACCOUNTING … match=true` as the accounting gate.

Selection: `taskset -c 0` = 1 vCPU, `taskset -c 0-1` = 2, `taskset -c 0-3` = 4, no mask = 8 (matches
E30's gradient methodology).

**(a) Chromium bare counter** (`drive_bare.js`, 1 tab):
```bash
ssh ubuntu@CLIENT-EAST-8 'cd ~/sbtest && SHARE_URL="https://TESTBOX/s/<code>?bare=1" CELL=c1-cr-8 \
  setsid nohup xvfb-run -a node drive_bare.js > /tmp/cell-c1-cr-8.log 2>&1 < /dev/null & echo started'
# 4-vCPU control point (E30 comparison):
#   ... setsid nohup xvfb-run -a taskset -c 0-3 node drive_bare.js ...
```
`?bare=1` must be part of `SHARE_URL` — `drive_bare.js` does not add it.

**(b) Firefox bare counter** (`drive_bare_ff.js`, 1 tab):
```bash
ssh ubuntu@CLIENT-EAST-8 'cd ~/sbtest && SHARE_URL="https://TESTBOX/s/<code>?bare=1" CELL=c2-ff-8 \
  setsid nohup xvfb-run -a node drive_bare_ff.js > /tmp/cell-c2-ff-8.log 2>&1 < /dev/null & echo started'
# add FF_NO_THROTTLE=1 only as a separate, deliberated arm
# persistent-display variant: DISPLAY=:99 instead of xvfb-run -a
```

**Do not put the backgrounded job and a `tail` in the same ssh command** (runbook hazard), and do not
`pkill` a client VM you are not certain is idle.

## 6. netem — the 70 ms arm of C1

**Interface: `ens3`** (default route dev, confirmed with `ip route get 1.1.1.1`).

Measured, on `CLIENT-EAST-8`, against CLIENT-EAST as a same-region peer (5×`ping`):

| state | RTT avg |
|---|---|
| baseline (no qdisc) | **0.624 ms** |
| `tc qdisc add dev ens3 root netem delay 30ms` | **30.678 ms** |
| + ingress ifb `netem delay 30ms` (both directions) | **60.751 ms** |
| after removal | 0.655 ms |

**CORRECTION to the task's shorthand.** `netem` on `root` delays **egress only**, so a download pays
it once, on the ACK path: `tc qdisc add dev ens3 root netem delay 30ms` yields **+30 ms RTT**, i.e.
~41 ms on the ~11 ms agent path — **not** the 70 ms arm. For ~71 ms you need both directions:

```bash
IF=ens3                                  # on CLIENT-EAST-8
sudo tc qdisc add dev ens3 root netem delay 30ms                     # egress (ACK path)
sudo modprobe ifb
sudo ip link add ifb0 type ifb && sudo ip link set ifb0 up
sudo tc qdisc add dev ens3 handle ffff: ingress
sudo tc filter add dev ens3 parent ffff: protocol ip u32 match u32 0 0 \
     action mirred egress redirect dev ifb0
sudo tc qdisc add dev ifb0 root netem delay 30ms                     # ingress (data path)
# remove (ALWAYS before the next cell; leaving it on silently corrupts a "clean" arm):
sudo tc qdisc del dev ens3 root; sudo tc filter del dev ens3 parent ffff:
sudo tc qdisc del dev ens3 ingress; sudo tc qdisc del dev ifb0 root; sudo ip link del ifb0
# verify: tc qdisc show dev ens3  -> only "qdisc fq_codel 0: root ..." ; ping CLIENT-EAST -> ~0.6 ms
```
`delay 30ms` both ways + ~11 ms agent baseline ≈ **71 ms**. For a 35 ms target on both legs use
`delay 35ms`. Note the two OVH VA boxes are in the **same datacentre (0.6 ms apart)**, so netem must
supply *all* of the latency — the operator's "~11 ms" is the home-agent↔client path, not client↔client.

## 7. Blockers / preconditions before the arms can produce a number

1. **`?bare=1` does not exist right now.** TESTBOX's test copy
   `/opt/sharebridge-test/web/src/app.js` is the **pristine** 91,308-byte file
   (sha256 `407ee7032f90e4298ee6b0416ed69dbf94147d0c6ff0933e7c6803f129bdf3c1`, `root:root`, 2026-09-19)
   — the same pre-patch hash E28 Part 0 recorded. `grep -c BARE_RECEIVE_MODE` = **0**. So both bare
   drivers today would exit `4` (`NO-BARE-COUNTER`, verified by smoke (c)). The E28 patch
   (`results/2026-09-18-exp28-bare-receive-path.md` §Patch) must be re-applied to the **test copy only**
   before either arm runs. Not applied here — that is a change to a field-rig service and was out of
   scope.
2. **Firefox identity caveat for C2.** Playwright launches its own juggler-patched Firefox build
   (`firefox-1543`, reports `rv:155.0`), **not** the apt Firefox 157.0.1 also installed. So C2 as built
   measures *Gecko-family receive path*, which is the defensible claim ("a different browser engine"),
   but not "stock Firefox 157". Driving **stock** Firefox requires WebDriver — `geckodriver 0.36.0` is
   installed on `CLIENT-EAST-8` for exactly that, but a geckodriver-based driver was **not written**.
   Decide which claim C2 is meant to support before running it.
3. One field experiment at a time (runbook rule 11): C1 and C2 cannot share a cell, and neither may run
   while the current agent holds the rig.

## 8. What was NOT done (honestly)

- No transfer cell, against anything — the field rig was left untouched and unmeasured.
- The E28 patch was not applied, so the bare counter is **not live**; smoke evidence is against a local
  mimic, as instructed.
- No tests of `drive_bare_ff.js` against the real app or the real agent (no share, and forbidden here).
- The new instance is **still running** (b2-30, hourly) — shelf it at campaign end; **never delete or
  resize it**. It was left otherwise idle: no `node`/`chrome`/`Xvfb`/http-server processes, no netem
  qdisc (`tc qdisc show dev ens3` = stock `fq_codel`), load ~0.6.
- Mismatch check on `peerRwnd`/`rwnd` semantics etc. is out of scope for a prep task.

## 9. Files

- `CLIENT-EAST-8:~/sbtest/drive_bare_ff.js` — new Firefox bare driver (also staged at
  `/tmp/sbtest-stage/drive_bare_ff.js` on this Mac).
- `CLIENT-EAST-8:~/sbtest/smoke/index.html` — local smoke page (bare gate mimic).
- `.worktrees/e2e-v1/INFRA.local.md` — real name / id / IP (gitignored; appended under `sb-client-8`).
