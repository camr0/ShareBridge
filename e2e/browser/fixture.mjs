// fixture.mjs — Task 24 hermetic fixture (plan Task 24; spec §23.4).
//
// Stands up, on loopback only:
//
//   1. the REAL agent data plane — agent/internal/direct's
//      TestE2EBrowserFixture (Binder SNI admission, the Task 23 connect
//      endpoint, the Phase 3 gallery placeholder over both RouteDirect and
//      RouteRelay origins) — built and run as a Go test binary, the same
//      go-test-driver seam Task 25 uses for control-internal helpers;
//   2. the REAL control interstitial surface — control's shipped
//      route-interstitial.html/.js/.css assets, rendered through the exact
//      marker substitution and the exact §9.3 CSP construction of
//      control/internal/directctl/interstitial.go, dispatched per the §9.1
//      matrix outcome each share code stands for (direct candidate → 200
//      no-store interstitial, relayOnly → 302 no-store to the derived relay
//      origin) — the browser-facing §9.3 route flow, byte-for-byte the page
//      the deployed control serves;
//   3. a loopback CONNECT proxy: the engines' per-hostname DNS shim (the
//      Playwright analogue of chromedp's --host-resolver-rules). Namespace
//      hostnames tunnel to the agent listener on any port (the virtual
//      mapped-port form), sharebridge.app tunnels to the control listener,
//      and the blackhole share codes are accepted-then-stalled tunnels — a
//      deterministic unroutable direct target with no fast network error.
//
// The control-internal prepare-route DECISION engine (inline STUN refresh,
// open_signal/ack, verified-tuple probe) is not browser-facing and is
// exercised by Tasks 18–21 and 25; here each share code deterministically
// stands for one §9.3 prepare outcome, exactly like control's own hermetic
// e2e test stubs the coordinator/DDNS while running the real Controller.
// Disclosed boundary: the control web assets (route-interstitial
// html/.js/.css) and the agent behavior (Binder SNI admission, connect
// endpoint, gallery) are the real shipped code; route dispatch (the §9.1
// per-share outcome), the prepare-route JSON, the relay-URL derivation, and
// the §9.3 CSP string construction are fixture ports of the control
// implementations, not the control server binary. The ported CSP string is
// drift-pinned against the shipped code by the shared golden
// (control/web/testdata/route-interstitial-csp.golden), asserted at fixture
// startup.

import { spawn } from 'node:child_process';
import { execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import http from 'node:http';
import https from 'node:https';
import net from 'node:net';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { tmpdir } from 'node:os';

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, '..', '..');
const agentDir = join(repoRoot, 'agent');
const controlWebDir = join(repoRoot, 'control', 'web');

// The fixture namespace mirrors a real control allocation (the same
// ^sb[0-9a-f]{8}$ form control issues) over the fixture base domain; the
// control origin is the production hostname the agent's connect endpoint
// pins as its single allowed caller.
const NS = 'sb0a1b2c3';
const BASE = 'example.com';
const NS_DOMAIN = `${NS}.${BASE}`;
const CONTROL_HOSTNAME = 'sharebridge.app';
const RELAY_SUFFIX = `.relay.${NS_DOMAIN}`;
const DIRECT_SUFFIX = `.${NS_DOMAIN}`;

// routes is the fixture's deterministic §9.1/§9.3 decision matrix: one entry
// per share code the spec drives. kind mirrors SelectRoute's outcome for the
// code (direct candidate → interstitial, relayOnly → 302); port is the
// virtual mapped port the prepare response embeds (a real router-forward
// shape — never 443); stall marks the deterministic blackhole direct target;
// relayPresence=false renders the §9.3 no-relay page variant and answers the
// preparation with the unavailable response (the unrelat06 case keeps the
// page parked on the interstitial so the scoped-CSP probe always runs on the
// document that carries it).
const routes = {
  direct01: { kind: 'direct', port: 40401, stall: false },
  blackh02: { kind: 'direct', port: 40402, stall: true },
  manual03: { kind: 'direct', port: 40403, stall: true },
  relayon04: { kind: 'relayOnly' },
  non44305: { kind: 'direct', port: 40405, stall: false },
  unrelat06: { kind: 'direct', port: 40406, stall: false, relayPresence: false, prepare: 'unavailable' },
  noscrpt08: { kind: 'direct', port: 40408, stall: false },
};

const relayURLFor = (code) => `https://${code}${RELAY_SUFFIX}/s/${code}`;
const directURLFor = (code, port) => `https://${code}${DIRECT_SUFFIX}:${port}/s/${code}`;
const noStore = (res) => res.setHeader('Cache-Control', 'no-store');

// ---------------------------------------------------------------------------
// Control interstitial surface: the shipped control assets, rendered exactly
// as control/internal/directctl renders them.
// ---------------------------------------------------------------------------

const INTERSTITIAL_MARKERS = ['{{NONCE}}', '{{CODE}}', '{{RELAY_HEAD_NOSCRIPT}}', '{{RELAY_BODY_NOSCRIPT}}', '{{USE_RELAY_BUTTON}}'];

// loadInterstitialAssets mirrors directctl.LoadInterstitialAssets: bounded
// reads of the three control-owned assets, failing closed when a template
// marker is missing (the fixture refuses to stand up a broken page, exactly
// like the deployed control fails its startup).
function loadInterstitialAssets() {
  const bounded = (name, limit) => {
    const data = readFileSync(join(controlWebDir, name));
    if (data.length > limit) throw new Error(`interstitial asset ${name} is ${data.length} bytes, limit ${limit}`);
    return data;
  };
  const html = bounded('route-interstitial.html', 16 << 10);
  const js = bounded('route-interstitial.js', 32 << 10);
  const css = bounded('route-interstitial.css', 16 << 10);
  const tpl = html.toString();
  for (const marker of INTERSTITIAL_MARKERS) {
    if (!tpl.includes(marker)) throw new Error(`interstitial template missing marker ${marker}`);
  }
  return { html, js, css };
}

// escapeHTML mirrors Go html.EscapeString's five escapes (the renderer's
// attribute-boundary defense for anything embedded in the template).
function escapeHTML(s) {
  return s.replace(/&/g, '&amp;').replace(/'/g, '&#39;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&#34;');
}

// buildInterstitialCSP is the §9.3 policy, verbatim from
// control/internal/directctl/interstitial.go (nonce-bound script/style,
// same-origin preparation plus the session namespace's HTTPS any-port
// wildcard, and nothing else). The port is pinned against control's own
// output: control/web/testdata/route-interstitial-csp.golden (generated by
// control's TestInterstitialCSPGolden through the real render path) is
// asserted at fixture startup by assertInterstitialCSPGolden below — drift
// on either side now fails loudly instead of staying verbatim-identical by
// luck.
function buildInterstitialCSP(nonce, namespaceDomain) {
  return (
    "default-src 'none'" +
    `; script-src 'nonce-${nonce}'` +
    `; style-src 'nonce-${nonce}'` +
    `; connect-src 'self' https://*.${namespaceDomain}:*` +
    "; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
  );
}

// CSP golden pin (Task 24 Phase A drift guard): the fixed nonce and the
// namespace scope the golden blocks were generated with — shared constants,
// kept in lockstep with control/internal/directctl's
// interstitial_golden_test.go and documented in the golden header.
const CSP_GOLDEN_PATH = join(repoRoot, 'control', 'web', 'testdata', 'route-interstitial-csp.golden');
const CSP_GOLDEN_NONCE = 'Z29sZGVuLWNzcC1waW4tbm9uY2UtMDAw';

// assertInterstitialCSPGolden reads the control golden and fails fixture
// startup unless the ported buildInterstitialCSP reproduces the golden block
// for every interstitial variant this fixture serves (relay-available for the
// lease-backed routes, relay-unavailable for unrelat06). Runtime serving is
// untouched: this is a startup constant-vs-constant comparison only.
function assertInterstitialCSPGolden() {
  const text = readFileSync(CSP_GOLDEN_PATH, 'utf8');
  const blocks = {};
  const lines = text.split('\n');
  for (let i = 0; i < lines.length; i++) {
    const m = /^# variant: (.+)$/.exec(lines[i]);
    if (m) blocks[m[1].trim()] = lines[i + 1];
  }
  for (const variant of ['relay-available', 'relay-unavailable']) {
    const want = blocks[variant];
    if (!want) {
      throw new Error(
        `control golden ${CSP_GOLDEN_PATH} has no "# variant: ${variant}" block — regenerate the golden (go test ./internal/directctl -run TestInterstitialCSPGolden -update, from control/)`
      );
    }
    const got = buildInterstitialCSP(CSP_GOLDEN_NONCE, NS_DOMAIN);
    if (got !== want) {
      throw new Error(
        `control golden drifted from fixture port — regenerate the golden or update the port\n` +
          `  golden ${CSP_GOLDEN_PATH} [${variant}]: ${want}\n` +
          `  port   e2e/browser/fixture.mjs buildInterstitialCSP: ${got}`
      );
    }
  }
}

// renderInterstitialPage mirrors directctl.renderInterstitialPage: the exact
// marker substitutions (head/body noscript and the use-relay button exist
// only when the relay URL is offered; otherwise the unavailable variant).
function renderInterstitialPage(assets, nonce, code, relayLoc) {
  let headNoScript = '';
  let bodyNoScript = '';
  let useRelay = '';
  if (relayLoc !== '') {
    const attr = escapeHTML(relayLoc);
    headNoScript = `<noscript><meta http-equiv="refresh" content="0; url=${attr}"></noscript>`;
    bodyNoScript = `<noscript><p><a id="sb-relay-link" href="${attr}">Open your share via relay</a></p></noscript>`;
    useRelay = `<button type="button" id="sb-use-relay" data-relay-url="${attr}">Use relay now</button>`;
  } else {
    bodyNoScript = `<noscript><p id="sb-noscript-unavailable">Your share can’t be opened right now. <a id="sb-noscript-retry" href="/s/${escapeHTML(code)}">Retry</a></p></noscript>`;
  }
  const page = String(
    assets.html
      .toString()
      .replaceAll('{{NONCE}}', nonce)
      .replaceAll('{{CODE}}', escapeHTML(code))
      .replaceAll('{{RELAY_HEAD_NOSCRIPT}}', headNoScript)
      .replaceAll('{{RELAY_BODY_NOSCRIPT}}', bodyNoScript)
      .replaceAll('{{USE_RELAY_BUTTON}}', useRelay),
  );
  if (page.includes('{{')) throw new Error('interstitial render left an unsubstituted marker');
  return page;
}

// serveControlRequest dispatches the canonical route surface for the
// sharebridge.app vhost: GET /s/{code} (and /share/{code}) per §9.1, POST
// /api/shares/{code}/prepare-route per §9.3, and the two real interstitial
// assets at their exact shipped paths. Every response is no-store.
function serveControlRequest(assets, req, res) {
  const send = (status, headers, body) => {
    noStore(res);
    for (const [k, v] of Object.entries(headers)) res.setHeader(k, v);
    res.writeHead(status);
    res.end(body);
  };
  const sendJSON = (status, obj) => send(status, { 'Content-Type': 'application/json' }, JSON.stringify(obj));

  const url = new URL(req.url, `https://${CONTROL_HOSTNAME}`);
  const shareMatch = /^\/(?:s|share)\/([A-Za-z0-9_-]+)$/.exec(url.pathname);
  if (req.method === 'GET' && shareMatch) {
    const code = shareMatch[1];
    const route = routes[code];
    if (!route) return send(404, { 'Content-Type': 'text/plain' }, 'session not found\n');
    if (route.kind === 'relayOnly') {
      // §6.1/§9.1 relayOnly branch: 302 to the control-constructed relay
      // origin, never cached — the browser never sees any direct surface.
      res.setHeader('Cache-Control', 'no-store');
      res.setHeader('Location', relayURLFor(code));
      res.writeHead(302);
      return res.end('<a href="/">Found</a>.\n\n');
    }
    // Direct candidate → the §9.3 no-store interstitial with the per-response
    // nonce CSP (relay elements only when the presence lease backs them).
    const nonce = randomBytes(24).toString('base64').replace(/=+$/, '');
    const relayLoc = route.relayPresence === false ? '' : relayURLFor(code);
    const page = renderInterstitialPage(assets, nonce, code, relayLoc);
    return send(
      200,
      { 'Content-Type': 'text/html; charset=utf-8', 'Content-Security-Policy': buildInterstitialCSP(nonce, NS_DOMAIN) },
      page,
    );
  }

  const prepareMatch = /^\/api\/shares\/([A-Za-z0-9_-]+)\/prepare-route$/.exec(url.pathname);
  if (req.method === 'POST' && prepareMatch) {
    const route = routes[prepareMatch[1]];
    if (!route) return sendJSON(404, { error: 'session not found' });
    if (route.prepare === 'unavailable') return sendJSON(503, { error: 'unavailable' });
    if (route.kind === 'relayOnly') return sendJSON(200, { status: 'relay', relay_url: relayURLFor(prepareMatch[1]) });
    // §9.3 direct success: control-derived URLs and the recipient budget the
    // page must enforce with its AbortController.
    return sendJSON(200, {
      status: 'direct',
      direct_url: directURLFor(prepareMatch[1], route.port),
      relay_url: relayURLFor(prepareMatch[1]),
      direct_timeout_ms: 4000,
    });
  }

  // The real shipped interstitial assets at their exact same-origin paths.
  if (req.method === 'GET' && url.pathname === '/web/route-interstitial.js') {
    return send(200, { 'Content-Type': 'text/javascript; charset=utf-8' }, assets.js);
  }
  if (req.method === 'GET' && url.pathname === '/web/route-interstitial.css') {
    return send(200, { 'Content-Type': 'text/css; charset=utf-8' }, assets.css);
  }
  return send(404, { 'Content-Type': 'text/plain' }, 'not found\n');
}

// ---------------------------------------------------------------------------
// Loopback CONNECT proxy: per-hostname virtual-port DNS shim + blackhole.
// ---------------------------------------------------------------------------

function startProxy(controlPort, agentPort) {
  const connections = [];
  const stalled = new Set();

  // resolveTarget maps a CONNECT hostname to its fixture tunnel: the control
  // listener for the control origin, the agent listener for every namespace
  // origin (relay suffix, or a direct candidate that is not a blackhole), a
  // deterministic stall for the blackhole codes, and nothing for anything
  // else (a real DNS failure — which the scoped CSP must prevent reaching).
  const resolveTarget = (host) => {
    if (host === CONTROL_HOSTNAME) return 'control';
    if (host.endsWith(RELAY_SUFFIX)) return 'agent';
    if (host.endsWith(DIRECT_SUFFIX)) {
      const label = host.slice(0, -DIRECT_SUFFIX.length);
      return routes[label]?.stall ? 'stall' : 'agent';
    }
    return null;
  };

  const server = http.createServer((req, res) => {
    // Plain HTTP on the proxy port carries only the log feed the spec reads
    // (never the browser — every browser request is HTTPS over CONNECT).
    if (req.method === 'GET' && req.url === '/__proxy/log') {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ connections }));
      return;
    }
    res.writeHead(404);
    res.end();
  });

  server.on('connect', (req, clientSocket, head) => {
    const [host, portStr] = String(req.url).split(':');
    connections.push({ host, port: Number(portStr || 443), at: Date.now() });
    const target = resolveTarget(host);
    if (target === null) {
      clientSocket.write('HTTP/1.1 403 Forbidden\r\n\r\n');
      clientSocket.destroy();
      return;
    }
    if (target === 'stall') {
      // Deterministic unroutable direct target: the tunnel is accepted so no
      // fast network error occurs, then every byte (the TLS ClientHello) is
      // swallowed and nothing ever answers — the browser's fetch hangs until
      // its §4.4 AbortController fires.
      clientSocket.write('HTTP/1.1 200 Connection established\r\n\r\n');
      stalled.add(clientSocket);
      clientSocket.on('data', () => {});
      clientSocket.on('error', () => clientSocket.destroy());
      clientSocket.on('close', () => stalled.delete(clientSocket));
      return;
    }
    const port = target === 'control' ? controlPort : agentPort;
    const upstream = net.connect(port, '127.0.0.1', () => {
      clientSocket.write('HTTP/1.1 200 Connection established\r\n\r\n');
      if (head && head.length) upstream.write(head);
      upstream.pipe(clientSocket);
      clientSocket.pipe(upstream);
    });
    const fail = () => {
      clientSocket.destroy();
      upstream.destroy();
    };
    upstream.on('error', fail);
    clientSocket.on('error', fail);
  });

  return new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => resolve({ server, port: server.address().port, stalled }));
  });
}

// ---------------------------------------------------------------------------
// The REAL agent data plane: agent/internal/direct's TestE2EBrowserFixture,
// built and driven as a Go test binary (the Task 25 go-test-driver seam).
// ---------------------------------------------------------------------------

function startAgentFixture() {
  const runDir = mkdtempSync(join(tmpdir(), 'sb-task24-'));
  const binPath = join(runDir, 'agent-fixture.test');
  execFileSync('go', ['test', '-c', '-o', binPath, './internal/direct'], { cwd: agentDir, stdio: 'inherit' });

  const child = spawn(binPath, ['-test.run', '^TestE2EBrowserFixture$'], {
    cwd: agentDir,
    env: { ...process.env, SB_E2E_BROWSER_FIXTURE: '1' },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  const state = { child, runDir, agentPort: 0, agentAdminPort: 0, exited: null };
  child.on('exit', (code, signal) => {
    state.exited = { code, signal };
    if (!state.resolved) {
      state.resolved = true;
      reject(new Error(`agent fixture exited early (code=${code} signal=${signal})`));
    }
  });

  let stdoutBuf = '';
  let reject = () => {};
  const ready = new Promise((res, rej) => {
    const done = (fn) => {
      if (!state.resolved) {
        state.resolved = true;
        fn();
      }
    };
    reject = (e) => done(() => rej(e));
    child.stdout.setEncoding('utf8');
    child.stdout.on('data', (chunk) => {
      stdoutBuf += chunk;
      process.stdout.write(chunk);
      for (const line of stdoutBuf.split('\n')) {
        const m = /^(SB_E2E_AGENT_[A-Z_]+)=(.*)$/.exec(line.trim());
        if (m) state[m[1]] = m[2];
      }
      if (state.SB_E2E_AGENT_READY === '1') {
        state.agentPort = Number(state.SB_E2E_AGENT_PORT);
        state.agentAdminPort = Number(state.SB_E2E_AGENT_ADMIN_PORT);
        done(res);
      }
    });
    child.stderr.setEncoding('utf8');
    child.stderr.on('data', (chunk) => process.stderr.write(chunk));
  });
  state.ready = Promise.race([
    ready,
    new Promise((_, rej) =>
      setTimeout(() => reject(new Error(`agent fixture not ready after 120s (stdout: ${stdoutBuf.slice(-2000)})`)), 120_000),
    ),
  ]);
  return state;
}

async function waitHealthy(agentAdminPort, timeoutMs = 15_000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      await new Promise((resolve, reject) => {
        const req = http.get({ host: '127.0.0.1', port: agentAdminPort, path: '/health' }, (res) => {
          res.resume();
          res.statusCode === 200 ? resolve() : reject(new Error(`health ${res.statusCode}`));
        });
        req.on('error', reject);
      });
      return;
    } catch {
      await new Promise((r) => setTimeout(r, 200));
    }
  }
  throw new Error('agent fixture admin endpoint never became healthy');
}

// selfSignedCert mints the fixture's throwaway TLS certificate (every engine
// runs with ignoreHTTPSErrors; only the TLS handshake shape must be real).
function selfSignedCert(runDir) {
  const keyPath = join(runDir, 'fixture-key.pem');
  const crtPath = join(runDir, 'fixture-crt.pem');
  execFileSync(
    'openssl',
    ['req', '-x509', '-newkey', 'rsa:2048', '-keyout', keyPath, '-out', crtPath, '-days', '2', '-nodes', '-subj', `/CN=${CONTROL_HOSTNAME}`],
    { stdio: 'ignore' },
  );
  return { key: readFileSync(keyPath), cert: readFileSync(crtPath) };
}

// ---------------------------------------------------------------------------
// setup / teardown
// ---------------------------------------------------------------------------

const runtime = { stopped: false };

export async function setup() {
  const assets = loadInterstitialAssets();
  assertInterstitialCSPGolden();
  const runDir = mkdtempSync(join(tmpdir(), 'sb-task24-ctl-'));
  const tls = selfSignedCert(runDir);

  const agent = startAgentFixture();
  await agent.ready;
  await waitHealthy(agent.agentAdminPort);

  const controlServer = https.createServer(tls, (req, res) => serveControlRequest(assets, req, res));
  await new Promise((resolve, reject) => {
    controlServer.once('error', reject);
    controlServer.listen(0, '127.0.0.1', resolve);
  });
  const controlPort = controlServer.address().port;

  // The control admin seam: the presence-lease regrant ritual the spec runs
  // before every case (in the fixture, relay presence is deterministically
  // backed, so the regrant is always acknowledged).
  const adminServer = http.createServer((req, res) => {
    if (req.method === 'POST' && req.url === '/admin/presence') {
      res.writeHead(200);
      res.end('ok');
      return;
    }
    res.writeHead(404);
    res.end();
  });
  await new Promise((resolve, reject) => {
    adminServer.once('error', reject);
    adminServer.listen(0, '127.0.0.1', resolve);
  });

  const proxy = await startProxy(controlPort, agent.agentPort);

  const state = {
    controlOrigin: `https://${CONTROL_HOSTNAME}`,
    namespaceDomain: NS_DOMAIN,
    controlPort,
    controlAdminPort: adminServer.address().port,
    proxyPort: proxy.port,
    agentPort: agent.agentPort,
    agentAdminPort: agent.agentAdminPort,
  };
  writeFileSync(join(here, '.state.json'), JSON.stringify(state, null, 2));

  Object.assign(runtime, {
    stopped: false,
    agent,
    controlServer,
    adminServer,
    proxyServer: proxy.server,
    proxyStalled: proxy.stalled,
    runDir,
  });
  console.log(`[fixture] control https on 127.0.0.1:${controlPort}, admin on :${state.controlAdminPort}, proxy on :${proxy.port}, agent on :${agent.agentPort}`);
  return state;
}

export async function teardown() {
  if (runtime.stopped) return;
  runtime.stopped = true;
  rmSync(join(here, '.state.json'), { force: true });
  const stop = async (server) => {
    if (!server) return;
    // closeAllConnections drops the engines' lingering keep-alive sockets —
    // without it close() waits on them and the teardown (and with it the
    // whole Playwright run) hangs.
    server.closeAllConnections?.();
    await new Promise((r) => server.close(() => r()));
  };
  await Promise.race([
    (async () => {
      await stop(runtime.controlServer);
      await stop(runtime.adminServer);
      await stop(runtime.proxyServer);
      if (runtime.proxyStalled) for (const s of runtime.proxyStalled) s.destroy();
      const agent = runtime.agent;
      if (agent?.child && agent.child.exitCode === null) {
        const exited = new Promise((r) => agent.child.once('exit', r));
        agent.child.kill('SIGTERM');
        const timer = new Promise((r) => setTimeout(r, 3000));
        await Promise.race([exited, timer]);
        if (agent.child.exitCode === null) agent.child.kill('SIGKILL');
        await exited;
      }
      if (runtime.runDir) rmSync(runtime.runDir, { recursive: true, force: true });
      if (agent?.runDir) rmSync(agent.runDir, { recursive: true, force: true });
    })(),
    // Bounded teardown: a stuck socket can never hang the run past this.
    new Promise((r) => setTimeout(r, 10_000)),
  ]);
}
