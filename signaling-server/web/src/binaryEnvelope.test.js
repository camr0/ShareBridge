import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  decodeBinaryEnvelope,
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

test('treats legacy raw binary as file chunks', () => {
  const decoded = decodeBinaryEnvelope(new Uint8Array([9, 8, 7]))
  assert.equal(decoded.type, FRAME_FILE_CHUNK)
  assert.deepEqual([...decoded.payload], [9, 8, 7])
})

test('decodes typed file chunk envelope', () => {
  const decoded = decodeBinaryEnvelope(new Uint8Array([0x10, 4, 5, 6]))
  assert.equal(decoded.type, FRAME_FILE_CHUNK)
  assert.deepEqual([...decoded.payload], [4, 5, 6])
})
