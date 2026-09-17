# Phase 4a — M6 acceptance environment record (Task 37 template)

Status: **PENDING — template only.** No M6 dark topology (second Hetzner VM,
relay DNS, private mTLS sync) was provisioned when this file was created. Every
field below is an explicit **PENDING** placeholder; it must be filled from an
actual harness run or the provider console — **never invented or extrapolated**.
A field stays PENDING until the run that observes it has happened.

This is the human-readable companion to the machine evidence. The harness
(`scripts/live-phase4a.sh`) writes, per run:

```
docs/operations/evidence/runs/<run-id>/
  summary.txt                gate -> result table + verdict
  manifest.txt               gate=RESULT|owner lines
  environment-facts.txt      the exact keys this document lists
  environment-facts.observed raw observed facts (pre-template)
  gate-<name>.txt            one immutable-ish record per gate
                             (timestamp, git sha, commands, sanitised output,
                              per-check PASS/FAIL, result, content sha256)
```

Copy measured values from `environment-facts.txt`; do not retype from memory.
Never paste secrets, share codes, `jti`, credentials, cookies or request bodies
into this record.

---

## 1. Run metadata

| Field | Value |
|---|---|
| Harness run id | **PENDING** |
| UTC of run | **PENDING** |
| Harness git sha (`scripts/live-phase4a.sh`) | **PENDING** |
| Harness host platform | **PENDING** |
| Gate verdict (GREEN / PARTIAL / RED) | **PENDING** |
| Operator | **PENDING** |

## 2. Hosts

| Field | Control VM | Relay VM |
|---|---|---|
| SSH target | **PENDING** | **PENDING** |
| Public host:port | **PENDING** | **PENDING** |
| Hetzner `instance-id` | **PENDING** | **PENDING** |
| Hetzner region / availability zone | **PENDING** | **PENDING** |
| Private IPv4 (same private network) | **PENDING** | **PENDING** |
| OS / kernel | **PENDING** | **PENDING** |
| Relevant systemd unit(s) | `sharebridge.service` **PENDING** | `sharebridge-relay-gateway.service` / `sharebridge-relay-frps.service` **PENDING** |
| Env file | **PENDING** | **PENDING** |
| Distinct from control (`instance-id` proof) | — | **PENDING** |

Home side (the harness host is the home Mac):

| Field | Value |
|---|---|
| Home Mac platform | **PENDING** |
| Agent process match (`pgrep -f` pre-filter / exact name) | **PENDING** / `frpc` |
| Agent version (git sha or image digest) | **PENDING** |
| Real Immich URL + observed `/api/server/ping` | **PENDING** |

## 3. Versions and digests

| Field | Value |
|---|---|
| Control build git sha | **PENDING** |
| Agent build git sha / image digest | **PENDING** |
| Relay gateway build git sha | **PENDING** |
| frps version (pinned) | **PENDING** |
| frps release tarball SHA-256 | **PENDING** |
| Installed `frps` executable SHA-256 | **PENDING** |
| Go toolchain version | **PENDING** |
| Tunnel transport certificate (serial / SPKI SHA-256 / SAN) | **PENDING** |
| Sync CA certificate SHA-256 | **PENDING** |
| Gateway sync client certificate (serial / SPKI SHA-256 / SAN) | **PENDING** |

## 4. DNS zone facts (per-name, not a zone-wide claim)

| Field | Value |
|---|---|
| Content base domain | **PENDING** |
| Enrolled test namespace (`sbXXXXXXXX`) | **PENDING** |
| Relay wildcard family | **PENDING** |
| Random-child wildcard synthesis probe | **PENDING** |
| `<relay-tunnel-host>` A | **PENDING** |
| `<relay-tunnel-host>` AAAA (empty or a valid IPv6 literal) | **PENDING** |
| `<relay-tunnel-host>` HTTPS / SVCB (must be empty) | **PENDING** |
| `ech=` parameter present anywhere for a relay name (must be absent) | **PENDING** |
| CAA context (diagnostic only; §3.2 is a separate release prerequisite) | **PENDING** |

Scope reminder: the harness queries the relay wildcard, one random child label
and the tunnel host. It does **not** perform an AXFR / zone dump, so this is a
per-name DNS-only/ECH claim, not a zone-wide absence proof.

## 5. Public relay firewall

| Field | Value |
|---|---|
| On-box nftables public allowlist (`inet sharebridge_relay`) | **PENDING** (must be exactly `443` + transport) |
| Input chain policy | **PENDING** (must be `drop`) |
| External TCP probe: `443` | **PENDING** |
| External TCP probe: transport port | **PENDING** |
| External TCP probe: forbidden ports `9001 9101 9102 7500 10000 10099` | **PENDING** (all must be closed) |
| Non-loopback UDP listeners on the relay VM | **PENDING** (must be none) |

## 6. Private mTLS control sync, health and restart ordering

| Field | Value |
|---|---|
| Control `CONTROL_SYNC_*` material present (all-or-nothing) | **PENDING** |
| `CONTROL_SYNC_BIND_ADDR` (private/loopback only) | **PENDING** |
| Gateway `SHAREBRIDGE_CONTROL_SYNC_URL` / `_SAN` / `_CA_FILE` | **PENDING** |
| Gateway sync cert/key + `LoadCredential=` entries | **PENDING** |
| `SHAREBRIDGE_GATEWAY_NAMESPACE` (`sb` + 8 hex) | **PENDING** |
| mTLS handshake accepted with the gateway client certificate | **PENDING** |
| mTLS enforced: handshake without a client certificate rejected | **PENDING** |
| Gateway `/healthz` `route_ready` | **PENDING** (must be `true`) |
| Gateway `/healthz` `frps_process_healthy` | **PENDING** |
| `sharebridge-relay-frps.service` `After=`/`Wants=` include the gateway | **PENDING** |
| Neither unit `BindsTo=`/`PartOf=` couples the pair; gateway does not `After=` frps | **PENDING** |
| Guarded live restart drill (only when `LIVE_PHASE4A_ALLOW_RESTART=1`) | **PENDING** |
| `acceptance_09` frps-restart recovery (only when `LIVE_PHASE4A_ALLOW_RESTART=1`) | **PENDING** (must be a measured recovery within `LIVE_PHASE4A_RECOVERY_BOUND_S`; offline observed within `LIVE_PHASE4A_TUNNEL_OFFLINE_BOUND_S`) |
| `acceptance_09` baseline relay bytes | **PENDING** |
| `acceptance_09` tunnel-offline seconds | **PENDING** |
| `acceptance_09` recovery seconds (measured from the frps restart) | **PENDING** |
| `acceptance_09` new frpc child identity (before → after, `PID:STARTTIME`) | **PENDING** |
| `acceptance_09` frpc child still alive immediately after the restart (identity unchanged) | **PENDING** |
| `acceptance_09` new tunnel session (`sharebridge_relay_tunnel_reconnects_total` before → after, corroboration) | **PENDING** |
| `acceptance_09` agent-specific fresh session (frps `new proxy [<proxy>] type [tcp] success` count before → after, for `LIVE_PHASE4A_AGENT_PROXY_NAME`) | **PENDING** |
| `acceptance_09` exact restart target confirmed (`LIVE_PHASE4A_FRPS_RESTART_CONFIRM` == `<host>`\|<unit>) | **PENDING** |
| `acceptance_09` recovery seconds measured after all stages (must be ≤ `LIVE_PHASE4A_RECOVERY_BOUND_S`) | **PENDING** |

`acceptance_09_restart_recovery` inputs (never values): `LIVE_PHASE4A_CONTROL_BASE_URL`,
`LIVE_PHASE4A_SHARE_CODE`, `LIVE_PHASE4A_GATEWAY_METRICS_URL` (or
`LIVE_PHASE4A_RELAY_HOST` + `LIVE_PHASE4A_GATEWAY_METRICS_ADDR`), `LIVE_PHASE4A_FRPS_SSH_HOST`
(default `LIVE_PHASE4A_RELAY_HOST`), `LIVE_PHASE4A_AGENT_SSH_HOST` (optional),
`LIVE_PHASE4A_FRPS_RESTART_CONFIRM` (exact `<host>|<unit>`), `LIVE_PHASE4A_AGENT_PROXY_NAME`
(default `sb-<LIVE_PHASE4A_NAMESPACE>`), the frps journal source
(`LIVE_PHASE4A_FRPS_JOURNAL_FILE`, else `journalctl -u LIVE_PHASE4A_FRPS_UNIT` over SSH), the
frpc identity configuration (`LIVE_PHASE4A_FRPC_PID_MATCH` pre-filter,
`LIVE_PHASE4A_FRPC_PID_NAME`, optional `LIVE_PHASE4A_FRPC_IDENTITY_CMD`),
`LIVE_PHASE4A_RECOVERY_BOUND_S` and `LIVE_PHASE4A_TUNNEL_OFFLINE_BOUND_S`. The gate never
restarts the agent's frpc child; it restarts frps only under `LIVE_PHASE4A_ALLOW_RESTART=1`
and only when the restart target is exactly confirmed.

## 7. Dark posture (§20 step 1)

| Field | Value |
|---|---|
| Observed `RELAY_SELECTION_ENABLED` in the control deployment | **PENDING** (must be observed as `false`; absent/unobservable is a FAIL, `true` is a FAIL) |

A missing topology must never be masked by an enabled flag. The harness records
the observed value from the deployed configuration — the running control
process environment, an explicit unit `Environment=`, or the env file the unit
loads (highest authority first). An absent or unreadable value fails the check
rather than assuming the code default and the dark posture.

## 8. Gate evidence index

Fill each result from the run's `summary.txt` and link the per-gate evidence
file. An unrun gate is never a pass.

| Gate | Owner | Result | Evidence |
|---|---|---|---|
| `gate_23_9_dark_topology` | Task 37 | **PENDING** | **PENDING** |
| `acceptance_01_owner_hairpin` | Task 38 | **PENDING** | **PENDING** |
| `acceptance_04_interstitial_blackhole` | Task 38 | **PENDING** | **PENDING** |
| `acceptance_02_cellular_relay_video` | Task 39 | **PENDING** | **PENDING** |
| `acceptance_11_content_parity` | Task 39 | **PENDING** | **PENDING** |
| `acceptance_03_relay_only` | Task 40 | **PENDING** | **PENDING** |
| `l4_no_plaintext_capture` | Task 41 | **PENDING** | **PENDING** |
| `acceptance_06_no_mapper_enrollment` | Task 42 | **PENDING** | **PENDING** |
| `acceptance_12_stun_mismatch` | Task 42 | **PENDING** | **PENDING** |
| `acceptance_13_stun_cadence_cold_budget` | Task 42 | **PENDING** | **PENDING** |
| `acceptance_07_no_relay_open_signal` | Task 43 | **PENDING** | **PENDING** |
| `acceptance_08_exact_routing` | Task 43 | **PENDING** | **PENDING** |
| `acceptance_09_restart_recovery` | Task 43 | **PENDING** | **PENDING** |
| `acceptance_10_lockdown` | Task 43 | **PENDING** | **PENDING** |
| `acceptance_15_heartbeat_tunnel_dns` | Task 43 | **PENDING** | **PENDING** |
| `release_go_no_go_rollback` | Task 44 | **PENDING** | **PENDING** |

§19 #14 (no-JS/CSP fallback) is a Task 24 hermetic browser gate and is not part
of this harness.

## 9. M6 ordering and operator involvement

1. **Task 37** — deploy the dark separate-VM topology, then prove
   `gate_23_9_dark_topology`. Requires the operator: a second Hetzner VM in the
   same region/private network, relay DNS for the test namespace, transport
   certificate trust, and the control/gateway sync mTLS material.
2. **Tasks 38–43** — live acceptance cases; each replaces one placeholder gate
   body in `scripts/live-phase4a.sh`. Tasks 38–40, 42 and 43 need the user
   (real browsers/Safari, a phone on cellular, the real Immich share, router
   NATs); Task 41 needs the relay VM only.
3. **Task 44** — final go/no-go and the rollback drill; needs all of the above
   plus the user's release decision.

Run the harness from the repository root; the full procedure, the environment
variable table and each gate's proof are documented in
`docs/operations/phase4a-relay.md` §13.
