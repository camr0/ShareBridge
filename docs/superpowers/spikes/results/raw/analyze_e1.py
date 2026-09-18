#!/usr/bin/env python3
"""E1 analysis: wire-overhead ratio, retransmit fraction, ramp/stall structure.

Inputs: /tmp/exp1/results.jsonl (envelope with full result JSON per run),
        /tmp/exp1/sanity_pre.jsonl, /tmp/exp1/sanity_post.jsonl.
Outputs per run: mbps, fwd, drop, datagrams/MiB, excess vs 1344.5, TTFB,
t50/t90 from first byte, stall count/duration/positions, longest stall.
"""
import json, math, statistics as st

MI = 1 << 20
BASELINE_DG_PER_MI = 1344.5

def load_runs(path):
    runs = []
    with open(path) as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            env = json.loads(line)
            if env.get("status") != "ok" or not env.get("result"):
                runs.append({"tag": env["tag"], "status": env.get("status"),
                             "stderr": env.get("stderr", "")[:200]})
                continue
            r = env["result"]
            r["_tag"] = env["tag"]
            r["_rtt"] = env.get("rtt", r.get("rtt_ms"))
            r["_loss"] = env.get("loss")
            r["_bw"] = env.get("bw", "0")
            r["_q"] = env.get("queue", "0")
            r["_load0"] = env.get("load0", "")
            runs.append(r)
    return runs

def sample_metrics(r):
    s = r.get("samples") or []
    final = r["received_bytes"]
    n = len(s)
    # first sample with bytes (TTFB relative to page load)
    first_idx = next((i for i, v in enumerate(s) if v > 0), None)
    if first_idx is None:
        return None
    # active window: until cumulative reaches final
    last_idx = next((i for i, v in enumerate(s) if v >= final), n - 1)
    deltas = [s[i] - s[i - 1] for i in range(first_idx + 1, last_idx + 1)]
    # drop the final partial-window delta (window containing last byte)
    deltas_active = deltas[:-1] if deltas else []
    med = st.median(deltas_active) if deltas_active else 0
    thr = 0.10 * med
    stalls = []
    i = 0
    dd = deltas  # index j in dd corresponds to window ending at sample first_idx+1+j
    while i < len(deltas_active):
        if deltas_active[i] < thr:
            j = i
            while j < len(deltas_active) and deltas_active[j] < thr:
                j += 1
            # sample index at stall start/end (absolute)
            a = first_idx + 1 + i      # cumulative at stall START = s[a-1]
            b = first_idx + 1 + j
            stalls.append({
                "dur_ms": (j - i) * 100,
                "bytes_before": s[a - 1],
                "frac": s[a - 1] / final if final else 0,
            })
            i = j
        else:
            i += 1
    def t_frac(f):
        tgt = f * final
        for i in range(first_idx, last_idx + 1):
            if s[i] >= tgt:
                return (i - first_idx) * 100
        return None
    return {
        "ttfb_ms": first_idx * 100,          # from page load (incl. ICE/DTLS)
        "t50_ms": t_frac(0.5),               # from first byte
        "t90_ms": t_frac(0.9),
        "active_ms": (last_idx - first_idx) * 100,
        "n_samples": n,
        "median_delta": med,
        "stalls": stalls,
        "stall_count": len(stalls),
        "stall_ms": sum(x["dur_ms"] for x in stalls),
        "longest_stall_ms": max((x["dur_ms"] for x in stalls), default=0),
    }

def run_metrics(r):
    pc = r.get("per_conn") or [{}]
    fwd = sum(c.get("shim_fwd", 0) for c in pc)
    drop = sum(c.get("shim_drop", 0) for c in pc)
    we = sum(c.get("shim_write_err", 0) for c in pc)
    recv = r["received_bytes"]
    dg_per_mi = fwd / (recv / MI)
    m = sample_metrics(r) or {}
    m.update({
        "tag": r["_tag"], "rtt": r["_rtt"], "loss": r["_loss"],
        "bw": r["_bw"], "q": r["_q"],
        "mbps": r.get("mbps"), "wall_mbps": r.get("wall_mbps"),
        "elapsed_ms": r.get("elapsed_ms"),
        "recv": recv, "fwd": fwd, "drop": drop, "werr": we,
        "dg_per_mi": dg_per_mi,
        "excess_per_mi": dg_per_mi - BASELINE_DG_PER_MI,
        "err": r.get("error", ""),
        "chrome_cores": r.get("chrome_cores"),
        "load0": r["_load0"],
    })
    return m

def stall_pos(f):
    return "early" if f < 1/3 else ("mid" if f < 2/3 else "tail")

def main():
    import sys
    runs = load_runs("/tmp/exp1/results.jsonl")
    ok = [r for r in runs if r.get("mbps") is not None]
    bad = [r for r in runs if r.get("mbps") is None]
    print(f"== {len(runs)} runs, {len(ok)} ok, {len(bad)} not-ok ==")
    for r in bad:
        print("NOT-OK:", r)
    ms = [run_metrics(r) for r in ok]
    # group by cell
    cells = {}
    for m in ms:
        key = (m["rtt"], str(m["loss"]), m["bw"], m["q"])
        cells.setdefault(key, []).append(m)
    hdr = ("cell", "n", "mbps(mean/min/max)", "dg/MiB", "excess/MiB", "drop(sum)",
           "ttfb_ms", "t50", "t90", "stalls(n/tot_ms/longest/pos)")
    print("\t".join(hdr))
    for key in sorted(cells, key=lambda k: (k[0], float(k[1]), len(k[2]))):
        g = cells[key]
        mb = [x["mbps"] for x in g]
        dg = [x["dg_per_mi"] for x in g]
        drops = sum(x["drop"] for x in g)
        pos = []
        sc = tm = lm = 0
        ttfb = t50 = t90 = []
        for x in g:
            sc += x["stall_count"]; tm += x["stall_ms"]
            lm = max(lm, x["longest_stall_ms"])
            for s in x["stalls"]:
                pos.append(f"{stall_pos(s['frac'])}@{s['frac']:.2f}({s['dur_ms']}ms)")
            if x["ttfb_ms"] is not None: ttfb.append(x["ttfb_ms"])
            if x["t50_ms"] is not None: t50.append(x["t50_ms"])
            if x["t90_ms"] is not None: t90.append(x["t90_ms"])
        avg = lambda v: f"{st.mean(v):.0f}" if v else "NA"
        cell = f"rtt{key[0]}_l{key[1]}_bw{key[2]}_q{key[3]}"
        print("\t".join([
            cell, str(len(g)),
            f"{st.mean(mb):.1f}/{min(mb):.1f}/{max(mb):.1f}",
            f"{st.mean(dg):.1f}", f"{st.mean(dg)-BASELINE_DG_PER_MI:+.1f}",
            str(drops), avg(ttfb), avg(t50), avg(t90),
            f"{sc}/{tm}ms/{lm}ms/{','.join(pos) if pos else '-'}",
        ]))
    # per-run detail dump for the results file
    print("\n== per-run ==")
    for m in ms:
        sts = "; ".join(f"{stall_pos(s['frac'])}@{s['frac']:.2f}:{s['dur_ms']}ms" for s in m["stalls"]) or "-"
        print(f"{m['tag']}\tmbps={m['mbps']:.1f}\tfwd={m['fwd']}\tdrop={m['drop']}\t"
              f"dg/MiB={m['dg_per_mi']:.1f}\texc={m['excess_per_mi']:+.1f}\t"
              f"ttfb={m['ttfb_ms']}\tt50={m['t50_ms']}\tt90={m['t90_ms']}\t"
              f"stalls={m['stall_count']}/{m['stall_ms']}ms/long{m['longest_stall_ms']}ms\t[{sts}]")

if __name__ == "__main__":
    main()
