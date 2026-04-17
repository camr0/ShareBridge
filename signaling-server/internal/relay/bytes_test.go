package relay

import (
	"crypto/rand"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
)

func TestByteTracker_rejectsConcurrentBrowsers(t *testing.T) {
	tracker := NewByteTracker(time.Minute)
	browser1 := mustPeerID(t)
	browser2 := mustPeerID(t)
	agent := mustPeerID(t)
	addr, _ := multiaddr.NewMultiaddr("/ip4/127.0.0.1/tcp/9001")

	tracker.Authorize(browser1, agent, "code-1", "key-1", time.Minute)
	tracker.Authorize(browser2, agent, "code-2", "key-2", time.Minute)

	if !tracker.AllowConnect(browser1, addr, agent) {
		t.Fatal("first browser should be allowed")
	}
	if tracker.AllowConnect(browser2, addr, agent) {
		t.Fatal("second concurrent browser should be rejected to avoid mis-attributed bytes")
	}
}

func mustPeerID(t *testing.T) peer.ID {
	t.Helper()
	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateEd25519Key: %v", err)
	}
	pid, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		t.Fatalf("IDFromPrivateKey: %v", err)
	}
	return pid
}
