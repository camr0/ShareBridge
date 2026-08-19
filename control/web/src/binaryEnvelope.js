export const FRAME_FILE_CHUNK = 'file_chunk'
export const FRAME_THUMBNAIL = 'thumbnail'

const KIND_FILE_CHUNK = 0x10
const KIND_THUMBNAIL = 0x11
const CHUNK_ENVELOPE_VERSION = 0x01
const CHUNK_ENVELOPE_BYTES = 14

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
    if (data.byteLength < CHUNK_ENVELOPE_BYTES) throw new Error('file chunk envelope too short')
    if (data[1] !== CHUNK_ENVELOPE_VERSION) throw new Error(`unsupported chunk envelope version: ${data[1]}`)
    const view = new DataView(data.buffer, data.byteOffset, data.byteLength)
    const operation = view.getBigUint64(2, false)
    if (operation === 0n) throw new Error('file chunk operation id must be nonzero')
    return {
      type: FRAME_FILE_CHUNK,
      operationId: operation.toString(10),
      generation: view.getUint32(10, false),
      payload: data.slice(CHUNK_ENVELOPE_BYTES),
    }
  }
  throw new Error(`unknown binary frame kind: 0x${(data[0] ?? 0).toString(16).padStart(2, '0')}`)
}
