package main

import (
	"net"
	"testing"
	"time"
)

func listenUDP(t *testing.T) (*net.UDPConn, *net.UDPAddr) {
	t.Helper()
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	return pc, pc.LocalAddr().(*net.UDPAddr)
}

func TestShimForwardsBidirectionallyWithDelay(t *testing.T) {
	peerA, aAddr := listenUDP(t) // "Go"
	defer peerA.Close()
	peerB, _ := listenUDP(t) // "Chrome"
	defer peerB.Close()

	s, err := NewShim(200*time.Millisecond, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetPeerA(aAddr)

	// Chrome → Go: first non-A source learns B, forwards to A after delay.
	start := time.Now()
	if _, err := peerB.WriteToUDP([]byte("ping"), s.Addr()); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	_ = peerA.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := peerA.ReadFromUDP(buf); err != nil {
		t.Fatalf("peer A did not receive: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("corrupted: %q", buf)
	}
	if time.Since(start) < 200*time.Millisecond {
		t.Fatalf("arrived too early: %v", time.Since(start))
	}

	// Go → Chrome: source A forwards to learned B after delay.
	if _, err := peerA.WriteToUDP([]byte("pong"), s.Addr()); err != nil {
		t.Fatal(err)
	}
	buf2 := make([]byte, 4)
	_ = peerB.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := peerB.ReadFromUDP(buf2); err != nil {
		t.Fatalf("peer B did not receive: %v", err)
	}
	if string(buf2) != "pong" {
		t.Fatalf("corrupted: %q", buf2)
	}
}

func TestShimDropsWithFullLoss(t *testing.T) {
	peerA, aAddr := listenUDP(t)
	defer peerA.Close()
	peerB, _ := listenUDP(t)
	defer peerB.Close()

	s, err := NewShim(0, 1.0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetPeerA(aAddr)

	if _, err := peerB.WriteToUDP([]byte("ping"), s.Addr()); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	_ = peerA.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if _, _, err := peerA.ReadFromUDP(buf); err == nil {
		t.Fatal("expected packet to be dropped, but it arrived")
	}
}

func TestShimDropsFromABeforeBLearned(t *testing.T) {
	peerA, aAddr := listenUDP(t)
	defer peerA.Close()
	peerB, _ := listenUDP(t)
	defer peerB.Close()

	s, err := NewShim(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetPeerA(aAddr)

	if _, err := peerA.WriteToUDP([]byte("early"), s.Addr()); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	_ = peerB.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if _, _, err := peerB.ReadFromUDP(buf); err == nil {
		t.Fatal("expected packet from A to be dropped before B is learned")
	}
}
