package signaling

import (
	"testing"
	"time"
)

func TestBackoff_FirstCall(t *testing.T) {
	b := NewBackoff()
	d := b.Next()

	// First call should return ~1s ±20% (800ms to 1200ms)
	min := time.Duration(float64(initialBackoff) * (1 - jitter))
	max := time.Duration(float64(initialBackoff) * (1 + jitter))

	if d < min || d > max {
		t.Errorf("First backoff duration %v outside expected range [%v, %v]", d, min, max)
	}
}

func TestBackoff_Doubles(t *testing.T) {
	b := NewBackoff()

	// First call
	first := b.Next()
	// Second call should be ~2x the base (before jitter)
	second := b.Next()

	// Base for second attempt is 2s, with jitter ±20% => 1.6s to 2.4s
	expectedBase := 2 * initialBackoff
	min := time.Duration(float64(expectedBase) * (1 - jitter))
	max := time.Duration(float64(expectedBase) * (1 + jitter))

	if second < min || second > max {
		t.Errorf("Second backoff duration %v outside expected range [%v, %v]", second, min, max)
	}

	// Sanity check: second should be greater than first (with high probability)
	// This could occasionally fail due to jitter, but that's expected
	if second <= first {
		t.Logf("Warning: second duration %v <= first duration %v (possible due to jitter)", second, first)
	}
}

func TestBackoff_Cap(t *testing.T) {
	b := NewBackoff()

	// Call Next() many times to reach max cap
	var last time.Duration
	for i := 0; i < 10; i++ {
		last = b.Next()
	}

	// After many attempts, should be capped at maxBackoff with jitter
	// maxBackoff = 30s, with ±20% jitter => max possible is 36s
	maxPossible := time.Duration(float64(maxBackoff) * (1 + jitter))

	if last > maxPossible {
		t.Errorf("Backoff duration %v exceeds max possible %v (capped at %v with jitter)", last, maxPossible, maxBackoff)
	}

	// Also verify it's actually using the max (with jitter it should be close)
	minExpected := time.Duration(float64(maxBackoff) * (1 - jitter))
	if last < minExpected {
		t.Errorf("Backoff duration %v less than expected min %v for capped backoff", last, minExpected)
	}
}

func TestBackoff_Reset(t *testing.T) {
	b := NewBackoff()

	// Call Next() several times
	for i := 0; i < 5; i++ {
		b.Next()
	}

	// Reset
	b.Reset()

	// Next call should be back to ~1s ±20%
	d := b.Next()
	min := time.Duration(float64(initialBackoff) * (1 - jitter))
	max := time.Duration(float64(initialBackoff) * (1 + jitter))

	if d < min || d > max {
		t.Errorf("Post-reset backoff duration %v outside expected range [%v, %v]", d, min, max)
	}
}
