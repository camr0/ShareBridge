package direct

import (
	"errors"
	"fmt"
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

// routerMapper is a listing-capable PortMapper that simulates a router's
// mapping table, so port selection (ChooseExternalPort lists) and ownership-
// checked deletion (DeleteOwnedMapping lists and compares identity) both see
// the live mapping — unlike recordingMapper, whose nil table means "listing
// unsupported" and therefore never exercises the identity comparison. delFails
// makes the first N DeletePortMapping calls fail, so a compensating delete can
// fail and the close-retry timer must retry with the mapping's true identity.
type routerMapper struct {
	mu         sync.Mutex
	internalIP string
	table      map[int]PortMapping
	delFails   int
	delCalls   int
	addCalls   int
	listCalls  int

	addEntered chan struct{}
	addRelease chan struct{}
}

func newRouterMapper(internalIP string, existing ...PortMapping) *routerMapper {
	m := &routerMapper{internalIP: internalIP, table: map[int]PortMapping{}}
	for _, e := range existing {
		m.table[e.ExternalPort] = e
	}
	return m
}

// gateAdd arms AddPortMapping to signal entry on entered and block until
// release is closed.
func (m *routerMapper) gateAdd(entered, release chan struct{}) {
	m.mu.Lock()
	m.addEntered, m.addRelease = entered, release
	m.mu.Unlock()
}

// setDelFails makes the first n DeletePortMapping calls fail.
func (m *routerMapper) setDelFails(n int) {
	m.mu.Lock()
	m.delFails = n
	m.mu.Unlock()
}

func (m *routerMapper) AddPortMapping(ext, internal int, desc string, lease int) (int, error) {
	m.mu.Lock()
	entered, release := m.addEntered, m.addRelease
	m.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
		<-release
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addCalls++
	m.table[ext] = PortMapping{
		ExternalPort:   ext,
		InternalPort:   internal,
		InternalClient: m.internalIP,
		Protocol:       "TCP",
		Description:    desc,
	}
	return ext, nil
}

func (m *routerMapper) DeletePortMapping(ext int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.delCalls++
	if m.delCalls <= m.delFails {
		return fmt.Errorf("delete failed (attempt %d)", m.delCalls)
	}
	delete(m.table, ext)
	return nil
}

func (m *routerMapper) ExternalIP() (string, error) { return "203.0.113.7", nil }

func (m *routerMapper) InternalIP() string { return m.internalIP }

func (m *routerMapper) ListPortMappings() ([]PortMapping, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listCalls++
	out := make([]PortMapping, 0, len(m.table))
	for _, v := range m.table {
		out = append(out, v)
	}
	return out, nil
}

func (m *routerMapper) mappingAt(ext int) (PortMapping, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.table[ext]
	return v, ok
}

func (m *routerMapper) mappingCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.table)
}

func (m *routerMapper) listCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listCalls
}

func (m *routerMapper) snapshotTable() map[int]PortMapping {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[int]PortMapping, len(m.table))
	for k, v := range m.table {
		out[k] = v
	}
	return out
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

func (r *loopStateRecorder) openCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, s := range r.states {
		if s == StateOpen {
			n++
		}
	}
	return n
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

// TestOpenForIfFencesAlreadyOpenNonRenewalSuccess is the fix-round-3 bypass:
// the already-open fast path that does NOT extend the lease performs no mapping
// write, so at the round-2 HEAD it had no second generation check at all — it
// rearmed and replied success. A §13.4 generation transition published between
// its top-of-case fence check and its reply therefore produced an OK open_ack
// after lockdown publication. The success choke point must refuse it, undo the
// stale mapping, and reply ErrOpenSuperseded instead of success.
//
// The fake clock gates the first clock read after the top-of-case fence — the
// only point in this path where a transition can be landed deterministically.
func TestOpenForIfFencesAlreadyOpenNonRenewalSuccess(t *testing.T) {
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	base := &recordingMapper{}
	p := newTestPort(fc, base, time.Minute)
	defer p.Close()
	rec := &loopStateRecorder{}
	p.SetTransitionCallback(rec.record)

	// A legitimate cold open under generation 0 with a long lease.
	if err := p.OpenForIf("share", time.Hour, 0); err != nil {
		t.Fatalf("cold open: %v", err)
	}
	if !p.Open() {
		t.Fatalf("cold open did not open the port")
	}
	opensAfterColdOpen, _, _, _ := base.snapshot()
	publishedOpens := rec.openCount()

	// Arm the one-shot clock gate: the next OpenForIf blocks after its
	// top-of-case fence check and before its success reply.
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	fc.gateNextNow(entered, release)

	// A 1s lease (clamped to minValidLease, 5s) does not extend the existing
	// one-hour deadline, so this is the already-open NON-renewal fast path.
	errCh := make(chan error, 1)
	go func() { errCh <- p.OpenForIf("share", time.Second, 0) }()
	awaitEntered(t, entered, "the already-open non-renewal open reaching its success window")

	// The generation transition lands after the top-of-case check and before
	// the success reply.
	p.SetGeneration(1, true)
	close(release)

	err := <-errCh
	if !errors.Is(err, ErrOpenSuperseded) {
		t.Fatalf("the already-open non-renewal path replied OK (err = %v); that reply becomes an "+
			"open_ack after lockdown publication, want ErrOpenSuperseded", err)
	}
	if got := rec.openCount(); got != publishedOpens {
		t.Fatalf("a superseded already-open success must not publish StateOpen: %v", rec.snapshot())
	}
	if p.Open() {
		t.Fatalf("the port must not report open after the superseded success was refused")
	}
	if got := p.State(); got != StateClosed {
		t.Fatalf("state = %v, want StateClosed (the stale mapping is undone)", got)
	}
	if n, _, _, _ := base.snapshot(); n != opensAfterColdOpen {
		t.Fatalf("the non-renewal path issued %d extra AddPortMapping calls, want 0", n-opensAfterColdOpen)
	}
	if _, closed, _, _ := base.snapshot(); closed == 0 {
		t.Fatalf("the stale mapping owned by the superseded generation was not removed")
	}
}

// TestOpenForIfLegitimateAlreadyOpenNonRenewal is the no-false-fencing control
// for the path the success choke point now covers: with an unchanged
// generation, an already-open signal whose lease does not extend the deadline
// replies success, issues no router write, and leaves the mapping open.
func TestOpenForIfLegitimateAlreadyOpenNonRenewal(t *testing.T) {
	fc := newFakeClock(time.Now())
	base := &recordingMapper{}
	p := newTestPort(fc, base, time.Minute)
	defer p.Close()

	if err := p.OpenForIf("share", time.Hour, 0); err != nil {
		t.Fatalf("cold open: %v", err)
	}
	opened, _, closed, _ := base.snapshot()

	if err := p.OpenForIf("share", time.Second, 0); err != nil {
		t.Fatalf("already-open non-renewal: %v", err)
	}
	if !p.Open() {
		t.Fatalf("the port must stay open after a legitimate non-renewal signal")
	}
	if o, _, c, _ := base.snapshot(); o != opened || c != closed {
		t.Fatalf("the non-renewal path must issue no mapping write or delete: opened %d→%d closed %d→%d", opened, o, closed, c)
	}
	if got := p.State(); got != StateOpen {
		t.Fatalf("state = %v, want StateOpen", got)
	}
}

// TestOpenForIfUndoRetainsCreatedMappingIdentity is the fix-round-3 undo leak:
// the fenced cold open's undo attempted the compensating delete and then
// restored p.extPort to the preferred port. When port selection had chosen a
// different port (443 occupied → 49152), a failed immediate delete left the
// close-retry timer building its delete identity from the RESTORED p.extPort:
// the live mapping is described "test-49152" while the retry required
// "test-443", so DeleteOwnedMapping refused it as foreign, the retries were
// exhausted, and the agent's mapping survived until router expiry. The undo
// must target the mapping it actually created.
func TestOpenForIfUndoRetainsCreatedMappingIdentity(t *testing.T) {
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	foreign := PortMapping{
		ExternalPort: 443, InternalPort: 80, InternalClient: "192.168.1.9",
		Protocol: "TCP", Description: "other-service",
	}
	mapper := newRouterMapper("192.168.1.20", foreign)
	mapper.setDelFails(1) // the undo's immediate delete fails exactly once
	p := newOwnedTestPort(fc, mapper, 443, 8443, time.Minute, "test", "192.168.1.20")
	defer p.Close()
	rec := &loopStateRecorder{}
	p.SetTransitionCallback(rec.record)

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	mapper.gateAdd(entered, release)

	errCh := make(chan error, 1)
	go func() { errCh <- p.OpenForIf("share", time.Minute, 0) }()
	awaitEntered(t, entered, "the cold open entering AddPortMapping")
	p.SetGeneration(1, true)
	close(release)

	if err := <-errCh; !errors.Is(err, ErrOpenSuperseded) {
		t.Fatalf("err = %v, want ErrOpenSuperseded", err)
	}
	if rec.sawOpen() {
		t.Fatalf("a fenced cold open must not publish StateOpen: %v", rec.snapshot())
	}
	// Port selection must have chosen the dynamic port, and the mapping the
	// open created must carry that port's description.
	created, ok := mapper.mappingAt(49152)
	if !ok {
		t.Fatalf("the cold open did not map the selected dynamic port 49152; table = %v", mapper.snapshotTable())
	}
	if created.Description != "test-49152" {
		t.Fatalf("created mapping description = %q, want %q", created.Description, "test-49152")
	}

	// Drive the close-retry timer: advanced one retry at a time, waiting for the
	// loop to re-arm, so the test is deterministic.
	for i := 0; i < maxCloseAttempts+2; i++ {
		if p.State() == StateCloseFailed || mapper.mappingCount() == 1 {
			break
		}
		before := mapper.listCount()
		fc.advance(closeRetryDelay + time.Millisecond)
		waitFor(t, func() bool { return mapper.listCount() > before || p.State() == StateCloseFailed })
	}

	if _, ok := mapper.mappingAt(49152); ok {
		t.Fatalf("the fenced open's mapping leaked: the retry did not target the created mapping; table = %v", mapper.snapshotTable())
	}
	if _, ok := mapper.mappingAt(443); !ok {
		t.Fatalf("the pre-existing foreign mapping at 443 must not be deleted: %v", mapper.snapshotTable())
	}
	if p.Open() {
		t.Fatalf("the port must not be open")
	}
	if got := p.State(); got != StateClosed {
		t.Fatalf("state = %v, want StateClosed (no close-failed escalation)", got)
	}
	if err := p.CloseError(); err != nil {
		t.Fatalf("CloseError = %v, want nil", err)
	}
}

// TestCloseIfReclaimsMappingAfterCloseFailed proves a §13.4 lockdown CloseIf is
// not a no-op while a mapping a failed close could not remove is still live: the
// close lever re-attempts the delete, so the agent cannot report itself locked
// with a mapping it created still present on the router.
func TestCloseIfReclaimsMappingAfterCloseFailed(t *testing.T) {
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	mapper := newRouterMapper("192.168.1.20")
	mapper.setDelFails(maxCloseAttempts) // every retry of the first close fails
	p := newOwnedTestPort(fc, mapper, 443, 8443, time.Minute, "test", "192.168.1.20")
	defer p.Close()

	if err := p.OpenFor("share", time.Minute); err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if err := p.Close(); !errors.Is(err, ErrDeleteRetry) {
		t.Fatalf("Close: want ErrDeleteRetry, got %v", err)
	}
	for i := 0; i < maxCloseAttempts+1 && p.State() != StateCloseFailed; i++ {
		before := mapper.listCount()
		fc.advance(closeRetryDelay + time.Millisecond)
		waitFor(t, func() bool { return mapper.listCount() > before || p.State() == StateCloseFailed })
	}
	if p.State() != StateCloseFailed {
		t.Fatalf("state = %v, want StateCloseFailed with the mapping still live", p.State())
	}
	if mapper.mappingCount() != 1 {
		t.Fatalf("the failed close's mapping is not lingering: %v", mapper.snapshotTable())
	}

	// The lockdown lever's CloseIf must re-attempt the delete (this sixth
	// attempt succeeds) instead of returning a no-op while the mapping lives.
	if err := p.CloseIf(func() bool { return true }); err != nil {
		t.Fatalf("CloseIf after close-failed: %v", err)
	}
	if mapper.mappingCount() != 0 {
		t.Fatalf("CloseIf left the mapping live while the agent is locked: %v", mapper.snapshotTable())
	}
	if got := p.State(); got != StateClosed {
		t.Fatalf("state = %v, want StateClosed", got)
	}
	if err := p.CloseError(); err != nil {
		t.Fatalf("CloseError = %v, want nil", err)
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

// TestOnDemandPortGenerationReflectsSetGeneration pins the introspection
// accessor the daemon tests use to observe a published fence: it returns the
// exact (epoch, locked) pair SetGeneration published, so a test can wait for a
// transition to be visible to the state loop instead of polling a flag the
// daemon sets just before publishing.
func TestOnDemandPortGenerationReflectsSetGeneration(t *testing.T) {
	fc := newFakeClock(time.Now())
	p := newTestPort(fc, &recordingMapper{}, time.Minute)
	defer p.Close()

	if epoch, locked := p.Generation(); epoch != 0 || locked {
		t.Fatalf("initial Generation() = (%d, %v), want (0, false)", epoch, locked)
	}
	p.SetGeneration(3, true)
	if epoch, locked := p.Generation(); epoch != 3 || !locked {
		t.Fatalf("Generation() after SetGeneration(3,true) = (%d, %v), want (3, true)", epoch, locked)
	}
	p.SetGeneration(4, false)
	if epoch, locked := p.Generation(); epoch != 4 || locked {
		t.Fatalf("Generation() after SetGeneration(4,false) = (%d, %v), want (4, false)", epoch, locked)
	}
}
