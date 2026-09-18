#!/usr/bin/env python3
"""exp22 row extractor: JSON + stderr log -> one TSV row."""
import json
import os
import re
import sys

OUT = os.path.dirname(os.path.abspath(__file__))

COLS = ["label", "mode", "rtt", "cap_bps", "mbps", "wall_mbps", "elapsed_ms",
        "go_cpu_s", "go_cores", "chrome_cores", "sent_bytes", "received_bytes",
        "shim_fwd", "shim_drop", "shim_write_err", "dg_per_mib", "wire_ratio",
        "dgram_bytes", "cs_per_gb", "cores_per_100mbps", "ceil_mbps"]


def parse_log(path):
    fwd = drop = werr = 0
    samples = []
    if not os.path.exists(path):
        return fwd, drop, werr, samples
    for line in open(path, errors="replace"):
        m = re.search(r"shim fwd=(\d+) writeErr=(\d+) drop=(\d+)", line)
        if m:
            fwd, werr, drop = int(m.group(1)), int(m.group(2)), int(m.group(3))
        m = re.search(r"mbps/100ms:(.*?)\|", line)
        if m:
            samples = [float(x) for x in m.group(1).split()]
    return fwd, drop, werr, samples


def ceiling(samples):
    """Median of a sliding 5-tuple of 100ms samples -- same definition as exp14:
    the best sustained plateau, robust to this Mac's stalls."""
    if len(samples) < 5:
        return max(samples) if samples else 0.0
    wins = sorted(sum(samples[i:i + 5]) / 5 for i in range(len(samples) - 4))
    return wins[-1] if wins else 0.0


def main(label):
    jpath = os.path.join(OUT, label + ".json")
    if not os.path.exists(jpath):
        print("\t".join([label] + ["ERR"] * (len(COLS) - 1)))
        return
    r = json.load(open(jpath))
    fwd, drop, werr, samples = parse_log(os.path.join(OUT, label + ".log"))
    sent = r.get("sent_bytes") or 0
    recv = r.get("received_bytes") or 0
    elapsed = r.get("elapsed_ms") or 0
    go = r.get("go_cpu_seconds") or 0
    mbps = r.get("mbps") or 0
    wall = r.get("wall_mbps") or 0
    go_cores = go / (elapsed / 1000) if elapsed > 0 else 0
    dg_per_mib = fwd / (recv / 1048576) if recv else 0
    wire_ratio = dg_per_mib / 1344.7 if dg_per_mib else 0  # exp1/exp14 clean-path datum
    dgram_bytes = (sent * 1.0 / fwd) if fwd else 0          # payload bytes/datagram
    cs_per_gb = go / (recv / 1e9) if recv else 0
    cores_per_100 = (go_cores / mbps) * 100 if mbps else 0
    row = [label, r.get("mode", ""), r.get("rtt_ms", ""), r.get("bandwidth_bps", ""),
           "%.2f" % mbps, "%.2f" % wall, "%.0f" % elapsed, "%.4f" % go,
           "%.4f" % go_cores, "%.4f" % (r.get("chrome_cores") or 0), sent, recv,
           fwd, drop, werr, "%.1f" % dg_per_mib, "%.4f" % wire_ratio,
           "%.1f" % dgram_bytes, "%.4f" % cs_per_gb, "%.5f" % cores_per_100,
           "%.1f" % ceiling(samples)]
    print("\t".join(str(c) for c in row))


if __name__ == "__main__":
    for lbl in sys.argv[1:]:
        main(lbl)
