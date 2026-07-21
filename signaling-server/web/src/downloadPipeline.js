const STALL_TIMEOUT_MS = 60_000

export async function createDownloadPipeline({
  header,
  sinkFactory,
  now = () => Date.now(),
  scheduleTimeout = (fn, ms) => setTimeout(fn, ms),
  clearScheduledTimeout = (id) => clearTimeout(id),
  onStateChange = () => {},
  onTerminalState = () => {},
}) {
  const sink = await sinkFactory(header)
  const startedAt = now()
  let receivedBytes = 0
  let receivedChunkCount = 0
  let finished = false
  let timeoutId = null

  const bumpTimeout = () => {
    if (finished) {
      return
    }
    if (timeoutId) {
      clearScheduledTimeout(timeoutId)
    }
    timeoutId = scheduleTimeout(() => {
      void fail('timeout', 'Transfer stalled before completion')
    }, STALL_TIMEOUT_MS)
  }

  bumpTimeout()
  onStateChange({
    phase: 'downloading',
    receivedBytes,
    expectedBytes: header.size,
    receivedChunkCount,
  })

  return {
    async append(bytes) {
      if (finished) {
        return
      }

      receivedBytes += bytes.length
      receivedChunkCount += 1
      bumpTimeout()
      try {
        await sink.append(bytes)
      } catch (err) {
        return fail('append-failed', buildOperationFailureMessage(err))
      }
      if (finished) {
        return { ok: false, code: 'already-finished' }
      }

      onStateChange(buildProgressState('downloading', { header, receivedBytes, receivedChunkCount }))
    },
    async complete({ expectedSize = header.size } = {}) {
      if (finished) {
        return { ok: false, code: 'already-finished' }
      }

      finished = true
      if (timeoutId) {
        clearScheduledTimeout(timeoutId)
      }

      onStateChange(buildProgressState('verifying', { header, receivedBytes, receivedChunkCount }))

      let result
      try {
        result = await sink.finalize({
          expectedSha1: header.sha1 || null,
          expectedSize,
          receivedBytes,
        })
      } catch (err) {
        return abortAndEmitFailure('finalize-failed', buildOperationFailureMessage(err))
      }
      const terminal = mapFinalizeResult({
        result,
        receivedBytes,
        startedAt,
        finishedAt: now(),
      })

      onTerminalState(terminal)
      return terminal
    },
    async fail(code, message) {
      return fail(code, message)
    },
    async failForDisconnect() {
      return fail('disconnected', 'Connection closed before completion')
    },
  }

  async function fail(code, message) {
    if (finished) {
      return { ok: false, code: 'already-finished' }
    }

    finished = true
    return abortAndEmitFailure(code, message)
  }

  async function abortAndEmitFailure(code, message) {
    if (timeoutId) {
      clearScheduledTimeout(timeoutId)
      timeoutId = null
    }

    try {
      await sink.abort(code)
    } catch {}
    const terminal = {
      ok: false,
      code,
      statusClass: 'failed',
      statusText: `✗ ${message}`,
      avgBytesPerSecond: computeAverageBytesPerSecond({
        receivedBytes,
        startedAt,
        finishedAt: now(),
      }),
      computedSha1: null,
    }
    onTerminalState(terminal)
    return terminal
  }
}

function buildProgressState(phase, { header, receivedBytes, receivedChunkCount }) {
  return {
    phase,
    receivedBytes,
    expectedBytes: header.size,
    receivedChunkCount,
  }
}

function buildOperationFailureMessage(err) {
  const detail = err instanceof Error ? err.message : String(err)
  return detail ? `Download failed: ${detail}` : 'Download failed'
}

function mapFinalizeResult({ result, receivedBytes, startedAt, finishedAt }) {
  const avgBytesPerSecond = computeAverageBytesPerSecond({
    receivedBytes,
    startedAt,
    finishedAt,
  })

  if (!result.ok) {
    return {
      ok: false,
      code: result.code,
      statusClass: result.code === 'checksum-mismatch' ? 'corrupted' : 'failed',
      statusText: result.code === 'checksum-mismatch' ? '✗ corrupted' : '✗ failed',
      avgBytesPerSecond,
      computedSha1: result.computedSha1 || null,
    }
  }

  return {
    ok: true,
    code: result.code,
    statusClass: result.code === 'intact' ? 'verified' : 'done',
    statusText: result.code === 'intact' ? '✓ intact' : '✓ done',
    avgBytesPerSecond,
    computedSha1: result.computedSha1 || null,
  }
}

function computeAverageBytesPerSecond({ receivedBytes, startedAt, finishedAt }) {
  const elapsedSeconds = Math.max(1, (finishedAt - startedAt) / 1000)
  return receivedBytes / elapsedSeconds
}
