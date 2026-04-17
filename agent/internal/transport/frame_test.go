package transport

import (
	"bytes"
	"io"
	"testing"
)

func TestFrame_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, FrameText, []byte("hello")); err != nil {
		t.Fatalf("WriteFrame text: %v", err)
	}
	if err := WriteFrame(&buf, FrameBinary, []byte{0xde, 0xad, 0xbe, 0xef}); err != nil {
		t.Fatalf("WriteFrame binary: %v", err)
	}

	kind, payload, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame 1: %v", err)
	}
	if kind != FrameText || string(payload) != "hello" {
		t.Fatalf("frame 1 mismatch: kind=%d payload=%q", kind, payload)
	}

	kind, payload, err = ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame 2: %v", err)
	}
	if kind != FrameBinary || !bytes.Equal(payload, []byte{0xde, 0xad, 0xbe, 0xef}) {
		t.Fatalf("frame 2 mismatch: kind=%d payload=%x", kind, payload)
	}
}

func TestFrame_RejectsOversized(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(byte(FrameBinary))
	buf.Write([]byte{0xff, 0xff, 0xff, 0xff}) // 4 GiB claimed length
	_, _, err := ReadFrame(&buf)
	if err == nil {
		t.Fatal("expected oversized frame to be rejected")
	}
}

func TestFrame_RejectsUnknownKind(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(0x99)
	buf.Write([]byte{0x00, 0x00, 0x00, 0x00})
	_, _, err := ReadFrame(&buf)
	if err == nil {
		t.Fatal("expected unknown kind to be rejected")
	}
}

func TestFrame_EOFBeforeHeader(t *testing.T) {
	var buf bytes.Buffer
	_, _, err := ReadFrame(&buf)
	if err != io.EOF {
		t.Fatalf("expected io.EOF at stream end, got %v", err)
	}
}