// agent/internal/direct/opensignal_test.go
package direct

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestSignalGate_ReplayReorderReuse(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	gate := NewSignalGate("agent-1", func(shareID string, k RouteKind) bool {
		return shareID == "share-a" || shareID == "share-b"
	})
	gate.now = func() time.Time { return now }

	var seq uint64
	mk := func(shareID, nonce string) OpenSignal {
		seq++
		return OpenSignal{
			Version: signalVersion, AgentID: "agent-1",
			ShareID: shareID, RouteKind: RouteDirect, Nonce: nonce, Seq: seq,
			ExpiresAt: now.Add(time.Minute), Lease: 30 * time.Second,
		}
	}

	if err := gate.Admit(mk("share-a", "n1")); err != nil {
		t.Fatalf("first admit: %v", err)
	}
	// Replay: same nonce, same share → ErrReplaySignal.
	if err := gate.Admit(mk("share-a", "n1")); !errors.Is(err, ErrReplaySignal) {
		t.Fatalf("replay: want ErrReplaySignal, got %v", err)
	}
	// Reuse: same nonce, different share → ErrNonceReuse.
	if err := gate.Admit(mk("share-b", "n1")); !errors.Is(err, ErrNonceReuse) {
		t.Fatalf("reuse: want ErrNonceReuse, got %v", err)
	}
	// Reorder: a newer nonce is accepted, then the old nonce is replayed.
	if err := gate.Admit(mk("share-a", "n2")); err != nil {
		t.Fatalf("second admit: %v", err)
	}
	if err := gate.Admit(mk("share-a", "n1")); !errors.Is(err, ErrReplaySignal) {
		t.Fatalf("reorder: want ErrReplaySignal, got %v", err)
	}
	// Reorder with a DISTINCT nonce but an OLDER (lower) sequence number: the
	// nonce was never seen, but Seq alone must reject the out-of-order signal
	// (the protocol has no other ordering field).
	old := mk("share-a", "n3")
	old.Seq = 1 // older than the highest admitted sequence
	if err := gate.Admit(old); !errors.Is(err, ErrReplaySignal) {
		t.Fatalf("distinct nonce with older seq: want ErrReplaySignal, got %v", err)
	}
}

func TestSignalGate_ExpiryLockdownSourceAuthRateLimit(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	gate := NewSignalGate("agent-1", func(shareID string, k RouteKind) bool {
		return shareID == "share-a"
	})
	gate.now = func() time.Time { return now }

	var seq uint64
	mk := func() OpenSignal {
		seq++
		return OpenSignal{
			Version: signalVersion, AgentID: "agent-1", ShareID: "share-a",
			RouteKind: RouteDirect, Nonce: "n", Seq: seq, ExpiresAt: now.Add(time.Minute),
			Lease: 30 * time.Second,
		}
	}

	s := mk()
	s.Version = 99
	s.Nonce = "v"
	if err := gate.Admit(s); !errors.Is(err, ErrBadVersion) {
		t.Fatalf("version: %v", err)
	}
	s = mk()
	s.AgentID = "agent-2"
	s.Nonce = "a"
	if err := gate.Admit(s); !errors.Is(err, ErrWrongAgent) {
		t.Fatalf("agent: %v", err)
	}
	s = mk()
	s.ExpiresAt = now.Add(-time.Second)
	s.Nonce = "e"
	if err := gate.Admit(s); !errors.Is(err, ErrExpiredSignal) {
		t.Fatalf("expired: %v", err)
	}
	s = mk()
	s.ShareID = "not-registered"
	s.Nonce = "u"
	if err := gate.Admit(s); !errors.Is(err, ErrSignalNotAuth) {
		t.Fatalf("unregistered: %v", err)
	}
	gate.SetLockdown(true)
	s = mk()
	s.Nonce = "l"
	if err := gate.Admit(s); !errors.Is(err, ErrSignalLockdown) {
		t.Fatalf("lockdown: %v", err)
	}
	gate.SetLockdown(false)

	// Rate limit: per-share limit exceeded.
	for i := 0; i < maxPerSharePerWin; i++ {
		nm := mk()
		nm.Nonce = fmt.Sprintf("r%d", i)
		if err := gate.Admit(nm); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}
	s = mk()
	s.Nonce = "overflow"
	if err := gate.Admit(s); !errors.Is(err, ErrSignalRate) {
		t.Fatalf("rate limit: %v", err)
	}
}
