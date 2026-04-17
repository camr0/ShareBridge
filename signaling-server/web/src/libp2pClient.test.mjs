import { test } from 'node:test';
import assert from 'node:assert/strict';
import { connect } from './libp2pClient.js';
import { FRAME_TEXT, writeFrame } from './frame.js';

function createReadableStream(chunks = []) {
  const queue = [...chunks];
  let waiter = null;
  let closed = false;

  return {
    sent: [],
    async send(bytes) {
      this.sent.push(bytes);
      return true;
    },
    async close() {
      closed = true;
      if (waiter) {
        waiter();
        waiter = null;
      }
    },
    push(bytes) {
      queue.push(bytes);
      if (waiter) {
        waiter();
        waiter = null;
      }
    },
    async *[Symbol.asyncIterator]() {
      while (!closed || queue.length > 0) {
        if (queue.length === 0) {
          await new Promise((resolve) => {
            waiter = resolve;
          });
          continue;
        }
        yield queue.shift();
      }
    },
  };
}

test('connect writes handshake frames but does not start the read pump before handlers are attached', async () => {
  const authOk = writeFrame(FRAME_TEXT, new TextEncoder().encode(JSON.stringify({ type: 'auth_ok' })));
  const fileList = writeFrame(FRAME_TEXT, new TextEncoder().encode(JSON.stringify({ type: 'file_list', files: [] })));
  const authStream = createReadableStream([authOk]);
  const fileStream = createReadableStream([fileList]);

  const dialCalls = [];
  const node = {
    async dial(addr) {
      dialCalls.push(['dial', addr.toString()]);
    },
    async dialProtocol(addr, protocol) {
      dialCalls.push(['dialProtocol', addr.toString(), protocol]);
      if (protocol === '/sharebridge/relay/1.0.0') return authStream;
      if (protocol === '/sharebridge/file/1.0.0') return fileStream;
      throw new Error(`unexpected protocol ${protocol}`);
    },
  };

  const channel = await connect(node, {
    relayMultiaddr: '/dns4/relay.example.com/tcp/443/wss/p2p/12D3KooRelay',
    agentPeerId: '12D3KooAgent',
    jwt: 'token-123',
    shareCode: 'share-abc',
    connId: 'conn-42',
  });

  assert.equal(authStream.sent.length, 1);
  assert.equal(fileStream.sent.length, 1);
  assert.equal(channel.readyState, 'connecting');

  const messages = [];
  channel.onmessage = (event) => {
    messages.push(event.data);
  };

  const done = channel.start();
  await new Promise((resolve) => {
    channel.onopen = resolve;
  });
  await new Promise((resolve) => setTimeout(resolve, 0));

  assert.equal(messages.length, 1);
  assert.equal(JSON.parse(messages[0]).type, 'file_list');

  channel.close();
  await done;
});
