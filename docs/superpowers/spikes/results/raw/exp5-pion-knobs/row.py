#!/usr/bin/env python3
"""Extract one benchdirect result into a TSV row.

usage: row.py <label> <json_path> <log_path>

raw mode writes `samples` (cumulative bytes per 100 ms) into the JSON; prod mode
does not, so its 100 ms series is parsed out of the stderr `trace` line instead.

Columns (tab separated):
label mbps ceil_mbps fw rxbuf maxmsg rtt loss wall_s nstall stall_s longest_s drop werr fwd error

`ceil_mbps` = median of the five fastest 100 ms windows - the plateau the cell
demonstrably reaches (the bench-reliability note calls the ceiling the robust
metric).  `nstall`/`stall_s`/`longest_s` are *interior* stalls only: leading and
trailing sub-threshold windows (chrome/SCTP start-up and drain) are trimmed
first, otherwise every prod cell reports its 0.4-1.0 s ramp as stalls.
"""
import json
import re
import statistics
import sys


def series_mbps(samples):
    """100 ms deltas in Mbps."""
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
    interior = mbps[first:len(mbps) - last]
    n = 0
    total = 0.0
    longest = 0.0
    run = 0
    for d in interior:
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

    samples = None
    if j and j.get("samples"):
        cum = [float(x) for x in j["samples"]]
        mbps = series_mbps(cum)
    else:
        traces = re.findall(r"mbps/100ms:(.*?)\|\s*shim", log)
        # the trace prints per-100 ms Mbps directly, not cumulative bytes
        mbps = [float(x) for x in traces[-1].split()] if traces else []
    ceil = statistics.median(sorted(mbps)[-5:]) if len(mbps) >= 5 else 0.0
    nst, sts, lng = interior_stalls(mbps)
    wall = (len(mbps) * 0.1) if mbps else 0.0

    drops = re.findall(r"shim fwd=(\d+) writeErr=(\d+) drop=(\d+)", log)
    fwd, werr, drop = drops[-1] if drops else ("", "", "")

    if j is None:
        errs = re.findall(r"bench failed: (.*)", log)
        err = errs[-1].strip() if errs else "no JSON"
        res_mbps = fw = rx = mm = rtt = loss = ""
    else:
        err = (j.get("error") or "").replace("\t", " ")
        res_mbps = "%.2f" % j.get("mbps", 0)
        fw = j.get("fast_rtx_wnd", "")
        rx = j.get("max_rx_buf", "")
        mm = j.get("max_msg", "")
        rtt = j.get("rtt_ms", "")
        loss = j.get("loss", "")

    print("\t".join(str(x) for x in [
        label, res_mbps, "%.1f" % ceil, fw, rx, mm, rtt, loss, "%.1f" % wall,
        nst, "%.1f" % sts, "%.1f" % lng, drop, werr, fwd, err,
    ]))


if __name__ == "__main__":
    main()
