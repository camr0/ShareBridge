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

func TestValidatePasswordPostsLoginAndStoresCookie(t *testing.T) {
	requestCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		switch requestCount {
		case 1:
			require.Equal(t, http.MethodPost, r.Method)
			require.Equal(t, "/api/shared-links/login", r.URL.Path)
			require.Equal(t, "sharekey", r.URL.Query().Get("key"))
			var body map[string]string
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "secret", body["password"])
			http.SetCookie(w, &http.Cookie{Name: "immich_shared_link_token", Value: "auth-token"})
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"key":"sharekey","assets":[]}`))
		case 2:
			require.Equal(t, http.MethodGet, r.Method)
			require.Equal(t, "/api/shared-links/me", r.URL.Path)
			require.Equal(t, "sharekey", r.URL.Query().Get("key"))
			cookie, err := r.Cookie("immich_shared_link_token")
			require.NoError(t, err)
			require.Equal(t, "auth-token", cookie.Value)
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
}

func TestValidatePasswordReturnsErrorWhenLoginDoesNotSetCookie(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/shared-links/login", r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"key":"sharekey","assets":[]}`))
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)
	ok, err := client.ValidatePassword(t.Context(), "secret")
	require.False(t, ok)
	require.ErrorContains(t, err, "auth cookie")
}

func TestValidatePasswordDoesNotSetCookieForInvalidStatus(t *testing.T) {
	var galleryCookie string
	requestCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		switch requestCount {
		case 1:
			require.Equal(t, "/api/shared-links/login", r.URL.Path)
			w.WriteHeader(http.StatusUnauthorized)
		case 2:
			require.Equal(t, "/api/shared-links/me", r.URL.Path)
			if cookie, err := r.Cookie("immich_shared_link_token"); err == nil {
				galleryCookie = cookie.Value
			}
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
	require.False(t, ok)

	_, err = client.ListGallery(t.Context())
	require.NoError(t, err)
	require.Empty(t, galleryCookie)
}

func TestValidatePasswordReturnsFalseForInvalidStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/shared-links/login", r.URL.Path)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)
	ok, err := client.ValidatePassword(t.Context(), "wrong")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestValidatePasswordFailureDoesNotSetPassword(t *testing.T) {
	var galleryCookie string
	requestCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		switch requestCount {
		case 1:
			require.Equal(t, "/api/shared-links/login", r.URL.Path)
			w.WriteHeader(http.StatusUnauthorized)
		case 2:
			require.Equal(t, "/api/shared-links/me", r.URL.Path)
			if cookie, err := r.Cookie("immich_shared_link_token"); err == nil {
				galleryCookie = cookie.Value
			}
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
	require.Empty(t, galleryCookie)
}

func TestListFilesConvertsImmichAssetsToGalleryItems(t *testing.T) {
	sha := bytes.Repeat([]byte{0xab}, 20)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/shared-links/me", r.URL.Path)
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

func TestListGalleryFallsBackToTimelineWhenAlbumShareHasNoExpandedAssets(t *testing.T) {
	requestCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		switch requestCount {
		case 1:
			require.Equal(t, "/api/shared-links/me", r.URL.Path)
			require.Equal(t, "sharekey", r.URL.Query().Get("key"))
			_, _ = w.Write([]byte(`{"key":"sharekey","type":"ALBUM","assets":[],"album":{"id":"album-1","albumName":"Grad Party","description":"Photos"}}`))
		case 2:
			require.Equal(t, "/api/timeline/buckets", r.URL.Path)
			require.Equal(t, "sharekey", r.URL.Query().Get("key"))
			require.Equal(t, "album-1", r.URL.Query().Get("albumId"))
			_, _ = w.Write([]byte(`[{"timeBucket":"2026-05-17","count":2}]`))
		case 3:
			require.Equal(t, "/api/timeline/bucket", r.URL.Path)
			require.Equal(t, "sharekey", r.URL.Query().Get("key"))
			require.Equal(t, "album-1", r.URL.Query().Get("albumId"))
			require.Equal(t, "2026-05-17", r.URL.Query().Get("timeBucket"))
			_, _ = w.Write([]byte(`{"id":["asset-1","asset-2"],"isImage":[true,false],"ratio":[1.5,1.777],"duration":[null,"00:01:34.500"]}`))
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
	require.Len(t, gallery.Items, 2)
	require.Equal(t, "asset-1", gallery.Items[0].ID)
	require.Equal(t, "image/jpeg", gallery.Items[0].MimeType)
	require.Equal(t, "asset-2", gallery.Items[1].ID)
	require.Equal(t, "video/mp4", gallery.Items[1].MimeType)
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

func TestGetPreviewBuildsAssetThumbnailPreviewURLAndStreamsBody(t *testing.T) {
	body := []byte("preview-bytes")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/assets/asset%2Fwith%20space/thumbnail", r.URL.EscapedPath())
		require.Equal(t, "sharekey", r.URL.Query().Get("key"))
		require.Equal(t, "preview", r.URL.Query().Get("size"))
		_, _ = w.Write(body)
	}))
	defer ts.Close()

	client, err := New(Config{BaseURL: ts.URL, AllowedHost: mustHost(t, ts.URL), ShareKey: "sharekey"})
	require.NoError(t, err)
	var got bytes.Buffer
	n, err := client.GetPreview(t.Context(), "asset/with space", &got)
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
