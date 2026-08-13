package immich

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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

func TestPollSharesAcceptsNumericDurationMilliseconds(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/shared-links", r.URL.Path)
		_, _ = w.Write([]byte(`[{"key":"sharekey","type":"ALBUM","assets":[{"id":"asset-1","type":"VIDEO","duration":94500}]}]`))
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), APIKey: "api-key"})
	require.NoError(t, err)
	shares, err := client.PollShares(t.Context())
	require.NoError(t, err)
	require.Len(t, shares, 1)
	require.Len(t, shares[0].Assets, 1)
	require.NotNil(t, shares[0].Assets[0].Duration.seconds())
	require.Equal(t, 94.5, *shares[0].Assets[0].Duration.seconds())
}

func TestAssetDurationJSONCompatibility(t *testing.T) {
	tests := []struct {
		name        string
		jsonValue   string
		wantSeconds *float64
		wantError   bool
	}{
		{name: "legacy timestamp", jsonValue: `"00:01:34.500"`, wantSeconds: float64Ptr(94.5)},
		{name: "integer milliseconds", jsonValue: `94500`, wantSeconds: float64Ptr(94.5)},
		{name: "null", jsonValue: `null`},
		{name: "wrong JSON type", jsonValue: `true`, wantError: true},
		{name: "negative milliseconds", jsonValue: `-1`, wantError: true},
		{name: "fractional milliseconds", jsonValue: `1.5`, wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var duration assetDuration
			err := json.Unmarshal([]byte(tt.jsonValue), &duration)
			if tt.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			if tt.wantSeconds == nil {
				require.Nil(t, duration.seconds())
				return
			}
			require.NotNil(t, duration.seconds())
			require.Equal(t, *tt.wantSeconds, *duration.seconds())
		})
	}
}

func float64Ptr(value float64) *float64 {
	return &value
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
				Type: "VIDEO", Duration: legacyAssetDuration("00:01:34.500"),
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
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/shared-links/me":
			require.Equal(t, "sharekey", r.URL.Query().Get("key"))
			_, _ = w.Write([]byte(`{"key":"sharekey","type":"ALBUM","assets":[],"album":{"id":"album-1","albumName":"Grad Party","description":"Photos"}}`))
		case "/api/server/version":
			_, _ = w.Write([]byte(`{"major":2,"minor":9,"patch":0,"prerelease":null}`))
		case "/api/albums/album-1":
			require.Equal(t, "sharekey", r.URL.Query().Get("key"))
			_, _ = w.Write([]byte(`{"id":"album-1","assets":[{"id":"asset-1","originalFileName":"photo.jpg","originalMimeType":"image/jpeg","type":"IMAGE","exifInfo":{"fileSizeInByte":1234}}]}`))
		default:
			t.Fatalf("unexpected request %s", r.URL.Path)
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

func TestListGalleryUsesPaginatedSearchForImmichV3(t *testing.T) {
	const albumID = "d9e585b3-ffc2-4f23-972c-662d5d5332c3"
	searchPages := 0
	versionRequests := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/server/version":
			versionRequests++
			require.Empty(t, r.URL.RawQuery)
			_ = json.NewEncoder(w).Encode(map[string]any{"major": 3, "minor": 0, "patch": 0, "prerelease": nil})
		case "/api/shared-links/me":
			require.Equal(t, "sharekey", r.URL.Query().Get("key"))
			require.Equal(t, "secret", r.URL.Query().Get("password"))
			_ = json.NewEncoder(w).Encode(SharedLink{
				Key: "sharekey", Type: "ALBUM", Album: &Album{ID: albumID, Name: "Aleena Grad Shoot"}, Assets: []Asset{},
			})
		case "/api/search/metadata":
			searchPages++
			require.Equal(t, "sharekey", r.URL.Query().Get("key"))
			require.Equal(t, "secret", r.URL.Query().Get("password"))
			var body struct {
				AlbumIDs []string `json:"albumIds"`
				Page     int      `json:"page"`
				Size     int      `json:"size"`
				WithExif bool     `json:"withExif"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, []string{albumID}, body.AlbumIDs)
			require.Equal(t, searchPages, body.Page)
			require.Equal(t, 1000, body.Size)
			require.True(t, body.WithExif)
			start := (body.Page - 1) * body.Size
			end := min(start+body.Size, 2453)
			items := make([]Asset, 0, end-start)
			for i := start; i < end; i++ {
				items = append(items, Asset{ID: fmt.Sprintf("asset-%04d", i)})
			}
			var nextPage any
			if end < 2453 {
				nextPage = strconv.Itoa(body.Page + 1)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"albums": map[string]any{"total": 0, "count": 0, "items": []any{}, "facets": []any{}},
				"assets": map[string]any{"total": len(items), "count": len(items), "items": items, "facets": []any{}, "nextPage": nextPage},
			})
		default:
			t.Fatalf("unexpected request %s", r.URL.Path)
		}
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey", Password: "secret"})
	require.NoError(t, err)
	gallery, err := client.ListGallery(t.Context())
	require.NoError(t, err)
	require.Len(t, gallery.Items, 2453)
	require.Equal(t, "asset-0000", gallery.Items[0].ID)
	require.Equal(t, "asset-2452", gallery.Items[2452].ID)
	require.Equal(t, 3, searchPages)
	_, err = client.getServerVersion(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, versionRequests)
}

func TestListGalleryFallsBackToLegacyAPIWhenVersionDetectionFails(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/shared-links/me":
			_, _ = w.Write([]byte(`{"type":"ALBUM","assets":[],"album":{"id":"album-1","albumName":"Fallback"}}`))
		case "/api/server/version":
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		case "/api/albums/album-1":
			_, _ = w.Write([]byte(`{"assets":[{"id":"legacy-asset"}]}`))
		default:
			t.Fatalf("unexpected request %s", r.URL.Path)
		}
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)
	gallery, err := client.ListGallery(t.Context())
	require.NoError(t, err)
	require.Len(t, gallery.Items, 1)
	require.Equal(t, "legacy-asset", gallery.Items[0].ID)
}

func TestListGalleryRetriesVersionDetectionAfterTransientFailure(t *testing.T) {
	versionRequests := 0
	legacyRequests := 0
	searchRequests := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/shared-links/me":
			_, _ = w.Write([]byte(`{"type":"ALBUM","assets":[],"album":{"id":"album-1"}}`))
		case "/api/server/version":
			versionRequests++
			if versionRequests == 1 {
				http.Error(w, "temporary", http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"major":3,"minor":0,"patch":0,"prerelease":null}`))
		case "/api/albums/album-1":
			legacyRequests++
			_, _ = w.Write([]byte(`{"assets":[{"id":"legacy"}]}`))
		case "/api/search/metadata":
			searchRequests++
			_, _ = w.Write([]byte(`{"assets":{"items":[{"id":"v3"}],"nextPage":null}}`))
		default:
			t.Fatalf("unexpected request %s", r.URL.Path)
		}
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)
	first, err := client.ListGallery(t.Context())
	require.NoError(t, err)
	require.Equal(t, "legacy", first.Items[0].ID)
	second, err := client.ListGallery(t.Context())
	require.NoError(t, err)
	require.Equal(t, "v3", second.Items[0].ID)
	require.Equal(t, 2, versionRequests)
	require.Equal(t, 1, legacyRequests)
	require.Equal(t, 1, searchRequests)
}

func TestGetServerVersionAllowsWaitingCallerToCancel(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/server/version", r.URL.Path)
		close(requestStarted)
		<-releaseRequest
		_, _ = w.Write([]byte(`{"major":3,"minor":0,"patch":0,"prerelease":null}`))
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL)})
	require.NoError(t, err)
	firstDone := make(chan error, 1)
	go func() {
		_, err := client.getServerVersion(t.Context())
		firstDone <- err
	}()
	<-requestStarted

	waiterCtx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = client.getServerVersion(waiterCtx)
	require.ErrorIs(t, err, context.Canceled)
	close(releaseRequest)
	require.NoError(t, <-firstDone)
}

func TestListGalleryFallsBackToSearchWhenLegacyAPIUnavailable(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/shared-links/me":
			_, _ = w.Write([]byte(`{"type":"ALBUM","assets":[],"album":{"id":"album-1"}}`))
		case "/api/server/version":
			http.Error(w, "temporary", http.StatusServiceUnavailable)
		case "/api/albums/album-1":
			http.NotFound(w, r)
		case "/api/search/metadata":
			_, _ = w.Write([]byte(`{"assets":{"items":[{"id":"search-asset"}],"nextPage":null}}`))
		default:
			t.Fatalf("unexpected request %s", r.URL.Path)
		}
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)
	gallery, err := client.ListGallery(t.Context())
	require.NoError(t, err)
	require.Len(t, gallery.Items, 1)
	require.Equal(t, "search-asset", gallery.Items[0].ID)
}

func TestSearchAlbumAssetsRejectsRepeatedNextPage(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/search/metadata", r.URL.Path)
		_, _ = w.Write([]byte(`{"assets":{"items":[],"nextPage":"1"}}`))
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)
	_, err = client.searchAlbumAssets(t.Context(), "album-1")
	require.ErrorContains(t, err, "repeated page 1")
}

func TestSearchAlbumAssetsRejectsInvalidNextPage(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/search/metadata", r.URL.Path)
		_, _ = w.Write([]byte(`{"assets":{"items":[],"nextPage":"not-a-page"}}`))
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)
	_, err = client.searchAlbumAssets(t.Context(), "album-1")
	require.ErrorContains(t, err, `invalid Immich search nextPage "not-a-page"`)
}

func TestListGalleryIncludesPasswordInAlbumRequest(t *testing.T) {
	var albumPassword string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/shared-links/me":
			_, _ = w.Write([]byte(`{"key":"sharekey","type":"ALBUM","assets":[],"album":{"id":"album-1","albumName":"Secret Album"}}`))
		case "/api/server/version":
			_, _ = w.Write([]byte(`{"major":2,"minor":9,"patch":0,"prerelease":null}`))
		case "/api/albums/album-1":
			albumPassword = r.URL.Query().Get("password")
			_, _ = w.Write([]byte(`{"id":"album-1","assets":[]}`))
		default:
			t.Fatalf("unexpected request %s", r.URL.Path)
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
