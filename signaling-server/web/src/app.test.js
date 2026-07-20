// signaling-server/web/src/app.test.js
import { test } from 'node:test'
import assert from 'node:assert/strict'
import vm from 'node:vm'
import { encodeChunkEnvelope, encodeThumbnailEnvelope } from './binaryEnvelope.js'
import {
  installSessionMessageHandler,
  publishGlobalActions,
  applyConnectionBadge,
  initializeIceConfigTransport,
  cancelDirectTransport,
  assertJoinNotActive,
  isTransferChannelActive,
  detachBrowserSignalingSocket,
  handleBrowserSignalingClose,
  __test,
} from './app.js'

function fakeLaneSet() {
  const endpoint = () => ({ readyState: 'open', sent: [], send(value) { this.sent.push(value) }, close() {} })
  return { readyState: 'open', control: endpoint(), media: endpoint(), bulk: endpoint(), closeCalls: 0, close() { this.closeCalls += 1 } }
}

function deferred() {
  let resolve
  let reject
  const promise = new Promise((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

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

test('detectPathMode returns gallery mode for /i links and file mode for /s links', () => {
  assert.deepEqual(__test.detectPathMode('/i/ffSw63qn'), { mode: 'gallery', code: 'ffSw63qn' })
  assert.deepEqual(__test.detectPathMode('/s/a3f9k2xp'), { mode: 'files', code: 'a3f9k2xp' })
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

test('stale transfer channel close does not reset a newer relay session', async () => {
  const statuses = []
  const sends = []
  const channels = [
    {
      readyState: 'open',
      bufferedAmount: 0,
      onopen: null,
      onmessage: null,
      onclose: null,
      send(text) { sends.push(['first', text]) },
      close() {},
    },
    {
      readyState: 'open',
      bufferedAmount: 0,
      onopen: null,
      onmessage: null,
      onclose: null,
      send(text) { sends.push(['second', text]) },
      close() {},
    },
  ]
  let connectCount = 0
  const controller = installSessionMessageHandler({
    updateStatus: (msg) => statuses.push(msg),
    connectTransferChannel: async () => ({ channel: channels[connectCount++], mode: 'relay' }),
    requestFileList: (channel, path) => channel.send(JSON.stringify({ type: 'list_request', path })),
    applyConnectionBadge: ({ mode }) => statuses.push(`badge:${mode}`),
    decodeRelayPolicyToken: () => ({ relayOnly: true, relayAllowed: true, expectedStaticPubHex: '00' }),
    hideSection: () => {},
  })

  await controller.handleMessage({ type: 'relay_policy', token: 'jwt', relay_allowed: true, relay_only: true })
  await controller.handleMessage({ type: 'relay_policy', token: 'jwt', relay_allowed: true, relay_only: true })

  statuses.length = 0
  channels[0].readyState = 'closed'
  channels[0].onclose()
  assert.deepEqual(statuses, [])
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

test('direct transport resolves only after all labeled lanes and version handshake', async () => {
  const sent = []
  const peer = { connectionState: 'new' }
  const resolved = []
  initializeIceConfigTransport({
    msg: { ice_servers: [], relay_only: false },
    ws: { send: (payload) => sent.push(JSON.parse(payload)) },
    createPeerConnection: () => peer,
    onDirectChannel: (set) => resolved.push(set),
    onDirectFailure: () => {},
  })

  const makeChannel = (label) => ({
    label,
    readyState: 'connecting',
    bufferedAmount: 0,
    sent: [],
    send(value) { this.sent.push(value) },
    close() { this.readyState = 'closed' },
  })
  const bulk = makeChannel('bulk')
  const control = makeChannel('control')
  const media = makeChannel('media')
  peer.ondatachannel({ channel: bulk })
  peer.ondatachannel({ channel: control })
  peer.ondatachannel({ channel: media })
  assert.equal(resolved.length, 0)

  for (const channel of [bulk, media, control]) {
    channel.readyState = 'open'
    channel.onopen()
  }
  await Promise.resolve()
  assert.equal(resolved.length, 0)
  control.onmessage({ data: JSON.stringify({ type: 'transport_ready', version: 2 }) })
  await Promise.resolve()
  await Promise.resolve()

  assert.equal(resolved.length, 1)
  assert.equal(resolved[0].readyState, 'open')
  assert.deepEqual(sent, [{ type: 'knock' }])
})

test('relay selection cancels direct resources and ignores late direct success and failure', async () => {
  let peerCloseCalls = 0
  const peer = {
    connectionState: 'new',
    close() {
      peerCloseCalls += 1
      this.connectionState = 'closed'
    },
  }
  let readyCalls = 0
  let failureCalls = 0
  initializeIceConfigTransport({
    msg: { ice_servers: [], relay_only: false },
    ws: { send() {} },
    createPeerConnection: () => peer,
    onDirectChannel: () => { readyCalls += 1 },
    onDirectFailure: () => { failureCalls += 1 },
  })

  const makeChannel = (label) => ({
    label,
    readyState: 'connecting',
    bufferedAmount: 0,
    closeCalls: 0,
    send() {},
    close() {
      this.closeCalls += 1
      this.readyState = 'closed'
    },
  })
  const channels = ['control', 'media', 'bulk'].map(makeChannel)
  channels.forEach((channel) => peer.ondatachannel({ channel }))
  channels.forEach((channel) => {
    channel.readyState = 'open'
    channel.onopen()
  })
  const staleReady = channels[0].onmessage

  cancelDirectTransport(peer, 'relay selected')
  cancelDirectTransport(peer, 'duplicate relay selection')
  staleReady({ data: JSON.stringify({ type: 'transport_ready', version: 2 }) })
  peer.onconnectionstatechange()
  await Promise.resolve()
  await Promise.resolve()

  assert.equal(readyCalls, 0)
  assert.equal(failureCalls, 0)
  assert.equal(peerCloseCalls, 1)
  assert.ok(channels.every((channel) => channel.closeCalls === 1))
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

test('isTransferChannelActive only treats open/connecting channels as active', () => {
  assert.equal(isTransferChannelActive(null), false)
  assert.equal(isTransferChannelActive({ readyState: 'closed' }), false)
  assert.equal(isTransferChannelActive({ readyState: 'closing' }), false)
  assert.equal(isTransferChannelActive({ readyState: 'connecting' }), true)
  assert.equal(isTransferChannelActive({ readyState: 'open' }), true)
})

test('detachBrowserSignalingSocket closes an open signaling socket and installs ignore handlers', () => {
  const logs = []
  const socket = {
    readyState: 1,
    onmessage: () => {},
    onerror: null,
    onclose: null,
    closeCalls: [],
    close(code, reason) {
      this.closeCalls.push({ code, reason })
    },
  }

  const detached = detachBrowserSignalingSocket(socket, (...args) => logs.push(args))

  assert.equal(detached, null)
  assert.equal(socket.onmessage, null)
  assert.equal(typeof socket.onerror, 'function')
  assert.equal(typeof socket.onclose, 'function')
  assert.deepEqual(socket.closeCalls, [{ code: 1000, reason: 'transfer channel active' }])
  assert.match(logs[0][0], /detaching browser signaling WebSocket/)
})

test('handleBrowserSignalingClose ignores signaling closure once transfer channel is active', () => {
  let peerClosed = false
  let resetCalled = false
  const logs = []

  const handled = handleBrowserSignalingClose({
    event: { code: 1006, reason: 'proxy idle timeout', wasClean: false },
    transferChannel: { readyState: 'open' },
    pc: { close() { peerClosed = true } },
    resetUI: () => { resetCalled = true },
    log: (...args) => logs.push(args),
  })

  assert.equal(handled, false)
  assert.equal(peerClosed, false)
  assert.equal(resetCalled, false)
  assert.match(logs[0][0], /ignoring browser signaling WebSocket close/)
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

test('relay_policy in gallery mode does not request the file list on channel open', async () => {
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
    isGalleryMode: () => true,
  })

  await controller.handleMessage({ type: 'relay_policy', token: 'jwt', relay_allowed: true, relay_only: true })

  assert.ok(statuses.includes('badge:relay'))
  assert.deepEqual(sends, [])
  assert.equal(statuses.at(-1), 'Loading gallery...')
})

test('password_required shows the Immich password form', async () => {
  const statuses = []
  const shown = []
  const hidden = []

  const controller = installSessionMessageHandler({
    updateStatus: (msg) => statuses.push(msg),
    connectTransferChannel: async () => { throw new Error('unexpected connect') },
    requestFileList: () => {},
    applyConnectionBadge: () => {},
    decodeRelayPolicyToken: () => ({}),
    hideSection: (id) => hidden.push(id),
    showSection: (id) => shown.push(id),
  })

  await controller.handleMessage({ type: 'password_required' })

  assert.deepEqual(hidden, ['join-section'])
  assert.deepEqual(shown, ['password-section'])
  assert.equal(statuses.at(-1), 'This Immich share is password protected.')
})

test('auth_fail keeps the password form visible with remaining attempts', async () => {
  const statuses = []
  const shown = []

  const controller = installSessionMessageHandler({
    updateStatus: (msg) => statuses.push(msg),
    connectTransferChannel: async () => { throw new Error('unexpected connect') },
    requestFileList: () => {},
    applyConnectionBadge: () => {},
    decodeRelayPolicyToken: () => ({}),
    hideSection: () => {},
    showSection: (id) => shown.push(id),
  })

  await controller.handleMessage({ type: 'auth_fail', attempts_remaining: 2 })

  assert.deepEqual(shown, ['password-section'])
  assert.equal(statuses.at(-1), 'Incorrect password. 2 attempts remaining.')
})

test('submitPassword in gallery mode sends password_submit instead of join', () => {
  const sent = []
  const originalDocument = globalThis.document
  globalThis.document = {
    getElementById(id) {
      assert.equal(id, 'password-input')
      return { value: 'secret' }
    },
  }

  try {
    __test.setGallerySession({
      galleryMode: true,
      sessionCode: 'immich-code',
      socket: { send: (payload) => sent.push(JSON.parse(payload)) },
    })

    __test.submitPassword()

    assert.deepEqual(sent, [{ type: 'password_submit', code: 'immich-code', password: 'secret' }])
  } finally {
    globalThis.document = originalDocument
    __test.setGallerySession({ galleryMode: false, sessionCode: '', socket: null })
  }
})

test('handleTransferMessage routes thumbnail_list to the gallery controller', async () => {
  const lists = []
  __test.setGalleryController({
    handleThumbnailList(msg) {
      lists.push(msg)
    },
  })

  await __test.handleTransferMessage({
    data: JSON.stringify({ type: 'thumbnail_list', albumName: 'Summer', items: [] }),
  })

  assert.deepEqual(lists, [{ type: 'thumbnail_list', albumName: 'Summer', items: [] }])
  __test.setGalleryController(null)
})

test('thumbnail_complete clears the gallery loading status', async () => {
  const completed = []
  const originalDocument = globalThis.document
  const status = { textContent: 'Loading gallery...' }
  globalThis.document = {
    getElementById(id) {
      assert.equal(id, 'status')
      return status
    },
  }
  __test.setGalleryController({
    handleThumbnailComplete(msg) {
      completed.push(msg)
    },
  })

  try {
    await __test.handleTransferMessage({ data: JSON.stringify({ type: 'thumbnail_complete', sent: 306, failed: 1 }) })

    assert.deepEqual(completed, [{ type: 'thumbnail_complete', sent: 306, failed: 1 }])
    assert.equal(status.textContent, '')
  } finally {
    globalThis.document = originalDocument
    __test.setGalleryController(null)
  }
})

test('gallery mode recreates its controller after reset before handling thumbnail_list', async () => {
  const originalDocument = globalThis.document
  const elements = new Map()
  const makeElement = () => ({
    innerHTML: '',
    textContent: '',
    value: '',
    disabled: false,
    className: '',
    classList: {
      add() {},
      remove() {},
    },
    addEventListener() {},
    querySelector() { return null },
  })

  for (const id of [
    'join-section',
    'password-section',
    'file-list',
    'gallery-section',
    'connection-status',
    'connection-type',
    'breadcrumb',
    'password-error',
    'password-input',
    'download-warning',
    'gallery-root',
  ]) {
    elements.set(id, makeElement())
  }

  globalThis.document = {
    getElementById(id) {
      const el = elements.get(id)
      if (!el) throw new Error('unexpected element id: ' + id)
      return el
    },
    querySelector(selector) {
      assert.equal(selector, '#password-section button')
      return makeElement()
    },
  }

  try {
    let destroyed = false
    __test.setGallerySession({ galleryMode: true, sessionCode: 'immich-code', socket: null })
    __test.setGalleryController({ destroy() { destroyed = true } })

    await __test.handleTransferClosure()
    await __test.handleTransferMessage({
      data: JSON.stringify({
        type: 'thumbnail_list',
        albumName: 'Summer',
        items: [{ id: 'asset-1', name: 'photo.jpg', mimeType: 'image/jpeg' }],
      }),
    })

    assert.equal(destroyed, true)
    assert.match(elements.get('gallery-root').innerHTML, /Summer/)
    assert.match(elements.get('gallery-root').innerHTML, /photo.jpg/)
  } finally {
    globalThis.document = originalDocument
    __test.setGallerySession({ galleryMode: false, sessionCode: '', socket: null })
    __test.setGalleryController(null)
  }
})

test('handleTransferMessage routes thumbnail envelopes before file chunks', async () => {
  const thumbs = []
  __test.setGalleryController({
    handleThumbnailData(index, payload) {
      thumbs.push({ index, payload: [...payload] })
    },
  })

  await __test.handleTransferMessage({ data: new Uint8Array([0x11, 0, 4, 9, 8]).buffer })

  assert.deepEqual(thumbs, [{ index: 4, payload: [9, 8] }])
  __test.setGalleryController(null)
})

test('handleTransferMessage accepts a binary frame from another JavaScript realm', async () => {
  const thumbnails = []
  const foreignBuffer = vm.runInNewContext('new Uint8Array([0x11, 0, 4, 9, 8]).buffer')
  assert.equal(foreignBuffer instanceof ArrayBuffer, false)

  __test.setGalleryController({
    handleThumbnailData(index, payload) {
      thumbnails.push({ index, payload: [...payload] })
    },
  })

  try {
    await __test.handleTransferMessage({ data: foreignBuffer })
    assert.deepEqual(thumbnails, [{ index: 4, payload: [9, 8] }])
  } finally {
    __test.setGalleryController(null)
  }
})

test('handleTransferMessage rejects an unclaimed malformed binary file frame', async () => {
  __test.setGalleryController({})

  try {
    await assert.rejects(() => __test.handleTransferMessage({ data: new Uint8Array([0x10, 9, 8]).buffer }), /too short/i)
  } finally {
    __test.setGalleryController(null)
  }
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
    ['download-warning', { textContent: '', className: '' }],
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
    querySelector() { return null },
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

test('handleTransferMessage appends typed file chunk payload only', async () => {
  const appended = []
  __test.setCurrentFile({ name: 'photo.jpg', size: 3, binary_envelope: true, operationId: '42' })
  __test.setActiveDownload({
    async append(bytes) {
      appended.push([...bytes])
    },
  })

  await __test.handleBulkMessage({ data: encodeChunkEnvelope('42', 0, new Uint8Array([1, 2, 3])).buffer })

  assert.deepEqual(appended, [[1, 2, 3]])
  __test.setActiveDownload(null)
  __test.setCurrentFile(null)
})

test('bulk lane rejects a legacy short 0x10 frame', async () => {
  await assert.rejects(() => __test.handleBulkMessage({ data: new Uint8Array([0x10, 8, 7]).buffer }), /too short/i)
})

test('bulk lane rejects thumbnail frames', async () => {
  await assert.rejects(() => __test.handleBulkMessage({ data: encodeThumbnailEnvelope(8, new Uint8Array([7])).buffer }), /non-chunk/i)
})

test('bulk lane rejects untyped legacy raw file chunks', async () => {
  await assert.rejects(() => __test.handleBulkMessage({ data: new Uint8Array([9, 8, 7]).buffer }), /unknown binary frame kind/i)
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

test('requestGalleryAsset ignores repeated requests while one is pending', async () => {
  await withMinimalDocument(async (elements) => {
    const sends = []
    __test.setTransferSession({ channel: { send: (text) => sends.push(text) }, mode: 'relay' })

    __test.requestGalleryAsset('asset-1')
    __test.requestGalleryAsset('asset-2')

    assert.deepEqual(sends.map((text) => JSON.parse(text).id), ['asset-1'])
    assert.equal(elements.get('status').textContent, 'Download in progress, please wait')

    await __test.handleError({ message: 'not found' })
    __test.requestGalleryAsset('asset-3')

    assert.deepEqual(sends.map((text) => JSON.parse(text).id), ['asset-1', 'asset-3'])
    await __test.handleError({ message: 'cleanup' })
    __test.setTransferSession({ channel: null, mode: null })
  })
})

test('requestAlbumDownload sends once, marks starting, and blocks every additional bulk request locally', async () => {
  await withMinimalDocument(async (elements) => {
    const channels = fakeLaneSet()
    const states = []
    __test.setTransferSession({ channels, mode: 'relay' })
    __test.setGalleryController({ setAlbumDownloadState: (state) => states.push(state) })

    __test.requestAlbumDownload()
    __test.requestAlbumDownload()
    __test.requestGalleryAsset('asset-1')
    __test.requestFile('ordinary.bin')

    assert.equal(channels.control.sent.length, 1)
    const request = JSON.parse(channels.control.sent[0])
    assert.deepEqual({ ...request, request_id: '<dynamic>' }, {
      type: 'album_download_request', request_id: '<dynamic>',
    })
    assert.match(request.request_id, /^bulk-/)
    assert.deepEqual(states, [{ phase: 'starting' }])
    assert.equal(elements.get('status').textContent, 'Download in progress, please wait')

    await __test.handleError({ type: 'error', scope: 'bulk', request_id: request.request_id, message: 'album unavailable' })
    assert.deepEqual(states.at(-1), { phase: 'failed' })
    __test.setGalleryController(null)
    __test.setTransferSession({ channels: null, mode: null })
  })
})

test('album parts use exact chunk_end sizes and acknowledge only terminal sink success', async () => {
  await withMinimalDocument(async () => {
    const channels = fakeLaneSet()
    const states = []
    const supportHeaders = []
    const pipelines = []
    __test.setTransferSession({ channels, mode: 'direct' })
    __test.setGalleryController({ setAlbumDownloadState: (state) => states.push(state) })
    __test.setDownloadTestDependencies({
      getSupport: async (header) => {
        supportHeaders.push(header)
        return { mode: 'blob', warning: null }
      },
      createPipeline: async (options) => {
        const record = { options, chunks: [], completions: [] }
        pipelines.push(record)
        return {
          append: async (bytes) => record.chunks.push([...bytes]),
          complete: async (completion) => record.completions.push(completion),
        }
      },
    })

    __test.requestAlbumDownload()
    const request = JSON.parse(channels.control.sent[0])
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'file_header', name: 'Summer+1.zip', size: 0, estimated_size: 4,
      mimeType: 'application/zip', scope: 'bulk', binary_envelope: true,
      operation_id: '42', request_id: request.request_id, batch_id: request.request_id,
      part_index: 1, part_count: 2,
    }) })
    await __test.handleBulkMessage({ data: encodeChunkEnvelope('42', 0, new Uint8Array([1, 2, 3])).buffer })
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'chunk_end', operation_id: '42', request_id: request.request_id, bytes_sent: '3',
    }) })

    assert.equal(supportHeaders[0].size, 4, 'estimated size selects browser capability/UX')
    assert.equal(pipelines[0].options.header.size, 0, 'the pipeline retains the unknown wire size')
    assert.deepEqual(pipelines[0].chunks, [[1, 2, 3]])
    assert.deepEqual(pipelines[0].completions, [{ expectedSize: 3 }])
    assert.equal(channels.control.sent.length, 1, 'chunk_end alone cannot acknowledge an unfinished sink')

    pipelines[0].options.onTerminalState({ ok: true, code: 'complete', statusClass: 'done', statusText: '✓ saved' })
    assert.deepEqual(JSON.parse(channels.control.sent[1]), {
      type: 'album_archive_ack', batch_id: request.request_id, part_index: 1, operation_id: '42', ok: true,
    })

    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'file_header', name: 'Summer+2.zip', size: 0, estimated_size: 9,
      mimeType: 'application/zip', scope: 'bulk', binary_envelope: true,
      operation_id: '43', request_id: request.request_id, batch_id: request.request_id,
      part_index: 2, part_count: 2,
    }) })
    await __test.handleBulkMessage({ data: encodeChunkEnvelope('43', 0, new Uint8Array([4, 5, 6, 7])).buffer })
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'chunk_end', operation_id: '43', request_id: request.request_id, bytes_sent: '4',
    }) })
    pipelines[1].options.onTerminalState({ ok: true, code: 'complete', statusClass: 'done', statusText: '✓ saved' })

    assert.deepEqual(JSON.parse(channels.control.sent[2]), {
      type: 'album_archive_ack', batch_id: request.request_id, part_index: 2, operation_id: '43', ok: true,
    })
    assert.notDeepEqual(states.at(-1), { phase: 'complete' }, 'part 2 sink success still waits for batch completion')

    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'album_download_complete', batch_id: request.request_id, part_count: 2, bytes_sent: '7',
    }) })
    assert.deepEqual(states.at(-1), { phase: 'complete' })

    __test.setDownloadTestDependencies()
    __test.setGalleryController(null)
    __test.setTransferSession({ channels: null, mode: null })
  })
})

test('failed album sink sends one negative acknowledgement and rejects later parts', async () => {
  await withMinimalDocument(async () => {
    const channels = fakeLaneSet()
    const states = []
    const pipelines = []
    __test.setTransferSession({ channels, mode: 'relay' })
    __test.setGalleryController({ setAlbumDownloadState: (state) => states.push(state) })
    __test.setDownloadTestDependencies({
      getSupport: async () => ({ mode: 'blob', warning: null }),
      createPipeline: async (options) => {
        pipelines.push(options)
        return { append: async () => {}, complete: async () => {} }
      },
    })

    __test.requestAlbumDownload()
    const request = JSON.parse(channels.control.sent[0])
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'file_header', name: 'Summer+1.zip', size: 0, estimated_size: 1,
      operation_id: '52', request_id: request.request_id, batch_id: request.request_id,
      part_index: 1, part_count: 2,
    }) })
    await __test.handleBulkMessage({ data: encodeChunkEnvelope('52', 0, new Uint8Array([1])).buffer })
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'chunk_end', operation_id: '52', request_id: request.request_id, bytes_sent: '1',
    }) })
    pipelines[0].onTerminalState({ ok: false, code: 'write-failed', statusClass: 'failed', statusText: '✗ failed' })
    pipelines[0].onTerminalState({ ok: false, code: 'write-failed', statusClass: 'failed', statusText: '✗ failed' })

    assert.deepEqual(JSON.parse(channels.control.sent[1]), {
      type: 'album_archive_ack', batch_id: request.request_id, part_index: 1, operation_id: '52', ok: false,
    })
    assert.equal(channels.control.sent.length, 2, 'a terminal callback cannot duplicate its acknowledgement')
    assert.deepEqual(states.at(-1), { phase: 'failed' })

    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'file_header', name: 'Summer+2.zip', size: 0, estimated_size: 1,
      operation_id: '53', request_id: request.request_id, batch_id: request.request_id,
      part_index: 2, part_count: 2,
    }) })
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'album_download_complete', batch_id: request.request_id, part_count: 2, bytes_sent: '2',
    }) })
    assert.equal(pipelines.length, 1)
    assert.deepEqual(states.at(-1), { phase: 'failed' })

    __test.setDownloadTestDependencies()
    __test.setGalleryController(null)
    __test.setTransferSession({ channels: null, mode: null })
  })
})

test('request-scoped album error between acknowledged parts fails the batch', async () => {
  await withMinimalDocument(async () => {
    const channels = fakeLaneSet()
    const states = []
    let pipelineOptions
    __test.setTransferSession({ channels, mode: 'relay' })
    __test.setGalleryController({ setAlbumDownloadState: (state) => states.push(state) })
    __test.setDownloadTestDependencies({
      getSupport: async () => ({ mode: 'blob', warning: null }),
      createPipeline: async (options) => {
        pipelineOptions = options
        return { append: async () => {}, complete: async () => {} }
      },
    })

    __test.requestAlbumDownload()
    const request = JSON.parse(channels.control.sent[0])
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'file_header', name: 'Summer+1.zip', size: 0, estimated_size: 1,
      operation_id: '54', request_id: request.request_id, batch_id: request.request_id,
      part_index: 1, part_count: 2,
    }) })
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'chunk_end', operation_id: '54', request_id: request.request_id, bytes_sent: '0',
    }) })
    pipelineOptions.onTerminalState({ ok: true, code: 'complete', statusClass: 'done', statusText: '✓ saved' })

    await __test.handleError({
      type: 'error', scope: 'bulk', request_id: request.request_id, message: 'next archive failed',
    })

    assert.deepEqual(states.at(-1), { phase: 'failed' })
    __test.setDownloadTestDependencies()
    __test.setGalleryController(null)
    __test.setTransferSession({ channels: null, mode: null })
  })
})

test('ordinary file terminal success sends no album acknowledgement', async () => {
  await withMinimalDocument(async () => {
    const channels = fakeLaneSet()
    let pipelineOptions
    let completion
    __test.setTransferSession({ channels, mode: 'relay' })
    __test.setDownloadTestDependencies({
      getSupport: async () => ({ mode: 'blob', warning: null }),
      createPipeline: async (options) => {
        pipelineOptions = options
        return { append: async () => {}, complete: async (value) => { completion = value } }
      },
    })

    __test.requestFile('ordinary.bin')
    const request = JSON.parse(channels.control.sent[0])
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'file_header', name: 'ordinary.bin', size: 3,
      operation_id: '55', request_id: request.request_id,
    }) })
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'chunk_end', operation_id: '55', request_id: request.request_id, bytes_sent: '0',
    }) })
    pipelineOptions.onTerminalState({ ok: true, code: 'complete', statusClass: 'done', statusText: '✓ saved' })

    assert.equal(channels.control.sent.length, 1)
    assert.deepEqual(completion, { expectedSize: 3 }, 'ordinary files retain the authoritative header size')
    __test.setDownloadTestDependencies()
    __test.setTransferSession({ channels: null, mode: null })
  })
})

test('album acknowledgement timeout after local finalization fails the active batch', async () => {
  await withMinimalDocument(async () => {
    const channels = fakeLaneSet()
    const states = []
    let pipelineOptions
    __test.setTransferSession({ channels, mode: 'relay' })
    __test.setGalleryController({ setAlbumDownloadState: (state) => states.push(state) })
    __test.setDownloadTestDependencies({
      getSupport: async () => ({ mode: 'blob', warning: null }),
      createPipeline: async (options) => {
        pipelineOptions = options
        return { append: async () => {}, complete: async () => {} }
      },
    })

    __test.requestAlbumDownload()
    const request = JSON.parse(channels.control.sent[0])
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'file_header', name: 'Summer.zip', size: 0, estimated_size: 1,
      operation_id: '57', request_id: request.request_id, batch_id: request.request_id,
      part_index: 1, part_count: 1,
    }) })
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'chunk_end', operation_id: '57', request_id: request.request_id, bytes_sent: '0',
    }) })
    pipelineOptions.onTerminalState({ ok: true, code: 'complete', statusClass: 'done', statusText: '✓ saved' })

    await __test.handleError({
      type: 'error', scope: 'bulk', operation_id: '57', request_id: request.request_id,
      message: 'album archive acknowledgement timed out',
    })

    assert.deepEqual(states.at(-1), { phase: 'failed' })
    __test.setDownloadTestDependencies()
    __test.setGalleryController(null)
    __test.setTransferSession({ channels: null, mode: null })
  })
})

test('album chunk_end rejects a byte count outside the browser safe integer range', async () => {
  await withMinimalDocument(async () => {
    const channels = fakeLaneSet()
    __test.setTransferSession({ channels, mode: 'relay' })
    __test.setGalleryController({ setAlbumDownloadState() {} })
    __test.setDownloadTestDependencies({
      getSupport: async () => ({ mode: 'blob', warning: null }),
      createPipeline: async () => ({ append: async () => {}, complete: async () => {} }),
    })

    __test.requestAlbumDownload()
    const request = JSON.parse(channels.control.sent[0])
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'file_header', name: 'Summer.zip', size: 0, estimated_size: 1,
      operation_id: '56', request_id: request.request_id, batch_id: request.request_id,
      part_index: 1, part_count: 1,
    }) })

    await assert.rejects(() => __test.handleControlMessage({ data: JSON.stringify({
      type: 'chunk_end', operation_id: '56', request_id: request.request_id,
      bytes_sent: '9007199254740992',
    }) }), /safe integer range/i)
    assert.equal(channels.closeCalls, 1)

    __test.setDownloadTestDependencies()
    __test.setGalleryController(null)
    __test.setTransferSession({ channels: null, mode: null })
  })
})

test('ordinary chunk_end keeps BigInt correlation beyond the Number safe integer boundary', async () => {
  await withMinimalDocument(async () => {
    const channels = fakeLaneSet()
    let completeCalls = 0
    __test.setTransferSession({ channels, mode: 'relay' })
    __test.setDownloadTestDependencies({
      getSupport: async () => ({ mode: 'blob', warning: null }),
      createPipeline: async () => ({
        append: async () => {},
        complete: async () => { completeCalls += 1 },
      }),
    })

    __test.requestFile('large.bin')
    const request = JSON.parse(channels.control.sent[0])
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'file_header', name: 'large.bin', size: 1,
      operation_id: '58', request_id: request.request_id,
    }) })

    await assert.doesNotReject(() => __test.handleControlMessage({ data: JSON.stringify({
      type: 'chunk_end', operation_id: '58', request_id: request.request_id,
      bytes_sent: '9007199254740992',
    }) }))
    assert.equal(channels.closeCalls, 0)
    assert.equal(completeCalls, 0, 'BigInt correlation still waits for the declared bytes')
    assert.equal(__test.getCurrentFile()?.operationId, '58')

    __test.setDownloadTestDependencies()
    __test.setTransferSession({ channels: null, mode: null })
  })
})

test('media remains independently routable during an album batch', async () => {
  await withMinimalDocument(async () => {
    const channels = fakeLaneSet()
    const previews = []
    const bulkChunks = []
    let albumPipeline
    __test.setTransferSession({ channels, mode: 'direct' })
    __test.setGalleryController({
      setAlbumDownloadState() {},
      handlePreviewData: (id, bytes) => previews.push([id, [...bytes]]),
    })
    __test.setDownloadTestDependencies({
      getSupport: async () => ({ mode: 'blob', warning: null }),
      createPipeline: async (options) => {
        albumPipeline = options
        return { append: async (bytes) => bulkChunks.push([...bytes]), complete: async () => {} }
      },
    })

    __test.requestAlbumDownload()
    const albumRequest = JSON.parse(channels.control.sent[0])
    __test.requestGalleryPreview('asset-1')
    const previewRequest = JSON.parse(channels.control.sent[1])
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'file_header', name: 'Summer.zip', size: 0, estimated_size: 1,
      operation_id: '62', request_id: albumRequest.request_id, batch_id: albumRequest.request_id,
      part_index: 1, part_count: 1,
    }) })
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'asset_preview_header', id: 'asset-1', mimeType: 'image/jpeg', size: 1,
      operation_id: '63', request_id: previewRequest.request_id,
    }) })
    await __test.handleMediaMessage({ data: encodeChunkEnvelope('63', 0, new Uint8Array([9])).buffer })
    await __test.handleBulkMessage({ data: encodeChunkEnvelope('62', 0, new Uint8Array([7])).buffer })
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'asset_preview_end', id: 'asset-1', operation_id: '63', request_id: previewRequest.request_id, bytes_sent: '1',
    }) })

    assert.deepEqual(previews, [['asset-1', [9]]])
    assert.deepEqual(bulkChunks, [[7]])
    albumPipeline.onTerminalState({ ok: false, code: 'cleanup', statusClass: 'failed', statusText: '✗ failed' })

    __test.setDownloadTestDependencies()
    __test.setGalleryController(null)
    __test.setTransferSession({ channels: null, mode: null })
  })
})

test('disconnect marks a pending album batch failed before gallery reset', async () => {
  await withMinimalDocument(async () => {
    const channels = fakeLaneSet()
    const states = []
    __test.setTransferSession({ channels, mode: 'relay' })
    __test.setGalleryController({
      setAlbumDownloadState: (state) => states.push(state),
      destroy() {},
    })

    __test.requestAlbumDownload()
    __test.setActiveDownload({ failForDisconnect: async () => ({ ok: false, code: 'disconnected' }) })
    await __test.handleTransferClosure()

    assert.deepEqual(states.at(-1), { phase: 'failed' })
    __test.setActiveDownload(null)
    __test.setGalleryController(null)
    __test.setTransferSession({ channels: null, mode: null })
  })
})

test('disconnect cleanup survives a direct album acknowledgement send throwing', async () => {
  await withMinimalDocument(async () => {
    const channels = fakeLaneSet()
    const states = []
    let pipelineOptions
    const originalSend = channels.control.send.bind(channels.control)
    channels.control.send = (value) => {
      const message = JSON.parse(value)
      if (message.type === 'album_archive_ack') throw new Error('RTCDataChannel is closed')
      return originalSend(value)
    }
    __test.setTransferSession({ channels, mode: 'direct' })
    __test.setGalleryController({ setAlbumDownloadState: (state) => states.push(state) })
    __test.setDownloadTestDependencies({
      getSupport: async () => ({ mode: 'blob', warning: null }),
      createPipeline: async (options) => {
        pipelineOptions = options
        return {
          append: async () => {},
          complete: async () => {},
          failForDisconnect: async () => {
            const result = { ok: false, code: 'disconnected', statusClass: 'failed', statusText: '✗ disconnected' }
            pipelineOptions.onTerminalState(result)
            return result
          },
        }
      },
    })

    __test.requestAlbumDownload()
    const request = JSON.parse(channels.control.sent[0])
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'file_header', name: 'Summer.zip', size: 0, estimated_size: 1,
      operation_id: '59', request_id: request.request_id, batch_id: request.request_id,
      part_index: 1, part_count: 1,
    }) })

    await assert.doesNotReject(() => __test.handleTransferClosure())
    assert.equal(__test.getCurrentFile(), null)
    assert.equal(__test.getTransferSession().transferChannel, null)
    assert.deepEqual(states.at(-1), { phase: 'failed' })

    __test.setDownloadTestDependencies()
    __test.setGalleryController(null)
    __test.setTransferSession({ channels: null, mode: null })
  })
})

test('rejected relay album acknowledgement is observed and fails the batch', async () => {
  await withMinimalDocument(async () => {
    const channels = fakeLaneSet()
    const states = []
    let pipelineOptions
    const originalSend = channels.control.send.bind(channels.control)
    channels.control.send = (value) => {
      const message = JSON.parse(value)
      originalSend(value)
      return message.type === 'album_archive_ack'
        ? Promise.reject(new Error('relay write failed'))
        : Promise.resolve()
    }
    __test.setTransferSession({ channels, mode: 'relay' })
    __test.setGalleryController({ setAlbumDownloadState: (state) => states.push(state) })
    __test.setDownloadTestDependencies({
      getSupport: async () => ({ mode: 'blob', warning: null }),
      createPipeline: async (options) => {
        pipelineOptions = options
        return { append: async () => {}, complete: async () => {} }
      },
    })

    __test.requestAlbumDownload()
    const request = JSON.parse(channels.control.sent[0])
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'file_header', name: 'Summer.zip', size: 0, estimated_size: 1,
      operation_id: '60', request_id: request.request_id, batch_id: request.request_id,
      part_index: 1, part_count: 1,
    }) })
    pipelineOptions.onTerminalState({ ok: true, code: 'complete', statusClass: 'done', statusText: '✓ saved' })
    await new Promise((resolve) => setTimeout(resolve, 0))

    assert.deepEqual(states.at(-1), { phase: 'failed' })
    __test.setDownloadTestDependencies()
    __test.setGalleryController(null)
    __test.setTransferSession({ channels: null, mode: null })
  })
})

test('requestGalleryPreview streams preview data into the gallery controller without starting a download', async () => {
  await withMinimalDocument(async () => {
    const sends = []
    const previews = []
    __test.setTransferSession({ channel: { send: (text) => sends.push(text) }, mode: 'relay' })
    __test.setGalleryController({
      handlePreviewData(id, payload, mimeType) {
        previews.push({ id, payload: Array.from(payload), mimeType })
      },
    })

    __test.requestGalleryPreview('asset-1')

    const request = JSON.parse(sends[0])
    assert.deepEqual({ ...request, request_id: '<dynamic>' },
      { type: 'asset_preview_request', id: 'asset-1', quality: 'preview', request_id: '<dynamic>' })

    await __test.handleTransferMessage({
      data: JSON.stringify({ type: 'asset_preview_header', id: 'asset-1', mimeType: 'image/jpeg', binary_envelope: true, operation_id: '11', request_id: request.request_id }),
    })
    await __test.handleMediaMessage({ data: encodeChunkEnvelope('11', 0, new Uint8Array([1, 2])).buffer })
    await __test.handleMediaMessage({ data: encodeChunkEnvelope('11', 0, new Uint8Array([3])).buffer })
    await __test.handleTransferMessage({ data: JSON.stringify({ type: 'asset_preview_end', id: 'asset-1', operation_id: '11', request_id: request.request_id, bytes_sent: '3' }) })

    assert.deepEqual(previews, [{ id: 'asset-1', payload: [1, 2, 3], mimeType: 'image/jpeg' }])
    assert.equal(__test.getCurrentFile(), null)

    __test.setGalleryController(null)
    __test.setTransferSession({ channel: null, mode: null })
  })
})

test('seek handler resets the SW generation without sending a page-computed byte offset', () => {
  const sent = []
  const swMessages = []
  const originalDocument = globalThis.document
  const navigatorDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'navigator')

  globalThis.document = {
    querySelector(selector) {
      assert.equal(selector, 'video.lg-video')
      return { currentTime: 20, duration: 40 }
    },
  }
  Object.defineProperty(globalThis, 'navigator', {
    configurable: true,
    value: {
      serviceWorker: {
        controller: {
          postMessage(payload) {
            swMessages.push(payload)
          },
        },
      },
    },
  })

  try {
    __test.setTransferSession({
      channel: { send: (payload) => sent.push(JSON.parse(payload)) },
      mode: 'relay',
    })

    const vp = {
      id: 'asset-1',
      mediaId: 'asset-1',
      operationId: '77',
      generation: 0,
      totalSize: 1000,
      streamStartOffset: 0,
      totalBytesReceived: 0,
      seekTimer: null,
      seeking: false,
    }

    const onSeeked = __test.createSeekHandler(vp, {
      setTimeoutFn(fn) {
        fn()
        return 1
      },
      clearTimeoutFn() {},
    })

    onSeeked()

    assert.equal(vp.generation, 1)
    assert.deepEqual(swMessages, [{ mediaId: 'asset-1', reset: true, generation: 1 }])
    assert.equal(sent.length, 1)
    assert.deepEqual(sent[0], {
      type: 'asset_preview_seek',
      id: 'asset-1',
      quality: 'video',
      start_offset: 500,
      generation: 1,
      operation_id: '77',
      request_id: sent[0].request_id,
    })
  } finally {
    globalThis.document = originalDocument
    if (navigatorDescriptor) {
      Object.defineProperty(globalThis, 'navigator', navigatorDescriptor)
    } else {
      delete globalThis.navigator
    }
    __test.setTransferSession({ channel: null, mode: null })
  }
})

test('seek handler leaves a target inside the browser buffer on the current generation', () => {
  const originalDocument = globalThis.document
  const navigatorDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'navigator')
  const swMessages = []
  const sent = []
  globalThis.document = {
    querySelector: () => ({
      currentTime: 20,
      duration: 100,
      buffered: {
        length: 1,
        start: () => 0,
        end: () => 40,
      },
    }),
  }
  Object.defineProperty(globalThis, 'navigator', {
    configurable: true,
    value: { serviceWorker: { controller: { postMessage: (message) => swMessages.push(message) } } },
  })
  __test.setTransferSession({
    channel: { send: (message) => sent.push(JSON.parse(message)) },
    mode: 'relay',
  })
  const vp = {
    id: 'asset-1', mediaId: 'asset-1', generation: 0, totalSize: 1000,
    totalBytesReceived: 0, streamStartOffset: 0, seekTimer: null, seeking: false,
  }

  try {
    __test.createSeekHandler(vp)()

    assert.equal(vp.generation, 0)
    assert.deepEqual(swMessages, [])
    assert.deepEqual(sent, [])
  } finally {
    globalThis.document = originalDocument
    if (navigatorDescriptor) Object.defineProperty(globalThis, 'navigator', navigatorDescriptor)
    else delete globalThis.navigator
    __test.setTransferSession({ channel: null, mode: null })
  }
})

test('exact Service Worker Range wins over the estimated video seek offset', () => {
  const documentDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'document')
  const navigatorDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'navigator')
  const sent = []
  const clearedTimers = []
  let fallbackTimer = null
  const video = {
    currentTime: 69.505,
    duration: 182,
    buffered: {
      length: 1,
      start: () => 0,
      end: () => 39.505,
    },
  }
  Object.defineProperty(globalThis, 'document', {
    configurable: true,
    writable: true,
    value: { querySelector: () => video },
  })
  Object.defineProperty(globalThis, 'navigator', {
    configurable: true,
    value: { serviceWorker: { controller: { postMessage() {} } } },
  })
  __test.setTransferSession({
    channel: { send: (message) => sent.push(JSON.parse(message)) },
    mode: 'relay',
  })
  const vp = {
    id: 'video-1', mediaId: 'video-1', generation: 3, totalSize: 61289309,
    operationId: '78',
    totalBytesReceived: 0, streamStartOffset: 0, seekTimer: null, seeking: false,
  }
  __test.setCurrentVideoPreview(vp)

  try {
    __test.createSeekHandler(vp, {
      setTimeoutFn(fn) {
        fallbackTimer = fn
        return 77
      },
      clearTimeoutFn(id) {
        clearedTimers.push(id)
      },
    })()

    const generation = vp.generation
    __test.handleMediaRangeRequest({
      type: 'media_range_request',
      mediaId: 'video-1',
      generation,
      startOffset: 22970368,
    })
    __test.handleMediaRangeRequest({
      type: 'media_range_request',
      mediaId: 'video-1',
      generation,
      startOffset: 22970368,
    })
    __test.handleMediaRangeRequest({
      type: 'media_range_request',
      mediaId: 'video-1',
      generation: generation - 1,
      startOffset: 1,
    })
    __test.handleMediaRangeRequest({
      type: 'media_range_request',
      mediaId: 'other-video',
      generation,
      startOffset: 2,
    })

    assert.equal(typeof fallbackTimer, 'function')
    assert.deepEqual(clearedTimers, [77])
    assert.deepEqual(sent, [{
      type: 'asset_preview_seek',
      id: 'video-1',
      quality: 'video',
      start_offset: 22970368,
      generation,
      operation_id: '78',
      request_id: sent[0].request_id,
    }])
  } finally {
    __test.setCurrentVideoPreview(null)
    __test.setTransferSession({ channel: null, mode: null })
    if (documentDescriptor) Object.defineProperty(globalThis, 'document', documentDescriptor)
    else delete globalThis.document
    if (navigatorDescriptor) Object.defineProperty(globalThis, 'navigator', navigatorDescriptor)
    else delete globalThis.navigator
  }
})

test('seek fallback sends the estimated offset when Chrome reuses its fetch', () => {
  const documentDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'document')
  const navigatorDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'navigator')
  const sent = []
  let fallbackTimer = null
  const video = {
    currentTime: 91,
    duration: 182,
    buffered: { length: 0 },
  }
  Object.defineProperty(globalThis, 'document', {
    configurable: true,
    writable: true,
    value: { querySelector: () => video },
  })
  Object.defineProperty(globalThis, 'navigator', {
    configurable: true,
    value: { serviceWorker: { controller: { postMessage() {} } } },
  })
  __test.setTransferSession({
    channel: { send: (message) => sent.push(JSON.parse(message)) },
    mode: 'relay',
  })
  const vp = {
    id: 'video-1', mediaId: 'video-1', generation: 8, totalSize: 1000,
    operationId: '79',
    totalBytesReceived: 0, streamStartOffset: 0, seekTimer: null, seeking: false,
  }
  __test.setCurrentVideoPreview(vp)

  try {
    __test.createSeekHandler(vp, {
      setTimeoutFn(fn) {
        fallbackTimer = fn
        return 88
      },
      clearTimeoutFn() {},
    })()

    fallbackTimer()

    assert.deepEqual(sent, [{
      type: 'asset_preview_seek',
      id: 'video-1',
      quality: 'video',
      start_offset: 500,
      generation: vp.generation,
      operation_id: '79',
      request_id: sent[0].request_id,
    }])
  } finally {
    __test.setCurrentVideoPreview(null)
    __test.setTransferSession({ channel: null, mode: null })
    if (documentDescriptor) Object.defineProperty(globalThis, 'document', documentDescriptor)
    else delete globalThis.document
    if (navigatorDescriptor) Object.defineProperty(globalThis, 'navigator', navigatorDescriptor)
    else delete globalThis.navigator
  }
})

test('cleanupCurrentVideoPreview disposes its buffering monitor and hides its banner', () => {
  let disposed = 0
  let hidden = 0
  __test.setCurrentVideoPreview({
    mediaId: 'video-1',
    seekTimer: null,
    resumePlaybackTimer: null,
    seekListener: null,
    bufferWarningMonitor: { destroy() { disposed += 1 } },
    hideBufferWarning: () => { hidden += 1 },
  })

  __test.cleanupCurrentVideoPreview()

  assert.deepEqual({ disposed, hidden }, { disposed: 1, hidden: 1 })
})

test('closing and reopening the same video starts a fresh higher generation', () => {
  const sent = []
  const swMessages = []
  const navigatorDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'navigator')
  Object.defineProperty(globalThis, 'navigator', {
    configurable: true,
    value: { serviceWorker: { controller: { postMessage: (message) => swMessages.push(message) } } },
  })
  __test.setTransferSession({
    channel: { send: (message) => sent.push(JSON.parse(message)) },
    mode: 'relay',
  })
  __test.setCurrentVideoPreview({
    id: 'video-1', mediaId: 'video-1', generation: 3,
    seekTimer: null, resumePlaybackTimer: null, seekListener: null,
  })

  try {
    __test.handleGalleryPreviewClose('video-1')
    __test.requestGalleryPreview('video-1', { priority: 'active', mimeType: 'video/mp4' })

    assert.deepEqual(swMessages, [{ mediaId: 'video-1', chunk: null }])
    assert.deepEqual(sent, [{
      type: 'asset_preview_request', id: 'video-1', quality: 'video', generation: 4,
      request_id: sent[0].request_id,
    }])
  } finally {
    __test.cleanupCurrentVideoPreview()
    __test.setTransferSession({ channel: null, mode: null })
    if (navigatorDescriptor) Object.defineProperty(globalThis, 'navigator', navigatorDescriptor)
    else delete globalThis.navigator
  }
})

test('seek playback recovery calls play when data is flowing but playback stays paused', async () => {
  let playCalls = 0
  const video = {
    paused: true,
    play() {
      playCalls += 1
      return Promise.resolve()
    },
  }
  const vp = {
    generation: 3,
    seeking: true,
    awaitingSeekPlayback: true,
    totalBytesReceived: 128 * 1024,
    resumePlaybackTimer: null,
  }

  __test.scheduleSeekPlaybackRecovery(vp, {
    delayMs: 0,
    queryVideo: () => video,
    isCurrentPreview: () => true,
    setTimeoutFn(fn) {
      fn()
      return 1
    },
    clearTimeoutFn() {},
  })

  await Promise.resolve()
  assert.equal(playCalls, 1)
})

test('seek playback recovery refetches a stalled unpaused video at the latest target', async () => {
  let loadCalls = 0
  let playCalls = 0
  let loadedMetadata
  const video = {
    currentTime: 63.7,
    paused: false,
    readyState: 1,
    addEventListener(type, handler) {
      assert.equal(type, 'loadedmetadata')
      loadedMetadata = handler
    },
    removeEventListener() {},
    load() {
      loadCalls += 1
      this.currentTime = 0
    },
    play() {
      playCalls += 1
      return Promise.resolve()
    },
  }
  const vp = {
    generation: 4,
    seeking: true,
    awaitingSeekPlayback: true,
    totalBytesReceived: 128 * 1024,
    resumePlaybackTimer: null,
  }

  __test.scheduleSeekPlaybackRecovery(vp, {
    delayMs: 0,
    queryVideo: () => video,
    isCurrentPreview: () => true,
    setTimeoutFn(fn) {
      fn()
      return 1
    },
    clearTimeoutFn() {},
  })

  assert.equal(loadCalls, 1)
  assert.equal(typeof loadedMetadata, 'function')
  loadedMetadata()
  await Promise.resolve()
  assert.equal(video.currentTime, 63.7)
  assert.equal(playCalls, 1)
})

test('a completed video range remains routed to the next seek range without refreshing the player', async () => {
  const swMessages = []
  const galleryCalls = []
  const navigatorDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'navigator')
  Object.defineProperty(globalThis, 'navigator', {
    configurable: true,
    value: { serviceWorker: { controller: { postMessage: (message) => swMessages.push(message) } } },
  })
  __test.setGalleryController({ handlePreviewData: (...args) => galleryCalls.push(args) })
  __test.setCurrentVideoPreview({
    id: 'video-1', mediaId: 'video-1', generation: 2, totalBytesReceived: 0,
    operationId: '82', requestId: 'media-test',
    seekTimer: null, resumePlaybackTimer: null,
  })

  try {
    await __test.handleTransferMessage({
      data: JSON.stringify({ type: 'asset_preview_end', id: 'video-1', generation: 2, operation_id: '82', request_id: 'media-test', bytes_sent: '2' }),
    })
    await __test.handleMediaMessage({ data: encodeChunkEnvelope('82', 2, new Uint8Array([9, 8])).buffer })

    assert.deepEqual(swMessages, [
      { mediaId: 'video-1', chunk: new Uint8Array([9, 8]), generation: 2 },
      { mediaId: 'video-1', chunk: null },
    ])
    assert.deepEqual(galleryCalls, [])
  } finally {
    __test.cleanupCurrentVideoPreview()
    __test.setGalleryController(null)
    if (navigatorDescriptor) Object.defineProperty(globalThis, 'navigator', navigatorDescriptor)
    else delete globalThis.navigator
  }
})

test('seek headers rebase the stream without reinitializing the video player', async () => {
  const swMessages = []
  const galleryCalls = []
  const navigatorDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'navigator')
  Object.defineProperty(globalThis, 'navigator', {
    configurable: true,
    value: { serviceWorker: { controller: { postMessage: (message) => swMessages.push(message) } } },
  })
  __test.setGalleryController({ handlePreviewData: (...args) => galleryCalls.push(args) })
  __test.setCurrentVideoPreview({
    id: 'video-1', mediaId: 'video-1', generation: 3, totalBytesReceived: 0,
    seekTimer: null, resumePlaybackTimer: null, seekListener: null,
  })

  try {
    __test.startVideoPreview({ id: 'video-1', generation: 3, size: 1000, byte_offset: 400 })
    await new Promise((resolve) => setTimeout(resolve, 0))
    assert.deepEqual(swMessages, [
      { mediaId: 'video-1', startOffset: 400, generation: 3 },
      { mediaId: 'video-1', size: 1000 },
    ])
    assert.deepEqual(galleryCalls, [])
  } finally {
    __test.cleanupCurrentVideoPreview()
    __test.setGalleryController(null)
    if (navigatorDescriptor) Object.defineProperty(globalThis, 'navigator', navigatorDescriptor)
    else delete globalThis.navigator
  }
})

test('an initial video header resets stale Service Worker state before creating the player', async () => {
  const swMessages = []
  const navigatorDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'navigator')
  const documentDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'document')
  Object.defineProperty(globalThis, 'navigator', {
    configurable: true,
    value: { serviceWorker: { controller: { postMessage: (message) => swMessages.push(message) } } },
  })
  Object.defineProperty(globalThis, 'document', {
    configurable: true,
    value: { querySelector: () => null },
  })
  __test.setGalleryController({ handlePreviewData() {} })
  __test.setCurrentVideoPreview({
    id: 'video-1', mediaId: 'video-1', generation: 0, totalBytesReceived: 0,
    seekTimer: null, resumePlaybackTimer: null, seekListener: null,
  })

  try {
    __test.startVideoPreview({ id: 'video-1', generation: 0, size: 1000, byte_offset: 0 })

    assert.deepEqual(swMessages, [
      { mediaId: 'video-1', startOffset: 0, generation: 0, freshPreview: true },
      { mediaId: 'video-1', size: 1000 },
    ])
  } finally {
    __test.cleanupCurrentVideoPreview()
    __test.setGalleryController(null)
    await new Promise((resolve) => setTimeout(resolve, 110))
    if (navigatorDescriptor) Object.defineProperty(globalThis, 'navigator', navigatorDescriptor)
    else delete globalThis.navigator
    if (documentDescriptor) Object.defineProperty(globalThis, 'document', documentDescriptor)
    else delete globalThis.document
  }
})

test('an out-of-buffer seek starts a new generation before seeked can fire', async () => {
  const swMessages = []
  const listeners = new Map()
  const removedListeners = []
  const navigatorDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'navigator')
  const documentDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'document')
  const video = {
    currentTime: 120,
    duration: 182,
    paused: false,
    addEventListener(type, listener) { listeners.set(type, listener) },
    removeEventListener(type, listener) { removedListeners.push([type, listener]) },
  }
  Object.defineProperty(globalThis, 'navigator', {
    configurable: true,
    value: { serviceWorker: { controller: { postMessage: (message) => swMessages.push(message) } } },
  })
  Object.defineProperty(globalThis, 'document', {
    configurable: true,
    value: { querySelector: () => video },
  })
  __test.setGalleryController({ handlePreviewData() {} })
  const vp = {
    id: 'video-1', mediaId: 'video-1', generation: 0, totalBytesReceived: 0,
    seekTimer: null, resumePlaybackTimer: null, seekListener: null,
  }
  __test.setCurrentVideoPreview(vp)

  try {
    __test.startVideoPreview({ id: 'video-1', generation: 0, size: 1000, byte_offset: 0 })
    await new Promise((resolve) => setTimeout(resolve, 110))

    assert.equal(listeners.has('seeking'), true)
    assert.equal(listeners.has('seeked'), false)
    listeners.get('seeking')()

    const reset = swMessages.find((message) => message.reset === true)
    assert.deepEqual(reset, { mediaId: 'video-1', reset: true, generation: vp.generation })
  } finally {
    __test.cleanupCurrentVideoPreview()
    __test.setGalleryController(null)
    if (navigatorDescriptor) Object.defineProperty(globalThis, 'navigator', navigatorDescriptor)
    else delete globalThis.navigator
    if (documentDescriptor) Object.defineProperty(globalThis, 'document', documentDescriptor)
    else delete globalThis.document
  }
})

test('requestGalleryPreview queues the latest preview while another preview is streaming', async () => {
  await withMinimalDocument(async () => {
    const sends = []
    const previews = []
    __test.setTransferSession({ channel: { send: (text) => sends.push(text) }, mode: 'relay' })
    __test.setGalleryController({
      handlePreviewData(id, payload) {
        previews.push({ id, payload: Array.from(payload) })
      },
    })

    __test.requestGalleryPreview('asset-1')
    __test.requestGalleryPreview('asset-2')
    __test.requestGalleryPreview('asset-3')

    assert.deepEqual(sends.map((text) => JSON.parse(text).id), ['asset-1'])
    const firstRequest = JSON.parse(sends[0])

    await __test.handleTransferMessage({
      data: JSON.stringify({ type: 'asset_preview_header', id: 'asset-1', mimeType: 'image/jpeg', binary_envelope: true, operation_id: '91', request_id: firstRequest.request_id }),
    })
    await __test.handleMediaMessage({ data: encodeChunkEnvelope('91', 0, new Uint8Array([1])).buffer })
    await __test.handleTransferMessage({ data: JSON.stringify({ type: 'asset_preview_end', id: 'asset-1', operation_id: '91', request_id: firstRequest.request_id, bytes_sent: '1' }) })

    assert.deepEqual(previews, [{ id: 'asset-1', payload: [1] }])
    assert.deepEqual(sends.map((text) => JSON.parse(text).id), ['asset-1', 'asset-3'])
    const secondRequest = JSON.parse(sends[1])

    await __test.handleTransferMessage({
      data: JSON.stringify({ type: 'asset_preview_header', id: 'asset-3', mimeType: 'image/jpeg', binary_envelope: true, operation_id: '92', request_id: secondRequest.request_id }),
    })
    await __test.handleMediaMessage({ data: encodeChunkEnvelope('92', 0, new Uint8Array([3])).buffer })
    await __test.handleTransferMessage({ data: JSON.stringify({ type: 'asset_preview_end', id: 'asset-3', operation_id: '92', request_id: secondRequest.request_id, bytes_sent: '1' }) })

    assert.deepEqual(previews, [
      { id: 'asset-1', payload: [1] },
      { id: 'asset-3', payload: [3] },
    ])
    __test.setGalleryController(null)
    __test.setTransferSession({ channel: null, mode: null })
  })
})

test('requestGalleryPreview prioritizes active previews over queued preloads', async () => {
  await withMinimalDocument(async () => {
    const sends = []
    __test.setTransferSession({ channel: { send: (text) => sends.push(text) }, mode: 'relay' })
    __test.setGalleryController({ handlePreviewData() {} })

    __test.requestGalleryPreview('asset-1', { priority: 'active' })
    __test.requestGalleryPreview('asset-0', { priority: 'preload' })
    __test.requestGalleryPreview('asset-2', { priority: 'preload' })
    __test.requestGalleryPreview('asset-3', { priority: 'active' })

    assert.deepEqual(sends.map((text) => JSON.parse(text).id), ['asset-1'])
    const firstRequest = JSON.parse(sends[0])

    await __test.handleTransferMessage({
      data: JSON.stringify({ type: 'asset_preview_header', id: 'asset-1', mimeType: 'image/jpeg', binary_envelope: true, operation_id: '93', request_id: firstRequest.request_id }),
    })
    await __test.handleMediaMessage({ data: encodeChunkEnvelope('93', 0, new Uint8Array([1])).buffer })
    await __test.handleTransferMessage({ data: JSON.stringify({ type: 'asset_preview_end', id: 'asset-1', operation_id: '93', request_id: firstRequest.request_id, bytes_sent: '1' }) })

    assert.deepEqual(sends.map((text) => JSON.parse(text).id), ['asset-1', 'asset-3'])
    const secondRequest = JSON.parse(sends[1])

    await __test.handleTransferMessage({
      data: JSON.stringify({ type: 'asset_preview_header', id: 'asset-3', mimeType: 'image/jpeg', binary_envelope: true, operation_id: '94', request_id: secondRequest.request_id }),
    })
    await __test.handleMediaMessage({ data: encodeChunkEnvelope('94', 0, new Uint8Array([3])).buffer })
    await __test.handleTransferMessage({ data: JSON.stringify({ type: 'asset_preview_end', id: 'asset-3', operation_id: '94', request_id: secondRequest.request_id, bytes_sent: '1' }) })

    assert.deepEqual(sends.map((text) => JSON.parse(text).id), ['asset-1', 'asset-3', 'asset-0'])
    __test.setGalleryController(null)
    __test.setTransferSession({ channel: null, mode: null })
  })
})

test('transfer in progress errors do not fail the current gallery download', async () => {
  await withMinimalDocument(async (elements) => {
    let failCalled = false
    __test.setActiveDownload({
      async fail() {
        failCalled = true
      },
    })

    await __test.handleError({ message: 'transfer in progress' })

    assert.equal(failCalled, false)
    assert.equal(elements.get('status').textContent, 'Download in progress, please wait')
    __test.setActiveDownload(null)
  })
})

test('control sends preview requests while early media chunks wait for their matching header', async () => {
  await withMinimalDocument(async () => {
    const channels = fakeLaneSet()
    const previews = []
    __test.setTransferSession({ channels, mode: 'relay' })
    __test.setGalleryController({ handlePreviewData: (id, bytes) => previews.push([id, [...bytes]]) })

    __test.requestGalleryPreview('early-image')
    const request = JSON.parse(channels.control.sent[0])
    assert.equal(channels.media.sent.length, 0)
    assert.equal(channels.bulk.sent.length, 0)

    await __test.handleMediaMessage({ data: encodeChunkEnvelope('500', 0, new Uint8Array([1, 2])).buffer })
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'asset_preview_header', id: 'early-image', mimeType: 'image/jpeg',
      operation_id: '500', request_id: request.request_id,
    }) })
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'asset_preview_end', id: 'early-image', operation_id: '500', request_id: request.request_id, bytes_sent: '2',
    }) })

    assert.deepEqual(previews, [['early-image', [1, 2]]])
    __test.setGalleryController(null)
    __test.setTransferSession({ channels: null, mode: null })
  })
})

test('unknown-size preview end waits for its exact bytes_sent when control overtakes media', async () => {
  await withMinimalDocument(async () => {
    const channels = fakeLaneSet()
    const previews = []
    __test.setTransferSession({ channels, mode: 'relay' })
    __test.setGalleryController({ handlePreviewData: (id, bytes) => previews.push([id, [...bytes]]) })
    __test.requestGalleryPreview('unknown-size')
    const request = JSON.parse(channels.control.sent[0])
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'asset_preview_header', id: 'unknown-size', mimeType: 'image/jpeg', size: 0,
      operation_id: '550', request_id: request.request_id,
    }) })
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'asset_preview_end', id: 'unknown-size', operation_id: '550',
      request_id: request.request_id, bytes_sent: '2',
    }) })
    assert.deepEqual(previews, [])
    await __test.handleMediaMessage({ data: encodeChunkEnvelope('550', 0, new Uint8Array([4, 5])).buffer })
    assert.deepEqual(previews, [['unknown-size', [4, 5]]])
    __test.setGalleryController(null)
    __test.setTransferSession({ channels: null, mode: null })
  })
})

test('malformed or overrun bytes_sent fails closed', async () => {
  const channels = fakeLaneSet()
  __test.setTransferSession({ channels, mode: 'relay' })
  __test.setCurrentVideoPreview({
    id: 'video', mediaId: 'video', operationId: '560', requestId: 'r', generation: 0,
    totalBytesReceived: 2, wireBytes: 2n,
  })
  await assert.rejects(() => __test.handleControlMessage({ data: JSON.stringify({
    type: 'asset_preview_end', id: 'video', operation_id: '560', request_id: 'r', generation: 0, bytes_sent: '01',
  }) }), /nonnegative decimal string/i)
  await assert.rejects(() => __test.handleControlMessage({ data: JSON.stringify({
    type: 'asset_preview_end', id: 'video', operation_id: '560', request_id: 'r', generation: 0, bytes_sent: '1',
  }) }), /more bytes than bytes_sent/i)
  __test.setCurrentVideoPreview(null)
  __test.setTransferSession({ channels: null, mode: null })
})

test('deferred media end times out per operation and stale timeout cannot touch a new session', async () => {
  await withMinimalDocument(async (elements) => {
    const firstChannels = fakeLaneSet()
    __test.setCompletionTimeoutMs(5)
    __test.setTransferSession({ channels: firstChannels, mode: 'relay' })
    __test.requestGalleryPreview('slow')
    const request = JSON.parse(firstChannels.control.sent[0])
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'asset_preview_header', id: 'slow', mimeType: 'image/jpeg', size: 0,
      operation_id: '570', request_id: request.request_id,
    }) })
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'asset_preview_end', id: 'slow', operation_id: '570', request_id: request.request_id, bytes_sent: '2',
    }) })
    await new Promise((resolve) => setTimeout(resolve, 15))
    assert.equal(__test.getCurrentPreview(), null)
    assert.match(elements.get('status').textContent, /before all bytes/i)

    __test.requestGalleryPreview('old-session')
    const oldRequest = JSON.parse(firstChannels.control.sent.at(-1))
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'asset_preview_header', id: 'old-session', mimeType: 'image/jpeg', size: 0,
      operation_id: '571', request_id: oldRequest.request_id,
    }) })
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'asset_preview_end', id: 'old-session', operation_id: '571', request_id: oldRequest.request_id, bytes_sent: '1',
    }) })
    const secondChannels = fakeLaneSet()
    __test.setTransferSession({ channels: secondChannels, mode: 'direct' })
    __test.setCurrentVideoPreview({ id: 'new', mediaId: 'new', operationId: '571', requestId: 'new-r', generation: 0 })
    await new Promise((resolve) => setTimeout(resolve, 15))
    assert.equal(__test.getCurrentVideoPreview().id, 'new')
    assert.equal(secondChannels.closeCalls, 0)
    __test.setCompletionTimeoutMs(60_000)
    __test.setTransferSession({ channels: null, mode: null })
  })
})

test('deferred end timeout refreshes on correlated chunk progress then fires after silence', async () => {
  await withMinimalDocument(async () => {
    const channels = fakeLaneSet()
    __test.setCompletionTimeoutMs(20)
    __test.setTransferSession({ channels, mode: 'relay' })
    __test.requestGalleryPreview('slow-progress')
    const request = JSON.parse(channels.control.sent[0])
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'asset_preview_header', id: 'slow-progress', mimeType: 'image/jpeg', size: 0,
      operation_id: '575', request_id: request.request_id,
    }) })
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'asset_preview_end', id: 'slow-progress', operation_id: '575',
      request_id: request.request_id, bytes_sent: '4',
    }) })
    await new Promise((resolve) => setTimeout(resolve, 12))
    await __test.handleMediaMessage({ data: encodeChunkEnvelope('575', 0, new Uint8Array([1])).buffer })
    await new Promise((resolve) => setTimeout(resolve, 12))
    await __test.handleMediaMessage({ data: encodeChunkEnvelope('575', 0, new Uint8Array([2])).buffer })
    assert.equal(__test.getCurrentPreview()?.id, 'slow-progress', 'progress past the original deadline must keep the operation alive')
    await new Promise((resolve) => setTimeout(resolve, 25))
    assert.equal(__test.getCurrentPreview(), null, 'silence after the latest progress must time out')
    assert.equal(channels.closeCalls, 0)
    __test.setCompletionTimeoutMs(60_000)
    __test.setTransferSession({ channels: null, mode: null })
  })
})

test('deferred bulk end timeout fails only its bulk pipeline', async () => {
  await withMinimalDocument(async () => {
    const channels = fakeLaneSet()
    const failures = []
    __test.setCompletionTimeoutMs(5)
    __test.setTransferSession({ channels, mode: 'relay' })
    __test.setDownloadTestDependencies({
      getSupport: async () => ({ mode: 'blob', warning: null }),
      createPipeline: async () => ({
        append: async () => {}, complete: async () => {},
        fail: async (...args) => failures.push(args),
      }),
    })
    __test.requestFile('short.bin')
    const request = JSON.parse(channels.control.sent[0])
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'file_header', name: 'short.bin', size: 2, operation_id: '580', request_id: request.request_id,
    }) })
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'chunk_end', operation_id: '580', request_id: request.request_id, bytes_sent: '2',
    }) })
    await new Promise((resolve) => setTimeout(resolve, 15))
    assert.deepEqual(failures, [['incomplete-transfer', 'Transfer ended before all bytes arrived']])
    assert.equal(channels.closeCalls, 0)
    assert.equal(__test.getCurrentFile(), null)
    __test.setCompletionTimeoutMs(60_000)
    __test.setDownloadTestDependencies()
    __test.setTransferSession({ channels: null, mode: null })
  })
})

test('media routes thumbnails independently of interactive preview state', async () => {
  const received = []
  __test.setGalleryController({ handleThumbnailData: (index, bytes) => received.push([index, [...bytes]]) })
  await __test.handleMediaMessage({ data: encodeThumbnailEnvelope(7, new Uint8Array([8, 9])).buffer })
  assert.deepEqual(received, [[7, [8, 9]]])
  __test.setGalleryController(null)
})

test('media and bulk chunks interleave without stealing each other bytes', async () => {
  const navigatorDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'navigator')
  const swMessages = []
  Object.defineProperty(globalThis, 'navigator', {
    configurable: true,
    value: { serviceWorker: { controller: { postMessage: (value) => swMessages.push(value) } } },
  })
  const bulkBytes = []
  __test.setCurrentFile({ name: 'archive.zip', operationId: '601', requestId: 'bulk-r' })
  __test.setActiveDownload({ append: async (bytes) => bulkBytes.push([...bytes]) })
  __test.setCurrentVideoPreview({
    id: 'video', mediaId: 'video', operationId: '602', requestId: 'media-r', generation: 4,
    totalBytesReceived: 0, seeking: true, awaitingSeekPlayback: false,
  })
  try {
    await __test.handleMediaMessage({ data: encodeChunkEnvelope('602', 4, new Uint8Array([2])).buffer })
    await __test.handleBulkMessage({ data: encodeChunkEnvelope('601', 0, new Uint8Array([1])).buffer })
    await __test.handleMediaMessage({ data: encodeChunkEnvelope('602', 4, new Uint8Array([4])).buffer })
    await __test.handleBulkMessage({ data: encodeChunkEnvelope('601', 0, new Uint8Array([3])).buffer })
    assert.deepEqual(bulkBytes, [[1], [3]])
    assert.deepEqual(swMessages.map((message) => [...message.chunk]), [[2], [4]])
  } finally {
    __test.setActiveDownload(null)
    __test.setCurrentFile(null)
    __test.setCurrentVideoPreview(null)
    if (navigatorDescriptor) Object.defineProperty(globalThis, 'navigator', navigatorDescriptor)
    else delete globalThis.navigator
  }
})

test('accepted video seek replaces operation A with B and ignores stale A frames', async () => {
  const navigatorDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'navigator')
  const documentDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'document')
  const channels = fakeLaneSet()
  const swMessages = []
  Object.defineProperty(globalThis, 'navigator', {
    configurable: true,
    value: { serviceWorker: { controller: { postMessage: (value) => swMessages.push(value) } } },
  })
  Object.defineProperty(globalThis, 'document', { configurable: true, value: { querySelector: () => null } })
  const vp = {
    id: 'video', mediaId: 'video', operationId: '700', requestId: 'initial', generation: 5,
    totalBytesReceived: 0, seekSentGeneration: null, freshSession: false,
  }
  __test.setTransferSession({ channels, mode: 'relay' })
  __test.setCurrentVideoPreview(vp)
  try {
    assert.equal(__test.sendVideoSeek(vp, 100, 5), true)
    const seek = JSON.parse(channels.control.sent[0])
    assert.equal(seek.operation_id, '700')
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'asset_preview_header', id: 'video', mimeType: 'video/mp4', generation: 4,
      byte_offset: 50, operation_id: '699', request_id: seek.request_id,
    }) })
    assert.equal(__test.getCurrentVideoPreview().operationId, '700')
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'asset_preview_header', id: 'video', mimeType: 'video/mp4', generation: 5,
      byte_offset: 100, operation_id: '701', request_id: seek.request_id,
    }) })
    assert.equal(__test.getCurrentVideoPreview().operationId, '701')
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'asset_preview_header', id: 'video', mimeType: 'video/mp4', generation: 5,
      byte_offset: 0, operation_id: '700', request_id: 'initial',
    }) })
    assert.equal(__test.getCurrentVideoPreview().operationId, '701')
    await __test.handleMediaMessage({ data: encodeChunkEnvelope('700', 5, new Uint8Array([1])).buffer })
    await __test.handleMediaMessage({ data: encodeChunkEnvelope('701', 5, new Uint8Array([2])).buffer })
    assert.deepEqual(swMessages.filter((message) => message.chunk).map((message) => [...message.chunk]), [[2]])
  } finally {
    __test.cleanupCurrentVideoPreview()
    __test.setTransferSession({ channels: null, mode: null })
    if (navigatorDescriptor) Object.defineProperty(globalThis, 'navigator', navigatorDescriptor)
    else delete globalThis.navigator
    if (documentDescriptor) Object.defineProperty(globalThis, 'document', documentDescriptor)
    else delete globalThis.document
  }
})

test('rapid seeks serialize operation replacement without orphaning the accepted media stream', async () => {
  const navigatorDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'navigator')
  const documentDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'document')
  const channels = fakeLaneSet()
  const swMessages = []
  Object.defineProperty(globalThis, 'navigator', {
    configurable: true,
    value: { serviceWorker: { controller: { postMessage: (value) => swMessages.push(value) } } },
  })
  Object.defineProperty(globalThis, 'document', { configurable: true, value: { querySelector: () => null } })
  const vp = {
    id: 'video', mediaId: 'video', operationId: '700', requestId: 'initial', generation: 5,
    totalBytesReceived: 0, seekSentGeneration: null, freshSession: false,
  }
  __test.setTransferSession({ channels, mode: 'relay' })
  __test.setCurrentVideoPreview(vp)
  try {
    assert.equal(__test.sendVideoSeek(vp, 100, 5), true)
    const firstSeek = JSON.parse(channels.control.sent[0])

    vp.generation = 6
    vp.seekSentGeneration = null
    assert.equal(__test.sendVideoSeek(vp, 200, 6), true)
    vp.generation = 7
    vp.seekSentGeneration = null
    assert.equal(__test.sendVideoSeek(vp, 300, 7), true)
    assert.equal(channels.control.sent.length, 1, 'the newer seek waits for the accepted parent operation')

    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'asset_preview_header', id: 'video', mimeType: 'video/mp4', generation: 5,
      byte_offset: 100, operation_id: '701', request_id: firstSeek.request_id,
    }) })

    assert.equal(channels.control.sent.length, 2)
    const secondSeek = JSON.parse(channels.control.sent[1])
    assert.equal(secondSeek.operation_id, '701')
    assert.equal(secondSeek.start_offset, 300)
    assert.equal(secondSeek.generation, 7)

    for (let i = 0; i < 17; i += 1) {
      await __test.handleMediaMessage({
        data: encodeChunkEnvelope('701', 5, new Uint8Array(64 * 1024)).buffer,
      })
    }
    assert.equal(channels.closeCalls, 0)

    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'asset_preview_header', id: 'video', mimeType: 'video/mp4', generation: 7,
      byte_offset: 300, operation_id: '702', request_id: secondSeek.request_id,
    }) })
    await __test.handleMediaMessage({ data: encodeChunkEnvelope('702', 7, new Uint8Array([9])).buffer })

    assert.equal(__test.getCurrentVideoPreview().operationId, '702')
    assert.deepEqual(swMessages.filter((message) => message.chunk).map((message) => [...message.chunk]), [[9]])
  } finally {
    __test.cleanupCurrentVideoPreview()
    __test.setTransferSession({ channels: null, mode: null })
    if (navigatorDescriptor) Object.defineProperty(globalThis, 'navigator', navigatorDescriptor)
    else delete globalThis.navigator
    if (documentDescriptor) Object.defineProperty(globalThis, 'document', documentDescriptor)
    else delete globalThis.document
  }
})

test('an exact Service Worker range replaces the queued estimate while an older seek is pending', async () => {
  const channels = fakeLaneSet()
  const vp = {
    id: 'video', mediaId: 'video', operationId: '720', requestId: 'initial', generation: 8,
    totalBytesReceived: 0, seekSentGeneration: null, freshSession: false,
  }
  __test.setTransferSession({ channels, mode: 'relay' })
  __test.setCurrentVideoPreview(vp)
  try {
    assert.equal(__test.sendVideoSeek(vp, 100, 8), true)
    const firstSeek = JSON.parse(channels.control.sent[0])

    vp.generation = 9
    vp.seekSentGeneration = null
    assert.equal(__test.sendVideoSeek(vp, 200, 9), true)
    assert.equal(__test.handleMediaRangeRequest({ mediaId: 'video', startOffset: 333, generation: 9 }), true)

    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'asset_preview_header', id: 'video', mimeType: 'video/mp4', generation: 8,
      byte_offset: 100, operation_id: '721', request_id: firstSeek.request_id,
    }) })

    assert.equal(channels.control.sent.length, 2)
    const exactSeek = JSON.parse(channels.control.sent[1])
    assert.equal(exactSeek.operation_id, '721')
    assert.equal(exactSeek.generation, 9)
    assert.equal(exactSeek.start_offset, 333)
  } finally {
    __test.cleanupCurrentVideoPreview()
    __test.setTransferSession({ channels: null, mode: null })
  }
})

test('closing a video abandons its pending seek without letting late media close bulk', async () => {
  const navigatorDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'navigator')
  const documentDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'document')
  const channels = fakeLaneSet()
  const bulkBytes = []
  Object.defineProperty(globalThis, 'navigator', {
    configurable: true,
    value: { serviceWorker: { controller: { postMessage() {} } } },
  })
  Object.defineProperty(globalThis, 'document', { configurable: true, value: { querySelector: () => null } })
  const vp = {
    id: 'video', mediaId: 'video', operationId: '730', requestId: 'initial', generation: 10,
    totalBytesReceived: 0, seekSentGeneration: null, freshSession: false,
  }
  __test.setTransferSession({ channels, mode: 'relay' })
  __test.setCurrentVideoPreview(vp)
  __test.setCurrentFile({ name: 'video.mp4', operationId: '830', requestId: 'bulk-r' })
  __test.setActiveDownload({ append: async (bytes) => bulkBytes.push([...bytes]) })
  try {
    assert.equal(__test.sendVideoSeek(vp, 400, 10), true)
    const seek = JSON.parse(channels.control.sent[0])
    __test.cleanupCurrentVideoPreview()

    for (let i = 0; i < 17; i += 1) {
      await __test.handleMediaMessage({
        data: encodeChunkEnvelope('731', 10, new Uint8Array(64 * 1024)).buffer,
      })
    }
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'asset_preview_header', id: 'video', mimeType: 'video/mp4', generation: 10,
      byte_offset: 400, operation_id: '731', request_id: seek.request_id,
    }) })
    for (let i = 0; i < 129; i += 1) {
      await __test.handleMediaMessage({ data: encodeChunkEnvelope('731', 10, new Uint8Array([1])).buffer })
    }
    await __test.handleBulkMessage({ data: encodeChunkEnvelope('830', 0, new Uint8Array([4])).buffer })

    assert.equal(channels.closeCalls, 0)
    assert.deepEqual(bulkBytes, [[4]])
  } finally {
    __test.setActiveDownload(null)
    __test.setCurrentFile(null)
    __test.setTransferSession({ channels: null, mode: null })
    if (navigatorDescriptor) Object.defineProperty(globalThis, 'navigator', navigatorDescriptor)
    else delete globalThis.navigator
    if (documentDescriptor) Object.defineProperty(globalThis, 'document', documentDescriptor)
    else delete globalThis.document
  }
})

test('a valid media operation can buffer the sender window before its header without closing bulk', async () => {
  const navigatorDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'navigator')
  const documentDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'document')
  const channels = fakeLaneSet()
  const swMessages = []
  const bulkBytes = []
  Object.defineProperty(globalThis, 'navigator', {
    configurable: true,
    value: { serviceWorker: { controller: { postMessage: (value) => swMessages.push(value) } } },
  })
  Object.defineProperty(globalThis, 'document', { configurable: true, value: { querySelector: () => null } })
  const vp = {
    id: 'video', mediaId: 'video', operationId: '710', requestId: 'initial', generation: 7,
    totalBytesReceived: 0, seekSentGeneration: null, freshSession: false,
  }
  __test.setTransferSession({ channels, mode: 'direct' })
  __test.setCurrentVideoPreview(vp)
  __test.setCurrentFile({ name: 'video.mp4', operationId: '800', requestId: 'bulk-r' })
  __test.setActiveDownload({ append: async (bytes) => bulkBytes.push([...bytes]) })
  try {
    assert.equal(__test.sendVideoSeek(vp, 300, 7), true)
    const seek = JSON.parse(channels.control.sent[0])

    for (let i = 0; i < 648; i += 1) {
      await __test.handleMediaMessage({
        data: encodeChunkEnvelope('711', 7, new Uint8Array(8 * 1024)).buffer,
      })
    }

    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'asset_preview_header', id: 'video', mimeType: 'video/mp4', generation: 7,
      byte_offset: 300, operation_id: '711', request_id: seek.request_id,
    }) })
    await __test.handleBulkMessage({ data: encodeChunkEnvelope('800', 0, new Uint8Array([4])).buffer })

    assert.equal(channels.closeCalls, 0)
    assert.equal(swMessages.filter((message) => message.chunk).length, 81)
    assert.deepEqual(bulkBytes, [[4]])
  } finally {
    __test.setActiveDownload(null)
    __test.setCurrentFile(null)
    __test.cleanupCurrentVideoPreview()
    __test.setTransferSession({ channels: null, mode: null })
    if (navigatorDescriptor) Object.defineProperty(globalThis, 'navigator', navigatorDescriptor)
    else delete globalThis.navigator
    if (documentDescriptor) Object.defineProperty(globalThis, 'document', documentDescriptor)
    else delete globalThis.document
  }
})

test('a request-scoped second bulk rejection cannot abort the active bulk operation', async () => {
  await withMinimalDocument(async () => {
    let failed = false
    __test.setCurrentFile({ name: 'active.bin', operationId: '800', requestId: 'first' })
    __test.setActiveDownload({ fail: async () => { failed = true } })
    await __test.handleError({ type: 'error', scope: 'bulk', request_id: 'second', message: 'transfer in progress' })
    await __test.handleError({ type: 'error', scope: 'bulk', operation_id: '799', request_id: 'old', message: 'stale' })
    assert.equal(failed, false)
    __test.setActiveDownload(null)
    __test.setCurrentFile(null)
  })
})

test('rapid file double click sends one bulk request and preserves its request id', async () => {
  await withMinimalDocument(async () => {
    const channels = fakeLaneSet()
    __test.setTransferSession({ channels, mode: 'direct' })
    __test.requestFile('first.bin')
    const first = JSON.parse(channels.control.sent[0])
    __test.requestFile('second.bin')
    assert.equal(channels.control.sent.length, 1)
    assert.equal(JSON.parse(channels.control.sent[0]).request_id, first.request_id)
    __test.setTransferSession({ channels: null, mode: null })
  })
})

test('real file lifecycle drains early and initializing chunks in strict order before end', async () => {
  await withMinimalDocument(async () => {
    const channels = fakeLaneSet()
    const support = deferred()
    const events = []
    __test.setTransferSession({ channels, mode: 'direct' })
    __test.setDownloadTestDependencies({
      getSupport: () => support.promise,
      createPipeline: async () => ({
        append: async (bytes) => events.push(['chunk', [...bytes]]),
        complete: async () => events.push(['end']),
      }),
    })
    __test.requestFile('report.pdf')
    const request = JSON.parse(channels.control.sent[0])
    await __test.handleBulkMessage({ data: encodeChunkEnvelope('910', 0, new Uint8Array([1])).buffer })
    const header = __test.handleControlMessage({ data: JSON.stringify({
      type: 'file_header', name: 'report.pdf', size: 3, mimeType: 'application/pdf',
      operation_id: '910', request_id: request.request_id,
    }) })
    const second = __test.handleBulkMessage({ data: encodeChunkEnvelope('910', 0, new Uint8Array([2, 3])).buffer })
    const end = __test.handleControlMessage({ data: JSON.stringify({
      type: 'chunk_end', operation_id: '910', request_id: request.request_id, bytes_sent: '3',
    }) })
    assert.deepEqual(events, [])
    support.resolve({ mode: 'blob', warning: null })
    await Promise.all([header, second, end])
    assert.deepEqual(events, [['chunk', [1]], ['chunk', [2, 3]], ['end']])
    assert.deepEqual(channels.media.sent, [])
    __test.setDownloadTestDependencies()
    __test.setTransferSession({ channels: null, mode: null })
  })
})

test('failed bulk initialization discards queued bytes before a new download', async () => {
  await withMinimalDocument(async () => {
    const channels = fakeLaneSet()
    const events = []
    let createCalls = 0
    __test.setTransferSession({ channels, mode: 'relay' })
    __test.setDownloadTestDependencies({
      getSupport: async () => ({ mode: 'blob', warning: null }),
      createPipeline: async () => {
        createCalls += 1
        if (createCalls === 1) throw new Error('sink failed')
        return { append: async (bytes) => events.push([...bytes]), complete: async () => {} }
      },
    })
    __test.requestFile('first.bin')
    const first = JSON.parse(channels.control.sent[0])
    const firstHeader = __test.handleControlMessage({ data: JSON.stringify({
      type: 'file_header', name: 'first.bin', size: 1, operation_id: '920', request_id: first.request_id,
    }) })
    const staleChunk = __test.handleBulkMessage({ data: encodeChunkEnvelope('920', 0, new Uint8Array([9])).buffer })
    await Promise.all([firstHeader, staleChunk])

    __test.requestFile('second.bin')
    const second = JSON.parse(channels.control.sent[1])
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'file_header', name: 'second.bin', size: 1, operation_id: '921', request_id: second.request_id,
    }) })
    await __test.handleBulkMessage({ data: encodeChunkEnvelope('921', 0, new Uint8Array([7])).buffer })
    assert.deepEqual(events, [[7]])
    __test.setDownloadTestDependencies()
    __test.setTransferSession({ channels: null, mode: null })
  })
})

test('wire receipt precedes a held append so end queues behind it and underdeclared end fails', async () => {
  await withMinimalDocument(async () => {
    const channels = fakeLaneSet()
    const appendGate = deferred()
    const events = []
    __test.setTransferSession({ channels, mode: 'relay' })
    __test.setDownloadTestDependencies({
      getSupport: async () => ({ mode: 'blob', warning: null }),
      createPipeline: async () => ({
        append: async () => { events.push('append-start'); await appendGate.promise; events.push('append-end') },
        complete: async () => events.push('complete'),
      }),
    })
    __test.requestFile('held.bin')
    const request = JSON.parse(channels.control.sent[0])
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'file_header', name: 'held.bin', size: 1, operation_id: '930', request_id: request.request_id,
    }) })
    const chunk = __test.handleBulkMessage({ data: encodeChunkEnvelope('930', 0, new Uint8Array([1])).buffer })
    await new Promise((resolve) => setTimeout(resolve, 0))
    const end = __test.handleControlMessage({ data: JSON.stringify({
      type: 'chunk_end', operation_id: '930', request_id: request.request_id, bytes_sent: '1',
    }) })
    assert.deepEqual(events, ['append-start'])
    appendGate.resolve()
    await Promise.all([chunk, end])
    assert.deepEqual(events, ['append-start', 'append-end', 'complete'])

    await assert.rejects(() => __test.handleControlMessage({ data: JSON.stringify({
      type: 'chunk_end', operation_id: '930', request_id: request.request_id, bytes_sent: '0',
    }) }), /more bytes than bytes_sent/i)
    assert.equal(channels.closeCalls, 1)
    __test.setDownloadTestDependencies()
    __test.setTransferSession({ channels: null, mode: null })
  })
})

test('delayed old-session sink completion cannot mutate a new session', async () => {
  await withMinimalDocument(async () => {
    const first = fakeLaneSet()
    const second = fakeLaneSet()
    const appendGate = deferred()
    let sessionCurrent = true
    __test.setTransferSession({ channels: first, mode: 'relay' })
    __test.setDownloadTestDependencies({
      getSupport: async () => ({ mode: 'blob', warning: null }),
      createPipeline: async () => ({ append: () => appendGate.promise, complete: async () => {} }),
    })
    __test.requestFile('old.bin')
    const request = JSON.parse(first.control.sent[0])
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'file_header', name: 'old.bin', size: 1, operation_id: '940', request_id: request.request_id,
    }) })
    const oldChunk = __test.handleBulkMessage(
      { data: encodeChunkEnvelope('940', 0, new Uint8Array([1])).buffer },
      { isCurrentSession: () => sessionCurrent },
    )
    await Promise.resolve()
    sessionCurrent = false
    __test.setTransferSession({ channels: second, mode: 'direct' })
    __test.setCurrentVideoPreview({ id: 'new', mediaId: 'new', operationId: '940', requestId: 'new-r', generation: 0 })
    appendGate.resolve()
    await oldChunk
    assert.equal(__test.getCurrentVideoPreview().id, 'new')
    assert.equal(__test.getCurrentFile(), null)
    assert.equal(second.closeCalls, 0)
    __test.setDownloadTestDependencies()
    __test.setTransferSession({ channels: null, mode: null })
  })
})

test('early-frame abuse closes the whole channel set and never drops into a consumer', async () => {
  const channels = fakeLaneSet()
  __test.setTransferSession({ channels, mode: 'relay' })
  for (let i = 0; i < 128; i += 1) {
    await __test.handleMediaMessage({ data: encodeChunkEnvelope(String(1000 + i), 0, new Uint8Array()).buffer })
  }
  await assert.rejects(
    () => __test.handleMediaMessage({ data: encodeChunkEnvelope('1200', 0, new Uint8Array()).buffer }),
    /buffer limit exceeded/i,
  )
  assert.equal(channels.closeCalls, 1)
  __test.setTransferSession({ channels: null, mode: null })
})

test('closing a video retires its operation so 129 late frames are ignored', async () => {
  const channels = fakeLaneSet()
  __test.setTransferSession({ channels, mode: 'relay' })
  __test.setCurrentVideoPreview({
    id: 'old', mediaId: 'old', operationId: '1300', requestId: 'old-r', generation: 1,
    totalBytesReceived: 0,
  })
  __test.cleanupCurrentVideoPreview()
  for (let i = 0; i < 129; i += 1) {
    await __test.handleMediaMessage({ data: encodeChunkEnvelope('1300', 1, new Uint8Array([i & 0xff])).buffer })
  }
  assert.equal(channels.closeCalls, 0)
  __test.setTransferSession({ channels: null, mode: null })
})

test('terminal image error retires its operation before late media frames arrive', async () => {
  await withMinimalDocument(async () => {
    const channels = fakeLaneSet()
    __test.setTransferSession({ channels, mode: 'relay' })
    __test.requestGalleryPreview('image')
    const request = JSON.parse(channels.control.sent[0])
    await __test.handleControlMessage({ data: JSON.stringify({
      type: 'asset_preview_header', id: 'image', mimeType: 'image/jpeg', operation_id: '1310', request_id: request.request_id,
    }) })
    await __test.handleError({
      type: 'error', scope: 'media', operation_id: '1310', request_id: request.request_id, message: 'failed',
    })
    for (let i = 0; i < 129; i += 1) {
      await __test.handleMediaMessage({ data: encodeChunkEnvelope('1310', 0, new Uint8Array([1])).buffer })
    }
    assert.equal(channels.closeCalls, 0)
    __test.setTransferSession({ channels: null, mode: null })
  })
})

test('an early-frame timer from an old session cannot close a reconnected session', async () => {
  const first = fakeLaneSet()
  const second = fakeLaneSet()
  __test.setEarlyFrameTtlMs(5)
  __test.setTransferSession({ channels: first, mode: 'relay' })
  await __test.handleMediaMessage({ data: encodeChunkEnvelope('1400', 0, new Uint8Array([1])).buffer })
  __test.setTransferSession({ channels: second, mode: 'direct' })
  await new Promise((resolve) => setTimeout(resolve, 15))
  assert.equal(first.closeCalls, 0)
  assert.equal(second.closeCalls, 0)
  __test.setEarlyFrameTtlMs(5000)
  __test.setTransferSession({ channels: null, mode: null })
})

test('captured lane handlers discard old-session frames even with a reused operation id', async () => {
  await withMinimalDocument(async () => {
    const first = fakeLaneSet()
    const second = fakeLaneSet()
    const attach = (channels) => __test.attachTransferChannel({
      channel: channels,
      mode: 'relay',
      updateStatus() {}, hideSection() {}, showSection() {}, requestFileList() {},
      applyConnectionBadge() {}, onClose() {}, isGalleryMode: () => false,
    })
    __test.setTransferSession({ channels: first, mode: 'relay' })
    attach(first)
    __test.setTransferSession({ channels: second, mode: 'direct' })
    attach(second)
    __test.setCurrentVideoPreview({
      id: 'new', mediaId: 'new', operationId: '1500', requestId: 'new-r', generation: 0,
      totalBytesReceived: 0, wireBytes: 0n,
    })
    for (let i = 0; i < 129; i += 1) {
      first.media.onmessage({ data: encodeChunkEnvelope('1500', 0, new Uint8Array([1])).buffer })
    }
    await new Promise((resolve) => setTimeout(resolve, 0))
    assert.equal(__test.getCurrentVideoPreview().totalBytesReceived, 0)
    assert.equal(first.closeCalls, 0)
    assert.equal(second.closeCalls, 0)
    __test.setTransferSession({ channels: null, mode: null })
  })
})

test('transfer closure cleanup executes once even if several required lanes report closure', async () => {
  await withMinimalDocument(async () => {
    let failures = 0
    __test.setTransferSession({ channels: fakeLaneSet(), mode: 'relay' })
    __test.setActiveDownload({ failForDisconnect: async () => { failures += 1 } })
    await Promise.all([__test.handleTransferClosure(), __test.handleTransferClosure(), __test.handleTransferClosure()])
    assert.equal(failures, 1)
    __test.setActiveDownload(null)
  })
})
