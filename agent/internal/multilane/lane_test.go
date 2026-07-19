package multilane

import (
	"bytes"
	"testing"
)

func TestProtocolConstantsAreStable(t *testing.T) {
	if ProtocolVersion != 2 {
		t.Fatalf("ProtocolVersion = %d, want 2", ProtocolVersion)
	}

	tests := []struct {
		name string
		lane Lane
		want byte
	}{
		{name: "control", lane: LaneControl, want: 0x00},
		{name: "media", lane: LaneMedia, want: 0x01},
		{name: "bulk", lane: LaneBulk, want: 0x02},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := byte(tt.lane); got != tt.want {
				t.Fatalf("lane ID = %#x, want %#x", got, tt.want)
			}
			if !tt.lane.Valid() {
				t.Fatal("stable lane must be valid")
			}
		})
	}

	if KindText != 0x01 || KindBinary != 0x02 {
		t.Fatalf("kind IDs = (%#x, %#x), want (0x1, 0x2)", KindText, KindBinary)
	}
}

func TestEnvelopeRoundTripCopiesPayload(t *testing.T) {
	original := []byte{0x00, 0x7f, 0xff}
	encoded, err := EncodeEnvelope(LaneMedia, original)
	if err != nil {
		t.Fatalf("EncodeEnvelope: %v", err)
	}
	if want := []byte{0x01, 0x00, 0x7f, 0xff}; !bytes.Equal(encoded, want) {
		t.Fatalf("encoded = %v, want %v", encoded, want)
	}

	original[0] = 0xaa
	if encoded[1] != 0x00 {
		t.Fatal("encoded envelope aliases input payload")
	}

	lane, decoded, err := DecodeEnvelope(encoded)
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}
	if lane != LaneMedia || !bytes.Equal(decoded, []byte{0x00, 0x7f, 0xff}) {
		t.Fatalf("decoded = (%#x, %v), want (%#x, [0 127 255])", lane, decoded, LaneMedia)
	}

	encoded[1] = 0xbb
	if decoded[0] != 0x00 {
		t.Fatal("decoded payload aliases encoded envelope")
	}
}

func TestEnvelopeRejectsInvalidLanes(t *testing.T) {
	for _, lane := range []Lane{0x03, 0xff} {
		if lane.Valid() {
			t.Fatalf("Lane(%#x).Valid() = true", lane)
		}
		if _, err := EncodeEnvelope(lane, []byte{1}); err == nil {
			t.Fatalf("EncodeEnvelope(%#x) succeeded", lane)
		}
		if _, _, err := DecodeEnvelope([]byte{byte(lane), 1}); err == nil {
			t.Fatalf("DecodeEnvelope(%#x) succeeded", lane)
		}
	}
}

func TestDecodeEnvelopeRejectsEmptyEnvelope(t *testing.T) {
	if _, _, err := DecodeEnvelope(nil); err == nil {
		t.Fatal("DecodeEnvelope(nil) succeeded")
	}
}

func TestLaneForClass(t *testing.T) {
	tests := []struct {
		class TrafficClass
		want  Lane
	}{
		{class: ClassControl, want: LaneControl},
		{class: ClassInteractiveMedia, want: LaneMedia},
		{class: ClassThumbnail, want: LaneMedia},
		{class: ClassBulk, want: LaneBulk},
	}
	for _, tt := range tests {
		got, err := LaneForClass(tt.class)
		if err != nil {
			t.Fatalf("LaneForClass(%d): %v", tt.class, err)
		}
		if got != tt.want {
			t.Fatalf("LaneForClass(%d) = %#x, want %#x", tt.class, got, tt.want)
		}
	}

	if _, err := LaneForClass(TrafficClass(0xff)); err == nil {
		t.Fatal("LaneForClass(unknown) succeeded")
	}
}
