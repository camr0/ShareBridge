let thumbnailRequestSequence = 0

export function createGalleryController({
  root,
  lightGallery = globalThis.lightGallery,
  createObjectURL = URL.createObjectURL,
  revokeObjectURL = URL.revokeObjectURL,
  setRefreshTimeout = globalThis.setTimeout?.bind(globalThis),
  clearRefreshTimeout = globalThis.clearTimeout?.bind(globalThis),
  createIntersectionObserver = typeof globalThis.IntersectionObserver === 'function'
    ? (callback, options) => new globalThis.IntersectionObserver(callback, options)
    : null,
  lightboxRoot = globalThis.document,
  onThumbnailBatchRequest,
  onPreviewRequest,
  onPreviewClose,
  onDownloadRequest,
  onAlbumDownloadRequest,
}) {
  const state = {
    items: [],
    thumbs: new Map(),
    urls: new Map(),
    previewRequests: new Set(),
    lightbox: null,
    loadedThumbs: 0,
    unavailableThumbs: 0,
    refreshTimer: null,
    activePreviewID: '',
    lightboxGrid: null,
    lightboxDownloadButton: null,
    videoPlaybackSequence: 0,
    albumDownloadPhase: 'idle',
    albumDownloadPartIndex: undefined,
    albumDownloadPartCount: undefined,
    thumbnailMode: '',
    renderedCount: 0,
    requestedThumbs: 0,
    inFlight: null,
    terminalIndices: new Set(),
    unavailableIndices: new Set(),
    pendingSuccessfulIndices: new Set(),
    observer: null,
    pendingExpansion: false,
    retryBatch: null,
    pullRequestErrored: false,
    initialPullUnresolved: false,
    generation: 0,
    ownedURLKeys: new Set(),
  }

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
      onAlbumDownloadRequest?.()
      return
    }

    const download = event.target?.closest?.('.gallery-download')
    if (download?.dataset?.galleryId) {
      event.preventDefault?.()
      event.stopPropagation?.()
      onDownloadRequest?.(download.dataset.galleryId)
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
        state.lightbox?.openGallery?.(index)
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
    if (!item?.mimeType?.startsWith('video/')) return
    state.urls.delete(`preview:${id}`)
    state.previewRequests.delete(id)
    state._lastVideoID = ''
    state.activePreviewID = ''
    onPreviewClose?.(id)
  }

  root.addEventListener?.('click', handleClick)

  function handleThumbnailList(msg) {
    state.generation += 1
    destroyLightbox()
    disconnectObserver()
    cancelLightboxRefresh()
    revokeAllUrls()
    state.items = msg.items || []
    state.thumbnailMode = msg.thumbnailMode === 'pull-v1' ? 'pull-v1' : ''
    state.thumbs.clear()
    state.previewRequests.clear()
    state.activePreviewID = ''
    state._lastVideoID = ''
    state.loadedThumbs = 0
    state.unavailableThumbs = 0
    state.requestedThumbs = 0
    state.inFlight = null
    state.pendingExpansion = false
    state.retryBatch = null
    state.pullRequestErrored = false
    state.terminalIndices.clear()
    state.unavailableIndices.clear()
    state.pendingSuccessfulIndices.clear()
    const initialCount = state.thumbnailMode === 'pull-v1'
      ? Math.min(THUMBNAIL_WINDOW_SIZE, state.items.length)
      : state.items.length
    state.initialPullUnresolved = state.thumbnailMode === 'pull-v1' && initialCount > 0
    state.renderedCount = initialCount
    root.innerHTML = renderGalleryShell(
      msg.albumName || 'Shared album',
      msg.albumDescription || '',
      state.items.slice(0, initialCount),
      { totalCount: state.items.length, progressive: state.thumbnailMode === 'pull-v1' },
    )
    setAlbumDownloadState({
      phase: state.albumDownloadPhase,
      partIndex: state.albumDownloadPartIndex,
      partCount: state.albumDownloadPartCount,
    })
    updateThumbnailProgress()
    initLightbox()
    initObserver()
    updateLoadMoreButton()
    if (state.thumbnailMode === 'pull-v1' && initialCount > 0) {
      requestBatch(0, initialCount)
    }
  }

  function handleThumbnailData(index, payload) {
    const item = state.items[index]
    if (!item) return
    if (state.thumbnailMode === 'pull-v1') {
      const range = state.inFlight
      const inActiveRange = range && index >= range.start && index < range.start + range.count
      const acceptingLegacyFallback = state.pullRequestErrored && !range
      if (
        (!inActiveRange && !state.pendingSuccessfulIndices.has(index) && !acceptingLegacyFallback && !state.initialPullUnresolved)
        || state.terminalIndices.has(index)
      ) return
      if (state.thumbs.has(item.id)) return
    }
    const firstLoad = !state.thumbs.has(item.id)
    state.thumbs.set(item.id, true)
    const blob = new Blob([payload], { type: 'image/jpeg' })
    const url = createObjectURL(blob)
    const urlKey = `thumb:${item.id}`
    const previousUrl = state.urls.get(urlKey)
    if (previousUrl) revokeObjectURL(previousUrl)
    state.urls.set(urlKey, url)
    state.ownedURLKeys.add(urlKey)
    const img = root.querySelector?.(`[data-thumb-id="${cssEscape(item.id)}"]`)
    if (img) img.src = url
    const galleryItem = root.querySelector?.(`[data-gallery-id="${cssEscape(item.id)}"]`)
    if (galleryItem?.dataset) {
      galleryItem.dataset.src = url
      galleryItem.dataset.downloadUrl = url
    }
    updateLightboxItemThumbnail(index, item, url)
    if (firstLoad) {
      state.loadedThumbs += 1
      state.pendingSuccessfulIndices.delete(index)
      if (state.thumbnailMode === 'pull-v1') state.terminalIndices.add(index)
      updateThumbnailProgress()
    }
  }

  function handleThumbnailComplete(msg) {
    if (state.thumbnailMode === 'pull-v1') {
      if (!state.pullRequestErrored) return
      state.thumbnailMode = ''
      state.inFlight = null
      state.retryBatch = null
      state.pendingExpansion = false
      if (state.renderedCount < state.items.length) {
        const grid = root.querySelector?.('.gallery-grid')
        grid?.insertAdjacentHTML?.('beforeend', renderItems(state.items.slice(state.renderedCount), state.renderedCount))
        state.renderedCount = state.items.length
      }
      updateLoadMoreButton()
    }
    state.unavailableThumbs = Math.max(0, Number(msg.failed) || 0)
    updateThumbnailProgress()
  }

  function handleThumbnailBatchComplete(msg) {
    const range = state.inFlight
    if (!range) return false
    if (
      msg.request_id !== range.requestId
      || !Number.isInteger(msg.start)
      || msg.start !== range.start
      || !Number.isInteger(msg.count)
      || msg.count !== range.count
    ) {
      return false
    }
    const failedIndices = new Set(Array.isArray(msg.failed_indices) ? msg.failed_indices : [])
    state.initialPullUnresolved = false
    for (let index = range.start; index < range.start + range.count; index++) {
      const item = state.items[index]
      if (!item) continue
      if (failedIndices.has(index)) {
        state.terminalIndices.add(index)
        state.pendingSuccessfulIndices.delete(index)
        if (!state.thumbs.has(item.id)) {
          state.unavailableIndices.add(index)
          markThumbnailUnavailable(index)
        }
      } else if (!state.thumbs.has(item.id)) {
        state.pendingSuccessfulIndices.add(index)
      } else {
        state.terminalIndices.add(index)
      }
    }
    state.unavailableThumbs = state.unavailableIndices.size
    state.inFlight = null
    updateThumbnailProgress()
    updateLoadMoreButton()
    if (state.pendingExpansion) {
      state.pendingExpansion = false
      requestNextWindow()
    }
    return true
  }

  function handleThumbnailBatchError(msg) {
    const range = state.inFlight
    if (!range || msg.request_id !== range.requestId) return false
    state.inFlight = null
    state.requestedThumbs = Math.max(0, state.requestedThumbs - range.count)
    state.pendingExpansion = false
    state.retryBatch = { start: range.start, count: range.count }
    state.pullRequestErrored = true
    state.initialPullUnresolved = false
    updateThumbnailProgress()
    updateLoadMoreButton()
    return true
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

  function handlePreviewData(id, payload, mimeType = 'image/jpeg') {
    state.previewRequests.delete(id)

    if (mimeType?.startsWith('video/')) {
      const url = `/media/${encodeURIComponent(id)}`
      state.urls.set(`preview:${id}`, url)

      const galleryItem = root.querySelector?.(`[data-gallery-id="${cssEscape(id)}"]`)
      if (galleryItem?.dataset) {
        const videoSrc = JSON.stringify([{ src: url, type: 'video/mp4' }])
        galleryItem.dataset.video = videoSrc
        galleryItem.dataset.src = url
        galleryItem.dataset.downloadUrl = 'false'
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
    state.ownedURLKeys.add(`preview:${id}`)

    const galleryItem = root.querySelector?.(`[data-gallery-id="${cssEscape(id)}"]`)
    if (galleryItem?.dataset) {
      galleryItem.dataset.src = url
      galleryItem.dataset.downloadUrl = 'false'
    }

    updateLightboxItemSource(id, url)

    if (state.activePreviewID === id) {
      updateActiveLightboxImage(url)
    }
  }

  function destroy() {
    state.generation += 1
    destroyLightbox()
    disconnectObserver()
    cancelLightboxRefresh()
    state.pendingExpansion = false
    state.retryBatch = null
    state.pullRequestErrored = false
    state.initialPullUnresolved = false
    state.pendingSuccessfulIndices.clear()
    revokeAllUrls()
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
    cancelLightboxRefresh()
  }

  function revokeAllUrls() {
    const revoked = new Set()
    for (const key of state.ownedURLKeys) {
      const url = state.urls.get(key)
      if (url && !revoked.has(url)) {
        revokeObjectURL(url)
        revoked.add(url)
      }
    }
    state.urls.clear()
    state.ownedURLKeys.clear()
  }

  function updateThumbnailProgress() {
    const total = state.items.length
    const completed = Math.min(total, state.loadedThumbs + state.unavailableThumbs)
    const progressTotal = state.thumbnailMode === 'pull-v1' ? state.requestedThumbs : total
    const pct = progressTotal === 0 ? 100 : Math.min(100, Math.round((completed / progressTotal) * 100))
    const text = root.querySelector?.('.gallery-progress-text')
    const fill = root.querySelector?.('.gallery-progress-fill')
    const progress = root.querySelector?.('.gallery-progress')
    if (text) {
      if (state.thumbnailMode === 'pull-v1') {
        text.textContent = `${state.requestedThumbs} requested · ${state.loadedThumbs} loaded · ${state.unavailableThumbs} unavailable · ${total} total`
      } else {
        text.textContent = state.unavailableThumbs > 0
          ? `${state.loadedThumbs} / ${total} thumbnails (${state.unavailableThumbs} unavailable)`
          : `${state.loadedThumbs} / ${total} thumbnails`
      }
    }
    if (fill?.style) fill.style.width = `${pct}%`
    const finished = state.thumbnailMode === 'pull-v1'
      ? state.renderedCount >= total && !state.inFlight && completed >= state.requestedThumbs
      : total > 0 && completed >= total
    if (progress?.classList && finished) {
      progress.classList.add('hidden')
    } else if (progress?.classList) {
      progress.classList.remove('hidden')
    }
  }

  function requestBatch(start, count, onAccepted) {
    if (state.thumbnailMode !== 'pull-v1' || state.inFlight || count <= 0) return false
    thumbnailRequestSequence += 1
    const generation = state.generation
    const request = {
      type: 'thumbnail_batch_request',
      request_id: `thumb-${Date.now().toString(36)}-${thumbnailRequestSequence}`,
      start,
      count,
    }
    state.inFlight = { requestId: request.request_id, start, count }
    state.requestedThumbs += count
    updateThumbnailProgress()
    updateLoadMoreButton()
    const rollback = () => {
      if (state.generation !== generation || state.inFlight?.requestId !== request.request_id) return
      state.inFlight = null
      state.requestedThumbs = Math.max(0, state.requestedThumbs - count)
      state.pendingExpansion = false
      state.retryBatch = { start, count, onAccepted }
      updateThumbnailProgress()
      updateLoadMoreButton()
    }
    const accept = () => {
      if (state.generation !== generation || state.inFlight?.requestId !== request.request_id) return
      state.retryBatch = null
      onAccepted?.()
      updateLoadMoreButton()
    }
    let result
    try {
      result = onThumbnailBatchRequest?.(request)
    } catch {
      rollback()
      return false
    }
    if (result && typeof result.then === 'function') {
      Promise.resolve(result).then((accepted) => {
        if (accepted === false) rollback()
        else accept()
      }, rollback)
      return true
    }
    if (result === false) {
      rollback()
      return false
    }
    accept()
    return true
  }

  function requestNextWindow() {
    if (state.thumbnailMode !== 'pull-v1') return false
    if (state.inFlight) {
      state.pendingExpansion = true
      return false
    }
    if (state.retryBatch) {
      const retry = state.retryBatch
      return requestBatch(retry.start, retry.count, retry.onAccepted)
    }
    if (state.renderedCount >= state.items.length) return false
    const start = state.renderedCount
    const count = Math.min(THUMBNAIL_WINDOW_SIZE, state.items.length - start)
    return requestBatch(start, count, () => {
      const grid = root.querySelector?.('.gallery-grid')
      grid?.insertAdjacentHTML?.('beforeend', renderItems(state.items.slice(start, start + count), start))
      state.renderedCount += count
    })
  }

  function updateLoadMoreButton() {
    const button = root.querySelector?.('.gallery-load-more')
    if (!button) return
    const atEnd = state.renderedCount >= state.items.length && !state.retryBatch
    button.disabled = Boolean(state.inFlight) || atEnd
    button.hidden = atEnd
    button.textContent = state.inFlight ? 'Loading thumbnails…' : 'Load More'
    button.setAttribute?.('aria-busy', String(Boolean(state.inFlight)))
  }

  function initObserver() {
    if (state.thumbnailMode !== 'pull-v1' || !createIntersectionObserver) return
    const sentinel = root.querySelector?.('.gallery-sentinel')
    if (!sentinel) return
    state.observer = createIntersectionObserver((entries) => {
      if (entries.some((entry) => entry.isIntersecting)) requestNextWindow()
    }, { rootMargin: '400px 0px' })
    state.observer?.observe?.(sentinel)
  }

  function disconnectObserver() {
    state.observer?.disconnect?.()
    state.observer = null
  }

  function markThumbnailUnavailable(index) {
    const tile = root.querySelector?.(`[data-gallery-index="${index}"]`)
    tile?.classList?.add?.('gallery-item-unavailable')
    tile?.setAttribute?.('aria-disabled', 'true')
    const img = tile?.querySelector?.('img')
    if (img) img.alt = `${state.items[index]?.name || 'Asset'} thumbnail unavailable`
  }

  function updateLightboxItemThumbnail(index, item, url) {
    const lightboxItem = state.lightbox?.galleryItems?.[index]
    if (!lightboxItem) return
    lightboxItem.thumb = url
    if (item.mimeType?.startsWith('video/')) {
      lightboxItem.poster = url
    } else if (!state.urls.has(`preview:${item.id}`)) {
      lightboxItem.src = url
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
    const id = state.activePreviewID
    const itemIndex = state.items.findIndex((item) => item.id === id)
    let attempts = 0
    const tryShow = () => {
      if (state.activePreviewID !== id) return
      const currentSlide = lightboxRoot?.querySelector?.('.lg-current')
      const datasetIndex = Number(currentSlide?.dataset?.index)
      const idIndex = Number(currentSlide?.id?.match?.(/-(\d+)$/)?.[1])
      const currentIndex = Number.isInteger(datasetIndex) ? datasetIndex : idIndex
      if (Number.isInteger(currentIndex) && currentIndex !== itemIndex) return

      let imgWrap = currentSlide?.querySelector?.('.lg-img-wrap')
        || lightboxRoot?.querySelector?.('.lg-current .lg-img-wrap')
      if (!imgWrap) {
        const createElement = lightboxRoot?.createElement?.bind(lightboxRoot)
          || globalThis.document?.createElement?.bind(globalThis.document)
        if (currentSlide && createElement) {
          // LightGallery can replace a revisited video slide with only its
          // error message. Rebuild the wrapper instead of waiting for markup
          // that will never return on its own.
          imgWrap = createElement('div')
          imgWrap.className = 'lg-img-wrap'
          currentSlide.innerHTML = ''
          currentSlide.appendChild?.(imgWrap)
        } else {
          attempts++
          if (attempts < 10) setTimeout(tryShow, 50)
          return
        }
      }
      const existing = imgWrap.querySelector('video.lg-video')
      if (existing && state._lastVideoID === id) {
        if (url && existing._sharebridgeMediaUrl !== url) {
          existing._sharebridgeMediaUrl = url
          existing.src = freshVideoPlaybackURL(url)
        }
        else if (!url) {
          const thumbUrl = state.urls.get(`thumb:${id}`)
            || root.querySelector?.(`[data-gallery-id="${cssEscape(id)}"]`)?.dataset?.src
          if (thumbUrl) existing.poster = thumbUrl
        }
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
        video.src = freshVideoPlaybackURL(url)
        video.autoplay = true
      } else {
        const thumbUrl = state.urls.get(`thumb:${id}`)
          || root.querySelector?.(`[data-gallery-id="${cssEscape(id)}"]`)?.dataset?.src
        if (thumbUrl) video.poster = thumbUrl
      }
      imgWrap.appendChild(video)
      state._lastVideoID = id
    }
    tryShow()
  }

  function freshVideoPlaybackURL(url) {
    const separator = url.includes('?') ? '&' : '?'
    state.videoPlaybackSequence += 1
    return `${url}${separator}play=${state.videoPlaybackSequence}`
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
    video.playsInline = true
    video.setAttribute('playsinline', '')
    video.style.maxWidth = '100%'
    video.style.maxHeight = '80vh'
		video.style.margin = '0 auto'
		video.style.objectFit = 'contain'
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
    button.className = 'lg-icon lg-download sharebridge-lightbox-download'
    button.title = 'Download'
    button.setAttribute?.('aria-label', 'Download')
    button.textContent = ''
    button.addEventListener?.('click', (event) => {
      event.preventDefault?.()
      if (state.activePreviewID) onDownloadRequest?.(state.activePreviewID)
    })
    toolbar.appendChild?.(button)
    state.lightboxDownloadButton = button
  }

  return {
    handleThumbnailList,
    handleThumbnailData,
    handleThumbnailComplete,
    handleThumbnailBatchComplete,
    handleThumbnailBatchError,
    handlePreviewData,
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
    <section class="gallery-progress${count === 0 ? ' hidden' : ''}" aria-live="polite">
      <div class="gallery-progress-row">
        <span class="gallery-progress-text">0 / ${count} thumbnails</span>
      </div>
      <div class="gallery-progress-track">
        <div class="gallery-progress-fill" style="width: 0%"></div>
      </div>
    </section>
    <section class="gallery-grid">
      ${renderItems(items, 0)}
    </section>
    ${progressive ? `
      <div class="gallery-progressive-controls">
        <button class="gallery-load-more" type="button" aria-busy="false">Load More</button>
        <div class="gallery-sentinel" aria-hidden="true"></div>
      </div>
    ` : ''}
  `
}

const THUMBNAIL_WINDOW_SIZE = 120
const LIGHTBOX_PLACEHOLDER_SRC = 'data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///ywAAAAAAQABAAACAUwAOw=='

function renderItems(items, startIndex) {
  return items.map((item, offset) => renderItem(item, startIndex + offset)).join('')
}

function renderItem(item, globalIndex) {
  const name = item.name || 'Untitled asset'
  const isVideo = item.mimeType?.startsWith('video/')
  const duration = isVideo ? formatDuration(item.duration) : ''
  const videoAttr = isVideo
    ? ` data-video='[{"src":"${LIGHTBOX_PLACEHOLDER_SRC}","type":"video/mp4"}]'`
    : ''
  return `
    <button class="gallery-item" type="button" data-gallery-id="${escapeHTML(item.id)}" data-gallery-index="${globalIndex}" data-src="${LIGHTBOX_PLACEHOLDER_SRC}" data-download-url="${LIGHTBOX_PLACEHOLDER_SRC}"${videoAttr} aria-label="Open ${escapeHTML(name)}">
      <img data-thumb-id="${escapeHTML(item.id)}" alt="${escapeHTML(name)}" loading="lazy" decoding="async">
      <span class="gallery-download" role="button" tabindex="0" data-gallery-id="${escapeHTML(item.id)}" aria-label="Download ${escapeHTML(name)}">↓</span>
      ${duration ? `<span class="gallery-duration">${escapeHTML(duration)}</span>` : ''}
    </button>
  `
}

function createDynamicLightboxItem(item) {
  const name = escapeHTML(item.name || 'Untitled asset')
  const entry = {
    src: LIGHTBOX_PLACEHOLDER_SRC,
    thumb: LIGHTBOX_PLACEHOLDER_SRC,
    alt: name,
    subHtml: `<p>${name}</p>`,
    downloadUrl: 'false',
  }
  if (item.mimeType?.startsWith('video/')) {
    entry.poster = LIGHTBOX_PLACEHOLDER_SRC
    entry.video = JSON.stringify([{ src: LIGHTBOX_PLACEHOLDER_SRC, type: 'video/mp4' }])
  }
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
