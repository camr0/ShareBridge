#!/usr/bin/env python3
"""E2 analysis: aggregate /tmp/exp2/results.jsonl into per-cell mean/min/max."""
import json, sys, collections

path = sys.argv[1] if len(sys.argv) > 1 else "/tmp/exp2/results.jsonl"
rows = []
with open(path) as f:
    for line in f:
        line = line.strip()
        if line:
            rows.append(json.loads(line))

# cells: (mode, rtt, rtomax) -> list of mbps values; track anomalies
cells = collections.defaultdict(list)
fails = []
shimwrite = []
for r in rows:
    key = (r["mode"], r["rtt"], r["rtomax"])
    if r.get("status") != "ok":
        fails.append((r["tag"], r.get("status"), r.get("rc"), r.get("stderr", "")[:200]))
        cells[key].append(None)
        continue
    res = r["result"]
    per = res.get("per_conn") or [{}]
    we = sum(p.get("shim_write_err", 0) for p in per)
    drop = sum(p.get("shim_drop", 0) for p in per)
    if we > 0:
        shimwrite.append((r["tag"], we))
    cells[key].append({
        "tag": r["tag"], "mbps": res["mbps"], "wall": res.get("wall_mbps", 0),
        "write_err": we, "drop": drop,
        "fwd": sum(p.get("shim_fwd", 0) for p in per),
        "chrome_cores": res.get("chrome_cores", 0), "go_cpu": res.get("go_cpu_seconds", 0),
        "err": res.get("error", ""), "load0": r.get("load0", ""), "wall_s": r.get("wall_s"),
    })

# aggregate (exclude cells with shim_write_err>0 from the stats, per method rules)
agg = {}
for key, vals in cells.items():
    good = [v for v in vals if isinstance(v, dict) and v["write_err"] == 0]
    if not good:
        agg[key] = None
        continue
    m = [v["mbps"] for v in good]
    agg[key] = {"n": len(m), "mean": sum(m) / len(m), "min": min(m), "max": max(m), "vals": m}

order = ["0", "500ms", "200ms"]
for mode in ["raw", "prod"]:
    print(f"\n=== {mode} ===")
    print(f"{'rtt':>4} {'rtomax':>7} {'n':>2} {'mean':>8} {'min':>8} {'max':>8} {'delta%':>8}")
    for rtt in [12, 25, 71, 100]:
        base = agg.get((mode, rtt, "0"))
        for rt in order:
            a = agg.get((mode, rtt, rt))
            if a is None:
                print(f"{rtt:>4} {rt:>7} {'--':>2} {'FAILED / no ok runs':>34}")
                continue
            d = "" if (rt == "0" or not base) else f"{(a['mean']/base['mean']-1)*100:+.1f}%"
            print(f"{rtt:>4} {rt:>7} {a['n']:>2} {a['mean']:>8.1f} {a['min']:>8.1f} {a['max']:>8.1f} {d:>8}")

print("\n=== per-run detail (anomalies: writeErr>0, drop>0, err, or |z|>25%) ===")
for key in sorted(cells, key=lambda k: (k[0], k[1], order.index(k[2]))):
    a = agg.get(key)
    if a is None:
        print(f"  {key}: no successful runs")
        continue
    for v in cells[key]:
        if not isinstance(v, dict):
            continue
        flag = []
        if v["write_err"] > 0: flag.append(f"writeErr={v['write_err']}")
        if v["drop"] > 0: flag.append(f"drop={v['drop']}")
        if v["err"]: flag.append(f"err={v['err']}")
        if a["mean"] and abs(v["mbps"] - a["mean"]) / a["mean"] > 0.25: flag.append("outlier>25%")
        if flag:
            print(f"  {key} {v['tag']}: mbps={v['mbps']:.1f} wall_s={v['wall_s']} " + " ".join(flag))

if fails:
    print("\n=== FAILED RUNS (verbatim) ===")
    for t in fails:
        print(f"  {t[0]}: status={t[1]} rc={t[2]} stderr={t[3]!r}")
if shimwrite:
    print("\n=== runs with shim_write_err>0 (excluded from stats) ===")
    for t in shimwrite:
        print(f"  {t[0]}: write_err={t[1]}")
print(f"\ntotal runs recorded: {len(rows)}")
