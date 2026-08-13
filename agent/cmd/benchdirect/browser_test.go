package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestServerEndpoints(t *testing.T) {
	s := newBenchServer(func() string { return "offer-sdp" }, make(chan string, 1), os.DirFS("web"))
	s.mode = "raw"
	s.size = 1024
	s.chunk = 256

	h := s.handler()

	t.Run("start POST returns config", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/start", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}

		var got struct {
			Offer string `json:"offer"`
			Mode  string `json:"mode"`
			Size  int64  `json:"size"`
			Chunk int    `json:"chunk"`
		}
		if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got.Offer != "offer-sdp" || got.Mode != "raw" || got.Size != 1024 || got.Chunk != 256 {
			t.Fatalf("unexpected response: %+v", got)
		}
	})

	t.Run("start GET rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/start", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
		}
		if body := w.Body.String(); body != "method not allowed\n" {
			t.Fatalf("body = %q, want %q", body, "method not allowed\\n")
		}
	})

	t.Run("answer POST writes SDP", func(t *testing.T) {
		body := bytes.NewBufferString(`{"sdp":"answer-sdp"}`)
		req := httptest.NewRequest(http.MethodPost, "/answer", body)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		select {
		case got := <-s.answerCh:
			if got != "answer-sdp" {
				t.Fatalf("answerCh = %q, want %q", got, "answer-sdp")
			}
		default:
			t.Fatal("answerCh was not written")
		}
	})

	t.Run("answer GET rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/answer", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
		}
		if body := w.Body.String(); body != "method not allowed\n" {
			t.Fatalf("body = %q, want %q", body, "method not allowed\\n")
		}
	})
}

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

func TestPageContract(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser page contract test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := startBrowser(ctx)
	if err != nil {
		t.Skipf("chrome unavailable: %v", err)
	}
	defer b.close()

	ts := httptest.NewServer(http.FileServer(http.Dir("web")))
	defer ts.Close()

	if err := chromedp.Run(b.ctx, chromedp.Navigate(ts.URL)); err != nil {
		t.Fatal(err)
	}

	var benchType string
	if err := b.eval("typeof window.__bench", &benchType); err != nil {
		t.Fatal(err)
	}
	if benchType != "object" {
		t.Fatalf("typeof window.__bench = %q, want %q", benchType, "object")
	}

	var summary struct {
		Received  float64 `json:"received"`
		ElapsedMs float64 `json:"elapsedMs"`
		Mbps      float64 `json:"mbps"`
	}
	if err := b.eval(`window.__benchSummary()`, &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Received != 0 || summary.ElapsedMs != 0 || summary.Mbps != 0 {
		t.Fatalf("summary = %+v, want zeros", summary)
	}

	var keysJSON string
	if err := chromedp.Run(b.ctx,
		chromedp.Evaluate(`JSON.stringify(Object.keys(window.__benchSummary()).sort())`, &keysJSON),
	); err != nil {
		t.Fatal(err)
	}
	if keysJSON != `["elapsedMs","mbps","received","samples"]` {
		t.Fatalf("summary keys = %s", keysJSON)
	}
}
