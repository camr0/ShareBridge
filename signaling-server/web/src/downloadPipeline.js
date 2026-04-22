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
      await sink.append(bytes)
      bumpTimeout()

      onStateChange({
        phase: 'downloading',
        receivedBytes,
        expectedBytes: header.size,
        receivedChunkCount,
      })
    },
    async complete() {
      if (finished) {
        return { ok: false, code: 'already-finished' }
      }

      finished = true
      if (timeoutId) {
        clearScheduledTimeout(timeoutId)
      }

      onStateChange({
        phase: 'verifying',
        receivedBytes,
        expectedBytes: header.size,
        receivedChunkCount,
      })

      const result = await sink.finalize({
        expectedSha1: header.sha1 || null,
        expectedSize: header.size,
        receivedBytes,
      })
      const terminal = mapFinalizeResult({
        result,
        receivedBytes,
        startedAt,
        finishedAt: now(),
      })

      onTerminalState(terminal)
      return terminal
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
    if (timeoutId) {
      clearScheduledTimeout(timeoutId)
    }

    await sink.abort(code)
    const terminal = {
      ok: false,
      code,
      statusClass: 'failed',
      statusText: `✗ ${message}`,
      avgBytesPerSecond: 0,
      computedSha1: null,
    }
    onTerminalState(terminal)
    return terminal
  }
}

function mapFinalizeResult({ result, receivedBytes, startedAt, finishedAt }) {
  const elapsedSeconds = Math.max(1, (finishedAt - startedAt) / 1000)
  const avgBytesPerSecond = receivedBytes / elapsedSeconds

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
