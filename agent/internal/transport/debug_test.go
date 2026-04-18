package transport

import (
	"reflect"
	"testing"
	"time"
)

type fakeDebugSession struct {
	rtt int64
}

type fakeDebugInnerStream struct {
	sendWindow uint32
	recvWindow uint32
	id         uint32
	session    *fakeDebugSession
}

type fakeDebugWrapper struct {
	stream *fakeDebugInnerStream
}

func TestDebugTransportEnabled(t *testing.T) {
	t.Setenv("SHAREBRIDGE_DEBUG_TRANSPORT", "1")
	if !DebugTransportEnabled() {
		t.Fatal("DebugTransportEnabled() = false, want true")
	}

	t.Setenv("SHAREBRIDGE_DEBUG_TRANSPORT", "false")
	if DebugTransportEnabled() {
		t.Fatal("DebugTransportEnabled() = true, want false")
	}
}

func TestDebugYamuxStateFromValue(t *testing.T) {
	v := reflect.ValueOf(&fakeDebugWrapper{
		stream: &fakeDebugInnerStream{
			sendWindow: 1234,
			recvWindow: 5678,
			id:         42,
			session:    &fakeDebugSession{rtt: int64((125 * time.Millisecond).Nanoseconds())},
		},
	})

	state, ok := debugYamuxStateFromValue(v)
	if !ok {
		t.Fatal("debugYamuxStateFromValue() = !ok, want ok")
	}
	if state.StreamID != 42 {
		t.Fatalf("StreamID = %d, want 42", state.StreamID)
	}
	if state.SendWindow != 1234 {
		t.Fatalf("SendWindow = %d, want 1234", state.SendWindow)
	}
	if state.RecvWindow != 5678 {
		t.Fatalf("RecvWindow = %d, want 5678", state.RecvWindow)
	}
	if state.RTT != 125*time.Millisecond {
		t.Fatalf("RTT = %s, want %s", state.RTT, 125*time.Millisecond)
	}
}

func TestDebugYamuxStateFromValueReturnsFalseForUnrelatedValue(t *testing.T) {
	type unrelated struct {
		value string
	}

	if _, ok := debugYamuxStateFromValue(reflect.ValueOf(&unrelated{value: "nope"})); ok {
		t.Fatal("debugYamuxStateFromValue() = ok, want false")
	}
}
