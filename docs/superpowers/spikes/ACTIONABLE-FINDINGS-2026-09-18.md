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
