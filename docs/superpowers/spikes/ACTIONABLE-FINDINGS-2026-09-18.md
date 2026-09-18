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

## Do NOT land — negative results (documented so they are not re-litigated)
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
