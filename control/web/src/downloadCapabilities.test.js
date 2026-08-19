import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  MOBILE_SAFARI_EXPERIMENT_BYTES,
  detectDownloadSupport,
  buildFallbackWarning,
  buildExperimentalWarning,
} from './downloadCapabilities.js'

test('detectDownloadSupport derives runtime defaults when args are omitted', async (t) => {
  const originalNavigator = Object.getOwnPropertyDescriptor(globalThis, 'navigator')
  const originalWritableStream = Object.getOwnPropertyDescriptor(globalThis, 'WritableStream')

  Object.defineProperty(globalThis, 'navigator', {
    configurable: true,
    writable: true,
    value: {
      userAgent: 'Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36',
      serviceWorker: {},
    },
  })
  Object.defineProperty(globalThis, 'WritableStream', {
    configurable: true,
    writable: true,
    value: class WritableStreamMock {},
  })

  t.after(() => {
    if (originalNavigator) {
      Object.defineProperty(globalThis, 'navigator', originalNavigator)
    } else {
      delete globalThis.navigator
    }

    if (originalWritableStream) {
      Object.defineProperty(globalThis, 'WritableStream', originalWritableStream)
    } else {
      delete globalThis.WritableStream
    }
  })

  const result = await detectDownloadSupport({
    registerServiceWorker: async () => ({ scope: '/src/vendor/' }),
  })

  assert.equal(result.mode, 'streaming')
  assert.equal(result.warning, null)
})

test('detectDownloadSupport chooses streaming when runtime primitives are present', async () => {
  const result = await detectDownloadSupport({
    hasWritableStream: true,
    hasServiceWorker: true,
    registerServiceWorker: async () => ({ scope: '/src/vendor/' }),
  })

  assert.equal(result.mode, 'streaming')
  assert.equal(result.warning, null)
})

test('detectDownloadSupport chooses experimental streaming for large Mobile Safari downloads', async () => {
  const result = await detectDownloadSupport({
    fileSize: 150 * 1024 * 1024,
    userAgent: 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1',
    hasWritableStream: true,
    hasServiceWorker: true,
    registerServiceWorker: async () => ({ scope: '/src/vendor/' }),
    registerExperimentalServiceWorker: async () => ({ scope: '/src/vendor/' }),
  })

  assert.equal(result.mode, 'experimental-streaming')
  assert.equal(result.warning.level, 'strong')
  assert.match(result.warning.message, /experimental streaming path/i)
})

test('detectDownloadSupport chooses experimental streaming at the exact Mobile Safari threshold', async () => {
  const result = await detectDownloadSupport({
    fileSize: MOBILE_SAFARI_EXPERIMENT_BYTES,
    userAgent: 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1',
    hasWritableStream: true,
    hasServiceWorker: true,
    registerServiceWorker: async () => ({ scope: '/src/vendor/' }),
    registerExperimentalServiceWorker: async () => ({ scope: '/src/vendor/' }),
  })

  assert.equal(result.mode, 'experimental-streaming')
  assert.equal(result.warning.level, 'strong')
  assert.match(result.warning.message, /experimental streaming path/i)
})

test('detectDownloadSupport keeps small Mobile Safari downloads on blob fallback', async () => {
  const result = await detectDownloadSupport({
    fileSize: 10 * 1024 * 1024,
    userAgent: 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1',
    hasWritableStream: true,
    hasServiceWorker: true,
    registerServiceWorker: async () => ({ scope: '/src/vendor/' }),
  })

  assert.equal(result.mode, 'blob')
  assert.match(result.reason, /below 100 mib/i)
})

test('detectDownloadSupport falls back when service worker registration fails', async () => {
  const result = await detectDownloadSupport({
    fileSize: 600 * 1024 * 1024,
    hasWritableStream: true,
    hasServiceWorker: true,
    registerServiceWorker: async () => { throw new Error('register failed') },
  })

  assert.equal(result.mode, 'blob')
  assert.match(result.reason, /register failed/i)
})

test('detectDownloadSupport fails large Mobile Safari downloads when experimental init fails', async () => {
  const result = await detectDownloadSupport({
    fileSize: 150 * 1024 * 1024,
    userAgent: 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1',
    hasWritableStream: true,
    hasServiceWorker: true,
    registerExperimentalServiceWorker: async () => {
      throw new Error('register failed')
    },
  })

  assert.equal(result.mode, 'fail')
  assert.match(result.reason, /register failed/i)
  assert.match(result.warning.message, /attempted the experimental streaming path/i)
  assert.match(result.warning.message, /initialization failed/i)
})

test('buildFallbackWarning strengthens copy for large files', () => {
  const small = buildFallbackWarning({ fileSize: 10 * 1024 * 1024, reason: 'unsupported browser' })
  const large = buildFallbackWarning({ fileSize: 600 * 1024 * 1024, reason: 'unsupported browser' })
  const mobile = buildExperimentalWarning()

  assert.match(small.message, /in-memory download path/i)
  assert.match(large.message, /in-memory download path/i)
  assert.match(large.message, /large downloads may fail/i)
  assert.match(mobile.message, /experimental streaming path/i)
})
