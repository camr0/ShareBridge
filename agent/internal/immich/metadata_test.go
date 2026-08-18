package immich

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestThumbnailInfoReportsContentTypeAndLength asserts ThumbnailInfo performs a
// HEAD request and reports the upstream Content-Type and Content-Length.
func TestThumbnailInfoReportsContentTypeAndLength(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodHead, r.Method)
		require.Equal(t, "/api/assets/asset-1/thumbnail", r.URL.EscapedPath())
		require.Equal(t, "sharekey", r.URL.Query().Get("key"))
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Content-Length", "1234")
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)

	contentType, length, known, err := client.ThumbnailInfo(t.Context(), "asset-1")
	require.NoError(t, err)
	require.Equal(t, "image/jpeg", contentType)
	require.Equal(t, int64(1234), length)
	require.True(t, known)
}

// TestThumbnailInfoUnknownWhenContentLengthAbsent asserts known=false when the
// upstream omits Content-Length (still reports the Content-Type).
func TestThumbnailInfoUnknownWhenContentLengthAbsent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)

	contentType, length, known, err := client.ThumbnailInfo(t.Context(), "asset-1")
	require.NoError(t, err)
	require.Equal(t, "image/jpeg", contentType)
	require.Equal(t, int64(0), length)
	require.False(t, known)
}

// TestPreviewInfoUsesPreviewSizeParamAndReportsMetadata asserts PreviewInfo uses
// the ?size=preview variant of the thumbnail endpoint.
func TestPreviewInfoUsesPreviewSizeParamAndReportsMetadata(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodHead, r.Method)
		require.Equal(t, "/api/assets/asset-preview/thumbnail", r.URL.EscapedPath())
		require.Equal(t, "preview", r.URL.Query().Get("size"))
		require.Equal(t, "sharekey", r.URL.Query().Get("key"))
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Content-Length", "5678")
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)

	contentType, length, known, err := client.PreviewInfo(t.Context(), "asset-preview")
	require.NoError(t, err)
	require.Equal(t, "image/jpeg", contentType)
	require.Equal(t, int64(5678), length)
	require.True(t, known)
}

// TestPreviewInfoUnknownWhenContentLengthAbsent asserts known=false when the
// upstream omits Content-Length.
func TestPreviewInfoUnknownWhenContentLengthAbsent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)

	_, length, known, err := client.PreviewInfo(t.Context(), "asset-preview")
	require.NoError(t, err)
	require.Equal(t, int64(0), length)
	require.False(t, known)
}

// TestPlaybackInfoReportsLengthWhenPresent asserts PlaybackInfo returns the
// transcoded-video Content-Length and known=true when present.
func TestPlaybackInfoReportsLengthWhenPresent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodHead, r.Method)
		require.Equal(t, "/api/assets/asset-vid/video/playback", r.URL.EscapedPath())
		w.Header().Set("Content-Length", "9000")
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)

	length, known, err := client.PlaybackInfo(t.Context(), "asset-vid")
	require.NoError(t, err)
	require.Equal(t, int64(9000), length)
	require.True(t, known)
}

// TestPlaybackInfoUnknownWhenContentLengthAbsent asserts known=false when the
// upstream omits the transcoded-video Content-Length (e.g. chunked encoding).
func TestPlaybackInfoUnknownWhenContentLengthAbsent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodHead, r.Method)
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)

	length, known, err := client.PlaybackInfo(t.Context(), "asset-vid")
	require.NoError(t, err)
	require.Equal(t, int64(0), length)
	require.False(t, known)
}
