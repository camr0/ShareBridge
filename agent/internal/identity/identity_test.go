package identity

import (
	"path/filepath"
	"testing"

	"github.com/libp2p/go-libp2p/core/crypto"
)

func TestLoadOrCreate_GeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent_identity.key")

	priv1, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("first LoadOrCreate: %v", err)
	}
	if priv1 == nil {
		t.Fatal("expected non-nil private key")
	}

	priv2, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("second LoadOrCreate: %v", err)
	}

	a, _ := crypto.MarshalPrivateKey(priv1)
	b, _ := crypto.MarshalPrivateKey(priv2)
	if string(a) != string(b) {
		t.Fatal("key changed across reloads — must be stable")
	}
}