# Security Review — v2 Direct Mode, Privacy & DNS Design Notes

**Date:** 2026-09-02
**Context:** Post–Phase 3 review session. State reviewed: branch `v2` @ `700df65` (Phase 3 complete, live-e2e-verified). **v2 is direct-only today; Phase 4 (FRP relay + measure-and-prefer) is not started.** This doc captures the security audit of the v2 design, the CT/privacy analysis, the PSL investigation, and the on-demand-DNS design discussion — as input to Phase 4 scoping.

---

## 1. Audit Summary

**Verdict: sound, no blockers.** The design's defense layering is correct; findings below are residual risks to track, not stop-the-line issues.

### What's strong

- TLS terminates at the agent; key never leaves (CSR → exact-two-SAN enforcement at coordinator → `ValidateChain` on agent). Standard PKI replaces custom crypto — a net security upgrade.
- Agent never trusts the control plane for authorization: open signals only *activate* agent-registered, source-verified shares. Signals are versioned, expiring, idempotent, nonce+seq-bound, replay-protected, rate-limited, with per-connection-epoch `SignalGate` fencing.
- Site isolation: content on `sharebridgeusercontent.com` keeps a compromised agent out of control-plane cookies/storage.
- Closed-by-default port; UPnP mapping as on/off switch; short leases; `DeleteOwnedMapping` (never clobbers foreign mappings); fails-closed listing; lockdown removes mapping *and* closes in-flight TCP (correct: mapping removal alone doesn't kill established NAT flows).
- Phase 3 serving hygiene: strict CSP (`script-src 'self'`, zero-violation browser test), nosniff/no-store, `Content-Disposition: attachment`, filename sanitization, TTL'd archive tokens, per-share streaming semaphores, download accounting.

### Findings (ranked)

1. **Control-plane trust is the real threat model (§11.3) — and its mitigations aren't built.** Whoever holds Cloudflare + ACME creds can issue a cert for an agent namespace, repoint DNS, and MITM. Spec documents it; CT monitoring + CAA records + least-privilege DNS tokens are planned but deferred. **Recommendation: land CT monitoring + CAA on `sharebridgeusercontent.com` at the start of Phase 4** (control-plane-only work). Future enhancement worth considering: out-of-band cert-fingerprint pinning to recapture Noise's key-verification property.
2. **CT logs leak namespace → home IP (the census).** Detailed in §3 below.
3. **STUN cross-check deferred** (Phase 2 shipped nonce-echo + verified-tuple gating only). Until landed, a malicious agent can aim control-plane probes at arbitrary public `ip:port` — a small scanning/DoS primitive, bounded by rate limits. **Land with Phase 4.**
4. **Lax-lease routers:** some hold mappings until reboot. Crash + lax router = port stays mapped with nobody home to delete it. Residual exposure is just the Go TLS handshake surface (valid SNI origin required). Mitigation candidate: startup-time reconciliation of stale own-mappings.

---

## 2. Is uPnP a bad idea? Is v1 WebRTC more secure?

### uPnP: fine as used — with eyes open

The blanket "UPnP is bad" advice targets the router's *always-on posture* (invisible service, any LAN app can punch permanent holes, sloppy persistence). ShareBridge inverts that trade: mapping exists only inside an active session window, never clobbers foreign mappings, fails closed, lockdown deletes everything. Against the *manual-forward baseline* its users would otherwise run, on-demand mapping is a strictly smaller window. Against *no-forward-at-all*, it's more exposure — which is what `relayOnly` expresses.

Honest concession: the self-hosting community (ShareBridge's exact user base) disproportionately disables UPnP router-wide. That's an **availability** funnel, not a security failure — UPnP-off users currently get relay-only (503 until Phase 4).

- **Feature suggestion: manual-endpoint override** ("I forwarded 443/8443 myself — use it, don't touch UPnP"). `report_endpoint` plumbing exists; skip the mapping step, let the user's static forward play the same role. Gives UPnP-skeptics direct-mode bandwidth while respecting their posture. Verified: no manual-endpoint path exists today (UPnP/NAT-PMP is the only route to direct).
- Keep the ladder order PCP → NAT-PMP → uPnP IGD (cleaner protocols first; some routers allow disabling IGD only).

### v1 WebRTC vs v2: v2 is better overall

| | v1 (WebRTC + Noise Secure Relay) | v2 (native HTTPS) |
|---|---|---|
| Encryption | DTLS (browser-enforced) + Noise XX | TLS 1.3 (browser-enforced, public CA) |
| Relay reads data? | No (Noise ciphertext) | No (Phase 4 FRP = L4 passthrough, TLS still e2e) — equivalent property, standard mechanism |
| Auth vs network attacker | Noise XX key verification | PKI chain — equivalent strength |
| Auth vs **compromised control plane** | Pinned Noise static key from signed policy — stronger *in principle* (but policy delivered by control plane, so never fully zero-trust) | Weaker — CA + DNS are control-plane-held (Finding 1) |
| Custom crypto surface | Large: Noise framing, multilane, service-worker HTTP-over-DataChannel | Near zero — deleted |
| Verification story | Custom protocol + custom tests | Battle-tested TLS, CT logs, revocation, browser-enforced |

The v1 Noise pin was a nicer *theoretical* property against infrastructure compromise (Finding 1); the deleted custom-protocol surface was a much bigger real-world risk. Phase 4's L4 relay preserves v1's good property (relay never decrypts) with the standard mechanism.

---

## 3. The CT census (Finding 2, in depth)

**Chain:** every enrollment publishes `*.<ns>.sharebridgeusercontent.com` to public CT logs → one `crt.sh` query on `sharebridgeusercontent.com` lists the whole fleet → resolving any name under a wildcard returns the agent's home IP. Passive, legal, continuously refreshed by renewals.

**What the census does NOT give:** identities, open ports (closed by default), share origins (wildcards hide them). It yields "IPs likely running ShareBridge" — weaponizable only with external identity→IP correlation, or by waiting for an open window.

### Comparison with DuckDNS (the correct mental model)

Same shared-parent-domain structure; DuckDNS has the identical CT property — this is the generic cost of the shared-domain model, not a ShareBridge-specific flaw. Differences in degree:

| Dimension | DuckDNS | ShareBridge | Better? |
|---|---|---|---|
| Label choice | user-chosen, pseudonymous (`johnshomeserver.duckdns.org` — findable for a specific target) | random `sb`-hex — census is anonymous, unguessable | ShareBridge |
| Self-classification | "some home server, unknown purpose" | "a ShareBridge agent" — census pre-sorted; if a ShareBridge vuln lands, CT list = victim list | DuckDNS |
| Port posture | typically open 24/7, Shodan-indexed banner | closed by default | ShareBridge |
| PSL membership | yes (verified in `publicsuffix/list`) | no | DuckDNS |

Empirically: DuckDNS has run this model ~a decade with millions of CT entries without catastrophe — background risk of public hosting, not a re-architecture trigger.

### Mitigation stacking (decisions)

1. **On-demand DNS record** — see §5. Shrinks census to "actively sharing right now."
2. **Namespace rotation** — the spec's reserved knob (§7 "possible but infrequent"). The only mechanism that decouples *historical* CT entries from the present (old namespaces stop resolving; no public map to the new one). Costs one cert order + DNS swap per rotation; canonical links survive (they re-resolve), cached direct origin URLs break. Cert rotation *without* namespace change is a no-op (same SAN re-logged) — skip it.
3. **`relayOnly`** — no direct resolution ever. Exists; consider a first-run privacy choice with one-sentence IP-exposure disclosure.
4. **Document it.** Privacy claims should note CT makes namespace→IP half-public.

---

## 4. PSL — investigated, **parked**

**What it is:** publicsuffix.org (Mozilla) — defines where user-registrable names begin. Registrable domain = PSL entry + 1 label. PRIVATE section = operator-declared multi-tenant domains (`github.io`, `appspot.com`, `duckdns.org`).

**What listing would give ShareBridge:** (a) each agent namespace becomes its own LE rate-limit domain (50/week per namespace — shared-pool bottleneck vanishes, Google CA/ZeroSSL contingency mostly evaporates); (b) browser-enforced cookie/storage isolation between agents — becomes load-bearing when password-share session cookies land (Phase 3 auth seam future work).

**Why it's parked (from CONTRIBUTING.md, verified 2026-09-02):**
- **"Smaller, private projects with <2000 stakeholders"** are an explicit rejection reason — ShareBridge today would be rejected on sight. Scale milestone, not a now-task.
- **"Attempts to work around vendor limits"** are explicitly rejected (issue #1245). Applying with rate limits as headline rationale is the disfavored framing; the legitimate rationale is multi-tenant site isolation.
- Domain must be registered **>2 years from expiry** (check `sharebridgeusercontent.com` renewal horizon when the time comes), DNS validation records required, verbose rationale, strict formatting; no ETA, no guarantee of inclusion; entries are effectively permanent.

**Decision:** park until (a) past the ~2000-stakeholder credibility bar and (b) password sessions make structural isolation worth permanence. Enrollment scaling stays with the spike-doc answer: Route53 (~$0.50/mo, 10k records) behind the `ddns` abstraction. PSL does nothing about CT either way.

---

## 5. On-demand DNS record — design analysis

**Core distinction (repeated confusion risk): the certificate and the DNS A record are separate resources.**
- Cert: issued at enrollment (unchanged), SANs only — **contains no IP**. CT logs it permanently at issuance; rotation doesn't help, namespace rotation decouples history.
- A record `*.<ns>` → home IP: today created at enrollment and resolvable 24/7 forever. **This is the live pointer and the only fixable half of the census.**

**Proposal:** create the A record when a direct session window opens; remove it when the window closes. Cert timing untouched; **share creation stays instant** (no CA, no DNS op — preserves FRP §608 invariant; the rejected per-share-cert/per-share-DNS designs are a different axis entirely).

### Timing / cache semantics

- **TTL never affects first lookups.** Fresh query → authoritative → ~10–50 ms regardless of TTL. Instant-ness comes from **create-before-redirect** (control creates the record in parallel with `open_signal`, before the 302), not from cache settings. Record create ~100–300 ms hidden inside the existing 1–3 s UPnP wait behind the interstitial. TTL=0 buys nothing (no first-lookup benefit; kills reconnect resilience; some resolvers clamp tiny TTLs) — keep TTL 60 (also bounds staleness after mid-session IP change).
- **Negative caching is the real constraint — governed by SOA minimum, NOT the A-record TTL.** Verified on our zone 2026-09-02: `sharebridgeusercontent.com` SOA minimum = **1800s (30 min)** (Cloudflare; not editable on standard plans). A query for an absent record gets NXDOMAIN/NODATA cached up to 30 min.

### Flow analysis

- Canonical-link flow: **immune** (create happens before the browser's first query).
- In-session revisits: positive record, 60s TTL, same IP — fine.
- Deletion: ≤60s of connection-refused after close — fails closed, identical semantics to the existing UPnP lease expiry.
- **The one genuine edge:** user with a stale direct URL (bookmark/history/email preview) hits it while record absent → NXDOMAIN negative-cached up to 30 min → subsequent canonical-link retry may still resolve NXDOMAIN from *their resolver* despite a live session. **Mitigation: remove the record on the same inactivity window that closes the port** (5–15 min hold) so "record absent" coincides with "port absent" — no new failure mode, just the existing lapsed-port flow. Accept + document the residual.
- Churn is session-grained (one create per access window via the same epoch-aware hold as the port, coalesced across recipients), not per-request. Cloudflare API budget (1,200 req/5 min per token) has ~3 orders of magnitude headroom. DNS churn is milder than the UPnP churn already in production (stale DNS fails closed; stale NAT mapping is the dangerous case).
- Crash handling: agent crash → control-side GC (session-window timeout / heartbeat loss) removes the record; reverts to enrollment-era behavior otherwise.

### Sinkhole variant (upgrade path — better, more moving parts)

Never delete — **flip the value.** Idle: `*.<ns>` → control-plane IP that TCP-resets everything (census shows the VPS, never a home IP). Session: flip value to agent IP. Because the name always resolves positively, **negative caching disappears entirely**; stale-URL users get instant connection-refused and can immediately retry canonical.

Trade-offs:
1. Records exist permanently → per-agent record count returns → Cloudflare 200-record cap binds → **Route53 migration becomes required, not optional** (needed at scale anyway per spike 2026-08-15).
2. **The sinkhole must stay a dumb TCP-refuser — never terminate TLS there.** A control-plane-held cert for the wildcards would structurally rebuild the §11.3 compromised-control-plane MITM path as a permanent fixture. RST at TCP layer, no cert.

### Recommendation

Spike **delete/re-add + create-before-redirect, TTL 60, record-removal tied to the port's inactivity hold** in Phase 4. Upgrade to the sinkhole variant only if telemetry shows the stale-URL edge is a real population — and let that upgrade double as the Route53 trigger.

---

## 6. Phase 4 scope candidates (from this review)

| Item | Type | Priority |
|---|---|---|
| CT monitoring + CAA on `sharebridgeusercontent.com` | control-plane only | **start of Phase 4** (cheap, closes Finding 1) |
| STUN cross-check for reachability probe | closes Finding 3 | with Phase 4 |
| On-demand DNS spike (delete/re-add) | privacy | Phase 4 candidate (§5) |
| Manual-endpoint override (UPnP-off users) | availability + user trust | Phase 4 candidate, small |
| Namespace rotation | privacy knob | reserved, infrequent (spec §7) |
| Sinkhole DNS variant | privacy upgrade | only on telemetry trigger; bundles Route53 |
| PSL application | rate limits + isolation | **parked** until ~2000 stakeholders + password sessions |
| Startup reconciliation of stale own-mappings | hardening | nice-to-have |

## 7. Housekeeping

Untracked in working tree: `signaling-server/` (216K stale `pb_data` + `redeploy.sh` after the `control/` rename — delete), `.claude/`, `.codex/`, `extensions/`, `package-lock.json` (triage: gitignore vs commit).
