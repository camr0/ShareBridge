package multilane

// BulkEndpointSet is implemented by channel sets that expose more than one bulk
// lane: N5 adds ordered bulk DataChannels to one association, and A1 aggregates
// bulk lanes from several independent associations. Endpoints are returned in
// striping order; index 0 is the primary bulk lane.
type BulkEndpointSet interface {
	BulkEndpoints() []Endpoint
}

// FanoutChannelSet presents a single logical channel set whose bulk lane is
// drawn from several endpoints, typically across independent PeerConnections
// (candidate A1). Control and media remain on the primary set, which also owns
// the lifecycle. With no bulk endpoints it degenerates to the primary set.
type FanoutChannelSet struct {
	primary ChannelSet
	bulk    []Endpoint
}

// NewFanoutChannelSet wraps a primary channel set with additional bulk lanes.
// Nil endpoints are ignored.
func NewFanoutChannelSet(primary ChannelSet, bulk []Endpoint) *FanoutChannelSet {
	filtered := make([]Endpoint, 0, len(bulk))
	for _, endpoint := range bulk {
		if endpoint != nil {
			filtered = append(filtered, endpoint)
		}
	}
	return &FanoutChannelSet{primary: primary, bulk: filtered}
}

// Endpoint returns the control/media lane from the primary set and the first
// bulk lane from the fan-out. Callers that stripe use BulkEndpoints instead.
func (s *FanoutChannelSet) Endpoint(lane Lane) Endpoint {
	if lane == LaneBulk && len(s.bulk) > 0 {
		return s.bulk[0]
	}
	return s.primary.Endpoint(lane)
}

// BulkEndpoints returns the ordered bulk lanes to stripe across, or nil when
// there is no fan-out (so the transfer manager keeps its single-lane path).
func (s *FanoutChannelSet) BulkEndpoints() []Endpoint {
	if len(s.bulk) < 2 {
		return nil
	}
	out := make([]Endpoint, len(s.bulk))
	copy(out, s.bulk)
	return out
}

// SetOnOpen delegates to the primary set.
func (s *FanoutChannelSet) SetOnOpen(handler func()) { s.primary.SetOnOpen(handler) }

// SetOnClose delegates to the primary set.
func (s *FanoutChannelSet) SetOnClose(handler func()) { s.primary.SetOnClose(handler) }

// AddOnClose delegates to the primary set when it supports the additive
// observer interface.
func (s *FanoutChannelSet) AddOnClose(handler func()) {
	if listener, ok := s.primary.(CloseListenerChannelSet); ok {
		listener.AddOnClose(handler)
	}
}

// Close closes the primary set.
func (s *FanoutChannelSet) Close() error { return s.primary.Close() }
