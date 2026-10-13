package bulkfan

import "time"

// TokenBucket paces sends at a fixed byte rate (candidate A3). The path
// polices at roughly 249 Mbps with 17% loss when offered 300 Mbps (E32), so an
// unpaced bursting sender can manufacture its own loss; pacing at ~1.2x the
// delivered rate keeps the burst under the policer.
//
// The bucket is not safe for concurrent use: one bucket serves one transfer.
type TokenBucket struct {
	rate   float64 // tokens (bytes) per second
	burst  float64 // bucket capacity in tokens
	tokens float64
	last   time.Time
	now    func() time.Time
}

// NewTokenBucket returns a bucket that accumulates bytesPerSecond tokens per
// second up to burstBytes. burstBytes below one byte is clamped to one. A
// non-positive rate disables the bucket (every Delay returns 0).
func NewTokenBucket(bytesPerSecond, burstBytes float64, now func() time.Time) *TokenBucket {
	if now == nil {
		now = time.Now
	}
	if burstBytes < 1 {
		burstBytes = 1
	}
	return &TokenBucket{
		rate:   bytesPerSecond,
		burst:  burstBytes,
		tokens: burstBytes,
		last:   now(),
		now:    now,
	}
}

// Delay reports how long the caller must wait before n bytes may leave, and
// reserves those tokens. It returns 0 when pacing is disabled or the bucket
// already holds enough credit.
func (b *TokenBucket) Delay(n int) time.Duration {
	if b == nil || b.rate <= 0 || n <= 0 {
		return 0
	}
	b.refill()

	need := float64(n)
	if need > b.burst {
		// A single chunk larger than the bucket can never be satisfied by
		// waiting alone; send it as soon as the bucket is full.
		b.tokens = 0
		return time.Duration((need - b.burst) / b.rate * float64(time.Second))
	}
	if b.tokens >= need {
		b.tokens -= need
		return 0
	}

	deficit := need - b.tokens
	b.tokens = 0
	wait := time.Duration(deficit / b.rate * float64(time.Second))
	// Advance the accounting clock past the reservation so the next refill
	// starts from the moment the tokens are actually spent.
	b.last = b.last.Add(wait)
	return wait
}

// Rate reports the configured rate in bytes per second.
func (b *TokenBucket) Rate() float64 { return b.rate }

func (b *TokenBucket) refill() {
	now := b.now()
	elapsed := now.Sub(b.last)
	if elapsed <= 0 {
		return
	}
	b.tokens += elapsed.Seconds() * b.rate
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
	b.last = now
}
