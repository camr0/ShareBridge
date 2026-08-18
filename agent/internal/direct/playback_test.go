// agent/internal/direct/playback_test.go
package direct

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sharebridge/agent/internal/immich"
)

// playbackBackend is a ContentBackend whose playback-relevant methods are
// function fields; every other method is an inert zero-value stub.
type playbackBackend struct {
	info   func(ctx context.Context, id string) (int64, bool, error)
	full   func(ctx context.Context, id string, w io.Writer) (int64, error)
	ranged func(ctx context.Context, id string, startOffset int64, w io.Writer) (int64, error)
}

func (b *playbackBackend) ListGallery(context.Context) (immich.Gallery, error) {
	return immich.Gallery{}, nil
}
func (b *playbackBackend) GetThumbnail(context.Context, string, io.Writer) (int64, error) {
	return 0, nil
}
func (b *playbackBackend) GetPreview(context.Context, string, io.Writer) (int64, error) {
	return 0, nil
}
func (b *playbackBackend) GetAssetInfo(context.Context, string) (immich.Asset, error) {
	return immich.Asset{}, nil
}
func (b *playbackBackend) GetFile(context.Context, string, io.Writer) (int64, error) {
	return 0, nil
}
func (b *playbackBackend) GetVideoPlayback(ctx context.Context, id string, w io.Writer) (int64, error) {
	if b.full == nil {
		return 0, nil
	}
	return b.full(ctx, id, w)
}
func (b *playbackBackend) GetVideoPlaybackRange(ctx context.Context, id string, startOffset int64, w io.Writer) (int64, error) {
	if b.ranged == nil {
		return 0, nil
	}
	return b.ranged(ctx, id, startOffset, w)
}
func (b *playbackBackend) HeadVideoPlayback(context.Context, string) (int64, error) {
	return 0, nil
}
func (b *playbackBackend) GetAlbumDownloadInfo(context.Context) (immich.AlbumDownload, error) {
	return immich.AlbumDownload{}, nil
}
func (b *playbackBackend) DownloadArchive(context.Context, []string, io.Writer) (int64, error) {
	return 0, nil
}
func (b *playbackBackend) ThumbnailInfo(context.Context, string) (string, int64, bool, error) {
	return "", 0, false, nil
}
func (b *playbackBackend) PreviewInfo(context.Context, string) (string, int64, bool, error) {
	return "", 0, false, nil
}
func (b *playbackBackend) PlaybackInfo(ctx context.Context, id string) (int64, bool, error) {
	if b.info == nil {
		return 0, false, nil
	}
	return b.info(ctx, id)
}

var _ ContentBackend = (*playbackBackend)(nil)

// newPlaybackHandler wires backend for share "abc" with a single member asset
// "vid" and returns the direct handler.
func newPlaybackHandler(t *testing.T, backend ContentBackend) http.Handler {
	t.Helper()
	return newHandlerServer(t, backend, map[string]struct{}{"vid": {}})
}

// playbackRequest issues a GET for the playback route with an optional Range
// header, carrying the SNI/Host the Binder authorizes.
func playbackRequest(handler http.Handler, path, rangeHeader string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://"+testOrigin+path, nil)
	req.TLS = &tls.ConnectionState{ServerName: testOrigin}
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	handler.ServeHTTP(rr, req)
	return rr
}

func TestPlaybackNoRangeFullBody(t *testing.T) {
	backend := &playbackBackend{
		info: func(context.Context, string) (int64, bool, error) { return 1000, true, nil },
		full: func(_ context.Context, _ string, w io.Writer) (int64, error) {
			n, err := io.WriteString(w, "FULL-BODY")
			return int64(n), err
		},
	}
	handler := newPlaybackHandler(t, backend)

	rr := playbackRequest(handler, "/s/abc/asset/vid/playback", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); got != "video/mp4" {
		t.Fatalf("Content-Type = %q, want video/mp4", got)
	}
	if got := rr.Body.String(); got != "FULL-BODY" {
		t.Fatalf("body = %q, want FULL-BODY", got)
	}
}

func TestPlaybackRangeReturns206(t *testing.T) {
	backend := &playbackBackend{
		info: func(context.Context, string) (int64, bool, error) { return 1000, true, nil },
		ranged: func(_ context.Context, _ string, _ int64, w io.Writer) (int64, error) {
			n, err := io.WriteString(w, strings.Repeat("x", 100))
			return int64(n), err
		},
	}
	handler := newPlaybackHandler(t, backend)

	rr := playbackRequest(handler, "/s/abc/asset/vid/playback", "bytes=0-99")
	if rr.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rr.Code)
	}
	if got := rr.Header().Get("Content-Range"); got != "bytes 0-99/1000" {
		t.Fatalf("Content-Range = %q, want bytes 0-99/1000", got)
	}
	if got := rr.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("Accept-Ranges = %q, want bytes", got)
	}
	if got := rr.Header().Get("Content-Type"); got != "video/mp4" {
		t.Fatalf("Content-Type = %q, want video/mp4", got)
	}
	if got := rr.Body.Len(); got != 100 {
		t.Fatalf("body length = %d, want 100", got)
	}
}

func TestPlaybackUnknownTotalIgnoresRange(t *testing.T) {
	backend := &playbackBackend{
		info: func(context.Context, string) (int64, bool, error) { return 0, false, nil },
		full: func(_ context.Context, _ string, w io.Writer) (int64, error) {
			n, err := io.WriteString(w, "FULL-BODY")
			return int64(n), err
		},
	}
	handler := newPlaybackHandler(t, backend)

	rr := playbackRequest(handler, "/s/abc/asset/vid/playback", "bytes=0-99")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Accept-Ranges"); got != "" {
		t.Fatalf("Accept-Ranges = %q, want empty (no range advertised)", got)
	}
	if got := rr.Body.String(); got != "FULL-BODY" {
		t.Fatalf("body = %q, want FULL-BODY", got)
	}
}

func TestPlaybackZeroLength416(t *testing.T) {
	backend := &playbackBackend{
		info: func(context.Context, string) (int64, bool, error) { return 0, true, nil },
	}
	handler := newPlaybackHandler(t, backend)

	rr := playbackRequest(handler, "/s/abc/asset/vid/playback", "bytes=0-99")
	if rr.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want 416", rr.Code)
	}
	if got := rr.Header().Get("Content-Range"); got != "bytes */0" {
		t.Fatalf("Content-Range = %q, want bytes */0", got)
	}
}

func TestPlaybackStartBeyondTotal416(t *testing.T) {
	backend := &playbackBackend{
		info: func(context.Context, string) (int64, bool, error) { return 1000, true, nil },
	}
	handler := newPlaybackHandler(t, backend)

	rr := playbackRequest(handler, "/s/abc/asset/vid/playback", "bytes=1000-")
	if rr.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want 416", rr.Code)
	}
	if got := rr.Header().Get("Content-Range"); got != "bytes */1000" {
		t.Fatalf("Content-Range = %q, want bytes */1000", got)
	}
}

func TestPlaybackBoundedWriterStopsAtEnd(t *testing.T) {
	backend := &playbackBackend{
		info: func(context.Context, string) (int64, bool, error) { return 1000, true, nil },
		// The upstream deliberately streams the full 1000 bytes regardless of
		// the requested offset; the bounded writer must stop forwarding at the
		// range bound (byte 99) and must not surface its bound sentinel as an
		// error (which would otherwise panic the mid-stream abort path).
		ranged: func(_ context.Context, _ string, _ int64, w io.Writer) (int64, error) {
			n, err := io.WriteString(w, strings.Repeat("x", 1000))
			return int64(n), err
		},
	}
	handler := newPlaybackHandler(t, backend)

	rr := playbackRequest(handler, "/s/abc/asset/vid/playback", "bytes=0-99")
	if rr.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rr.Code)
	}
	if got := rr.Body.Len(); got != 100 {
		t.Fatalf("body length = %d, want 100 (bound must stop the upstream read)", got)
	}
	if got := rr.Header().Get("Content-Range"); got != "bytes 0-99/1000" {
		t.Fatalf("Content-Range = %q, want bytes 0-99/1000", got)
	}
}

func TestPlaybackHeadIgnoresRange(t *testing.T) {
	streamCalled := false
	backend := &playbackBackend{
		info: func(context.Context, string) (int64, bool, error) { return 1000, true, nil },
		full: func(context.Context, string, io.Writer) (int64, error) {
			streamCalled = true
			return 0, nil
		},
	}
	handler := newPlaybackHandler(t, backend)

	req := httptest.NewRequest(http.MethodHead, "http://"+testOrigin+"/s/abc/asset/vid/playback", nil)
	req.TLS = &tls.ConnectionState{ServerName: testOrigin}
	req.Header.Set("Range", "bytes=0-99")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("HEAD body = %q, want empty", rr.Body.String())
	}
	if got := rr.Header().Get("Content-Range"); got != "" {
		t.Fatalf("HEAD Content-Range = %q, want empty (HEAD ignores Range)", got)
	}
	if streamCalled {
		t.Fatal("HEAD must not invoke the streaming GET path")
	}
}
