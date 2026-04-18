export function getConnectionStatusInfo(pendingConnInfo, connections) {
  if (!pendingConnInfo?.agentPeerId) return { type: 'relay', addr: '' };
  const conns = connections ?? [];
  for (const conn of conns) {
    if (conn?.remotePeer?.toString?.() !== pendingConnInfo.agentPeerId) continue;
    const addr = conn?.remoteAddr?.toString?.() ?? '';
    if (addr.includes('/webrtc')) return { type: 'direct', addr };
  }
  return { type: 'relay', addr: '' };
}

export function getConnectionBadgeType(pendingConnInfo, connections) {
  return getConnectionStatusInfo(pendingConnInfo, connections).type;
}

export function shouldResetUiOnSignalingClose(dataChannel) {
  if (!dataChannel) return true;
  return dataChannel.readyState === 'closing' || dataChannel.readyState === 'closed';
}
