package bulkfan

import (
	"errors"
	"testing"
)

type fakeSink struct {
	sent     [][]byte
	buffered uint64
	err      error
}

func (s *fakeSink) Send(data []byte) error {
	if s.err != nil {
		return s.err
	}
	s.sent = append(s.sent, append([]byte(nil), data...))
	return nil
}

func (s *fakeSink) BufferedAmount() uint64 { return s.buffered }

func TestNewFanoutRequiresTwoSinks(t *testing.T) {
	if got := NewFanout(nil); got != nil {
		t.Fatalf("NewFanout(nil) = %v, want nil", got)
	}
	if got := NewFanout([]Sink{&fakeSink{}}); got != nil {
		t.Fatalf("NewFanout(one) = %v, want nil (single lane path)", got)
	}
}

func TestFanoutRoundRobinsInOrder(t *testing.T) {
	sinks := []Sink{&fakeSink{}, &fakeSink{}, &fakeSink{}}
	f := NewFanout(sinks)
	if f == nil {
		t.Fatal("NewFanout returned nil for three sinks")
	}
	if f.Len() != 3 {
		t.Fatalf("Len() = %d, want 3", f.Len())
	}

	wantIndex := []int{0, 1, 2, 0, 1, 2, 0}
	for i, want := range wantIndex {
		index, seq := f.Next()
		if index != want {
			t.Fatalf("Next() #%d index = %d, want %d", i, index, want)
		}
		if seq != uint64(i) {
			t.Fatalf("Next() #%d seq = %d, want %d", i, seq, uint64(i))
		}
		if err := f.SendOn(index, []byte{byte(i)}); err != nil {
			t.Fatalf("SendOn(%d): %v", index, err)
		}
	}

	for i, sink := range sinks {
		if len(sink.(*fakeSink).sent) == 0 {
			t.Fatalf("sink %d received nothing", i)
		}
	}
	// Sink 0 got chunks 0, 3, 6.
	got := sinks[0].(*fakeSink).sent
	if len(got) != 3 || got[0][0] != 0 || got[1][0] != 3 || got[2][0] != 6 {
		t.Fatalf("sink 0 received %v, want chunk ids 0,3,6", got)
	}
}

func TestFanoutGlobalSequenceIsStrictlyIncreasing(t *testing.T) {
	f := NewFanout([]Sink{&fakeSink{}, &fakeSink{}, &fakeSink{}, &fakeSink{}})
	var last uint64
	for i := 0; i < 100; i++ {
		_, seq := f.Next()
		if i > 0 && seq != last+1 {
			t.Fatalf("seq #%d = %d, want %d", i, seq, last+1)
		}
		last = seq
	}
}

func TestFanoutBufferedAmountIsMax(t *testing.T) {
	a := &fakeSink{buffered: 10}
	b := &fakeSink{buffered: 50}
	c := &fakeSink{buffered: 30}
	f := NewFanout([]Sink{a, b, c})
	if got := f.BufferedAmount(); got != 50 {
		t.Fatalf("BufferedAmount() = %d, want 50 (deepest queue)", got)
	}
	b.buffered = 5
	if got := f.BufferedAmount(); got != 30 {
		t.Fatalf("BufferedAmount() = %d, want 30 after sink 1 drained", got)
	}
}

func TestFanoutSendOnPropagatesError(t *testing.T) {
	boom := errors.New("boom")
	f := NewFanout([]Sink{&fakeSink{}, &fakeSink{err: boom}})
	index, _ := f.Next()
	if index != 0 {
		t.Fatalf("first index = %d, want 0", index)
	}
	index, _ = f.Next()
	if index != 1 {
		t.Fatalf("second index = %d, want 1", index)
	}
	if err := f.SendOn(index, []byte("x")); !errors.Is(err, boom) {
		t.Fatalf("SendOn error = %v, want %v", err, boom)
	}
}
