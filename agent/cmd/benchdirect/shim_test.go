package main

import (
	"net"
	"testing"
	"time"
)

func echoServer(t *testing.T) (*net.UDPConn, *net.UDPAddr) {
	t.Helper()
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	return pc, pc.LocalAddr().(*net.UDPAddr)
}

func TestAddRouteRejectsInvalidLoss(t *testing.T) {
	s := NewShim()
	defer s.Close()

	for _, loss := range []float64{-0.01, 1.01} {
		if _, err := s.AddRoute(0, loss); err == nil {
			t.Fatalf("expected error for loss %v", loss)
		}
	}
}

func TestRouteForwardsAfterDelay(t *testing.T) {
	target, targetAddr := echoServer(t)
	defer target.Close()

	s := NewShim()
	defer s.Close()
	r, err := s.AddRoute(200*time.Millisecond, 0)
	if err != nil {
		t.Fatal(err)
	}
	r.SetForward(targetAddr)

	src, err := net.DialUDP("udp4", nil, r.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	start := time.Now()
	if _, err := src.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 4)
	_ = target.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := target.ReadFromUDP(buf); err != nil {
		t.Fatalf("no packet received: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 200*time.Millisecond {
		t.Fatalf("packet arrived too early: %v", elapsed)
	}
	if string(buf) != "ping" {
		t.Fatalf("payload corrupted: %q", buf)
	}
}

func TestRouteDropsWithFullLoss(t *testing.T) {
	target, targetAddr := echoServer(t)
	defer target.Close()

	s := NewShim()
	defer s.Close()
	r, err := s.AddRoute(0, 1.0) // 100% loss
	if err != nil {
		t.Fatal(err)
	}
	r.SetForward(targetAddr)

	src, err := net.DialUDP("udp4", nil, r.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 4)
	_ = target.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if _, _, err := target.ReadFromUDP(buf); err == nil {
		t.Fatal("expected packet to be dropped, but it arrived")
	}
}

func TestRoutePreservesPacketOrder(t *testing.T) {
	target, targetAddr := echoServer(t)
	defer target.Close()

	s := NewShim()
	defer s.Close()
	r, err := s.AddRoute(50*time.Millisecond, 0)
	if err != nil {
		t.Fatal(err)
	}
	r.SetForward(targetAddr)

	src, err := net.DialUDP("udp4", nil, r.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	for _, payload := range []string{"one", "two"} {
		if _, err := src.Write([]byte(payload)); err != nil {
			t.Fatal(err)
		}
	}

	buf := make([]byte, 3)
	for _, want := range []string{"one", "two"} {
		_ = target.SetReadDeadline(time.Now().Add(time.Second))
		n, _, err := target.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("missing packet %q: %v", want, err)
		}
		if got := string(buf[:n]); got != want {
			t.Fatalf("packet order broken: got %q, want %q", got, want)
		}
	}
}

func TestRouteDropsPacketsReceivedBeforeForward(t *testing.T) {
	target, _ := echoServer(t)
	defer target.Close()

	s := NewShim()
	defer s.Close()
	r, err := s.AddRoute(0, 0)
	if err != nil {
		t.Fatal(err)
	}

	src, err := net.DialUDP("udp4", nil, r.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	if _, err := src.Write([]byte("late")); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 4)
	_ = target.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if _, _, err := target.ReadFromUDP(buf); err == nil {
		t.Fatal("expected packet sent before SetForward to be dropped, but it arrived")
	}
}
