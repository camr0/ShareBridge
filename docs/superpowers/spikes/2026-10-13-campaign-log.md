# v1-direct campaign log — 2026-10-13 (the "finish the remaining investigations" pass)

Companion to `2026-10-02-campaign-log.md` (the previous pass, E33–E39). Written incrementally by the
orchestrator; each experiment also has its own results file.

**Operator brief:** unshelve the OVH rig, run *all* the remaining candidate investigations from
`2026-10-02-v1-direct-candidate-experiments-review.md`, use subagents on `opencode-go/deepseek-v4.1-flash`,
log everything.

## Rig as brought up (02:24Z)

- TESTBOX swapped from the v2 stack back to **v1** (`caddy` + `sharebridge-test`; signalling 200). v2 units
  stopped. To be restored at teardown.
- `sb-run` on VERSA from `sb-agent:pristine`, `UI_PORT=7879`, host networking, `SB_SCTP_CA_STEP=32768`.
- Fresh shares created: direct `relay_only=false` and relay `relay_only=true`, both 48 h / 500 downloads,
  one 754 MiB source = **790,626,304 B = 6,325.010432 Mb**.
- Clients: CLIENT-EAST (~12.6 ms measured today, 4 vCPU) and CLIENT-WEST (~70.75 ms, 4 vCPU). Served client
  confirmed **unfixed** (`hashWorker.js` absent) — the same client as every October number.
- **A new 8-vCPU instance was created for the C1 client-scaling test** (see exp41). That is a 4th instance;
  it must be shelved with the others at teardown.

## exp40 — what caps the sender at ~176 Mbps? **THE ~176 IS NOT A CAP.** ✅

`results/2026-10-13-exp40-sender-side-176-cap.md`. 10 field cells, 5 traces, 2 path controls, zero failures
and zero relay fallbacks.

**Verdict.** The ~176.5 Mbps is **the duty cycle of a cwnd sawtooth around a real ~234.4 Mbps ceiling** — and
the specific 176 is produced by **the rig's own `SB_SCTP_CA_STEP=32768`**.

- **Instantaneous ceiling 234.357 Mbps**, repeated *exactly* across many seconds = 1788 × 16,384 B/s =
  **96 % of the 245 Mbps path**. Not a smooth 176 at all.
- **Same binary, same session, 100 s apart, one env var changed: 177.592 → 223.633 Mbps (+26 %) when the knob
  was REMOVED.** Knob-on n=4: 163.2–177.6 (never above 177.6). Knob-off n=6: 184.3–223.6 (never below 184.3).
- The knob inflates cwnd to **2.04 MB** and srtt to **60 ms** → standing-queue overflow → **67 retransmits /
  9 RTOs** vs **4 / 0** in 676k packets.
- **H2 dead:** app queue holds a median 4.7 of 5 MiB and *never drains* — the sender never waits on the app.
- **H4 dead:** `peerRwnd` binds in ≤5/144 samples at a 16 MiB window.
- **H5 dead:** in-session path control TCP **244.52** / UDP **246.9** (300 offered, 17.69 % loss).
- **H3 not the cap:** CPU ≈0.80 core at both 168 and 217 Mbps; E21's 0.65 core does not reproduce.
- **H1 partly true:** `cwndFill` = 0.998 — **cwnd-limited 100 %** of samples — but by cut-and-slow-recover
  (1 MTU/RTT linear recovery, 0.8–14 s), not by a static window. D2 is a three-state sawtooth: 240 SCTP-Mbps
  plateau → one-sample cliff → linear ramp.

**Consequences (both are bigger than the experiment):**
1. **A1/N5 lose their premise.** There is no per-association window cap to multiply. Both were built this
   campaign (exp42) and are now demoted to insurance.
2. **`SB_SCTP_CA_STEP=32768` must NOT be enabled unconditionally in production.** `main`'s own `80a1daf3`
   found +36 % at ~71 ms clean and 3.4× under loss; this finds **−26 % at ~12.6 ms**. The step is
   **RTT-conditional**: it helps when the path is bandwidth/loss-limited and hurts when it is not. Production
   currently sets nothing, which is the *good* low-RTT configuration. **F1's "Action" is now wrong** and
   carries a banner saying so.
3. **Every absolute number in the campaign was measured knob-ON**, so the low-RTT figures understate
   performance by up to ~26 %. Hence exp43.

**Two doc corrections applied:** F1 (banner — RTT-conditional, do not enable unconditionally) and E35
(banner — the `sb-agent:instrumented` image was **never actually built**, and the instrumented binary carries
**no `SB_SCTP_*` wiring** because `benchdirect` is 34 commits behind `main` and lacks the tuning commit
`a4015bba`). That missing wiring is exactly what produced exp40's initial ~19 % arm separation, which exp40
then chased down into the headline.

## exp41 — 8-vCPU client VM + Firefox bare driver ✅ (prep only)

`results/2026-10-13-exp41-client-scaling-prep.md`. Instance `sb-client-8`, b2-30, US-EAST-VA-1, **8 vCPU /
28 GiB / 200 GB**, and a **500 Mbps** NIC (vs the b2-15's 250) — so C1 will test CPU, not the NIC cap.
Node 22.23.3, Playwright 1.63.0 (chromium-1243, firefox-1543), Xvfb, Firefox 157.0.1, geckodriver 0.36.0.
Full `~/sbtest` mirror plus a new `drive_bare_ff.js`. Both smoke tests passed; negative control exits 4.

Two findings:
- **The E28 `?bare=1` patch no longer exists anywhere** — it was applied to TESTBOX's web root and reverted
  byte-for-byte. Recovered as source by exp44.
- **netem correction:** `root netem delay 30ms` is **egress-only → +30 ms RTT**, not 70. The 70 ms arms need
  ingress-ifb + egress. Interface `ens3`.

## exp42 — A1 / N5 / A3 build ✅ (with an honest blocker)

`results/2026-10-13-exp42-a1-n5-a3-build.md`. New `agent/internal/bulkfan` package. `go build ./... &&
go test ./...` → 16 packages ok (27 new tests); client suite 274/274; default path proven byte-identical when
the flags are unset.

| flag | default | field-testable? |
|---|---|---|
| A3 `SB_BULK_PACE_MBPS` | `0` (off) | ✅ yes, no client change |
| N5 `SB_BULK_LANES` | `1` (off) | ⚠️ agent-side only — the browser rejects unknown lane labels and cannot reorder |
| A1 `SB_BULK_CONNS` | `1` (off) | ⚠️ agent-side only — also needs daemon peer-creation, a capability handshake, a signalling `peer_id` round-trip and browser fan-in |

The build agent's own recommendation: field-test A3 only; **enabling A1/N5 as-is breaks transfers.** exp40
subsequently removed A1/N5's *premise*, so they stay unbuilt.

## exp44 — client-side wiring (bare patch + N5) ✅

`results/2026-10-13-exp44-client-side-wiring.md`. The E28 `?bare=1` bare receiver is now **permanent source**
(`signaling-server/web/src/bareReceive.js` + three guarded touch points in `app.js`), inert without the query
param. Deploy/revert via `agent/deploy_bare_client.sh deploy|revert|manifest [bare|n5|all]` — prints
sha256/size/mode/owner manifests before and after and only reports `REVERT-OK` when the post-revert manifest
sorts identically to the recorded pre-deploy one. `agent/deploy_bare_client.selftest.sh` runs the whole cycle
locally (8 checks, exit 0). N5 client wiring also landed: `channelSet.js` accepts `bulk-<n>`, `directChannel.js`
maps them to LANE_BULK, `binaryEnvelope.js` decodes v2, `app.js` routes through `bulkReorder.js`. Client tests
**305 pass / 0 fail** (31 new). Nothing deployed; N5 proven only on synthetic frames.

## exp43 — the knob 2×2 re-baseline ✅ **and it overturns exp40's production implication**

`results/2026-10-13-exp43-knob-2x2-baseline.md`. **20 field cells, all valid, zero relay fallbacks.**
`{knob off, knob on} × {~12.0 ms EAST, ~70.0 ms WEST}` on the **real browser client**, n=3 per arm, plus an
interleaved ON/OFF control and a native-receiver arm.

| arm | RTT | knob | n | mean Mbps | range |
|---|---|---|---|---|---|
| A | 12.013 ms | **unset** (= production) | 3 | **95.757** | 93.9–98.0 |
| B | 12.013 ms | 32768 | 3 | **117.799** | 107.9–132.9 |
| C | 69.954 ms | **unset** (= production) | 3 | **13.405** | 11.9–15.2 |
| D | 69.954 ms | 32768 | 3 | **59.326** | 55.7–63.5 |

- **The knob HELPS the browser at both RTTs: +23.0 % at ~12 ms** (non-overlapping; A's best 97.95 < B's worst
  107.91; re-confirmed **+18.3 %** by a two-minute interleaved ON/OFF pair) **and 4.43× at ~70 ms**
  (non-overlapping by 4×).
- **B1 = 132.886 reproduces October's 132.0/133.1 almost exactly**, so the historical numbers are sound — they
  were simply all knob-ON.
- **The alarming number: production runs pion defaults, so the *shipped* browser-direct figures are ~95.8 Mbps
  at 12 ms and ~13.4 Mbps at 70 ms** — at 70 ms that is **~1/5 of the 64.54 this campaign has been quoting**.
  The knob-off 70 ms arm never escaped the regime: NIC-counter trace oscillating 4.7–24.2 Mbps for 533 s.
- **Mechanism / reconciliation with exp40:** the sender is cwnd-limited 100 % of the time and
  `rate = cwnd / (base RTT + queueing)`. At 1 MTU/RTT the ramp is ~85 KB/s at 12 ms but **~17 KB/s at 70 ms**,
  so a ~525 KB BDP takes ~30 s to refill and every loss resets it. A browser sink is *drain*-limited and cannot
  be refilled by that. On a **clean** path a big step instead overshoots the shallow buffer (exp40's
  mechanism). **So the sign is set by path loss, not by RTT alone.**
- **It is not a clean refutation of exp40:** arm E *reversed* exp40's native-receiver direction (ON 183.3 >
  OFF 156.1, ranges slightly overlapping), and today's path was **5–6× lossier** (125–147 TCP retransmits per
  10 s control vs exp40's 24) at a capacity swinging 210 ↔ 244 Mbps. exp40's cleanest single-variable pair
  (177.592 ON vs 223.633 OFF) was **not** reproduced.
- **Doc corrections:** the F1 banner written earlier today ("do NOT enable it in production") is **wrong and has
  been rewritten** — the action *stands* (enable it; ~1.2× at 12 ms, ~4.4× at 70 ms to a browser), with a
  path-dependence caveat. Verdict doc gained a **SIXTH REVISION**.
- **Method note M1:** the launch-ssh hang (backgrounded `nohup` + a second command in one ssh call) cost ~55 min
  and zero cells. Fixed with `KEEPALIVE_MS` set explicitly plus a bounded local launch
  (`ssh … & SSHPID=$!; (sleep 20; kill $SSHPID) & wait`); verified the `setsid`-detached driver survives the
  channel cut. A2's window was recovered from the agent log rather than discarded.
- **Not run:** the optional native knob sweep {unset, 8192, 16384, 32768} — arm C's four knob-off 70 ms transfers
  took 7–9 min each and consumed the budget. No monotone curve yet.

## exp36 (E36, previous pass) — closed

The Firefox agent was not hung on Playwright: its retry log is timestamped `02:08–02:12Z` **after** the
instances were shelved, so "all OVH endpoints unreachable" was an artefact of the shelve, not a network fault.
It had no give-up condition and burned ~6 h. Lesson recorded as runbook rule 14 (**liveness**), which then paid
off immediately: exp40's first attempt wedged silently for 12 minutes with zero child processes and was
restarted rather than waited on.
