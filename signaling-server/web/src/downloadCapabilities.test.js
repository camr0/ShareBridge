import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  MOBILE_SAFARI_EXPERIMENT_BYTES,
  detectDownloadSupport,
  buildFallbackWarning,
  buildExperimentalWarning,
} from './downloadCapabilities.js'

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
  assert.match(result.warning.message, /experimental streaming path/i)
})

test('buildFallbackWarning strengthens copy for large files', () => {
  const small = buildFallbackWarning({ fileSize: 10 * 1024 * 1024, reason: 'unsupported browser' })
  const large = buildFallbackWarning({ fileSize: 600 * 1024 * 1024, reason: 'unsupported browser' })
  const mobile = buildExperimentalWarning()

  assert.match(small.message, /in-memory download path/i)
  assert.match(large.message, /large downloads may fail/i)
  assert.match(mobile.message, /experimental streaming path/i)
})
