# Experiment E5 — three never-swept pion knobs (FastRtxWnd / MaxRxBuf / MaxMsg) in `--mode prod` — 2026-09-18

**Verdict:** **All three knobs are null on the clean path, and none of them moves the loss-collapse
threshold in any material way.** `FastRtxWnd` (the retransmit-burst knob E11 nominated as the most promising
remaining lever) raises the rtt12 / loss 1×10⁻³ cell by at most ~+9 % and the rtt71 / loss 1×10⁻³ cell by
~+16 % — inside or barely outside a run-to-run spread of 1.37×, and still **3 % of the clean-path rate**
(5.2–6.0 Mbps vs 204 Mbps). It is a smaller lever than the RTO floor E11 measured (+14 % at the same loss),
not a fix. `MaxRxBuf` is a pure no-op on every cell (PASS), consistent with it being a **receive-side**
setting while the agent is the sender. `MaxMsg` is also a no-op above 65,600 B, and **catastrophic below
it**: our advertised max-message-size must be ≥ the largest message the transfer manager sends
(**65,536 B payload + framing**, boundary measured to the byte), otherwise the transfer dies with
`timed out waiting for receiver: 0/<size>` — 0 bytes, no JSON, exit 1. **Consequence for the fix hunt: the
collapse is not a retransmit-burst problem, so it must be attacked at the window/cwnd state itself
(`--mincwnd`) — see E14.**

**Setup:** lab harness `agent/cmd/benchdirect` in `.worktrees/benchdirect` on the lab Mac. **No field rig, no
container, no VM touched** (runbook §1). Every cell `--mode prod --size 64MiB --chunk 16KiB --bandwidth 30MB
--queue 5MB` (240 Mbps cap, 5 MB queue — the same cap as E11, so the numbers are directly comparable),
`--deadline 180` (300 for the loss arm), one `benchdirect` process at a time. Runner
`raw/exp5-pion-knobs/run-exp5.sh` (phases `sanity base fw rx msg loss topup tail`), log
`runner.log` — **93 runs, 0 runner failures, 8 cells failed with a recorded harness error** (all of them the
`maxmsg 16KiB`/`64KiB` cells, which is the finding, not an accident). Machine state (`uptime`,
`vm.swapusage`, top-8 CPU) recorded before **and** after every single run: `system-snapshots.txt`.

## 0. Prerequisite: the knobs were NOT wired for `--mode prod` (E2's correction, fixed here)

Confirmed as stated in the brief: before this session `runProd` built its `SettingEngine` with only
`SetIncludeLoopbackCandidate` and `SetSCTPMinCwnd`, and there was **no** `SB_SCTP*`/flag path for
`SetSCTPFastRtxWnd` / `SetSCTPMaxReceiveBufferSize` / `SetSCTPMaxMessageSize` anywhere in the benchdirect
tree. The minimal diff (3 files, `cmd/benchdirect/`):

- `main.go` — three new flags `-fastrtxwnd`, `-maxrxbuf`, `-maxmsg` (byte sizes via the existing
  `parseByteSize`), range-checked to `0 … 4294967295` (the `uint32` the pion setters take), passed through
  `runConfig`.
- `prodbench.go` — applies `SetSCTPFastRtxWnd` / `SetSCTPMaxReceiveBufferSize` / `SetSCTPMaxMessageSize`
  when > 0 (mirroring `rawbench.go`'s `SetSCTPRTOMax` block), records the values, and now also echoes
  `bandwidth_bps`, `queue_bytes` and `loss` (previously prod cells recorded `loss: 0` and `bw: 0` even when
  set). **Also: `runProd` now calls `shim.SetQueueBytes(cfg.queue)`** — prod previously ignored `-queue`
  and silently used the 100 ms derived buffer, which at 30 MB/s means 3 MB and ~860 tail drops per 64 MiB
  cell (the first smoke test gave 64 Mbps with `drop=860` for that reason alone). With `-queue 5MB`
  the prod cells are clean (`shim_drop = 0` in all 85 successful cells).
- `rawbench.go` — same three setters in the per-connection `SettingEngine`, plus the three values in the
  result struct. `rawResult` gained `fast_rtx_wnd`, `max_rx_buf`, `max_msg`, `min_cwnd`(see E14) fields.

Verification (a) the values appear verbatim in the JSON of every cell — e.g.
`"fast_rtx_wnd":65536,"max_rx_buf":1048576,"max_msg":262144` on a smoke run, and `"fast_rtx_wnd":262144`
in every `e5-fw256KiB-*` cell (the `fast_rtx_wnd` column of the appendix is read back *from the JSON*, so
the whole sweep is evidence the setter path is live); (b) out-of-range values are rejected verbatim:
`invalid -fastrtxwnd 8589934592: must be between 0 and 4294967295 (uint32)` (exit 2). `go build ./...` and
`go vet ./cmd/benchdirect/` both pass.

**Accepted ranges / semantics read from the pion source (`pion/webrtc/v4 v4.2.11`, `pion/sctp v1.9.4`):**
all three setters take `uint32` bytes, and `0` means "leave pion's default" (they are only applied when
non-zero). `SetSCTPFastRtxWnd` → `sctp.WithFastRtxWnd` → `association.go:1267`
`fastRetransWnd := int(max(a.MTU(), a.fastRtxWnd))` and `:1293` breaks the fast-retransmit burst when it
would exceed that many bytes. So **FastRtxWnd is a byte budget per fast-retransmit event, floored at the
MTU (1200 B)** — values below ~1200 B are no-ops, and the default (0 → MTU) allows roughly one packet per
event. `SetSCTPMaxReceiveBufferSize` → `sctp.WithMaxReceiveBufferSize`, default 1 MiB
(`initialRecvBufSize`), feeds the INIT `advertisedReceiverWindowCredit` and the receive payload queue.
`SetSCTPMaxMessageSize` → the `a=max-message-size` we advertise (`peerconnection.go:2929/3114`) and the
inbound reassembly limit (`datachannel.go:410`), default `defaultMaxSCTPMessageSize` = 1073741823 when
unset; our *outbound* limit comes from the remote's advertised value, not this one.

## 1. Clean path (loss 0): nothing moves, and that is the expected answer

`--mode prod`, 64 MiB, 240 Mbps cap. The 240 Mbps cap is not quite reached at rtt 71 (the sender idles
between burst/ack rounds), so the two RTTs have different plateaus — 229 Mbps at rtt 12 and 204 Mbps at
rtt 71.

| knob | value | rtt12 mean (min–max) n=2 | rtt71 mean (min–max) n=2 |
|---|---|---|---|
| — (baseline) | default | **229.0** (228.9–229.1) n=3 | 204.3 / 24.1 / 203.9 (bistable, n=3) |
| FastRtxWnd | 16 KiB | 228.0 (226.9–229.0) | 113.2 (27.5–199.0) *low-state draw* |
| FastRtxWnd | 64 KiB | 229.0 (229.0–229.0) | **204.4** (204.2–204.6) |
| FastRtxWnd | 256 KiB | 229.0 (229.0–229.0) | 203.7 (203.1–204.3) |
| FastRtxWnd | 1 MiB | 228.9 (228.9–229.0) | 123.7 (43.2–204.2) *low-state draw* |
| MaxRxBuf | 256 KiB | 229.0 (229.0–229.0) | 203.4 (202.3–204.6) |
| MaxRxBuf | 4 MiB | 229.0 (229.0–229.1) | 168.3 (132.1–204.4) *low-state draw* |
| MaxRxBuf | 16 MiB | 229.0 (229.0–229.0) | 196.1 (187.8–204.3) |
| MaxRxBuf | 64 MiB | 229.0 (229.0–229.1) | 118.8 (33.4–204.3) *low-state draw* |
| MaxMsg | 16 KiB | **FAIL** (both reps) | **FAIL** (both reps) |
| MaxMsg | 64 KiB | **FAIL** (both reps) | **FAIL** (both reps) |
| MaxMsg | 256 KiB | 228.9 (228.9–228.9) | 204.4 / 25.8 |
| MaxMsg | 1 MiB | 228.9 (228.9–229.0) | 22.1 / 12.7 *low-state draws* |

- **rtt12 is the anchor: 16 of 16 FastRtxWnd/MaxRxBuf/MaxMsg(≥256 KiB) cells sit in 226.9–229.1 Mbps
  (0.97 % spread) and `shim_drop = 0`.** The knob value cannot be inferred from any of them.
- **rtt71's 204 Mbps high state is reproduced by every arm**, and the occasional low-state draws
  (27.5, 43.2, 132.1, 33.4, 25.8, 22.1, 12.7) occur at *random* knob values including the baseline at
  loss 0 — they are the same **bistable-regime collapse E11 documented at rtt71/loss 0** (its caveat 6:
  191.2 vs 11.7/10.2 Mbps with zero added loss), not a knob effect. Reading them as knob effects would be
  wrong; they are the reason the brief says a null result must be stated plainly.
- Wire volume is flat across the whole clean sweep (86,057–89,218 datagrams per 64 MiB, ±1 %): nothing
  here is retransmitting, so a retransmit-burst knob has nothing to act on. **That is the mechanism behind
  the null.**

### MaxMsg is not a no-op below the message size — it is a hard failure

Every `-maxmsg 16KiB` and `-maxmsg 64KiB` cell (4 cells × 2 reps, both RTTs) failed identically. Verbatim
harness error (all 8 logs contain exactly this, nothing else):

```
bench failed: timed out waiting for receiver: 0/67108864
```

0 bytes delivered, the DataChannels opened, exit 1, **no JSON written**. Boundary located to the byte by
direct probes (prod, 1 MiB payload, 15 s deadline): `-maxmsg 65536` → **fails**,
`-maxmsg 65600` → 155.3 Mbps OK, `-maxmsg 70000` → OK, `-maxmsg 100000` → OK. In `--mode raw` (chunk
16 KiB) `-maxmsg 4KiB` → `sent=0 received=0`, `error=receiver timeout: 0/1048576 bytes`, while
`-maxmsg 16KiB` → 95.4 Mbps OK. The mechanism is the one read from the source: we advertise
`a=max-message-size` in the offer, **Chrome enforces it on what we send**, and the transfer manager's
largest outbound message is 65,536 B of payload plus its framing header (so 65,536 fails, 65,600 passes).
`--maxmsg` is therefore a **receive-side/negotiation knob with a hard floor at the sender's largest
message**; in v1 production it is inert for the data path (the agent is the sender) unless it is set below
the manager's message size, in which case it breaks the transfer outright. Note this also implies
`SetSCTPMaxMessageSize` in `peer.go` is *not* the knob that would let the agent send 256 KiB messages —
that limit comes from the *client's* advertisement.

## 2. FastRtxWnd under loss — the cell E11 nominated as decisive

Same cap, same rtt 12/71, prod mode, `--loss` is the only loss source (`shim_drop = 0` in every successful
cell). E11's raw-mode references at this cap: loss 1×10⁻³ → **28.3 Mbps**, loss 2×10⁻⁴ → **96.5 Mbps**.

**rtt 12, loss 1×10⁻³ (mean of n=3, min–max):**

| FastRtxWnd | mean | min | max | runs | wire datagrams | stalls (interior) |
|---|---|---|---|---|---|---|
| default (MTU = 1200 B) | 29.4 | 25.7 | 35.3 | 27.16 / 25.70 / 35.34 | 87,724 | 0 |
| 64 KiB | **32.1** | 29.8 | 36.1 | 30.35 / 29.80 / 36.13 | 88,531 | 9 (0.9 s) |
| 256 KiB | 30.1 | 27.3 | 32.1 | 30.82 / 32.11 / 27.27 | 88,050 | 0 |
| 1 MiB | 29.6 | 27.0 | 31.0 | 30.74 / 27.02 / 31.04 | 88,176 | 9 (0.9 s) |

**Null.** The first two reps of the default (27.16, 25.70) suggested a clean non-overlapping win at
256 KiB (30.82, 32.11); **the third interleaved rep (35.34) landed above every 64 KiB/256 KiB/1 MiB run and
destroyed the claim.** Final spread of the default arm is 25.7–35.3 (1.37×) against a knob effect of at
most +9 % → *inside the noise*. This is precisely the trap the runbook's bench-reliability note warns about,
and it is why the third rep was added.

**rtt 71, loss 1×10⁻³ (n=3):**

| FastRtxWnd | mean | min | max | runs | interior stall s | longest stall | ceiling (median of top-5 100 ms windows) |
|---|---|---|---|---|---|---|---|
| default | 5.21 | 4.69 | 5.78 | 4.69 / 5.78 / 5.16 | **73.7** | 0.5 s | 26.0 |
| 256 KiB | **6.03** | 5.26 | 6.56 | 6.56 / 6.26 / 5.26 | **47.8** | 0.2 s | 31.7 |

+16 % on the means with overlapping ranges (5.26 vs 5.78) — suggestive, not established, and the effect is
structural as much as volumetric: the 256 KiB arm spends **35 % less wall time in stalls** (73.7 s → 47.8 s)
with the same wire volume (87,819 vs 87,952 datagrams, −0.15 %) and no drops. So FastRtxWnd is *not* fixing
the window; it is shortening the per-loss recovery gap a little, exactly the shape of E11's RTO-floor
result (+14 % at 1×10⁻³, −73 % stall time). **Whatever the mechanism, 6.0 Mbps against a 204 Mbps clean-path
rate is 3 % — the collapse is untouched.**

**The cliff, loss 2×10⁻⁴ (n=2, the multi-stable transition region):**

| FastRtxWnd | rtt12 runs | rtt71 runs |
|---|---|---|
| default | 79.0 / 69.0 (mean 74.0) | 13.6 / 27.6 (mean 20.6) |
| 64 KiB | 83.2 / 141.8 | — |
| 256 KiB | 74.6 / 113.2 | 15.3 / 12.8 |
| 1 MiB | 95.7 / 56.3 | — |

**No interpretable signal** — every arm spans both attractors (E11 §1.2: the transition is bimodal and
"means across the cliff are not physical"). One probe on the way in is worth recording as a warning: an
8 MiB `rtt71/loss 2e-4` pilot gave default 33.9 Mbps vs 256 KiB **111.4** Mbps, which looked like a 3.3×
win; at 64 MiB **it did not reproduce** (12.8–15.3 Mbps). Single small runs in the transition region are
worthless; the 64 MiB n=2 data supersedes it.

## 3. Machine state and error bars (why the nulls are trustworthy)

- **The 240 Mbps cap makes the clean-path cells bit-reproducible while the machine is healthy:** rtt12
  baseline 228.85 / 229.08 / 229.08 (**±0.05 %**), and 16/16 clean sweep cells in 226.9–229.1.
- **The machine degraded during the session and the tail bracket caught it.** Baseline brackets, same
  config: 16:54 rtt12 = 229.0, 229.1, 228.9; 17:45 tail = 228.9, **53.4**, 228.7 (1 of 3 collapsed to a
  CPU-limited 50 Mbps plateau, `mediaanalysisd` at 106 % CPU + Time Machine, load avg 3.35 → 6.18);
  rtt71 16:54 = 204.3 / 24.1 / 203.9 vs 17:45 tail = 13.2 / 9.9 / 9.6 (the high state vanished entirely).
  Every loss-arm cell (the whole §2 table) was measured 16:57–17:16, i.e. **inside the healthy block**;
  the tail bracket is reported as the drift control, not as a knob result.
- `vm.swapusage` pinned at ~6.27–6.35 GB of 7.17 GB used throughout; load avg 2.9–6.2.
- 85/93 cells had `shim_drop = 0` and `shim_write_err = 0`; the 8 exceptions are the `maxmsg` failures
  above (writeErr = 0 there too). No cell was discarded for a write error.
- The rtt71/loss-0 low-state draws in §1 (2 of 16 sweep cells + 1 baseline) are the bench's known
  bistability: they appear in the *baseline* too, and they are the reason §1's rtt71 columns are reported
  as "high state, with N low-state draws".

## 4. Interpretation for the v1-vs-v2 decision

- **The three never-swept knobs are a dead end.** On a clean path all three are bit-identical no-ops, which
  is what the mechanism predicts (no loss ⇒ no retransmit burst to bound; MaxRxBuf/MaxMsg are receive-side
  while the agent is the sender). Under loss the only one that acts is FastRtxWnd, and it is worth ~+9 %
  (rtt12) to ~+16 % (rtt71) at 1×10⁻³ — the same order as the RTO floor, and it leaves the flow at 3 % of
  its clean-path rate. **E11's "send window collapses to ~10 datagrams" is not a retransmit-*burst* defect:**
  enlarging the burst does not re-open the window, because a fast retransmit does not add to flight size.
- **This makes the cwnd/`ssthresh` state the operative variable**, which is what E14 (`--mincwnd`) attacks
  directly, and it is the reason `peer.go`'s existing `SB_SCTP_MIN_CWND` / `SB_SCTP_CA_STEP` wiring is the
  more interesting pair than the three swept here.
- **`MaxMsg` and `MaxRxBuf` should be left alone in production.** `MaxRxBuf` provably does nothing for a
  sender; `MaxMsg` does nothing above ~65.6 KB and *breaks the transfer* below it. Any change here is pure
  risk.
- **For the v1 field story (112 Mbps vs a 246 Mbps path):** nothing in this experiment narrows that gap.
  It remains consistent with E11's client-sink explanation, not with a transport knob.

## 5. Caveats

1. **Lab, not field.** No field rig, container or VM was touched (runbook §1). No v1/v2 field numbers.
2. **The rtt12 numbers are cap-limited, the rtt71 numbers are not.** 229 Mbps is 95 % of the 240 Mbps cap;
   204 Mbps at rtt 71 is the sender's own burst/ack pacing below the cap. A delta at rtt 71 is therefore
   softer evidence than the same delta at rtt 12, and the rtt71 baseline is bistable.
3. **The loss-arm effects are inside or at the edge of the measured spread** (rtt12 1×10⁻³: default spread
   1.37×; rtt71 1×10⁻³: ranges overlap). They are reported as "marginal/suggestive", not as established
   deltas. n=3 per arm, one machine, one night, no statistical tests.
4. **`--rtomax` is still not applied in `--mode prod`** (E2's other finding). This session deliberately did
   not add it — the brief scoped the harness change to the three knobs — so no prod RTO comparisons exist,
   and the RTO-floor lever of E11 is still only measured in raw mode.
5. **The interior-stall metric differs from E11's.** Leading/trailing windows below 10 % of the median
   non-zero delta are trimmed first (prod cells have a 0.4–1.0 s chrome/SCTP ramp that would otherwise be
   counted as stalls); the definition is in `row.py`. Stall counts in §2 are therefore *interior* stalls
   (E11's rtt12 raw cells report the same 0 stalls; E11's rtt71 raw default at 1×10⁻³ reported 23 stalls /
   2.3 s, here 169–321 interior stalls / 17–32 s — prod mode's 3-lane manager is stallier than raw mode).
6. **MaxMsg boundary measured with probes outside the sweep** (65536 fails / 65600 passes, prod, 1 MiB
   payload, n=1 each) — enough to locate it to the framing header, not a distribution.
7. **The 8 MiB pilot mentioned in §2 is retained as a warning, not as data.**

## 6. Artifacts

Directory `docs/superpowers/spikes/results/raw/exp5-pion-knobs/`:

| file | what |
|---|---|
| `run-exp5.sh` | the runner (phases `sanity base fw rx msg loss topup tail`), one `benchdirect` at a time |
| `runner.log` | START/OK/FAIL per cell (93 runs; 8 `FAIL` = the `maxmsg` finding) + full stderr of every cell |
| `row.py` | JSON/log → TSV row extractor (goodput, ceiling, interior stalls, wire counters, applied knob values) |
| `cells.tsv`, `*.row` | one row per cell, appended **after every cell** (the incremental results-file mechanism) |
| `system-snapshots.txt` | `uptime` + `vm.swapusage` + top-8 CPU before and after every run, plus block snapshots |
| `<label>.json` / `<label>.log` | the raw result and the full stderr trace of each cell (85 JSONs) |
| `../2026-09-18-exp5-pion-knobs.md` | this file — appendix = every cell, verbatim |

Harness diff: `agent/cmd/benchdirect/{main.go,rawbench.go,prodbench.go}` (3 files, additive; see §0 —
descriptions of each hunk, not a patch, since git operations are forbidden for run agents).

## 7. Appendix — every cell (appended incrementally during the run)

`fast_rtx_wnd` / `max_rx_buf` / `max_msg` are read back **from each cell's JSON**, so a non-zero value here
is proof the setter path took effect. `ceil_mbps` = median of the five fastest 100 ms windows. Stalls are
interior. `mbps` is the browser-summary goodput (prod) / the same for raw.

| label | mbps | ceil_mbps | fast_rtx_wnd | max_rx_buf | max_msg | rtt | loss | wall_s | nstall | stall_s | longest_s | drop | write_err | fwd | error |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| sanity-raw-rtt71-r1 | 83.93 | 367.0 | 0 | 0 | 0 | 71 | 0 | 4.2 | 1 | 0.1 | 0.1 | 0 | 0 | 45739 |  |
| sanity-raw-rtt71-r2 | 10.78 | 22.3 | 0 | 0 | 0 | 71 | 0 | 25.9 | 13 | 1.3 | 1.0 | 0 | 0 | 44356 |  |
| e5-base-rtt12-r1 | 228.85 | 225.0 | 0 | 0 | 0 | 12 | 0 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86060 |  |
| e5-base-rtt12-r2 | 229.08 | 225.0 | 0 | 0 | 0 | 12 | 0 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86062 |  |
| e5-base-rtt12-r3 | 229.08 | 225.0 | 0 | 0 | 0 | 12 | 0 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86061 |  |
| e5-base-rtt71-r1 | 204.27 | 294.0 | 0 | 0 | 0 | 71 | 0 | 3.7 | 0 | 0.0 | 0.0 | 0 | 0 | 85892 |  |
| e5-base-rtt71-r2 | 24.05 | 152.0 | 0 | 0 | 0 | 71 | 0 | 23.4 | 3 | 0.3 | 0.1 | 0 | 0 | 89218 |  |
| e5-base-rtt71-r3 | 203.89 | 294.0 | 0 | 0 | 0 | 71 | 0 | 3.7 | 1 | 0.1 | 0.1 | 0 | 0 | 87845 |  |
| e5-fw16KiB-rtt12-r1 | 226.92 | 225.0 | 16384 | 0 | 0 | 12 | 0 | 2.8 | 1 | 0.1 | 0.1 | 0 | 0 | 88324 |  |
| e5-fw16KiB-rtt12-r2 | 229.04 | 225.0 | 16384 | 0 | 0 | 12 | 0 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86061 |  |
| e5-fw16KiB-rtt71-r1 | 198.97 | 299.0 | 16384 | 0 | 0 | 71 | 0 | 3.7 | 2 | 0.2 | 0.2 | 0 | 0 | 87553 |  |
| e5-fw16KiB-rtt71-r2 | 27.47 | 204.0 | 16384 | 0 | 0 | 71 | 0 | 20.6 | 2 | 0.2 | 0.1 | 0 | 0 | 89108 |  |
| e5-fw1MiB-rtt12-r1 | 228.93 | 225.0 | 1048576 | 0 | 0 | 12 | 0 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86062 |  |
| e5-fw1MiB-rtt12-r2 | 228.97 | 225.0 | 1048576 | 0 | 0 | 12 | 0 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86060 |  |
| e5-fw1MiB-rtt71-r1 | 204.24 | 273.0 | 1048576 | 0 | 0 | 71 | 0 | 3.7 | 0 | 0.0 | 0.0 | 0 | 0 | 85929 |  |
| e5-fw1MiB-rtt71-r2 | 43.25 | 246.0 | 1048576 | 0 | 0 | 71 | 0 | 13.5 | 0 | 0.0 | 0.0 | 0 | 0 | 89544 |  |
| e5-fw256KiB-rtt12-r1 | 229.04 | 225.0 | 262144 | 0 | 0 | 12 | 0 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86063 |  |
| e5-fw256KiB-rtt12-r2 | 229.04 | 225.0 | 262144 | 0 | 0 | 12 | 0 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86062 |  |
| e5-fw256KiB-rtt71-r1 | 204.31 | 231.0 | 262144 | 0 | 0 | 71 | 0 | 3.7 | 0 | 0.0 | 0.0 | 0 | 0 | 86061 |  |
| e5-fw256KiB-rtt71-r2 | 203.10 | 288.0 | 262144 | 0 | 0 | 71 | 0 | 3.7 | 1 | 0.1 | 0.1 | 0 | 0 | 87695 |  |
| e5-fw64KiB-rtt12-r1 | 229.04 | 225.0 | 65536 | 0 | 0 | 12 | 0 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86060 |  |
| e5-fw64KiB-rtt12-r2 | 229.01 | 225.0 | 65536 | 0 | 0 | 12 | 0 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86061 |  |
| e5-fw64KiB-rtt71-r1 | 204.23 | 294.0 | 65536 | 0 | 0 | 71 | 0 | 3.7 | 0 | 0.0 | 0.0 | 0 | 0 | 85885 |  |
| e5-fw64KiB-rtt71-r2 | 204.59 | 262.0 | 65536 | 0 | 0 | 71 | 0 | 3.7 | 0 | 0.0 | 0.0 | 0 | 0 | 85962 |  |
| e5-rx16MiB-rtt12-r1 | 228.97 | 225.0 | 0 | 16777216 | 0 | 12 | 0 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86060 |  |
| e5-rx16MiB-rtt12-r2 | 228.99 | 225.0 | 0 | 16777216 | 0 | 12 | 0 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86061 |  |
| e5-rx16MiB-rtt71-r1 | 187.79 | 288.0 | 0 | 16777216 | 0 | 71 | 0 | 4.0 | 1 | 0.1 | 0.1 | 0 | 0 | 88969 |  |
| e5-rx16MiB-rtt71-r2 | 204.34 | 225.0 | 0 | 16777216 | 0 | 71 | 0 | 3.7 | 0 | 0.0 | 0.0 | 0 | 0 | 85931 |  |
| e5-rx256KiB-rtt12-r1 | 228.98 | 225.0 | 0 | 262144 | 0 | 12 | 0 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86066 |  |
| e5-rx256KiB-rtt12-r2 | 229.04 | 225.0 | 0 | 262144 | 0 | 12 | 0 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86063 |  |
| e5-rx256KiB-rtt71-r1 | 204.55 | 288.0 | 0 | 262144 | 0 | 71 | 0 | 3.7 | 0 | 0.0 | 0.0 | 0 | 0 | 85923 |  |
| e5-rx256KiB-rtt71-r2 | 202.26 | 357.0 | 0 | 262144 | 0 | 71 | 0 | 3.7 | 1 | 0.1 | 0.1 | 0 | 0 | 87857 |  |
| e5-rx4MiB-rtt12-r1 | 228.97 | 225.0 | 0 | 4194304 | 0 | 12 | 0 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86062 |  |
| e5-rx4MiB-rtt12-r2 | 229.12 | 225.0 | 0 | 4194304 | 0 | 12 | 0 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86060 |  |
| e5-rx4MiB-rtt71-r1 | 132.13 | 346.0 | 0 | 4194304 | 0 | 71 | 0 | 5.1 | 3 | 0.3 | 0.1 | 0 | 0 | 89856 |  |
| e5-rx4MiB-rtt71-r2 | 204.40 | 294.0 | 0 | 4194304 | 0 | 71 | 0 | 3.7 | 0 | 0.0 | 0.0 | 0 | 0 | 85917 |  |
| e5-rx64MiB-rtt12-r1 | 229.09 | 225.0 | 0 | 67108864 | 0 | 12 | 0 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86061 |  |
| e5-rx64MiB-rtt12-r2 | 228.97 | 225.0 | 0 | 67108864 | 0 | 12 | 0 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86060 |  |
| e5-rx64MiB-rtt71-r1 | 33.39 | 252.0 | 0 | 67108864 | 0 | 71 | 0 | 17.1 | 3 | 0.3 | 0.1 | 0 | 0 | 89034 |  |
| e5-rx64MiB-rtt71-r2 | 204.27 | 288.0 | 0 | 67108864 | 0 | 71 | 0 | 3.7 | 0 | 0.0 | 0.0 | 0 | 0 | 85899 |  |
| e5-mm16KiB-rtt12-r1 |  | 0.0 | 0 | 0 | 0 |  |  | 0.0 | 0 | 0.0 | 0.0 |  |  |  | timed out waiting for receiver: 0/67108864 |
| e5-mm16KiB-rtt12-r2 |  | 0.0 | 0 | 0 | 0 |  |  | 0.0 | 0 | 0.0 | 0.0 |  |  |  | timed out waiting for receiver: 0/67108864 |
| e5-mm16KiB-rtt71-r1 |  | 0.0 | 0 | 0 | 0 |  |  | 0.0 | 0 | 0.0 | 0.0 |  |  |  | timed out waiting for receiver: 0/67108864 |
| e5-mm16KiB-rtt71-r2 |  | 0.0 | 0 | 0 | 0 |  |  | 0.0 | 0 | 0.0 | 0.0 |  |  |  | timed out waiting for receiver: 0/67108864 |
| e5-mm1MiB-rtt12-r1 | 228.96 | 225.0 | 0 | 0 | 1048576 | 12 | 0 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86062 |  |
| e5-mm1MiB-rtt12-r2 | 228.90 | 225.0 | 0 | 0 | 1048576 | 12 | 0 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86061 |  |
| e5-mm1MiB-rtt71-r1 | 22.08 | 157.0 | 0 | 0 | 1048576 | 71 | 0 | 25.4 | 3 | 0.3 | 0.1 | 0 | 0 | 88098 |  |
| e5-mm1MiB-rtt71-r2 | 12.73 | 47.0 | 0 | 0 | 1048576 | 71 | 0 | 43.3 | 8 | 0.8 | 0.1 | 0 | 0 | 87406 |  |
| e5-mm256KiB-rtt12-r1 | 228.94 | 225.0 | 0 | 0 | 262144 | 12 | 0 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86062 |  |
| e5-mm256KiB-rtt12-r2 | 228.89 | 225.0 | 0 | 0 | 262144 | 12 | 0 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86061 |  |
| e5-mm256KiB-rtt71-r1 | 204.35 | 283.0 | 0 | 0 | 262144 | 71 | 0 | 3.7 | 0 | 0.0 | 0.0 | 0 | 0 | 85898 |  |
| e5-mm256KiB-rtt71-r2 | 25.76 | 252.0 | 0 | 0 | 262144 | 71 | 0 | 21.9 | 4 | 0.4 | 0.1 | 0 | 0 | 88810 |  |
| e5-mm64KiB-rtt12-r1 |  | 0.0 | 0 | 0 | 0 |  |  | 0.0 | 0 | 0.0 | 0.0 |  |  |  | timed out waiting for receiver: 0/67108864 |
| e5-mm64KiB-rtt12-r2 |  | 0.0 | 0 | 0 | 0 |  |  | 0.0 | 0 | 0.0 | 0.0 |  |  |  | timed out waiting for receiver: 0/67108864 |
| e5-mm64KiB-rtt71-r1 |  | 0.0 | 0 | 0 | 0 |  |  | 0.0 | 0 | 0.0 | 0.0 |  |  |  | timed out waiting for receiver: 0/67108864 |
| e5-mm64KiB-rtt71-r2 |  | 0.0 | 0 | 0 | 0 |  |  | 0.0 | 0 | 0.0 | 0.0 |  |  |  | timed out waiting for receiver: 0/67108864 |
| e5-loss0.0002-fw1MiB-rtt12-r1 | 95.68 | 262.0 | 1048576 | 0 | 0 | 12 | 0 | 6.0 | 0 | 0.0 | 0.0 | 0 | 0 | 89066 |  |
| e5-loss0.0002-fw1MiB-rtt12-r2 | 56.33 | 115.0 | 1048576 | 0 | 0 | 12 | 0 | 10.0 | 0 | 0.0 | 0.0 | 0 | 0 | 87124 |  |
| e5-loss0.0002-fw256KiB-rtt12-r1 | 74.63 | 136.0 | 262144 | 0 | 0 | 12 | 0 | 7.7 | 0 | 0.0 | 0.0 | 0 | 0 | 88387 |  |
| e5-loss0.0002-fw256KiB-rtt12-r2 | 113.18 | 341.0 | 262144 | 0 | 0 | 12 | 0 | 5.2 | 0 | 0.0 | 0.0 | 0 | 0 | 89955 |  |
| e5-loss0.0002-fw256KiB-rtt71-r1 | 15.34 | 131.0 | 262144 | 0 | 0 | 71 | 0 | 36.1 | 7 | 0.7 | 0.1 | 0 | 0 | 89337 |  |
| e5-loss0.0002-fw256KiB-rtt71-r2 | 12.83 | 42.0 | 262144 | 0 | 0 | 71 | 0 | 42.9 | 11 | 1.1 | 0.2 | 0 | 0 | 86773 |  |
| e5-loss0.0002-fw64KiB-rtt12-r1 | 83.23 | 336.0 | 65536 | 0 | 0 | 12 | 0 | 6.9 | 0 | 0.0 | 0.0 | 0 | 0 | 90152 |  |
| e5-loss0.0002-fw64KiB-rtt12-r2 | 141.84 | 336.0 | 65536 | 0 | 0 | 12 | 0 | 4.3 | 1 | 0.1 | 0.1 | 0 | 0 | 90336 |  |
| e5-loss0.0002-fwdefault-rtt12-r1 | 79.00 | 220.0 | 0 | 0 | 0 | 12 | 0 | 7.2 | 0 | 0.0 | 0.0 | 0 | 0 | 87664 |  |
| e5-loss0.0002-fwdefault-rtt12-r2 | 69.00 | 220.0 | 0 | 0 | 0 | 12 | 0 | 8.3 | 0 | 0.0 | 0.0 | 0 | 0 | 87688 |  |
| e5-loss0.0002-fwdefault-rtt71-r1 | 13.61 | 84.0 | 0 | 0 | 0 | 71 | 0 | 40.5 | 2 | 0.2 | 0.1 | 0 | 0 | 87693 |  |
| e5-loss0.0002-fwdefault-rtt71-r2 | 27.62 | 184.0 | 0 | 0 | 0 | 71 | 0 | 20.5 | 1 | 0.1 | 0.1 | 0 | 0 | 90182 |  |
| e5-loss0.001-fw1MiB-rtt12-r1 | 30.74 | 115.0 | 1048576 | 0 | 0 | 12 | 0 | 17.9 | 0 | 0.0 | 0.0 | 0 | 0 | 88026 |  |
| e5-loss0.001-fw1MiB-rtt12-r2 | 27.02 | 58.0 | 1048576 | 0 | 0 | 12 | 0 | 20.3 | 9 | 0.9 | 0.9 | 0 | 0 | 88729 |  |
| e5-loss0.001-fw256KiB-rtt12-r1 | 30.82 | 136.0 | 262144 | 0 | 0 | 12 | 0 | 17.9 | 0 | 0.0 | 0.0 | 0 | 0 | 87940 |  |
| e5-loss0.001-fw256KiB-rtt12-r2 | 32.11 | 73.0 | 262144 | 0 | 0 | 12 | 0 | 17.2 | 0 | 0.0 | 0.0 | 0 | 0 | 88738 |  |
| e5-loss0.001-fw256KiB-rtt71-r1 | 6.56 | 37.0 | 262144 | 0 | 0 | 71 | 0 | 82.9 | 108 | 10.8 | 0.2 | 0 | 0 | 87811 |  |
| e5-loss0.001-fw256KiB-rtt71-r2 | 6.26 | 37.0 | 262144 | 0 | 0 | 71 | 0 | 86.9 | 137 | 13.7 | 0.2 | 0 | 0 | 88064 |  |
| e5-loss0.001-fw64KiB-rtt12-r1 | 30.35 | 89.0 | 65536 | 0 | 0 | 12 | 0 | 18.1 | 0 | 0.0 | 0.0 | 0 | 0 | 87969 |  |
| e5-loss0.001-fw64KiB-rtt12-r2 | 29.80 | 52.0 | 65536 | 0 | 0 | 12 | 0 | 18.5 | 0 | 0.0 | 0.0 | 0 | 0 | 87922 |  |
| e5-loss0.001-fwdefault-rtt12-r1 | 27.16 | 47.0 | 0 | 0 | 0 | 12 | 0 | 20.2 | 0 | 0.0 | 0.0 | 0 | 0 | 87452 |  |
| e5-loss0.001-fwdefault-rtt12-r2 | 25.70 | 68.0 | 0 | 0 | 0 | 12 | 0 | 21.3 | 0 | 0.0 | 0.0 | 0 | 0 | 87451 |  |
| e5-loss0.001-fwdefault-rtt71-r1 | 4.69 | 21.0 | 0 | 0 | 0 | 71 | 0 | 115.5 | 321 | 32.1 | 0.5 | 0 | 0 | 87876 |  |
| e5-loss0.001-fwdefault-rtt71-r2 | 5.78 | 31.0 | 0 | 0 | 0 | 71 | 0 | 94.0 | 169 | 16.9 | 0.3 | 0 | 0 | 88560 |  |
| e5-topup-loss0.001-fw1MiB-rtt12-r3 | 31.04 | 84.0 | 1048576 | 0 | 0 | 12 | 0 | 17.7 | 0 | 0.0 | 0.0 | 0 | 0 | 87774 |  |
| e5-topup-loss0.001-fw1MiB-rtt71-r3 | 6.36 | 26.0 | 1048576 | 0 | 0 | 71 | 0 | 85.4 | 138 | 13.8 | 0.2 | 0 | 0 | 87649 |  |
| e5-topup-loss0.001-fw256KiB-rtt12-r3 | 27.27 | 58.0 | 262144 | 0 | 0 | 12 | 0 | 20.1 | 0 | 0.0 | 0.0 | 0 | 0 | 87473 |  |
| e5-topup-loss0.001-fw256KiB-rtt71-r3 | 5.26 | 21.0 | 262144 | 0 | 0 | 71 | 0 | 103.5 | 233 | 23.3 | 1.1 | 0 | 0 | 87582 |  |
| e5-topup-loss0.001-fw64KiB-rtt12-r3 | 36.13 | 157.0 | 65536 | 0 | 0 | 12 | 0 | 15.3 | 9 | 0.9 | 0.9 | 0 | 0 | 89702 |  |
| e5-topup-loss0.001-fwdefault-rtt12-r3 | 35.34 | 84.0 | 0 | 0 | 0 | 12 | 0 | 15.6 | 0 | 0.0 | 0.0 | 0 | 0 | 88269 |  |
| e5-topup-loss0.001-fwdefault-rtt71-r3 | 5.16 | 26.0 | 0 | 0 | 0 | 71 | 0 | 105.1 | 247 | 24.7 | 0.3 | 0 | 0 | 87419 |  |
| e5-tail-rtt12-r1 | 228.93 | 225.0 | 0 | 0 | 0 | 12 | 0 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86060 |  |
| e5-tail-rtt12-r2 | 53.38 | 79.0 | 0 | 0 | 0 | 12 | 0 | 10.5 | 0 | 0.0 | 0.0 | 0 | 0 | 87013 |  |
| e5-tail-rtt12-r3 | 228.72 | 225.0 | 0 | 0 | 0 | 12 | 0 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86063 |  |
| e5-tail-rtt71-r1 | 13.19 | 79.0 | 0 | 0 | 0 | 71 | 0 | 41.8 | 5 | 0.5 | 0.1 | 0 | 0 | 88585 |  |
| e5-tail-rtt71-r2 | 9.87 | 31.0 | 0 | 0 | 0 | 71 | 0 | 55.5 | 21 | 2.1 | 0.1 | 0 | 0 | 86969 |  |
| e5-tail-rtt71-r3 | 9.55 | 26.0 | 0 | 0 | 0 | 71 | 0 | 57.3 | 59 | 5.9 | 1.8 | 0 | 0 | 88764 |  |
