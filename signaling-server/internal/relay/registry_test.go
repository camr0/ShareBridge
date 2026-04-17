package relay

import (
	"testing"

	"github.com/libp2p/go-libp2p/core/peer"
)

func TestRegistry_registerAndLookup(t *testing.T) {
	reg := NewAgentRegistry()
	pid, err := peer.Decode("12D3KooWGRUacMd4aSXwsNyEYxh3nC1dT3rBjoJ5ycnqZRuiETxF")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	reg.Register("api-key-123", pid)

	got, ok := reg.Lookup("api-key-123")
	if !ok || got != pid {
		t.Fatalf("lookup: got %v ok=%v", got, ok)
	}
	keyID, ok := reg.Reverse(pid)
	if !ok || keyID != "api-key-123" {
		t.Fatalf("reverse: got %q ok=%v", keyID, ok)
	}
}

func TestRegistry_unregisterRemovesBothDirections(t *testing.T) {
	reg := NewAgentRegistry()
	pid, _ := peer.Decode("12D3KooWGRUacMd4aSXwsNyEYxh3nC1dT3rBjoJ5ycnqZRuiETxF")
	reg.Register("key-A", pid)
	reg.Unregister("key-A")
	if _, ok := reg.Lookup("key-A"); ok {
		t.Fatal("lookup still present after unregister")
	}
	if _, ok := reg.Reverse(pid); ok {
		t.Fatal("reverse still present after unregister")
	}
}

func TestRegistry_reRegisterCleansOldAssociations(t *testing.T) {
	reg := NewAgentRegistry()
	pid1, _ := peer.Decode("12D3KooWGRUacMd4aSXwsNyEYxh3nC1dT3rBjoJ5ycnqZRuiETxF")
	pid2, _ := peer.Decode("12D3KooWAnotherAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")

	// First registration
	reg.Register("key-A", pid1)

	// Re-register same key with different peer
	reg.Register("key-A", pid2)

	// Old peer should no longer map to key-A
	if key, ok := reg.Reverse(pid1); ok {
		t.Fatalf("old pid1 should be orphaned, but Reverse returned %q", key)
	}

	// New peer should map to key-A
	if key, ok := reg.Reverse(pid2); !ok || key != "key-A" {
		t.Fatalf("pid2 should map to key-A, got %q ok=%v", key, ok)
	}

	// key-A should map to new peer
	if p, ok := reg.Lookup("key-A"); !ok || p != pid2 {
		t.Fatalf("key-A should map to pid2, got %v ok=%v", p, ok)
	}
}