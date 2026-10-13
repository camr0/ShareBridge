package bulkfan

import (
	"errors"
	"testing"
)

func pushAll(t *testing.T, r *ReorderBuffer, seq uint64, payload string) [][]byte {
	t.Helper()
	out, err := r.Push(seq, []byte(payload))
	if err != nil {
		t.Fatalf("Push(%d, %q): %v", seq, payload, err)
	}
	return out
}

func collect(out [][]byte) []string {
	got := make([]string, len(out))
	for i, item := range out {
		got[i] = string(item)
	}
	return got
}

func TestReorderInOrderPassesThrough(t *testing.T) {
	r := NewReorderBuffer()
	for i := uint64(0); i < 3; i++ {
		out := pushAll(t, r, i, string(rune('a'+i)))
		if len(out) != 1 || string(out[0]) != string(rune('a'+i)) {
			t.Fatalf("Push(%d) emitted %v, want one chunk", i, collect(out))
		}
	}
	if r.Pending() != 0 {
		t.Fatalf("Pending() = %d, want 0", r.Pending())
	}
	if r.Next() != 3 {
		t.Fatalf("Next() = %d, want 3", r.Next())
	}
}

func TestReorderBuffersUntilGapFilled(t *testing.T) {
	r := NewReorderBuffer()
	if out := pushAll(t, r, 1, "b"); out != nil {
		t.Fatalf("Push(1) emitted %v, want nil", collect(out))
	}
	if out := pushAll(t, r, 2, "c"); out != nil {
		t.Fatalf("Push(2) emitted %v, want nil", collect(out))
	}
	if r.Pending() != 2 {
		t.Fatalf("Pending() = %d, want 2", r.Pending())
	}
	if r.Next() != 0 {
		t.Fatalf("Next() = %d, want 0 (still waiting for the first chunk)", r.Next())
	}

	out := pushAll(t, r, 0, "a")
	want := []string{"a", "b", "c"}
	if len(out) != len(want) {
		t.Fatalf("filling the gap emitted %v, want %v", collect(out), want)
	}
	for i := range want {
		if string(out[i]) != want[i] {
			t.Fatalf("out[%d] = %q, want %q", i, out[i], want[i])
		}
	}
	if r.Pending() != 0 {
		t.Fatalf("Pending() = %d, want 0 after drain", r.Pending())
	}
	if r.Next() != 3 {
		t.Fatalf("Next() = %d, want 3", r.Next())
	}
}

func TestReorderDropsDuplicatesAndLateChunks(t *testing.T) {
	r := NewReorderBuffer()
	pushAll(t, r, 0, "a")
	pushAll(t, r, 1, "b")
	if out := pushAll(t, r, 1, "b-again"); out != nil {
		t.Fatalf("late duplicate emitted %v, want nil", collect(out))
	}
	// A duplicate of a chunk still buffered is also dropped, keeping the first.
	pushAll(t, r, 3, "d")
	if out := pushAll(t, r, 3, "d-again"); out != nil {
		t.Fatalf("buffered duplicate emitted %v, want nil", collect(out))
	}
	out := pushAll(t, r, 2, "c")
	if len(out) != 2 || string(out[0]) != "c" || string(out[1]) != "d" {
		t.Fatalf("drain emitted %v, want [c d]", collect(out))
	}
	if r.Dropped() != 2 {
		t.Fatalf("Dropped() = %d, want 2", r.Dropped())
	}
}

func TestReorderRequiresSequenceZeroFirst(t *testing.T) {
	r := NewReorderBuffer()
	// A stream always starts at 0; an early 5 must wait for the gap to close.
	if out := pushAll(t, r, 5, "f"); out != nil {
		t.Fatalf("Push(5) emitted %v, want nil (waiting for 0)", collect(out))
	}
	if r.Pending() != 1 {
		t.Fatalf("Pending() = %d, want 1", r.Pending())
	}
	out := pushAll(t, r, 0, "a")
	if len(out) != 1 || string(out[0]) != "a" {
		t.Fatalf("Push(0) emitted %v, want [a] (5 is not consecutive yet)", collect(out))
	}
}

func TestReorderOverflowReportsError(t *testing.T) {
	r := NewReorderBufferWithLimit(2)
	pushAll(t, r, 1, "b")
	pushAll(t, r, 2, "c")
	if _, err := r.Push(3, []byte("d")); !errors.Is(err, ErrReorderOverflow) {
		t.Fatalf("Push beyond limit error = %v, want ErrReorderOverflow", err)
	}
	// The overflow must not corrupt the buffer: filling the gap still drains.
	out := pushAll(t, r, 0, "a")
	if len(out) != 3 || string(out[2]) != "c" {
		t.Fatalf("drain after overflow = %v, want [a b c]", collect(out))
	}
}

func TestReorderReset(t *testing.T) {
	r := NewReorderBufferWithLimit(4)
	pushAll(t, r, 3, "d")
	if r.Pending() != 1 {
		t.Fatalf("Pending() = %d, want 1", r.Pending())
	}
	r.Reset()
	if r.Pending() != 0 || r.Next() != 0 || r.Dropped() != 0 {
		t.Fatalf("after Reset: pending=%d next=%d dropped=%d, want all zero", r.Pending(), r.Next(), r.Dropped())
	}
	out := pushAll(t, r, 0, "a")
	if len(out) != 1 {
		t.Fatalf("after Reset, Push(0) emitted %v, want [a]", collect(out))
	}
}

func TestReorderCopiesBufferedPayload(t *testing.T) {
	r := NewReorderBuffer()
	buf := []byte("payload")
	pushAll(t, r, 1, "b")
	if _, err := r.Push(0, buf); err != nil {
		t.Fatal(err)
	}
	buf[0] = 'X'
	// The buffered out-of-order chunk must be unaffected by later mutation of
	// the slice handed to Push for a different sequence.
	out := pushAll(t, r, 2, "c")
	if string(out[0]) != "c" {
		t.Fatalf("out[0] = %q, want c", out[0])
	}
}
