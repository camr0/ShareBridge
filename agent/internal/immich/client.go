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
	password   string
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
	log.Printf("immich gallery: GET /api/shared-links/me share=%s", redactKey(c.shareKey))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.buildSharedLinkURL().String(), nil)
	if err != nil {
		return Gallery{}, err
	}

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
	log.Printf("immich gallery: share=%s type=%s album_id=%s album_name=%q inline_assets=%d",
		redactKey(c.shareKey), link.Type, albumID, albumName, len(link.Assets))

	assets := link.Assets
	if len(assets) == 0 && strings.EqualFold(link.Type, "ALBUM") && link.Album != nil && link.Album.ID != "" {
		albumAssets, err := c.listAlbumAssets(ctx, link.Album.ID)
		if err != nil {
			return Gallery{}, err
		}
		assets = albumAssets
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

func (c *Client) GetThumbnail(ctx context.Context, id string, w io.Writer) (int64, error) {
	return c.getAsset(ctx, c.assetURL(id, "/thumbnail"), w)
}

func (c *Client) GetPreview(ctx context.Context, id string, w io.Writer) (int64, error) {
	u, _ := url.Parse(c.assetURL(id, "/thumbnail"))
	q := u.Query()
	q.Set("size", "preview")
	u.RawQuery = q.Encode()
	return c.getAsset(ctx, u.String(), w)
}

func (c *Client) GetFile(ctx context.Context, id string, w io.Writer) (int64, error) {
	return c.getAsset(ctx, c.assetURL(id, "/original"), w)
}

// GetVideoPlayback returns the transcoded video stream URL that the browser can play directly.
// Immich serves an HLS/video stream at /api/assets/{id}/video/playback.
func (c *Client) GetVideoPlayback(ctx context.Context, id string, w io.Writer) (int64, error) {
	return c.getAsset(ctx, c.assetURL(id, "/video/playback"), w)
}

func (c *Client) getAsset(ctx context.Context, rawURL string, w io.Writer) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}

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
		return fmt.Errorf("GET %s returned %d: %s", req.URL.Path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
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
