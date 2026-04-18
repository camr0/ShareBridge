// src/libp2pClient.js
import { createLibp2p } from 'libp2p';
import { webSockets } from '@libp2p/websockets';
import { webRTC } from '@libp2p/webrtc';
import { noise } from '@chainsafe/libp2p-noise';
import { yamux } from '@chainsafe/libp2p-yamux';
import { circuitRelayTransport } from '@libp2p/circuit-relay-v2';
import { identify } from '@libp2p/identify';
import { multiaddr } from '@multiformats/multiaddr';
import { writeFrame, FRAME_TEXT, FrameDecoder } from './frame.js';
import { LibP2PDataChannel } from './dataChannelAdapter.js';

const RELAY_AUTH_PROTOCOL = '/sharebridge/relay/1.0.0';
const FILE_PROTOCOL = '/sharebridge/file/1.0.0';

function yamuxDebugEnabled() {
  if (globalThis.SHAREBRIDGE_DEBUG === true) return true;
  try {
    const params = new URLSearchParams(globalThis.location?.search ?? '');
    if (params.get('debug') === '1') return true;
  } catch {
    // ignore
  }
  try {
    return globalThis.localStorage?.getItem('sharebridge:debug') === '1';
  } catch {
    return false;
  }
}

function yamuxDebug(...args) {
  if (yamuxDebugEnabled()) {
    console.log('[sharebridge][yamux]', ...args);
  }
}

function instrumentYamuxStream(stream, muxerDirection, muxer) {
  if (stream == null || stream.__sharebridgeYamuxPatched === true) return;
  stream.__sharebridgeYamuxPatched = true;

  if (typeof stream.sendWindowUpdate === 'function') {
    const originalSendWindowUpdate = stream.sendWindowUpdate.bind(stream);
    stream.sendWindowUpdate = (...args) => {
      const beforeCapacity = stream.recvWindowCapacity;
      const beforeWindow = stream.recvWindow;
      const result = originalSendWindowUpdate(...args);
      const delta = stream.recvWindowCapacity - beforeCapacity;
      const rtt = typeof muxer.getRTT === 'function' ? muxer.getRTT() : -1;

      if (delta >= 256 * 1024 || stream.recvWindow !== beforeWindow) {
        yamuxDebug('window-update:out', {
          direction: muxerDirection,
          streamId: stream.streamId,
          delta,
          recvWindow: stream.recvWindow,
          recvWindowCapacity: stream.recvWindowCapacity,
          rtt,
        });
      }

      return result;
    };
  }

  if (typeof stream.handleWindowUpdate === 'function') {
    const originalHandleWindowUpdate = stream.handleWindowUpdate.bind(stream);
    stream.handleWindowUpdate = (frame) => {
      const before = stream.sendWindowCapacity;
      const result = originalHandleWindowUpdate(frame);
      const after = stream.sendWindowCapacity;

      if (frame?.header?.length >= 256 * 1024 || before === 0 || after > 256 * 1024) {
        yamuxDebug('window-update:in', {
          direction: muxerDirection,
          streamId: stream.streamId,
          delta: frame?.header?.length,
          sendWindowBefore: before,
          sendWindowAfter: after,
        });
      }

      return result;
    };
  }
}

function instrumentYamuxMuxer(muxer, direction) {
  if (muxer == null || muxer.__sharebridgeYamuxPatched === true) return;
  muxer.__sharebridgeYamuxPatched = true;
  yamuxDebug('muxer:created', { direction });

  if (typeof muxer.ping === 'function') {
    const originalPing = muxer.ping.bind(muxer);
    muxer.ping = async (...args) => {
      const rtt = await originalPing(...args);
      yamuxDebug('ping:rtt', { direction, rtt });
      return rtt;
    };
  }

  if (typeof muxer._newStream === 'function') {
    const originalNewStream = muxer._newStream.bind(muxer);
    muxer._newStream = (...args) => {
      const stream = originalNewStream(...args);
      instrumentYamuxStream(stream, direction, muxer);
      yamuxDebug('stream:created', {
        direction,
        streamId: stream?.streamId,
        state: stream?.state,
      });
      return stream;
    };
  }
}

export function createYamuxMuxer() {
  const factory = yamux({
    streamOptions: {
      initialStreamWindowSize: 8 * 1024 * 1024,
      maxStreamWindowSize: 16 * 1024 * 1024,
    },
  });

  return () => {
    const muxerFactory = factory();
    if (!yamuxDebugEnabled() || typeof muxerFactory.createStreamMuxer !== 'function') {
      return muxerFactory;
    }

    const originalCreateStreamMuxer = muxerFactory.createStreamMuxer.bind(muxerFactory);
    muxerFactory.createStreamMuxer = (maConn) => {
      const muxer = originalCreateStreamMuxer(maConn);
      instrumentYamuxMuxer(muxer, maConn?.direction ?? 'unknown');
      return muxer;
    };
    return muxerFactory;
  };
}

// createNode: ephemeral libp2p node. We do NOT persist the peer identity —
// the browser mints a fresh Ed25519 keypair every page load.
export async function createNode() {
  return await createLibp2p({
    transports: [
      webSockets(),                  // outbound connection to relay
      circuitRelayTransport(),       // dial agent through relay
      webRTC(),                      // DCUtR direct-path upgrade
    ],
    connectionEncrypters: [noise()],
    streamMuxers: [createYamuxMuxer()],
    services: { identify: identify() },
    connectionGater: createConnectionGater(globalThis.location?.hostname ?? ''),
  });
}

export function createConnectionGater(currentHostname) {
  return {
    denyDialMultiaddr(targetMultiaddr) {
      const addr = targetMultiaddr.toString();

      if (isLoopbackPage(currentHostname) && isLoopbackMultiaddr(addr)) {
        return false;
      }

      if (isInsecureWebSocket(addr)) {
        return true;
      }

      return isPrivateMultiaddr(addr);
    },
  };
}

// getLocalPeerId: call before knock/join so the browser can advertise its
// peer ID to the signaling server (for JWT binding).
export function getLocalPeerId(node) {
  return node.peerId.toString();
}

// connect: given a relay multiaddr string, an agent peer ID string, and a
// JWT, perform:
//   1. dial relay
//   2. open /sharebridge/relay/1.0.0, send JWT frame, await ack
//   3. dial agent via the circuit
//   4. open /sharebridge/file/1.0.0 on the agent
//   5. send the open envelope as the first text frame
//   6. return a LibP2PDataChannel wrapping the stream; caller starts read pump
//
// Throws on any failure. The caller handles UI feedback.
export async function connect(node, { relayMultiaddr, agentPeerId, jwt, shareCode, connId }) {
  const relayAddr = multiaddr(relayMultiaddr);
  await node.dial(relayAddr);

  // Step 2: JWT handshake on the relay.
  const authStream = await node.dialProtocol(relayAddr, RELAY_AUTH_PROTOCOL, {
    runOnLimitedConnection: true,
  });
  const jwtBytes = new TextEncoder().encode(JSON.stringify({ type: 'jwt', token: jwt }));
  await authStream.send(writeFrame(FRAME_TEXT, jwtBytes));
  await expectAck(authStream);
  // leave authStream open — closing it would tear the circuit on some relay
  // implementations. Relay will close it on token expiry or session end.

  // Step 3+4: dial agent through the circuit by composing a p2p-circuit addr.
  const circuitAddr = multiaddr(
    `${relayMultiaddr}/p2p-circuit/p2p/${agentPeerId}`
  );
  const fileStream = await node.dialProtocol(circuitAddr, FILE_PROTOCOL, {
    runOnLimitedConnection: true,
  });

  // Step 5: open envelope.
  const envelope = JSON.stringify({ type: 'open', share_code: shareCode, conn_id: connId });
  await fileStream.send(writeFrame(FRAME_TEXT, new TextEncoder().encode(envelope)));

  // Step 6: wrap and return. The caller wires handlers, then starts the pump.
  return new LibP2PDataChannel(fileStream);
}

async function expectAck(stream) {
  const decoder = new FrameDecoder();
  for await (const chunk of stream) {
    const bytes = chunk.subarray ? chunk.subarray() : chunk;
    for (const frame of decoder.push(bytes)) {
      if (frame.kind !== FRAME_TEXT) throw new Error('relay ack not text frame');
      let msg;
      try {
        msg = JSON.parse(new TextDecoder().decode(frame.payload));
      } catch {
        throw new Error('relay sent malformed JSON in auth response');
      }
      if (msg.type === 'auth_ok') return;
      if (msg.type === 'error') throw new Error(`relay auth rejected: ${msg.message || 'unknown'}`);
      throw new Error(`unexpected relay message: ${msg.type}`);
    }
  }
  throw new Error('relay closed stream before ack');
}

function isLoopbackPage(hostname) {
  return hostname === '127.0.0.1' || hostname === 'localhost' || hostname === '::1' || hostname === '[::1]';
}

function isLoopbackMultiaddr(addr) {
  return (
    addr.includes('/ip4/127.') ||
    addr.includes('/dns4/localhost/') ||
    addr.includes('/dns6/localhost/') ||
    addr.includes('/dns/localhost/') ||
    addr.includes('/ip6/::1/')
  );
}

function isInsecureWebSocket(addr) {
  return addr.includes('/ws') && !addr.includes('/wss');
}

function isPrivateMultiaddr(addr) {
  return (
    addr.includes('/ip4/127.') ||
    addr.includes('/ip4/10.') ||
    addr.includes('/ip4/192.168.') ||
    is172Private(addr) ||
    addr.includes('/dns4/localhost/') ||
    addr.includes('/dns6/localhost/') ||
    addr.includes('/ip6/::1/') ||
    addr.includes('/ip6/fc') ||
    addr.includes('/ip6/fd')
  );
}

function is172Private(addr) {
  const match = addr.match(/\/ip4\/172\.(\d+)\./);
  if (!match) return false;
  const octet = Number(match[1]);
  return Number.isInteger(octet) && octet >= 16 && octet <= 31;
}
