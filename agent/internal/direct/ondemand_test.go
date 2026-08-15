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
}

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{now: start} }

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) NewTimer(d time.Duration) portTimer {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTimer{c: make(chan time.Time, 1), at: f.now.Add(d)}
	f.timers = append(f.timers, t)
	return t
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
	addPorts       []int // ext argument of every AddPortMapping call
}

func (r *recordingMapper) AddPortMapping(ext, internal int, desc string, lease int) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.opened++
	r.lastLease = lease
	r.addPorts = append(r.addPorts, ext)
	if r.remap != 0 {
		return r.remap, nil
	}
	return ext, nil
}
func (r *recordingMapper) DeletePortMapping(ext int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.delCalls++
	if r.alwaysFailDel || r.delCalls <= r.delFails {
		return fmt.Errorf("delete failed (attempt %d)", r.delCalls)
	}
	r.closed++
	return nil
}
func (r *recordingMapper) ExternalIP() (string, error) { return "203.0.113.7", nil }

func (r *recordingMapper) ListPortMappings() ([]PortMapping, error) {
	return nil, ErrListingUnsupported
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

func newTestPort(fc *fakeClock, mapper PortMapper, idleTimeout time.Duration) *OnDemandPort {
	p := &OnDemandPort{
		mapper:      mapper,
		extPort:     8443,
		intPort:     8443,
		idleTimeout: idleTimeout,
		renewWindow: 2 * time.Second,
		clock:       fc,
		cmds:        make(chan portCommand),
		done:        make(chan struct{}),
	}
	go p.loop()
	return p
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

	if err := p.OpenFor("share-1", time.Minute); err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if err := p.Close(); !errors.Is(err, ErrDeleteRetry) {
		t.Fatalf("want ErrDeleteRetry, got %v", err)
	}
	fc.advance(closeRetryDelay + time.Millisecond)
	waitFor(t, func() bool { _, _, d, _ := rm.snapshot(); return d >= 2 })
	fc.advance(closeRetryDelay + time.Millisecond)
	waitFor(t, func() bool { return p.CloseError() == nil })
	_, closed, _, _ := rm.snapshot()
	if closed != 1 {
		t.Fatalf("closed = %d, want 1", closed)
	}
}

func TestOnDemandPort_CloseEscalatesAfterMaxAttempts(t *testing.T) {
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	rm := &recordingMapper{alwaysFailDel: true}
	p := newTestPort(fc, rm, time.Minute)

	if err := p.OpenFor("share-1", time.Minute); err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if err := p.Close(); !errors.Is(err, ErrDeleteRetry) {
		t.Fatalf("want ErrDeleteRetry, got %v", err)
	}
	for i := 0; i < maxCloseAttempts; i++ {
		fc.advance(closeRetryDelay + time.Millisecond)
		// Yield to the single state-loop goroutine so it processes the fired
		// retry timer and re-arms the next one before the clock advances
		// again. The final tick fires no timer (escalation disarms), so the
		// wait is capped at maxCloseAttempts.
		waitFor(t, func() bool { _, _, d, _ := rm.snapshot(); return d >= i+2 || d >= maxCloseAttempts })
	}
	waitFor(t, func() bool { _, _, d, _ := rm.snapshot(); return d >= maxCloseAttempts })
	if p.CloseError() == nil {
		t.Fatalf("CloseError must be non-nil after escalation")
	}
}
