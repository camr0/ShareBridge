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
| `SHAREBRIDGE_GATEWAY_MAX_TRACKED_AGENTS` | `4096` | **hard ceiling**: bound on the persistent per-agent byte map; it may only be lowered, never raised (a larger value refuses startup). A new agent beyond the configured ceiling is refused (fail-closed, never evicted). At the ceiling the map costs roughly 1 MiB (≈2 map entries × ≤64-byte agent ID per agent), negligible against the 512 MiB gateway `MemoryMax` |

All ten are declared and installed explicitly in `gateway.env` so the deployed
values are auditable. The §14 product-tier bandwidth throttle stays **off** in
Phase 4a (the pinned FRP release exposes no approved server-side cap); per-agent
byte counters and saturation alerts are the operator's emergency tools, and the
cap is deferred to Phase 4b.

Set `LimitNOFILE` above `MAX_STREAMS_GLOBAL` plus overhead (the shipped unit
uses `65536`); when the host FD budget is below `8192`, lower the global bound
instead of raising the unit's limit. The §12 capacity baseline validates this
budget on real load; `scripts/relay-capacity-gate.sh` derives the safe default
from the host's descriptor limit.

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

### Control-side §11.3 sync listener (task #16 / ledger I4-partial)

The gateway's client above points at an endpoint that **control** serves. Until
task #16 control's production entrypoint never constructed the listener, so the
channel was dark in production: no `/status` endpoint existed, publisher lease
renewal and presence never reached control, and the gateway fail-closed to
not-ready. Control now constructs and serves the private mTLS listener when the
following variables are configured (all four material/identity variables must
be set **together**; a partial configuration is a startup error, and none set
means the channel stays dark):

| Variable | Purpose |
|---|---|
| `CONTROL_SYNC_BIND_ADDR` | private bind (default `127.0.0.1:9443`; only loopback/RFC 1918/ULA accepted) |
| `CONTROL_SYNC_CERT_FILE` | control sync server leaf (PEM) |
| `CONTROL_SYNC_KEY_FILE` | control sync server key (PEM) |
| `CONTROL_SYNC_CLIENT_CA_FILE` | CA that issued the gateway's client leaf |
| `CONTROL_SYNC_EXPECTED_CLIENT_SAN` | exact gateway client SAN accepted |

The listener is TLS 1.3 with `RequireAndVerifyClientCert`: it accepts only a
client leaf issued by `CONTROL_SYNC_CLIENT_CA_FILE` and carrying exactly
`CONTROL_SYNC_EXPECTED_CLIENT_SAN`. The gateway side must point at it with
`SHAREBRIDGE_CONTROL_SYNC_URL=https://<control-sync-host:port>`,
`SHAREBRIDGE_CONTROL_SYNC_SAN` equal to the control leaf's SAN, and
`SHAREBRIDGE_CONTROL_SYNC_CA_FILE` equal to the CA that issued the control
leaf. The three control-side files stay `root:root 0600`; mount them read-only.

Private-only by construction: the bind must be numeric loopback or RFC
1918/ULA (an unspecified/public address fails startup), and in the collocated
test deployment it is loopback, so it is never added to the firewall. In the
separate-VM production topology bind control's private-network address and let
the relay VM reach it over the private network — never the public interface.

Material generation (operator step; no generator ships yet). One shared sync
CA signs control's server leaf and the gateway's client leaf:

```bash
# On a trusted workstation (not a service host):
openssl ecparam -genkey -name prime256v1 -noout -out sync-ca.key
openssl req -x509 -new -key sync-ca.key -sha256 -days 825 \
  -subj /CN=sharebridge-sync-ca -out sync-ca.crt
# Control server leaf, SAN = the control sync endpoint name (the value the
# gateway pins in SHAREBRIDGE_CONTROL_SYNC_SAN).
openssl ecparam -genkey -name prime256v1 -noout -out control-sync.key
openssl req -new -key control-sync.key -subj /CN=control-sync.internal -out control-sync.csr
openssl x509 -req -in control-sync.csr -CA sync-ca.crt -CAkey sync-ca.key \
  -CAcreateserial -days 825 -out control-sync.crt \
  -extfile <(printf 'subjectAltName=DNS:control-sync.internal\nextendedKeyUsage=serverAuth\nkeyUsage=digitalSignature')
# Gateway client leaf, SAN = CONTROL_SYNC_EXPECTED_CLIENT_SAN exactly.
openssl ecparam -genkey -name prime256v1 -noout -out gateway-sync.key
openssl req -new -key gateway-sync.key -subj /CN=sharebridge-relay-gateway.sync.internal -out gateway-sync.csr
openssl x509 -req -in gateway-sync.csr -CA sync-ca.crt -CAkey sync-ca.key \
  -CAcreateserial -days 825 -out gateway-sync.crt \
  -extfile <(printf 'subjectAltName=DNS:sharebridge-relay-gateway.sync.internal\nextendedKeyUsage=clientAuth\nkeyUsage=digitalSignature')
chmod 600 sync-ca.key control-sync.key gateway-sync.key
```

Install `control-sync.{crt,key}` + `sync-ca.crt` on control as
`root:root 0600` (`CONTROL_SYNC_CERT_FILE`, `CONTROL_SYNC_KEY_FILE`,
`CONTROL_SYNC_CLIENT_CA_FILE`) and `CONTROL_SYNC_EXPECTED_CLIENT_SAN` =
the gateway leaf SAN. On the relay VM pass `--sync-ca sync-ca.crt --sync-cert
gateway-sync.crt --sync-key gateway-sync.key --sync-san control-sync.internal`
to `relay/deploy/install.sh`; it installs them `root:root 0600` and serves
them to the gateway through systemd credentials.

**Collocated test topology.** `control/deploy-testing-relay.sh` performs the
steps above automatically for the single-box test deployment: it generates one
shared sync CA and both leaves locally in `control/.env.testing.d/` (never
committed), installs the public material `root:root 0600` in
`/opt/sharebridge/control-sync/` (the systemd equivalent of the container
topology's `/run/sharebridge-sync` read-only mount of `./control-sync`), writes
all five `CONTROL_SYNC_*` variables into control's env, and writes the six
gateway sync variables **together** into the gateway env. Because control
generates the §6 namespace per agent at enrollment and the applier is
single-namespace, the test script ships the gateway **dark** until the operator
pins the enrolled agent's namespace:

```bash
control/deploy-testing-relay.sh <user@host> --pin-namespace sbXXXXXXXX
```

That pin is a deliberate single-agent test simplification. The observable is
the gateway's private `route_ready` (`curl -sf http://127.0.0.1:9101/healthz`
on the box) — the sync listener is loopback-private and is never probed over
the network. Full operator steps:
`docs/operations/phase4a-test-vps-deploy.md` §3c and §6.

Presence transport: the gateway republishes its full authoritative presence
state every **15 seconds** (and immediately on a transition), one third of the
45-second presence lease, so control's stored lease cannot expire while the
tunnel is healthy. The first republish after a gateway boot is its **empty**
boot snapshot carrying the top-level `gateway_boot_id`/`revision`, which is what
makes a restarted gateway adoptable at control (its later events are no longer
discarded as a superseded boot's replay).

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

- ~~**Presence-transport gap (ledger I4-partial) — open, not solved here.**~~
  **Resolved by task #16.** Control constructs and serves the private mTLS sync
  listener from `CONTROL_SYNC_*` (see §8), the gateway republishes its
  authoritative presence every 15 s, and an empty boot snapshot carries the
  reporting boot identity so a restarted gateway is adoptable. The remaining
  operational prerequisite is the mTLS material itself (control server
  leaf/key, the gateway client leaf/key, and the shared client CA), which is
  operator-provisioned on both hosts.
- The Phase 4a bandwidth throttle is intentionally disabled (deferred to
  Phase 4b); the global stream ceiling / FD budget and per-agent counters are
  the MVP safety tools.
- CT/CAA monitoring remains a separate user-owned release prerequisite
  (spec §3.2); the DNS audit records CAA context but does not gate on it.

---

## 12. Capacity and safety baseline (Task 36, spec §23.8)

Task 36 establishes the §23.8 capacity/safety baseline and validates the §14
defaults without implementing Phase 4b selection. **The results below do not
alter route selection**: no selection input, preference, or automatic fallback
is added or changed by this task. They are also **not a substitute for the
Phase 4b bandwidth-bounded packet-impairment work** (§18.5: RTT/loss-only
loopback tests previously produced a false SCTP conclusion). The §14
product-tier bandwidth throttle stays **DISABLED** in Phase 4a; the pinned FRP
release exposes no approved server-side cap, so un-throttled operation gathers
honest relay throughput data for Phase 4b. Per-agent byte counters and
saturation alerts remain the operator's emergency tools.

Reproduce the whole local baseline (it prints the values in this table):

```bash
bash scripts/relay-capacity-gate.sh
```

The gate runs the hermetic real-FRP suite
(`relay/internal/integration/load_test.go`) over loopback:

```bash
cd relay && SHAREBRIDGE_FRP_INTEGRATION=1 \
  go test -race -count=5 -timeout 45m ./internal/integration \
  -run '^TestRelayCapacity' -v
```

### 12.1 What was measured here vs what is pending

Measured on this development host (darwin/arm64, Go 1.26.1, loopback only,
`-race -count=5`). Every value is machine-emitted as a
`CAPACITY(EVIDENCE)` line; nothing below is extrapolated. The target relay VM
is a **separate host provisioned by Task 37** and is not available in Phase 4a,
so every hardware-dependent cell is explicitly **PENDING HARDWARE (Task 37)**.

| Cell | Local loopback (measured, this host) | Target relay VM |
|---|---|---|
| One-agent throughput | 616–763 req/s, 227–282 KiB/s (48 requests/run, 0 errors) | **PENDING HARDWARE (Task 37)** |
| Multi-agent throughput | 897–1058 req/s, 331–390 KiB/s (64 requests/run over 2 agents, 0 errors) | **PENDING HARDWARE (Task 37)** |
| Per-agent byte accounting | both agents' counters advance; the `relayed_bytes_total` global/agent/origin scopes agree (229 KiB single-agent run) | **PENDING HARDWARE (Task 37)** |
| CPU | not a loopback-meaningful number | **PENDING HARDWARE (Task 37)** |
| RSS | not a loopback-meaningful number | **PENDING HARDWARE (Task 37)** |
| Open FDs | process baseline 14–15, peak 47 at concurrency 8, returns to 14–15; every admission slot released | **PENDING HARDWARE (Task 37)** |
| NIC saturation | not a loopback-meaningful number | **PENDING HARDWARE (Task 37)** |
| Global stream default | `8192` (host FD budget 1,048,575 ≫ 2×8192+reserve) | **PENDING HARDWARE (Task 37)**: confirm on the VM size |

Safety cases (all GREEN, `-race -count=5`):

| Property | Result |
|---|---|
| §14 per-agent saturation without cross-agent starvation | Agent A saturated at its ceiling (`agent_a_limit=2`), new A streams generically closed, agent B admitted and served content meanwhile, 2 bounded `reason=limits` rejections recorded, all slots released and A readmitted afterwards |
| Active stream beyond the idle window | configured idle 2 s; stream stayed alive 5.44–5.45 s while bytes flowed, then closed 1.55 s after the last byte |
| No-byte idle close | configured idle 2 s; closed 2.001 s after the handshake |
| Absolute lifetime (hard close) | configured lifetime 2 s with idle 300 s; continuously active stream closed at 1.994–2.001 s (activity never extended it) |
| Cancellation | 3 concurrent streams cancelled; agent observed cancellation, registry and every admission slot returned to 0, capacity reused afterwards |
| Goroutine / FD / retained-buffer plateau | after 64 served requests across 4 rounds at concurrency 8: goroutines returned to the run baseline (final = baseline ±1), FDs baseline 14–15 → final 15, **measured retained live heap** (GC-stabilised `HeapAlloc` delta) 162–237 KiB against a **1 MiB** bound, peak live streams 8 (concurrency bound, copy-buffer footprint ≤ (8+1)×2×32 KiB ≈ 576 KiB). One 32 KiB copy buffer retained per served connection would be 64 × 32 KiB = 2 MiB and fail; the 64 KiB/connection reviewer mutation (4 MiB) fails. |

Interpretation limits (stated so the numbers are not over-read):

- The plateau figures are **process-wide** measurements of the hermetic test
  process. Under `-count=5` the absolute pre-load goroutine baseline drifts as
  more hermetic stacks are created in one process (a test-harness lifecycle
  effect); the asserted property is the **within-run** delta — resources return
  to the run's own baseline and do not scale with cumulative requests.
- The retained-heap figure is the **measured** `HeapAlloc` delta between the
  settled baseline and the settled post-load state after two forced
  collections (the second drains the `sync.Pool` victim cache, so idle pooled
  buffers are not miscounted as retained). It is grounded in a measured
  quantity, not inferred from the live-stream count. The measurement is
  process-wide and therefore also carries the harness's own per-connection TLS
  byte tap; the evidence line reports that contribution separately as
  `harness_tap_growth` (≈114 KiB at this request count). The 1 MiB bound is
  deliberately below one 32 KiB copy buffer per served connection (2 MiB across
  64 requests), so a realistic per-connection buffer retention fails while the
  measured overhead keeps >4× headroom under `-race`.
- Loopback throughput includes real pinned `frps`/`frpc` processes, real TLS
  and real L4 passthrough, but no WAN RTT, no packet loss and no target-VM
  CPU/NIC ceiling; it is a safety baseline, not a capacity promise.
- The `absolute-lifetime` and idle cases use short configured windows to fit a
  bounded test; the defaults themselves (`5m` idle, `24h` lifetime) are
  unchanged and unit-tested in `relay/internal/limits`.

### 12.2 Safe defaults and the host FD budget

The §14 global ceiling is `8192`; the deployed `MAX_STREAMS_GLOBAL` may only be
lowered to the host file-descriptor budget, never raised. Each admitted public
stream costs the gateway one accepted socket plus one loopback dial to the
agent's FRP proxy port (2 FDs), so the required descriptor budget is
approximately `2 × MAX_STREAMS_GLOBAL + 256` (listeners, plugin, metrics,
control-sync TLS, runtime). The shipped unit sets `LimitNOFILE=65536`, which
covers the `8192` default (≈16.6 k descriptors) with margin.

`scripts/relay-capacity-gate.sh` computes
`safe_global = min(8192, (ulimit -n − 256) / 2)` and prints it. On hosts whose
FD budget is below ≈16.6 k, lower `SHAREBRIDGE_GATEWAY_MAX_STREAMS_GLOBAL`
accordingly instead of raising the unit's `LimitNOFILE`. The chosen default for
the documented deployment is therefore **8192**, which is ≤ the host FD budget
on every supported host, and Task 37 records the measured gateway FD count at
saturation to confirm it.

### 12.3 Target-VM procedure (Task 37)

On the provisioned relay VM, with both services running, run:

```bash
bash scripts/relay-capacity-gate.sh --target
```

It samples the gateway's open FDs (`/proc/<pid>/fd`), RSS (`VmRSS`), CPU
(`/proc/<pid>/stat` delta), NIC saturation (`/proc/net/dev` against
`SHAREBRIDGE_GATEWAY_NIC_CAPACITY_BYTES_PER_SEC`), the private §17.3 counters
from `127.0.0.1:9101/metrics`, and, when `SHAREBRIDGE_CAPACITY_RELAY_URL` is
set, a bounded request sample against the live relay origin. Any input it
cannot read is printed as **PENDING HARDWARE (Task 37)** — the gate never
prints a number it did not measure.

---

## 13. M6 live acceptance harness (Tasks 37–44)

`scripts/live-phase4a.sh` is the single, gated, evidence-producing harness for
the M6 live acceptance cases. Task 37 ships the skeleton: the real §23.9 dark
topology gate plus one registered placeholder per later acceptance case, so
Tasks 38–44 each change **one gate function body** and nothing in the runner.

### 13.1 Running it

Run from the repository root (the default evidence path is relative to the
working directory):

```bash
# every registered gate (the full M6 run)
scripts/live-phase4a.sh

# one gate only (unselected gates are listed NOT_RUN and excluded from the verdict)
scripts/live-phase4a.sh --case gate_23_9_dark_topology

# show the gate table, or the planned run without executing anything
scripts/live-phase4a.sh --list
scripts/live-phase4a.sh --dry-run

# prove the pass/fail plumbing (trivially-true, trivially-false, skip,
# zero-check, note-only, crash-after-PASS, placeholder, secret-detail cases)
scripts/live-phase4a.sh --selftest

# redirect evidence somewhere other than docs/operations/evidence/runs
scripts/live-phase4a.sh --evidence-dir /tmp/phase4a-evidence
```

Exit codes: `0` GREEN (every selected gate PASS; unscoped, every registered
gate), `1` RED (any selected gate FAIL / MISSING / NOT_IMPLEMENTED), `2` usage
error, `3` PARTIAL (no failures but at least one SKIP, or a dry run).

**Honesty contract (this project has been bitten by skip-as-PASS twice).** A
gate that executes zero checks is `MISSING`, never PASS. A gate whose function
exits non-zero after recording PASSes is `FAIL`. An unimplemented registered
gate is `NOT_IMPLEMENTED`, never PASS. A `SKIP` sub-check makes the gate `SKIP`
(PARTIAL overall). A gate needs at least one measured, PASSing `check`: the
`note()` helper is informational and cannot carry a verdict, so a note-only (or
otherwise check-less) gate is `MISSING` and the run is RED. `--case` scoping may
only narrow the verdict to the gates it ran; unselected gates are printed as
`NOT_RUN` and explicitly excluded. The only GREEN is every selected gate PASS.

**Safety.** Remote commands are read-only (`systemctl show/cat`, `nft list`,
`ss`, `curl`, `dig`, `openssl s_client`, `ip addr`, `grep`); nothing is written
on control or the relay VM. The only writes are local evidence files. Every
captured line passes through a sanitizer that redacts key/token/password/
secret/cookie/authorization/`jti` values, private-key blocks and share codes.
The optional live restart drill runs only with
`LIVE_PHASE4A_ALLOW_RESTART=1`. The collocated test VPS (`178.156.174.47` by
default) is explicitly rejected as the M6 relay target
(`LIVE_PHASE4A_EXCLUDED_RELAY_IPS`); the M6 gate needs the new separate VM.

Running it before the topology exists is expected to fail with a diagnostic
naming every missing input. That is Task 37 Step 2's RED, not a harness bug:

```bash
$ scripts/live-phase4a.sh --case gate_23_9_dark_topology   # today
  [FAIL] control_reachable: LIVE_PHASE4A_CONTROL_HOST is unset — ...
  [FAIL] relay_vm_distinct: LIVE_PHASE4A_RELAY_HOST is unset — ...
  [FAIL] selection_flag_remains_false: LIVE_PHASE4A_CONTROL_HOST is unset — ...
  VERDICT: RED
```

The dark-posture assertion is deliberately a separate check: a missing topology
must never be masked, and `RELAY_SELECTION_ENABLED` is **observed** in the
deployed control configuration — the running control process environment, an
explicit unit `Environment=`, or the env file the unit loads (highest authority
first). Absent means unobservable: the check FAILs rather than assuming the
code default (`never assume the dark posture`), an unreadable config FAILs
instead of assuming it, and an observed `true` fails loudly.

### 13.2 Environment surface

All inputs are documented env vars; unset required values FAIL the gate and name
the variable. No host is hard-coded.

| Variable | Default | Needed for |
|---|---|---|
| `LIVE_PHASE4A_CONTROL_HOST` | — | control SSH target (all control checks) |
| `LIVE_PHASE4A_RELAY_HOST` | — | relay VM SSH target (topology, firewall, health, ordering) |
| `LIVE_PHASE4A_RELAY_PUBLIC_IP` | — | external firewall probe, DNS match, excluded-VPS check |
| `LIVE_PHASE4A_RELAY_TUNNEL_HOST` | — | `<relay-tunnel-host>` DNS + transport cert |
| `LIVE_PHASE4A_NAMESPACE` | — | relay wildcard synthesis probe |
| `LIVE_PHASE4A_IMMICH_URL` | — | real Immich ping on the home Mac |
| `LIVE_PHASE4A_TRANSPORT_CA_FILE` | — | transport certificate validation |
| `LIVE_PHASE4A_SYNC_CA_FILE`, `LIVE_PHASE4A_GATEWAY_SYNC_CERT`, `LIVE_PHASE4A_GATEWAY_SYNC_KEY` | — | live sync mTLS handshake (paths **on the relay VM**) |
| `LIVE_PHASE4A_AGENT_PID_MATCH` | `sharebridge-agent` | home Mac agent process |
| `LIVE_PHASE4A_TRANSPORT_PORT` | `7000` | firewall allowlist, cert probe |
| `LIVE_PHASE4A_BASE_DOMAIN` | `sharebridgeusercontent.com` | wildcard DNS |
| `LIVE_PHASE4A_CONTROL_UNIT` / `_GATEWAY_UNIT` / `_FRPS_UNIT` | `sharebridge.service` / `sharebridge-relay-gateway.service` / `sharebridge-relay-frps.service` | reachability + restart ordering |
| `LIVE_PHASE4A_CONTROL_ENV_FILE` | `/opt/sharebridge/.env` | `RELAY_SELECTION_ENABLED`, `CONTROL_SYNC_*` |
| `LIVE_PHASE4A_GATEWAY_ENV_FILE` | `/etc/sharebridge/relay/gateway.env` | gateway sync + namespace |
| `LIVE_PHASE4A_HEALTHZ_URL` | `http://127.0.0.1:9101/healthz` | `route_ready` (private, probed over SSH) |
| `LIVE_PHASE4A_EVIDENCE_DIR` | `docs/operations/evidence/runs` | evidence output root |
| `LIVE_PHASE4A_ALLOW_RESTART` | `0` | enables the guarded live restart drill |
| `LIVE_PHASE4A_EXCLUDED_RELAY_IPS` | `178.156.174.47` | refuses the collocated test VPS |
| `LIVE_PHASE4A_SSH_OPTS`, `_SSH_CONNECT_TIMEOUT`, `_REMOTE_TIMEOUT` | —, `8`, `20` | SSH plumbing |

The environment record these gates populate is
`docs/operations/evidence/phase4a-environment.md`; the harness writes the same
keys to `<run-dir>/environment-facts.txt`. A field is only filled from an
observation — never extrapolated.

### 13.3 What each gate proves, and who runs it

| Gate | Task | Proves | Needs |
|---|---|---|---|
| `gate_23_9_dark_topology` | 37 | §23.9: control reachable; a NEW relay VM distinct from control in the same Hetzner region/private network (metadata `instance-id`/`region` + private `/24`); home Mac agent and real Immich reachable; public allowlist exactly 443 + transport with `policy drop` and no public UDP; private mTLS `CONTROL_SYNC_*`/`SHAREBRIDGE_*` configured with a live authenticated handshake and enforced client auth; tunnel-host DNS (A/AAAA/no ECH) plus wildcard synthesis and dedicated transport-cert validation; `route_ready` snapshot health and frps process health; gateway-before-frps restart ordering with no `BindsTo`/`PartOf`; `RELAY_SELECTION_ENABLED` observed and still false | second VM, DNS, mTLS material (operator) |
| `acceptance_01_owner_hairpin` | 38 | §19 #1 owner hairpin on the confirmed non-hairpin router | user (router, 3 browsers) |
| `acceptance_04_interstitial_blackhole` | 38 | §19 #4 deterministic interstitial fallback to the exact relay origin | user (browser) |
| `acceptance_02_cellular_relay_video` | 39 | §19 #2 cellular/no-direct gallery + video, ≥2 valid `206` seeks | user (phone, cellular) |
| `acceptance_11_content_parity` | 39 | §19 #11 Phase 3 content/Range/accounting parity through relay | user (browser) |
| `acceptance_03_relay_only` | 40 | §19 #3 relayOnly never activates direct | user (browser) + instrumentation |
| `l4_no_plaintext_capture` | 41 | §19 #5 / §18.4 no plaintext at the relay (canary capture; any plaintext is NO-GO) | relay VM (`scripts/l4-canary-capture.sh`) |
| `acceptance_06_no_mapper_enrollment` | 42 | §19 #6 no-mapper enrollment + outbound-tunnel serving | user (NAT setup) |
| `acceptance_12_stun_mismatch` | 42 | §19 #12 egress mismatch → `relay_fallback`, no public probe | user (NAT) |
| `acceptance_13_stun_cadence_cold_budget` | 42 | §19 #13 cadence and four-second cold budget | user (NAT) |
| `acceptance_07_no_relay_open_signal` | 43 | §19 #7 no open signal/probe/mapper on route=relay | instrumented run |
| `acceptance_08_exact_routing` | 43 | §19 #8 exact SNI routing, no cross-agent routing | live topology |
| `acceptance_09_restart_recovery` | 43 | §19 #9 availability only after fresh presence | live topology |
| `acceptance_10_lockdown` | 43 | §19 #10 lockdown closes both connection kinds; fresh-credential unlock | live topology |
| `acceptance_15_heartbeat_tunnel_dns` | 43 | §19 #15 tunnel DNS/cert, 10 s Pings, 45 s expiry | live topology |
| `release_go_no_go_rollback` | 44 | §20 steps 5–7 + §23 blocking rule; manifest gate + rollback drill | all evidence + user GO |

§19 #14 (no-JS/CSP) is a Task 24 hermetic browser gate and is intentionally not
in this harness.

### 13.4 M6 ordering

`37 (dark topology) → 38 → 39 → 40 → 41 → 42 → 43 → 44`. Tasks 38–43 each add
one acceptance gate body to `scripts/live-phase4a.sh` (and Task 41 adds
`scripts/l4-canary-capture.sh`); Task 44 adds `scripts/phase4a-release-gate.sh`
and the release manifest. Gate names in the registry follow the plan's
`acceptance_NN_*` names so the later briefs slot in without touching the runner.
Within a case, "Verify RED" runs the still-placeholder/unmet precondition and
requires a FAIL before the case is implemented — the same RED discipline this
skeleton follows.
