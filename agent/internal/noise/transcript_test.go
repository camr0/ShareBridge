// agent/internal/noise/transcript_test.go
package noise

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestInitialize(t *testing.T) {
	h, ck := initialize()

	// "Noise_XX_P256_AESGCM_SHA256" is 27 bytes; padded to 32 with 5 zero bytes
	name := []byte("Noise_XX_P256_AESGCM_SHA256")
	var want [32]byte
	copy(want[:], name)

	if h != want {
		t.Errorf("h mismatch:\n got  %s\n want %s", hex.EncodeToString(h[:]), hex.EncodeToString(want[:]))
	}
	if ck != want {
		t.Errorf("ck mismatch:\n got  %s\n want %s", hex.EncodeToString(ck[:]), hex.EncodeToString(want[:]))
	}
}

func TestMixHash(t *testing.T) {
	h, _ := initialize()
	// MixHash with empty data should change h
	h2 := mixHash(h, []byte{})
	if h2 == h {
		t.Fatal("mixHash with empty data must change h")
	}
	// MixHash is deterministic
	h3 := mixHash(h, []byte{})
	if h2 != h3 {
		t.Fatal("mixHash must be deterministic")
	}
	// Appending different data produces different hashes
	h4 := mixHash(h, []byte{0x01})
	if h2 == h4 {
		t.Fatal("mixHash with different data must produce different h")
	}
}

func TestHkdf2(t *testing.T) {
	h, ck := initialize()
	// Use a dummy ikm
	ikm := bytes.Repeat([]byte{0xAB}, 32)
	newCK, newK := hkdf2(ck, ikm)
	// Both outputs are 32 bytes (enforced by [32]byte type)
	// Outputs must differ from ck and from each other
	if newCK == ck {
		t.Fatal("hkdf2 output1 must differ from input ck")
	}
	if newK == h {
		t.Fatal("hkdf2 output2 must differ from h")
	}
	if newCK == newK {
		t.Fatal("hkdf2 output1 and output2 must differ")
	}
	// Deterministic
	newCK2, newK2 := hkdf2(ck, ikm)
	if newCK != newCK2 || newK != newK2 {
		t.Fatal("hkdf2 must be deterministic")
	}
}