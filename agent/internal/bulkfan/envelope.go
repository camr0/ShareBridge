package bulkfan

import (
	"encoding/binary"
	"fmt"
)

// Chunk envelope versions. v1 is the agent's original layout (14 bytes); v2
// adds an 8-byte global chunk sequence and is only emitted when a striping
// lever is enabled. The browser decoder must accept both.
const (
	ChunkFrameKind     = byte(0x10) // binaryFrameFileChunk
	ChunkEnvelopeV1    = byte(0x01)
	ChunkEnvelopeV2    = byte(0x02)
	ChunkEnvelopeV1Len = 14 // kind + version + uint64 operation + uint32 generation
	ChunkEnvelopeV2Len = 22 // v1 + uint64 global sequence
)

// EncodeChunkFrame builds a v2 envelope carrying the global chunk sequence.
// Stripe sinks are independent streams, so the receiver needs the sequence to
// reassemble the source order.
func EncodeChunkFrame(operationID uint64, generation uint32, seq uint64, data []byte) []byte {
	out := make([]byte, ChunkEnvelopeV2Len+len(data))
	out[0] = ChunkFrameKind
	out[1] = ChunkEnvelopeV2
	binary.BigEndian.PutUint64(out[2:10], operationID)
	binary.BigEndian.PutUint32(out[10:14], generation)
	binary.BigEndian.PutUint64(out[14:22], seq)
	copy(out[ChunkEnvelopeV2Len:], data)
	return out
}

// DecodeChunkFrame parses either envelope version. v1 frames report seq 0; the
// caller only treats seq as meaningful when the frame is v2.
func DecodeChunkFrame(frame []byte) (operationID uint64, generation uint32, seq uint64, payload []byte, err error) {
	if len(frame) < ChunkEnvelopeV1Len {
		return 0, 0, 0, nil, fmt.Errorf("chunk envelope too short: %d", len(frame))
	}
	if frame[0] != ChunkFrameKind {
		return 0, 0, 0, nil, fmt.Errorf("unexpected chunk frame type: 0x%02x", frame[0])
	}
	operationID = binary.BigEndian.Uint64(frame[2:10])
	generation = binary.BigEndian.Uint32(frame[10:14])
	switch frame[1] {
	case ChunkEnvelopeV1:
		return operationID, generation, 0, append([]byte(nil), frame[ChunkEnvelopeV1Len:]...), nil
	case ChunkEnvelopeV2:
		if len(frame) < ChunkEnvelopeV2Len {
			return 0, 0, 0, nil, fmt.Errorf("chunk envelope v2 too short: %d", len(frame))
		}
		seq = binary.BigEndian.Uint64(frame[14:22])
		return operationID, generation, seq, append([]byte(nil), frame[ChunkEnvelopeV2Len:]...), nil
	default:
		return 0, 0, 0, nil, fmt.Errorf("unsupported chunk envelope version: %d", frame[1])
	}
}

// EnvelopeVersion reports the version byte of a chunk frame, or an error when
// the frame is not a chunk frame.
func EnvelopeVersion(frame []byte) (byte, error) {
	if len(frame) < ChunkEnvelopeV1Len {
		return 0, fmt.Errorf("chunk envelope too short: %d", len(frame))
	}
	if frame[0] != ChunkFrameKind {
		return 0, fmt.Errorf("unexpected chunk frame type: 0x%02x", frame[0])
	}
	return frame[1], nil
}
