// agent/internal/direct/handlers_test.go
package direct

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sharebridge/agent/internal/immich"
)

const testOrigin = "demo.sbdeadbeef.example.com"

// handlerBackend is a ContentBackend whose handler-relevant methods are
// function fields, so each test configures exactly the behavior it needs.
// Methods left nil behave as inert zero-value stubs.
type handlerBackend struct {
	thumbInfo   func(ctx context.Context, id string) (string, int64, bool, error)
	previewInfo func(ctx context.Context, id string) (string, int64, bool, error)
	assetInfo   func(ctx context.Context, id string) (immich.Asset, error)
	thumb       func(ctx context.Context, id string, w io.Writer) (int64, error)
	preview     func(ctx context.Context, id string, w io.Writer) (int64, error)
	file        func(ctx context.Context, id string, w io.Writer) (int64, error)
}

func (b *handlerBackend) ListGallery(context.Context) (immich.Gallery, error) {
	return immich.Gallery{}, nil
}
func (b *handlerBackend) GetThumbnail(ctx context.Context, id string, w io.Writer) (int64, error) {
	if b.thumb == nil {
		return 0, nil
	}
	return b.thumb(ctx, id, w)
}
func (b *handlerBackend) GetPreview(ctx context.Context, id string, w io.Writer) (int64, error) {
	if b.preview == nil {
		return 0, nil
	}
	return b.preview(ctx, id, w)
}
func (b *handlerBackend) GetAssetInfo(ctx context.Context, id string) (immich.Asset, error) {
	if b.assetInfo == nil {
		return immich.Asset{}, nil
	}
	return b.assetInfo(ctx, id)
}
func (b *handlerBackend) GetFile(ctx context.Context, id string, w io.Writer) (int64, error) {
	if b.file == nil {
		return 0, nil
	}
	return b.file(ctx, id, w)
}
func (b *handlerBackend) GetVideoPlayback(context.Context, string, io.Writer) (int64, error) {
	return 0, nil
}
func (b *handlerBackend) GetVideoPlaybackRange(context.Context, string, int64, io.Writer) (int64, error) {
	return 0, nil
}
func (b *handlerBackend) HeadVideoPlayback(context.Context, string) (int64, error) {
	return 0, nil
}
func (b *handlerBackend) GetAlbumDownloadInfo(context.Context) (immich.AlbumDownload, error) {
	return immich.AlbumDownload{}, nil
}
func (b *handlerBackend) DownloadArchive(context.Context, []string, io.Writer) (int64, error) {
	return 0, nil
}
func (b *handlerBackend) ThumbnailInfo(ctx context.Context, id string) (string, int64, bool, error) {
	if b.thumbInfo == nil {
		return "", 0, false, nil
	}
	return b.thumbInfo(ctx, id)
}
func (b *handlerBackend) PreviewInfo(ctx context.Context, id string) (string, int64, bool, error) {
	if b.previewInfo == nil {
		return "", 0, false, nil
	}
	return b.previewInfo(ctx, id)
}
func (b *handlerBackend) PlaybackInfo(context.Context, string) (int64, bool, error) {
	return 0, false, nil
}

var _ ContentBackend = (*handlerBackend)(nil)

// newHandlerServer builds a DirectServer wired with backend for share "abc"
// (membership = the given id set) and returns its handler.
func newHandlerServer(t *testing.T, backend ContentBackend, membership map[string]struct{}) http.Handler {
	t.Helper()
	return newHandlerServerResolve(t, &fakeResolver{sessions: map[string]*ContentSession{
		"abc": {Backend: backend, Membership: membership},
	}})
}

// newHandlerServerResolve builds a DirectServer wired with an explicit resolver.
func newHandlerServerResolve(t *testing.T, r Resolver) http.Handler {
	t.Helper()
	ns, base := "sbdeadbeef", "example.com"
	cert := testServerCert(t, ns, base)
	gate := NewSignalGate("a", func(string, RouteKind) bool { return true })
	srv := NewDirectServer(ns, base, nil, &rotatableCerts{cert}, gate, 1<<20)
	_ = srv.Binder().Allow(testOrigin, RouteDirect, "abc")
	srv.SetResolver(r)
	return srv.Handler()
}

func doRequest(handler http.Handler, method, path string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, admittedRequest(method, path, testOrigin))
	return rr
}

// errResolver resolves every code to a fixed error, for sentinel-mapping tests.
type errResolver struct{ err error }

func (e errResolver) Resolve(string) (*ContentSession, error) { return nil, e.err }

func TestHandlerThumbAndPreviewServeContentAndHeaders(t *testing.T) {
	backend := &handlerBackend{
		thumbInfo: func(context.Context, string) (string, int64, bool, error) {
			return "image/jpeg", 3, true, nil
		},
		thumb: func(_ context.Context, _ string, w io.Writer) (int64, error) {
			n, err := io.WriteString(w, "THB")
			return int64(n), err
		},
		previewInfo: func(context.Context, string) (string, int64, bool, error) {
			return "image/webp", 3, true, nil
		},
		preview: func(_ context.Context, _ string, w io.Writer) (int64, error) {
			n, err := io.WriteString(w, "PVW")
			return int64(n), err
		},
	}
	handler := newHandlerServer(t, backend, map[string]struct{}{"asset-1": {}})

	for _, tc := range []struct {
		path     string
		wantType string
		wantBody string
	}{
		{"/s/abc/thumb/asset-1", "image/jpeg", "THB"},
		{"/s/abc/preview/asset-1", "image/webp", "PVW"},
	} {
		rr := doRequest(handler, http.MethodGet, tc.path)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", tc.path, rr.Code)
		}
		if got := rr.Header().Get("Content-Type"); got != tc.wantType {
			t.Fatalf("%s: Content-Type = %q, want %q", tc.path, got, tc.wantType)
		}
		if got := rr.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Fatalf("%s: X-Content-Type-Options = %q, want nosniff", tc.path, got)
		}
		if got := rr.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("%s: Cache-Control = %q, want no-store", tc.path, got)
		}
		if got := rr.Header().Get("Referrer-Policy"); got != "no-referrer" {
			t.Fatalf("%s: Referrer-Policy = %q, want no-referrer", tc.path, got)
		}
		if got := rr.Body.String(); got != tc.wantBody {
			t.Fatalf("%s: body = %q, want %q", tc.path, got, tc.wantBody)
		}
	}
}

func TestHandlerUnknownAssetNotFoundBeforeStream(t *testing.T) {
	streamCalled := false
	backend := &handlerBackend{
		thumb: func(context.Context, string, io.Writer) (int64, error) {
			streamCalled = true
			return 0, nil
		},
	}
	handler := newHandlerServer(t, backend, map[string]struct{}{"known": {}})

	rr := doRequest(handler, http.MethodGet, "/s/abc/thumb/unknown")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	if streamCalled {
		t.Fatal("unknown asset must be rejected before streaming begins")
	}
}

func TestHandlerUnknownCodeNotFound(t *testing.T) {
	// The binder enforces path code == bound share code, so an unknown code
	// reaches the resolver as ErrUnknown for the bound code.
	handler := newHandlerServerResolve(t, &fakeResolver{sessions: map[string]*ContentSession{}})

	rr := doRequest(handler, http.MethodGet, "/s/abc/thumb/asset-1")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestHandlerSentinelMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"forbidden", ErrForbidden, http.StatusForbidden},
		{"unready", ErrUnready, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := newHandlerServerResolve(t, errResolver{err: tc.err})
			rr := doRequest(handler, http.MethodGet, "/s/abc/thumb/asset-1")
			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d", rr.Code, tc.want)
			}
		})
	}
}

func TestHandlerHeadReturnsHeadersNoBody(t *testing.T) {
	streamCalled := false
	backend := &handlerBackend{
		thumbInfo: func(context.Context, string) (string, int64, bool, error) {
			return "image/jpeg", 1234, true, nil
		},
		thumb: func(context.Context, string, io.Writer) (int64, error) {
			streamCalled = true
			return 0, nil
		},
	}
	handler := newHandlerServer(t, backend, map[string]struct{}{"asset-1": {}})

	rr := doRequest(handler, http.MethodHead, "/s/abc/thumb/asset-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("HEAD body = %q, want empty", rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Fatalf("Content-Type = %q, want image/jpeg", got)
	}
	if got := rr.Header().Get("Content-Length"); got != "1234" {
		t.Fatalf("Content-Length = %q, want 1234", got)
	}
	if streamCalled {
		t.Fatal("HEAD must not invoke the streaming GET path")
	}
}

func TestHandlerAssetAttachmentFilenameSanitized(t *testing.T) {
	backend := &handlerBackend{
		assetInfo: func(context.Context, string) (immich.Asset, error) {
			return immich.Asset{
				OriginalFileName: "../../IMG_0001.JPG",
				OriginalMimeType: "image/jpeg",
			}, nil
		},
		file: func(_ context.Context, _ string, w io.Writer) (int64, error) {
			n, err := io.WriteString(w, "DATA")
			return int64(n), err
		},
	}
	handler := newHandlerServer(t, backend, map[string]struct{}{"asset-1": {}})

	rr := doRequest(handler, http.MethodGet, "/s/abc/asset/asset-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	cd := rr.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("Content-Disposition = %q, want attachment prefix", cd)
	}
	if strings.ContainsAny(cd, "/\\") {
		t.Fatalf("Content-Disposition = %q, must not contain path separators", cd)
	}
	if got := rr.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Fatalf("Content-Type = %q, want image/jpeg", got)
	}
}

func TestHandlerStreamingErrorMapsBeforeFirstByte(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"not found", &immich.NotFoundError{}, http.StatusNotFound},
		{"auth", &immich.AuthError{Status: http.StatusUnauthorized}, http.StatusForbidden},
		{"upstream 500", &immich.UpstreamError{Status: http.StatusInternalServerError}, http.StatusBadGateway},
		{"timeout", context.DeadlineExceeded, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &handlerBackend{
				thumbInfo: func(context.Context, string) (string, int64, bool, error) {
					return "image/jpeg", 0, false, nil
				},
				thumb: func(context.Context, string, io.Writer) (int64, error) {
					return 0, tc.err
				},
			}
			handler := newHandlerServer(t, backend, map[string]struct{}{"asset-1": {}})

			rr := doRequest(handler, http.MethodGet, "/s/abc/thumb/asset-1")
			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d", rr.Code, tc.want)
			}
			// The image bytes must never be written; a committed 200 would have
			// left the status at 200 and this assertion would have failed.
			if strings.Contains(rr.Body.String(), "image") {
				t.Fatalf("unexpected body: %q", rr.Body.String())
			}
		})
	}
}

func TestHandlerPreflightErrorMapping(t *testing.T) {
	backend := &handlerBackend{
		thumbInfo: func(context.Context, string) (string, int64, bool, error) {
			return "", 0, false, &immich.UpstreamError{Status: http.StatusInternalServerError}
		},
	}
	handler := newHandlerServer(t, backend, map[string]struct{}{"asset-1": {}})

	rr := doRequest(handler, http.MethodGet, "/s/abc/thumb/asset-1")
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
}

func TestHandlerMidStreamFailurePanicsAbort(t *testing.T) {
	backend := &handlerBackend{
		thumbInfo: func(context.Context, string) (string, int64, bool, error) {
			return "image/jpeg", 0, false, nil
		},
		thumb: func(_ context.Context, _ string, w io.Writer) (int64, error) {
			_, _ = w.Write([]byte("partial"))
			return 0, errors.New("connection reset mid-stream")
		},
	}
	handler := newHandlerServer(t, backend, map[string]struct{}{"asset-1": {}})

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected mid-stream failure to panic")
		}
		if r != http.ErrAbortHandler {
			t.Fatalf("panic = %v, want http.ErrAbortHandler", r)
		}
	}()
	_ = doRequest(handler, http.MethodGet, "/s/abc/thumb/asset-1")
}
