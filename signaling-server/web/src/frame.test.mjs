// src/frame.test.mjs — run with `node --test src/frame.test.mjs`
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { FRAME_TEXT, FRAME_BINARY, writeFrame, readFrameFromChunks } from './frame.js';

test('round-trip text and binary', () => {
  const textFrame = writeFrame(FRAME_TEXT, new TextEncoder().encode('hello'));
  const binFrame = writeFrame(FRAME_BINARY, new Uint8Array([0xde, 0xad, 0xbe, 0xef]));
  const concat = new Uint8Array(textFrame.length + binFrame.length);
  concat.set(textFrame, 0);
  concat.set(binFrame, textFrame.length);

  const reader = readFrameFromChunks([concat]);
  const first = reader.next();
  assert.equal(first.value.kind, FRAME_TEXT);
  assert.equal(new TextDecoder().decode(first.value.payload), 'hello');

  const second = reader.next();
  assert.equal(second.value.kind, FRAME_BINARY);
  assert.deepEqual(Array.from(second.value.payload), [0xde, 0xad, 0xbe, 0xef]);

  assert.ok(reader.next().done);
});

test('rejects oversized frame', () => {
  // kind=binary, length = 0xFFFFFFFF
  const bad = new Uint8Array([0x02, 0xff, 0xff, 0xff, 0xff]);
  assert.throws(() => readFrameFromChunks([bad]).next(), /too large/);
});

test('rejects unknown kind', () => {
  const bad = new Uint8Array([0x99, 0, 0, 0, 0]);
  assert.throws(() => readFrameFromChunks([bad]).next(), /unknown frame kind/);
});

test('handles chunk boundary mid-header', () => {
  const frame = writeFrame(FRAME_TEXT, new TextEncoder().encode('xy'));
  const chunk1 = frame.slice(0, 2);
  const chunk2 = frame.slice(2);
  const reader = readFrameFromChunks([chunk1, chunk2]);
  const { value } = reader.next();
  assert.equal(value.kind, FRAME_TEXT);
  assert.equal(new TextDecoder().decode(value.payload), 'xy');
});