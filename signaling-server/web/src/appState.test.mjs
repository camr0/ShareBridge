import { test } from 'node:test';
import assert from 'node:assert/strict';
import { getConnectionBadgeType, shouldResetUiOnSignalingClose } from './appState.js';

test('getConnectionBadgeType shows Direct for direct shares', () => {
  assert.equal(getConnectionBadgeType(null), 'relay');
  assert.equal(getConnectionBadgeType({ dcutrAllowed: false }), 'relay');
  assert.equal(getConnectionBadgeType({ dcutrAllowed: true }), 'direct');
});

test('shouldResetUiOnSignalingClose keeps an active transport alive', () => {
  assert.equal(shouldResetUiOnSignalingClose(undefined), true);
  assert.equal(shouldResetUiOnSignalingClose({ readyState: 'closed' }), true);
  assert.equal(shouldResetUiOnSignalingClose({ readyState: 'closing' }), true);
  assert.equal(shouldResetUiOnSignalingClose({ readyState: 'connecting' }), false);
  assert.equal(shouldResetUiOnSignalingClose({ readyState: 'open' }), false);
});
