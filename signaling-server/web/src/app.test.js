// signaling-server/web/src/app.test.js
import { test } from 'node:test'
import assert from 'node:assert/strict'
import vm from 'node:vm'
import {
  installSessionMessageHandler,
  publishGlobalActions,
  applyConnectionBadge,
  initializeIceConfigTransport,
  assertJoinNotActive,
  isTransferChannelActive,
  detachBrowserSignalingSocket,
  handleBrowserSignalingClose,
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

test('handleTransferMessage does not parse an unclaimed binary file frame as JSON', async () => {
  __test.setGalleryController({})

  try {
    await __test.handleTransferMessage({ data: new Uint8Array([0x10, 9, 8]).buffer })
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

test('handleTransferMessage appends typed file chunk payload only', async () => {
  const appended = []
  __test.setCurrentFile({ name: 'photo.jpg', size: 3, binary_envelope: true })
  __test.setActiveDownload({
    async append(bytes) {
      appended.push([...bytes])
    },
  })

  await __test.handleTransferMessage({ data: new Uint8Array([0x10, 1, 2, 3]).buffer })

  assert.deepEqual(appended, [[1, 2, 3]])
  __test.setActiveDownload(null)
  __test.setCurrentFile(null)
})

test('handleTransferMessage appends legacy raw chunk starting with 0x10', async () => {
  const appended = []
  __test.setCurrentFile({ name: 'legacy-10.bin', size: 3 })
  __test.setActiveDownload({
    async append(bytes) {
      appended.push([...bytes])
    },
  })

  await __test.handleTransferMessage({ data: new Uint8Array([0x10, 8, 7]).buffer })

  assert.deepEqual(appended, [[0x10, 8, 7]])
  __test.setActiveDownload(null)
  __test.setCurrentFile(null)
})

test('handleTransferMessage appends legacy raw chunk starting with 0x11', async () => {
  const appended = []
  __test.setCurrentFile({ name: 'legacy-11.bin', size: 3 })
  __test.setActiveDownload({
    async append(bytes) {
      appended.push([...bytes])
    },
  })

  await __test.handleTransferMessage({ data: new Uint8Array([0x11, 8, 7]).buffer })

  assert.deepEqual(appended, [[0x11, 8, 7]])
  __test.setActiveDownload(null)
  __test.setCurrentFile(null)
})

test('handleTransferMessage still appends legacy raw file chunks', async () => {
  const appended = []
  __test.setCurrentFile({ name: 'legacy.bin', size: 3 })
  __test.setActiveDownload({
    async append(bytes) {
      appended.push([...bytes])
    },
  })

  await __test.handleTransferMessage({ data: new Uint8Array([9, 8, 7]).buffer })

  assert.deepEqual(appended, [[9, 8, 7]])
  __test.setActiveDownload(null)
  __test.setCurrentFile(null)
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

    assert.deepEqual(sends.map((text) => JSON.parse(text)), [
      { type: 'asset_preview_request', id: 'asset-1', quality: 'preview' },
    ])

    await __test.handleTransferMessage({
      data: JSON.stringify({ type: 'asset_preview_header', id: 'asset-1', mimeType: 'image/jpeg', binary_envelope: true }),
    })
    await __test.handleTransferMessage({ data: new Uint8Array([0x10, 1, 2]).buffer })
    await __test.handleTransferMessage({ data: new Uint8Array([0x10, 3]).buffer })
    await __test.handleTransferMessage({ data: JSON.stringify({ type: 'asset_preview_end', id: 'asset-1' }) })

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
    seekTimer: null, resumePlaybackTimer: null,
  })

  try {
    await __test.handleTransferMessage({
      data: JSON.stringify({ type: 'asset_preview_end', id: 'video-1', generation: 2 }),
    })
    await __test.handleTransferMessage({ data: new Uint8Array([0x10, 0, 0, 0, 2, 9, 8]).buffer })

    assert.deepEqual(swMessages, [
      { mediaId: 'video-1', chunk: null },
      { mediaId: 'video-1', chunk: new Uint8Array([9, 8]), generation: 2 },
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

    await __test.handleTransferMessage({
      data: JSON.stringify({ type: 'asset_preview_header', id: 'asset-1', mimeType: 'image/jpeg', binary_envelope: true }),
    })
    await __test.handleTransferMessage({ data: new Uint8Array([0x10, 1]).buffer })
    await __test.handleTransferMessage({ data: JSON.stringify({ type: 'asset_preview_end', id: 'asset-1' }) })

    assert.deepEqual(previews, [{ id: 'asset-1', payload: [1] }])
    assert.deepEqual(sends.map((text) => JSON.parse(text).id), ['asset-1', 'asset-3'])

    await __test.handleTransferMessage({
      data: JSON.stringify({ type: 'asset_preview_header', id: 'asset-3', mimeType: 'image/jpeg', binary_envelope: true }),
    })
    await __test.handleTransferMessage({ data: new Uint8Array([0x10, 3]).buffer })
    await __test.handleTransferMessage({ data: JSON.stringify({ type: 'asset_preview_end', id: 'asset-3' }) })

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

    await __test.handleTransferMessage({
      data: JSON.stringify({ type: 'asset_preview_header', id: 'asset-1', mimeType: 'image/jpeg', binary_envelope: true }),
    })
    await __test.handleTransferMessage({ data: new Uint8Array([0x10, 1]).buffer })
    await __test.handleTransferMessage({ data: JSON.stringify({ type: 'asset_preview_end', id: 'asset-1' }) })

    assert.deepEqual(sends.map((text) => JSON.parse(text).id), ['asset-1', 'asset-3'])

    await __test.handleTransferMessage({
      data: JSON.stringify({ type: 'asset_preview_header', id: 'asset-3', mimeType: 'image/jpeg', binary_envelope: true }),
    })
    await __test.handleTransferMessage({ data: new Uint8Array([0x10, 3]).buffer })
    await __test.handleTransferMessage({ data: JSON.stringify({ type: 'asset_preview_end', id: 'asset-3' }) })

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
