// ShareBridge recipient entry script.
//
// Served at /s/{code}/static/app.js. It derives the share code from the URL,
// builds the code-prefixed URL factories for the HTTP content endpoints, and
// hands them to the (transport-agnostic) gallery presentation layer. Downloads
// are native browser navigations to /asset/{id} and /archive/{token}/{part}
// (the server sets Content-Disposition: attachment); video playback streams
// from /asset/{id}/playback.

import { createGalleryController } from './gallery.js'
import { createVideoBufferWarningMonitor } from './videoBufferWarning.js'

const VIDEO_BUFFER_WARNING_CLASS = 'sharebridge-video-buffer-warning'
const VIDEO_BUFFER_WARNING_TEXT =
  'This video is buffering on the current connection. A lower-bitrate Immich transcode may improve playback.'

function deriveShareCode() {
  const parts = window.location.pathname.split('/').filter(Boolean)
  if (parts[0] === 's' && parts[1]) return decodeURIComponent(parts[1])
  return ''
}

const code = deriveShareCode()
const base = `/s/${encodeURIComponent(code)}`

const thumbUrl = (id) => `${base}/thumb/${encodeURIComponent(id)}`
const previewUrl = (id) => `${base}/preview/${encodeURIComponent(id)}`
const assetUrl = (id) => `${base}/asset/${encodeURIComponent(id)}`
const playbackUrl = (id) => `${base}/asset/${encodeURIComponent(id)}/playback`
const archiveManifestUrl = () => `${base}/archive`
const archivePartUrl = (token, index) => `${base}/archive/${encodeURIComponent(token)}/${index}`
const placeholderUrl = () => `${base}/static/placeholder.gif`

let controller = null
let activeMonitor = null

function setStatus(message) {
  const el = document.getElementById('status')
  if (el) el.textContent = message
}

function showWarning(container) {
  if (!container || container.querySelector(`.${VIDEO_BUFFER_WARNING_CLASS}`)) return
  const warning = document.createElement('div')
  warning.className = VIDEO_BUFFER_WARNING_CLASS
  warning.setAttribute('role', 'status')
  warning.textContent = VIDEO_BUFFER_WARNING_TEXT
  container.appendChild(warning)
}

function hideWarning(container) {
  container?.querySelector?.(`.${VIDEO_BUFFER_WARNING_CLASS}`)?.remove?.()
}

function attachBufferWarning(video) {
  detachBufferWarning()
  const container = video?.closest?.('.lg-img-wrap') || video?.parentElement || null
  if (!video || !container) return
  activeMonitor = createVideoBufferWarningMonitor({
    video,
    showWarning: () => showWarning(container),
    hideWarning: () => hideWarning(container),
  })
}

function detachBufferWarning() {
  activeMonitor?.destroy?.()
  activeMonitor = null
}

async function loadGallery() {
  const root = document.getElementById('gallery-root')
  const section = document.getElementById('gallery-section')
  if (!root) return
  setStatus('Loading gallery…')
  let data
  try {
    const response = await fetch(`${base}/items`)
    if (!response.ok) throw new Error(`items ${response.status}`)
    data = await response.json()
  } catch {
    setStatus('Failed to load gallery')
    return
  }
  controller = createGalleryController({
    root,
    thumbUrl,
    previewUrl,
    playbackUrl,
    assetUrl,
    archiveManifestUrl,
    archivePartUrl,
    placeholderUrl,
    onVideoElement: attachBufferWarning,
    onVideoClose: detachBufferWarning,
  })
  section?.classList?.remove('hidden')
  controller.render(data.items || [], {
    albumName: data.albumName || '',
    albumDescription: data.albumDescription || '',
  })
  setStatus('')
}

document.addEventListener('DOMContentLoaded', () => {
  if (code) loadGallery()
  else setStatus('Missing share code')
})
