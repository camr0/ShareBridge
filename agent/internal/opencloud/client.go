package opencloud

import (
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// FileInfo describes a file in an OpenCloud share.
type FileInfo struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	ContentType string `json:"mimeType"`
}

// Client provides WebDAV access to OpenCloud public shares.
type Client struct {
	baseURL    string // https://host/remote.php/dav/public-files/{token}
	token      string
	httpClient *http.Client
}

// New creates a WebDAV client for the given public share URL.
// allowedHost restricts which hosts are permitted (SSRF protection).
func New(shareURL string, allowedHost string) (*Client, error) {
	u, err := url.Parse(shareURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if u.Host != allowedHost {
		return nil, fmt.Errorf("host %q not in allowed list", u.Host)
	}

	// Extract token from path like /s/{token} or /remote.php/dav/public-files/{token}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	token := parts[len(parts)-1]
	if token == "" {
		return nil, fmt.Errorf("could not extract token from URL")
	}

	// Build WebDAV base URL
	baseURL := fmt.Sprintf("https://%s/remote.php/dav/public-files/%s", u.Host, token)

	return &Client{
		baseURL: baseURL,
		token:   token,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}, nil
}

// authHeader returns the Basic Auth header for WebDAV requests.
// OpenCloud public shares use the token as username with empty password.
func (c *Client) authHeader() string {
	auth := base64.StdEncoding.EncodeToString([]byte(c.token + ":"))
	return "Basic " + auth
}

// ListFiles returns file info for the share.
// For folder shares: returns all items.
// For file shares: returns a single item.
func (c *Client) ListFiles() ([]FileInfo, error) {
	req, err := http.NewRequest("PROPFIND", c.baseURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", "1")
	req.Header.Set("Authorization", c.authHeader())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("PROPFIND request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMultiStatus {
		return nil, fmt.Errorf("PROPFIND returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	return parsePROPFIND(body, c.token)
}

// GetFile streams the named file to the writer.
// Returns number of bytes written.
func (c *Client) GetFile(name string, w io.Writer) (int64, error) {
	url := c.baseURL + "/" + name
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", c.authHeader())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("GET request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("GET returned %d", resp.StatusCode)
	}

	return io.Copy(w, resp.Body)
}

// PROPFIND response parsing types
type multistatus struct {
	XMLName  xml.Name   `xml:"multistatus"`
	Response []response `xml:"response"`
}

type response struct {
	Href   string `xml:"href"`
	Propstat struct {
		Prop struct {
			ContentLength string   `xml:"getcontentlength"`
			ContentType   string   `xml:"getcontenttype"`
			ResourceType  struct {
				Collection *struct{} `xml:"collection"`
			} `xml:"resourcetype"`
		} `xml:"prop"`
	} `xml:"propstat"`
}

// parsePROPFIND parses the WebDAV PROPFIND response XML.
func parsePROPFIND(data []byte, token string) ([]FileInfo, error) {
	var ms multistatus
	if err := xml.Unmarshal(data, &ms); err != nil {
		return nil, fmt.Errorf("parse XML: %w", err)
	}

	var files []FileInfo

	for _, r := range ms.Response {
		// Skip collections (directories)
		if r.Propstat.Prop.ResourceType.Collection != nil {
			continue
		}

		// Extract filename from href using path.Base
		// This handles both single-file shares and files in folders
		name := path.Base(r.Href)
		if name == "" || name == "." {
			continue
		}

		size := int64(0)
		if r.Propstat.Prop.ContentLength != "" {
			size, _ = parseInt64(r.Propstat.Prop.ContentLength)
		}

		files = append(files, FileInfo{
			Name:        name,
			Size:        size,
			ContentType: r.Propstat.Prop.ContentType,
		})
	}

	return files, nil
}

func parseInt64(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}
