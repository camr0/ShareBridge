# ACTIONABLE FINDINGS — 2026-09-18 overnight experiments

**Purpose:** if you are an agent (or human) asked to *"review these experiments and pick the good ones to
add to main"*, read this file first. It classifies every finding so the category most likely to be missed
does not get missed.

## Classify every finding into one of three buckets — (b) is the one that gets lost

| bucket | meaning |
|---|---|
| **(a) ALREADY IN MAIN** | the code is committed; there is nothing to add |
| **(b) IN MAIN BUT NOT ENABLED / NOT DEPLOYED** | **the code exists, but the product does not get the benefit.** A reviewer looking only at `main` will see the feature and wrongly conclude "done" |
| **(c) NOT IN MAIN** | needs a code change |

**Do not stop at "is this in main?" — ask "does production actually run with it?"** For this codebase,
check deployment env explicitly (see F1 for the worked example).

---

## F1 — **(b)** CA-step tuning is committed, tested, and **NOT enabled in production**. Measured upside ~1.89×.
The highest value/effort item in this document; it needs no new engineering.

- **The knob:** `SB_SCTP_CA_STEP=32768` → `se.SetSCTPCwndCAStep(v)`.
- **In main (branch `main`, worktree `.worktrees/e2e-v1`):** `agent/internal/peer/peer.go:125-127`, with the
  rationale documented in code at `:91-97` and a unit test at `agent/internal/peer/sctp_tuning_test.go`.
- **The gap:** the guard is `if v := sctpEnvUint(getenv, "SB_SCTP_CA_STEP"); v > 0` — **unset means stock
  behaviour**. No default is set in code, and no deployment config in this repo sets it.
- **Direct evidence (2026-09-18):** the production container `sharebridge-agent` (UI port **7878**) has
  **no `SB_SCTP_*` variables set at all**; the test rig `sb-run` has `SB_SCTP_CA_STEP=32768`.
- **Consequence:** *every throughput number in this directory was measured with the tune ON.* The product's
  out-of-the-box direct-transfer performance is expected to be roughly half.
- **Field value of the tune — now measured in THIS configuration (2026-09-18, E13):** stock
  (`run-variant.sh u`) vs tuned (`c`), n=2 per client, 754 MiB, click-only, agent-side windows:
  **CLIENT-EAST 84.45 / 86.68 Mbps stock (mean 85.6) → 102–111 tuned = 1.19–1.30×**;
  **CLIENT-WEST 50.38 / 48.90 stock (mean 49.6) → 60.2–61.4 tuned = 1.21–1.24×.**
  Rep spread ≤1.5% and no overlap between the stock and tuned cells — though the tuned baseline is a
  different session rather than an interleaved same-session A/B (caveat noted in the report).
- **So the honest multiplier is ~1.2×, not the 1.89× recorded in the 2026-09-15 campus session**
  (`.worktrees/e2e-v1/docs/superpowers/spikes/2026-09-15-e2e-campus-home-field-test.md`, direct
  49.6 → 93.6 Mbps). **That figure does not generalise to this path** — quote the 1.2× when justifying
  the change.
- **What users currently get:** production ships stock (see the direct evidence above), so out-of-the-box
  direct throughput is **~86 Mbps (CLIENT-EAST) / ~50 Mbps (CLIENT-WEST)**, and ~86% of every number
  quoted elsewhere in these documents (102–111 / 60.2) already has the tune applied.
- **Action:** enable `SB_SCTP_CA_STEP=32768` in the production deployment **or** make it the code default,
  then re-measure with the same field methodology. Note it is a **production** change — requires operator
  approval, not an autonomous edit.
- **Caveat:** the *stock* baseline was never re-measured in tonight's configuration (see F6), so the exact
  in-configuration delta is not yet known. The 1.89× is from a different field session.

## F2 — **(c)** The real v1-direct defect: a **send window that collapses to ~10 datagrams** under tiny loss
This is the mechanism any further optimization must target.
- Measured collapse (lab, rtt 12 ms, bottleneck capped at 240 Mbps, E11): loss 0 → **225.2**, 1e-4 → **211.0**,
  **2e-4 → 96.5**, 3e-4 → 60.1, 1e-3 → 28.3, 9e-3 → 8.6 Mbps. A **cliff between 1e-4 and 2e-4**, not a gradient.
- The collapsed rate scales as **1/RTT** (5.0–5.6× for a 5.9× RTT step) ⇒ **window-limited, W ≈ 10 datagrams**.
  This explains why v1 direct halves as RTT doubles in the field (111 → 60 Mbps) and why striping cannot help.
- The sender **idles rather than congesting**: wire ratio drifts only 1.089→1.140, `shim_drop` = 0 throughout,
  and Chrome's CPU *falls* 0.70 → 0.04 cores as loss rises.
- **Corroborating pre-existing insight:** `docs/superpowers/specs/2026-04-16-slice13-libp2p-transport-design.md`
  already noted "Throughput = window / RTT" and that the agent side is untuned. Tonight quantifies it.
- **Levers, in order:** (i) `min-cwnd` **under loss** (a cwnd floor is the direct counter to the collapse; the
  existing "min-cwnd has published results" note only covers *clean* paths, which is the one regime where the
  collapse does not occur) → (ii) `FastRtxWnd` under loss → (iii) pacing / chunk size → (iv) if knobs fail,
  **patch the sctp congestion control** using the existing tooling `agent/apply_sctp_patch.sh`
  (`go mod edit -replace github.com/pion/sctp=./forks/sctp`); the failure is a control-loop defect, so this is
  the surgical option.

## F3 — **(c)** Untested knobs already wired in main
`agent/internal/peer/peer.go:129-152` reads `SB_SCTP_MIN_CWND`, `SB_SCTP_FAST_RTX_WND`, `SB_SCTP_MAX_RX_BUF`,
`SB_SCTP_MAX_MSG`, `SB_SCTP_RTO_MAX_MS`. None of the first four has been swept **under loss** (the only regime
that matters for this defect). Note the harness needs the wiring added to `prodbench.go` before it can sweep
them (the harness has no `SB_SCTP*` env lookup; E2 proved prod-mode sweeps of `--rtomax` were silent no-ops).

## F4 — Fork status: **NOT removed**, currently unused — and **do not delete the patch tooling blindly**
- The fork is **not wired into any build**: `go.mod` has no `replace` in either tree (`.worktrees/e2e-v1/agent/go.mod`
  and `.worktrees/benchdirect/agent/go.mod` both resolve upstream `pion/sctp v1.9.4`), and **no `sb-agent:fork`
  image exists** on the rig — only `sb-agent:pristine` and `sb-agent:phase4a-test`. So rig variants `s`/`f`
  would fail outright if anyone tried them.
- The Sept-16 field report recommended deleting it (*"the fork is dead weight … fork ≈ CA-step-only within
  noise. Recommendation: delete the fork, keep the `SB_SCTP_CA_STEP` env knob"*, `2026-09-16-71ms-rtt-patch-field-test.md`).
  **That was a recommendation; nothing was deleted** (the BBR patch source is still at `agent/forks/_bbr`).
- **However — do not delete the tooling as a blunt cleanup.** `agent/apply_sctp_patch.sh`
  (`go mod edit -replace github.com/pion/sctp=./forks/sctp`) is the **only vehicle for a code-level
  congestion-control patch**, because **`ssthresh` is not one of the six exposed `SettingEngine` knobs**
  (documented in `2026-09-15-transport-investigation-results.md:97`). The *BBR patch content* is dead
  (BBR-lite lost to stock because pion does not pace), but the tooling is the route to F2's fallback fix.
- Note the first-line fix for F2 needs **no fork at all**: `SB_SCTP_MIN_CWND` → `SetSCTPMinCwnd` is already
  exposed (see F3). Whether pion actually clamps `cwnd` to that minimum **after an RTO** (`ssthresh =
  max(cwnd/2, 4*MTU); cwnd = 1 MTU`) is precisely what the queued experiment must determine — if it does,
  the window collapse is fixable with a knob; if it does not, the fork becomes necessary.

## F5 — **(b)** v1's **relay** mode is the weakest of the four paths — do not treat it as a fallback
Measured 2026-09-18 (E12): v1 relay **41.99 Mbps** (CLIENT-EAST, n=2) and **55.31 Mbps** (CLIENT-WEST, n=2),
versus v1 **direct** 102–111 / 60.2, and v2 relay 233 / 215–228. So v1 relay is 2.43–2.64× *slower* than v1
direct on the fast path. Any decision that assumes "if direct fails we can fall back to relay at similar speed"
is wrong without separate work. (Also: relay was *slower on the lower-RTT path*, pointing at the relay server or
its per-message framing rather than RTT.)

## F6 — **RESOLVED (2026-09-18, E13)** — the stock baseline is now quantified
The rig supports variant `u` (stock, no CA-step) vs `c` (CA-step); E13 measured **both** on both clients:
stock **85.6 Mbps** CLIENT-EAST / **49.6 Mbps** CLIENT-WEST versus tuned **102–111** / **60.2–61.4**
⇒ **1.19–1.30× / 1.21–1.24×**. Rep spread ≤1.5%, no overlap. Full detail in
`results/2026-09-18-exp13-stock-vs-ca-step.md`. See F1 for what this means for the product.

## F7 — Context: the architecture comparison (for prioritization, not a code change)

> **Correction (2026-09-18):** an earlier version of this work claimed E8b *falsified* the per-client-host
> cap hypothesis. That comparison used E8b's **window-average** (89.9 Mbps) against a **sum of per-session
> rates** (158.7) — miscalibrated. The **concurrent** rate in that cell was 88.8 + 45.4 = **134.2 Mbps**,
> i.e. **1.38× a single host** (97.3), versus only **1.09× for two tabs on one host**. So per-host limits
> are real, not falsified — and the client-app finding in F9 is consistent with that.
Matched field cells, same clients/paths/hour, 754 MiB per cell, v1 measured agent-side, v2 at 87–97% of path
capacity:
- **v1 direct:** EAST 102–111 Mbps (n=3), WEST 60.2 (n=2–3).
- **v2 relay:** EAST 233.3–233.7 (n=3, spread 0.18%), WEST 214.9–228.4 (n=2); a real browser-sink cell gave
  **239.05 Mbps**, so the curl cells were not flattering v2.
- Path capacity: **~246 Mbps UDP** (both hosts), 240 Mbps over 4 TCP flows.
- **v1 halves as RTT rises; v2 barely moves** — so the v2 advantage grows with distance (2.1–2.3× at 12 ms,
  3.6–3.8× at 71 ms).
- **Corroborated by earlier work, from the other direction:** the 2026-09-15 investigation compared SCTP against
  **kernel TCP under `tc netem` at the same nominal settings** (rtt ≈100 ms, 64 Mbps cap, ~800 KB queue): SCTP
  N=1 **8.7 Mbps** vs kernel TCP **54.7 Mbps**, 0 drops; at 1% loss, SCTP 1.1 Mbps vs TCP 47.2 Mbps. Tonight's
  field result — v1's userspace SCTP at 112 Mbps against a kernel-TCP transport at 233 Mbps on the same path —
  is the same phenomenon at a different scale.

## F8 — **(b)** `SB_SCTP_MIN_CWND` — **CLOSED. No field value at any size.** Plus a reusable BDP rule
- **Lab (E14, 240 Mbps cap / rtt 12 / 5 MB queue):** a 2 MiB floor took 27.9 → **204.8 Mbps** at 1e-3 loss
  and 64.1 → **225.0** at 2e-4, and was a no-op on a clean path; mechanism confirmed in code (pion floors
  every cwnd write to `minCwnd`, incl. the RTO path).
- **Field (E17/E18): it does not transfer.** 2 MiB (≈12× BDP) **broke it outright** — 0 of 4 cells
  completed, one trickled at ~2.7 Mbps. Small floors were **harmless but useless**: 32 KiB 122.2,
  64 KiB 100.7, 128 KiB 115.6 Mbps, all inside the same-session stock spread (117.7 / 124.4 / 98.9).
  256 KiB (≈1.6× BDP) degraded 3–4× (36.5 Mbps).
- **Why:** the lab benefit depended on its synthetic 5 MB queue absorbing the overshoot (the 2 MiB floor was
  ~5.8× the 360 KB BDP there); the field's queue is far smaller. And **the field never enters the lab's
  collapse regime at all**: its measured in-flow loss is **0.27–0.68%** (E19), 15–45× the loss E11 inferred,
  yet the sender holds **90–111 Mbps** where E11's ladder predicts 15–18 — so the "collapse" is an artifact
  of the lab's uniform-random drop model, and there is no field window collapse for a floor to prevent. See F10.
- **Reusable rule:** never set a cwnd floor above ≈1× BDP on this path (BDP = rate × RTT ≈ 165 KB at
  12 ms × 110 Mbps). Degradation begins by ~1.6× BDP; breakage by ~12× BDP.
- **Consequence for the goal:** the *exposed* SCTP knob space for v1 direct is now exhausted — CA-step
  (~1.2×, E13) is the only positive, and RTO-max, FastRtxWnd, MaxRxBuf, MaxMsg and min-cwnd are all null or
  harmful in the field. Making v1 direct appreciably faster now requires either a real
  congestion-control patch (much less attractive given the path has no collapse to fix at its operating
  rate) or work outside the transport (see F9).

## F9 — **(b)** The client app sink — the best remaining **user-visible** win, and transport-independent
- **Field A/B (E15):** removing SHA-1 verification + StreamSaver writes raised the **agent's own send
  window** by **23.8%** (CLIENT-EAST 112.9 → 151.0 Mbps) and **15.6%** (CLIENT-WEST 63.9 → 75.9) — the sink
  throttles the *transport* through flow control, not just the UI.
- **Plus an ~80 s post-transfer tail:** in the control arm the client UI reached `verified` **78.7 s / 82.0 s
  after** the agent finished sending a ~60 s transfer; with the sink removed, completion was **0.21 s** after.
  The user-visible download is therefore ~**2.3×** the transport window (~139 s vs ~60 s for 754 MiB), and
  the rate a user actually experiences is ~45 Mbps while the transport moved ~105.
- **Actionable, in app code:** move SHA-1 to a Worker with `crypto.subtle`, avoid double-buffering through
  the service worker, or skip verification for direct transfers. **Applies to v2 as well** — any transport
  feeding this sink pays it.
- Caveat: measured on the TESTBOX test-web-root copy with a byte-count-validating discard sink; the real fix
  needs its own A/B (`results/2026-09-18-exp15-app-sink-isolation.md`).

## F10 — The loss story, corrected: the field is lossy, and it doesn't matter
**This section supersedes the loss narrative in the earlier overnight summary (§3 of
`2026-09-17-transport-tier1-tier2-experiments.md`) and in E11's interpretation.**
- **Measured in-flow loss on a real v1 direct transfer (E19, client-side capture, DTLS record-seq gaps):**
  CLIENT-EAST **2.70×10⁻³** (at 111.4 Mbps) and **3.87×10⁻³** (at 90.6 Mbps); CLIENT-WEST **6.77×10⁻³** (at
  58.8 Mbps). The **ACK direction had zero loss**, and retransmits close at ≈1 retransmission per loss.
- **The open-loop iperf3 figure (0.43% at 100 Mbps) was right; E11's ~1.5–2×10⁻⁴ inference was wrong by
  15–45×.** The inference extrapolated a lab ladder; the field sender never occupies that regime.
- **The lab's loss model is the artifact, not the field.** At 2.7–3.9×10⁻³ real loss the field delivers
  90–111 Mbps where E11's ladder predicts 15–18 Mbps (5.6–7× gap) — consistent with the shim's
  **uniform-random per-datagram** drops being far more damaging than the field's likely **bursty queue** drops.
- **Therefore:** every loss-tolerance lever (RTO floor E2/E11, min-cwnd floors E14/E17/E18) was calibrated
  against a lab-only regime and is correctly closed. There is **no field window collapse to fix**, and the v1
  field ceiling is **loss-independent** — it is the client sink (F9, ~24%) plus the sender/userspace path.
- **Do not use SNMP UDP counters on this rig** — they undercount by 50–3000× under GRO/GSO at these rates
  (measured at both endpoints). NIC `tx_packets` on the sender is wire-exact but the host's background traffic
  forbids 10⁻⁴ resolution.
- **Assert the path mode in every cell:** silent relay fallback hit **2 of 6** direct attempts in one session,
  and relayed cells read ~44.7 Mbps — which looks like a legitimate slow measurement unless you check for
  `DataChannel lanes ready` and an unused relay standby.

## F11 — The send path's **per-byte CPU cost**, measured — and a correction to the old "225 Mbps ceiling"
- **E22 (39 cells, 0 failures, all rate-capped where a mean is quoted):** the v1 `prod`-mode send path costs
  **~46 CPU-s/GB** — three independent fits give 45.8 / 46.6 / 47.5. The cost is **per byte, not fixed
  overhead**: the size sweep (16/32/64/128 MiB at a pinned rate) fits `go_cpu = +0.022 s + 0.0464 s/MiB`,
  **R²=0.998** — a line through the origin (128 MiB costs 8.5× what 16 MiB does).
- **0.52–0.58 cores per 100 Mbps** above 150 Mbps (0.71 at 37 Mbps; that rise is the harness's own 50 ms poll
  loop, fitted at 0.03–0.05 cores, not pion). **One core saturates at ~180 Mbps** (fits 181/184/187;
  measured 169–202 Mbps/core).
- **Correction — the previously quoted 225 Mbps "clean-path ceiling" was a rate CAP, not a ceiling.** It came
  from E14, whose runs were capped near 240 Mbps. Uncapped at rtt 12 the **same production stack does
  **521–533 Mbps** (at 3.35 cores). So the pion send path is **not inherently slow**, and the lab-vs-field gap
  is **~4.3×, not 2×**. Any earlier statement of the form "the lab's clean-path ceiling is ~225" describes the
  cap; the corresponding *knob* nulls in E5 must be read as "no effect at a rate the stack was not
  constrained by" and are weakest exactly where they were measured (the clean path), while the *loss-path*
  E5/E11/E14 results sit far below any cap and are unaffected.
- **Field bridge:** VERSA's implied cost is **63–113 CPU-s/GB (1.4–2.4× the Mac's)** — consistent with a server
  core being slower than an Apple-silicon core — which predicts one core saturating at **75–129 Mbps**,
  **bracketing the field's measured 122 Mbps (EAST) and 68 (WEST)**. The *shape* transfers; absolute cores/Mbps
  does not.
- **What this does NOT establish:** that the field is sender-CPU-**bound**. A sender using ~1.3 cores while
  idle cores sit unused is consuming CPU as a *cost*, not necessarily starving. The discriminator is the field
  `--cpus` sweep (E21); the per-layer attribution (what a fork should target) is E23.
- **Why it matters for the fork question:** a fork is justified only if the field cap is per-byte CPU in the
  send path. E22 makes that *plausible and quantitative* (one core ≈ 180 Mbps on a fast core, ≈ 75–129 on the
  server's) — but the previously failed fork (BBR-style pacing) attacked the wrong layer, since E19 shows the
  field rides its 0.27–0.68% loss with ~1 retransmission per loss.

## F12 — Where the send path's CPU goes (E23): crypto and the app layer are free — but two harness artifacts bound the result
- **E23 (63 cells, 0 failures, lab only).** Sender cost at 128 MiB @ 80 Mbps, CPU-s/GB: **app layer +1.3…+2.9
  (2.4–5.5 %)**; **pion datagram path + in-process shim ≈50.3 (94 %)**. Inside that: plain-UDP sender+relay
  floor **11.4 (unpaced) / 39.9 (paced)**; per-datagram Go timer **+16**; **DTLS AES-128-GCM crypto 0.16
  (0.3 % — 6.0–6.5 GB/s/core)**; bench poll 3–5; **residual (SCTP chunking / DTLS framing / queue plumbing /
  GC) ≈23–40, undecomposed.** Receiver (Chrome) 27.6–31.1 capped / 17.5–20.9 uncapped.
- **So these are NOT fork targets:** AES-GCM crypto (0.3 % — any "offload the crypto" idea is dead), the app
  layer (2.4–5.5 %), and **chunk size** (16/64/256 KiB moves CPU-s/GB <4 %, i.e. noise, **because SCTP
  re-fragments to the same datagrams**). That last one closes the question E6 left open.
- **Artifact 1 — the prod-mode shim is UNCONDITIONAL** (`agent/cmd/benchdirect/prodbench.go:113`; there is no
  flag to bypass it). Every lab datagram is relayed in-process with a per-datagram allocation
  (`shim.go packet{data []byte, …}`) and, when `--bandwidth` is set, a **spin-loop token bucket**
  (`shim.go rateLimiter.take`, which calls `time.Now()` in a retry loop). **Therefore every lab CPU-s/GB figure
  is an UPPER BOUND that includes harness machinery production never runs** — the 11.4→39.9 paced penalty is
  mostly the token-bucket spin. Consequence: do **not** read the lab's 46 CPU-s/GB as production's send cost,
  and do **not** present VERSA's 63–113 CPU-s/GB (a real field measurement) as "1.4–2.4× the lab's pion cost".
- **Artifact 2 (UNVERIFIED, and it contradicts the field) — E23's per-datagram size.** E23 reports 1.377 M
  datagrams/GB at **779.7 B** each (~40 % below MTU), and derives its headline fork recommendation from it
  ("collapse datagram count"). **The field says otherwise:** E1 measured **896 data datagrams/MiB at
  1228-B SCTP packets / 1265-B wire (1200-B payload + 28 SCTP + 37 DTLS)** — i.e. production is already at
  pion's MTU. No prod-mode JSON even carries a packet count (`shim_fwd` exists only in raw mode), so the claim
  could not be reproduced from E22's raw data. **Until E24 resolves it, the "collapse datagram count" lever is
  NOT established for production**; if the lab really emits 780-B datagrams that is a lab artifact to fix, not
  a production win.
- **What survives:** the *ratios* — crypto free, app layer free, chunk size irrelevant, cost is **per datagram**
  (syscalls, copies, queue plumbing). The only fork direction with support is therefore **reducing per-datagram
  overhead itself — syscall batching (GSO / `sendmmsg` / `writev`) and allocation removal — not record size or
  crypto.** Even that cannot be sized from this harness, and production is already MTU-efficient, so its
  headroom is limited to syscall count, not bytes.
- **Environment caveat for E23 itself:** its sanity gate FAILED (10.7/13.7 vs ≈100 Mbps expected), load rose
  3.6→6.4 and swap was 6.2/7.2 GB — its absolutes run ~1.25× E22's. Use E23's **ratios only**; do not compare
  its absolutes against E22's or the field's.
- **Practical upshot:** further lab work on the fork question has low value because the instrument, not the
  product, is now the limiting factor. The answer lives in the field (E21's `--cpus` sweep).

## F13 — The sender's CPU, quantified in the field — and the **host hypothesis FALSIFIED** (E21)
- **Field cells (CLIENT-EAST, 754 MiB) with the agent's cgroup CPU quota varied** (agent-side metric, mode
  asserted): `--cpus=0.5` → **38.31** (n=1); `--cpus=1` → **87.48 / 90.98, mean 89.2** (n=2); `--cpus=2` →
  **124.17** (n=1); **uncapped → 125.69 / 115.23 / 100.64, mean 113.9** (n=3).
- **The host is not the problem.** During an uncapped cell: host busy **24.2 %** / idle **75.6 %**, **steal
  0.000 % (0 jiffies on all six CPUs)**, per-CPU busy 23–26 %, run-queue `r` mostly 0–4, load ≤1.06; the agent
  itself used only **0.83 mean / 1.42 peak** cores. **So "move the agent to a quieter or faster box" buys
  nothing** — which also retires the long-standing "VERSA is contended by its co-tenants" hypothesis, and with
  it the planned co-tenant-load experiment as low value. (Note the correction the agent made to its own first
  sampler: `vmstat 1 1` yields since-boot averages, not interval figures.)
- **Per-core work rate ≈ 85–100 Mbps/core** in the field (1 core → 89.2 Mbps). This **independently corroborates
  F11's field bridge** (VERSA's 63–113 CPU-s/GB predicting 75–129 Mbps/core) — two different methods, same
  answer, which is the strongest support the per-byte-cost story has.
- **But the plateau is NOT purely CPU-quota-set.** A 2-core quota (124.17) already equals the uncapped ceiling
  (−8 %), whereas the per-core rate would predict ~170 Mbps at 2 cores. So the sender has **more CPU available
  than it uses** at ~120 Mbps, and something else bounds it there. The experiment cannot by itself separate
  "CPU-set" from "path-set" — the follow-up (E25) tests the **client** side, which E15's 23.8 % sink result
  makes the leading candidate.
- **Consequence for the fork question:** the only CPU-side lever remaining is per-byte send cost, and its
  headroom is real but **bounded**; a fork alone cannot explain the plateau, because the sender is sitting on
  unused CPU headroom while the rate stalls. A fork plus a client-sink fix is a more coherent story than either
  alone.
- **Trap validated again:** **2 of 8 cells (25 %) silently relayed** and had to be discarded (relayed cells read
  ~44.7 Mbps and look like plausible slow measurements). Keep asserting the mode in every cell.
- Rig restored and independently verified after the sweep (`sb-agent:pristine`, `NanoCpus=0`, `UI_PORT=7879`,
  `SB_SCTP_CA_STEP=32768`, no `SB_SCTP_MIN_CWND`, signalling 200, clients clean).

## F14 — E24 resolves the datagram contradiction: there was **no** lab/field difference — E23's number was an averaging error
- **Measured (lab, `--mode raw`, 16 KiB chunk, rate-capped): strongly bimodal.** DATA **53,260 × 1237 B wire**
  (1200-B SCTP = 28 header + **1172 payload**, + 37 DTLS) and **4,097 × 1213 B** (the last fragment; 13.00:1, as
  predicted); SACK **28,551 × 65 B**. **Nothing between 70 and 1150 B.** pion's own `Association.MTU()` reads
  **1200**. Payload per *data* datagram **1169.6 B**.
- **The field agrees with the lab to within 0.4 %:** lab **896.6 data + 450.3 SACK = 1346.8 datagrams/MiB** vs the
  field's measured **896 data + 448.7 SACK** (E1). So the harness **is** representative on packet structure —
  a lab/field corroboration worth having, and the opposite of the discrepancy I suspected.
- **E23's "779.7 B per datagram" was payload ÷ ALL datagrams (67,108,864 / 86,198 = 778.5 B) — a mixed average
  including SACKs, not a size.** With that corrected, **E23's headline fork recommendation ("collapse datagram
  count") does not survive**: the only count headroom is the fragment tail (~0.1 %). F12's artifact-2 flag was
  right to withhold it.
- **Correction to E1's packet model (small, ~2 %):** E1's "1228-B SCTP packet" came from reading the *overridden*
  `initialMTU`. pion/webrtc v4 applies **`WithMTU(outboundMTU=1200)` in both roles
  (`sctptransport.go:120,164`)**, so the real packet is **1200 B SCTP (1172 payload + 28 header) on a 1237-B wire
  datagram**. E1's *datagram counts* were correct; its *size attribution* was high by ~2 %. Also note this is a
  **hard-coded 1200**, i.e. production leaves path-MTU headroom unused by design.
- **Corrected production-representative cost: ≈32–43 CPU-s/GB (central ≈37)**, down from the 46 headline, after
  subtracting the shim's per-datagram relay (10–10.5) and the chromedp poll (3–5). VERSA's field figure
  (63–113 CPU-s/GB) is then **1.7–3×** the corrected lab number — consistent with a server core being ~2× an
  Apple-silicon core, which is the same story F11 told.
- **Fork headroom, honestly bounded — this is everything left after E22 + E23 + E24:** (i) **MTU 1200 → path MTU
  (~7–10 %** of rate, and PMTU-discovery-risky — the hard-coded 1200 is the reason); (ii) **GSO / syscall
  batching (≤10 CPU-s/GB** of ~37, unquantified); (iii) **per-datagram allocations** (needs `pprof` to size).
  **Combined, that is ≈1.2–1.4× at best — not the ~2× v1 direct would need to reach relay class, and it cannot
  be the whole story anyway, because E21 shows the sender sitting on unused CPU headroom at the plateau.**
- **Caveat:** E24's sanity gate also failed (8.4/9.6 Mbps vs ≈100 expected) — absolutes are banded; the
  *structural* findings (bimodality, `MTU()=1200`, and the field-matching counts) are load-independent.

## F15 — The whole pion fork is worth **−16 % to −24 % of CPU per byte**. The fork question is CLOSED: no.
**E26 sized the only levers left after F11–F14, and the total is small.** (Harness change: 6 lines + one inert
file; `go build ./...` and `go vet` pass.)
- **(A) Syscall batching — 5.6 %, a ceiling.** Baseline `WriteToUDP` = **2.40 CPU-s/GB** (2.37–2.46); batched
  (batch ≥ 8) = **0.33** ⇒ **−2.1 CPU-s/GB**. This is a *ceiling* rather than a measurement: macOS exposes
  neither `sendmmsg` nor `UDP_SEGMENT` (both confirmed absent), so a same-bytes/fewer-syscalls proxy was used;
  Linux realisations are included in the harness and cross-compile.
  - Bonus, free, no fork needed: **connecting the UDP socket instead of `WriteToUDP` buys 0.72 CPU-s/GB (~2 %)**.
- **(B) Allocations — 10.3 %, ceiling −3.8 CPU-s/GB.** **GC is only 0.27 % of CPU**, which kills the recurring
  "Go's garbage collector is the bottleneck" hypothesis outright. Allocation work (`mallocgc` + `memclr`) is
  10.3 %: pion allocates **~8 fresh 1.2 KB buffers per datagram** (~12× amplification over the bytes delivered).
- **(C) MTU 1200 → path MTU — unmeasured estimate ≈ −3.0 CPU-s/GB.** `outboundMTU = 1200` is a **package
  const** (`constants.go:41`), so raising it requires patching pion. Estimated from the −13.9 % datagram count.
  **Label this an estimate, not a measurement.**
- **Bounded total: ≈28 CPU-s/GB, i.e. −24 %** (optimistic: two ceilings + one estimate); **measured-only levers
  bound at −16 %**. Converted to throughput, a fork's best case is **≈1.19–1.32×**.
- **Why that settles it:** v1 direct would need **~2×** (122 → 233 Mbps) merely to *match* the v2 relay, and E21
  showed the field plateau is **not purely CPU-quota-set** (a 2-core quota already equals the uncapped ceiling),
  so even the optimistic 1.3× is not reachable in practice by cutting per-byte cost alone. **A pion fork cannot
  deliver relay-class speed; the ceiling is not where a fork would act.**
- **Caveat:** E26's sanity gate also failed (9.63 Mbps vs ≈100 expected; swap 6.06/7.0 GB), so its absolute
  level is a band — but every lever here is a **ratio measured within one session**, which is exactly what is
  load-robust, and the three levers were each bounded independent of the others.

## F16 — **The client is the binding constraint** at the v1-direct plateau (E25). This is the capstone of F11–F15.
- **Client CPU sweep (field, 754 MiB, direct asserted in all 5 cells, 0 discarded):** browser pinned to
  **1 vCPU → 37.19 Mbps (n=2)**; **2 vCPU → 48.31 (n=1)**; **unpinned 4 vCPU → 107.84 (n=2)**. Client steal
  **0.0 % in every interval**, pinned arms saturated their mask exactly, dominant chrome process 0.71 / 1.45 /
  2.3 cores.
- **The deciding number:** the client yields **~35 Mbps per core** (37 → 48 → 108 as cores go 1 → 2 → 4) while
  the sender yields **~85–100 Mbps per core** and plateaus by 2 cores (E21: 89 Mbps on one core). **A 4-core
  client ≈ a 2-core sender.** So at the ~110–120 Mbps plateau both ends are near their limits, and the *client*
  is the tighter one.
- **This closes the fork question from the other side as well.** A sender-side fork bounded at 1.19–1.32× (F15)
  cannot move a plateau that the client sets — consistent with E21's finding that the sender sits on unused CPU
  headroom at the plateau. **Sender-side work is the wrong end of the wire.**
- **Open tension, being resolved by E27.** This same client VM downloaded at **239.05 Mbps through the v2 relay**
  (E10c), so the client's cost is **not** a flat ~35 Mbps/core for every path. Either the **app download sink**
  (SHA-1 + StreamSaver) or the **WebRTC DataChannel receive path** is responsible — or both. E15 showed sink
  removal alone gives **+23.8 %**; E27 separates the two by running the sink-free client at 1 and 4 vCPUs. The
  answer decides whether the fix is app code (helps v1 and v2 alike) or architectural (v2's HTTP path is the
  answer for the client side too).
- **Test-rig hazard found (TEST RIG ONLY — production untouched).** From **22:41Z, TESTBOX's TLS front
  intermittently answered every external handshake with TLS alert 80 (internal_error)**; Chromium failed ~55
  loads while `curl` was 200 12/12. E25's cells therefore ran through a **loopback front to TESTBOX:8080** — the
  measurements stand, but the caveat is attached to them. Diagnosed and documented, **not repaired** (out of
  scope for an experiment); it needs an operator decision and it will affect any later run that uses the browser
  client through the external front.
- Rig restored and verified afterwards (`sb-agent:pristine`, `NanoCpus=0`, `UI_PORT=7879`, `CA_STEP=32768`,
  no `MIN_CWND`, API 200, signalling 200, clients clean).

## F17 — The client's cost is **66 % WebRTC receive path / 34 % app sink** (E27) — and that first bucket is UNSEPARATED
- **Sink-free (E15's technique), all direct, pin-verified:** **1 vCPU → 56.35 Mbps** (56.21/56.50, n=2); **4 vCPU →
  119.45** (122.65/116.24, n=2). **Sink-ON control, same session, 4 vCPU → 108.76**, which reproduces E25's 107.84
  to **0.9 %** ⇒ no session drift, and that is what justifies comparing the 1-vCPU arm against E25's 37.19 (all
  three fresh pinned sink-ON controls fell to relay).
- **Ratios: 1.52× at 1 vCPU, 1.098× at 4 vCPU.** With cores to spare the sink costs ~10 % of throughput; it only
  becomes decisive when the client is CPU-starved.
- **The cost is in the chrome RENDERER**: sink-free **0.66–0.86 cores** vs sink-ON **1.95–2.01 cores**; the network
  service is flat (0.38–0.51), GPU 0.06, node ≤0.06. So the sink (hash-wasm + StreamSaver) is in-renderer JS — not
  network, GPU or driver.
- **But 66 % of the client's per-byte cost is not the sink.** Deciding number: a sink-free 1-core client does
  **56.35 Mbps**, still ~**1.5× worse per core** than the sender's 85–100 Mbps/core. That remainder is bucketed as
  "the WebRTC receive path" — which is **browser-internal DataChannel receive PLUS the app's own onmessage /
  chunk-assembly JS, and E27 does not separate them.** Whether v1 can be lifted by app-code changes (no fork, no
  v2) hinges entirely on that split, so it is the next experiment (E28, a bare receive path).
- **Do not conflate two quantities from E27's wording:** the sink's share of *renderer CPU* (34 % at 1 vCPU, 48 %
  at 4 vCPU) and its share of *throughput* (52 % at 1 vCPU, 9.8 % at 4 vCPU). Different denominators.
- **Methodological catch worth keeping:** pinned (1-vCPU) attempts **fell to relay in 4 of 8 tries, at +10.0 s**
  (0 of 4 unpinned). CPU starvation delays the handshake past the direct-mode timeout, so **pinning the client
  CAUSES silent relay fallback** — any future pinned-client experiment must budget retries and assert the mode.
- **Web root restored and verified byte-for-byte** (`cmp` identical, 91,308 B, 0 markers, HTTP matches).
- **E25's TESTBOX alert-80 did NOT reproduce** (Chromium 3/3 loads, curl 6/6 across 3 hosts; the front was never
  restarted, NRestarts=0) — that hazard was transient. No operator action needed, but it stays on record.
- **Benchmark to beat, unchanged:** this same client VM did **239 Mbps through the v2 relay** (E10c), so the client
  is not inherently limited to ~120 — the limit is in the v1 client's path, not the hardware.

## F18 — **The headline v1-vs-v2 comparison is CONFOUNDED: the "v2 browser sink" was a NATIVE browser download, not an app client**
- **The E10/E10c "v2 browser sink" arm was not a client at all.** `phase-4a/agent/internal/direct/static/app.js:1-7`
  documents downloads as *"native browser navigations to /asset/{id} … (the server sets
  Content-Disposition: attachment)"*, triggered by `gallery.js:157 anchor.href = url`; `handlers.go:347
  handleAsset` streams straight to the HTTP writer. **Zero page-JS per byte — no DataChannel, no SHA-1, no
  StreamSaver.** So the 239 Mbps measured **Chrome's native HTTP download manager** and says nothing about any
  DataChannel receive path.
- **Therefore the campaign's headline comparison mixes transport with client implementation.** v1 direct's
  numbers (102–151 Mbps) came through the app's **JS** receive path (DataChannel + SHA-1 + StreamSaver), while v2
  relay's (233–239) came through the browser's **native** downloader. The agent-side metric counts bytes the
  agent sent, which the client's consumption gates via flow control — so **"v2 is 2.1–3.8× faster" must not be
  presented as a pure transport result.** Any write-up (including the professor discussion) needs that caveat.
- **v1's transport ceiling is NOT established at ~120 Mbps.** The same pion stack does **521–533 Mbps uncapped**
  on loopback (E22), and the field path carries ~246 Mbps. With a client that keeps up, v1 direct could sit near
  the path limit. **So E28 (a bare receive path) is not merely a client experiment — it is the fair-transport
  comparison**, and its result decides whether the earlier "v2 is ~2× faster" claim survives in any form.
- **v1 client per-byte costs, ranked from the code** (`e2e-v1/signaling-server/web/src/`):
  1. **`downloadSinks.js:20-31` re-concatenates the 1 MiB verification tail on EVERY append** — ~1.06 MiB
     allocated and copied per 64 KiB flushed (**~17× amplification**) **on the main thread**. This is a
     bug-class inefficiency and pure app code.
  2. `vendor/streamsaver.js:287 postMessage(chunk)` — structured clone plus a service-worker hop per chunk.
  3. `hash-wasm` SHA-1 **on the main thread**, no Worker.
  4. `binaryEnvelope.js:58 data.slice(14)` — a 64 KiB copy per chunk.
  5. ~4 promise hops, 2 timer pairs and 3 DOM writes per chunk.
- **The recommended first change was "move bulk bytes off the DataChannel onto plain HTTP"** — but that follows
  only if E28 shows a lean DataChannel receive path is *itself* slow. If a bare path is fast, then items 1–5 are
  the actual defect and HTTP is not required. **E28 decides.**
- **Evidential status:** this section is **code reading, not measurement**. The ranked list is a hypothesis about
  where the client's cost sits; it is *consistent* with E27's finding that the cost is in the renderer
  (0.66–0.86 cores sink-free vs 1.95–2.01 sink-ON) but does not confirm the magnitudes.

## F19 — **E28 settles it: the browser's DataChannel receive is the v1 ceiling.** App JS costs only ~14 % at 4 vCPUs (but 2.23× at 1 vCPU)

> **RESOLVED by E30 (n=4) — F19's magnitudes STAND; E29's 1.27× was NOT reproducible.** E29 (n=2, interleaved)
> suggested the app's JS cost ~27 % and the browser's floor was ≥146.5 Mbps. **E30 re-measured the fixed client
> at n=4: 119.17 Mbps mean (107.5 / 110.8 / 127.5 / 130.9) against an unfixed control of 108.06 (n=2) — 1.10×,
> with 146.54 not reproduced.** So the app's JS is worth ~10 % of the *rate* at 4 vCPUs, and the browser's
> receive floor is ~120 Mbps. **What E29 established robustly is the tail and CPU win, not the rate** — see F20
> and F21. F19's remaining conclusions (the ceiling is client-side, the sender has headroom, a fork cannot help,
> the v1-vs-v2 gap is client-architecture) are all confirmed and strengthened by E30.
- **Bare receive path** (real signalling/ICE/lanes unchanged; bulk `onmessage` counts `byteLength` and drops the
  payload — no decode, no assembly, no sink). Byte accounting exact on **all 4 cells** (790,626,304 B;
  48,258–48,263 frames), so the arms are valid, not merely fast:
  - **unpinned 4 vCPU: 123.68 Mbps** (122.29 / 125.07, n=2) — vs sink-free 119.45 (**1.035×**) and the real client
    108.76 (**1.137×**);
  - **pinned 1 vCPU: 82.99 Mbps** (79.76 / 86.22, n=2) — vs sink-free 56.35 (**1.47×**) and the real client 37.19
    (**2.23×**).
  - Renderer cores: **0.63** @ 123.68 unpinned, **0.35** @ 82.99 pinned — and the pinned arm was **not**
    pin-saturated (0.78–0.85 busy, unlike E25/E27). The bare path therefore needs only ~0.35–0.63 renderer cores,
    so its ~83–124 Mbps is a **browser-internal limit, not CPU exhaustion**.
- **Verdict: the app's JS is not what caps the plateau.** Removing *everything* app-side buys only **13.7 %** at
  4 vCPUs (108.76 → 123.68), and the bare path lands on the same ~120–124 Mbps plateau the sender showed
  (E21: uncapped mean 113.9, and 124.17 at a 2-core quota). **This supersedes F18's ranked-defect list as an
  explanation of the plateau** — those defects still matter enormously when the client is CPU-starved (2.23× at
  1 vCPU) and they still carry the ~80 s verify/write tail, but they are not the plateau's cause.
- **The residual is browser-internal.** A bare JS handler cannot exceed ~124 Mbps on this 4-vCPU client, while the
  sender demonstrably has headroom: its CPU was never saturated in any field cell (E21) and the same pion stack
  does **521–533 Mbps uncapped against a Go receiver** on loopback (E22). So the field's ~120 Mbps ceiling is the
  **browser's WebRTC DataChannel receive path** — not the transport, not the agent, not the app.
- **This resolves F18's confound WITHOUT rescuing v1.** The 2.1–3.8× v1-vs-v2 gap is **not** a TCP-vs-SCTP
  difference and **not** a kernel-transport difference: **v1 forces a JS receive path (DataChannel) while v2's
  relay lets the browser use its native HTTP downloader with zero page-JS.** The comparison therefore remains
  valid **as a user-visible comparison**, but it must be described as a **client-architecture** gap, not a
  transport one. Both halves of that matter: v2's win is real for users, and the mechanism is not what the
  earlier write-ups implied.
- **Answer to "can v1 direct be optimized to relay class?" — not with agent-side or app-code changes.** Remaining
  app-side headroom is ~14 % at 4 vCPUs plus the ~80 s tail, while the browser-limited remainder would need bulk
  bytes on an HTTP/native-download path — exactly what v2's relay already does. A **pion fork (≤1.3×, F15)** would
  also act on the end of the wire that has headroom, so it cannot help either.
- **Caveat / open question (needs a resource decision, not an unattended experiment):** the test client VM has
  only **4 vCPUs**, and E25 showed the client scaling 37 → 48 → 108 across 1 → 2 → 4 cores (sub-linear but real).
  **So ~124 Mbps may understate v1 on 8–16-core client machines — untested.** E20 also showed two tabs on one host
  do **not** simply add up (72.30 Mbps combined), so a single client does not trivially multiply either.
- **Methodological catch (now confirmed twice):** pinned arms fell to relay in **3 of 5** attempts (0 of 2
  unpinned), all with the **+10.0 s** direct-deadline shape — **pinning the client causes silent relay fallback.**
- Web root restored and verified (`407ee7032f90e429…`, 91,308 B, 644, `cmp`-identical, 0 markers, HTTP-served hash
  matches); rig restored (`sb-agent:pristine`, `NanoCpus=0`, API 200, clients clean).

## F20 — **E29: the verification-preserving client fix removes the ~87 s tail — a 3.23× user-visible win**

> **SUPERSEDED IN PART by E30 (n=4): the RATE gain is 1.10×, not 1.27×, and 146.54 Mbps was not reproduced.**
> Everything below about the **tail, the user-visible time and the CPU halving is confirmed**; the
> "146.54 Mbps" figure and the 1.27× multiplier are single-session n=2 excursions. See F21.
- **The fix (minimal, in the isolated test-web-root copy; full diff in the results file):** (a) keep a **1 MiB tail
  buffer** instead of re-concatenating the verification tail on every append (`downloadSinks.js:20-31`, the ~17×
  amplification) and (b) move **SHA-1 into a module worker**. Nothing else changed.
- **Unpinned 4 vCPU, n=2 each, interleaved:** control **115.36 Mbps**, 54.83 s window, tail **85.17 s**,
  user-visible **139.87 s**, renderer **2.07 cores** → fixed **146.54 Mbps**, 43.17 s, tail **0.31 s**,
  user-visible **43.35 s**, renderer **1.07 cores**. That is **1.27× agent-side, 3.23× user-visible, and 48 %
  less renderer CPU**.
- **Pinned to 1 vCPU (n=1 each):** the **rate is identical** (37.53 Mbps both arms) but the **tail collapses
  ≤131.8 s → 1.34 s**, so user-visible time goes **≥299.2 s → 169.55 s (1.76×)**. The fixed arm used only
  **0.77 of 1.0 core** — so the pinned ceiling is **not** app CPU; it is the **StreamSaver service-worker hop
  plus the receive path**. Removing hashing from the main thread does not help when the client is pinned; the
  remaining fix there would be replacing StreamSaver's write path (e.g. the File System Access API).
- **Verification is preserved and was independently checked:** every fixed cell reported `✓ intact`, the worker
  digest matched the displayed expected digest, there was no FIX-NOT-ENGAGED/HASH-DISAGREE case, and **200/200
  randomized equivalence trials were byte-identical**. The fix is therefore a legitimate repair, not a
  bypass — which matters, because a faster client that no longer verifies would be worthless.
- **This corrects F19's magnitudes** (see the correction banner there): the app's own JS was ~27 % of the
  achievable rate at 4 vCPUs, not ~14 %, and the browser's DataChannel receive floor is **≥146.5 Mbps**, not
  ~124. E28's "bare" handler was itself a main-thread per-message cost, so it under-measured the browser.
- **What this does NOT change:** the sender still has headroom, a pion fork is still ≤1.19–1.32× (F15) and still
  aimed at the wrong end of the wire, and the v1-vs-v2 gap is still **client-architecture** (F18/F19) — the
  fix lands v1 at ~146 Mbps, which is a large gain from ~115 but still short of the relay's 233.
- Web root restored (manifest byte-identical, 140 files); rig unchanged; **6/6 unpinned cells direct**, 2 pinned
  cells discarded as relay.

## F21 — **E30: "who binds" answered — the client's receive side, and it is SERIALIZED (~120 Mbps). E29's rate gain was noise.**
- **Arms** (0 of 9 cells discarded to relay; both pinned cells stayed direct first try):

  | arm | n | Mbps | tail | renderer cores |
  |---|---|---|---|---|
  | fixed client, 4 vCPU, sender uncapped | 4 | **119.17** (107.5 / 110.8 / 127.5 / 130.9) | 0.53 s | 0.98 |
  | fixed client + sender `--cpus=2` | 1 | 115.18 | 0.58 s | 0.98 |
  | fixed client pinned to 2 vCPU | 1 | 68.34 | 0.80 s | 0.57 |
  | fixed client pinned to 1 vCPU | 1 | 39.02 | 1.82 s | 0.35 |
  | unfixed control, 4 vCPU | 2 | 108.06 | **87.4 s** | 2.06 |

- **The sender is exonerated, and more strongly than before:** capping it at 2 cores changed nothing (115.18 vs
  119.17) and it used only **0.65 core**, while iperf3 on the same path carried **246 Mbps UDP / 241 TCP** — 2.1×
  spare. So the ceiling is not sender CPU and not the path.
- **The client's receive side binds, and it is serialized.** The pin gradient is **39.0 / 68.3 / 119.2 Mbps at
  1 / 2 / 4 vCPU** while the client **never saturates its allowance** (0.98 of 4 cores at the ceiling). Needing
  ~1 core's worth, being hurt by fewer cores and *not helped by more* is the signature of a **serial**
  bottleneck — which also means **the remaining app-side work (the ~10 % rate gain) cannot lift it.**
- **Four independent configurations now land on the same ~108–124 Mbps**: the fixed client (119.17), the unfixed
  control (108.06), a bare counting handler (123.68, E28), and the old client's "sender plateau" (E21: 124.17
  at a 2-core quota). The plateau is therefore the **browser's WebRTC DataChannel receive path** — not the
  sender, the transport, the host, the path, the loss response, or the app's JS.
- **This answers the "bigger client" open question by inference:** the client uses only ~0.98 of 4 cores at the
  ceiling and is not helped by more CPU, so **a larger client VM would not be expected to raise it** — inference
  from the serial signature, not a measurement, and the one remaining assumption worth stating as such.
- **The app fix still earns its place, on the tail rather than the rate:** the unfixed control's tail was
  **87.4 s** (renderer 2.06 cores) versus **0.53 s** fixed (0.98 cores) — a **user-visible 3.23×**
  (139.87 s → 43.35 s) and a halving of client CPU, which is what matters on constrained devices.
- **Restore verified:** web-root manifest identical to E29's 140-file original; `sb-run` pristine, `NanoCpus=0`,
  API 200; clients clean (`node`/`chrome`/`Xvfb`/`iperf3` = 0).

## F22 — The client's real per-frame cost is **~65× amplification + 48,260 service-worker hops** — and the v1 **relay** path pays all of it *plus* per-frame JS Noise. Every figure in this campaign was measured through that client.
- **The code** (`e2e-v1/signaling-server/web/src/downloadSinks.js:19-32`): `append(bytes)` calls
  `concatBytes(tail, bytes)` on **every** append with `tail` up to 1 MiB, `await writer.write(...)`, then sets
  `tail = combined.subarray(...)` — a view that keeps the whole previous buffer alive. Hashing is
  `hasher.update(bytes)`: **streaming** (once per byte, *not* re-hashed per chunk — an earlier suspicion of
  re-hashing was wrong), but via **`hash-wasm` SHA-1 on the main thread**.
- **Granularity, which makes the earlier "~17×" quotation optimistic.** The client receives **~16 KiB frames**:
  E28's byte accounting counted **48,258–48,263 frames** for 790,626,304 B. At 16 KiB appends against a 1 MiB
  tail, each append allocates and copies ~1.016 MiB → **≈49 GB allocated+copied for a 754 MiB file ≈ 65×
  amplification**, plus **48,260 StreamSaver `postMessage`/service-worker round trips**, plus 48,260 JS SHA-1
  updates. All on the main thread.
- **That single mechanism explains three separate measurements:** the **~87 s tail** (a backlog draining at the
  client's ~10 MB/s effective processing rate), the **~10 % back-pressure** on the *agent-side* rate at 4 vCPUs
  (E27/E30), and the **up to 2.2× penalty** when the client is CPU-starved (E27/E28).
- **The v1 RELAY path pays all of the above plus per-frame JavaScript Noise decryption.**
  `secureRelayChannel.js` imports `NoiseXX` from the **JS** `noise-p256` implementation and decrypts each frame
  in `_processMessage`; `MAX_RELAY_PAYLOAD_BYTES` is capped per frame. That is a **fixed per-frame cost on the
  same main thread**, and it fits what E12 actually measured: **42.0 Mbps EAST (12.5 ms) and 55.3 Mbps WEST
  (74 ms)** — **latency-independent**, nothing like a bandwidth-delay-product shape, with the agent only
  **11–19 % busy** and the client idle 40–48 %.
- **Consequence 1 — every throughput number in this campaign was measured through this client**, including the
  "~120 Mbps browser ceiling" and the v1 relay's 42/55. Both are ceilings *through a main thread doing ~65× the
  necessary memory traffic, 48k service-worker hops, and (for relay) JS crypto per frame*.
- **Consequence 2 — the E29 fix was incomplete, and that is now the most promising remaining lever.** It removed
  the tail re-copy and moved hashing off the main thread (tail 87.4 s → 0.53 s) but **kept StreamSaver**, and
  E30's rate moved only 1.10× (108.06 → 119.17). So **StreamSaver's per-frame service-worker hop is the residual
  main-thread cost** — and removing it (File System Access API, or batching frames before writing) is the next
  untested client fix, and the first one expected to move the **rate** rather than just the tail.

  > **CORRECTION (E31/F23): this paragraph's scope was too narrow.** The "only 1.10×" figure is **true on the direct
  > path** (where the browser's DataChannel receive, not the sink, is the cap) and **badly wrong on the relay path,
  > where the same fix is worth 4.36× (51.58 → 224.82 Mbps)**. E31 also refuted this finding's *prediction* that the
  > relay would stay slow because of its own per-frame JS Noise cost. The diagnosis of the sink's cost stands; the
  > conclusion that it does not move the rate does not, and StreamSaver remains a candidate residual worth measuring
  > on the relay path specifically.
- **Consequence 3 — the v1-relay figure needs re-testing with a fixed client.** E12 predates every fix, so its
  42/55 may be a *main-thread* ceiling rather than a property of the relay design. This is directly load-bearing
  for the v1-direct-vs-v1-relay choice: it contradicts the belief that v1 relay runs near line speed, but it was
  measured with the throttled client.
- **Comparison-pairing correction:** the campaign's headline compared **v1 direct vs v2 relay** — not a
  like-for-like pairing. The clean matrix is 2×2: v1 direct **measured** (102–131 EAST / 60–68 WEST), v1 relay
  **measured but client-throttled** (42.0 / 55.3), v2 relay **measured** (233 / 215–228), and **v2 direct never
  measured** (explicitly skipped by request).

## F23 — **E31 OVERTURNS E12: v1 relay is ~225–230 Mbps (line speed), not 42/55. The unfixed client was the entire deficit — and relay BEATS direct with the same fixed client.**
- **Arms** (one matched session, all mode-asserted, exact byte accounting; E12's agent-side metric reused verbatim):

  | arm | Mbps | n |
  |---|---|---|
  | **A** relay + unfixed client | **51.58** (50.684, 49.517 EAST; 54.550 WEST) | 3 |
  | **B** relay + E29's fix | **224.82** (228.631 EAST, 221.003 WEST) | 2 |
  | **C** relay + discard sink (**relay channel + JS Noise decryption intact**) | **229.93** (227.958 EAST, 231.896 WEST) | 2 |
  | **D** direct + identical fixed client | **149.090** | 1 |

- **Verdict: the relay's 42/55 was the client sink, not the relay path.** With the sink neutralised but the relay
  channel and its per-frame **JS Noise decryption fully intact**, the relay delivers **229.93 Mbps ≈ 96–99 % of the
  same-session 235–239 Mbps TCP capacity** — i.e. it runs at the client VM's ingress ceiling. **C/A = 4.46×.**
- **E29's fix delivers nearly all of it to real users: 224.82 Mbps, a 4.36× uplift**, with the tail collapsing
  **3.86 s → 0.60 s** and renderer cost falling (1.67 → 1.40 cores) while moving 4.4× more bytes per second.
- **v1 relay with the fix is 1.53× FASTER than v1 direct with the identical fixed client** (224.82 vs 149.09). The
  browser's **DataChannel** receive is therefore the *slower* path, and a WebSocket + JS-Noise channel beats it —
  the opposite of the intuition that native DTLS/SCTP must outrun JS crypto over WebSocket.
- **F22 correction:** F22's *diagnosis* (~65× memory amplification + ~48 k main-thread service-worker hops) is
  **confirmed causal**; F22's *prediction* that the relay would stay slow because of its own per-frame Noise cost is
  **REFUTED** — Noise decryption is not the binding cost at these rates.
- **The client fix is worth far more than the direct path suggested.** Direct: **~1.10×** (108.06 → 119.17 — capped by
  the browser's DataChannel, *not* the sink). Relay: **4.36×** (51.58 → 224.82). Any earlier statement that the fix
  "buys only ~10 % of rate" is **true for direct and false for relay**.
- **Retractions:** E12/F5's *"v1 relay is the weakest of the four paths — do not treat it as a fallback"* is
  **WITHDRAWN**, and so is the accompanying *"relay-as-fallback is a quality cliff"* claim. The relay is a
  **line-speed** path once the client is fixed.
- **The v1-vs-v2 speed argument largely disappears:** v1 relay (224.82 with the fix) ≈ v2 relay (233) ≈ the
  235–239 Mbps TCP capacity. What remains between v1 and v2 is implementation and ops, **not throughput**.
- **Updated matrix:** v1 direct **149.09** | v1 relay **224.82** (unfixed: 51.58) | v2 relay **233** | v2 direct never
  measured. Note the ordering is now counterintuitive: **relay > direct on v1**, which reframes the whole
  v1-vs-v2 comparison — the earlier "2.1–3.8×" was *v1 direct vs v2 relay*, i.e. the wrong pair twice over.
- **Caveats:** CLIENT-EAST↔TESTBOX clock skew ≈1 s (sub-second tails are ±1 s; tens-of-seconds claims unaffected);
  the relay share was reused from E12 (agent counter 4→n); Arm D is n=1; and the **v2 units on TESTBOX had to be
  stopped at 07:19Z** to free :8080/:443 for the v1 rig (documented in the results file's "Rig state change" — a
  peer session may need them restarted).

## Do NOT land — negative results (documented so they are not re-litigated)
- **`SB_SCTP_MIN_CWND` at any size** — CLOSED by E17/E18: above ~1.6× BDP it degrades 3–4×, above ~12× BDP it
  breaks outright (0/4 cells completed), and below BDP it is harmless but never beats stock because the
  field's clean fast path has no loss-induced collapse to remove. See F8.
- **RTO-floor tuning** (`--rtomax` / `SB_SCTP_RTO_MAX_MS`): **not a fix — but do not misread this as "never helped".**
  - *Clean paths:* no effect at any RTT (rtt 12/25/71/100, 91 runs, identical ceilings, wire volume flat) — E2.
    Consistent with the mechanism: no drops ⇒ no RTO firings ⇒ the floor never engages.
  - *Under loss, a real but inconsistent effect already on record:* the 2026-09-15 sweep
    (`2026-09-15-multiconnection-webrtc-benchmark.md`) at a high-RTT / 64 Mbps regime found `rtoMax=200ms`
    lifting **N=4 at 0.1% loss from 18.5 → 56.6 Mbps (≈3×)** — while **hurting N=1 at the same loss from
    26.3 → 7.3 Mbps (≈3.6× worse)** — at n=3 with very wide spreads (200ms/N=4 ranged 17.3–95.2) and an 8 MiB
    size confound. That report's own verdict was *"the 1-second RTO floor is real, but `rtoMax` is not a clean
    substitute for lowering `rtoMin`, and this does not demonstrate a reliable win"*, and it asked for a 32 MiB
    re-run.
  - *Tonight's cleaner test* (E11, interleaved, n=5, baseline-bracketed): **+13.8% / +14.8%** at 0.1% loss and
    **inside spread** at 0.9% — with the entire gain being the removal of 1 s stall time (4.5 s → 1.2 s). The
    sender idles less; it does not send more.
  - **Net: the effect tracks LOSS, not RTT.** High RTT alone does nothing; high RTT with loss is sometimes
    large but sign-unstable, so it is not a dependable lever. `rtoMax=100ms` (below path RTT) is actively
    catastrophic — constant spurious retransmits even at zero loss.
- **Multi-session striping:** actively counterproductive — v1 cross-host N=2 (89.9 Mbps) lands *below* v1 N=1
  (102–122), with both client hosts slowing simultaneously.
- **The field "wire ÷ 3 = goodput" overhead story:** a measurement artefact. Lab wire/payload is 1.08× (L4) /
  1.12× (incl. IP+UDP) with no spurious retransmission, established three independent ways.
- **The BBR fork** (see F4).
- **"Client-side sink is the binding constraint"** (an earlier conclusion in these docs): **falsified** — two
  independent client hosts did not add throughput.
- **Retired:** "long mid-transfer plateaus" (128 runs, longest stall 1.2 s) and Exp 9's "agent CPU 1.53%" (real
  value 0.9–1.6 cores) and "N=4 one session dies early" (did not reproduce, 4/4 twice).

## Provenance
Per-experiment reports: `results/2026-09-18-exp*.md` in this directory (E1, E2, E8b, E8c, E10, E10b, E10c,
E11, E12) with raw JSON under `results/raw/`. Summary and method: `2026-09-17-transport-tier1-tier2-experiments.md`
§ "OVERNIGHT RESULTS SUMMARY (2026-09-18)" and `RUNBOOK-overnight-2026-09-18.md`. Earlier CA-step/fork field
work: `.worktrees/e2e-v1/docs/superpowers/spikes/2026-09-15-*` and `2026-09-16-*`.
