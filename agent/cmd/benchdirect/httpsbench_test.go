package main

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestHTTPSRangeAndFullFetch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	url, err := serveFile(ctx, 1<<20, "127.0.0.1") // 1 MiB
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig:   insecureTLSConfig(),
		DisableKeepAlives: true,
	}}

	// Range request returns 206 with exactly the requested slice.
	req, _ := http.NewRequest(http.MethodGet, url+"/bench.bin", nil)
	req.Header.Set("Range", "bytes=0-1023")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("Range status = %d, want 206", resp.StatusCode)
	}
	if b, _ := io.ReadAll(resp.Body); len(b) != 1024 {
		t.Fatalf("Range body = %d bytes, want 1024", len(b))
	}

	// Full fetch returns the whole payload.
	results, err := fetch(ctx, url+"/bench.bin", 1<<20, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.Received != 1<<20 {
			t.Fatalf("received = %d, want %d", r.Received, int64(1<<20))
		}
	}
}

func TestCountStalls_DetectsFullStall(t *testing.T) {
	if got := countStalls(make([]float64, 25)); got < 1 {
		t.Fatalf("countStalls(25 zero windows) = %d, want >= 1", got)
	}
	if got := countStalls(make([]float64, 30)); got != 1 {
		t.Fatalf("countStalls(30 zero windows) = %d, want 1", got)
	}
	fast := make([]float64, 30)
	for i := range fast {
		fast[i] = 50.0
	}
	if got := countStalls(fast); got != 0 {
		t.Fatalf("countStalls(30x50.0) = %d, want 0", got)
	}
}

// stallingReader emits a burst of bytes, then blocks (stalls) for a fixed
// duration, then emits a final burst and io.EOF. It simulates a full-stop
// collapse where Read blocks and zero bytes arrive.
type stallingReader struct {
	bursts [][]byte
	stall  time.Duration
	idx    int
	off    int
}

func (r *stallingReader) Read(p []byte) (int, error) {
	if r.idx >= len(r.bursts) {
		return 0, io.EOF
	}
	b := r.bursts[r.idx]
	if r.off >= len(b) {
		r.idx++
		r.off = 0
		if r.idx == 1 { // after the first burst, stall once
			time.Sleep(r.stall)
		}
		return r.Read(p)
	}
	n := copy(p, b[r.off:])
	r.off += n
	return n, nil
}

func TestMeasureCopy_EmitsSamplesDuringStall(t *testing.T) {
	sr := &stallingReader{
		bursts: [][]byte{
			make([]byte, 256*1024),
			make([]byte, 128*1024),
		},
		stall: 250 * time.Millisecond,
	}
	samples, total, err := measureCopy(sr)
	if err != nil {
		t.Fatalf("measureCopy err = %v", err)
	}
	want := int64(256*1024 + 128*1024)
	if total != want {
		t.Fatalf("total = %d, want %d", total, want)
	}
	if len(samples) < 3 {
		t.Fatalf("len(samples) = %d, want >= 3 (wall-clock sampling should fire during the ~250ms stall)", len(samples))
	}
	found := false
	for _, s := range samples {
		if s < 1.0 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no sample < 1.0 Mbps during stall, samples=%v", samples)
	}
}
