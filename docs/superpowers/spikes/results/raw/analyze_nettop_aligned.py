#!/usr/bin/env python3
"""Window-aligned wire/payload ratios for the instrumented nettop runs.
Aligns the browser 100ms samples to nettop epoch timestamps via process end,
then computes, for a steady window [w0, w1] (fractions of the run):
  payload delivered in window (from samples)
  data-dir wire bytes in window (out-in deltas)
  sack-dir wire bytes in window
"""
import json, re, sys
from datetime import datetime

nt_path, json_path, snap_end_epoch, w0f, w1f = (
    sys.argv[1], sys.argv[2], float(sys.argv[3]), float(sys.argv[4]), float(sys.argv[5]))

series = []
snap_t = None
for ln in open(nt_path):
    if ln.startswith("SNAP"):
        snap_t = float(ln.split()[1])
    else:
        m = re.search(r"benchdirect\.\d+\s+(\d+)\s+(\d+)", ln)
        if m and snap_t is not None:
            series.append((snap_t, int(m.group(1)), int(m.group(2))))
series.sort()
clean = []
for t, i, o in series:
    if clean and t - clean[-1][0] < 0.5:
        continue
    clean.append((t, i, o))

r = json.load(open(json_path))
s = r["samples"]
n = len(s)
# page start epoch: process ended ~snap_end_epoch; teardown (final evals, browser
# close) ~0.4s before that; samples ran from page load. Uncertainty ~ +/-0.5s.
page_start = snap_end_epoch - n * 0.1 - 0.4

def payload_between(ta, tb):
    ia = max(0, int((ta - page_start) / 0.1))
    ib = min(n - 1, int((tb - page_start) / 0.1))
    return s[ib] - s[ia], ia, ib

t0 = clean[0][0] + w0f * (clean[-1][0] - clean[0][0])
t1 = clean[0][0] + w1f * (clean[-1][0] - clean[0][0])
a = min(range(len(clean)), key=lambda k: abs(clean[k][0] - t0))
b = min(range(len(clean)), key=lambda k: abs(clean[k][0] - t1))
if b < a:
    a, b = b, a
(ta, ina, outa), (tb, inb, outb) = clean[a], clean[b]
dt = tb - ta
din, dout = inb - ina, outb - ina if False else outb - outa
payload, ia, ib = payload_between(ta, tb)
fwd = r["per_conn"][0]["shim_fwd"]
print(f"window [{ta:.1f},{tb:.1f}] dt={dt:.1f}s  samples[{ia}:{ib}]")
print(f"payload_in_window={payload} ({payload/dt/1e6:.3f} MB/s)")
print(f"in_delta={din} ({din/dt/1e6:.3f} MB/s)  out_delta={dout} ({dout/dt/1e6:.3f} MB/s)")
data_dir = dout - din
sack_dir = 2 * din - dout
print(f"data-dir wire={data_dir} ({data_dir/dt/1e6:.3f} MB/s) -> ratio vs payload = {data_dir/payload:.4f}")
print(f"sack-dir wire={sack_dir} ({sack_dir/dt/1e6:.3f} MB/s) -> vs payload = {sack_dir/payload:.4f}")
print(f"both-dirs L4 ratio = {(din)/payload:.4f}   (+28B/dgram L3 ~ +{28*fwd*dt/r['elapsed_ms']/1000/payload:.4f})")
