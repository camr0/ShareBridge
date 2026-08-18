package immich

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestHTTPStatusesClassifiedAsTypedErrors exercises the status-flattening path
// (getAsset -> responseStatusError) and asserts each upstream status is
// classified into the matching typed error via errors.As.
func TestHTTPStatusesClassifiedAsTypedErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		check  func(t *testing.T, err error)
	}{
		{
			name:   "401 is AuthError",
			status: http.StatusUnauthorized,
			check: func(t *testing.T, err error) {
				var authErr *AuthError
				require.ErrorAs(t, err, &authErr)
				require.Equal(t, http.StatusUnauthorized, authErr.Status)
			},
		},
		{
			name:   "403 is AuthError",
			status: http.StatusForbidden,
			check: func(t *testing.T, err error) {
				var authErr *AuthError
				require.ErrorAs(t, err, &authErr)
				require.Equal(t, http.StatusForbidden, authErr.Status)
			},
		},
		{
			name:   "404 is NotFoundError",
			status: http.StatusNotFound,
			check: func(t *testing.T, err error) {
				var notFoundErr *NotFoundError
				require.ErrorAs(t, err, &notFoundErr)
			},
		},
		{
			name:   "500 is UpstreamError",
			status: http.StatusInternalServerError,
			check: func(t *testing.T, err error) {
				var upstreamErr *UpstreamError
				require.ErrorAs(t, err, &upstreamErr)
				require.Equal(t, http.StatusInternalServerError, upstreamErr.Status)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte("boom"))
			}))
			defer ts.Close()

			client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
			require.NoError(t, err)

			_, err = client.GetThumbnail(t.Context(), "asset-id", &bytes.Buffer{})
			require.Error(t, err)
			tt.check(t, err)
		})
	}
}

// TestGetVideoPlaybackRangeNotSatisfiableIsUpstreamError asserts the 416
// branch of getAsset is a typed UpstreamError (not a bare fmt.Errorf).
func TestGetVideoPlaybackRangeNotSatisfiableIsUpstreamError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "bytes=100-", r.Header.Get("Range"))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)

	_, err = client.GetVideoPlaybackRange(t.Context(), "asset-vid", 100, &bytes.Buffer{})
	var upstreamErr *UpstreamError
	require.ErrorAs(t, err, &upstreamErr)
	require.Equal(t, http.StatusRequestedRangeNotSatisfiable, upstreamErr.Status)
}

// TestHeadVideoPlaybackClassifiesErrors asserts HeadVideoPlayback no longer
// returns a bare fmt.Errorf for non-200 responses.
func TestHeadVideoPlaybackClassifiesErrors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodHead, r.Method)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)

	_, err = client.HeadVideoPlayback(t.Context(), "asset-vid")
	var notFoundErr *NotFoundError
	require.ErrorAs(t, err, &notFoundErr)
}
