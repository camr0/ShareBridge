// agent/internal/relaychannel/frame_test.go
package relaychannel

import (
	"strings"
	"testing"
)

func TestWriteAndDecodeFrame(t *testing.T) {
	encoded, err := WriteFrame(FrameText, []byte("hello"))
	if err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	frame, rest, err := DecodeOneFrame(encoded)
	if err != nil {
		t.Fatalf("DecodeOneFrame: %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("expected no trailing bytes, got %d", len(rest))
	}
	if frame.Kind != FrameText || string(frame.Payload) != "hello" {
		t.Fatalf("decoded frame = %+v", frame)
	}
}

func TestDecodeOneFrame_RejectsUnknownKind(t *testing.T) {
	_, _, err := DecodeOneFrame([]byte{0xff, 0, 0, 0, 0})
	if err == nil || !strings.Contains(err.Error(), "unknown frame kind") {
		t.Fatalf("expected unknown frame kind error, got %v", err)
	}
}

func TestWriteFrame_RejectsUnknownKind(t *testing.T) {
	_, err := WriteFrame(0xff, []byte("test"))
	if err == nil || !strings.Contains(err.Error(), "unknown frame kind") {
		t.Fatalf("expected unknown frame kind error, got %v", err)
	}
}

func TestWriteFrame_RespectsMaxHandshakePayload(t *testing.T) {
	// Should succeed at limit
	_, err := WriteFrame(FrameHandshake, make([]byte, MaxHandshakePayload))
	if err != nil {
		t.Fatalf("WriteFrame at limit failed: %v", err)
	}

	// Should fail over limit
	_, err = WriteFrame(FrameHandshake, make([]byte, MaxHandshakePayload+1))
	if err == nil || !strings.Contains(err.Error(), "frame too large") {
		t.Fatalf("expected frame too large error, got %v", err)
	}
}

func TestWriteFrame_RespectsMaxFramePayload(t *testing.T) {
	// Should succeed at limit
	_, err := WriteFrame(FrameText, make([]byte, MaxFramePayload))
	if err != nil {
		t.Fatalf("WriteFrame at limit failed: %v", err)
	}

	// Should fail over limit
	_, err = WriteFrame(FrameText, make([]byte, MaxFramePayload+1))
	if err == nil || !strings.Contains(err.Error(), "frame too large") {
		t.Fatalf("expected frame too large error, got %v", err)
	}
}

func TestDecodeOneFrame_IncompleteHeader(t *testing.T) {
	_, _, err := DecodeOneFrame([]byte{0x01, 0, 0, 0})
	if err == nil || !strings.Contains(err.Error(), "incomplete frame header") {
		t.Fatalf("expected incomplete frame header error, got %v", err)
	}
}

func TestDecodeOneFrame_IncompletePayload(t *testing.T) {
	// Header says 10 bytes, but only 3 provided
	_, _, err := DecodeOneFrame([]byte{0x01, 0, 0, 0, 10, 'a', 'b', 'c'})
	if err == nil || !strings.Contains(err.Error(), "incomplete frame payload") {
		t.Fatalf("expected incomplete frame payload error, got %v", err)
	}
}

func TestDecodeOneFrame_ReturnsRest(t *testing.T) {
	encoded, _ := WriteFrame(FrameText, []byte("hello"))
	buf := append(encoded, 0x01, 0x02, 0x03)
	frame, rest, err := DecodeOneFrame(buf)
	if err != nil {
		t.Fatalf("DecodeOneFrame: %v", err)
	}
	if string(frame.Payload) != "hello" {
		t.Fatalf("unexpected payload: %s", frame.Payload)
	}
	if len(rest) != 3 {
		t.Fatalf("expected 3 trailing bytes, got %d", len(rest))
	}
}

func TestWriteFrame_BinaryKind(t *testing.T) {
	data := []byte{0x00, 0x01, 0x02, 0xff, 0xfe}
	encoded, err := WriteFrame(FrameBinary, data)
	if err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	frame, _, err := DecodeOneFrame(encoded)
	if err != nil {
		t.Fatalf("DecodeOneFrame: %v", err)
	}
	if frame.Kind != FrameBinary {
		t.Fatalf("expected FrameBinary, got %d", frame.Kind)
	}
	if string(frame.Payload) != string(data) {
		t.Fatalf("payload mismatch")
	}
}

func TestWriteFrame_HandshakeKind(t *testing.T) {
	data := make([]byte, 100)
	encoded, err := WriteFrame(FrameHandshake, data)
	if err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	frame, _, err := DecodeOneFrame(encoded)
	if err != nil {
		t.Fatalf("DecodeOneFrame: %v", err)
	}
	if frame.Kind != FrameHandshake {
		t.Fatalf("expected FrameHandshake, got %d", frame.Kind)
	}
}

func TestDecodeOneFrame_PayloadExceedsCap(t *testing.T) {
	// Manually craft a frame with handshake kind but length > MaxHandshakePayload
	buf := make([]byte, 5+100)
	buf[0] = FrameHandshake
	binaryBigEndianPutUint32(buf[1:5], MaxHandshakePayload+1)
	_, _, err := DecodeOneFrame(buf)
	if err == nil || !strings.Contains(err.Error(), "frame too large") {
		t.Fatalf("expected frame too large error, got %v", err)
	}
}

func TestWriteFrame_EmptyPayload(t *testing.T) {
	encoded, err := WriteFrame(FrameText, []byte{})
	if err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if len(encoded) != 5 {
		t.Fatalf("expected 5 bytes for empty payload, got %d", len(encoded))
	}
	frame, _, err := DecodeOneFrame(encoded)
	if err != nil {
		t.Fatalf("DecodeOneFrame: %v", err)
	}
	if len(frame.Payload) != 0 {
		t.Fatalf("expected empty payload, got %d bytes", len(frame.Payload))
	}
}

// Helper for binary big-endian put uint32 (for crafting malformed frames in tests)
func binaryBigEndianPutUint32(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}