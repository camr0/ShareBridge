# Experiment 35 — instrumented pion build: 1 Hz SCTP send-side trace (E32 follow-up) — 2026-10-02

**Status:** COMPLETE — patch written, compiles, vetted, unit-tested, and exercised end-to-end in the local
lab harness. **No remote host touched, no benchmark campaign run, nothing added to the git index.** Two
read-only `git status --short` probes were run at the very start before the constraint was re-read; no add,
commit, checkout or stash was ever issued, and the working tree is the only place changes live.

**Verdict.** A surgical patch — one new file in the fork plus one inserted line — exposes, at ~1 Hz,
everything needed to
discriminate A5 (cwnd floor), A1 (N PeerConnections) and A2/N5 (head-of-line blocking): `cwnd`, `ssthresh`,
`rwnd` (plus the peer's advertised window and the effective gate), bytes outstanding, and retransmissions
split RTO vs fast. It is inert unless `SB_SCTP_TRACE=1` (no goroutine, no timer, no write, one `os.Getenv`
per association when off). `go build ./...` and `go vet ./...` are clean both patched and unpatched, the four
new fork unit tests pass, and a live 64 MiB / RTT 70 ms cell emitted 5 correct 1 Hz lines with the switch on
and 0 with it off. Per-event retransmit attribution was **not** attempted (see "Not reachable").

## 1. What was patched

Two tracked files under `agent/forks/_trace/` (underscore directory, like `_bbr`, so the Go tool ignores it
until the script copies it into `forks/sctp`):

| File | Purpose |
| --- | --- |
| `agent/forks/_trace/sctp_trace.go` | `(*Association).TraceSnapshot()` + env-gated 1 Hz JSONL emitter |
| `agent/forks/_trace/sctp_trace_test.go` | 4 tests: snapshot correctness, inertness, enablement, interval floors |

`agent/apply_sctp_patch.sh` gained `patch_trace()` and one **one-line** insertion into the pristine
`association.go`, at the end of `createAssociationFromConfigWithTsn` (after every queue and timer exists, no
lock held, before any packet can be sent):

```go
	assoc.ackTimer = newAckTimer(assoc)

	assoc.startTrace()          // <- added; returns immediately unless SB_SCTP_TRACE is set

	return assoc
```

No other line of the fork changes: no packet path, no window arithmetic, no timer. `TraceSnapshot()` reads
`inflightQueue`/`pendingQueue`/`ssthresh`/`inFastRecovery` under `a.lock.RLock()` (the payload queues keep no
lock of their own) and `cwnd`/`rwnd`/counters via their atomics — one short read lock per second per
association.

The script's variant handling was made compositional (`rtomin+trace`, `bbr+trace`, …) while keeping every
existing variant — `none`, `rtomin`, `ssthresh`, `ssth768`, `bbr`, and the legacy `both` alias — working
unchanged (`run_sctppatch.sh` still calls `both`).

## 2. The switch and the trace format

```
SB_SCTP_TRACE=1            enable (anything else / unset = off)
SB_SCTP_TRACE_FILE=path    append JSONL to path instead of stderr (default)
SB_SCTP_TRACE_EVERY_MS=N   sample interval, default 1000, floor 10
```

One JSON object per line, one line per association per tick, written to **stderr** by default so it lands in
the agent's existing `log` output with no wiring. Counters are cumulative — diff consecutive lines. The
second command turns a whole run into per-interval deltas: `dRTO`/`dFast` are the retransmissions that
happened *in that interval*, and a negative `dSsthresh` is a loss signal in that interval.

```sh
# raw series
grep '^{"ts"' trace.jsonl | jq -c '{t:.ts[11:19],state,cwnd,ssthresh,rwnd,peerRwnd,eff:.effectiveWindow,out:.outstandingBytes,fill:.windowFill,srtt:.srttMs,rto:.retransRTO,fast:.retransFast}'

# per-interval deltas, which is how a loss-correlated stall is localised in time
grep '^{"ts"' trace.jsonl | jq -c -s '.[1:] as $r | .[0:-1] as $p | range(0; $r|length) as $i |
  {t:$r[$i].ts[11:19], cwnd:$r[$i].cwnd, eff:$r[$i].effectiveWindow, out:$r[$i].outstandingBytes,
   fill:$r[$i].windowFill, dRTO:($r[$i].retransRTO-$p[$i].retransRTO), dFast:($r[$i].retransFast-$p[$i].retransFast),
   dSsthresh:($r[$i].ssthresh-$p[$i].ssthresh)}'
```

Fields: `ts, pid, name, state, cwnd, ssthresh, rwnd, peerRwnd, effectiveWindow, mtu, outstandingBytes,
outstandingChunks, pendingBytes, inFastRecovery, srttMs, sentBytes, recvBytes, packetsSent, packetsReceived,
dataChunks, retransRTO, retransFast, retransTotal, ackTimeouts, sacksReceived, sacksSent, cwndFill,
windowFill`.

**Read `rwnd` carefully.** In pion it is not the peer's raw advertised window: it is a *remaining send
credit*, recomputed on every SACK as `a_rwnd - bytesOutstanding` (RFC 4960 §6.2.1 D.ii) and decremented as new
data is sent. The snapshot therefore also reports `peerRwnd` (= `rwnd + outstanding`, the peer's advertised
window, exact between SACKs) and `effectiveWindow` (= `min(cwnd, rwnd)`, the value `association.go` actually
gates on: `awnd := min32(a.CWND(), a.RWND())`). Comparing `cwndFill` with `windowFill` says which window
binds: `windowFill ≈ 1` with `rwnd < cwnd` means the peer/browser window is the constraint, `cwndFill ≈ 1`
with `cwnd < rwnd` means the sender's cwnd is.

## 3. What is reachable, and what is not

Reachable, all read non-invasively at 1 Hz:

- `cwnd`, `ssthresh`, `rwnd` — the three quantities that were invisible before, plus `peerRwnd`/`effectiveWindow`.
- `outstanding` in bytes **and** chunks (`inflightQueue`), plus `pendingBytes` (queued but not yet handed to SCTP).
- **RTO-vs-fast split is free.** This fork's `association_stats.go` already increments `nT3Timeouts` on the T3
  (RTO) path and `nFastRetrans` on the fast-retransmit path. Pion only ever *logged* them, once, inside
  `Close()`; the patch surfaces them as a running series, so diffing consecutive samples attributes each
  retransmission to a cause in time.
- `srttMs`, cumulative packets/DATA/SACK counters, `ackTimeouts`, `inFastRecovery`, association state.

Not attempted (deliberately, to keep the patch surgical):

- **Per-event** retransmit attribution. A "cause" field on each retransmit would need a second insertion into
  `association.go` at the two retransmit sites (fast path ~line 1335, T3 handler ~line 3472). The 1 Hz split
  localises a stall to a second, which is enough to correlate with the per-interval rate dip; a sub-second
  burst inside one interval is only visible as a counter jump.
- **The browser's SCTP state.** This instruments the agent/sender's association. `cwnd`/`outstanding` are
  therefore the agent's, while `rwnd`/`peerRwnd` are the agent's instantaneous view of Chrome's advertised
  window. Genuine browser-side `snd_cwnd` needs `chrome://webrtc-internals` or the client `getStats()` — out of scope.
- **A caveat the campaign must apply when reading the split:** `nFastRetrans` counts chunks actually
  retransmitted on the fast path, not "fast recovery entered". The fork halves `ssthresh`
  (`= max(cwnd/2, 4*MTU)`) on a dup-ACK/RACK loss signal even when nothing is retransmitted. Observed in the
  live run below: `ssthresh` fell 5,242,880 → 1,468,390 with `retransFast == 0` and `inFastRecovery == false`
  at both samples. So diff `ssthresh` across samples as well: a `ssthresh` drop with `retransFast` flat is a
  loss signal that cost no retransmit; `retransRTO` advancing is a real stall.

## 4. Build and packaging recipe

Built and verified locally (nothing was copied to VERSA, no remote host was contacted):

```sh
cd .worktrees/benchdirect/agent

# instrumented Linux binary (the trace patch is applied from the pristine copy each run)
./apply_sctp_patch.sh trace
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o bin/agent-instrumented-linux-amd64 ./cmd/agent
./apply_sctp_patch.sh none          # leave the shared tree unpatched again
```

Result: `agent/bin/agent-instrumented-linux-amd64`, 21,579,119 B,
`ELF 64-bit LSB executable, x86-64, statically linked`, and `strings` confirms the `SB_SCTP_TRACE` symbols.
This is exactly the binary the tests below ran against.

Packaging as a new image on the field rig (do this from the Mac; VERSA needs no toolchain and no `forks/`):

```sh
# 1. stage a runtime-only context (Dockerfile.instrumented + the prebuilt binary)
ssh ali@VERSA 'mkdir -p /home/ali/sharebridge-test/img-instrumented'
scp agent/Dockerfile.instrumented ali@VERSA:/home/ali/sharebridge-test/img-instrumented/Dockerfile
scp agent/bin/agent-instrumented-linux-amd64 ali@VERSA:/home/ali/sharebridge-test/img-instrumented/agent

# 2. build next to, and not over, sb-agent:pristine
ssh ali@VERSA 'cd /home/ali/sharebridge-test && docker build -t sb-agent:instrumented img-instrumented/'

# 3. run it with the switch on; capture stderr into the agent log
#    (same run command as pristine, plus:)
#      -e SB_SCTP_TRACE=1
#      -e SB_SCTP_TRACE_FILE=/app/trace.jsonl   # optional; default is stderr
```

`agent/Dockerfile.instrumented` is a deliberate copy of the *final stage* of the existing `agent/Dockerfile`
(`alpine` + `ca-certificates`, `WORKDIR /app`, `COPY agent .`, `EXPOSE 7878`, `CMD ["./agent", "daemon"]`) —
no builder stage, so the image cannot be rebuilt from different source than the binary that was tested.
**Not verified: the `docker build` itself.** The Docker daemon is not running on the lab Mac, so the context
was not built into an image; the Dockerfile is a byte-equivalent of the known-good runtime stage.

Why `benchdirect` is the right build tree: `diff -rq` shows `benchdirect/agent` and `e2e-v1/agent` differ only
in the `cmd/benchdirect` harness, `internal/peer/peer.go`'s opt-in `SB_SCTP_*` tuning (which returns pion's
default API when no `SB_SCTP_*` variable is set, per its own `apiForPeers` contract), and that file's test.
With no `SB_SCTP_*` and no `SB_SCTP_TRACE`, the two agents differ only by one `os.Getenv` per association —
which is why the trace can be built from the tree that owns the patch vehicle. If the campaign wants
`sb-agent:instrumented` to be built from the v1-direct tree instead, copy `apply_sctp_patch.sh` and
`forks/_trace/` into it and run the same three commands; nothing in the trace depends on `peer.go`.

## 5. Evidence

**(a) Compiles and vets, patched and unpatched.**

```
$ ./apply_sctp_patch.sh trace && go build ./... && go vet ./...
BUILD_OK
VET_OK
$ cd forks/sctp && go vet .        # vet the patched fork itself (deps are not vetted by the parent)
FORK_VET_OK
$ ./apply_sctp_patch.sh none && go build ./... && go vet ./...
BUILD_OK
VET_OK
$ go list -f '{{.Dir}} {{.Module.Replace.Path}}' github.com/pion/sctp
/…/benchdirect/agent/forks/sctp ./forks/sctp        # proves the fork is actually wired in
```

`go.mod`/`go.sum` are byte-identical to `e2e-v1`'s after `apply_sctp_patch.sh none` (the replace is added and
dropped, never committed) — verified by `diff`.

**(b) Fork unit tests (patched state), `cd forks/sctp && go test -run Trace -count=1`:**

```
--- PASS: TestTraceSnapshotReadsAssociationState   (cwnd/ssthresh/rwnd/peerRwnd/effective/outstanding)
--- PASS: TestTraceInertWhenDisabled               (0 bytes written, 0 goroutines created)
--- PASS: TestTraceEmitsWhenEnabled                (JSON line, all fields present, cwnd > 0)
--- PASS: TestTraceIntervalFloors                  (default 1 s; 0 -> default; 1 ms -> 10 ms floor)
ok  github.com/pion/sctp  0.27s
```

`TestTraceInertWhenDisabled` is the default-behaviour proof: the emitter writes nothing and the goroutine
count is unchanged.

**(c) Live end-to-end, local harness.** `go build -o bin/benchdirect ./cmd/benchdirect`, then one cell
`--mode prod --size 64MiB --chunk 16KiB --rtt 70 --bandwidth 30MB --queue 5MB --deadline 180`, once with the
switch off and once on:

| Run | `SB_SCTP_TRACE` | trace lines in stderr | reported Mbps |
| --- | --- | --- | --- |
| C | unset | **0** | 9.19 |
| D | `1` | **5** (≈1 Hz over a 5.5 s run) | 148.80 |

The two throughput numbers are **not** a comparison and no claim is made from them: C and D are the same cell
run minutes apart on a Mac shared with other agents (load average 3.85, 22 users) and differ by 16× in the
direction *opposite* to any plausible cost of the trace (one env lookup per association, one read lock per
second). Treat throughput here as unreliable, as the task brief instructed; the load-bearing evidence is
that the trace appears only when enabled.

The trace itself was correct and immediately informative — run D, 64 MiB over a 70 ms path:

```
16:01:15 state=CookieWait  cwnd=   4380 ssthresh=      0 rwnd=     0 eff=      0 out=      0 fill=0.000 srtt= 0 rto=0 fast=0 ackTO=0 sacksIn=0
16:01:16 state=Established cwnd=1798991 ssthresh=5242880 rwnd=2896002 eff=1798991 out=1797978 fill=0.999 srtt=73 rto=0 fast=0 ackTO=1 sacksIn=768
16:01:17 state=Established cwnd=1483934 ssthresh=1468390 rwnd=3207426 eff=1483934 out=1483038 fill=0.999 srtt=72 rto=0 fast=0 ackTO=1 sacksIn=10981
16:01:18 state=Established cwnd=1500734 ssthresh=1468390 rwnd=3171094 eff=1500734 out=1500618 fill=1.000 srtt=71 rto=0 fast=0 ackTO=1 sacksIn=19893
16:01:19 state=Established cwnd=1517534 ssthresh=1468390 rwnd=3154686 eff=1517534 out=1517026 fill=1.000 srtt=72 rto=0 fast=0 ackTO=1 sacksIn=28909
```

(`fill` = `windowFill`; the same-line `outstandingChunks`/`pendingBytes`/`peerRwnd`/`cwndFill` were elided for
width.) What it shows on this cell: outstanding tracks `min(cwnd, rwnd)` to within one chunk, `rwnd` credit
(≈3.1 MB) is well above `cwnd` (≈1.5 MB), so the sender is **cwnd-limited**, not peer-window-limited; cwnd
sits at roughly the 70 ms BDP for the observed rate; there are **no retransmissions at all** (RTO and fast
both 0) yet `ssthresh` is halved once — the loss-signal-without-retransmit case flagged in §3. One cell on a
loaded machine is not a conclusion; it is proof the instrumentation now sees the discriminator.

## 6. Files touched

Working tree only (no commit; `git status --short` was the only git command issued, read-only, at the start):

- `agent/forks/_trace/sctp_trace.go` — **new** (the patch source).
- `agent/forks/_trace/sctp_trace_test.go` — **new** (the 4 tests).
- `agent/apply_sctp_patch.sh` — **modified** (`patch_trace()`, compositional variants, `both` kept as an alias).
- `agent/Dockerfile.instrumented` — **new** (runtime-only packaging for the field rig).
- `agent/bin/agent-instrumented-linux-amd64` — **new** build artifact (`bin/` is gitignored).
- `agent/bin/benchdirect` — rebuilt from the patched source (`bin/` is gitignored).
- `docs/superpowers/spikes/results/2026-10-02-exp35-instrumentation-build.md` — this note.

Reverted / left as found: `agent/go.mod` and `agent/go.sum` (replace dropped; `diff` against `e2e-v1` shows
no residual change), `agent/forks/sctp/` (restored pristine, gitignored), `agent/forks/sctp-pristine/`
(untouched), `internal/peer/peer.go` (untouched), client JS and `.worktrees/e2e-v1` (untouched). Scratch
output from the live runs is outside the repo in `/tmp/e35/`.
