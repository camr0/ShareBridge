package main

import (
	"math/rand"
	"net"
	"sync"
	"time"
)

type packet struct {
	data []byte
	addr *net.UDPAddr
	due  time.Time
}

// Shim is an in-process reflexive NAT that injects latency and loss between two
// peers. Peer A is configured via SetPeerA; peer B is learned from the first
// datagram whose source address is not A. Datagrams from A are forwarded to B
// and vice versa, each after a fixed delay.
type Shim struct {
	mu     sync.Mutex
	conn   *net.UDPConn
	delay  time.Duration
	loss   float64
	a      *net.UDPAddr
	b      *net.UDPAddr
	queue  []packet
	notify chan struct{}
	closed bool
}

func NewShim(delay time.Duration, loss float64) (*Shim, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	s := &Shim{conn: conn, delay: delay, loss: loss, notify: make(chan struct{}, 1)}
	go s.readLoop()
	go s.drainLoop()
	return s, nil
}

func (s *Shim) Addr() *net.UDPAddr { return s.conn.LocalAddr().(*net.UDPAddr) }

func (s *Shim) SetPeerA(a *net.UDPAddr) {
	s.mu.Lock()
	s.a = a
	s.mu.Unlock()
}

func (s *Shim) readLoop() {
	buf := make([]byte, 64*1024)
	for {
		n, src, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		data := make([]byte, n)
		copy(data, buf[:n])

		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			continue
		}
		if rand.Float64() < s.loss {
			s.mu.Unlock()
			continue
		}
		var target *net.UDPAddr
		if s.a != nil && src.Port == s.a.Port && src.IP.Equal(s.a.IP) {
			target = s.b
		} else {
			if s.b == nil {
				s.b = src
			}
			target = s.a
		}
		if target == nil {
			s.mu.Unlock()
			continue
		}
		headEmpty := len(s.queue) == 0
		s.queue = append(s.queue, packet{data: data, addr: target, due: time.Now().Add(s.delay)})
		if headEmpty {
			select {
			case s.notify <- struct{}{}:
			default:
			}
		}
		s.mu.Unlock()
	}
}

func (s *Shim) drainLoop() {
	for {
		<-s.notify
		for {
			s.mu.Lock()
			if len(s.queue) == 0 {
				s.mu.Unlock()
				break
			}
			head := s.queue[0]
			wait := time.Until(head.due)
			s.queue = s.queue[1:]
			s.mu.Unlock()

			if wait > 0 {
				time.Sleep(wait)
			}
			_, _ = s.conn.WriteToUDP(head.data, head.addr)
		}
	}
}

func (s *Shim) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return s.conn.Close()
}
