package relay

import (
	"context"
	"testing"
	"time"
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