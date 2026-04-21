// agent/internal/relaychannel/frame.go
package relaychannel

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	FrameHandshake byte = 0x00
	FrameText      byte = 0x01
	FrameBinary    byte = 0x02

	MaxHandshakePayload = 4 * 1024
	MaxFramePayload     = 8 * 1024 * 1024
)

type Frame struct {
	Kind    byte
	Payload []byte
}

func WriteFrame(kind byte, payload []byte) ([]byte, error) {
	cap, err := capForKind(kind)
	if err != nil {
		return nil, err
	}
	if len(payload) > cap {
		return nil, fmt.Errorf("frame too large: %d", len(payload))
	}
	out := make([]byte, 5+len(payload))
	out[0] = kind
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out, nil
}

func DecodeOneFrame(buf []byte) (Frame, []byte, error) {
	if len(buf) < 5 {
		return Frame{}, nil, errors.New("incomplete frame header")
	}
	cap, err := capForKind(buf[0])
	if err != nil {
		return Frame{}, nil, err
	}
	length := int(binary.BigEndian.Uint32(buf[1:5]))
	if length > cap {
		return Frame{}, nil, fmt.Errorf("frame too large: %d", length)
	}
	if len(buf) < 5+length {
		return Frame{}, nil, errors.New("incomplete frame payload")
	}
	frame := Frame{Kind: buf[0], Payload: append([]byte(nil), buf[5:5+length]...)}
	return frame, buf[5+length:], nil
}

func capForKind(kind byte) (int, error) {
	switch kind {
	case FrameHandshake:
		return MaxHandshakePayload, nil
	case FrameText, FrameBinary:
		return MaxFramePayload, nil
	default:
		return 0, fmt.Errorf("unknown frame kind: 0x%02x", kind)
	}
}