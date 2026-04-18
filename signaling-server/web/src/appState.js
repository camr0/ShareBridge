export function getConnectionBadgeType(pendingConnInfo, connections) {
  if (!pendingConnInfo?.agentPeerId) return 'relay';
  const conns = connections ?? [];
  for (const conn of conns) {
    if (conn?.remotePeer?.toString?.() !== pendingConnInfo.agentPeerId) continue;
    const addr = conn?.remoteAddr?.toString?.() ?? '';
    if (addr.includes('/webrtc')) return 'direct';
  }
  return 'relay';
}

export function shouldResetUiOnSignalingClose(dataChannel) {
  if (!dataChannel) return true;
  return dataChannel.readyState === 'closing' || dataChannel.readyState === 'closed';
}
