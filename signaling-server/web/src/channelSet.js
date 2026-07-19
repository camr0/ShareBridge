import { PROTOCOL_VERSION } from './multiLaneProtocol.js'

const REQUIRED_LABELS = Object.freeze(['control', 'media', 'bulk'])
const DEFAULT_HANDSHAKE_TIMEOUT_MS = 10_000

export function createChannelSet({ handshakeTimeoutMs = DEFAULT_HANDSHAKE_TIMEOUT_MS } = {}) {
  if (!Number.isFinite(handshakeTimeoutMs) || handshakeTimeoutMs < 0) {
    throw new RangeError('handshakeTimeoutMs must be a non-negative finite number')
  }
  return new BrowserChannelSet(handshakeTimeoutMs)
}

class BrowserChannelSet {
  constructor(handshakeTimeoutMs) {
    this.readyState = 'connecting'
    this.onclose = null
    this._channels = new Map()
    this._settled = false
    this._closed = false
    this.ready = new Promise((resolve, reject) => {
      this._resolveReady = resolve
      this._rejectReady = reject
    })
    this._handshakeTimeoutMs = handshakeTimeoutMs
    this._timer = null
  }

  get control() { return this._channels.get('control') ?? null }
  get media() { return this._channels.get('media') ?? null }
  get bulk() { return this._channels.get('bulk') ?? null }

  attach(label, channel) {
    if (!REQUIRED_LABELS.includes(label)) {
      const error = new Error(`unknown channel label: ${label}`)
      channel?.close?.()
      this._fail(error)
      throw error
    }
    if (this._channels.has(label)) {
      const error = new Error(`duplicate channel label: ${label}`)
      channel?.close?.()
      this._fail(error)
      throw error
    }
    if (this._closed) {
      channel?.close?.()
      throw new Error('channel set is closed')
    }
    if (!channel || typeof channel.send !== 'function' || typeof channel.close !== 'function') {
      const error = new TypeError(`invalid channel for ${label}`)
      this._fail(error)
      throw error
    }

    this._armTimeout()

    this._channels.set(label, channel)
    channel.onopen = () => this._maybeBeginHandshake()
    channel.onclose = () => this._fail(new Error(`required channel closed: ${label}`))

    if (channel.readyState === 'closed' || channel.readyState === 'closing') {
      this._fail(new Error(`required channel is not openable: ${label}`))
    } else {
      this._maybeBeginHandshake()
    }
    return channel
  }

  close() {
    this._fail(new Error('channel set closed'))
  }

  fail(error) {
    this._fail(error instanceof Error ? error : new Error(String(error)))
  }

  _armTimeout() {
    if (this._timer !== null || this._closed || this._settled) return
    this._timer = setTimeout(() => this._onTimeout(), this._handshakeTimeoutMs)
  }

  _maybeBeginHandshake() {
    if (this._closed || this.readyState !== 'connecting') return
    if (!REQUIRED_LABELS.every((label) => this._channels.get(label)?.readyState === 'open')) return

    this.readyState = 'handshaking'
    const control = this.control
    control.onmessage = (event) => this._handleHandshakeMessage(event?.data)
    try {
      control.send(JSON.stringify({ type: 'transport_hello', version: PROTOCOL_VERSION }))
    } catch (error) {
      this._fail(error instanceof Error ? error : new Error(String(error)))
    }
  }

  _handleHandshakeMessage(data) {
    if (this._closed || this._settled || this.readyState !== 'handshaking') return
    let message
    try {
      message = JSON.parse(data)
    } catch {
      this._fail(new Error('expected transport_ready handshake message'))
      return
    }
    if (message?.type === 'error' && message.scope === 'connection') {
      const detail = typeof message.message === 'string' && message.message.length > 0
        ? message.message
        : 'transport handshake rejected'
      this._fail(new Error(detail))
      return
    }
    if (message?.type !== 'transport_ready') {
      this._fail(new Error('expected transport_ready handshake message'))
      return
    }
    if (message.version !== PROTOCOL_VERSION) {
      this._fail(new Error(`incompatible transport version: ${message.version}`))
      return
    }

    clearTimeout(this._timer)
    this.control.onmessage = null
    this.readyState = 'open'
    this._settled = true
    this._resolveReady(this)
  }

  _onTimeout() {
    const missing = REQUIRED_LABELS.filter((label) => !this._channels.has(label))
    const waiting = REQUIRED_LABELS.filter((label) => {
      const channel = this._channels.get(label)
      return channel && channel.readyState !== 'open'
    })
    const detail = missing.length > 0
      ? `missing required lanes: ${missing.join(', ')}`
      : waiting.length > 0
        ? `lanes not open: ${waiting.join(', ')}`
        : 'transport version acknowledgement not received'
    this._fail(new Error(`channel set timed out: ${detail}`))
  }

  _fail(error) {
    if (this._closed) return
    this._closed = true
    clearTimeout(this._timer)
    this.readyState = 'closed'
    for (const channel of this._channels.values()) {
      try {
        channel.close()
      } catch {
        // Closing the other required lanes remains best-effort.
      }
    }
    if (!this._settled) {
      this._settled = true
      this._rejectReady(error)
    }
    this.onclose?.(error)
  }
}
