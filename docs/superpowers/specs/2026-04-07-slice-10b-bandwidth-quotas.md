# Slice 10b — Bandwidth Tracking + Quota Enforcement

## Goal

Add per-account bandwidth tracking and quota enforcement for relay traffic. Prepare infrastructure for BYOK TURN and tiered plans in Slice 10c.

**Important limitation:** This implements "soft quotas" — new relay sessions are blocked when quota is exceeded, but existing active sessions continue until their TURN credentials expire (up to 24 hours). This is a fundamental limitation of how TURN credential validation works.

---

## Architecture

```
┌─────────────┐     ┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│   Account   │────<│  API Keys   │────<│  Sessions   │────<│   Coturn    │
│             │     │             │     │             │     │             │
│  Quota:     │     │             │     │  relay_only │     │  HMAC auth  │
│  50GB/30d   │     │             │     │  expires_at │     │  per-account│
│  used: 32GB │     │             │     │             │     │             │
└─────────────┘     └─────────────┘     └─────────────┘     └──────┬──────┘
       │                                                           │
       │                    ┌──────────────┐                       │
       └───────────────────>│  Prometheus  │<──────────────────────┘
                            │  (required)  │
                            └──────────────┘
```

**Note:** Prometheus is required for quota enforcement. Self-hosted deployments that don't want quotas can run without it (no bandwidth limits).

---

## Design Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Quota exceeded | Soft stop (new sessions blocked) | Existing TURN sessions continue until credential expiry; see [Future hardening](#future-hardening) |
| Quota period | Fixed 30-day period | From account creation date; aligns with annual billing |
| Quota tiers | Single free tier (50GB) | Pro tier with Stripe in Slice 10c; BYOK deferred to Slice 10c |
| Prometheus | Required for quotas | Self-hosted without Prometheus = no bandwidth limits (acceptable for homelab) |
| Tracking method | Prometheus metrics | `turn_traffic_sent{username="account_id"}` per-account scoped |

---

## Prometheus + Coturn Integration

### Account-Scoped TURN Credentials (Always)

TURN credentials now use account-scoped usernames (not per-session):

```go
// Before (per-session, unlimited usernames → Prometheus memory leak risk)
username := fmt.Sprintf("%d:%s", expiry.Unix(), sessionCode)

// After (per-account, bounded usernames)
username := fmt.Sprintf("%d:%s", expiry.Unix(), accountID)
```

The browser/agent doesn't care about the username format - it just uses what we provide. Using account-scoped usernames:
- Prevents Prometheus memory leaks (bounded cardinality)
- Simplifies code (one code path, not two)
- Allows per-account bandwidth tracking when quotas are enabled
- Per-session tracking happens at the agent level (peer metrics) if needed

**Tradeoff:** Revoking an API key prevents new TURN credential issuance but does not immediately terminate existing relay sessions (TURN creds remain valid until expiry). For abuse response, this stops the bleeding within credential TTL (24 hours).

### Prometheus Configuration

```yaml
# docker-compose.yml additions
services:
  prometheus:
    image: prom/prometheus:latest
    volumes:
      - ./prometheus.yml:/etc/prometheus/prometheus.yml
      - prometheus-data:/prometheus
    ports:
      - "9090:9090"  # Optional: only expose if you want UI access
    command:
      - '--config.file=/etc/prometheus/prometheus.yml'
      - '--storage.tsdb.retention.time=35d'  # 35 days covers full month + buffer

  coturn:
    # ... existing config ...
    command: >
      -n
      --log-file=stdout
      --min-port=49152
      --max-port=65535
      --realm=sharebridge
      --use-auth-secret
      --static-auth-secret=${TURN_SECRET}
      --prometheus
      --prometheus-port=9641
      --prometheus-username-labels
      --denied-peer-ip=10.0.0.0/8
      --denied-peer-ip=172.16.0.0/12
      --denied-peer-ip=192.168.0.0/16
      --denied-peer-ip=169.254.0.0/16
      --denied-peer-ip=127.0.0.0/8
      --denied-peer-ip=0.0.0.0/8
      --denied-peer-ip=::1/128
```

**prometheus.yml:**
```yaml
global:
  scrape_interval: 60s

scrape_configs:
  - job_name: 'coturn'
    static_configs:
      - targets: ['coturn:9641']
```

### Bandwidth Aggregation

The signaling server polls Prometheus for per-account bandwidth usage since the start of the current quota period:

```promql
increase(turn_traffic_sent{username="account_id"}[$QUOTA_PERIOD_DURATION] offset $TIME_SINCE_PERIOD_START)
```

Or simpler: server queries Prometheus once per minute and maintains a running counter, resetting it at period boundaries.

**Implementation:**
- Store `quota_period_start` in database
- Poll Prometheus every `QUOTA_CHECK_INTERVAL` for usage since `quota_period_start`
- Update `current_period_usage_gb` in PocketBase
- At period end, archive to `bandwidth_usage` and reset counter

---

## Database Schema (PocketBase)

### `users` collection (extends PocketBase `users`)

| Field | Type | Notes |
|-------|------|-------|
| `relay_quota_gb` | Number | Default: 50; 30-day relay quota in GB |
| `current_period_usage_gb` | Number | Updated periodically from Prometheus |
| `quota_period_start` | Date | Start of current 30-day period |
| `quota_period_end` | Date | End of current 30-day period (start + 30 days) |

### `bandwidth_usage` collection (audit log)

| Field | Type | Notes |
|-------|------|-------|
| `account_id` | Relation → users | |
| `period_start` | Date | Start of 30-day period |
| `period_end` | Date | End of 30-day period |
| `bytes_transferred` | Number | Total bytes for the period |
| `updated_at` | Date | Last Prometheus poll |

---

## API Changes

### Browser WebSocket join (`/ws/client`)

When the browser connects, the server checks quota after the WebSocket upgrade:

- **Quota OK:** send ICE config with STUN + TURN credentials as normal
- **Quota exceeded:** send a `relay_quota_exceeded` WS message, then send ICE config with STUN only (no TURN credentials)

The browser can display the quota message immediately. If the direct connection also fails, the user already has a clear explanation of why relay was unavailable.

**Note:** IP protection for relay-only shares is enforced on the agent side via `ICETransportPolicyRelay` — the agent never emits host/srflx candidates regardless of what credentials the server issues to the browser. The server does not need to know whether a session is relay-only.

**WS message when quota exceeded:**
```json
{
  "type": "relay_quota_exceeded",
  "message": "Relay quota exceeded (52.3 GB used). Resets after Feb 14.",
  "usage_gb": 52.3,
  "quota_gb": 50,
  "period_end": "2026-02-14T10:30:00Z"
}
```

### `GET /api/account` (extended)

```json
{
  "id": "abc123",
  "email": "user@example.com",
  "quota": {
    "limit_gb": 50,
    "used_gb": 32.5,
    "remaining_gb": 17.5,
    "period_start": "2026-01-15T10:30:00Z",
    "period_end": "2026-02-14T10:30:00Z",
    "percentage_used": 65
  }
}
```

---

## UI Changes

### Account Settings Page (`/account`)

**Quota Card:**
```
┌─────────────────────────────────────────┐
│  Relay Usage (30-day period)            │
│                                         │
│  [████████████░░░░░░░░░░] 65%           │
│  32.5 GB used / 50 GB limit             │
│  Resets in 12 days                      │
└─────────────────────────────────────────┘
```

### Share Creation Form (agent UI)

When "Relay Mode" selected:

```
┌─────────────────────────────────────────┐
│  ⚠️  Relay Mode (hide your IP)          │
│                                         │
│  [x] Use relay server                   │
│                                         │
│  Quota: 32.5 GB / 50 GB used            │
│  [████████████░░░░░░░░░░]               │
│                                         │
│  Estimated remaining shares: ~8         │
└─────────────────────────────────────────┘
```

If quota exceeded:
```
┌─────────────────────────────────────────┐
│  ⚠️  Relay Mode (hide your IP)          │
│                                         │
│  ❌ Relay quota exceeded                │
│  Direct mode available (unlimited)      │
│                                         │
│  [Switch to Direct Mode]                │
└─────────────────────────────────────────┘
```

---

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `DEFAULT_QUOTA_GB` | `50` | Default 30-day quota for new accounts |
| `PROMETHEUS_URL` | `http://prometheus:9090` | Prometheus query endpoint (required when TURN is configured) |
| `QUOTA_CHECK_INTERVAL` | `5m` | How often to poll Prometheus |

**Note:** Quotas are always enforced when TURN is configured. If you don't want quotas, don't run a TURN server — direct-only mode works fine for private homelabs. Running a public TURN server without bandwidth limits is insecure.

---

## TURN Security — SSRF Protection

Without IP filtering, an attacker with a valid TURN credential can use Coturn as an HTTP proxy to reach internal networks and cloud metadata services (e.g., `169.254.169.254` on AWS/Hetzner). The 50 GB quota and 24-hour TTL limit damage but don't block access during the active window.

**Defense-in-depth fix:** Coturn's `--denied-peer-ip` flag blocks TURN from relaying to specified IP ranges. Add this to the Coturn command:

```yaml
coturn:
  command: >
    ...
    --denied-peer-ip=10.0.0.0/8
    --denied-peer-ip=172.16.0.0/12
    --denied-peer-ip=192.168.0.0/16
    --denied-peer-ip=169.254.0.0/16
    --denied-peer-ip=127.0.0.0/8
    --denied-peer-ip=0.0.0.0/8
    --denied-peer-ip=::1/128
```

This blocks:
- RFC 1918 private ranges (internal services, databases, admin panels)
- Link-local `169.254.x.x` (cloud instance metadata on AWS, GCP, Hetzner, Azure)
- Loopback

**What this doesn't fix:** Port exhaustion (many concurrent allocations) and long-lived credential abuse. Those are deferred to Slice 12 (hardening).

---

## Implementation Tasks

### Task 1: Update TURN credentials to be account-scoped
- Modify `turn/credentials.go` to accept `accountID` instead of `sessionCode`
- Update call sites in `handler/browser_ws.go`

### Task 2: Add Prometheus metrics scraping
- Create `internal/metrics` package
- Poll `turn_traffic_sent` metric every `QUOTA_CHECK_INTERVAL`
- Update `users.current_period_usage_gb` in PocketBase

### Task 3: Add quota enforcement middleware
- Check quota before returning TURN credentials
- Return 403 with descriptive error if exceeded
- Document: existing relay sessions continue (soft quota)

### Task 4: Extend PocketBase collections
- Add fields to `users` collection for quota tracking
- Create `bandwidth_usage` collection for audit log

### Task 5: Update account dashboard UI
- Add quota progress bar
- Show quota info in share creation form

### Task 6: Update docker-compose for Prometheus + SSRF protection
- Add Prometheus service
- Configure Coturn with `--prometheus` flags and volume for metrics retention
- Add `--denied-peer-ip` flags for all private/link-local ranges (SSRF defense)

### Task 7: Add quota reset job
- Calculate quota period from `account.created` date (fixed 30-day period)
- Reset `current_period_usage_gb` to 0 every 30 days from account creation
- Archive previous period's usage to `bandwidth_usage` collection
- Update `quota_period_end` to next period end date

### Task 8: Write tests
- Quota enforcement (allowed vs denied)
- Prometheus metrics aggregation
- Quota reset logic

---

## Acceptance Criteria

- [ ] When TURN is configured, Prometheus is required and quotas are always enforced
- [ ] When TURN is not configured (direct-only), Prometheus is not required and no bandwidth limits apply
- [ ] New accounts get 50GB/30 days quota by default
- [ ] Relay sessions rejected with clear error when quota exceeded
- [ ] Quota usage displays in account dashboard with progress bar
- [ ] Quota period is fixed 30 days from account creation date
- [ ] Bandwidth usage logged for audit purposes
- [ ] Self-hosters can configure `DEFAULT_QUOTA_GB`

---

## Out of Scope (Slice 10c)

- Stripe integration
- Pro tier with higher quotas
- Tier upgrade UI
- Usage-based billing
- BYOK TURN (Bring Your Own Key)

---

## Future Hardening (Post 10b)

### "Hard" Quota Enforcement

The current implementation uses "soft" quotas — existing relay sessions continue after the quota is exceeded. For true hard quotas (immediate termination), Coturn's CLI interface can be used:

```bash
# Coturn CLI (telnet to port 5766)
> ps  # List active sessions
> kick <username>  # Terminate specific session
```

**Implementation approach:**
1. Enable Coturn CLI (`--cli` flag)
2. When quota exceeded, query CLI for active sessions by username
3. Issue `kick` command to terminate them
4. Alternative: Shorten TURN credential TTL to 1 hour for faster natural expiry

**Note:** This adds complexity (parsing CLI output, handling connection errors) and should be deferred until abuse becomes a real issue.

---

## Breaking Changes

| Before | After |
|--------|-------|
| Per-session TURN creds (`{timestamp}:{code}`) | Account-scoped TURN creds (`{timestamp}:{accountID}`) always |
| No bandwidth limits | 50GB/30 days default quota when quotas enabled |

---

## Notes

**Self-hosted without TURN:**
- No Prometheus required — direct-only mode works fine for private homelabs sharing with family
- No bandwidth limits apply (no relay = nothing to meter)
- Account-scoped TURN credentials are still used if TURN is ever added later

**Soft quota limitation:**
- Only new relay sessions are blocked when quota exceeded
- Existing sessions continue until TURN credentials expire (up to 24 hours)
- See [Future Hardening](#future-hardening) for "hard" quota options

**Migration:**
- Existing sessions continue using old creds until expiry
- New sessions get account-scoped creds after deploy
