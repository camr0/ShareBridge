package bulkfan

import (
	"bytes"
	"testing"
)

func TestEncodeChunkFrameV2RoundTrip(t *testing.T) {
	payload := []byte("hello chunk")
	frame := EncodeChunkFrame(42, 7, 1234, payload)

	if frame[0] != ChunkFrameKind {
		t.Fatalf("kind = 0x%02x, want 0x%02x", frame[0], ChunkFrameKind)
	}
	if frame[1] != ChunkEnvelopeV2 {
		t.Fatalf("version = 0x%02x, want v2", frame[1])
	}
	if len(frame) != ChunkEnvelopeV2Len+len(payload) {
		t.Fatalf("len = %d, want %d", len(frame), ChunkEnvelopeV2Len+len(payload))
	}

	operation, generation, seq, got, err := DecodeChunkFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if operation != 42 || generation != 7 || seq != 1234 {
		t.Fatalf("decoded (op=%d gen=%d seq=%d), want (42,7,1234)", operation, generation, seq)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload = %q, want %q", got, payload)
	}
}

func TestDecodeChunkFrameAcceptsV1(t *testing.T) {
	// Byte-for-byte the agent's original 14-byte envelope.
	frame := make([]byte, ChunkEnvelopeV1Len)
	frame[0] = ChunkFrameKind
	frame[1] = ChunkEnvelopeV1
	frame[2] = 0
	frame[9] = 5 // operation 5
	frame[10] = 0
	frame[13] = 2 // generation 2
	frame = append(frame, []byte("v1 payload")...)

	operation, generation, seq, payload, err := DecodeChunkFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if operation != 5 || generation != 2 || seq != 0 {
		t.Fatalf("decoded (op=%d gen=%d seq=%d), want (5,2,0)", operation, generation, seq)
	}
	if string(payload) != "v1 payload" {
		t.Fatalf("payload = %q, want %q", payload, "v1 payload")
	}
}

func TestDecodeChunkFrameRejectsMalformed(t *testing.T) {
	cases := []struct {
		name  string
		frame []byte
	}{
		{"empty", nil},
		{"short", []byte{ChunkFrameKind, ChunkEnvelopeV1, 0}},
		{"wrong kind", append([]byte{0x11}, make([]byte, ChunkEnvelopeV1Len-1)...)},
		{"unknown version", append([]byte{ChunkFrameKind, 0x09}, make([]byte, ChunkEnvelopeV1Len-2)...)},
		{"v2 short", append([]byte{ChunkFrameKind, ChunkEnvelopeV2}, make([]byte, ChunkEnvelopeV1Len-2)...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, _, err := DecodeChunkFrame(tc.frame); err == nil {
				t.Fatalf("DecodeChunkFrame(%v) returned no error", tc.frame)
			}
		})
	}
}

func TestEnvelopeVersion(t *testing.T) {
	frame := EncodeChunkFrame(1, 0, 0, nil)
	version, err := EnvelopeVersion(frame)
	if err != nil || version != ChunkEnvelopeV2 {
		t.Fatalf("EnvelopeVersion = (0x%02x, %v), want (v2, nil)", version, err)
	}
	if _, err := EnvelopeVersion([]byte{0x11, 0x01}); err == nil {
		t.Fatal("EnvelopeVersion accepted a non-chunk frame")
	}
}
