#!/usr/bin/env python3
"""Calibrate netstat -I lo0 -b accounting: send N UDP datagrams of S payload
bytes over loopback, print packet/byte deltas so the per-datagram accounting
overhead (IP+UDP header bytes included by the counter) can be derived."""
import socket, subprocess, sys, time

N = int(sys.argv[1]) if len(sys.argv) > 1 else 20000
S = int(sys.argv[2]) if len(sys.argv) > 2 else 1000

def snap():
    out = subprocess.run(["netstat", "-I", "lo0", "-b", "-d"],
                         capture_output=True, text=True).stdout.splitlines()
    for ln in out:
        f = ln.split()
        if f[0] == "lo0":
            return int(f[7]), int(f[10])  # Opkts, Obytes (link line: name mtu addr ipkts ierrs ibytes opkts oerrs obytes coll)
    raise SystemExit("no lo0 line")

rx = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
rx.bind(("127.0.0.1", 0))
addr = rx.getsockname()
rx.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 4 << 20)

tx = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
payload = b"x" * S

p0, b0 = snap()
t0 = time.time()
sent = 0
recv = 0
import threading
stop = threading.Event()
def drain():
    global recv
    rx.setblocking(False)
    while not stop.is_set():
        try:
            d = rx.recv(65536)
            recv += 1
        except BlockingIOError:
            time.sleep(0.001)
th = threading.Thread(target=drain); th.start()
for i in range(N):
    tx.sendto(payload, addr)
    sent += 1
tx.close()
time.sleep(0.5)
stop.set(); th.join()
p1, b1 = snap()
dt = time.time() - t0
dp, db = p1 - p0, b1 - b0
print(f"sent={sent} drained={recv} dt={dt:.2f}s dOpkts={dp} dObytes={db}")
if dp > 0:
    print(f"bytes/pkt={db/dp:.2f} payload={S} implied_overhead={db/dp - S:.2f}")
