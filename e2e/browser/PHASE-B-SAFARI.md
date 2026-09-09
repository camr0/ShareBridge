# Phase B — real Safari / iOS Safari smoke checklist (Task 24, §23.4)

Phase A (this branch) proves the route flow on Playwright Chromium, Firefox,
and WebKit. Playwright WebKit is **not** a substitute for deployed Safari, so
this manual checklist is executed by a human against a real deployment
before the §23.4 gate is signed. Every case below is a PASS/FAIL marker; the
GO/NO-GO rule is at the end. Total time: roughly 30–45 minutes.

Everything is driven from ordinary Safari windows — no developer tools
required except where noted (the Web Inspector is needed only to view
network/CSP details; if it is unavailable, record the visible page behavior
and mark the case OBSERVED-LIMITED).

## 0. Prerequisites

- A deployed control (`https://sharebridge.app`) and at least one agent with:
  - a **direct candidate** share (agent behind a port-forwarded, direct-capable
    network) — note its share code as `<DIRECT>` and its code as shown on the
    share link;
  - a **non-443 direct** share (the agent's mapped external port is not 443) —
    code `<NON443>`;
  - a **relay-only** share — code `<RELAYONLY>`;
  - a **blackhole direct** share (agent network where the mapped port is
    silently dropped, e.g. firewall DROP, not REJECT) — code `<BLACKHOLE>`;
  - the agent's namespace domain `<NS>` (the part after the first label of the
    share's direct origin, e.g. `sb0a1b2c3.example.com`).
- RELAY_SELECTION_ENABLED=true on the control, relay gateway reachable, and
  the agent showing relay presence (the interstitial must offer "Use relay
  now").
- One browser profile on each device used, nothing else installed.

## 1. Environment capture (do this first, paste into the results file)

For each device/browser used, record:

- Device model / OS version (macOS: Apple menu → About This Mac; iOS:
  Settings → General → About).
- Browser name and full version (macOS Safari: Safari → About Safari; iOS
  Safari: same as iOS version).
- Network type for the device (Wi-Fi / cellular / Ethernet).
- Screenshot of the About page (filename `00-version-<device>.png`).

## 2. Cases

Perform the steps exactly; after each case, save the named screenshot and
mark PASS / FAIL (with one line of what differed).

### 2.1 Direct success never touches relay (macOS Safari + iOS Safari)

1. In a fresh tab, open `https://sharebridge.app/s/<DIRECT>`.
2. Do not interact; wait up to 10 s.

Expected: a brief "Preparing your share…" page, then the share's gallery
loads at the direct origin — the URL shows `https://<DIRECT>.<NS>:<port>/s/<DIRECT>`
with a screenshot (`01-direct-<device>.png`). No relay origin
(`*.relay.<NS>`) ever appears in the address bar.

### 2.2 Blackhole falls back within bound (macOS Safari; time it)

1. Open a new tab, ready a stopwatch (or the phone's clock with seconds).
2. Open `https://sharebridge.app/s/<BLACKHOLE>` and start the stopwatch at
   the same moment.
3. Stop the stopwatch the moment the gallery appears.

Expected: the "Preparing…" page holds for roughly 4–5 seconds, then the
gallery loads from the relay origin
(`https://<BLACKHOLE>.relay.<NS>/s/<BLACKHOLE>`). Measured time must be
between 4 and 8 seconds — noticeably bounded, not a hang, and not an instant
bounce. Screenshot `02-blackhole-<device>.png`; write down the seconds.

### 2.3 Manual relay cancels direct (macOS Safari + iOS Safari)

1. Open `https://sharebridge.app/s/<BLACKHOLE>` again in a fresh tab.
2. As soon as the "Preparing…" page shows the blue **Use relay now** button,
   tap/click it.

Expected: navigation to the relay origin happens immediately (well under a
second of visible wait), gallery renders. Screenshot `03-manual-<device>.png`.

### 2.4 relayOnly emits no direct request (macOS Safari + iOS Safari)

1. Open `https://sharebridge.app/s/<RELAYONLY>` in a fresh tab.

Expected: **no** "Preparing…" interstitial — the browser lands directly on
the relay origin `https://<RELAYONLY>.relay.<NS>/s/<RELAYONLY>` and the
gallery renders. Screenshot `04-relayonly-<device>.png`.

### 2.5 Non-443 direct passes scoped CSP (macOS Safari)

1. Open `https://sharebridge.app/s/<NON443>` in a fresh tab.
2. Wait for the gallery on the direct origin (URL contains a `:<port>` that
   is not 443).
3. Open Web Inspector → Network tab → reload the page once, and check: the
   `prepare-route` request succeeded; the `connect` request went to
   `https://<NON443>.<NS>:<port>/s/<NON443>/connect` and returned **204**;
   there are **no** red CSP violation entries in the Console.

Expected: gallery renders on the non-443 direct origin with zero console CSP
errors. Screenshot `05-non443-<device>.png` (page + inspector).

### 2.6 Unrelated destinations are blocked (macOS Safari)

1. With the `<NON443>` gallery still open (from 2.5), open Web Inspector →
   Console.
2. Run:
   `fetch("https://probe.unrelated.example.com/s/x/connect").then(r => "allowed " + r.status, e => "blocked " + e.name)`

Expected: the promise resolves to `blocked TypeError` (or similar blocked
error) within a moment, and the Console shows a Content Security Policy
message naming `connect-src`. No request to the unrelated host appears in the
Network tab. Screenshot `06-blocked-<device>.png` (console).

### 2.7 Connect has no cookies/content (macOS Safari)

1. Open Web Inspector → Network, then open `https://sharebridge.app/s/<NON443>`
   in a fresh tab (once the gallery appears, stop).
2. In the Network list, select the `connect` request.

Expected: method GET, status **204**, response headers include
`access-control-allow-origin: https://sharebridge.app` and `cache-control:
no-store`, **no** `set-cookie` header, response size 0 bytes, and the request
carries no `Cookie` header. Screenshot `07-connect-<device>.png`.

### 2.8 JavaScript-disabled follows exact noscript relay (macOS Safari)

1. Safari → Settings → Security: uncheck "Enable JavaScript".
2. Open `https://sharebridge.app/s/<NON443>` in a fresh tab.

Expected: without any interstitial activity, the browser is redirected to
the relay origin `https://<NON443>.relay.<NS>/s/<NON443>` and the gallery
renders (the page's `<noscript>` meta refresh). Re-enable JavaScript
afterwards. Screenshot `08-noscript-<device>.png`.

## 3. Results + GO/NO-GO

Fill one row per case per browser, then apply the rule.

| Case | macOS Safari (version) | iOS Safari (version) |
|------|------------------------|----------------------|
| 2.1 direct success | | |
| 2.2 blackhole bound (s) | | |
| 2.3 manual relay | | |
| 2.4 relayOnly 302 | | |
| 2.5 non-443 CSP | | |
| 2.6 unrelated blocked | | |
| 2.7 connect clean | | |
| 2.8 noscript relay | | |

**GO/NO-GO rule (§23.4):** if any browser cannot observe the credentialless
CORS **204** (2.7), the scoped **non-443 CSP** (2.5), or the **noscript
fallback** (2.8), the result is **NO-GO for automatic fallback**. Bounded
fallback (2.2) outside 4–8 s, or a relay-only/direct case landing on the
wrong origin, is also NO-GO until fixed. Save this file with the screenshots
as the §23.4 Phase B evidence.
