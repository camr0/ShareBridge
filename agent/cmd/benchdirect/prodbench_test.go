package main

import (
	"context"
	"testing"
	"time"
)

func TestRunProdSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser smoke test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := runProd(ctx, runConfig{
		mode: "prod", rttMs: 0, loss: 0, size: 8 << 20, chunk: 64 << 10, backpressure: "poll", deadline: 30 * time.Second,
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
