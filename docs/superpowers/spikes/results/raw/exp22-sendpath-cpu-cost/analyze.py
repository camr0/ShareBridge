#!/usr/bin/env python3
"""exp22 analysis: separate per-byte cost from per-second overhead.

Model:  go_cpu_s = a + b*(MiB) + c*(elapsed_s)
  b -> true per-byte send-path cost (CPU-seconds per MiB, and per GB)
  c -> per-second overhead (the bench's 50 ms chromedp poll loop)
  a -> fixed startup cost inside the measured window
Least squares via normal equations + Gaussian elimination (no numpy needed).
"""
import csv
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))


def solve(A, y, n):
    """Solve an n x n linear system by Gaussian elimination with partial pivoting."""
    M = [row[:] + [y[i]] for i, row in enumerate(A)]
    for col in range(n):
        p = max(range(col, n), key=lambda r: abs(M[r][col]))
        M[col], M[p] = M[p], M[col]
        if abs(M[col][col]) < 1e-12:
            return None
        for r in range(col + 1, n):
            f = M[r][col] / M[col][col]
            for c2 in range(col, n + 1):
                M[r][c2] -= f * M[col][c2]
    x = [0.0] * n
    for r in range(n - 1, -1, -1):
        s = M[r][n] - sum(M[r][c2] * x[c2] for c2 in range(r + 1, n))
        x[r] = s / M[r][r]
    return x


MIB_PER_MBPS_S = 1e6 / 8 / 1048576  # MiB/s delivered per Mbps


def sat_mbps(b_per_mib):
    """Rate at which the per-byte cost alone spends exactly 1 core."""
    return 1.0 / (b_per_mib * MIB_PER_MBPS_S)


def fit(rows, label, use_time=True, quiet=False):
    n = len(rows)
    ncol = 3 if use_time else 2
    if n < ncol + 2:
        if not quiet:
            print("%s: too few rows (%d)" % (label, n))
        return None
    X = [[1.0, r["mib"]] + ([r["elapsed_s"]] if use_time else []) for r in rows]
    y = [r["go_cpu"] for r in rows]
    A = [[sum(X[k][i] * X[k][j] for k in range(n)) for j in range(ncol)] for i in range(ncol)]
    bv = [sum(X[k][i] * y[k] for k in range(n)) for i in range(ncol)]
    coef = solve(A, bv, ncol)
    if coef is None:
        return None
    a, b = coef[0], coef[1]
    c = coef[2] if use_time else float("nan")
    pred = [sum(coef[i] * X[k][i] for i in range(ncol)) for k in range(n)]
    resid = [y[i] - pred[i] for i in range(n)]
    sst = sum((v - sum(y) / n) ** 2 for v in y)
    sse = sum(r * r for r in resid)
    r2 = 1 - sse / sst if sst else float("nan")
    if not quiet:
        print("\n=== %s (n=%d) ===" % (label, n))
        print("  a (fixed startup)       = %+.4f s" % a)
        print("  b (per-byte)            = %.5f s/MiB = %.2f CPU-seconds per GB" % (b, b * 1024))
        if use_time:
            print("  c (per-second overhead) = %.4f cores (%.2f ms CPU per 50 ms poll)" % (c, c * 50))
        print("  R^2 = %.5f   max|resid| = %.3f s" % (r2, max(abs(r) for r in resid)))
        print("  => per-byte cost alone saturates 1 core at %.0f Mbps" % sat_mbps(b))
    return dict(a=a, b=b, c=c, r2=r2)


def load():
    out = []
    with open(os.path.join(HERE, "cells.tsv")) as f:
        for row in csv.DictReader(f, delimiter="\t"):
            if row.get("mode") != "prod":
                continue
            try:
                out.append(dict(
                    label=row["label"], rtt=int(row["rtt"]),
                    mib=int(row["sent_bytes"]) / 1048576.0,
                    elapsed_s=float(row["elapsed_ms"]) / 1000.0,
                    go_cpu=float(row["go_cpu_s"]),
                    go_cores=float(row["go_cores"]),
                    mbps=float(row["mbps"]),
                    cap=int(row["cap_bps"]),
                    drop=int(row["shim_drop"]), cerr=int(row["shim_write_err"]),
                    recv=int(row["received_bytes"])))
            except (ValueError, KeyError):
                continue
    return out


def main():
    rows = load()
    print("prod cells loaded: %d" % len(rows))
    bad = [r for r in rows if r["drop"] or r["cerr"] or r["recv"] != int(r["mib"] * 1048576)]
    print("cells with drop/writeErr/short-delivery (excluded from fits): %s"
          % ([r["label"] for r in bad] or "none"))
    rows = [r for r in rows if r not in bad]

    fit(rows, "ALL prod cells")
    fit([r for r in rows if r["rtt"] == 12], "prod rtt=12")
    fit([r for r in rows if r["rtt"] == 71], "prod rtt=71")
    # Constant-rate size sweep: elapsed is nearly proportional to size, so b and c
    # are collinear -- fit size alone.
    fit([r for r in rows if r["cap"] == 30000000 and r["rtt"] == 12],
        "prod rtt=12 cap=30MB, 16/32/64/128 MiB (size-only fit)", use_time=False)

    # Direct ratio: group by capped arm (identical config), mean rate vs mean cores.
    print("\n=== direct ratio per capped arm (mean of n=3 reps) ===")
    arms = {}
    for r in rows:
        if r["cap"] == 0:
            continue
        k = (r["rtt"], r["cap"])
        arms.setdefault(k, []).append(r)
    for (rtt, cap), rs in sorted(arms.items()):
        mb = sum(x["mbps"] for x in rs) / len(rs)
        co = sum(x["go_cores"] for x in rs) / len(rs)
        print("  rtt=%2d cap=%2dMB  n=%d  %7.2f Mbps  %7.4f cores  -> %6.1f Mbps/core"
              % (rtt, cap // 1000000, len(rs), mb, co, mb / co))

    # cores vs rate
    print("\n=== cores per 100 Mbps (measured, per cell) ===")
    for r in sorted(rows, key=lambda r: (r["rtt"], r["mbps"])):
        print("  %-34s rtt=%2d %7.2f Mbps  %9.4f cores  -> %6.1f Mbps/core  (%6.2f cores/100Mbps)"
              % (r["label"], r["rtt"], r["mbps"], r["go_cores"], r["mbps"] / r["go_cores"],
                 r["go_cores"] / r["mbps"] * 100))


if __name__ == "__main__":
    main()
