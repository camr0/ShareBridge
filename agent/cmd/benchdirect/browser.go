package main

import (
	"context"
	"errors"
	"os/exec"

	"github.com/chromedp/chromedp"
)

type browserSession struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func chromePath() string {
	for _, candidate := range []string{
		"google-chrome",
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"chromium",
	} {
		if path, err := exec.LookPath(candidate); err == nil {
			return path
		}
	}
	return ""
}

func chromeAvailable() bool {
	return chromePath() != ""
}

func startBrowser(parent context.Context) (*browserSession, error) {
	path := chromePath()
	if path == "" {
		return nil, errors.New("chrome not found")
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(path),
		chromedp.Flag("disable-features", "WebRtcHideLocalIpsWithMdns"),
		chromedp.Flag("headless", "new"),
		chromedp.Flag("no-sandbox", true),
	)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(parent, opts...)
	ctx, cancel := chromedp.NewContext(allocCtx)
	return &browserSession{ctx: ctx, cancel: func() { cancel(); cancelAlloc() }}, nil
}

func (b *browserSession) eval(expr string, res interface{}) error {
	return chromedp.Run(b.ctx, chromedp.Evaluate(expr, res))
}

func (b *browserSession) close() { b.cancel() }
