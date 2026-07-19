// Package multilane defines the wire-level lane protocol shared by transports.
package multilane

import (
	"encoding/json"
	"fmt"
	"sync"
)

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

// Endpoint is one logical transport lane. Implementations may be backed by a
// WebRTC DataChannel or by a multiplexed relay connection.
type Endpoint interface {
	SendText(string) error
	SendBinary([]byte) error
	// SendBinaryClass preserves transport-neutral media sub-priority. The
	// requested class must map to this endpoint's lane.
	SendBinaryClass(TrafficClass, []byte) error
	BufferedAmount() uint64
	SetOnMessage(func([]byte))
}

// ChannelSet owns the three required logical transport lanes as one lifecycle.
type ChannelSet interface {
	Endpoint(Lane) Endpoint
	SetOnOpen(func())
	SetOnClose(func())
	Close() error
}

type transportHandshake struct {
	Type    string `json:"type"`
	Version int    `json:"version"`
}

type transportError struct {
	Type    string `json:"type"`
	Scope   string `json:"scope"`
	Message string `json:"message"`
}

// InstallHandshakeResponder reserves the first control message for transport
// version negotiation. onReady is responsible for installing the application
// control handler after the transport_ready acknowledgement has been sent.
func InstallHandshakeResponder(set ChannelSet, onReady func()) {
	if set == nil {
		return
	}
	control := set.Endpoint(LaneControl)
	if control == nil {
		_ = set.Close()
		return
	}

	var first sync.Once
	control.SetOnMessage(func(data []byte) {
		first.Do(func() {
			var hello transportHandshake
			if err := json.Unmarshal(data, &hello); err != nil || hello.Type != "transport_hello" {
				sendHandshakeError(control, "expected transport_hello version 2")
				_ = set.Close()
				return
			}
			if hello.Version != ProtocolVersion {
				sendHandshakeError(control, fmt.Sprintf("incompatible transport version: %d", hello.Version))
				_ = set.Close()
				return
			}

			response, err := json.Marshal(transportHandshake{
				Type:    "transport_ready",
				Version: ProtocolVersion,
			})
			if err != nil || control.SendText(string(response)) != nil {
				_ = set.Close()
				return
			}

			control.SetOnMessage(nil)
			if onReady != nil {
				onReady()
			}
		})
	})
}

func sendHandshakeError(control Endpoint, message string) {
	response, err := json.Marshal(transportError{
		Type:    "error",
		Scope:   "connection",
		Message: message,
	})
	if err == nil {
		_ = control.SendText(string(response))
	}
}
