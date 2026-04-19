// agent/internal/noise/handshake_test.go
package noise

import (
	"bytes"
	"testing"
)

// TestFullHandshake runs a complete Noise_XX handshake between two in-process
// peers and verifies that post-handshake encryption works in both directions.
func TestFullHandshake(t *testing.T) {
	// Responder (agent) has a static key pair
	responderStatic, err := generateKeypair()
	if err != nil {
		t.Fatalf("generate responder static: %v", err)
	}

	initiator, err := NewInitiator()
	if err != nil {
		t.Fatalf("NewInitiator: %v", err)
	}
	responder, err := NewResponder(responderStatic)
	if err != nil {
		t.Fatalf("NewResponder: %v", err)
	}

	// Message 1: initiator -> responder (65 bytes)
	msg1, err := initiator.WriteMessage1()
	if err != nil {
		t.Fatalf("WriteMessage1: %v", err)
	}
	if len(msg1) != 65 {
		t.Fatalf("msg1 length: got %d, want 65", len(msg1))
	}
	if err := responder.ReadMessage1(msg1); err != nil {
		t.Fatalf("ReadMessage1: %v", err)
	}

	// Message 2: responder -> initiator (162 bytes)
	msg2, err := responder.WriteMessage2()
	if err != nil {
		t.Fatalf("WriteMessage2: %v", err)
	}
	if len(msg2) != 162 {
		t.Fatalf("msg2 length: got %d, want 162", len(msg2))
	}
	if err := initiator.ReadMessage2(msg2); err != nil {
		t.Fatalf("ReadMessage2: %v", err)
	}

	// Initiator must see responder's static public key after msg2
	if !bytes.Equal(initiator.RemoteStaticPub(), responderStatic.PublicKey().Bytes()) {
		t.Fatal("initiator RemoteStaticPub mismatch after ReadMessage2")
	}

	// Message 3: initiator -> responder (97 bytes)
	msg3, err := initiator.WriteMessage3()
	if err != nil {
		t.Fatalf("WriteMessage3: %v", err)
	}
	if len(msg3) != 97 {
		t.Fatalf("msg3 length: got %d, want 97", len(msg3))
	}
	if err := responder.ReadMessage3(msg3); err != nil {
		t.Fatalf("ReadMessage3: %v", err)
	}

	// Both sides split into cipher states
	iSend, iRecv := initiator.Split()
	rSend, rRecv := responder.Split()

	// Initiator sends to responder
	plaintext := []byte("hello from browser")
	ct, err := iSend.encryptWithAd(nil, plaintext)
	if err != nil {
		t.Fatalf("iSend.encrypt: %v", err)
	}
	pt, err := rRecv.decryptWithAd(nil, ct)
	if err != nil {
		t.Fatalf("rRecv.decrypt: %v", err)
	}
	if !bytes.Equal(pt, plaintext) {
		t.Fatalf("i->r plaintext mismatch: got %q, want %q", pt, plaintext)
	}

	// Responder sends to initiator
	reply := []byte("hello from agent")
	ct2, err := rSend.encryptWithAd(nil, reply)
	if err != nil {
		t.Fatalf("rSend.encrypt: %v", err)
	}
	pt2, err := iRecv.decryptWithAd(nil, ct2)
	if err != nil {
		t.Fatalf("iRecv.decrypt: %v", err)
	}
	if !bytes.Equal(pt2, reply) {
		t.Fatalf("r->i plaintext mismatch: got %q, want %q", pt2, reply)
	}
}

func TestHandshakeRejectsOutOfOrderCalls(t *testing.T) {
	initiator, err := NewInitiator()
	if err != nil {
		t.Fatalf("NewInitiator: %v", err)
	}
	if _, err := initiator.WriteMessage3(); err == nil {
		t.Fatal("WriteMessage3 before ReadMessage2 must fail")
	}

	responderStatic, err := generateKeypair()
	if err != nil {
		t.Fatalf("generate responder static: %v", err)
	}
	responder, err := NewResponder(responderStatic)
	if err != nil {
		t.Fatalf("NewResponder: %v", err)
	}
	if _, err := responder.WriteMessage2(); err == nil {
		t.Fatal("WriteMessage2 before ReadMessage1 must fail")
	}
}