import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import net from 'node:net';
import path from 'node:path';
import { spawn } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { chromium } from 'playwright';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const serverDir = path.resolve(__dirname, '..', '..');
const repoRoot = path.resolve(serverDir, '..');
const agentDir = path.join(repoRoot, 'agent');
const baseURL = process.env.SHAREBRIDGE_BASE_URL ?? 'http://127.0.0.1:8080';
const signalingURL = process.env.SIGNALING_SERVER ?? 'ws://127.0.0.1:8080';
const dataDir = process.env.DATA_DIR ?? '/tmp/sharebridge-pb_data';
const relayListenAddr = process.env.RELAY_LISTEN_ADDR ?? '/ip4/127.0.0.1/tcp/9001/ws';
const sharebridgeAPIKey =
  process.env.SHAREBRIDGE_API_KEY ?? 'g68k4qshlyl06ie.zSCvKUKdY71wevq0URy_KHff2MBoQGHHw7hkmHv4MVs';
const shareCode = process.env.SHAREBRIDGE_SHARE_CODE ?? 'n69ftwbo';
const headless = process.env.PLAYWRIGHT_HEADLESS === '1';

test('browser can open debug websocket and receive hello frame', { timeout: 60000 }, async () => {
  const runURL = new URL(baseURL);
  const wsURL = `${runURL.protocol === 'https:' ? 'wss' : 'ws'}://${runURL.host}/ws/debug`;
  const serverAlreadyRunning = await isReachable(runURL.href);
  const server = serverAlreadyRunning ? null : startShareBridgeServer();
  let agent;

  let browser;
  try {
    await waitForReachable(runURL.href, server, 'server');
    if (!serverAlreadyRunning) {
      agent = startAgent();
      await sleep(1500);
    }

    browser = await chromium.launch({ headless });
    const page = await browser.newPage();
    await page.goto(runURL.href, { waitUntil: 'load' });
    await page.evaluate(() => {
      document.body.innerHTML = '<pre id="log" style="font: 14px/1.4 monospace; white-space: pre-wrap;"></pre>';
    });

    const result = await page.evaluate((debugWsUrl) => {
      return new Promise((resolve) => {
        const events = [];
        const logNode = document.getElementById('log');
        const log = (line) => {
          if (logNode) {
            logNode.textContent += `${line}\n`;
          }
        };
        log(`ws ${debugWsUrl}`);
        const ws = new WebSocket(debugWsUrl);
        const finish = (extra = {}) => {
          clearTimeout(timeout);
          resolve({ events, ...extra });
        };
        const timeout = setTimeout(() => finish({ timedOut: true, readyState: ws.readyState }), 5000);

        ws.onopen = () => {
          events.push({ type: 'open' });
          log('open');
        };
        ws.onmessage = (event) => {
          events.push({ type: 'message', data: String(event.data) });
          log(`message ${String(event.data)}`);
          ws.close(1000, 'done');
        };
        ws.onerror = () => {
          events.push({ type: 'error' });
          log('error');
        };
        ws.onclose = (event) => {
          events.push({
            type: 'close',
            code: event.code,
            reason: event.reason,
            wasClean: event.wasClean,
          });
          log(`close code=${event.code} reason=${event.reason} clean=${event.wasClean}`);
          finish({ readyState: ws.readyState });
        };
      });
    }, wsURL);

    assert.equal(result.timedOut, undefined, `websocket timed out: ${JSON.stringify(result)}`);
    assert.ok(
      result.events.some((event) => event.type === 'open'),
      `expected websocket open event, got ${JSON.stringify(result.events)}`,
    );
    const hello = result.events.find((event) => event.type === 'message');
    assert.ok(hello, `expected hello message, got ${JSON.stringify(result.events)}`);
    assert.match(hello.data, /"type":"hello"/, `unexpected hello payload: ${hello.data}`);
  } finally {
    if (browser) {
      await browser.close();
    }
    await stopProcess(agent);
    await stopProcess(server);
  }
});

test('share page connects relay transport without transport error', { timeout: 60000 }, async () => {
  const runURL = new URL(baseURL);
  const shareURL = new URL(`/s/${shareCode}?debug=1`, runURL);
  const serverAlreadyRunning = await isReachable(runURL.href);
  const server = serverAlreadyRunning ? null : startShareBridgeServer();
  let agent;

  let browser;
  try {
    await waitForReachable(runURL.href, server, 'server');

    const sessionInfo = await fetchJSON(new URL(`/sessions/${shareCode}`, runURL).href);
    if (!sessionInfo?.is_active) {
      agent = startAgent(await getFreePort());
      await waitForSessionActive(runURL, shareCode, agent);
    }

    browser = await chromium.launch({ headless });
    const page = await browser.newPage();
    const consoleLines = [];
    page.on('console', (msg) => {
      consoleLines.push(msg.text());
    });
    page.on('pageerror', (err) => {
      consoleLines.push(`pageerror ${String(err)}`);
    });

    await page.goto(shareURL.href, { waitUntil: 'load' });
    await page.waitForTimeout(12000);

    const body = await page.locator('body').innerText();
    const joinedLogs = consoleLines.join('\n');

    assert.match(joinedLogs, /\[sharebridge\] ws:relay-info -> startTransport/, joinedLogs);
    assert.match(
      joinedLogs,
      /\[sharebridge\] transport:connected/,
      `expected relay transport to connect, got logs:\n${joinedLogs}\n\nBody:\n${body}`,
    );
    assert.doesNotMatch(
      body,
      /Transport error:/,
      `share page reported transport error\n${body}\n\nLogs:\n${joinedLogs}`,
    );
  } finally {
    if (browser) {
      await browser.close();
    }
    await stopProcess(agent);
    await stopProcess(server);
  }
});

function startShareBridgeServer() {
  return spawnManaged(
    'server',
    'go',
    ['run', './cmd/server', 'serve', '--http=127.0.0.1:8080'],
    {
      cwd: serverDir,
      env: {
        ...process.env,
        DEBUG_WS: '1',
        JWT_SECRET: process.env.JWT_SECRET ?? randomHex(32),
        PORT: '8080',
        DATA_DIR: dataDir,
        RELAY_LISTEN_ADDR: relayListenAddr,
      },
    },
  );
}

function startAgent(uiPort) {
  return spawnManaged(
    'agent',
    'go',
    ['run', './cmd/agent', 'daemon'],
    {
      cwd: agentDir,
      env: {
        ...process.env,
        SIGNALING_SERVER: signalingURL,
        SHAREBRIDGE_API_KEY: sharebridgeAPIKey,
        UI_PORT: String(uiPort),
      },
    },
  );
}

function spawnManaged(name, command, args, options) {
  const child = spawn(command, args, {
    ...options,
    detached: true,
    stdio: ['ignore', 'pipe', 'pipe'],
  });

  child.stdout.setEncoding('utf8');
  child.stderr.setEncoding('utf8');

  let output = '';
  child.stdout.on('data', (chunk) => {
    output += chunk;
  });
  child.stderr.on('data', (chunk) => {
    output += chunk;
  });

  child.output = () => output;
  child.label = name;
  return child;
}

function fetchStatus(url) {
  return new Promise((resolve, reject) => {
    const req = http.get(url, (res) => {
      res.resume();
      resolve(res.statusCode ?? 0);
    });
    req.on('error', reject);
  });
}

function fetchJSON(url) {
  return new Promise((resolve, reject) => {
    const req = http.get(url, (res) => {
      let body = '';
      res.setEncoding('utf8');
      res.on('data', (chunk) => {
        body += chunk;
      });
      res.on('end', () => {
        try {
          resolve(body ? JSON.parse(body) : null);
        } catch (error) {
          reject(error);
        }
      });
    });
    req.on('error', reject);
  });
}

async function waitForReachable(url, child, label) {
  const deadline = Date.now() + 20000;
  while (Date.now() < deadline) {
    if (child?.exitCode != null) {
      throw new Error(`${label} exited early (${child.exitCode})\n${child.output()}`);
    }
    try {
      const status = await fetchStatus(url);
      if (status > 0) {
        return;
      }
    } catch {
      // keep polling until ready
    }
    await sleep(200);
  }
  throw new Error(`${label} at ${url} did not become reachable\n${child?.output?.() ?? ''}`);
}

async function isReachable(url) {
  try {
    return (await fetchStatus(url)) > 0;
  } catch {
    return false;
  }
}

async function waitForSessionActive(runURL, code, agent) {
  const deadline = Date.now() + 20000;
  const sessionURL = new URL(`/sessions/${code}`, runURL).href;
  while (Date.now() < deadline) {
    if (agent?.exitCode != null) {
      throw new Error(`agent exited early (${agent.exitCode})\n${agent.output()}`);
    }
    try {
      const sessionInfo = await fetchJSON(sessionURL);
      if (sessionInfo?.is_active) {
        return;
      }
    } catch {
      // keep polling until ready
    }
    await sleep(250);
  }
  throw new Error(`session ${code} did not become active\n${agent?.output?.() ?? ''}`);
}

async function stopProcess(child) {
  if (!child || child.exitCode != null) {
    return;
  }
  const pgid = -child.pid;
  try {
    process.kill(pgid, 'SIGTERM');
  } catch (error) {
    if (error.code !== 'ESRCH') {
      throw error;
    }
  }
  const exited = await Promise.race([
    new Promise((resolve) => child.once('exit', resolve)),
    sleep(5000).then(() => false),
  ]);
  if (exited === false && child.exitCode == null) {
    try {
      process.kill(pgid, 'SIGKILL');
    } catch (error) {
      if (error.code !== 'ESRCH') {
        throw error;
      }
    }
    await new Promise((resolve) => child.once('exit', resolve));
  }
}

function getFreePort() {
  return new Promise((resolve, reject) => {
    const server = net.createServer();
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => {
      const address = server.address();
      server.close((err) => {
        if (err) {
          reject(err);
          return;
        }
        resolve(address.port);
      });
    });
  });
}

function randomHex(bytes) {
  return randomBytes(bytes).toString('hex');
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
