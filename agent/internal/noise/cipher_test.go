// agent/internal/noise/cipher_test.go
package noise

import (
	"bytes"
	"testing"
)

func TestCipherStateRoundTrip(t *testing.T) {
	var key [32]byte
	copy(key[:], bytes.Repeat([]byte{0x42}, 32))

	cs := newCipherState(key)
	ad := []byte("associated-data")
	plaintext := []byte("hello sharebridge")

	ct, err := cs.encryptWithAd(ad, plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if len(ct) != len(plaintext)+16 {
		t.Fatalf("ciphertext length: got %d, want %d", len(ct), len(plaintext)+16)
	}

	cs2 := newCipherState(key)
	pt, err := cs2.decryptWithAd(ad, ct)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(pt, plaintext) {
		t.Fatalf("plaintext mismatch: got %q, want %q", pt, plaintext)
	}
}

func TestCipherStateNonceIncrements(t *testing.T) {
	var key [32]byte
	copy(key[:], bytes.Repeat([]byte{0x11}, 32))
	cs := newCipherState(key)

	ct1, _ := cs.encryptWithAd(nil, []byte("msg1"))
	ct2, _ := cs.encryptWithAd(nil, []byte("msg1")) // same plaintext, different nonce
	if bytes.Equal(ct1, ct2) {
		t.Fatal("same plaintext with different nonces must produce different ciphertext")
	}
}

func TestCipherStateWrongAdFails(t *testing.T) {
	var key [32]byte
	copy(key[:], bytes.Repeat([]byte{0x33}, 32))
	cs := newCipherState(key)

	ct, _ := cs.encryptWithAd([]byte("correct-ad"), []byte("data"))

	cs2 := newCipherState(key)
	_, err := cs2.decryptWithAd([]byte("wrong-ad"), ct)
	if err == nil {
		t.Fatal("decrypt with wrong AD must fail")
	}
}

func TestCipherStateNonceExhaustion(t *testing.T) {
	var key [32]byte
	copy(key[:], bytes.Repeat([]byte{0x55}, 32))
	cs := newCipherState(key)
	cs.n = ^uint64(0)

	if _, err := cs.encryptWithAd(nil, []byte("x")); err == nil {
		t.Fatal("encrypt with exhausted nonce must fail")
	}
}

func TestSplit(t *testing.T) {
	_, ck := initialize()
	k1, k2 := splitKeys(ck)
	if k1 == k2 {
		t.Fatal("split keys must differ")
	}
	// Deterministic
	k1b, k2b := splitKeys(ck)
	if k1 != k1b || k2 != k2b {
		t.Fatal("split must be deterministic")
	}
}