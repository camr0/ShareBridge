# Can v1 direct be optimized to approach relay-class speed? — verdict, 2026-09-18

> **MAJOR REVISION — 2026-09-19 (E31 / F23). Read this before the tables below.** A follow-up experiment
> re-measured the **v1 relay** with a fixed client and it **changed two conclusions in this document**:
>
> 1. **v1 relay is NOT the weakest path — it is line speed.** E12's 42/55 Mbps was entirely the unfixed client's
>    sink. With the sink neutralised but the relay channel and its JS Noise decryption intact the relay delivers
>    **229.93 Mbps ≈ 96–99 % of same-session TCP capacity**; with E29's fix a real client gets **224.82 Mbps (4.36×)**.
>    E12's/F5's *"do not treat relay as a fallback"* is **withdrawn**.
> 2. **The client fix is worth much more than this document says** — see §4: on **direct** it is ~1.10× (the browser's
>    DataChannel is the cap), but on **relay** it is **4.36×**. Statements here that the fix buys "only ~10 % of rate"
>    are true for direct and **false for relay**.
>
> It also produced a counterintuitive ordering — **v1 relay (224.82) > v1 direct (149.09)** with the same fixed
> client — and closed the v1-vs-v2 throughput gap: v1 relay ≈ v2 relay ≈ 233 ≈ line speed. The v1-vs-v2 question is
> therefore about implementation and ops, **not speed**. The body below is retained as the pre-E31 record.
>
> **SECOND REVISION — 2026-09-19 (E32 / F24): direct's speed is latency-dependent; the relay's is not.**
> Direct with the fixed client measures **137.71 Mbps (17.2 MB/s) at 10.9 ms** but **74.71 (9.3 MB/s) at 70.4 ms** —
> **1.84×**, with a bare receive handler matching the fixed client (77.36) on 3.1× less CPU, so the loss is the
> **browser's DataChannel/SCTP receive path**, not app code (the path had 2.1–3.3× headroom). The relay lost only
> ~3 % over the same span (228.6 → 221.0), so **at ~70 ms the relay is ~3× faster than direct**. Any statement in
> this document of the form "v1 direct does ~120–150 Mbps" is **a ~12 ms figure** and must not be read as typical —
> realistic mobile/distant latency halves it.
>
> **THIRD REVISION — 2026-10-02 (E38): the browser is a real cost at high RTT, but it is NOT *the* ceiling, and
> the near-field plateau is not browser-specific.** A native **Go/pion receiver** was finally run on the real field
> path (E2, `results/2026-10-02-exp38-native-receiver-field.md`):
> - At **~70.75 ms** it does **106.4 Mbps** (100.1 / 107.6 / 111.6, n=3) against the browser's **64.54** — so the
>   browser's high-RTT receive path costs a real **~45 %**, and the competing "the pion *sender* is the cap"
>   hypothesis is **refuted**. That much of the verdict holds.
> - **But the absolute claim — "the v1-direct ceiling *is* the browser's DataChannel receive path" — is wrong.**
>   At **~10.94 ms** a properly-buffered native receiver lands on **138.98 Mbps**, statistically identical to the
>   browser's **139.0** (n=2). **At low RTT the browser is not a cap at all.** The ~120–139 Mbps near-field
>   plateau this document attributes to a "serialized browser receive path" is therefore **shared**, not
>   browser-specific — and what caps it is still open (E39 is testing whether it is a receive-side buffer/RTO
>   artifact: the same native receiver *bursts* to **233–237 Mbps** in a sawtooth while averaging 105.8).
> - **Two levers this document never considered, both found by E38:** (i) **pion's default 1 MiB SCTP advertised
>   receive window** (`initialRecvBufSize`, pion/sctp v1.9.4 `association.go:74`) hard-caps a stock pion receiver
>   at `rwnd/RTT` = 1 MiB / 70.75 ms = **118.5 predicted vs 114.95 measured (97 %)**; lifted to 4 MiB the sender
>   ramps to **232 Mbps = 94 % of the 245 Mbps path capacity**, then collapses cyclically (net 84.9). (ii) The
>   **client's OS UDP receive buffer** (`net.core.rmem_default` = **208 KiB**) causes SCTP loss and 1–3 s RTO
>   stalls on low-RTT bursts; at 4 MiB the native receiver goes 105.8 → **138.98**. Chrome sets its own socket
>   buffers, so (ii) is a native-receiver concern — but it means **stock native-receiver numbers are a floor**.
> - **Corrected ordering at 70 ms:** path capacity **245** > pion sender + 4 MiB window, peak **232** (unstable,
>   net 85) > stock pion receiver **115 ≈ rwnd/RTT** > deployed browser **64.5**.
>
> Net effect: the *direction* of the campaign's conclusion (the browser's receive path is expensive at high RTT)
> survives; its *absolute form* does not. Do not quote "the ceiling is the browser" without this revision.
>
> **FOURTH REVISION — 2026-10-02 (E39): the near-field plateau is a RECEIVE-SIDE ARTIFACT; the browser is a ~25 %
> cost, not the ceiling.** E39 swept pion's advertised SCTP receive window (`--max-rx-buf`) on the native receiver
> at ~10.94 ms with the path controlled **in-session**: TCP 1-flow **244.80** / 4-flow **244.80** / UDP **247.34**
> Mbps — so the path is ~245, **not** 139. Sustained native throughput by window: **1 MiB → 128.3 (n=5), 8 MiB →
> 155.4 (n=5), 16 MiB → 176.5 (n=6), 32 MiB → 177.0 (saturated)**; peaks 231–237 at 1 MiB rising to 234–292 at
> 16 MiB. The lever is **pion's advertised window, not the OS `rmem`** (16 MiB window: 176.2 at 4 MiB rmem ≈
> 176.6 at 16 MiB). The browser in the same session did **132.0 / 133.1 (mean 132.5)** — the native receiver is
> **+33 %** above it.
> **Corrected near-field ordering: path capacity 245 > residual buffer-insensitive sender-side cap ~176 > browser
> 132.5–139 > default native receiver 128.** Consequences: (i) this document's "~120–139 Mbps serialized browser
> ceiling" was **partly an artifact of an untuned receiver** and must not be quoted as the browser's limit;
> (ii) the browser remains a real **~25 %** cost at 11 ms (132.5 vs 176 achievable); (iii) there is a **residual
> ~176 Mbps cap that receive buffers do not move** — buffer-insensitive, therefore **sender-side**, and now the
> most interesting unexplained limit in the near field. It sits awkwardly against E21's "sender exonerated"
> (0.65 core at the plateau) and E22's loopback figure, and needs its own experiment.
>
> **FIFTH REVISION — 2026-10-13 (exp40): the ~176 "cap" is not a cap, and it is the rig's own tuning.**
> An instrumented native receiver on the ~12.6 ms path shows the "plateau" is a **cwnd sawtooth**: a hard
> **234.357 Mbps** instantaneous ceiling (repeated exactly — 1788 × 16 KiB/s = **96 % of the 245 Mbps path**)
> interrupted by one-sample cliffs with linear 1 MTU/RTT recovery. **Every campaign number (139, 176, 177,
> 232) is a window average over that duty cycle.** `cwndFill` = 0.998 (**cwnd-limited 100 %** of samples);
> `pendingBytes` sits at 4.7 of 5 MiB and never drains; `peerRwnd` binds in ≤5/144 samples; CPU ≈0.80 core;
> in-session path control 244.5 TCP / 246.9 UDP.
> **The specific ~176 is produced by the rig's own `SB_SCTP_CA_STEP=32768`:** same binary, same session, 100 s
> apart, one env var changed → **177.592 → 223.633 Mbps (+26 %) when the knob was REMOVED** (knob-on n=4,
> never above 177.6; knob-off n=6, never below 184.3). The knob inflates cwnd to 2.04 MB and srtt to 60 ms,
> overflowing a standing queue: **67 retransmits / 9 RTOs vs 4 / 0** in 676k packets.
> **Consequences:** (i) the ~176 sender-side cap is **not a property of the transport**, so A1/N5 (window
> multiplication) lose their premise; (ii) `main`'s `80a1daf3` (+36 % at ~71 ms) and this result (−26 % at
> ~12.6 ms) together make the CA step **RTT-conditional** — do not enable it unconditionally (see the F1
> banner); (iii) **every absolute number in this document was measured knob-ON**, so the low-RTT figures
> understate performance by up to ~26 % and need a re-baseline.
>
> **SIXTH REVISION — 2026-10-13 (exp43): the fifth revision's production implication was WRONG.** The fifth
> revision read exp40 as "drop the knob". exp43 then measured the **real browser client** across 20 cells and
> found the knob **helps at both RTTs** — **+23.0 % at ~12 ms** (117.8 vs 95.8, non-overlapping, re-confirmed
> +18.3 % by an interleaved ON/OFF pair) and **4.43× at ~70 ms** (59.3 vs 13.4, non-overlapping). Its
> native-receiver arm *reversed* exp40's direction (ON 183.3 > OFF 156.1), and its path was 5–6× lossier
> (125–147 TCP retransmits per control vs exp40's 24) at a capacity swinging 210 ↔ 244 Mbps.
> **Reconciliation:** a large CA step hurts a clean shallow-buffered path and helps a lossy or ramp-limited one;
> a browser sink is also *drain*-limited, so at 70 ms it cannot be refilled by a 1-MTU-per-RTT ramp at all.
> The knob's **sign is path-condition-dependent**; its *magnitude* at 70 ms is not in doubt.
> **The production consequence is larger than the fifth revision said.** Production sets no `SB_SCTP_*` at all,
> so the **shipped** browser-direct performance is **~95.8 Mbps at ~12 ms and ~13.4 Mbps at ~70 ms** — not the
> 132–139 / 64.5 this document has quoted throughout. Enabling the knob is the largest confirmed direct-path
> win available and at 70 ms it is a ~4.4× one. It should still be re-measured post-deployment rather than
> treated as a constant. See `results/2026-10-13-exp43-knob-2x2-baseline.md`.
>
> **SEVENTH REVISION — 2026-10-13 (exp45): the client CPU *is* the cap. "A bigger client VM won't help" is
> FALSIFIED, and the verdict reopens.**
> The last unverified assumption behind this document was that 4 vCPU is enough for the browser receive path.
> exp45 measured an **8-vCPU** client and ran a **same-host, same-path, same-container `taskset -c 0-3` pin
> control**:
> - 8 vCPU vs exp43's 4 vCPU: **1.52× / 1.55× / 1.63× / 2.99×** (12 ms knob-ON / 12 ms knob-OFF / 72 ms
>   knob-ON / 72 ms knob-OFF).
> - The pin control is decisive: **179.554 → 121.680 Mbps (1.48×)** on one machine with only the CPU budget
>   changed, landing **+3.3 %** on exp43's real 4-vCPU number (117.799) — even though that host's TCP path
>   carried **1.56×** CLIENT-EAST's capacity. The 4-vCPU ceiling is therefore not the host, NIC or path.
> - **Not raw CPU exhaustion:** the 8-vCPU client uses 3.2–4.0 of 8 cores and the pinned one 2.62 of 4, yet
>   halving the budget halves throughput. It is **serialization/queueing** in the receive pipeline
>   (network-service SCTP/DTLS → renderer `onmessage` → sink). **E28's "serialized receive path" framing
>   survives; what dies is the inference that 4 vCPU is out of its way.**
> - **Production is the most CPU-limited arm in the matrix:** the knob-OFF 72 ms arm scales **2.99×**
>   (13.405 → 40.070) versus 1.63× for knob-ON. So the sixth revision's "production does ~13 Mbps at 70 ms"
>   is partly a *client-hardware* effect — but it is still 2.4× worse than knob-ON.
> - The knob's 12 ms benefit is **not** a client artefact (+21.1 % on 8 vCPU vs +23.0 % on 4 vCPU); its 70 ms
>   benefit shrinks relatively (4.43× → 2.41×) only because the bigger client rescues the knob-OFF arm more.
> - **C2:** with the app pipeline removed entirely, **Firefox (Gecko) delivers 0.525× Chromium (Blink)**
>   bare-vs-bare, same client, same hour (112.077 vs 213.469 Mbps) — a first-order receive-path variable that
>   every E25/E27/E28 conclusion (all Chromium-only) never controlled for.
> **Consequence: the binding constraint on v1 direct is the client's receive-pipeline CPU**, which makes the
> still-unlanded **E29 client fix** (−48 % client CPU, 3.23× user-visible, 4.36× on relay) the highest-value
> item in the whole programme — ahead of any transport knob. See
> `results/2026-10-13-exp45-client-scaling-and-firefox.md`.
>
> **EIGHTH REVISION — 2026-10-13 (exp46): the attribution must be SPLIT BY RTT — airtight at 12 ms, collapsed
> at 70 ms.** The campaign's claim ("the ceiling is the browser's WebRTC DataChannel receive path") had never
> been tested against the same browser doing a **native HTTPS download** of the same bytes on the same path in
> the same hour. exp46 did exactly that, 13 transfer cells:
> - **At ~12.1 ms the attribution is AIRTIGHT.** Browser native HTTPS = **231.57 Mbps** = **97.5 %** of a
>   non-browser `curl` on the identical conduit (237.5) and **95.0 %** of the direct `iperf3` path control
>   (243.7) — while the same browser's DataChannel in the production configuration does **97.69–103.99**
>   (**117.80** with the knob). **A 2.23–2.37× gap that neither the conduit, the tunnel, the path, nor
>   Chromium's download machinery can explain.** The ceiling is inside the WebRTC/SCTP/DTLS DataChannel +
>   app-sink path — and per exp45 its binding constraint there is the **client's receive-pipeline CPU**.
> - **At ~70.0 ms the attribution does NOT survive in its strong form.** The *direct path itself* carried only
>   **72.8–120.3 Mbps** of single-flow TCP this session (it moved **1.65×** within the session), and a tunnelled
>   HTTPS stream over that path delivered **67.2–75.8** — while the browser's DataChannel with the knob ON
>   delivered **62.3–66.8**. The DataChannel is **at the level of an ordinary TCP stream** on the same path; it
>   is not the demonstrated ceiling. The alarming 70 ms production figure (13.8–35.4, one hard stall) is a
>   **sender-side cwnd** phenomenon, exactly as exp43 argued.
> **Consequence: this document's headline is a ~12 ms result.** At 70 ms the limit is the path/transport and the
> fix is the CA step (sixth revision), not the receive path. Caveats: the 70 ms arm was served through a
> **reverse SSH tunnel** (VERSA has no reachable inbound TCP — residential NAT + IPv6 firewall), so TCP-over-TCP
> confounds it; and the 111–163 Mbps native-TCP band the brief assumed for 70 ms did not hold today
> (72.8–120.3). See `results/2026-10-13-exp46-native-https-reference.md`.
>
> **NINTH REVISION — 2026-10-13 (exp47): there is no RTT at which direct is competitive, and the ratio curve
> corroborates the eighth revision.** The D3 band sweep (8 vCPU client, n=2, in-band TCP control per band) ran
> the curve out to 150 ms:
>
> | band | knob OFF (production) | knob ON | relay | direct ÷ relay (OFF) |
> |---|---|---|---|---|
> | ~30 ms | **73.29** | **96.29** | ~225–230 | 3.1× |
> | ~50 ms | **62.29** | **74.18** | ~225–230 | 3.6× |
> | ~100 ms | **34.53** | **53.22** | ~225–230 | 6.6× |
> | ~150 ms | **24.85** | **36.32** | ~225–230 | 9.1× |
>
> The curve is **monotone decreasing and never approaches the relay** — the gap *widens* with RTT — so "where
> does v1 direct become unusable?" answers as **everywhere measured**. The knob helps in every band (+19 % to
> +54 %), reproducing the sixth revision's direction at four RTTs rather than two.
> **The mechanism corroborates the eighth revision:** direct-as-a-fraction-of-TCP **rises with RTT** (0.474 →
> 0.898 knob off; 0.504 → 1.51 knob on), i.e. at low RTT the WebRTC/SCTP/DTLS + app-sink path is the extra cost
> and at high RTT the raw path is the limit. At 150 ms with the knob on the DataChannel even **exceeds** the
> same-band single-flow TCP control — the raw path is binding, not the transport.
> **Caveat:** the eight TCP controls are not a coherent set (24.1–207.6 Mbps) on a path that moved 1.65× within
> a session, so the ratios are soft and the absolute numbers plus the monotone trend are the robust part. See
> `results/2026-10-13-exp47-rtt-bands.md`.

**Question.** v1 direct (browser WebRTC DataChannel) delivers ~100–125 Mbps in the field, while v2's relay
delivers ~233 Mbps through the same path. Is there a change — up to and including **forking pion** — that closes
that gap?

**Answer: no — but one substantial win is available anyway.** The gap is not in the transport, the agent, the
host, the path or the loss response. It is the **browser's WebRTC DataChannel receive path, which is serialized
at ~120 Mbps** — the client uses 0.98 of 4 cores at the ceiling, is hurt by fewer cores and **not helped by
more**, and four independent configurations land on 108–124 Mbps (E30/E28/E21). **A pion fork is worth at most
1.19–1.32× and would act on the end of the wire that demonstrably has headroom** (sender capped at 2 cores: no
change, 0.65 core used; host 75 % idle; path carries 246 Mbps with 2.1× spare). The app's own JavaScript is
worth only **~10 % of the rate** — but it carries an **~87 s post-transfer tail**, and a verification-preserving
fix for it is worth **3.23× in user-visible time** (139.87 s → 43.35 s) and **−48 % client CPU** (E29/E30). That
is the win to take. It does **not**, however, move v1 toward the relay's 233 Mbps: the browser's JS receive path
is the ceiling, and the native HTTP download path is not subject to it.

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
| Client app sink + assembly (SHA-1, StreamSaver, tail re-concat) | E15/E27: **1.52× at 1 vCPU, 1.098× at 4 vCPU** + an **~87 s tail**. **E29/E30 measured the fix: 3.23× user-visible time, −48 % client CPU**, ~1.10× rate, verification preserved | **recovered — the user-visible win** |
| Browser DataChannel receive | **~120 Mbps, SERIALIZED** (E30: 119.17 with a fixed client using 0.98 of 4 cores, not helped by more; E28 bare 123.68; E21's old-client plateau 124.17) | **the ceiling — not addressable in app or agent code** |

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
| v1 direct **with the client fix** (E32) | **137.71** (17.2 MB/s) | **74.71** (9.3 MB/s) |
| v1 relay | 42.0 | 55.3 |
| v1 relay **with E29's client fix** (E31) | **228.6** | **221.0** |
| v2 relay (native download) | 233.3–233.7 (n=3, spread 0.18 %) | 214.9–228.4 |
| v2 native-download browser arm | 239.05 | — |
| path reference | ~246 (UDP) | — |

## 4. What is actually recoverable

1. **The client fix — MEASURED, and it is the change to make (E29 + E30 + E31).** A minimal, verification-preserving
   change (1 MiB tail buffer instead of re-concatenating the verification tail per append; SHA-1 in a module
   worker) gives **3.23× the user-visible time (139.87 s → 43.35 s; the tail goes 87.4 s → 0.53 s)** and
   **−48 % client CPU**, with **200/200 randomized equivalence trials byte-identical**. Its **rate** value depends
   entirely on the path: **~1.10× on direct** (which the browser's DataChannel caps) but **4.36× on relay**
   (51.58 → 224.82 Mbps, E31). App code; take it — the relay number means it is also the change that makes the
   relay a genuinely fast path.
2. **Enable the CA-step tuning in production (~1.2×).** Committed and tested on `main`, **not deployed**;
   production users currently get stock timing (~86/50 Mbps-class). A deployment change needing operator
   approval.
3. **Consider whether v1 needs to be the bulk-transfer path at all.** v1's direct path is capped by the browser;
   v2's relay is not. If bulk throughput matters, the relay/native-download path is the answer — which is the
   v2 proposition, now with the *reason* correctly identified as client architecture.

## 5. What would change this verdict (not tested)

- **With the client fixed, who binds now? — ANSWERED (E30): still the client, and it is serialized.** The fixed
  client does **119.17 Mbps (n=4, range 107.5–130.9)** while using **0.98 of 4 cores**; it is hurt by fewer cores
  (68.34 at 2, 39.02 at 1) and **not helped by more**; capping the *sender* at 2 cores changes nothing (115.18 vs
  119.17, sender using 0.65 core), and iperf3 on the same path carries 246 Mbps UDP / 241 TCP. So the ~120 Mbps
  ceiling is the **browser's receive path**, and v1 does **not** climb toward the ~246 Mbps path limit.
- **A larger client — now expected NOT to help (inference, not measurement).** The client uses only 0.98 of 4
  cores at the ceiling and is not helped by more cores, which is the signature of a **serial** bottleneck; a
  bigger VM would therefore not be expected to raise it. This is the last unverified assumption behind the
  verdict, and it is stated as inference because testing it needs a resource decision, not another run here.
- **Replacing StreamSaver's write path** (File System Access API, or batching frames before writing) — **now the
  most promising remaining lever, and the first one expected to move the rate rather than the tail.** The client
  receives ~16 KiB frames (48,260 of them for 754 MiB), and today each one costs a ~1 MiB tail copy plus a
  StreamSaver `postMessage`/service-worker hop, on the main thread (F22). E29 removed the tail copy and moved
  hashing off-thread but **kept StreamSaver**, which is why the rate only moved 1.10×. The v1 **relay** path pays
  the same per-frame cost *plus* JS Noise decryption per frame, so this is likely to lift relay too.

  > **CORRECTION (2026-10-02) — this bullet is STALE; its rate claim is closed for 4-vCPU clients.** E31 and E32
  > bound the **entire** app receive path — of which the StreamSaver hop is a subset — at ≤4%: E32-W5 bare counter
  > **77.36** vs fixed client **74.71** = **1.04×** (direct, 70 ms, same session), and E31 arm C discard sink
  > **229.93** vs fix **224.82** = **1.02×** (relay, JS Noise decryption intact). The "1.10× ⇒ StreamSaver is the
  > residual" reading does not follow. **Caveat that keeps a sliver open:** this is rate-at-4-vCPU only — at
  > **1 vCPU** the app path still matters (E28-bare pinned 82.99 vs E29-fixed pinned 37.53 ≈ **2.2×**), so for
  > low-end/mobile clients the sink write path remains a live **CPU/tail** lever. See
  > `2026-10-02-v1-direct-candidate-experiments-review.md` §2.1.
- **Re-testing the v1 relay with a fixed client.** E12's 42.0 / 55.3 Mbps is latency-independent and was measured
  with the throttled client, so it may be a main-thread ceiling rather than a property of the relay design. Worth
  settling, because it decides the v1-direct-vs-v1-relay comparison (F22).
- **A non-browser v1 client.** The 521–533 Mbps loopback result shows the pion stack itself is not the limit; a
  native v1 client would bypass the browser cap entirely (relevant only if such a client is a product goal).
- **Two tabs do not add up** (E20: 72.30 Mbps combined), so a single client cannot be multiplied by opening
  more sessions in one browser.

## 6. Recommended actions, ranked

1. **Apply the client fix (MEASURED, E29 + E30) — the highest-value change available.** A minimal,
   verification-preserving change (1 MiB tail buffer + SHA-1 in a module worker) gives **3.23× the user-visible
   time (139.87 s → 43.35 s) and −48 % client CPU** for ~1.10× on the rate, with 200/200 randomized equivalence
   trials byte-identical. App code; helps v1 and v2 alike.
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
