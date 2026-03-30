package signaling

import (
	"math"
	"math/rand"
	"sync"
	"time"
)

const (
	initialBackoff = 1 * time.Second
	maxBackoff     = 30 * time.Second
	backoffFactor  = 2.0
	jitter         = 0.2
)

// Backoff implements exponential backoff with jitter for reconnection delays.
type Backoff struct {
	mu      sync.Mutex
	attempt int
}

// NewBackoff creates a new Backoff instance.
func NewBackoff() *Backoff {
	return &Backoff{}
}

// Next returns the next backoff duration and increments the attempt counter.
// It calculates min(initial * factor^attempt, max) * (1 ± jitter).
func (b *Backoff) Next() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Calculate exponential backoff: initial * factor^attempt
	duration := time.Duration(float64(initialBackoff) * math.Pow(backoffFactor, float64(b.attempt)))

	// Cap at max backoff
	if duration > maxBackoff {
		duration = maxBackoff
	}

	// Apply jitter: duration * (1 ± jitter)
	jitterFactor := 1.0 + jitter*(2.0*rand.Float64()-1.0)
	duration = time.Duration(float64(duration) * jitterFactor)

	b.attempt++
	return duration
}

// Reset resets the attempt counter to zero.
func (b *Backoff) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.attempt = 0
}
