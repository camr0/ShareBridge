package immich

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	BaseURL     string
	AllowedHost string
	APIKey      string
	ShareKey    string
	Password    string
}

type Client struct {
	baseURL       *url.URL
	allowed       string
	apiKey        string
	shareKey      string
	password      string
	httpClient    *http.Client
	versionMu     sync.Mutex
	version       serverVersion
	versionLoaded bool
	versionDone   chan struct{}
}

type serverVersion struct {
	Major      int  `json:"major"`
	Minor      int  `json:"minor"`
	Patch      int  `json:"patch"`
	Prerelease *int `json:"prerelease"`
}

type SharedLink struct {
	Key      string  `json:"key"`
	Password string  `json:"password"`
	Type     string  `json:"type"`
	Album    *Album  `json:"album"`
	Assets   []Asset `json:"assets"`
}

type Album struct {
	ID          string `json:"id"`
	Name        string `json:"albumName"`
	Description string `json:"description"`
}

type Asset struct {
	ID               string        `json:"id"`
	OriginalFileName string        `json:"originalFileName"`
	OriginalMimeType string        `json:"originalMimeType"`
	Type             string        `json:"type"`
	Duration         assetDuration `json:"duration"`
	Checksum         string        `json:"checksum"`
	ExifInfo         *ExifInfo     `json:"exifInfo"`
}

// assetDuration accepts both Immich's legacy timestamp strings and its current
// integer millisecond representation.
type assetDuration struct {
	legacy       string
	milliseconds *float64
}

func (d assetDuration) MarshalJSON() ([]byte, error) {
	if d.milliseconds != nil {
		return json.Marshal(*d.milliseconds)
	}
	if d.legacy != "" {
		return json.Marshal(d.legacy)
	}
	return []byte("null"), nil
}

func (d *assetDuration) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, []byte("null")) {
		*d = assetDuration{}
		return nil
	}
	if len(data) > 0 && data[0] == '"' {
		var legacy string
		if err := json.Unmarshal(data, &legacy); err != nil {
			return fmt.Errorf("decode duration string: %w", err)
		}
		*d = assetDuration{legacy: legacy}
		return nil
	}

	var milliseconds float64
	if err := json.Unmarshal(data, &milliseconds); err != nil {
		return fmt.Errorf("duration must be a string, number, or null: %w", err)
	}
	if milliseconds < 0 || math.IsInf(milliseconds, 0) || math.IsNaN(milliseconds) || math.Trunc(milliseconds) != milliseconds {
		return fmt.Errorf("duration milliseconds must be a non-negative integer")
	}
	*d = assetDuration{milliseconds: &milliseconds}
	return nil
}

func (d assetDuration) seconds() *float64 {
	if d.milliseconds != nil {
		seconds := *d.milliseconds / 1000
		return &seconds
	}
	return parseDurationSeconds(d.legacy)
}

func legacyAssetDuration(value string) assetDuration {
	return assetDuration{legacy: value}
}

type ExifInfo struct {
	ExifImageWidth  int   `json:"exifImageWidth"`
	ExifImageHeight int   `json:"exifImageHeight"`
	FileSizeInByte  int64 `json:"fileSizeInByte"`
}

func (a Asset) FileSize() int64 {
	if a.ExifInfo != nil {
		return a.ExifInfo.FileSizeInByte
	}
	return 0
}

func (s SharedLink) IsPasswordProtected() bool {
	return s.Password != ""
}

type Gallery struct {
	AlbumName        string
	AlbumDescription string
	Items            []GalleryItem
}

type GalleryItem struct {
	ID       string
	Name     string
	MimeType string
	Width    int
	Height   int
	Size     int64
	Duration *float64
	SHA1     string
}

type DownloadArchive struct {
	AssetIDs []string `json:"assetIds"`
	Size     int64    `json:"size"`
}

type AlbumDownload struct {
	AlbumName string
	TotalSize int64             `json:"totalSize"`
	Archives  []DownloadArchive `json:"archives"`
}

const maxHTTPErrorBody = 4 * 1024

func New(cfg Config) (*Client, error) {
	baseURL, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}
	if baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, fmt.Errorf("invalid base URL: missing scheme or host")
	}
	if !hostAllowed(baseURL.Host, cfg.AllowedHost) {
		return nil, fmt.Errorf("host %q not allowed", baseURL.Host)
	}

	return &Client{
		baseURL:  baseURL,
		allowed:  cfg.AllowedHost,
		apiKey:   cfg.APIKey,
		shareKey: cfg.ShareKey,
		password: cfg.Password,
		httpClient: &http.Client{
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if !hostAllowed(req.URL.Host, cfg.AllowedHost) {
					return fmt.Errorf("redirect host %q not allowed", req.URL.Host)
				}
				return nil
			},
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout: 10 * time.Second,
				}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
			},
		},
	}, nil
}

func hostAllowed(got, allowed string) bool {
	if allowed == "" {
		return false
	}
	return canonicalHost(got) == canonicalHost(allowed)
}

func canonicalHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

// PollShares lists all shared links (authenticated with API key).
func (c *Client) PollShares(ctx context.Context) ([]SharedLink, error) {
	log.Printf("immich poll: GET /api/shared-links host=%s", c.baseURL.Host)
	u := c.baseURL.ResolveReference(&url.URL{Path: "/api/shared-links"})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("x-api-key", c.apiKey)
	}

	var shares []SharedLink
	if err := c.doJSON(req, &shares); err != nil {
		return nil, err
	}
	log.Printf("immich poll: loaded %d shared links", len(shares))
	return shares, nil
}

// ValidatePassword tries the Immich share endpoint with a password.
// Immich validates by accepting/rejecting the request — there is no separate login step.
func (c *Client) ValidatePassword(ctx context.Context, password string) (bool, error) {
	log.Printf("immich auth: validating password share=%s", redactKey(c.shareKey))
	u := c.buildSharedLinkURL()
	q := u.Query()
	q.Set("password", password)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("immich auth: rejected password status=%d share=%s", resp.StatusCode, redactKey(c.shareKey))
		return false, nil
	}
	// Success — store the password for subsequent asset requests.
	c.password = password
	log.Printf("immich auth: password accepted share=%s", redactKey(c.shareKey))
	return true, nil
}

// ListGallery returns the album gallery for the shared link.
// For album-type shares, a second request to /api/albums/{id} populates assets.
func (c *Client) ListGallery(ctx context.Context) (Gallery, error) {
	link, err := c.getSharedLink(ctx)
	if err != nil {
		return Gallery{}, err
	}
	albumID := ""
	albumName := ""
	if link.Album != nil {
		albumID = link.Album.ID
		albumName = link.Album.Name
	}
	log.Printf("immich gallery: share=%s type=%s album_id=%s album_name=%q inline_assets=%d",
		redactKey(c.shareKey), link.Type, albumID, albumName, len(link.Assets))

	assets := link.Assets
	if len(assets) == 0 && strings.EqualFold(link.Type, "ALBUM") && link.Album != nil && link.Album.ID != "" {
		assets, err = c.loadAlbumAssets(ctx, link.Album.ID)
		if err != nil {
			return Gallery{}, err
		}
	}

	gallery := Gallery{Items: make([]GalleryItem, 0, len(assets))}
	if link.Album != nil {
		gallery.setAlbum(link.Album)
	}
	for _, asset := range assets {
		gallery.Items = append(gallery.Items, galleryItemFromAsset(asset))
	}
	log.Printf("immich gallery: share=%s produced %d items", redactKey(c.shareKey), len(gallery.Items))
	return gallery, nil
}

func (c *Client) loadAlbumAssets(ctx context.Context, albumID string) ([]Asset, error) {
	version, err := c.getServerVersion(ctx)
	if err == nil {
		if version.Major >= 3 {
			log.Printf("immich gallery: using paginated asset search API (Immich v3-compatible)")
			return c.searchAlbumAssets(ctx, albumID)
		}
		log.Printf("immich gallery: using legacy album assets API (Immich v2-compatible)")
		return c.listAlbumAssets(ctx, albumID)
	}

	log.Printf("immich gallery: server version detection failed, using capability fallback: %v", err)
	assets, legacyErr := c.listAlbumAssets(ctx, albumID)
	if legacyErr == nil && len(assets) > 0 {
		log.Printf("immich gallery: capability fallback selected legacy album assets API")
		return assets, nil
	}
	if legacyErr != nil {
		log.Printf("immich gallery: legacy album API failed, trying paginated asset search API: %v", legacyErr)
	} else {
		log.Printf("immich gallery: legacy album response empty, trying paginated asset search API")
	}
	searchAssets, searchErr := c.searchAlbumAssets(ctx, albumID)
	if searchErr != nil && legacyErr != nil {
		return nil, fmt.Errorf("legacy album API failed: %v; paginated asset search API failed: %w", legacyErr, searchErr)
	}
	return searchAssets, searchErr
}

func (c *Client) getServerVersion(ctx context.Context) (serverVersion, error) {
	for {
		c.versionMu.Lock()
		if c.versionLoaded {
			version := c.version
			c.versionMu.Unlock()
			return version, nil
		}
		if c.versionDone != nil {
			done := c.versionDone
			c.versionMu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return serverVersion{}, ctx.Err()
			}
		}
		c.versionDone = make(chan struct{})
		done := c.versionDone
		c.versionMu.Unlock()

		version, err := c.fetchServerVersion(ctx)
		c.versionMu.Lock()
		if err == nil {
			c.version = version
			c.versionLoaded = true
		}
		c.versionDone = nil
		close(done)
		c.versionMu.Unlock()
		if err != nil {
			return serverVersion{}, err
		}
		log.Printf("immich api: detected server version %d.%d.%d", version.Major, version.Minor, version.Patch)
		return version, nil
	}
}

func (c *Client) fetchServerVersion(ctx context.Context) (serverVersion, error) {
	u := c.baseURL.ResolveReference(&url.URL{Path: "/api/server/version"})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return serverVersion{}, err
	}
	var version serverVersion
	if err := c.doJSON(req, &version); err != nil {
		return serverVersion{}, err
	}
	return version, nil
}

func (c *Client) GetAlbumDownloadInfo(ctx context.Context) (AlbumDownload, error) {
	link, err := c.getSharedLink(ctx)
	if err != nil {
		return AlbumDownload{}, err
	}
	if !strings.EqualFold(link.Type, "ALBUM") || link.Album == nil || link.Album.ID == "" {
		return AlbumDownload{}, fmt.Errorf("shared link is not an album")
	}

	payload, err := json.Marshal(struct {
		AlbumID string `json:"albumId"`
	}{AlbumID: link.Album.ID})
	if err != nil {
		return AlbumDownload{}, err
	}
	u := c.baseURL.ResolveReference(&url.URL{Path: "/api/download/info"})
	c.addSharedLinkParams(u)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
	if err != nil {
		return AlbumDownload{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	result := AlbumDownload{AlbumName: link.Album.Name}
	if err := c.doJSONStatus(req, http.StatusCreated, &result); err != nil {
		return AlbumDownload{}, err
	}
	return result, nil
}

func (c *Client) DownloadArchive(ctx context.Context, assetIDs []string, w io.Writer) (int64, error) {
	payload, err := json.Marshal(struct {
		AssetIDs []string `json:"assetIds"`
		Edited   bool     `json:"edited"`
	}{AssetIDs: assetIDs, Edited: true})
	if err != nil {
		return 0, err
	}
	u := c.baseURL.ResolveReference(&url.URL{Path: "/api/download/archive"})
	c.addSharedLinkParams(u)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/octet-stream")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, responseStatusError(req, resp)
	}
	return io.Copy(w, resp.Body)
}

func (c *Client) getSharedLink(ctx context.Context) (SharedLink, error) {
	log.Printf("immich gallery: GET /api/shared-links/me share=%s", redactKey(c.shareKey))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.buildSharedLinkURL().String(), nil)
	if err != nil {
		return SharedLink{}, err
	}
	var link SharedLink
	if err := c.doJSON(req, &link); err != nil {
		return SharedLink{}, err
	}
	return link, nil
}

func (c *Client) listAlbumAssets(ctx context.Context, albumID string) ([]Asset, error) {
	log.Printf("immich gallery: GET /api/albums/%s share=%s", albumID, redactKey(c.shareKey))
	u := c.baseURL.ResolveReference(&url.URL{Path: "/api/albums/" + albumID})
	c.addSharedLinkParams(u)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}

	var album struct {
		Assets []Asset `json:"assets"`
	}
	if err := c.doJSON(req, &album); err != nil {
		return nil, err
	}
	log.Printf("immich gallery: album_id=%s returned %d assets", albumID, len(album.Assets))
	return album.Assets, nil
}

func (c *Client) searchAlbumAssets(ctx context.Context, albumID string) ([]Asset, error) {
	const pageSize = 1000
	page := 1
	seenPages := map[int]bool{}
	var assets []Asset
	for {
		if seenPages[page] {
			return nil, fmt.Errorf("Immich search pagination repeated page %d", page)
		}
		seenPages[page] = true

		payload, err := json.Marshal(struct {
			AlbumIDs []string `json:"albumIds"`
			Page     int      `json:"page"`
			Size     int      `json:"size"`
			WithExif bool     `json:"withExif"`
		}{AlbumIDs: []string{albumID}, Page: page, Size: pageSize, WithExif: true})
		if err != nil {
			return nil, err
		}
		u := c.baseURL.ResolveReference(&url.URL{Path: "/api/search/metadata"})
		c.addSharedLinkParams(u)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		var result struct {
			Assets struct {
				Items    []Asset `json:"items"`
				NextPage *string `json:"nextPage"`
			} `json:"assets"`
		}
		if err := c.doJSON(req, &result); err != nil {
			return nil, err
		}
		assets = append(assets, result.Assets.Items...)
		log.Printf("immich gallery: search page=%d returned=%d accumulated=%d", page, len(result.Assets.Items), len(assets))
		if result.Assets.NextPage == nil {
			log.Printf("immich gallery: paginated asset search complete album_id=%s assets=%d", albumID, len(assets))
			return assets, nil
		}
		nextPage, err := strconv.Atoi(*result.Assets.NextPage)
		if err != nil || nextPage < 1 {
			return nil, fmt.Errorf("invalid Immich search nextPage %q", *result.Assets.NextPage)
		}
		page = nextPage
	}
}

func (g *Gallery) setAlbum(album *Album) {
	g.AlbumName = album.Name
	g.AlbumDescription = album.Description
}

func galleryItemFromAsset(asset Asset) GalleryItem {
	item := GalleryItem{
		ID:       asset.ID,
		Name:     asset.OriginalFileName,
		MimeType: asset.OriginalMimeType,
		Size:     asset.FileSize(),
		Duration: asset.Duration.seconds(),
		SHA1:     decodeBase64SHA1(asset.Checksum),
	}
	if asset.ExifInfo != nil {
		item.Width = asset.ExifInfo.ExifImageWidth
		item.Height = asset.ExifInfo.ExifImageHeight
	}
	return item
}

func (c *Client) GetThumbnail(ctx context.Context, id string, w io.Writer) (int64, error) {
	return c.getAsset(ctx, c.assetURL(id, "/thumbnail"), 0, w)
}

func (c *Client) GetPreview(ctx context.Context, id string, w io.Writer) (int64, error) {
	u, _ := url.Parse(c.assetURL(id, "/thumbnail"))
	q := u.Query()
	q.Set("size", "preview")
	u.RawQuery = q.Encode()
	return c.getAsset(ctx, u.String(), 0, w)
}

func (c *Client) GetAssetInfo(ctx context.Context, id string) (Asset, error) {
	u := c.baseURL.ResolveReference(&url.URL{
		Path:    "/api/assets/" + id,
		RawPath: "/api/assets/" + url.PathEscape(id),
	})
	c.addSharedLinkParams(u)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Asset{}, err
	}
	var asset Asset
	if err := c.doJSON(req, &asset); err != nil {
		return Asset{}, err
	}
	return asset, nil
}

func (c *Client) GetFile(ctx context.Context, id string, w io.Writer) (int64, error) {
	return c.getAsset(ctx, c.assetURL(id, "/original"), 0, w)
}

// GetVideoPlayback returns the transcoded video stream starting from byte 0.
func (c *Client) GetVideoPlayback(ctx context.Context, id string, w io.Writer) (int64, error) {
	return c.getAsset(ctx, c.assetURL(id, "/video/playback"), 0, w)
}

// GetVideoPlaybackRange returns the transcoded video stream starting from startOffset.
// Sends an HTTP Range request; Immich returns 206 + Content-Range for valid offsets.
func (c *Client) GetVideoPlaybackRange(ctx context.Context, id string, startOffset int64, w io.Writer) (int64, error) {
	return c.getAsset(ctx, c.assetURL(id, "/video/playback"), startOffset, w)
}

// HeadVideoPlayback returns the Content-Length of the transcoded video stream,
// or 0 if Immich doesn't report one (e.g. chunked transfer encoding).
// Uses a HEAD request so the response body is not consumed.
func (c *Client) HeadVideoPlayback(ctx context.Context, id string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.assetURL(id, "/video/playback"), nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HEAD /video/playback returned %s", resp.Status)
	}
	return resp.ContentLength, nil
}

func (c *Client) getAsset(ctx context.Context, rawURL string, startOffset int64, w io.Writer) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	if startOffset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", startOffset))
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		return 0, fmt.Errorf("range not satisfiable: start_offset=%d", startOffset)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return 0, responseStatusError(req, resp)
	}
	return io.Copy(w, resp.Body)
}

func (c *Client) doJSON(req *http.Request, out any) error {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return responseStatusError(req, resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func (c *Client) doJSONStatus(req *http.Request, status int, out any) error {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != status {
		return responseStatusError(req, resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func responseStatusError(req *http.Request, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxHTTPErrorBody))
	return fmt.Errorf("%s %s returned %s: %s", req.Method, req.URL.Path, resp.Status, strings.TrimSpace(string(body)))
}

func (c *Client) buildSharedLinkURL() *url.URL {
	u := c.baseURL.ResolveReference(&url.URL{Path: "/api/shared-links/me"})
	c.addSharedLinkParams(u)
	return u
}

func (c *Client) addSharedLinkParams(u *url.URL) {
	q := u.Query()
	q.Set("key", c.shareKey)
	if c.password != "" {
		q.Set("password", c.password)
	}
	u.RawQuery = q.Encode()
}

func (c *Client) assetURL(id, suffix string) string {
	u := c.baseURL.ResolveReference(&url.URL{
		Path:    "/api/assets/" + id + suffix,
		RawPath: "/api/assets/" + url.PathEscape(id) + suffix,
	})
	c.addSharedLinkParams(u)
	return u.String()
}

func decodeBase64SHA1(input string) string {
	raw, err := base64.StdEncoding.DecodeString(input)
	if err != nil || len(raw) != 20 {
		return ""
	}
	return hex.EncodeToString(raw)
}

func parseDurationSeconds(input string) *float64 {
	if input == "" {
		return nil
	}
	parts := strings.Split(input, ":")
	if len(parts) != 3 {
		return nil
	}
	hours, err := strconv.Atoi(parts[0])
	if err != nil {
		return nil
	}
	minutes, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil
	}
	seconds, err := strconv.ParseFloat(parts[2], 64)
	if err != nil {
		return nil
	}
	total := float64(hours*3600+minutes*60) + seconds
	return &total
}

func redactKey(key string) string {
	if key == "" {
		return "<empty>"
	}
	if len(key) <= 8 {
		return key
	}
	return key[:8] + "..."
}
