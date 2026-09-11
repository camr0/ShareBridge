package clienthello

import (
	"errors"
	"net"
	"testing"
	"time"
)

// peekBoundsInput feeds input into one end of an in-memory pipe and runs
// PeekWithBounds on the other end, mirroring peekInput for the configurable
// §14 bounds.
func peekBoundsInput(t *testing.T, input []byte, maxBytes int, readTimeout time.Duration, closeAfterWrite bool) (*Hello, error) {
	t.Helper()
	gatewaySide, clientSide := net.Pipe()
	t.Cleanup(func() {
		gatewaySide.Close()
		clientSide.Close()
	})
	go func() {
		written := 0
		for written < len(input) {
			count, err := clientSide.Write(input[written:])
			if err != nil {
				return
			}
			written += count
		}
		if closeAfterWrite {
			clientSide.Close()
		}
	}()
	return PeekWithBounds(gatewaySide, maxBytes, readTimeout)
}

// TestParserPeekWithBoundsHonorsConfiguredBounds proves the §14 hello byte
// and read-time bounds are real parser inputs, not inert configuration
// fields: a lowered budget actually rejects a hello the default budget would
// accept, and a lowered deadline actually fires. Invalid bounds fail closed
// with ErrInvalidBound rather than silently falling back to the 64 KiB/5 s
// defaults (main.go validates configuration up front; this is defense in
// depth).
func TestParserPeekWithBoundsHonorsConfiguredBounds(t *testing.T) {
	input := fixtureRecord(t, "tls12-clienthello.hex")

	t.Run("default-equivalent bounds accept the fixture", func(t *testing.T) {
		hello, err := peekBoundsInput(t, input, MaxBufferedBytes, ReadTimeout, true)
		if err != nil {
			t.Fatalf("PeekWithBounds(default bounds) = %v, want nil", err)
		}
		if hello.SNI != expectedSNI {
			t.Fatalf("SNI = %q, want %q", hello.SNI, expectedSNI)
		}
	})

	t.Run("a sufficient non-default byte budget accepts the fixture", func(t *testing.T) {
		if _, err := peekBoundsInput(t, input, len(input), ReadTimeout, true); err != nil {
			t.Fatalf("PeekWithBounds(%d bytes) = %v, want nil (the whole hello fits)", len(input), err)
		}
	})

	t.Run("a lowered byte budget is enforced", func(t *testing.T) {
		_, err := peekBoundsInput(t, input, len(input)-1, ReadTimeout, true)
		if !errors.Is(err, ErrTooLarge) {
			t.Fatalf("PeekWithBounds(%d bytes) error = %v, want %v", len(input)-1, err, ErrTooLarge)
		}
	})

	t.Run("a lowered read deadline is enforced", func(t *testing.T) {
		started := time.Now()
		_, err := peekBoundsInput(t, nil, MaxBufferedBytes, 50*time.Millisecond, false)
		if !errors.Is(err, ErrTimeout) {
			t.Fatalf("PeekWithBounds(50ms) error = %v, want %v", err, ErrTimeout)
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("PeekWithBounds waited %v, ignoring the configured 50ms deadline", elapsed)
		}
	})

	t.Run("invalid bounds fail closed", func(t *testing.T) {
		for _, maxBytes := range []int{-1, 0, MaxBufferedBytes + 1} {
			if _, err := peekBoundsInput(t, input, maxBytes, ReadTimeout, true); !errors.Is(err, ErrInvalidBound) {
				t.Fatalf("PeekWithBounds(maxBytes=%d) error = %v, want %v", maxBytes, err, ErrInvalidBound)
			}
		}
		for _, readTimeout := range []time.Duration{0, -time.Second} {
			if _, err := peekBoundsInput(t, input, MaxBufferedBytes, readTimeout, true); !errors.Is(err, ErrInvalidBound) {
				t.Fatalf("PeekWithBounds(timeout=%v) error = %v, want %v", readTimeout, err, ErrInvalidBound)
			}
		}
	})
}
