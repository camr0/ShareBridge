// Package multilane defines the wire-level lane protocol shared by transports.
package multilane

import "fmt"

const ProtocolVersion = 2

// Lane identifies a logical transport lane on the wire.
type Lane byte

const (
	LaneControl Lane = 0x00
	LaneMedia   Lane = 0x01
	LaneBulk    Lane = 0x02
)

// Valid reports whether the lane is assigned by this protocol version.
func (lane Lane) Valid() bool {
	switch lane {
	case LaneControl, LaneMedia, LaneBulk:
		return true
	default:
		return false
	}
}

// EncodeEnvelope prefixes a copied payload with its lane ID.
func EncodeEnvelope(lane Lane, payload []byte) ([]byte, error) {
	if !lane.Valid() {
		return nil, fmt.Errorf("invalid lane: %#x", byte(lane))
	}

	encoded := make([]byte, len(payload)+1)
	encoded[0] = byte(lane)
	copy(encoded[1:], payload)
	return encoded, nil
}

// DecodeEnvelope separates a lane ID from a copied payload.
func DecodeEnvelope(encoded []byte) (Lane, []byte, error) {
	if len(encoded) == 0 {
		return 0, nil, fmt.Errorf("empty lane envelope")
	}

	lane := Lane(encoded[0])
	if !lane.Valid() {
		return 0, nil, fmt.Errorf("invalid lane: %#x", encoded[0])
	}

	payload := make([]byte, len(encoded)-1)
	copy(payload, encoded[1:])
	return lane, payload, nil
}

// Kind identifies the application payload representation.
type Kind byte

const (
	KindText   Kind = 0x01
	KindBinary Kind = 0x02
)

// TrafficClass describes the delivery needs of a payload.
type TrafficClass byte

const (
	ClassControl TrafficClass = iota
	ClassInteractiveMedia
	ClassThumbnail
	ClassBulk
)

// LaneForClass maps an application traffic class to its logical lane.
func LaneForClass(class TrafficClass) (Lane, error) {
	switch class {
	case ClassControl:
		return LaneControl, nil
	case ClassInteractiveMedia, ClassThumbnail:
		return LaneMedia, nil
	case ClassBulk:
		return LaneBulk, nil
	default:
		return 0, fmt.Errorf("unknown traffic class: %d", class)
	}
}
