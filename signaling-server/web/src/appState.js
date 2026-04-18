export function getConnectionBadgeType(pendingConnInfo) {
  return pendingConnInfo?.dcutrAllowed ? 'direct' : 'relay';
}

export function shouldResetUiOnSignalingClose(dataChannel) {
  if (!dataChannel) return true;
  return dataChannel.readyState === 'closing' || dataChannel.readyState === 'closed';
}
