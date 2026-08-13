import { test } from 'node:test'
import assert from 'node:assert/strict'
import { createGalleryController, renderGalleryShell } from './gallery.js'

function fakeRoot() {
  return {
    innerHTML: '',
    children: [],
    appendChild(node) { this.children.push(node) },
    querySelector() { return null },
  }
}

function galleryItems(count) {
  return Array.from({ length: count }, (_, index) => ({
    id: `asset-${index}`,
    name: `photo-${index}.jpg`,
    mimeType: 'image/jpeg',
  }))
}

test('pull-v1 initially renders and requests only the first 120 thumbnails', () => {
  const requests = []
  const root = fakeRoot()
  const controller = createGalleryController({
    root,
    onThumbnailBatchRequest: (request) => requests.push(request),
  })

  controller.handleThumbnailList({
    thumbnailMode: 'pull-v1',
    items: galleryItems(2453),
  })

  assert.equal((root.innerHTML.match(/class="gallery-item"/g) || []).length, 120)
  assert.equal(requests.length, 1)
  assert.match(requests[0].request_id, /^thumb-/)
  assert.deepEqual(
    { type: requests[0].type, start: requests[0].start, count: requests[0].count },
    { type: 'thumbnail_batch_request', start: 0, count: 120 },
  )
  assert.equal(controller.state.renderedCount, 120)
  assert.equal(controller.state.inFlight.requestId, requests[0].request_id)
})

test('legacy thumbnail lists render eagerly without sending pull requests', () => {
  const requests = []
  const root = fakeRoot()
  const controller = createGalleryController({
    root,
    createObjectURL: () => 'blob:legacy',
    revokeObjectURL: () => {},
    onThumbnailBatchRequest: (request) => requests.push(request),
  })

  controller.handleThumbnailList({ items: galleryItems(121) })
  controller.handleThumbnailData(120, new Uint8Array([1]))
  controller.handleThumbnailComplete({ sent: 121, failed: 0 })

  assert.equal((root.innerHTML.match(/class="gallery-item"/g) || []).length, 121)
  assert.deepEqual(requests, [])
  assert.equal(controller.state.loadedThumbs, 1)
})

test('Load More appends one window and deduplicates requests until completion', () => {
  const requests = []
  let clickHandler
  const appended = []
  const loadMore = { disabled: false, hidden: false, textContent: '', setAttribute() {} }
  const grid = { insertAdjacentHTML: (_position, html) => appended.push(html), addEventListener() {}, removeEventListener() {} }
  const root = fakeRoot()
  root.addEventListener = (event, handler) => { if (event === 'click') clickHandler = handler }
  root.querySelector = (selector) => {
    if (selector === '.gallery-grid') return grid
    if (selector === '.gallery-load-more') return loadMore
    return null
  }
  const controller = createGalleryController({
    root,
    lightGallery: () => ({ galleryItems: galleryItems(245).map(() => ({})), destroy() {} }),
    onThumbnailBatchRequest: (request) => requests.push(request),
  })
  controller.handleThumbnailList({ thumbnailMode: 'pull-v1', items: galleryItems(245) })
  const first = requests[0]
  controller.handleThumbnailBatchComplete({ ...first, request_id: first.request_id, sent: 120, failed: 0 })

  const event = {
    preventDefault() {},
    target: { closest: (selector) => selector === '.gallery-load-more' ? loadMore : null },
  }
  clickHandler(event)
  clickHandler(event)

  assert.equal(requests.length, 2)
  assert.deepEqual(
    { start: requests[1].start, count: requests[1].count },
    { start: 120, count: 120 },
  )
  assert.equal((appended[0].match(/class="gallery-item"/g) || []).length, 120)
  assert.equal(loadMore.disabled, true)

  controller.handleThumbnailBatchComplete({ ...requests[1], request_id: requests[1].request_id, sent: 120, failed: 0 })
  clickHandler(event)
  assert.deepEqual(
    { start: requests[2].start, count: requests[2].count },
    { start: 240, count: 5 },
  )
})

test('intersection sentinel and Load More share the same one-in-flight path', () => {
  const requests = []
  let observerCallback
  let observed = null
  const sentinel = {}
  const loadMore = { disabled: false, hidden: false, setAttribute() {} }
  const grid = { insertAdjacentHTML() {}, addEventListener() {}, removeEventListener() {} }
  const root = fakeRoot()
  root.querySelector = (selector) => {
    if (selector === '.gallery-grid') return grid
    if (selector === '.gallery-sentinel') return sentinel
    if (selector === '.gallery-load-more') return loadMore
    return null
  }
  const controller = createGalleryController({
    root,
    lightGallery: () => ({ galleryItems: galleryItems(240).map(() => ({})), destroy() {} }),
    createIntersectionObserver: (callback) => {
      observerCallback = callback
      return { observe: (node) => { observed = node }, disconnect() {} }
    },
    onThumbnailBatchRequest: (request) => requests.push(request),
  })
  controller.handleThumbnailList({ thumbnailMode: 'pull-v1', items: galleryItems(240) })
  controller.handleThumbnailBatchComplete({ ...requests[0], request_id: requests[0].request_id, sent: 120, failed: 0 })

  observerCallback([{ isIntersecting: true, target: sentinel }])
  observerCallback([{ isIntersecting: true, target: sentinel }])

  assert.equal(observed, sentinel)
  assert.equal(requests.length, 2)
  assert.deepEqual({ start: requests[1].start, count: requests[1].count }, { start: 120, count: 120 })
})

test('intersection events during a batch queue one next window after completion', () => {
  const requests = []
  const appended = []
  let observerCallback
  const sentinel = {}
  const grid = {
    insertAdjacentHTML: (_position, html) => appended.push(html),
    addEventListener() {},
    removeEventListener() {},
  }
  const root = fakeRoot()
  root.querySelector = (selector) => {
    if (selector === '.gallery-grid') return grid
    if (selector === '.gallery-sentinel') return sentinel
    return null
  }
  const controller = createGalleryController({
    root,
    lightGallery: () => ({ galleryItems: galleryItems(360).map(() => ({})), destroy() {} }),
    createIntersectionObserver: (callback) => {
      observerCallback = callback
      return { observe() {}, disconnect() {} }
    },
    onThumbnailBatchRequest: (request) => requests.push(request),
  })
  controller.handleThumbnailList({ thumbnailMode: 'pull-v1', items: galleryItems(360) })
  const first = requests[0]

  observerCallback([{ isIntersecting: true, target: sentinel }])
  observerCallback([{ isIntersecting: true, target: sentinel }])
  observerCallback([{ isIntersecting: true, target: sentinel }])

  assert.equal(requests.length, 1)
  assert.equal(controller.state.pendingExpansion, true)

  controller.handleThumbnailBatchComplete({ ...first, request_id: first.request_id, sent: 120, failed: 0 })

  assert.equal(requests.length, 2)
  assert.deepEqual({ start: requests[1].start, count: requests[1].count }, { start: 120, count: 120 })
  assert.equal(controller.state.renderedCount, 240)
  assert.equal((appended[0].match(/class="gallery-item"/g) || []).length, 120)
  assert.equal(controller.state.pendingExpansion, false)
})

for (const [label, failureFactory] of [
  ['false', () => false],
  ['synchronous throw', () => { throw new Error('send failed') }],
  ['rejected promise', () => Promise.reject(new Error('send rejected'))],
]) {
  test(`initial thumbnail request recovers from ${label} and retries the same range`, async () => {
    const requests = []
    let attempt = 0
    let clickHandler
    const loadMore = { disabled: false, hidden: false, textContent: '', setAttribute() {} }
    const root = fakeRoot()
    root.addEventListener = (event, handler) => { if (event === 'click') clickHandler = handler }
    root.querySelector = (selector) => selector === '.gallery-load-more' ? loadMore : null
    const controller = createGalleryController({
      root,
      onThumbnailBatchRequest: (request) => {
        requests.push(request)
        attempt += 1
        return attempt === 1 ? failureFactory() : true
      },
    })
    controller.handleThumbnailList({ thumbnailMode: 'pull-v1', items: galleryItems(240) })
    await Promise.resolve()
    await Promise.resolve()

    assert.equal(controller.state.inFlight, null)
    assert.equal(controller.state.requestedThumbs, 0)
    assert.equal(loadMore.disabled, false)
    clickHandler({ preventDefault() {}, target: { closest: (selector) => selector === '.gallery-load-more' ? loadMore : null } })

    assert.equal(requests.length, 2)
    assert.deepEqual(requests.map(({ start, count }) => ({ start, count })), [
      { start: 0, count: 120 },
      { start: 0, count: 120 },
    ])
    assert.equal(controller.state.renderedCount, 120)
  })
}

for (const [label, failureFactory] of [
  ['false', () => false],
  ['synchronous throw', () => { throw new Error('send failed') }],
  ['rejected promise', () => Promise.reject(new Error('send rejected'))],
]) {
  test(`Load More recovers from ${label} without permanently appending the range`, async () => {
    const requests = []
    const appended = []
    let failNext = false
    let clickHandler
    const loadMore = { disabled: false, hidden: false, textContent: '', setAttribute() {} }
    const grid = { insertAdjacentHTML: (_position, html) => appended.push(html), addEventListener() {}, removeEventListener() {} }
    const root = fakeRoot()
    root.addEventListener = (event, handler) => { if (event === 'click') clickHandler = handler }
    root.querySelector = (selector) => {
      if (selector === '.gallery-grid') return grid
      if (selector === '.gallery-load-more') return loadMore
      return null
    }
    const controller = createGalleryController({
      root,
      lightGallery: () => ({ galleryItems: galleryItems(240).map(() => ({})), destroy() {} }),
      onThumbnailBatchRequest: (request) => {
        requests.push(request)
        if (failNext) {
          failNext = false
          return failureFactory()
        }
        return true
      },
    })
    controller.handleThumbnailList({ thumbnailMode: 'pull-v1', items: galleryItems(240) })
    controller.handleThumbnailBatchComplete({ ...requests[0], sent: 120, failed: 0 })
    failNext = true
    const event = { preventDefault() {}, target: { closest: (selector) => selector === '.gallery-load-more' ? loadMore : null } }
    clickHandler(event)
    await Promise.resolve()
    await Promise.resolve()

    assert.equal(controller.state.inFlight, null)
    assert.equal(controller.state.renderedCount, 120)
    assert.equal(appended.length, 0)
    assert.equal(loadMore.disabled, false)
    clickHandler(event)

    assert.deepEqual(requests.slice(1).map(({ start, count }) => ({ start, count })), [
      { start: 120, count: 120 },
      { start: 120, count: 120 },
    ])
    assert.equal(controller.state.renderedCount, 240)
    assert.equal(appended.length, 1)
  })
}

test('late rejected thumbnail request cannot roll back a newer gallery', async () => {
  let rejectOld
  let calls = 0
  const controller = createGalleryController({
    root: fakeRoot(),
    onThumbnailBatchRequest: () => {
      calls += 1
      if (calls === 1) return new Promise((_resolve, reject) => { rejectOld = reject })
      return true
    },
  })
  controller.handleThumbnailList({ thumbnailMode: 'pull-v1', items: galleryItems(240) })
  controller.handleThumbnailList({ thumbnailMode: 'pull-v1', items: galleryItems(120) })
  const currentRequest = controller.state.inFlight.requestId

  rejectOld(new Error('old session failed'))
  await Promise.resolve()
  await Promise.resolve()

  assert.equal(controller.state.inFlight.requestId, currentRequest)
  assert.equal(controller.state.requestedThumbs, 120)
})

test('pull completion correlates the exact range and makes missing frames terminal', () => {
  const requests = []
  const root = fakeRoot()
  const controller = createGalleryController({
    root,
    createObjectURL: () => 'blob:thumb',
    revokeObjectURL: () => {},
    onThumbnailBatchRequest: (request) => requests.push(request),
  })
  controller.handleThumbnailList({ thumbnailMode: 'pull-v1', items: galleryItems(125) })
  const request = requests[0]
  controller.handleThumbnailData(0, new Uint8Array([1]))

  controller.handleThumbnailBatchComplete({ ...request, request_id: 'stale', sent: 1, failed: 119 })
  controller.handleThumbnailBatchComplete({ ...request, start: 1, sent: 1, failed: 119 })
  const { start: _start, ...missingStart } = request
  controller.handleThumbnailBatchComplete({ ...missingStart, request_id: request.request_id, sent: 1, failed: 119 })
  controller.handleThumbnailBatchComplete({ ...request, start: null, request_id: request.request_id, sent: 1, failed: 119 })
  assert.ok(controller.state.inFlight)

  controller.handleThumbnailBatchComplete({
    type: 'thumbnail_batch_complete',
    request_id: request.request_id,
    start: 0,
    count: 120,
    sent: 1,
    failed: 119,
    failed_indices: Array.from({ length: 119 }, (_, offset) => offset + 1),
  })
  assert.equal(controller.state.inFlight, null)
  assert.equal(controller.state.unavailableThumbs, 119)
  assert.equal(controller.state.terminalIndices.has(1), true)

  controller.handleThumbnailData(1, new Uint8Array([2]))
  controller.handleThumbnailBatchComplete({ ...request, request_id: request.request_id, sent: 120, failed: 0 })
  assert.equal(controller.state.loadedThumbs, 1)
  assert.equal(controller.state.unavailableThumbs, 119)
  assert.equal(requests.length, 1)
})

test('batch completion accepts late successful frames and rejects explicit failed indices', () => {
  const requests = []
  const controller = createGalleryController({
    root: fakeRoot(),
    createObjectURL: () => 'blob:thumb',
    revokeObjectURL: () => {},
    onThumbnailBatchRequest: (request) => { requests.push(request); return true },
  })
  controller.handleThumbnailList({ thumbnailMode: 'pull-v1', items: galleryItems(3) })
  const request = requests[0]
  controller.handleThumbnailBatchComplete({
    ...request,
    sent: 2,
    failed: 1,
    failed_indices: [1],
  })

  assert.equal(controller.state.unavailableThumbs, 1)
  assert.equal(controller.state.loadedThumbs, 0)
  controller.handleThumbnailData(0, new Uint8Array([1]))
  controller.handleThumbnailData(2, new Uint8Array([2]))
  controller.handleThumbnailData(1, new Uint8Array([3]))
  controller.handleThumbnailData(0, new Uint8Array([4]))

  assert.equal(controller.state.loadedThumbs, 2)
  assert.equal(controller.state.unavailableThumbs, 1)
  assert.deepEqual([...controller.state.pendingSuccessfulIndices], [])
})

test('thumbnail batch error rolls back the exact request and permits retry', () => {
  const requests = []
  let clickHandler
  const root = fakeRoot()
  root.addEventListener = (event, handler) => { if (event === 'click') clickHandler = handler }
  const loadMore = { disabled: false, hidden: false, setAttribute() {} }
  root.querySelector = (selector) => selector === '.gallery-load-more' ? loadMore : null
  const controller = createGalleryController({ root, onThumbnailBatchRequest: (request) => { requests.push(request); return true } })
  controller.handleThumbnailList({ thumbnailMode: 'pull-v1', items: galleryItems(240) })

  assert.equal(controller.handleThumbnailBatchError({ request_id: 'stale' }), false)
  assert.equal(controller.handleThumbnailBatchError({ request_id: requests[0].request_id }), true)
  assert.equal(controller.state.inFlight, null)
  clickHandler({ preventDefault() {}, target: { closest: (selector) => selector === '.gallery-load-more' ? loadMore : null } })
  assert.deepEqual(requests.map(({ start, count }) => ({ start, count })), [
    { start: 0, count: 120 },
    { start: 0, count: 120 },
  ])
})

test('thumbnail_complete after a pull batch error switches to legacy eager handling', () => {
  const controller = createGalleryController({
    root: fakeRoot(),
    createObjectURL: () => 'blob:legacy',
    revokeObjectURL: () => {},
    onThumbnailBatchRequest: () => true,
  })
  controller.handleThumbnailList({ thumbnailMode: 'pull-v1', items: galleryItems(3) })
  controller.handleThumbnailBatchError({ request_id: controller.state.inFlight.requestId })
  controller.handleThumbnailData(2, new Uint8Array([1]))
  controller.handleThumbnailComplete({ sent: 2, failed: 1 })

  assert.equal(controller.state.thumbnailMode, '')
  assert.equal(controller.state.loadedThumbs, 1)
  assert.equal(controller.state.unavailableThumbs, 1)
})

test('legacy fallback frames that overtake pull rejection are accepted for the current gallery', () => {
  const requests = []
  const controller = createGalleryController({
    root: fakeRoot(),
    createObjectURL: () => 'blob:fallback',
    revokeObjectURL: () => {},
    onThumbnailBatchRequest: (request) => { requests.push(request); return true },
  })
  controller.handleThumbnailList({ thumbnailMode: 'pull-v1', items: galleryItems(240) })

  controller.handleThumbnailData(130, new Uint8Array([1, 2, 3]))
  controller.handleThumbnailData(130, new Uint8Array([4]))
  controller.handleThumbnailData(999, new Uint8Array([5]))
  assert.equal(controller.state.loadedThumbs, 1)
  assert.equal(controller.state.urls.get('thumb:asset-130'), 'blob:fallback')

  controller.handleThumbnailBatchError({ request_id: requests[0].request_id })

  assert.equal(controller.state.loadedThumbs, 1)
  assert.equal(controller.state.urls.get('thumb:asset-130'), 'blob:fallback')
})

test('valid pull completion incorporates an early current-gallery frame without duplicating it', () => {
  const requests = []
  const controller = createGalleryController({
    root: fakeRoot(),
    createObjectURL: () => 'blob:stale',
    revokeObjectURL: () => {},
    onThumbnailBatchRequest: (request) => { requests.push(request); return true },
  })
  controller.handleThumbnailList({ thumbnailMode: 'pull-v1', items: galleryItems(240) })
  controller.handleThumbnailData(130, new Uint8Array([1]))
  controller.handleThumbnailBatchComplete({ ...requests[0], sent: 120, failed: 0, failed_indices: [] })
  controller.handleThumbnailData(130, new Uint8Array([2]))

  assert.equal(controller.state.loadedThumbs, 1)
  assert.equal(controller.state.urls.get('thumb:asset-130'), 'blob:stale')
})

test('dynamic lightbox contains the complete album and opens a tile by global index', () => {
  let options
  const opened = []
  let clickHandler
  const grid = { addEventListener() {}, removeEventListener() {} }
  const root = fakeRoot()
  root.addEventListener = (event, handler) => { if (event === 'click') clickHandler = handler }
  root.querySelector = (selector) => selector === '.gallery-grid' ? grid : null
  const controller = createGalleryController({
    root,
    lightGallery: (_container, received) => {
      options = received
      return { galleryItems: received.dynamicEl.map((item) => ({ ...item })), openGallery: (index) => opened.push(index), destroy() {} }
    },
    onThumbnailBatchRequest: () => {},
  })
  controller.handleThumbnailList({ thumbnailMode: 'pull-v1', items: galleryItems(2453) })

  clickHandler({
    target: {
      closest: (selector) => selector === '[data-gallery-index]'
        ? { dataset: { galleryIndex: '130', galleryId: 'asset-130' } }
        : null,
    },
  })

  assert.equal(options.dynamic, true)
  assert.equal(options.download, false)
  assert.equal(options.dynamicEl.length, 2453)
  assert.deepEqual(opened, [130])
})

test('dynamic lightbox entries use escaped placeholders and the existing video schema', () => {
  let options
  const grid = { addEventListener() {}, removeEventListener() {} }
  const root = fakeRoot()
  root.querySelector = (selector) => selector === '.gallery-grid' ? grid : null
  const controller = createGalleryController({
    root,
    lightGallery: (_container, received) => {
      options = received
      return { galleryItems: received.dynamicEl, destroy() {} }
    },
  })
  controller.handleThumbnailList({ items: [
    { id: 'image-1', name: '<photo & sun>.jpg', mimeType: 'image/jpeg' },
    { id: 'video-1', name: '<clip>.mp4', mimeType: 'video/mp4' },
  ] })

  assert.match(options.dynamicEl[0].src, /^data:image\/gif;base64,/)
  assert.equal(options.dynamicEl[0].thumb, options.dynamicEl[0].src)
  assert.equal(options.dynamicEl[0].alt, '&lt;photo &amp; sun&gt;.jpg')
  assert.equal(options.dynamicEl[0].subHtml, '<p>&lt;photo &amp; sun&gt;.jpg</p>')
  assert.equal(options.dynamicEl[0].downloadUrl, 'false')
  assert.equal(options.dynamicEl[1].poster, options.dynamicEl[1].src)
  assert.deepEqual(JSON.parse(options.dynamicEl[1].video), [
    { src: options.dynamicEl[1].src, type: 'video/mp4' },
  ])
})

test('gallery summary renders Download All immediately after an escaped item count', () => {
  const html = renderGalleryShell('<Summer & Sun>', 'Beach <script>alert(1)</script>', [
    { id: 'asset-1', name: '<photo>.jpg', mimeType: 'image/jpeg' },
  ])

  assert.match(html, /<div class="gallery-summary">\s*<span>1 item<\/span>\s*<button class="gallery-download-all" type="button"[^>]*>Download All<\/button>/)
  assert.match(html, /aria-describedby="gallery-download-all-status"/)
  assert.match(html, /aria-busy="false"/)
  assert.match(html, /<span id="gallery-download-all-status" class="gallery-download-all-status" role="status" aria-live="polite">Ready to download album<\/span>/)
  assert.doesNotMatch(html, /<Summer & Sun>|<script>|<photo>/)
  assert.match(html, /&lt;Summer &amp; Sun&gt;/)
  assert.match(html, /Beach &lt;script&gt;alert\(1\)&lt;\/script&gt;/)
  assert.match(html, /&lt;photo&gt;\.jpg/)
})

test('gallery disables Download All for an empty album', () => {
  const html = renderGalleryShell('Empty', '', [])

  assert.match(html, /<button class="gallery-download-all"[^>]* disabled>Download All<\/button>/)
})

test('gallery Download All click requests the album without opening an item', () => {
  const albumRequests = []
  const previews = []
  const downloads = []
  let clickHandler = null
  const root = fakeRoot()
  root.addEventListener = (event, handler) => {
    if (event === 'click') clickHandler = handler
  }
  createGalleryController({
    root,
    createObjectURL: () => 'blob:thumb',
    revokeObjectURL: () => {},
    onAlbumDownloadRequest: () => albumRequests.push('album'),
    onPreviewRequest: (id) => previews.push(id),
    onDownloadRequest: (id) => downloads.push(id),
  })

  let defaultPrevented = false
  let propagationStopped = false
  clickHandler?.({
    preventDefault: () => { defaultPrevented = true },
    stopPropagation: () => { propagationStopped = true },
    target: {
      closest: (selector) => selector === '.gallery-download-all' ? {} : null,
    },
  })

  assert.equal(defaultPrevented, true)
  assert.equal(propagationStopped, true)
  assert.deepEqual(albumRequests, ['album'])
  assert.deepEqual(previews, [])
  assert.deepEqual(downloads, [])
})

test('gallery controller renders and announces album download lifecycle state', () => {
  const button = {
    textContent: '',
    disabled: false,
    attributes: {},
    setAttribute(name, value) { this.attributes[name] = value },
  }
  const liveStatus = { textContent: '' }
  const root = fakeRoot()
  root.querySelector = (selector) => {
    if (selector === '.gallery-download-all') return button
    if (selector === '.gallery-download-all-status') return liveStatus
    return null
  }
  const controller = createGalleryController({ root })
  controller.handleThumbnailList({ items: [{ id: 'asset-1', name: 'photo.jpg', mimeType: 'image/jpeg' }] })

  controller.setAlbumDownloadState({ phase: 'idle' })
  assert.deepEqual({ label: button.textContent, disabled: button.disabled }, { label: 'Download All', disabled: false })
  assert.equal(button.attributes['aria-busy'], 'false')
  assert.equal(liveStatus.textContent, 'Ready to download album')

  controller.setAlbumDownloadState({ phase: 'starting' })
  assert.deepEqual({ label: button.textContent, disabled: button.disabled }, { label: 'Starting…', disabled: true })
  assert.equal(button.attributes['aria-busy'], 'true')
  assert.equal(liveStatus.textContent, 'Starting album download')

  controller.setAlbumDownloadState({ phase: 'downloading', partIndex: 1, partCount: 2 })
  assert.deepEqual({ label: button.textContent, disabled: button.disabled }, { label: 'Downloading 1/2', disabled: true })
  assert.equal(button.attributes['aria-busy'], 'true')
  assert.equal(liveStatus.textContent, 'Downloading album part 1 of 2')

  controller.setAlbumDownloadState({ phase: 'failed' })
  assert.deepEqual({ label: button.textContent, disabled: button.disabled }, { label: 'Retry Download', disabled: false })
  assert.equal(button.attributes['aria-busy'], 'false')
  assert.equal(liveStatus.textContent, 'Album download failed')

  controller.setAlbumDownloadState({ phase: 'complete' })
  assert.deepEqual({ label: button.textContent, disabled: button.disabled }, { label: 'Download Complete', disabled: false })
  assert.equal(button.attributes['aria-busy'], 'false')
  assert.equal(liveStatus.textContent, 'Album download complete')
})

test('gallery renders thumbnail shells from thumbnail_list', () => {
  const root = fakeRoot()
  const controller = createGalleryController({
    root,
    createObjectURL: () => 'blob:thumb',
    revokeObjectURL: () => {},
    sendAssetRequest: () => {},
  })

  controller.handleThumbnailList({
    albumName: 'Summer',
    albumDescription: 'Beach',
    items: [{ id: 'asset-1', name: 'photo.jpg', mimeType: 'image/jpeg', width: 4000, height: 3000, size: 12, duration: null }],
  })

  assert.match(root.innerHTML, /Summer/)
  assert.match(root.innerHTML, /photo.jpg/)
})

test('gallery item clicks do not start an original asset request', () => {
  const sent = []
  const previewed = []
  let clickHandler = null
  const root = fakeRoot()
  root.addEventListener = (event, handler) => {
    if (event === 'click') clickHandler = handler
  }
  const controller = createGalleryController({
    root,
    createObjectURL: () => 'blob:thumb',
    revokeObjectURL: () => {},
    onPreviewRequest: (id) => previewed.push(id),
    onDownloadRequest: (id) => sent.push(id),
  })

  controller.handleThumbnailList({ albumName: 'Summer', items: [{ id: 'asset-1', name: 'photo.jpg', mimeType: 'image/jpeg' }] })
  clickHandler?.({
    target: {
      closest: (selector) => {
        if (selector === '[data-gallery-id]') return { dataset: { galleryId: 'asset-1' } }
        return null
      },
    },
  })

  assert.deepEqual(sent, [])
  assert.deepEqual(previewed, ['asset-1'])
})

test('gallery item clicks request the active preview and neighboring preloads', () => {
  const previewed = []
  let clickHandler = null
  const root = fakeRoot()
  root.addEventListener = (event, handler) => {
    if (event === 'click') clickHandler = handler
  }
  const controller = createGalleryController({
    root,
    createObjectURL: () => 'blob:thumb',
    revokeObjectURL: () => {},
    onPreviewRequest: (id, opts) => previewed.push({ id, priority: opts?.priority }),
  })

  controller.handleThumbnailList({
    albumName: 'Summer',
    items: [
      { id: 'asset-1', name: 'one.jpg', mimeType: 'image/jpeg' },
      { id: 'asset-2', name: 'two.jpg', mimeType: 'image/jpeg' },
      { id: 'asset-3', name: 'three.jpg', mimeType: 'image/jpeg' },
    ],
  })
  clickHandler?.({
    target: {
      closest: (selector) => {
        if (selector === '[data-gallery-id]') return { dataset: { galleryId: 'asset-2' } }
        return null
      },
    },
  })

  assert.deepEqual(previewed, [
    { id: 'asset-2', priority: 'active' },
    { id: 'asset-1', priority: 'preload' },
    { id: 'asset-3', priority: 'preload' },
  ])
})

test('gallery download button requests the original without opening preview', () => {
  const downloaded = []
  const previewed = []
  let clickHandler = null
  const root = fakeRoot()
  root.addEventListener = (event, handler) => {
    if (event === 'click') clickHandler = handler
  }
  const controller = createGalleryController({
    root,
    createObjectURL: () => 'blob:thumb',
    revokeObjectURL: () => {},
    onPreviewRequest: (id) => previewed.push(id),
    onDownloadRequest: (id) => downloaded.push(id),
  })

  controller.handleThumbnailList({ albumName: 'Summer', items: [{ id: 'asset-1', name: 'photo.jpg', mimeType: 'image/jpeg' }] })
  let defaultPrevented = false
  let propagationStopped = false
  clickHandler?.({
    preventDefault: () => { defaultPrevented = true },
    stopPropagation: () => { propagationStopped = true },
    target: {
      closest: (selector) => {
        if (selector === '.gallery-download') return { dataset: { galleryId: 'asset-1' } }
        return null
      },
    },
  })

  assert.equal(defaultPrevented, true)
  assert.equal(propagationStopped, true)
  assert.deepEqual(downloaded, ['asset-1'])
  assert.deepEqual(previewed, [])
})

test('gallery renders lightGallery source attributes on items', () => {
  const root = fakeRoot()
  const controller = createGalleryController({
    root,
    createObjectURL: () => 'blob:thumb',
    revokeObjectURL: () => {},
    sendAssetRequest: () => {},
  })

  controller.handleThumbnailList({
    albumName: 'Summer',
    items: [{ id: 'asset-1', name: 'photo.jpg', mimeType: 'image/jpeg' }],
  })

  assert.match(root.innerHTML, /data-src="data:image\/gif;base64,/)
  assert.match(root.innerHTML, /data-download-url="data:image\/gif;base64,/)
})

test('gallery updates lightGallery source attributes when thumbnail data arrives without refreshing per thumbnail', () => {
  const root = fakeRoot()
  const itemNode = { dataset: {} }
  const imgNode = { src: '' }
  let refreshed = 0
  root.querySelector = (selector) => {
    if (selector === '.gallery-grid') return {}
    if (selector === '[data-gallery-id="asset-1"]') return itemNode
    if (selector === '[data-thumb-id="asset-1"]') return imgNode
    return null
  }
  const controller = createGalleryController({
    root,
    lightGallery: () => ({ refresh: () => { refreshed += 1 }, destroy() {} }),
    createObjectURL: () => 'blob:thumb',
    revokeObjectURL: () => {},
    sendAssetRequest: () => {},
  })

  controller.handleThumbnailList({
    albumName: 'Summer',
    items: [{ id: 'asset-1', name: 'photo.jpg', mimeType: 'image/jpeg' }],
  })
  controller.handleThumbnailData(0, new Uint8Array([1]))

  assert.equal(imgNode.src, 'blob:thumb')
  assert.equal(itemNode.dataset.src, 'blob:thumb')
  assert.equal(itemNode.dataset.downloadUrl, 'blob:thumb')
  assert.equal(refreshed, 0)
})

test('gallery updates dynamic lightGallery items without scheduling a DOM refresh', () => {
  const root = fakeRoot()
  const itemNodes = new Map([
    ['asset-1', { dataset: {} }],
    ['asset-2', { dataset: {} }],
  ])
  const imgNodes = new Map([
    ['asset-1', { src: '' }],
    ['asset-2', { src: '' }],
  ])
  let refreshed = 0
  const galleryItems = [{}, {}]
  const timers = []
  root.querySelector = (selector) => {
    if (selector === '.gallery-grid') return {}
    const galleryMatch = selector.match(/^\[data-gallery-id="(.+)"\]$/)
    if (galleryMatch) return itemNodes.get(galleryMatch[1]) || null
    const thumbMatch = selector.match(/^\[data-thumb-id="(.+)"\]$/)
    if (thumbMatch) return imgNodes.get(thumbMatch[1]) || null
    return null
  }
  const controller = createGalleryController({
    root,
    lightGallery: () => ({ galleryItems, refresh: () => { refreshed += 1 }, destroy() {} }),
    createObjectURL: () => `blob:thumb-${timers.length}`,
    revokeObjectURL: () => {},
    setRefreshTimeout: (fn, delay) => {
      timers.push({ fn, delay })
      return timers.length
    },
    clearRefreshTimeout: () => {},
  })

  controller.handleThumbnailList({
    albumName: 'Summer',
    items: [
      { id: 'asset-1', name: 'one.jpg', mimeType: 'image/jpeg' },
      { id: 'asset-2', name: 'two.jpg', mimeType: 'image/jpeg' },
    ],
  })
  controller.handleThumbnailData(0, new Uint8Array([1]))
  controller.handleThumbnailData(1, new Uint8Array([2]))

  assert.equal(refreshed, 0)
  assert.equal(timers.length, 0)
  assert.equal(galleryItems[0].src, 'blob:thumb-0')
  assert.equal(galleryItems[1].src, 'blob:thumb-0')
})

test('gallery preview data upgrades the active lightbox image', () => {
  let clickHandler = null
  const root = fakeRoot()
  const itemNode = { dataset: { src: 'blob:thumb' } }
  const activeImage = { src: 'blob:thumb' }
  root.addEventListener = (event, handler) => {
    if (event === 'click') clickHandler = handler
  }
  root.querySelector = (selector) => {
    if (selector === '.gallery-grid') return {}
    if (selector === '[data-gallery-id="asset-1"]') return itemNode
    return null
  }
  const controller = createGalleryController({
    root,
    lightGallery: () => ({ refresh() {}, destroy() {} }),
    createObjectURL: () => 'blob:preview',
    revokeObjectURL: () => {},
    lightboxRoot: { querySelector: () => activeImage },
  })

  controller.handleThumbnailList({
    albumName: 'Summer',
    items: [{ id: 'asset-1', name: 'photo.jpg', mimeType: 'image/jpeg' }],
  })
  clickHandler?.({
    target: {
      closest: (selector) => {
        if (selector === '[data-gallery-id]') return { dataset: { galleryId: 'asset-1' } }
        return null
      },
    },
  })
  controller.handlePreviewData('asset-1', new Uint8Array([1, 2, 3]), 'image/jpeg')

  assert.equal(itemNode.dataset.src, 'blob:preview')
  assert.equal(activeImage.src, 'blob:preview')
})

test('gallery rebuilds a video player when lightGallery replaced the slide with an error', () => {
  const root = fakeRoot()
  const galleryItem = { dataset: {} }
  const currentSlide = {
    innerHTML: '<span class="lg-error-msg">Oops... Failed to load content...</span>',
    children: [],
    appendChild(node) { this.children.push(node) },
  }
  const createElement = (tagName) => ({
    tagName: tagName.toUpperCase(),
    className: '',
    children: [],
    style: {},
    appendChild(node) { this.children.push(node) },
    querySelector(selector) {
      return selector === 'video.lg-video'
        ? this.children.find((node) => node.tagName === 'VIDEO') || null
        : null
    },
    setAttribute() {},
  })

  root.querySelector = (selector) => {
    if (selector === '.gallery-grid') return {}
    if (selector === '[data-gallery-id="video-1"]') return galleryItem
    return null
  }

  const controller = createGalleryController({
    root,
    lightGallery: () => ({ galleryItems: [{}], refresh() {}, destroy() {} }),
    revokeObjectURL: () => {},
    lightboxRoot: {
      querySelector(selector) {
        if (selector === '.lg-current .lg-img-wrap') return null
        if (selector === '.lg-current') return currentSlide
        return null
      },
      createElement,
    },
  })

  controller.handleThumbnailList({
    albumName: 'Summer',
    items: [{ id: 'video-1', name: 'clip.mp4', mimeType: 'video/mp4' }],
  })
  controller.state.activePreviewID = 'video-1'
  controller.handlePreviewData('video-1', new Uint8Array(0), 'video/mp4')

  const wrapper = currentSlide.children[0]
  assert.equal(wrapper?.className, 'lg-img-wrap')
  assert.equal(wrapper?.children[0]?.className, 'lg-object lg-video')
  assert.match(wrapper?.children[0]?.src, /^\/media\/video-1\?play=\d+$/)
})

test('gallery does not mount a video into a different current slide during navigation', () => {
  const root = fakeRoot()
  const galleryItem = { dataset: {} }
  const imageNode = { tagName: 'IMG' }
  const wrapper = {
    children: [imageNode],
    style: {},
    querySelector: () => null,
    appendChild(node) { this.children.push(node) },
    set innerHTML(value) {
      if (value === '') this.children = []
    },
  }
  const currentSlide = {
    id: 'lg-item-1-1',
    dataset: { index: '1' },
    querySelector(selector) {
      return selector === '.lg-img-wrap' ? wrapper : null
    },
  }
  const createElement = (tagName) => ({
    tagName: tagName.toUpperCase(),
    className: '',
    style: {},
    setAttribute() {},
  })

  root.querySelector = (selector) => {
    if (selector === '.gallery-grid') return {}
    if (selector === '[data-gallery-id="video-1"]') return galleryItem
    return null
  }

  const controller = createGalleryController({
    root,
    lightGallery: () => ({ galleryItems: [{}, {}], refresh() {}, destroy() {} }),
    revokeObjectURL: () => {},
    lightboxRoot: {
      querySelector(selector) {
        if (selector === '.lg-current .lg-img-wrap') return wrapper
        if (selector === '.lg-current') return currentSlide
        return null
      },
      createElement,
    },
  })

  controller.handleThumbnailList({
    albumName: 'Summer',
    items: [
      { id: 'video-1', name: 'clip.mp4', mimeType: 'video/mp4' },
      { id: 'image-1', name: 'photo.jpg', mimeType: 'image/jpeg' },
    ],
  })
  controller.state.activePreviewID = 'video-1'
  controller.handlePreviewData('video-1', new Uint8Array(0), 'video/mp4')

  assert.deepEqual(wrapper.children, [imageNode])
})

test('gallery gives each rebuilt video element a fresh playback URL', () => {
  const root = fakeRoot()
  const galleryItem = { dataset: {} }
  let slideHandler = null
  let wrapper = makeVideoWrapper()
  const currentSlide = {
    id: 'lg-item-1-0',
    querySelector(selector) {
      return selector === '.lg-img-wrap' ? wrapper : null
    },
  }
  const grid = {
    addEventListener(event, handler) {
      if (event === 'lgAfterSlide') slideHandler = handler
    },
    removeEventListener() {},
  }
  const createElement = (tagName) => ({
    tagName: tagName.toUpperCase(),
    className: '',
    dataset: {},
    style: {},
    setAttribute() {},
  })

  root.querySelector = (selector) => {
    if (selector === '.gallery-grid') return grid
    if (selector === '[data-gallery-id="video-1"]') return galleryItem
    return null
  }

  const controller = createGalleryController({
    root,
    lightGallery: () => ({ galleryItems: [{}], refresh() {}, destroy() {} }),
    revokeObjectURL: () => {},
    lightboxRoot: {
      querySelector(selector) {
        if (selector === '.lg-current .lg-img-wrap') return wrapper
        if (selector === '.lg-current') return currentSlide
        return null
      },
      createElement,
    },
  })

  controller.handleThumbnailList({
    albumName: 'Summer',
    items: [{ id: 'video-1', name: 'clip.mp4', mimeType: 'video/mp4' }],
  })
  controller.state.activePreviewID = 'video-1'
  controller.handlePreviewData('video-1', new Uint8Array(0), 'video/mp4')
  const firstURL = wrapper.children[0].src

  wrapper = makeVideoWrapper()
  slideHandler?.({ detail: { index: 0 } })
  const secondURL = wrapper.children[0].src

  assert.match(firstURL, /^\/media\/video-1/)
  assert.match(secondURL, /^\/media\/video-1/)
  assert.notEqual(secondURL, firstURL)
})

test('closing a video invalidates its cached preview so reopening requests a fresh stream', () => {
  const previewed = []
  const closed = []
  let clickHandler = null
  let closeHandler = null
  const root = fakeRoot()
  const grid = {
    addEventListener(event, handler) {
      if (event === 'lgAfterClose') closeHandler = handler
    },
    removeEventListener() {},
  }
  root.addEventListener = (event, handler) => {
    if (event === 'click') clickHandler = handler
  }
  root.querySelector = (selector) => selector === '.gallery-grid' ? grid : null

  const controller = createGalleryController({
    root,
    lightGallery: () => ({ refresh() {}, destroy() {} }),
    revokeObjectURL: () => {},
    onPreviewRequest: (id) => previewed.push(id),
    onPreviewClose: (id) => closed.push(id),
  })
  controller.handleThumbnailList({
    albumName: 'Summer',
    items: [{ id: 'video-1', name: 'clip.mp4', mimeType: 'video/mp4' }],
  })
  controller.state.activePreviewID = 'video-1'
  controller.state.urls.set('preview:video-1', '/media/video-1')

  closeHandler?.()
  clickHandler?.({
    target: {
      closest: (selector) => selector === '[data-gallery-id]'
        ? { dataset: { galleryId: 'video-1' } }
        : null,
    },
  })

  assert.deepEqual(closed, ['video-1'])
  assert.deepEqual(previewed, ['video-1'])
})

function makeVideoWrapper() {
  return {
    children: [],
    style: {},
    querySelector(selector) {
      return selector === 'video.lg-video'
        ? this.children.find((node) => node.tagName === 'VIDEO') || null
        : null
    },
    appendChild(node) { this.children.push(node) },
    set innerHTML(value) {
      if (value === '') this.children = []
    },
  }
}

test('gallery requests active and neighboring previews when lightGallery slide changes', () => {
  const previewed = []
  let slideHandler = null
  const root = fakeRoot()
  const grid = {
    addEventListener(event, handler) {
      if (event === 'lgAfterSlide') slideHandler = handler
    },
    removeEventListener() {},
  }
  root.querySelector = (selector) => {
    if (selector === '.gallery-grid') return grid
    return null
  }
  const controller = createGalleryController({
    root,
    lightGallery: () => ({ refresh() {}, destroy() {} }),
    createObjectURL: () => 'blob:thumb',
    revokeObjectURL: () => {},
    onPreviewRequest: (id, opts) => previewed.push({ id, priority: opts?.priority }),
  })

  controller.handleThumbnailList({
    albumName: 'Summer',
    items: [
      { id: 'asset-1', name: 'one.jpg', mimeType: 'image/jpeg' },
      { id: 'asset-2', name: 'two.jpg', mimeType: 'image/jpeg' },
      { id: 'asset-3', name: 'three.jpg', mimeType: 'image/jpeg' },
    ],
  })
  slideHandler?.({ detail: { index: 1 } })

  assert.deepEqual(previewed, [
    { id: 'asset-2', priority: 'active' },
    { id: 'asset-1', priority: 'preload' },
    { id: 'asset-3', priority: 'preload' },
  ])
  assert.equal(controller.state.activePreviewID, 'asset-2')
})

test('gallery pauses every outgoing video before slide navigation', () => {
  let beforeSlideHandler = null
  const pauseCounts = [0, 0]
  const videos = pauseCounts.map((_, index) => ({
    currentTime: 12 + index,
    pause() { pauseCounts[index] += 1 },
  }))
  const root = fakeRoot()
  const grid = {
    addEventListener(event, handler) {
      if (event === 'lgBeforeSlide') beforeSlideHandler = handler
    },
    removeEventListener() {},
  }
  root.querySelector = (selector) => selector === '.gallery-grid' ? grid : null
  const controller = createGalleryController({
    root,
    lightGallery: () => ({ refresh() {}, destroy() {} }),
    lightboxRoot: {
      querySelectorAll(selector) {
        return selector === '.lg-current video' ? videos : []
      },
    },
  })

  controller.handleThumbnailList({
    items: [
      { id: 'video-1', mimeType: 'video/mp4' },
      { id: 'image-1', mimeType: 'image/jpeg' },
    ],
  })
  beforeSlideHandler?.({ detail: { index: 1 } })

  assert.deepEqual(pauseCounts, [1, 1])
  assert.deepEqual(videos.map((video) => video.currentTime), [12, 13])
})

test('gallery pauses every outgoing video before close', () => {
  let beforeCloseHandler = null
  const pauseCounts = [0, 0]
  const videos = pauseCounts.map((_, index) => ({
    currentTime: 30 + index,
    pause() { pauseCounts[index] += 1 },
  }))
  const root = fakeRoot()
  const grid = {
    addEventListener(event, handler) {
      if (event === 'lgBeforeClose') beforeCloseHandler = handler
    },
    removeEventListener() {},
  }
  root.querySelector = (selector) => selector === '.gallery-grid' ? grid : null
  const controller = createGalleryController({
    root,
    lightGallery: () => ({ refresh() {}, destroy() {} }),
    lightboxRoot: {
      querySelectorAll(selector) {
        return selector === '.lg-current video' ? videos : []
      },
    },
  })

  controller.handleThumbnailList({
    items: [{ id: 'video-1', mimeType: 'video/mp4' }],
  })
  beforeCloseHandler?.()

  assert.equal(typeof beforeCloseHandler, 'function')
  assert.deepEqual(pauseCounts, [1, 1])
  assert.deepEqual(videos.map((video) => video.currentTime), [30, 31])
})

test('gallery starts preview work before slide transition completes without duplicate after-slide requests', () => {
  const previewed = []
  let beforeSlideHandler = null
  let afterSlideHandler = null
  const root = fakeRoot()
  const grid = {
    addEventListener(event, handler) {
      if (event === 'lgBeforeSlide') beforeSlideHandler = handler
      if (event === 'lgAfterSlide') afterSlideHandler = handler
    },
    removeEventListener() {},
  }
  root.querySelector = (selector) => {
    if (selector === '.gallery-grid') return grid
    return null
  }
  const controller = createGalleryController({
    root,
    lightGallery: () => ({ refresh() {}, destroy() {} }),
    createObjectURL: () => 'blob:thumb',
    revokeObjectURL: () => {},
    onPreviewRequest: (id, opts) => previewed.push({ id, priority: opts?.priority }),
  })

  controller.handleThumbnailList({
    albumName: 'Summer',
    items: [
      { id: 'asset-1', name: 'one.jpg', mimeType: 'image/jpeg' },
      { id: 'asset-2', name: 'two.jpg', mimeType: 'image/jpeg' },
      { id: 'asset-3', name: 'three.jpg', mimeType: 'image/jpeg' },
    ],
  })
  assert.equal(typeof beforeSlideHandler, 'function')
  assert.equal(typeof afterSlideHandler, 'function')

  beforeSlideHandler({ detail: { index: 1 } })
  assert.deepEqual(previewed, [
    { id: 'asset-2', priority: 'active' },
    { id: 'asset-1', priority: 'preload' },
    { id: 'asset-3', priority: 'preload' },
  ])

  afterSlideHandler({ detail: { index: 1 } })

  assert.deepEqual(previewed, [
    { id: 'asset-2', priority: 'active' },
    { id: 'asset-1', priority: 'preload' },
    { id: 'asset-3', priority: 'preload' },
  ])
  assert.equal(controller.state.activePreviewID, 'asset-2')
})

test('gallery uses an already preloaded preview when swiping to that slide', () => {
  const previewed = []
  let slideHandler = null
  let nextUrl = 1
  const root = fakeRoot()
  const activeImage = { src: 'blob:thumb-2' }
  const itemNodes = new Map([
    ['asset-1', { dataset: { src: 'blob:thumb-1' } }],
    ['asset-2', { dataset: { src: 'blob:thumb-2' } }],
  ])
  const grid = {
    addEventListener(event, handler) {
      if (event === 'lgAfterSlide') slideHandler = handler
    },
    removeEventListener() {},
  }
  root.querySelector = (selector) => {
    if (selector === '.gallery-grid') return grid
    const galleryMatch = selector.match(/^\[data-gallery-id="(.+)"\]$/)
    if (galleryMatch) return itemNodes.get(galleryMatch[1]) || null
    return null
  }
  const controller = createGalleryController({
    root,
    lightGallery: () => ({ refresh() {}, destroy() {} }),
    createObjectURL: () => `blob:preview-${nextUrl++}`,
    revokeObjectURL: () => {},
    lightboxRoot: {
      querySelector(selector) {
        if (selector === '.lg-current .lg-object') return activeImage
        return null
      },
    },
    onPreviewRequest: (id, opts) => previewed.push({ id, priority: opts?.priority }),
  })

  controller.handleThumbnailList({
    albumName: 'Summer',
    items: [
      { id: 'asset-1', name: 'one.jpg', mimeType: 'image/jpeg' },
      { id: 'asset-2', name: 'two.jpg', mimeType: 'image/jpeg' },
    ],
  })
  controller.handlePreviewData('asset-2', new Uint8Array([2]), 'image/jpeg')

  assert.equal(activeImage.src, 'blob:thumb-2')

  slideHandler?.({ detail: { index: 1 } })

  assert.equal(activeImage.src, 'blob:preview-1')
  assert.deepEqual(previewed, [
    { id: 'asset-1', priority: 'preload' },
  ])
  assert.equal(controller.state.activePreviewID, 'asset-2')
})

test('gallery preview data upgrades rendered neighboring lightbox slide images', () => {
  let nextUrl = 1
  const root = fakeRoot()
  const itemNodes = new Map([
    ['asset-1', { dataset: { src: 'blob:thumb-1' } }],
    ['asset-2', { dataset: { src: 'blob:thumb-2' } }],
  ])
  const neighborImage = { src: 'blob:thumb-2' }
  root.querySelector = (selector) => {
    if (selector === '.gallery-grid') return {}
    const galleryMatch = selector.match(/^\[data-gallery-id="(.+)"\]$/)
    if (galleryMatch) return itemNodes.get(galleryMatch[1]) || null
    return null
  }
  const lightbox = {
    galleryItems: [
      { src: 'blob:thumb-1', downloadUrl: 'blob:thumb-1' },
      { src: 'blob:thumb-2', downloadUrl: 'blob:thumb-2' },
    ],
    refresh() {},
    destroy() {},
  }
  const controller = createGalleryController({
    root,
    lightGallery: () => lightbox,
    createObjectURL: () => `blob:preview-${nextUrl++}`,
    revokeObjectURL: () => {},
    lightboxRoot: {
      querySelector(selector) {
        if (selector === '.lg-current .lg-object') return null
        return null
      },
      querySelectorAll(selector) {
        if (selector === '.lg-object[data-index="1"]') return [neighborImage]
        return []
      },
    },
  })

  controller.handleThumbnailList({
    albumName: 'Summer',
    items: [
      { id: 'asset-1', name: 'one.jpg', mimeType: 'image/jpeg' },
      { id: 'asset-2', name: 'two.jpg', mimeType: 'image/jpeg' },
    ],
  })
  controller.handlePreviewData('asset-2', new Uint8Array([2]), 'image/jpeg')

  assert.equal(neighborImage.src, 'blob:preview-1')
  assert.equal(lightbox.galleryItems[1].src, 'blob:preview-1')
  assert.equal(lightbox.galleryItems[1].downloadUrl, 'false')
})

test('gallery adds a lightbox download button for the active asset', () => {
  const downloaded = []
  let clickHandler = null
  const root = fakeRoot()
  root.addEventListener = (event, handler) => {
    if (event === 'click') clickHandler = handler
  }
  root.querySelector = (selector) => {
    if (selector === '.gallery-grid') return {}
    return null
  }
  const toolbar = {
    children: [],
    querySelector() { return null },
    appendChild(node) { this.children.push(node) },
  }
  const lightboxRoot = {
    querySelector(selector) {
      if (selector === '.lg-toolbar') return toolbar
      return null
    },
    createElement(tagName) {
      return {
        tagName,
        className: '',
        textContent: '',
        title: '',
        type: '',
        setAttribute(name, value) { this[name] = value },
        addEventListener(event, handler) {
          if (event === 'click') this.click = handler
        },
      }
    },
  }
  const controller = createGalleryController({
    root,
    lightGallery: () => ({ refresh() {}, destroy() {} }),
    createObjectURL: () => 'blob:thumb',
    revokeObjectURL: () => {},
    lightboxRoot,
    onDownloadRequest: (id) => downloaded.push(id),
  })

  controller.handleThumbnailList({ albumName: 'Summer', items: [{ id: 'asset-1', name: 'photo.jpg', mimeType: 'image/jpeg' }] })
  clickHandler?.({
    target: {
      closest: (selector) => {
        if (selector === '[data-gallery-id]') return { dataset: { galleryId: 'asset-1' } }
        return null
      },
    },
  })
  toolbar.children[0].click({ preventDefault() {} })

  assert.equal(toolbar.children[0].className, 'lg-icon lg-download sharebridge-lightbox-download')
  assert.equal(toolbar.children[0].textContent, '')
  assert.equal(toolbar.children[0]['aria-label'], 'Download')
  assert.deepEqual(downloaded, ['asset-1'])
})

test('gallery renders thumbnails with browser rendering hints', () => {
  const root = fakeRoot()
  const controller = createGalleryController({
    root,
    createObjectURL: () => 'blob:thumb',
    revokeObjectURL: () => {},
    sendAssetRequest: () => {},
  })

  controller.handleThumbnailList({
    albumName: 'Summer',
    items: [{ id: 'asset-1', name: 'photo.jpg', mimeType: 'image/jpeg' }],
  })

  assert.match(root.innerHTML, /loading="lazy"/)
  assert.match(root.innerHTML, /decoding="async"/)
})

test('gallery tracks thumbnail loading progress', () => {
  const root = fakeRoot()
  const progressText = { textContent: '' }
  const progressFill = { style: { width: '' } }
  const progress = {
    classList: {
      added: [],
      removed: [],
      add(name) { this.added.push(name) },
      remove(name) { this.removed.push(name) },
    },
  }
  root.querySelector = (selector) => {
    if (selector === '.gallery-progress-text') return progressText
    if (selector === '.gallery-progress-fill') return progressFill
    if (selector === '.gallery-progress') return progress
    return null
  }
  const controller = createGalleryController({
    root,
    createObjectURL: () => 'blob:thumb',
    revokeObjectURL: () => {},
    sendAssetRequest: () => {},
  })

  controller.handleThumbnailList({
    albumName: 'Summer',
    items: [
      { id: 'asset-1', name: 'one.jpg', mimeType: 'image/jpeg' },
      { id: 'asset-2', name: 'two.jpg', mimeType: 'image/jpeg' },
    ],
  })

  assert.match(root.innerHTML, /gallery-progress/)
  assert.equal(progressText.textContent, '0 / 2 thumbnails')
  assert.equal(progressFill.style.width, '0%')

  controller.handleThumbnailData(1, new Uint8Array([1]))
  assert.equal(progressText.textContent, '1 / 2 thumbnails')
  assert.equal(progressFill.style.width, '50%')
  assert.deepEqual(progress.classList.added, [])

  controller.handleThumbnailData(1, new Uint8Array([2]))
  assert.equal(progressText.textContent, '1 / 2 thumbnails')

  controller.handleThumbnailData(0, new Uint8Array([3]))
  assert.equal(progressText.textContent, '2 / 2 thumbnails')
  assert.equal(progressFill.style.width, '100%')
  assert.deepEqual(progress.classList.added, ['hidden'])
})

test('gallery completes thumbnail progress when the agent reports unavailable thumbnails', () => {
  const root = fakeRoot()
  const progressText = { textContent: '' }
  const progressFill = { style: { width: '' } }
  const progress = {
    classList: {
      added: [],
      remove() {},
      add(name) { this.added.push(name) },
    },
  }
  root.querySelector = (selector) => {
    if (selector === '.gallery-progress-text') return progressText
    if (selector === '.gallery-progress-fill') return progressFill
    if (selector === '.gallery-progress') return progress
    return null
  }
  const controller = createGalleryController({ root, createObjectURL: () => 'blob:thumb', revokeObjectURL: () => {} })

  controller.handleThumbnailList({
    albumName: 'Summer',
    items: [
      { id: 'asset-1', name: 'one.jpg', mimeType: 'image/jpeg' },
      { id: 'asset-2', name: 'missing.jpg', mimeType: 'image/jpeg' },
    ],
  })
  controller.handleThumbnailData(0, new Uint8Array([1]))
  controller.handleThumbnailComplete({ failed: 1 })

  assert.equal(progressText.textContent, '1 / 2 thumbnails (1 unavailable)')
  assert.equal(progressFill.style.width, '100%')
  assert.deepEqual(progress.classList.added, ['hidden'])
})

test('gallery initializes lightGallery when available', () => {
  let initialized = false
  let options = null
  const root = fakeRoot()
  root.querySelector = () => ({})
  const controller = createGalleryController({
    root,
    lightGallery: (_grid, receivedOptions) => {
      initialized = true
      options = receivedOptions
      return { destroy() {} }
    },
    createObjectURL: () => 'blob:thumb',
    revokeObjectURL: () => {},
    sendAssetRequest: () => {},
  })

  controller.handleThumbnailList({ albumName: 'Summer', items: [] })

  assert.equal(initialized, true)
  assert.equal(options.download, false)
})

test('gallery destroys lightGallery before rerendering and destroying', () => {
  const destroyed = []
  let nextLightbox = 1
  const root = fakeRoot()
  root.querySelector = () => ({})
  const controller = createGalleryController({
    root,
    lightGallery: () => {
      const id = nextLightbox++
      return { destroy: () => destroyed.push(id) }
    },
    createObjectURL: () => 'blob:thumb',
    revokeObjectURL: () => {},
    sendAssetRequest: () => {},
  })

  controller.handleThumbnailList({ albumName: 'Summer', items: [] })
  controller.handleThumbnailList({ albumName: 'Winter', items: [] })
  controller.destroy()

  assert.deepEqual(destroyed, [1, 2])
})

test('gallery teardown destroys the lightbox and observer before clearing timers and revoking owned URLs', () => {
  const events = []
  let nextURL = 1
  const grid = { addEventListener() {}, removeEventListener() {} }
  const sentinel = {}
  const root = fakeRoot()
  root.querySelector = (selector) => {
    if (selector === '.gallery-grid') return grid
    if (selector === '.gallery-sentinel') return sentinel
    return null
  }
  const controller = createGalleryController({
    root,
    lightGallery: (_grid, options) => ({
      galleryItems: options.dynamicEl.map((item) => ({ ...item })),
      destroy: () => events.push('destroy-lightbox'),
    }),
    createIntersectionObserver: () => ({ observe() {}, disconnect: () => events.push('disconnect-observer') }),
    createObjectURL: () => `blob:${nextURL++}`,
    revokeObjectURL: (url) => events.push(`revoke:${url}`),
    clearRefreshTimeout: () => events.push('clear-timer'),
    onThumbnailBatchRequest: () => {},
  })
  controller.handleThumbnailList({ thumbnailMode: 'pull-v1', items: galleryItems(2) })
  controller.handleThumbnailData(0, new Uint8Array([1]))
  controller.handlePreviewData('asset-0', new Uint8Array([2]), 'image/jpeg')
  controller.state.refreshTimer = 99
  events.length = 0

  controller.handleThumbnailList({ thumbnailMode: 'pull-v1', items: galleryItems(1) })

  assert.deepEqual(events, [
    'destroy-lightbox',
    'clear-timer',
    'disconnect-observer',
    'revoke:blob:1',
    'revoke:blob:2',
  ])
})

test('preview upgrades preserve the grid thumbnail and use separate URL ownership keys', () => {
  const img = { src: '' }
  const grid = { addEventListener() {}, removeEventListener() {} }
  const root = fakeRoot()
  root.querySelector = (selector) => {
    if (selector === '.gallery-grid') return grid
    if (selector === '[data-thumb-id="asset-0"]') return img
    return null
  }
  let nextURL = 1
  const lightbox = { galleryItems: [{}], destroy() {} }
  const controller = createGalleryController({
    root,
    lightGallery: () => lightbox,
    createObjectURL: () => `blob:${nextURL++}`,
    revokeObjectURL: () => {},
  })
  controller.handleThumbnailList({ items: galleryItems(1) })
  controller.handleThumbnailData(0, new Uint8Array([1]))
  controller.handlePreviewData('asset-0', new Uint8Array([2]), 'image/jpeg')

  assert.equal(img.src, 'blob:1')
  assert.equal(controller.state.urls.get('thumb:asset-0'), 'blob:1')
  assert.equal(controller.state.urls.get('preview:asset-0'), 'blob:2')
  assert.equal(lightbox.galleryItems[0].src, 'blob:2')
})

test('a new thumbnail list resets active preview and terminal range state', () => {
  const controller = createGalleryController({
    root: fakeRoot(),
    onThumbnailBatchRequest: () => {},
  })
  controller.handleThumbnailList({ thumbnailMode: 'pull-v1', items: galleryItems(2) })
  controller.state.activePreviewID = 'asset-0'
  controller.state._lastVideoID = 'asset-0'
  controller.state.terminalIndices.add(0)
  controller.state.unavailableIndices.add(0)
  controller.state.pendingExpansion = true

  controller.handleThumbnailList({ thumbnailMode: 'pull-v1', items: [{ id: 'new-1', mimeType: 'image/jpeg' }] })

  assert.equal(controller.state.activePreviewID, '')
  assert.equal(controller.state._lastVideoID, '')
  assert.equal(controller.state.pendingExpansion, false)
  assert.deepEqual([...controller.state.terminalIndices], [])
  assert.deepEqual([...controller.state.unavailableIndices], [])

  controller.state.pendingExpansion = true
  controller.destroy()
  assert.equal(controller.state.pendingExpansion, false)
})

test('gallery revokes a previous thumbnail URL when replacing it', () => {
  const revoked = []
  const controller = createGalleryController({
    root: fakeRoot(),
    createObjectURL: () => `blob:thumb-${controller.state.urls.size + 1}`,
    revokeObjectURL: (url) => revoked.push(url),
    sendAssetRequest: () => {},
  })

  controller.handleThumbnailList({
    albumName: 'Summer',
    items: [{ id: 'asset-1', name: 'photo.jpg', mimeType: 'image/jpeg' }],
  })
  controller.handleThumbnailData(0, new Uint8Array([1]))
  controller.handleThumbnailData(0, new Uint8Array([2]))

  assert.deepEqual(revoked, ['blob:thumb-1'])
})

test('gallery revokes thumbnail URLs when rerendering and destroying', () => {
  const revoked = []
  let nextUrl = 1
  const controller = createGalleryController({
    root: fakeRoot(),
    createObjectURL: () => `blob:thumb-${nextUrl++}`,
    revokeObjectURL: (url) => revoked.push(url),
    sendAssetRequest: () => {},
  })

  controller.handleThumbnailList({
    albumName: 'Summer',
    items: [{ id: 'asset-1', name: 'photo.jpg', mimeType: 'image/jpeg' }],
  })
  controller.handleThumbnailData(0, new Uint8Array([1]))
  controller.handleThumbnailList({
    albumName: 'Winter',
    items: [{ id: 'asset-2', name: 'snow.jpg', mimeType: 'image/jpeg' }],
  })

  assert.deepEqual(revoked, ['blob:thumb-1'])

  controller.handleThumbnailData(0, new Uint8Array([2]))
  controller.destroy()

  assert.deepEqual(revoked, ['blob:thumb-1', 'blob:thumb-2'])
})

test('gallery escapes labels and renders video duration badges', () => {
  const root = fakeRoot()
  const controller = createGalleryController({
    root,
    createObjectURL: () => 'blob:thumb',
    revokeObjectURL: () => {},
    sendAssetRequest: () => {},
  })

  controller.handleThumbnailList({
    albumName: '<Summer>',
    albumDescription: 'Beach & "sun"',
    items: [{ id: 'asset-1', name: '<clip>.mp4', mimeType: 'video/mp4', duration: 62 }],
  })

  assert.match(root.innerHTML, /&lt;Summer&gt;/)
  assert.match(root.innerHTML, /Beach &amp; &quot;sun&quot;/)
  assert.match(root.innerHTML, /&lt;clip&gt;.mp4/)
  assert.match(root.innerHTML, /1:02/)
})
