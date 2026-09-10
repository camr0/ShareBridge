package direct

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// gatedListMapper blocks ListPortMappings (the port-selection router I/O of a
// cold open) behind a channel, so a test can advance the caller's generation
// while OpenForIf is inside ChooseExternalPort. It otherwise behaves like
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

// TestOpenForIfFencesColdOpenAfterPortSelection proves the fence is
// re-evaluated on the state loop as the last thing before the cold-open mapping
// is created: a generation that advances during ChooseExternalPort's router I/O
// must still prevent the mapping.
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

	var mu sync.Mutex
	current := true
	fence := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return current
	}

	errCh := make(chan error, 1)
	go func() { errCh <- p.OpenForIf("share", time.Minute, fence) }()

	select {
	case <-mapper.listEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("OpenForIf never reached port selection")
	}
	// The generation advances while the state loop is inside router I/O.
	mu.Lock()
	current = false
	mu.Unlock()
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

// TestOpenForIfFencesRenewal proves a stale caller cannot extend the lease of
// an already-open mapping: the top-of-handler fence refuses the renewal before
// any AddPortMapping, and the existing mapping is left for lockdown's CloseIf to
// remove.
func TestOpenForIfFencesRenewal(t *testing.T) {
	fc := newFakeClock(time.Now())
	base := &recordingMapper{}
	p := newTestPort(fc, base, time.Minute)
	defer p.Close()

	var mu sync.Mutex
	current := true
	fence := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return current
	}

	if err := p.OpenForIf("share", time.Minute, fence); err != nil {
		t.Fatalf("legitimate open: %v", err)
	}
	if opened, _, _, _ := base.snapshot(); opened != 1 {
		t.Fatalf("open calls = %d, want 1", opened)
	}
	// Move past the renewal decision point so a stale OpenForIf would renew.
	fc.advance(3 * time.Second)

	mu.Lock()
	current = false
	mu.Unlock()
	if err := p.OpenForIf("share", 5*time.Minute, fence); !errors.Is(err, ErrOpenSuperseded) {
		t.Fatalf("renewal err = %v, want ErrOpenSuperseded", err)
	}
	if opened, _, _, _ := base.snapshot(); opened != 1 {
		t.Fatalf("fenced renewal issued %d AddPortMapping calls total, want 1", opened)
	}
	if !p.Open() {
		t.Fatalf("the pre-existing mapping must stay until lockdown's CloseIf removes it")
	}

	// A fenced open must not wedge the loop: a later legitimate renewal works.
	if err := p.OpenForIf("share", 5*time.Minute, func() bool { return true }); err != nil {
		t.Fatalf("post-fence legitimate renewal: %v", err)
	}
	if opened, _, _, _ := base.snapshot(); opened != 2 {
		t.Fatalf("post-fence renewal open calls = %d, want 2", opened)
	}
}

// TestOpenForIfLegitimateOpenAndRenewal is the no-false-fencing control: with a
// fence that stays true, a cold open and a renewal both issue mappings.
func TestOpenForIfLegitimateOpenAndRenewal(t *testing.T) {
	fc := newFakeClock(time.Now())
	base := &recordingMapper{}
	p := newTestPort(fc, base, time.Minute)
	defer p.Close()

	fence := func() bool { return true }
	if err := p.OpenForIf("share", time.Minute, fence); err != nil {
		t.Fatalf("open: %v", err)
	}
	fc.advance(3 * time.Second)
	if err := p.OpenForIf("share", 5*time.Minute, fence); err != nil {
		t.Fatalf("renewal: %v", err)
	}
	if opened, _, _, _ := base.snapshot(); opened != 2 {
		t.Fatalf("open calls = %d, want 2", opened)
	}
	if !p.Open() {
		t.Fatalf("port must be open")
	}
}

// TestOpenForIfNilFenceIsUnconditional pins that a nil fence (i.e. plain
// OpenFor) is never refused.
func TestOpenForIfNilFenceIsUnconditional(t *testing.T) {
	fc := newFakeClock(time.Now())
	base := &recordingMapper{}
	p := newTestPort(fc, base, time.Minute)
	defer p.Close()

	if err := p.OpenForIf("share", time.Minute, nil); err != nil {
		t.Fatalf("nil-fence open: %v", err)
	}
	if opened, _, _, _ := base.snapshot(); opened != 1 {
		t.Fatalf("open calls = %d, want 1", opened)
	}
}
