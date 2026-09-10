# Phase 4a test-VPS deployment — relay + STUN (operations runbook)

Zero-context runbook for deploying the **phase-4a DEV/TEST relay stack** to the
existing test VPS and wiring the two user-assisted gates that depend on it:

- **Task 24 Phase B** — real Safari / iOS Safari checklist
  (`e2e/browser/PHASE-B-SAFARI.md`, §23.4).
- **Task 25** — real-NAT authenticated STUN gate
  (`scripts/stun-nat-gate.sh --target remote`, §23.5).

This is the collocated deployment explicitly allowed by spec §4.6 for
development: control, the SNI relay gateway and frps share **one box**
(“second bound IP or nonstandard public port”). Production acceptance
(separate relay VM, gateway owning public 443) is M6 and out of scope here.

Deploy tool: `control/deploy-testing-relay.sh` (extends the phase-3
`control/deploy-testing.sh` pattern). The deployment is **idempotent** —
re-running it upgrades the box in place, preserving `pb_data`.

---

## 1. Topology on the test VPS (one box)

| Process (systemd unit)             | Listens on                          | Purpose |
|------------------------------------|-------------------------------------|---------|
| `sharebridge.service`              | `:8080` plain HTTP + UDP `3478`     | control (signaling, interstitial, STUN listener) |
| `sharebridge-relay-gateway.service`| `:443` TCP + `127.0.0.1:9001` TCP   | SNI gateway (TLS passthrough) + FRP authorization plugin (loopback-only, enforced by the binary) |
| `sharebridge-relay-frps.service`   | `:<transport>` TCP (default `7000`) + `127.0.0.1:<10000-10099>` TCP | pinned frps v0.71.0; transport TLS; proxy ports are loopback-only (§15.1) |

UFW (if active): `443/tcp`, `3478/udp`, `<transport>/tcp`, `8080/tcp`. SSH is
never touched by the script.

Key cross-machine facts:

- **Agents’ frpc** connects to `<RELAY_GATEWAY_HOST>:<RELAY_GATEWAY_PORT>`
  with **verified TLS** (`transport.tls.serverName = <RELAY_GATEWAY_HOST>`)
  against a CA bundle the operator copies to the home server
  (`SHAREBRIDGE_RELAY_CA_FILE`). The deploy script issues that CA + server
  cert locally (SAN = `RELAY_GATEWAY_HOST` + VPS IPv4).
- **Browsers** reach relay origins at
  `<label>.relay.<namespace>.<base-domain>` (:443, SNI passthrough to the
  agent). Control provisions the per-namespace wildcard A record →
  `RELAY_GATEWAY_IPV4` at enrollment.
- **frps ↔ gateway plugin**: frps calls
  `http://sharebridge-frps:<secret>@127.0.0.1:9001/frp/authorize` — same box,
  loopback only, secret from `.env.testing`.

---

## 2. Prerequisites

- **VPS SSH access**: `ssh <user>@<host>` with sudo/root (the phase-3 test
  deploy used root; the units run without a `User=` directive).
- **Cloudflare**: an API token with **DNS edit** on the **test base domain
  zone** (`CONTENT_BASE_DOMAIN`). It is used for direct-mode DDNS/ACME
  DNS-01 *and* for the enrollment-time `*.relay.<namespace>.<base-domain>`
  wildcard record — the relay subzone lives in the same zone. Also verify
  the §17.2 DNS invariant: no HTTPS/SVCB (ECH) records on content names;
  content records stay DNS-only.
- **Local tooling** (on the machine you run the deploy from): Go, `openssl`,
  `curl`, `python3`, `ssh`/`scp`, `tar`, `shasum` or `sha256sum`.
- **Snapshot state**: the test VPS is restored from a **phase-3-era
  snapshot**: `/opt/sharebridge/{server,.env,web/,pb_data/}` plus the old
  `sharebridge.service`. That is the expected starting point — see §4
  (upgrade-in-place). A fresh box also works (§4b).
- **Secrets file**: `cp control/.env.testing.example control/.env.testing`
  and fill in real values (the template documents every key; values never go
  into committed files or chat logs).

### `.env.testing` keys (names only — see the example file for full comments)

Required: `CLOUDFLARE_TOKEN`, `RELAY_GATEWAY_HOST`, `RELAY_GATEWAY_IPV4`,
`RELAY_AUTH_KEY_SEED` (64 hex chars; **keep stable across re-runs**),
`SHAREBRIDGE_FRP_PLUGIN_SHARED_SECRET` (hex).

Optional (defaults in the template): `CONTENT_BASE_DOMAIN`, `ACME_EMAIL`,
`ACME_CA_DIR`, `PORT`, `RELAY_GATEWAY_PORT`, `RELAY_PORT_MIN`,
`RELAY_PORT_MAX`, `STUN_ADVERTISE_ADDR`, `RELAY_SELECTION_ENABLED`.

The relay transport CA lands in `control/.env.testing.d/` (gitignored via the
`.env.*` pattern): `relay-transport-ca.pem` (+ key). **Keep the CA stable** —
the script reuses it on re-runs so a CA already copied to the home server
never goes invalid.

---

## 3. Deploy

```bash
cd control
./deploy-testing-relay.sh root@<vps-host> --bootstrap
```

What it does, in order: validates `.env.testing` (seed/secret charset, port
collisions, IPv4/hostname shape) → derives the gateway’s Ed25519 verification
key from `RELAY_AUTH_KEY_SEED` (the seed is piped to a throwaway Go helper
over **stdin**, never on an argv element, so it is invisible to `ps`) →
cross-compiles control + gateway static Linux binaries → downloads the
**pinned frps v0.71.0** and verifies its SHA-256 against
`relay/frp/manifest.json` (fails closed on mismatch) → ensures the transport
CA and issues a fresh server cert → `scp`s binaries, web assets, certs →
writes remote `.env`, `relay-gateway.env`, `frps.toml` (over ssh stdin;
secrets never on a command line) → installs and **restarts** all three
systemd units → applies UFW rules (only if ufw is already active) → prints
on-box service/listener evidence and a verification checklist.

`--bootstrap` additionally creates a throwaway user + API key and writes the
agent env block (including `CONNECT_ALLOWED_ORIGIN`, the CA copy line and
the ready-made Task 25 gate command) to
`.env.testing.d/bootstrap-<host>.env` with mode 0600. Only the file path is
printed — the secrets never appear in terminal scrollback.

`--teardown` stops and disables all three units (binaries, `pb_data`,
`relay-data` and configs stay on the box; delete the box in the cloud
console to stop billing).

### What the generated frps.toml enforces (§15.1)

`proxyBindAddr = 127.0.0.1` (proxy ports never public — the collocated
gateway is their only client), `allowPorts = [{start = RELAY_PORT_MIN,
end = RELAY_PORT_MAX}]`, `maxPortsPerClient = 1`, `userConnTimeout = 10`,
`transport.tcpMux = false`, `transport.heartbeatTimeout = 45`,
`transport.tls.force = true` with the issued cert/key, and the **mandatory**
authorization plugin pointing at the gateway’s loopback endpoint with ops
`Login/NewProxy/CloseProxy/Ping/NewUserConn`. No dashboard is configured.
This is the config shape proven by `relay/internal/frptest` (real frps
v0.71.0 gates), with only the binds adapted to the collocated box.

### Upgrade-in-place from the phase-3 snapshot (the normal path)

The script **replaces** `/opt/sharebridge/server`, `web/*` and `.env`, and
reinstalls the `sharebridge.service` unit. It does **not** touch
`/opt/sharebridge/pb_data` (your phase-3 users/keys survive). The old
phase-3 `.env` line `RELAY_JWT_SECRET` disappears with the rewrite — current
config code does not read it (stale v1-era name). The new gateway/frps units
are additive. You do not need to clean anything by hand first; if the box
was snapshotted mid-deploy, just re-run the script.

### 4b. Fresh-box path (only if the snapshot is gone)

1. Create the VM, note its public IPv4, SSH in once (`sudo ufw` state will
   be whatever the image ships; the script only *adds* rules when ufw is
   active).
2. Put `root@<new-ip>` into the deploy command. Everything else is identical
   — there is no phase-3 state to preserve, so `pb_data` starts empty and
   `--bootstrap` creates the first throwaway user/key.

---

## 5. Agent side (home server)

The home server runs the agent from Docker via `agent/redeploy.sh`
(rsync → `docker compose build --no-cache` → `up -d`).

1. **Stage the pinned linux frpc** into the agent build context — the
   Dockerfile copies `frpc` from the build context and its checksum is
   enforced in release packaging (`relay/frp/manifest.json`, v0.71.0):
   - Easiest: the deploy script already verified the linux_amd64 tarball —
     take `relay/.cache/frp/deploy-testing-relay/v0.71.0-sha256-<digest>/frpc`
     (see the script’s NOTE line for the exact path) from the machine you
     deployed from, and copy it to `agent/frpc` in your repo checkout.
   - Or run `relay/scripts/fetch-frp.sh` **on the home server** (Linux) and
     copy `frpc` out of its cache into `agent/`.
   For an arm64 home server use the `linux_arm64` artifact instead — verify
   the digest yourself against `relay/frp/manifest.json`.
2. **Set the agent env** (compose reads `agent/.env` for variable
   substitution — copy these from the `--bootstrap` env file,
   `.env.testing.d/bootstrap-<host>.env`):

   ```
   SIGNALING_SERVER=ws://<vps-host>:8080
   SHAREBRIDGE_API_KEY=<from the --bootstrap env file>
   CONNECT_ALLOWED_ORIGIN=http://<vps-host>:8080
   UI_ADDR=127.0.0.1
   # UI_PASSWORD=<admin UI password — required iff UI_ADDR is non-loopback>
   ```

   The agent admin UI is **loopback-only by default** (`UI_ADDR` defaults to
   `127.0.0.1`). Because `agent/docker-compose.yml` uses
   `network_mode: host`, that means the UI is reachable only from the home
   server itself. Binding it to a non-loopback address (`0.0.0.0`, a LAN IP,
   a hostname) requires `UI_PASSWORD`: without it the agent **refuses to
   start**. See `docs/operations/agent-admin-ui-security.md` for the full
   posture, the breaking-change note and the TLS/tunnel guidance before
   exposing the UI beyond the host.

   `CONNECT_ALLOWED_ORIGIN` must be the **exact** interstitial origin of the
   test deployment (`http://<vps-host>:8080`): the agent’s `/connect` CORS
   check compares byte-exactly against this value. It is a test-only
   override — unset (production) keeps the `https://sharebridge.app`
   default; a malformed value fails closed to that same default at config
   load.

3. **Copy the relay transport CA into the container’s data volume.** The
   agent verifies frps transport TLS against `tunnel_ca_file` from its
   `config.json` (persisted in the `sharebridge-agent-data` volume). The
   compose file currently does not forward `SHAREBRIDGE_RELAY_CA_FILE`, and
   `agent/redeploy.sh` rsync-overwrites local compose edits, so the durable
   path is the volume:

   ```bash
   scp <deploy-machine>:control/.env.testing.d/relay-transport-ca.pem .
   cd ~/sharebridge/agent   # wherever docker-compose.yml lives
   docker compose down      # no concurrent config.json writes
   VOL=$(docker volume ls --format '{{.Name}}' | grep sharebridge-agent-data)
   docker run --rm -v "$PWD":/host -v "$VOL":/data alpine sh -c '
     apk add --no-cache jq >/dev/null
     cp /host/relay-transport-ca.pem /data/relay-transport-ca.pem
     jq ". + {\"tunnel_ca_file\": \"/root/.sharebridge/relay-transport-ca.pem\", \"base_domain\": \"<CONTENT_BASE_DOMAIN>\", \"config_version\": 2}" \
       /data/config.json > /data/config.json.tmp && mv /data/config.json.tmp /data/config.json'
   docker compose up -d
   ```

   `tunnel_ca_file` is the JSON tag of `Config.TunnelCAFile` and
   `base_domain` of `Config.BaseDomain` (`agent/internal/config/config.go`).
   `base_domain` must be the **test base domain** so the agent’s certificate
   manager and origin derivation match control. Re-check after every
   redeploy that the volume contents are still intact (they persist; only
   the image is rebuilt).

4. **Redeploy**: from your checkout, `cd agent && ./redeploy.sh`.
5. **DIRECT-capable port mapping**: for direct-path shares the home network
   needs a public TCP mapping to the agent’s loopback HTTPS listener
   (127.0.0.1:8443) — the existing phase-3 port-forward. For the `NON443`
   share make sure the mapped **external** port is **not** 443 (§7).

---

## 6. Post-deploy verification gates (in order)

Run these top to bottom; do not skip ahead. `<vps-host>` is the ssh target,
`<transport>` the configured `RELAY_GATEWAY_PORT`.

1. **Services + listeners on-box** — the deploy prints this evidence:
   `systemctl is-active sharebridge sharebridge-relay-gateway
   sharebridge-relay-frps` all `active`; `ss -ltn` shows `:443`,
   `:<transport>`, `:8080`; `ss -lun` shows `:3478`; `ss -ltn | grep 9001`
   shows **`127.0.0.1:9001`** (never `0.0.0.0`).
2. **Control health**: `curl -sf http://<vps-host>:8080/_/` answers
   (PocketBase health route used by the deploy script).
3. **Agent enrollment succeeds**: with the agent up (§5), the agent log
   shows the enrollment handshake completing and control’s log
   (`ssh <vps> journalctl -u sharebridge -f`) shows the WS connect; control
   must log its STUN line at startup
   (`stun observation listener on 0.0.0.0:3478 (udp/3478)`).
4. **Relay presence + DNS**: after enrollment, control provisions the
   namespace wildcard —
   `dig +short '*.relay.<namespace>.<CONTENT_BASE_DOMAIN>'` must answer
   `RELAY_GATEWAY_IPV4`. Absent/invalid `RELAY_GATEWAY_IPV4` keeps baseline
   readiness unreachable (§7.1), so this gate also proves the relay policy
   env is live.
5. **STUN reachable from the home network** (Task 25 precheck): from the
   home server, `nc -uvz <STUN_ADVERTISE host> 3478` — and the real check is
   the agent’s own gating: control issues `stun_challenge` immediately after
   enrollment (§10.2); a successful exchange appears in control/agent logs.
   `nc` from *inside* the VPS proves nothing (loopback).
6. **Interstitial live in a real browser**: create a share and open
   `http://<vps-host>:8080/s/<code>` — the no-store “Preparing your share…”
   interstitial renders with the Task 22 assets. (Plain HTTP on :8080 is the
   phase-3 test posture; content origins are still HTTPS.)

---

## 7. Fabricating the four Phase B test shares

The Phase B checklist needs one share of each kind, from the enrolled agent
(agent UI → create share, pick the mode; note each share code):

| Checklist code | Share to fabricate | How |
|----------------|--------------------|-----|
| `<DIRECT>` | direct candidate | The existing phase-3 port-forward to the agent (mapped external port may be 443 or any). Agent must be STUN-match fresh — it will be, per §6.5. |
| `<NON443>` | non-443 direct | Same network, but the router’s mapped **external** port ≠ 443 (e.g. forward external 8443 → agent 8443). |
| `<RELAYONLY>` | relay-only | Create the share with the **Relay** mode toggle in the agent share form (`relay_only=true`). |
| `<BLACKHOLE>` | direct + silent drop | While the PnP mapping is live, make the mapped **external** port DROP (not REJECT) — options below; revert after the case. |

BLACKHOLE options (pick one; revert after the case). The mapped external
port is the one the agent reports (visible in control logs /
`report_endpoint`), not a static forward:

- **Router firewall**: WAN inbound DROP rule on the currently mapped external
  port. Nothing may answer — no RST, no ICMP-refused.
- **Host iptables/nftables on the home server**:
  `sudo iptables -I INPUT -p tcp --dport <ext-port> -j DROP` **and**
  `sudo iptables -I FORWARD -p tcp --dport <ext-port> -j DROP` (DNAT’d
  traffic traverses FORWARD). Remove with `-D` in reverse order.
- **Docker**: if the mapping is published by Docker, add a
  DOCKER-USER chain drop:
  `sudo iptables -I DOCKER-USER -p tcp --dport <ext-port> -j DROP`.

`<NS>` for the checklist is the share’s namespace domain:
`<namespace>.<CONTENT_BASE_DOMAIN>` (the part after the first label of the
direct origin, e.g. `sb0a1b2c3.test.example.com`).

> **Wiring status caveat (read before running Phase B):** cases 2.2, 2.3,
> 2.4 and 2.8 need a **live relay tunnel + presence lease** so the
> interstitial renders the relay URL / “Use relay now” / noscript redirect.
> The daemon↔tunnel-manager wiring (`agent/internal/tunnel`) lands with
> plan **Task 28**; until then the deployed agent will not supervise frpc,
> no presence lease exists, and the interstitial correctly offers no relay
> button (fail-closed §9.1). The direct-path cases (2.1, 2.5, 2.6, 2.7) are
> executable regardless. Verify the current wiring state before scheduling
> the human checklist session.
>
> The same gap blocks **creating** the `<RELAYONLY>` share: the daemon
> currently rejects relay-only sessions at `agent/internal/daemon/daemon.go`
> (phase-3 enforcement, lifted by Task 28). Fabricate `<DIRECT>`, `<NON443>`
> and `<BLACKHOLE>` now; create `<RELAYONLY>` after Task 28 lands.

---

## 8. Task 25 wiring (real-NAT STUN gate)

Run from the **home network** (the NAT under test), with the home server as
the gate client. The gate script and its seven cases:
`scripts/stun-nat-gate.sh` (see its header for the full contract).

```bash
export STUN_GATE_API_KEY=<agent API key — copy from the --bootstrap env file
                             .env.testing.d/bootstrap-<host>.env>  # env-only: never a flag/argv
scripts/stun-nat-gate.sh --target remote \
  --server ws://<vps-host>:8080 \
  --stun-addr <STUN_ADVERTISE_ADDR> \
  --expected-public-ip <home-network-egress-IPv4>
```

- `STUN_GATE_SERVER` / `STUN_GATE_STUN_ADDR` /
  `STUN_GATE_EXPECTED_PUBLIC_IP` are the env forms of the three flags (the
  script exports them internally).
- `--expected-public-ip` = your home network’s current egress IPv4
  (`curl -s https://api.ipify.org` from the home network) — the
  `mismatched-egress` case asserts the §10.3 stop when the observed address
  differs.
- **Fast path first:** if an agent cert is already enrolled on this control,
  set `STUN_GATE_CERT_FINGERPRINT=<fingerprint of the persisted agents-row
  cert>` to skip the CSR step. **Without it**, the gate’s remote client
  falls back to the CSR flow, which triggers **real certificate issuance**
  against the test zone’s ACME account — acceptable here, but avoid the
  churn: prefer the fingerprint fast path.
- The two happy-path cases wait ~4m15s for the §10.2 rechallenge by default
  (`STUN_GATE_WAIT_RECHALLENGE=1`); that delay is the wire-visible proof of
  acceptance — don’t “fix” it with `--no-wait-rechallenge` for evidence runs.
- Evidence lands in
  `docs/operations/evidence/phase4a-stun-nat-gate.md` (§23.5; all seven
  cases PASS required for the blocking GO).

---

## 9. Phase B wiring (real Safari / iOS checklist)

Open `e2e/browser/PHASE-B-SAFARI.md` and apply these substitutions for the
**test deployment**:

| Checklist placeholder | Test value |
|------------------------|------------|
| `https://sharebridge.app` (canonical share URL base) | `http://<vps-host>:8080` |
| `<NS>` | `<namespace>.<CONTENT_BASE_DOMAIN>` |
| `<DIRECT>` / `<NON443>` / `<RELAYONLY>` / `<BLACKHOLE>` / `<PARKED>` | codes from §7 |
| expected ACAO + ACAO/CSP origin in cases 2.6–2.7 (`https://sharebridge.app`) | `http://<vps-host>:8080` — the agent runs with `CONNECT_ALLOWED_ORIGIN=http://<vps-host>:8080` (§5); test-only override, production keeps the `https://sharebridge.app` default |

Device prerequisites: one macOS Safari and one iOS Safari, each on its own
normal network (iOS: Wi-Fi; optionally cellular for extra coverage), same
clock sanity, ~10 s screen auto-lock disabled for timing case 2.2, and
network reachability to the VPS on `8080/tcp`, `443/tcp`,
`<transport>/tcp`, `3478/udp`. Record versions/network per checklist §1.

Re-read the §7 wiring-status caveat before starting: relay-origin cases are
only meaningful once the agent tunnel supervision is live. Record NO-GO
honestly per the checklist’s GO/NO-GO rule — do not reclassify a relay
failure as “environment”.

Save the filled checklist + screenshots as the §23.4 Phase B evidence.

---

## 10. Snapshot / rollback

- **Rollback asymmetry is deliberate**: a restore of the *old* (phase-3)
  image brings back a control without the relay policy env/gateway units —
  relay becomes **permanently unavailable** on that box until this script is
  re-run. Phase-3 behavior (users, keys, direct shares) keeps working from
  `pb_data`.
- **Snapshot AFTER a good deploy** (all §6 gates green): that snapshot is
  your fast rollback point with relay intact. Cloud console snapshot before
  risky experiments, restore in one click, re-run
  `deploy-testing-relay.sh` if the snapshot predates any env change.
- Agents self-heal after control/gateway/frps restarts (fresh credentials on
  reconnect, §7.2/§15.2) — no agent-side action needed for redeploys.

## 11. Teardown

```bash
./deploy-testing-relay.sh root@<vps-host> --teardown
```

Stops + disables `sharebridge-relay-frps`, `sharebridge-relay-gateway`,
`sharebridge`. Files under `/opt/sharebridge` (including `pb_data`) remain —
delete the box in the cloud console to stop billing. UFW rules are left in
place (harmless on a deleted box; remove with `ufw delete allow ...` if you
keep the VM).
