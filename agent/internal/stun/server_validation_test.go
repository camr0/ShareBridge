package stun

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestParseChallengeServerHostContract pins the shared §10.1 stun_challenge
// `server` host contract. It mirrors control's STUNAdvertise validator: the
// host is either an IPv4 literal or an RFC 1123 hostname (1-63 byte labels of
// ASCII letters/digits with interior hyphens only), bounded to 253 bytes.
// Empty hosts, leading/trailing hyphens, a trailing dot, IPv6 literals and
// characters outside the hostname set all fail closed with
// ErrMalformedChallenge before any network activity.
func TestParseChallengeServerHostContract(t *testing.T) {
	longLabel := strings.Repeat("a", 64) + ".example.net"
	overLongHost := strings.Join([]string{
		strings.Repeat("a", 63),
		strings.Repeat("b", 63),
		strings.Repeat("c", 63),
		strings.Repeat("d", 63),
	}, ".")

	cases := []struct {
		name    string
		host    string
		wantErr bool
	}{
		// Rejections: shapes the previous validator accepted.
		{"empty host", "", true},
		{"empty host with port only", ":3478", true},
		{"leading hyphen", "-control.example.net", true},
		{"trailing hyphen", "control-.example.net", true},
		{"leading hyphen in interior label", "control.-bad.example.net", true},
		{"trailing hyphen in interior label", "control.bad-.example.net", true},
		{"trailing dot", "control.example.net.", true},
		{"underscore label", "control_sync.example.net", true},
		{"over-length label", longLabel, true},
		{"over-length host", overLongHost, true},
		{"IPv6 literal", "[2001:db8::1]", true},
		{"space in host", "contr ol.example.net", true},
		{"empty label from double dot", "control..example.net", true},

		// Acceptances: legitimate shapes must keep working.
		{"embedded hyphen", "control-sync.example.net", false},
		{"multi-label", "a.b.c.example.net", false},
		{"single label", "localhost", false},
		{"uppercase", "Control.Example.NET", false},
		{"max-length label", strings.Repeat("a", 63) + ".example.net", false},
		{"IPv4 literal", "203.0.113.10", false},
		{"all-numeric hostname", "999.1.1.1", false},
	}

	now := time.Unix(1_700_000_000, 0)
	expiry := now.Add(30 * time.Second)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := tc.host + ":3478"
			challenge, err := ParseChallenge(ChallengeVersion, testChallengeField(), server, expiry, now)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("server %q accepted, want ErrMalformedChallenge", server)
				}
				if !errors.Is(err, ErrMalformedChallenge) {
					t.Fatalf("server %q error = %v, want ErrMalformedChallenge", server, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("server %q rejected: %v", server, err)
			}
			if challenge.server != server {
				t.Fatalf("challenge.server = %q, want %q", challenge.server, server)
			}
		})
	}
}
