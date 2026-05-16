package immich

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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
	if got == allowed {
		return true
	}
	host, _, err := net.SplitHostPort(got)
	if err != nil {
		return false
	}
	return host == allowed
}

func (c *Client) PollShares(ctx context.Context) ([]SharedLink, error) {
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
	return shares, nil
}

func (c *Client) ValidatePassword(ctx context.Context, password string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.sharedLinkURL(password), nil)
	if err != nil {
		return false, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return false, nil
	}

	var link SharedLink
	if err := json.NewDecoder(resp.Body).Decode(&link); err != nil {
		return false, fmt.Errorf("decode shared link: %w", err)
	}
	return true, nil
}

func (c *Client) ListGallery(ctx context.Context) (Gallery, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.sharedLinkURL(c.password), nil)
	if err != nil {
		return Gallery{}, err
	}

	var link SharedLink
	if err := c.doJSON(req, &link); err != nil {
		return Gallery{}, err
	}

	gallery := Gallery{Items: make([]GalleryItem, 0, len(link.Assets))}
	if link.Album != nil {
		gallery.AlbumName = link.Album.Name
		gallery.AlbumDescription = link.Album.Description
	}
	for _, asset := range link.Assets {
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
		gallery.Items = append(gallery.Items, item)
	}
	return gallery, nil
}

func (c *Client) GetThumbnail(ctx context.Context, id string, w io.Writer) (int64, error) {
	return c.getAsset(ctx, c.assetURL(id, "/thumbnail"), w)
}

func (c *Client) GetFile(ctx context.Context, id string, w io.Writer) (int64, error) {
	return c.getAsset(ctx, c.assetURL(id, "/original"), w)
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
		return fmt.Errorf("GET %s returned %s: %s", req.URL.Path, resp.Status, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func (c *Client) sharedLinkURL(password string) string {
	u := c.baseURL.ResolveReference(&url.URL{Path: "/api/shared-links/my-share"})
	q := u.Query()
	q.Set("key", c.shareKey)
	if password != "" {
		q.Set("password", password)
	}
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
