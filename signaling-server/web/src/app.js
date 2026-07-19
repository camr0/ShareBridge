import { createDirectChannelSet, waitForDirectChannelOpen } from './directChannel.js'
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
import { createVideoBufferWarningMonitor } from './videoBufferWarning.js'

// Module state
let pc, ws, dc
let transferChannels = null // ChannelSet with independent control, media, and bulk endpoints.
let currentTransferMode = null // 'direct' or 'relay'
let pendingCandidates = []
let remoteDescSet = false
let relayQuotaExceeded = false
let quotaPeriodEnd = null
let activeDownload = null
let transferClosureHandled = false
let requestSequence = 0
let pendingBulkRequestId = ''
let pendingMediaRequestId = ''
let pendingMediaID = ''
let pendingMediaGeneration = undefined
const pendingEnds = { media: null, bulk: null }
const EARLY_FRAME_MAX_BYTES = 1024 * 1024
const EARLY_FRAME_MAX_COUNT = 128
const EARLY_FRAME_TTL_MS = 5000
const earlyFrames = { media: new Map(), bulk: new Map() }
const retiredOperations = { media: new Set(), bulk: new Set() }
let earlyFrameBytes = 0
let earlyFrameCount = 0
let earlyFrameTimer = null

function nextRequestId(scope) {
  requestSequence += 1
  return `${scope}-${requestSequence}`
}

function controlEndpoint() {
  return transferChannels?.control ?? null
}

function asChannelSet(value) {
  if (!value || value.control) return value
  return {
    control: value,
    media: value,
    bulk: value,
    get readyState() { return value.readyState },
    get onopen() { return value.onopen },
    set onopen(handler) { value.onopen = handler },
    get onclose() { return value.onclose },
    set onclose(handler) { value.onclose = handler },
    close: () => value.close?.(),
  }
}
const directAttemptCancels = new WeakMap()

const DEBUG = typeof location !== 'undefined' && (
  location.search.includes('debug=1') ||
  (typeof localStorage !== 'undefined' && localStorage.getItem('sharebridge_debug'))
)

function debugLog(...args) {
  if (DEBUG) console.log('[secure-relay]', ...args)
}

// Service Workers have a separate console. Route media control messages in
// every build, while surfacing telemetry only in the existing debug console.
if (typeof navigator !== 'undefined' && navigator.serviceWorker) {
  navigator.serviceWorker.addEventListener('message', (event) => {
    if (DEBUG && event.data?.type === 'media_debug') {
      debugLog('media worker', JSON.stringify(event.data))
    }
    if (event.data?.type === 'media_range_request') {
      handleMediaRangeRequest(event.data)
    }
  })
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
let galleryPreviewRequestPending = false
let queuedGalleryPreviewID = ''
let queuedGalleryPreloadIDs = []
let currentPreview = null
let currentVideoPreview = null
// { id, mediaId, generation, totalSize, streamStartOffset, totalBytesReceived, seekTimer, seekListener, seeking, awaitingSeekPlayback, resumePlaybackTimer }

let globalGeneration = 0  // uint32, never reset, unique across all previews
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
  const directSet = createDirectChannelSet()
  let directAttemptActive = true
  let directFailureReported = false
  const reportDirectFailure = (error) => {
    if (!directAttemptActive || directFailureReported) return
    directFailureReported = true
    onDirectFailure(error)
  }
  directAttemptCancels.set(peer, (reason) => {
    if (!directAttemptActive) return
    directAttemptActive = false
    debugLog('cancelling direct attempt', { reason })
    directSet.close()
    peer.close()
  })
  directSet.ready.then((set) => {
    if (directAttemptActive) onDirectChannel(set)
    else set.close()
  }).catch(reportDirectFailure)
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
    debugLog('received RTCDataChannel from direct peer', { label: dc.label })
    try {
      directSet.accept(dc)
    } catch (error) {
      reportDirectFailure(error)
    }
  }

  peer.onconnectionstatechange = () => {
    debugLog('RTCPeerConnection state change', peer.connectionState)
    if (peer.connectionState === 'failed' || peer.connectionState === 'closed') {
      reportDirectFailure(new Error(`PeerConnection ${peer.connectionState}`))
    }
  }

  debugLog('sending initial knock from browser for non-relay_only session')
  ws.send(JSON.stringify({ type: 'knock' }))
  return peer
}

export function cancelDirectTransport(peer, reason = 'direct attempt cancelled') {
  if (!peer) return
  const cancel = directAttemptCancels.get(peer)
  if (cancel) {
    cancel(reason)
    return
  }
  peer.close?.()
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

          transferChannels = asChannelSet(result.channel)
          transferClosureHandled = false
          currentTransferMode = result.mode
          attachTransferChannel({
            channel: transferChannels,
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
  let controlChain = Promise.resolve()
  let mediaChain = Promise.resolve()
  let bulkChain = Promise.resolve()
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
      requestFileList(channel.control, '')
    }
    ws = detachBrowserSignalingSocket(ws)
  }

  channel.onopen = handleOpen
  channel.control.onmessage = (event) => {
    controlChain = controlChain.then(() => handleControlMessage(event)).catch(handleLaneError)
  }
  channel.media.onmessage = (event) => {
    mediaChain = mediaChain.then(() => handleMediaMessage(event)).catch(handleLaneError)
  }
  channel.bulk.onmessage = (event) => {
    bulkChain = bulkChain.then(() => handleBulkMessage(event)).catch(handleLaneError)
  }
  function handleLaneError(err) {
    console.error('[secure-relay] transfer message handler error:', err)
    channel.close?.()
  }
  channel.onclose = () => {
    if (transferChannels && transferChannels !== channel) {
      debugLog('ignoring stale transfer channel close', {
        mode,
        channelReadyState: channel.readyState,
        activeReadyState: transferChannels.readyState,
        activeMode: currentTransferMode,
      })
      return
    }
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
                else if (s === 'falling-back-to-relay') {
                  cancelDirectTransport(pc, 'falling back to relay')
                  updateStatus('Direct failed, using relay...')
                }
                else if (s === 'connected-direct') updateStatus('Connected directly')
                else if (s === 'connected-relay') updateStatus('Connected via relay')
                else if (s === 'failed') {
                  cancelDirectTransport(pc, 'direct connection failed')
                  updateStatus('Connection failed')
                }
              },
            })
            debugLog('relay_policy: connectTransferChannel resolved', { mode: result?.mode, readyState: result?.channel?.readyState })
          } catch (err) {
            debugLog('connectTransferChannel failed', err)
            updateStatus(err.message)
            throw err
          }

          transferChannels = asChannelSet(result.channel)
          transferClosureHandled = false
          currentTransferMode = result.mode
          if (result.mode === 'relay') cancelDirectTransport(pc, 'relay selected')
          attachTransferChannel({
            channel: transferChannels,
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
    if (isTransferChannelActive(transferChannels)) {
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
      transferChannel: transferChannels,
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

async function handleControlMessage(event) {
  if (isBinaryTransferData(event?.data)) throw new Error('control lane received binary data')
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
    case 'thumbnail_complete':
      galleryController?.handleThumbnailComplete(msg)
      updateStatus('')
      break
    case 'file_header':
      if (!acceptHeader(msg, 'bulk')) return
      {
        const startup = startDownload({ ...msg, operationId: msg.operation_id, requestId: msg.request_id })
        await drainEarlyFrames('bulk', msg.operation_id)
        await startup
      }
      break
    case 'asset_preview_header':
      if (!acceptHeader(msg, 'media')) return
      if (msg.mimeType?.startsWith('video/')) {
        startVideoPreview(msg)
      } else {
        startGalleryPreview(msg)
      }
      await drainEarlyFrames('media', msg.operation_id)
      break
    case 'asset_preview_end':
      if (!matchesCurrentMedia(msg)) return
      if (shouldDeferEnd('media', msg)) {
        pendingEnds.media = msg
        return
      }
      if (!currentVideoPreview) retireOperation('media', msg.operation_id)
      else currentVideoPreview.completedBytesSent = BigInt(msg.bytes_sent)
      completeGalleryPreview(msg)
      break
    case 'chunk_end':
      if (!matchesCurrentBulk(msg)) return
      if (shouldDeferEnd('bulk', msg)) {
        pendingEnds.bulk = msg
        return
      }
      retireOperation('bulk', msg.operation_id)
      currentFile.completedBytesSent = BigInt(msg.bytes_sent)
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

async function handleMediaMessage(event) {
  if (!isBinaryTransferData(event?.data)) throw new Error('media lane received text data')
  const frame = decodeBinaryEnvelope(event.data)
  if (frame.type === FRAME_THUMBNAIL) {
    galleryController?.handleThumbnailData(frame.index, frame.payload)
    return
  }
  if (frame.type !== FRAME_FILE_CHUNK) throw new Error('media lane received unsupported frame kind')
  await routeOrBufferFrame('media', frame)
}

async function handleBulkMessage(event) {
  if (!isBinaryTransferData(event?.data)) throw new Error('bulk lane received text data')
  const frame = decodeBinaryEnvelope(event.data)
  if (frame.type !== FRAME_FILE_CHUNK) throw new Error('bulk lane received non-chunk frame')
  if (frame.generation !== 0) throw new Error('bulk chunk generation must be zero')
  await routeOrBufferFrame('bulk', frame)
}

// Compatibility entry point retained for focused tests; production installs the
// three lane-specific handlers above. Correlation, never active-state priority,
// decides which consumer receives a chunk.
async function handleTransferMessage(event) {
  if (!isBinaryTransferData(event?.data)) return handleControlMessage(event)
  const frame = decodeBinaryEnvelope(event.data)
  if (frame.type === FRAME_THUMBNAIL) return handleMediaMessage(event)
  if (frame.operationId === currentFile?.operationId) return handleBulkMessage(event)
  return handleMediaMessage(event)
}

function canonicalOperationId(value) {
  if (typeof value !== 'string' || !/^[1-9][0-9]*$/.test(value)) return null
  try {
    const operation = BigInt(value)
    if (operation > 0xffffffffffffffffn) return null
    return operation.toString(10)
  } catch {
    return null
  }
}

function acceptHeader(msg, scope) {
  const operationId = canonicalOperationId(msg.operation_id)
  if (!operationId || msg.operation_id !== operationId) return false
  const pending = scope === 'bulk' ? pendingBulkRequestId : pendingMediaRequestId
  const current = scope === 'bulk' ? currentFile : (currentVideoPreview || currentPreview)
  if (!pending) return false
  if (msg.request_id !== pending) return false
  if (scope === 'media' && (msg.id !== pendingMediaID ||
    (pendingMediaGeneration !== undefined && msg.generation !== pendingMediaGeneration))) return false
  if (current?.operationId && current.operationId !== operationId) {
    retireOperation(scope, current.operationId)
  }
  if (scope === 'bulk') pendingBulkRequestId = ''
  else {
    pendingMediaRequestId = ''
    pendingMediaID = ''
    pendingMediaGeneration = undefined
  }
  pendingEnds[scope] = null
  return true
}

function retireOperation(scope, operationId) {
  if (!operationId) return
  retiredOperations[scope].add(operationId)
  if (retiredOperations[scope].size > 64) retiredOperations[scope].delete(retiredOperations[scope].values().next().value)
}

function matchesCurrentBulk(msg) {
  return Boolean(currentFile && msg.operation_id === currentFile.operationId &&
    msg.request_id === currentFile.requestId)
}

function matchesCurrentMedia(msg) {
  const current = currentVideoPreview || currentPreview
  return Boolean(current && msg.operation_id === current.operationId &&
    msg.request_id === current.requestId &&
    (msg.generation === undefined || msg.generation === (current.generation ?? 0)))
}

async function routeOrBufferFrame(lane, frame) {
  const current = lane === 'bulk' ? currentFile : (currentVideoPreview || currentPreview)
  if (!current || current.operationId !== frame.operationId) {
    if (retiredOperations[lane].has(frame.operationId)) return
    bufferEarlyFrame(lane, frame)
    return
  }
  if (lane === 'bulk') {
    const nextWireBytes = (currentFile?.wireBytes ?? 0n) + BigInt(frame.payload.byteLength)
    if (currentFile?.completedBytesSent !== undefined && nextWireBytes > currentFile.completedBytesSent) {
      failTransferProtocol('operation received bytes after its declared end')
    }
    await appendChunk(frame.payload)
    if (currentFile?.operationId === frame.operationId) {
      currentFile.wireBytes = nextWireBytes
      await completeDeferredEnd('bulk')
    }
    return
  }
  if (frame.generation !== (current.generation ?? 0)) return
  if (currentVideoPreview) {
    const nextWireBytes = (currentVideoPreview.wireBytes ?? 0n) + BigInt(frame.payload.byteLength)
    if (currentVideoPreview.completedBytesSent !== undefined && nextWireBytes > currentVideoPreview.completedBytesSent) {
      failTransferProtocol('operation received bytes after its declared end')
    }
    navigator.serviceWorker?.controller?.postMessage({
      mediaId: currentVideoPreview.mediaId,
      chunk: frame.payload,
      generation: frame.generation > 0 ? frame.generation : undefined,
    })
    currentVideoPreview.totalBytesReceived += frame.payload.byteLength
    currentVideoPreview.wireBytes = nextWireBytes
    currentVideoPreview.seeking = false
    if (currentVideoPreview.awaitingSeekPlayback) scheduleSeekPlaybackRecovery(currentVideoPreview)
    await completeDeferredEnd('media')
    return
  }
  currentPreview.chunks.push(frame.payload)
  currentPreview.bytes += frame.payload.byteLength
  currentPreview.wireBytes += BigInt(frame.payload.byteLength)
  await completeDeferredEnd('media')
}

function shouldDeferEnd(lane, msg) {
  if (typeof msg.bytes_sent !== 'string' || !/^(0|[1-9][0-9]*)$/.test(msg.bytes_sent)) {
    failTransferProtocol('end bytes_sent must be a nonnegative decimal string')
  }
  const expected = BigInt(msg.bytes_sent)
  if (expected > 0xffffffffffffffffn) failTransferProtocol('end bytes_sent exceeds uint64')
  const current = lane === 'bulk' ? currentFile : (currentVideoPreview || currentPreview)
  const received = lane === 'bulk'
    ? (current?.wireBytes ?? 0n)
    : currentVideoPreview
      ? (currentVideoPreview.wireBytes ?? 0n)
      : (currentPreview?.wireBytes ?? 0n)
  if (received > expected) failTransferProtocol('operation received more bytes than bytes_sent')
  return received < expected
}

function failTransferProtocol(message) {
  transferChannels?.close?.()
  throw new Error(message)
}

async function completeDeferredEnd(lane) {
  const msg = pendingEnds[lane]
  if (!msg || shouldDeferEnd(lane, msg)) return
  pendingEnds[lane] = null
  if (lane === 'bulk') {
    if (!matchesCurrentBulk(msg)) return
    retireOperation('bulk', msg.operation_id)
    currentFile.completedBytesSent = BigInt(msg.bytes_sent)
    await completeDownload()
    return
  }
  if (!matchesCurrentMedia(msg)) return
  if (!currentVideoPreview) retireOperation('media', msg.operation_id)
  else currentVideoPreview.completedBytesSent = BigInt(msg.bytes_sent)
  completeGalleryPreview(msg)
}

function bufferEarlyFrame(lane, frame, now = Date.now()) {
  pruneEarlyFrames(now)
  if (frame.payload.byteLength + earlyFrameBytes > EARLY_FRAME_MAX_BYTES || earlyFrameCount >= EARLY_FRAME_MAX_COUNT) {
    transferChannels?.close?.()
    throw new Error('early frame buffer limit exceeded')
  }
  const entries = earlyFrames[lane].get(frame.operationId) ?? []
  entries.push({ frame, receivedAt: now })
  earlyFrames[lane].set(frame.operationId, entries)
  earlyFrameBytes += frame.payload.byteLength
  earlyFrameCount += 1
  armEarlyFrameTimer()
}

function pruneEarlyFrames(now = Date.now()) {
  let expired = false
  for (const lane of ['media', 'bulk']) {
    for (const [operationId, entries] of earlyFrames[lane]) {
      const kept = entries.filter((entry) => {
        if (now - entry.receivedAt <= EARLY_FRAME_TTL_MS) return true
        earlyFrameBytes -= entry.frame.payload.byteLength
        earlyFrameCount -= 1
        expired = true
        return false
      })
      if (kept.length) earlyFrames[lane].set(operationId, kept)
      else earlyFrames[lane].delete(operationId)
    }
  }
  if (expired) {
    transferChannels?.close?.()
    throw new Error('early frame expired before control header')
  }
}

function armEarlyFrameTimer() {
  if (earlyFrameTimer !== null) return
  earlyFrameTimer = setTimeout(() => {
    earlyFrameTimer = null
    if (earlyFrameCount === 0) return
    clearEarlyFrames()
    transferChannels?.close?.()
  }, EARLY_FRAME_TTL_MS + 1)
  earlyFrameTimer.unref?.()
}

async function drainEarlyFrames(lane, operationId) {
  const entries = earlyFrames[lane].get(operationId) ?? []
  earlyFrames[lane].delete(operationId)
  for (const entry of entries) {
    earlyFrameBytes -= entry.frame.payload.byteLength
    earlyFrameCount -= 1
    await routeOrBufferFrame(lane, entry.frame)
  }
  if (earlyFrameCount === 0 && earlyFrameTimer !== null) {
    clearTimeout(earlyFrameTimer)
    earlyFrameTimer = null
  }
}

function isBinaryTransferData(data) {
  return data instanceof ArrayBuffer ||
    ArrayBuffer.isView(data) ||
    Object.prototype.toString.call(data) === '[object ArrayBuffer]'
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
  const control = controlEndpoint()
  if (!control) {
    updateStatus('Connection closed')
    return
  }
  const fullPath = [...currentPath, name].join('/')
  debugLog('requesting file', {
    name,
    fullPath,
    mode: currentTransferMode,
    channelReadyState: control?.readyState,
  })
  pendingBulkRequestId = nextRequestId('bulk')
  control.send(JSON.stringify({ type: 'file_request', path: fullPath, request_id: pendingBulkRequestId }))
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
  currentFile.wireBytes = 0n
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
        prePipelineChunks.length = 0
      },
    })
    while (prePipelineChunks.length) {
      await activeDownload.append(prePipelineChunks.shift())
    }
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

const prePipelineChunks = []

async function appendChunk(bytes) {
  if (!currentFile) return
  if (!activeDownload) {
    prePipelineChunks.push(bytes)
    return
  }
  while (prePipelineChunks.length) {
    await activeDownload.append(prePipelineChunks.shift())
  }
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
  if (!controlEndpoint()) {
    updateStatus('Connection closed')
    return
  }
  requestFileList(controlEndpoint(), currentPath.join('/'))
}

function openFolder(name) {
  if (activeDownload) {
    updateStatus('Download in progress, please wait')
    return
  }
  if (!controlEndpoint()) {
    updateStatus('Connection closed')
    return
  }
  currentPath.push(name)
  requestFileList(controlEndpoint(), currentPath.join('/'))
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

  if (msg.scope === 'connection') {
    transferChannels?.close?.()
    await handleTransferClosure()
    return
  }

  if (msg.scope === 'bulk') {
    if (!msg.operation_id) {
      if (msg.request_id && msg.request_id !== pendingBulkRequestId) return
      pendingBulkRequestId = ''
      galleryAssetRequestPending = false
      updateStatus(message === 'transfer in progress' ? 'Download in progress, please wait' : 'Error: ' + message)
      return
    }
    if (!matchesCurrentBulk(msg)) return
    retireOperation('bulk', msg.operation_id)
  } else if (msg.scope === 'media') {
    if (!msg.operation_id) {
      if (msg.request_id && msg.request_id !== pendingMediaRequestId) return
      pendingMediaRequestId = ''
      pendingMediaID = ''
      pendingMediaGeneration = undefined
      galleryPreviewRequestPending = false
      updateStatus('Error: ' + message)
      return
    }
    if (!matchesCurrentMedia(msg)) return
    retireOperation('media', msg.operation_id)
    galleryPreviewRequestPending = false
    currentPreview = null
    cleanupCurrentVideoPreview()
    updateStatus('Error: ' + message)
    return
  }

  if (activeDownload?.fail) {
    if (!msg.scope && message === 'transfer in progress') {
      updateStatus('Download in progress, please wait')
      return
    }
    updateStatus('Transfer failed')
    await activeDownload.fail('transfer-error', message || 'Transfer failed')
    if (!msg.scope) clearClosedTransferSession()
    return
  }

  galleryAssetRequestPending = false
  galleryPreviewRequestPending = false
  pendingBulkRequestId = ''
  pendingMediaRequestId = ''
  pendingMediaID = ''
  pendingMediaGeneration = undefined
  queuedGalleryPreviewID = ''
  queuedGalleryPreloadIDs = []
  currentPreview = null
  cleanupCurrentVideoPreview()
  updateStatus('Error: ' + message)
}

async function handleTransferClosure() {
  if (transferClosureHandled) return
  transferClosureHandled = true
  const download = activeDownload
  if (download?.failForDisconnect) {
    await download.failForDisconnect()
    clearClosedTransferSession()
    return
  }

  resetUI()
}

function cleanupCurrentVideoPreview() {
  if (currentVideoPreview) {
    clearTimeout(currentVideoPreview.seekTimer)
    currentVideoPreview.seekTimer = null
    clearSeekPlaybackRecovery(currentVideoPreview)
    currentVideoPreview.bufferWarningMonitor?.destroy()
    currentVideoPreview.hideBufferWarning?.()
    // Remove seeking listener from video element if present.
    if (currentVideoPreview.seekListener) {
      const video = document.querySelector('video.lg-video')
      if (video) {
        video.removeEventListener('seeking', currentVideoPreview.seekListener)
      }
      currentVideoPreview.seekListener = null
    }
    navigator.serviceWorker?.controller?.postMessage({
      mediaId: currentVideoPreview.mediaId,
      chunk: null,
    })
  }
  currentVideoPreview = null
}

const VIDEO_BUFFER_WARNING_CLASS = 'sharebridge-video-buffer-warning'
const VIDEO_BUFFER_WARNING_TEXT = 'This video is buffering on the current connection. A lower-bitrate Immich transcode may improve playback.'

function getVideoBufferWarningContainer(video) {
  return video?.closest?.('.lg-img-wrap') || video?.parentElement || null
}

function showVideoBufferWarning(video) {
  const container = getVideoBufferWarningContainer(video)
  if (!container || container.querySelector?.(`.${VIDEO_BUFFER_WARNING_CLASS}`)) return

  const createElement = globalThis.document?.createElement?.bind(globalThis.document)
  if (!createElement) return
  const warning = createElement('div')
  warning.className = VIDEO_BUFFER_WARNING_CLASS
  warning.setAttribute('role', 'status')
  warning.textContent = VIDEO_BUFFER_WARNING_TEXT
  container.appendChild(warning)
}

function hideVideoBufferWarning(video) {
  const warning = getVideoBufferWarningContainer(video)?.querySelector?.(`.${VIDEO_BUFFER_WARNING_CLASS}`)
  warning?.remove?.()
}

function clearClosedTransferSession() {
  galleryAssetRequestPending = false
  galleryPreviewRequestPending = false
  queuedGalleryPreviewID = ''
  queuedGalleryPreloadIDs = []
  currentPreview = null
  clearEarlyFrames()
  cleanupCurrentVideoPreview()
  transferChannels = null
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
  cleanupCurrentVideoPreview()
  activeDownload = null
  currentFile = null
  galleryAssetRequestPending = false
  galleryPreviewRequestPending = false
  queuedGalleryPreviewID = ''
  queuedGalleryPreloadIDs = []
  currentPreview = null
  clearEarlyFrames()
  receivedBytes = 0
  receivedChunkCount = 0
  transferStartTime = 0
  pendingNonce = null
  currentPath = []
  sessionPassword = ''
  galleryController?.destroy()
  galleryController = null
  transferChannels = null
  currentTransferMode = null
  ws = null
  pc = null
  dc = null
  pendingCandidates = []
  remoteDescSet = false
}

function clearEarlyFrames() {
  if (earlyFrameTimer !== null) clearTimeout(earlyFrameTimer)
  earlyFrameTimer = null
  earlyFrames.media.clear()
  earlyFrames.bulk.clear()
  retiredOperations.media.clear()
  retiredOperations.bulk.clear()
  earlyFrameBytes = 0
  earlyFrameCount = 0
  pendingBulkRequestId = ''
  pendingMediaRequestId = ''
  pendingMediaID = ''
  pendingMediaGeneration = undefined
  pendingEnds.media = null
  pendingEnds.bulk = null
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
      onPreviewRequest: requestGalleryPreview,
      onPreviewClose: handleGalleryPreviewClose,
      onDownloadRequest: requestGalleryAsset,
    })
  }
  return galleryController
}

function requestGalleryPreview(id, { priority = 'active', mimeType = '' } = {}) {
  if (galleryPreviewRequestPending) {
    if (priority === 'active') {
      queuedGalleryPreviewID = id
      return
    }
    queueGalleryPreload(id)
    return
  }
  const control = controlEndpoint()
  if (!control) {
    updateStatus('Connection closed')
    return
  }

  // Keep one interactive media operation active at a time. Preloads wait while
  // a video session is active; an explicit user selection replaces it.
  if (currentVideoPreview && currentVideoPreview.id !== id) {
    if (priority !== 'active') {
      queueGalleryPreload(id)
      return
    }
    cleanupCurrentVideoPreview()
  }

  galleryPreviewRequestPending = true
  const quality = mimeType?.startsWith('video/') ? 'video' : 'preview'

  // For video, pre-create currentVideoPreview with a unique generation
  // BEFORE sending the request, so it's ready when the header arrives.
  // Initial load uses generation=0 (no binary frame generation encoding)
  // to avoid a race between SW postMessage and the initial fetch event.
  // Seeks increment globalGeneration and use generation-encoded frames.
  if (quality === 'video') {
    currentVideoPreview = {
      id,
      mediaId: id,
      generation: globalGeneration,  // 0 for initial load; seek handlers increment
      totalSize: 0,
      streamStartOffset: 0,
      totalBytesReceived: 0,
      seekTimer: null,
      seekListener: null,
      seeking: false,
      seekSentGeneration: null,
      awaitingSeekPlayback: false,
      resumePlaybackTimer: null,
      freshSession: true,
    }
  }

  pendingMediaRequestId = nextRequestId('media')
  pendingMediaID = id
  pendingMediaGeneration = quality === 'video' ? currentVideoPreview.generation : undefined
  control.send(JSON.stringify({
    type: 'asset_preview_request', id, quality,
    generation: quality === 'video' ? currentVideoPreview.generation : undefined,
    request_id: pendingMediaRequestId,
  }))
}

function queueGalleryPreload(id) {
  if (!id || id === currentPreview?.id || id === queuedGalleryPreviewID || queuedGalleryPreloadIDs.includes(id)) return
  queuedGalleryPreloadIDs.push(id)
  if (queuedGalleryPreloadIDs.length > 2) {
    queuedGalleryPreloadIDs = queuedGalleryPreloadIDs.slice(-2)
  }
}

function requestGalleryAsset(id) {
  if (activeDownload || galleryAssetRequestPending) {
    updateStatus('Download in progress, please wait')
    return
  }
  const control = controlEndpoint()
  if (!control) {
    updateStatus('Connection closed')
    return
  }
  galleryAssetRequestPending = true
  pendingBulkRequestId = nextRequestId('bulk')
  control.send(JSON.stringify({ type: 'asset_request', id, quality: 'original', request_id: pendingBulkRequestId }))
}

function startGalleryPreview(header) {
  currentPreview = {
    id: header.id,
    operationId: header.operation_id,
    requestId: header.request_id,
    generation: header.generation ?? 0,
    mimeType: header.mimeType || 'image/jpeg',
    chunks: [],
    bytes: 0,
    wireBytes: 0n,
  }
}

function startVideoPreview(header) {
  const vp = currentVideoPreview
  if (!vp) return

  // Check if this header is for the current generation.
  if (header.generation !== undefined && header.generation !== vp.generation) return

  vp.operationId = header.operation_id
  vp.requestId = header.request_id

  // Store size/offset from header for seek calculations.
  if (header.size > 0) {
    vp.totalSize = header.size
  }
  vp.streamStartOffset = header.byte_offset || 0
  vp.totalBytesReceived = 0
  vp.wireBytes = 0n
  vp.completedBytesSent = undefined
  const freshSession = vp.freshSession ?? vp.generation === 0

  navigator.serviceWorker?.controller?.postMessage({
    mediaId: vp.mediaId,
    startOffset: vp.streamStartOffset,
    generation: vp.generation,
    ...(freshSession ? { freshPreview: true } : {}),
    ...(DEBUG ? { debug: true } : {}),
  })

  // Forward total size for Content-Length / Accept-Ranges.
  if (header.size > 0) {
    navigator.serviceWorker?.controller?.postMessage({
      mediaId: vp.mediaId,
      size: header.size,
      ...(DEBUG ? { debug: true } : {}),
    })
  }

  // Later seek headers only rebase the existing Service Worker stream. Calling
  // the gallery controller again would invoke video.load() on the active
  // element and reset playback to 0.
  if (!freshSession) return
  vp.freshSession = false

  // Defer creating the <video> element (which triggers the fetch) by
  // 100ms.  This gives the SW's message handler time to process the
  // size / reset postMessages before Chrome issues its initial Range
  // probe, so the response carries Content-Length + Accept-Ranges and
  // Chrome recognizes the resource as seekable.
  setTimeout(() => {
    galleryController?.handlePreviewData?.(header.id, new Uint8Array(0), 'video/mp4')

    // Start a new range as soon as a seek begins. Waiting for `seeked` creates
    // a deadlock when the target is outside the playable buffer because that
    // event cannot fire until the missing range arrives.
    const video = document.querySelector('video.lg-video')
    if (video && !vp.seekListener) {
      const onSeeking = createSeekHandler(vp)
      vp.seekListener = onSeeking
      video.addEventListener('seeking', onSeeking)
      vp.hideBufferWarning = () => hideVideoBufferWarning(video)
      vp.bufferWarningMonitor = createVideoBufferWarningMonitor({
        video,
        showWarning: () => showVideoBufferWarning(video),
        hideWarning: vp.hideBufferWarning,
      })
    }
  }, 100)
}

function handleGalleryPreviewClose(id) {
  if (!currentVideoPreview || currentVideoPreview.id !== id) return
  globalGeneration = Math.max(globalGeneration, currentVideoPreview.generation || 0) + 1
  cleanupCurrentVideoPreview()
  galleryPreviewRequestPending = false
}

function createSeekHandler(vp, { setTimeoutFn = setTimeout, clearTimeoutFn = clearTimeout } = {}) {
  return () => {
    const video = document.querySelector('video.lg-video')
    if (!video || !video.duration || !vp.totalSize) return  // metadata not loaded

    // Chrome can satisfy seeks that remain inside its decoded buffer without
    // replacing the response stream. Resetting that stream would turn a local
    // seek into a stall, so only ask the agent for genuinely missing media.
    for (let i = 0; i < (video.buffered?.length || 0); i++) {
      if (video.currentTime >= video.buffered.start(i) && video.currentTime < video.buffered.end(i)) {
        return
      }
    }

    const byteOffset = Math.floor(video.currentTime / video.duration * vp.totalSize)

    // Immediately tell the SW a new seek generation started
    // so it can intercept Chrome's Range: bytes=0- init-segment request
    // that fires synchronously when the video seeks.  The agent round-
    // trip is debounced below.
    clearSeekPlaybackRecovery(vp, clearTimeoutFn)
    vp.generation = ++globalGeneration
    vp.seekSentGeneration = null
    vp.totalBytesReceived = 0
    vp.streamStartOffset = 0
    vp.awaitingSeekPlayback = true
    navigator.serviceWorker?.controller?.postMessage({
      mediaId: vp.mediaId,
      reset: true,
      generation: vp.generation,
      ...(DEBUG ? { debug: true } : {}),
    })

    // Debounce: clear any pending seek, schedule a new one.
    if (vp.seekTimer) clearTimeoutFn(vp.seekTimer)
    const generation = vp.generation
    vp.clearSeekTimer = () => {
      if (!vp.seekTimer) return
      clearTimeoutFn(vp.seekTimer)
      vp.seekTimer = null
    }
    vp.seekTimer = setTimeoutFn(() => {
      vp.seekTimer = null
      sendVideoSeek(vp, byteOffset, generation)
    }, 300)
  }
}

function sendVideoSeek(vp, startOffset, generation = vp?.generation) {
  if (!vp || generation !== vp.generation) return false
  if (!Number.isInteger(startOffset) || startOffset < 0) return false
  const control = controlEndpoint()
  if (!control || vp.seekSentGeneration === generation || !vp.operationId) return false

  vp.seekSentGeneration = generation
  vp.seeking = true
  pendingMediaRequestId = nextRequestId('media')
  pendingMediaID = vp.id
  pendingMediaGeneration = generation
  control.send(JSON.stringify({
    type: 'asset_preview_seek',
    id: vp.id,
    quality: 'video',
    start_offset: startOffset,
    generation,
    request_id: pendingMediaRequestId,
    operation_id: vp.operationId,
  }))
  return true
}

function handleMediaRangeRequest(message) {
  const vp = currentVideoPreview
  if (!vp || message?.mediaId !== vp.mediaId || message?.generation !== vp.generation) return false
  if (!Number.isInteger(message.startOffset) || message.startOffset <= 0) return false

  vp.clearSeekTimer?.()
  return sendVideoSeek(vp, message.startOffset, message.generation)
}

function clearSeekPlaybackRecovery(vp, clearTimeoutFn = clearTimeout) {
  if (!vp?.resumePlaybackTimer) return
  clearTimeoutFn(vp.resumePlaybackTimer)
  vp.resumePlaybackTimer = null
}

function scheduleSeekPlaybackRecovery(
  vp,
  {
    delayMs = 3000,
    queryVideo = () => document.querySelector('video.lg-video'),
    isCurrentPreview = () => currentVideoPreview === vp,
    setTimeoutFn = setTimeout,
    clearTimeoutFn = clearTimeout,
  } = {},
) {
  if (!vp?.awaitingSeekPlayback || vp.resumePlaybackTimer) return

  const generation = vp.generation
  vp.resumePlaybackTimer = setTimeoutFn(() => {
    vp.resumePlaybackTimer = null
    if (!isCurrentPreview() || vp.generation !== generation || !vp.awaitingSeekPlayback) return

    const video = queryVideo()
    if (!video) return

    vp.awaitingSeekPlayback = false
    if (!video.paused) return
    const playResult = typeof video.play === 'function' ? video.play() : null
    Promise.resolve(playResult).catch(() => {})
  }, delayMs)
}

function completeGalleryPreview(msg) {
  if (currentVideoPreview && currentVideoPreview.id === msg.id) {
    // Ignore stale end messages from old generations.
    if (msg.generation !== undefined && msg.generation !== currentVideoPreview.generation) return

    const vp = currentVideoPreview
    galleryPreviewRequestPending = false
    debugLog('video preview complete', JSON.stringify({
      id: vp.id,
      generation: vp.generation,
      totalBytesReceived: vp.totalBytesReceived,
      expectedBytes: vp.totalSize,
    }))
    // This ends one byte-range, not the video preview session. Retain the
    // routing state for the next seek range.
    navigator.serviceWorker?.controller?.postMessage({
      mediaId: vp.mediaId,
      chunk: null,
    })
    return
  }

  if (!currentPreview) return
  const preview = currentPreview
  currentPreview = null
  galleryPreviewRequestPending = false
  const merged = new Uint8Array(preview.bytes)
  let offset = 0
  for (const chunk of preview.chunks) {
    merged.set(chunk, offset)
    offset += chunk.byteLength
  }
  galleryController?.handlePreviewData?.(msg.id || preview.id, merged, preview.mimeType)
  const nextID = queuedGalleryPreviewID || queuedGalleryPreloadIDs.shift()
  queuedGalleryPreviewID = ''
  if (nextID) requestGalleryPreview(nextID)
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
  setTransferSession({ channels, channel, mode }) {
    const value = channels ?? channel
    transferChannels = asChannelSet(value)
    transferClosureHandled = false
    currentTransferMode = mode
    if (!value) {
      galleryAssetRequestPending = false
      galleryPreviewRequestPending = false
      clearEarlyFrames()
    }
  },
  setGallerySession({ galleryMode: nextGalleryMode, sessionCode: nextSessionCode, socket }) {
    galleryMode = nextGalleryMode
    sessionCode = nextSessionCode
    ws = socket
  },
  setGalleryController(controller) {
    galleryController = controller
  },
  setCurrentVideoPreview(preview) {
    currentVideoPreview = preview
  },
  getCurrentFile() {
    return currentFile
  },
  getCurrentVideoPreview() {
    return currentVideoPreview
  },
  getTransferSession() {
    return {
      transferChannel: transferChannels?.control ?? null,
      transferChannels,
      currentTransferMode,
    }
  },
  handleTransferMessage,
  handleControlMessage,
  handleMediaMessage,
  handleBulkMessage,
  requestFile,
  submitPassword,
  handleTransferClosure,
  handleError,
  requestGalleryPreview,
  requestGalleryAsset,
  startVideoPreview,
  handleGalleryPreviewClose,
  cleanupCurrentVideoPreview,
  applyFinalDownloadState,
  createSeekHandler,
  handleMediaRangeRequest,
  sendVideoSeek,
  scheduleSeekPlaybackRecovery,
}
