export const FRAME_FILE_CHUNK = 'file_chunk'
export const FRAME_THUMBNAIL = 'thumbnail'

const KIND_FILE_CHUNK = 0x10
const KIND_THUMBNAIL = 0x11

export function encodeThumbnailEnvelope(index, payload) {
  const out = new Uint8Array(3 + payload.length)
  out[0] = KIND_THUMBNAIL
  out[1] = (index >> 8) & 0xff
  out[2] = index & 0xff
  out.set(payload, 3)
  return out
}

export function decodeBinaryEnvelope(input) {
  const data = input instanceof Uint8Array ? input : new Uint8Array(input)
  if (data[0] === KIND_THUMBNAIL) {
    return { type: FRAME_THUMBNAIL, index: (data[1] << 8) | data[2], payload: data.slice(3) }
  }
  if (data[0] === KIND_FILE_CHUNK) {
    return { type: FRAME_FILE_CHUNK, payload: data.slice(1) }
  }
  return { type: FRAME_FILE_CHUNK, payload: data }
}
