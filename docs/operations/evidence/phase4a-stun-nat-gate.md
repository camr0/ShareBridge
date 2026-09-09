# Phase 4a — STUN Real-NAT Receipt Gate Evidence (§23.5, BLOCKING)

Status: **EXECUTED 2026-09-09 — 7/7 PASS — GO.** The full seven-case matrix ran
against the deployed test control through two genuinely distinct NAT surfaces
(home router WAN + iPhone cellular hotspot). All secrets stay out of this
record: receipts, transaction IDs and challenge IDs appear only as SHA-256
hashes pasted verbatim from `STUN_GATE_EVIDENCE` lines.

## Run metadata

| Field | Value |
|---|---|
| Date/time (start — end, timezone) | 2026-09-09 15:26 — 16:18 EDT (UTC-4) |
| Control version (git sha of deployed build) | `3819bd07` (binaries built from worktree state `1691a4fc` + the ETXTBSY stop-before-copy fix, committed immediately after the deploy) |
| Agent version (git sha of deployed build) | `f8869d05` (image `sb-agent:phase4a-test`, built on versa from the phase-4a agent tree) |
| STUN listener host (public `host:3478`) | `178.156.174.47:3478` (collocated §4.6 test deployment, `relay-test.sharebridgeusercontent.com`) |
| Agent API key hash (SHA-256, for correlation only) | `4c0909d8a7711be3634cde1aa07add1fbfd5832c20c6ae134f9ca7f912fcfdd8` |
| Local pre-flight (`scripts/stun-nat-gate.sh --target local`) | PASS 7/7, exit 0 (at `9e91e4ac`; re-verified by the fix re-review) |
| Operator | ali, driven by the phase-4a controller session |

## Case: owner-router-nat

- Date/time: 2026-09-09 ~15:27–15:36 EDT (run 2)
- NAT description (router model, firmware, NAT type if known): owner home router (Comcast WAN `173.54.233.213`); model/firmware not recorded
- Listener version+host: deployed control `3819bd07`, `178.156.174.47:3478`
- Observed source IP (public, control-side mapping): `173.54.233.213:61599`
- Receipt hash (SHA-256): `151162272c11b7a057b2431028497e78febffe912ef45d0cf221ca6fc1ed2a65`
- Transaction hash (SHA-256): `96d770d51bbfd93fdfe0916cca012454acd125f12982c1ebbff20496c752af78`
- Rechallenge acceptance proof (§10.2 rechallenge observed at +4m–4m15s): rechallenge at +4m11s — control accepted the observation
- Result (PASS/FAIL): **PASS** — "real challenge over the deployed control WS; integrity-verified receipt through the NAT path; stun_result echoed; §10.2 rechallenge at +4m11s proves control accepted the observation"
- Notes: command line (per run): `--target remote --server ws://178.156.174.47:8080 --stun-addr 178.156.174.47:3478 --expected-public-ip 173.54.233.213` with `STUN_GATE_CERT_FINGERPRINT=<enrolled row>`; cert fast path used, no CSR issuance.

## Case: phone-hotspot

- Date/time: 2026-09-09 16:11–16:17 EDT (run 4; an earlier protocol pass in run 2 rode the home NAT and is superseded by this honest cellular run)
- NAT description (phone model, iOS version, carrier, USB/Wi-Fi tether): iPhone personal hotspot, Wi-Fi tether; carrier/iOS version not recorded; egress `174.237.223.169` (CGNAT expected, not a failure)
- Listener version+host: deployed control `3819bd07`, `178.156.174.47:3478`
- Observed source IP (public; CGNAT is expected and is NOT a failure here — the receipt must still verify; §10.3 classification of CGNAT for direct eligibility is Task 19/20 policy, not a listener rejection): `174.237.223.169:62560`
- Receipt hash (SHA-256): `7875b377d35e47099c504e8efc8471b80643ce8d3593982267a7b66a193fac56`
- Transaction hash (SHA-256): `b2dcd6fa5ff9d7fa4b10393b3ffc2f138f0279f81f4b210475ab17e654c6bcda`
- Rechallenge acceptance proof: rechallenge at +4m14s — control accepted the observation
- Result (PASS/FAIL): **PASS**
- Notes: distinct surface from case 1 verified (`174.237.223.169` ≠ `173.54.233.213`).

## Case: blocked-udp

- Date/time: 2026-09-09 ~16:00 EDT
- Block method (firewall toggle / pf rule / unroutable target): macOS pf, `block drop out proto udp from any to 178.156.174.47 port 3478` (single-line ruleset, pf enabled for the case, disabled immediately after — DROP/black-hole semantics, not REJECT)
- Listener version+host: deployed control `3819bd07`, `178.156.174.47:3478`
- Observed behavior (no response expected): Binding request dropped at the local kernel; no response within the 4 s window; nothing echoed wire-side — `STUN_GATE_EVIDENCE blocked-udp receipt_sha256=none`
- Result (PASS/FAIL — PASS = no receipt, fail closed): **PASS**
- Recovery check (post-unblock challenge succeeds): UDP path recovery on the home surface is evidenced by the run 2 baseline (pre-toggle) and the local normative run's recovery semantics; a dedicated post-unblock same-surface rerun was not scheduled (not §23.5-blocking; pf was fully disabled, restoring the pre-toggle state)
- Notes: challenge consumption by parallel flows explicitly checked by the case (401 on the blocked path would have flagged it) — none occurred.

## Case: spoof

- Date/time: 2026-09-09 ~15:30 EDT (run 2)
- Listener version+host: deployed control `3819bd07`, `178.156.174.47:3478`
- Observed behavior (wrong-integrity request 401 + credential burned; honest exchange then fails closed): attacker_source `192.168.1.224:54895`, honest_source `192.168.1.224:52266`, `receipt_sha256=none` — "wrong-integrity request through the real path: 401 + credential burned; honest exchange failed closed; nothing echoed"
- Result (PASS/FAIL — PASS = no observation manufacturable): **PASS**
- Notes: run from the home LAN (source is the RFC1918 host address; the listener's 401 is keyed on the one-use credential, not source IP).

## Case: mismatched-egress

- Date/time: 2026-09-09 16:11–16:17 EDT (run 4)
- Egress setup (VPN provider/client, expected surface IP without VPN): iPhone cellular hotspot surface (not a VPN client; the required "different surface" mechanism — same §10.3 code path); expected surface without the switch: `173.54.233.213` (home WAN)
- Listener version+host: deployed control `3819bd07`, `178.156.174.47:3478`
- Observed source IP (public, via VPN): `174.237.223.169:62945`
- Required surface IP (`STUN_GATE_EXPECTED_PUBLIC_IP`): `173.54.233.213`
- Result (PASS/FAIL — PASS = observed ≠ required, i.e. the §10.3 exact-match comparison fails and the flow stops before the public reachability probe): **PASS** — "mismatched egress observable: mapped source differs from the required surface; §10.3 exact-match fails and stops before the public probe"
- Notes: receipt hash `439147ebcbf69297100bf3d7be7cf58a4f3554259608729d4fbcfb065126380b` (the UDP receipt itself verifies — the mismatch is refused at the policy layer, exactly as specified).

## Case: receipt-replay

- Date/time: 2026-09-09 15:37–15:44 EDT (run 3)
- Listener version+host: deployed control `3819bd07`, `178.156.174.47:3478`
- Observed behavior (duplicate `stun_result` rejected; WS healthy; rechallenge still armed and on time if the acceptance proof ran): `replay_outcome=duplicate_sent_connection_healthy`; §10.2 rechallenge at +4m11s proves acceptance and intact scheduling after the replay
- Result (PASS/FAIL): **PASS**
- Notes: control-side single-use rejection is normatively asserted by the local run (remote rejection is silent by design); this run proves the real WS tolerated the duplicate and acceptance/scheduling stayed healthy.

## Case: expired-challenge

- Date/time: 2026-09-09 15:37–15:44 EDT (run 3)
- Listener version+host: deployed control `3819bd07`, `178.156.174.47:3478`
- Observed behavior (Binding after the 60 s TTL answered 401, no receipt): `late_request_outcome=401_binding_error`, `receipt_sha256=none`
- Result (PASS/FAIL): **PASS**
- Notes: listener-side expiry enforced under the real deployed controller.

## §23.5 GO/NO-GO

**Decision line (fill one): GO** — automatic relay fallback direct probe path is enabled only on GO.

- Decision date/time: 2026-09-09 16:20 EDT
- Deciding evidence: the seven case results above (all PASS required for GO) — 7/7 PASS across runs 2–4 on the deployed real control, with §10.2 rechallenge acceptance proofs (+4m11s / +4m14s / +4m11s) on all three acceptance-proven cases, through two distinct NAT surfaces.
- Any NO-GO reason: —

### Operator notes (discovered during execution)

1. **The gate must not share an API key with a live deployed agent.** R1's authoritative session-replacement fencing closes the gate's WebSocket whenever the deployed agent daemon reconnects with the same key (first full attempt: 0/7 with `control connection closed`). Stop the deployed test agent — or provision a separate key — for gate runs. (The test agent was stopped for runs 2–4.)
2. **Full-matrix runs need a larger go-test timeout.** The two happy-path cases each wait ~4m15s for the §10.2 rechallenge; `go test`'s default 10m budget kills the trailing cases (run 2: receipt-replay/expired-challenge MISSING with `panic: test timed out after 10m0s`). Use `STUN_GATE_GOFLAGS="-timeout 30m"` for default (no `--case`) remote runs.
3. `--case`-scoped runs avoid the rechallenge waits entirely for the negative cases; per-case invocations were used here for the network toggles.

---

## EXECUTION RUNBOOK (user-assisted; plan Task 25 Steps 3–4)

Everything below happens from a checkout of the `phase-4a` branch on the Mac
that sits behind the NAT under test. The gate script drives the deployed
control as a minimal test agent over the real authenticated WebSocket; the
API key is passed by environment variable only (never a flag — flags leak via
`ps` and shell history).

### 0. Preconditions (once per deployment)

1. Deploy control with the STUN listener enabled on UDP 3478 and the public
   advertise address configured (the `stun_challenge` `server` field must be
   reachable from the networks under test). Record the control git sha.
2. Provision an agent API key for the gate and record its SHA-256
   (`printf '%s' "$KEY" | shasum -a 256`) — never the key itself.
3. Pre-flight MUST be green first:

   ```sh
   scripts/stun-nat-gate.sh --target local
   ```

   All seven cases PASS, exit 0. If not, stop — the deployment is not the
   thing under test yet.
4. Recommended: find the enrolled agent's certificate fingerprint in the
   control data store (the `agents` row, `cert_fingerprint` field) and export
   it. Without it the driver performs a REAL certificate issuance against the
   deployed control's CA for its test agent, which is slower and consumes
   issuer rate limits:

   ```sh
   export STUN_GATE_CERT_FINGERPRINT=<value of agents.cert_fingerprint>
   ```

5. Export the key and server for the session:

   ```sh
   export STUN_GATE_API_KEY=<agent api key>
   ```

### 1. Owner-router NAT (Mac on the home/owner router)

```sh
scripts/stun-nat-gate.sh --target remote --server wss://<control-host> --case owner-router-nat
```

- Takes ~5 minutes: the exchange itself is seconds; the acceptance proof
  waits for the §10.2 rechallenge (~4m15s). Use `--no-wait-rechallenge` only
  for quick connectivity checks — the recorded GO evidence should include the
  rechallenge proof.
- PASS = integrity-verified receipt through the NAT + `stun_result` accepted
  (proven by the rechallenge, not assumed).
- Record in the template: date/time, router model/firmware, observed public
  source (should equal the router's WAN IP), receipt hash, transaction hash.

### 2. Mac on iPhone hotspot

1. Connect the Mac to the iPhone personal hotspot (Wi-Fi or USB; record
   which).
2. Run the same gate for the hotspot case:

   ```sh
   scripts/stun-nat-gate.sh --target remote --server wss://<control-host> --case phone-hotspot
   ```

3. Record: phone model/iOS version, carrier, observed public IP (CGNAT is
   expected — record it; it is not a failure for this gate), hashes.

### 3. blocked-UDP

Either mechanism is acceptable; record which:

- Firewall toggle: enable the macOS packet filter and drop outbound UDP 3478:

  ```sh
  echo "block drop out proto udp to any port 3478" | sudo pfctl -f -
  sudo pfctl -E   # enable pf if not already running
  ```

- Or an unroutable target instead of a firewall rule:

  ```sh
  scripts/stun-nat-gate.sh --target remote --server wss://<control-host> \
    --case blocked-udp --stun-addr 203.0.113.1:3478
  ```

Run:

```sh
scripts/stun-nat-gate.sh --target remote --server wss://<control-host> --case blocked-udp
```

PASS = no response, fail closed. Afterwards disable the rule
(`sudo pfctl -d` / restore the previous pf ruleset) and, recommended, rerun
case 1 to record recovery on the unblocked path.

### 4. VPN mismatched-egress

1. Without the VPN, note the expected surface IP (what the agent reports as
   its public endpoint — the router WAN IP from case 1).
2. Enable the VPN client under test (record provider/client/version).
3. Run:

   ```sh
   STUN_GATE_EXPECTED_PUBLIC_IP=<expected-surface-ip> \
   scripts/stun-nat-gate.sh --target remote --server wss://<control-host> --case mismatched-egress
   ```

PASS = the UDP exchange still verifies a receipt but its mapped source
differs from the expected surface, i.e. the §10.3 exact-match comparison
fails and direct selection stops before the public probe. (The probe stop
itself is proven against the real listener code by the local pre-flight;
this run proves the mismatch is observable through the real egress path.)

### 5. Spoof, receipt-replay, expired-challenge

These are protocol/acceptance cases; run them once per deployment from any
network (record which):

```sh
scripts/stun-nat-gate.sh --target remote --server wss://<control-host> \
  --case spoof,receipt-replay,expired-challenge
```

(`receipt-replay` includes the ~4.5-minute acceptance proof by default.)
PASS criteria: spoof — 401 + burned one-use credential, honest path fails
closed; receipt-replay — duplicate `stun_result` rejected, connection
healthy, scheduling intact; expired-challenge — Binding after 60 s TTL
answered 401, no receipt.

### 6. Finish

1. A full default run (no `--case`) per network condition is an alternative
   to the per-case invocations above; both produce the same evidence lines.
2. Paste each case's `STUN_GATE_EVIDENCE` lines and table rows from the
   script output into the sections above, fill the metadata and the
   GO/NO-GO line.
3. Evidence policy reminders: hashes only; never paste the API key, the
   packed challenge, or any raw receipt/secret; the script never prints
   them and neither should the operator.
