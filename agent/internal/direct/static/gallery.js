// ShareBridge recipient gallery — presentation + URL-factory data layer.
//
// This module renders the gallery grid and drives the lightGallery lightbox.
// Its data layer is entirely URL-factory based: thumbnails become
// `<img src="…/thumb/{id}" loading="lazy">`, images preview via `previewUrl(id)`,
// videos stream via `playbackUrl(id)`, and downloads use `assetUrl(id)` /
// `archiveManifestUrl()` / `archivePartUrl(token, index)`. It has zero imports
// and is decoupled from the HTTP transport via the injected factory callbacks.

const WINDOW_SIZE = 120

function defaultPlaceholderUrl() {
  try {
    return new URL('placeholder.gif', import.meta.url).href
  } catch {
    return 'static/placeholder.gif'
  }
}

export function createGalleryController({
  root,
  lightGallery = globalThis.lightGallery,
  lightboxRoot = globalThis.document,
  thumbUrl = (id) => id,
  previewUrl = (id) => id,
  playbackUrl = (id) => id,
  assetUrl = (id) => id,
  archiveManifestUrl = () => '',
  archivePartUrl = (token, index) => `${token}/${index}`,
  placeholderUrl = defaultPlaceholderUrl,
  fetchImpl = globalThis.fetch,
  setTimeoutFn = globalThis.setTimeout?.bind(globalThis),
  onVideoElement,
  onVideoClose,
}) {
  const state = {
    items: [],
    lightbox: null,
    activePreviewID: '',
    lightboxGrid: null,
    lightboxDownloadButton: null,
    albumDownloadPhase: 'idle',
    albumDownloadPartIndex: undefined,
    albumDownloadPartCount: undefined,
    renderedCount: 0,
    _lastVideoID: '',
    generation: 0,
  }

  let placeholder = ''
  try {
    placeholder = placeholderUrl()
  } catch {
    placeholder = defaultPlaceholderUrl()
  }

  const delay = (ms) => new Promise((resolve) => {
    if (typeof setTimeoutFn === 'function') setTimeoutFn(resolve, ms)
    else resolve()
  })

  const handleClick = (event) => {
    const loadMore = event.target?.closest?.('.gallery-load-more')
    if (loadMore) {
      event.preventDefault?.()
      requestNextWindow()
      return
    }

    const albumDownload = event.target?.closest?.('.gallery-download-all')
    if (albumDownload) {
      event.preventDefault?.()
      event.stopPropagation?.()
      requestAlbumDownload()
      return
    }

    const download = event.target?.closest?.('.gallery-download')
    if (download?.dataset?.galleryId) {
      event.preventDefault?.()
      event.stopPropagation?.()
      downloadAsset(download.dataset.galleryId)
      return
    }

    const item = event.target?.closest?.('[data-gallery-index]')
      || event.target?.closest?.('[data-gallery-id]')
    if (item?.dataset?.galleryId) {
      const globalIndex = Number(item.dataset.galleryIndex)
      const index = Number.isInteger(globalIndex)
        ? globalIndex
        : state.items.findIndex((galleryItem) => galleryItem.id === item.dataset.galleryId)
      if (index >= 0) {
        state.activePreviewID = state.items[index]?.id
        ensureLightboxDownloadButton()
        state.lightbox?.openGallery?.(index)
      }
    }
  }

  const handleLightboxSlide = (event) => {
    const index = Number(event.detail?.index)
    if (!Number.isInteger(index)) return
    const item = state.items[index]
    if (!item) return
    state.activePreviewID = item.id
    ensureLightboxDownloadButton()
    if (item.mimeType?.startsWith('video/')) {
      showVideoInLightbox(playbackUrl(item.id))
    }
  }

  const pauseCurrentSlideVideos = () => {
    const videos = lightboxRoot?.querySelectorAll?.('.lg-current video') || []
    for (const video of videos) video.pause?.()
  }

  const handleLightboxBeforeSlide = (event) => {
    pauseCurrentSlideVideos()
    handleLightboxSlide(event)
  }

  const handleLightboxClose = () => {
    const id = state.activePreviewID
    const item = state.items.find((candidate) => candidate.id === id)
    state.activePreviewID = ''
    state._lastVideoID = ''
    if (item?.mimeType?.startsWith('video/')) onVideoClose?.()
  }

  root.addEventListener?.('click', handleClick)
  // Image load errors do not bubble; capture them so a broken thumbnail can be
  // marked unavailable without an inline handler (CSP forbids onerror=).
  root.addEventListener?.('error', (event) => {
    const img = event.target
    const id = img?.dataset?.thumbId
    if (!id || !state.items.some((item) => item.id === id)) return
    const tile = root.querySelector?.(`[data-gallery-id="${cssEscape(id)}"]`)
    tile?.classList?.add?.('gallery-item-unavailable')
    tile?.setAttribute?.('aria-disabled', 'true')
    if (img?.alt !== undefined) {
      const item = state.items.find((candidate) => candidate.id === id)
      img.alt = `${item?.name || 'Asset'} thumbnail unavailable`
    }
  }, true)

  function downloadAsset(id) {
    triggerDownload(assetUrl(id))
  }

  function triggerDownload(url) {
    if (!url) return
    const createElement = lightboxRoot?.createElement?.bind(lightboxRoot)
      || globalThis.document?.createElement?.bind(globalThis.document)
    if (!createElement) return
    const anchor = createElement('a')
    anchor.href = url
    anchor.rel = 'noopener'
    const host = lightboxRoot?.body || lightboxRoot
    host?.appendChild?.(anchor)
    anchor.click?.()
    anchor.remove?.()
  }

  async function requestAlbumDownload() {
    if (state.albumDownloadPhase === 'starting' || state.albumDownloadPhase === 'downloading') return
    setAlbumDownloadState({ phase: 'starting' })
    let manifest
    try {
      const response = await fetchImpl(archiveManifestUrl())
      if (!response?.ok) throw new Error(`archive ${response?.status ?? 'failed'}`)
      manifest = await response.json()
    } catch {
      setAlbumDownloadState({ phase: 'failed' })
      return
    }
    const token = manifest?.token
    const parts = Array.isArray(manifest?.parts) ? manifest.parts : []
    if (!token || parts.length === 0) {
      setAlbumDownloadState({ phase: 'failed' })
      return
    }
    for (let i = 0; i < parts.length; i++) {
      const part = parts[i]
      setAlbumDownloadState({ phase: 'downloading', partIndex: i + 1, partCount: parts.length })
      triggerDownload(archivePartUrl(token, typeof part.index === 'number' ? part.index : i))
      // Stagger the batch so the browser does not coalesce/suppress parts.
      await delay(250)
    }
    setAlbumDownloadState({ phase: 'complete' })
  }

  function setAlbumDownloadState({ phase = 'idle', partIndex, partCount } = {}) {
    state.albumDownloadPhase = phase
    state.albumDownloadPartIndex = partIndex
    state.albumDownloadPartCount = partCount
    const button = root.querySelector?.('.gallery-download-all')
    const liveStatus = root.querySelector?.('.gallery-download-all-status')
    let label = 'Download All'
    let announcement = 'Ready to download album'
    let busy = false
    let disabled = state.items.length === 0
    if (phase === 'starting') {
      label = 'Starting…'
      announcement = 'Starting album download'
      busy = true
      disabled = true
    } else if (phase === 'downloading') {
      label = partIndex && partCount ? `Downloading ${partIndex}/${partCount}` : 'Downloading…'
      announcement = partIndex && partCount
        ? `Downloading album part ${partIndex} of ${partCount}`
        : 'Downloading album'
      busy = true
      disabled = true
    } else if (phase === 'failed') {
      label = 'Retry Download'
      announcement = 'Album download failed'
    } else if (phase === 'complete') {
      label = 'Download Complete'
      announcement = 'Album download complete'
    }
    if (button) {
      button.textContent = label
      button.disabled = disabled
      button.setAttribute?.('aria-busy', String(busy))
    }
    if (liveStatus) liveStatus.textContent = announcement
  }

  function render(items, { albumName = 'Shared album', albumDescription = '' } = {}) {
    state.generation += 1
    destroyLightbox()
    state.items = (items || []).map((item) => ({
      ...item,
      thumbUrl: thumbUrl(item.id) || placeholder,
      previewUrl: item.mimeType?.startsWith('video/') ? '' : (previewUrl(item.id) || ''),
    }))
    state.activePreviewID = ''
    state._lastVideoID = ''
    state.renderedCount = Math.min(WINDOW_SIZE, state.items.length)
    root.innerHTML = renderGalleryShell(
      albumName,
      albumDescription,
      state.items.slice(0, state.renderedCount),
      { totalCount: state.items.length, progressive: state.items.length > state.renderedCount },
    )
    setAlbumDownloadState({
      phase: state.albumDownloadPhase,
      partIndex: state.albumDownloadPartIndex,
      partCount: state.albumDownloadPartCount,
    })
    initLightbox()
    updateLoadMoreButton()
  }

  function destroy() {
    state.generation += 1
    destroyLightbox()
    root.removeEventListener?.('click', handleClick)
  }

  function initLightbox() {
    if (!lightGallery || !root.querySelector) return
    const grid = root.querySelector('.gallery-grid')
    if (!grid) return
    state.lightbox = lightGallery(grid, {
      dynamic: true,
      dynamicEl: state.items.map(createDynamicLightboxItem),
      download: false,
    })
    state.lightboxGrid = grid
    grid.addEventListener?.('lgAfterOpen', handleLightboxSlide)
    grid.addEventListener?.('lgBeforeSlide', handleLightboxBeforeSlide)
    grid.addEventListener?.('lgAfterSlide', handleLightboxSlide)
    grid.addEventListener?.('lgBeforeClose', pauseCurrentSlideVideos)
    grid.addEventListener?.('lgAfterClose', handleLightboxClose)
  }

  function destroyLightbox() {
    state.lightboxGrid?.removeEventListener?.('lgAfterOpen', handleLightboxSlide)
    state.lightboxGrid?.removeEventListener?.('lgBeforeSlide', handleLightboxBeforeSlide)
    state.lightboxGrid?.removeEventListener?.('lgAfterSlide', handleLightboxSlide)
    state.lightboxGrid?.removeEventListener?.('lgBeforeClose', pauseCurrentSlideVideos)
    state.lightboxGrid?.removeEventListener?.('lgAfterClose', handleLightboxClose)
    state.lightboxGrid = null
    state.lightboxDownloadButton = null
    state.lightbox?.destroy?.()
    state.lightbox = null
  }

  function requestNextWindow() {
    if (state.renderedCount >= state.items.length) return
    const start = state.renderedCount
    const count = Math.min(WINDOW_SIZE, state.items.length - start)
    const grid = root.querySelector?.('.gallery-grid')
    grid?.insertAdjacentHTML?.('beforeend', renderItems(state.items.slice(start, start + count), start))
    state.renderedCount += count
    updateLoadMoreButton()
  }

  function updateLoadMoreButton() {
    const button = root.querySelector?.('.gallery-load-more')
    if (!button) return
    const atEnd = state.renderedCount >= state.items.length
    button.disabled = atEnd
    button.hidden = atEnd
    button.textContent = 'Load More'
  }

  function showVideoInLightbox(url) {
    const id = state.activePreviewID
    const itemIndex = state.items.findIndex((item) => item.id === id)
    let attempts = 0
    const tryShow = () => {
      if (state.activePreviewID !== id) return
      const currentSlide = lightboxRoot?.querySelector?.('.lg-current')
      const datasetIndex = Number(currentSlide?.dataset?.index)
      const currentIndex = Number.isInteger(datasetIndex) ? datasetIndex : undefined
      if (Number.isInteger(currentIndex) && currentIndex !== itemIndex) return

      let imgWrap = currentSlide?.querySelector?.('.lg-img-wrap')
        || lightboxRoot?.querySelector?.('.lg-current .lg-img-wrap')
      if (!imgWrap) {
        const createElement = lightboxRoot?.createElement?.bind(lightboxRoot)
          || globalThis.document?.createElement?.bind(globalThis.document)
        if (currentSlide && createElement) {
          imgWrap = createElement('div')
          imgWrap.className = 'lg-img-wrap'
          currentSlide.innerHTML = ''
          currentSlide.appendChild?.(imgWrap)
        } else {
          attempts++
          if (attempts < 20) delay(50).then(tryShow)
          return
        }
      }
      const existing = imgWrap.querySelector('video.lg-video')
      if (existing && state._lastVideoID === id) {
        if (url && existing._sharebridgeMediaUrl !== url) {
          existing._sharebridgeMediaUrl = url
          existing.src = url
        }
        onVideoElement?.(existing)
        return
      }
      imgWrap.innerHTML = ''
      const createElement = lightboxRoot?.createElement?.bind(lightboxRoot)
        || globalThis.document?.createElement?.bind(globalThis.document)
      if (!createElement) return
      const video = createElement('video')
      video.className = 'lg-object lg-video'
      video.controls = true
      video.playsInline = true
      video.setAttribute('playsinline', '')
      video.style.maxWidth = '100%'
      video.style.maxHeight = '80vh'
      video.style.margin = '0 auto'
      video.style.objectFit = 'contain'
      if (url) {
        video._sharebridgeMediaUrl = url
        video.src = url
        video.autoplay = true
      }
      const poster = thumbUrl(id) || placeholder
      if (poster) video.poster = poster
      imgWrap.appendChild(video)
      state._lastVideoID = id
      onVideoElement?.(video)
    }
    tryShow()
  }

  function ensureLightboxDownloadButton() {
    const toolbar = lightboxRoot?.querySelector?.('.lg-toolbar')
    if (!toolbar) return
    const existing = toolbar.querySelector?.('.sharebridge-lightbox-download')
    if (existing) {
      state.lightboxDownloadButton = existing
      return
    }
    const createElement = lightboxRoot?.createElement?.bind(lightboxRoot)
      || globalThis.document?.createElement?.bind(globalThis.document)
    if (!createElement) return
    const button = createElement('button')
    button.type = 'button'
    button.className = 'lg-icon lg-download sharebridge-lightbox-download'
    button.title = 'Download'
    button.setAttribute?.('aria-label', 'Download')
    button.textContent = ''
    button.addEventListener?.('click', (event) => {
      event.preventDefault?.()
      if (state.activePreviewID) downloadAsset(state.activePreviewID)
    })
    toolbar.appendChild?.(button)
    state.lightboxDownloadButton = button
  }

  return {
    render,
    setAlbumDownloadState,
    destroy,
    state,
  }
}

export function renderGalleryShell(albumName, albumDescription, items, { totalCount = items.length, progressive = false } = {}) {
  const count = totalCount
  return `
    <section class="gallery-header">
      <div>
        <h2>${escapeHTML(albumName)}</h2>
        ${albumDescription ? `<p>${escapeHTML(albumDescription)}</p>` : ''}
      </div>
      <div class="gallery-summary">
        <span>${count} item${count === 1 ? '' : 's'}</span>
        <button class="gallery-download-all" type="button" aria-describedby="gallery-download-all-status" aria-busy="false"${count === 0 ? ' disabled' : ''}>Download All</button>
        <span id="gallery-download-all-status" class="gallery-download-all-status" role="status" aria-live="polite">Ready to download album</span>
      </div>
    </section>
    <section class="gallery-grid">
      ${renderItems(items, 0)}
    </section>
    ${progressive ? `
      <div class="gallery-progressive-controls">
        <button class="gallery-load-more" type="button" aria-busy="false">Load More</button>
      </div>
    ` : ''}
  `
}

function renderItems(items, startIndex) {
  return items.map((item, offset) => renderItem(item, startIndex + offset)).join('')
}

function renderItem(item, globalIndex) {
  const name = item.name || 'Untitled asset'
  const isVideo = item.mimeType?.startsWith('video/')
  const duration = isVideo ? formatDuration(item.duration) : ''
  return `
    <button class="gallery-item" type="button" data-gallery-id="${escapeHTML(item.id)}" data-gallery-index="${globalIndex}" aria-label="Open ${escapeHTML(name)}">
      <img data-thumb-id="${escapeHTML(item.id)}" src="${escapeHTML(item.thumbUrl || '')}" alt="${escapeHTML(name)}" loading="lazy" decoding="async">
      <span class="gallery-download" role="button" tabindex="0" data-gallery-id="${escapeHTML(item.id)}" aria-label="Download ${escapeHTML(name)}">↓</span>
      ${duration ? `<span class="gallery-duration">${escapeHTML(duration)}</span>` : ''}
    </button>
  `
}

function createDynamicLightboxItem(item) {
  const name = escapeHTML(item.name || 'Untitled asset')
  const id = item.id
  const isVideo = item.mimeType?.startsWith('video/')
  const entry = {
    src: isVideo ? item.thumbUrl : (item.previewUrl || item.thumbUrl),
    thumb: item.thumbUrl,
    alt: name,
    subHtml: `<p>${name}</p>`,
    downloadUrl: 'false',
  }
  if (isVideo) entry.poster = item.thumbUrl
  return entry
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
