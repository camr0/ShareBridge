package main

import (
	"errors"
	"math/rand"
	"net"
	"sync"
	"time"
)

type packet struct {
	data []byte
	due  time.Time
}

type Route struct {
	mu      sync.RWMutex
	conn    *net.UDPConn
	delay   time.Duration
	loss    float64
	target  *net.UDPAddr
	packets chan packet
	done    chan struct{}
	once    sync.Once
	wg      sync.WaitGroup
}

func (r *Route) Addr() *net.UDPAddr {
	return r.conn.LocalAddr().(*net.UDPAddr)
}

func (r *Route) SetForward(addr *net.UDPAddr) {
	r.mu.Lock()
	r.target = addr
	r.mu.Unlock()
}

func (r *Route) close() error {
	var err error
	r.once.Do(func() {
		close(r.done)
		err = r.conn.Close()
	})
	r.wg.Wait()
	return err
}

func (r *Route) readLoop() {
	defer close(r.packets)
	defer r.wg.Done()

	buf := make([]byte, 64*1024)
	for {
		n, _, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}

		r.mu.RLock()
		target := r.target
		loss := r.loss
		r.mu.RUnlock()
		if target == nil || rand.Float64() < loss {
			continue
		}

		data := make([]byte, n)
		copy(data, buf[:n])
		pkt := packet{data: data, due: time.Now().Add(r.delay)}

		select {
		case r.packets <- pkt:
		case <-r.done:
			return
		}
	}
}

func (r *Route) forwardLoop() {
	defer r.wg.Done()

	for {
		select {
		case <-r.done:
			return
		case pkt, ok := <-r.packets:
			if !ok {
				return
			}

			if wait := time.Until(pkt.due); wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-timer.C:
				case <-r.done:
					if !timer.Stop() {
						<-timer.C
					}
					return
				}
			}

			r.mu.RLock()
			target := r.target
			r.mu.RUnlock()
			if target == nil {
				continue
			}
			_, _ = r.conn.WriteToUDP(pkt.data, target)
		}
	}
}

type Shim struct {
	mu     sync.Mutex
	routes []*Route
}

func NewShim() *Shim {
	return &Shim{}
}

func (s *Shim) AddRoute(delay time.Duration, loss float64) (*Route, error) {
	if loss < 0 || loss > 1 {
		return nil, errors.New("loss must be between 0 and 1")
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}

	r := &Route{
		conn:    conn,
		delay:   delay,
		loss:    loss,
		packets: make(chan packet, 1024),
		done:    make(chan struct{}),
	}
	r.wg.Add(2)
	go r.readLoop()
	go r.forwardLoop()

	s.mu.Lock()
	s.routes = append(s.routes, r)
	s.mu.Unlock()

	return r, nil
}

func (s *Shim) Close() error {
	s.mu.Lock()
	routes := append([]*Route(nil), s.routes...)
	s.routes = nil
	s.mu.Unlock()

	var errs []error
	for _, r := range routes {
		if err := r.close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}
