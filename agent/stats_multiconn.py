#!/usr/bin/env python3
"""Summarise multiconn_results.jsonl.

Reports p10/p50/p90 (not just the mean) because the documented failure mode at
high RTT is bimodal, and reports per-connection balance because the hypothesis
is that N associations prevent one collapse from sinking the transfer.
"""
import json
import statistics
import sys

path = sys.argv[1] if len(sys.argv) > 1 else "multiconn_results.jsonl"

rows = []
with open(path) as fh:
    for line in fh:
        line = line.strip()
        if line:
            rows.append(json.loads(line))

if not rows:
    print("no results", file=sys.stderr)
    sys.exit(1)


def pct(vals, p):
    s = sorted(vals)
    if not s:
        return float("nan")
    if len(s) == 1:
        return s[0]
    k = (len(s) - 1) * p
    lo = int(k)
    hi = min(lo + 1, len(s) - 1)
    return s[lo] + (s[hi] - s[lo]) * (k - lo)


groups = {}
for r in rows:
    groups.setdefault(r["label"], []).append(r)

hdr = f"{'label':<14}{'n':>3}{'stall':>6}{'mean':>9}{'p10':>9}{'p50':>9}{'p90':>9}{'spread':>8}{'kern':>7}{'gocpu':>7}"
print(hdr)
print("-" * len(hdr))
for label in sorted(groups):
    rs = groups[label]
    vals = [r["wall_mbps"] for r in rs]
    stalls = sum(1 for r in rs if r.get("error"))
    spread = (max(vals) / min(vals)) if min(vals) > 0 else float("inf")
    kern = statistics.mean([r.get("chrome_cores", 0) for r in rs])
    gocpu = statistics.mean([r.get("go_cpu_seconds", 0) for r in rs])
    print(
        f"{label:<14}{len(vals):>3}{stalls:>6}{statistics.mean(vals):>9.1f}"
        f"{pct(vals, 0.1):>9.1f}{pct(vals, 0.5):>9.1f}{pct(vals, 0.9):>9.1f}"
        f"{spread:>8.2f}{kern:>7.2f}{gocpu:>7.2f}"
    )

# Scaling table: mean/p50 per group at each connection count.
print()
print("Scaling by group (wall Mbps). speedup is vs conns=1 mean.")
scale_hdr = f"{'group':<10}{'conns':>6}{'n':>3}{'mean':>9}{'p50':>9}{'stall':>6}{'speedup':>9}"
print(scale_hdr)
print("-" * len(scale_hdr))
by_group = {}
for r in rows:
    by_group.setdefault(r.get("group", "?"), {}).setdefault(r["conns"], []).append(r)
for group in sorted(by_group):
    base = None
    for conns in sorted(by_group[group]):
        rs = by_group[group][conns]
        vals = [r["wall_mbps"] for r in rs]
        mean = statistics.mean(vals)
        if conns == 1:
            base = mean
        speedup = (mean / base) if base else float("nan")
        stalls = sum(1 for r in rs if r.get("error"))
        print(
            f"{group:<10}{conns:>6}{len(vals):>3}{mean:>9.1f}{pct(vals, 0.5):>9.1f}"
            f"{stalls:>6}{speedup:>9.2f}"
        )

# Per-connection balance: the starvation signal.
print()
print("Per-connection balance (share of the largest connection's bytes).")
print("1.00 = every association carried an equal share; low = one starved.")
print(f"{'label':<14}{'n':>3}{'min_share_mean':>16}{'worst_share':>13}")
print("-" * 46)
for label in sorted(groups):
    shares = []
    for r in groups[label]:
        pc = r.get("per_conn") or []
        recv = [c.get("received", 0) for c in pc]
        if len(recv) > 1 and max(recv) > 0:
            shares.append(min(recv) / max(recv))
    if shares:
        print(f"{label:<14}{len(shares):>3}{statistics.mean(shares):>16.2f}{min(shares):>13.2f}")
