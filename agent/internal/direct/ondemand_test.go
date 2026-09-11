// agent/internal/direct/ondemand_test.go
package direct

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// --- fake clock: drives the single state-loop timer deterministically ---

type fakeTimer struct {
	c    chan time.Time
	at   time.Time
	stop bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.c }
func (t *fakeTimer) Stop() bool {
	if t.stop {
		return false
	}
	t.stop = true
	return true
}

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer

	// armed is signalled (non-blocking, buffered cap 1) on every NewTimer call,
	// i.e. every time the state loop installs a timer. Tests gate a clock
	// advance on it so an advance can never race the loop's re-arm: the
	// delete-retry tests used to advance again while the loop was still between
	// tryDelete and rearm, which made the next advance fire nothing and the test
	// flake under cross-package load. It is test machinery only.
	armed chan struct{}

	// nowEntered/nowRelease are a one-shot gate: while armed, the next Now()
	// call signals entry on nowEntered and blocks until nowRelease is closed.
	// It lets a test publish a generation transition in the window between a
	// state-loop operation's top-of-case fence check and its success commit —
	// the already-open non-renewal fast path's only such window.
	nowEntered chan struct{}
	nowRelease chan struct{}
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start, armed: make(chan struct{}, 1)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	entered, release := f.nowEntered, f.nowRelease
	f.nowEntered, f.nowRelease = nil, nil
	now := f.now
	f.mu.Unlock()
	// Blocking happens after the unlock so the test can still read the clock.
	if entered != nil {
		entered <- struct{}{}
		<-release
	}
	return now
}

// gateNextNow arms the one-shot Now() gate for the next clock read.
func (f *fakeClock) gateNextNow(entered, release chan struct{}) {
	f.mu.Lock()
	f.nowEntered, f.nowRelease = entered, release
	f.mu.Unlock()
}

func (f *fakeClock) NewTimer(d time.Duration) portTimer {
	f.mu.Lock()
	t := &fakeTimer{c: make(chan time.Time, 1), at: f.now.Add(d)}
	f.timers = append(f.timers, t)
	armed := f.armed
	f.mu.Unlock()
	if armed != nil {
		select {
		case armed <- struct{}{}:
		default:
		}
	}
	return t
}

// drainArmed removes any pending timer-armed signal so a subsequent
// advanceGated observes only the re-arm caused by its own fire.
func (f *fakeClock) drainArmed() {
	for {
		select {
		case <-f.armed:
		default:
			return
		}
	}
}

// advanceGated advances the fake clock by d to fire the timer the state loop
// has armed and blocks until the loop has either installed its next timer or
// reached the terminal state signalled by done (which may be nil). Both waits
// are channel-gated, so the advance cannot race the loop's re-arm.
func (f *fakeClock) advanceGated(t *testing.T, d time.Duration, done <-chan struct{}) {
	t.Helper()
	f.drainArmed()
	f.advance(d)
	select {
	case <-f.armed:
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("state loop neither re-armed nor reached the terminal state after advancing %s", d)
	}
}

// channelClosed reports whether ch is already closed (non-blocking).
func channelClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// stateSignal returns a channel closed the first time p transitions into the
// given state. It must be installed before the transition of interest; it is
// used to gate a clock advance on a terminal transition (a close that stops
// retrying and never re-arms).
func stateSignal(p *OnDemandPort, want PortState) <-chan struct{} {
	ch := make(chan struct{})
	var once sync.Once
	p.SetTransitionCallback(func(_, next PortState, _ int) {
		if next == want {
			once.Do(func() { close(ch) })
		}
	})
	return ch
}

// escalateCloseFailure drives p's delete-retry timer to StateCloseFailed,
// gating every advance on the loop having re-armed the next retry (or reached
// the terminal transition). p must already be in the retrying close state.
func escalateCloseFailure(t *testing.T, fc *fakeClock, failed <-chan struct{}) {
	t.Helper()
	for i := 0; i < maxCloseAttempts; i++ {
		if channelClosed(failed) {
			return
		}
		fc.advanceGated(t, closeRetryDelay+time.Millisecond, failed)
	}
	if !channelClosed(failed) {
		t.Fatalf("close did not escalate to StateCloseFailed")
	}
}

// advance moves time forward and fires every non-stopped timer whose deadline
// is reached. Firing happens after unlocking so the loop can re-arm.
func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	var fire []*fakeTimer
	var keep []*fakeTimer
	for _, t := range f.timers {
		if !t.stop && !t.at.After(f.now) {
			fire = append(fire, t)
		} else {
			keep = append(keep, t)
		}
	}
	f.timers = keep
	f.mu.Unlock()
	for _, t := range fire {
		t.stop = true
		select {
		case t.c <- f.now:
		default:
		}
	}
}

// --- recording mapper ---

type recordingMapper struct {
	mu             sync.Mutex
	opened, closed int
	delCalls       int
	lastLease      int
	delFails       int // the first N DeletePortMapping calls fail
	alwaysFailDel  bool
	remap          int   // if non-zero, AddPortMapping returns this instead of ext
	addErr         error // if set, AddPortMapping returns this error
	addPorts       []int // ext argument of every AddPortMapping call
	addDescs       []string
	events         []string      // "add:<ext>"/"del:<ext>" in call order
	listMappings   []PortMapping // if non-nil, ListPortMappings returns these (else ErrListingUnsupported)
}

func (r *recordingMapper) AddPortMapping(ext, internal int, desc string, lease int) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.opened++
	r.lastLease = lease
	r.addPorts = append(r.addPorts, ext)
	r.addDescs = append(r.addDescs, desc)
	r.events = append(r.events, fmt.Sprintf("add:%d", ext))
	if r.addErr != nil {
		return 0, r.addErr
	}
	if r.remap != 0 {
		return r.remap, nil
	}
	return ext, nil
}
func (r *recordingMapper) DeletePortMapping(ext int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.delCalls++
	r.events = append(r.events, fmt.Sprintf("del:%d", ext))
	if r.alwaysFailDel || r.delCalls <= r.delFails {
		return fmt.Errorf("delete failed (attempt %d)", r.delCalls)
	}
	r.closed++
	return nil
}
func (r *recordingMapper) ExternalIP() (string, error) { return "203.0.113.7", nil }

func (r *recordingMapper) InternalIP() string { return "" }

func (r *recordingMapper) ListPortMappings() ([]PortMapping, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listMappings == nil {
		return nil, ErrListingUnsupported
	}
	out := make([]PortMapping, len(r.listMappings))
	copy(out, r.listMappings)
	return out, nil
}

// lastDesc returns the description argument of the most recent AddPortMapping.
func (r *recordingMapper) lastDesc() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.addDescs) == 0 {
		return ""
	}
	return r.addDescs[len(r.addDescs)-1]
}

// eventsSnapshot returns a copy of the add/delete call sequence.
func (r *recordingMapper) eventsSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	copy(out, r.events)
	return out
}

func (r *recordingMapper) snapshot() (opened, closed, delCalls, lastLease int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.opened, r.closed, r.delCalls, r.lastLease
}

// ports returns a copy of every ext argument passed to AddPortMapping, in call order.
func (r *recordingMapper) ports() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int, len(r.addPorts))
	copy(out, r.addPorts)
	return out
}

// --- helpers ---

func newOwnedTestPort(fc *fakeClock, mapper PortMapper, extPort, intPort int, idleTimeout time.Duration, descPrefix, intClient string) *OnDemandPort {
	p := &OnDemandPort{
		mapper:      mapper,
		extPort:     extPort,
		intPort:     intPort,
		idleTimeout: idleTimeout,
		renewWindow: 2 * time.Second,
		clock:       fc,
		descPrefix:  descPrefix,
		intClient:   intClient,
		cmds:        make(chan portCommand),
		done:        make(chan struct{}),
		state:       StateClosed,
	}
	go p.loop()
	return p
}

func newTestPort(fc *fakeClock, mapper PortMapper, idleTimeout time.Duration) *OnDemandPort {
	return newOwnedTestPort(fc, mapper, 8443, 8443, idleTimeout, DescriptionPrefix, "")
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within timeout")
}

// --- tests ---

func TestOnDemandPort_ClosedByDefaultAndClamp(t *testing.T) {
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	rm := &recordingMapper{}
	p := newTestPort(fc, rm, time.Minute)
	defer p.Close()

	if p.Open() {
		t.Fatalf("port must be closed by default")
	}
	// A 1ms lease must be clamped up so int(lease.Seconds()) is never 0.
	if err := p.OpenFor("share-1", time.Millisecond); err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if !p.Open() {
		t.Fatalf("port must be open after OpenFor")
	}
	_, _, _, lastLease := rm.snapshot()
	if lastLease < int(minValidLease.Seconds()) {
		t.Fatalf("lease = %d, want >= %d (clamped)", lastLease, int(minValidLease.Seconds()))
	}
}

func TestOnDemandPort_LeaseExpiresUnusedCloses(t *testing.T) {
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	rm := &recordingMapper{}
	p := newTestPort(fc, rm, time.Minute)

	if err := p.OpenFor("share-1", minValidLease); err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	fc.advance(minValidLease + time.Second) // lease expiry fires → start close
	waitFor(t, func() bool { return !p.Open() })
	fc.advance(closeRetryDelay + time.Millisecond) // first delete attempt
	waitFor(t, func() bool { _, c, _, _ := rm.snapshot(); return c >= 1 })
}

func TestOnDemandPort_RenewalUsesGrantedPort(t *testing.T) {
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	rm := &recordingMapper{remap: 52000}
	p := newTestPort(fc, rm, time.Minute)
	defer p.Close()

	if err := p.OpenFor("share-1", 30*time.Second); err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if got := p.GrantedPort(); got != 52000 {
		t.Fatalf("GrantedPort = %d, want 52000", got)
	}
	if _, err := p.BeginSession("share-1"); err != nil {
		t.Fatalf("BeginSession: %v", err)
	}

	// Past renewAt with an active session → the mapping is renewed.
	fc.advance(30*time.Second - 2*time.Second + time.Millisecond)
	waitFor(t, func() bool { return len(rm.ports()) >= 2 })

	ports := rm.ports()
	if last := ports[len(ports)-1]; last != 52000 {
		t.Fatalf("last AddPortMapping ext = %d, want 52000 (granted), not 8443", last)
	}
}

func TestOnDemandPort_ConcurrentSessionsKeepOpenAndRenew(t *testing.T) {
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	rm := &recordingMapper{}
	p := newTestPort(fc, rm, time.Minute)
	defer p.Close()

	if err := p.OpenFor("share-1", 30*time.Second); err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	a, err := p.BeginSession("share-1")
	if err != nil {
		t.Fatalf("BeginSession a: %v", err)
	}
	b, err := p.BeginSession("share-1")
	if err != nil {
		t.Fatalf("BeginSession b: %v", err)
	}

	opened, _, _, _ := rm.snapshot()
	// Just past the renewal point (lease - renewWindow): with sessions active
	// the mapping is renewed (AddPortMapping again) instead of closed.
	fc.advance(30*time.Second - 2*time.Second + time.Millisecond)
	waitFor(t, func() bool { o, _, _, _ := rm.snapshot(); return o > opened })
	if !p.Open() {
		t.Fatalf("port must stay open while sessions are active")
	}

	// Ending one session keeps the port open (another is still active).
	p.EndSession(a)
	if !p.Open() {
		t.Fatalf("port must stay open while a session remains")
	}

	// Ending the last session starts the inactivity/lease close path.
	p.EndSession(b)
	fc.advance(time.Minute + time.Second)
	waitFor(t, func() bool { return !p.Open() })
	fc.advance(closeRetryDelay + time.Millisecond)
	waitFor(t, func() bool { _, c, _, _ := rm.snapshot(); return c >= 1 })
}

func TestOnDemandPort_ActivityPostponesIdleClose(t *testing.T) {
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	rm := &recordingMapper{}
	p := newTestPort(fc, rm, 20*time.Second) // idle timeout 20s

	if err := p.OpenFor("share-1", time.Minute); err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	s, err := p.BeginSession("share-1")
	if err != nil {
		t.Fatalf("BeginSession: %v", err)
	}
	// Idle for 19s (just under the timeout), then activity resets it.
	fc.advance(19 * time.Second)
	p.Activity(s)
	fc.advance(19 * time.Second) // 38s since start, only 19s since activity
	if !p.Open() {
		t.Fatalf("port should still be open after activity reset the idle timer")
	}
	fc.advance(2 * time.Second) // now 21s since activity → idle close
	waitFor(t, func() bool { return !p.Open() })
}

func TestOnDemandPort_CloseIdempotent(t *testing.T) {
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	rm := &recordingMapper{}
	p := newTestPort(fc, rm, time.Minute)

	if err := p.OpenFor("share-1", time.Minute); err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close (idempotent): %v", err)
	}
	_, closed, _, _ := rm.snapshot()
	if closed != 1 {
		t.Fatalf("DeletePortMapping called %d times, want 1", closed)
	}
}

func TestOnDemandPort_CloseRetriesThenSucceeds(t *testing.T) {
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	rm := &recordingMapper{delFails: 2}
	p := newTestPort(fc, rm, time.Minute)
	settled := stateSignal(p, StateClosed)

	if err := p.OpenFor("share-1", time.Minute); err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if err := p.Close(); !errors.Is(err, ErrDeleteRetry) {
		t.Fatalf("want ErrDeleteRetry, got %v", err)
	}
	// Attempt 2 fails; the advance is gated on the loop re-arming.
	fc.advanceGated(t, closeRetryDelay+time.Millisecond, settled)
	if _, _, d, _ := rm.snapshot(); d != 2 {
		t.Fatalf("delete attempts = %d, want 2", d)
	}
	// Attempt 3 succeeds → StateClosed; the gate observes the transition.
	fc.advanceGated(t, closeRetryDelay+time.Millisecond, settled)
	if !channelClosed(settled) {
		t.Fatalf("port did not reach StateClosed after the retries")
	}
	if p.CloseError() != nil {
		t.Fatalf("CloseError = %v, want nil after a successful retry", p.CloseError())
	}
	_, closed, _, _ := rm.snapshot()
	if closed != 1 {
		t.Fatalf("closed = %d, want 1", closed)
	}
}

func TestOnDemandPort_CloseEscalatesAfterMaxAttempts(t *testing.T) {
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	rm := &recordingMapper{alwaysFailDel: true}
	p := newTestPort(fc, rm, time.Minute)
	failed := stateSignal(p, StateCloseFailed)

	if err := p.OpenFor("share-1", time.Minute); err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if err := p.Close(); !errors.Is(err, ErrDeleteRetry) {
		t.Fatalf("want ErrDeleteRetry, got %v", err)
	}
	escalateCloseFailure(t, fc, failed)
	if p.CloseError() == nil {
		t.Fatalf("CloseError must be non-nil after escalation")
	}
	if _, _, d, _ := rm.snapshot(); d < maxCloseAttempts {
		t.Fatalf("delete attempts = %d, want >= %d", d, maxCloseAttempts)
	}
}

func TestOnDemandPort_OpenForFailureDuringCloseKeepsRetrying(t *testing.T) {
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	rm := &recordingMapper{alwaysFailDel: true}
	p := newTestPort(fc, rm, time.Minute)
	defer p.Close()

	if err := p.OpenFor("share-1", time.Minute); err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	// First delete fails → the port enters closing with an armed delete-retry
	// timer (closing=true, open=false).
	if err := p.Close(); !errors.Is(err, ErrDeleteRetry) {
		t.Fatalf("want ErrDeleteRetry, got %v", err)
	}
	fc.advanceGated(t, closeRetryDelay+time.Millisecond, nil)
	if _, _, d, _ := rm.snapshot(); d < 2 {
		t.Fatalf("delete attempts = %d, want >= 2", d)
	}

	// Now a reopen fails to add the mapping. The buggy code cleared `closing`
	// BEFORE AddPortMapping, so on failure the armed delete-retry timer was
	// abandoned (closing=false/open=false, and no case matches anymore). The
	// fix leaves `closing` intact so the retry keeps firing.
	rm.mu.Lock()
	rm.addErr = errors.New("add failed")
	rm.mu.Unlock()
	if err := p.OpenFor("share-2", time.Minute); err == nil {
		t.Fatalf("OpenFor with addErr must fail")
	}
	if p.Open() {
		t.Fatalf("port must stay closed after a failed OpenFor during close")
	}

	_, _, base, _ := rm.snapshot()
	fc.advanceGated(t, closeRetryDelay+time.Millisecond, nil)
	_, _, after, _ := rm.snapshot()
	if after <= base {
		t.Fatalf("delete retry did not fire after failed OpenFor: delCalls %d -> %d", base, after)
	}

	// And it keeps firing on the next tick, proving the retry is re-armed and
	// not a one-off. That tick is the fifth failed attempt, so the loop
	// escalates instead of re-arming; the terminal transition is the gate.
	failed := stateSignal(p, StateCloseFailed)
	fc.advanceGated(t, closeRetryDelay+time.Millisecond, failed)
	if _, _, d, _ := rm.snapshot(); d <= after {
		t.Fatalf("delete retry did not re-arm: delCalls %d -> %d", after, d)
	}
}

func TestOpenForFastPathRenewsOnlyWhenLeaseTooShort(t *testing.T) {
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	rm := &recordingMapper{}
	p := newTestPort(fc, rm, time.Minute)
	defer p.Close()

	if err := p.OpenFor("share-1", 60*time.Second); err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	fc.advance(10 * time.Second) // 50s remaining on the router lease

	// OpenFor(30s) fits within the remaining 50s → must NOT renew.
	if err := p.OpenFor("share-2", 30*time.Second); err != nil {
		t.Fatalf("OpenFor(30s): %v", err)
	}
	if o, _, _, _ := rm.snapshot(); o != 1 {
		t.Fatalf("OpenFor(30s) within remaining lease must not renew: AddPortMapping called %d times, want 1", o)
	}

	// OpenFor(120s) exceeds the remaining 50s → MUST renew.
	if err := p.OpenFor("share-3", 120*time.Second); err != nil {
		t.Fatalf("OpenFor(120s): %v", err)
	}
	if o, _, _, _ := rm.snapshot(); o != 2 {
		t.Fatalf("OpenFor(120s) must renew: AddPortMapping called %d times, want 2", o)
	}
}

func TestColdOpenCleansLingeringMapping(t *testing.T) {
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	// The first maxCloseAttempts deletes fail, so Close escalates to
	// close-failed with the mapping still lingering on the router.
	rm := &recordingMapper{delFails: maxCloseAttempts}
	p := newTestPort(fc, rm, time.Minute)
	defer p.Close()

	if err := p.OpenFor("share-1", time.Minute); err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if err := p.Close(); !errors.Is(err, ErrDeleteRetry) {
		t.Fatalf("Close: want ErrDeleteRetry, got %v", err)
	}
	// Drive the delete-retry timer to escalation (close-failed).
	escalateCloseFailure(t, fc, stateSignal(p, StateCloseFailed))
	if got := p.State(); got != StateCloseFailed {
		t.Fatalf("State = %v, want StateCloseFailed", got)
	}

	// A subsequent cold OpenFor must delete the lingering mapping BEFORE
	// re-selecting and adding the new mapping.
	if err := p.OpenFor("share-2", time.Minute); err != nil {
		t.Fatalf("OpenFor after close-failed: %v", err)
	}
	ev := rm.eventsSnapshot()
	if n := len(ev); n < 2 || ev[n-2] != "del:8443" || ev[n-1] != "add:8443" {
		t.Fatalf("cold open must delete lingering mapping before re-adding; events = %v", ev)
	}
}

func TestOwnershipTokenInDescription(t *testing.T) {
	// Listing-supported mapper with an empty list, so ChooseExternalPort keeps
	// 443 and the description records the per-agent token + port.
	rm := &recordingMapper{listMappings: []PortMapping{}}
	p := NewOnDemandPortOwned(rm, 443, 8443, 5*time.Minute, "sharebridge-abc", "192.168.1.2")
	defer p.Close()

	if err := p.OpenFor("share-1", time.Minute); err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if got := rm.lastDesc(); got != "sharebridge-abc-443" {
		t.Fatalf("description = %q, want sharebridge-abc-443", got)
	}

	want := PortMapping{ExternalPort: 443, InternalPort: 8443, InternalClient: "192.168.1.2", Protocol: "TCP", Description: "sharebridge-abc-443"}

	// DeleteOwnedMapping refuses a mapping whose description differs.
	foreign := &fakeMapper{mappings: map[int]PortMapping{
		443: {ExternalPort: 443, InternalPort: 8443, InternalClient: "192.168.1.2", Protocol: "TCP", Description: "someone-else-443"},
	}}
	if err := DeleteOwnedMapping(foreign, 443, want); err != ErrForeignMapping {
		t.Fatalf("DeleteOwnedMapping (description mismatch) = %v, want ErrForeignMapping", err)
	}

	// ...and one whose internal client (per-agent identity) differs.
	wrongClient := &fakeMapper{mappings: map[int]PortMapping{
		443: {ExternalPort: 443, InternalPort: 8443, InternalClient: "192.168.1.99", Protocol: "TCP", Description: "sharebridge-abc-443"},
	}}
	if err := DeleteOwnedMapping(wrongClient, 443, want); err != ErrForeignMapping {
		t.Fatalf("DeleteOwnedMapping (internal client mismatch) = %v, want ErrForeignMapping", err)
	}
}

// --- transition callback helpers ---

type stateTransition struct {
	from, to    PortState
	grantedPort int
}

// transitionRecorder is a SetTransitionCallback consumer that captures every
// state transition (fired synchronously on the state loop) for assertion.
type transitionRecorder struct {
	mu   sync.Mutex
	seen []stateTransition
}

func (r *transitionRecorder) cb(old, new PortState, grantedPort int) {
	r.mu.Lock()
	r.seen = append(r.seen, stateTransition{old, new, grantedPort})
	r.mu.Unlock()
}

func (r *transitionRecorder) snapshot() []stateTransition {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]stateTransition, len(r.seen))
	copy(out, r.seen)
	return out
}

func assertTransitions(t *testing.T, got, want []stateTransition) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("transitions = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("transition %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// An immediate, successful Close must observe the same StateOpen →
// StateClosing → StateClosed sequence as the timer-driven close path, with the
// granted external port on the open/closing transitions and 0 once closed.
func TestOnDemandPort_StateTransitionsOnImmediateClose(t *testing.T) {
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	rm := &recordingMapper{remap: 52000}
	p := newTestPort(fc, rm, time.Minute)
	defer p.Close()

	rec := &transitionRecorder{}
	p.SetTransitionCallback(rec.cb)

	if err := p.OpenFor("share-1", time.Minute); err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if p.State() != StateOpen {
		t.Fatalf("State = %v, want StateOpen", p.State())
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if p.State() != StateClosed {
		t.Fatalf("State = %v, want StateClosed", p.State())
	}

	assertTransitions(t, rec.snapshot(), []stateTransition{
		{StateClosed, StateOpen, 52000},
		{StateOpen, StateClosing, 52000},
		{StateClosing, StateClosed, 0},
	})
}

// A close whose deletion repeatedly fails must report StateClosing while
// retrying, then StateCloseFailed once retries are exhausted — both with the
// granted external port still in effect.
func TestOnDemandPort_StateCloseFailedTransition(t *testing.T) {
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	rm := &recordingMapper{alwaysFailDel: true, remap: 52000}
	p := newTestPort(fc, rm, time.Minute)
	defer p.Close()

	rec := &transitionRecorder{}
	failed := make(chan struct{})
	var once sync.Once
	p.SetTransitionCallback(func(old, new PortState, granted int) {
		rec.cb(old, new, granted)
		if new == StateCloseFailed {
			once.Do(func() { close(failed) })
		}
	})

	if err := p.OpenFor("share-1", time.Minute); err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if err := p.Close(); !errors.Is(err, ErrDeleteRetry) {
		t.Fatalf("Close: want ErrDeleteRetry, got %v", err)
	}
	if p.State() != StateClosing {
		t.Fatalf("State = %v, want StateClosing while retrying", p.State())
	}
	// Drive the delete-retry timer to escalation (close-failed).
	escalateCloseFailure(t, fc, failed)

	assertTransitions(t, rec.snapshot(), []stateTransition{
		{StateClosed, StateOpen, 52000},
		{StateOpen, StateClosing, 52000},
		{StateClosing, StateCloseFailed, 52000},
	})
}

// Regression: a second OpenFor (a fresh admitted signal) while a session is
// active must not reset the idle deadline to zero — that previously armed an
// immediate close which dropped the active session mid-transfer.
func TestOnDemandPort_SecondOpenForKeepsActiveSession(t *testing.T) {
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	rm := &recordingMapper{}
	p := newTestPort(fc, rm, 30*time.Second) // idle timeout 30s
	defer p.Close()

	if err := p.OpenFor("share-1", time.Minute); err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if _, err := p.BeginSession("share-1"); err != nil {
		t.Fatalf("BeginSession: %v", err)
	}
	// A second signal arrives while the first recipient is mid-transfer.
	if err := p.OpenFor("share-2", time.Minute); err != nil {
		t.Fatalf("second OpenFor: %v", err)
	}
	// Well under the idle timeout the port must stay open (the old bug armed
	// a zero-duration timer that closed it immediately).
	fc.advance(closeRetryDelay + time.Millisecond)
	if !p.Open() {
		t.Fatalf("port must stay open after second OpenFor with an active session")
	}
	// The session's idle deadline still governs: idle past the timeout closes.
	fc.advance(30*time.Second + time.Second)
	waitFor(t, func() bool { return !p.Open() })
}
