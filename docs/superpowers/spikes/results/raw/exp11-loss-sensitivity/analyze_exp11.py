#!/usr/bin/env python3
"""Analyse exp11 loss-sensitivity raw JSON. Deterministic-cap cells only (bw 30MB/s)."""
import json, glob, os, re, statistics as st

D = os.path.dirname(os.path.abspath(__file__))
MIB = 1048576.0
# E1 calibration: 1344.7 datagrams/MiB both-dirs on a clean deterministic path;
# mean L4 datagram size 844.8 B => clean L4 wire/payload = 1.0834.
DG_PER_MIB_CLEAN = 1344.7
MEAN_L4 = 844.8
WIRE_RATIO = lambda fwd, recv: (fwd / (recv / MIB)) * MEAN_L4 / MIB if recv else float('nan')

def load(f):
    with open(os.path.join(D, f)) as fh:
        return json.load(fh)

def stalls(samples):
    """Intervals (100ms) whose delta < 10% of the cell's median nonzero delta."""
    d = [samples[i+1]-samples[i] for i in range(len(samples)-1)]
    nz = [x for x in d if x > 0]
    if not nz:
        return 0, 0.0, 0.0
    med = st.median(nz)
    thr = 0.10 * med
    cnt = tot = 0
    longest = cur = 0
    for x in d:
        if x < thr:
            cnt += 1; tot += 1; cur += 1
            longest = max(longest, cur)
        else:
            cur = 0
    return cnt, tot * 0.1, longest * 0.1

def cell(label_pat):
    files = sorted(glob.glob(os.path.join(D, label_pat + '.json')),
                   key=lambda p: int(re.search(r'-r(\d+)\.json$', p).group(1)))
    rows = []
    for p in files:
        d = load(os.path.basename(p))
        pc = d['per_conn'][0]
        recv = d['received_bytes']
        elapsed = d['elapsed_ms'] / 1000.0
        # goodput over the ACTUAL wall time of the transfer (not deadline)
        gp = recv * 8 / 1e6 / elapsed if elapsed else float('nan')
        cnt, stall_t, stall_long = stalls(d['samples'])
        rows.append(dict(
            rep=os.path.basename(p).replace('.json', ''),
            mbps=d['mbps'], wall_mbps=d['wall_mbps'], gp_recomputed=gp,
            recv_mib=recv / MIB, elapsed=elapsed,
            fwd=pc['shim_fwd'], drop=pc['shim_drop'], werr=pc['shim_write_err'],
            dg_per_mib=pc['shim_fwd'] / (recv / MIB) if recv else float('nan'),
            wire_ratio=WIRE_RATIO(pc['shim_fwd'], recv),
            stalls=cnt, stall_s=round(stall_t, 2), stall_long=round(stall_long, 2),
            chrome_cores=d.get('chrome_cores'), go_cpu=d.get('go_cpu_seconds'),
            rto_max_ms=d.get('rto_max_ms'), error=d.get('error'),
            complete=recv == 67108864,
        ))
    return rows

def summarize(name, rows):
    print(f'\n### {name}')
    hdr = f"{'file':<34} {'MiB':>7} {'s':>7} {'Mbps(json)':>10} {'Mbps(gp)':>9} {'dg/MiB':>8} {'wire':>6} {'drop':>4} {'#stall':>6} {'stallS':>7} {'longS':>6} {'chrome':>6} {'rto':>6}"
    print(hdr)
    for r in rows:
        print(f"{r['rep']:<34} {r['recv_mib']:>7.1f} {r['elapsed']:>7.2f} {r['mbps']:>10.2f} "
              f"{r['gp_recomputed']:>9.2f} {r['dg_per_mib']:>8.1f} {r['wire_ratio']:>6.3f} {r['drop']:>4} "
              f"{r['stalls']:>6} {r['stall_s']:>7.1f} {r['stall_long']:>6.1f} "
              f"{(r['chrome_cores'] or 0):>6.2f} {str(r['rto_max_ms']):>6}"
              + (f"  ERR={r['error']}" if r['error'] else ''))
    full = [r for r in rows if r['complete']]
    if full:
        g = [r['gp_recomputed'] for r in full]
        w = [r['wire_ratio'] for r in full]
        print(f"  -> complete n={len(full)}/{len(rows)}  goodput mean/min/max = "
              f"{st.mean(g):.2f} / {min(g):.2f} / {max(g):.2f} Mbps   "
              f"wire mean {st.mean(w):.3f}   dg/MiB mean {st.mean([r['dg_per_mib'] for r in full]):.1f}   "
              f"drop sum {sum(r['drop'] for r in rows)}")
    else:
        g = [r['gp_recomputed'] for r in rows]
        print(f"  -> NO complete runs; partial wall-goodput {min(g):.2f}-{max(g):.2f} Mbps")

if __name__ == '__main__':
    for rtt in (12, 71):
        for loss in ('0', '0.0001', '0.0003', '0.001', '0.003', '0.01'):
            summarize(f'sweep-rtt{rtt}-loss{loss}', cell(f'sweep-rtt{rtt}-loss{loss}-r*'))
    summarize('fieldcap-rtt12-loss0.009', cell('fieldcap-rtt12-loss0.009-r*'))
    for loss in ('0.001', '0.009'):
        for rto in ('rtodefault', 'rto200ms', 'rto500ms'):
            summarize(f'rto-rtt12-loss{loss}-{rto}', cell(f'rto-rtt12-loss{loss}-{rto}-r*'))
    for n in (1, 2, 3):
        summarize(f'sanity-pre-{n}', cell(f'sanity-pre-{n}'))
        summarize(f'sanity-post-{n}', cell(f'sanity-post-{n}'))
