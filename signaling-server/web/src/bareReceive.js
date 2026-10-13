// Bare-receive mode — the EXP28 ablation (2026-09-18) recovered as a permanent,
// reversible artifact by EXP44 (2026-10-13).
//
// Activated ONLY by the explicit test query parameter `?bare=1`. When the
// parameter is absent `installBareReceive()` returns `null`, app.js installs no
// bare branch anywhere, and the production receive path is byte-for-byte the
// code that ships today.
//
// The receiver counts DataChannel *wire* bytes (envelope + payload) and frames,
// exactly as TESTBOX's temporary EXP28 patch did. It deliberately decodes
// nothing: no `decodeBinaryEnvelope`, no `routeOrBufferFrame`, no chunk
// assembly, no early-frame buffer, no StreamSaver/service-worker writer, no
// SHA-1, no `concatBytes` copy, no per-chunk DOM update.
//
// The published counter object is read by the field drivers `drive_bare.js` /
// `drive_bare_ff.js` on the client VMs, which expect exactly these five fields:
//
//     globalThis.__bare = { bytes, frames, header, startedAt, endedAt }
//
// Accounting gate: `wire - envelopeBytes * frames == payload`. For the v1
// single-lane default `envelopeBytes` is 14 (CHUNK_ENVELOPE_BYTES in
// binaryEnvelope.js); a striped N5/A1 lane emits 22-byte v2 envelopes, so the
// parameter can override it with `?bare=1&bare_env=22`.
// bareReceive.test.js asserts this default equals binaryEnvelope's
// CHUNK_ENVELOPE_BYTES so the duplication cannot silently drift.

export const BARE_RECEIVE_PARAM = 'bare'
export const BARE_ENVELOPE_PARAM = 'bare_env'
export const DEFAULT_ENVELOPE_BYTES = 14

/** True when `search` (a `location.search` string) requests bare receive mode. */
export function isBareReceiveEnabled(search) {
  if (typeof search !== 'string' || search.length === 0) return false
  try {
    const params = new URLSearchParams(search.startsWith('?') ? search : `?${search}`)
    if (!params.has(BARE_RECEIVE_PARAM)) return false
    const value = params.get(BARE_RECEIVE_PARAM)
    // `?bare`, `?bare=`, `?bare=1` and `?bare=true` all enable it; an explicit
    // falsey spelling does not.
    return value === null || value === '' || value === '1' || value === 'true'
  } catch {
    return false
  }
}

function resolveEnvelopeBytes(search, override) {
  if (Number.isInteger(override) && override > 0) return override
  try {
    const params = new URLSearchParams(search.startsWith('?') ? search : `?${search}`)
    const raw = params.get(BARE_ENVELOPE_PARAM)
    if (raw !== null) {
      const parsed = Number(raw)
      if (Number.isInteger(parsed) && parsed > 0) return parsed
    }
  } catch {
    // fall through to the default
  }
  return DEFAULT_ENVELOPE_BYTES
}

function byteLengthOf(data) {
  if (data === null || data === undefined) return 0
  if (typeof data.byteLength === 'number') return data.byteLength
  if (typeof data.size === 'number') return data.size // Blob fallback
  if (typeof data.length === 'number') return data.length
  return 0
}

/**
 * Builds the bare counter and publishes it as `globalThis.__bare`.
 * Returns `null` when bare mode is off, which is what makes the caller inert.
 */
export function installBareReceive({
  search = typeof location !== 'undefined' ? location.search : '',
  scope = typeof globalThis !== 'undefined' ? globalThis : {},
  clock = Date.now,
  envelopeBytes,
} = {}) {
  if (!isBareReceiveEnabled(search)) return null

  const receiver = {
    bytes: 0,
    frames: 0,
    header: null,
    startedAt: 0,
    endedAt: 0,
    envelopeBytes: resolveEnvelopeBytes(search, envelopeBytes),
    /** Counts one arriving DataChannel message. Never parses it. */
    record(data) {
      const length = byteLengthOf(data)
      this.bytes += length
      this.frames += 1
      const now = clock()
      if (this.startedAt === 0) this.startedAt = now
      this.endedAt = now
    },
    /** Records the framing header that the control lane still delivers. */
    markHeader(msg) {
      if (msg?.type !== 'file_header') return
      this.header = {
        name: msg.name,
        size: msg.size,
        estimatedSize: msg.estimated_size,
      }
    },
    /** Payload bytes implied by the wire count, per the EXP28 gate. */
    payloadBytes() {
      return this.bytes - this.envelopeBytes * this.frames
    },
  }

  scope.__bare = receiver
  return receiver
}
