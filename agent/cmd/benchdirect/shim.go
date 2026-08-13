package main

import (
	"errors"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type packet struct {
	data []byte
	addr *net.UDPAddr
	due  time.Time
}

// rateLimiter enforces a byte-rate ceiling using a token bucket.
type rateLimiter struct {
	rate   float64 // bytes per second
	burst  float64 // max tokens (bytes)
	tokens float64
	last   time.Time
}

func newRateLimiter(bytesPerSec float64) *rateLimiter {
	burst := bytesPerSec * 0.1 // 100ms of buffering (typical router queue)
	return &rateLimiter{rate: bytesPerSec, burst: burst, tokens: burst, last: time.Now()}
}

func (r *rateLimiter) take(n int) {
	if r.rate <= 0 {
		return
	}
	for {
		now := time.Now()
		elapsed := now.Sub(r.last).Seconds()
		r.last = now
		r.tokens += elapsed * r.rate
		if r.tokens > r.burst {
			r.tokens = r.burst
		}
		if r.tokens >= float64(n) {
			r.tokens -= float64(n)
			return
		}
		deficit := float64(n) - r.tokens
		time.Sleep(time.Duration(deficit / r.rate * float64(time.Second)))
	}
}

// Shim is an in-process reflexive NAT that injects latency and loss between two
// peers. Peer A is configured via SetPeerA; peer B is learned from the first
// datagram whose source address is not A. Datagrams from A are forwarded to B
// and vice versa, each after a fixed delay.
type Shim struct {
	mu        sync.Mutex
	conn      *net.UDPConn
	delay     time.Duration
	loss      float64
	jitter    time.Duration
	a      *net.UDPAddr
	b      *net.UDPAddr
	queue     []packet
	notify    chan struct{}
	done      chan struct{}
	wg        sync.WaitGroup
	closed        bool
	limiter       *rateLimiter
	maxQueueBytes int64
	queueBytes    int64
	forwarded     atomic.Int64
	writeErrs     atomic.Int64
	dropped       atomic.Int64
}

func NewShim(delay time.Duration, loss float64) (*Shim, error) {
	if loss < 0 || loss > 1 {
		return nil, errors.New("loss must be between 0 and 1")
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	s := &Shim{
		conn:   conn,
		delay:  delay,
		loss:   loss,
		notify: make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
	s.wg.Add(2)
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

func (s *Shim) SetJitter(j time.Duration) {
	s.mu.Lock()
	s.jitter = j
	s.mu.Unlock()
}

func (s *Shim) SetBandwidth(bytesPerSec int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bytesPerSec <= 0 {
		s.limiter = nil
		s.maxQueueBytes = 0
		return
	}
	s.limiter = newRateLimiter(float64(bytesPerSec))
	s.maxQueueBytes = bytesPerSec / 10 // 100ms buffer before tail drop
}

func (s *Shim) readLoop() {
	defer s.wg.Done()
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
		if s.a != nil && src.IP.Equal(s.a.IP) && src.Port == s.a.Port {
			target = s.b
		} else if s.b != nil && src.IP.Equal(s.b.IP) && src.Port == s.b.Port {
			target = s.a
		} else if s.b == nil {
			s.b = src
			target = s.a
		} else {
			s.mu.Unlock()
			continue
		}
		if target == nil {
			s.mu.Unlock()
			continue
		}
		if s.maxQueueBytes > 0 && s.queueBytes+int64(len(data)) > s.maxQueueBytes {
			s.dropped.Add(1)
			s.mu.Unlock()
			continue
		}
		headEmpty := len(s.queue) == 0
		d := s.delay
		if s.jitter > 0 {
			d += time.Duration((rand.Float64()*2 - 1) * float64(s.jitter))
		}
		s.queue = append(s.queue, packet{data: data, addr: target, due: time.Now().Add(d)})
		s.queueBytes += int64(len(data))
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
	defer s.wg.Done()
	for {
		select {
		case <-s.done:
			return
		case <-s.notify:
		}
		for {
			s.mu.Lock()
			if len(s.queue) == 0 {
				s.mu.Unlock()
				break
			}
			head := s.queue[0]
			wait := time.Until(head.due)
			s.queue = s.queue[1:]
			s.queueBytes -= int64(len(head.data))
			limiter := s.limiter
			s.mu.Unlock()

			if wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-s.done:
					if !timer.Stop() {
						<-timer.C
					}
					return
				case <-timer.C:
				}
			}
			select {
			case <-s.done:
				return
			default:
			}
			if limiter != nil {
				limiter.take(len(head.data))
			}
			if _, err := s.conn.WriteToUDP(head.data, head.addr); err != nil {
				s.writeErrs.Add(1)
			} else {
				s.forwarded.Add(1)
			}
		}
	}
}

func (s *Shim) Stats() (forwarded, writeErrs, dropped int64) {
	return s.forwarded.Load(), s.writeErrs.Load(), s.dropped.Load()
}

func (s *Shim) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.done)
	s.mu.Unlock()
	err := s.conn.Close()
	s.wg.Wait()
	return err
}
