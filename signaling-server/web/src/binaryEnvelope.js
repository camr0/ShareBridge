export const FRAME_FILE_CHUNK = 'file_chunk'
export const FRAME_THUMBNAIL = 'thumbnail'

const KIND_FILE_CHUNK = 0x10
const KIND_THUMBNAIL = 0x11
const CHUNK_ENVELOPE_VERSION = 0x01

// Chunk envelope versions. v1 is the agent's original 14-byte layout
// (kind + version + uint64 operation + uint32 generation). v2 prepends the same
// 14 bytes and appends a uint64 global chunk sequence; the agent emits it only
// when a striping lever (SB_BULK_LANES / SB_BULK_CONNS) is enabled, and the
// receiver uses the sequence to restore source order across lanes.
// Mirrors agent/internal/bulkfan/envelope.go.
const CHUNK_ENVELOPE_VERSION_V2 = 0x02
export const CHUNK_ENVELOPE_BYTES = 14
export const CHUNK_ENVELOPE_V2_BYTES = 22

export function encodeThumbnailEnvelope(index, payload) {
  const out = new Uint8Array(3 + payload.length)
  out[0] = KIND_THUMBNAIL
  out[1] = (index >> 8) & 0xff
  out[2] = index & 0xff
  out.set(payload, 3)
  return out
}

export function encodeChunkEnvelope(operationId, generation, payload) {
  let operation
  try {
    operation = BigInt(operationId)
  } catch {
    throw new RangeError('operation id must be a uint64 decimal string')
  }
  if (operation <= 0n || operation > 0xffffffffffffffffn) {
    throw new RangeError('operation id must be between 1 and uint64 max')
  }
  if (!Number.isInteger(generation) || generation < 0 || generation > 0xffffffff) {
    throw new RangeError('generation must be a uint32')
  }
  const bytes = payload instanceof Uint8Array ? payload : new Uint8Array(payload)
  const out = new Uint8Array(CHUNK_ENVELOPE_BYTES + bytes.byteLength)
  const view = new DataView(out.buffer)
  out[0] = KIND_FILE_CHUNK
  out[1] = CHUNK_ENVELOPE_VERSION
  view.setBigUint64(2, operation, false)
  view.setUint32(10, generation, false)
  out.set(bytes, CHUNK_ENVELOPE_BYTES)
  return out
}

export function decodeBinaryEnvelope(input) {
  const data = input instanceof Uint8Array ? input : new Uint8Array(input)
  if (data[0] === KIND_THUMBNAIL) {
    if (data.byteLength < 3) throw new Error('thumbnail envelope too short')
    return { type: FRAME_THUMBNAIL, index: (data[1] << 8) | data[2], payload: data.slice(3) }
  }
  if (data[0] === KIND_FILE_CHUNK) {
    // Length is checked before the version so a truncated frame is reported as
    // truncated, matching bulkfan.DecodeChunkFrame's ordering.
    if (data.byteLength < CHUNK_ENVELOPE_BYTES) throw new Error('file chunk envelope too short')
    const version = data[1]
    if (version !== CHUNK_ENVELOPE_VERSION && version !== CHUNK_ENVELOPE_VERSION_V2) {
      throw new Error(`unsupported chunk envelope version: ${version}`)
    }
    const envelopeBytes = version === CHUNK_ENVELOPE_VERSION_V2
      ? CHUNK_ENVELOPE_V2_BYTES
      : CHUNK_ENVELOPE_BYTES
    if (data.byteLength < envelopeBytes) throw new Error('file chunk envelope too short')
    const view = new DataView(data.buffer, data.byteOffset, data.byteLength)
    const operation = view.getBigUint64(2, false)
    if (operation === 0n) throw new Error('file chunk operation id must be nonzero')
    const frame = {
      type: FRAME_FILE_CHUNK,
      operationId: operation.toString(10),
      generation: view.getUint32(10, false),
      payload: data.slice(envelopeBytes),
    }
    if (version === CHUNK_ENVELOPE_VERSION_V2) {
      // BigInt: the agent's sequence counter is a uint64.
      frame.envelopeVersion = CHUNK_ENVELOPE_VERSION_V2
      frame.sequence = view.getBigUint64(CHUNK_ENVELOPE_BYTES, false)
    }
    return frame
  }
  throw new Error(`unknown binary frame kind: 0x${(data[0] ?? 0).toString(16).padStart(2, '0')}`)
}

/**
 * Encodes a v2 chunk envelope carrying the global stripe sequence. Browser-side
 * mirror of `bulkfan.EncodeChunkFrame`; used by tests and by any future client
 * that needs to emit striped frames.
 */
export function encodeChunkEnvelopeV2(operationId, generation, sequence, payload) {
  const v1 = encodeChunkEnvelope(operationId, generation, payload)
  const seq = typeof sequence === 'bigint' ? sequence : BigInt(sequence)
  if (seq < 0n || seq > 0xffffffffffffffffn) {
    throw new RangeError('sequence must be a uint64')
  }
  const payloadBytes = v1.length - CHUNK_ENVELOPE_BYTES
  const out = new Uint8Array(CHUNK_ENVELOPE_V2_BYTES + payloadBytes)
  out.set(v1.subarray(0, CHUNK_ENVELOPE_BYTES))
  out[1] = CHUNK_ENVELOPE_VERSION_V2
  new DataView(out.buffer).setBigUint64(CHUNK_ENVELOPE_BYTES, seq, false)
  out.set(v1.subarray(CHUNK_ENVELOPE_BYTES), CHUNK_ENVELOPE_V2_BYTES)
  return out
}
