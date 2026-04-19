// agent/internal/noise/cipher.go
package noise

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
)

// CipherState holds a symmetric key and monotonic nonce counter (Noise spec §4.1).
type CipherState struct {
	key [32]byte
	n   uint64
}

func newCipherState(key [32]byte) *CipherState {
	return &CipherState{key: key}
}

// Encrypt encrypts plaintext with explicit associated data.
func (cs *CipherState) Encrypt(ad, plaintext []byte) ([]byte, error) {
	return cs.encryptWithAd(ad, plaintext)
}

// Decrypt decrypts ciphertext with explicit associated data.
func (cs *CipherState) Decrypt(ad, ciphertext []byte) ([]byte, error) {
	return cs.decryptWithAd(ad, ciphertext)
}

// encryptWithAd encrypts plaintext with AES-256-GCM using current nonce, then increments n.
func (cs *CipherState) encryptWithAd(ad, plaintext []byte) ([]byte, error) {
	if isZeroKey(cs.key) {
		return nil, errors.New("noise: cipher key not initialized")
	}
	if cs.n == ^uint64(0) {
		return nil, errors.New("noise: nonce exhausted")
	}
	gcm, err := newGCM(cs.key)
	if err != nil {
		return nil, err
	}
	ct := gcm.Seal(nil, nonceBytes(cs.n), plaintext, ad)
	cs.n++
	return ct, nil
}

// decryptWithAd decrypts ciphertext; increments n only on success.
func (cs *CipherState) decryptWithAd(ad, ciphertext []byte) ([]byte, error) {
	if isZeroKey(cs.key) {
		return nil, errors.New("noise: cipher key not initialized")
	}
	if cs.n == ^uint64(0) {
		return nil, errors.New("noise: nonce exhausted")
	}
	gcm, err := newGCM(cs.key)
	if err != nil {
		return nil, err
	}
	pt, err := gcm.Open(nil, nonceBytes(cs.n), ciphertext, ad)
	if err != nil {
		return nil, errors.New("noise: AEAD authentication failed")
	}
	cs.n++
	return pt, nil
}

// splitKeys derives two post-handshake cipher keys from the final chaining key.
// Uses Noise Split (§5.2): HKDF(ck, zerolen, 2) where zerolen = b"".
func splitKeys(ck [32]byte) (k1, k2 [32]byte) {
	return hkdf2(ck, []byte{}) // empty input, NOT zeros(32)
}

func newGCM(key [32]byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("noise: aes.NewCipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("noise: cipher.NewGCM: %w", err)
	}
	return gcm, nil
}

// nonceBytes constructs the 12-byte AES-GCM nonce: 4 zero bytes || n as uint64 big-endian.
func nonceBytes(n uint64) []byte {
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint64(nonce[4:], n)
	return nonce
}

func isZeroKey(key [32]byte) bool {
	var zero [32]byte
	return key == zero
}
