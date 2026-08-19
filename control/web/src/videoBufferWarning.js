const BUFFER_WINDOW_MS = 20_000
const BUFFER_EVENT_THRESHOLD = 2

export function createVideoBufferWarningMonitor({
  video,
  showWarning,
  hideWarning,
  now = Date.now,
}) {
  let playbackStarted = false
  let warningShown = false
  let bufferEvents = []

  const onPlaying = () => {
    playbackStarted = true
    if (warningShown) hideWarning()
  }

  const onBuffer = () => {
    if (!playbackStarted || warningShown) return

    const timestamp = now()
    bufferEvents = bufferEvents.filter((time) => timestamp - time <= BUFFER_WINDOW_MS)
    bufferEvents.push(timestamp)
    if (bufferEvents.length >= BUFFER_EVENT_THRESHOLD) {
      warningShown = true
      showWarning()
    }
  }

  video.addEventListener('playing', onPlaying)
  video.addEventListener('waiting', onBuffer)
  video.addEventListener('stalled', onBuffer)

  return {
    destroy() {
      video.removeEventListener('playing', onPlaying)
      video.removeEventListener('waiting', onBuffer)
      video.removeEventListener('stalled', onBuffer)
    },
  }
}
