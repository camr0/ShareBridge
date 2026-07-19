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
	ErrEmptyPayload    = errors.New("multilane scheduler payload must be non-empty")
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

type admissionWaiter struct {
	class   TrafficClass
	kind    Kind
	payload []byte // caller-owned until admission; copied by admitWaitersLocked
	ready   chan error
	request *sendRequest
}

// Scheduler serializes bounded traffic-class queues with byte-weighted DRR.
type Scheduler struct {
	mu               sync.Mutex
	writer           WriteFunc
	queues           map[TrafficClass][]*sendRequest
	bytes            map[TrafficClass]int // queued plus currently writing
	admissionWaiters map[TrafficClass][]*admissionWaiter
	notify           chan struct{}
	terminal         error
	active           *sendRequest

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
		writer:           writer,
		queues:           make(map[TrafficClass][]*sendRequest),
		bytes:            make(map[TrafficClass]int),
		admissionWaiters: make(map[TrafficClass][]*admissionWaiter),
		notify:           make(chan struct{}),
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

// Send waits for bounded admitted-byte capacity and for the writer to accept the request.
// Scheduled frames require a non-empty payload; the lane envelope API has its own contract.
// Payload bytes remain caller-owned while admission is blocked and are copied on enqueue.
// The caller must not mutate payload concurrently with Send; after Send returns, ownership is unrestricted.
func (s *Scheduler) Send(ctx context.Context, class TrafficClass, kind Kind, payload []byte) error {
	capBytes, valid := classCap(class)
	if !valid {
		return fmt.Errorf("unknown traffic class: %d", class)
	}
	if len(payload) > capBytes {
		return fmt.Errorf("%w: class %d payload is %d bytes, cap is %d", ErrRequestTooLarge, class, len(payload), capBytes)
	}
	if kind != KindText && kind != KindBinary {
		return fmt.Errorf("unknown kind: %d", kind)
	}
	if len(payload) == 0 {
		return ErrEmptyPayload
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	if s.terminal != nil {
		err := s.terminal
		s.mu.Unlock()
		return err
	}
	var request *sendRequest
	if len(s.admissionWaiters[class]) == 0 && s.bytes[class]+len(payload) <= capBytes {
		request = s.enqueueLocked(class, kind, payload)
		s.mu.Unlock()
	} else {
		waiter := &admissionWaiter{class: class, kind: kind, payload: payload, ready: make(chan error, 1)}
		s.admissionWaiters[class] = append(s.admissionWaiters[class], waiter)
		s.mu.Unlock()
		select {
		case err := <-waiter.ready:
			if err != nil {
				return err
			}
			request = waiter.request
		case <-ctx.Done():
			s.mu.Lock()
			removed := s.removeAdmissionWaiterLocked(waiter)
			if removed {
				s.admitWaitersLocked(class)
			}
			s.mu.Unlock()
			if removed {
				return ctx.Err()
			}
			if err := <-waiter.ready; err != nil {
				return err
			}
			request = waiter.request
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
			s.admitWaitersLocked(class)
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
		s.failAdmissionWaitersLocked(ErrSchedulerClosed)
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
					s.failAdmissionWaitersLocked(err)
					s.failQueuedLocked(err)
				} else if err == nil && s.terminal == nil {
					s.admitWaitersLocked(request.class)
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
	queue := s.queues[class]
	request := queue[0]
	queue[0] = nil
	s.queues[class] = queue[1:]
	if len(s.queues[class]) == 0 {
		s.resetEmptyLaneLocked(class)
	}
	return request
}

func (s *Scheduler) resetEmptyLaneLocked(class TrafficClass) {
	if !s.hasClassDemandLocked(class) {
		switch class {
		case ClassInteractiveMedia:
			s.innerDeficit[0] = 0
		case ClassThumbnail:
			s.innerDeficit[1] = 0
		case ClassBulk:
			s.outerDeficit[1] = 0
		}
	}
	if !s.hasMediaDemandLocked() {
		s.outerDeficit[0] = 0
		s.innerCurrent = 0
		s.innerDeficit = [2]int{}
		s.innerStarted = false
	}
	if !s.hasMediaDemandLocked() && !s.hasClassDemandLocked(ClassBulk) {
		s.outerCurrent = 0
		s.outerDeficit = [2]int{}
		s.outerStarted = false
	}
}

func (s *Scheduler) hasClassDemandLocked(class TrafficClass) bool {
	return len(s.queues[class]) > 0 || len(s.admissionWaiters[class]) > 0
}

func (s *Scheduler) hasMediaDemandLocked() bool {
	return s.hasClassDemandLocked(ClassInteractiveMedia) || s.hasClassDemandLocked(ClassThumbnail)
}

func (s *Scheduler) enqueueLocked(class TrafficClass, kind Kind, payload []byte) *sendRequest {
	request := &sendRequest{class: class, kind: kind, payload: append([]byte(nil), payload...), done: make(chan error, 1)}
	s.bytes[class] += len(request.payload)
	s.queues[class] = append(s.queues[class], request)
	s.signalLocked()
	return request
}

func (s *Scheduler) admitWaitersLocked(class TrafficClass) {
	capBytes, _ := classCap(class)
	waiters := s.admissionWaiters[class]
	for len(waiters) > 0 && s.bytes[class]+len(waiters[0].payload) <= capBytes {
		waiter := waiters[0]
		waiters[0] = nil
		waiters = waiters[1:]
		waiter.request = s.enqueueLocked(waiter.class, waiter.kind, waiter.payload)
		waiter.ready <- nil
	}
	s.admissionWaiters[class] = waiters
}

func (s *Scheduler) removeAdmissionWaiterLocked(want *admissionWaiter) bool {
	waiters := s.admissionWaiters[want.class]
	for i, waiter := range waiters {
		if waiter == want {
			copy(waiters[i:], waiters[i+1:])
			waiters[len(waiters)-1] = nil
			s.admissionWaiters[want.class] = waiters[:len(waiters)-1]
			if len(s.queues[want.class]) == 0 {
				s.resetEmptyLaneLocked(want.class)
			}
			return true
		}
	}
	return false
}

func (s *Scheduler) failAdmissionWaitersLocked(err error) {
	for class, waiters := range s.admissionWaiters {
		for i, waiter := range waiters {
			waiter.ready <- err
			waiters[i] = nil
		}
		s.admissionWaiters[class] = nil
	}
}

func (s *Scheduler) removeQueuedLocked(want *sendRequest) bool {
	queue := s.queues[want.class]
	for i, request := range queue {
		if request == want {
			copy(queue[i:], queue[i+1:])
			queue[len(queue)-1] = nil
			s.queues[want.class] = queue[:len(queue)-1]
			if len(s.queues[want.class]) == 0 {
				s.resetEmptyLaneLocked(want.class)
			}
			return true
		}
	}
	return false
}

func (s *Scheduler) failQueuedLocked(err error) {
	for class, queue := range s.queues {
		for i, request := range queue {
			s.bytes[class] -= len(request.payload)
			request.done <- err
			queue[i] = nil
		}
		s.queues[class] = nil
	}
}

func (s *Scheduler) signalLocked() {
	close(s.notify)
	s.notify = make(chan struct{})
}
