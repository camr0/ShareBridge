import { createSHA1 } from './vendor/hash-wasm.js'

let genericStreamSaverPromise = null
let safariStreamSaverPromise = null

export async function createStreamingSink({
  fileName,
  mimeType,
  tailBytes,
  createWriter,
  createHasher,
  deferCloseSettlement = false,
  onDeferredCloseError,
}) {
  const writer = await createWriter(fileName, mimeType)
  const hasher = await createHasher()
  let tail = new Uint8Array(0)

  return {
    async append(bytes) {
      hasher.update(bytes)
      const combined = concatBytes(tail, bytes)

      if (combined.length <= tailBytes) {
        tail = combined
        return
      }

      const flushLength = combined.length - tailBytes
      await writer.write(combined.subarray(0, flushLength))
      tail = combined.subarray(flushLength)
    },
    bufferedTailSize() {
      return tail.length
    },
    async finalize({ expectedSha1, expectedSize, receivedBytes }) {
      const normalizedExpectedSha1 = normalizeExpectedSha1(expectedSha1)
      const computedSha1 = normalizedExpectedSha1 ? hasher.digest('hex') : null

      if (receivedBytes !== expectedSize) {
        await writer.abort('size-mismatch')
        return { ok: false, code: 'size-mismatch' }
      }

      if (normalizedExpectedSha1 && computedSha1 !== normalizedExpectedSha1) {
        await writer.abort('checksum-mismatch')
        return { ok: false, code: 'checksum-mismatch', computedSha1 }
      }

      if (tail.length > 0) {
        await writer.write(tail)
      }
      const closeResult = writer.close()
      if (deferCloseSettlement) {
        void Promise.resolve(closeResult).then(undefined, (error) => {
          try {
            onDeferredCloseError?.(error)
          } catch (_callbackError) {}
        })
      } else {
        await closeResult
      }

      return {
        ok: true,
        code: normalizedExpectedSha1 ? 'intact' : 'done',
        computedSha1,
      }
    },
    async abort(reason) {
      await writer.abort(reason)
    },
  }
}

// SHA-1 over the same byte stream, computed in a module Web Worker.
// crypto.subtle.digest has no incremental API (it cannot hash a 754 MiB stream
// chunk-by-chunk), so the worker runs the same incremental hash-wasm SHA-1.
export function createWorkerSha1({ workerUrl = new URL('./hashWorker.js', import.meta.url), maxOutstanding = 512 } = {}) {
  const worker = new Worker(workerUrl, { type: 'module' })
  const ackWaiters = []
  let outstanding = 0
  let seq = 0
  let digestWaiter = null
  let failure = null

  worker.onmessage = (event) => {
    const msg = event.data || {}
    if (msg.type === 'ack') {
      outstanding -= 1
      const release = ackWaiters.shift()
      if (release) release()
      return
    }
    if (msg.type === 'digest' && digestWaiter && msg.id === digestWaiter.id) {
      const { resolve } = digestWaiter
      digestWaiter = null
      try {
        globalThis.__exp29 = { ...(globalThis.__exp29 || {}), computedSha1: msg.hex, worker: true }
      } catch (_) {}
      resolve(msg.hex)
      return
    }
    if (msg.type === 'error') {
      failure = new Error('sha1 worker: ' + msg.message)
      if (digestWaiter) { const { reject } = digestWaiter; digestWaiter = null; reject(failure) }
    }
  }
  worker.onerror = (event) => { failure = new Error('sha1 worker failed: ' + (event.message || 'unknown')) }

  return {
    update(bytes) {
      if (failure) throw failure
      seq += 1
      // One copy per chunk (64 KiB) is the price of transferable, non-blocking
      // hashing; it replaces a ~1.06 MiB allocation+copy per chunk.
      const copy = new Uint8Array(bytes.length)
      copy.set(bytes)
      outstanding += 1
      worker.postMessage({ type: 'update', seq, buf: copy.buffer }, [copy.buffer])
      if (outstanding >= maxOutstanding) return new Promise((resolve) => ackWaiters.push(resolve))
      return undefined
    },
    digest() {
      const id = 'digest-' + seq
      return new Promise((resolve, reject) => {
        if (failure) { reject(failure); return }
        digestWaiter = { id, resolve, reject }
        worker.postMessage({ type: 'digest', id })
      })
    },
  }
}

// Fixed streaming sink: writes the byte stream through a single fixed
// tailBytes buffer instead of re-building concatBytes(tail, bytes) per append.
export async function createStreamingSinkFixed({
  fileName, mimeType, tailBytes, createWriter, createHasher,
  deferCloseSettlement = false, onDeferredCloseError,
}) {
  const writer = await createWriter(fileName, mimeType)
  const hasher = await createHasher()
  const tail = new Uint8Array(tailBytes)
  let tailLength = 0

  return {
    async append(bytes) {
      const updateResult = hasher.update(bytes)
      if (updateResult && typeof updateResult.then === 'function') await updateResult

      const chunk = bytes instanceof Uint8Array ? bytes : new Uint8Array(bytes)
      let offset = 0
      while (offset < chunk.length) {
        if (tailLength === tailBytes) { await writer.write(tail); tailLength = 0 }
        const take = Math.min(tailBytes - tailLength, chunk.length - offset)
        tail.set(chunk.subarray(offset, offset + take), tailLength)
        tailLength += take
        offset += take
      }
    },
    bufferedTailSize() { return tailLength },
    async finalize({ expectedSha1, expectedSize, receivedBytes }) {
      const normalizedExpectedSha1 = normalizeExpectedSha1(expectedSha1)
      const computedSha1 = normalizedExpectedSha1 ? await hasher.digest('hex') : null

      if (receivedBytes !== expectedSize) {
        await writer.abort('size-mismatch')
        return { ok: false, code: 'size-mismatch' }
      }
      if (normalizedExpectedSha1 && computedSha1 !== normalizedExpectedSha1) {
        await writer.abort('checksum-mismatch')
        return { ok: false, code: 'checksum-mismatch', computedSha1 }
      }
      if (tailLength > 0) await writer.write(tail.subarray(0, tailLength))
      const closeResult = writer.close()
      if (deferCloseSettlement) {
        void Promise.resolve(closeResult).then(undefined, (error) => {
          try { onDeferredCloseError?.(error) } catch (_callbackError) {}
        })
      } else {
        await closeResult
      }
      return { ok: true, code: normalizedExpectedSha1 ? 'intact' : 'done', computedSha1 }
    },
    async abort(reason) { await writer.abort(reason) },
  }
}

export async function createBlobSink({ fileName, mimeType, subtleDigest, triggerBrowserSave }) {
  const chunks = []

  return {
    async append(bytes) {
      chunks.push(bytes)
    },
    async finalize({ expectedSha1, expectedSize, receivedBytes }) {
      const normalizedExpectedSha1 = normalizeExpectedSha1(expectedSha1)
      const combined = combineChunks(chunks)

      if (receivedBytes !== expectedSize) {
        return { ok: false, code: 'size-mismatch' }
      }

      let computedSha1 = null
      if (normalizedExpectedSha1) {
        computedSha1 = await sha1FromSubtle(subtleDigest, combined)
        if (computedSha1 !== normalizedExpectedSha1) {
          return { ok: false, code: 'checksum-mismatch', computedSha1 }
        }
      }

      await triggerBrowserSave(
        new Blob([combined], { type: mimeType || 'application/octet-stream' }),
        fileName,
      )

      return {
        ok: true,
        code: normalizedExpectedSha1 ? 'intact' : 'done',
        computedSha1,
      }
    },
    bufferedTailSize() {
      return 0
    },
    async abort(_reason) {},
  }
}

export async function createBrowserStreamWriter(fileName, mimeType, options) {
  const streamSaver = await loadGenericStreamSaver(options?.importModule)
  const fileStream = streamSaver.createWriteStream(fileName, {
    size: undefined,
    mimeType,
    writableStrategy: undefined,
  })
  return fileStream.getWriter()
}

export async function createSafariBrowserStreamWriter(fileName, mimeType, options) {
  const streamSaver = await loadSafariStreamSaver(options?.importModule)
  const fileStream = streamSaver.createWriteStream(fileName, {
    size: undefined,
    mimeType,
    writableStrategy: undefined,
  })
  return fileStream.getWriter()
}

export async function createIncrementalSha1() {
  return createSHA1()
}

async function loadSafariStreamSaver(importModule = defaultImportModule) {
  if (importModule !== defaultImportModule) {
    return loadStreamSaverModule({
      modulePath: './vendor/streamsaver-safari.js',
      mitmPath: '/src/vendor/streamsaver-safari-mitm.html',
      importModule,
    })
  }

  if (!safariStreamSaverPromise) {
    safariStreamSaverPromise = loadStreamSaverModule({
      modulePath: './vendor/streamsaver-safari.js',
      mitmPath: '/src/vendor/streamsaver-safari-mitm.html',
    })
  }

  return safariStreamSaverPromise
}

async function loadGenericStreamSaver(importModule = defaultImportModule) {
  if (importModule !== defaultImportModule) {
    return loadStreamSaverModule({
      modulePath: './vendor/streamsaver.js',
      mitmPath: '/src/vendor/streamsaver-mitm.html',
      importModule,
    })
  }

  if (!genericStreamSaverPromise) {
    genericStreamSaverPromise = loadStreamSaverModule({
      modulePath: './vendor/streamsaver.js',
      mitmPath: '/src/vendor/streamsaver-mitm.html',
    })
  }

  return genericStreamSaverPromise
}

function defaultImportModule(path) {
  return import(path)
}

async function loadStreamSaverModule({ modulePath, mitmPath, importModule = defaultImportModule }) {
  const previousStreamSaver = Object.getOwnPropertyDescriptor(globalThis, 'streamSaver')
  delete globalThis.streamSaver

  try {
    await importModule(modulePath)

    const streamSaver = globalThis.streamSaver
    if (!streamSaver || typeof streamSaver.createWriteStream !== 'function') {
      throw new Error(`StreamSaver failed to load from ${modulePath}`)
    }

    streamSaver.mitm = mitmPath
    return streamSaver
  } catch (error) {
    if (previousStreamSaver) {
      Object.defineProperty(globalThis, 'streamSaver', previousStreamSaver)
    }
    throw error
  }
}

function normalizeExpectedSha1(expectedSha1) {
  return expectedSha1 ? expectedSha1.toLowerCase() : null
}

function concatBytes(left, right) {
  if (left.length === 0) return right
  if (right.length === 0) return left

  const combined = new Uint8Array(left.length + right.length)
  combined.set(left, 0)
  combined.set(right, left.length)
  return combined
}

function combineChunks(chunks) {
  const totalLength = chunks.reduce((sum, chunk) => sum + chunk.length, 0)
  const combined = new Uint8Array(totalLength)
  let offset = 0

  for (const chunk of chunks) {
    combined.set(chunk, offset)
    offset += chunk.length
  }

  return combined
}

async function sha1FromSubtle(subtleDigest, bytes) {
  const hashBuffer = await subtleDigest('SHA-1', bytes.buffer)
  return Array.from(new Uint8Array(hashBuffer))
    .map((byte) => byte.toString(16).padStart(2, '0'))
    .join('')
}
