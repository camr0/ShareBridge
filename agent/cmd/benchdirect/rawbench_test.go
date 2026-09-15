package main

import (
	"context"
	"testing"
	"time"
)

func TestShardSizesSumToTotal(t *testing.T) {
	cases := []struct {
		total int64
		n     int
	}{
		{100 << 20, 1},
		{100 << 20, 2},
		{100 << 20, 4},
		{10, 3},
		{7, 4},
		{1, 4},
		{0, 4},
	}
	for _, tc := range cases {
		shards := shardSizes(tc.total, tc.n)
		if len(shards) != tc.n {
			t.Fatalf("shardSizes(%d, %d) returned %d shards", tc.total, tc.n, len(shards))
		}
		var sum, min, max int64
		min = -1
		for _, s := range shards {
			if s < 0 {
				t.Fatalf("negative shard %d", s)
			}
			sum += s
			if min < 0 || s < min {
				min = s
			}
			if s > max {
				max = s
			}
		}
		if sum != tc.total {
			t.Fatalf("shardSizes(%d, %d) sums to %d", tc.total, tc.n, sum)
		}
		if max-min > 1 {
			t.Fatalf("shardSizes(%d, %d) uneven: min %d max %d", tc.total, tc.n, min, max)
		}
	}
}

func TestShardSizesSingleConnectionIsWholePayload(t *testing.T) {
	shards := shardSizes(8<<20, 1)
	if len(shards) != 1 || shards[0] != 8<<20 {
		t.Fatalf("single connection should carry the whole payload, got %v", shards)
	}
}

func TestShardSizesHandlesNonPositiveN(t *testing.T) {
	if shards := shardSizes(1024, 0); len(shards) != 0 {
		t.Fatalf("shardSizes with n=0 should return no shards, got %v", shards)
	}
}

func TestRunRawSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser smoke test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := runRaw(ctx, runConfig{
		mode: "raw", rttMs: 0, loss: 0, size: 8 << 20, chunk: 16 << 10, backpressure: "event", deadline: 30 * time.Second, window: 5 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Received < res.SentBytes {
		t.Fatalf("received %d < sent %d", res.Received, res.SentBytes)
	}
	if res.Mbps <= 0 {
		t.Fatalf("expected positive throughput, got %.2f Mbps", res.Mbps)
	}
}
