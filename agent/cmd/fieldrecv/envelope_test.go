package main

import (
	"encoding/binary"
	"testing"
)

// encodeTestChunk builds a bulk frame exactly as
// signaling-server/web/src/binaryEnvelope.js encodeChunkEnvelope does.
func encodeTestChunk(t *testing.T, operation uint64, generation uint32, payload []byte) []byte {
	t.Helper()
	out := make([]byte, chunkEnvelopeBytes+len(payload))
	out[0] = frameKindFileChunk
	out[1] = chunkEnvelopeVersion
	binary.BigEndian.PutUint64(out[2:10], operation)
	binary.BigEndian.PutUint32(out[10:14], generation)
	copy(out[chunkEnvelopeBytes:], payload)
	return out
}

func TestFramePayloadLenExactAccounting(t *testing.T) {
	payload := make([]byte, 16384)
	for i := range payload {
		payload[i] = byte(i)
	}
	frame := encodeTestChunk(t, 42, 7, payload)

	got, kind, err := framePayloadLen(frame)
	if err != nil {
		t.Fatalf("framePayloadLen: %v", err)
	}
	if kind != frameKindFileChunk {
		t.Fatalf("kind = 0x%02x, want 0x%02x", kind, frameKindFileChunk)
	}
	if got != len(payload) {
		t.Fatalf("payload len = %d, want %d", got, len(payload))
	}

	// The E28 accounting gate: wire bytes == payload + 14 * frames.
	var payloadBytes, wireBytes, frames int64
	sizes := []int{1, 1200, 16384, 65536, 777}
	for i, size := range sizes {
		f := encodeTestChunk(t, uint64(i+1), 0, make([]byte, size))
		got, _, err := framePayloadLen(f)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		payloadBytes += int64(got)
		wireBytes += int64(len(f))
		frames++
	}
	if wireBytes != payloadBytes+chunkEnvelopeBytes*frames {
		t.Fatalf("accounting mismatch: wire=%d payload=%d frames=%d", wireBytes, payloadBytes, frames)
	}
}

func TestFramePayloadLenThumbnail(t *testing.T) {
	frame := append([]byte{frameKindThumbnail, 0x01, 0x02}, make([]byte, 100)...)
	got, kind, err := framePayloadLen(frame)
	if err != nil {
		t.Fatalf("framePayloadLen: %v", err)
	}
	if kind != frameKindThumbnail || got != 100 {
		t.Fatalf("got kind=0x%02x len=%d, want thumbnail len=100", kind, got)
	}
}

func TestFramePayloadLenRejects(t *testing.T) {
	cases := map[string][]byte{
		"empty":           {},
		"short chunk":     {frameKindFileChunk, chunkEnvelopeVersion, 0, 0},
		"bad version":     append([]byte{frameKindFileChunk, 0x02}, make([]byte, 12)...),
		"zero operation":  append([]byte{frameKindFileChunk, chunkEnvelopeVersion}, make([]byte, 12)...),
		"unknown kind":    {0x99, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13},
		"short thumbnail": {frameKindThumbnail, 0x00},
	}
	for name, data := range cases {
		if _, _, err := framePayloadLen(data); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}
