#!/usr/bin/env python3
"""Pool E2's loss-0 raw runs (rtt 12/71, 64MiB, samples every 100ms) for stall
structure statistics, using the same stall definitions as analyze_e1.py."""
import json
from statistics import median

MI = 1 << 20

def sample_metrics(s, final):
    n = len(s)
    first_idx = next((i for i, v in enumerate(s) if v > 0), None)
    if first_idx is None:
        return None
    last_idx = next((i for i, v in enumerate(s) if v >= final), n - 1)
    deltas = [s[i] - s[i-1] for i in range(first_idx + 1, last_idx + 1)]
    deltas_active = deltas[:-1] if deltas else []
    med = median(deltas_active) if deltas_active else 0
    thr = 0.10 * med
    stalls = []
    i = 0
    while i < len(deltas_active):
        if deltas_active[i] < thr:
            j = i
            while j < len(deltas_active) and deltas_active[j] < thr:
                j += 1
            a = first_idx + 1 + i
            stalls.append({"dur_ms": (j - i) * 100, "frac": s[a-1] / final if final else 0})
            i = j
        else:
            i += 1
    def t_frac(f):
        tgt = f * final
        for i in range(first_idx, last_idx + 1):
            if s[i] >= tgt:
                return (i - first_idx) * 100
    return {"ttfb": first_idx*100, "t50": t_frac(0.5), "t90": t_frac(0.9),
            "active_ms": (last_idx-first_idx)*100, "med": med, "stalls": stalls}

def pos(f):
    return "early" if f < 1/3 else ("mid" if f < 2/3 else "tail")

runs = []
for line in open("/Users/ali/Git/ShareBridge/.worktrees/benchdirect/docs/superpowers/spikes/results/raw/exp2-results.jsonl"):
    e = json.loads(line)
    if e.get("status") != "ok":
        continue
    r = e.get("result") or {}
    if r.get("mode") != "raw" or r.get("rtt_ms") not in (12, 71):
        continue
    if r.get("received_bytes") != 64 * MI:
        continue
    m = sample_metrics(r.get("samples") or [], r["received_bytes"])
    if not m:
        continue
    m.update(tag=e["tag"], rtt=r["rtt_ms"], mbps=r["mbps"],
             dg_mi=(r["per_conn"][0]["shim_fwd"])/(r["received_bytes"]/MI))
    runs.append(m)

print(f"pooled E2 loss-0 runs: {len(runs)}")
for rtt in (12, 71):
    g = [r for r in runs if r["rtt"] == rtt]
    fast = [r for r in g if r["mbps"] > 100]
    slow = [r for r in g if r["mbps"] <= 100]
    print(f"\n== rtt {rtt}: n={len(g)} (fast {len(fast)}, slow/collapsed {len(slow)}) ==")
    for lbl, sub in (("fast", fast), ("slow", slow)):
        if not sub:
            continue
        nst = sum(len(r["stalls"]) for r in sub)
        tot = sum(s["dur_ms"] for r in sub for s in r["stalls"])
        lng = max((s["dur_ms"] for r in sub for s in r["stalls"]), default=0)
        posn = {}
        for r in sub:
            for s in r["stalls"]:
                p = pos(s["frac"])
                posn.setdefault(p, []).append(s["dur_ms"])
        print(f" {lbl}: runs={len(sub)} stalls_total={nst} "
              f"(avg {nst/len(sub):.1f}/run) stall_ms_total={tot} (avg {tot/len(sub):.0f}/run) "
              f"longest={lng}ms; by position: " +
              ", ".join(f"{k}:{len(v)}x/{sum(v)}ms" for k, v in sorted(posn.items())))
        for r in sorted(sub, key=lambda x: x["mbps"])[:4]:
            sts = ", ".join(f"{pos(s['frac'])}@{s['frac']:.2f}:{s['dur_ms']}ms" for s in r["stalls"]) or "-"
            print(f"   {r['tag']} mbps={r['mbps']:.0f} ttfb={r['ttfb']} t50={r['t50']} t90={r['t90']} stalls[{sts}]")
