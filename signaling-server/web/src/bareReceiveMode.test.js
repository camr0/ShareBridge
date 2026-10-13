// EXP44 — proves the recovered EXP28 `?bare=1` patch is wired into app.js and
// does exactly its three jobs, and that it is inert without the parameter.
//
// app.js installs the bare receiver at module evaluation time from
// `location.search`, so this file loads a second, cache-distinct instance of the
// module under a stubbed global `location`.
import { test } from 'node:test'
import assert from 'node:assert/strict'

function fakeLane() {
  return {
    readyState: 'connecting',
    sent: [],
    send(value) { this.sent.push(value) },
    close() {},
    onopen: null,
    onmessage: null,
    onclose: null,
  }
}

function fakeLaneSet() {
  return {
    readyState: 'connecting',
    control: fakeLane(),
    media: fakeLane(),
    bulk: fakeLane(),
    close() {},
  }
}

async function withBareModule(search, run) {
  const previousLocation = Object.getOwnPropertyDescriptor(globalThis, 'location')
  const previousStorage = Object.getOwnPropertyDescriptor(globalThis, 'localStorage')
  Object.defineProperty(globalThis, 'location', { value: { search }, configurable: true, writable: true })
  // app.js's DEBUG probe touches localStorage once location exists; Node's own
  // localStorage emits an experimental warning on access, so shadow it.
  try {
    Object.defineProperty(globalThis, 'localStorage', { value: { getItem: () => null }, configurable: true, writable: true })
  } catch { /* older Node has no localStorage global to shadow */ }
  let mod
  try {
    mod = await import(`./app.js?bare-mode-${encodeURIComponent(search)}`)
  } finally {
    if (previousLocation) Object.defineProperty(globalThis, 'location', previousLocation)
    else delete globalThis.location
    if (previousStorage) Object.defineProperty(globalThis, 'localStorage', previousStorage)
    else delete globalThis.localStorage
  }
  try {
    await run(mod)
  } finally {
    mod.__test.setTransferSession({ channels: null, mode: null })
    delete globalThis.__bare
  }
}

const flush = () => new Promise((resolve) => setTimeout(resolve, 0))

test('without ?bare=1 the app installs no bare receiver at all', async () => {
  const mod = await import('./app.js?bare-mode-off')
  assert.equal(mod.__test.getBareReceive(), null)
  assert.equal(Object.hasOwn(globalThis, '__bare'), false)
})

test('?bare=1 exposes the EXP28 counter, counts bulk frames, and ignores the framing control messages', async () => {
  await withBareModule('?bare=1', async (mod) => {
    const bare = mod.__test.getBareReceive()
    assert.ok(bare, 'bareReceive must be installed')
    assert.equal(globalThis.__bare, bare, 'the field drivers read window.__bare')
    assert.deepEqual(
      { bytes: bare.bytes, frames: bare.frames, header: bare.header, startedAt: bare.startedAt, endedAt: bare.endedAt },
      { bytes: 0, frames: 0, header: null, startedAt: 0, endedAt: 0 },
    )

    const channel = fakeLaneSet()
    mod.__test.setTransferSession({ channels: channel, mode: 'direct' })
    mod.__test.attachTransferChannel({
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

    // (2) every bulk-lane message is counted and nothing else is done with it.
    channel.bulk.onmessage({ data: new ArrayBuffer(1014) })
    channel.bulk.onmessage({ data: new Uint8Array(600) })
    assert.equal(bare.frames, 2)
    assert.equal(bare.bytes, 1614, 'wire bytes = raw DataChannel byteLength')
    assert.equal(bare.payloadBytes(), 1614 - 14 * 2)

    // (3) the framing control messages are recorded / dropped, never dispatched.
    await channel.control.onmessage({ data: JSON.stringify({
      type: 'file_header', name: 'debian-13.4.0-amd64-netinst.iso', size: 790626304,
      estimated_size: 790626304, operation_id: '1', request_id: 'bulk-1',
    }) })
    await flush()
    assert.deepEqual(bare.header, {
      name: 'debian-13.4.0-amd64-netinst.iso', size: 790626304, estimatedSize: 790626304,
    })
    assert.equal(mod.__test.getCurrentFile(), null, 'no download operation is created in bare mode')

    await channel.control.onmessage({ data: JSON.stringify({
      type: 'chunk_end', operation_id: '1', request_id: 'bulk-1', bytes_sent: '790626304',
    }) })
    await flush()
    assert.equal(mod.__test.getCurrentFile(), null, 'chunk_end does not complete a download in bare mode')

    // The control lane is otherwise the real one: an unknown type is still logged
    // and ignored rather than throwing.
    await channel.control.onmessage({ data: JSON.stringify({ type: 'not_a_control_type' }) })
    await flush()
  })
})
