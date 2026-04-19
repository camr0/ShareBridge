// agent/internal/noise/transcript.go
package noise

import (
	"crypto/hmac"
	"crypto/sha256"
)

const protocolName = "Noise_XX_P256_AESGCM_SHA256"

// initialize sets h and ck to the zero-padded protocol name (Noise spec §5.2).
// "Noise_XX_P256_AESGCM_SHA256" is 27 bytes < HASHLEN(32), so pad with zeros.
func initialize() (h, ck [32]byte) {
	copy(h[:], protocolName) // Go zero-initializes arrays; copy leaves tail as 0x00
	ck = h
	return
}

// mixHash appends data to h and rehashes (Noise spec §5.2 MixHash).
func mixHash(h [32]byte, data []byte) [32]byte {
	digest := sha256.New()
	digest.Write(h[:])
	digest.Write(data)
	var result [32]byte
	copy(result[:], digest.Sum(nil))
	return result
}

// hkdf2 is Noise's HKDF returning two 32-byte outputs (Noise spec §4.2).
// temp_key = HMAC-SHA256(ck, ikm)
// out1     = HMAC-SHA256(temp_key, 0x01)
// out2     = HMAC-SHA256(temp_key, out1 || 0x02)
func hkdf2(ck [32]byte, ikm []byte) (out1, out2 [32]byte) {
	tempKey := hmacSHA256(ck[:], ikm)
	o1 := hmacSHA256(tempKey, []byte{0x01})
	o2 := hmacSHA256(tempKey, append(o1, 0x02))
	copy(out1[:], o1)
	copy(out2[:], o2)
	return
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}