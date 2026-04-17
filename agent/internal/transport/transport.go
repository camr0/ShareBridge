package transport

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	circuitv2client "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	webrtc "github.com/libp2p/go-libp2p/p2p/transport/webrtc"
	ws "github.com/libp2p/go-libp2p/p2p/transport/websocket"
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
	relay    *peer.AddrInfo
	ctx      context.Context
	cancel   context.CancelFunc
	ensureCh chan struct{}
	ready    atomic.Bool
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
		libp2p.ListenAddrStrings(
			"/ip4/0.0.0.0/udp/0/webrtc-direct",
			"/ip6/::/udp/0/webrtc-direct",
		),
		libp2p.Transport(ws.New),     // outbound relay connection over ws/wss
		libp2p.Transport(webrtc.New), // direct WebRTC path for DCUtR upgrades
	)
	if err != nil {
		return nil, fmt.Errorf("libp2p.New: %w", err)
	}

	transportCtx := context.Background()
	if ctx != nil {
		transportCtx = ctx
	}
	transportCtx, cancel := context.WithCancel(transportCtx)

	t := &Transport{
		host:     h,
		ctx:      transportCtx,
		cancel:   cancel,
		ensureCh: make(chan struct{}, 1),
	}
	h.SetStreamHandler(FileProtocolID, t.handleStream)
	h.Network().Notify(&network.NotifyBundle{
		DisconnectedF: func(_ network.Network, conn network.Conn) {
			if t.isRelayPeer(conn.RemotePeer()) {
				t.ready.Store(false)
				t.triggerEnsure()
			}
		},
	})
	go t.ensureRelayLoop()
	return t, nil
}

// Host exposes the underlying libp2p Host (used in tests).
func (t *Transport) Host() host.Host { return t.host }

// PeerID returns the libp2p peer ID string.
func (t *Transport) PeerID() string { return t.host.ID().String() }

// Ready reports whether the relay connection is established and has an active reservation.
func (t *Transport) Ready() bool { return t.ready.Load() }

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
	t.mu.Lock()
	t.relay = info
	t.mu.Unlock()

	if err := t.connectAndReserve(ctx, *info); err != nil {
		t.ready.Store(false)
		t.triggerEnsure()
		return err
	}
	return nil
}

// ConnectDirect dials a peer by its addresses. Test-only helper used in
// transport_test.go — production code always goes via DialRelay.
func (t *Transport) ConnectDirect(ctx context.Context, addrs []multiaddr.Multiaddr, id peer.ID) error {
	return t.host.Connect(ctx, peer.AddrInfo{ID: id, Addrs: addrs})
}

// Close shuts down the Host.
func (t *Transport) Close() error {
	t.cancel()
	return t.host.Close()
}

func (t *Transport) connectAndReserve(ctx context.Context, info peer.AddrInfo) error {
	if err := t.host.Connect(ctx, info); err != nil {
		return fmt.Errorf("connect to relay: %w", err)
	}
	if _, err := circuitv2client.Reserve(ctx, t.host, info); err != nil {
		return fmt.Errorf("reserve circuit relay slot: %w", err)
	}
	t.ready.Store(true)
	return nil
}

func (t *Transport) ensureRelayLoop() {
	for {
		select {
		case <-t.ctx.Done():
			return
		case <-t.ensureCh:
			backoff := 250 * time.Millisecond
			for {
				if t.ctx.Err() != nil || t.Ready() {
					break
				}
				info, ok := t.relayInfo()
				if !ok {
					break
				}
				attemptCtx, cancel := context.WithTimeout(t.ctx, time.Second)
				err := t.connectAndReserve(attemptCtx, info)
				cancel()
				if err == nil {
					break
				}
				select {
				case <-t.ctx.Done():
					return
				case <-time.After(backoff):
				}
				if backoff < 2*time.Second {
					backoff *= 2
				}
			}
		}
	}
}

func (t *Transport) relayInfo() (peer.AddrInfo, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.relay == nil {
		return peer.AddrInfo{}, false
	}
	return *t.relay, true
}

func (t *Transport) isRelayPeer(id peer.ID) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.relay != nil && t.relay.ID == id
}

func (t *Transport) triggerEnsure() {
	select {
	case t.ensureCh <- struct{}{}:
	default:
	}
}
