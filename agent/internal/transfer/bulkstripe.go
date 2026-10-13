package transfer

import (
	"fmt"

	"sharebridge/agent/internal/bulkfan"
	"sharebridge/agent/internal/multilane"
)

// stripedEndpoint is a multilane.Endpoint that distributes bulk chunk frames
// round-robin across several underlying bulk lanes and stamps a global chunk
// sequence (envelope v2) so the receiver can restore source order. It backs
// both A1 (lanes on independent PeerConnections) and N5 (ordered lanes on one
// association); the levers differ only in which lanes were supplied.
//
// The manager swaps it in for m.bulk only when a lever is enabled, so the
// default path never constructs it.
type stripedEndpoint struct {
	fan   *bulkfan.Fanout
	sinks []multilane.Endpoint
}

// newStripedEndpoint returns nil unless at least two lanes are available.
func newStripedEndpoint(endpoints []multilane.Endpoint) *stripedEndpoint {
	sinks := make([]bulkfan.Sink, 0, len(endpoints))
	usable := make([]multilane.Endpoint, 0, len(endpoints))
	for _, endpoint := range endpoints {
		if endpoint == nil {
			continue
		}
		sinks = append(sinks, endpointSink{endpoint: endpoint})
		usable = append(usable, endpoint)
	}
	fan := bulkfan.NewFanout(sinks)
	if fan == nil {
		return nil
	}
	return &stripedEndpoint{fan: fan, sinks: usable}
}

func (e *stripedEndpoint) SendText(string) error {
	return fmt.Errorf("bulk stripe endpoint cannot carry text")
}

func (e *stripedEndpoint) SendBinary(data []byte) error {
	return e.SendBinaryClass(multilane.ClassBulk, data)
}

func (e *stripedEndpoint) SendBinaryClass(class multilane.TrafficClass, data []byte) error {
	if class != multilane.ClassBulk {
		return fmt.Errorf("bulk stripe endpoint only carries ClassBulk, got %d", class)
	}
	index, seq := e.fan.Next()
	return e.sinks[index].SendBinaryClass(class, stampChunkSequence(data, seq))
}

// BufferedAmount reports the deepest lane queue so the shared source is
// throttled by whichever association is furthest behind.
func (e *stripedEndpoint) BufferedAmount() uint64 { return e.fan.BufferedAmount() }

// SetOnMessage is a no-op: bulk lanes are send-only.
func (e *stripedEndpoint) SetOnMessage(func([]byte)) {}

// stampChunkSequence rewrites a v1 chunk frame as a v2 frame carrying the
// global sequence. Non-chunk frames pass through unchanged.
func stampChunkSequence(frame []byte, seq uint64) []byte {
	operation, generation, _, payload, err := bulkfan.DecodeChunkFrame(frame)
	if err != nil {
		return frame
	}
	return bulkfan.EncodeChunkFrame(operation, generation, seq, payload)
}

// endpointSink adapts a multilane.Endpoint to bulkfan.Sink, forcing every send
// onto the bulk traffic class.
type endpointSink struct{ endpoint multilane.Endpoint }

func (s endpointSink) Send(data []byte) error {
	return s.endpoint.SendBinaryClass(multilane.ClassBulk, data)
}

func (s endpointSink) BufferedAmount() uint64 { return s.endpoint.BufferedAmount() }
