package relay

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/metrics"
	"github.com/libp2p/go-libp2p/core/network"
	circuitv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	"github.com/multiformats/go-multiaddr"
)

// Config configures the relay Host.
type Config struct {
	ListenAddr     string        // multiaddr, e.g. "/ip4/127.0.0.1/tcp/9001/ws"
	AnnounceAddr   string        // optional public multiaddr advertised to peers
	PrivateKeyPath string        // PEM path; empty = ephemeral (dev/test only)
	JWTSecret      []byte
	JWTTTL         time.Duration
}

// Relay wraps a libp2p Host configured as a ShareBridge relay.
type Relay struct {
	h          host.Host
	bwc        *metrics.BandwidthCounter
	acl        *CircuitACL
	circuitSvc *circuitv2.Relay
	issuer     *Issuer
	jtis       *JTIStore
	agents     *AgentRegistry
	handler    Handler

	// OnCircuitClosed is called with quota bytes when a browser peer disconnects.
	OnCircuitClosed func(apiKeyID, shareCode string, bytesIn, bytesOut int64)
}

// New constructs and starts the relay Host. Call Start() to register protocol handlers.
func New(_ context.Context, cfg Config) (*Relay, error) {
	if len(cfg.JWTSecret) < 32 {
		return nil, errors.New("JWTSecret must be >= 32 bytes")
	}
	priv, err := loadOrGenerateKey(cfg.PrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load key: %w", err)
	}
	listenMA, err := multiaddr.NewMultiaddr(cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("parse listen addr: %w", err)
	}

	bwc := metrics.NewBandwidthCounter()

	opts := []libp2p.Option{
		libp2p.Identity(priv),
		libp2p.ListenAddrs(listenMA),
		libp2p.BandwidthReporter(bwc),
		// NOTE: Do NOT add DisableRelay() — circuit relay v2 must remain active.
	}
	if cfg.AnnounceAddr != "" {
		announceMA, err := multiaddr.NewMultiaddr(cfg.AnnounceAddr)
		if err != nil {
			return nil, fmt.Errorf("parse announce addr: %w", err)
		}
		opts = append(opts, libp2p.AddrsFactory(func(_ []multiaddr.Multiaddr) []multiaddr.Multiaddr {
			return []multiaddr.Multiaddr{announceMA}
		}))
	}

	h, err := libp2p.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("new libp2p host: %w", err)
	}

	acl := newCircuitACL(cfg.JWTTTL)

	// Start the circuit relay v2 service. This is what provides E2E Noise encryption:
	// the relay forwards ciphertext between browser and agent without being able to read it.
	// DCUtR hole-punching flows through this service automatically.
	circuitSvc, err := circuitv2.New(h, circuitv2.WithACL(acl))
	if err != nil {
		h.Close()
		return nil, fmt.Errorf("circuit relay v2: %w", err)
	}

	issuer := NewIssuer(cfg.JWTSecret, cfg.JWTTTL)
	jtis := NewJTIStore(cfg.JWTTTL)
	agents := NewAgentRegistry()

	r := &Relay{
		h:          h,
		bwc:        bwc,
		acl:        acl,
		circuitSvc: circuitSvc,
		issuer:     issuer,
		jtis:       jtis,
		agents:     agents,
	}
	r.handler = Handler{
		Issuer:  issuer,
		JTIs:    jtis,
		Agents:  agents,
		ACL:     acl,
		AuthTTL: cfg.JWTTTL,
	}
	return r, nil
}

// Host returns the underlying libp2p Host.
func (r *Relay) Host() host.Host { return r.h }

// Issuer returns the JWT issuer (used by agent_ws to mint tokens).
func (r *Relay) Issuer() *Issuer { return r.issuer }

// Agents returns the agent registry (populated in 13b when agents connect).
func (r *Relay) Agents() *AgentRegistry { return r.agents }

// SetCodeResolver wires the share_code → api_key_id lookup after construction.
func (r *Relay) SetCodeResolver(f func(shareCode string) (apiKeyID string, ok bool)) {
	r.handler.CodeToAPIKey = f
}

// Start registers the /sharebridge/relay/1.0.0 auth handler and the bandwidth notifier.
// Returns the relay's listen multiaddrs.
func (r *Relay) Start() []multiaddr.Multiaddr {
	r.h.SetStreamHandler(ProtocolID, r.handler.Handle)

	// When a browser peer disconnects, read their cumulative bandwidth and
	// report it to the quota accumulator. Browser peer IDs are ephemeral
	// (new Ed25519 key per page load), so cumulative == per-circuit.
	r.h.Network().Notify(&network.NotifyBundle{
		DisconnectedF: func(_ network.Network, conn network.Conn) {
			peerID := conn.RemotePeer()
			entry, ok := r.acl.GetEntry(peerID)
			if !ok {
				return
			}
			stat := r.bwc.GetBandwidthForPeer(peerID)
			if r.OnCircuitClosed != nil {
				r.OnCircuitClosed(entry.APIKeyID, entry.ShareCode, stat.TotalIn, stat.TotalOut)
			}
			r.acl.Remove(peerID)
		},
	})

	return r.h.Addrs()
}

// Close stops the circuit relay service, the Host, and the JTI prune goroutine.
func (r *Relay) Close() error {
	r.jtis.Close()
	r.circuitSvc.Close()
	return r.h.Close()
}

func loadOrGenerateKey(path string) (crypto.PrivKey, error) {
	if path == "" {
		priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
		return priv, err
	}
	b, err := os.ReadFile(path)
	if err == nil {
		return crypto.UnmarshalPrivateKey(b)
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		return nil, err
	}
	marshaled, err := crypto.MarshalPrivateKey(priv)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, marshaled, 0600); err != nil {
		return nil, fmt.Errorf("write key: %w", err)
	}
	return priv, nil
}

