// EXP44 — node proof of the recovered EXP28 bare-receive byte accounting.
//
// The field gate from 2026-09-18-exp28-bare-receive-path.md is
//     wire - CHUNK_ENVELOPE_BYTES * frames == 790,626,304
// for the 754 MiB campaign payload. These tests re-derive that from synthetic
// DataChannel frames only — no browser, no field cell, no agent.
import { test } from 'node:test'
import assert from 'node:assert/strict'

import { CHUNK_ENVELOPE_BYTES, CHUNK_ENVELOPE_V2_BYTES } from './binaryEnvelope.js'
import {
  BARE_RECEIVE_PARAM,
  DEFAULT_ENVELOPE_BYTES,
  installBareReceive,
  isBareReceiveEnabled,
} from './bareReceive.js'

// The campaign payload: 754 MiB.
const CAMPAIGN_PAYLOAD = 790626304
// E28 B-A1's observed agent-side frame count / wire bytes, reproduced synthetically.
const E28_FRAMES = 48263
const E28_WIRE = 791301986

test('bare mode is enabled only by an explicit truthy bare parameter', () => {
  assert.equal(BARE_RECEIVE_PARAM, 'bare')
  assert.equal(isBareReceiveEnabled('?bare=1'), true)
  assert.equal(isBareReceiveEnabled('?bare'), true)
  assert.equal(isBareReceiveEnabled('?bare='), true)
  assert.equal(isBareReceiveEnabled('?bare=true'), true)
  assert.equal(isBareReceiveEnabled('?debug=1&bare=1'), true)
  assert.equal(isBareReceiveEnabled('?bare=0'), false)
  assert.equal(isBareReceiveEnabled('?bare=false'), false)
  assert.equal(isBareReceiveEnabled('?barely=1'), false)
  assert.equal(isBareReceiveEnabled('?debug=1'), false)
  assert.equal(isBareReceiveEnabled(''), false)
  assert.equal(isBareReceiveEnabled(undefined), false)
})

test('an absent bare parameter produces no receiver and no global', () => {
  const scope = {}
  const receiver = installBareReceive({ search: '?debug=1', scope, clock: () => 0 })

  assert.equal(receiver, null)
  assert.equal(Object.hasOwn(scope, '__bare'), false, 'production path must stay inert')
})

test('bare mode publishes the EXP28 counter shape and counts wire bytes only', () => {
  const scope = {}
  let now = 1000
  const receiver = installBareReceive({ search: '?bare=1', scope, clock: () => now })

  assert.equal(scope.__bare, receiver, 'the driver reads window.__bare')
  assert.deepEqual(
    { bytes: receiver.bytes, frames: receiver.frames, header: receiver.header, startedAt: receiver.startedAt, endedAt: receiver.endedAt },
    { bytes: 0, frames: 0, header: null, startedAt: 0, endedAt: 0 },
  )

  receiver.record(new ArrayBuffer(1014))
  now = 1100
  receiver.record(new Uint8Array(507))
  now = 1250
  receiver.record(undefined) // a non-binary message under the bare handler

  assert.equal(receiver.frames, 3)
  assert.equal(receiver.bytes, 1014 + 507)
  assert.equal(receiver.startedAt, 1000, 'clock starts at the first frame')
  assert.equal(receiver.endedAt, 1250, 'clock follows the last frame')
  assert.equal(receiver.payloadBytes(), 1521 - DEFAULT_ENVELOPE_BYTES * 3)
})

test('markHeader retains only the framing fields the driver reports', () => {
  const receiver = installBareReceive({ search: '?bare=1', scope: {}, clock: () => 0 })
  receiver.markHeader({ type: 'chunk_end', operation_id: '42' })
  assert.equal(receiver.header, null, 'chunk_end is not a header')
  receiver.markHeader({ type: 'file_header', name: 'debian.iso', size: CAMPAIGN_PAYLOAD, estimated_size: CAMPAIGN_PAYLOAD })
  assert.deepEqual(receiver.header, { name: 'debian.iso', size: CAMPAIGN_PAYLOAD, estimatedSize: CAMPAIGN_PAYLOAD })
})

test('the default envelope size agrees with the wire envelope module', () => {
  assert.equal(DEFAULT_ENVELOPE_BYTES, CHUNK_ENVELOPE_BYTES)
  assert.equal(CHUNK_ENVELOPE_BYTES, 14)
  assert.equal(CHUNK_ENVELOPE_V2_BYTES, 22)
})

test('synthetic frames reproduce the field gate exactly (uniform 16 KiB chunking)', () => {
  const scope = {}
  const receiver = installBareReceive({ search: '?bare=1', scope, clock: () => 0 })

  // 790,626,304 / 16,384 == 48,256 frames of 16,384-byte payload, each wrapped
  // in the 14-byte v1 envelope: 16,398 wire bytes per frame.
  const frames = 48256
  const wirePerFrame = 16384 + CHUNK_ENVELOPE_BYTES
  for (let i = 0; i < frames; i++) receiver.record(new ArrayBuffer(wirePerFrame))

  assert.equal(receiver.frames, frames)
  assert.equal(receiver.bytes, frames * wirePerFrame)
  assert.equal(
    receiver.bytes - CHUNK_ENVELOPE_BYTES * receiver.frames,
    CAMPAIGN_PAYLOAD,
    'wire - 14 * frames == 790,626,304',
  )
  assert.equal(receiver.payloadBytes(), CAMPAIGN_PAYLOAD)
})

test('synthetic frames reproduce E28 B-A1 exactly (mixed chunk sizes)', () => {
  const receiver = installBareReceive({ search: '?bare=1', scope: {}, clock: () => 0 })

  // E28 counted 48,263 frames on 791,301,986 wire bytes. Rebuild the same
  // shape: 48,255 full 16 KiB payloads plus 8 short 2 KiB tails.
  for (let i = 0; i < 48255; i++) receiver.record(new ArrayBuffer(16384 + CHUNK_ENVELOPE_BYTES))
  for (let i = 0; i < 8; i++) receiver.record(new ArrayBuffer(2048 + CHUNK_ENVELOPE_BYTES))

  assert.equal(receiver.frames, E28_FRAMES)
  assert.equal(receiver.bytes, E28_WIRE, 'synthetic wire total matches the 2026-09-18 field log')
  assert.equal(
    receiver.bytes - CHUNK_ENVELOPE_BYTES * receiver.frames,
    CAMPAIGN_PAYLOAD,
    'the E28 frame count also satisfies the gate',
  )
})

test('a single missing frame fails the gate (the gate is not vacuous)', () => {
  const receiver = installBareReceive({ search: '?bare=1', scope: {}, clock: () => 0 })
  const wirePerFrame = 16384 + CHUNK_ENVELOPE_BYTES
  for (let i = 0; i < 48255; i++) receiver.record(new ArrayBuffer(wirePerFrame))

  assert.notEqual(
    receiver.bytes - CHUNK_ENVELOPE_BYTES * receiver.frames,
    CAMPAIGN_PAYLOAD,
    'one dropped frame must be visible to the accounting gate',
  )
})

test('a v2 (striped) envelope size is selectable and changes the implied payload', () => {
  const receiver = installBareReceive({ search: '?bare=1&bare_env=22', scope: {}, clock: () => 0 })
  assert.equal(receiver.envelopeBytes, CHUNK_ENVELOPE_V2_BYTES)
  receiver.record(new ArrayBuffer(16384 + CHUNK_ENVELOPE_V2_BYTES))
  assert.equal(receiver.payloadBytes(), 16384)

  const bogus = installBareReceive({ search: '?bare=1&bare_env=nope', scope: {}, clock: () => 0 })
  assert.equal(bogus.envelopeBytes, DEFAULT_ENVELOPE_BYTES)
})
