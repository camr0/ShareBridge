// agent/internal/noise/handshake.go
package noise

import (
	"crypto/ecdh"
	"errors"
	"fmt"
)

type role int

const (
	roleInitiator role = iota
	roleResponder
)

type phase int

const (
	phaseReady phase = iota
	phaseMsg1Sent
	phaseMsg1Received
	phaseMsg2Sent
	phaseMsg2Received
	phaseDone
)

// NoiseXX implements the Noise_XX handshake state machine.
type NoiseXX struct {
	role  role
	phase phase
	h     [32]byte
	ck    [32]byte
	k     [32]byte // zero until first MixKey
	hasK  bool

	// Ephemeral keys (generated during handshake)
	ePriv *ecdh.PrivateKey
	ePub  []byte // 65 bytes

	// Static keys
	sPriv *ecdh.PrivateKey
	sPub  []byte // 65 bytes

	// Received remote keys
	rEPub []byte // remote ephemeral pub (65 bytes)
	rSPub []byte // remote static pub (65 bytes)
}

// NewInitiator creates a NoiseXX handshake as the initiator (browser).
// Generates a fresh per-session static key pair.
func NewInitiator() (*NoiseXX, error) {
	staticPriv, err := generateKeypair()
	if err != nil {
		return nil, fmt.Errorf("noise: generate initiator static: %w", err)
	}
	h, ck := initialize()
	h = mixHash(h, nil) // MixHash(prologue) where prologue = b""
	return &NoiseXX{
		role:  roleInitiator,
		phase: phaseReady,
		h:     h,
		ck:    ck,
		sPriv: staticPriv,
		sPub:  staticPriv.PublicKey().Bytes(),
	}, nil
}

// NewResponder creates a NoiseXX handshake as the responder (agent).
// Uses the agent's long-lived static key pair.
func NewResponder(staticPriv *ecdh.PrivateKey) (*NoiseXX, error) {
	h, ck := initialize()
	h = mixHash(h, nil) // MixHash(prologue) where prologue = b""
	return &NoiseXX{
		role:  roleResponder,
		phase: phaseReady,
		h:     h,
		ck:    ck,
		sPriv: staticPriv,
		sPub:  staticPriv.PublicKey().Bytes(),
	}, nil
}

// RemoteStaticPub returns the remote party's static public key (65 bytes).
// Only valid after ReadMessage2 (initiator) or ReadMessage3 (responder).
func (n *NoiseXX) RemoteStaticPub() []byte {
	return n.rSPub
}

// WriteMessage1 -- initiator sends: e (65 bytes)
func (n *NoiseXX) WriteMessage1() ([]byte, error) {
	if n.role != roleInitiator {
		return nil, errors.New("noise: WriteMessage1 called on responder")
	}
	if n.phase != phaseReady {
		return nil, fmt.Errorf("noise: WriteMessage1 invalid in phase %d", n.phase)
	}
	var err error
	n.ePriv, err = generateKeypair()
	if err != nil {
		return nil, fmt.Errorf("noise: generate ephemeral: %w", err)
	}
	n.ePub = n.ePriv.PublicKey().Bytes()
	n.h = mixHash(n.h, n.ePub) // token: e
	n.phase = phaseMsg1Sent
	return n.ePub, nil
}

// ReadMessage1 -- responder receives: e
func (n *NoiseXX) ReadMessage1(msg []byte) error {
	if n.role != roleResponder {
		return errors.New("noise: ReadMessage1 called on initiator")
	}
	if n.phase != phaseReady {
		return fmt.Errorf("noise: ReadMessage1 invalid in phase %d", n.phase)
	}
	if len(msg) != 65 {
		return fmt.Errorf("noise: ReadMessage1: expected 65 bytes, got %d", len(msg))
	}
	n.rEPub = msg
	n.h = mixHash(n.h, n.rEPub) // token: e
	n.phase = phaseMsg1Received
	return nil
}

// WriteMessage2 -- responder sends: e, ee, s, es (162 bytes)
func (n *NoiseXX) WriteMessage2() ([]byte, error) {
	if n.role != roleResponder {
		return nil, errors.New("noise: WriteMessage2 called on initiator")
	}
	if n.phase != phaseMsg1Received {
		return nil, fmt.Errorf("noise: WriteMessage2 invalid in phase %d", n.phase)
	}
	var err error
	n.ePriv, err = generateKeypair()
	if err != nil {
		return nil, fmt.Errorf("noise: generate ephemeral: %w", err)
	}
	n.ePub = n.ePriv.PublicKey().Bytes()

	// token: e
	n.h = mixHash(n.h, n.ePub)
	buf := make([]byte, 0, 162)
	buf = append(buf, n.ePub...) // 65 bytes

	// token: ee -- DH(re, ie)
	rEPubKey, err := importPublicKey(n.rEPub)
	if err != nil {
		return nil, fmt.Errorf("noise: import remote ephemeral: %w", err)
	}
	ee, err := dhP256(n.ePriv, rEPubKey)
	if err != nil {
		return nil, fmt.Errorf("noise: DH ee: %w", err)
	}
	n.mixKey(ee)

	// token: s -- EncryptAndHash(rs_pub)
	encS, err := n.encryptAndHash(n.sPub)
	if err != nil {
		return nil, fmt.Errorf("noise: EncryptAndHash s: %w", err)
	}
	buf = append(buf, encS...) // 81 bytes

	// token: es -- DH(rs, ie)
	rEPubKey2, err := importPublicKey(n.rEPub)
	if err != nil {
		return nil, err
	}
	es, err := dhP256(n.sPriv, rEPubKey2)
	if err != nil {
		return nil, fmt.Errorf("noise: DH es: %w", err)
	}
	n.mixKey(es)

	// empty payload
	tag, err := n.encryptAndHash([]byte{})
	if err != nil {
		return nil, err
	}
	buf = append(buf, tag...) // 16 bytes
	n.phase = phaseMsg2Sent
	return buf, nil
}

// ReadMessage2 -- initiator receives: e, ee, s, es (162 bytes)
func (n *NoiseXX) ReadMessage2(msg []byte) error {
	if n.role != roleInitiator {
		return errors.New("noise: ReadMessage2 called on responder")
	}
	if n.phase != phaseMsg1Sent {
		return fmt.Errorf("noise: ReadMessage2 invalid in phase %d", n.phase)
	}
	if len(msg) != 162 {
		return fmt.Errorf("noise: ReadMessage2: expected 162 bytes, got %d", len(msg))
	}

	// token: e
	n.rEPub = msg[:65]
	n.h = mixHash(n.h, n.rEPub)

	// token: ee -- DH(ie, re)
	rEPubKey, err := importPublicKey(n.rEPub)
	if err != nil {
		return fmt.Errorf("noise: import remote ephemeral: %w", err)
	}
	ee, err := dhP256(n.ePriv, rEPubKey)
	if err != nil {
		return fmt.Errorf("noise: DH ee: %w", err)
	}
	n.mixKey(ee)

	// token: s -- DecryptAndHash(encrypted_rs_pub)
	rsPub, err := n.decryptAndHash(msg[65:146])
	if err != nil {
		return fmt.Errorf("noise: DecryptAndHash s: %w", err)
	}
	n.rSPub = rsPub

	// token: es -- DH(ie, rs)
	rSPubKey, err := importPublicKey(rsPub)
	if err != nil {
		return fmt.Errorf("noise: import remote static: %w", err)
	}
	es, err := dhP256(n.ePriv, rSPubKey)
	if err != nil {
		return fmt.Errorf("noise: DH es: %w", err)
	}
	n.mixKey(es)

	// verify empty payload tag
	_, err = n.decryptAndHash(msg[146:162])
	if err == nil {
		n.phase = phaseMsg2Received
	}
	return err
}

// WriteMessage3 -- initiator sends: s, se (97 bytes)
func (n *NoiseXX) WriteMessage3() ([]byte, error) {
	if n.role != roleInitiator {
		return nil, errors.New("noise: WriteMessage3 called on responder")
	}
	if n.phase != phaseMsg2Received {
		return nil, fmt.Errorf("noise: WriteMessage3 invalid in phase %d", n.phase)
	}
	buf := make([]byte, 0, 97)

	// token: s -- EncryptAndHash(is_pub)
	encS, err := n.encryptAndHash(n.sPub)
	if err != nil {
		return nil, fmt.Errorf("noise: EncryptAndHash s: %w", err)
	}
	buf = append(buf, encS...) // 81 bytes

	// token: se -- DH(is, re)
	rEPubKey, err := importPublicKey(n.rEPub)
	if err != nil {
		return nil, err
	}
	se, err := dhP256(n.sPriv, rEPubKey)
	if err != nil {
		return nil, fmt.Errorf("noise: DH se: %w", err)
	}
	n.mixKey(se)

	// empty payload
	tag, err := n.encryptAndHash([]byte{})
	if err != nil {
		return nil, err
	}
	buf = append(buf, tag...) // 16 bytes
	n.phase = phaseDone
	return buf, nil
}

// ReadMessage3 -- responder receives: s, se (97 bytes)
func (n *NoiseXX) ReadMessage3(msg []byte) error {
	if n.role != roleResponder {
		return errors.New("noise: ReadMessage3 called on initiator")
	}
	if n.phase != phaseMsg2Sent {
		return fmt.Errorf("noise: ReadMessage3 invalid in phase %d", n.phase)
	}
	if len(msg) != 97 {
		return fmt.Errorf("noise: ReadMessage3: expected 97 bytes, got %d", len(msg))
	}

	// token: s -- DecryptAndHash(encrypted_is_pub)
	isPub, err := n.decryptAndHash(msg[:81])
	if err != nil {
		return fmt.Errorf("noise: DecryptAndHash s: %w", err)
	}
	n.rSPub = isPub

	// token: se -- DH(re, is)
	iSPubKey, err := importPublicKey(isPub)
	if err != nil {
		return fmt.Errorf("noise: import initiator static: %w", err)
	}
	se, err := dhP256(n.ePriv, iSPubKey)
	if err != nil {
		return fmt.Errorf("noise: DH se: %w", err)
	}
	n.mixKey(se)

	// verify empty payload tag
	_, err = n.decryptAndHash(msg[81:97])
	if err == nil {
		n.phase = phaseDone
	}
	return err
}

// Split derives post-handshake cipher states.
// Returns (send, recv) from the caller's perspective.
func (n *NoiseXX) Split() (send, recv *CipherState) {
	if n.phase != phaseDone {
		panic("noise: Split called before handshake completion")
	}
	k1, k2 := splitKeys(n.ck)
	cs1, cs2 := newCipherState(k1), newCipherState(k2)
	if n.role == roleInitiator {
		return cs1, cs2 // initiator sends on k1, receives on k2
	}
	return cs2, cs1 // responder sends on k2, receives on k1
}

// mixKey updates ck and k from a DH output (Noise spec §5.2 MixKey).
func (n *NoiseXX) mixKey(ikm []byte) {
	n.ck, n.k = hkdf2(n.ck, ikm)
	n.hasK = true
}

// encryptAndHash: ciphertext = EncryptWithAd(h, pt); MixHash(ciphertext)
func (n *NoiseXX) encryptAndHash(plaintext []byte) ([]byte, error) {
	if !n.hasK {
		return nil, errors.New("noise: cipher key not initialized")
	}
	cs := &CipherState{key: n.k}
	ct, err := cs.encryptWithAd(n.h[:], plaintext)
	if err != nil {
		return nil, err
	}
	n.h = mixHash(n.h, ct)
	return ct, nil
}

// decryptAndHash: plaintext = DecryptWithAd(h, ct); MixHash(ciphertext)
func (n *NoiseXX) decryptAndHash(ciphertext []byte) ([]byte, error) {
	if !n.hasK {
		return nil, errors.New("noise: cipher key not initialized")
	}
	cs := &CipherState{key: n.k}
	pt, err := cs.decryptWithAd(n.h[:], ciphertext)
	if err != nil {
		return nil, err
	}
	n.h = mixHash(n.h, ciphertext)
	return pt, nil
}