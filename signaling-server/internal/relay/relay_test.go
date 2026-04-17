package relay

import (
	"context"
	"strings"
	"testing"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
)

func TestNewHost_startsAndStops(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	r, err := New(ctx, Config{
		ListenAddr: "/ip4/127.0.0.1/tcp/0",
		JWTSecret:  []byte("test-secret-do-not-use-in-prod-abcd1234"),
		JWTTTL:     time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if r.Host() == nil {
		t.Fatal("Host() nil")
	}
	if r.Host().ID().String() == "" {
		t.Fatal("empty peer ID")
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestNewHost_persistsIdentity(t *testing.T) {
	tmp := t.TempDir()
	keyPath := tmp + "/relay.key"

	r1, err := New(context.Background(), Config{
		ListenAddr:     "/ip4/127.0.0.1/tcp/0",
		PrivateKeyPath: keyPath,
		JWTSecret:      []byte("test-secret-do-not-use-in-prod-abcd1234"),
		JWTTTL:         time.Minute,
	})
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	firstID := r1.Host().ID()
	r1.Close()

	r2, err := New(context.Background(), Config{
		ListenAddr:     "/ip4/127.0.0.1/tcp/0",
		PrivateKeyPath: keyPath,
		JWTSecret:      []byte("test-secret-do-not-use-in-prod-abcd1234"),
		JWTTTL:         time.Minute,
	})
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	defer r2.Close()

	if r2.Host().ID() != firstID {
		t.Fatalf("peer ID changed across restart: %s != %s", firstID, r2.Host().ID())
	}
}

func TestRelay_startRegistersAuthHandler(t *testing.T) {
	rly, err := New(context.Background(), Config{
		ListenAddr: "/ip4/127.0.0.1/tcp/0",
		JWTSecret:  []byte("test-secret-do-not-use-in-prod-abcd1234"),
		JWTTTL:     time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer rly.Close()

	rly.SetCodeResolver(func(string) (string, bool) { return "k", false })
	addrs := rly.Start()
	if len(addrs) == 0 {
		t.Fatal("Start() returned no listen addrs")
	}

	// Should be able to dial the auth protocol.
	dialer, _ := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	defer dialer.Close()
	dialer.Peerstore().AddAddrs(rly.Host().ID(), rly.Host().Addrs(), time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s, err := dialer.NewStream(ctx, rly.Host().ID(), ProtocolID)
	if err != nil {
		t.Fatalf("NewStream after Start: %v", err)
	}
	s.Close()
}

func TestRelay_advertiseAddrFallsBackToListenAddr(t *testing.T) {
	rly, err := New(context.Background(), Config{
		ListenAddr: "/ip4/127.0.0.1/tcp/0",
		JWTSecret:  []byte("test-secret-do-not-use-in-prod-abcd1234"),
		JWTTTL:     time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer rly.Close()

	rly.Start()

	got := rly.AdvertiseAddr()
	if got == "" {
		t.Fatal("AdvertiseAddr() returned empty string")
	}
	if !strings.HasPrefix(got, "/ip4") {
		t.Fatalf("AdvertiseAddr() should fall back to a dialable host addr, got %q", got)
	}
}
