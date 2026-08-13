package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestReportJSONRoundTrip(t *testing.T) {
	r := rawResult{Mode: "raw", RTT: 25, SentBytes: 1024, Received: 1024, Mbps: 12.5}
	var sb strings.Builder
	if err := writeJSON(&sb, r); err != nil {
		t.Fatal(err)
	}
	var back rawResult
	if err := json.Unmarshal([]byte(sb.String()), &back); err != nil {
		t.Fatal(err)
	}
	if back != r {
		t.Fatalf("round trip mismatch: %+v != %+v", back, r)
	}
}

func TestHumanSummary(t *testing.T) {
	r := rawResult{Mode: "prod", RTT: 50, SentBytes: 1024, Received: 1024, Mbps: 3.2}
	s := humanSummary(r)
	if !strings.Contains(s, "prod") || !strings.Contains(s, "3.2") {
		t.Fatalf("unexpected summary: %s", s)
	}
}

func TestParseByteSize(t *testing.T) {
	cases := map[string]int64{
		"1024":  1024,
		"16KiB": 16 << 10,
		"8MiB":  8 << 20,
	}
	for input, want := range cases {
		got, err := parseByteSize(input)
		if err != nil {
			t.Fatalf("parseByteSize(%q) error: %v", input, err)
		}
		if got != want {
			t.Fatalf("parseByteSize(%q) = %d, want %d", input, got, want)
		}
	}
}

func TestValidateRunConfig(t *testing.T) {
	t.Run("rejects invalid loss", func(t *testing.T) {
		if err := validateRunConfig(runConfig{loss: -0.1, backpressure: "event"}); err == nil {
			t.Fatal("expected error for negative loss")
		}
		if err := validateRunConfig(runConfig{loss: 1.1, backpressure: "event"}); err == nil {
			t.Fatal("expected error for loss > 1")
		}
	})

	t.Run("rejects invalid backpressure", func(t *testing.T) {
		if err := validateRunConfig(runConfig{loss: 0.5, backpressure: "burst"}); err == nil {
			t.Fatal("expected error for invalid backpressure")
		}
	})
}
