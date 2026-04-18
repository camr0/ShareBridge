package relay_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	circuitv2client "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	"github.com/multiformats/go-multiaddr"

	"sharebridge/server/internal/relay"
)

const fileProto = "/sharebridge/file/1.0.0"

func writeTextFrame(t *testing.T, s network.Stream, payload string) {
	t.Helper()
	if _, err := s.Write([]byte{0x01}); err != nil {
		t.Fatalf("write kind: %v", err)
	}
	if err := binary.Write(s, binary.BigEndian, uint32(len(payload))); err != nil {
		t.Fatalf("write length: %v", err)
	}
	if _, err := s.Write([]byte(payload)); err != nil {
		t.Fatalf("write payload: %v", err)
	}
}

func readTextFrame(t *testing.T, s network.Stream) map[string]any {
	t.Helper()
	kindBuf := make([]byte, 1)
	if _, err := io.ReadFull(s, kindBuf); err != nil {
		t.Fatalf("read kind: %v", err)
	}
	if kindBuf[0] != 0x01 {
		t.Fatalf("expected text frame kind 0x01, got 0x%02x", kindBuf[0])
	}
	var length uint32
	if err := binary.Read(s, binary.BigEndian, &length); err != nil {
		t.Fatalf("read length: %v", err)
	}
	if length > 8*1024*1024 {
		t.Fatalf("frame too large: %d bytes", length)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(s, buf); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(buf, &resp); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return resp
}

func TestIntegration_e2eCircuitRelay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// ── Relay ────────────────────────────────────────────────────────────────
	rly, err := relay.New(ctx, relay.Config{
		ListenAddr: "/ip4/127.0.0.1/tcp/0",
		JWTSecret:  []byte("test-secret-do-not-use-in-prod-abcd1234"),
		JWTTTL:     time.Minute,
	})
	if err != nil {
		t.Fatalf("relay.New: %v", err)
	}
	defer rly.Close()

	rly.SetCodeResolver(func(code string) (string, bool) {
		if code == "test-share" {
			return "api-key-A", true
		}
		return "", false
	})

	var closedMu sync.Mutex
	var closedBytesIn, closedBytesOut int64
	rly.SetCircuitClosedHook(func(apiKeyID, shareCode string, bytesIn, bytesOut int64) {
		closedMu.Lock()
		closedBytesIn += bytesIn
		closedBytesOut += bytesOut
		closedMu.Unlock()
	})

	relayAddrs := rly.Start()
	if len(relayAddrs) == 0 {
		t.Fatal("no relay addrs")
	}
	relayAddr := relayAddrs[0]

	// ── Agent ─────────────────────────────────────────────────────────────────
	agentHost, err := libp2p.New(
		libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"),
		libp2p.EnableRelay(),
	)
	if err != nil {
		t.Fatalf("agent libp2p.New: %v", err)
	}
	defer agentHost.Close()

	// Register echo handler for file protocol
	agentHost.SetStreamHandler(fileProto, func(s network.Stream) {
		defer s.Close()
		io.Copy(s, s)
	})

	// Agent connects to relay and reserves circuit
	agentHost.Peerstore().AddAddrs(rly.Host().ID(), rly.Host().Addrs(), time.Minute)
	if err := agentHost.Connect(ctx, peer.AddrInfo{ID: rly.Host().ID(), Addrs: rly.Host().Addrs()}); err != nil {
		t.Fatalf("agent connect to relay: %v", err)
	}
	_, err = circuitv2client.Reserve(ctx, agentHost, peer.AddrInfo{ID: rly.Host().ID(), Addrs: rly.Host().Addrs()})
	if err != nil {
		t.Fatalf("agent reserve: %v", err)
	}

	// Register agent in relay registry
	rly.Agents().Register("api-key-A", agentHost.ID())

	// ── Browser ───────────────────────────────────────────────────────────────
	browserHost, err := libp2p.New(
		libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"),
		libp2p.EnableRelay(),
	)
	if err != nil {
		t.Fatalf("browser libp2p.New: %v", err)
	}
	defer browserHost.Close()
	browserHost.Peerstore().AddAddrs(rly.Host().ID(), rly.Host().Addrs(), time.Minute)

	// Browser connects to relay first (required for circuit dial)
	if err := browserHost.Connect(ctx, peer.AddrInfo{ID: rly.Host().ID(), Addrs: rly.Host().Addrs()}); err != nil {
		t.Fatalf("browser connect to relay: %v", err)
	}

	// Step 1: Browser presents JWT on auth stream
	tok, err := rly.Issuer().Issue(relay.Claims{
		ShareCode:     "test-share",
		BrowserPeerID: browserHost.ID().String(),
		RelayAllowed:  true,
		DCUtRAllowed:  false,
	})
	if err != nil {
		t.Fatalf("issue JWT: %v", err)
	}

	authStream, err := browserHost.NewStream(ctx, rly.Host().ID(), relay.ProtocolID)
	if err != nil {
		t.Fatalf("auth NewStream: %v", err)
	}

	envelope, _ := json.Marshal(map[string]string{"type": "jwt", "token": tok})
	writeTextFrame(t, authStream, string(envelope))
	authStream.CloseWrite()

	resp := readTextFrame(t, authStream)
	if resp["type"] != "auth_ok" {
		t.Fatalf("auth failed: %v", resp)
	}
	authStream.Close()

	// Step 2: Browser dials agent via circuit relay
	p2pRelayComp, _ := multiaddr.NewMultiaddr("/p2p/" + rly.Host().ID().String())
	circuitComp, _ := multiaddr.NewMultiaddr("/p2p-circuit/p2p/" + agentHost.ID().String())
	circuitAddr := relayAddr.Encapsulate(p2pRelayComp).Encapsulate(circuitComp)

	if err := browserHost.Connect(ctx, peer.AddrInfo{
		ID:    agentHost.ID(),
		Addrs: []multiaddr.Multiaddr{circuitAddr},
	}); err != nil {
		t.Fatalf("browser connect via circuit relay: %v", err)
	}

	// Step 3: Open file protocol stream (E2E Noise encrypted)
	// Circuit relay connections are "limited" (transient), so we need to use WithAllowLimitedConn
	fileStream, err := browserHost.NewStream(network.WithAllowLimitedConn(ctx, fileProto), agentHost.ID(), fileProto)
	if err != nil {
		t.Fatalf("file NewStream: %v", err)
	}

	payload := make([]byte, 256*1024)
	for i := range payload {
		payload[i] = byte(i % 256)
	}
	if _, err := fileStream.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	fileStream.CloseWrite()

	echoed, err := io.ReadAll(fileStream)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if len(echoed) != len(payload) {
		t.Fatalf("echo length: got %d want %d", len(echoed), len(payload))
	}
	for i, b := range echoed {
		if b != payload[i] {
			t.Fatalf("echo mismatch at byte %d", i)
		}
	}

	// Step 4: Close and verify byte accounting
	fileStream.Close()
	browserHost.Network().ClosePeer(rly.Host().ID())
	time.Sleep(200 * time.Millisecond)

	closedMu.Lock()
	totalReported := closedBytesIn + closedBytesOut
	closedMu.Unlock()

	if totalReported == 0 {
		t.Error("OnCircuitClosed never fired -- byte accounting is broken")
	}
	t.Logf("bytes reported to quota accumulator: in=%d out=%d", closedBytesIn, closedBytesOut)
}
