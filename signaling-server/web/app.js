let pc, ws, dc;
let pendingCandidates = [];
let remoteDescSet = false;
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
let sessionPassword = ''; // set from URL hash on load, or from password input

// HMAC pre-challenge state
let pendingNonce = null; // nonce received from agent, consumed on join

function status(msg) {
  document.getElementById('status').textContent = msg;
}

function showSection(id) {
  document.getElementById(id).classList.remove('hidden');
}

function hideSection(id) {
  document.getElementById(id).classList.add('hidden');
}

function join() {
  const code = document.getElementById('code').value.trim();
  if (!code) return;
  status('Connecting...');

  // Reset quota state for new connection
  relayQuotaExceeded = false;
  quotaPeriodEnd = null;

  ws = new WebSocket(`ws://${location.host}/ws/client?session=${code}`);

  ws.onmessage = async (event) => {
    const msg = JSON.parse(event.data);

    switch (msg.type) {
      case 'ice_config':
        pc = new RTCPeerConnection({ iceServers: msg.ice_servers });

        // Store quota state for use if connection fails
        if (msg.relay_quota_exceeded) {
          relayQuotaExceeded = true;
          quotaPeriodEnd = msg.quota_period_end;
        }

        pc.onicecandidate = (e) => {
          if (e.candidate) {
            ws.send(JSON.stringify({
              type: 'ice_candidate',
              candidate: e.candidate.toJSON(),
            }));
          }
        };

        pc.ondatachannel = (e) => {
          dc = e.channel;
          setupDataChannel();
        };

        pc.onconnectionstatechange = () => {
          if (pc.connectionState === 'failed' || pc.connectionState === 'closed') {
            if (relayQuotaExceeded) {
              const periodEnd = quotaPeriodEnd ? new Date(quotaPeriodEnd).toLocaleDateString() : 'soon';
              status(`Connection failed: Direct unavailable, relay blocked (quota exceeded). Resets ${periodEnd}.`);
            } else {
              status('Connection lost');
            }
            resetUI();
          }
        };

        // Send knock immediately after ICE config received
        ws.send(JSON.stringify({ type: 'knock' }));
        break;

      case 'nonce':
        pendingNonce = msg.value;
        if (msg.has_password && !sessionPassword) {
          showSection('password-section');
          document.getElementById('password-input').focus();
        } else {
          sendJoin();
        }
        break;

      case 'auth_failed':
        {
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
            // Knock again to get a fresh nonce
            ws.send(JSON.stringify({ type: 'knock' }));
          }
        }
        break;

      case 'offer':
        if (!pc) return;
        await pc.setRemoteDescription({ type: 'offer', sdp: msg.sdp });
        remoteDescSet = true;

        for (const c of pendingCandidates) {
          await pc.addIceCandidate(c);
        }
        pendingCandidates = [];

        const answer = await pc.createAnswer();
        await pc.setLocalDescription(answer);
        ws.send(JSON.stringify({ type: 'answer', sdp: answer.sdp }));
        status('Negotiating...');
        break;

      case 'ice_candidate':
        if (!pc) return;
        if (!remoteDescSet) {
          pendingCandidates.push(msg.candidate);
        } else {
          await pc.addIceCandidate(msg.candidate);
        }
        break;

      case 'error':
        status('Error: ' + msg.message);
        break;
    }
  };

  ws.onerror = () => {
    if (relayQuotaExceeded) {
      const periodEnd = quotaPeriodEnd ? new Date(quotaPeriodEnd).toLocaleDateString() : 'soon';
      status(`Connection failed: Direct unavailable, relay blocked (quota exceeded). Resets ${periodEnd}.`);
    } else {
      status('WebSocket error');
    }
  };
  ws.onclose = () => {
    if (pc) pc.close();
    resetUI();
  };
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

async function sendJoin() {
  if (!pendingNonce) return;
  const hmac = sessionPassword ? await computeHMAC(sessionPassword, pendingNonce) : '';
  ws.send(JSON.stringify({ type: 'join', hmac }));
  pendingNonce = null;
}

function setupDataChannel() {
  dc.binaryType = 'arraybuffer';

  dc.onopen = () => {
    status('DataChannel open!');
    hideSection('join-section');
    hideSection('password-section');
    setTimeout(updateConnectionStatus, 1000);
    // Request file list immediately - auth already done via HMAC
    requestFileList('');
  };

  dc.onmessage = (event) => {
    if (event.data instanceof ArrayBuffer) {
      const bytes = new Uint8Array(event.data);
      appendChunk(bytes);
      return;
    }

    const msg = JSON.parse(event.data);
    switch (msg.type) {
      case 'file_list':
        renderFileList(msg.files);
        break;
      case 'file_header':
        startDownload(msg);
        break;
      case 'chunk_end':
        completeDownload();
        break;
      case 'error':
        handleError(msg);
        break;
    }
  };

  dc.onclose = () => {
    status('Connection closed');
    resetUI();
  };
}

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
    } catch (_) {
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
  sendJoin();
}

function requestFileList(subpath) {
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

async function detectConnectionType() {
  if (!pc) return null;

  const stats = await pc.getStats();
  let selectedPair = null;
  let localCandidateType = null;
  let remoteCandidateType = null;

  // Pass 1: find the nominated (selected) candidate pair
  stats.forEach(report => {
    if (report.type === 'candidate-pair' && report.state === 'succeeded' && report.nominated) {
      selectedPair = report;
    }
  });

  // If no nominated pair found, ICE hasn't settled yet
  if (!selectedPair) return null;

  // Pass 2: look up candidate types using the selected pair's IDs
  stats.forEach(report => {
    if (report.type === 'local-candidate' && report.id === selectedPair.localCandidateId) {
      localCandidateType = report.candidateType;
    }
    if (report.type === 'remote-candidate' && report.id === selectedPair.remoteCandidateId) {
      remoteCandidateType = report.candidateType;
    }
  });

  // If either side uses relay, it's a relayed connection
  if (localCandidateType === 'relay' || remoteCandidateType === 'relay') {
    return 'relay';
  }
  return 'direct';
}

function updateConnectionStatus(retries = 5) {
  detectConnectionType().then(type => {
    if (!type) {
      // ICE not settled yet, retry with backoff
      if (retries > 0) {
        setTimeout(() => updateConnectionStatus(retries - 1), 500);
      }
      return;
    }

    const statusEl = document.getElementById('connection-status');
    const typeEl = document.getElementById('connection-type');

    statusEl.classList.remove('hidden');
    typeEl.classList.remove('connection-direct', 'connection-relay');

    if (type === 'direct') {
      typeEl.textContent = '● Connected (Direct)';
      typeEl.classList.add('connection-direct');
    } else {
      typeEl.textContent = '● Connected (Relay)';
      typeEl.classList.add('connection-relay');
    }
  });
}

function initFromURL() {
  const parts = window.location.pathname.split('/');
  // /s/ABC123 → ['', 's', 'ABC123']
  if (parts[1] === 's' && parts[2]) {
    document.getElementById('code').value = parts[2];
  }

  if (window.location.hash) {
    sessionPassword = decodeURIComponent(window.location.hash.slice(1));
    history.replaceState(null, '', window.location.pathname);
  }
}

document.addEventListener('DOMContentLoaded', () => {
  initFromURL();
  if (document.getElementById('code').value) {
    join();
  }
});
