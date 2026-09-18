#!/usr/bin/env python3
"""Analyze the instrumented nettop run: (t, bytes_in, bytes_out) series from
benchdirect per-process counters + the run JSON. Derives:
 - steady-state in/out byte rates (robust: median of 1s-window slopes)
 - data-direction wire rate = out_rate - in_rate (pion sends + shim fwd minus originals)
 - wire/goodput ratios L4 and L3 (IP+UDP +28/datagram)
 - token-bucket cross-check: R * active_window
"""
import json, re, sys
from statistics import median

nt_path, json_path, rate = sys.argv[1], sys.argv[2], float(sys.argv[3])
series = []
snap_t = None
for ln in open(nt_path):
    if ln.startswith("SNAP"):
        snap_t = float(ln.split()[1])
    else:
        m = re.search(r"benchdirect\.\d+\s+(\d+)\s+(\d+)", ln)
        if m and snap_t is not None:
            series.append((snap_t, int(m.group(1)), int(m.group(2))))
# monotonic filter + dedupe
series.sort()
clean = []
for t, i, o in series:
    if clean and t - clean[-1][0] < 0.05:
        continue
    clean.append((t, i, o))
# final values: use max (counter refresh jitter at process teardown)
final_in = max(i for _, i, _ in clean)
final_out = max(o for _, _, o in clean)
# rates: linear regression over samples after a warmup cut (skip first `cut`
# seconds so ICE/DTLS/ramp is excluded), on both counters.
def rates(cut=9.0, min_span=10.0):
    pts = [(t, i, o) for (t, i, o) in clean if t - clean[0][0] >= cut]
    if not pts or pts[-1][0] - pts[0][0] < min_span:
        pts = clean
    (t0, i0, o0), (t1, i1, o1) = pts[0], pts[-1]
    dt = t1 - t0
    return (i1 - i0) / dt, (o1 - o0) / dt, len(pts)

r = json.load(open(json_path))
recv = r["received_bytes"]
fwd = r["per_conn"][0]["shim_fwd"]
drop = r["per_conn"][0]["shim_drop"]
elapsed = r["elapsed_ms"] / 1000.0
goodput = recv / elapsed

rin, rout, nwin = rates()
print(f"recv={recv} fwd={fwd} drop={drop} elapsed={elapsed:.2f}s goodput={goodput/1e6:.3f} MB/s")
print(f"final_in={final_in} final_out={final_out}  (windows used: {nwin})")
print(f"in_rate={rin/1e6:.3f} MB/s out_rate={rout/1e6:.3f} MB/s")
data_rate = rout - rin          # data-direction wire (L4) volume rate
sack_rate = 2 * rin - rout      # in = data + sack; out = data + fwd(=data+sack) => sack = 2*in - out
print(f"data-dir wire rate={data_rate/1e6:.3f} MB/s  sack-dir rate={sack_rate/1e6:.3f} MB/s")
tot_l4 = rin                     # originals = full wire L4 both directions (approx steady state)
print(f"ratio L4 (both dirs)      = tot_l4/goodput    = {tot_l4/goodput:.4f}")
print(f"ratio L4 (data dir only)  = data_rate/goodput = {data_rate/goodput:.4f}")
dgr = fwd / elapsed
l3_extra = 28 * dgr
print(f"ratio L3 both dirs        = {(rin + l3_extra)/goodput:.4f}")
print(f"ratio L3 data dir only    = {(data_rate + 28*dgr)/goodput:.4f}")
bucket = rate * elapsed
print(f"bucket meter: R*elapsed   = {bucket/1e6:.3f} MB forwarded L4; mean L4 B/fwd-dgram = {bucket/fwd:.1f}")
print(f"wire L4 both dirs (totals)= in_rate*elapsed/1e6 = {rin*elapsed/1e6:.3f} MB; data-dir = {data_rate*elapsed/1e6:.3f} MB")
