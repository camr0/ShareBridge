#!/usr/bin/env python3
"""E2 analysis v2: per-cell robust stats, deltas vs default, permutation test,
wire-datagram ratio (from JSON per_conn, or from the stderr trace line for prod),
and stall structure from the 100 ms sample array."""
import json, re, sys, math, random, collections, statistics

random.seed(1234)
path = sys.argv[1] if len(sys.argv) > 1 else "/tmp/exp2/results.jsonl"
ordr = ["0", "500ms", "200ms"]

rows = []
with open(path) as f:
    for line in f:
        line = line.strip()
        if line:
            rows.append(json.loads(line))

TRACE = re.compile(r"shim fwd=(\d+) writeErr=(\d+) drop=(\d+)")

def stats(v):
    v = sorted(v)
    n = len(v)
    return {
        "n": n, "mean": statistics.mean(v), "min": v[0], "max": v[-1],
        "med": statistics.median(v),
        "p25": v[max(0, int(0.25 * (n - 1)))], "p75": v[min(n - 1, int(0.75 * (n - 1)))],
        "sd": statistics.pstdev(v) if n > 1 else 0.0,
    }

def perm_test(a, b, iters=20000):
    """two-sided permutation test on the difference of means"""
    obs = abs(statistics.mean(a) - statistics.mean(b))
    pool = list(a) + list(b)
    na = len(a)
    hits = 0
    for _ in range(iters):
        random.shuffle(pool)
        if abs(statistics.mean(pool[:na]) - statistics.mean(pool[na:])) >= obs:
            hits += 1
    return hits / iters

cells = collections.defaultdict(list)
meta = collections.defaultdict(list)
fails, writeerrs = [], []
for r in rows:
    key = (r["mode"], r["rtt"], r["rtomax"])
    if r.get("status") != "ok":
        fails.append((r["tag"], r.get("status"), r.get("rc"), (r.get("stderr") or "")[:300]))
        continue
    res = r["result"]
    m = res["mbps"]
    # wire datagram count: JSON per_conn, else parse the trace line of the .err file
    per = res.get("per_conn") or []
    fwd = sum(p.get("shim_fwd", 0) for p in per)
    we = sum(p.get("shim_write_err", 0) for p in per)
    drop = sum(p.get("shim_drop", 0) for p in per)
    if not per:
        try:
            txt = open(f"/tmp/exp2/{r['tag']}.err").read()
            mm = TRACE.search(txt)
            if mm:
                fwd, we, drop = int(mm.group(1)), int(mm.group(2)), int(mm.group(3))
        except OSError:
            pass
    s = res.get("samples") or []
    d = [(s[i] - s[i - 1]) * 8 / 0.1 / 1e6 for i in range(1, len(s))]
    zeros = sum(1 for x in d if x == 0)
    longest = cur = 0
    for x in d:
        cur = cur + 1 if x == 0 else 0
        longest = max(longest, cur)
    recv = res["received_bytes"]
    if we > 0:
        writeerrs.append((r["tag"], we))
    meta[key].append({
        "tag": r["tag"], "rep": r["rep"], "mbps": m, "wall_s": r.get("wall_s"),
        "fwd_per_mib": fwd / (recv / 2**20) if recv else 0,
        "write_err": we, "drop": drop, "zeros": zeros, "longest_zero": longest,
        "err": res.get("error", ""), "load0": r.get("load0", ""),
        "elapsed_ms": res.get("elapsed_ms", 0),
    })
    if we == 0:
        cells[key].append(m)

for mode in ["raw", "prod"]:
    print(f"\n=== {mode}: throughput per cell (ok runs with shim_write_err=0) ===")
    print(f"{'rtt':>4} {'rtomax':>7} {'n':>2} {'mean':>7} {'median':>7} {'min':>7} {'max':>7} "
          f"{'d_mean':>7} {'d_med':>7} {'p(perm)':>8}")
    for rtt in [12, 25, 71, 100]:
        base = cells.get((mode, rtt, "0"), [])
        for rt in ordr:
            v = cells.get((mode, rtt, rt), [])
            if not v:
                print(f"{rtt:>4} {rt:>7} {'0':>2}   no successful runs")
                continue
            s = stats(v)
            if rt == "0" or not base:
                dm = dmed = p = ""
            else:
                dm = f"{(s['mean']/statistics.mean(base)-1)*100:+.0f}%"
                dmed = f"{(s['med']/statistics.median(base)-1)*100:+.0f}%"
                p = f"{perm_test(v, base):.2f}"
            print(f"{rtt:>4} {rt:>7} {s['n']:>2} {s['mean']:>7.1f} {s['med']:>7.1f} "
                  f"{s['min']:>7.1f} {s['max']:>7.1f} {dm:>7} {dmed:>7} {p:>8}")

print("\n=== wire datagrams per MiB delivered (retransmission proxy; 1344.5 = baseline) ===")
print(f"{'mode':>5} {'rtt':>4} {'rtomax':>7} {'n':>2} {'mean':>8} {'min':>8} {'max':>8}")
for mode in ["raw", "prod"]:
    for rtt in [12, 25, 71, 100]:
        for rt in ordr:
            ms = [x for x in meta.get((mode, rtt, rt), []) if x["write_err"] == 0 and x["fwd_per_mib"] > 0]
            if not ms:
                continue
            v = [x["fwd_per_mib"] for x in ms]
            print(f"{mode:>5} {rtt:>4} {rt:>7} {len(v):>2} {statistics.mean(v):>8.1f} {min(v):>8.1f} {max(v):>8.1f}")

print("\n=== stall structure (100 ms sample array, per cell) ===")
print(f"{'mode':>5} {'rtt':>4} {'rtomax':>7} {'n':>2} {'mean_zeros':>10} {'max_longest0':>12}")
for mode in ["raw", "prod"]:
    for rtt in [12, 25, 71, 100]:
        for rt in ordr:
            ms = [x for x in meta.get((mode, rtt, rt), []) if x["write_err"] == 0]
            if not ms:
                continue
            print(f"{mode:>5} {rtt:>4} {rt:>7} {len(ms):>2} "
                  f"{statistics.mean([x['zeros'] for x in ms]):>10.1f} "
                  f"{max(x['longest_zero'] for x in ms):>12}")

print("\n=== all runs, chronological (mbps, wall_s, fwd/MiB, zeros) ===")
for mode in ["raw", "prod"]:
    for rtt in [12, 25, 71, 100]:
        for rt in ordr:
            for x in meta.get((mode, rtt, rt), []):
                print(f"  {mode:>4} rtt={rtt:<3} rto={rt:<6} r{x['rep']:<2} mbps={x['mbps']:>7.1f} "
                      f"wall={x['wall_s']:>3}s fwd/MiB={x['fwd_per_mib']:>7.1f} zeros={x['zeros']:>3} "
                      f"long0={x['longest_zero']:>2} load={x['load0']:<5} {x['err']}")

if fails:
    print("\n=== FAILED / non-ok runs ===")
    for t in fails:
        print(f"  {t[0]}: status={t[1]} rc={t[2]} stderr={t[3]!r}")
if writeerrs:
    print("\n=== runs with shim_write_err>0 (excluded from throughput stats) ===")
    for t in writeerrs:
        print(f"  {t[0]}: write_err={t[1]}")
print(f"\ntotal runs recorded: {len(rows)}")
