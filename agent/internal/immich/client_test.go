package immich

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewRejectsHostOutsideAllowList(t *testing.T) {
	_, err := New(Config{
		BaseURL:     "http://evil.example:2283",
		AllowedHost: "immich.lan",
		ShareKey:    "sharekey",
	})
	require.ErrorContains(t, err, `host "evil.example:2283" not allowed`)
}

func TestPollSharesUsesAPIKeyAndNormalizesProtection(t *testing.T) {
	var gotKey string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/shared-links", r.URL.Path)
		gotKey = r.Header.Get("x-api-key")
		_ = json.NewEncoder(w).Encode([]SharedLink{
			{Key: "plainkey", Type: "ALBUM"},
			{Key: "protectedkey", Password: "********", Type: "ALBUM"},
		})
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), APIKey: "api-key"})
	require.NoError(t, err)
	shares, err := client.PollShares(t.Context())
	require.NoError(t, err)
	require.Equal(t, "api-key", gotKey)
	require.False(t, shares[0].IsPasswordProtected())
	require.True(t, shares[1].IsPasswordProtected())
}

func TestPollSharesRejectsRedirectToHostOutsideAllowList(t *testing.T) {
	disallowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]SharedLink{{Key: "redirected", Type: "ALBUM"}})
	}))
	defer disallowed.Close()

	allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, disallowed.URL+"/api/shared-links", http.StatusFound)
	}))
	defer allowed.Close()

	client, err := New(Config{BaseURL: allowed.URL, AllowedHost: mustHost(t, allowed.URL), APIKey: "api-key"})
	require.NoError(t, err)
	_, err = client.PollShares(t.Context())
	require.ErrorContains(t, err, `redirect host "`)
	require.ErrorContains(t, err, `not allowed`)
}

func TestValidatePasswordSendsPasswordQueryParam(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/shared-links/my-share", r.URL.Path)
		require.Equal(t, "sharekey", r.URL.Query().Get("key"))
		require.Equal(t, "secret", r.URL.Query().Get("password"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"key":"sharekey","assets":[]}`))
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)
	ok, err := client.ValidatePassword(t.Context(), "secret")
	require.NoError(t, err)
	require.True(t, ok)
}

func TestValidatePasswordStoresPasswordForSubsequentListGallery(t *testing.T) {
	var galleryPassword string
	requestCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		switch requestCount {
		case 1:
			require.Equal(t, "secret", r.URL.Query().Get("password"))
			_, _ = w.Write([]byte(`{"key":"sharekey","assets":[]}`))
		case 2:
			galleryPassword = r.URL.Query().Get("password")
			_, _ = w.Write([]byte(`{"key":"sharekey","assets":[]}`))
		default:
			t.Fatalf("unexpected request %d", requestCount)
		}
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)
	ok, err := client.ValidatePassword(t.Context(), "secret")
	require.NoError(t, err)
	require.True(t, ok)

	_, err = client.ListGallery(t.Context())
	require.NoError(t, err)
	require.Equal(t, "secret", galleryPassword)
}

func TestValidatePasswordStoresPasswordForSubsequentGetFile(t *testing.T) {
	var filePassword string
	requestCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		switch requestCount {
		case 1:
			require.Equal(t, "/api/shared-links/my-share", r.URL.Path)
			require.Equal(t, "secret", r.URL.Query().Get("password"))
			_, _ = w.Write([]byte(`{"key":"sharekey","assets":[]}`))
		case 2:
			require.Equal(t, "/api/assets/asset-1/original", r.URL.Path)
			filePassword = r.URL.Query().Get("password")
			_, _ = w.Write([]byte("asset"))
		default:
			t.Fatalf("unexpected request %d", requestCount)
		}
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)
	ok, err := client.ValidatePassword(t.Context(), "secret")
	require.NoError(t, err)
	require.True(t, ok)

	var got bytes.Buffer
	_, err = client.GetFile(t.Context(), "asset-1", &got)
	require.NoError(t, err)
	require.Equal(t, "secret", filePassword)
}

func TestValidatePasswordReturnsFalseForInvalidStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/shared-links/my-share", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)
	ok, err := client.ValidatePassword(t.Context(), "wrong")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestValidatePasswordFailureDoesNotSetPassword(t *testing.T) {
	var galleryPassword string
	requestCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		switch requestCount {
		case 1:
			w.WriteHeader(http.StatusNotFound)
		case 2:
			galleryPassword = r.URL.Query().Get("password")
			_, _ = w.Write([]byte(`{"key":"sharekey","assets":[]}`))
		default:
			t.Fatalf("unexpected request %d", requestCount)
		}
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)
	ok, err := client.ValidatePassword(t.Context(), "wrong")
	require.NoError(t, err)
	require.False(t, ok)

	_, err = client.ListGallery(t.Context())
	require.NoError(t, err)
	require.Empty(t, galleryPassword)
}

func TestListFilesConvertsImmichAssetsToGalleryItems(t *testing.T) {
	sha := bytes.Repeat([]byte{0xab}, 20)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/shared-links/my-share", r.URL.Path)
		_ = json.NewEncoder(w).Encode(SharedLink{
			Key:   "sharekey",
			Album: &Album{Name: "Summer", Description: "Beach"},
			Assets: []Asset{{
				ID: "asset-1", OriginalFileName: "photo.jpg", OriginalMimeType: "image/jpeg",
				FileSizeInByte: 1234, Type: "IMAGE", Checksum: base64.StdEncoding.EncodeToString(sha),
				ExifInfo: &ExifInfo{ExifImageWidth: 4000, ExifImageHeight: 3000},
			}, {
				ID: "asset-2", OriginalFileName: "clip.mp4", OriginalMimeType: "video/mp4",
				FileSizeInByte: 4567, Type: "VIDEO", Duration: "00:01:34.500",
			}},
		})
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)
	gallery, err := client.ListGallery(t.Context())
	require.NoError(t, err)
	require.Equal(t, "Summer", gallery.AlbumName)
	require.Equal(t, "Beach", gallery.AlbumDescription)
	require.Equal(t, "asset-1", gallery.Items[0].ID)
	require.Equal(t, "abababababababababababababababababababab", gallery.Items[0].SHA1)
	require.NotNil(t, gallery.Items[1].Duration)
	require.Equal(t, 94.5, *gallery.Items[1].Duration)
}

func TestGetThumbnailBuildsAssetURLAndStreamsBody(t *testing.T) {
	body := []byte("thumb-bytes")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/assets/asset%2Fwith%20space/thumbnail", r.URL.EscapedPath())
		require.Equal(t, "sharekey", r.URL.Query().Get("key"))
		_, _ = w.Write(body)
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)
	var got bytes.Buffer
	n, err := client.GetThumbnail(t.Context(), "asset/with space", &got)
	require.NoError(t, err)
	require.Equal(t, int64(len(body)), n)
	require.Equal(t, body, got.Bytes())
}

func TestGetFileBuildsAssetURLAndStreamsBody(t *testing.T) {
	body := []byte("file-bytes")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/assets/asset%2Fwith%20space/original", r.URL.EscapedPath())
		require.Equal(t, "sharekey", r.URL.Query().Get("key"))
		_, _ = w.Write(body)
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)
	var got bytes.Buffer
	n, err := client.GetFile(t.Context(), "asset/with space", &got)
	require.NoError(t, err)
	require.Equal(t, int64(len(body)), n)
	require.Equal(t, body, got.Bytes())
}

func mustHost(t *testing.T, rawURL string) string {
	t.Helper()

	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	return u.Host
}
