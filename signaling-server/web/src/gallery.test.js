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

test('gallery sends asset_request when an item is opened', () => {
  const sent = []
  const controller = createGalleryController({
    root: fakeRoot(),
    createObjectURL: () => 'blob:thumb',
    revokeObjectURL: () => {},
    sendAssetRequest: (id) => sent.push(id),
  })

  controller.handleThumbnailList({ albumName: 'Summer', items: [{ id: 'asset-1', name: 'photo.jpg', mimeType: 'image/jpeg' }] })
  controller.openItem('asset-1')

  assert.deepEqual(sent, ['asset-1'])
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

test('gallery updates lightGallery source attributes when thumbnail data arrives', () => {
  const root = fakeRoot()
  const itemNode = { dataset: {} }
  const imgNode = { src: '' }
  let refreshed = false
  root.querySelector = (selector) => {
    if (selector === '.gallery-grid') return {}
    if (selector === '[data-gallery-id="asset-1"]') return itemNode
    if (selector === '[data-thumb-id="asset-1"]') return imgNode
    return null
  }
  const controller = createGalleryController({
    root,
    lightGallery: () => ({ refresh: () => { refreshed = true }, destroy() {} }),
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
  assert.equal(refreshed, true)
})

test('gallery initializes lightGallery when available', () => {
  let initialized = false
  const root = fakeRoot()
  root.querySelector = () => ({})
  const controller = createGalleryController({
    root,
    lightGallery: () => {
      initialized = true
      return { destroy() {} }
    },
    createObjectURL: () => 'blob:thumb',
    revokeObjectURL: () => {},
    sendAssetRequest: () => {},
  })

  controller.handleThumbnailList({ albumName: 'Summer', items: [] })

  assert.equal(initialized, true)
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
