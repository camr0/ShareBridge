import assert from 'node:assert/strict';
import test from 'node:test';

import {
  PROTOCOL_VERSION,
  LANE_CONTROL,
  LANE_MEDIA,
  LANE_BULK,
  TRAFFIC_CLASS_CONTROL,
  TRAFFIC_CLASS_INTERACTIVE_MEDIA,
  TRAFFIC_CLASS_THUMBNAIL,
  TRAFFIC_CLASS_BULK,
  encodeLaneEnvelope,
  decodeLaneEnvelope,
} from './multiLaneProtocol.js';

test('protocol constants match the cross-language wire contract', () => {
  assert.equal(PROTOCOL_VERSION, 2);
  assert.equal(LANE_CONTROL, 0x00);
  assert.equal(LANE_MEDIA, 0x01);
  assert.equal(LANE_BULK, 0x02);
  assert.equal(TRAFFIC_CLASS_CONTROL, 'control');
  assert.equal(TRAFFIC_CLASS_INTERACTIVE_MEDIA, 'interactive-media');
  assert.equal(TRAFFIC_CLASS_THUMBNAIL, 'thumbnail');
  assert.equal(TRAFFIC_CLASS_BULK, 'bulk');
});

test('lane envelope is byte-for-byte compatible and copies payloads', () => {
  const original = new Uint8Array([0x00, 0x7f, 0xff]);
  const encoded = encodeLaneEnvelope(LANE_MEDIA, original);
  assert.deepEqual(encoded, new Uint8Array([0x01, 0x00, 0x7f, 0xff]));

  original[0] = 0xaa;
  assert.equal(encoded[1], 0x00);

  const decoded = decodeLaneEnvelope(encoded);
  assert.equal(decoded.lane, LANE_MEDIA);
  assert.deepEqual(decoded.payload, new Uint8Array([0x00, 0x7f, 0xff]));

  encoded[1] = 0xbb;
  assert.equal(decoded.payload[0], 0x00);
});

test('lane envelope accepts a valid lane with an empty payload', () => {
  const encoded = encodeLaneEnvelope(LANE_CONTROL, new Uint8Array());
  assert.deepEqual(encoded, new Uint8Array([LANE_CONTROL]));
  assert.deepEqual(decodeLaneEnvelope(encoded), {
    lane: LANE_CONTROL,
    payload: new Uint8Array(),
  });
});

test('lane envelope rejects empty and reserved envelopes', () => {
  assert.throws(() => decodeLaneEnvelope(new Uint8Array()), /empty/i);
  assert.throws(() => decodeLaneEnvelope(new Uint8Array([0x03])), /lane/i);
  assert.throws(() => decodeLaneEnvelope(new Uint8Array([0xff, 0x01])), /lane/i);
});

test('lane envelope rejects reserved lane IDs while encoding', () => {
  assert.throws(() => encodeLaneEnvelope(0x03, new Uint8Array([1])), /lane/i);
  assert.throws(() => encodeLaneEnvelope(0xff, new Uint8Array([1])), /lane/i);
});
