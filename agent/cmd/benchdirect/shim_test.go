package main

import (
	"errors"
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

func TestShimRejectsInvalidLoss(t *testing.T) {
	for _, loss := range []float64{-0.1, 1.5} {
		s, err := NewShim(0, loss)
		if err == nil {
			if s != nil {
				_ = s.Close()
			}
			t.Fatalf("expected error for loss=%v", loss)
		}
	}
}

func TestShimIgnoresThirdPartyAfterBLearned(t *testing.T) {
	peerA, aAddr := listenUDP(t)
	defer peerA.Close()
	peerB, _ := listenUDP(t)
	defer peerB.Close()
	peerC, _ := listenUDP(t)
	defer peerC.Close()

	s, err := NewShim(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetPeerA(aAddr)

	if _, err := peerB.WriteToUDP([]byte("learn"), s.Addr()); err != nil {
		t.Fatal(err)
	}
	bufA := make([]byte, 16)
	_ = peerA.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := peerA.ReadFromUDP(bufA); err != nil {
		t.Fatalf("peer A did not receive learning packet: %v", err)
	}

	if _, err := peerC.WriteToUDP([]byte("third-party"), s.Addr()); err != nil {
		t.Fatal(err)
	}

	bufB := make([]byte, 32)
	_ = peerA.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if n, _, err := peerA.ReadFromUDP(bufA); err == nil {
		t.Fatalf("peer A unexpectedly received third-party packet: %q", string(bufA[:n]))
	} else if !errors.Is(err, net.ErrClosed) {
		if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
			t.Fatalf("peer A read failed unexpectedly: %v", err)
		}
	}
	_ = peerB.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if n, _, err := peerB.ReadFromUDP(bufB); err == nil {
		t.Fatalf("peer B unexpectedly received third-party packet: %q", string(bufB[:n]))
	} else if !errors.Is(err, net.ErrClosed) {
		if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
			t.Fatalf("peer B read failed unexpectedly: %v", err)
		}
	}
}

func TestShimCloseReturns(t *testing.T) {
	s, err := NewShim(0, 0)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- s.Close()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Close did not return promptly")
	}
}

// waitForCounter polls until fn reaches want, so assertions do not race the
// drain goroutine (counters are incremented after the datagram is written).
func waitForCounter(t *testing.T, want int64, fn func() int64, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := fn(); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s = %d, want %d", what, fn(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// learnAndEcho establishes a flow: B's first datagram teaches the shim B's
// address, then A's datagram proves the return path works. Returns nothing but
// fails the test on any error.
func learnAndEcho(t *testing.T, f *Flow, aConn, bConn *net.UDPConn, mark byte) {
	t.Helper()
	if _, err := bConn.WriteToUDP([]byte{mark}, f.Addr()); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	_ = aConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := aConn.ReadFromUDP(buf); err != nil {
		t.Fatalf("peer A did not receive learning packet: %v", err)
	}
	if _, err := aConn.WriteToUDP([]byte{mark, mark}, f.Addr()); err != nil {
		t.Fatal(err)
	}
	_ = bConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := bConn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("peer B did not receive echoed packet: %v", err)
	}
	if n != 2 || buf[0] != mark || buf[1] != mark {
		t.Fatalf("corrupted echo: %v", buf[:n])
	}
}

// One Shaper must carry several flows without mixing their peer pairings.
func TestShaperCarriesMultipleFlows(t *testing.T) {
	s, err := NewShaper(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const n = 3
	for i := 0; i < n; i++ {
		f, err := s.NewFlow()
		if err != nil {
			t.Fatal(err)
		}
		aConn, aAddr := listenUDP(t)
		defer aConn.Close()
		bConn, _ := listenUDP(t)
		defer bConn.Close()
		f.SetPeerA(aAddr)
		learnAndEcho(t, f, aConn, bConn, byte(i))
		if f.Addr().Port == 0 {
			t.Fatal("flow has no address")
		}
	}
}

// Every flow on a Shaper gets its own socket, so flows cannot share a 5-tuple.
func TestFlowsHaveDistinctAddresses(t *testing.T) {
	s, err := NewShaper(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	seen := map[int]bool{}
	for i := 0; i < 4; i++ {
		f, err := s.NewFlow()
		if err != nil {
			t.Fatal(err)
		}
		port := f.Addr().Port
		if seen[port] {
			t.Fatalf("duplicate flow port %d", port)
		}
		seen[port] = true
	}
}

// Shaper.Stats must aggregate per-flow counters, and per-flow Stats must report
// only that flow's share.
func TestShaperStatsAggregateAcrossFlows(t *testing.T) {
	s, err := NewShaper(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	f0, err := s.NewFlow()
	if err != nil {
		t.Fatal(err)
	}
	f1, err := s.NewFlow()
	if err != nil {
		t.Fatal(err)
	}
	a0, a0Addr := listenUDP(t)
	defer a0.Close()
	b0, _ := listenUDP(t)
	defer b0.Close()
	a1, a1Addr := listenUDP(t)
	defer a1.Close()
	b1, _ := listenUDP(t)
	defer b1.Close()

	f0.SetPeerA(a0Addr)
	f1.SetPeerA(a1Addr)
	learnAndEcho(t, f0, a0, b0, 1)
	learnAndEcho(t, f1, a1, b1, 2)

	// Each flow forwarded two datagrams (one learning, one echo).
	waitForCounter(t, 4, func() int64 { fw, _, _ := s.Stats(); return fw }, "shaper forwarded")
	waitForCounter(t, 2, func() int64 { fw, _, _ := f0.Stats(); return fw }, "flow 0 forwarded")
	waitForCounter(t, 2, func() int64 { fw, _, _ := f1.Stats(); return fw }, "flow 1 forwarded")
}

// A bandwidth cap on a Shaper is shared: overflowing the single queue must drop
// datagrams, and the per-flow drop counts must add up to the shaper total.
func TestShaperBandwidthCapIsSharedAcrossFlows(t *testing.T) {
	s, err := NewShaper(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetBandwidth(8 << 10) // 8 KiB/s => 800-byte shared queue

	flows := make([]*Flow, 2)
	senders := make([]*net.UDPConn, 2)
	buf := make([]byte, 16)
	for i := range flows {
		f, err := s.NewFlow()
		if err != nil {
			t.Fatal(err)
		}
		aConn, aAddr := listenUDP(t)
		defer aConn.Close()
		bConn, _ := listenUDP(t)
		defer bConn.Close()
		f.SetPeerA(aAddr)
		learnAndEcho(t, f, aConn, bConn, byte(i))
		_ = aConn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		flows[i], senders[i] = f, aConn
	}

	payload := make([]byte, 512)
	for burst := 0; burst < 40; burst++ {
		for i := range flows {
			if _, err := senders[i].WriteToUDP(payload, flows[i].Addr()); err != nil {
				t.Fatal(err)
			}
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	var total int64
	for time.Now().Before(deadline) {
		_, _, total = s.Stats()
		if total > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if total == 0 {
		t.Fatal("expected tail drop on the shared queue, got none")
	}

	_, _, d0 := flows[0].Stats()
	_, _, d1 := flows[1].Stats()
	if d0+d1 != total {
		t.Fatalf("per-flow drops %d+%d != shaper drops %d", d0, d1, total)
	}
	_ = buf
}

// Separate Shapers must not share accounting or capacity.
func TestSeparateShapersAreIsolated(t *testing.T) {
	s0, err := NewShaper(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s0.Close()
	s1, err := NewShaper(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()

	f0, err := s0.NewFlow()
	if err != nil {
		t.Fatal(err)
	}
	a0, a0Addr := listenUDP(t)
	defer a0.Close()
	b0, _ := listenUDP(t)
	defer b0.Close()
	f0.SetPeerA(a0Addr)
	learnAndEcho(t, f0, a0, b0, 1)

	waitForCounter(t, 2, func() int64 { fw, _, _ := s0.Stats(); return fw }, "shaper 0 forwarded")
	if fw, _, _ := s1.Stats(); fw != 0 {
		t.Fatalf("shaper 1 forwarded = %d, want 0 (isolated)", fw)
	}
}

// An explicit queue depth must force tail drop even with no rate cap, and must
// work regardless of whether it is set before or after SetBandwidth.
func TestQueueBytesOverrideForcesTailDrop(t *testing.T) {
	for _, order := range []string{"queue-first", "bandwidth-first"} {
		t.Run(order, func(t *testing.T) {
			s, err := NewShaper(50*time.Millisecond, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if order == "queue-first" {
				s.SetQueueBytes(1000)
			} else {
				s.SetBandwidth(0) // no cap; must not clear an explicit queue
				s.SetQueueBytes(1000)
			}

			f, err := s.NewFlow()
			if err != nil {
				t.Fatal(err)
			}
			aConn, aAddr := listenUDP(t)
			defer aConn.Close()
			bConn, _ := listenUDP(t)
			defer bConn.Close()
			f.SetPeerA(aAddr)
			learnAndEcho(t, f, aConn, bConn, 1)
			time.Sleep(150 * time.Millisecond) // let the learning exchange drain

			_, _, before := f.Stats()
			payload := make([]byte, 512)
			for i := 0; i < 20; i++ {
				if _, err := aConn.WriteToUDP(payload, f.Addr()); err != nil {
					t.Fatal(err)
				}
			}

			// 20 x 512 B = 10 KiB against a 1 KiB queue held for 50 ms: must drop.
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				_, _, dropped := f.Stats()
				if dropped > before {
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
			t.Fatal("expected tail drop with an explicit 1000-byte queue, got none")
		})
	}
}

func TestNewFlowAfterCloseFails(t *testing.T) {
	s, err := NewShaper(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NewFlow(); err == nil {
		t.Fatal("expected error creating a flow on a closed shaper")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close returned error: %v", err)
	}
}
