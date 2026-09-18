#!/usr/bin/env python3
"""E6 row extractor: chunk size at high RTT.

usage: row.py <label> <json_path> <log_path>

Columns (tab separated):
label mbps ceil_mbps chunk rtt loss wall_s nstall stall_s longest_s drop werr fwd dg_per_mib wire_ratio error

wire_ratio follows E1's calibrated model (mean L4 datagram 844.8 B; clean-path
reference 1344.7 datagrams/MiB -> 1.083): dg_per_mib / 1241.3.
"""
import json
import re
import statistics
import sys

REF_DG_PER_MIB = 1241.3


def series_mbps(samples):
    if not samples or len(samples) < 2:
        return []
    return [(samples[i] - samples[i - 1]) * 8 / 0.1 / 1e6 for i in range(1, len(samples))]


def interior_stalls(mbps):
    nz = [d for d in mbps if d > 0]
    if not nz:
        return 0, 0.0, 0.0
    thr = 0.1 * statistics.median(nz)
    first = next((i for i, d in enumerate(mbps) if d >= thr), None)
    last = next((i for i, d in enumerate(reversed(mbps)) if d >= thr), None)
    if first is None or last is None:
        return 0, 0.0, 0.0
    n = 0
    total = 0.0
    longest = 0.0
    run = 0
    for d in mbps[first:len(mbps) - last]:
        if d < thr:
            run += 1
            n += 1
            total += 0.1
            longest = max(longest, 0.1 * run)
        else:
            run = 0
    return n, total, longest


def main():
    label, json_path, log_path = sys.argv[1], sys.argv[2], sys.argv[3]
    try:
        with open(json_path) as f:
            j = json.load(f)
    except Exception:
        j = None
    log = ""
    try:
        with open(log_path) as f:
            log = f.read()
    except Exception:
        pass

    if j and j.get("samples"):
        mbps = series_mbps([float(x) for x in j["samples"]])
    else:
        traces = re.findall(r"mbps/100ms:(.*?)\|\s*shim", log)
        mbps = [float(x) for x in traces[-1].split()] if traces else []
    ceil = statistics.median(sorted(mbps)[-5:]) if len(mbps) >= 5 else 0.0
    nst, sts, lng = interior_stalls(mbps)
    wall = (len(mbps) * 0.1) if mbps else 0.0

    drops = re.findall(r"shim fwd=(\d+) writeErr=(\d+) drop=(\d+)", log)
    fwd, werr, drop = drops[-1] if drops else ("", "", "")

    if j is None:
        errs = re.findall(r"bench failed: (.*)", log)
        err = errs[-1].strip() if errs else "no JSON"
        res_mbps = chunk = rtt = loss = dgmib = ratio = ""
    else:
        err = (j.get("error") or "").replace("\t", " ")
        res_mbps = "%.2f" % j.get("mbps", 0)
        chunk = j.get("chunk", "")
        rtt = j.get("rtt_ms", "")
        loss = j.get("loss", "")
        recv = j.get("received_bytes", 0) or 0
        if recv > 0 and fwd:
            dgmib = float(fwd) / (recv / (1 << 20))
            ratio = "%.3f" % (dgmib / REF_DG_PER_MIB)
            dgmib = "%.1f" % dgmib
        else:
            dgmib = ratio = ""

    print("\t".join(str(x) for x in [
        label, res_mbps, "%.1f" % ceil, chunk, rtt, loss, "%.1f" % wall,
        nst, "%.1f" % sts, "%.1f" % lng, drop, werr, fwd, dgmib, ratio, err,
    ]))


if __name__ == "__main__":
    main()
