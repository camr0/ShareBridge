export function createGalleryController({
  root,
  lightGallery = globalThis.lightGallery,
  createObjectURL = URL.createObjectURL,
  revokeObjectURL = URL.revokeObjectURL,
  sendAssetRequest,
}) {
  const state = { items: [], thumbs: new Map(), urls: new Map(), lightbox: null }

  const handleClick = (event) => {
    const item = event.target?.closest?.('[data-gallery-id]')
    if (item?.dataset?.galleryId) openItem(item.dataset.galleryId)
  }

  root.addEventListener?.('click', handleClick)

  function handleThumbnailList(msg) {
    destroyLightbox()
    revokeAllUrls()
    state.items = msg.items || []
    state.thumbs.clear()
    root.innerHTML = renderGalleryShell(msg.albumName || 'Shared album', msg.albumDescription || '', state.items)
    initLightbox()
  }

  function handleThumbnailData(index, payload) {
    const item = state.items[index]
    if (!item) return
    const blob = new Blob([payload], { type: 'image/jpeg' })
    const url = createObjectURL(blob)
    const previousUrl = state.urls.get(item.id)
    if (previousUrl) revokeObjectURL(previousUrl)
    state.urls.set(item.id, url)
    const img = root.querySelector?.(`[data-thumb-id="${cssEscape(item.id)}"]`)
    if (img) img.src = url
    const galleryItem = root.querySelector?.(`[data-gallery-id="${cssEscape(item.id)}"]`)
    if (galleryItem?.dataset) {
      galleryItem.dataset.src = url
      galleryItem.dataset.downloadUrl = url
      state.lightbox?.refresh?.()
    }
  }

  function openItem(id) {
    if (sendAssetRequest) sendAssetRequest(id)
  }

  function destroy() {
    root.removeEventListener?.('click', handleClick)
    destroyLightbox()
    revokeAllUrls()
  }

  function initLightbox() {
    if (!lightGallery || !root.querySelector) return
    const grid = root.querySelector('.gallery-grid')
    if (!grid) return
    state.lightbox = lightGallery(grid, { selector: '.gallery-item', download: true })
  }

  function destroyLightbox() {
    state.lightbox?.destroy?.()
    state.lightbox = null
  }

  function revokeAllUrls() {
    for (const url of state.urls.values()) revokeObjectURL(url)
    state.urls.clear()
  }

  return { handleThumbnailList, handleThumbnailData, openItem, destroy, state }
}

export function renderGalleryShell(albumName, albumDescription, items) {
  const count = items.length
  return `
    <section class="gallery-header">
      <div>
        <h2>${escapeHTML(albumName)}</h2>
        ${albumDescription ? `<p>${escapeHTML(albumDescription)}</p>` : ''}
      </div>
      <span>${count} item${count === 1 ? '' : 's'}</span>
    </section>
    <section class="gallery-grid">
      ${items.map(renderItem).join('')}
    </section>
  `
}

const LIGHTBOX_PLACEHOLDER_SRC = 'data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///ywAAAAAAQABAAACAUwAOw=='

function renderItem(item) {
  const name = item.name || 'Untitled asset'
  const isVideo = item.mimeType?.startsWith('video/')
  const duration = isVideo ? formatDuration(item.duration) : ''
  return `
    <button class="gallery-item" type="button" data-gallery-id="${escapeHTML(item.id)}" data-src="${LIGHTBOX_PLACEHOLDER_SRC}" data-download-url="${LIGHTBOX_PLACEHOLDER_SRC}" aria-label="Open ${escapeHTML(name)}">
      <img data-thumb-id="${escapeHTML(item.id)}" alt="${escapeHTML(name)}">
      ${duration ? `<span class="gallery-duration">${escapeHTML(duration)}</span>` : ''}
    </button>
  `
}

function formatDuration(duration) {
  if (duration == null) return ''
  const seconds = Math.max(0, Math.floor(Number(duration)))
  if (!Number.isFinite(seconds)) return ''
  const mins = Math.floor(seconds / 60)
  const secs = seconds % 60
  return `${mins}:${String(secs).padStart(2, '0')}`
}

function escapeHTML(value) {
  return String(value ?? '')
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
    .replaceAll("'", '&#39;')
}

function cssEscape(value) {
  if (typeof CSS !== 'undefined' && CSS.escape) return CSS.escape(String(value))
  return String(value).replaceAll('\\', '\\\\').replaceAll('"', '\\"')
}
