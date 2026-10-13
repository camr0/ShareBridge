import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  CHUNK_ENVELOPE_BYTES,
  CHUNK_ENVELOPE_V2_BYTES,
  decodeBinaryEnvelope,
  encodeChunkEnvelope,
  encodeChunkEnvelopeV2,
  encodeThumbnailEnvelope,
  FRAME_FILE_CHUNK,
  FRAME_THUMBNAIL
} from './binaryEnvelope.js'

test('decodes typed thumbnail envelope', () => {
  const encoded = encodeThumbnailEnvelope(258, new Uint8Array([1, 2, 3]))
  const decoded = decodeBinaryEnvelope(encoded)
  assert.equal(decoded.type, FRAME_THUMBNAIL)
  assert.equal(decoded.index, 258)
  assert.deepEqual([...decoded.payload], [1, 2, 3])
})

test('rejects untyped binary under protocol v2', () => {
  assert.throws(() => decodeBinaryEnvelope(new Uint8Array([9, 8, 7])), /unknown binary frame kind/i)
})

test('decodes v1 file chunk envelope without losing uint64 precision', () => {
  const encoded = encodeChunkEnvelope('18446744073709551615', 0xfedcba98, new Uint8Array([4, 5, 6]))
  const decoded = decodeBinaryEnvelope(encoded)
  assert.equal(decoded.type, FRAME_FILE_CHUNK)
  assert.equal(decoded.operationId, '18446744073709551615')
  assert.equal(decoded.generation, 0xfedcba98)
  assert.deepEqual([...decoded.payload], [4, 5, 6])
})

test('decodes a v2 striped chunk envelope and preserves the uint64 sequence', () => {
  const encoded = encodeChunkEnvelopeV2('18446744073709551615', 0, 48263n, new Uint8Array([4, 5, 6]))
  assert.equal(encoded.byteLength, CHUNK_ENVELOPE_V2_BYTES + 3)
  assert.equal(encoded[1], 0x02)

  const decoded = decodeBinaryEnvelope(encoded)
  assert.equal(decoded.type, FRAME_FILE_CHUNK)
  assert.equal(decoded.operationId, '18446744073709551615')
  assert.equal(decoded.generation, 0)
  assert.equal(decoded.sequence, 48263n)
  assert.equal(decoded.envelopeVersion, 2)
  assert.deepEqual([...decoded.payload], [4, 5, 6])
})

test('v1 frames carry no sequence so the unstriped path is unchanged', () => {
  const decoded = decodeBinaryEnvelope(encodeChunkEnvelope('42', 0, new Uint8Array([1])))
  assert.equal(decoded.sequence, undefined)
  assert.equal(decoded.envelopeVersion, undefined)
  assert.equal(Object.hasOwn(decoded, 'sequence'), false)
})

test('rejects a truncated v2 envelope and an unknown version byte', () => {
  // version 0x02 in a 14-byte frame is a truncated v2, not a valid v1 frame
  assert.throws(
    () => decodeBinaryEnvelope(new Uint8Array(14).fill(0).map((value, index) => index === 0 ? 0x10 : index === 1 ? 0x02 : value)),
    /too short/i,
  )
  assert.throws(
    () => decodeBinaryEnvelope(new Uint8Array(22).fill(0).map((value, index) => index === 0 ? 0x10 : index === 1 ? 0x03 : value)),
    /version/i,
  )
})

test('rejects malformed and unsupported file chunk envelopes', () => {
  assert.throws(() => decodeBinaryEnvelope(new Uint8Array([0x10, 0x01])), /too short/i)
  assert.throws(() => encodeChunkEnvelope('0', 0, new Uint8Array()), /operation/i)
  assert.throws(() => encodeChunkEnvelopeV2('42', 0, -1n, new Uint8Array()), /uint64/i)
  assert.equal(CHUNK_ENVELOPE_BYTES, 14)
})
