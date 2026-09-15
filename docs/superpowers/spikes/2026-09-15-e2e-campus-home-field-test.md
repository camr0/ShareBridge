# End-to-end field test: campus browser → home agent (2026-09-15)

First real-world test of the v1 (WebRTC/relay) transport, run outside the lab. All prior
measurements were loopback with synthetic `netem` delay/loss.

## Topology

| node | location | network | role |
|---|---|---|---|
| Browser | <campus>, NJ | campus Wi-Fi | client (this is the "restrictive network") |
| Agent | <home-agent-host>, <home-city> NJ | home ISP, behind home NAT | v1 agent, host networking |
| Signaling + relay | Hetzner, Ashburn VA | public VPS | v1 signaling server + WebSocket/Noise relay |

Both peers are ~15 miles apart; the relay detours ~200 miles via Ashburn.

## Setup

- Signaling server: v1 `main` built for linux/amd64, systemd unit at `/opt/sharebridge-test`,
  fronted by Caddy with a real Let's Encrypt cert for `<test-signaling-host>`.
- Agent: v1 `main` + env-driven SCTP tuning, container `sharebridge-agent-e2e` on <home-agent-host>
  (host net, UI 127.0.0.1:7879 vs production 7878). Production agent untouched.
- Content: OpenCloud public share of `debian-13.4.0-amd64-netinst.iso` (754 MB), added
  manually via `POST /api/v1/shares` (OpenCloud/Nextcloud have no autodiscovery; Immich does).
- Two sessions on the same file: `relay_only=false` (direct) and `relay_only=true` (relay).

## Ceilings (measured first, to bound interpretation)

| measurement | Mbps |
|---|---|
| Campus Wi-Fi download (Cloudflare, same browser, same session) | **213** |
| Home upload (<home-agent-host> → Cloudflare, 2 runs) | **~200–224** |

Neither endpoint is near saturation, so any lower number is a transport result, not a link result.

## Results — 754 MB each, SHA-1 verified

| run | transport | avg | final |
|---|---|---|---|
| Direct, unpatched | pion SCTP | **49.6 Mbps** (6.2 MB/s) | `✓ intact` |
| Relay (`relay_only=true`) | kernel TCP over WebSocket/Noise | **96 Mbps** (12.0 MB/s) | `✓ intact` |
| Direct, `SB_SCTP_CA_STEP=32768` | pion SCTP | **93.6 Mbps** (11.7 MB/s) | `✓ intact` |

All three produced identical SHA-1 `a7e0206e573edbef0c4d8107a151271fbeccf2fe`.

## Findings

### 1. A direct P2P path forms from a campus network to a home server

The premise the whole WebRTC-direct architecture rests on — and the one thing never measured —
holds. ICE selected:

- browser: `srflx udp <campus-client-ip>:30478` (campus public IP)
- agent: `srflx udp <home-public-ip>:37789` (home public IP)
- pair type `srflx` ↔ `prflx`

That is a **STUN hole-punched** path: no TURN, no relay, **no port forwarding on the home
router**, no inbound firewall change. RTT 12–24 ms. Campus networks are frequently assumed to
block this; this one does not.

### 2. With default settings, the relay was ~2× faster than direct

v1 ships `DefaultRelayOnly: true` (`config.go:118`). That default was vindicated here: relayed
kernel TCP delivered **96 Mbps** where unpatched direct SCTP delivered **49.6 Mbps**. The direct
run also oscillated hard per-second (9.7 → 95.9 Mbps), the congestion-collapse signature seen in
the lab.

This is consistent with the loopback finding that pion's SCTP loses badly to kernel TCP, and it
means the July 2026 relay-default decision protects users from the slower path by default.

### 3. `cwndCAStep` closes the gap in the field — and needs no fork

Setting `SB_SCTP_CA_STEP=32768` lifted direct from **49.6 → 93.6 Mbps (1.89×)**, reaching parity
with the relay (96 Mbps) while keeping the direct path's advantages (no relay egress, no IP
exposure to a third party, no ~200-mile detour).

The mechanism is collapse-**recovery** speed, not the initial ramp. After an RTO pion sets
`cwnd = 1 MTU`; rebuilding a ~375 KB BDP (200 Mbps × 15 ms) at the default `+1 MTU/RTT` takes
~312 RTTs ≈ 4.7 s, versus ~12 RTTs ≈ 180 ms at `+32 KB/RTT`. That is why the step size matters
even at a low 15 ms RTT — it is the *number of RTTs* to refill the window that governs recovery
time, not the wall-clock RTT alone.

The change is a single already-public API call (`SettingEngine.SetSCTPCwndCAStep`) — **no pion
fork required**.

### 4. Host networking produces severe ICE candidate bloat

The agent advertised **~70 candidates**, almost all useless: every Docker bridge
(`172.17–172.31.x`, `10.253–10.254.x`, `192.168.0/16` subnets), the Tailscale address, and IPv6.
Only two mattered. ICE coped, but with a 10 s direct-connect timeout in
`connectTransferChannel.js` this is a real risk on slower paths — worth filtering to the default
route's interface.

## Caveats

- **n=1 per configuration.** The direct runs were visibly noisy; treat the specific averages as
  indicative. A repeated A/B would firm up the 42 → 94 Mbps claim (already n=3 unpatched at
  300 MB in the lab, which agreed).
- One day, one campus network, one home ISP, one RTT (~15 ms), one relay location. Campus
  networks and mobile/CGNAT will differ; a hostile network could still force relay.
- `DefaultRelayOnly=true` means most users never reach the direct path — the patch's value scales
  with how many users actually choose Direct.
- The ssthresh-cap fork (which the lab found helps small payloads) was **not** tested here; only
  the no-fork `cwndCAStep` knob was.
- The relay is co-located with the signaling server, so the relay test also exercised the
  signaling server's egress; server bandwidth is a real cost that direct mode avoids.

## Reproducing

Agent env for the patched run (`~/sharebridge-test/sb-test.env` on <home-agent-host>):

```
SHAREBRIDGE_API_KEY=<test key>
SHAREBRIDGE_AGENT_API_KEY=<local UI key>
SIGNALING_SERVER=wss://<test-signaling-host>
UI_PORT=7879
UI_ADDR=127.0.0.1
ALLOWED_SHAREBRIDGE_HOST=<opencloud-host>
NC_ALLOWED_SHAREBRIDGE_HOST=<opencloud-host>
SB_SCTP_CA_STEP=32768
```

Create a share:

```bash
curl -X POST http://127.0.0.1:7879/api/v1/shares \
  -H "X-API-Key: $SHAREBRIDGE_AGENT_API_KEY" -H 'Content-Type: application/json' \
  -d '{"share_url":"<opencloud share>","share_type":"opencloud",
       "expiry_hours":24,"max_downloads":50,"relay_only":false}'
```

Then open the returned `public_url`, click the filename, and read
`pc.getStats()` → `candidate-pair.currentRoundTripTime` and `transport.bytesReceived`.
