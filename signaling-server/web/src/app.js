import { DirectChannel, waitForDirectChannelOpen } from './directChannel.js'
import { SecureRelayChannel } from './secureRelayChannel.js'
import { connectTransferChannel, buildDirectIceServers, decodeRelayPolicyToken } from './connectTransferChannel.js'
import { detectDownloadSupport } from './downloadCapabilities.js'
import {
  createBlobSink,
  createBrowserStreamWriter,
  createIncrementalSha1,
  createSafariBrowserStreamWriter,
  createStreamingSink,
} from './downloadSinks.js'
import { createDownloadPipeline } from './downloadPipeline.js'
import { decodeBinaryEnvelope, FRAME_FILE_CHUNK, FRAME_THUMBNAIL } from './binaryEnvelope.js'
import { createGalleryController } from './gallery.js'

// Module state
let pc, ws, dc
let transferChannel = null // Unified channel: DirectChannel or SecureRelayChannel
let currentTransferMode = null // 'direct' or 'relay'
let pendingCandidates = []
let remoteDescSet = false
let relayQuotaExceeded = false
let quotaPeriodEnd = null
let activeDownload = null

const DEBUG = typeof location !== 'undefined' && (
  location.search.includes('debug=1') ||
  (typeof localStorage !== 'undefined' && localStorage.getItem('sharebridge_debug'))
)

function debugLog(...args) {
  if (DEBUG) console.log('[secure-relay]', ...args)
}

// Direct connection promise handling
let directChannelResolve = null
let directChannelReject = null
let directChannelPromise = new Promise((resolve, reject) => {
  directChannelResolve = resolve
  directChannelReject = reject
})

// Download state
let currentFile = null
let receivedBytes = 0
let transferStartTime = 0
let receivedChunkCount = 0

// Navigation state
let currentPath = []
let sessionCode = ''
let galleryMode = false
let galleryController = null
let galleryAssetRequestPending = false
let sessionPassword = '' // set from URL hash on load, or from password input

// HMAC pre-challenge state
let pendingNonce = null // nonce received from agent, consumed on join

export function detectPathMode(pathname = window.location.pathname) {
  const [, prefix, ...rest] = pathname.split('/')
  const code = decodeURIComponent(rest.join('/'))
  if (prefix === 'i' && code) return { mode: 'gallery', code }
  if (prefix === 's' && code) return { mode: 'files', code }
  return { mode: 'files', code: '' }
}

export function assertJoinNotActive(socket, log = debugLog) {
  if (!socket) return
  if (socket.readyState === 3) return

  const err = new Error(`join() re-entered while browser signaling socket is still active (readyState=${socket.readyState})`)
  log('FATAL', err.message)
  throw err
}

export function isTransferChannelActive(channel) {
  return Boolean(channel && (channel.readyState === 'open' || channel.readyState === 'connecting'))
}

export function detachBrowserSignalingSocket(socket, log = debugLog) {
  if (!socket) {
    return null
  }

  log('detaching browser signaling WebSocket', { readyState: socket.readyState })
  socket.onmessage = null
  socket.onerror = (event) => {
    log('ignoring browser signaling WebSocket error after transfer channel open', event)
  }
  socket.onclose = (event) => {
    log('ignoring browser signaling WebSocket close after transfer channel open', {
      code: event?.code,
      reason: event?.reason,
      wasClean: event?.wasClean,
    })
  }

  if (socket.readyState === 0 || socket.readyState === 1) {
    socket.close(1000, 'transfer channel active')
  }

  return null
}

export function handleBrowserSignalingClose({
  event,
  transferChannel,
  pc,
  resetUI,
  log = debugLog,
}) {
  if (isTransferChannelActive(transferChannel)) {
    log('ignoring browser signaling WebSocket close after transfer channel became active', {
      code: event?.code,
      reason: event?.reason,
      wasClean: event?.wasClean,
      transferMode: currentTransferMode,
      channelReadyState: transferChannel?.readyState,
    })
    return false
  }

  log('browser signaling WebSocket close', { code: event.code, reason: event.reason, wasClean: event.wasClean })
  if (pc) pc.close()
  resetUI()
  return true
}

// Export for testing
export function publishGlobalActions(globals, { join, submitPassword, navigateTo }) {
  globals.join = join
  globals.submitPassword = submitPassword
  globals.navigateTo = navigateTo
}

// Export for testing
export function applyConnectionBadge({ statusContainer, badge, mode }) {
  statusContainer.classList.remove('hidden')
  badge.classList.remove('connection-direct', 'connection-relay')
  if (mode === 'direct') {
    badge.textContent = '● Connected (Direct)'
    badge.classList.add('connection-direct')
  } else {
    badge.textContent = '● Connected (Relay)'
    badge.classList.add('connection-relay')
  }
}

export function initializeIceConfigTransport({
  msg,
  ws,
  createPeerConnection = (config) => new RTCPeerConnection(config),
  onDirectChannel,
  onDirectFailure,
}) {
  if (msg.relay_only) {
    debugLog('ice_config indicates relay_only; skipping RTCPeerConnection setup')
    return null
  }

  const peer = createPeerConnection({ iceServers: buildDirectIceServers(msg.ice_servers) })
  debugLog('created RTCPeerConnection for direct path')

  peer.onicecandidate = (e) => {
    if (e.candidate) {
      debugLog('sending ICE candidate to signaling server')
      ws.send(
        JSON.stringify({
          type: 'ice_candidate',
          candidate: e.candidate.toJSON(),
        })
      )
    }
  }

  peer.ondatachannel = (e) => {
    dc = e.channel
    dc.binaryType = 'arraybuffer'
    debugLog('received RTCDataChannel from direct peer')
    const directChannel = new DirectChannel(dc)
    onDirectChannel(directChannel)
  }

  peer.onconnectionstatechange = () => {
    debugLog('RTCPeerConnection state change', peer.connectionState)
    if (peer.connectionState === 'failed' || peer.connectionState === 'closed') {
      onDirectFailure()
    }
  }

  debugLog('sending initial knock from browser for non-relay_only session')
  ws.send(JSON.stringify({ type: 'knock' }))
  return peer
}

// Export for testing - creates a message handler with injected dependencies
export function installSessionMessageHandler({
  updateStatus,
  connectTransferChannel: connectTransfer,
  requestFileList,
  applyConnectionBadge: applyBadge,
  decodeRelayPolicyToken: decodeToken,
  hideSection,
  showSection = () => {},
  isGalleryMode = () => galleryMode,
}) {
  return {
    async handleMessage(msg) {
      switch (msg.type) {
        case 'password_required':
          hideSection('join-section')
          showSection('password-section')
          updateStatus('This Immich share is password protected.')
          break
        case 'auth_fail':
          showSection('password-section')
          updateStatus(`Incorrect password. ${msg.attempts_remaining ?? 0} attempts remaining.`)
          break
        case 'relay_policy':
          updateStatus('Connecting to agent...')
          const relayPolicy = decodeToken(msg.token)
          // Use relay_only from message (server's authoritative value), not from decoded token
          relayPolicy.relayOnly = msg.relay_only
          relayPolicy.relayAllowed = msg.relay_allowed

          const directConnect = async () => {
            const channel = await directChannelPromise
            return waitForDirectChannelOpen(channel)
          }

          const relayConnect = async () => {
            const relayToken = msg.token
            const protocol = location.protocol === 'https:' ? 'wss:' : 'ws:'
            const relayURL = `${protocol}//${location.host}/ws/relay`
            const expectedStaticPub = hexToBytes(relayPolicy.expectedStaticPubHex)

            const relayChannel = new SecureRelayChannel({
              relayURL,
              relayToken,
              expectedStaticPub,
            })
            await relayChannel.start()
            return relayChannel
          }

          let result
          try {
            result = await connectTransfer({
              relayPolicy,
              directConnect,
              relayConnect,
              onStatusChange: (s) => {
                // Map internal states to UI messages
                if (s === 'connecting-direct') updateStatus('Connecting directly...')
                else if (s === 'connecting-relay') updateStatus('Connecting via relay...')
                else if (s === 'falling-back-to-relay') updateStatus('Direct failed, using relay...')
                else if (s === 'connected-direct') updateStatus('Connected directly')
                else if (s === 'connected-relay') updateStatus('Connected via relay')
                else if (s === 'failed') updateStatus('Connection failed')
              },
            })
          } catch (err) {
            updateStatus(err.message)
            throw err
          }

          transferChannel = result.channel
          currentTransferMode = result.mode
          attachTransferChannel({
            channel: transferChannel,
            mode: result.mode,
            updateStatus,
            hideSection,
            showSection,
            requestFileList,
            applyConnectionBadge: ({ mode }) => applyBadge({ mode }),
            handleTransferMessage,
            onClose: handleTransferClosure,
            isGalleryMode,
          })
          break
      }
    },
  }
}

function hexToBytes(hex) {
  const bytes = new Uint8Array(hex.length / 2)
  for (let i = 0; i < bytes.length; i++) {
    bytes[i] = parseInt(hex.substr(i * 2, 2), 16)
  }
  return bytes
}

function attachTransferChannel({
  channel,
  mode,
  updateStatus,
  hideSection,
  showSection = () => {},
  requestFileList,
  applyConnectionBadge,
  handleTransferMessage,
  onClose,
  isGalleryMode = () => galleryMode,
}) {
  let opened = false
  let messageChain = Promise.resolve()
  const handleOpen = () => {
    if (opened) return
    opened = true
    debugLog('transfer channel opened', { mode, readyState: channel.readyState })
    updateStatus('Transfer channel open!')
    hideSection('join-section')
    hideSection('password-section')
    applyConnectionBadge({ mode })
    if (isGalleryMode()) {
      ensureGalleryController()
      showSection('gallery-section')
      updateStatus('Loading gallery...')
    } else {
      requestFileList(channel, '')
    }
    ws = detachBrowserSignalingSocket(ws)
  }

  channel.onopen = handleOpen
  channel.onmessage = (event) => {
    messageChain = messageChain.then(() => handleTransferMessage(event)).catch((err) => {
      console.error('[secure-relay] transfer message handler error:', err)
    })
  }
  channel.onclose = () => {
    debugLog('transfer channel closed', {
      mode,
      channelReadyState: channel.readyState,
      currentFile: currentFile?.name || null,
      receivedBytes,
      receivedChunkCount,
    })
    updateStatus('Connection closed')
    void Promise.resolve(onClose()).catch((err) => {
      console.error('[secure-relay] transfer close handler error:', err)
    })
  }

  if (channel.readyState === 'open') {
    handleOpen()
  }
}

function getConnectionStatusEl() {
  return document.getElementById('connection-status')
}

function getConnectionTypeEl() {
  return document.getElementById('connection-type')
}

function updateStatus(msg) {
  document.getElementById('status').textContent = msg
}

function showSection(id) {
  document.getElementById(id).classList.remove('hidden')
}

function hideSection(id) {
  document.getElementById(id).classList.add('hidden')
}

function join() {
  assertJoinNotActive(ws)

  const code = document.getElementById('code').value.trim()
  if (!code) return
  debugLog('join invoked', { code })
  updateStatus('Connecting...')

  // Reset quota state for new connection
  relayQuotaExceeded = false
  quotaPeriodEnd = null

  // Reset direct channel promise for new connection
  directChannelPromise = new Promise((resolve, reject) => {
    directChannelResolve = resolve
    directChannelReject = reject
  })

  const protocol = location.protocol === 'https:' ? 'wss:' : 'ws:'
  ws = new WebSocket(`${protocol}//${location.host}/ws/client?session=${code}`)
  debugLog('opening browser signaling WebSocket', ws.url)

  ws.onopen = () => {
    debugLog('browser signaling WebSocket open')
  }

  ws.onmessage = async (event) => {
    try {
      debugLog('browser signaling WebSocket message received', event.data)

      let msg
      try {
        msg = JSON.parse(event.data)
      } catch (err) {
        debugLog('failed to parse signaling message JSON', err)
        throw err
      }

      switch (msg.type) {
        case 'ice_config':
          debugLog('handling ice_config', msg)
          // Store quota state for use if connection fails
          if (msg.relay_quota_exceeded) {
            relayQuotaExceeded = true
            quotaPeriodEnd = msg.quota_period_end
          }

          pc = initializeIceConfigTransport({
            msg,
            ws,
            onDirectChannel: (directChannel) => {
              directChannelResolve(directChannel)
            },
            onDirectFailure: () => {
              if (relayQuotaExceeded) {
                const periodEnd = quotaPeriodEnd ? new Date(quotaPeriodEnd).toLocaleDateString() : 'soon'
                updateStatus(`Connection failed: Direct unavailable, relay blocked (quota exceeded). Resets ${periodEnd}.`)
              } else {
                updateStatus('Connection lost')
              }
              if (directChannelReject) {
                directChannelReject(new Error('PeerConnection failed'))
              }
              void handleTransferClosure()
            },
          })
          break

        case 'nonce':
          debugLog('received nonce for browser challenge', { hasPassword: msg.has_password, connID: msg.conn_id })
          pendingNonce = msg.value
          if (msg.has_password && !sessionPassword) {
            debugLog('nonce requires password input before join')
            showSection('password-section')
            document.getElementById('password-input').focus()
          } else {
            debugLog('nonce can be consumed immediately; sending join')
            sendJoin()
          }
          break

        case 'auth_failed': {
          debugLog('received auth_failed', msg)
          const errorDiv = document.getElementById('password-error')
          const attemptsRemaining = msg.attempts_remaining || 0
          if (attemptsRemaining <= 0) {
            errorDiv.textContent = 'Too many incorrect attempts. Connection closed.'
            document.getElementById('password-input').disabled = true
            document.querySelector('#password-section button').disabled = true
          } else {
            errorDiv.textContent = `Incorrect password. ${attemptsRemaining} attempt${attemptsRemaining === 1 ? '' : 's'} remaining.`
            document.getElementById('password-input').value = ''
            document.getElementById('password-input').focus()
            // Knock again to get a fresh nonce
            ws.send(JSON.stringify({ type: 'knock' }))
          }
          break
        }

        case 'password_required':
          hideSection('join-section')
          showSection('password-section')
          updateStatus('This Immich share is password protected.')
          break

        case 'auth_fail':
          showSection('password-section')
          updateStatus(`Incorrect password. ${msg.attempts_remaining ?? 0} attempts remaining.`)
          break

        case 'offer':
          debugLog('received WebRTC offer')
          if (!pc) return
          await pc.setRemoteDescription({ type: 'offer', sdp: msg.sdp })
          remoteDescSet = true

          for (const c of pendingCandidates) {
            await pc.addIceCandidate(c)
          }
          pendingCandidates = []

          const answer = await pc.createAnswer()
          await pc.setLocalDescription(answer)
          ws.send(JSON.stringify({ type: 'answer', sdp: answer.sdp }))
          updateStatus('Negotiating...')
          break

        case 'ice_candidate':
          debugLog('received ICE candidate from signaling server')
          if (!pc) return
          if (!remoteDescSet) {
            pendingCandidates.push(msg.candidate)
          } else {
            await pc.addIceCandidate(msg.candidate)
          }
          break

        case 'relay_policy': {
          debugLog('received relay_policy', msg)
          updateStatus('Connecting to agent...')
          debugLog('relay_policy: decoding token')
          const relayPolicy = decodeRelayPolicyToken(msg.token)
          debugLog('relay_policy: decoded token', relayPolicy)
          // Use relay_only from message (server's authoritative value), not from decoded token
          // Token may be empty when relay is not configured
          relayPolicy.relayOnly = msg.relay_only
          relayPolicy.relayAllowed = msg.relay_allowed
          debugLog('relay_policy: effective policy', relayPolicy)

          const directConnect = async () => {
            debugLog('relay_policy: directConnect invoked')
            const channel = await directChannelPromise
            return waitForDirectChannelOpen(channel)
          }

          const relayConnect = async () => {
            debugLog('relayConnect called, creating SecureRelayChannel')
            const relayToken = msg.token
            const protocol = location.protocol === 'https:' ? 'wss:' : 'ws:'
            const relayURL = `${protocol}//${location.host}/ws/relay`
            const expectedStaticPub = hexToBytes(relayPolicy.expectedStaticPubHex)

            debugLog('relayConnect: creating channel', { relayURL, expectedStaticPubLength: expectedStaticPub?.length })
            const relayChannel = new SecureRelayChannel({
              relayURL,
              relayToken,
              expectedStaticPub,
            })
            debugLog('relayConnect: calling channel.start()')
            await relayChannel.start()
            debugLog('relayConnect: channel.start() completed successfully')
            return relayChannel
          }

          let result
          try {
            debugLog('relay_policy: calling connectTransferChannel')
            result = await connectTransferChannel({
              relayPolicy,
              directConnect,
              relayConnect,
              onStatusChange: (s) => {
                debugLog('connectTransferChannel status', { status: s })
                if (s === 'connecting-direct') updateStatus('Connecting directly...')
                else if (s === 'connecting-relay') updateStatus('Connecting via relay...')
                else if (s === 'falling-back-to-relay') updateStatus('Direct failed, using relay...')
                else if (s === 'connected-direct') updateStatus('Connected directly')
                else if (s === 'connected-relay') updateStatus('Connected via relay')
                else if (s === 'failed') updateStatus('Connection failed')
              },
            })
            debugLog('relay_policy: connectTransferChannel resolved', { mode: result?.mode, readyState: result?.channel?.readyState })
          } catch (err) {
            debugLog('connectTransferChannel failed', err)
            updateStatus(err.message)
            throw err
          }

          transferChannel = result.channel
          currentTransferMode = result.mode
          attachTransferChannel({
            channel: transferChannel,
            mode: result.mode,
            updateStatus,
            hideSection,
            showSection,
            requestFileList,
            applyConnectionBadge: ({ mode }) =>
              applyConnectionBadge({
                statusContainer: getConnectionStatusEl(),
                badge: getConnectionTypeEl(),
                mode,
              }),
            handleTransferMessage,
            onClose: handleTransferClosure,
            isGalleryMode: () => galleryMode,
          })
          break
        }

        case 'error':
          debugLog('received signaling error message', msg)
          updateStatus('Error: ' + msg.message)
          break
      }
    } catch (err) {
      console.error('[secure-relay] signaling message handler error:', err)
    }
  }

  ws.onerror = (event) => {
    if (isTransferChannelActive(transferChannel)) {
      debugLog('ignoring browser signaling WebSocket error after transfer channel became active', event)
      return
    }
    debugLog('browser signaling WebSocket error', event)
    if (relayQuotaExceeded) {
      const periodEnd = quotaPeriodEnd ? new Date(quotaPeriodEnd).toLocaleDateString() : 'soon'
      updateStatus(`Connection failed: Direct unavailable, relay blocked (quota exceeded). Resets ${periodEnd}.`)
    } else {
      updateStatus('WebSocket error')
    }
  }
  ws.onclose = (event) => {
    handleBrowserSignalingClose({
      event,
      transferChannel,
      pc,
      resetUI,
      log: debugLog,
    })
  }
}

async function computeHMAC(password, nonce) {
  const encoder = new TextEncoder()
  const key = await crypto.subtle.importKey('raw', encoder.encode(password), { name: 'HMAC', hash: 'SHA-256' }, false, [
    'sign',
  ])
  const signature = await crypto.subtle.sign('HMAC', key, encoder.encode(nonce))
  return Array.from(new Uint8Array(signature))
    .map((b) => b.toString(16).padStart(2, '0'))
    .join('')
}

async function sendJoin() {
  if (!pendingNonce) {
    debugLog('sendJoin called without a pending nonce; skipping')
    return
  }
  debugLog('computing HMAC for join', { hasPassword: Boolean(sessionPassword), nonceLength: pendingNonce.length })
  const hmac = sessionPassword ? await computeHMAC(sessionPassword, pendingNonce) : ''
  debugLog('sending join message to signaling server', { hmacLength: hmac.length })
  ws.send(JSON.stringify({ type: 'join', hmac }))
  pendingNonce = null
}

async function handleTransferMessage(event) {
  if (event.data instanceof ArrayBuffer) {
    debugLog('transfer binary message received', {
      byteLength: event.data.byteLength,
      currentFile: currentFile?.name || null,
    })
    if (galleryController) {
      const frame = decodeBinaryEnvelope(event.data)
      if (frame.type === FRAME_THUMBNAIL) {
        galleryController.handleThumbnailData(frame.index, frame.payload)
        return
      }
    }
    if (!currentFile?.binary_envelope) {
      await appendChunk(new Uint8Array(event.data))
      return
    }
    const frame = decodeBinaryEnvelope(event.data)
    if (frame.type === FRAME_FILE_CHUNK) {
      await appendChunk(frame.payload)
      return
    }
    if (frame.type === FRAME_THUMBNAIL) {
      debugLog('thumbnail frame received before gallery UI is enabled', { index: frame.index })
      return
    }
    return
  }

  debugLog('transfer text message received', {
    length: typeof event.data === 'string' ? event.data.length : undefined,
    preview: typeof event.data === 'string' ? event.data.slice(0, 160) : String(event.data),
  })
  const msg = JSON.parse(event.data)
  switch (msg.type) {
    case 'file_list':
      debugLog('file_list received', { count: msg.files?.length || 0, currentPath })
      renderFileList(msg.files)
      break
    case 'thumbnail_list':
      ensureGalleryController()
      galleryController?.handleThumbnailList(msg)
      break
    case 'file_header':
      await startDownload(msg)
      break
    case 'chunk_end':
      await completeDownload()
      break
    case 'error':
      await handleError(msg)
      break
    default:
      debugLog('unhandled transfer message type', { type: msg.type })
      break
  }
}

function renderFileList(files) {
  hideSection('password-section')
  showSection('file-list')
  renderBreadcrumb()

  const container = document.getElementById('file-list')
  container.innerHTML = ''

  if (files.length === 0) {
    const emptyMsg = currentPath.length === 0 ? 'No files in share' : 'No files in this folder'
    container.innerHTML = `<p style="color:#6c7086;margin-top:8px">${emptyMsg}</p>`
    return
  }

  // Sort: folders first, then files, each group alphabetically
  const sorted = [...files].sort((a, b) => {
    if (a.isDir !== b.isDir) return a.isDir ? -1 : 1
    return a.name.localeCompare(b.name)
  })

  sorted.forEach((file) => {
    const div = document.createElement('div')
    div.className = 'file-item'
    div.dataset.name = file.name

    if (file.isDir) {
      div.innerHTML = `
        <div class="file-main">
          <div class="file-name">📁 ${escapeHtml(file.name)}</div>
        </div>
      `
      div.onclick = () => openFolder(file.name)
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
      `
      div.onclick = () => requestFile(file.name)
    }
    container.appendChild(div)
  })
}

function getFileItem(name) {
  return document.querySelector(`.file-item[data-name="${CSS.escape(name)}"]`)
}

function requestFile(name) {
  if (activeDownload) {
    updateStatus('Download in progress, please wait')
    return
  }
  if (!transferChannel) {
    updateStatus('Connection closed')
    return
  }
  const fullPath = [...currentPath, name].join('/')
  debugLog('requesting file', {
    name,
    fullPath,
    mode: currentTransferMode,
    channelReadyState: transferChannel?.readyState,
  })
  transferChannel.send(JSON.stringify({ type: 'file_request', path: fullPath }))
}

async function getDownloadSupport(header) {
  return detectDownloadSupport({
    fileSize: header.size,
    userAgent: typeof navigator !== 'undefined' ? navigator.userAgent : '',
    registerServiceWorker: () => navigator.serviceWorker.register('/src/vendor/streamsaver-sw.js'),
    registerExperimentalServiceWorker: () => navigator.serviceWorker.register('/src/vendor/streamsaver-safari-sw.js'),
  })
}

async function startDownload(header) {
  galleryAssetRequestPending = false
  currentFile = header
  receivedBytes = 0
  transferStartTime = Date.now()
  receivedChunkCount = 0
  debugLog('download started', {
    name: header.name,
    size: header.size,
    mimeType: header.mimeType,
    sha1: header.sha1 || null,
    mode: currentTransferMode,
  })
  updateStatus('')

  const fileItem = getFileItem(header.name)
  if (fileItem) {
    fileItem.classList.add('downloading')
    fileItem.onclick = null
    fileItem.querySelector('.file-progress').classList.remove('hidden')
    fileItem.querySelector('.file-progress-text').textContent = '0%'
    fileItem.querySelector('.file-progress-fill').style.width = '0%'
    fileItem.querySelector('.file-hash').textContent = ''
  }

  const support = await getDownloadSupport(header)
  renderDownloadWarning(support.warning)

  if (support.mode === 'fail') {
    finalizeDownloadUI(header, {
      ok: false,
      code: 'unsupported',
      statusClass: 'failed',
      statusText: '✗ experimental download unavailable',
      avgBytesPerSecond: 0,
      computedSha1: null,
    })
    currentFile = null
    transferStartTime = 0
    galleryAssetRequestPending = false
    return
  }

  try {
    activeDownload = await createDownloadPipeline({
      header,
      sinkFactory: async () => buildDownloadSink(header, support),
      onStateChange: (state) => updateDownloadUI(header, state),
      onTerminalState: (result) => {
        finalizeDownloadUI(header, result)
        activeDownload = null
        currentFile = null
        galleryAssetRequestPending = false
        receivedBytes = 0
        receivedChunkCount = 0
        transferStartTime = 0
      },
    })
  } catch (err) {
    debugLog('download initialization failed', {
      file: header.name,
      error: err instanceof Error ? err.message : String(err),
    })
    finalizeDownloadUI(header, {
      ok: false,
      code: 'init-failed',
      statusClass: 'failed',
      statusText: '✗ failed',
      avgBytesPerSecond: 0,
      computedSha1: null,
    })
    activeDownload = null
    currentFile = null
    galleryAssetRequestPending = false
    receivedBytes = 0
    receivedChunkCount = 0
    transferStartTime = 0
  }
}

async function appendChunk(bytes) {
  if (!activeDownload || !currentFile) return
  await activeDownload.append(bytes)
}

async function completeDownload() {
  if (!activeDownload || !currentFile) return
  debugLog('download complete frame received', {
    file: currentFile.name,
    receivedBytes,
    chunkCount: receivedChunkCount,
  })
  await activeDownload.complete()
}

async function buildDownloadSink(header, support) {
  if (support.mode === 'experimental-streaming') {
    return createStreamingSink({
      fileName: header.name,
      mimeType: header.mimeType,
      tailBytes: 1024 * 1024,
      createWriter: createSafariBrowserStreamWriter,
      createHasher: createIncrementalSha1,
    })
  }

  if (support.mode === 'streaming') {
    return createStreamingSink({
      fileName: header.name,
      mimeType: header.mimeType,
      tailBytes: 1024 * 1024,
      createWriter: createBrowserStreamWriter,
      createHasher: createIncrementalSha1,
    })
  }

  return createBlobSink({
    fileName: header.name,
    mimeType: header.mimeType,
    subtleDigest: (algorithm, bytes) => crypto.subtle.digest(algorithm, bytes),
    triggerBrowserSave: saveBlobToDisk,
  })
}

function renderDownloadWarning(warning) {
  const el = document.getElementById('download-warning')
  if (!el) return

  if (!warning) {
    el.textContent = ''
    el.className = 'download-warning hidden'
    return
  }

  el.textContent = warning.message
  el.className = `download-warning${warning.level === 'strong' ? ' download-warning-strong' : ''}`
}

function saveBlobToDisk(blob, fileName = currentFile?.name || 'download') {
  const objectUrl = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = objectUrl
  anchor.download = fileName
  document.body.appendChild(anchor)
  anchor.click()
  document.body.removeChild(anchor)
  URL.revokeObjectURL(objectUrl)
}

function updateDownloadUI(file, state) {
  const fileItem = getFileItem(file.name)
  if (!fileItem) return

  receivedBytes = state.receivedBytes
  receivedChunkCount = state.receivedChunkCount

  if (receivedChunkCount <= 3 || receivedChunkCount % 25 === 0 || receivedBytes === file.size) {
    debugLog('download progress update', {
      file: file.name,
      phase: state.phase,
      receivedBytes,
      expectedBytes: file.size,
      receivedChunkCount,
    })
  }

  if (state.phase === 'verifying') {
    const statusEl = fileItem.querySelector('.file-status')
    statusEl.textContent = '… verifying'
    statusEl.className = 'file-status speed'
    return
  }

  const pct = file.size > 0 ? Math.round((state.receivedBytes / file.size) * 100) : 0
  const elapsed = (Date.now() - transferStartTime) / 1000
  const speedBps = elapsed > 0 ? state.receivedBytes / elapsed : 0

  fileItem.querySelector('.file-progress-fill').style.width = pct + '%'
  fileItem.querySelector('.file-progress-text').textContent =
    `${pct}% — ${formatBytes(state.receivedBytes)} of ${formatBytes(file.size)}`
  const statusEl = fileItem.querySelector('.file-status')
  statusEl.textContent = '↓ ' + formatSpeed(speedBps)
  statusEl.className = 'file-status speed'
}

function finalizeDownloadUI(file, result) {
  if (result.code !== 'disconnected' && result.code !== 'transfer-error') {
    updateStatus('')
  }
  applyFinalDownloadState(getFileItem(file.name), file, result)
}

function applyFinalDownloadState(fileItem, file, result) {
  if (!fileItem) return

  fileItem.classList.remove('downloading', 'verified', 'corrupted', 'done', 'failed')
  fileItem.classList.add(result.statusClass)
  fileItem.querySelector('.file-progress').classList.add('hidden')
  fileItem.querySelector('.file-status').textContent = result.statusText
  fileItem.querySelector('.file-status').className = 'file-status ' + (result.ok ? 'ok' : 'fail')
  fileItem.querySelector('.file-size').textContent = `${formatBytes(file.size)} · avg ${formatSpeed(result.avgBytesPerSecond || 0)}`

  if (file.sha1 && result.ok) {
    fileItem.querySelector('.file-hash').textContent = 'SHA-1: ' + file.sha1.toLowerCase()
    return
  }

  if (file.sha1 && result.computedSha1) {
    fileItem.querySelector('.file-hash').textContent =
      `expected ${file.sha1.slice(0, 8).toLowerCase()}… got ${result.computedSha1.slice(0, 8).toLowerCase()}…`
    return
  }

  fileItem.querySelector('.file-hash').textContent = ''
}

function renderBreadcrumb() {
  const breadcrumb = document.getElementById('breadcrumb')
  if (currentPath.length === 0) {
    breadcrumb.classList.add('hidden')
    return
  }
  breadcrumb.classList.remove('hidden')
  const parts = [{ label: 'Share root', index: -1 }, ...currentPath.map((seg, i) => ({ label: seg, index: i }))]
  breadcrumb.innerHTML = parts
    .map((part, i) => {
      const isLast = i === parts.length - 1
      if (isLast) {
        return `<span class="breadcrumb-current">${escapeHtml(part.label)}</span>`
      }
      return `<span class="breadcrumb-link" onclick="navigateTo(${part.index})">${escapeHtml(part.label)}</span>`
    })
    .join('<span class="breadcrumb-sep"> › </span>')
}

function navigateTo(index) {
  // index -1 = share root, 0 = first segment, 1 = second, etc.
  currentPath = index === -1 ? [] : currentPath.slice(0, index + 1)
  if (!transferChannel) {
    updateStatus('Connection closed')
    return
  }
  requestFileList(transferChannel, currentPath.join('/'))
}

function openFolder(name) {
  if (activeDownload) {
    updateStatus('Download in progress, please wait')
    return
  }
  if (!transferChannel) {
    updateStatus('Connection closed')
    return
  }
  currentPath.push(name)
  requestFileList(transferChannel, currentPath.join('/'))
}

function requestFileList(channel, subpath) {
  if (!channel) {
    updateStatus('Connection closed')
    return
  }
  debugLog('requesting file list', {
    subpath,
    mode: currentTransferMode,
    channelReadyState: channel?.readyState,
  })
  channel.send(JSON.stringify({ type: 'list_request', path: subpath }))
}

function submitPassword() {
  sessionPassword = document.getElementById('password-input').value
  if (galleryMode) {
    ws.send(JSON.stringify({ type: 'password_submit', code: sessionCode, password: sessionPassword }))
    return
  }
  sendJoin()
}

async function handleError(msg) {
  const message = msg.message || ''
  debugLog('transfer error message received', {
    message,
    currentFile: currentFile?.name || null,
    receivedBytes,
    receivedChunkCount,
  })

  if (activeDownload?.fail) {
    if (message === 'transfer in progress') {
      updateStatus('Download in progress, please wait')
      return
    }
    updateStatus('Transfer failed')
    await activeDownload.fail('transfer-error', message || 'Transfer failed')
    clearClosedTransferSession()
    return
  }

  galleryAssetRequestPending = false
  updateStatus('Error: ' + message)
}

async function handleTransferClosure() {
  const download = activeDownload
  if (download?.failForDisconnect) {
    await download.failForDisconnect()
    clearClosedTransferSession()
    return
  }

  resetUI()
}

function clearClosedTransferSession() {
  galleryAssetRequestPending = false
  transferChannel = null
  currentTransferMode = null
  getConnectionStatusEl().classList.add('hidden')
  getConnectionTypeEl().className = 'connection-badge'
  showSection('join-section')
}

function resetUI() {
  debugLog('resetUI', {
    currentFile: currentFile?.name || null,
    receivedBytes,
    receivedChunkCount,
    currentTransferMode,
  })
  showSection('join-section')
  hideSection('password-section')
  hideSection('file-list')
  hideSection('gallery-section')
  getConnectionStatusEl().classList.add('hidden')
  getConnectionTypeEl().className = 'connection-badge'
  document.getElementById('breadcrumb').classList.add('hidden')
  document.getElementById('file-list').innerHTML = ''
  document.getElementById('password-error').textContent = ''
  document.getElementById('password-input').value = ''
  document.getElementById('password-input').disabled = false
  document.querySelector('#password-section button').disabled = false
  renderDownloadWarning(null)
  activeDownload = null
  currentFile = null
  galleryAssetRequestPending = false
  receivedBytes = 0
  receivedChunkCount = 0
  transferStartTime = 0
  pendingNonce = null
  currentPath = []
  sessionPassword = ''
  galleryController?.destroy()
  galleryController = null
  transferChannel = null
  currentTransferMode = null
  ws = null
  pc = null
  dc = null
  pendingCandidates = []
  remoteDescSet = false
}

function escapeHtml(text) {
  const div = document.createElement('div')
  div.textContent = text
  return div.innerHTML
}

function formatBytes(bytes) {
  if (bytes === 0) return '0 B'
  const k = 1024
  const sizes = ['B', 'KB', 'MB', 'GB']
  const i = Math.floor(Math.log(bytes) / Math.log(k))
  return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + ' ' + sizes[i]
}

function formatSpeed(bps) {
  if (bps >= 1024 * 1024) return (bps / (1024 * 1024)).toFixed(1) + ' MB/s'
  if (bps >= 1024) return (bps / 1024).toFixed(0) + ' KB/s'
  return Math.round(bps) + ' B/s'
}

function ensureGalleryController() {
  if (galleryController || !galleryMode) return galleryController
  const root = document.getElementById('gallery-root')
  if (root) {
    galleryController = createGalleryController({
      root,
      sendAssetRequest: requestGalleryAsset,
    })
  }
  return galleryController
}

function requestGalleryAsset(id) {
  if (activeDownload || galleryAssetRequestPending) {
    updateStatus('Download in progress, please wait')
    return
  }
  if (!transferChannel) {
    updateStatus('Connection closed')
    return
  }
  galleryAssetRequestPending = true
  transferChannel.send(JSON.stringify({ type: 'asset_request', id, quality: 'original' }))
}

function initFromURL() {
  const pathMode = detectPathMode()
  sessionCode = pathMode.code
  galleryMode = pathMode.mode === 'gallery'
  if (galleryMode) {
    ensureGalleryController()
  }
  if (sessionCode) {
    document.getElementById('code').value = sessionCode
  }

  if (window.location.hash) {
    sessionPassword = decodeURIComponent(window.location.hash.slice(1))
    history.replaceState(null, '', window.location.pathname)
  }
}

// Make functions available globally for inline handlers (browser only)
if (typeof window !== 'undefined') {
  publishGlobalActions(window, { join, submitPassword, navigateTo })

  window.addEventListener('beforeunload', () => {
    debugLog('window beforeunload', { readyState: ws?.readyState })
  })
  window.addEventListener('pagehide', () => {
    debugLog('window pagehide', { readyState: ws?.readyState })
  })
  document.addEventListener('visibilitychange', () => {
    debugLog('document visibilitychange', { visibilityState: document.visibilityState, readyState: ws?.readyState })
  })

  document.addEventListener('DOMContentLoaded', () => {
    initFromURL()
    if (document.getElementById('code').value) {
      join()
    }
  })
}

// Export test helpers for unit tests
export const __test = {
  detectPathMode,
  setQuotaState({ exceeded, periodEnd }) {
    relayQuotaExceeded = exceeded
    quotaPeriodEnd = periodEnd
  },
  setActiveDownload(download) {
    activeDownload = download
  },
  setCurrentFile(file) {
    currentFile = file
  },
  setTransferSession({ channel, mode }) {
    transferChannel = channel
    currentTransferMode = mode
  },
  setGallerySession({ galleryMode: nextGalleryMode, sessionCode: nextSessionCode, socket }) {
    galleryMode = nextGalleryMode
    sessionCode = nextSessionCode
    ws = socket
  },
  setGalleryController(controller) {
    galleryController = controller
  },
  getTransferSession() {
    return {
      transferChannel,
      currentTransferMode,
    }
  },
  handleTransferMessage,
  submitPassword,
  handleTransferClosure,
  handleError,
  requestGalleryAsset,
  applyFinalDownloadState,
}
