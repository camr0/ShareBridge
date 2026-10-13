package multilane

import "testing"

// bulkTestChannelSet is a testChannelSet that advertises extra bulk lanes and
// supports the additive close-observer interface.
type bulkTestChannelSet struct {
	*testChannelSet
	bulk     []Endpoint
	closeObs []func()
}

func (set *bulkTestChannelSet) BulkEndpoints() []Endpoint { return set.bulk }
func (set *bulkTestChannelSet) AddOnClose(handler func()) {
	set.closeObs = append(set.closeObs, handler)
}

func TestFanoutChannelSetWithoutBulkEndpointsDelegates(t *testing.T) {
	primary := newTestChannelSet()
	set := NewFanoutChannelSet(primary, nil)

	if set.Endpoint(LaneControl) != primary.endpoints[LaneControl] {
		t.Fatal("control lane must come from the primary set")
	}
	if set.Endpoint(LaneMedia) != primary.endpoints[LaneMedia] {
		t.Fatal("media lane must come from the primary set")
	}
	if set.Endpoint(LaneBulk) != primary.endpoints[LaneBulk] {
		t.Fatal("bulk lane must fall back to the primary set")
	}
	if got := set.BulkEndpoints(); got != nil {
		t.Fatalf("BulkEndpoints() = %v, want nil so the manager keeps one lane", got)
	}
}

func TestFanoutChannelSetAggregatesBulkLanes(t *testing.T) {
	primary := newTestChannelSet()
	extraA, extraB := &testEndpoint{}, &testEndpoint{}
	set := NewFanoutChannelSet(primary, []Endpoint{primary.endpoints[LaneBulk], extraA, nil, extraB})

	lanes := set.BulkEndpoints()
	if len(lanes) != 3 {
		t.Fatalf("BulkEndpoints() = %d lanes, want 3 (nil ignored)", len(lanes))
	}
	if lanes[0] != primary.endpoints[LaneBulk] || lanes[1] != extraA || lanes[2] != extraB {
		t.Fatal("BulkEndpoints() must preserve striping order")
	}
	if set.Endpoint(LaneBulk) != primary.endpoints[LaneBulk] {
		t.Fatal("Endpoint(LaneBulk) must be the primary lane")
	}

	// Mutating the returned slice must not affect the set.
	lanes[0] = nil
	if set.BulkEndpoints()[0] != primary.endpoints[LaneBulk] {
		t.Fatal("BulkEndpoints() leaked internal state")
	}
}

func TestFanoutChannelSetSingleBulkLaneReportsNil(t *testing.T) {
	primary := newTestChannelSet()
	set := NewFanoutChannelSet(primary, []Endpoint{primary.endpoints[LaneBulk]})
	if got := set.BulkEndpoints(); got != nil {
		t.Fatalf("BulkEndpoints() = %v, want nil for a single lane", got)
	}
}

func TestFanoutChannelSetLifecycleDelegates(t *testing.T) {
	base := newTestChannelSet()
	primary := &bulkTestChannelSet{testChannelSet: base}
	set := NewFanoutChannelSet(primary, []Endpoint{base.endpoints[LaneBulk], &testEndpoint{}})

	var opened, closed int
	set.SetOnOpen(func() { opened++ })
	set.SetOnClose(func() { closed++ })
	set.AddOnClose(func() { closed += 10 })

	// The fake ChannelSet never fires close observers itself, so assert the
	// delegation instead: the observer landed on the primary's additive list and
	// the owner callback on the primary's owner slot.
	if len(primary.closeObs) != 1 {
		t.Fatalf("additive observers = %d, want 1 delegated to the primary", len(primary.closeObs))
	}
	base.onOpen()
	base.onClose()
	if opened != 1 {
		t.Fatalf("open handler calls = %d, want 1", opened)
	}
	if closed != 1 {
		t.Fatalf("owner close handler calls = %d, want 1", closed)
	}
	primary.closeObs[0]()
	if closed != 11 {
		t.Fatalf("after invoking the delegated observer, close count = %d, want 11", closed)
	}
	if err := set.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if base.closed != 1 {
		t.Fatalf("primary closed %d times, want 1", base.closed)
	}
}

func TestFanoutChannelSetAddOnCloseNoOpWithoutSupport(t *testing.T) {
	primary := newTestChannelSet() // does not implement CloseListenerChannelSet
	set := NewFanoutChannelSet(primary, nil)
	set.AddOnClose(func() { t.Fatal("unsupported primary must not receive the observer") })
}
