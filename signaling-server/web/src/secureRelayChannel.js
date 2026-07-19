import { NoiseXX } from '../noise-p256/index.js'
import { FRAME_HANDSHAKE, FRAME_TEXT, FRAME_BINARY, MAX_FRAME_PAYLOAD, FrameDecoder, writeFrame } from './frame.js'
import { createChannelSet } from './channelSet.js'
import { LaneScheduler } from './laneScheduler.js'
import {
  LANE_BULK,
  LANE_CONTROL,
  LANE_MEDIA,
  TRAFFIC_CLASS_BULK,
  TRAFFIC_CLASS_CONTROL,
  TRAFFIC_CLASS_INTERACTIVE_MEDIA,
  TRAFFIC_CLASS_THUMBNAIL,
  decodeLaneEnvelope,
  encodeLaneEnvelope,
} from './multiLaneProtocol.js'

const DEBUG = typeof location !== 'undefined' && (
  location.search.includes('debug=1') ||
  (typeof localStorage !== 'undefined' && localStorage.getItem('sharebridge_debug'))
)

const NOISE_AEAD_OVERHEAD_BYTES = 16
export const MAX_RELAY_PAYLOAD_BYTES = MAX_FRAME_PAYLOAD - 1 - NOISE_AEAD_OVERHEAD_BYTES

function debugLog(...args) {
  if (DEBUG) console.log('[secure-relay]', ...args)
}

export class SecureRelayChannel {
  constructor({ relayURL, relayToken, expectedStaticPub, websocketFactory = (url) => new WebSocket(url) }) {
    this._relayURL = relayURL
    this._relayToken = relayToken
    this._expectedStaticPub = expectedStaticPub
    this._websocketFactory = websocketFactory
    this._socket = null
    this._decoder = new FrameDecoder()
    this._noise = null
    this._sendCipher = null
    this._recvCipher = null
    this._scheduler = new LaneScheduler({ write: (request) => this._writeScheduled(request) })
    this.control = new RelayLaneEndpoint(this, LANE_CONTROL, TRAFFIC_CLASS_CONTROL)
    this.media = new RelayLaneEndpoint(this, LANE_MEDIA, TRAFFIC_CLASS_INTERACTIVE_MEDIA)
    this.bulk = new RelayLaneEndpoint(this, LANE_BULK, TRAFFIC_CLASS_BULK)
    this._endpoints = new Map([
      [LANE_CONTROL, this.control],
      [LANE_MEDIA, this.media],
      [LANE_BULK, this.bulk],
    ])
    this._channelSet = null
    this.ready = null
    this._closed = false
    this._receiveChain = Promise.resolve()
    this.readyState = 'connecting'
    this.bufferedAmount = 0
    this.onopen = null
    this.onmessage = null
    this.onclose = null
  }

  async start() {
    try {
      return await this._start()
    } catch (error) {
      this._closeAll(error?.message ?? 'relay start failed')
      throw error
    }
  }

  async _start() {
    debugLog('SecureRelayChannel.start() called')
    this._noise = await NoiseXX.createInitiator()
    this._channelSet = createChannelSet()
    this._channelSet.onclose = (error) => this._closeAll(error?.message ?? 'channel set closed')
    this._channelSet.attach('control', this.control)
    this._channelSet.attach('media', this.media)
    this._channelSet.attach('bulk', this.bulk)
    this.ready = this._channelSet.ready
    // start() reports handshake failures; keep the shared ready promise from
    // becoming a second unhandled rejection when Noise fails first.
    this.ready.catch(() => {})
    debugLog('NoiseXX initiator created')
    this._socket = this._websocketFactory(this._relayURL)
    this._socket.binaryType = 'arraybuffer'
    debugLog('WebSocket created, url:', this._relayURL, 'readyState:', this._socket.readyState)
    this._socket.addEventListener('error', (event) => {
      debugLog('relay transport socket error', {
        readyState: this.readyState,
        socketReadyState: this._socket?.readyState,
        eventType: event.type,
      })
    })
    this._socket.addEventListener('close', (event) => {
      debugLog('relay transport socket close', {
        code: event?.code,
        reason: event?.reason,
        wasClean: event?.wasClean,
        readyState: this.readyState,
        socketReadyState: this._socket?.readyState,
      })
      this._closeAll(event?.reason || 'relay websocket closed')
    })

    await new Promise((resolve, reject) => {
      const handleOpen = async () => {
        debugLog('WebSocket open event fired')
        this._socket.removeEventListener('close', handleClose)
        try {
          this._socket.send(new TextEncoder().encode(JSON.stringify({ token: this._relayToken })))
          debugLog('Hello token sent')
          this._socket.addEventListener('message', this._handleMessage)
          const msg1 = await this._noise.writeMessage1()
          debugLog('Noise msg1 created, sending...')
          this._socket.send(writeFrame(FRAME_HANDSHAKE, msg1))
          debugLog('msg1 sent, waiting for msg2...')
          resolve()
        } catch (err) {
          console.error('[secure-relay] Error in handleOpen:', err)
          reject(err)
        }
      }
      const handleClose = () => {
        debugLog('WebSocket closed in first phase, readyState:', this.readyState)
        this.readyState = 'closed'
        reject(new Error('websocket closed during handshake'))
      }
      this._socket.addEventListener('open', handleOpen, { once: true })
      this._socket.addEventListener('error', (e) => {
        console.error('[secure-relay] WebSocket error in first phase:', e)
        reject(e)
      }, { once: true })
      this._socket.addEventListener('close', handleClose, { once: true })
      if (this._socket.readyState === 1) {
        debugLog('WebSocket already open, calling handleOpen immediately')
        handleOpen()
      }
    })

    await new Promise((resolve, reject) => {
      this._handshakeResolve = resolve
      this._handshakeReject = reject
      debugLog('Waiting for handshake completion (msg2)...')
      // If socket closes during handshake, reject the handshake promise
      const handleClose = () => {
        // Only reject if handshake hasn't completed and we haven't already rejected
        if (this.readyState !== 'open' && !this._handshakeError) {
          debugLog('WebSocket closed during handshake phase 2')
          this.readyState = 'closed'
          reject(new Error('websocket closed during handshake'))
        }
      }
      this._socket.addEventListener('close', handleClose, { once: true })
      // Remove this handler once handshake succeeds
      this._handshakeCleanup = () => {
        this._socket.removeEventListener('close', handleClose)
      }
    })
    debugLog('Handshake promise resolved, calling cleanup')
    this._handshakeCleanup?.()
    await this.ready
    this.readyState = 'open'
    this.onopen?.()
    debugLog('SecureRelayChannel.start() completed')
  }

  _handleMessage = (event) => {
    this._receiveChain = this._receiveChain
      .then(() => this._processMessage(event))
      .catch((error) => {
        console.error('[secure-relay] receive sequence error:', error)
        this._closeAll('relay receive sequence error')
      })
  }

  _processMessage = async (event) => {
    debugLog('WebSocket message received, data length:', event.data?.byteLength || event.data?.length || 'unknown')
    const chunk = new Uint8Array(event.data)
    debugLog('Processing chunk, length:', chunk.length)
    for (const frame of this._decoder.push(chunk)) {
      debugLog('Frame decoded, kind:', frame.kind, 'payload length:', frame.payload?.length)
      if (frame.kind === FRAME_HANDSHAKE) {
        debugLog('Handling handshake frame')
        try {
          await this._handleHandshakeFrame(frame.payload)
        } catch (err) {
          this._handshakeError = err
          console.error('[secure-relay] Handshake frame error:', err)
          this.close('handshake frame error')
          this._handshakeReject?.(err)
        }
        continue
      }

      try {
        if (frame.kind !== FRAME_TEXT && frame.kind !== FRAME_BINARY) {
          throw new Error(`unknown frame kind: 0x${frame.kind.toString(16)}`)
        }
        const plaintext = await this._recvCipher.decrypt(new Uint8Array(0), frame.payload)
        const { lane, payload } = decodeLaneEnvelope(plaintext)
        const endpoint = this._endpoints.get(lane)
        if (!endpoint) throw new Error(`unknown relay lane: ${lane}`)
        if (frame.kind === FRAME_TEXT) {
          debugLog('relay text frame decrypted', { byteLength: plaintext.length })
          endpoint._deliver(new TextDecoder().decode(payload))
        } else {
          debugLog('relay binary frame decrypted', { byteLength: plaintext.length })
          const arrayBuffer = new ArrayBuffer(payload.length)
          new Uint8Array(arrayBuffer).set(payload)
          endpoint._deliver(arrayBuffer)
        }
        if (lane === LANE_CONTROL && this.readyState === 'open') {
          this.onmessage?.({ data: frame.kind === FRAME_TEXT ? new TextDecoder().decode(payload) : payload.buffer })
        }
      } catch (err) {
        console.error('[secure-relay] decrypt/forward error:', err.message, err.stack)
        this.close('decrypt/forward error')
      }
    }
  }

  async _handleHandshakeFrame(msg2) {
    debugLog('Processing msg2 from responder')
    await this._noise.readMessage2(msg2)
    if (!equalBytes(this._noise.remoteStaticPub, this._expectedStaticPub)) {
      throw new Error('unexpected agent static key')
    }
    debugLog('msg2 validated, sending msg3')
    const msg3 = await this._noise.writeMessage3()
    this._socket.send(writeFrame(FRAME_HANDSHAKE, msg3))
    ;[this._sendCipher, this._recvCipher] = await this._noise.split()
    for (const endpoint of this._endpoints.values()) endpoint._open()
    debugLog('Noise handshake complete; waiting for transport version acknowledgement')
    this._handshakeResolve?.()
  }

  // Compatibility shim while application callers migrate to the control endpoint.
  async send(text) {
    return this.control.send(text)
  }

  async sendBinary(bytes) {
    return this.control.send(bytes)
  }

  close(reason = 'local close') {
    debugLog('SecureRelayChannel.close()', {
      reason,
      readyState: this.readyState,
      socketReadyState: this._socket?.readyState,
    })
    this._closeAll(reason)
  }

  async _writeScheduled({ className, kind, payload }) {
    try {
      const lane = laneForClass(className)
      const plaintext = encodeLaneEnvelope(lane, payload)
      const ciphertext = await this._sendCipher.encrypt(new Uint8Array(0), plaintext)
      this._socket.send(writeFrame(kind, ciphertext))
    } catch (error) {
      this._closeAll('relay write failed')
      throw error
    }
  }

  _closeAll(reason) {
    if (this._closed) return
    this._closed = true
    this.readyState = 'closed'
    void this._scheduler.close()
    for (const endpoint of this._endpoints.values()) endpoint._closeFromOwner()
    this._socket?.close()
    this.onclose?.(reason instanceof Error ? reason : new Error(String(reason)))
  }
}

class RelayLaneEndpoint {
  constructor(owner, lane, className) {
    this._owner = owner
    this.lane = lane
    this.className = className
    this.readyState = 'connecting'
    this.bufferedAmount = 0
    this.onopen = null
    this.onmessage = null
    this.onclose = null
  }

  send(value) {
    const isText = typeof value === 'string'
    const payload = isText ? new TextEncoder().encode(value) : toUint8Array(value)
    if (payload.byteLength > MAX_RELAY_PAYLOAD_BYTES) {
      throw new RangeError(`relay payload is ${payload.byteLength} bytes; maximum is ${MAX_RELAY_PAYLOAD_BYTES}`)
    }
    const completion = this._owner._scheduler.send({
      className: this.className,
      kind: isText ? FRAME_TEXT : FRAME_BINARY,
      payload,
    })
    completion.catch(() => this._owner._closeAll('relay lane write failed'))
    return completion
  }

  sendThumbnail(value) {
    return this.sendBinaryClass(TRAFFIC_CLASS_THUMBNAIL, value)
  }

  sendBinaryClass(className, value) {
    if (laneForClass(className) !== this.lane) {
      throw new RangeError(`traffic class ${className} does not map to relay lane ${this.lane}`)
    }
    const payload = toUint8Array(value)
    if (payload.byteLength > MAX_RELAY_PAYLOAD_BYTES) {
      throw new RangeError(`relay payload is ${payload.byteLength} bytes; maximum is ${MAX_RELAY_PAYLOAD_BYTES}`)
    }
    const completion = this._owner._scheduler.send({
      className,
      kind: FRAME_BINARY,
      payload,
    })
    completion.catch(() => this._owner._closeAll('relay thumbnail write failed'))
    return completion
  }

  close() { this._owner._closeAll(`required relay lane closed: ${this.lane}`) }

  _open() {
    if (this.readyState !== 'connecting') return
    this.readyState = 'open'
    this.onopen?.()
  }

  _deliver(data) { this.onmessage?.({ data }) }

  _closeFromOwner() {
    if (this.readyState === 'closed') return
    this.readyState = 'closed'
    this.onclose?.()
  }
}

function laneForClass(className) {
  if (className === TRAFFIC_CLASS_CONTROL) return LANE_CONTROL
  if (className === TRAFFIC_CLASS_INTERACTIVE_MEDIA || className === TRAFFIC_CLASS_THUMBNAIL) return LANE_MEDIA
  if (className === TRAFFIC_CLASS_BULK) return LANE_BULK
  throw new RangeError(`unknown traffic class: ${className}`)
}

function toUint8Array(value) {
  if (value instanceof Uint8Array) return value
  if (value instanceof ArrayBuffer) return new Uint8Array(value)
  if (ArrayBuffer.isView(value)) return new Uint8Array(value.buffer, value.byteOffset, value.byteLength)
  throw new TypeError('relay lane payload must be text or bytes')
}

function equalBytes(left, right) {
  if (!left || !right || left.length !== right.length) return false
  for (let i = 0; i < left.length; i += 1) {
    if (left[i] !== right[i]) return false
  }
  return true
}
