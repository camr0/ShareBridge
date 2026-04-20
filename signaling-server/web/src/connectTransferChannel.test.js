// signaling-server/web/src/connectTransferChannel.test.js
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { connectTransferChannel, decodeRelayPolicyToken } from './connectTransferChannel.js'

function createDeferred() {
  let resolve
  let reject
  const promise = new Promise((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

test('returns direct channel when direct opens before timeout', async () => {
  const statuses = []
  const directReady = createDeferred()
  const directChannel = { kind: 'direct', readyState: 'open', close() {} }

  const resultPromise = connectTransferChannel({
    relayPolicy: { relayAllowed: true, relayOnly: false },
    directConnect: async () => {
      statuses.push('direct-attempted')
      await directReady.promise
      return directChannel
    },
    relayConnect: async () => {
      throw new Error('relay should not be used')
    },
    directTimeoutMs: 5000,
    onStatusChange: (status) => statuses.push(status),
  })

  directReady.resolve()
  const connected = await resultPromise
  assert.equal(connected.mode, 'direct')
  assert.equal(connected.channel, directChannel)
  assert.deepEqual(statuses.slice(0, 2), ['connecting-direct', 'direct-attempted'])
})

test('falls back to relay when direct times out and relay is allowed', async () => {
  const statuses = []
  const relayChannel = { kind: 'relay', readyState: 'open', close() {} }

  const connected = await connectTransferChannel({
    relayPolicy: { relayAllowed: true, relayOnly: false },
    directConnect: async () => new Promise(() => {}),
    relayConnect: async () => relayChannel,
    directTimeoutMs: 5,
    onStatusChange: (status) => statuses.push(status),
  })

  assert.equal(connected.mode, 'relay')
  assert.equal(connected.channel, relayChannel)
  assert.deepEqual(statuses, ['connecting-direct', 'falling-back-to-relay', 'connected-relay'])
})

test('skips direct when relay_only is true', async () => {
  let directCalled = false
  const relayChannel = { kind: 'relay', readyState: 'open', close() {} }

  const connected = await connectTransferChannel({
    relayPolicy: { relayAllowed: true, relayOnly: true },
    directConnect: async () => {
      directCalled = true
      return null
    },
    relayConnect: async () => relayChannel,
    onStatusChange: () => {},
  })

  assert.equal(directCalled, false)
  assert.equal(connected.mode, 'relay')
})

test('fails closed when direct fails and relay is disallowed', async () => {
  await assert.rejects(() => connectTransferChannel({
    relayPolicy: { relayAllowed: false, relayOnly: false },
    directConnect: async () => { throw new Error('direct failed') },
    relayConnect: async () => ({ kind: 'relay' }),
    directTimeoutMs: 5,
    onStatusChange: () => {},
  }), /direct failed/i)
})

test('decodeRelayPolicyToken extracts the expected static key from the JWT payload', () => {
  const payload = Buffer.from(JSON.stringify({
    expected_agent_static_pub: '04abcd',
    relay_allowed: true,
    relay_only: false,
  })).toString('base64url')
  const token = ['header', payload, 'sig'].join('.')

  assert.deepEqual(decodeRelayPolicyToken(token), {
    expectedStaticPubHex: '04abcd',
    relayAllowed: true,
    relayOnly: false,
  })
})