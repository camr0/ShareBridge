export const MOBILE_SAFARI_EXPERIMENT_BYTES = 100 * 1024 * 1024
const LARGE_FILE_WARNING_BYTES = 500 * 1024 * 1024

function isMobileSafari(userAgent = '') {
  return /Safari/i.test(userAgent) && /Mobile/i.test(userAgent) && !/CriOS|FxiOS|EdgiOS|OPiOS|Chrome|Chromium|Android/i.test(userAgent)
}

function formatReason(reason) {
  return reason instanceof Error ? reason.message : String(reason)
}

export function buildFallbackWarning({ fileSize, reason }) {
  const isLargeFile = typeof fileSize === 'number' && fileSize >= LARGE_FILE_WARNING_BYTES

  return {
    level: isLargeFile ? 'strong' : 'normal',
    message: isLargeFile
      ? `This browser is using the in-memory download path, and large downloads may fail. Reason: ${reason}.`
      : `This browser is using the in-memory download path. Reason: ${reason}.`,
  }
}

export function buildExperimentalWarning() {
  return {
    level: 'strong',
    message: 'This browser is using the experimental streaming path for large downloads.',
  }
}

export async function detectDownloadSupport({
  fileSize = 0,
  userAgent = typeof navigator !== 'undefined' ? navigator.userAgent : '',
  hasWritableStream = typeof WritableStream !== 'undefined',
  hasServiceWorker = typeof navigator !== 'undefined' && !!navigator.serviceWorker,
  registerServiceWorker,
  registerExperimentalServiceWorker,
} = {}) {
  const mobileSafari = isMobileSafari(userAgent)
  const hasRuntimeStreaming = hasWritableStream && hasServiceWorker

  if (mobileSafari) {
    if (fileSize < MOBILE_SAFARI_EXPERIMENT_BYTES) {
      return {
        mode: 'blob',
        reason: `Mobile Safari downloads below 100 MiB stay on the blob fallback.`,
        warning: buildFallbackWarning({
          fileSize,
          reason: 'Mobile Safari downloads below 100 MiB stay on the blob fallback',
        }),
      }
    }

    try {
      if (typeof registerExperimentalServiceWorker !== 'function') {
        throw new Error('experimental registration unavailable')
      }

      await registerExperimentalServiceWorker()
      return {
        mode: 'experimental-streaming',
        warning: buildExperimentalWarning(),
      }
    } catch (error) {
      return {
        mode: 'fail',
        reason: formatReason(error),
        warning: buildExperimentalWarning(),
      }
    }
  }

  if (hasRuntimeStreaming) {
    try {
      if (typeof registerServiceWorker !== 'function') {
        throw new Error('service worker registration unavailable')
      }

      await registerServiceWorker()
      return {
        mode: 'streaming',
        warning: null,
      }
    } catch (error) {
      const reason = formatReason(error)
      return {
        mode: 'blob',
        reason,
        warning: buildFallbackWarning({ fileSize, reason }),
      }
    }
  }

  const reason = 'runtime streaming primitives unavailable'
  return {
    mode: 'blob',
    reason,
    warning: buildFallbackWarning({ fileSize, reason }),
  }
}
