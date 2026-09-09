# Phase 4a — STUN Real-NAT Receipt Gate Evidence (§23.5, BLOCKING)

Status: **UNEXECUTED — template.** This file is the §23.5 evidence record for
plan Task 25 (`docs/superpowers/plans/2026-09-03-phase4a-relay-mvp.md`). The
local pre-flight (in-process, loopback standing in for the NAT) is automated
by `scripts/stun-nat-gate.sh --target local`; the real-NAT cases below are
**user-assisted** runs against a deployed control. Execution discipline:

- Never record secrets. The script prints receipts, transaction IDs and
  challenge IDs only as SHA-256 hashes; paste exactly those.
- Failure of any case under representative NAT is **NO-GO** for automatic
  relay fallback (§23.5). Do not substitute an agent-reported IP for the
  control-observed source.
- A partial run (some cases skipped) is not a GO. The §23.5 gate requires the
  full seven-case matrix: the two happy-path cases must succeed through real
  NATs and every negative case must fail closed.

## Run metadata

| Field | Value |
|---|---|
| Date/time (start — end, timezone) | — |
| Control version (git sha of deployed build) | — |
| Agent version (git sha of deployed build) | — |
| STUN listener host (public `host:3478`) | — |
| Agent API key hash (SHA-256, for correlation only) | — |
| Local pre-flight (`scripts/stun-nat-gate.sh --target local`) | — (must be all-PASS before any remote run) |
| Operator | — |

## Case: owner-router-nat

- Date/time: —
- NAT description (router model, firmware, NAT type if known): —
- Listener version+host: —
- Observed source IP (public, control-side mapping): —
- Receipt hash (SHA-256): —
- Transaction hash (SHA-256): —
- Rechallenge acceptance proof (§10.2 rechallenge observed at +4m–4m15s): —
- Result (PASS/FAIL): —
- Notes (script command line, verbatim `STUN_GATE_EVIDENCE` line): —

## Case: phone-hotspot

- Date/time: —
- NAT description (phone model, iOS version, carrier, USB/Wi-Fi tether): —
- Listener version+host: —
- Observed source IP (public; CGNAT is expected and is NOT a failure here —
  the receipt must still verify; §10.3 classification of CGNAT for direct
  eligibility is Task 19/20 policy, not a listener rejection): —
- Receipt hash (SHA-256): —
- Transaction hash (SHA-256): —
- Rechallenge acceptance proof: —
- Result (PASS/FAIL): —
- Notes: —

## Case: blocked-udp

- Date/time: —
- Block method (firewall toggle / pf rule / unroutable target): —
- Listener version+host: —
- Observed behavior (no response expected): —
- Result (PASS/FAIL — PASS = no receipt, fail closed): —
- Recovery check (post-unblock challenge succeeds): —
- Notes: —

## Case: spoof

- Date/time: —
- Listener version+host: —
- Observed behavior (wrong-integrity request 401 + credential burned; honest
  exchange then fails closed): —
- Result (PASS/FAIL — PASS = no observation manufacturable): —
- Notes: —

## Case: mismatched-egress

- Date/time: —
- Egress setup (VPN provider/client, expected surface IP without VPN): —
- Listener version+host: —
- Observed source IP (public, via VPN): —
- Required surface IP (`STUN_GATE_EXPECTED_PUBLIC_IP`): —
- Result (PASS/FAIL — PASS = observed ≠ required, i.e. the §10.3 exact-match
  comparison fails and the flow stops before the public reachability probe): —
- Notes: —

## Case: receipt-replay

- Date/time: —
- Listener version+host: —
- Observed behavior (duplicate `stun_result` rejected; WS healthy; rechallenge
  still armed and on time if the acceptance proof ran): —
- Result (PASS/FAIL): —
- Notes: —

## Case: expired-challenge

- Date/time: —
- Listener version+host: —
- Observed behavior (Binding after the 60 s TTL answered 401, no receipt): —
- Result (PASS/FAIL): —
- Notes: —

## §23.5 GO/NO-GO

**Decision line (fill one):** GO / NO-GO — automatic relay fallback direct
probe path is enabled only on GO.

- Decision date/time: —
- Deciding evidence: the seven case results above (all PASS required for GO).
- Any NO-GO reason: —

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
