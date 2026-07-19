package multilane

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

type testEndpoint struct {
	onMessage func([]byte)
	texts     []string
	sendErr   error
}

func (endpoint *testEndpoint) SendText(text string) error {
	endpoint.texts = append(endpoint.texts, text)
	return endpoint.sendErr
}

func (*testEndpoint) SendBinary([]byte) error                    { return nil }
func (*testEndpoint) SendBinaryClass(TrafficClass, []byte) error { return nil }
func (*testEndpoint) BufferedAmount() uint64                     { return 0 }
func (endpoint *testEndpoint) SetOnMessage(handler func([]byte)) { endpoint.onMessage = handler }

type testChannelSet struct {
	endpoints map[Lane]*testEndpoint
	onOpen    func()
	onClose   func()
	closed    int
}

func newTestChannelSet() *testChannelSet {
	return &testChannelSet{endpoints: map[Lane]*testEndpoint{
		LaneControl: {}, LaneMedia: {}, LaneBulk: {},
	}}
}

func (set *testChannelSet) Endpoint(lane Lane) Endpoint { return set.endpoints[lane] }
func (set *testChannelSet) SetOnOpen(handler func())    { set.onOpen = handler }
func (set *testChannelSet) SetOnClose(handler func())   { set.onClose = handler }
func (set *testChannelSet) Close() error                { set.closed++; return nil }

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

func TestInstallHandshakeResponderAcknowledgesVersionTwo(t *testing.T) {
	set := newTestChannelSet()
	readyCalls := 0
	InstallHandshakeResponder(set, func() { readyCalls++ })

	set.endpoints[LaneControl].onMessage([]byte(`{"type":"transport_hello","version":2}`))

	if readyCalls != 1 {
		t.Fatalf("onReady calls = %d, want 1", readyCalls)
	}
	if set.closed != 0 {
		t.Fatalf("Close calls = %d, want 0", set.closed)
	}
	if len(set.endpoints[LaneControl].texts) != 1 {
		t.Fatalf("control sends = %d, want 1", len(set.endpoints[LaneControl].texts))
	}
	var response struct {
		Type    string `json:"type"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal([]byte(set.endpoints[LaneControl].texts[0]), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Type != "transport_ready" || response.Version != ProtocolVersion {
		t.Fatalf("response = %+v", response)
	}
	if set.endpoints[LaneControl].onMessage != nil {
		t.Fatal("handshake handler remains installed during onReady handoff")
	}
}

func TestInstallHandshakeResponderRejectsInvalidFirstMessage(t *testing.T) {
	tests := []struct {
		name    string
		message []byte
	}{
		{name: "malformed", message: []byte(`not json`)},
		{name: "wrong type", message: []byte(`{"type":"file_request","version":2}`)},
		{name: "wrong version", message: []byte(`{"type":"transport_hello","version":1}`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set := newTestChannelSet()
			readyCalls := 0
			InstallHandshakeResponder(set, func() { readyCalls++ })
			set.endpoints[LaneControl].onMessage(tt.message)
			if set.closed != 1 {
				t.Fatalf("Close calls = %d, want 1", set.closed)
			}
			if readyCalls != 0 {
				t.Fatalf("onReady calls = %d, want 0", readyCalls)
			}
		})
	}
}

func TestInstallHandshakeResponderReportsIncompatibleVersion(t *testing.T) {
	set := newTestChannelSet()
	InstallHandshakeResponder(set, nil)

	set.endpoints[LaneControl].onMessage([]byte(`{"type":"transport_hello","version":1}`))

	if len(set.endpoints[LaneControl].texts) != 1 {
		t.Fatalf("control sends = %d, want 1", len(set.endpoints[LaneControl].texts))
	}
	var response struct {
		Type    string `json:"type"`
		Scope   string `json:"scope"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(set.endpoints[LaneControl].texts[0]), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Type != "error" || response.Scope != "connection" || response.Message != "incompatible transport version: 1" {
		t.Fatalf("response = %+v", response)
	}
}

func TestInstallHandshakeResponderClosesWhenAcknowledgementFails(t *testing.T) {
	set := newTestChannelSet()
	set.endpoints[LaneControl].sendErr = errors.New("send failed")
	readyCalls := 0
	InstallHandshakeResponder(set, func() { readyCalls++ })

	set.endpoints[LaneControl].onMessage([]byte(`{"type":"transport_hello","version":2}`))

	if set.closed != 1 || readyCalls != 0 {
		t.Fatalf("Close calls = %d, onReady calls = %d; want 1, 0", set.closed, readyCalls)
	}
}
