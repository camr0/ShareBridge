package immich

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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

func TestHostAllowedRequiresExactHostAndPort(t *testing.T) {
	require.True(t, hostAllowed("immich.lan:2283", "immich.lan:2283"))
	require.True(t, hostAllowed("immich.lan", "immich.lan"))
	require.False(t, hostAllowed("immich.lan:9999", "immich.lan"))
	require.False(t, hostAllowed("immich.lan:9999", "immich.lan:2283"))
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

func TestValidatePasswordUsesPasswordAsQueryParam(t *testing.T) {
	var gotPassword string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/api/shared-links/me", r.URL.Path)
		require.Equal(t, "sharekey", r.URL.Query().Get("key"))
		gotPassword = r.URL.Query().Get("password")
		_ = json.NewEncoder(w).Encode(SharedLink{
			Key:    "sharekey",
			Assets: []Asset{},
		})
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)
	ok, err := client.ValidatePassword(t.Context(), "secret")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "secret", gotPassword)
}

func TestValidatePasswordRejectsOnNon200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "sharekey", r.URL.Query().Get("key"))
		require.Equal(t, "wrong", r.URL.Query().Get("password"))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)
	ok, err := client.ValidatePassword(t.Context(), "wrong")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestValidatePasswordStoresPasswordForSubsequentRequests(t *testing.T) {
	var calls []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		passwords := r.URL.Query().Get("password")
		calls = append(calls, passwords)
		if len(calls) == 1 {
			// ValidatePassword call
			require.Equal(t, "secret", passwords)
			_ = json.NewEncoder(w).Encode(SharedLink{Key: "sharekey", Assets: []Asset{}})
		} else {
			// ListGallery call — should reuse stored password
			require.Equal(t, "secret", passwords)
			_ = json.NewEncoder(w).Encode(SharedLink{Key: "sharekey", Assets: []Asset{}})
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
	require.Len(t, calls, 2)
	require.Equal(t, "secret", calls[0])
	require.Equal(t, "secret", calls[1])
}

func TestValidatePasswordFailureDoesNotStorePassword(t *testing.T) {
	var galleryPassword string
	requestCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		switch requestCount {
		case 1:
			require.Equal(t, "wrong", r.URL.Query().Get("password"))
			w.WriteHeader(http.StatusUnauthorized)
		case 2:
			galleryPassword = r.URL.Query().Get("password")
			_ = json.NewEncoder(w).Encode(SharedLink{Key: "sharekey", Assets: []Asset{}})
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

func TestListGalleryConvertsImmichAssetsToGalleryItems(t *testing.T) {
	sha := bytes.Repeat([]byte{0xab}, 20)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/shared-links/me", r.URL.Path)
		_ = json.NewEncoder(w).Encode(SharedLink{
			Key:   "sharekey",
			Album: &Album{Name: "Summer", Description: "Beach"},
			Assets: []Asset{{
				ID: "asset-1", OriginalFileName: "photo.jpg", OriginalMimeType: "image/jpeg",
				Type: "IMAGE", Checksum: base64.StdEncoding.EncodeToString(sha),
				ExifInfo: &ExifInfo{ExifImageWidth: 4000, ExifImageHeight: 3000, FileSizeInByte: 1234},
			}, {
				ID: "asset-2", OriginalFileName: "clip.mp4", OriginalMimeType: "video/mp4",
				Type: "VIDEO", Duration: "00:01:34.500",
				ExifInfo: &ExifInfo{FileSizeInByte: 4567},
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

func TestListGalleryFallsBackToAlbumAPIWhenInlineAssetsEmpty(t *testing.T) {
	requestCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		switch requestCount {
		case 1:
			require.Equal(t, "/api/shared-links/me", r.URL.Path)
			require.Equal(t, "sharekey", r.URL.Query().Get("key"))
			_, _ = w.Write([]byte(`{"key":"sharekey","type":"ALBUM","assets":[],"album":{"id":"album-1","albumName":"Grad Party","description":"Photos"}}`))
		case 2:
			require.Equal(t, "/api/albums/album-1", r.URL.Path)
			require.Equal(t, "sharekey", r.URL.Query().Get("key"))
			_, _ = w.Write([]byte(`{"id":"album-1","assets":[{"id":"asset-1","originalFileName":"photo.jpg","originalMimeType":"image/jpeg","type":"IMAGE","exifInfo":{"fileSizeInByte":1234}}]}`))
		default:
			t.Fatalf("unexpected request %d", requestCount)
		}
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)
	gallery, err := client.ListGallery(t.Context())
	require.NoError(t, err)
	require.Equal(t, "Grad Party", gallery.AlbumName)
	require.Len(t, gallery.Items, 1)
	require.Equal(t, "asset-1", gallery.Items[0].ID)
}

func TestListGalleryIncludesPasswordInAlbumRequest(t *testing.T) {
	var albumPassword string
	requestCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		switch requestCount {
		case 1:
			_, _ = w.Write([]byte(`{"key":"sharekey","type":"ALBUM","assets":[],"album":{"id":"album-1","albumName":"Secret Album"}}`))
		case 2:
			require.Equal(t, "/api/albums/album-1", r.URL.Path)
			albumPassword = r.URL.Query().Get("password")
			_, _ = w.Write([]byte(`{"id":"album-1","assets":[]}`))
		default:
			t.Fatalf("unexpected request %d", requestCount)
		}
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey", Password: "secret"})
	require.NoError(t, err)
	_, err = client.ListGallery(t.Context())
	require.NoError(t, err)
	require.Equal(t, "secret", albumPassword)
}

func TestGetAlbumDownloadInfoUsesSharedAlbumAndPreservesArchiveOrder(t *testing.T) {
	var paths []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		require.Equal(t, "sharekey", r.URL.Query().Get("key"))
		require.Equal(t, "secret", r.URL.Query().Get("password"))
		switch r.URL.Path {
		case "/api/shared-links/me":
			require.Equal(t, http.MethodGet, r.Method)
			_ = json.NewEncoder(w).Encode(SharedLink{
				Type:  "ALBUM",
				Album: &Album{ID: "album-1", Name: "Summer"},
			})
		case "/api/download/info":
			require.Equal(t, http.MethodPost, r.Method)
			require.Equal(t, "application/json", r.Header.Get("Content-Type"))
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, map[string]any{"albumId": "album-1"}, body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"totalSize":30,"archives":[{"assetIds":["asset-2","asset-1"],"size":20},{"assetIds":["asset-3"],"size":10}]}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer ts.Close()

	client, err := New(Config{
		BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey", Password: "secret",
	})
	require.NoError(t, err)

	download, err := client.GetAlbumDownloadInfo(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{"/api/shared-links/me", "/api/download/info"}, paths)
	require.Equal(t, "Summer", download.AlbumName)
	require.Equal(t, int64(30), download.TotalSize)
	require.Equal(t, []DownloadArchive{
		{AssetIDs: []string{"asset-2", "asset-1"}, Size: 20},
		{AssetIDs: []string{"asset-3"}, Size: 10},
	}, download.Archives)
}

func TestGetAlbumDownloadInfoRejectsNonAlbumShare(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(SharedLink{Type: "INDIVIDUAL"})
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)
	_, err = client.GetAlbumDownloadInfo(t.Context())
	require.ErrorContains(t, err, "album")
}

func TestDownloadArchiveStreamsEditedAssets(t *testing.T) {
	body := []byte("zip-stream-bytes")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/api/download/archive", r.URL.Path)
		require.Equal(t, "sharekey", r.URL.Query().Get("key"))
		require.Equal(t, "secret", r.URL.Query().Get("password"))
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))
		require.Equal(t, "application/octet-stream", r.Header.Get("Accept"))
		var request struct {
			AssetIDs []string `json:"assetIds"`
			Edited   bool     `json:"edited"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		require.Equal(t, []string{"asset-2", "asset-1"}, request.AssetIDs)
		require.True(t, request.Edited)
		_, _ = w.Write(body)
	}))
	defer ts.Close()

	client, err := New(Config{
		BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey", Password: "secret",
	})
	require.NoError(t, err)
	var got bytes.Buffer
	n, err := client.DownloadArchive(t.Context(), []string{"asset-2", "asset-1"}, &got)
	require.NoError(t, err)
	require.Equal(t, int64(len(body)), n)
	require.Equal(t, body, got.Bytes())
}

func TestDownloadArchiveBoundsHTTPErrorBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(strings.Repeat("x", 32*1024)))
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)
	_, err = client.DownloadArchive(t.Context(), []string{"asset-1"}, &bytes.Buffer{})
	require.Error(t, err)
	require.Less(t, len(err.Error()), 8*1024)
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

func TestGetPreviewBuildsAssetThumbnailURLAndStreamsBody(t *testing.T) {
	body := []byte("preview-bytes")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/assets/asset-preview/thumbnail", r.URL.EscapedPath())
		require.Equal(t, "sharekey", r.URL.Query().Get("key"))
		_, _ = w.Write(body)
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)
	var got bytes.Buffer
	n, err := client.GetPreview(t.Context(), "asset-preview", &got)
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

func TestGetVideoPlaybackBuildsURLAndStreamsBody(t *testing.T) {
	body := []byte("transcoded-video-data")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/assets/asset-vid/video/playback", r.URL.EscapedPath())
		require.Equal(t, "sharekey", r.URL.Query().Get("key"))
		_, _ = w.Write(body)
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)
	var got bytes.Buffer
	n, err := client.GetVideoPlayback(t.Context(), "asset-vid", &got)
	require.NoError(t, err)
	require.Equal(t, int64(len(body)), n)
	require.Equal(t, body, got.Bytes())
}

func TestAssetURLIncludesPasswordWhenSet(t *testing.T) {
	var passwordParam string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		passwordParam = r.URL.Query().Get("password")
		_, _ = w.Write([]byte("ok"))
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey", Password: "secret"})
	require.NoError(t, err)
	var got bytes.Buffer
	_, err = client.GetFile(t.Context(), "asset-id", &got)
	require.NoError(t, err)
	require.Equal(t, "secret", passwordParam)
}

func mustHost(t *testing.T, rawURL string) string {
	t.Helper()

	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	return u.Host
}
