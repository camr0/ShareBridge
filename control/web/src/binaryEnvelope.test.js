import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  decodeBinaryEnvelope,
  encodeChunkEnvelope,
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

test('rejects malformed and unsupported file chunk envelopes', () => {
  assert.throws(() => decodeBinaryEnvelope(new Uint8Array([0x10, 0x01])), /too short/i)
  assert.throws(() => decodeBinaryEnvelope(new Uint8Array(14).fill(0).map((value, index) => index === 0 ? 0x10 : index === 1 ? 0x02 : value)), /version/i)
  assert.throws(() => encodeChunkEnvelope('0', 0, new Uint8Array()), /operation/i)
})
