// EXP44 — end-to-end proof of the N5 (SB_BULK_LANES) client wiring in app.js:
// extra `bulk-*` lanes offered by the agent are handed to the same bulk handler
// as the primary lane, and their v2-envelope chunks are reassembled in global
// sequence order through bulkReorder.js.
import { test } from 'node:test'
import assert from 'node:assert/strict'

import { encodeChunkEnvelope, encodeChunkEnvelopeV2 } from './binaryEnvelope.js'
import { __test } from './app.js'

function fakeLane() {
  return {
    readyState: 'open',
    sent: [],
    send(value) { this.sent.push(value) },
    close() {},
    onopen: null,
    onmessage: null,
    onclose: null,
  }
}

/**
 * A lane set shaped like DirectChannelSet: the primary `bulk` lane plus a
 * `subscribeExtraBulkLane` hook that fires for existing and future `bulk-N`
 * lanes. `addExtraBulkLane()` plays the role of the agent's extra DataChannel
 * arriving late.
 */
function fakeLaneSet({ withExtraSubscription = true } = {}) {
  let handler = null
  const extras = []
  const channel = {
    readyState: 'connecting',
    control: fakeLane(),
    media: fakeLane(),
    bulk: fakeLane(),
    close() {},
    addExtraBulkLane() {
      const lane = fakeLane()
      extras.push(lane)
      if (handler) lane.onmessage = handler
      return lane
    },
  }
  if (withExtraSubscription) {
    // Mirrors DirectChannelSet.subscribeExtraBulkLane: install the handler as
    // the onmessage of every current and future extra bulk lane.
    channel.subscribeExtraBulkLane = (next) => {
      handler = next
      for (const lane of extras) lane.onmessage = next
      return () => {
        if (handler === next) handler = null
        for (const lane of extras) if (lane.onmessage === next) lane.onmessage = null
      }
    }
  }
  return channel
}

function attach(channel) {
  __test.setTransferSession({ channels: channel, mode: 'direct' })
  __test.attachTransferChannel({
    channel,
    mode: 'direct',
    updateStatus() {},
    hideSection() {},
    showSection() {},
    requestFileList() {},
    applyConnectionBadge() {},
    handleTransferMessage() {},
    onClose() {},
    isGalleryMode: () => false,
  })
}

function beginDownload(operationId) {
  const appended = []
  __test.setCurrentFile({ name: 'striped.iso', size: 6, binary_envelope: true, operationId })
  __test.setActiveDownload({ async append(bytes) { appended.push([...bytes]) } })
  return appended
}

async function flush() {
  await new Promise((resolve) => setTimeout(resolve, 0))
}

test('extra bulk lanes offered by the agent are wired to the same handler', async () => {
  const channel = fakeLaneSet()
  const appended = beginDownload('90')

  try {
    // The extra lane arrives *after* attachTransferChannel, which is the race
    // the subscription hook exists to close.
    attach(channel)
    const extra = channel.addExtraBulkLane()

    await extra.onmessage({ data: encodeChunkEnvelopeV2('90', 0, 1n, new Uint8Array([9])).buffer })
    await extra.onmessage({ data: encodeChunkEnvelopeV2('90', 0, 0n, new Uint8Array([1, 2])).buffer })
    await flush()

    assert.deepEqual(appended, [[1, 2], [9]], 'lane order is repaired by the global sequence')
    assert.deepEqual(__test.getBulkReorderState(), { operationId: '90', pending: 0, next: 2n })

    // The primary lane keeps its original v1 behaviour in the same session.
    await channel.bulk.onmessage({ data: encodeChunkEnvelope('90', 0, new Uint8Array([7])).buffer })
    await flush()
    assert.deepEqual(appended, [[1, 2], [9], [7]])
  } finally {
    __test.setActiveDownload(null)
    __test.setCurrentFile(null)
    __test.setTransferSession({ channels: null, mode: null })
  }
})

test('a lane set without the extra-lane hook still uses the primary bulk lane', async () => {
  const channel = fakeLaneSet({ withExtraSubscription: false })
  const appended = beginDownload('91')

  try {
    attach(channel)
    assert.equal(typeof channel.subscribeExtraBulkLane, 'undefined')

    await channel.bulk.onmessage({ data: encodeChunkEnvelope('91', 0, new Uint8Array([1])).buffer })
    await flush()
    assert.deepEqual(appended, [[1]])
    assert.equal(__test.getBulkReorderState().operationId, null, 'no reorder state for an unstriped peer')
  } finally {
    __test.setActiveDownload(null)
    __test.setCurrentFile(null)
    __test.setTransferSession({ channels: null, mode: null })
  }
})
