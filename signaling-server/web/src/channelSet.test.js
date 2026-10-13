import assert from 'node:assert/strict'
import test from 'node:test'

import { createChannelSet } from './channelSet.js'

function fakeChannel(label, state = 'open') {
  return {
    label,
    readyState: state,
    sent: [],
    closeCalls: 0,
    onopen: null,
    onmessage: null,
    onclose: null,
    send(value) { this.sent.push(value) },
    close() {
      this.closeCalls++
      this.readyState = 'closed'
    },
    open() {
      this.readyState = 'open'
      this.onopen?.()
    },
    deliver(value) { this.onmessage?.({ data: value }) },
    remoteClose() {
      this.readyState = 'closed'
      this.onclose?.()
    },
  }
}

function attachRequired(set, state = 'open') {
  const channels = {
    bulk: fakeChannel('bulk', state),
    control: fakeChannel('control', state),
    media: fakeChannel('media', state),
  }
  set.attach('bulk', channels.bulk)
  set.attach('control', channels.control)
  set.attach('media', channels.media)
  return channels
}

test('becomes ready only after all three lanes and version acknowledgement', async () => {
  const set = createChannelSet({ handshakeTimeoutMs: 100 })
  const channels = attachRequired(set)

  assert.equal(set.readyState, 'handshaking')
  assert.deepEqual(JSON.parse(channels.control.sent[0]), { type: 'transport_hello', version: 2 })
  channels.control.deliver(JSON.stringify({ type: 'transport_ready', version: 2 }))

  await set.ready
  assert.equal(set.readyState, 'open')
  assert.equal(set.control, channels.control)
  assert.equal(set.media, channels.media)
  assert.equal(set.bulk, channels.bulk)
  assert.equal(channels.control.onmessage, null)
})

test('waits for every attached lane to open before handshaking', async () => {
  const set = createChannelSet({ handshakeTimeoutMs: 100 })
  const channels = attachRequired(set, 'connecting')
  channels.bulk.open()
  channels.control.open()
  assert.equal(set.readyState, 'connecting')
  assert.equal(channels.control.sent.length, 0)

  channels.media.open()
  assert.equal(set.readyState, 'handshaking')
  channels.control.deliver(JSON.stringify({ type: 'transport_ready', version: 2 }))
  await set.ready
})

test('rejects unknown and duplicate labels and closes the session', async (t) => {
  await t.test('unknown label', async () => {
    const set = createChannelSet({ handshakeTimeoutMs: 100 })
    const channel = fakeChannel('other')
    assert.throws(() => set.attach('other', channel), /unknown.*label/i)
    await assert.rejects(set.ready, /unknown.*label/i)
    assert.equal(set.readyState, 'closed')
  })

  await t.test('duplicate label', async () => {
    const set = createChannelSet({ handshakeTimeoutMs: 100 })
    const first = fakeChannel('control')
    const duplicate = fakeChannel('control')
    set.attach('control', first)
    assert.throws(() => set.attach('control', duplicate), /duplicate.*control/i)
    await assert.rejects(set.ready, /duplicate.*control/i)
    assert.equal(first.closeCalls, 1)
  })
})

test('fails when a required lane is still missing at the handshake deadline', async () => {
  const set = createChannelSet({ handshakeTimeoutMs: 5 })
  set.attach('control', fakeChannel('control'))
  set.attach('bulk', fakeChannel('bulk'))

  await assert.rejects(set.ready, /timed out.*media/i)
  assert.equal(set.readyState, 'closed')
})

test('does not start the missing-lane deadline until the first channel arrives', async () => {
  const set = createChannelSet({ handshakeTimeoutMs: 5 })
  let settled = false
  set.ready.then(() => { settled = true }, () => { settled = true })

  await new Promise((resolve) => setTimeout(resolve, 15))
  assert.equal(settled, false)

  set.attach('control', fakeChannel('control'))
  await assert.rejects(set.ready, /timed out.*media.*bulk/i)
})

test('late handshake delivery cannot reopen a channel set after timeout', async () => {
  const set = createChannelSet({ handshakeTimeoutMs: 5 })
  const channels = attachRequired(set, 'connecting')
  channels.bulk.open()
  channels.media.open()
  channels.control.open()
  const staleHandshakeHandler = channels.control.onmessage

  await assert.rejects(set.ready, /acknowledgement not received/i)
  staleHandshakeHandler({ data: JSON.stringify({ type: 'transport_ready', version: 2 }) })

  assert.equal(set.readyState, 'closed')
  assert.equal(channels.control.closeCalls, 1)
})

test('rejects a version mismatch and closes every attached lane', async () => {
  const set = createChannelSet({ handshakeTimeoutMs: 100 })
  const channels = attachRequired(set)
  channels.control.deliver(JSON.stringify({ type: 'transport_ready', version: 1 }))

  await assert.rejects(set.ready, /incompatible.*version/i)
  assert.equal(set.readyState, 'closed')
  assert.deepEqual(Object.values(channels).map((channel) => channel.closeCalls), [1, 1, 1])
})

test('surfaces a connection-scoped handshake error from the responder', async () => {
  const set = createChannelSet({ handshakeTimeoutMs: 100 })
  const channels = attachRequired(set)
  channels.control.deliver(JSON.stringify({
    type: 'error',
    scope: 'connection',
    message: 'incompatible transport version: 1',
  }))

  await assert.rejects(set.ready, /incompatible transport version: 1/i)
  assert.equal(set.readyState, 'closed')
})

test('rejects application messages before handshake readiness', async () => {
  const set = createChannelSet({ handshakeTimeoutMs: 100 })
  const channels = attachRequired(set)
  channels.control.deliver(JSON.stringify({ type: 'file_list' }))

  await assert.rejects(set.ready, /expected transport_ready/i)
  assert.equal(set.readyState, 'closed')
})

test('closing any required lane closes the complete set exactly once', async () => {
  const set = createChannelSet({ handshakeTimeoutMs: 100 })
  const channels = attachRequired(set)
  let closeCalls = 0
  set.onclose = () => { closeCalls++ }
  channels.control.deliver(JSON.stringify({ type: 'transport_ready', version: 2 }))
  await set.ready

  channels.media.remoteClose()
  channels.bulk.remoteClose()

  assert.equal(set.readyState, 'closed')
  assert.equal(closeCalls, 1)
  assert.equal(channels.control.closeCalls, 1)
  assert.equal(channels.bulk.closeCalls, 1)
})

// --- N5 (SB_BULK_LANES) extra bulk lanes -------------------------------------

test('a peer that offers no extra bulk lanes reports exactly the primary lane', async () => {
  const set = createChannelSet({ handshakeTimeoutMs: 100 })
  const channels = attachRequired(set)
  channels.control.deliver(JSON.stringify({ type: 'transport_ready', version: 2 }))
  await set.ready

  assert.deepEqual(set.bulkLanes, [channels.bulk])
})

test('accepts N5 extra bulk lanes without making them required', async () => {
  const set = createChannelSet({ handshakeTimeoutMs: 100 })
  const channels = attachRequired(set)
  const notified = []
  set.onbulklane = (channel, label) => notified.push([label, channel])

  const extra = fakeChannel('bulk-1')
  set.attach('bulk-1', extra)

  assert.deepEqual(notified, [['bulk-1', extra]])
  assert.equal(set.readyState, 'handshaking', 'an extra lane does not block the handshake')

  channels.control.deliver(JSON.stringify({ type: 'transport_ready', version: 2 }))
  await set.ready
  assert.deepEqual(set.bulkLanes, [channels.bulk, extra])

  // A lane that attaches after readiness is still announced, and still does not
  // disturb the resolved set.
  const late = fakeChannel('bulk-2')
  set.attach('bulk-2', late)
  assert.deepEqual(notified.map(([label]) => label), ['bulk-1', 'bulk-2'])
  assert.deepEqual(set.bulkLanes, [channels.bulk, extra, late])
  assert.equal(set.readyState, 'open')
})

test('sorts extra bulk lanes by numeric suffix', async () => {
  const set = createChannelSet({ handshakeTimeoutMs: 100 })
  const channels = attachRequired(set)
  const ten = fakeChannel('bulk-10')
  const two = fakeChannel('bulk-2')
  const one = fakeChannel('bulk-1')
  set.attach('bulk-10', ten)
  set.attach('bulk-2', two)
  set.attach('bulk-1', one)

  assert.deepEqual(set.bulkLanes, [channels.bulk, one, two, ten])
  channels.control.deliver(JSON.stringify({ type: 'transport_ready', version: 2 }))
  await set.ready
  assert.deepEqual(set.bulkLanes, [channels.bulk, one, two, ten])
})

test('still rejects labels that are neither required nor an N5 bulk lane', async () => {
  for (const label of ['bulk-0', 'bulk-', 'bulk-x', 'bulkFoo', 'data']) {
    const set = createChannelSet({ handshakeTimeoutMs: 100 })
    assert.throws(() => set.attach(label, fakeChannel(label)), /unknown.*label/i, label)
    await assert.rejects(set.ready, /unknown.*label/i)
  }
})

test('an extra bulk lane closing fails the set and closes every lane', async () => {
  const set = createChannelSet({ handshakeTimeoutMs: 100 })
  const channels = attachRequired(set)
  const extra = fakeChannel('bulk-1')
  set.attach('bulk-1', extra)
  channels.control.deliver(JSON.stringify({ type: 'transport_ready', version: 2 }))
  await set.ready

  let closeCalls = 0
  set.onclose = () => { closeCalls++ }
  extra.remoteClose()

  assert.equal(set.readyState, 'closed')
  assert.equal(closeCalls, 1)
  assert.equal(channels.control.closeCalls, 1)
  assert.equal(channels.bulk.closeCalls, 1)
})

test('a duplicate extra bulk lane label is rejected', async () => {
  const set = createChannelSet({ handshakeTimeoutMs: 100 })
  const first = fakeChannel('bulk-1')
  set.attach('bulk-1', first)
  assert.throws(() => set.attach('bulk-1', fakeChannel('bulk-1')), /duplicate.*bulk-1/i)
  await assert.rejects(set.ready, /duplicate.*bulk-1/i)
  assert.equal(first.closeCalls, 1)
})
