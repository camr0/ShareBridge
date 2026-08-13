window.__bench = { received: 0, firstByteTs: 0, lastByteTs: 0 }

window.__benchSummary = () => {
  const ms = window.__bench.lastByteTs - window.__bench.firstByteTs
  return {
    received: window.__bench.received,
    elapsedMs: ms,
    mbps: ms > 0 ? (window.__bench.received * 8 / (ms / 1000)) / 1e6 : 0,
  }
}

function note(bytes) {
  const now = performance.now()
  if (window.__bench.firstByteTs === 0) window.__bench.firstByteTs = now
  window.__bench.lastByteTs = now
  window.__bench.received += bytes
}

async function run() {
  const res = await fetch('/start', { method: 'POST' })
  const cfg = await res.json()
  const pc = new RTCPeerConnection({ iceServers: [] })

  pc.ondatachannel = (e) => {
    const ch = e.channel
    ch.binaryType = 'arraybuffer'
    if (cfg.mode === 'prod' && ch.label !== 'bulk') return
    ch.onmessage = (m) => {
      if (cfg.mode === 'prod') {
        if (m.data.byteLength < 14) return
        note(m.data.byteLength - 14)
      } else note(m.data.byteLength)
    }
  }

  await pc.setRemoteDescription({ type: 'offer', sdp: cfg.offer })
  const answer = await pc.createAnswer()
  await pc.setLocalDescription(answer)
  await new Promise((resolve) => {
    if (pc.iceGatheringState === 'complete') return resolve()
    pc.onicegatheringstatechange = () => {
      if (pc.iceGatheringState === 'complete') resolve()
    }
  })
  await fetch('/answer', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ sdp: pc.localDescription.sdp }),
  })
}

run()
