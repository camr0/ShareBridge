package main

import (
	"context"
	"io"
	"net/http"
	"testing"
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
