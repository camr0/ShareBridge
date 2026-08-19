import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  FRAME_HANDSHAKE,
  FRAME_TEXT,
  FRAME_BINARY,
  MAX_FRAME_PAYLOAD,
  MAX_HANDSHAKE_PAYLOAD,
  writeFrame,
  FrameDecoder,
} from './frame.js'

test('encodes and decodes handshake and data frames', () => {
  const decoder = new FrameDecoder()
  const encoded = writeFrame(FRAME_TEXT, new TextEncoder().encode('hello'))
  const [frame] = [...decoder.push(encoded)]
  assert.equal(frame.kind, FRAME_TEXT)
  assert.equal(new TextDecoder().decode(frame.payload), 'hello')
})

test('rejects unknown frame kinds', () => {
  const decoder = new FrameDecoder()
  assert.throws(() => [...decoder.push(Uint8Array.from([0xff, 0, 0, 0, 0]))], /unknown frame kind/i)
})

test('enforces handshake and payload caps independently', () => {
  assert.throws(
    () => writeFrame(FRAME_HANDSHAKE, new Uint8Array(MAX_HANDSHAKE_PAYLOAD + 1)),
    /handshake frame too large/i
  )
  assert.throws(
    () => writeFrame(FRAME_BINARY, new Uint8Array(MAX_FRAME_PAYLOAD + 1)),
    /frame too large/i
  )
})