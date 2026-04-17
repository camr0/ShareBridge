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