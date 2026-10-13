import { createSHA1 } from './vendor/hash-wasm.js'
const hasherPromise = createSHA1()
self.onmessage = async (event) => {
  const msg = event.data || {}
  try {
    if (msg.type === 'update') {
      const hasher = await hasherPromise
      hasher.update(new Uint8Array(msg.buf))
      if (msg.seq) self.postMessage({ type: 'ack', seq: msg.seq })
      return
    }
    if (msg.type === 'digest') {
      const hasher = await hasherPromise
      self.postMessage({ type: 'digest', id: msg.id, hex: hasher.digest('hex') })
      return
    }
  } catch (error) {
    self.postMessage({ type: 'error', message: String(error && error.message ? error.message : error) })
  }
}
