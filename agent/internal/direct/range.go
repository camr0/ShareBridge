// agent/internal/direct/range.go
package direct

import (
	"strconv"
	"strings"
)

// ParseRange parses a single HTTP byte-range request header value for a
// resource whose authoritative length is total.
//
// It returns:
//   - start, end: the inclusive byte offsets of the requested range, with end
//     clamped to total-1. Meaningful only when ok is true.
//   - partial: whether the response should be a 206 Partial Content. Per §4.2 a
//     valid range always yields 206, so partial is true whenever ok is true.
//   - ok: whether the header names a single, syntactically valid, satisfiable
//     range under an authoritative non-zero total.
//
// ok is false for: unknown totals (known=false — the caller ignores Range and
// serves 200), authoritative zero length (known=true, total=0 — the caller
// replies 416), suffix ranges (bytes=-N), multiple ranges, non-numeric or
// overflowing values, start >= total, and start > end after clamping.
func ParseRange(h string, total int64, known bool) (start, end int64, partial bool, ok bool) {
	if !known {
		return 0, 0, false, false
	}
	if total <= 0 {
		return 0, 0, false, false
	}

	s := strings.TrimSpace(h)
	// Case-insensitive "bytes" prefix, optional whitespace, then "=".
	if len(s) < 6 || !strings.EqualFold(s[:5], "bytes") {
		return 0, 0, false, false
	}
	s = strings.TrimSpace(s[5:])
	if len(s) == 0 || s[0] != '=' {
		return 0, 0, false, false
	}
	s = strings.TrimSpace(s[1:])
	if len(s) == 0 {
		return 0, 0, false, false
	}

	// Multiple ranges are not supported (§4.2).
	if strings.Contains(s, ",") {
		return 0, 0, false, false
	}

	// The first '-' separates start and end; a dash at index 0 is a suffix
	// range (bytes=-N), which is also unsupported.
	dash := strings.IndexByte(s, '-')
	if dash < 1 {
		return 0, 0, false, false
	}
	startStr := s[:dash]
	endStr := s[dash+1:]

	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil || start < 0 {
		return 0, 0, false, false
	}
	if start >= total {
		return 0, 0, false, false
	}

	if endStr == "" {
		end = total - 1
	} else {
		end, err = strconv.ParseInt(endStr, 10, 64)
		if err != nil || end < 0 {
			return 0, 0, false, false
		}
		if end >= total {
			end = total - 1
		}
	}

	if start > end {
		return 0, 0, false, false
	}

	return start, end, true, true
}
