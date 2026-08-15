# DNS record-limit research (Cloudflare free plan)

**Date:** 2026-08-15
**Context:** direct-TCP mode uses a wildcard A record per agent namespace on the
content domain `sharebridgeusercontent.com` — ~2 records/agent (`*.ns…` direct +
`*.relay.ns…` relay fallback). During the cert/DDNS spike we hit Cloudflare's
200-record free-tier limit and asked whether this is a real scaling constraint.

## Findings

- **Cloudflare:** Free = **200 records/zone** (for zones created after
  2024-09-01); Pro & Business = **3,500**; Enterprise = per-account (no per-zone
  cap). A wildcard `*.ns…` is a single RRset → counts as 1 record. API rate
  limit 1,200 req/5 min per token (a batch DNS API exists for bulk creates).
- **Our scheme:** ~2 records/agent → **~100 agents on Free**, ~1,750 on Pro.
- **Managed alternatives** for "many wildcards + programmatic DDNS":

| Provider | Record limit | Cost @ ~1k agents | Notes |
|---|---|---|---|
| Route53 | 10,000/zone included | ~$0.50/mo | excellent API, instant propagation |
| GCP Cloud DNS | no hard cap | ~$0.20/zone + queries | good API |
| Cloudflare Pro | 3,500 | $25/mo | zero code change |
| DNSimple | 100–1,000 | too low | reject |
| deSEC | no cap | $0 | min TTL 3600 (kills 60s DDNS), write rate limits |
| self-hosted PowerDNS/NSD | unlimited | $0 (own infra) | lose anycast/DDoS absorption + own DNSSEC; not worth it yet |

## Verdict

**Easy to solve — a config change behind the existing `ddns.Cloudflare`
abstraction, not a re-architecture.**

Recommendation (when we approach the 200-record ceiling): delegate/move the
content zone to **Route53** (~$0.50/mo, 10k records ≈ 5,000 agents) and add a
`ddns.Route53` backend implementing the same interface. GCP Cloud DNS is the
cheaper no-cap alternative (~$0.20/zone). Cloudflare Pro ($25/mo) is the
zero-code option if we'd rather pay than add a backend.

Rejected: self-hosted authoritative DNS (operational cost outweighs the ~$0.50/mo
of Route53); deSEC (min TTL 3600 defeats short-TTL DDNS); a single-wildcard +
SNI-gateway collapse (reintroduces the relay hop direct mode exists to remove).

**Non-urgent:** we are at 2 throwaway namespaces, nowhere near 200 records.
