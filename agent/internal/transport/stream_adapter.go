package transport

import (
	"io"
	"sync"
)

// StreamAdapter wraps an io.ReadWriteCloser (typically a libp2p network.Stream)
// so it satisfies the transfer.DataChannel interface expected by
// transfer.Manager.
//
// Framing: every SendText/SendBinary call writes one length-prefixed frame,
// so the peer can distinguish text from binary messages without a separate
// signaling layer.
//
// BufferedAmount always returns 0: yamux (the libp2p stream muxer) enforces
// backpressure internally by blocking Write once the peer's receive window is
// full, so the manager's sendWithBackpressure loop does not need to sleep.
type StreamAdapter struct {
	rw     io.ReadWriteCloser
	writeM sync.Mutex // serialises Write calls so frames never interleave
}

// NewStreamAdapter wraps rw.
func NewStreamAdapter(rw io.ReadWriteCloser) *StreamAdapter {
	return &StreamAdapter{rw: rw}
}

// SendText sends a JSON control message as a FrameText frame.
func (s *StreamAdapter) SendText(text string) error {
	s.writeM.Lock()
	defer s.writeM.Unlock()
	return WriteFrame(s.rw, FrameText, []byte(text))
}

// SendBinary sends a binary chunk as a FrameBinary frame.
func (s *StreamAdapter) SendBinary(data []byte) error {
	s.writeM.Lock()
	defer s.writeM.Unlock()
	return WriteFrame(s.rw, FrameBinary, data)
}

// BufferedAmount is always 0. See type doc.
func (s *StreamAdapter) BufferedAmount() uint64 { return 0 }

// Close closes the underlying stream.
func (s *StreamAdapter) Close() error { return s.rw.Close() }