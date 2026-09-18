# Experiment E19 — in-flow loss of a real v1 direct transfer (capture on the receiving client) — 2026-09-18

**Verdict:** *(pending — written incrementally per runbook rule 13)*

**Goal:** measure the *actual* per-datagram loss / retransmit fraction experienced by a real v1
direct download in the field, to decide between the E11-inferred in-flow loss (~1.5–2×10⁻⁴) and the
open-loop iperf3 UDP figure (0.43% at 100 Mbps offered). This number decides whether any
loss-tolerance work on v1 is worth pursuing.

**Setup:** VERSA test agent `sb-run` untouched (`sb-agent:pristine`, `UI_PORT=7879`, host networking,
`SB_SCTP_CA_STEP=32768`, no `SB_SCTP_MIN_CWND` — verified by `docker inspect` 19:54:55Z, **no container
recreation**, per instruction). TESTBOX v1 signalling. Payload 754 MiB = 790,626,304 B = 6,325.01 Mb.
Driver `drive_click.js`, one click-only tab (`NTABS=1`), no Playwright Download API. Metric:
agent-side first `DataChannel lanes ready` → `download complete`, agent clock UTC; Mbps = 6,325.01 ÷ window s.
Capture on the **receiving client VM** (`CLIENT-EAST`; `CLIENT-WEST` if time allows), capture filter
`udp and host <VERSA-ip>` (VERSA public IP obtained at runtime, not recorded here per runbook §1.5).

## Method (important — the runbook's assumed SCTP parse needs qualification)

v1 direct = WebRTC DataChannel = **SCTP encapsulated inside encrypted DTLS**, so plain
`tshark -Y sctp.chunk_type==0` can only work if DTLS is decrypted (keylog). Three independent
measurements, in order of robustness:

1. **DTLS record sequence-number gaps (decryption-free, primary).** The DTLS record header — incl.
   epoch + 48-bit sequence number (DTLS 1.2) — is **plaintext**. Every SCTP packet (new TSN or
   retransmission) is sent as a new DTLS app-data record with an incrementing sequence number. At the
   receiver, `missing = (max_seq − min_seq + 1) − distinct_arrived` over epoch-1 records = the number
   of datagrams actually lost in flight on that direction. This measures *network* loss on the flow's
   own 5-tuple at its own operating rate — exactly the number E11 §5 asked for.
2. **Duplicate-TSN DATA chunks (if SCTP becomes parseable, e.g. via `SSLKEYLOGFILE` DTLS keylog):**
   retransmissions received = the SCTP-level response (true loss + spurious/ack-loss-driven). Together
   with (1), the difference separates true data-loss from spurious retransmission.
3. **Wire-ratio fallback:** data-direction datagrams × mean payload vs 790,626,304 B delivered.

Tooling: **tshark 4.2.2 installed on CLIENT-EAST** (`apt-get install -y tshark`; test VM — allowed;
also `sudo` confirmed passwordless). tcpdump pre-existed on both VMs. Nothing else installed; no
agent/infra change whatsoever.

## Pre-state (all times UTC)

- 19:54:55Z health: VERSA `docker ps` → `sb-run` running, image `sb-agent:pristine`, env
  `UI_PORT=7879`, `SB_SCTP_CA_STEP=32768`, no `SB_SCTP_MIN_CWND` → rig exactly as E18 restored it.
- TESTBOX signalling: HTTP **200 from VERSA** at 19:55Z. (Anomaly, noted for the record: the same URL
  from the lab Mac gives curl exit 35/000 — Mac-local TLS/network issue, not rig-side; the runbook's
  health check is satisfied rig-side, and the driver VMs reach TESTBOX normally as they did all day.)
- CLIENT-EAST + CLIENT-WEST both reachable, **zero stray `node`/`chrome`/`Xvfb`** on either (checked
  19:54:55Z). No other field agent active.
- CLIENT-EAST: iface `ens3`, `/tmp` on `/` with 90 G free — enough for ~1 GB pcap.
- Share: newest unexpired = the 754 MiB OpenCloud payload share, 4/500 downloads used at 19:55Z
  (code withheld here per runbook §1.5).

## Raw cells (appended after every step)

### Cell E0 — CLIENT-EAST, capture mis-scoped (ABORTED as a loss measurement; valid goodput datapoint)

| time (UTC) | event |
|---|---|
| 19:56:33.170 | pre-state: `Udp: InDatagrams 2067618 InErrors 416 OutDatagrams 120918 RcvbufErrors 416`; capture started `tshark -i ens3 -f "udp and host <VERSA-v4>"` (VERSA public v4 = its ipify egress) |
| 19:56:41.5 | driver launched (`NTABS=1`, `SSLKEYLOGFILE` set — never populated, see E1) |
| 19:56:43.3 | agent: HMAC verified → creating peer; relay standby channel connected |
| 19:56:44.134 | **`DataChannel lanes ready`** |
| 19:56:44.248 | `[tab0] clicked` |
| 19:57:28.389 | relay standby expired unused (`pending wait window exceeded`) ⇒ **direct, not relayed** |
| 19:57:41.069 | **`download complete` (count 57)** |

**Goodput: window 56.935 s → 111.1 Mbps** — textbook EAST direct (E18 same-day stock: 98.9–124.4).

**But the capture saw nothing**: pcap stayed at 32 KB, containing only v4 ICE/STUN connectivity checks from VERSA (random src ports → client :47794). Diagnosis: CLIENT-EAST has global IPv6 and VERSA egress v6 is a **rotating privacy address** (`scope global dynamic noprefixroute`), so (a) the selected candidate pair may be v6, invisible to my v4-host BPF filter, and (b) `host <v6>` filtering is unreliable anyway. Additionally, host-wide `UdpInDatagrams` moved only **+202 / +211 out** across the whole transfer — no bulk UDP visible at SNMP level v4 *or* v6 (possible kernel GRO undercount; pcap is ground truth, so E1 captures **all UDP, no host filter**, post-filtered by the observed 5-tuple). Share budget: 57/500. Browser stack cleaned (`pkill -x node|chrome|Xvfb`; a dying `node` still listed 1 s post-pkill — re-verified before E1).

### Cell E1 — CLIENT-EAST attempt 1 (ABORTED: relayed)

Launched 20:00:12Z: agent peer created 20:00:14.36, **peer `closed` 20:00:24.38**, relaychannel handshake complete 20:00:24.42 ⇒ fell back to **relay** — not a measurement (instruction §4). Client log shows `PAGEERROR join() re-entered while browser signaling socket is still active` and a 10 s join delay. Killed, cleaned. Its pcap (145,844 B) shows ICE over **both** v4 (20 DTLS frames) and v6 (21 DTLS frames) before death. tshark installed; captures hereafter are `udp and not port 5353` (family-agnostic) — no host filter, because VERSA's v6 is a rotating privacy address.

### Cell E2 — CLIENT-EAST, capture OK, in-window: **direct, 111.4 Mbps** (loss analysis method pivoted — see below)

| time (UTC) | event |
|---|---|
| 20:01:21.06 | pre-state: `Udp: InDatagrams 2068031 OutDatagrams 121367`; rx_bytes 44848325132; capture started (`udp and not port 5353`) |
| 20:01:37.79 | agent: HMAC verified → creating peer; relay standby connected |
| 20:01:38.587 | **`DataChannel lanes ready`** |
| 20:01:39.203 | `[tab0] clicked` |
| 20:02:05–20:02:31 | mid-transfer: NIC rx 12,810 KiB/s; vmstat `28 us / 39 sy / 34 id` (aggregate over 4 vCPU); pcap 459→589 MB |
| 20:02:22.977 | relay standby expired unused (`pending wait window exceeded`) ⇒ **direct confirmed, no relay data** |
| 20:02:35.363 | **`download complete` (count 58)** |
| 20:02:58.74 | capture stopped (+23 s tail; heartbeats/consent checks only) |

**Goodput: window 56.776 s → 111.4 Mbps.** Capture: 1,101,279 packets / 979,302,812 B / 81.767 s (capinfos). Post `Udp: In 2068242 Out 126373` — SNMP moved only **+211** despite ~890 MB arriving (kernel-level UDP GRO-style undercount; pcap is ground truth). rx_bytes Δ = 889,888,890 B.

**The flow is IPv6** (VERSA rotating-privacy v6 → client global v6; ports agent:36547 → client:44861):

| direction | packets | bytes (L2) | mean frame |
|---|---|---|---|
| VERSA → CLIENT (data) | **684,824** | **889,070,618** | 1298.2 B |
| CLIENT → VERSA (SACKs) | **411,209** | 53,227,696 | 129.4 B |
| v4 leftovers (ICE) | ~5,018 | ~0.57 MB | — |

**Why the planned tshark TSN/DTLS methods failed (all verified empirically, tshark 4.2.2):**
1. `sctp.chunk_type`/`sctp.data_tsn`: zero hits — v1 direct negotiated **DTLS 1.3** (ServerHello `0xfefd`); tshark 4.2 decrypts the *handshake* with `SSLKEYLOGFILE` secrets (verified: Certificate flight appears) but does **not** decrypt DTLS-1.3 application data, so inner SCTP is invisible. (Keylog export itself works — Chromium headless-shell honors `SSLKEYLOGFILE`, 81 lines.) `dtls.port==<p>,sctp` decode-as: no effect.
2. DTLS record **sequence-number gaps**: unusable — DTLS 1.3 seqs are implicit; tshark's reconstructed `dtls.record.sequence_number` space is ~10× sparser than the record count in *both* directions (data: 684,740 recs, seq max 6,841,746; SACK: 411,130 recs, max 4,282,627) ⇒ any gap arithmetic is meaningless, not 90%-loss.

### Cells E2+E3 — THE MEASUREMENT (CLIENT-EAST, n=2, both direct)

**Method that worked (after two failed pivots, below):** the data connection's DTLS record layer carries **explicit, per-record epoch-1 sequence numbers** (record-layer style dissected as legacy-1.2 layout — visible without decryption). Every SCTP packet sent (first transmission *or* retransmission — DTLS re-encrypts, so every send = one new record seq) increments the seq; at the receiver, **`missing = (max_seq − min_seq + 1) − distinct_arrived` = datagrams lost in flight**. Validated: consecutive mid-transfer records show seq 248100, 248101… (no sparsity). My first pass mis-parsed decimal seq strings as hex (the "~10× sparse seq space" in the E2 section above is **my bug, not the network's** — disregard that line; the DTLS-1.3-implicit-seq theory was wrong too: negotiated record layer shows explicit seqs). Analysis: `tshark -Y "udp.port==<agent> || udp.port==<client>" -T fields -e ipv6.src -e dtls.record.epoch -e dtls.record.sequence_number` → per-direction epoch-1 seq accounting.

| cell | goodput (window) | data dir: sent / missing | **data-dir loss** | SACK dir | notes |
|---|---|---|---|---|---|
| E2 | **111.4 Mbps** (56.78 s) | 686,593 / 1,853 | **2.70×10⁻³ (0.27%)** | 411,130 recs in seq range [4774, 415903]: **0 missing** | SACK seqs 0–4773 absent from capture — early SACKs likely rode the pre-nomination candidate pair; up-direction loss bounded < 2.4×10⁻⁶ within observed range |
| E3 | **90.6 Mbps** (69.83 s) | 686,103 / 2,654 | **3.87×10⁻³ (0.39%)** | 438,275 recs, seq 0–438,274: **0 missing** | clean full-range SACK seqs; also had VERSA-side `enp4s0` NIC tx brackets (mid-transfer 64,898/5.044 s vs post-cell noise 5,501/5 s) — noise swamps 10⁻⁴ resolution, abandoned for the seq method |

**E3 flow-window wire facts** (E2 nearly identical): data 683,536 rcvd datagrams / 887.4 MB (mean frame 1298.2 B = ~1236 B UDP payload); SACK 438,366 / 57.1 MB (mean 130.3 B). Flow is **IPv6 end-to-end** (VERSA rotating-privacy v6 → client global v6); v4 carried only losing ICE checks.

**Consistency check (retransmit accounting):** E2: 790,626,304 B ÷ ~1,155 B/record ≈ 684,521 unique + 1,853 retransmissions + ~hb ≈ 686,4xx ≈ 686,593 sent ✓. E3: ÷1,157 ≈ 683,4xx + 2,654 + hb ≈ 686,1xx ✓. I.e. ≈ one retransmission per lost datagram, **no retransmit storm**, and the retransmit fraction ≈ the loss fraction (SACK path lossless ⇒ no spurious-retransmit component).

**E3 other observations:** SNMP `UdpInDatagrams` on the client moved +211 while ~890 MB arrived (kernel UDP-GRO-style undercount on both endpoints — VERSA `UdpOutDatagrams` also undercounted ~50×; do not use SNMP UDP counters on this rig). Client vmstat mid-transfer: 28 us / 39 sy / 34 id (4-vCPU aggregate). VERSA background tx noise ≈ 5.5k frames/5 s (home host), rx ≈ 1.3k/5 s.

### Cell W1 — CLIENT-WEST (~71 ms), capture OK, direct, **58.8 Mbps**

Attempt 1 (20:25:23Z) relayed (peer closed +10 s, relay handshake complete — same silent-fallback mode as E1) → aborted, killed, retried. Attempt 2: **lanes ready 20:26:19.051 → download complete 20:28:06.629 = 107.578 s → 58.8 Mbps** (count 60), relay standby expired unused ⇒ direct. Capture `tcpdump -i ens3 -w /tmp/exp19-w1.pcap 'udp and not port 5353'` (tcpdump pre-existed on WEST; nothing installed there).

Flow again **IPv6** (VERSA privacy v6 → WEST global v6, agent:60129 → client:35481):

| direction | records (epoch-1) | seq range | **missing** | **loss** |
|---|---|---|---|---|
| VERSA → WEST (data) | 676,550 received | 0–681,162 (681,163 sent) | **4,613** | **6.77×10⁻³ (0.68%)** |
| WEST → VERSA (SACK) | 372,552 | 0–372,551 | **0** | < 2.7×10⁻⁶ |

Retransmit accounting closes (790,626,304 B ÷ ~1,170 B/record ≈ 676.2k unique + 4,613 retrans + heartbeats ≈ 681.1k sent ✓).

## Final table (all cells, direct, agent-side windows)

| cell | client | RTT | goodput | **data-dir in-flow loss** | SACK-dir loss | wire retransmit check |
|---|---|---|---|---|---|---|
| E2 | EAST | ~12 ms | 111.4 Mbps | **2.70×10⁻³** | 0 (in [4774,415903]) | ≈1 retrans/loss ✓ |
| E3 | EAST | ~12 ms | 90.6 Mbps | **3.87×10⁻³** | 0 | ✓ |
| W1 | WEST | ~71 ms | 58.8 Mbps | **6.77×10⁻³** | 0 | ✓ |

## Interpretation

1. **The open question is closed: the in-flow loss is 2.7–6.8×10⁻³ — the 0.43% open-loop figure's regime, ~15–45× the 1.5–2×10⁻⁴ E11 inference.** EAST in-flow (0.27–0.39%) straddles iperf's 0.43%-at-100-Mbps; WEST (0.68%) shows loss is path-specific and *worse* on the long path despite a lower wire rate.
2. **Why the inference was wrong:** it extrapolated the E11 lab ladder, but the field sender demonstrably does not occupy that regime — at 2.7–3.9×10⁻³ real loss the field delivers 90–111 Mbps where the lab ladder predicts 15–18 Mbps (5.6–7× gap). E11's own caveat 4 named this branch ("if the field's figure is correct, the lab is simply too pessimistic"); that is now measured fact. Likely lab/field differences: the shim's per-datagram *random* loss vs the field's likely *bursty* queue drops, plus the lab's missing receiver sink — not adjudicated here.
3. **Loss-tolerance work: still not worth pursuing, but for the corrected reason.** The path is *not* clean (~0.3–0.7% downstream) — yet the deployed stack (`SB_SCTP_CA_STEP=32768` rig, stock SCTP recovery) rides through it at 90–111 Mbps with ≈1 retransmission per loss and **zero spurious-retransmit pressure** (SACK path lossless). E18's min-cwnd null is thereby explained without assuming a clean path: there is no field window *collapse* for a floor to prevent. The E11/E14 levers were calibrated against a lab-only regime; the v1 field ceiling (~100–131 Mbps EAST vs 225 Mbps lab clean-path) remains attributed to the client sink/architecture (Exp 9 / E17 / E18), not loss.
4. **Free operational finding: silent relay fallback is frequent** — 2 of 6 direct attempts tonight (E1, W1-attempt-1) died at +10 s post-peer-creation and silently relayed. Any throughput cell that skips the `lanes ready` + `relay standby expired unused` assertion may be measuring the 44.7 Mbps relay. (Also: the *newest* API share is `relay_only:true` — unusable for direct cells; the E17/E18 share was used, 4 downloads consumed, 60/500.)

## Caveats

- Loss is at **DTLS-record/SCTP-packet granularity** (1 record = 1 UDP datagram on this flow; mean data frame 1298 B): datagrams lost in flight (seq never arrived) = per-datagram network loss on the data direction; retransmissions ride *new* seqs and do not inflate it.
- n=2 EAST, n=1 WEST (timebox). E2's SACK seqs 0–4773 predate the nominated pair (absent, not lost). W1 attempt-1 (relayed) discarded per instruction §4.
- Capture-side drops would *inflate* missing; none observed (byte totals reconcile with the 754 MiB payload to <0.1%; tshark/tcpdump idle).
- **Do not use SNMP UDP counters on this rig** (undercount ~50–3000× under GRO/GSO at these rates, measured both endpoints). NIC `tx_packets` on VERSA is wire-exact but home-host background (~1.1k fps) forbids 10⁻⁴ resolution.
- Lab↔field asymmetry (§2) is a hypothesis, not a diagnosis; adjudicating it (bursty vs random loss shaping, cf. E7) would be a new lab experiment.
- No container recreated; no agent env changed; `sb-run` = `sb-agent:pristine`, `UI_PORT=7879`, `SB_SCTP_CA_STEP=32768`, no `SB_SCTP_MIN_CWND` throughout (verified by `docker inspect` at start and end). Production, v2 stack, DNS/ACME untouched; no instance state changed; no git command run; no IPs/hostnames/codes in this file.

## Cleanup / installed tooling disclosure

- **Installed:** `tshark` 4.2.2 (apt) on CLIENT-EAST; Wireshark 4.6.8 (Homebrew) on the lab Mac (analysis only). **Pre-existing:** tcpdump on both VMs.
- Deleted on VMs: all pcaps, fields dumps, `SSLKEYLOGFILE` keylogs (DTLS/TLS secrets — deleted everywhere incl. the Mac), analysis scripts. Both VMs verified zero `node`/`chrome`/`Xvfb`/`xvfb-run` at close.
- Mac `/tmp` retains raw pcap copies for the record: `exp19-e3.pcapng` (981,677,124 B), `exp19-w1.pcap` (943,944,387 B); E2's pcap deleted on VM after its seq pass (numbers above from the on-VM pass; no local copy).

## Artifacts

- This file: `.worktrees/benchdirect/docs/superpowers/spikes/results/2026-09-18-exp19-in-flow-loss.md`
- Mac: `/tmp/exp19-e3.pcapng`, `/tmp/exp19-w1.pcap`
- VMs (deleted): `/tmp/exp19-e2.pcapng`, `/tmp/exp19-e3.pcapng` (EAST), `/tmp/exp19-w1.pcap` (WEST), driver logs `/tmp/cell-e19-*.log`, keylogs
