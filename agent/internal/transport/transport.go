package transport

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	circuitv2client "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	webrtc "github.com/libp2p/go-libp2p/p2p/transport/webrtc"
	"github.com/multiformats/go-multiaddr"
)

// FileProtocolID is the libp2p protocol for ShareBridge file-transfer streams.
const FileProtocolID protocol.ID = "/sharebridge/file/1.0.0"

// StreamInfo is what the daemon's stream handler receives.
type StreamInfo struct {
	Stream network.Stream
	Peer   peer.ID
}

// StreamHandler is the daemon's callback for each inbound file stream.
type StreamHandler func(StreamInfo)

// Options configures Transport.
type Options struct {
	PrivKey crypto.PrivKey // libp2p identity; required
}

// Transport owns the libp2p Host and the outbound relay connection.
type Transport struct {
	host host.Host

	mu       sync.Mutex
	onStream StreamHandler
}

// New creates a libp2p Host with the given identity and WebRTC transport for
// DCUtR direct-path upgrades. It does not dial the relay — call DialRelay afterwards.
// The Host uses NoListenAddrs for traditional transports (tcp/ws) since the agent
// is outbound-only, but WebRTC is enabled for hole-punching via DCUtR.
func New(ctx context.Context, opts Options) (*Transport, error) {
	if opts.PrivKey == nil {
		return nil, errors.New("transport: PrivKey is required")
	}

	h, err := libp2p.New(
		libp2p.Identity(opts.PrivKey),
		libp2p.NoListenAddrs,          // no tcp/ws listening — agent is outbound via relay
		libp2p.Transport(webrtc.New), // WebRTC transport for DCUtR direct-path upgrade
	)
	if err != nil {
		return nil, fmt.Errorf("libp2p.New: %w", err)
	}

	t := &Transport{host: h}
	h.SetStreamHandler(FileProtocolID, t.handleStream)
	return t, nil
}

// Host exposes the underlying libp2p Host (used in tests).
func (t *Transport) Host() host.Host { return t.host }

// PeerID returns the libp2p peer ID string.
func (t *Transport) PeerID() string { return t.host.ID().String() }

// OnStream sets the callback for each inbound file stream. Must be called
// before any stream can arrive; typically wired during daemon init.
func (t *Transport) OnStream(h StreamHandler) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onStream = h
}

func (t *Transport) handleStream(s network.Stream) {
	t.mu.Lock()
	h := t.onStream
	t.mu.Unlock()
	if h == nil {
		s.Reset()
		return
	}
	h(StreamInfo{Stream: s, Peer: s.Conn().RemotePeer()})
}

// DialRelay dials the relay multiaddr, reserves a circuit relay v2 slot, and keeps
// a persistent connection open. relayMultiaddr must include the relay's /p2p/<peer-id>
// component. Without reservation, browsers cannot open circuits to this agent.
func (t *Transport) DialRelay(ctx context.Context, relayMultiaddr string) error {
	addr, err := multiaddr.NewMultiaddr(relayMultiaddr)
	if err != nil {
		return fmt.Errorf("parse relay multiaddr %q: %w", relayMultiaddr, err)
	}
	info, err := peer.AddrInfoFromP2pAddr(addr)
	if err != nil {
		return fmt.Errorf("extract peer info: %w", err)
	}
	if err := t.host.Connect(ctx, *info); err != nil {
		return fmt.Errorf("connect to relay: %w", err)
	}
	_, err = circuitv2client.Reserve(ctx, t.host, *info)
	if err != nil {
		return fmt.Errorf("reserve circuit relay slot: %w", err)
	}
	return nil
}

// ConnectDirect dials a peer by its addresses. Test-only helper used in
// transport_test.go — production code always goes via DialRelay.
func (t *Transport) ConnectDirect(ctx context.Context, addrs []multiaddr.Multiaddr, id peer.ID) error {
	return t.host.Connect(ctx, peer.AddrInfo{ID: id, Addrs: addrs})
}

// Close shuts down the Host.
func (t *Transport) Close() error { return t.host.Close() }