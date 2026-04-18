import { test } from 'node:test';
import assert from 'node:assert/strict';
import { getConnectionBadgeType, shouldResetUiOnSignalingClose } from './appState.js';

test('getConnectionBadgeType reflects the active libp2p transport', () => {
  assert.equal(getConnectionBadgeType({ agentPeerId: 'peer-a' }, []), 'relay');
  assert.equal(
    getConnectionBadgeType(
      { agentPeerId: 'peer-a' },
      [{ remotePeer: { toString: () => 'peer-a' }, remoteAddr: { toString: () => '/ip4/127.0.0.1/tcp/9001/ws/p2p/relay/p2p-circuit' } }],
    ),
    'relay',
  );
  assert.equal(
    getConnectionBadgeType(
      { agentPeerId: 'peer-a' },
      [{ remotePeer: { toString: () => 'peer-a' }, remoteAddr: { toString: () => '/ip4/127.0.0.1/udp/5000/webrtc-direct/certhash/uEi...' } }],
    ),
    'direct',
  );
  assert.equal(
    getConnectionBadgeType(
      { agentPeerId: 'peer-a' },
      [{ remotePeer: { toString: () => 'peer-b' }, remoteAddr: { toString: () => '/ip4/127.0.0.1/udp/5000/webrtc-direct' } }],
    ),
    'relay',
  );
});

test('shouldResetUiOnSignalingClose keeps an active transport alive', () => {
  assert.equal(shouldResetUiOnSignalingClose(undefined), true);
  assert.equal(shouldResetUiOnSignalingClose({ readyState: 'closed' }), true);
  assert.equal(shouldResetUiOnSignalingClose({ readyState: 'closing' }), true);
  assert.equal(shouldResetUiOnSignalingClose({ readyState: 'connecting' }), false);
  assert.equal(shouldResetUiOnSignalingClose({ readyState: 'open' }), false);
});
