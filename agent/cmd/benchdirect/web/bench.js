window.__bench = { received: 0, firstByteTs: 0, lastByteTs: 0, samples: [], perConn: [] }

setInterval(() => { window.__bench.samples.push(window.__bench.received) }, 100)

window.__benchSummary = () => {
  const ms = window.__bench.lastByteTs - window.__bench.firstByteTs
  // wallMs spans first byte to now, so a stall is charged its full cost instead
  // of being hidden by a lastByteTs that simply stopped advancing.
  const wallMs = window.__bench.firstByteTs > 0 ? performance.now() - window.__bench.firstByteTs : 0
  return {
    received: window.__bench.received,
    elapsedMs: ms,
    mbps: ms > 0 ? (window.__bench.received * 8 / (ms / 1000)) / 1e6 : 0,
    wallMs,
    wallMbps: wallMs > 0 ? (window.__bench.received * 8 / (wallMs / 1000)) / 1e6 : 0,
    samples: window.__bench.samples,
    perConn: window.__bench.perConn.map((p) => ({
      received: p ? p.received : 0,
      firstByteTs: p ? p.firstByteTs : 0,
      lastByteTs: p ? p.lastByteTs : 0,
    })),
  }
}

// note records received bytes both globally and per connection index, so we can
// see whether one association starves while the others keep flowing.
function note(connIndex, bytes) {
  const now = performance.now()
  if (window.__bench.firstByteTs === 0) window.__bench.firstByteTs = now
  window.__bench.lastByteTs = now
  window.__bench.received += bytes
  let p = window.__bench.perConn[connIndex]
  if (!p) p = window.__bench.perConn[connIndex] = { received: 0, firstByteTs: 0, lastByteTs: 0 }
  if (p.firstByteTs === 0) p.firstByteTs = now
  p.lastByteTs = now
  p.received += bytes
}

function attachChannel(ch, connIndex, mode) {
  ch.binaryType = 'arraybuffer'
  if (mode === 'prod' && ch.label !== 'bulk') return
  ch.onmessage = (m) => {
    if (mode === 'prod') {
      if (m.data.byteLength < 14) return
      note(connIndex, m.data.byteLength - 14)
    } else note(connIndex, m.data.byteLength)
  }
}

async function gatherIce(pc) {
  await new Promise((resolve) => {
    if (pc.iceGatheringState === 'complete') return resolve()
    pc.onicegatheringstatechange = () => {
      if (pc.iceGatheringState === 'complete') resolve()
    }
  })
}

async function run() {
  const cfg = await (await fetch('/start', { method: 'POST' })).json()
  const count = cfg.count || 1
  window.__bench.pcs = []

  for (let i = 0; i < count; i++) {
    const { offer } = await (await fetch('/offer?i=' + i)).json()
    const pc = new RTCPeerConnection({ iceServers: [] })
    pc.ondatachannel = (e) => attachChannel(e.channel, i, cfg.mode)
    await pc.setRemoteDescription({ type: 'offer', sdp: offer })
    const answer = await pc.createAnswer()
    await pc.setLocalDescription(answer)
    await gatherIce(pc)
    await fetch('/answer', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ i, sdp: pc.localDescription.sdp }),
    })
    window.__bench.pcs.push(pc)
  }
}

run()
