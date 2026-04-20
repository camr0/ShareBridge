package main

import (
	"bytes"
	"testing"
)

type relayKeyStoreStub struct {
	called bool
	key    []byte
	err    error
}

func (s *relayKeyStoreStub) GetRelayStaticPrivateKey() ([]byte, error) {
	s.called = true
	return s.key, s.err
}

func TestRegistrationRelayStaticPub_DisabledTransportSkipsKeyLookup(t *testing.T) {
	st := &relayKeyStoreStub{
		key: bytes.Repeat([]byte{0x11}, 32),
	}

	pub, err := registrationRelayStaticPub(false, st)
	if err != nil {
		t.Fatalf("registrationRelayStaticPub(false): %v", err)
	}
	if pub != "" {
		t.Fatalf("registrationRelayStaticPub(false) = %q, want empty", pub)
	}
	if st.called {
		t.Fatal("relay static key lookup should be skipped when relay transport is disabled")
	}
}

