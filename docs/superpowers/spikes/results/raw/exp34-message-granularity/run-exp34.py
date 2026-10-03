#!/usr/bin/env python3
"""E34 - DataChannel message granularity (N1) on the benchdirect lab harness.

Go/pion SENDS, headless Chrome RECEIVES (bare byte counter = the E28 "bare" arm).
--chunk IS the DataChannel message size (SCTP re-fragments on the wire and
reassembles before delivery).

Blocks:
  sanity    : rtt 71 / 32 MiB / 16 KiB, uncapped, n=3  (runbook sanity gate)
  sanity0   : rtt 0  / 32 MiB / 16 KiB, uncapped, n=1  (reference)
  ceil      : uncapped, 64 MiB, rtt {0,71} x chunk {16,64,256} x n=3
  cap8mb    : --bandwidth 8MB  (=64 Mbps), same grid
  cap30mb   : --bandwidth 30MB (=240 Mbps), same grid

Writes one row per cell to cells.tsv immediately after the cell completes.
"""
import json
import os
import subprocess
import sys
import time

AGENT = "/Users/ali/Git/ShareBridge/.worktrees/benchdirect/agent"
BIN = os.path.join(AGENT, "bin", "benchdirect")
OUT = os.path.dirname(os.path.abspath(__file__))
DEADLINE = "120"

sys.path.insert(0, OUT)
from row import make_row, TSV_HEADER, snapshot  # noqa: E402


def run_cell(label, rtt, chunk, size, bandwidth=None, extra=None):
    jpath = os.path.join(OUT, label + ".json")
    lpath = os.path.join(OUT, label + ".log")
    cmd = [BIN, "--mode", "raw", "--rtt", str(rtt), "--chunk", chunk,
           "--size", size, "--backpressure", "poll", "--deadline", DEADLINE,
           "--out", jpath]
    if bandwidth:
        cmd += ["--bandwidth", bandwidth]
    if extra:
        cmd += extra
    with open(lpath, "w") as lf:
        lf.write("CMD " + " ".join(cmd) + "\n")
        lf.flush()
        t0 = time.time()
        p = subprocess.run(cmd, stdout=lf, stderr=subprocess.STDOUT, cwd=AGENT)
        wall = time.time() - t0
    row = make_row(label, cmd, rtt, chunk, size, bandwidth, jpath, wall, p.returncode)
    with open(os.path.join(OUT, "cells.tsv"), "a") as f:
        f.write("\t".join(str(row.get(k, "")) for k in TSV_HEADER) + "\n")
    with open(os.path.join(OUT, "runner.log"), "a") as f:
        f.write("%s %s rc=%d wall=%.1fs mbps=%s cores=%s err=%s\n" % (
            label, "OK" if p.returncode == 0 else "FAIL", p.returncode, wall,
            row.get("mbps"), row.get("chrome_cores"), row.get("error", "")))
    snap = snapshot()
    with open(os.path.join(OUT, "system-snapshots.txt"), "a") as f:
        f.write("### %s rc=%d wall=%.1fs\n%s\n" % (label, p.returncode, wall, snap))
    print("%-38s rc=%d wall=%5.1fs mbps=%8s cores=%s fwd=%s drop=%s werr=%s" % (
        label, p.returncode, wall, row.get("mbps"), row.get("chrome_cores"),
        row.get("fwd"), row.get("drop"), row.get("shim_write_err")), flush=True)
    return row


BLOCKS = [
    ("sanity",  [("e34-sanity-rtt71-32MiB-16KiB-r%d" % r, 71, "16KiB", "32MiB", None) for r in (1, 2, 3)]
              + [("e34-sanity-rtt0-32MiB-16KiB-r1", 0, "16KiB", "32MiB", None)]),
    ("ceil",    None),
    ("cap8mb",  None),
    ("cap30mb", None),
    ("small",   None),
]
GRID = [(rtt, ck, rep) for rtt in (0, 71) for rep in (1, 2, 3) for ck in ("16KiB", "64KiB", "256KiB")]
CAPS = {"ceil": None, "cap8mb": "8MB", "cap30mb": "30MB"}

# Block D - the decisive per-message probe: PIN the delivered rate and shrink the
# message BELOW 16 KiB so the onmessage rate goes far above the field's ~944/s.
# Same rate, same wire datagram count, up to 16x more browser message deliveries.
SMALL = [("e34-small-%s-%s-r%d" % (b, ck, r), 0, ck, "64MiB", CAPS[b])
         for b in ("ceil", "cap30mb", "cap8mb")
         for r in (1, 2, 3) for ck in ("1KiB", "4KiB", "16KiB")]


def main():
    only = sys.argv[1:] if len(sys.argv) > 1 else None
    if not os.path.exists(os.path.join(OUT, "cells.tsv")):
        with open(os.path.join(OUT, "cells.tsv"), "w") as f:
            f.write("\t".join(TSV_HEADER) + "\n")
    with open(os.path.join(OUT, "system-snapshots.txt"), "a") as f:
        f.write("### SESSION START\n" + snapshot() + "\n")
    for name, cells in BLOCKS:
        if only and name not in only:
            continue
        if name == "small":
            cells = SMALL
        if cells is None:
            if name == "sanity":
                continue
            cap = CAPS[name]
            cells = [("e34-%s-rtt%d-%s-r%d" % (name, rtt, ck, rep), rtt, ck, "64MiB", cap)
                     for (rtt, ck, rep) in GRID]
        with open(os.path.join(OUT, "runner.log"), "a") as f:
            f.write("=== BLOCK %s ===\n" % name)
        for label, rtt, ck, size, bw in cells:
            run_cell(label, rtt, ck, size, bw)
    with open(os.path.join(OUT, "system-snapshots.txt"), "a") as f:
        f.write("### SESSION END\n" + snapshot() + "\n")


if __name__ == "__main__":
    main()
