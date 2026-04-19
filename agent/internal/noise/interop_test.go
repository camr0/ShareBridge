// agent/internal/noise/interop_test.go
package noise

import (
	"crypto/ecdh"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// InteropVector captures the full handshake transcript for JS verification.
type InteropVector struct {
	InitiatorEphemeralPriv string `json:"initiator_ephemeral_priv"` // hex
	ResponderEphemeralPriv string `json:"responder_ephemeral_priv"`
	InitiatorStaticPriv    string `json:"initiator_static_priv"`
	ResponderStaticPriv    string `json:"responder_static_priv"`
	Msg1                   string `json:"msg1"`
	Msg2                   string `json:"msg2"`
	Msg3                   string `json:"msg3"`
	K1                     string `json:"k1"` // initiator→responder post-handshake key
	K2                     string `json:"k2"` // responder→initiator post-handshake key
	// A sample encrypted message from each direction
	SamplePlaintext   string `json:"sample_plaintext"`
	SampleCtInitiator string `json:"sample_ct_initiator"` // iSend.Encrypt(sample)
	SampleCtResponder string `json:"sample_ct_responder"` // rSend.Encrypt(sample)
}

// privateKeyFromHex imports a P-256 private key from a 32-byte hex-encoded scalar.
func privateKeyFromHex(t *testing.T, hexStr string) *ecdh.PrivateKey {
	t.Helper()
	raw, err := hex.DecodeString(hexStr)
	if err != nil {
		t.Fatalf("hex decode: %v", err)
	}
	priv, err := ecdh.P256().NewPrivateKey(raw)
	if err != nil {
		t.Fatalf("import private key: %v", err)
	}
	return priv
}

// TestGenerateInteropVectors generates testdata/interop_vector.json.
// Run with: go test -run TestGenerateInteropVectors ./internal/noise/
func TestGenerateInteropVectors(t *testing.T) {
	// Fixed P-256 private key scalars (32 bytes each, hex-encoded).
	// These are arbitrary values in [1, curve_order); verified to be valid P-256 scalars.
	const (
		iEPrivHex = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
		rEPrivHex = "2122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f40"
		iSPrivHex = "4142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f60"
		rSPrivHex = "6162636465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f00"
	)

	iEPriv := privateKeyFromHex(t, iEPrivHex)
	rEPriv := privateKeyFromHex(t, rEPrivHex)
	iSPriv := privateKeyFromHex(t, iSPrivHex)
	rSPriv := privateKeyFromHex(t, rSPrivHex)

	// Build initiator and responder with injected keys
	h, ck := initialize()
	initiator := &NoiseXX{role: roleInitiator, h: h, ck: ck, sPriv: iSPriv, sPub: iSPriv.PublicKey().Bytes()}
	h2, ck2 := initialize()
	responder := &NoiseXX{role: roleResponder, h: h2, ck: ck2, sPriv: rSPriv, sPub: rSPriv.PublicKey().Bytes()}

	// Inject deterministic ephemeral keys (override generateKeypair in WriteMessage1/2)
	initiator.ePriv = iEPriv
	initiator.ePub = iEPriv.PublicKey().Bytes()
	initiator.h = mixHash(initiator.h, initiator.ePub) // simulate WriteMessage1 token processing
	initiator.phase = phaseMsg1Sent

	msg1 := initiator.ePub // 65 bytes

	if err := responder.ReadMessage1(msg1); err != nil {
		t.Fatalf("ReadMessage1: %v", err)
	}

	// Inject responder ephemeral
	responder.ePriv = rEPriv
	responder.ePub = rEPriv.PublicKey().Bytes()

	// Re-run WriteMessage2 logic with injected ephemeral
	// (call the method but it will re-generate ephemeral — we need to intercept)
	// Instead, reconstruct manually using the same steps as WriteMessage2
	responder.h = mixHash(responder.h, responder.ePub)
	msg2buf := make([]byte, 0, 162)
	msg2buf = append(msg2buf, responder.ePub...)

	rEPubKey, _ := importPublicKey(responder.rEPub)
	ee, _ := dhP256(responder.ePriv, rEPubKey)
	responder.mixKey(ee)
	encS, _ := responder.encryptAndHash(responder.sPub)
	msg2buf = append(msg2buf, encS...)
	rEPubKey2, _ := importPublicKey(responder.rEPub)
	es, _ := dhP256(responder.sPriv, rEPubKey2)
	responder.mixKey(es)
	tag, _ := responder.encryptAndHash([]byte{})
	msg2buf = append(msg2buf, tag...)
	msg2 := msg2buf
	responder.phase = phaseMsg2Sent // CRITICAL: must set phase before ReadMessage3 will work

	if err := initiator.ReadMessage2(msg2); err != nil {
		t.Fatalf("ReadMessage2: %v", err)
	}

	// Inject initiator static for WriteMessage3
	initiator.sPriv = iSPriv
	initiator.sPub = iSPriv.PublicKey().Bytes()
	msg3, err := initiator.WriteMessage3()
	if err != nil {
		t.Fatalf("WriteMessage3: %v", err)
	}
	if err := responder.ReadMessage3(msg3); err != nil {
		t.Fatalf("ReadMessage3: %v", err)
	}

	iSend, iRecv := initiator.Split()
	rSend, rRecv := responder.Split()

	const samplePlaintext = "sharebridge-interop-test"
	sampleCTi, _ := iSend.encryptWithAd(nil, []byte(samplePlaintext))
	sampleCTr, _ := rSend.encryptWithAd(nil, []byte(samplePlaintext))

	// Verify cross-decryption before writing the file
	pt1, err := rRecv.decryptWithAd(nil, sampleCTi)
	if err != nil || string(pt1) != samplePlaintext {
		t.Fatalf("cross-decrypt i->r failed: %v", err)
	}
	pt2, err := iRecv.decryptWithAd(nil, sampleCTr)
	if err != nil || string(pt2) != samplePlaintext {
		t.Fatalf("cross-decrypt r->i failed: %v", err)
	}

	vec := InteropVector{
		InitiatorEphemeralPriv: iEPrivHex,
		ResponderEphemeralPriv: rEPrivHex,
		InitiatorStaticPriv:    iSPrivHex,
		ResponderStaticPriv:    rSPrivHex,
		Msg1:                   hex.EncodeToString(msg1),
		Msg2:                   hex.EncodeToString(msg2),
		Msg3:                   hex.EncodeToString(msg3),
		K1:                     hex.EncodeToString(iSend.key[:]),
		K2:                     hex.EncodeToString(rSend.key[:]),
		SamplePlaintext:        samplePlaintext,
		SampleCtInitiator:      hex.EncodeToString(sampleCTi),
		SampleCtResponder:      hex.EncodeToString(sampleCTr),
	}

	if err := os.MkdirAll("testdata", 0755); err != nil {
		t.Fatalf("mkdir testdata: %v", err)
	}
	out, _ := json.MarshalIndent(vec, "", "  ")
	if err := os.WriteFile(filepath.Join("testdata", "interop_vector.json"), out, 0644); err != nil {
		t.Fatalf("write vector: %v", err)
	}
	t.Logf("interop_vector.json written (%d bytes)", len(out))
}