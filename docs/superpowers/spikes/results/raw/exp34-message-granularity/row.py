#!/usr/bin/env python3
"""JSON/log -> one TSV row. Mirrors E6/E23's row.py conventions."""
import json
import os
import re
import subprocess

TSV_HEADER = [
    "label", "rc", "mode", "rtt", "cap", "chunk", "size", "mbps", "wall_mbps",
    "ceil_mbps", "wall_s", "recv", "sent", "fwd", "drop", "shim_write_err",
    "go_cpu", "go_cores", "chrome_cpu", "chrome_cores", "chunk_eff",
    "cs_per_gb", "chrome_cs_per_gb", "msg_per_s", "msg_total", "us_per_msg_go",
    "us_per_msg_chrome", "nstall", "stall_s", "error",
]


def ceiling(samples, n=5):
    """Best sliding n x 100 ms plateau in Mbps."""
    d = [samples[i + 1] - samples[i] for i in range(len(samples) - 1)]
    if len(d) < n:
        return 0.0
    best = 0
    for i in range(len(d) - n + 1):
        best = max(best, sum(d[i:i + n]))
    return best * 8 / (n * 0.1) / 1e6


def stalls(samples):
    """Interior 100 ms windows below 10 % of the median non-zero delta."""
    d = [samples[i + 1] - samples[i] for i in range(len(samples) - 1)]
    nz = sorted(x for x in d if x > 0)
    if not nz:
        return 0, 0.0
    med = nz[len(nz) // 2]
    # trim leading/trailing ramp
    first = next((i for i, x in enumerate(d) if x > 0), 0)
    last = len(d) - 1 - next((i for i, x in enumerate(reversed(d)) if x > 0), 0)
    cnt, secs = 0, 0.0
    for x in d[first:last + 1]:
        if x < 0.10 * med:
            cnt += 1
            secs += 0.1
    return cnt, secs


def make_row(label, cmd, rtt, chunk, size, bandwidth, jpath, wall, rc):
    row = {k: "" for k in TSV_HEADER}
    row.update(label=label, rc=rc, rtt=rtt, chunk=chunk, size=size,
               wall_s="%.2f" % wall, cap=bandwidth or "0")
    try:
        d = json.load(open(jpath))
    except Exception as e:  # noqa: BLE001
        row["error"] = "nojson:%s" % e
        return row
    pc = (d.get("per_conn") or [{}])[0]
    samples = d.get("samples") or []
    el = d.get("elapsed_ms") or 0.0
    el_s = el / 1000.0
    recv = d.get("received_bytes") or 0
    chunkv = d.get("chunk") or 0
    mbps = d.get("mbps") or 0.0
    row.update(
        mode=d.get("mode"), mbps="%.3f" % mbps, wall_mbps="%.3f" % (d.get("wall_mbps") or 0),
        ceil_mbps="%.1f" % ceiling(samples), recv=recv, sent=d.get("sent_bytes"),
        fwd=pc.get("shim_fwd"), drop=pc.get("shim_drop"),
        shim_write_err=pc.get("shim_write_err"),
        go_cpu="%.4f" % (d.get("go_cpu_seconds") or 0),
        go_cores="%.4f" % ((d.get("go_cpu_seconds") or 0) / el_s if el_s else 0),
        chrome_cpu="%.4f" % (d.get("chrome_cpu_seconds") or 0),
        chrome_cores="%.4f" % (d.get("chrome_cores") or 0),
        chunk_eff=chunkv,
        error=d.get("error", "") or "",
    )
    if el_s and recv:
        row["cs_per_gb"] = "%.3f" % ((d.get("go_cpu_seconds") or 0) / (recv / 1e9))
        row["chrome_cs_per_gb"] = "%.3f" % ((d.get("chrome_cpu_seconds") or 0) / (recv / 1e9))
    msgs = recv / chunkv if chunkv else 0
    msg_per_s = msgs / el_s if el_s else 0
    row["msg_total"] = int(msgs)
    row["msg_per_s"] = "%.1f" % msg_per_s
    if msgs and recv:
        row["us_per_msg_go"] = "%.1f" % ((d.get("go_cpu_seconds") or 0) / msgs * 1e6)
        row["us_per_msg_chrome"] = "%.1f" % ((d.get("chrome_cpu_seconds") or 0) / msgs * 1e6)
    nst, ss = stalls(samples)
    row["nstall"], row["stall_s"] = nst, "%.1f" % ss
    return row


def snapshot():
    out = []
    for c in (["uptime"], ["sysctl", "vm.swapusage"]):
        out.append("$ " + " ".join(c) + "\n" + subprocess.run(
            c, capture_output=True, text=True).stdout.strip())
    out.append("$ ps -Ao %cpu,comm -r | head -8\n" + "\n".join(
        subprocess.run(["ps", "-Ao", "%cpu,comm", "-r"], capture_output=True, text=True)
        .stdout.splitlines()[:9]))
    return "\n".join(out)


if __name__ == "__main__":
    import sys
    print(TSV_HEADER)
