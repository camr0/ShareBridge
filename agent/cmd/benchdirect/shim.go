package main

import (
	"errors"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// flowState is the per-5-tuple state of one Flow. All fields are guarded by the
// owning Shaper's mutex so a single lock orders both queue and flow state.
type flowState struct {
	conn *net.UDPConn
	a    *net.UDPAddr
	b    *net.UDPAddr

	forwarded atomic.Int64
	writeErrs atomic.Int64
	dropped   atomic.Int64
}

type packet struct {
	data []byte
	st   *flowState
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

// Shaper is a shared bottleneck: one delay/loss/jitter model, one rate limiter
// and one FIFO queue shared by every Flow attached to it.
//
// Attach several Flows to one Shaper to model N connections competing for a
// single link (realistic for a home uplink). Create several Shapers to model N
// independent links (each connection gets its own capacity).
type Shaper struct {
	mu            sync.Mutex
	delay         time.Duration
	loss          float64
	jitter        time.Duration
	queue         []packet
	notify        chan struct{}
	done          chan struct{}
	wg            sync.WaitGroup
	closed        bool
	limiter       *rateLimiter
	maxQueueBytes int64
	queueBytes    int64
	flows         []*flowState
}

// NewShaper starts a bottleneck with the given one-way delay and packet loss.
func NewShaper(delay time.Duration, loss float64) (*Shaper, error) {
	if loss < 0 || loss > 1 {
		return nil, errors.New("loss must be between 0 and 1")
	}
	s := &Shaper{
		delay:  delay,
		loss:   loss,
		notify: make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
	s.wg.Add(1)
	go s.drainLoop()
	return s, nil
}

// Flow is one 5-tuple pair (one UDP socket) carried by a Shaper. Datagrams from
// peer A are forwarded to peer B and vice versa, each after the Shaper's delay.
// Peer A is configured via SetPeerA; peer B is learned from the first datagram
// whose source address is not A.
type Flow struct {
	sh *Shaper
	st *flowState
}

// NewFlow binds a fresh loopback UDP socket to this Shaper.
func (s *Shaper) NewFlow() (*Flow, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	// Generous socket buffers so the kernel does not drop datagrams that the
	// Shaper intends to model itself.
	_ = conn.SetReadBuffer(16 << 20)
	_ = conn.SetWriteBuffer(16 << 20)

	st := &flowState{conn: conn}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = conn.Close()
		return nil, errors.New("shaper closed")
	}
	s.flows = append(s.flows, st)
	s.wg.Add(1)
	s.mu.Unlock()

	go s.readLoop(st)
	return &Flow{sh: s, st: st}, nil
}

func (f *Flow) Addr() *net.UDPAddr { return f.st.conn.LocalAddr().(*net.UDPAddr) }

func (f *Flow) SetPeerA(a *net.UDPAddr) {
	f.sh.mu.Lock()
	f.st.a = a
	f.sh.mu.Unlock()
}

// Stats returns this flow's forwarded, write-error and dropped datagram counts.
func (f *Flow) Stats() (forwarded, writeErrs, dropped int64) {
	return f.st.forwarded.Load(), f.st.writeErrs.Load(), f.st.dropped.Load()
}

// ExtendJitter sets the per-packet uniform +/- delay jitter.
func (s *Shaper) SetJitter(j time.Duration) {
	s.mu.Lock()
	s.jitter = j
	s.mu.Unlock()
}

// SetBandwidth caps the aggregate byte rate across all flows on this Shaper.
// Zero disables the cap.
func (s *Shaper) SetBandwidth(bytesPerSec int64) {
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

// Stats aggregates counters across every flow on this Shaper.
func (s *Shaper) Stats() (forwarded, writeErrs, dropped int64) {
	s.mu.Lock()
	flows := append([]*flowState(nil), s.flows...)
	s.mu.Unlock()
	for _, st := range flows {
		forwarded += st.forwarded.Load()
		writeErrs += st.writeErrs.Load()
		dropped += st.dropped.Load()
	}
	return forwarded, writeErrs, dropped
}

func sameUDPAddr(x, y *net.UDPAddr) bool {
	return x != nil && y != nil && x.Port == y.Port && x.IP.Equal(y.IP)
}

func (s *Shaper) readLoop(st *flowState) {
	defer s.wg.Done()
	buf := make([]byte, 64*1024)
	for {
		n, src, err := st.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		data := make([]byte, n)
		copy(data, buf[:n])
		s.ingest(st, src, data)
	}
}

func (s *Shaper) ingest(st *flowState, src *net.UDPAddr, data []byte) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if rand.Float64() < s.loss {
		s.mu.Unlock()
		return
	}

	var target *net.UDPAddr
	switch {
	case sameUDPAddr(src, st.a):
		target = st.b
	case sameUDPAddr(src, st.b):
		target = st.a
	case st.b == nil && st.a != nil:
		// First datagram from the far side: learn it as peer B.
		st.b = &net.UDPAddr{IP: append(net.IP(nil), src.IP...), Port: src.Port, Zone: src.Zone}
		target = st.a
	default:
		s.mu.Unlock()
		return
	}
	if target == nil {
		s.mu.Unlock()
		return
	}

	if s.maxQueueBytes > 0 && s.queueBytes+int64(len(data)) > s.maxQueueBytes {
		st.dropped.Add(1)
		s.mu.Unlock()
		return
	}
	headEmpty := len(s.queue) == 0
	d := s.delay
	if s.jitter > 0 {
		d += time.Duration((rand.Float64()*2 - 1) * float64(s.jitter))
	}
	s.queue = append(s.queue, packet{data: data, st: st, addr: target, due: time.Now().Add(d)})
	s.queueBytes += int64(len(data))
	if headEmpty {
		select {
		case s.notify <- struct{}{}:
		default:
		}
	}
	s.mu.Unlock()
}

func (s *Shaper) drainLoop() {
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
			if _, err := head.st.conn.WriteToUDP(head.data, head.addr); err != nil {
				head.st.writeErrs.Add(1)
			} else {
				head.st.forwarded.Add(1)
			}
		}
	}
}

// Close stops the Shaper and every Flow attached to it.
func (s *Shaper) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.done)
	conns := make([]*net.UDPConn, 0, len(s.flows))
	for _, st := range s.flows {
		conns = append(conns, st.conn)
	}
	s.mu.Unlock()

	for _, c := range conns {
		_ = c.Close()
	}
	s.wg.Wait()
	return nil
}

// Shim is a single-flow Shaper: the original single-connection harness surface.
type Shim struct {
	*Shaper
	flow *Flow
}

// NewShim returns a Shaper carrying exactly one Flow.
func NewShim(delay time.Duration, loss float64) (*Shim, error) {
	s, err := NewShaper(delay, loss)
	if err != nil {
		return nil, err
	}
	f, err := s.NewFlow()
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	return &Shim{Shaper: s, flow: f}, nil
}

func (s *Shim) Addr() *net.UDPAddr { return s.flow.Addr() }

func (s *Shim) SetPeerA(a *net.UDPAddr) { s.flow.SetPeerA(a) }
