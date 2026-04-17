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
    streamMuxers: [yamux()],
    services: { identify: identify() },
  });
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
//   6. return a LibP2PDataChannel wrapping the stream (readyState='open')
//
// Throws on any failure. The caller handles UI feedback.
export async function connect(node, { relayMultiaddr, agentPeerId, jwt, shareCode, connId }) {
  const relayAddr = multiaddr(relayMultiaddr);
  await node.dial(relayAddr);

  // Step 2: JWT handshake on the relay.
  const authStream = await node.dialProtocol(relayAddr, RELAY_AUTH_PROTOCOL);
  const jwtBytes = new TextEncoder().encode(JSON.stringify({ type: 'jwt', token: jwt }));
  await authStream.sink([writeFrame(FRAME_TEXT, jwtBytes)]);
  await expectAck(authStream);
  // leave authStream open — closing it would tear the circuit on some relay
  // implementations. Relay will close it on token expiry or session end.

  // Step 3+4: dial agent through the circuit by composing a p2p-circuit addr.
  const circuitAddr = multiaddr(
    `${relayMultiaddr}/p2p-circuit/p2p/${agentPeerId}`
  );
  const fileStream = await node.dialProtocol(circuitAddr, FILE_PROTOCOL);

  // Step 5: open envelope.
  const envelope = JSON.stringify({ type: 'open', share_code: shareCode, conn_id: connId });
  await fileStream.sink([writeFrame(FRAME_TEXT, new TextEncoder().encode(envelope))]);

  // Step 6: wrap and return. Caller starts the read pump.
  const channel = new LibP2PDataChannel(fileStream);
  // Intentionally no await — the pump runs for the lifetime of the channel.
  channel.start();
  return channel;
}

async function expectAck(stream) {
  const decoder = new FrameDecoder();
  for await (const chunk of stream.source) {
    const bytes = chunk.subarray ? chunk.subarray() : chunk;
    for (const frame of decoder.push(bytes)) {
      if (frame.kind !== FRAME_TEXT) throw new Error('relay ack not text frame');
      const msg = JSON.parse(new TextDecoder().decode(frame.payload));
      if (msg.type === 'auth_ok') return;
      if (msg.type === 'error') throw new Error(`relay auth rejected: ${msg.message || 'unknown'}`);
      throw new Error(`unexpected relay message: ${msg.type}`);
    }
  }
  throw new Error('relay closed stream before ack');
}