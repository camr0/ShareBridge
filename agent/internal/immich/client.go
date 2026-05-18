package immich

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
	baseURL    *url.URL
	allowed    string
	apiKey     string
	shareKey   string
	tokenMu    sync.RWMutex
	token      string
	httpClient *http.Client
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
	ID               string    `json:"id"`
	OriginalFileName string    `json:"originalFileName"`
	OriginalMimeType string    `json:"originalMimeType"`
	FileSizeInByte   int64     `json:"fileSizeInByte"`
	Type             string    `json:"type"`
	Duration         string    `json:"duration"`
	Checksum         string    `json:"checksum"`
	ExifInfo         *ExifInfo `json:"exifInfo"`
}

type ExifInfo struct {
	ExifImageWidth  int `json:"exifImageWidth"`
	ExifImageHeight int `json:"exifImageHeight"`
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

type timeBucket struct {
	TimeBucket string `json:"timeBucket"`
	Count      int    `json:"count"`
}

type timelineBucket struct {
	ID       []string              `json:"id"`
	IsImage  []bool                `json:"isImage"`
	Ratio    []float64             `json:"ratio"`
	Duration []timelineDurationSec `json:"duration"`
}

type timelineDurationSec struct {
	value *float64
}

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

func (c *Client) ValidatePassword(ctx context.Context, password string) (bool, error) {
	log.Printf("immich auth: POST /api/shared-links/login share=%s", redactKey(c.shareKey))
	body, err := json.Marshal(map[string]string{"password": password})
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.sharedLinkLoginURL(), strings.NewReader(string(body)))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		log.Printf("immich auth: rejected password status=%s share=%s", resp.Status, redactKey(c.shareKey))
		return false, nil
	}

	var link SharedLink
	if err := json.NewDecoder(resp.Body).Decode(&link); err != nil {
		return false, fmt.Errorf("decode shared link: %w", err)
	}
	token := ""
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "immich_shared_link_token" {
			token = cookie.Value
			break
		}
	}
	if token == "" {
		return false, fmt.Errorf("shared link login response did not include auth cookie")
	}
	c.setToken(token)
	log.Printf("immich auth: accepted password and stored shared-link cookie share=%s", redactKey(c.shareKey))
	return true, nil
}

func (c *Client) ListGallery(ctx context.Context) (Gallery, error) {
	log.Printf("immich gallery: GET /api/shared-links/me share=%s", redactKey(c.shareKey))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.sharedLinkURL(), nil)
	if err != nil {
		return Gallery{}, err
	}
	c.addSharedLinkToken(req)

	var link SharedLink
	if err := c.doJSON(req, &link); err != nil {
		return Gallery{}, err
	}
	albumID := ""
	albumName := ""
	if link.Album != nil {
		albumID = link.Album.ID
		albumName = link.Album.Name
	}
	log.Printf("immich gallery: share=%s type=%s album_id=%s album_name=%q expanded_assets=%d", redactKey(c.shareKey), link.Type, albumID, albumName, len(link.Assets))

	assets := link.Assets
	if len(assets) == 0 && strings.EqualFold(link.Type, "ALBUM") && link.Album != nil && link.Album.ID != "" {
		log.Printf("immich gallery: share=%s using timeline fallback album_id=%s", redactKey(c.shareKey), link.Album.ID)
		items, err := c.listAlbumTimelineItems(ctx, link.Album.ID)
		if err != nil {
			return Gallery{}, err
		}
		gallery := Gallery{Items: items}
		gallery.setAlbum(link.Album)
		log.Printf("immich gallery: share=%s timeline fallback produced %d items", redactKey(c.shareKey), len(items))
		return gallery, nil
	}

	gallery := Gallery{Items: make([]GalleryItem, 0, len(assets))}
	if link.Album != nil {
		gallery.setAlbum(link.Album)
	}
	for _, asset := range assets {
		gallery.Items = append(gallery.Items, galleryItemFromAsset(asset))
	}
	log.Printf("immich gallery: share=%s expanded assets produced %d items", redactKey(c.shareKey), len(gallery.Items))
	return gallery, nil
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
		Size:     asset.FileSizeInByte,
		Duration: parseDurationSeconds(asset.Duration),
		SHA1:     decodeBase64SHA1(asset.Checksum),
	}
	if asset.ExifInfo != nil {
		item.Width = asset.ExifInfo.ExifImageWidth
		item.Height = asset.ExifInfo.ExifImageHeight
	}
	return item
}

func (c *Client) listAlbumTimelineItems(ctx context.Context, albumID string) ([]GalleryItem, error) {
	log.Printf("immich gallery: GET /api/timeline/buckets share=%s album_id=%s", redactKey(c.shareKey), albumID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.timelineBucketsURL(albumID), nil)
	if err != nil {
		return nil, err
	}
	c.addSharedLinkToken(req)

	var buckets []timeBucket
	if err := c.doJSON(req, &buckets); err != nil {
		return nil, err
	}
	log.Printf("immich gallery: album_id=%s buckets=%d", albumID, len(buckets))

	items := make([]GalleryItem, 0)
	for _, bucket := range buckets {
		if bucket.TimeBucket == "" || bucket.Count == 0 {
			continue
		}
		log.Printf("immich gallery: GET /api/timeline/bucket share=%s album_id=%s bucket=%s expected_count=%d", redactKey(c.shareKey), albumID, bucket.TimeBucket, bucket.Count)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.timelineBucketURL(albumID, bucket.TimeBucket), nil)
		if err != nil {
			return nil, err
		}
		c.addSharedLinkToken(req)

		var timeline timelineBucket
		if err := c.doJSON(req, &timeline); err != nil {
			return nil, err
		}
		log.Printf("immich gallery: album_id=%s bucket=%s ids=%d duration_values=%d", albumID, bucket.TimeBucket, len(timeline.ID), len(timeline.Duration))
		for i, id := range timeline.ID {
			item := GalleryItem{
				ID:       id,
				Name:     id,
				MimeType: "image/jpeg",
			}
			if i < len(timeline.IsImage) && !timeline.IsImage[i] {
				item.MimeType = "video/mp4"
			}
			if i < len(timeline.Ratio) && timeline.Ratio[i] > 0 {
				item.Width = 1000
				item.Height = int(1000 / timeline.Ratio[i])
			}
			if i < len(timeline.Duration) && timeline.Duration[i].value != nil {
				seconds := *timeline.Duration[i].value
				item.Duration = &seconds
			}
			items = append(items, item)
		}
	}
	return items, nil
}

func (c *Client) GetThumbnail(ctx context.Context, id string, w io.Writer) (int64, error) {
	return c.getAsset(ctx, c.assetURL(id, "/thumbnail"), w)
}

func (c *Client) GetPreview(ctx context.Context, id string, w io.Writer) (int64, error) {
	return c.getAsset(ctx, c.assetURLWithSize(id, "/thumbnail", "preview"), w)
}

func (c *Client) GetFile(ctx context.Context, id string, w io.Writer) (int64, error) {
	return c.getAsset(ctx, c.assetURL(id, "/original"), w)
}

func (c *Client) getAsset(ctx context.Context, rawURL string, w io.Writer) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	c.addSharedLinkToken(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("GET %s returned %s: %s", req.URL.Path, resp.Status, strings.TrimSpace(string(body)))
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
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GET %s returned %s: %s", req.URL.Path, resp.Status, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func (c *Client) sharedLinkURL() string {
	u := c.baseURL.ResolveReference(&url.URL{Path: "/api/shared-links/me"})
	q := u.Query()
	q.Set("key", c.shareKey)
	u.RawQuery = q.Encode()
	return u.String()
}

func (c *Client) sharedLinkLoginURL() string {
	u := c.baseURL.ResolveReference(&url.URL{Path: "/api/shared-links/login"})
	q := u.Query()
	q.Set("key", c.shareKey)
	u.RawQuery = q.Encode()
	return u.String()
}

func (c *Client) assetURL(id, suffix string) string {
	u := c.baseURL.ResolveReference(&url.URL{
		Path:    "/api/assets/" + id + suffix,
		RawPath: "/api/assets/" + url.PathEscape(id) + suffix,
	})
	q := u.Query()
	q.Set("key", c.shareKey)
	u.RawQuery = q.Encode()
	return u.String()
}

func (c *Client) assetURLWithSize(id, suffix, size string) string {
	u, _ := url.Parse(c.assetURL(id, suffix))
	q := u.Query()
	q.Set("size", size)
	u.RawQuery = q.Encode()
	return u.String()
}

func (c *Client) timelineBucketsURL(albumID string) string {
	u := c.baseURL.ResolveReference(&url.URL{Path: "/api/timeline/buckets"})
	q := u.Query()
	q.Set("key", c.shareKey)
	q.Set("albumId", albumID)
	u.RawQuery = q.Encode()
	return u.String()
}

func (c *Client) timelineBucketURL(albumID, bucket string) string {
	u := c.baseURL.ResolveReference(&url.URL{Path: "/api/timeline/bucket"})
	q := u.Query()
	q.Set("key", c.shareKey)
	q.Set("albumId", albumID)
	q.Set("timeBucket", bucket)
	u.RawQuery = q.Encode()
	return u.String()
}

func (c *Client) currentToken() string {
	c.tokenMu.RLock()
	defer c.tokenMu.RUnlock()
	return c.token
}

func (c *Client) setToken(token string) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	c.token = token
}

func (c *Client) addSharedLinkToken(req *http.Request) {
	if token := c.currentToken(); token != "" {
		req.AddCookie(&http.Cookie{Name: "immich_shared_link_token", Value: token})
	}
}

func decodeBase64SHA1(input string) string {
	raw, err := base64.StdEncoding.DecodeString(input)
	if err != nil || len(raw) != 20 {
		return ""
	}
	return hex.EncodeToString(raw)
}

func (d *timelineDurationSec) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		d.value = nil
		return nil
	}
	var numeric float64
	if err := json.Unmarshal(data, &numeric); err == nil {
		seconds := numeric / 1000
		d.value = &seconds
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	d.value = parseDurationSeconds(text)
	return nil
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
