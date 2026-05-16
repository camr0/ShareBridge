/* Vendored from https://raw.githubusercontent.com/camr0/StreamSaver.js at cfecfdda5e45bc1eecb0ce2c2fb0c420107a0a2e (StreamSaver.js). Updated via scripts/update_streamsaver_vendor.sh. */
/*! streamsaver. MIT License. Jimmy Wärting <https://jimmy.warting.se/opensource> */

/* global chrome location ReadableStream define MessageChannel TransformStream */

;((name, definition) => {
  typeof module !== 'undefined'
    ? module.exports = definition()
    : typeof define === 'function' && typeof define.amd === 'object'
      ? define(definition)
      : globalThis[name] = definition()
})('streamSaver', () => {
  'use strict'

  const global = typeof window === 'object' ? window : globalThis
  if (!global.HTMLElement) console.warn('streamsaver is meant to run on browsers main thread')

  let mitmTransporter = null
  let supportsTransferable = false
  const fallbackChunkSize = 256 * 1024
  const test = fn => { try { fn() } catch (e) {} }
  const ponyfill = global.WebStreamsPolyfill || {}
  const isSecureContext = global.isSecureContext
// TODO: Must come up with a real detection test (#69)
  // Safari should work now with service workers, so don't force blob fallback
  let useBlobFallback = false
  const downloadStrategy = isSecureContext || 'MozAppearance' in document.documentElement.style
    ? 'iframe'
    : 'navigate'

  const streamSaver = {
    createWriteStream,
    WritableStream: global.WritableStream || ponyfill.WritableStream,
    supported: true,
    version: { full: '2.0.5', major: 2, minor: 0, dot: 5 },
    mitm: 'mitm.html'
  }

  /**
   * create a hidden iframe and append it to the DOM (body)
   *
   * @param  {string} src page to load
   * @return {HTMLIFrameElement} page to load
   */
  function makeIframe (src) {
    if (!src) throw new Error('meh')
    const iframe = document.createElement('iframe')
    iframe.hidden = true
    iframe.src = src
    iframe.loaded = false
    iframe.name = 'iframe'
    iframe.isIframe = true
    iframe.postMessage = (...args) => iframe.contentWindow.postMessage(...args)
    iframe.addEventListener('load', () => {
      iframe.loaded = true
    }, { once: true })
    document.body.appendChild(iframe)
    return iframe
  }

  /**
   * create a popup that simulates the basic things
   * of what a iframe can do
   *
   * @param  {string} src page to load
   * @return {object}     iframe like object
   */
  function makePopup (src) {
    const options = 'width=200,height=100'
    const delegate = document.createDocumentFragment()
    const popup = {
      frame: global.open(src, 'popup', options),
      loaded: false,
      isIframe: false,
      isPopup: true,
      remove () { popup.frame.close() },
      addEventListener (...args) { delegate.addEventListener(...args) },
      dispatchEvent (...args) { delegate.dispatchEvent(...args) },
      removeEventListener (...args) { delegate.removeEventListener(...args) },
      postMessage (...args) { popup.frame.postMessage(...args) }
    }

    const onReady = evt => {
      if (evt.source === popup.frame) {
        popup.loaded = true
        global.removeEventListener('message', onReady)
        popup.dispatchEvent(new Event('load'))
      }
    }

    global.addEventListener('message', onReady)

    return popup
  }

  try {
    // We can't look for service worker since it may still work on http
    new Response(new ReadableStream())
    if (isSecureContext && !('serviceWorker' in navigator)) {
      useBlobFallback = true
    }
  } catch (err) {
    useBlobFallback = true
  }

  test(() => {
    const { readable } = new TransformStream()
    const mc = new MessageChannel()
    mc.port1.postMessage(readable, [readable])
    mc.port1.close()
    mc.port2.close()
    supportsTransferable = true
    console.log('[StreamSaver] Transferable streams supported - backpressure handled by browser')
    Object.defineProperty(streamSaver, 'TransformStream', {
      configurable: false,
      writable: false,
      value: TransformStream
    })
  })

  if (!supportsTransferable) {
    console.log('[StreamSaver] Transferable streams NOT supported - using chunk-by-chunk postMessage')
  }

  function loadTransporter () {
    if (!mitmTransporter) {
      mitmTransporter = isSecureContext
        ? makeIframe(streamSaver.mitm)
        : makePopup(streamSaver.mitm)
    }
  }

  /**
   * @param  {string} filename filename that should be used
   * @param  {object} options  [description]
   * @param  {number} size     deprecated
   * @return {WritableStream<Uint8Array>}
   */
  function createWriteStream (filename, options, size) {
    // Store original filename for Safari compatibility
    const originalFilename = filename

    let opts = {
      size: null,
      pathname: null,
      writableStrategy: undefined,
      readableStrategy: undefined
    }

    let bytesWritten = 0 // by StreamSaver.js (not the service worker)
    let downloadUrl = null
    let channel = null
    let ts = null

    // normalize arguments
    if (Number.isFinite(options)) {
      [ size, options ] = [ options, size ]
      console.warn('[StreamSaver] Deprecated pass an object as 2nd argument when creating a write stream')
      opts.size = size
      opts.writableStrategy = options
    } else if (options && options.highWaterMark) {
      console.warn('[StreamSaver] Deprecated pass an object as 2nd argument when creating a write stream')
      opts.size = size
      opts.writableStrategy = options
    } else {
      opts = options || {}
    }
    if (!useBlobFallback) {
      loadTransporter()

      channel = new MessageChannel()

      // Make filename RFC5987 compatible
      filename = encodeURIComponent(filename.replace(/\//g, ':'))
        .replace(/['()]/g, escape)
        .replace(/\*/g, '%2A')

      const response = {
        transferringReadable: supportsTransferable,
        pathname: opts.pathname || Math.random().toString().slice(-6) + '/' + filename,
        headers: {
          'Content-Type': 'application/octet-stream',
          // Dual format: Safari needs plain filename=, others use filename*= 
          'Content-Disposition': `attachment; filename="${originalFilename}"; filename*=UTF-8''${filename}`
        }
      }

      if (opts.size) {
        response.headers['Content-Length'] = opts.size
      }

      const args = [ response, '*', [ channel.port2 ] ]

      if (supportsTransferable) {
        const transformer = downloadStrategy === 'iframe' ? undefined : {
          // This transformer & flush method is only used by insecure context.
          transform (chunk, controller) {
            if (!(chunk instanceof Uint8Array)) {
              throw new TypeError('Can only write Uint8Arrays')
            }
            bytesWritten += chunk.length
            controller.enqueue(chunk)

            if (downloadUrl) {
              location.href = downloadUrl
              downloadUrl = null
            }
          },
          flush () {
            if (downloadUrl) {
              location.href = downloadUrl
            }
          }
        }
        ts = new streamSaver.TransformStream(
          transformer,
          opts.writableStrategy,
          opts.readableStrategy
        )
        const readableStream = ts.readable

        channel.port1.postMessage({ readableStream }, [ readableStream ])
      }

      channel.port1.onmessage = evt => {
        // Service worker sent us a link that we should open.
        if (evt.data.download) {
          // Special treatment for popup...
          if (downloadStrategy === 'navigate') {
            mitmTransporter.remove()
            mitmTransporter = null
            if (bytesWritten) {
              location.href = evt.data.download
            } else {
              downloadUrl = evt.data.download
            }
          } else {
            if (mitmTransporter.isPopup) {
              mitmTransporter.remove()
              mitmTransporter = null
              // Special case for firefox, they can keep sw alive with fetch
              if (downloadStrategy === 'iframe') {
                makeIframe(streamSaver.mitm)
              }
            }

            // We never remove this iframes b/c it can interrupt saving
            makeIframe(evt.data.download)
          }
        } else if (evt.data.abort) {
          chunks = []
          channel.port1.postMessage('abort')
          channel.port1.onmessage = null
          channel.port1.close()
          channel.port2.close()
          channel = null
        } else if (evt.data.done) {
          channel.port1.onmessage = null
          channel.port1.close()
          channel.port2.close()
          channel = null
        } else if (evt.data.pull) {
          grantCredits(typeof evt.data.pull === 'number' ? evt.data.pull : 1)
        }
      }

      if (mitmTransporter.loaded) {
        mitmTransporter.postMessage(...args)
      } else {
        mitmTransporter.addEventListener('load', () => {
          mitmTransporter.postMessage(...args)
        }, { once: true })
      }
    }

    let chunks = []
    let availableCredits = 0
    let pendingResolve = null

    const getFallbackChunks = chunk => {
      if (supportsTransferable || chunk.byteLength <= fallbackChunkSize) {
        return [chunk]
      }

      const chunkParts = []
      for (let offset = 0; offset < chunk.byteLength; offset += fallbackChunkSize) {
        chunkParts.push(chunk.subarray(offset, offset + fallbackChunkSize))
      }
      return chunkParts
    }

    const postChunk = chunk => {
      channel.port1.postMessage(chunk)
      bytesWritten += chunk.length

      if (downloadUrl) {
        location.href = downloadUrl
        downloadUrl = null
      }
    }

    const waitForCredit = () => {
      if (availableCredits > 0) {
        availableCredits--
        return Promise.resolve()
      }

      return new Promise(resolve => {
        pendingResolve = () => {
          availableCredits--
          resolve()
        }
      })
    }

    const grantCredits = count => {
      availableCredits += count

      if (pendingResolve && availableCredits > 0) {
        const resolve = pendingResolve
        pendingResolve = null
        resolve()
      }
    }

    return (!useBlobFallback && ts && ts.writable) || new streamSaver.WritableStream({
      write (chunk) {
        if (!(chunk instanceof Uint8Array)) {
          throw new TypeError('Can only write Uint8Arrays')
        }
        if (useBlobFallback) {
          chunks.push(chunk)
          return
        }

        const writeParts = getFallbackChunks(chunk)
        return writeParts.reduce((promise, writePart) => {
          return promise.then(() => waitForCredit()).then(() => {
            postChunk(writePart)
          })
        }, Promise.resolve())
      },
      close () {
        if (useBlobFallback) {
          const blob = new Blob(chunks, { type: 'application/octet-stream' })
          const link = document.createElement('a')
          link.href = URL.createObjectURL(blob)
          link.download = originalFilename
          link.click()
        } else {
          console.log('[StreamSaver] Sending "end" to service worker')
          channel.port1.postMessage('end')
          console.log('[StreamSaver] "end" sent, download should finalize')
        }
      },
      abort () {
        chunks = []
        channel.port1.postMessage('abort')
        channel.port1.onmessage = null
        channel.port1.close()
        channel.port2.close()
        channel = null
      }
    }, opts.writableStrategy)
  }

  return streamSaver
})
