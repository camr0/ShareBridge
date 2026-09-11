package directctl

// Task 34 control-side tests: the §17.3 direct-preparation / browser-fallback /
// STUN counters are bounded, metadata-only, and exposed only on a private
// monitoring surface.

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// metricValue extracts the value of one rendered Prometheus series line whose
// text begins with prefix (prefix includes any `{label="value"}` selector).
// The control counters are package-global, so tests must assert DELTAS rather
// than absolute values to be repeat-safe under -count=N.
func metricValue(t *testing.T, text, prefix string) int64 {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, prefix+" ") {
			continue
		}
		value, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, prefix)), 10, 64)
		if err != nil {
			t.Fatalf("series %q has a non-integer value: %q", prefix, line)
		}
		return value
	}
	t.Fatalf("series %q not present in:\n%s", prefix, text)
	return 0
}

func TestControlRelaySignalsAreBoundedAndPrivate(t *testing.T) {
	base := RelayMetricsText()
	// Hostile, user-derived values must never create a series: the label value
	// set is fixed by the enumerated constants.
	for _, hostile := range []string{
		"share-code-tYwYQPI0",
		"425bcaea60e1.sbd0903ed4.sharebridgeusercontent.com",
		"203.0.113.9",
		"credential-jti-0123456789abcdef",
		"/s/tYwYQPI0/connect",
	} {
		recordPreparation(hostile)
		recordSTUN(hostile)
	}
	if after := RelayMetricsText(); after != base {
		t.Fatalf("hostile label values changed the bounded control metric surface:\nbefore:\n%s\nafter:\n%s", base, after)
	}

	recordPreparation(metricPreparationDirect)
	recordPreparation(metricPreparationRelay)
	recordPreparation(metricPreparationUnavailable)
	recordSTUN(metricSTUNMatch)
	recordSTUN(metricSTUNMismatch)
	recordSTUN(metricSTUNTimeout)

	text := RelayMetricsText()
	// A relay fallback is the browser-fallback decision: exactly one delta each.
	checks := []struct {
		series string
	}{
		{`sharebridge_relay_direct_preparation_total{outcome="direct"}`},
		{`sharebridge_relay_direct_preparation_total{outcome="relay"}`},
		{`sharebridge_relay_direct_preparation_total{outcome="unavailable"}`},
		{"sharebridge_relay_browser_fallback_total"},
		{`sharebridge_relay_stun_total{outcome="match"}`},
		{`sharebridge_relay_stun_total{outcome="mismatch"}`},
		{`sharebridge_relay_stun_total{outcome="timeout"}`},
	}
	for _, check := range checks {
		before := metricValue(t, base, check.series)
		after := metricValue(t, text, check.series)
		if after-before != 1 {
			t.Errorf("series %q delta = %d, want 1 (before=%d after=%d)", check.series, after-before, before, after)
		}
	}
}

func TestControlMetricsHandlerIsPrivateOnly(t *testing.T) {
	handler := RelayMetricsHandler()
	cases := []struct {
		name       string
		remote     string
		host       string
		wantStatus int
	}{
		{"loopback", "127.0.0.1:5555", "127.0.0.1:9102", http.StatusOK},
		{"localhost", "127.0.0.1:5555", "localhost:9102", http.StatusOK},
		{"public host", "127.0.0.1:5555", "control.sharebridgeusercontent.com", http.StatusForbidden},
		{"public peer", "203.0.113.9:40000", "127.0.0.1:9102", http.StatusForbidden},
		{"empty host", "127.0.0.1:5555", "", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/metrics", nil)
			request.RemoteAddr = tc.remote
			request.Host = tc.host
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%q)", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
		})
	}

	if _, err := BindMetricsLoopback("0.0.0.0:9102"); err == nil {
		t.Fatal("BindMetricsLoopback accepted a public bind")
	}
	if _, err := BindMetricsLoopback("127.0.0.1:9102"); err != nil {
		t.Fatalf("BindMetricsLoopback rejected loopback: %v", err)
	}
}
