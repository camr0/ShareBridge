// Package main implements fieldrecv: a native (non-browser) ShareBridge v1
// direct-path receiver used to test whether the observed ~64-125 Mbps v1
// ceiling is the browser's WebRTC receive path or the pion sender's
// congestion control.
//
// The receiver deliberately does the minimum a fair bulk receiver can do:
// decode the binary envelope and account for payload bytes. It does not
// assemble chunks, write a sink, or hash.
package main

import (
	"encoding/binary"
	"fmt"
)

// Binary frame kinds shared with signaling-server/web/src/binaryEnvelope.js.
const (
	frameKindFileChunk = 0x10
	frameKindThumbnail = 0x11

	chunkEnvelopeVersion   = 0x01
	chunkEnvelopeBytes     = 14 // kind + version + uint64 operation + uint32 generation
	thumbnailEnvelopeBytes = 3
)

// framePayloadLen validates one binary DataChannel frame and returns the
// length of its application payload, mirroring decodeBinaryEnvelope in the
// browser client. The returned kind lets the caller separate bulk file chunks
// from media-lane thumbnails.
func framePayloadLen(data []byte) (payloadLen int, kind byte, err error) {
	if len(data) == 0 {
		return 0, 0, fmt.Errorf("empty binary frame")
	}
	switch data[0] {
	case frameKindFileChunk:
		if len(data) < chunkEnvelopeBytes {
			return 0, data[0], fmt.Errorf("file chunk envelope too short: %d < %d", len(data), chunkEnvelopeBytes)
		}
		if data[1] != chunkEnvelopeVersion {
			return 0, data[0], fmt.Errorf("unsupported chunk envelope version: %d", data[1])
		}
		operation := binary.BigEndian.Uint64(data[2:10])
		if operation == 0 {
			return 0, data[0], fmt.Errorf("file chunk operation id must be nonzero")
		}
		return len(data) - chunkEnvelopeBytes, frameKindFileChunk, nil
	case frameKindThumbnail:
		if len(data) < thumbnailEnvelopeBytes {
			return 0, data[0], fmt.Errorf("thumbnail envelope too short: %d < %d", len(data), thumbnailEnvelopeBytes)
		}
		return len(data) - thumbnailEnvelopeBytes, frameKindThumbnail, nil
	default:
		return 0, data[0], fmt.Errorf("unknown binary frame kind: 0x%02x", data[0])
	}
}
