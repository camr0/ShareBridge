export const FRAME_HANDSHAKE = 0x00
export const FRAME_TEXT = 0x01
export const FRAME_BINARY = 0x02

export const MAX_HANDSHAKE_PAYLOAD = 4 * 1024
export const MAX_FRAME_PAYLOAD = 8 * 1024 * 1024

function getCapForKind(kind) {
  if (kind === FRAME_HANDSHAKE) return MAX_HANDSHAKE_PAYLOAD
  if (kind === FRAME_TEXT || kind === FRAME_BINARY) return MAX_FRAME_PAYLOAD
  throw new Error(`unknown frame kind: 0x${kind.toString(16)}`)
}

export function writeFrame(kind, payload) {
  const cap = getCapForKind(kind)
  if (payload.length > cap) {
    if (kind === FRAME_HANDSHAKE) {
      throw new Error(`handshake frame too large: ${payload.length} bytes`)
    }
    throw new Error(`frame too large: ${payload.length} bytes`)
  }

  const out = new Uint8Array(5 + payload.length)
  out[0] = kind
  out[1] = (payload.length >>> 24) & 0xff
  out[2] = (payload.length >>> 16) & 0xff
  out[3] = (payload.length >>> 8) & 0xff
  out[4] = payload.length & 0xff
  out.set(payload, 5)
  return out
}

export class FrameDecoder {
  constructor() {
    this.buffer = new Uint8Array(0)
  }

  *push(chunk) {
    const next = new Uint8Array(this.buffer.length + chunk.length)
    next.set(this.buffer, 0)
    next.set(chunk, this.buffer.length)
    this.buffer = next

    while (this.buffer.length >= 5) {
      const kind = this.buffer[0]
      const cap = getCapForKind(kind)
      const view = new DataView(this.buffer.buffer, this.buffer.byteOffset, this.buffer.byteLength)
      const length = view.getUint32(1, false)
      if (length > cap) {
        if (kind === FRAME_HANDSHAKE) {
          throw new Error(`handshake frame too large: ${length} bytes`)
        }
        throw new Error(`frame too large: ${length} bytes`)
      }
      if (this.buffer.length < 5 + length) break
      const payload = this.buffer.slice(5, 5 + length)
      this.buffer = this.buffer.slice(5 + length)
      yield { kind, payload }
    }
  }
}