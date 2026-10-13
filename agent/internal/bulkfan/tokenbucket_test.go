package bulkfan

import (
	"testing"
	"time"
)

// fakeClock is a manual clock for deterministic pacing tests.
type fakeClock struct{ t time.Time }

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Unix(0, 0)}
}

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func TestTokenBucketDisabledWhenRateNonPositive(t *testing.T) {
	clock := newFakeClock()
	b := NewTokenBucket(0, 4096, clock.Now)
	if got := b.Delay(4096); got != 0 {
		t.Fatalf("Delay with rate 0 = %v, want 0", got)
	}
	var nilBucket *TokenBucket
	if got := nilBucket.Delay(1024); got != 0 {
		t.Fatalf("nil bucket Delay = %v, want 0", got)
	}
}

func TestTokenBucketBurstThenPace(t *testing.T) {
	clock := newFakeClock()
	// 1 KB/s with a 1 KB burst: the first KB is free, the second costs 1 s.
	b := NewTokenBucket(1024, 1024, clock.Now)

	if got := b.Delay(1024); got != 0 {
		t.Fatalf("first Delay(1024) = %v, want 0 (burst)", got)
	}
	if got := b.Delay(1024); got != time.Second {
		t.Fatalf("second Delay(1024) = %v, want 1s", got)
	}
	// Sustained rate: the reservation consumed the tokens earned during the
	// wait, so after the caller sleeps and returns the next chunk waits again.
	clock.Advance(time.Second)
	if got := b.Delay(1024); got != time.Second {
		t.Fatalf("steady-state Delay = %v, want 1s (1 KB/s sustained)", got)
	}
}

func TestTokenBucketRefillsFromClock(t *testing.T) {
	clock := newFakeClock()
	b := NewTokenBucket(1000, 1000, clock.Now)
	if got := b.Delay(1000); got != 0 {
		t.Fatalf("burst Delay = %v, want 0", got)
	}
	clock.Advance(500 * time.Millisecond)
	// 500 tokens accumulated: 500 byte send is free, the next 500 costs 500ms.
	if got := b.Delay(500); got != 0 {
		t.Fatalf("Delay(500) after half-second refill = %v, want 0", got)
	}
	if got := b.Delay(500); got != 500*time.Millisecond {
		t.Fatalf("Delay(500) with empty bucket = %v, want 500ms", got)
	}
}

func TestTokenBucketCapsAtBurst(t *testing.T) {
	clock := newFakeClock()
	b := NewTokenBucket(1000, 1000, clock.Now)
	clock.Advance(10 * time.Second) // far more than the burst
	if got := b.Delay(1000); got != 0 {
		t.Fatalf("Delay after long idle = %v, want 0 (capped at burst)", got)
	}
	if got := b.Delay(1001); got == 0 {
		t.Fatalf("chunk larger than burst should still be paced")
	}
}

func TestTokenBucketZeroLengthIsFree(t *testing.T) {
	clock := newFakeClock()
	b := NewTokenBucket(1, 1, clock.Now)
	if got := b.Delay(0); got != 0 {
		t.Fatalf("Delay(0) = %v, want 0", got)
	}
}

func TestTokenBucketSustainedRateAccounting(t *testing.T) {
	clock := newFakeClock()
	const rate = 64 * 1024 // bytes per second
	const chunk = 16 * 1024
	b := NewTokenBucket(rate, rate, clock.Now) // 1 s burst

	const chunks = 40
	totalDelay := time.Duration(0)
	for i := 0; i < chunks; i++ {
		totalDelay += b.Delay(chunk)
	}
	// The burst covers the first second of data for free; every byte after it
	// must be paid for at the configured rate. With a frozen clock the
	// accounting is deterministic.
	ideal := time.Duration(float64(chunks*chunk-int(rate)) / rate * float64(time.Second))
	if diff := totalDelay - ideal; diff < -time.Millisecond || diff > time.Millisecond {
		t.Fatalf("total pacing delay = %v, want %v (+/- 1ms)", totalDelay, ideal)
	}
}
