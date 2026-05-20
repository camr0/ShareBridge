export function createGalleryController({
  root,
  lightGallery = globalThis.lightGallery,
  createObjectURL = URL.createObjectURL,
  revokeObjectURL = URL.revokeObjectURL,
  setRefreshTimeout = globalThis.setTimeout?.bind(globalThis),
  clearRefreshTimeout = globalThis.clearTimeout?.bind(globalThis),
  lightboxRoot = globalThis.document,
  onPreviewRequest,
  onDownloadRequest,
}) {
  const state = {
    items: [],
    thumbs: new Map(),
    urls: new Map(),
    previewRequests: new Set(),
    lightbox: null,
    loadedThumbs: 0,
    refreshTimer: null,
    activePreviewID: '',
    lightboxGrid: null,
    lightboxDownloadButton: null,
  }

  const handleClick = (event) => {
    const download = event.target?.closest?.('.gallery-download')
    if (download?.dataset?.galleryId) {
      event.preventDefault?.()
      event.stopPropagation?.()
      onDownloadRequest?.(download.dataset.galleryId)
      return
    }

    const item = event.target?.closest?.('[data-gallery-id]')
    if (item?.dataset?.galleryId) {
      const index = state.items.findIndex((galleryItem) => galleryItem.id === item.dataset.galleryId)
      if (index >= 0) {
        requestPreviewByIndex(index)
      } else {
        state.activePreviewID = item.dataset.galleryId
        ensureLightboxDownloadButton()
        onPreviewRequest?.(item.dataset.galleryId, { priority: 'active' })
      }
    }
  }

  const handleLightboxSlide = (event) => {
    const index = Number(event.detail?.index)
    if (!Number.isInteger(index)) return
    requestPreviewByIndex(index)
  }

  root.addEventListener?.('click', handleClick)

  function handleThumbnailList(msg) {
    destroyLightbox()
    revokeAllUrls()
    state.items = msg.items || []
    state.thumbs.clear()
    state.previewRequests.clear()
    state.loadedThumbs = 0
    root.innerHTML = renderGalleryShell(msg.albumName || 'Shared album', msg.albumDescription || '', state.items)
    updateThumbnailProgress()
    initLightbox()
  }

  function handleThumbnailData(index, payload) {
    const item = state.items[index]
    if (!item) return
    const firstLoad = !state.thumbs.has(item.id)
    state.thumbs.set(item.id, true)
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
      scheduleLightboxRefresh()
    }
    if (firstLoad) {
      state.loadedThumbs += 1
      updateThumbnailProgress()
    }
  }

  function handlePreviewData(id, payload, mimeType = 'image/jpeg') {
    state.previewRequests.delete(id)

    if (mimeType?.startsWith('video/')) {
      const blob = new Blob([payload], { type: mimeType })
      const url = createObjectURL(blob)
      const previousUrl = state.urls.get(`preview:${id}`)
      if (previousUrl) revokeObjectURL(previousUrl)
      state.urls.set(`preview:${id}`, url)

      const galleryItem = root.querySelector?.(`[data-gallery-id="${cssEscape(id)}"]`)
      if (galleryItem?.dataset) {
        const videoSrc = JSON.stringify([{ src: url, type: mimeType }])
        galleryItem.dataset.video = videoSrc
        galleryItem.dataset.src = url
        galleryItem.dataset.downloadUrl = 'false'
        scheduleLightboxRefresh()
      }

      updateLightboxItemVideo(id, url, mimeType)

      if (state.activePreviewID === id) {
        showVideoInLightbox(url)
      }
      return
    }

    const blob = new Blob([payload], { type: mimeType })
    const url = createObjectURL(blob)
    const previousUrl = state.urls.get(`preview:${id}`)
    if (previousUrl) revokeObjectURL(previousUrl)
    state.urls.set(`preview:${id}`, url)

    const galleryItem = root.querySelector?.(`[data-gallery-id="${cssEscape(id)}"]`)
    if (galleryItem?.dataset) {
      galleryItem.dataset.src = url
      galleryItem.dataset.downloadUrl = 'false'
      scheduleLightboxRefresh()
    }

    updateLightboxItemSource(id, url)

    if (state.activePreviewID === id) {
      updateActiveLightboxImage(url)
    }
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
    state.lightbox = lightGallery(grid, { selector: '.gallery-item', download: false })
    state.lightboxGrid = grid
    grid.addEventListener?.('lgAfterOpen', handleLightboxSlide)
    grid.addEventListener?.('lgBeforeSlide', handleLightboxSlide)
    grid.addEventListener?.('lgAfterSlide', handleLightboxSlide)
  }

  function destroyLightbox() {
    cancelLightboxRefresh()
    state.lightboxGrid?.removeEventListener?.('lgAfterOpen', handleLightboxSlide)
    state.lightboxGrid?.removeEventListener?.('lgBeforeSlide', handleLightboxSlide)
    state.lightboxGrid?.removeEventListener?.('lgAfterSlide', handleLightboxSlide)
    state.lightboxGrid = null
    state.lightboxDownloadButton = null
    state.lightbox?.destroy?.()
    state.lightbox = null
  }

  function revokeAllUrls() {
    for (const url of state.urls.values()) revokeObjectURL(url)
    state.urls.clear()
  }

  function updateThumbnailProgress() {
    const total = state.items.length
    const pct = total === 0 ? 100 : Math.round((state.loadedThumbs / total) * 100)
    const text = root.querySelector?.('.gallery-progress-text')
    const fill = root.querySelector?.('.gallery-progress-fill')
    const progress = root.querySelector?.('.gallery-progress')
    if (text) text.textContent = `${state.loadedThumbs} / ${total} thumbnails`
    if (fill?.style) fill.style.width = `${pct}%`
    if (progress?.classList && total > 0 && state.loadedThumbs >= total) {
      progress.classList.add('hidden')
    } else if (progress?.classList) {
      progress.classList.remove('hidden')
    }
  }

  function scheduleLightboxRefresh() {
    if (!state.lightbox?.refresh || state.refreshTimer != null || !setRefreshTimeout) return
    state.refreshTimer = setRefreshTimeout(() => {
      state.refreshTimer = null
      state.lightbox?.refresh?.()
    }, 50)
  }

  function cancelLightboxRefresh() {
    if (state.refreshTimer == null) return
    clearRefreshTimeout?.(state.refreshTimer)
    state.refreshTimer = null
  }

  function requestPreviewByIndex(index) {
    const item = state.items[index]
    if (!item) return
    state.activePreviewID = item.id
    ensureLightboxDownloadButton()
    const existingPreviewUrl = state.urls.get(`preview:${item.id}`)
    if (existingPreviewUrl) {
      if (item.mimeType?.startsWith('video/')) {
        showVideoInLightbox(existingPreviewUrl)
      } else {
        updateActiveLightboxImage(existingPreviewUrl)
      }
    } else {
      if (item.mimeType?.startsWith('video/')) {
        showVideoInLightbox(null)
      }
      requestPreviewForItem(item, 'active')
    }
    requestPreviewForItem(state.items[index - 1], 'preload')
    requestPreviewForItem(state.items[index + 1], 'preload')
  }

  function showVideoInLightbox(url) {
    // Try up to 10 times with 50ms delays waiting for lightbox DOM
    let attempts = 0
    const tryShow = () => {
      const imgWrap = lightboxRoot?.querySelector?.('.lg-current .lg-img-wrap')
      if (imgWrap) {
        imgWrap.innerHTML = ''
        const video = document.createElement('video')
        video.className = 'lg-object lg-video'
        video.controls = true
        video.muted = true
        video.playsInline = true
        video.setAttribute('playsinline', '')
        video.style.maxWidth = '100%'
        video.style.maxHeight = '100%'
        video.style.display = 'block'
        if (url) {
          video.src = url
          video.autoplay = true
        } else {
          const thumbUrl = state.urls.get(`thumb:${state.activePreviewID}`)
          if (thumbUrl) video.poster = thumbUrl
        }
        imgWrap.appendChild(video)
        return
      }
      attempts++
      if (attempts < 10) setTimeout(tryShow, 50)
    }
    tryShow()
  }

  function requestPreviewForItem(item, priority) {
    if (!item || state.urls.has(`preview:${item.id}`) || state.previewRequests.has(item.id)) return
    if (priority === 'preload' && item.mimeType?.startsWith('video/')) return
    state.previewRequests.add(item.id)
    onPreviewRequest?.(item.id, { priority, mimeType: item.mimeType })
  }

  function updateActiveLightboxImage(url) {
    const activeImage = lightboxRoot?.querySelector?.('.lg-current .lg-object')
    if (activeImage) activeImage.src = url
  }

  function updateLightboxItemSource(id, url) {
    const index = state.items.findIndex((item) => item.id === id)
    if (index < 0) return
    if (state.lightbox?.galleryItems?.[index]) {
      state.lightbox.galleryItems[index].src = url
      state.lightbox.galleryItems[index].downloadUrl = 'false'
    }
    const renderedImages = lightboxRoot?.querySelectorAll?.(`.lg-object[data-index="${index}"]`) || []
    for (const image of renderedImages) image.src = url
  }

  function updateActiveLightboxVideo(url, isPoster) {
    const existingVideo = lightboxRoot?.querySelector?.('.lg-current video.lg-video, .lg-current .lg-video-cont video')
    if (existingVideo) {
      if (isPoster) {
        existingVideo.poster = url
      } else {
        existingVideo.src = url
        existingVideo.load()
      }
      return
    }
    const imgWrap = lightboxRoot?.querySelector?.('.lg-current .lg-img-wrap')
    if (!imgWrap) return
    imgWrap.innerHTML = ''
    const video = document.createElement('video')
    video.className = 'lg-object lg-video'
    video.controls = true
    video.muted = true
    video.playsInline = true
    video.setAttribute('playsinline', '')
    video.style.maxWidth = '100%'
    video.style.maxHeight = '100%'
    video.style.display = 'block'
    if (isPoster) {
      video.poster = url
    } else {
      video.src = url
      video.autoplay = true
    }
    imgWrap.appendChild(video)
  }

  function updateLightboxItemVideo(id, url, mimeType) {
    const index = state.items.findIndex((item) => item.id === id)
    if (index < 0) return
    if (state.lightbox?.galleryItems?.[index]) {
      state.lightbox.galleryItems[index].src = url
      state.lightbox.galleryItems[index].video = JSON.stringify([{ src: url, type: mimeType }])
      state.lightbox.galleryItems[index].downloadUrl = 'false'
    }
  }

  function ensureLightboxDownloadButton() {
    const toolbar = lightboxRoot?.querySelector?.('.lg-toolbar')
    if (!toolbar) return
    const existing = toolbar.querySelector?.('.sharebridge-lightbox-download')
    if (existing) {
      state.lightboxDownloadButton = existing
      return
    }
    const createElement = lightboxRoot?.createElement?.bind(lightboxRoot) || globalThis.document?.createElement?.bind(globalThis.document)
    if (!createElement) return
    const button = createElement('button')
    button.type = 'button'
    button.className = 'lg-download sharebridge-lightbox-download'
    button.title = 'Download'
    button.setAttribute?.('aria-label', 'Download')
    button.textContent = '↓'
    button.addEventListener?.('click', (event) => {
      event.preventDefault?.()
      if (state.activePreviewID) onDownloadRequest?.(state.activePreviewID)
    })
    toolbar.appendChild?.(button)
    state.lightboxDownloadButton = button
  }

  return { handleThumbnailList, handleThumbnailData, handlePreviewData, destroy, state }
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
    <section class="gallery-progress${count === 0 ? ' hidden' : ''}" aria-live="polite">
      <div class="gallery-progress-row">
        <span class="gallery-progress-text">0 / ${count} thumbnails</span>
      </div>
      <div class="gallery-progress-track">
        <div class="gallery-progress-fill" style="width: 0%"></div>
      </div>
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
  const videoAttr = isVideo
    ? ` data-video='[{"src":"${LIGHTBOX_PLACEHOLDER_SRC}","type":"video/mp4"}]'`
    : ''
  return `
    <button class="gallery-item" type="button" data-gallery-id="${escapeHTML(item.id)}" data-src="${LIGHTBOX_PLACEHOLDER_SRC}" data-download-url="${LIGHTBOX_PLACEHOLDER_SRC}"${videoAttr} aria-label="Open ${escapeHTML(name)}">
      <img data-thumb-id="${escapeHTML(item.id)}" alt="${escapeHTML(name)}" loading="lazy" decoding="async">
      <span class="gallery-download" role="button" tabindex="0" data-gallery-id="${escapeHTML(item.id)}" aria-label="Download ${escapeHTML(name)}">↓</span>
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
