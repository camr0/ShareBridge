let pc, ws, dc;
let pendingCandidates = [];
let remoteDescSet = false;

// Download state
let currentFile = null;
let receivedBytes = 0;
let fileChunks = [];
let isDownloading = false;

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
  dc.binaryType = 'arraybuffer';  // Required: receive binary as ArrayBuffer, not Blob

  dc.onopen = () => {
    status('DataChannel open!');
    hideSection('join-section');
    showSection('file-list');
    // Request file list
    dc.send(JSON.stringify({ type: 'list_request' }));
  };

  dc.onmessage = (event) => {
    if (event.data instanceof ArrayBuffer) {
      // Binary frame: file chunk
      const bytes = new Uint8Array(event.data);
      appendChunk(bytes);
      return;
    }

    // Text frame: JSON control message
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
        status('Error: ' + msg.message);
        isDownloading = false;
        break;
    }
  };

  dc.onclose = () => {
    status('Connection closed');
    resetUI();
  };
}

function renderFileList(files) {
  const container = document.getElementById('file-list');
  container.innerHTML = '';

  if (files.length === 0) {
    container.innerHTML = '<p>No files in share</p>';
    return;
  }

  files.forEach(file => {
    const div = document.createElement('div');
    div.className = 'file-item';
    div.innerHTML = `
      <div>${escapeHtml(file.name)}</div>
      <div class="file-size">${formatBytes(file.size)}</div>
    `;
    div.onclick = () => requestFile(file.name);
    container.appendChild(div);
  });
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
  status(`Downloading ${header.name}...`);
  showSection('progress-container');
  updateProgress(0, header.size);
}

function appendChunk(bytes) {
  fileChunks.push(bytes);
  receivedBytes += bytes.length;
  updateProgress(receivedBytes, currentFile.size);
}

function updateProgress(received, total) {
  const pct = total > 0 ? Math.round((received / total) * 100) : 0;
  document.getElementById('progress-bar').style.width = pct + '%';
  document.getElementById('progress-text').textContent = `${pct}% (${formatBytes(received)} / ${formatBytes(total)})`;
}

function completeDownload() {
  // Combine all chunks
  const totalLength = fileChunks.reduce((sum, c) => sum + c.length, 0);
  const combined = new Uint8Array(totalLength);
  let offset = 0;
  for (const chunk of fileChunks) {
    combined.set(chunk, offset);
    offset += chunk.length;
  }

  // Create download
  const blob = new Blob([combined], { type: currentFile.mimeType || 'application/octet-stream' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = currentFile.name;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  URL.revokeObjectURL(url);

  status(`Downloaded ${currentFile.name}`);
  isDownloading = false;
  hideSection('progress-container');
  fileChunks = [];
}

function resetUI() {
  showSection('join-section');
  hideSection('file-list');
  hideSection('progress-container');
  document.getElementById('file-list').innerHTML = '';
  currentFile = null;
  fileChunks = [];
  isDownloading = false;
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
