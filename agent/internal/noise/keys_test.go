// agent/internal/noise/keys_test.go
package noise

import (
	"bytes"
	"testing"
)

func TestGenerateKeypair(t *testing.T) {
	priv, err := generateKeypair()
	if err != nil {
		t.Fatalf("generateKeypair: %v", err)
	}
	pub := priv.PublicKey()
	pubBytes := pub.Bytes() // uncompressed P-256: 65 bytes
	if len(pubBytes) != 65 {
		t.Fatalf("public key length: got %d, want 65", len(pubBytes))
	}
	if pubBytes[0] != 0x04 {
		t.Fatalf("public key must start with 0x04 (uncompressed), got 0x%02x", pubBytes[0])
	}
}

func TestDH_SharedSecretMatches(t *testing.T) {
	alicePriv, _ := generateKeypair()
	bobPriv, _ := generateKeypair()

	// DH is commutative: alice(bob_pub) == bob(alice_pub)
	sharedAB, err := dhP256(alicePriv, bobPriv.PublicKey())
	if err != nil {
		t.Fatalf("dh alice->bob: %v", err)
	}
	sharedBA, err := dhP256(bobPriv, alicePriv.PublicKey())
	if err != nil {
		t.Fatalf("dh bob->alice: %v", err)
	}
	if !bytes.Equal(sharedAB, sharedBA) {
		t.Fatal("DH shared secrets must match")
	}
	if len(sharedAB) != 32 {
		t.Fatalf("shared secret length: got %d, want 32", len(sharedAB))
	}
}

func TestPublicKeyRoundTrip(t *testing.T) {
	priv, _ := generateKeypair()
	pubBytes := priv.PublicKey().Bytes()

	imported, err := importPublicKey(pubBytes)
	if err != nil {
		t.Fatalf("import public key: %v", err)
	}
	if !bytes.Equal(imported.Bytes(), pubBytes) {
		t.Fatal("public key round-trip mismatch")
	}
}