package direct

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// gatedListMapper blocks ListPortMappings (the port-selection router I/O of a
// cold open) behind a channel, so a test can publish a new generation while
// OpenForIf is inside ChooseExternalPort. It otherwise behaves like
// recordingMapper (no enumerated mappings → the preferred port is chosen).
type gatedListMapper struct {
	*recordingMapper
	listEntered chan struct{}
	listRelease chan struct{}
}

func (m *gatedListMapper) ListPortMappings() ([]PortMapping, error) {
	if m.listEntered != nil {
		m.listEntered <- struct{}{}
		<-m.listRelease
	}
	return m.recordingMapper.ListPortMappings()
}

// gatedAddMapper blocks AddPortMapping (the mapping write itself) behind a
// channel, so a test can land the generation transition in the reviewer's
// round-2 gap: after the predicate passed and while the write is in flight.
type gatedAddMapper struct {
	*recordingMapper
	addEntered chan struct{}
	addRelease chan struct{}
}

func (m *gatedAddMapper) AddPortMapping(ext, internal int, desc string, lease int) (int, error) {
	if m.addEntered != nil {
		m.addEntered <- struct{}{}
		<-m.addRelease
	}
	return m.recordingMapper.AddPortMapping(ext, internal, desc, lease)
}

// loopStateRecorder records the port's published state transitions.
type loopStateRecorder struct {
	mu     sync.Mutex
	states []PortState
}

func (r *loopStateRecorder) record(_, next PortState, _ int) {
	r.mu.Lock()
	r.states = append(r.states, next)
	r.mu.Unlock()
}

func (r *loopStateRecorder) sawOpen() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.states {
		if s == StateOpen {
			return true
		}
	}
	return false
}

func (r *loopStateRecorder) snapshot() []PortState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]PortState(nil), r.states...)
}

func awaitEntered(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never arrived", what)
	}
}

// TestOpenForIfFencesColdOpenAfterPortSelection proves the fence is
// re-evaluated on the state loop after the cold open's port-selection router
// I/O but before the mapping write: a generation published during
// ChooseExternalPort must prevent the mapping and must not publish the
// selected preferred port.
func TestOpenForIfFencesColdOpenAfterPortSelection(t *testing.T) {
	fc := newFakeClock(time.Now())
	base := &recordingMapper{}
	mapper := &gatedListMapper{
		recordingMapper: base,
		listEntered:     make(chan struct{}, 1),
		listRelease:     make(chan struct{}),
	}
	p := newTestPort(fc, mapper, time.Minute)
	defer p.Close()

	errCh := make(chan error, 1)
	go func() { errCh <- p.OpenForIf("share", time.Minute, 0) }()

	awaitEntered(t, mapper.listEntered, "OpenForIf reaching port selection")
	// The generation advances while the state loop is inside router I/O.
	p.SetGeneration(1, true)
	close(mapper.listRelease)

	if err := <-errCh; !errors.Is(err, ErrOpenSuperseded) {
		t.Fatalf("err = %v, want ErrOpenSuperseded", err)
	}
	if opened, _, _, _ := base.snapshot(); opened != 0 {
		t.Fatalf("fenced cold open issued %d AddPortMapping calls, want 0", opened)
	}
	if p.Open() {
		t.Fatalf("port must remain closed after a fenced cold open")
	}
	if got := p.State(); got != StateClosed {
		t.Fatalf("state = %v, want StateClosed", got)
	}
}

// TestOpenForIfFencesColdOpenDuringAddPortMapping is the cold-open half of the
// reviewer's round-2 gap: the predicate has passed and the state loop is inside
// the mapping write when the generation is published. The open must fail
// closed, must never publish StateOpen, and must not leave a mapping behind —
// the port undoes the router write it can no longer authorize.
func TestOpenForIfFencesColdOpenDuringAddPortMapping(t *testing.T) {
	fc := newFakeClock(time.Now())
	base := &recordingMapper{}
	mapper := &gatedAddMapper{
		recordingMapper: base,
		addEntered:      make(chan struct{}, 1),
		addRelease:      make(chan struct{}),
	}
	p := newTestPort(fc, mapper, time.Minute)
	defer p.Close()
	rec := &loopStateRecorder{}
	p.SetTransitionCallback(rec.record)

	errCh := make(chan error, 1)
	go func() { errCh <- p.OpenForIf("share", time.Minute, 0) }()
	awaitEntered(t, mapper.addEntered, "OpenForIf entering AddPortMapping")

	// The generation transition lands in the predicate-to-write gap.
	p.SetGeneration(1, true)
	close(mapper.addRelease)

	if err := <-errCh; !errors.Is(err, ErrOpenSuperseded) {
		t.Fatalf("err = %v, want ErrOpenSuperseded", err)
	}
	if rec.sawOpen() {
		t.Fatalf("port transitioned open for a fenced cold open: %v", rec.snapshot())
	}
	if p.Open() {
		t.Fatalf("port must not be open after a fenced cold open")
	}
	if got := p.State(); got != StateClosed {
		t.Fatalf("state = %v, want StateClosed", got)
	}
	if _, closed, _, _ := base.snapshot(); closed == 0 {
		t.Fatalf("the mapping written by the fenced open was not removed")
	}

	// A later legitimate open (after a lockdown→unlock cycle that published a
	// newer, unlocked stamp) must still work: no wedged loop, no leftover
	// preferred-port state.
	mapper.addEntered = nil
	p.SetGeneration(2, false)
	if err := p.OpenForIf("share", time.Minute, 2); err != nil {
		t.Fatalf("post-fence legitimate open: %v", err)
	}
	if started, _, _, _ := base.snapshot(); started != 2 {
		t.Fatalf("post-fence open AddPortMapping calls = %d, want 2", started)
	}
	if !p.Open() {
		t.Fatalf("post-fence legitimate open must open the port")
	}
}

// TestOpenForIfFencesRenewalDuringAddPortMapping is the renewal half of the
// reviewer's round-2 gap: an already-open mapping, the renewal predicate
// passed, and the generation published while the renewal write is in flight.
// The renewal must not be published and the mapping must not survive.
func TestOpenForIfFencesRenewalDuringAddPortMapping(t *testing.T) {
	fc := newFakeClock(time.Now())
	base := &recordingMapper{}
	mapper := &gatedAddMapper{recordingMapper: base}
	p := newTestPort(fc, mapper, time.Minute)
	defer p.Close()

	if err := p.OpenForIf("share", time.Minute, 0); err != nil {
		t.Fatalf("legitimate open: %v", err)
	}
	// Move past the renewal decision point so the second call renews.
	fc.advance(3 * time.Second)

	mapper.addEntered = make(chan struct{}, 1)
	mapper.addRelease = make(chan struct{})
	errCh := make(chan error, 1)
	go func() { errCh <- p.OpenForIf("share", 5*time.Minute, 0) }()
	awaitEntered(t, mapper.addEntered, "renewal entering AddPortMapping")

	p.SetGeneration(1, true)
	close(mapper.addRelease)

	if err := <-errCh; !errors.Is(err, ErrOpenSuperseded) {
		t.Fatalf("renewal err = %v, want ErrOpenSuperseded", err)
	}
	if p.Open() {
		t.Fatalf("a fenced renewal must not leave the mapping open")
	}
	if got := p.State(); got != StateClosed {
		t.Fatalf("state = %v, want StateClosed", got)
	}
}

// TestOnDemandPortFencesTimerRenewal proves the autonomous timer-driven
// renewal (the `timerC` branch) consults the generation stamp. The timer is
// driven deterministically by the fake clock; the renewal write is gated so
// the generation can be published while it is in flight.
func TestOnDemandPortFencesTimerRenewal(t *testing.T) {
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	base := &recordingMapper{}
	mapper := &gatedAddMapper{recordingMapper: base}
	p := newTestPort(fc, mapper, time.Minute)
	defer p.Close()
	rec := &loopStateRecorder{}
	p.SetTransitionCallback(rec.record)

	// minValidLease is 5s and renewWindow is 2s → renewAt is 3s out.
	if err := p.OpenForIf("share", 5*time.Second, 0); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := p.BeginSession("share"); err != nil {
		t.Fatalf("BeginSession: %v", err)
	}

	// Arm the gate for the scheduled renewal only, then drive the clock to it.
	mapper.addEntered = make(chan struct{}, 1)
	mapper.addRelease = make(chan struct{})

	// The scheduled renewal fires and blocks inside AddPortMapping. The clock
	// advance is the timer's own schedule, not a synchronization sleep.
	fc.advance(3 * time.Second)
	awaitEntered(t, mapper.addEntered, "the timer renewal entering AddPortMapping")

	p.SetGeneration(1, true)
	close(mapper.addRelease)

	waitFor(t, func() bool { return !p.Open() })
	if got := p.State(); got != StateClosed {
		t.Fatalf("state = %v, want StateClosed", got)
	}
	if _, closed, _, _ := base.snapshot(); closed == 0 {
		t.Fatalf("the timer-renewed mapping was not removed")
	}
}

// TestOpenForIfLegitimateOpenAndRenewalUnfenced is the no-false-fencing
// control: with an unchanged generation, a cold open and a renewal both issue
// mappings and stay open.
func TestOpenForIfLegitimateOpenAndRenewalUnfenced(t *testing.T) {
	fc := newFakeClock(time.Now())
	base := &recordingMapper{}
	p := newTestPort(fc, base, time.Minute)
	defer p.Close()

	if err := p.OpenForIf("share", time.Minute, 0); err != nil {
		t.Fatalf("open: %v", err)
	}
	fc.advance(3 * time.Second)
	if err := p.OpenForIf("share", 5*time.Minute, 0); err != nil {
		t.Fatalf("renewal: %v", err)
	}
	if opened, _, _, _ := base.snapshot(); opened != 2 {
		t.Fatalf("open calls = %d, want 2", opened)
	}
	if !p.Open() {
		t.Fatalf("port must be open")
	}
}

// TestOpenForIfLegitimateAfterUnlockGeneration proves an open admitted under
// the CURRENT generation still works after a lockdown/unlock cycle published a
// newer stamp.
func TestOpenForIfLegitimateAfterUnlockGeneration(t *testing.T) {
	fc := newFakeClock(time.Now())
	base := &recordingMapper{}
	p := newTestPort(fc, base, time.Minute)
	defer p.Close()

	p.SetGeneration(1, true)
	p.SetGeneration(2, false)
	if err := p.OpenForIf("share", time.Minute, 2); err != nil {
		t.Fatalf("post-unlock open: %v", err)
	}
	if !p.Open() {
		t.Fatalf("a post-unlock open under the current generation must open the port")
	}
}

// TestOpenForIsUnfenced pins that the unconditional OpenFor does not consult
// the generation stamp (the standalone spike/test callers), while OpenForIf
// does.
func TestOpenForIsUnfenced(t *testing.T) {
	fc := newFakeClock(time.Now())
	base := &recordingMapper{}
	p := newTestPort(fc, base, time.Minute)
	defer p.Close()

	p.SetGeneration(7, true)
	if err := p.OpenFor("share", time.Minute); err != nil {
		t.Fatalf("unfenced OpenFor: %v", err)
	}
	if !p.Open() {
		t.Fatalf("unfenced OpenFor must open the port")
	}
	// A fenced call under the same (locked) stamp must still be refused.
	if err := p.OpenForIf("share", time.Minute, 7); !errors.Is(err, ErrOpenSuperseded) {
		t.Fatalf("fenced open while locked err = %v, want ErrOpenSuperseded", err)
	}
}
