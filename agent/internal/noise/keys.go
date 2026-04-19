// agent/internal/noise/keys.go
package noise

import (
	"crypto/ecdh"
	"crypto/rand"
)

// generateKeypair generates a fresh P-256 key pair.
func generateKeypair() (*ecdh.PrivateKey, error) {
	return ecdh.P256().GenerateKey(rand.Reader)
}

// dhP256 performs P-256 ECDH and returns the 32-byte shared secret (x-coordinate).
func dhP256(priv *ecdh.PrivateKey, pub *ecdh.PublicKey) ([]byte, error) {
	return priv.ECDH(pub)
}

// importPublicKey parses a 65-byte uncompressed P-256 public key.
func importPublicKey(raw []byte) (*ecdh.PublicKey, error) {
	return ecdh.P256().NewPublicKey(raw)
}