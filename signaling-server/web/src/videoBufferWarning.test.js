import { test } from 'node:test'
import assert from 'node:assert/strict'
import { createVideoBufferWarningMonitor } from './videoBufferWarning.js'

function createFakeVideo() {
  const listeners = new Map()
  return {
    addEventListener(type, listener) {
      listeners.set(type, listener)
    },
    removeEventListener(type, listener) {
      if (listeners.get(type) === listener) listeners.delete(type)
    },
    emit(type) {
      listeners.get(type)?.()
    },
    listenerCount() {
      return listeners.size
    },
  }
}

test('ignores startup buffering before playback begins', () => {
  const video = createFakeVideo()
  let warnings = 0
  const monitor = createVideoBufferWarningMonitor({
    video,
    showWarning: () => { warnings += 1 },
    hideWarning: () => {},
    now: () => 0,
  })

  video.emit('waiting')
  video.emit('stalled')

  assert.equal(warnings, 0)
  monitor.destroy()
})

test('warns once after two post-start buffer events within twenty seconds', () => {
  const video = createFakeVideo()
  let now = 0
  let warnings = 0
  const monitor = createVideoBufferWarningMonitor({
    video,
    showWarning: () => { warnings += 1 },
    hideWarning: () => {},
    now: () => now,
  })

  video.emit('playing')
  video.emit('waiting')
  now = 19_000
  video.emit('stalled')
  video.emit('waiting')

  assert.equal(warnings, 1)
  monitor.destroy()
})

test('does not combine buffer events outside the rolling window', () => {
  const video = createFakeVideo()
  let now = 0
  let warnings = 0
  const monitor = createVideoBufferWarningMonitor({
    video,
    showWarning: () => { warnings += 1 },
    hideWarning: () => {},
    now: () => now,
  })

  video.emit('playing')
  video.emit('waiting')
  now = 20_001
  video.emit('stalled')

  assert.equal(warnings, 0)
  monitor.destroy()
})

test('playing hides a shown warning without allowing another warning', () => {
  const video = createFakeVideo()
  let now = 0
  let warnings = 0
  let hides = 0
  const monitor = createVideoBufferWarningMonitor({
    video,
    showWarning: () => { warnings += 1 },
    hideWarning: () => { hides += 1 },
    now: () => now,
  })

  video.emit('playing')
  video.emit('waiting')
  now = 1
  video.emit('stalled')
  video.emit('playing')
  now = 2
  video.emit('waiting')

  assert.deepEqual({ warnings, hides }, { warnings: 1, hides: 1 })
  monitor.destroy()
})

test('destroy detaches listeners and prevents later UI changes', () => {
  const video = createFakeVideo()
  let warnings = 0
  const monitor = createVideoBufferWarningMonitor({
    video,
    showWarning: () => { warnings += 1 },
    hideWarning: () => {},
    now: () => 0,
  })

  monitor.destroy()
  video.emit('playing')
  video.emit('waiting')
  video.emit('stalled')

  assert.deepEqual({ warnings, listeners: video.listenerCount() }, { warnings: 0, listeners: 0 })
})
