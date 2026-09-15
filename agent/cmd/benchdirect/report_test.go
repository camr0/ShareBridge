package main

import (
	"encoding/json"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
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
	if !reflect.DeepEqual(back, r) {
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
	valid := runConfig{mode: "raw", rttMs: 25, loss: 0.5, size: 8 << 20, chunk: 16 << 10, backpressure: "event", deadline: 30 * time.Second, window: 5 << 20, conns: 1, sharing: "shared"}
	cases := []struct {
		name    string
		mutate  func(*runConfig)
		wantErr bool
	}{
		{"valid", func(*runConfig) {}, false},
		{"negative rtt", func(c *runConfig) { c.rttMs = -1 }, true},
		{"zero size", func(c *runConfig) { c.size = 0 }, true},
		{"negative size", func(c *runConfig) { c.size = -8 }, true},
		{"zero chunk", func(c *runConfig) { c.chunk = 0 }, true},
		{"negative chunk", func(c *runConfig) { c.chunk = -16 }, true},
		{"nan loss", func(c *runConfig) { c.loss = math.NaN() }, true},
		{"positive inf loss", func(c *runConfig) { c.loss = math.Inf(1) }, true},
		{"negative inf loss", func(c *runConfig) { c.loss = math.Inf(-1) }, true},
		{"loss below range", func(c *runConfig) { c.loss = -0.1 }, true},
		{"loss above range", func(c *runConfig) { c.loss = 1.1 }, true},
		{"invalid backpressure", func(c *runConfig) { c.backpressure = "burst" }, true},
		{"invalid mode", func(c *runConfig) { c.mode = "quic" }, true},
		{"zero deadline", func(c *runConfig) { c.deadline = 0 }, true},
		{"negative deadline", func(c *runConfig) { c.deadline = -time.Second }, true},
		{"zero window", func(c *runConfig) { c.window = 0 }, true},
		{"prod skips backpressure", func(c *runConfig) { c.mode = "prod"; c.backpressure = "burst" }, false},
		{"zero conns", func(c *runConfig) { c.conns = 0 }, true},
		{"negative conns", func(c *runConfig) { c.conns = -2 }, true},
		{"multi conns ok", func(c *runConfig) { c.conns = 4 }, false},
		{"invalid sharing", func(c *runConfig) { c.sharing = "split" }, true},
		{"independent sharing ok", func(c *runConfig) { c.sharing = "independent" }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid
			tc.mutate(&cfg)
			err := validateRunConfig(cfg)
			if tc.wantErr && err == nil {
				t.Fatalf("validateRunConfig(%+v) = nil, want error", cfg)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateRunConfig(%+v) = %v, want nil", cfg, err)
			}
		})
	}
}

func TestChunkFitsInt(t *testing.T) {
	maxInt := int64(int(^uint(0) >> 1))
	cases := []struct {
		name string
		b    int64
		want bool
	}{
		{"zero", 0, true},
		{"small", 16 << 10, true},
		{"max int", maxInt, true},
	}
	// int64 cannot exceed maxInt on 64-bit platforms, so the rejection path
	// is only exercisable on 32-bit builds.
	if strconv.IntSize == 32 {
		cases = append(cases, struct {
			name string
			b    int64
			want bool
		}{"max int plus one", maxInt + 1, false})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chunkFitsInt(tc.b); got != tc.want {
				t.Fatalf("chunkFitsInt(%d) = %v, want %v", tc.b, got, tc.want)
			}
		})
	}
}
