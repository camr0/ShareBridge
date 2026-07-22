// signaling-server/web/src/connectTransferChannel.js
const DEFAULT_DIRECT_TIMEOUT_MS = 10000
const DEBUG = typeof location !== 'undefined' && (
  location.search.includes('debug=1') ||
  (typeof localStorage !== 'undefined' && localStorage.getItem('sharebridge_debug'))
)

function debugLog(...args) {
  if (DEBUG) console.log('[secure-relay]', ...args)
}

export async function connectTransferChannel({
  relayPolicy,
  directConnect,
  relayConnect,
  directTimeoutMs = DEFAULT_DIRECT_TIMEOUT_MS,
  onStatusChange = () => {},
}) {
  debugLog('connectTransferChannel: start', {
    relayOnly: relayPolicy.relayOnly,
    relayAllowed: relayPolicy.relayAllowed,
    directTimeoutMs,
  })
  if (relayPolicy.relayOnly) {
    if (!relayPolicy.relayAllowed) {
      throw new Error('Relay is required for this share but relay service is not available')
    }
    debugLog('connectTransferChannel: relay_only path')
    onStatusChange('connecting-relay')
    const relayChannel = await relayConnect()
    debugLog('connectTransferChannel: relay connected', { readyState: relayChannel?.readyState })
    onStatusChange('connected-relay')
    return { channel: relayChannel, mode: 'relay' }
  }

  debugLog('connectTransferChannel: attempting direct path')
  onStatusChange('connecting-direct')

  try {
    const directChannel = await withTimeout(directConnect(), directTimeoutMs)
    debugLog('connectTransferChannel: direct connected', { readyState: directChannel?.readyState })
    onStatusChange('connected-direct')
    return { channel: directChannel, mode: 'direct' }
  } catch (err) {
    debugLog('connectTransferChannel: direct failed', err)
    if (!relayPolicy.relayAllowed) {
      onStatusChange('failed')
      throw err
    }
    debugLog('connectTransferChannel: falling back to relay')
    onStatusChange('falling-back-to-relay')
    const relayChannel = await relayConnect()
    debugLog('connectTransferChannel: relay connected after fallback', { readyState: relayChannel?.readyState })
    onStatusChange('connected-relay')
    return { channel: relayChannel, mode: 'relay' }
  }
}

export function buildDirectIceServers(iceServers) {
  return iceServers
    .map((entry) => {
      const urls = Array.isArray(entry.urls) ? entry.urls : [entry.urls]
      const stunOnly = urls.filter((url) => typeof url === 'string' && url.startsWith('stun:'))
      return stunOnly.length > 0 ? { urls: stunOnly } : null
    })
    .filter(Boolean)
}

export function decodeRelayPolicyToken(token) {
  // Handle empty token (relay not configured) - relay not allowed
  if (!token) {
    return {
      expectedStaticPubHex: '',
      relayAllowed: false,
      relayOnly: false,
    }
  }
  const [, payload] = token.split('.')
  const json = JSON.parse(new TextDecoder().decode(base64UrlToBytes(payload)))
  return {
    expectedStaticPubHex: json.expected_agent_static_pub,
    relayAllowed: Boolean(json.relay_allowed),
    relayOnly: Boolean(json.relay_only),
  }
}

function withTimeout(promise, timeoutMs) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error(`direct connect timeout after ${timeoutMs}ms`)), timeoutMs)
    promise.then(
      (value) => {
        clearTimeout(timer)
        resolve(value)
      },
      (err) => {
        clearTimeout(timer)
        reject(err)
      },
    )
  })
}

function base64UrlToBytes(base64Url) {
  const base64 = base64Url.replace(/-/g, '+').replace(/_/g, '/')
  const padded = base64 + '='.repeat((4 - (base64.length % 4)) % 4)
  const binary = atob(padded)
  return Uint8Array.from(binary, (char) => char.charCodeAt(0))
}
