# Overnight experiment runbook — 2026-09-18

Companion to `2026-09-17-transport-tier1-tier2-experiments.md` (the plan + Experiment 9 results).
Every agent running an experiment tonight MUST read this file first and obey §1 absolutely.

## 0. Why these experiments, in this order

Experiment 9 (field, 2026-09-18) found that **N-striping does not scale**: at ~12 ms N=1→N=2 = 1.01×,
at ~71 ms 1.09×. Meanwhile the path carried 156 Mbps over 4 TCP flows, the agent sat at 1.53% CPU,
and the client went 63% → 85% busy as N went 1 → 2. So the leading hypothesis is a **client-side sink
cap** (browser userspace + SHA-1 verify + service-worker writes), not the transport, path, or agent.

Tonight we run the cheap/decisive tests first: the ones that need **no code change** (existing flags),
then the ones that need a small harness change. Order of dispatch is easiest-win-first.

## 1. HARD SAFETY RULES — violating any of these fails the run

1. **NEVER touch production.** On `VERSA` (home host) the following are production and off-limits:
   container **`sharebridge-agent`** (UI port **7878**, created 2026-08-13), and every non-ShareBridge
   container (`ringbearer`, `hermes`, `qwen-vllm`, `open-webui`, `jellyfin`, `immich-*`, `komodo-core`,
   `backrest`, …). Do not start/stop/restart/exec them. Do not read their env.
2. **The experiment agent is `sb-run` ONLY** — image `sb-agent:pristine`, `UI_PORT=7879`,
   `SB_SCTP_CA_STEP=32768`, host networking, on `VERSA`. Its API is
   `http://127.0.0.1:7879/api/v1/shares`, key `SHAREBRIDGE_AGENT_API_KEY` in
   `VERSA:/home/ali/sharebridge-test/env-run`. Port 7879 is the *test slot*; the v2 test agent
   `sharebridge-agent-test` also uses 7879 and is **mutually exclusive** with `sb-run` — leave it
   stopped. **If an experiment seems to need port 7878 or the production container, STOP.**
3. **Never touch the v2 relay stack** (on TESTBOX or `VERSA`) or the v2 test agent. It is
   intentionally stopped for the duration of the v1 work.
4. **Never deploy to production**, never touch DNS/ACME/namespaces, never create or modify anything
   outside the test namespace.
5. **No IPs, hostnames, credentials, API keys, share codes or `jti`s in any committed file.** The real
   hosts live in `.worktrees/e2e-v1/INFRA.local.md` (gitignored) — read it, but write `VERSA`,
   `TESTBOX`, `CLIENT-EAST`, `CLIENT-WEST` into results files.
6. **Never shelve, stop, delete or resize any instance.** The operator does infra teardown.
7. **One writer per file.** Never edit a file another agent is writing. **Do NOT run any `git` command**
   (`git add` / `git commit` / `git checkout` / history ops) — several agents share this worktree and the
   index; the orchestrator commits results files serially. Just write your file and report it.
8. **Read-only on `VERSA` except the experiment agent.** Prefer `docker logs sb-run`. Never
   `docker restart` anything.
9. **Report honestly** — including "could not run", failed cells and negative results. Never tidy away
   contradictory data. Report the *raw* numbers you measured, not a rounded story.
11. **ONE FIELD EXPERIMENT AT A TIME.** Every field cell shares the same agent (`sb-run`) and the same
   two client VMs, so two concurrent field runs silently corrupt each other's numbers (and can fight
   over the same share's download budget). Before starting a field cell: confirm no other field agent
   is active, then list driver/browser processes on BOTH client VMs and **abort the cell if an
   unexpected `node`/`chrome`/`Xvfb` is running** — that turns silent interference into a detected
   abort. Report any such abort rather than retrying through it. (Added 2026-09-18 after a wedged
   field agent was replaced while still nominally alive.)
12. **NEVER `pkill` the client VMs unless you are certain you are the only field agent running.** A
   cleanup `pkill -x node|chrome|Xvfb` destroys whatever cell is in flight — including another
   agent's. This actually happened on 2026-09-18: a stuck agent's exit cleanup killed the live
   agent's just-started cross-host cell. If you are not sure, leave the processes and report them.
10. **If the rig is unhealthy** (agent 7879 down, signalling not HTTP 200, a client VM unreachable):
    STOP and report. Do not improvise infrastructure changes.

## 2. Layout

| thing | where |
|---|---|
| Lab harness source | `.worktrees/benchdirect/agent/cmd/benchdirect` (this Mac — NOT the field rig) |
| Lab runners (existing) | `.worktrees/benchdirect/agent/run_matrix.sh`, `run_matrix2.sh`, `stats_matrix.sh` |
| Field rig facts / wake-up | `.worktrees/e2e-v1/INFRA.local.md` (gitignored — never quote into committed files) |
| Field driver | `CLIENT-*:~/sbtest/` — `drive.js`, `drive_click.js`, `drive_keep.js`, `run_cell2.sh`, `analyze.py` |
| **Results files** | `docs/superpowers/spikes/results/<date>-exp<N>-<slug>.md` — your own file only |

### How to actually reach the hosts (do not hunt for ssh aliases)

`VERSA`, `TESTBOX`, `CLIENT-EAST` and `CLIENT-WEST` are **documentation placeholders, not ssh config
names.** Concretely:

- `VERSA` → `ssh versa` (resolves directly; this is the only one that works by name)
- `TESTBOX`, `CLIENT-EAST`, `CLIENT-WEST` → look up their IPs in `.worktrees/e2e-v1/INFRA.local.md`
  and connect as `ssh ubuntu@<ip>` (non-root user).

Do not spend tool calls searching `~/.ssh/config` for aliases — read `INFRA.local.md` and use the IPs.
(A previous agent burned 14 tool calls and ran zero cells because of exactly this gap.)

## 3. Lab harness — how to run it

```bash
cd .worktrees/benchdirect/agent
go build -o bin/benchdirect ./cmd/benchdirect
./bin/benchdirect --mode raw|prod --rtt <ms> --loss <frac> --size <N>MiB --chunk <N>KiB \
  --backpressure poll|event [--rtomax 500ms] [--cwndcastep 32KB] [--mincwnd 2MiB] \
  [--conns N] [--sharing shared|independent] [--bandwidth 8MB] [--queue 5MB] [--jitter N] \
  --out /tmp/bench.json
```

JSON: `mbps`, `wall_mbps`, `received_bytes`, `samples` (cumulative bytes each 100 ms), per-conn
`shim_fwd` (datagrams forwarded) / `shim_drop` / `shim_write_err`, `chrome_cpu_seconds` /
`chrome_cores`, `go_cpu_seconds`.

**Roles matter for interpretation:** in the lab the **sender is a real Chrome tab** (chromedp) and the
**receiver is Go** (`window.__bench.received`), shimmed by a UDP-loopback bottleneck that models
delay/loss/jitter/bandwidth/queue. In the field the **receiver is Chrome + the app's download sink**.
So lab numbers are an upper bound for anything involving the client-side sink — say so when reporting.

**Method rules:** n≥3 per cell; report mean/min/max; **run lab cells strictly one at a time** (a single
`benchdirect` process at a time — throughput here is CPU-sensitive); discard a cell with
`shim_write_err>0`; sanity check that `--rtt 71 --mode raw --size 32MiB` gives ≈100 Mbps before
trusting a sweep.

### ⚠️ LAB BENCH RELIABILITY (2026-09-18) — read before designing a lab experiment

E2 found this Mac cannot currently produce trustworthy **throughput means**: the sanity config varied
**19.1–245.9 Mbps over 7 repeats (12.8×)**. Identified sources: `mds` (Spotlight), a Time Machine
backup (~91% CPU, 51 MB/s), another agent's `grep` pegged at 99% CPU, and load averages 3–7.4.

Consequences — design lab experiments around the metrics that survive:

- **Robust:** the *ceiling* (best-case plateau of a cell), **wire datagram/byte volume** per MiB
  delivered, `shim_drop`, and any code-level fact. E2's ceiling table was identical across `--rtomax`
  values to within ~1% despite the noise.
- **Unreliable tonight:** mean/median throughput comparisons between cells, and anything where a
  modest percentage difference is the result. A cell can collapse into a stalled regime for reasons
  unrelated to the knob under test.
- If you must compare means: re-run the baseline immediately before and after the sweep in the same
  block, report the baseline spread as the bench's error bar, and state clearly when a difference is
  inside that error bar. Do not report a delta the noise can explain.
- Record `uptime`, `sysctl vm.swapusage` and the top CPU consumers with each sweep block.

## 4. Field rig — how to run it

Health check first (all three must pass, else STOP and report):
1. `ssh VERSA 'docker ps --format "{{.Names}}" | grep -x sb-run'` → prints `sb-run`.
2. `curl -s -o /dev/null -w '%{http_code}' https://TESTBOX/` → `200` (v1 signalling, Caddy TLS).
3. `ssh ubuntu@CLIENT-EAST true && ssh ubuntu@CLIENT-WEST true` → both reachable.

Getting the share URL (do **not** write the code into any repo file):
```bash
ssh VERSA 'set -a; . /home/ali/sharebridge-test/env-run; set +a; \
  curl -s -H "X-API-Key: $SHAREBRIDGE_AGENT_API_KEY" http://127.0.0.1:7879/api/v1/shares' | jq .
```
Reuse the newest share that is unexpired with downloads remaining (Experiment 9 used a 48 h /
`max_downloads=500` share over one 754 MiB file; it expires ~48 h after 2026-09-18 04:00). If none is
valid, STOP and report — do not invent a share body.

Driving (per client VM), click-only driver — **never call Playwright's Download API** (`download.path()`
/ `download.saveAs()` abort the transfers they measure and have produced false "failure" verdicts):
```bash
ssh ubuntu@CLIENT-X 'cd ~/sbtest && SHARE_URL=https://TESTBOX/s/<code> NTABS=<N> \
  setsid nohup xvfb-run -a node drive_click.js > /tmp/cell-<label>.log 2>&1 < /dev/null & echo started'
```
Poll with **short, separate** calls. Never put a backgrounded `nohup` job in the same ssh command as a
`tail` — the channel stays open and the call hangs for minutes.

**Metric — agent-side only** (driver-independent; client-side artefacts are unreliable and were seen
vanishing mid-run):
```bash
ssh VERSA 'docker logs --timestamps --since <t0> sb-run 2>&1 | grep -E "lanes ready|download complete|closed"'
```
Window = first `DataChannel lanes ready` → last `download complete`. Aggregate =
`N_files × 6325.01 Mb ÷ window_seconds`. Each file is 754 MiB = 790,626,304 B = 6,325.01 Mb.
Also sample the agent's CPU (`docker stats --no-stream sb-run`) and, per client VM,
`vmstat 1 2 | tail -1` during the transfer (idle% is an *aggregate* across vCPUs — note in the result
that it cannot exclude one saturated critical thread).

Ops hazards (all reproduced tonight): `pkill -f '<pat>'` self-matches inside an ssh command string and
kills its own shell → use `pkill -x node|chrome|Xvfb`; nested `ssh` inside a polling loop hangs →
sample on the target VM; `timeout` is not on macOS → use `ssh -o ConnectTimeout` + remote `timeout`.

Cleanup after a cell: `pkill -x node; pkill -x chrome; pkill -x Xvfb; pkill -x iperf3` on each VM.

## 5. Experiment specs (dispatch order = easy wins first)

### E2 — RTO floor on the clean path (lab; zero code) ← **first**
Hypothesis: a lower RTO floor cuts spurious retransmission on a clean path. The earlier dismissal
("no drops ⇒ no RTOs") came from a *loss-induced* lab context and may be wrong on the clean path.
Cells: rtt {12, 25, 71, 100} × `--rtomax` {default(0), 500ms, 200ms} × mode raw (poll) × size 64MiB × n=3.
Report: mean/min/max Mbps per cell + the delta vs default, and the same for `--mode prod`.
Note: `--rtomax` below 1 s also lowers the effective RTO floor (per the flag's own help).

### E6 — chunk size at high RTT (lab; zero code) ← **second**
Existing sweep compared 16 KiB vs 64 KiB at rtt=0 only, where neither matters.
Cells: rtt {25, 71, 100} × chunk {16 KiB, 64 KiB, 256 KiB} × loss {0, 0.001} × mode raw × 64MiB × n=3.
If a cell errors (SCTP max-message-size), record the error verbatim as a result.

### E1 — retransmit / wire-overhead diagnosis (lab; zero code) ← **third**
Hypothesis: at `loss=0` the shim drops nothing, so any wire overhead is **spurious** retransmission or
queue overflow — not path loss. Competing hypothesis: real path loss.
Method: run `loss=0` and `loss=0.001` at rtt {12, 71} × 64MiB × n=3, and compute per cell:
`shim_drop` (must be 0 at loss=0), datagrams forwarded (`shim_fwd`) vs datagrams *expected* from
payload ÷ mean datagram size, i.e. the wire/goodput ratio, and retransmit-attributable extra datagrams.
Also report the `samples` array's stall structure (the Sept-15 traces showed long 0-Mbps plateaus).
Decides: whether the `wire ÷ ~3 = goodput` headroom is software-side (fixable) or path loss (not).

### E5 — three never-swept pion knobs (lab; **needs code — see note**) ← **fourth**
`SB_SCTP_FAST_RTX_WND` (`SetSCTPFastRtxWnd`), `SB_SCTP_MAX_RX_BUF` (`SetSCTPMaxReceiveBufferSize`),
`SB_SCTP_MAX_MSG` (`SetSCTPMaxMessageSize`) — already env-wired in `agent/internal/peer/peer.go`.

> **CORRECTION (2026-09-18, from E2):** in the harness these are **NOT** wired for `--mode prod`.
> `runProd` (`prodbench.go:119-124`) builds its `SettingEngine` with only
> `SetIncludeLoopbackCandidate` and `SetSCTPMinCwnd`, and there is **no `SB_SCTP*` env lookup anywhere in
> the benchdirect tree**. So a prod-mode env sweep measures nothing. E2 proved this the hard way:
> `--rtomax` is applied only at `rawbench.go:225`, and all 24 prod runs that passed `--rtomax 200ms`
> recorded `"rto_max_ms": 0`. **You must first add the flag/env wiring to `prodbench.go`** (mirror
> `rawbench.go`), then sweep. It is a code experiment, not a zero-code one.

Sweep each over ~4 sensible values (read the pion adapter for accepted ranges; record the value's
effect, including "no effect" and "error") × {rtt 12, 71} × mode prod × 64MiB × n=2.
Confirm with a code read that the env var is actually consumed before claiming a null result.

### E3a — app-level pacing (lab; small code change) ← **fifth**
Add a token bucket to the harness send loop (`rawbench.go` / `prodbench.go`), rate-limit to ~1.2×
measured delivery, no fork. Cells: pacing on/off × rtt {12, 71, 100} × N=1 × 64MiB × n=3.
Report goodput, wire ratio, retransmit proxies. Stage 3b (pacer inside the pion fork) is **out of
scope tonight** — write the recommendation instead.

### E4 — `BufferedAmount` time series (lab; small code change) ← **sixth**
Sample `BufferedAmount` every 100 ms during a transfer alongside wire counters; emit to the JSONL.
Report the queue-depth envelope against wire rate and the stall structure time-aligned. This is the
direct observable for the "unbudgeted send queue" claim.

### E7 — ACK-direction loss (lab; shim change) ← **seventh**
The shim's loss model is symmetric; add direction-aware loss (data vs ack) at the flow level.
Cells: rtt {25, 71} × {data 1%, ack 1%, both 1%, none} × 64MiB × n=2.
Report whether ACK loss alone reproduces the collapse currently attributed to data-direction loss.

### E8b — cross-host striping (field; zero code) ← **dispatched early, runs in parallel with lab work**
Hypothesis (re-framed after Exp 9): the field ceiling is **per client host**, not per agent.
- Baselines already measured at the same payload/tuning: `CLIENT-WEST` N=1 = **61.4 Mbps**,
  `CLIENT-EAST` N=1 = **97.3 Mbps** (window 103 s / 65 s).
- Cell X: 1 tab on `CLIENT-EAST` **and** 1 tab on `CLIENT-WEST`, started within a few seconds of each
  other. Aggregate from agent-side timestamps.
  - ≈ **158 Mbps** (sum of the two singles) ⇒ each host ran at its own independent rate ⇒ the
    per-host-client cap is real, and the agent+transport can do ≥158 Mbps aggregate (vs the 67 Mbps
    same-host N=2 result). **This is the decisive number.**
  - ≈ **67–100 Mbps** ⇒ the cap is shared (per-agent or transport), contradicting the client-sink
    hypothesis.
- Cell Y (if time): 2 tabs on each host (4 sessions, 2 hosts). Report the aggregate and how many
  sessions completed.
- Controls to re-measure on the same cell: `iperf3` versa→CLIENT-WEST 1-flow and 4-flow.
- Watch for peer `closed` / takeover entries with timestamps — **at N=4 one of four sessions died
  early in both regimes tonight**, and the candidate cause is the one-connection-per-API-key takeover
  (`agent/internal/hub.go:20`). Report exactly which session died and when.

## 6. Result-file template

```markdown
# Experiment <N/name> — <date>
**Verdict:** <one line, the answer>
**Setup:** <flags/env/matrix; hosts as VERSA/TESTBOX/CLIENT-*>
**Raw:** <table of every cell: cell, n, mean, min, max, notes>  (include failures)
**Interpretation:** <what it means for v1 vs v2 — be explicit about lab/field asymmetry>
**Caveats:** <what this does NOT establish>
**Artifacts:** <paths of raw JSON/logs, e.g. /tmp/... — copies for the record>
```

## 7. Reporting back (keep the reply short)

Reply with **≤200 words**: experiment, verdict, the key numbers, anything that blocks, and the results
file path. No raw logs in the reply — put them in the results file. Flag immediately (don't wait) if
you hit a safety-rule conflict, a broken rig, or a result that contradicts Experiment 9.
