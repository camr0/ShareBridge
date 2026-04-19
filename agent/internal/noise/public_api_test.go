package noise_test

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"testing"

	noise "sharebridge/agent/internal/noise"
)

func TestSplitCipherStatesExposePublicEncryptDecrypt(t *testing.T) {
	responderStatic, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate responder static: %v", err)
	}

	initiator, err := noise.NewInitiator()
	if err != nil {
		t.Fatalf("NewInitiator: %v", err)
	}
	responder, err := noise.NewResponder(responderStatic)
	if err != nil {
		t.Fatalf("NewResponder: %v", err)
	}

	msg1, err := initiator.WriteMessage1()
	if err != nil {
		t.Fatalf("WriteMessage1: %v", err)
	}
	if err := responder.ReadMessage1(msg1); err != nil {
		t.Fatalf("ReadMessage1: %v", err)
	}

	msg2, err := responder.WriteMessage2()
	if err != nil {
		t.Fatalf("WriteMessage2: %v", err)
	}
	if err := initiator.ReadMessage2(msg2); err != nil {
		t.Fatalf("ReadMessage2: %v", err)
	}

	msg3, err := initiator.WriteMessage3()
	if err != nil {
		t.Fatalf("WriteMessage3: %v", err)
	}
	if err := responder.ReadMessage3(msg3); err != nil {
		t.Fatalf("ReadMessage3: %v", err)
	}

	iSend, iRecv := initiator.Split()
	rSend, rRecv := responder.Split()

	plaintext := []byte("hello from exported api")
	ct, err := iSend.Encrypt(nil, plaintext)
	if err != nil {
		t.Fatalf("iSend.Encrypt: %v", err)
	}
	pt, err := rRecv.Decrypt(nil, ct)
	if err != nil {
		t.Fatalf("rRecv.Decrypt: %v", err)
	}
	if !bytes.Equal(pt, plaintext) {
		t.Fatalf("plaintext mismatch: got %q, want %q", pt, plaintext)
	}

	reply := []byte("reply via exported api")
	ct2, err := rSend.Encrypt(nil, reply)
	if err != nil {
		t.Fatalf("rSend.Encrypt: %v", err)
	}
	pt2, err := iRecv.Decrypt(nil, ct2)
	if err != nil {
		t.Fatalf("iRecv.Decrypt: %v", err)
	}
	if !bytes.Equal(pt2, reply) {
		t.Fatalf("reply mismatch: got %q, want %q", pt2, reply)
	}
}
