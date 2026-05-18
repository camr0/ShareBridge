import { NoiseXX } from '../noise-p256/index.js'
import { FRAME_HANDSHAKE, FRAME_TEXT, FRAME_BINARY, FrameDecoder, writeFrame } from './frame.js'

const DEBUG = typeof location !== 'undefined' && (
  location.search.includes('debug=1') ||
  (typeof localStorage !== 'undefined' && localStorage.getItem('sharebridge_debug'))
)

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
    this.readyState = 'connecting'
    this.bufferedAmount = 0
    this.onopen = null
    this.onmessage = null
    this.onclose = null
  }

  async start() {
    debugLog('SecureRelayChannel.start() called')
    this._noise = await NoiseXX.createInitiator()
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
        if (this.onclose) this.onclose()
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
          if (this.onclose) this.onclose()
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
    debugLog('SecureRelayChannel.start() completed')
  }

  _handleMessage = async (event) => {
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
        const plaintext = await this._recvCipher.decrypt(new Uint8Array(0), frame.payload)
        if (frame.kind === FRAME_TEXT) {
          debugLog('relay text frame decrypted', { byteLength: plaintext.length })
          this.onmessage?.({ data: new TextDecoder().decode(plaintext) })
        } else if (frame.kind === FRAME_BINARY) {
          debugLog('relay binary frame decrypted', { byteLength: plaintext.length })
          // Create a proper ArrayBuffer copy to avoid browser-specific issues with underlying buffer
          const arrayBuffer = new ArrayBuffer(plaintext.length)
          new Uint8Array(arrayBuffer).set(plaintext)
          this.onmessage?.({ data: arrayBuffer })
        } else {
          throw new Error(`unknown frame kind: 0x${frame.kind.toString(16)}`)
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
    this.readyState = 'open'
    debugLog('Handshake complete, readyState is open')
    this._handshakeResolve?.()
    this.onopen?.()
  }

  async send(text) {
    const plaintext = new TextEncoder().encode(text)
    debugLog('sending relay text frame', { byteLength: plaintext.length })
    const ciphertext = await this._sendCipher.encrypt(new Uint8Array(0), plaintext)
    this._socket.send(writeFrame(FRAME_TEXT, ciphertext))
  }

  async sendBinary(bytes) {
    debugLog('sending relay binary frame', { byteLength: bytes.length })
    const ciphertext = await this._sendCipher.encrypt(new Uint8Array(0), bytes)
    this._socket.send(writeFrame(FRAME_BINARY, ciphertext))
  }

  close(reason = 'local close') {
    debugLog('SecureRelayChannel.close()', {
      reason,
      readyState: this.readyState,
      socketReadyState: this._socket?.readyState,
    })
    this.readyState = 'closing'
    this._socket?.close()
  }
}

function equalBytes(left, right) {
  if (!left || !right || left.length !== right.length) return false
  for (let i = 0; i < left.length; i += 1) {
    if (left[i] !== right[i]) return false
  }
  return true
}
