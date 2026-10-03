#!/usr/bin/env python3
"""E34 analysis: per-message vs per-byte receiver cost, from cells.tsv."""
import csv
import statistics as st

BLOCK_OF = {"ceil": None, "cap8mb": "8MB", "cap30mb": "30MB"}
CAP_MBPS = {None: None, "8MB": 64.0, "30MB": 240.0}
SIZES = [("1KiB", 1024), ("4KiB", 4096), ("16KiB", 16384), ("64KiB", 65536), ("256KiB", 262144)]


PREFIX = {None: ["e34-ceil-", "e34-small-ceil-"],
          "8MB": ["e34-cap8mb-", "e34-small-cap8mb-"],
          "30MB": ["e34-cap30mb-", "e34-small-cap30mb-"]}


def cells(rows, cap, rtt, ck):
    return [r for r in rows
            if any(r["label"].startswith(p) for p in PREFIX[cap])
            and r["rtt"] == rtt and r["chunk"] == ck]


def num(r, k):
    try:
        return float(r[k])
    except (TypeError, ValueError):
        return None


def fit(xs, ys):
    n = len(xs)
    mx, my = sum(xs) / n, sum(ys) / n
    sxx = sum((x - mx) ** 2 for x in xs)
    sxy = sum((x - mx) * (y - my) for x, y in zip(xs, ys))
    slope = sxy / sxx if sxx else 0.0
    resid = [y - (my + slope * (x - mx)) for x, y in zip(xs, ys)]
    return slope, mx, my, max(abs(r) for r in resid)


def main():
    rows = list(csv.DictReader(open("cells.tsv"), delimiter="\t"))
    L = []
    L.append("cells=%d  failures(rc!=0)=%d  shim_write_err>0=%d" % (
        len(rows), sum(1 for r in rows if r["rc"] != "0"),
        sum(1 for r in rows if r["shim_write_err"] not in ("0", ""))))

    for cap in (None, "8MB", "30MB"):
        L.append("\n=== cap=%s (%s) rtt=0 ===" % (cap, CAP_MBPS[cap]))
        L.append("%-7s %-4s %-28s %-28s %-28s %-8s %-8s" % (
            "chunk", "n", "mbps", "chrome_cores", "chrome_cs/GB", "msg/s", "up%"))
        xs, ys, wts = [], [], []
        for ck, cb in SIZES:
            cs = cells(rows, cap, "0", ck)
            # drop cells that missed the cap by >10 % (collapsed), keep them in the raw table
            good = [r for r in cs if cap is None or (num(r, "mbps") or 0) >= 0.9 * CAP_MBPS[cap]]
            for r in good:
                xs.append(num(r, "msg_per_s"))
                ys.append(num(r, "chrome_cores"))
            if not good:
                L.append("%-7s %-4d COLLAPSED (all cells below 90%% of cap)" % (ck, len(cs)))
                continue
            L.append("%-7s %-4d %-28s %-28s %-28s %-8.0f %-8.2f" % (
                ck, len(cs),
                " ".join("%.0f" % num(r, "mbps") for r in cs),
                " ".join("%.3f" % num(r, "chrome_cores") for r in cs),
                " ".join("%.1f" % num(r, "chrome_cs_per_gb") for r in cs),
                sum(num(r, "msg_per_s") for r in cs) / len(cs),
                100 * (1 - st.mean([num(r, "chrome_cs_per_gb") for r in good]) /
                       st.mean([num(r, "chrome_cs_per_gb")
                                for r in cells(rows, cap, "0", "16KiB")]))))
        slope, _, _, resid = fit(xs, ys)
        L.append("  least-squares chrome_cores vs msg/s over %d cells (1KiB..256KiB): "
                 "slope = %.1f us/msg, max residual %.3f cores" % (len(xs), slope * 1e6, resid))
        b16 = st.mean([num(r, "chrome_cs_per_gb") for r in cells(rows, cap, "0", "16KiB")])
        b256 = st.mean([num(r, "chrome_cs_per_gb") for r in cells(rows, cap, "0", "256KiB")])
        L.append("  16KiB -> 256KiB: chrome_cs/GB %.2f -> %.2f (%+.1f%%)  [16x fewer messages]"
                 % (b16, b256, 100 * (b256 - b16) / b16))
        b1 = st.mean([num(r, "chrome_cs_per_gb") for r in cells(rows, cap, "0", "1KiB")])
        L.append("  16KiB -> 1KiB:  chrome_cs/GB %.2f -> %.2f (%+.1f%%)  [16x more messages]"
                 % (b16, b1, 100 * (b1 - b16) / b16))

    # field projection
    L.append("\n=== field projection (E28/E32 bare arms) ===")
    L.append("E28 4-vCPU bare : 123.68 Mbps, 16 KiB msgs -> 943.6 msg/s, renderer 0.63 core, client 1.27 cores")
    L.append("E32-W5 1-vCPU bare: 77.363 Mbps, 48,258 msgs over 81.758 s -> 590 msg/s, renderer 0.337 core")
    for cap in (None, "8MB", "30MB"):
        cs = cells(rows, cap, "0", "16KiB")
        xs, ys = [], []
        for ck, _ in SIZES:
            for r in cells(rows, cap, "0", ck):
                if cap is None or (num(r, "mbps") or 0) >= 0.9 * CAP_MBPS[cap]:
                    xs.append(num(r, "msg_per_s"))
                    ys.append(num(r, "chrome_cores"))
        slope = fit(xs, ys)[0]
        L.append("  cap=%-5s slope %.1f us/msg -> at 943.6 msg/s (16 KiB, field near-field plateau) "
                 "the per-message term is %.4f core of the receiver; cutting 16KiB->64KiB saves %.2f%% "
                 "of receiver CPU" % (cap, slope * 1e6, slope * 943.6,
                                      100 * slope * (943.6 - 235.9) / st.mean(ys)))
    # rtt71 collapse
    L.append("\n=== rtt 71 (all cells collapse; chunk size irrelevant) ===")
    for cap in (None, "8MB", "30MB"):
        for ck, _ in SIZES:
            cs = cells(rows, cap, "71", ck)
            if not cs:
                continue
            L.append("  cap=%-5s %-7s mbps %s | ceil %s | chrome_cs/GB %s | fwd %s" % (
                cap, ck, " ".join("%.1f" % num(r, "mbps") for r in cs),
                " ".join("%.0f" % num(r, "ceil_mbps") for r in cs),
                " ".join("%.1f" % num(r, "chrome_cs_per_gb") for r in cs),
                " ".join("%s" % r["fwd"] for r in cs)))
    # sanity
    L.append("\n=== sanity (runbook: rtt 71 raw 32 MiB ~= 100 Mbps) ===")
    for r in rows:
        if "sanity" in r["label"]:
            L.append("  %-36s mbps %-8s ceil %-6s cores %s" % (
                r["label"], r["mbps"], r["ceil_mbps"], r["chrome_cores"]))
    # baseline spread, in-block
    L.append("\n=== in-block 16 KiB baselines (n=3 before + n=3 after each sweep) ===")
    for cap, tag in ((None, "ceil"), ("8MB", "cap8mb"), ("30MB", "cap30mb")):
        allc = [num(r, "chrome_cores") for r in cells(rows, cap, "0", "16KiB")]
        pre = [num(r, "chrome_cores") for r in rows
               if r["label"].startswith(PREFIX[cap][0]) and r["rtt"] == "0" and r["chunk"] == "16KiB"]
        post = [num(r, "chrome_cores") for r in rows
                if r["label"].startswith(PREFIX[cap][1]) and r["rtt"] == "0" and r["chunk"] == "16KiB"]
        L.append("  %-6s pre n=%d %s | post n=%d %s | spread %.1f%%" % (
            cap, len(pre), " ".join("%.3f" % x for x in pre),
            len(post), " ".join("%.3f" % x for x in post),
            100 * (max(allc) - min(allc)) / min(allc)))
    L.append("\n=== uncapped rtt-0 ceiling per chunk (best-case plateau, load-robust) ===")
    for ck, _ in SIZES:
        cs = cells(rows, None, "0", ck)
        L.append("  %-7s mbps %-24s ceil %s" % (
            ck, " ".join("%.0f" % num(r, "mbps") for r in cs),
            " ".join("%.0f" % num(r, "ceil_mbps") for r in cs)))
    out = "\n".join(L)
    open("analysis.txt", "w").write(out + "\n")
    print(out)


if __name__ == "__main__":
    main()
