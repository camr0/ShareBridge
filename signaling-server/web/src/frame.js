// src/frame.js
export const FRAME_TEXT = 0x01;
export const FRAME_BINARY = 0x02;
export const MAX_FRAME_PAYLOAD = 8 * 1024 * 1024;

// writeFrame: returns a single Uint8Array containing the 5-byte header + payload.
export function writeFrame(kind, payload) {
  if (kind !== FRAME_TEXT && kind !== FRAME_BINARY) {
    throw new Error(`unknown frame kind: 0x${kind.toString(16)}`);
  }
  if (payload.length > MAX_FRAME_PAYLOAD) {
    throw new Error(`frame too large: ${payload.length} bytes`);
  }
  const out = new Uint8Array(5 + payload.length);
  out[0] = kind;
  out[1] = (payload.length >>> 24) & 0xff;
  out[2] = (payload.length >>> 16) & 0xff;
  out[3] = (payload.length >>> 8) & 0xff;
  out[4] = payload.length & 0xff;
  out.set(payload, 5);
  return out;
}

// readFrameFromChunks: iterator-style decoder over a sequence of Uint8Array
// chunks. Yields { kind, payload } for each complete frame. Used in tests; in
// production we feed chunks from a libp2p stream (see libp2pClient.js).
export function* readFrameFromChunks(chunks) {
  const decoder = new FrameDecoder();
  for (const chunk of chunks) {
    for (const frame of decoder.push(chunk)) yield frame;
  }
}

// FrameDecoder: streaming decoder. Call push(chunk) to get an iterable of
// complete frames extracted so far. Holds partial data internally.
export class FrameDecoder {
  constructor() {
    this._buf = new Uint8Array(0);
  }

  *push(chunk) {
    if (chunk.length === 0) return;
    const combined = new Uint8Array(this._buf.length + chunk.length);
    combined.set(this._buf, 0);
    combined.set(chunk, this._buf.length);
    this._buf = combined;

    while (this._buf.length >= 5) {
      const kind = this._buf[0];
      if (kind !== FRAME_TEXT && kind !== FRAME_BINARY) {
        throw new Error(`unknown frame kind: 0x${kind.toString(16)}`);
      }
      const view = new DataView(this._buf.buffer, this._buf.byteOffset, this._buf.byteLength);
      const length = view.getUint32(1, false); // big-endian
      if (length > MAX_FRAME_PAYLOAD) {
        throw new Error(`frame too large: ${length} bytes`);
      }
      if (this._buf.length < 5 + length) break;
      const payload = this._buf.slice(5, 5 + length);
      this._buf = this._buf.slice(5 + length);
      yield { kind, payload };
    }
  }
}