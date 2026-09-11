# Phase 4a relay VM — operations runbook

Zero-context runbook for the **production separate-VM relay topology** (spec
§4.6, §6, §17). It covers provisioning, hardening, restart ordering, the DNS
audit, the complete operator environment surface, and the known open items.

Scope: the relay VM only. The collocated DEV/TEST VPS (control + gateway +
frps on one box) is a different, test-only deployment described in
[`phase4a-test-vps-deploy.md`](phase4a-test-vps-deploy.md); production
acceptance uses the separate VM described here (Task 37 is the live dark
deployment that exercises this runbook).

---

## 1. Topology and ports

One relay VM in the same region/private network as control:

| Process (systemd unit) | Public | Private / loopback |
|---|---|---|
| `sharebridge-relay-gateway.service` | `443/tcp` (SNI L4 passthrough) | `127.0.0.1:9001` FRP plugin, `127.0.0.1:9101` metrics/health |
| `sharebridge-relay-frps.service` | `<transport>/tcp`, pinned to `7000` | `127.0.0.1:10000-10099` agent proxy ports |

`relay/deploy/firewall.nft` allows **only** `443/tcp` and the pinned transport
port. The proxy range, plugin endpoint, metrics/health and any FRP dashboard
are dropped; they bind to numeric loopback in the binaries/service config and
are never reachable from off-box. The relay VM runs no public UDP listener.
The pinned `bindPort` in `relay/config/frps.toml` and the firewall allowlist
are cross-checked by `relay/deploy/deploy_test.sh`, so they cannot drift.

Control↔gateway route/presence sync is an **outbound mTLS** connection from the
gateway to control; no inbound control port is opened on the relay VM.

---

## 2. Provisioning

```bash
# One-time (or per host) dedicated FRP transport certificate:
relay/deploy/install.sh --generate-transport-cert <relay-tunnel-host>

# Install and start both hardened services:
relay/deploy/install.sh \
  --tunnel-host <relay-tunnel-host> \
  --sync-url https://<control-sync-endpoint> --sync-san <control-sync-san> \
  --namespace sb0123abcd \
  --sync-ca control-ca.crt --sync-cert gateway-sync.crt --sync-key gateway-sync.key \
  --transport-cert tunnel-server.crt --transport-key tunnel-server.key

# Secret-safe plan with no changes and no secret output:
relay/deploy/install.sh --dry-run --tunnel-host <relay-tunnel-host> \
  --sync-url https://<endpoint> --sync-san <san> --namespace sb0123abcd \
  --sync-ca c --sync-cert c --sync-key k --transport-cert c --transport-key k
```

Installer requirements, in the environment (never on the command line and never
echoed):

- `SHAREBRIDGE_FRP_PLUGIN_SHARED_SECRET` — 16-128 lowercase hex; shared by
  frps and the gateway plugin.
- `SHAREBRIDGE_CONTROL_RELAY_PUBLIC_KEY` — 64 lowercase hex; the Ed25519
  public key the gateway verifies relay credentials with.

**What the relay VM must never hold:** an agent content certificate/key, a
`sharebridgeusercontent.com` DNS/ACME credential, a Cloudflare token, a share
code, or any cookie/path/header. The dedicated FRP transport certificate is a
transport credential only; it is not a content origin certificate. Remote
proof of this is part of the Task 37 dark-topology gate.

`install.sh` uses the committed checksum pins (`relay/frp/manifest.json` via
`relay/scripts/fetch-frp.sh`): the release tarball digest and the extracted
`frps` executable digest (`frps_sha256`). It never downloads `latest`, never
trusts a cache marker as proof, and fails closed on a digest mismatch; the
installed copy is re-hashed after the copy. The published transport
certificate is copied to agents as their `SHAREBRIDGE_RELAY_CA_FILE` before
enrollment.

---

## 3. Hardening inventory

Every claim below is enforced by a specific check in
`relay/deploy/deploy_test.sh`; run `bash relay/deploy/deploy_test.sh` before
and after any change to the units, firewall, installer, or this document.

| Property | Enforcement |
|---|---|
| Distinct unprivileged users (`sharebridge-relay-gateway`, `sharebridge-relay-frps`), never root/`nobody` | `User=`/`Group=` in both units; `useradd --system` in `install.sh` (A2) |
| Secrets and config `root:root 0600` | `install_root_secret` helper + per-file calls for `frps.toml`, `gateway.env`, `frps.env`, `tunnel-server.key`, `gateway-sync.key`, `control-ca.crt` (A3) |
| No plaintext secret in a unit or doc | units carry no secret value, no ECH/ACME token; no committed PEM private-key block (A3) |
| Read-only filesystem except explicit state/run dirs | `ProtectSystem=strict`, `ProtectHome=true`, `PrivateTmp=true`; `ReadWritePaths` allowlisted to `/var/lib/sharebridge-relay-*` and `/run/sharebridge-relay-*`; `StateDirectory`/`RuntimeDirectory` declared (A4) |
| Kernel/filesystem sandbox | `NoNewPrivileges`, `ProtectKernel{Tunables,Modules,Logs}`, `ProtectControlGroups`, `ProtectClock`, `ProtectHostname`, `ProtectProc=invisible`, `ProcSubset=pid`, `RestrictAddressFamilies`, `RestrictNamespaces`, `RestrictRealtime`, `RestrictSUIDSGID`, `LockPersonality`, `MemoryDenyWriteExecute`, `SystemCallFilter=@system-service`, `CapabilityBoundingSet` (A4) |
| Least privilege for `:443` | gateway has exactly `AmbientCapabilities=CAP_NET_BIND_SERVICE` / `CapabilityBoundingSet=CAP_NET_BIND_SERVICE`; frps holds none; neither may hold `CAP_SYS_ADMIN`/`CAP_NET_ADMIN` (A4) |
| Bounded descriptors and memory | `LimitNOFILE=65536` (≥ the §14 global stream ceiling), `MemoryMax=512M` gateway / `256M` frps (A5) |
| Restart and log-rate policy | `Restart=always`, `RestartSec=2`, `StartLimitIntervalSec`/`StartLimitBurst`, `LogRateLimitIntervalSec=30`, `LogRateLimitBurst=200` (A6) |
| frps after the gateway plugin | `After=`/`Wants=sharebridge-relay-gateway.service` on frps; no `BindsTo=`/`PartOf=` coupling the gateway to frps (A7) |
| Process-readable secrets via systemd credentials | `LoadCredential=` for the frps config, transport cert/key and the gateway sync cert/key/CA; `ExecStart` reads `${CREDENTIALS_DIRECTORY}`; the gateway's sync paths use `%d` (A8) |
| Firewall public allowlist is exactly `443` + transport | nftables input `policy drop`; `elements = { 443, 7000 }`; loopback accepted (A9) |
| No public proxy/plugin/metrics/admin port | forbidden ports (`9001`, `9101`, `9102`, `7500`, the proxy range) cannot appear in an accept rule; plugin/metrics binds are pinned loopback in the unit (A10) |
| Transport certificate only, no ACME/content material | `install.sh` provisions only `tunnel-server.{crt,key}`; no `fullchain`/`privkey`/`ACME_*`/`CLOUDFLARE_TOKEN`; `transport.tls.force = true` (A11) |
| Pinned, checksum-verified frps | `install.sh` checks every `frps` against the per-platform `frps_sha256` in `frp/manifest.json` (or an explicit `--frps-sha256`) and re-hashes the installed copy; `fetch-frp.sh` re-verifies staged binaries against the pinned tarball (never the `.verified` marker); `auth.method = "token"` with a mandatory plugin (A12) |
| DNS-only relay names, no HTTPS/SVCB/ECH for them | `install.sh --audit-dns` proves random-child wildcard synthesis and per-name HTTPS/SVCB/ECH absence; §5 below (A13). This is **not** a zone-wide AXFR proof |
| Deferred operator surface documented | §6–§8 of this runbook (A14) |

The gateway's own journal write rate is additionally bounded in-process
(200 lines/s, burst 100) so a public-connection flood cannot fill the journal;
the unit's `LogRateLimit*` bounds the per-service journald rate on top.

---

## 4. Restart ordering

The gateway serves the FRP authorization plugin, so **gateway before frps**:

1. `systemctl restart sharebridge-relay-gateway` — frps is not stopped.
   Existing agent tunnels persist, but new `Login`/`NewProxy`/`Ping`
   authorizations fail while the plugin is down. After restart the gateway
   boots **unready with an empty route table** and only admits routes after a
   full control-sync snapshot; `frps` process health is an independent truth
   that reads unhealthy until a fresh authenticated plugin fact arrives.
2. `systemctl restart sharebridge-relay-frps` — this is a §15.2 session reset:
   the gateway clears presence immediately and terminates the affected agent's
   established streams through the streams registry; a warm replacement tunnel
   is not killed early. Process-up is never treated as tunnel-present.
3. `systemctl restart` ordering is never reversed by an operator script.

`systemd-analyze verify relay/deploy/sharebridge-relay-gateway.service
relay/deploy/sharebridge-relay-frps.service` must pass before enabling; the
installer runs it.

---

## 5. DNS audit (spec §6 invariant)

An HTTPS or SVCB resource record (in particular one carrying an `ech=`
parameter) would publish an **ECH** key for a relay name. A browser that uses
ECH hides the SNI from both the gateway and the agent Binder, and exact relay
routing silently breaks. The `sharebridgeusercontent.com` zone must therefore
publish the relay wildcard family and the tunnel host as **DNS-only A/AAAA
names with no HTTPS/SVCB/ECH record**.

Run the built-in audit (it exits nonzero when the invariant is violated for a
queried name):

```bash
relay/deploy/install.sh --audit-dns \
  --namespace sb0123abcd --tunnel-host <relay-tunnel-host> \
  [--expect-ipv4 <relay-ipv4>] [--expect-ipv6 <relay-ipv6>]
```

The audit proves, **for the names it queries**:

1. **Wildcard synthesis** — a random child label
   (`probe-<time>-<pid>-<rand>.relay.<ns>.sharebridgeusercontent.com`) resolves
   to an A record. A bare `dig A '*.relay.…'` query alone would not prove that
   browsers resolving arbitrary labels reach the relay.
2. **DNS-only A/AAAA** — the relay wildcard, the random child label and the
   tunnel host resolve as A records; any AAAA answer that is present must be a
   valid IPv6 literal (and match `--expect-ipv6` when supplied).
3. **No HTTPS/SVCB/ECH** — the relay wildcard, the random child label and the
   tunnel host publish no HTTPS or SVCB record and no `ech=` parameter.

Equivalent manual commands — every one of these must return the documented
result:

```bash
# 1. The relay wildcard synthesizes a record for a random child label.
dig +short A "probe-$RANDOM.relay.sb0123abcd.sharebridgeusercontent.com"  # -> relay VM IPv4

# 2. The relay wildcard and tunnel host are DNS-only.
dig +short A    '*.relay.sb0123abcd.sharebridgeusercontent.com'   # -> relay VM IPv4
dig +short AAAA '*.relay.sb0123abcd.sharebridgeusercontent.com'   # -> empty or a valid IPv6
dig +short A    '<relay-tunnel-host>'                             # -> relay VM IPv4
dig +short AAAA '<relay-tunnel-host>'                             # -> empty or a valid IPv6

# 3. No HTTPS/SVCB records for a queried relay name (an ech= key here hides SNI).
dig +short HTTPS '*.relay.sb0123abcd.sharebridgeusercontent.com'  # -> EMPTY
dig +short HTTPS '<relay-tunnel-host>'                            # -> EMPTY
dig +short SVCB  '*.relay.sb0123abcd.sharebridgeusercontent.com'  # -> EMPTY
dig +short SVCB  '<relay-tunnel-host>'                            # -> EMPTY
dig +short HTTPS "probe-$RANDOM.relay.sb0123abcd.sharebridgeusercontent.com"  # -> EMPTY

# 4. Belt-and-braces: no ech= parameter in the HTTPS answers queried above.
dig +short HTTPS '*.relay.sb0123abcd.sharebridgeusercontent.com' '<relay-tunnel-host>' | grep -i 'ech=' || echo 'no ECH (correct)'

# 5. Diagnostic context only — CT/CAA monitoring is a separate release
#    prerequisite (spec §3.2), not an MVP gate.
dig +short CAA sharebridgeusercontent.com
```

**Scope of the claim.** The audit queries the relay wildcard, a random child
label and the tunnel host; it does **not** perform an authoritative zone
transfer (AXFR) or a DNS-API dump. It therefore proves **no HTTPS/SVCB/ECH
record for those relay names** — it does not prove zone-wide absence for
unrelated names. A zone-wide claim would need the authoritative provider's
transfer/API access and its own credential handling, which is out of scope for
this DNS-only gate. The deployment audit re-checks the three properties above,
and Task 37 repeats it on the live dark topology.

---

## 6. Gateway §14 resource bounds

Every bound has one production variable in `gateway.env`. An unset (or empty)
variable keeps its documented default. A set value must be a **positive
integer** (or Go duration); anything else — including a value above a hard
ceiling — makes the gateway **refuse startup** rather than run with a silently
wrong bound.

| Variable | Default | Notes |
|---|---|---|
| `SHAREBRIDGE_GATEWAY_MAX_STREAMS_PER_SOURCE_IP` | `16` | concurrent connections from one source IP |
| `SHAREBRIDGE_GATEWAY_MAX_STREAMS_PER_ORIGIN` | `32` | concurrent streams on one exact origin |
| `SHAREBRIDGE_GATEWAY_MAX_STREAMS_PER_AGENT` | `64` | concurrent streams across one agent's routes |
| `SHAREBRIDGE_GATEWAY_MAX_STREAMS_GLOBAL` | `8192` | **hard ceiling**: may only be lowered to the host file-descriptor budget, never raised |
| `SHAREBRIDGE_GATEWAY_MAX_HELLO_BYTES` | `65536` | **hard ceiling**: the ClientHello inspection budget is 64 KiB; a larger value refuses startup |
| `SHAREBRIDGE_GATEWAY_HELLO_TIMEOUT` | `5s` | ClientHello read deadline |
| `SHAREBRIDGE_GATEWAY_DIAL_TIMEOUT` | `2s` | loopback connect budget |
| `SHAREBRIDGE_GATEWAY_IDLE_TIMEOUT` | `5m` | no-byte stream idle timeout (activity in either direction resets it) |
| `SHAREBRIDGE_GATEWAY_ABSOLUTE_LIFETIME` | `24h` | hard connection close; continuous activity cannot extend it |
| `SHAREBRIDGE_GATEWAY_MAX_TRACKED_AGENTS` | `4096` | bound on the persistent per-agent byte map; a new agent beyond it is refused (fail-closed, never evicted) |

All ten are declared and installed explicitly in `gateway.env` so the deployed
values are auditable. The §14 product-tier bandwidth throttle stays **off** in
Phase 4a (the pinned FRP release exposes no approved server-side cap); per-agent
byte counters and saturation alerts are the operator's emergency tools, and the
cap is deferred to Phase 4b.

Set `LimitNOFILE` above `MAX_STREAMS_GLOBAL` plus overhead (the shipped unit
uses `65536`); when the host FD budget is below `8192`, lower the global bound
instead of raising the unit's limit.

---

## 7. Private metrics and health listeners

Both metrics endpoints are **private-only by design**; they refuse a
non-loopback bind at startup and independently reject a public peer or Host.
They must never be added to the firewall allowlist.

| Listener | Address | Variable | Endpoints |
|---|---|---|---|
| relay gateway | `127.0.0.1:9101` | `SHAREBRIDGE_GATEWAY_METRICS_ADDR` | `/metrics`, `/healthz` |
| control | `127.0.0.1:9102` | `CONTROL_METRICS_ADDR` | `/metrics` |

Scrape them from the private network or an SSH tunnel only. `/healthz` reports
two independent truths:

- `route_ready` — full route **snapshot** applied **and** control sync live.
  This is the gateway's ability to route; it never depends on frps process
  health.
- frps **process** health — separate. It is proven by fresh authenticated
  plugin traffic and reads unhealthy (fail-closed) when that traffic is
  missing.

---

## 8. Control sync, NIC saturation and the frps freshness window

The gateway's control-sync client is configured with six variables that must be
set **together**; a **partial** configuration is a startup error and a
**completely absent** configuration runs the gateway in the dark posture
(route readiness stays fail-closed false, with a warning).

| Variable | Purpose |
|---|---|
| `SHAREBRIDGE_CONTROL_SYNC_URL` | HTTPS control-sync endpoint |
| `SHAREBRIDGE_CONTROL_SYNC_SAN` | expected server SAN (verified) |
| `SHAREBRIDGE_CONTROL_SYNC_CA_FILE` | CA bundle verifying control |
| `SHAREBRIDGE_GATEWAY_SYNC_CERT_FILE` | gateway client certificate (mTLS) |
| `SHAREBRIDGE_GATEWAY_SYNC_KEY_FILE` | gateway client private key (mTLS) |
| `SHAREBRIDGE_GATEWAY_NAMESPACE` | this gateway's §6 namespace (`sb` + 8 hex) |

In the hardened unit the three file paths point at `%d` (the service's systemd
credential directory); the on-disk files stay `root:root 0600`.

NIC saturation (an optional §17.3 gauge) is reported only when **both** of the
following are set; otherwise the gauge renders `NaN` (unavailable) rather than
a fabricated `0`:

- `SHAREBRIDGE_GATEWAY_NIC_INTERFACE` — interface to sample (`/proc/net/dev`);
- `SHAREBRIDGE_GATEWAY_NIC_CAPACITY_BYTES_PER_SEC` — positive link capacity.

### frps freshness window

`DefaultFRPSFreshnessWindow` is **30 seconds**, grounded in the generated
frpc application-heartbeat cadence of **10 seconds** (three nominal beats). The
frps↔gateway plugin channel is per-operation HTTP with no persistent link, so
liveness is proven by fresh authenticated facts: any authenticated plugin call
stamps the window and a `SessionReset` proves a restart. Consequences:

- the unit must **restart frps promptly** after a crash — a dead frps stops all
  agent heartbeats, and the gateway then reports frps **unhealthy within 30
  seconds** instead of staying stale-true (the `SetFRPSHealthy(false)`
  supervisor seam remains available for an explicit immediate down transition);
- with no authenticated traffic at all (never-seen or idle frps) the truth is
  unproven and reads **unhealthy** (fail-closed);
- frps process health never gates `route_ready` and process-up is never
  presence.

The §17.3 restart→restoration histogram is anchored only by a real §15.2
`SessionReset` fact, so an ordinary offline→online transition is never
misreported as a restart restoration.

---

## 9. Verification

```bash
bash relay/deploy/deploy_test.sh                       # static hardening gate (must be GREEN)
systemd-analyze verify relay/deploy/sharebridge-relay-gateway.service \
                       relay/deploy/sharebridge-relay-frps.service
nft -c -f relay/deploy/firewall.nft                    # ruleset syntax check
ss -ltnp | grep -E ':(443|7000|9001|9101|100[0-9]{2})\b'  # 9001/9101/proxies must show 127.0.0.1
```

Then confirm: `systemctl is-active` both units; `/healthz` on
`127.0.0.1:9101` shows `route_ready=false` until control sync completes; a
secret scan of the new deployment files finds no key material; and the §5 DNS
audit passes.

---

## 10. Upgrade and teardown

- **Upgrade:** rerun `install.sh` with the same arguments (idempotent). It
  re-verifies the pinned FRP digest, reinstalls binaries/units, and restarts;
  the transport certificate and control-sync material are replaced only when
  new files are supplied.
- **Teardown:** `systemctl disable --now sharebridge-relay-frps
  sharebridge-relay-gateway`, then `nft delete table inet sharebridge_relay`.
  Remove the relay wildcard DNS record and delete the VM to stop billing.
- **Rollback:** set `RELAY_SELECTION_ENABLED=false` in the control deployment
  (relay is then never selected; direct candidates still get the interstitial),
  then stop the relay services. See `docs/RELEASING.md`.

---

## 11. Known open items

- **Presence-transport gap (ledger I4-partial) — open, not solved here.** The
  gateway's presence is gateway-authoritative over the control-sync channel,
  but the presence-transport completeness item carried by the security ledger
  remains a **known open item with its own task**. This runbook does not claim
  it is resolved: do not treat these units, the firewall, or the DNS audit as
  closing it.
- The Phase 4a bandwidth throttle is intentionally disabled (deferred to
  Phase 4b); the global stream ceiling / FD budget and per-agent counters are
  the MVP safety tools.
- CT/CAA monitoring remains a separate user-owned release prerequisite
  (spec §3.2); the DNS audit records CAA context but does not gate on it.
