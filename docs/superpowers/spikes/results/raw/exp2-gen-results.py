#!/usr/bin/env python3
"""Generate the E2 results markdown with tables derived directly from the JSONL."""
import json, re, statistics, collections, glob, os

SRC = "/tmp/exp2/results.jsonl"
ORD = ["0", "500ms", "200ms"]
rows = [json.loads(l) for l in open(SRC) if l.strip()]

TRACE = re.compile(r"shim fwd=(\d+) writeErr=(\d+) drop=(\d+)")
meta = collections.defaultdict(list)
for r in rows:
    if r.get("status") != "ok":
        continue
    res = r["result"]
    per = res.get("per_conn") or []
    fwd = sum(p.get("shim_fwd", 0) for p in per)
    if not per:
        try:
            m = TRACE.search(open(f"/tmp/exp2/{r['tag']}.err").read())
            if m:
                fwd = int(m.group(1))
        except OSError:
            fwd = 0
    recv = res["received_bytes"]
    meta[(r["mode"], r["rtt"], r["rtomax"])].append({
        "tag": r["tag"], "rep": int(r["rep"]), "mbps": res["mbps"],
        "wall": int(r.get("wall_s") or 0),
        "fwd_per_mib": fwd / (recv / 2**20) if recv else 0,
        "cores": res.get("chrome_cores", 0), "load": r.get("load0", "").split()[0] if r.get("load0") else "",
        "err": res.get("error", ""),
    })

def cell_table(mode):
    out = ["| rtt (ms) | `--rtomax` | n | mean | median | min | max | ceiling | runs <50% of ceiling | datagrams/MiB |",
           "|---|---|---|---|---|---|---|---|---|---|"]
    for rtt in (12, 25, 71, 100):
        for rt in ORD:
            v = meta.get((mode, rtt, rt), [])
            if not v:
                out.append(f"| {rtt} | {rt} | 0 | - | - | - | - | - | - | - |")
                continue
            m = [x["mbps"] for x in v]
            ceil = max(m)
            bad = sum(1 for x in v if x["mbps"] < 0.5 * ceil)
            fw = [x["fwd_per_mib"] for x in v if x["fwd_per_mib"] > 0]
            out.append(f"| {rtt} | {rt} | {len(v)} | {statistics.mean(m):.1f} | {statistics.median(m):.1f} | "
                       f"{min(m):.1f} | {ceil:.1f} | {ceil:.1f} | {bad}/{len(v)} | "
                       f"{statistics.mean(fw):.1f} |" if fw else
                       f"| {rtt} | {rt} | {len(v)} | {statistics.mean(m):.1f} | {statistics.median(m):.1f} | "
                       f"{min(m):.1f} | {ceil:.1f} | {ceil:.1f} | {bad}/{len(v)} | - |")
    return "\n".join(out)

def ceiling_table():
    out = ["| rtt (ms) | `--rtomax 0` | `--rtomax 500ms` | `--rtomax 200ms` |", "|---|---|---|---|"]
    for rtt in (12, 25, 71, 100):
        cs = []
        for rt in ORD:
            v = meta.get(("raw", rtt, rt), [])
            cs.append(f"{max([x['mbps'] for x in v]):.1f}" if v else "-")
        out.append(f"| {rtt} | {cs[0]} | {cs[1]} | {cs[2]} |")
    return "\n".join(out)

def collfree_table():
    out = ["| rtt (ms) | `--rtomax` | n kept | mean | min | max | delta of mean vs default |", "|---|---|---|---|---|---|---|"]
    for rtt in (12, 25, 71, 100):
        base = None
        for rt in ORD:
            v = meta.get(("raw", rtt, rt), [])
            if not v:
                continue
            ceil = max(x["mbps"] for x in v)
            keep = [x["mbps"] for x in v if x["mbps"] >= 0.5 * ceil]
            if base is None:
                base = keep
            d = "" if rt == "0" else f"{(statistics.mean(keep)/statistics.mean(base)-1)*100:+.1f}%"
            out.append(f"| {rtt} | {rt} | {len(keep)} | {statistics.mean(keep):.1f} | {min(keep):.1f} | "
                       f"{max(keep):.1f} | {d} |")
    return "\n".join(out)

def per_run_table(mode):
    out = ["| tag | mbps | wall (s) | datagrams/MiB | chrome cores | load0 |", "|---|---|---|---|---|---|"]
    for rtt in (12, 25, 71, 100):
        for rt in ORD:
            for x in sorted(meta.get((mode, rtt, rt), []), key=lambda y: y["rep"]):
                out.append(f"| {x['tag']} | {x['mbps']:.1f} | {x['wall']} | {x['fwd_per_mib']:.1f} | "
                           f"{x['cores']:.2f} | {x['load']} |")
    return "\n".join(out)

# sanity baseline runs (the runbook §3 check: --rtt 71 --mode raw --size 32MiB)
sanity = []
for f, bp in [("/tmp/exp2_sanity.json", "event (runbook default)"),
              ("/tmp/exp2/leakcheck.json", "poll"), ("/tmp/exp2/leak_1.json", "poll"),
              ("/tmp/exp2/leak_2.json", "poll"), ("/tmp/exp2/baseline/sanity_1.json", "poll"),
              ("/tmp/exp2/baseline/sanity_2.json", "poll"), ("/tmp/exp2/baseline/sanity_3.json", "poll")]:
    if os.path.exists(f):
        r = json.load(open(f))
        sanity.append((os.path.basename(f), r["mbps"], r.get("wall_mbps", 0), r.get("chrome_cores", 0), bp))
sanity_lines = "\n".join(f"| {n} | {m:.1f} | {w:.1f} | {c:.2f} | {bp} |" for n, m, w, c, bp in sanity)
svals = [s[1] for s in sanity]
spoll = [s[1] for s in sanity if s[4] == "poll"]

doc = f"""# Experiment E2 — RTO floor on the clean path — 2026-09-18

**Verdict:** On the clean path (`--loss 0`) lowering the SCTP RTO floor via `--rtomax` has **no
measurable effect**: the achievable throughput ceiling is identical for all three `--rtomax` values at
every RTT, and the number of datagrams put on the wire per MiB delivered is unchanged (~1345, i.e. no
extra retransmission). **However**, this bench could not produce a trustworthy *throughput* sweep
tonight: the runbook's own sanity config varied {min(svals):.1f}–{max(svals):.1f} Mbps over {len(svals)} repeats
({max(svals)/min(svals):.1f}x spread; one event-backpressure run plus six poll-backpressure runs), so the per-cell
means below are marked
**load-contaminated** and only the ceiling / wire-volume results should be relied on. Separately, and
independently of load: **`--mode prod` does not wire `--rtomax` at all**, so any prod RTO-floor sweep
measures nothing (see "Findings" 1).

**Setup:** lab harness `agent/cmd/benchdirect` in `.worktrees/benchdirect` on the lab Mac, built at
2026-09-18 02:14 (`go build -o bin/benchdirect ./cmd/benchdirect`; go.mod resolves
`github.com/pion/webrtc/v4 v4.2.11` → `github.com/pion/sctp v1.9.4`). No host outside the Mac was
touched; this was a lab-only experiment, no field rig, no production container.

Matrix as specified: `--mode raw|prod` × `--rtt {{12,25,71,100}}` × `--rtomax {{0,500ms,200ms}}` ×
`--size 64MiB` × `--chunk 16KiB` × `--backpressure poll` × `--loss 0` × n=3, plus extra reps on the
noisy cells (final n = 4–5 for most raw cells). `--deadline` raised from the 30 s default to 120 s so
that a slow (but not stalled) run is not truncated into a false error; this only widens a safety net
and does not change steady-state behaviour. Runs were executed **strictly one `benchdirect` process at
a time**, sequentially, in two blocks:

* block 1 (02:15:42–02:22:02) — full specified matrix, raw then prod, n=3, `--rtomax` order rotated per RTT.
* block 2 (02:23:39–02:26:52) — extended reps, order randomised per rep, to spread drift across cells.
* block 3 (02:29–02:35) — baseline re-validation runs, after block 2 produced 10–39 Mbps cells.

## Findings

**1. `--mode prod` silently ignores `--rtomax` (code read — this one is not load-dependent).**
`--rtomax` is applied only in raw mode: the only call site of `se.SetSCTPRTOMax` outside `forks/` is
`cmd/benchdirect/rawbench.go:225`. `runProd` (`cmd/benchdirect/prodbench.go:119-124`) builds its
`SettingEngine` with only `SetIncludeLoopbackCandidate` and `SetSCTPMinCwnd`; there is no
`SetSCTPRTOMax` call and no `SB_SCTP_RTO*` environment variable anywhere in the tree
(`grep -rn "SB_SCTP" --include=*.go` returns nothing). The flag is parsed and validated
(`main.go:70-72,164-165`) and then never consulted by `runProd`. Confirmed empirically: all 24 prod runs
that passed `--rtomax 200ms`/`500ms` recorded `"rto_max_ms": 0` in their JSON. Consequently the prod
table below is **not a test of the RTO floor** — it is an accidental noise control (and it behaves like
one: prod "deltas" of up to −74 % appear with the flag inert, which is a direct measure of this bench's
noise floor). The prod sweep was therefore not extended beyond the specified n=3.

**2. Mechanism, verified.** With the resolved pion/sctp v1.9.4, RTO is
`rto = min(max(srtt + 4*rttvar, rtoMin), rtoMax)` with `rtoMin = 1000 ms` hard-coded
(`rtx_timer.go`). So `--rtomax` below 1 s does not merely lower a floor — it pins the association's
RTO to exactly `rtomax` (500 ms / 200 ms), because the `max(...)` term is always ≥ 1000. The flag help
text is right. raw mode applies this to the **Go** `SettingEngine`, and in raw mode Go is the **DATA
sender** (`rawbench.go` `pump()` → `dc.Send`; `cmd/benchdirect/web/bench.js` `note()` counts on the
Chrome side). The knob is therefore on the sending association's data path, not an inert peer: if a
lower RTO floor could matter on a clean path, this configuration would show it.

**3. Throughput ceiling is unaffected (robust — immune to the collapse noise).**
Every run of a given RTT reaches the same best-case plateau regardless of `--rtomax`:

{ceiling_table()}

**4. Wire datagram volume is unaffected (robust — same payload in every run).**
Datagrams forwarded per MiB of payload delivered sit at the 1344.5/MiB baseline
(= 2^20/780 B, the fixed pion SCTP fragmentation) in every cell, including the collapsed ones; the worst
single value seen anywhere was 1440.7/MiB (+7 %). At `--loss 0` the shim dropped nothing
(`shim_drop = 0` in all 91 runs), so extra datagrams could only be retransmissions — and there are none.
This is the direct answer to the experiment's premise: **a lower RTO floor does not create spurious
retransmission on a clean path.**

Per-cell datagram/MiB means are in the tables below (raw); no cell departs from the 1344.5 baseline by
more than the run-to-run scatter of that cell itself.

## Raw — specified matrix, `--mode raw`

All 91 runs returned `status=ok`: **0 errors, 0 timeouts, `shim_write_err = 0` and `shim_drop = 0`
everywhere**, so no cell was discarded. "runs <50% of ceiling" = number of runs in the cell that
collapsed to a stalled regime (this is the contamination marker, not a config property).

{cell_table("raw")}

### Collapse-free subset (runs ≥ 50 % of their own cell's ceiling)

Reported because the collapsed runs are a bimodal machine/regime artefact and would otherwise dominate
the means. Deltas are scattered in sign and are not monotone in `--rtomax` (500 ms and 200 ms disagree
in sign at rtt 12, 25 and 100) — i.e. no dose–response:

{collfree_table()}

## Raw — per-run detail

{per_run_table("raw")}

## Raw — wire datagram volume per cell

Already summarised in the table above (`datagrams/MiB` column, mean per cell): range 1344.2–1412.7
across all 12 raw cells, against a baseline of 1344.5. No cell shows retransmission inflation.

## Prod (`--rtomax` NOT wired — noise control only, see Finding 1)

{cell_table("prod")}

### Prod — per-run detail

{per_run_table("prod")}

## Rig health / why the throughput means are not trustworthy

Baseline check exactly as runbook §3 specifies (`--mode raw --rtt 71 --size 32MiB --backpressure poll`,
expected ≈100 Mbps; the orchestrator measured ≈103 Mbps on a clean machine):

| run | mbps | wall mbps | chrome cores | backpressure |
|---|---|---|---|---|
{sanity_lines}

The very first §3 check (`02:14`, event backpressure, the runbook command verbatim) gave 118.6 Mbps and
passed. Repeating the same 32 MiB / rtt 71 measurement {len(spoll)} more times (poll backpressure) gave
**{min(spoll):.1f}–{max(spoll):.1f} Mbps ({max(spoll)/min(spoll):.1f}x)**, including one run at {min(spoll):.1f} Mbps — 5x below the expected
value. The check therefore did **not** reproduce on this machine
tonight, and per the runbook's own stop rule the sweep should not have been trusted; the cells above are
reported as measured-with-evidence rather than as a clean result.

Environment evidence gathered while the sweep ran (all from `uptime`, `sysctl vm.swapusage`, `vm_stat`,
`iostat`, `ps`):

* load average 3.1–7.4 on a 10-CPU Mac for the whole window, from ~24 concurrent user sessions.
* another agent's `grep -rilE VERSA=` (PID 43917) started 02:14:20 and ran through the whole sweep at up
  to ~99 % CPU; macOS `mds`/Metadata at ~109 %; `triald` at 43–60 %; Time Machine `backupd` spiked to
  91 % CPU with ~51 MB/s of disk writes from ~02:25.
* swap: `used = 6366.31M, free = 801.69M` of 7168M, constant for the whole window; compressor holding
  682,737 pages (~10.4 GiB). Idle swap-out rate measured at 1 page / 15 s, so the machine is
  RAM-compressed but **not** actively thrashing — the damage is CPU/scheduling contention, not swap I/O.
* no leaked processes of mine: `chromedp` tears Chrome down correctly — 0 leftover `--headless=new`
  processes after each single run and 0 after both verification runs; 0 stray `benchdirect` processes
  between runs. Earlier "9 Chrome processes" counts were the 1 browser + its helpers *during* a run.

Mechanism of the contamination, from the harness's own counters: `chrome_cores` tracks throughput
almost linearly (0.04 cores at 10.7 Mbps → 1.25 cores at 500 Mbps) while `go_cpu_seconds` stays flat
at ~2.8–4.4 s. So the slow runs are **waiting stalls** (both ends idle, no retransmission, no wire
inflation) — most consistent with a scheduling hiccup knocking the SCTP congestion window down into a
low regime that it does not leave within the transfer. That is a real, time-correlated property of this
bench under load, and it is exactly the phenomenon E1/E4 are designed to characterise; it is **not**
attributable to `--rtomax` (it happens with the flag inert, in prod, and at rtt 12).

## Interpretation

* **For the open v1-vs-v2 question, E2 contributes a negative:** the SCTP RTO floor is not a lever on a
  clean path. The project note's *conclusion* ("a lower RTO floor cannot matter on a clean path") is not
  contradicted by anything measured here. Its stated *reason* ("no drops ⇒ no RTOs") is unproven by this
  run but also unnecessary: even with the RTO demonstrably pinned to 500/200 ms (Finding 2), wire
  datagram volume and achievable ceiling are unchanged. There is no throughput headroom to be had from
  this knob, so it is not part of the answer to the v1/v2 transport question.
* The prod half of this experiment exposes a harness gap rather than a transport result: if the RTO
  floor is ever to be evaluated against the production peer stack, `--rtomax` must first be plumbed into
  `runProd` (a one-line `se.SetSCTPRTOMax(cfg.rtoMax)` mirroring `rawbench.go:225`, plus setting
  `res.RtoMaxMs`). Until then `--mode prod --rtomax …` is a silent no-op and must not be reported as a
  measurement.
* Lab/field asymmetry: this is a loopback lab bench with a shim; the receiver is Chrome in both modes
  (see Caveats), so it does share the client-sink structure of the field — but the harness's collapse
  regime, not the path, dominated the variance tonight.

## Caveats

1. **The throughput means are load-contaminated and should not be quoted** (see rig health). Only the
   ceiling table and the datagram-volume result are load-robust.
2. The RTO value actually applied inside pion is established by **code read only** (Findings 1–2), not
   by direct instrumentation — nothing in the harness JSON reports the live RTO. If someone needs this
   to be airtight, log `rtoManager.getRTO()`/the timer's timeout in the fork.
3. n is small (3–5 per raw cell, 3 per prod cell) with a heavy-tailed distribution; no cell reaches
   conventional significance in a permutation test (all p ≥ 0.07 on raw), so this is "no effect
   detected", not a tight bound. Given the observed spread, an effect smaller than roughly ±20 % cannot
   be excluded even in the collapse-free subset; a clean re-run should target n ≥ 10 with the bench
   validated first.
4. Prod is a no-op for this knob (Finding 1), so **the prod half of the specified E2 matrix is not
   evidence about the RTO floor at all** — it is a noise control, reported as such.
5. Runbook §3 says the lab sender is Chrome and the receiver is Go; the code says the opposite for both
   modes (raw: `pump()`→`dc.Send` in Go, `note()` on the Chrome side; prod: the real
   `transfer.Manager` sends to the Chrome download sink). Both lab modes therefore have **Chrome as the
   receiver/sink**, which is what makes lab numbers an upper bound for client-sink effects — the
   conclusion in §3 holds, but the roles as written are inverted.
6. `--chunk 16KiB` (the harness default) was used; the September chunk sweep used 64 KiB. Absolute
   Mbps here are therefore not comparable with that sweep. Note also that the "good" runs here
   (118–246 Mbps for the 32 MiB sanity config) bracket and exceed the expected ≈100 Mbps, so the
   runbook's sanity target itself did not reproduce on this machine.
7. Only `--loss 0`, `--conns 1`, `--chunk 16KiB`, `--window 5MiB` (default) were exercised. This says
   nothing about the RTO floor under loss, which would require the knob to actually be exercised
   (and would be the natural positive control that was not available for prod).

## Recommendation

Re-dispatch E2 on a quiet machine (load < 1–2, no Time Machine backup, no filesystem-wide `grep` from
other agents) with: (a) the sanity check as an acceptance gate, run 3x and required to be within
±20 %; (b) n ≥ 10 per cell; (c) `--rtomax` plumbed into `runProd` first if the prod half is wanted;
(d) the same two load-robust metrics (ceiling, datagrams/MiB) reported alongside the means, since they
survive the collapse regime that breaks the means. If the window is tight, the cheap decisive subset is
rtt {{71,100}} × `--rtomax` {{0,200ms}} × n=10 in raw only.

## Artifacts

Copied for the record to `docs/superpowers/spikes/results/raw/`:

| file | what |
|---|---|
| `exp2-results.jsonl` | all 91 runs, full JSON (incl. 100 ms `samples` arrays, per-conn shim counters, load at run start) |
| `exp2-cells.md` | the same cell tables in plain text (all-runs + collapse-free + per-run detail) |
| `exp2-baseline-sanity.jsonl` | the 7 sanity-config runs used for the baseline-spread table |
| `exp2-runner-part1.log`, `exp2-runner-part2.log` | chronological run logs with timestamps, wall times, load average |
| `exp2-runner.sh`, `exp2-runner-b.sh`, `exp2-analyze.py`, `exp2-analyze-b.py` | the exact runner and analysis code used |

Not copied (large, transient): the 101 per-run `.json`/`.err` files under `/tmp/exp2/`, including the
representative collapse traces (`raw_rtt100_rto500ms_r5.json` = 10.7 Mbps / 53 s /
`raw_rtt71_rto0_r5.json` = 12.0 Mbps / 48 s). Ask if these should be preserved before `/tmp` is cleared.
"""

dest = "/Users/ali/Git/ShareBridge/.worktrees/benchdirect/docs/superpowers/spikes/results/2026-09-18-exp2-rto-floor.md"
os.makedirs(os.path.dirname(dest), exist_ok=True)
open(dest, "w").write(doc)
print("wrote", dest, len(doc), "bytes")

# plain-text cell tables artifact
with open("/tmp/exp2/cells.md", "w") as f:
    f.write("## raw — specified matrix (all runs)\n\n" + cell_table("raw") + "\n\n")
    f.write("## raw — ceiling per rtt\n\n" + ceiling_table() + "\n\n")
    f.write("## raw — collapse-free subset\n\n" + collfree_table() + "\n\n")
    f.write("## raw — per-run\n\n" + per_run_table("raw") + "\n\n")
    f.write("## prod — control (--rtomax inert)\n\n" + cell_table("prod") + "\n\n")
    f.write("## prod — per-run\n\n" + per_run_table("prod") + "\n")
print("wrote /tmp/exp2/cells.md")
