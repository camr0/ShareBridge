package transport

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

type FrameKind byte

const (
	FrameText   FrameKind = 0x01
	FrameBinary FrameKind = 0x02

	MaxFramePayload = 8 * 1024 * 1024 // 8 MiB defensive cap
)

// WriteFrame writes a single framed message to w.
func WriteFrame(w io.Writer, kind FrameKind, payload []byte) error {
	if kind != FrameText && kind != FrameBinary {
		return fmt.Errorf("unknown frame kind: 0x%x", byte(kind))
	}
	if len(payload) > MaxFramePayload {
		return fmt.Errorf("frame too large: %d bytes (max %d)", len(payload), MaxFramePayload)
	}
	var header [5]byte
	header[0] = byte(kind)
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// ReadFrame reads a single framed message from r.
// Returns io.EOF cleanly when r is exhausted at a frame boundary.
func ReadFrame(r io.Reader) (FrameKind, []byte, error) {
	var header [5]byte
	n, err := io.ReadFull(r, header[:])
	if err != nil {
		if n == 0 && errors.Is(err, io.EOF) {
			return 0, nil, io.EOF
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, nil, fmt.Errorf("truncated frame header: %w", err)
		}
		return 0, nil, err
	}
	kind := FrameKind(header[0])
	if kind != FrameText && kind != FrameBinary {
		return 0, nil, fmt.Errorf("unknown frame kind: 0x%x", header[0])
	}
	length := binary.BigEndian.Uint32(header[1:])
	if length > MaxFramePayload {
		return 0, nil, fmt.Errorf("frame too large: %d bytes (max %d)", length, MaxFramePayload)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, fmt.Errorf("read frame payload: %w", err)
	}
	return kind, payload, nil
}