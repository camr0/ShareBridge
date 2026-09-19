# Can v1 direct be optimized to approach relay-class speed? — verdict, 2026-09-18

**Question.** v1 direct (browser WebRTC DataChannel) delivers ~100–125 Mbps in the field, while v2's relay
delivers ~233 Mbps through the same path. Is there a change — up to and including **forking pion** — that closes
that gap?

**Answer: no — but with one important exception.** The gap is not in the transport, the agent, the host, the path
or the loss response. It is on the **client**, and the *transport-transport* gap is not closable: a pion fork is
worth at most **1.19–1.32×** and would act on the end of the wire that has demonstrable headroom (~85–100
Mbps/core available, CPU never saturated, host 75 % idle). **What *is* closable is the client's app JS: a
verification-preserving fix measured at 1.27× the rate and 3.23× the user-visible time (E29)**, taking v1 from
~115 to ~146 Mbps and removing an ~80 s tail. That is a large win — but it lands v1 at ~146 Mbps, still well
short of the relay's 233, because the browser's WebRTC DataChannel receive path requires JavaScript that the
native HTTP download path does not.

This document is the synthesis. Per-experiment detail lives in `results/`; the classified findings and their
caveats are F1–F19 of `ACTIONABLE-FINDINGS-2026-09-18.md`, which is the authoritative record where the two
documents disagree.

---

## 1. The lever table — everything tested, and what it gave

| lever | result | status |
|---|---|---|
| SCTP **CA-step** tuning | **1.19–1.30×** field (E13; the 1.89× campus figure does not generalise) | **the only positive** — and it is **committed on `main` but NOT enabled in the production container** (F1) |
| cwnd floor (`SB_SCTP_MIN_CWND`) | ≤ BDP harmless but never better; ≥1.6× BDP degrades 3–4×; ≥12× BDP breaks (E14/E17/E18) | **CLOSED** — the field has no loss-induced collapse to remove |
| RTO floor (`RTO_MAX`) | no effect on clean paths (91 runs), ~+14 % under lab loss only | **CLOSED** |
| Other pion knobs (`FAST_RTX_WND`, `MAX_RX_BUF`, `MAX_MSG`) | null; `MAX_MSG` below 65,536 B payload kills the transfer (E5) | **CLOSED** |
| Chunk size | 64 KiB wins on a clean path; under loss irrelevant; re-tested uncapped in E23 — **<4 %, noise, because SCTP re-fragments** | **CLOSED** |
| Striping / multi-session | two tabs on one host **1.07×**, two hosts **1.16×** (0.75× the sum of solos) (E20) | **CLOSED** |
| Loss tolerance | field rides 0.27–0.68 % loss with ~1 retransmission per loss; the ceiling is **loss-independent** (E19) | **CLOSED** |
| Aggressive lab knobs under real loss | the lab's "collapse" is a **model artifact** (uniform-random drops), not a field regime (E19/F10) | **CLOSED** |
| Host capacity | 0.000 % steal, 0.000 jiffies, 75 % idle, load ≤1.06; agent uses 0.83 mean / 1.42 peak cores (E21) | **FALSIFIED** as a remedy |
| **pion fork** | crypto AES-GCM **0.3 %**, app layer 2.4–5.5 %, datagram size already at pion's hard-coded MTU 1200, GC **0.27 %**; remaining levers = syscall batching 5.6 %, allocations 10.3 %, MTU estimate ~8 % | **≤1.19–1.32× — insufficient** (E22–E26) |
| Client app sink + assembly (SHA-1, StreamSaver, tail re-concat) | E15/E27: **1.52× at 1 vCPU, 1.098× at 4 vCPU** + an **~80 s tail**. **E29 measured the fix: 1.27× rate, 3.23× user-visible, −48 % renderer CPU**, verification preserved | **recovered — the campaign's biggest win** |
| Browser DataChannel receive | ≥**146.5 Mbps** at 4 vCPUs (E29's optimized client; E28's naive bare handler under-measured it at 123.68) | **the residual ceiling — not addressable in app or agent code** |

## 2. Why the ceiling is where it is

1. **The path is not the limit.** ~246 Mbps UDP, 240 over 4 TCP flows; v2 moves 233 through it.
2. **The sender is not the limit.** The same pion stack reaches **521–533 Mbps uncapped** against a *Go*
   receiver on loopback (E22); in the field its CPU is never saturated and giving it a 2-core quota changes
   nothing (E21). Its per-byte cost (~37 CPU-s/GB production-representative) buys ~85–100 Mbps/core, i.e. ~1.3
   cores at 120 Mbps — a *cost*, not a *constraint*.
3. **The host is not the limit.** 0.000 % steal, 75 % idle, zero run-queue pressure (E21).
4. **The client is.** The chain that isolates it:
   - the **real client** does 108.76 Mbps at 4 vCPUs and 37.19 at 1 vCPU (E25/E27);
   - removing the **sink** gives 119.45 / 56.35 (E27);
   - removing the **sink and all assembly** — a handler that only counts `byteLength` — gives **123.68 / 82.99**
     (E28), using only 0.63 / 0.35 renderer cores. It is not CPU-exhausted; it is **browser-limited**.
5. **Therefore the ~120 Mbps plateau is the browser's WebRTC DataChannel receive path**, and v1's architecture
   *requires* that path (a DataChannel cannot be handed to the browser's download manager).

## 3. The v1-vs-v2 comparison, correctly framed

The often-quoted "v2 relay is 2.1–3.8× faster" is **confounded as a transport claim** and needs restating:

- v1's arms necessarily run through the app's **JS** receive path;
- v2's arms ran through the browser's **native HTTP download** — `Content-Disposition: attachment`, **zero
  page-JS** (`app.js:1-7`, `gallery.js:157`, `handlers.go:347`).

So the multiplier is a **client-architecture** difference, not a TCP-vs-SCTP or kernel-transport one. **It is
still real for users** — a user downloading through v2 genuinely gets 233 Mbps where v1 gives ~110–125 — but it
must not be presented as evidence that WebRTC/SCTP is inherently 2× slower than the relay's TCP. Both halves
matter; the earlier write-ups only had the first.

Matched numbers, for the record (client-side implementation differs as above):

| path | CLIENT-EAST ~12 ms | CLIENT-WEST ~71 ms |
|---|---|---|
| v1 direct (app JS) | 102–131 Mbps | 60–68 Mbps |
| v1 relay | 42.0 | 55.3 |
| v2 relay (native download) | 233.3–233.7 (n=3, spread 0.18 %) | 214.9–228.4 |
| v2 native-download browser arm | 239.05 | — |
| path reference | ~246 (UDP) | — |

## 4. What is actually recoverable

1. **The client sink and assembly — MEASURED, and it is the big one (E29).** A minimal, verification-preserving
   fix (1 MiB tail buffer instead of re-concatenating the verification tail per append; SHA-1 in a module
   worker) gives **1.27× the rate (115.36 → 146.54 Mbps), 3.23× the user-visible time (139.87 s → 43.35 s),
   and 48 % less renderer CPU**, with **200/200 randomized equivalence trials byte-identical** and every cell
   reporting `✓ intact`. Pinned to 1 vCPU the rate is unchanged but the **tail collapses (≤131.8 s → 1.34 s)**,
   because that ceiling is StreamSaver's service-worker hop, not hashing. **This is the highest-value change
   available and it is app code.** Transport-independent, so it helps v2 as well.
2. **Enable the CA-step tuning in production (~1.2×).** Committed and tested on `main`, **not deployed**;
   production users currently get stock timing (~86/50 Mbps-class). A deployment change needing operator
   approval.
3. **Consider whether v1 needs to be the bulk-transfer path at all.** v1's direct path is capped by the browser;
   v2's relay is not. If bulk throughput matters, the relay/native-download path is the answer — which is the
   v2 proposition, now with the *reason* correctly identified as client architecture.

## 5. What would change this verdict (not tested)

- **With the client fixed, who binds now?** E29 lifted the joint rate to **146.54 Mbps**, which is *above* the
  ~120–124 Mbps plateau earlier attributed to the sender (E21) — so that plateau was itself client-limited, and
  the sender's true ceiling is higher. Whether v1 then climbs toward the ~246 Mbps path limit, or stalls
  somewhere below it, is **the open question** (E30).
- **A larger client.** The test client has 4 vCPUs and the browser's receive scaled 37 → 48 → 108 across
  1 → 2 → 4 cores. On an 8–16-core machine the residual ceiling may be higher. **This needs a resource
  decision (a bigger VM), not another experiment on this rig.**
- **Replacing StreamSaver's write path** (e.g. the File System Access API): E29's pinned arm shows the residual
  at 1 vCPU is the service-worker hop, not hashing (0.77 of 1.0 core used) — untested as a fix.
- **A non-browser v1 client.** The 521–533 Mbps loopback result shows the pion stack itself is not the limit; a
  native v1 client would bypass the browser cap entirely (relevant only if such a client is a product goal).
- **Two tabs do not add up** (E20: 72.30 Mbps combined), so a single client cannot be multiplied by opening
  more sessions in one browser.

## 6. Recommended actions, ranked

1. **Apply the client fix (MEASURED, E29) — the highest-value change available.** A minimal,
   verification-preserving change (1 MiB tail buffer + SHA-1 in a module worker) gives **1.27× the rate,
   3.23× the user-visible time (−96 s on a 754 MiB transfer), and −48 % renderer CPU**, with 200/200
   randomized equivalence trials byte-identical. App code; helps v1 and v2 alike.
2. **Deploy the CA-step tuning** (~1.2×) — the only transport-side win available, already written and tested.
3. **Settle the v1-vs-v2 question on client architecture, not transport speed.** If the relay path is the
   product's bulk-transfer route, say so explicitly, because that — not SCTP vs TCP — is the mechanism.
4. **Do not fork pion.** Bounded at 1.19–1.32× and aimed at the wrong end of the wire.
5. **Shelve the three test VMs** when the campaign closes (they are still billing).

## 7. Provenance

Experiments E1–E29 (2026-09-17/18), each with its own results file under `results/` and raw data under
`results/raw/`. Findings F1–F19 in `ACTIONABLE-FINDINGS-2026-09-18.md` carry the caveats, including the
corrections this campaign had to make to its own earlier claims:

- the "~225 Mbps lab ceiling" was a **rate cap**, not a ceiling (uncapped: 521–533 Mbps);
- the "779.7 B datagram" was an **averaging error** (payload ÷ all datagrams including SACKs);
- E1's "1228-B SCTP packet" was a misread of an overridden constant (really 1200 B / 1237 B wire);
- E8b's "per-client-host cap FALSIFIED" headline is **withdrawn** (1.07×/1.16×);
- the lab's loss model is **not** representative of the field;
- and the v1-vs-v2 multiplier is a **client-architecture** comparison, not a transport one.

Method lessons that cost real time and are worth keeping: always assert the path mode (silent relay fallback
hit 25 % of one experiment's cells and ~60 % of pinned-client attempts, reading ~44.7 Mbps); never use SNMP UDP
counters under GRO/GSO (50–3000× undercount); never call Playwright's Download API (it aborts what it measures);
and on this Mac, quote only rate-capped and ratio figures, because absolute throughput varies ~13× with load.
