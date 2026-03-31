let pc, ws, dc;
let pendingCandidates = [];
let remoteDescSet = false;

// Download state
let currentFile = null;
let receivedBytes = 0;
let fileChunks = [];
let isDownloading = false;
let transferStartTime = 0;

// Auth state
let authAttempts = 0;

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

  ws = new WebSocket(`ws://${location.host}/ws/client?session=${code}`);

  ws.onmessage = async (event) => {
    const msg = JSON.parse(event.data);

    switch (msg.type) {
      case 'ice_config':
        pc = new RTCPeerConnection({ iceServers: msg.ice_servers });

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
            status('Connection lost');
            resetUI();
          }
        };
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

  ws.onerror = () => status('WebSocket error');
  ws.onclose = () => {
    if (pc) pc.close();
    resetUI();
  };
}

function setupDataChannel() {
  dc.binaryType = 'arraybuffer';

  dc.onopen = () => {
    status('DataChannel open!');
    hideSection('join-section');
  };

  dc.onmessage = (event) => {
    if (event.data instanceof ArrayBuffer) {
      const bytes = new Uint8Array(event.data);
      appendChunk(bytes);
      return;
    }

    const msg = JSON.parse(event.data);
    switch (msg.type) {
      case 'hello':
        handleHello(msg);
        break;
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
    if (authAttempts >= 3) {
      status('Too many incorrect attempts. Connection closed.');
    } else {
      status('Connection closed');
    }
    resetUI();
  };
}

function renderFileList(files) {
  hideSection('password-section');
  showSection('file-list');
  const container = document.getElementById('file-list');
  container.innerHTML = '';

  if (files.length === 0) {
    container.innerHTML = '<p style="color:#6c7086;margin-top:8px">No files in share</p>';
    return;
  }

  files.forEach(file => {
    const div = document.createElement('div');
    div.className = 'file-item';
    div.dataset.name = file.name;
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
  dc.send(JSON.stringify({ type: 'file_request', name }));
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

function handleHello(msg) {
  if (msg.password_required) {
    showSection('password-section');
    document.getElementById('password-input').focus();
  } else {
    requestFileList('');
  }
}

function submitPassword() {
  const password = document.getElementById('password-input').value;
  requestFileList(password);
}

function requestFileList(password) {
  dc.send(JSON.stringify({ type: 'list_request', password: password }));
}

function handleError(msg) {
  const message = msg.message || '';
  if (message.toLowerCase().includes('incorrect password')) {
    authAttempts++;
    const errorDiv = document.getElementById('password-error');
    if (authAttempts >= 3) {
      errorDiv.textContent = 'Too many incorrect attempts. Connection closed.';
      document.getElementById('password-input').disabled = true;
      document.querySelector('#password-section button').disabled = true;
    } else {
      errorDiv.textContent = `Incorrect password. ${3 - authAttempts} attempts remaining.`;
      document.getElementById('password-input').value = '';
      document.getElementById('password-input').focus();
    }
  } else {
    status('Error: ' + message);
    isDownloading = false;
  }
}

function resetUI() {
  showSection('join-section');
  hideSection('password-section');
  hideSection('file-list');
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
  authAttempts = 0;
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
