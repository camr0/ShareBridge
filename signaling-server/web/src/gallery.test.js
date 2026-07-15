import { test } from 'node:test'
import assert from 'node:assert/strict'
import { createGalleryController } from './gallery.js'

function fakeRoot() {
  return {
    innerHTML: '',
    children: [],
    appendChild(node) { this.children.push(node) },
    querySelector() { return null },
  }
}

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

test('gallery coalesces lightGallery refresh after thumbnail source updates', () => {
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
    lightGallery: () => ({ refresh: () => { refreshed += 1 }, destroy() {} }),
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
  assert.equal(timers.length, 1)
  assert.equal(timers[0].delay, 50)

  timers[0].fn()
  assert.equal(refreshed, 1)
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
