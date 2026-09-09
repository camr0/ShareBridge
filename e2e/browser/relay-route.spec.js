// relay-route.spec.js — Task 24 (spec §18.3, gate §23.4): the four-second
// browser/CORS/CSP/noscript behavior of the §9.3 route flow, proven against
// the REAL control (canonical routes + interstitial + prepare-route) and the
// REAL agent (Binder, connect endpoint, Phase 3 gallery) stood up hermetically
// by fixture.mjs.
//
// The eight named cases from plan Task 24 run on chromium, firefox and
// webkit. Timing assertions pin the §4.4 AbortController budget with CI
// margins: the blackhole fallback must land on relay inside a bounded window
// (not eventually) and must not fall back prematurely.

import { test, expect } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import { request as nodeRequest } from 'node:http';

const here = dirname(fileURLToPath(import.meta.url));
const state = JSON.parse(readFileSync(join(here, '.state.json'), 'utf8'));

const CONTROL_ORIGIN = state.controlOrigin; // https://sharebridge.app (loopback via proxy)
const NS_DOMAIN = state.namespaceDomain; // sb0a1b2c3.example.com
const EVIL_HOST = `unrelat99.sbother99.example.com`; // other agent's namespace

const canonical = (code) => `${CONTROL_ORIGIN}/s/${code}`;
const relayURLFor = (code) => `https://${code}.relay.${NS_DOMAIN}/s/${code}`;

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// recordRequests wires request/response collection with wall-clock timestamps
// (ms). Timestamps come from the Node process at event delivery; all bounds
// below carry slack for that indirection.
function recordRequests(page) {
  const events = [];
  const t = () => Date.now();
  page.on('request', (r) => events.push({ kind: 'request', t: t(), url: r.url(), method: r.method(), req: r }));
  page.on('response', (r) => events.push({ kind: 'response', t: t(), url: r.url(), status: r.status(), resp: r }));
  return events;
}

const hostOf = (url) => new URL(url).host;

// requestsTo filters requests by a host regex or by a (url, host) predicate.
const requestsTo = (events, match) =>
  events.filter((e) => {
    if (e.kind !== 'request') return false;
    if (typeof match === 'function') return match(e.url, hostOf(e.url));
    return match.test(hostOf(e.url));
  });

// connectRequestsOf returns the page's GET /s/<code>/connect requests — the
// interstitial's direct check, matched by PATH (the host is the direct origin
// and never contains "connect").
const connectRequestsOf = (events, code) =>
  events.filter((e) => e.kind === 'request' && new URL(e.url).pathname === `/s/${code}/connect`);

// CSP-violation-shaped console messages across engines ("Content Security
// Policy …" on Firefox/WebKit, "Refused to …" on Chromium).
const cspViolationRe = /content security policy|connect-src|refused to/i;

function collectConsole(page) {
  const messages = [];
  page.on('console', (m) => messages.push({ type: m.type(), text: m.text() }));
  return messages;
}

// regrantPresence refreshes the §7.3 presence lease (45 s) through the
// control harness admin endpoint so relay-dependent cases never race expiry.
async function regrantPresence() {
  await new Promise((resolve, reject) => {
    const req = nodeRequest(
      { host: '127.0.0.1', port: state.controlAdminPort, path: '/admin/presence', method: 'POST' },
      (res) => {
        res.resume();
        res.statusCode === 200 ? resolve() : reject(new Error(`presence regrant: ${res.statusCode}`));
      },
    );
    req.on('error', reject);
    req.end();
  });
}

// proxyConnections fetches the CONNECT log from the fixture proxy (plain Node
// HTTP — never via the browser proxy).
function proxyConnections() {
  return new Promise((resolve, reject) => {
    const req = nodeRequest(
      { host: '127.0.0.1', port: state.proxyPort, path: '/__proxy/log', method: 'GET' },
      (res) => {
        let body = '';
        res.on('data', (c) => (body += c));
        res.on('end', () => {
          try {
            resolve(JSON.parse(body).connections);
          } catch (err) {
            reject(err);
          }
        });
      },
    );
    req.on('error', reject);
    req.end();
  });
}

// agentConnectObservations fetches the agent-side record of the observed
// /connect requests (method + headers) and their served response fields.
function agentConnectObservations() {
  return new Promise((resolve, reject) => {
    const req = nodeRequest(
      { host: '127.0.0.1', port: state.agentAdminPort, path: '/admin/connect-observations', method: 'GET' },
      (res) => {
        let body = '';
        res.on('data', (c) => (body += c));
        res.on('end', () => {
          try {
            resolve(JSON.parse(body).observations);
          } catch (err) {
            reject(err);
          }
        });
      },
    );
    req.on('error', reject);
    req.end();
  });
}

// newProxiedContext: every browser context routes through the fixture's
// loopback CONNECT proxy — the fixture DNS shim (per-hostname virtual-port
// mapping), the engines' analogue of chromedp's --host-resolver-rules.
async function newProxiedContext(browser, extra = {}) {
  return browser.newContext({
    proxy: { server: `http://127.0.0.1:${state.proxyPort}` },
    ignoreHTTPSErrors: true,
    ...extra,
  });
}

async function prepareResponseFor(page, code) {
  return page.waitForResponse(
    (r) => r.url().endsWith(`/api/shares/${code}/prepare-route`) && r.request().method() === 'POST',
    { timeout: 20_000 },
  );
}

// waitForConnectRequest resolves when the interstitial's CORS reachability
// check for `code` leaves the page, returning { request, at }.
async function waitForConnectRequest(page, code, events) {
  const deadline = Date.now() + 20_000;
  while (Date.now() < deadline) {
    const hit = events.find(
      (e) => e.kind === 'request' && e.method === 'GET' && new URL(e.url).pathname === `/s/${code}/connect`,
    );
    if (hit) return hit;
    await page.waitForTimeout(25);
  }
  throw new Error(`no /s/${code}/connect request observed`);
}

async function waitForConnectResponse(page, code) {
  return page.waitForResponse((r) => new URL(r.url()).pathname === `/s/${code}/connect`, { timeout: 20_000 });
}

const galleryRendered = (page) =>
  page
    .waitForFunction(
      () => document.querySelectorAll('#gallery-root .gallery-item').length === 3,
      undefined,
      { timeout: 20_000 },
    )
    .then(() => true)
    .catch(() => false);

test.beforeEach(async () => {
  await regrantPresence();
});

// ---------------------------------------------------------------------------
// 1. direct success never touches relay
// ---------------------------------------------------------------------------

test('direct success never touches relay', async ({ browser }) => {
  const context = await newProxiedContext(browser);
  const page = await context.newPage();
  const events = recordRequests(page);
  const consoleMessages = collectConsole(page);

  const prepare = await (async () => {
    const nav = page.goto(canonical('direct01'));
    const prepare = await prepareResponseFor(page, 'direct01');
    await nav;
    return prepare;
  })();
  expect(prepare.status()).toBe(200);
  const plan = await prepare.json();
  // §9.3 prepare contract: control-derived direct URL + optional relay URL and
  // the §4.4 recipient budget echoed to the page.
  expect(plan.status).toBe('direct');
  expect(plan.direct_timeout_ms).toBe(4000);
  expect(plan.direct_url).toMatch(new RegExp(`^https://direct01\\.${NS_DOMAIN}:\\d+/s/direct01$`));
  expect(plan.relay_url).toBe(relayURLFor('direct01'));

  const connect = await waitForConnectResponse(page, 'direct01');
  expect(connect.status()).toBe(204);

  // Terminal navigation is the DIRECT origin — never the relay URL that the
  // page also carried.
  await page.waitForURL(plan.direct_url, { timeout: 20_000 });
  expect(page.url()).toBe(plan.direct_url);
  expect(await galleryRendered(page)).toBe(true);

  // The relay origin was never requested by this navigation.
  expect(requestsTo(events, /\.relay\./)).toEqual([]);

  // Zero CSP violations on the whole direct flow (§18.3).
  expect(consoleMessages.filter((m) => cspViolationRe.test(m.text))).toEqual([]);
  await context.close();
});

// ---------------------------------------------------------------------------
// 2. blackhole falls back within bound
// ---------------------------------------------------------------------------

test('blackhole falls back within bound', async ({ browser }) => {
  const context = await newProxiedContext(browser);
  const page = await context.newPage();
  const events = recordRequests(page);

  const nav = page.goto(canonical('blackh02'));
  const prepare = await prepareResponseFor(page, 'blackh02');
  await nav;
  const plan = await prepare.json();
  expect(plan.status).toBe('direct');
  expect(plan.direct_timeout_ms).toBe(4000);
  expect(plan.relay_url).toBe(relayURLFor('blackh02'));

  // The direct check is issued and blackholes (accepted, then stalled by the
  // fixture — deterministic unreachable-direct, no fast network error).
  const connectRequest = await waitForConnectRequest(page, 'blackh02', events);
  await expect
    .poll(async () => connectRequestsOf(events, 'blackh02').length, { timeout: 5_000 })
    .toBe(1);

  // §4.4: the AbortController budget must govern the fallback — observed
  // within a bounded window, and not prematurely. connectRequest.at is the
  // CONNECT-observed instant; the abort fires ≥4000 ms after the fetch began.
  await page.waitForURL(relayURLFor('blackh02'), { timeout: 15_000 });
  const relayHit = events.find((e) => e.kind === 'request' && hostOf(e.url) === `blackh02.relay.${NS_DOMAIN}`);
  expect(relayHit).toBeTruthy();
  const fallbackMs = relayHit.t - connectRequest.t;
  expect(fallbackMs).toBeGreaterThanOrEqual(3800); // budget honored (no premature fallback)
  expect(fallbackMs).toBeLessThanOrEqual(7000); // bounded — never a hang

  // The fallback lands on real relay-served content (agent RouteRelay origin).
  expect(await galleryRendered(page)).toBe(true);

  // Exactly one direct check was attempted — the timeout is terminal.
  expect(connectRequestsOf(events, 'blackh02').length).toBe(1);
  await context.close();

  // Report the measured sample (rendered in the JSON report / CI logs).
  console.log(`[timing] blackh02 fallback (prepare→relay navigation): ${fallbackMs} ms after connect start`);
});

// ---------------------------------------------------------------------------
// 3. manual relay cancels direct
// ---------------------------------------------------------------------------

test('manual relay cancels direct', async ({ browser }) => {
  const context = await newProxiedContext(browser);
  const page = await context.newPage();
  const events = recordRequests(page);

  const nav = page.goto(canonical('manual03'));
  await prepareResponseFor(page, 'manual03');
  await nav;

  // While the direct check is blackholed, the immediate "Use relay now"
  // action must cancel it and navigate to the already-returned relay URL.
  await waitForConnectRequest(page, 'manual03', events);
  const clickAt = Date.now();
  await page.click('#sb-use-relay');

  await page.waitForURL(relayURLFor('manual03'), { timeout: 5_000 });
  const relayHit = events.find((e) => e.kind === 'request' && hostOf(e.url) === `manual03.relay.${NS_DOMAIN}`);
  expect(relayHit).toBeTruthy();
  const manualMs = relayHit.t - clickAt;
  expect(manualMs).toBeLessThanOrEqual(2500); // immediate — it did not wait out the budget

  expect(await galleryRendered(page)).toBe(true);

  // The aborted direct attempt was not retried: exactly one connect request.
  expect(connectRequestsOf(events, 'manual03').length).toBe(1);
  await context.close();

  console.log(`[timing] manual03 click→relay navigation: ${manualMs} ms`);
});

// ---------------------------------------------------------------------------
// 4. relayOnly emits no direct request
// ---------------------------------------------------------------------------

test('relayOnly emits no direct request', async ({ browser }) => {
  const context = await newProxiedContext(browser);
  const page = await context.newPage();
  const events = recordRequests(page);

  // §9.1: relay-only resolution is the canonical navigation itself — 302 to
  // the derived relay origin, before any page exists.
  const nav = page.goto(canonical('relayon04'));
  const first = await page.waitForEvent('response', { timeout: 20_000 });
  expect(first.status()).toBe(302);
  expect(first.headers().location).toBe(relayURLFor('relayon04'));
  expect(first.headers()['cache-control']).toBe('no-store');
  await nav;

  await page.waitForURL(relayURLFor('relayon04'), { timeout: 20_000 });
  expect(await galleryRendered(page)).toBe(true);

  // No request ever went to ANY direct (non-relay) content origin.
  const directHostRequests = requestsTo(events, (u, h) => h.endsWith(NS_DOMAIN) && !h.includes('.relay.'));
  expect(directHostRequests).toEqual([]);
  // …and none to this share's own §6 direct origin in particular (the relay
  // host shares the label — match the exact direct origin host).
  expect(requestsTo(events, (u, h) => h === `relayon04.${NS_DOMAIN}`)).toEqual([]);
  await context.close();
});

// ---------------------------------------------------------------------------
// 5. non443 direct passes scoped CSP
// ---------------------------------------------------------------------------

test('non443 direct passes scoped CSP', async ({ browser }) => {
  const context = await newProxiedContext(browser);
  const page = await context.newPage();
  const events = recordRequests(page);
  const consoleMessages = collectConsole(page);

  const nav = page.goto(canonical('non44305'));
  const prepare = await prepareResponseFor(page, 'non44305');
  // The interstitial response (goto resolves with it) carries the §9.3 scoped
  // policy; the prepare response never does.
  const interstitial = await nav;
  const cspHeader =
    prepare.headers()['content-security-policy'] ?? interstitial.headers()['content-security-policy'];

  // The page's effective policy is the §9.3 scoped policy: same-origin
  // preparation plus the CURRENT session namespace's HTTPS any-port wildcard
  // only — no frames, objects, or other destinations.
  const nonce = /script-src 'nonce-([^']+)'/.exec(cspHeader)?.[1];
  expect(nonce).toBeTruthy();
  expect(cspHeader).toBe(
    `default-src 'none'` +
      `; script-src 'nonce-${nonce}'` +
      `; style-src 'nonce-${nonce}'` +
      `; connect-src 'self' https://*.${NS_DOMAIN}:*` +
      `; frame-ancestors 'none'; base-uri 'none'; form-action 'none'`,
  );

  // The direct check targets a prepared NON-443 port (the on-demand mapped
  // port is learned after the headers were sent — exactly what `:*` covers).
  const connect = await waitForConnectResponse(page, 'non44305');
  expect(connect.status()).toBe(204);
  expect(new URL(connect.url()).port).not.toBe('');
  expect(new URL(connect.url()).port).not.toBe('443');

  const plan = await prepare.json();
  await page.waitForURL(plan.direct_url, { timeout: 20_000 });
  expect(await galleryRendered(page)).toBe(true);

  // Zero violations across the whole non-443 direct flow.
  expect(consoleMessages.filter((m) => cspViolationRe.test(m.text))).toEqual([]);
  await context.close();
});

// ---------------------------------------------------------------------------
// 6. unrelated destinations are blocked
// ---------------------------------------------------------------------------

test('unrelated destinations are blocked', async ({ browser }) => {
  const context = await newProxiedContext(browser);
  const page = await context.newPage();
  const consoleMessages = collectConsole(page);

  const nav = page.goto(canonical('unrelat06'));
  await prepareResponseFor(page, 'unrelat06');
  await nav;
  // The interstitial's own flow produced no violations.
  expect(consoleMessages.filter((m) => cspViolationRe.test(m.text))).toEqual([]);

  const before = await proxyConnections();

  // Attempt a cross-namespace connect from the page: the scoped CSP must
  // block it before any packet leaves (the target is a REAL control-derived
  // origin of a DIFFERENT agent's namespace).
  const outcome = await page.evaluate(async (evil) => {
    try {
      const resp = await fetch(`https://${evil}/s/unrelat99/connect`, {
        method: 'GET',
        credentials: 'omit',
        cache: 'no-store',
        mode: 'cors',
      });
      return `allowed:${resp.status}`;
    } catch (err) {
      return `blocked:${err.name}`;
    }
  }, EVIL_HOST);
  expect(outcome).toMatch(/^blocked:/);

  // Blocked pre-flight: the proxy never saw a CONNECT for the unrelated host.
  const after = await proxyConnections();
  expect(
    after.filter((c) => c.host === EVIL_HOST && !before.some((b) => b.host === EVIL_HOST && b.at <= c.at)),
  ).toEqual([]);

  // The block is observable as a CSP violation event in the console.
  const violations = consoleMessages.filter((m) => cspViolationRe.test(m.text));
  expect(violations.length).toBeGreaterThanOrEqual(1);
  await context.close();
});

// ---------------------------------------------------------------------------
// 7. connect has no cookies/content
// ---------------------------------------------------------------------------

test('connect has no cookies/content', async ({ browser }) => {
  const context = await newProxiedContext(browser);
  const page = await context.newPage();
  const events = recordRequests(page);

  const nav = page.goto(canonical('direct01'));
  await prepareResponseFor(page, 'direct01');
  await nav;

  const connect = await waitForConnectResponse(page, 'direct01');
  expect(connect.status()).toBe(204);

  // Browser-observed response contract (§9.3): credential-free, content-free.
  const headers = await connect.allHeaders();
  expect(headers['cache-control']).toBe('no-store');
  expect(headers['access-control-allow-origin']).toBe('https://sharebridge.app');
  expect(headers['set-cookie']).toBeUndefined();
  // 204 carries no body by definition; reading it from the page races the
  // terminal direct navigation (the resource is gone once navigation starts),
  // so the content-free half is proven by the agent-side byte count below.

  // Exactly one simple CORS GET reached the agent — no preflight, no cookie.
  const connectRequests = events.filter(
    (e) => e.kind === 'request' && new URL(e.url).pathname === '/s/direct01/connect',
  );
  expect(connectRequests.length).toBe(1);
  expect(connectRequests[0].method).toBe('GET');

  // Agent-side observation of the SAME requests: no Cookie header ever
  // arrived, and the handler served 204 with an empty body.
  const observations = await agentConnectObservations();
  const direct01 = observations.filter((o) => o.code === 'direct01');
  expect(direct01.length).toBeGreaterThanOrEqual(1);
  const last = direct01[direct01.length - 1];
  expect(last.method).toBe('GET');
  expect(last.requestHeaders['cookie']).toBeUndefined();
  expect(last.responseStatus).toBe(204);
  expect(last.responseHeaders['set-cookie']).toBeUndefined();
  expect(last.responseBytes).toBe(0);
  await context.close();
});

// ---------------------------------------------------------------------------
// 8. javascript-disabled follows exact noscript relay
// ---------------------------------------------------------------------------

test('javascript-disabled follows exact noscript relay', async ({ browser }) => {
  const context = await newProxiedContext(browser, { javaScriptEnabled: false });
  const page = await context.newPage();
  const events = recordRequests(page);

  // With JS disabled the page's <noscript> meta refresh is the fallback
  // (§9.3): navigate the EXACT control-derived relay URL — no preparation
  // call, no direct check.
  const nav = page.goto(canonical('noscrpt08'));
  await page.waitForURL(relayURLFor('noscrpt08'), { timeout: 20_000 });
  await nav;

  expect(page.url()).toBe(relayURLFor('noscrpt08'));
  const relayResp = events.find(
    (e) => e.kind === 'response' && hostOf(e.url) === `noscrpt08.relay.${NS_DOMAIN}` && new URL(e.url).pathname === '/s/noscrpt08',
  );
  expect(relayResp).toBeTruthy();
  expect(relayResp.status).toBe(200);

  // No-JS means exactly that: no preparation fetch ever happened.
  expect(requestsTo(events, (u) => u.includes('prepare-route'))).toEqual([]);
  expect(connectRequestsOf(events, 'noscrpt08')).toEqual([]);
  await context.close();
});
