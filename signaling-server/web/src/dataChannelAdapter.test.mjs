import { test } from 'node:test';
import assert from 'node:assert/strict';
import { LibP2PDataChannel } from './dataChannelAdapter.js';
import { FRAME_TEXT, FRAME_BINARY, writeFrame, readFrameFromChunks } from './frame.js';

function createMockStream() {
  const sent = [];
  const inbound = [];
  let waiter = null;
  let closed = false;

  const stream = {
    async send(bytes) {
      sent.push(bytes);
      return true;
    },
    async close() {
      closed = true;
      if (waiter) {
        waiter();
        waiter = null;
      }
    },
    pushIncoming(bytes) {
      inbound.push(bytes);
      if (waiter) {
        waiter();
        waiter = null;
      }
    },
    get sent() {
      return sent;
    },
    get closed() {
      return closed;
    },
    async *[Symbol.asyncIterator]() {
      while (!closed || inbound.length > 0) {
        if (inbound.length === 0) {
          await new Promise((resolve) => {
            waiter = resolve;
          });
          continue;
        }
        yield inbound.shift();
      }
    },
  };

  return stream;
}

test('sends text and binary frames through libp2p stream.send', async () => {
  const stream = createMockStream();
  const channel = new LibP2PDataChannel(stream);
  const done = channel.start();

  await new Promise((resolve) => {
    channel.onopen = resolve;
  });

  await channel.send(JSON.stringify({ type: 'ping' }));
  await channel.sendBinary(new Uint8Array([1, 2, 3]));
  channel.close();
  await done;

  assert.equal(stream.sent.length, 2);

  const [textFrame] = [...readFrameFromChunks([stream.sent[0]])];
  assert.equal(textFrame.kind, FRAME_TEXT);
  assert.equal(new TextDecoder().decode(textFrame.payload), '{"type":"ping"}');

  const [binaryFrame] = [...readFrameFromChunks([stream.sent[1]])];
  assert.equal(binaryFrame.kind, FRAME_BINARY);
  assert.deepEqual(Array.from(binaryFrame.payload), [1, 2, 3]);
});

test('reads frames from the stream async iterator', async () => {
  const stream = createMockStream();
  const channel = new LibP2PDataChannel(stream);
  const messages = [];

  channel.onmessage = (event) => {
    messages.push(event.data);
  };

  const done = channel.start();
  await new Promise((resolve) => {
    channel.onopen = resolve;
  });

  stream.pushIncoming(writeFrame(FRAME_TEXT, new TextEncoder().encode('hello')));
  stream.pushIncoming(writeFrame(FRAME_BINARY, new Uint8Array([9, 8, 7])));

  await new Promise((resolve) => setTimeout(resolve, 0));
  channel.close();
  await done;

  assert.equal(messages[0], 'hello');
  assert.ok(messages[1] instanceof ArrayBuffer);
  assert.deepEqual(Array.from(new Uint8Array(messages[1])), [9, 8, 7]);
});
