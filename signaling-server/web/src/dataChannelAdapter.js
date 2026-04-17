// src/dataChannelAdapter.js
import { FRAME_TEXT, FRAME_BINARY, writeFrame, FrameDecoder } from './frame.js';

// LibP2PDataChannel wraps a libp2p stream to look like an RTCDataChannel to
// the rest of app.js: assign .onopen, .onmessage, .onclose; call .send() /
// .sendBinary() to write.
//
// .readyState mirrors the browser DataChannel enum ('connecting' | 'open' |
// 'closing' | 'closed').
//
// binaryType is fixed to 'arraybuffer' to match the existing app.js
// assumption (app.js:184).
export class LibP2PDataChannel {
  constructor(stream) {
    this._stream = stream;
    this._decoder = new FrameDecoder();
    this.readyState = 'connecting';
    this.binaryType = 'arraybuffer';
    this.onopen = null;
    this.onmessage = null;
    this.onclose = null;
  }

  // start: begin the read pump. Call once from libp2pClient after the first
  // non-handshake frame is expected. Resolves when the stream ends.
  async start() {
    this.readyState = 'open';
    queueMicrotask(() => { if (this.onopen) this.onopen(); });
    try {
      for await (const chunk of this._stream.source) {
        const bytes = chunk.subarray ? chunk.subarray() : chunk;
        for (const frame of this._decoder.push(bytes)) {
          this._dispatch(frame);
        }
      }
    } catch (err) {
      console.error('libp2p stream read error:', err);
    } finally {
      this.readyState = 'closed';
      if (this.onclose) this.onclose();
    }
  }

  _dispatch({ kind, payload }) {
    if (!this.onmessage) return;
    if (kind === FRAME_TEXT) {
      this.onmessage({ data: new TextDecoder().decode(payload) });
    } else {
      // binaryType arraybuffer → deliver ArrayBuffer, not Uint8Array
      this.onmessage({ data: payload.buffer.slice(payload.byteOffset, payload.byteOffset + payload.byteLength) });
    }
  }

  // send: JSON string messages (control plane — same shape as old
  // dc.send(JSON.stringify(...))). Assumes the caller has already stringified.
  async send(text) {
    if (this.readyState !== 'open') throw new Error('data channel not open');
    const payload = new TextEncoder().encode(text);
    await this._stream.sink([writeFrame(FRAME_TEXT, payload)]);
  }

  // sendBinary: bytes message (unused by the current browser but kept for
  // parity). Present so the shim is symmetric with the agent-side adapter.
  async sendBinary(bytes) {
    if (this.readyState !== 'open') throw new Error('data channel not open');
    await this._stream.sink([writeFrame(FRAME_BINARY, bytes)]);
  }

  close() {
    this.readyState = 'closing';
    this._stream.close().catch(() => {});
  }
}