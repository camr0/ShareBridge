import { NoiseXX } from '../noise-p256/index.js'
import { FRAME_HANDSHAKE, FRAME_TEXT, FRAME_BINARY, FrameDecoder, writeFrame } from './frame.js'

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
    this._noise = await NoiseXX.createInitiator()
    this._socket = this._websocketFactory(this._relayURL)
    this._socket.binaryType = 'arraybuffer'

    await new Promise((resolve, reject) => {
      const handleOpen = async () => {
        try {
          this._socket.send(new TextEncoder().encode(JSON.stringify({ token: this._relayToken })))
          this._socket.addEventListener('message', this._handleMessage)
          const msg1 = await this._noise.writeMessage1()
          this._socket.send(writeFrame(FRAME_HANDSHAKE, msg1))
          resolve()
        } catch (err) {
          reject(err)
        }
      }
      this._socket.addEventListener('open', handleOpen, { once: true })
      this._socket.addEventListener('error', reject, { once: true })
      this._socket.addEventListener('close', () => {
        this.readyState = 'closed'
        if (this.onclose) this.onclose()
      }, { once: true })
      if (this._socket.readyState === 1) {
        handleOpen()
      }
    })

    await new Promise((resolve, reject) => {
      this._handshakeResolve = resolve
      this._handshakeReject = reject
    })
  }

  _handleMessage = async (event) => {
    const chunk = new Uint8Array(event.data)
    for (const frame of this._decoder.push(chunk)) {
      if (frame.kind === FRAME_HANDSHAKE) {
        try {
          await this._handleHandshakeFrame(frame.payload)
        } catch (err) {
          this.close()
          this._handshakeReject?.(err)
        }
        continue
      }

      try {
        const plaintext = await this._recvCipher.decrypt(new Uint8Array(0), frame.payload)
        if (frame.kind === FRAME_TEXT) {
          this.onmessage?.({ data: new TextDecoder().decode(plaintext) })
        } else if (frame.kind === FRAME_BINARY) {
          this.onmessage?.({ data: plaintext.buffer.slice(plaintext.byteOffset, plaintext.byteOffset + plaintext.byteLength) })
        } else {
          throw new Error(`unknown frame kind: 0x${frame.kind.toString(16)}`)
        }
      } catch (err) {
        this.close()
      }
    }
  }

  async _handleHandshakeFrame(msg2) {
    await this._noise.readMessage2(msg2)
    if (!equalBytes(this._noise.remoteStaticPub, this._expectedStaticPub)) {
      throw new Error('unexpected agent static key')
    }
    const msg3 = await this._noise.writeMessage3()
    this._socket.send(writeFrame(FRAME_HANDSHAKE, msg3))
    ;[this._sendCipher, this._recvCipher] = await this._noise.split()
    this.readyState = 'open'
    this._handshakeResolve?.()
    this.onopen?.()
  }

  async send(text) {
    const plaintext = new TextEncoder().encode(text)
    const ciphertext = await this._sendCipher.encrypt(new Uint8Array(0), plaintext)
    this._socket.send(writeFrame(FRAME_TEXT, ciphertext))
  }

  async sendBinary(bytes) {
    const ciphertext = await this._sendCipher.encrypt(new Uint8Array(0), bytes)
    this._socket.send(writeFrame(FRAME_BINARY, ciphertext))
  }

  close() {
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
