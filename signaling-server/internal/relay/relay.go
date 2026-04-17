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
	h      host.Host
	bwc    *metrics.BandwidthCounter
	issuer *Issuer
	jtis   *JTIStore
	agents *AgentRegistry

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

	return &Relay{
		h:      h,
		bwc:    bwc,
		issuer: NewIssuer(cfg.JWTSecret, cfg.JWTTTL),
		jtis:   NewJTIStore(cfg.JWTTTL),
		agents: NewAgentRegistry(),
	}, nil
}

// Host returns the underlying libp2p Host.
func (r *Relay) Host() host.Host { return r.h }

// Issuer returns the JWT issuer (used by agent_ws to mint tokens).
func (r *Relay) Issuer() *Issuer { return r.issuer }

// Agents returns the agent registry (populated in 13b when agents connect).
func (r *Relay) Agents() *AgentRegistry { return r.agents }

// Close stops the Host and releases resources.
func (r *Relay) Close() error {
	r.jtis.Close()
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

