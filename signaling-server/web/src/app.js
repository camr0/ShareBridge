// src/app.js
import { createNode, getLocalPeerId, connect } from './libp2pClient.js';
import { getConnectionBadgeType, shouldResetUiOnSignalingClose } from './appState.js';

let ws;
let dc;                 // LibP2PDataChannel (RTCDataChannel-shaped)
let node;               // js-libp2p node
let relayQuotaExceeded = false;
let quotaPeriodEnd = null;

// Download state
let currentFile = null;
let receivedBytes = 0;
let fileChunks = [];
let isDownloading = false;
let transferStartTime = 0;

// Navigation state
let currentPath = [];
let sessionPassword = '';

// HMAC pre-challenge state
let pendingNonce = null;

// Connection info received from the signaling server after auth_ok.
let pendingConnInfo = null; // { relay_multiaddr, agent_peer_id, jwt, conn_id, share_code, relay_allowed, dcutr_allowed }

function debugEnabled() {
  if (window.SHAREBRIDGE_DEBUG === true) return true;
  const params = new URLSearchParams(window.location.search);
  if (params.get('debug') === '1') return true;
  try {
    return window.localStorage.getItem('sharebridge:debug') === '1';
  } catch {
    return false;
  }
}

function debug(...args) {
  if (debugEnabled()) {
    console.log('[sharebridge]', ...args);
  }
}

function summarizeMessage(msg) {
  if (!msg || typeof msg !== 'object') return msg;
  const summary = { ...msg };
  if ('jwt' in summary) {
    summary.jwt = '<redacted>';
  }
  if ('value' in summary && typeof summary.value === 'string') {
    summary.value = `<nonce:${summary.value.length}>`;
  }
  if ('token' in summary) {
    summary.token = '<redacted>';
  }
  return summary;
}

function status(msg) {
  document.getElementById('status').textContent = msg;
}

function showSection(id) {
  document.getElementById(id).classList.remove('hidden');
}

function hideSection(id) {
  document.getElementById(id).classList.add('hidden');
}

async function join() {
  const code = document.getElementById('code').value.trim();
  if (!code) return;
  status('Starting libp2p...');
  debug('join:start', { code });

  relayQuotaExceeded = false;
  quotaPeriodEnd = null;

  // 1. Start the libp2p node so we have a peer ID before the knock.
  if (!node) {
    node = await createNode();
    debug('libp2p:node-created');
  }
  const browserPeerId = getLocalPeerId(node);
  debug('libp2p:peer-id', browserPeerId);

  // 2. Open the signaling WebSocket.
  const protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const wsURL = `${protocol}//${location.host}/ws/client?session=${code}`;
  debug('ws:create', wsURL);
  ws = new WebSocket(wsURL);

  ws.onopen = () => {
    debug('ws:open');
    status('Connecting...');
    // Send knock with our browser peer id up-front so the server has it
    // available when it later mints the JWT.
    const knock = { type: 'knock', browser_peer_id: browserPeerId };
    debug('ws:send', summarizeMessage(knock));
    ws.send(JSON.stringify(knock));
  };

  ws.onmessage = async (event) => {
    debug('ws:message:raw', event.data);
    const msg = JSON.parse(event.data);
    debug('ws:message', summarizeMessage(msg));
    switch (msg.type) {
      case 'quota_status':
        if (msg.relay_quota_exceeded) {
          relayQuotaExceeded = true;
          quotaPeriodEnd = msg.quota_period_end;
        }
        break;

      case 'nonce':
        pendingNonce = msg.value;
        if (msg.has_password && !sessionPassword) {
          showSection('password-section');
          document.getElementById('password-input').focus();
        } else {
          debug('ws:nonce -> sendJoin');
          await sendJoin(browserPeerId, code);
        }
        break;

      case 'auth_failed': {
        const errorDiv = document.getElementById('password-error');
        const attemptsRemaining = msg.attempts_remaining || 0;
        if (attemptsRemaining <= 0) {
          errorDiv.textContent = 'Too many incorrect attempts. Connection closed.';
          document.getElementById('password-input').disabled = true;
          document.querySelector('#password-section button').disabled = true;
        } else {
          errorDiv.textContent = `Incorrect password. ${attemptsRemaining} attempt${attemptsRemaining === 1 ? '' : 's'} remaining.`;
          document.getElementById('password-input').value = '';
          document.getElementById('password-input').focus();
          ws.send(JSON.stringify({ type: 'knock', browser_peer_id: browserPeerId }));
        }
        break;
      }

      case 'relay_info':
        // Server has confirmed HMAC success and minted the JWT.
        pendingConnInfo = {
          relayMultiaddr: msg.relay_multiaddr,
          agentPeerId: msg.agent_peer_id,
          jwt: msg.jwt,
          shareCode: code,
          connId: msg.conn_id,
          relayAllowed: msg.relay_allowed,
          dcutrAllowed: msg.dcutr_allowed,
        };
        // If relay is not allowed and DCUtR is not allowed, we cannot transfer.
        if (!pendingConnInfo.relayAllowed && !pendingConnInfo.dcutrAllowed) {
          status('Connection failed: relay quota exceeded and direct connection unavailable.');
          return;
        }
        debug('ws:relay-info -> startTransport', summarizeMessage(pendingConnInfo));
        await startTransport();
        break;

      case 'error':
        debug('ws:error-message', summarizeMessage(msg));
        status('Error: ' + msg.message);
        break;
    }
  };

  ws.onerror = (event) => {
    debug('ws:error-event', event);
    if (relayQuotaExceeded) {
      const periodEnd = quotaPeriodEnd ? new Date(quotaPeriodEnd).toLocaleDateString() : 'soon';
      status(`Connection failed: Direct unavailable, relay blocked (quota exceeded). Resets ${periodEnd}.`);
    } else {
      status('WebSocket error');
    }
  };

  ws.onclose = (event) => {
    debug('ws:close', { code: event.code, reason: event.reason, wasClean: event.wasClean });
    ws = null;
    if (!shouldResetUiOnSignalingClose(dc)) {
      debug('ws:close:transport-still-active', { readyState: dc.readyState });
      return;
    }
    resetUI();
  };
}

async function startTransport() {
  try {
    status('Connecting to relay...');
    debug('transport:start', summarizeMessage(pendingConnInfo));
    dc = await connect(node, pendingConnInfo);
    debug('transport:connected');
    setupDataChannel();
    debug('transport:pump-start');
    void dc.start();
  } catch (err) {
    console.error(err);
    debug('transport:error', err);
    status('Transport error: ' + (err.message || err));
  }
}

async function computeHMAC(password, nonce) {
  const encoder = new TextEncoder();
  const key = await crypto.subtle.importKey(
    'raw',
    encoder.encode(password),
    { name: 'HMAC', hash: 'SHA-256' },
    false,
    ['sign']
  );
  const signature = await crypto.subtle.sign('HMAC', key, encoder.encode(nonce));
  return Array.from(new Uint8Array(signature))
    .map(b => b.toString(16).padStart(2, '0'))
    .join('');
}

async function sendJoin(browserPeerId, shareCode) {
  if (!pendingNonce) return;
  const hmac = sessionPassword ? await computeHMAC(sessionPassword, pendingNonce) : '';
  const joinMsg = {
    type: 'join',
    hmac,
    browser_peer_id: browserPeerId,
  };
  debug('ws:send', { type: 'join', hmac: hmac ? '<redacted>' : '', browser_peer_id: browserPeerId });
  ws.send(JSON.stringify(joinMsg));
  pendingNonce = null;
}

function setupDataChannel() {
  dc.onopen = () => {
    debug('dc:open');
    status('Connection open');
    hideSection('join-section');
    hideSection('password-section');
    setTimeout(updateConnectionStatus, 500);
    requestFileList('');
  };

  dc.onmessage = (event) => {
    if (event.data instanceof ArrayBuffer) {
      debug('dc:message:binary', { bytes: event.data.byteLength });
      const bytes = new Uint8Array(event.data);
      appendChunk(bytes);
      return;
    }
    debug('dc:message:text:raw', event.data);
    const msg = JSON.parse(event.data);
    debug('dc:message:text', summarizeMessage(msg));
    switch (msg.type) {
      case 'file_list':   renderFileList(msg.files); break;
      case 'file_header': startDownload(msg); break;
      case 'chunk_end':   completeDownload(); break;
      case 'error':       handleError(msg); break;
    }
  };

  dc.onclose = () => {
    debug('dc:close');
    status('Connection closed');
    resetUI();
  };
}

// --- Download logic (unchanged from old app.js) ---

function renderFileList(files) {
  hideSection('password-section');
  showSection('file-list');
  renderBreadcrumb();

  const container = document.getElementById('file-list');
  container.innerHTML = '';

  if (files.length === 0) {
    const emptyMsg = currentPath.length === 0 ? 'No files in share' : 'No files in this folder';
    container.innerHTML = `<p style="color:#6c7086;margin-top:8px">${emptyMsg}</p>`;
    return;
  }

  // Sort: folders first, then files, each group alphabetically
  const sorted = [...files].sort((a, b) => {
    if (a.isDir !== b.isDir) return a.isDir ? -1 : 1;
    return a.name.localeCompare(b.name);
  });

  sorted.forEach(file => {
    const div = document.createElement('div');
    div.className = 'file-item';
    div.dataset.name = file.name;

    if (file.isDir) {
      div.innerHTML = `
        <div class="file-main">
          <div class="file-name">📁 ${escapeHtml(file.name)}</div>
        </div>
      `;
      div.onclick = () => openFolder(file.name);
    } else {
      div.innerHTML = `
        <div class="file-main">
          <div class="file-name">${escapeHtml(file.name)}</div>
          <div class="file-status"></div>
        </div>
        <div class="file-meta">
          <span class="file-size">${formatBytes(file.size)}</span>
          <span class="file-hash"></span>
        </div>
        <div class="file-progress hidden">
          <div class="file-progress-track">
            <div class="file-progress-fill"></div>
          </div>
          <div class="file-progress-text"></div>
        </div>
      `;
      div.onclick = () => requestFile(file.name);
    }
    container.appendChild(div);
  });
}

function getFileItem(name) {
  return document.querySelector(`.file-item[data-name="${CSS.escape(name)}"]`);
}

function requestFile(name) {
  if (isDownloading) {
    status('Download in progress, please wait');
    return;
  }
  const fullPath = [...currentPath, name].join('/');
  debug('dc:send:file_request', { path: fullPath });
  dc.send(JSON.stringify({ type: 'file_request', path: fullPath }));
}

function startDownload(header) {
  currentFile = header;
  receivedBytes = 0;
  fileChunks = [];
  isDownloading = true;
  transferStartTime = Date.now();
  status('');

  const fileItem = getFileItem(header.name);
  if (fileItem) {
    fileItem.classList.add('downloading');
    fileItem.onclick = null;
    fileItem.querySelector('.file-progress').classList.remove('hidden');
    fileItem.querySelector('.file-progress-text').textContent = '0%';
  }
}

function appendChunk(bytes) {
  if (!currentFile) return;
  fileChunks.push(bytes);
  receivedBytes += bytes.length;

  const pct = currentFile.size > 0 ? Math.round((receivedBytes / currentFile.size) * 100) : 0;
  const elapsed = (Date.now() - transferStartTime) / 1000;
  const speedBps = elapsed > 0 ? receivedBytes / elapsed : 0;

  const fileItem = getFileItem(currentFile.name);
  if (fileItem) {
    fileItem.querySelector('.file-progress-fill').style.width = pct + '%';
    fileItem.querySelector('.file-progress-text').textContent =
      `${pct}% — ${formatBytes(receivedBytes)} of ${formatBytes(currentFile.size)}`;
    const statusEl = fileItem.querySelector('.file-status');
    statusEl.textContent = '↓ ' + formatSpeed(speedBps);
    statusEl.className = 'file-status speed';
  }
}

async function completeDownload() {
  if (!currentFile) return;
  // Assemble all chunks
  const totalLength = fileChunks.reduce((sum, c) => sum + c.length, 0);
  const combined = new Uint8Array(totalLength);
  let offset = 0;
  for (const chunk of fileChunks) {
    combined.set(chunk, offset);
    offset += chunk.length;
  }

  // Trigger browser file save immediately — don't wait for SHA-1
  const blob = new Blob([combined], { type: currentFile.mimeType || 'application/octet-stream' });
  const objectUrl = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = objectUrl;
  a.download = currentFile.name;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  URL.revokeObjectURL(objectUrl);

  const elapsed = (Date.now() - transferStartTime) / 1000;
  const avgSpeed = elapsed > 0 ? formatSpeed(totalLength / elapsed) : '—';

  // Capture state before reset
  const downloadedFile = currentFile;
  const downloadedBuffer = combined;

  // Reset transfer state
  isDownloading = false;
  status('');
  currentFile = null;
  fileChunks = [];
  receivedBytes = 0;
  transferStartTime = 0;

  const fileItem = getFileItem(downloadedFile.name);
  if (fileItem) {
    fileItem.querySelector('.file-progress').classList.add('hidden');
  }

  // SHA-1 verification (async — updates UI after save dialog appears)
  if (downloadedFile.sha1 && typeof crypto !== 'undefined' && crypto.subtle) {
    try {
      const hashBuffer = await crypto.subtle.digest('SHA-1', downloadedBuffer.buffer);
      const hashArray = Array.from(new Uint8Array(hashBuffer));
      const computed = hashArray.map(b => b.toString(16).padStart(2, '0')).join('');
      const ok = computed === downloadedFile.sha1.toLowerCase();

      if (fileItem) {
        fileItem.classList.remove('downloading');
        fileItem.classList.add(ok ? 'verified' : 'corrupted');
        fileItem.querySelector('.file-status').textContent = ok ? '✓ intact' : '✗ corrupted';
        fileItem.querySelector('.file-status').className = 'file-status ' + (ok ? 'ok' : 'fail');
        fileItem.querySelector('.file-size').textContent =
          formatBytes(downloadedFile.size) + ' · avg ' + avgSpeed;
        fileItem.querySelector('.file-hash').textContent = ok
          ? 'SHA-1: ' + downloadedFile.sha1.toLowerCase()
          : 'expected ' + downloadedFile.sha1.slice(0, 8) + '… got ' + computed.slice(0, 8) + '…';
      }
    } catch (e) {
      markFileDone(fileItem, downloadedFile, avgSpeed);
    }
  } else {
    markFileDone(fileItem, downloadedFile, avgSpeed);
  }
}

function markFileDone(fileItem, file, avgSpeed) {
  if (!fileItem) return;
  fileItem.classList.remove('downloading');
  fileItem.classList.add('done');
  fileItem.querySelector('.file-status').textContent = '✓ done';
  fileItem.querySelector('.file-status').className = 'file-status ok';
  fileItem.querySelector('.file-size').textContent =
    formatBytes(file.size) + ' · avg ' + avgSpeed;
  if (file.sha1) {
    fileItem.querySelector('.file-hash').textContent = 'SHA-1: ' + file.sha1.toLowerCase();
  }
}

function renderBreadcrumb() {
  const breadcrumb = document.getElementById('breadcrumb');
  if (currentPath.length === 0) {
    breadcrumb.classList.add('hidden');
    return;
  }
  breadcrumb.classList.remove('hidden');
  const parts = [
    { label: 'Share root', index: -1 },
    ...currentPath.map((seg, i) => ({ label: seg, index: i })),
  ];
  breadcrumb.innerHTML = parts.map((part, i) => {
    const isLast = i === parts.length - 1;
    if (isLast) {
      return `<span class="breadcrumb-current">${escapeHtml(part.label)}</span>`;
    }
    return `<span class="breadcrumb-link" onclick="navigateTo(${part.index})">${escapeHtml(part.label)}</span>`;
  }).join('<span class="breadcrumb-sep"> › </span>');
}

function navigateTo(index) {
  // index -1 = share root, 0 = first segment, 1 = second, etc.
  currentPath = index === -1 ? [] : currentPath.slice(0, index + 1);
  requestFileList(currentPath.join('/'));
}

function openFolder(name) {
  if (isDownloading) {
    status('Download in progress, please wait');
    return;
  }
  currentPath.push(name);
  requestFileList(currentPath.join('/'));
}

function submitPassword() {
  sessionPassword = document.getElementById('password-input').value;
  const code = document.getElementById('code').value.trim();
  if (ws && ws.readyState === WebSocket.OPEN && pendingNonce) {
    // We already received nonce, just send join
    const browserPeerId = node ? getLocalPeerId(node) : '';
    sendJoin(browserPeerId, code);
  }
}

function requestFileList(subpath) {
  debug('dc:send:list_request', { path: subpath });
  dc.send(JSON.stringify({ type: 'list_request', path: subpath }));
}

function handleError(msg) {
  const message = msg.message || '';
  status('Error: ' + message);
  isDownloading = false;
}

function resetUI() {
  showSection('join-section');
  hideSection('password-section');
  hideSection('file-list');
  document.getElementById('connection-status').classList.add('hidden');
  document.getElementById('connection-type').className = 'connection-badge';
  document.getElementById('breadcrumb').classList.add('hidden');
  document.getElementById('file-list').innerHTML = '';
  document.getElementById('password-error').textContent = '';
  document.getElementById('password-input').value = '';
  document.getElementById('password-input').disabled = false;
  document.querySelector('#password-section button').disabled = false;
  currentFile = null;
  fileChunks = [];
  isDownloading = false;
  receivedBytes = 0;
  transferStartTime = 0;
  pendingNonce = null;
  currentPath = [];
  sessionPassword = '';
}

function escapeHtml(text) {
  const div = document.createElement('div');
  div.textContent = text;
  return div.innerHTML;
}

function formatBytes(bytes) {
  if (bytes === 0) return '0 B';
  const k = 1024;
  const sizes = ['B', 'KB', 'MB', 'GB'];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + ' ' + sizes[i];
}

function formatSpeed(bps) {
  if (bps >= 1024 * 1024) return (bps / (1024 * 1024)).toFixed(1) + ' MB/s';
  if (bps >= 1024) return (bps / 1024).toFixed(0) + ' KB/s';
  return Math.round(bps) + ' B/s';
}

// --- Connection status (libp2p-based) ---

async function updateConnectionStatus() {
  // libp2p gives us explicit connection type instead of inferring from ICE.
  const statusEl = document.getElementById('connection-status');
  const typeEl = document.getElementById('connection-type');
  statusEl.classList.remove('hidden');
  typeEl.classList.remove('connection-direct', 'connection-relay');

  const type = detectConnectionType();
  if (type === 'direct') {
    typeEl.textContent = '● Connected (Direct)';
    typeEl.classList.add('connection-direct');
  } else {
    typeEl.textContent = '● Connected (Relay)';
    typeEl.classList.add('connection-relay');
  }
}

function detectConnectionType() {
  return getConnectionBadgeType(pendingConnInfo);
}

// Periodically re-check in case DCUtR upgrades the connection mid-session.
setInterval(() => {
  if (dc && dc.readyState === 'open') updateConnectionStatus();
}, 5000);

function initFromURL() {
  const parts = window.location.pathname.split('/');
  if (parts[1] === 's' && parts[2]) {
    document.getElementById('code').value = parts[2];
  }
  if (window.location.hash) {
    sessionPassword = decodeURIComponent(window.location.hash.slice(1));
    history.replaceState(null, '', window.location.pathname);
  }
  debug('init', {
    code: document.getElementById('code').value,
    debug: debugEnabled(),
  });
}

// Expose functions used by inline HTML onclick handlers.
window.join = join;
window.submitPassword = submitPassword;
window.navigateTo = navigateTo;

document.addEventListener('DOMContentLoaded', () => {
  initFromURL();
  if (document.getElementById('code').value) {
    join();
  }
});
