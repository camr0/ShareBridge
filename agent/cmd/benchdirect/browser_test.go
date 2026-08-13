package main

import (
	"context"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// Skip when Chrome is unavailable or under -short, so the unit suite stays
// runnable in CI without a browser.
func TestBrowserLaunchesAndEvaluates(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser smoke test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := startBrowser(ctx)
	if err != nil {
		t.Skipf("chrome unavailable: %v", err)
	}
	defer b.close()

	if err := chromedp.Run(b.ctx, chromedp.Navigate("data:text/html,<title>benchdirect</title>")); err != nil {
		t.Fatal(err)
	}
	var title string
	if err := b.eval("document.title", &title); err != nil {
		t.Fatal(err)
	}
	if title != "benchdirect" {
		t.Fatalf("title = %q, want %q", title, "benchdirect")
	}
}
