// signaling-server/web/src/app.test.js
import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  installSessionMessageHandler,
  publishGlobalActions,
  applyConnectionBadge,
  initializeIceConfigTransport,
  assertJoinNotActive,
  __test,
} from './app.js'

test('publishGlobalActions preserves inline button handlers after the move to an ES module', () => {
  const globals = {}
  const join = () => {}
  const submitPassword = () => {}
  const navigateTo = () => {}

  publishGlobalActions(globals, { join, submitPassword, navigateTo })

  assert.equal(globals.join, join)
  assert.equal(globals.submitPassword, submitPassword)
  assert.equal(globals.navigateTo, navigateTo)
})

test('applyConnectionBadge preserves the colored direct/relay badge', () => {
  const statusContainer = {
    classList: {
      removed: [],
      added: [],
      remove(...names) { this.removed.push(...names) },
      add(...names) { this.added.push(...names) },
    },
  }
  const badge = {
    textContent: '',
    classList: {
      removed: [],
      added: [],
      remove(...names) { this.removed.push(...names) },
      add(...names) { this.added.push(...names) },
    },
  }

  applyConnectionBadge({ statusContainer, badge, mode: 'relay' })

  assert.equal(badge.textContent, '● Connected (Relay)')
  assert.deepEqual(statusContainer.classList.removed, ['hidden'])
  assert.deepEqual(badge.classList.removed, ['connection-direct', 'connection-relay'])
  assert.deepEqual(badge.classList.added, ['connection-relay'])
})

test('relay_policy immediately initializes an already-open transfer channel without waiting for a new onopen event', async () => {
  const statuses = []
  const sends = []
  const fakeChannel = {
    readyState: 'open',
    bufferedAmount: 0,
    onopen: null,
    onmessage: null,
    onclose: null,
    send(text) { sends.push(text) },
    sendBinary() {},
    close() {},
  }

  const controller = installSessionMessageHandler({
    updateStatus: (msg) => statuses.push(msg),
    connectTransferChannel: async () => ({ channel: fakeChannel, mode: 'relay' }),
    requestFileList: (channel, path) => channel.send(JSON.stringify({ type: 'list_request', path })),
    applyConnectionBadge: ({ mode }) => statuses.push(`badge:${mode}`),
    decodeRelayPolicyToken: () => ({ relayOnly: true, relayAllowed: true, expectedStaticPubHex: '00' }),
    hideSection: () => {},
  })

  await controller.handleMessage({ type: 'relay_policy', token: 'jwt', relay_allowed: true, relay_only: true })

  assert.equal(statuses.at(-1), 'badge:relay')
  assert.equal(sends[0], JSON.stringify({ type: 'list_request', path: '' }))
})

test('relay_policy with relay_allowed false preserves the quota-exceeded user message on direct failure', async () => {
  const statuses = []
  const controller = installSessionMessageHandler({
    updateStatus: (msg) => statuses.push(msg),
    connectTransferChannel: async ({ onStatusChange }) => {
      onStatusChange('failed')
      throw new Error('Direct unavailable, relay blocked (quota exceeded).')
    },
    requestFileList: () => {},
    applyConnectionBadge: () => {},
    decodeRelayPolicyToken: () => ({ relayOnly: false, relayAllowed: false, expectedStaticPubHex: '00' }),
    hideSection: () => {},
  })

  await assert.rejects(() => controller.handleMessage({
    type: 'relay_policy',
    token: 'jwt',
    relay_allowed: false,
    relay_only: false,
  }))

  assert.match(statuses.at(-1), /quota exceeded/i)
})

test('relay_only ice_config skips direct peer creation and leaves initial knock to the server', () => {
  const sent = []
  let createCalls = 0
  const ws = {
    send(payload) {
      sent.push(payload)
    },
  }

  const peer = initializeIceConfigTransport({
    msg: { type: 'ice_config', ice_servers: [{ urls: ['stun:stun.cloudflare.com:3478'] }], relay_only: true },
    ws,
    createPeerConnection: () => {
      createCalls += 1
      return {}
    },
    onDirectChannel: () => {},
    onDirectFailure: () => {},
  })

  assert.equal(peer, null)
  assert.equal(createCalls, 0)
  assert.deepEqual(sent, [])
})

test('assertJoinNotActive throws loudly when join re-enters on an active socket', () => {
  const logs = []
  assert.throws(
    () => assertJoinNotActive({ readyState: 1 }, (...args) => logs.push(args)),
    /join\(\) re-entered/
  )
  assert.match(logs[0][1], /join\(\) re-entered/)
})

test('assertJoinNotActive allows a closed socket', () => {
  assert.doesNotThrow(() => assertJoinNotActive({ readyState: 3 }, () => {}))
})

test('relay_policy opens the relay channel and requests the root file list', async () => {
  const statuses = []
  const sends = []
  const fakeChannel = {
    readyState: 'open',
    bufferedAmount: 0,
    onopen: null,
    onmessage: null,
    onclose: null,
    send(text) { sends.push(text) },
    sendBinary() {},
    close() {},
  }

  const controller = installSessionMessageHandler({
    updateStatus: (msg) => statuses.push(msg),
    connectTransferChannel: async () => ({ channel: fakeChannel, mode: 'relay' }),
    requestFileList: (channel, path) => channel.send(JSON.stringify({ type: 'list_request', path })),
    applyConnectionBadge: ({ mode }) => statuses.push(`badge:${mode}`),
    decodeRelayPolicyToken: () => ({ relayOnly: true, relayAllowed: true, expectedStaticPubHex: '00' }),
    hideSection: () => {},
  })

  await controller.handleMessage({ type: 'relay_policy', token: 'jwt', relay_allowed: true, relay_only: true })

  assert.equal(statuses.at(-1), 'badge:relay')
  assert.equal(sends[0], JSON.stringify({ type: 'list_request', path: '' }))
})

test('direct failure after quota warning keeps the quota-blocked message', async () => {
  // Set up quota exceeded state
  __test.setQuotaState({ exceeded: true, periodEnd: '2025-12-31T23:59:59Z' })

  const statuses = []
  const controller = installSessionMessageHandler({
    updateStatus: (msg) => statuses.push(msg),
    connectTransferChannel: async ({ onStatusChange }) => {
      onStatusChange('failed')
      throw new Error('Direct unavailable, relay blocked (quota exceeded).')
    },
    requestFileList: () => {},
    applyConnectionBadge: () => {},
    decodeRelayPolicyToken: () => ({ relayOnly: false, relayAllowed: false, expectedStaticPubHex: '00' }),
    hideSection: () => {},
  })

  try {
    await controller.handleMessage({
      type: 'relay_policy',
      token: 'jwt',
      relay_allowed: false,
      relay_only: false,
    })
  } catch (err) {
    // Expected to throw
  }

  // Verify the quota exceeded message is shown
  const lastStatus = statuses.at(-1)
  assert.match(lastStatus, /quota exceeded/i, 'Expected quota exceeded message, got: ' + lastStatus)

  // Reset quota state
  __test.setQuotaState({ exceeded: false, periodEnd: null })
})

function fakeFileItem() {
  const parts = {
    status: { textContent: '', className: '' },
    size: { textContent: '' },
    hash: { textContent: '' },
    progress: {
      classList: {
        added: [],
        add(name) {
          this.added.push(name)
        },
      },
    },
  }

  return {
    classList: {
      added: [],
      removed: [],
      add(name) {
        this.added.push(name)
      },
      remove(...names) {
        this.removed.push(...names)
      },
    },
    querySelector(selector) {
      if (selector === '.file-status') return parts.status
      if (selector === '.file-size') return parts.size
      if (selector === '.file-hash') return parts.hash
      if (selector === '.file-progress') return parts.progress
      throw new Error('unexpected selector: ' + selector)
    },
  }
}

function withMinimalDocument(fn) {
  const originalDocument = globalThis.document
  const elements = new Map([
    ['status', {
      textContent: '',
    }],
    ['join-section', {
      classList: {
        removed: [],
        added: [],
        remove(name) { this.removed.push(name) },
        add(name) { this.added.push(name) },
      },
    }],
    ['connection-status', {
      classList: {
        added: [],
        add(name) { this.added.push(name) },
      },
    }],
    ['connection-type', {
      className: '',
    }],
  ])

  globalThis.document = {
    getElementById(id) {
      const el = elements.get(id)
      if (!el) {
        throw new Error('unexpected element id: ' + id)
      }
      return el
    },
  }

  return Promise.resolve()
    .then(() => fn(elements))
    .finally(() => {
      globalThis.document = originalDocument
    })
}

test('applyFinalDownloadState maps checksum-backed success to intact', () => {
  const fileItem = fakeFileItem()

  __test.applyFinalDownloadState(fileItem, {
    name: 'report.pdf',
    size: 1024,
    sha1: 'abc123',
  }, {
    ok: true,
    code: 'intact',
    statusClass: 'verified',
    statusText: '✓ intact',
    avgBytesPerSecond: 2048,
    computedSha1: 'abc123',
  })

  assert.equal(fileItem.querySelector('.file-status').textContent, '✓ intact')
  assert.equal(fileItem.querySelector('.file-status').className, 'file-status ok')
  assert.equal(fileItem.classList.added.at(-1), 'verified')
  assert.equal(fileItem.querySelector('.file-hash').textContent, 'SHA-1: abc123')
})

test('handleTransferClosure fails an active download before chunk_end', async () => {
  await withMinimalDocument(async () => {
    let failed = false
    __test.setTransferSession({ channel: { readyState: 'closed' }, mode: 'relay' })
    __test.setActiveDownload({
      async failForDisconnect() {
        failed = true
        return { ok: false, code: 'disconnected', statusText: '✗ Connection closed before completion' }
      },
    })

    await __test.handleTransferClosure()

    assert.equal(failed, true)
    assert.equal(__test.getTransferSession().transferChannel, null)
    __test.setActiveDownload(null)
  })
})

test('handleError fails the active download and clears the dead session', async () => {
  await withMinimalDocument(async () => {
    let failedArgs = null
    __test.setTransferSession({ channel: { readyState: 'closed' }, mode: 'relay' })
    __test.setActiveDownload({
      async fail(code, message) {
        failedArgs = { code, message }
        return { ok: false, code, statusText: `✗ ${message}` }
      },
    })

    await __test.handleError({ message: 'agent exploded' })

    assert.deepEqual(failedArgs, { code: 'transfer-error', message: 'agent exploded' })
    assert.equal(__test.getTransferSession().transferChannel, null)
    __test.setActiveDownload(null)
  })
})
