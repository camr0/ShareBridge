// agent/internal/direct/range_test.go
package direct

import "testing"

func TestParseRange(t *testing.T) {
	tests := []struct {
		name        string
		h           string
		total       int64
		known       bool
		wantStart   int64
		wantEnd     int64
		wantPartial bool
		wantOK      bool
	}{
		{
			name:        "closed range",
			h:           "bytes=0-99",
			total:       1000,
			known:       true,
			wantStart:   0,
			wantEnd:     99,
			wantPartial: true,
			wantOK:      true,
		},
		{
			name:        "open-ended range",
			h:           "bytes=100-",
			total:       1000,
			known:       true,
			wantStart:   100,
			wantEnd:     999,
			wantPartial: true,
			wantOK:      true,
		},
		{
			name:  "suffix range",
			h:     "bytes=-500",
			total: 1000,
			known: true,
		},
		{
			name:  "multiple ranges",
			h:     "bytes=0-99,100-199",
			total: 1000,
			known: true,
		},
		{
			name:  "non-numeric start",
			h:     "bytes=abc-99",
			total: 1000,
			known: true,
		},
		{
			name:  "non-numeric end",
			h:     "bytes=0-xyz",
			total: 1000,
			known: true,
		},
		{
			name:  "unknown total ignored",
			h:     "bytes=0-99",
			total: 1000,
			known: false,
		},
		{
			name:  "authoritative zero",
			h:     "bytes=0-99",
			total: 0,
			known: true,
		},
		{
			name:  "start at or beyond total",
			h:     "bytes=1000-",
			total: 1000,
			known: true,
		},
		{
			name:        "end clamped to total-1",
			h:           "bytes=0-99999",
			total:       1000,
			known:       true,
			wantStart:   0,
			wantEnd:     999,
			wantPartial: true,
			wantOK:      true,
		},
		{
			name:  "start overflow",
			h:     "bytes=9223372036854775808-",
			total: 1000,
			known: true,
		},
		{
			name:  "end overflow",
			h:     "bytes=0-9223372036854775808",
			total: 1000,
			known: true,
		},
		{
			name:        "case and whitespace tolerant",
			h:           "  Bytes = 0-99 ",
			total:       1000,
			known:       true,
			wantStart:   0,
			wantEnd:     99,
			wantPartial: true,
			wantOK:      true,
		},
		{
			name:  "start greater than end",
			h:     "bytes=50-10",
			total: 1000,
			known: true,
		},
		{
			name:  "missing bytes prefix",
			h:     "0-99",
			total: 1000,
			known: true,
		},
		{
			name:  "empty header",
			h:     "",
			total: 1000,
			known: true,
		},
		{
			name:        "single byte range",
			h:           "bytes=999-999",
			total:       1000,
			known:       true,
			wantStart:   999,
			wantEnd:     999,
			wantPartial: true,
			wantOK:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end, partial, ok := ParseRange(tt.h, tt.total, tt.known)
			if ok != tt.wantOK {
				t.Fatalf("ok: ParseRange(%q, %d, %v) = ok %v, want %v (start=%d end=%d partial=%v)",
					tt.h, tt.total, tt.known, ok, tt.wantOK, start, end, partial)
			}
			if partial != tt.wantPartial {
				t.Fatalf("partial: got %v, want %v", partial, tt.wantPartial)
			}
			if start != tt.wantStart {
				t.Fatalf("start: got %d, want %d", start, tt.wantStart)
			}
			if end != tt.wantEnd {
				t.Fatalf("end: got %d, want %d", end, tt.wantEnd)
			}
		})
	}
}
