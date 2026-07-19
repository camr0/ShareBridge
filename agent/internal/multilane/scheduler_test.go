package multilane

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

const testFrameSize = 64 * 1024

type recordedWrite struct {
	class   TrafficClass
	kind    Kind
	payload []byte
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func queueBytes(s *Scheduler, class TrafficClass) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes[class]
}

func asyncSend(s *Scheduler, ctx context.Context, class TrafficClass, marker byte) <-chan error {
	done := make(chan error, 1)
	go func() {
		done <- s.Send(ctx, class, KindBinary, bytesOf(testFrameSize, marker))
	}()
	return done
}

func bytesOf(size int, marker byte) []byte {
	p := make([]byte, size)
	p[0] = marker
	return p
}

func TestSchedulerWeightedOuterServiceStartsThreeMediaToOneBulk(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	var mu sync.Mutex
	var writes []TrafficClass
	s := NewScheduler(func(class TrafficClass, _ Kind, _ []byte) error {
		mu.Lock()
		writes = append(writes, class)
		position := len(writes)
		mu.Unlock()
		if position == 1 {
			entered <- struct{}{}
			<-gate
		}
		return nil
	})
	t.Cleanup(func() { _ = s.Close() })

	control := asyncSend(s, context.Background(), ClassControl, 0)
	<-entered
	var sends []<-chan error
	for i := 0; i < 8; i++ {
		sends = append(sends, asyncSend(s, context.Background(), ClassInteractiveMedia, byte(i+1)))
	}
	for i := 0; i < 4; i++ {
		sends = append(sends, asyncSend(s, context.Background(), ClassBulk, byte(i+20)))
	}
	waitFor(t, "data admission", func() bool {
		return queueBytes(s, ClassInteractiveMedia) == 8*testFrameSize && queueBytes(s, ClassBulk) == 4*testFrameSize
	})
	close(gate)
	if err := <-control; err != nil {
		t.Fatal(err)
	}
	for _, done := range sends {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}

	mu.Lock()
	got := append([]TrafficClass(nil), writes[1:9]...)
	mu.Unlock()
	want := []TrafficClass{ClassInteractiveMedia, ClassInteractiveMedia, ClassInteractiveMedia, ClassBulk, ClassInteractiveMedia, ClassInteractiveMedia, ClassInteractiveMedia, ClassBulk}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("service order = %v, want %v", got, want)
	}
}

func TestSchedulerIdleResetsOuterAndInnerDeficits(t *testing.T) {
	tests := []struct {
		name  string
		fresh []TrafficClass
		want  []TrafficClass
	}{
		{name: "outer", fresh: []TrafficClass{ClassInteractiveMedia, ClassInteractiveMedia, ClassInteractiveMedia, ClassInteractiveMedia, ClassInteractiveMedia, ClassInteractiveMedia, ClassBulk, ClassBulk}, want: []TrafficClass{ClassInteractiveMedia, ClassInteractiveMedia, ClassInteractiveMedia, ClassBulk}},
		{name: "inner", fresh: []TrafficClass{ClassInteractiveMedia, ClassInteractiveMedia, ClassInteractiveMedia, ClassInteractiveMedia, ClassInteractiveMedia, ClassInteractiveMedia, ClassThumbnail, ClassThumbnail}, want: []TrafficClass{ClassInteractiveMedia, ClassInteractiveMedia, ClassInteractiveMedia, ClassThumbnail}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gate := make(chan struct{})
			entered := make(chan struct{})
			var mu sync.Mutex
			var writes []TrafficClass
			s := NewScheduler(func(class TrafficClass, _ Kind, _ []byte) error {
				mu.Lock()
				writes = append(writes, class)
				mu.Unlock()
				if class == ClassControl {
					close(entered)
					<-gate
				}
				return nil
			})
			if err := s.Send(context.Background(), ClassInteractiveMedia, KindBinary, bytesOf(testFrameSize, 1)); err != nil {
				t.Fatal(err)
			}
			control := asyncSend(s, context.Background(), ClassControl, 0)
			<-entered
			var sends []<-chan error
			for i, class := range tt.fresh {
				sends = append(sends, asyncSend(s, context.Background(), class, byte(i+2)))
			}
			waitFor(t, "fresh traffic admission", func() bool {
				return queueBytes(s, ClassInteractiveMedia)+queueBytes(s, ClassThumbnail)+queueBytes(s, ClassBulk) == len(tt.fresh)*testFrameSize
			})
			close(gate)
			if err := <-control; err != nil {
				t.Fatal(err)
			}
			for _, done := range sends {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
			_ = s.Close()
			mu.Lock()
			got := append([]TrafficClass(nil), writes[2:6]...)
			mu.Unlock()
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Fatalf("fresh service = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSchedulerWaitingDemandPreservesOuterAndInnerDeficits(t *testing.T) {
	tests := []struct {
		name    string
		classes []TrafficClass
		sizes   []int
		want    []TrafficClass
	}{
		{name: "outer", classes: append(repeatClass(ClassInteractiveMedia, 6), repeatClass(ClassBulk, 4)...), sizes: append(repeatInt(512*1024, 6), repeatInt(256*1024, 4)...), want: []TrafficClass{ClassInteractiveMedia, ClassBulk, ClassInteractiveMedia, ClassInteractiveMedia, ClassBulk}},
		{name: "inner", classes: append(repeatClass(ClassInteractiveMedia, 13), repeatClass(ClassThumbnail, 2)...), sizes: append(repeatInt(512*1024, 13), repeatInt(2*1024*1024, 2)...), want: append(repeatClass(ClassInteractiveMedia, 12), ClassThumbnail)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gate := make(chan struct{})
			entered := make(chan struct{})
			var mu sync.Mutex
			var writes []TrafficClass
			s := NewScheduler(func(class TrafficClass, _ Kind, _ []byte) error {
				mu.Lock()
				writes = append(writes, class)
				mu.Unlock()
				if class == ClassControl {
					close(entered)
					<-gate
				}
				return nil
			})
			control := asyncSend(s, context.Background(), ClassControl, 0)
			<-entered
			var sends []<-chan error
			for i, class := range tt.classes {
				done := make(chan error, 1)
				size := tt.sizes[i]
				go func() { done <- s.Send(context.Background(), class, KindBinary, make([]byte, size)) }()
				sends = append(sends, done)
			}
			waitFor(t, "sustained waiting demand", func() bool {
				s.mu.Lock()
				defer s.mu.Unlock()
				total := 0
				for _, ws := range s.admissionWaiters {
					total += len(ws)
				}
				return total >= len(tt.classes)-2
			})
			close(gate)
			_ = <-control
			for _, done := range sends {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
			_ = s.Close()
			mu.Lock()
			got := append([]TrafficClass(nil), writes[1:1+len(tt.want)]...)
			mu.Unlock()
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Fatalf("sustained service = %v, want %v", got, tt.want)
			}
		})
	}
}

func repeatClass(class TrafficClass, count int) []TrafficClass {
	values := make([]TrafficClass, count)
	for i := range values {
		values[i] = class
	}
	return values
}
func repeatInt(value, count int) []int {
	values := make([]int, count)
	for i := range values {
		values[i] = value
	}
	return values
}

func TestSchedulerUsesByteDeficitsForVariableFrames(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{})
	var mu sync.Mutex
	var writes []TrafficClass
	s := NewScheduler(func(class TrafficClass, _ Kind, _ []byte) error {
		mu.Lock()
		writes = append(writes, class)
		n := len(writes)
		mu.Unlock()
		if n == 1 {
			close(entered)
			<-gate
		}
		return nil
	})
	control := asyncSend(s, context.Background(), ClassControl, 0)
	<-entered
	var sends []<-chan error
	for i := 0; i < 4; i++ {
		done := make(chan error, 1)
		go func() { done <- s.Send(context.Background(), ClassInteractiveMedia, KindBinary, make([]byte, 96*1024)) }()
		sends = append(sends, done)
	}
	for i := 0; i < 2; i++ {
		sends = append(sends, asyncSend(s, context.Background(), ClassBulk, byte(i+1)))
	}
	waitFor(t, "variable traffic admission", func() bool {
		return queueBytes(s, ClassInteractiveMedia) == 4*96*1024 && queueBytes(s, ClassBulk) == 2*testFrameSize
	})
	close(gate)
	_ = <-control
	for _, done := range sends {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Close()
	mu.Lock()
	got := append([]TrafficClass(nil), writes[1:4]...)
	mu.Unlock()
	want := []TrafficClass{ClassInteractiveMedia, ClassInteractiveMedia, ClassBulk}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("service = %v, want byte-weighted %v", got, want)
	}
}

func TestSchedulerLimitsControlBurstToEight(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{})
	var mu sync.Mutex
	var writes []TrafficClass
	s := NewScheduler(func(class TrafficClass, _ Kind, _ []byte) error {
		mu.Lock()
		writes = append(writes, class)
		n := len(writes)
		mu.Unlock()
		if n == 1 {
			close(entered)
			<-gate
		}
		return nil
	})
	t.Cleanup(func() { _ = s.Close() })
	first := asyncSend(s, context.Background(), ClassControl, 0)
	<-entered
	var sends []<-chan error
	for i := 0; i < 15; i++ {
		sends = append(sends, asyncSend(s, context.Background(), ClassControl, byte(i+1)))
	}
	sends = append(sends, asyncSend(s, context.Background(), ClassBulk, 99))
	waitFor(t, "control and bulk admission", func() bool {
		return queueBytes(s, ClassControl) == 16*testFrameSize && queueBytes(s, ClassBulk) == testFrameSize
	})
	close(gate)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	for _, done := range sends {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	bulkAt := -1
	for i, class := range writes {
		if class == ClassBulk {
			bulkAt = i + 1
			break
		}
	}
	if bulkAt < 1 || bulkAt > 9 {
		t.Fatalf("bulk appeared at position %d, want <= 9", bulkAt)
	}
}

func TestSchedulerBulkCapacityBlocksFifthSenderUntilDrain(t *testing.T) {
	controlGate := make(chan struct{})
	bulkGate := make(chan struct{})
	bulkEntered := make(chan struct{})
	s := NewScheduler(func(class TrafficClass, _ Kind, _ []byte) error {
		if class == ClassControl {
			<-controlGate
		}
		if class == ClassBulk {
			select {
			case <-bulkEntered:
			default:
				close(bulkEntered)
			}
			<-bulkGate
		}
		return nil
	})
	t.Cleanup(func() { _ = s.Close() })
	control := asyncSend(s, context.Background(), ClassControl, 0)
	var sends []<-chan error
	for i := 0; i < 4; i++ {
		sends = append(sends, asyncSend(s, context.Background(), ClassBulk, byte(i+1)))
	}
	waitFor(t, "full bulk queue", func() bool { return queueBytes(s, ClassBulk) == BulkQueueCapBytes })
	fifth := asyncSend(s, context.Background(), ClassBulk, 9)
	time.Sleep(10 * time.Millisecond)
	if got := queueBytes(s, ClassBulk); got != BulkQueueCapBytes {
		t.Fatalf("bulk bytes = %d, want cap", got)
	}
	close(controlGate)
	<-bulkEntered
	select {
	case err := <-fifth:
		t.Fatalf("fifth completed before drain: %v", err)
	default:
	}
	close(bulkGate)
	if err := <-control; err != nil {
		t.Fatal(err)
	}
	for _, done := range sends {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if err := <-fifth; err != nil {
		t.Fatal(err)
	}
}

func TestSchedulerBulkAdmissionWaitersAreFIFO(t *testing.T) {
	permits := make(chan struct{})
	entered := make(chan byte, 8)
	var sends []<-chan error
	s := NewScheduler(func(_ TrafficClass, _ Kind, payload []byte) error { entered <- payload[0]; <-permits; return nil })
	for i := byte(1); i <= 4; i++ {
		sends = append(sends, asyncSend(s, context.Background(), ClassBulk, i))
		want := int(i) * testFrameSize
		waitFor(t, "ordered initial admission", func() bool { return queueBytes(s, ClassBulk) == want })
	}
	if got := <-entered; got != 1 {
		t.Fatalf("first write = %d", got)
	}
	waitFor(t, "full admitted bulk capacity", func() bool { return queueBytes(s, ClassBulk) == BulkQueueCapBytes })
	for i := byte(10); i <= 12; i++ {
		sends = append(sends, asyncSend(s, context.Background(), ClassBulk, i))
		want := int(i - 9)
		waitFor(t, "FIFO admission waiter", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.admissionWaiters[ClassBulk]) == want })
	}
	order := []byte{1}
	for i := 0; i < 6; i++ {
		permits <- struct{}{}
		order = append(order, <-entered)
	}
	permits <- struct{}{}
	for _, done := range sends {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	want := []byte{1, 2, 3, 4, 10, 11, 12}
	if fmt.Sprint(order) != fmt.Sprint(want) {
		t.Fatalf("write order = %v, want %v", order, want)
	}
	_ = s.Close()
}

func TestSchedulerAdmissionWaiterCancellationAndClose(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{})
	s := NewScheduler(func(_ TrafficClass, _ Kind, _ []byte) error {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-gate
		return nil
	})
	var admitted []<-chan error
	for i := 0; i < 4; i++ {
		admitted = append(admitted, asyncSend(s, context.Background(), ClassBulk, byte(i+1)))
	}
	<-entered
	waitFor(t, "full capacity", func() bool { return queueBytes(s, ClassBulk) == BulkQueueCapBytes })
	ctx, cancel := context.WithCancel(context.Background())
	canceled := asyncSend(s, ctx, ClassBulk, 10)
	waiting := asyncSend(s, context.Background(), ClassBulk, 11)
	waitFor(t, "two admission waiters", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.admissionWaiters[ClassBulk]) == 2 })
	cancel()
	if err := <-canceled; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter cancellation = %v", err)
	}
	waitFor(t, "canceled waiter removal", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.admissionWaiters[ClassBulk]) == 1 })
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-waiting; !errors.Is(err, ErrSchedulerClosed) {
		t.Fatalf("waiting close error = %v", err)
	}
	close(gate)
	for _, done := range admitted {
		<-done
	}
}

func TestSchedulerWriterFailureUnblocksAdmissionWaiters(t *testing.T) {
	boom := errors.New("admission writer failure")
	gate := make(chan struct{})
	entered := make(chan struct{})
	s := NewScheduler(func(_ TrafficClass, _ Kind, _ []byte) error {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-gate
		return boom
	})
	for i := 0; i < 4; i++ {
		_ = asyncSend(s, context.Background(), ClassBulk, byte(i+1))
	}
	<-entered
	waitFor(t, "full capacity", func() bool { return queueBytes(s, ClassBulk) == BulkQueueCapBytes })
	waiting := asyncSend(s, context.Background(), ClassBulk, 9)
	waitFor(t, "admission waiter", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.admissionWaiters[ClassBulk]) == 1 })
	close(gate)
	if err := <-waiting; !errors.Is(err, boom) {
		t.Fatalf("waiting writer error = %v", err)
	}
}

func TestSchedulerBlockedPayloadCopiesOnlyWhenAdmitted(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{})
	var mu sync.Mutex
	var markers []byte
	s := NewScheduler(func(_ TrafficClass, _ Kind, payload []byte) error {
		mu.Lock()
		markers = append(markers, payload[0])
		n := len(markers)
		mu.Unlock()
		if n == 1 {
			close(entered)
			<-gate
		}
		return nil
	})
	var sends []<-chan error
	for i := 1; i <= 4; i++ {
		sends = append(sends, asyncSend(s, context.Background(), ClassBulk, byte(i)))
	}
	<-entered
	waitFor(t, "full bulk capacity", func() bool { return queueBytes(s, ClassBulk) == BulkQueueCapBytes })
	original := bytesOf(testFrameSize, 7)
	blocked := make(chan error, 1)
	go func() { blocked <- s.Send(context.Background(), ClassBulk, KindBinary, original) }()
	waitFor(t, "blocked admission", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.admissionWaiters[ClassBulk]) == 1 })
	original[0] = 9
	close(gate)
	for _, done := range sends {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if err := <-blocked; err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	mu.Lock()
	got := markers[len(markers)-1]
	mu.Unlock()
	if got != 9 {
		t.Fatalf("blocked payload marker = %d, want admission-time value 9", got)
	}
}

func TestSchedulerRejectsInvalidKindAndPreCanceledContextBeforeAdmission(t *testing.T) {
	var mu sync.Mutex
	writes := 0
	s := NewScheduler(func(TrafficClass, Kind, []byte) error { mu.Lock(); writes++; mu.Unlock(); return nil })
	if err := s.Send(context.Background(), ClassBulk, Kind(0xff), []byte{1}); err == nil {
		t.Fatal("invalid kind succeeded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Send(ctx, ClassBulk, KindBinary, []byte{1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled Send = %v", err)
	}
	if err := s.Send(context.Background(), ClassBulk, KindBinary, []byte{1}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := writes
	mu.Unlock()
	if got != 1 {
		t.Fatalf("writer calls = %d, want only valid request", got)
	}
	_ = s.Close()
}

func TestSchedulerIdleLanesBorrowAndMediaSublanesAreWeighted(t *testing.T) {
	tests := []struct {
		name    string
		classes []TrafficClass
		want    []TrafficClass
	}{
		{name: "bulk borrows", classes: []TrafficClass{ClassBulk, ClassBulk, ClassBulk, ClassBulk}, want: []TrafficClass{ClassBulk, ClassBulk, ClassBulk, ClassBulk}},
		{name: "media borrows", classes: []TrafficClass{ClassInteractiveMedia, ClassInteractiveMedia, ClassInteractiveMedia, ClassInteractiveMedia}, want: []TrafficClass{ClassInteractiveMedia, ClassInteractiveMedia, ClassInteractiveMedia, ClassInteractiveMedia}},
		{name: "interactive three to thumbnail one", classes: []TrafficClass{ClassInteractiveMedia, ClassThumbnail, ClassInteractiveMedia, ClassThumbnail, ClassInteractiveMedia, ClassInteractiveMedia, ClassInteractiveMedia, ClassInteractiveMedia}, want: []TrafficClass{ClassInteractiveMedia, ClassInteractiveMedia, ClassInteractiveMedia, ClassThumbnail, ClassInteractiveMedia, ClassInteractiveMedia, ClassInteractiveMedia, ClassThumbnail}},
		{name: "thumbnail borrows", classes: []TrafficClass{ClassThumbnail, ClassThumbnail, ClassThumbnail, ClassThumbnail}, want: []TrafficClass{ClassThumbnail, ClassThumbnail, ClassThumbnail, ClassThumbnail}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gate := make(chan struct{})
			entered := make(chan struct{})
			var mu sync.Mutex
			var writes []TrafficClass
			s := NewScheduler(func(class TrafficClass, _ Kind, _ []byte) error {
				mu.Lock()
				writes = append(writes, class)
				n := len(writes)
				mu.Unlock()
				if n == 1 {
					close(entered)
					<-gate
				}
				return nil
			})
			first := asyncSend(s, context.Background(), ClassControl, 0)
			<-entered
			var sends []<-chan error
			for i, class := range tt.classes {
				sends = append(sends, asyncSend(s, context.Background(), class, byte(i+1)))
			}
			waitFor(t, "all requests admitted", func() bool {
				total := queueBytes(s, ClassInteractiveMedia) + queueBytes(s, ClassThumbnail) + queueBytes(s, ClassBulk)
				return total == len(tt.classes)*testFrameSize
			})
			close(gate)
			if err := <-first; err != nil {
				t.Fatal(err)
			}
			for _, done := range sends {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
			_ = s.Close()
			mu.Lock()
			got := append([]TrafficClass(nil), writes[1:]...)
			mu.Unlock()
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Fatalf("service order = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSchedulerCopiesPayloadAndRejectsOversizedRequest(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{})
	var got byte
	s := NewScheduler(func(class TrafficClass, _ Kind, payload []byte) error {
		if class == ClassControl {
			close(entered)
			<-gate
		} else {
			got = payload[0]
		}
		return nil
	})
	t.Cleanup(func() { _ = s.Close() })
	first := asyncSend(s, context.Background(), ClassControl, 0)
	<-entered
	payload := bytesOf(testFrameSize, 7)
	done := make(chan error, 1)
	go func() { done <- s.Send(context.Background(), ClassBulk, KindBinary, payload) }()
	waitFor(t, "payload admission", func() bool { return queueBytes(s, ClassBulk) == testFrameSize })
	payload[0] = 99
	close(gate)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got != 7 {
		t.Fatalf("writer payload[0] = %d, want copied value 7", got)
	}
	if err := s.Send(context.Background(), ClassBulk, KindBinary, make([]byte, BulkQueueCapBytes+1)); !errors.Is(err, ErrRequestTooLarge) {
		t.Fatalf("oversized error = %v", err)
	}
}

func TestSchedulerRejectsEmptyPayloadBeforeAdmissionWithoutPoisoningFairness(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{})
	var mu sync.Mutex
	var writes []TrafficClass
	s := NewScheduler(func(class TrafficClass, _ Kind, payload []byte) error {
		mu.Lock()
		writes = append(writes, class)
		mu.Unlock()
		if len(payload) > 0 && class == ClassControl {
			close(entered)
			<-gate
		}
		return nil
	})
	for _, class := range []TrafficClass{ClassControl, ClassInteractiveMedia, ClassThumbnail, ClassBulk} {
		for _, kind := range []Kind{KindText, KindBinary} {
			if err := s.Send(context.Background(), class, kind, nil); !errors.Is(err, ErrEmptyPayload) {
				t.Fatalf("Send(%d, %d, empty) = %v", class, kind, err)
			}
		}
	}
	s.mu.Lock()
	changed := s.outerStarted || s.innerStarted || s.outerDeficit != [2]int{} || s.innerDeficit != [2]int{}
	for _, class := range []TrafficClass{ClassControl, ClassInteractiveMedia, ClassThumbnail, ClassBulk} {
		changed = changed || s.bytes[class] != 0 || len(s.queues[class]) != 0 || len(s.admissionWaiters[class]) != 0
	}
	if changed {
		t.Fatalf("empty sends changed scheduler state: bytes=%v queues=%v waiters=%v", s.bytes, s.queues, s.admissionWaiters)
	}
	s.mu.Unlock()
	control := asyncSend(s, context.Background(), ClassControl, 1)
	<-entered
	sends := []<-chan error{asyncSend(s, context.Background(), ClassInteractiveMedia, 2), asyncSend(s, context.Background(), ClassInteractiveMedia, 3), asyncSend(s, context.Background(), ClassInteractiveMedia, 4), asyncSend(s, context.Background(), ClassBulk, 5)}
	waitFor(t, "fair traffic admission", func() bool {
		return queueBytes(s, ClassInteractiveMedia) == 3*testFrameSize && queueBytes(s, ClassBulk) == testFrameSize
	})
	close(gate)
	_ = <-control
	for _, done := range sends {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Close()
	mu.Lock()
	got := append([]TrafficClass(nil), writes[1:]...)
	mu.Unlock()
	want := []TrafficClass{ClassInteractiveMedia, ClassInteractiveMedia, ClassInteractiveMedia, ClassBulk}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("post-rejection service = %v, want %v", got, want)
	}
}

func TestSchedulerEmptyMediaCannotStarveBulk(t *testing.T) {
	var calls int
	var got TrafficClass
	s := NewScheduler(func(class TrafficClass, _ Kind, _ []byte) error { calls++; got = class; return nil })
	for i := 0; i < 1000; i++ {
		if err := s.Send(context.Background(), ClassInteractiveMedia, KindBinary, nil); !errors.Is(err, ErrEmptyPayload) {
			t.Fatalf("empty send %d = %v", i, err)
		}
	}
	if err := s.Send(context.Background(), ClassBulk, KindBinary, []byte{1}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || got != ClassBulk {
		t.Fatalf("writes = %d, last class %d; want one bulk", calls, got)
	}
	_ = s.Close()
}

func TestSchedulerCancellationRestoresCapacity(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{})
	s := NewScheduler(func(class TrafficClass, _ Kind, _ []byte) error {
		if class == ClassControl {
			close(entered)
			<-gate
		}
		return nil
	})
	t.Cleanup(func() { _ = s.Close() })
	first := asyncSend(s, context.Background(), ClassControl, 0)
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancelled := asyncSend(s, ctx, ClassBulk, 1)
	var sends []<-chan error
	for i := 0; i < 3; i++ {
		sends = append(sends, asyncSend(s, context.Background(), ClassBulk, byte(i+2)))
	}
	waitFor(t, "full bulk queue", func() bool { return queueBytes(s, ClassBulk) == BulkQueueCapBytes })
	replacement := asyncSend(s, context.Background(), ClassBulk, 9)
	cancel()
	if err := <-cancelled; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
	waitFor(t, "replacement admission", func() bool { return queueBytes(s, ClassBulk) == BulkQueueCapBytes })
	close(gate)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	for _, done := range sends {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if err := <-replacement; err != nil {
		t.Fatal(err)
	}
}

func TestSchedulerCancellationAfterActivationWaitsForWriter(t *testing.T) {
	writerResult := errors.New("writer result")
	active := make(chan struct{})
	release := make(chan struct{})
	s := NewScheduler(func(TrafficClass, Kind, []byte) error {
		close(active)
		<-release
		return writerResult
	})
	t.Cleanup(func() { _ = s.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Send(ctx, ClassBulk, KindBinary, []byte{1}) }()
	<-active
	cancel()
	select {
	case err := <-done:
		close(release)
		t.Fatalf("Send returned while active writer was blocked: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-done; !errors.Is(err, writerResult) {
		t.Fatalf("Send error = %v, want writer result", err)
	}
}

func TestSchedulerWriterFailureAndCloseAreTerminal(t *testing.T) {
	boom := errors.New("boom")
	failureGate := make(chan struct{})
	failureEntered := make(chan struct{})
	s := NewScheduler(func(TrafficClass, Kind, []byte) error { close(failureEntered); <-failureGate; return boom })
	firstFailure := asyncSend(s, context.Background(), ClassControl, 1)
	<-failureEntered
	queuedFailure := asyncSend(s, context.Background(), ClassBulk, 2)
	waitFor(t, "request queued behind failing writer", func() bool { return queueBytes(s, ClassBulk) == testFrameSize })
	close(failureGate)
	if err := <-firstFailure; !errors.Is(err, boom) {
		t.Fatalf("first error = %v", err)
	}
	if err := <-queuedFailure; !errors.Is(err, boom) {
		t.Fatalf("queued error = %v", err)
	}
	if err := s.Send(context.Background(), ClassControl, KindText, []byte{1}); !errors.Is(err, boom) {
		t.Fatalf("future error = %v", err)
	}

	gate := make(chan struct{})
	entered := make(chan struct{})
	s2 := NewScheduler(func(class TrafficClass, _ Kind, _ []byte) error {
		if class == ClassControl {
			close(entered)
			<-gate
		}
		return nil
	})
	first := asyncSend(s2, context.Background(), ClassControl, 0)
	<-entered
	queued := asyncSend(s2, context.Background(), ClassBulk, 1)
	waitFor(t, "queued request", func() bool { return queueBytes(s2, ClassBulk) == testFrameSize })
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-queued; !errors.Is(err, ErrSchedulerClosed) {
		t.Fatalf("queued close error = %v", err)
	}
	if err := s2.Send(context.Background(), ClassBulk, KindBinary, []byte{1}); !errors.Is(err, ErrSchedulerClosed) {
		t.Fatalf("future close error = %v", err)
	}
	if err := s2.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	close(gate)
	_ = <-first
}
