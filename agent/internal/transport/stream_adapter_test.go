package transport

import (
	"bytes"
	"io"
	"testing"
)

// readWriteCloser is a minimal stand-in for a network.Stream.
type readWriteCloser struct {
	r      io.Reader
	w      *bytes.Buffer
	closed bool
}

func (rw *readWriteCloser) Read(p []byte) (int, error)  { return rw.r.Read(p) }
func (rw *readWriteCloser) Write(p []byte) (int, error) { return rw.w.Write(p) }
func (rw *readWriteCloser) Close() error                { rw.closed = true; return nil }

func TestStreamAdapter_SendText(t *testing.T) {
	rw := &readWriteCloser{r: bytes.NewReader(nil), w: &bytes.Buffer{}}
	a := NewStreamAdapter(rw)

	if err := a.SendText(`{"type":"hello"}`); err != nil {
		t.Fatalf("SendText: %v", err)
	}

	kind, payload, err := ReadFrame(rw.w)
	if err != nil {
		t.Fatalf("decode written frame: %v", err)
	}
	if kind != FrameText || string(payload) != `{"type":"hello"}` {
		t.Fatalf("wrong frame written: kind=%d payload=%q", kind, payload)
	}
}

func TestStreamAdapter_SendBinary(t *testing.T) {
	rw := &readWriteCloser{r: bytes.NewReader(nil), w: &bytes.Buffer{}}
	a := NewStreamAdapter(rw)

	if err := a.SendBinary([]byte{0x01, 0x02, 0x03}); err != nil {
		t.Fatalf("SendBinary: %v", err)
	}

	kind, payload, err := ReadFrame(rw.w)
	if err != nil {
		t.Fatalf("decode written frame: %v", err)
	}
	if kind != FrameBinary || !bytes.Equal(payload, []byte{0x01, 0x02, 0x03}) {
		t.Fatalf("wrong frame written: kind=%d payload=%x", kind, payload)
	}
}

func TestStreamAdapter_BufferedAmountAlwaysZero(t *testing.T) {
	a := NewStreamAdapter(&readWriteCloser{r: bytes.NewReader(nil), w: &bytes.Buffer{}})
	if got := a.BufferedAmount(); got != 0 {
		t.Fatalf("BufferedAmount = %d, want 0", got)
	}
}

func TestStreamAdapter_Close(t *testing.T) {
	rw := &readWriteCloser{r: bytes.NewReader(nil), w: &bytes.Buffer{}}
	a := NewStreamAdapter(rw)
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !rw.closed {
		t.Fatal("underlying stream not closed")
	}
}