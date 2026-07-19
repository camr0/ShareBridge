package multilane

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

const (
	BaseQuantumBytes           = 64 * 1024
	ControlBurstLimit          = 8
	ControlQueueCapBytes       = 8 * 1024 * 1024
	InteractiveQueueCapBytes   = 512 * 1024
	ThumbnailQueueCapBytes     = 2 * 1024 * 1024
	BulkQueueCapBytes          = 256 * 1024
	NativeBufferHighWaterBytes = 256 * 1024
)

var (
	ErrRequestTooLarge = errors.New("multilane request exceeds queue capacity")
	ErrSchedulerClosed = errors.New("multilane scheduler closed")
)

// WriteFunc accepts one complete request. Calls are serialized by Scheduler.
type WriteFunc func(class TrafficClass, kind Kind, payload []byte) error

type sendRequest struct {
	class   TrafficClass
	kind    Kind
	payload []byte
	done    chan error
}

// Scheduler serializes bounded traffic-class queues with byte-weighted DRR.
type Scheduler struct {
	mu       sync.Mutex
	writer   WriteFunc
	queues   map[TrafficClass][]*sendRequest
	bytes    map[TrafficClass]int // queued plus currently writing
	notify   chan struct{}
	terminal error
	active   *sendRequest

	controlBurst int
	outerCurrent int
	outerDeficit [2]int
	outerStarted bool
	innerCurrent int
	innerDeficit [2]int
	innerStarted bool
}

func NewScheduler(writer WriteFunc) *Scheduler {
	s := &Scheduler{
		writer: writer,
		queues: make(map[TrafficClass][]*sendRequest),
		bytes:  make(map[TrafficClass]int),
		notify: make(chan struct{}),
	}
	go s.pump()
	return s
}

func classCap(class TrafficClass) (int, bool) {
	switch class {
	case ClassControl:
		return ControlQueueCapBytes, true
	case ClassInteractiveMedia:
		return InteractiveQueueCapBytes, true
	case ClassThumbnail:
		return ThumbnailQueueCapBytes, true
	case ClassBulk:
		return BulkQueueCapBytes, true
	default:
		return 0, false
	}
}

// Send waits for bounded queue capacity and for the writer to accept the request.
func (s *Scheduler) Send(ctx context.Context, class TrafficClass, kind Kind, payload []byte) error {
	capBytes, valid := classCap(class)
	if !valid {
		return fmt.Errorf("unknown traffic class: %d", class)
	}
	if len(payload) > capBytes {
		return fmt.Errorf("%w: class %d payload is %d bytes, cap is %d", ErrRequestTooLarge, class, len(payload), capBytes)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	request := &sendRequest{class: class, kind: kind, payload: append([]byte(nil), payload...), done: make(chan error, 1)}

	for {
		s.mu.Lock()
		if s.terminal != nil {
			err := s.terminal
			s.mu.Unlock()
			return err
		}
		if s.bytes[class]+len(request.payload) <= capBytes {
			s.queues[class] = append(s.queues[class], request)
			s.bytes[class] += len(request.payload)
			s.signalLocked()
			s.mu.Unlock()
			break
		}
		changed := s.notify
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}

	select {
	case err := <-request.done:
		return err
	case <-ctx.Done():
		s.mu.Lock()
		removed := s.removeQueuedLocked(request)
		if removed {
			s.bytes[class] -= len(request.payload)
			s.signalLocked()
		}
		s.mu.Unlock()
		if removed {
			return ctx.Err()
		}
		return <-request.done
	}
}

// Close rejects queued and future requests. It is safe to call repeatedly.
func (s *Scheduler) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminal == nil {
		s.terminal = ErrSchedulerClosed
		s.failQueuedLocked(ErrSchedulerClosed)
		s.signalLocked()
	}
	return nil
}

func (s *Scheduler) pump() {
	for {
		s.mu.Lock()
		for s.terminal == nil {
			request := s.nextLocked()
			if request != nil {
				s.active = request
				s.mu.Unlock()
				err := s.writer(request.class, request.kind, request.payload)
				s.mu.Lock()
				s.active = nil
				s.bytes[request.class] -= len(request.payload)
				if err != nil && s.terminal == nil {
					s.terminal = err
					s.failQueuedLocked(err)
				}
				s.signalLocked()
				s.mu.Unlock()
				request.done <- err
				goto next
			}
			changed := s.notify
			s.mu.Unlock()
			<-changed
			s.mu.Lock()
		}
		s.mu.Unlock()
		return
	next:
	}
}

func (s *Scheduler) nextLocked() *sendRequest {
	dataWaiting := s.hasMediaLocked() || len(s.queues[ClassBulk]) > 0
	if len(s.queues[ClassControl]) > 0 && (s.controlBurst < ControlBurstLimit || !dataWaiting) {
		s.controlBurst++
		return s.popLocked(ClassControl)
	}
	if dataWaiting {
		request := s.nextDataLocked()
		if request != nil {
			s.controlBurst = 0
			return request
		}
	}
	if len(s.queues[ClassControl]) > 0 {
		s.controlBurst++
		return s.popLocked(ClassControl)
	}
	return nil
}

func (s *Scheduler) nextDataLocked() *sendRequest {
	if !s.outerStarted {
		s.addOuterRoundLocked()
		s.outerStarted = true
	}
	for s.hasMediaLocked() || len(s.queues[ClassBulk]) > 0 {
		if s.outerCurrent == 0 {
			if !s.hasMediaLocked() {
				s.outerDeficit[0] = 0
				s.advanceOuterLocked()
				continue
			}
			innerCurrent, innerDeficit, innerStarted := s.innerCurrent, s.innerDeficit, s.innerStarted
			request := s.peekMediaLocked()
			if len(request.payload) <= s.outerDeficit[0] {
				s.outerDeficit[0] -= len(request.payload)
				return s.popLocked(request.class)
			}
			s.innerCurrent, s.innerDeficit, s.innerStarted = innerCurrent, innerDeficit, innerStarted
			s.advanceOuterLocked()
			continue
		}
		if len(s.queues[ClassBulk]) == 0 {
			s.outerDeficit[1] = 0
			s.advanceOuterLocked()
			continue
		}
		request := s.queues[ClassBulk][0]
		if len(request.payload) <= s.outerDeficit[1] {
			s.outerDeficit[1] -= len(request.payload)
			return s.popLocked(ClassBulk)
		}
		s.advanceOuterLocked()
	}
	return nil
}

func (s *Scheduler) addOuterRoundLocked() {
	if s.hasMediaLocked() {
		s.outerDeficit[0] += 3 * BaseQuantumBytes
	} else {
		s.outerDeficit[0] = 0
	}
	if len(s.queues[ClassBulk]) > 0 {
		s.outerDeficit[1] += BaseQuantumBytes
	} else {
		s.outerDeficit[1] = 0
	}
}

func (s *Scheduler) advanceOuterLocked() {
	s.outerCurrent = (s.outerCurrent + 1) % 2
	if s.outerCurrent == 0 {
		s.addOuterRoundLocked()
	}
}

func (s *Scheduler) peekMediaLocked() *sendRequest {
	if !s.innerStarted {
		s.addInnerRoundLocked()
		s.innerStarted = true
	}
	for s.hasMediaLocked() {
		class := ClassInteractiveMedia
		if s.innerCurrent == 1 {
			class = ClassThumbnail
		}
		if len(s.queues[class]) == 0 {
			s.innerDeficit[s.innerCurrent] = 0
			s.advanceInnerLocked()
			continue
		}
		request := s.queues[class][0]
		if len(request.payload) <= s.innerDeficit[s.innerCurrent] {
			s.innerDeficit[s.innerCurrent] -= len(request.payload)
			return request
		}
		s.advanceInnerLocked()
	}
	return nil
}

func (s *Scheduler) addInnerRoundLocked() {
	if len(s.queues[ClassInteractiveMedia]) > 0 {
		s.innerDeficit[0] += 3 * BaseQuantumBytes
	} else {
		s.innerDeficit[0] = 0
	}
	if len(s.queues[ClassThumbnail]) > 0 {
		s.innerDeficit[1] += BaseQuantumBytes
	} else {
		s.innerDeficit[1] = 0
	}
}

func (s *Scheduler) advanceInnerLocked() {
	s.innerCurrent = (s.innerCurrent + 1) % 2
	if s.innerCurrent == 0 {
		s.addInnerRoundLocked()
	}
}

func (s *Scheduler) hasMediaLocked() bool {
	return len(s.queues[ClassInteractiveMedia]) > 0 || len(s.queues[ClassThumbnail]) > 0
}

func (s *Scheduler) popLocked(class TrafficClass) *sendRequest {
	request := s.queues[class][0]
	s.queues[class] = s.queues[class][1:]
	return request
}

func (s *Scheduler) removeQueuedLocked(want *sendRequest) bool {
	queue := s.queues[want.class]
	for i, request := range queue {
		if request == want {
			s.queues[want.class] = append(queue[:i], queue[i+1:]...)
			return true
		}
	}
	return false
}

func (s *Scheduler) failQueuedLocked(err error) {
	for class, queue := range s.queues {
		for _, request := range queue {
			s.bytes[class] -= len(request.payload)
			request.done <- err
		}
		s.queues[class] = nil
	}
}

func (s *Scheduler) signalLocked() {
	close(s.notify)
	s.notify = make(chan struct{})
}
