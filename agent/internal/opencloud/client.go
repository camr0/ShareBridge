package opencloud

import (
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

const propfindBody = `<?xml version="1.0" encoding="UTF-8"?>
<D:propfind xmlns:D="DAV:" xmlns:oc="http://owncloud.org/ns">
  <D:prop>
    <D:getcontentlength/>
    <D:getcontenttype/>
    <D:resourcetype/>
    <oc:checksums/>
    <oc:fileid/>
  </D:prop>
</D:propfind>`

// FileInfo describes a file or directory in an OpenCloud share.
type FileInfo struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	ContentType string `json:"mimeType"`
	IsDir       bool   `json:"isDir"`
	SHA1        string `json:"-"` // not sent in file_list; passed separately in file_header
}

// Client provides WebDAV access to OpenCloud public shares.
type Client struct {
	baseURL    string // https://host/remote.php/dav/public-files/{token}
	token      string
	password   string // OpenCloud share password (empty if share is unprotected)
	httpClient *http.Client
}

// New creates a WebDAV client for the given public share URL.
// password is the OpenCloud share password; pass empty string for unprotected shares.
// allowedHost restricts which hosts are permitted (SSRF protection).
func New(shareURL string, allowedHost string, password string) (*Client, error) {
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
		baseURL:  baseURL,
		token:    token,
		password: password,
		httpClient: &http.Client{
			// No global Timeout: large file bodies take minutes to stream.
			// Use transport-level timeouts only (dial, TLS, headers).
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

// authHeader returns the Basic Auth header for WebDAV requests.
// OpenCloud public shares use the token as username, share password (or empty) as password.
func (c *Client) authHeader() string {
	auth := base64.StdEncoding.EncodeToString([]byte(c.token + ":" + c.password))
	return "Basic " + auth
}

// ListFiles returns file and directory info for the share at the given subpath.
// Pass "" for the share root. Pass "docs/reports" for a nested subfolder.
func (c *Client) ListFiles(subpath string) ([]FileInfo, error) {
	requestURL := c.baseURL
	selfPath := "/remote.php/dav/public-files/" + c.token
	if subpath != "" {
		requestURL = c.baseURL + "/" + subpath
		selfPath += "/" + subpath
	}

	req, err := http.NewRequest("PROPFIND", requestURL, strings.NewReader(propfindBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", "1")
	req.Header.Set("Authorization", c.authHeader())
	req.Header.Set("Content-Type", "application/xml")

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

	return parsePROPFIND(body, selfPath)
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

// GetRootFileID returns the oc:fileid of the share root via a Depth:0 PROPFIND.
// Returns empty string without error if oc:fileid is absent (graceful degradation
// for older OpenCloud versions or non-OpenCloud WebDAV servers).
func (c *Client) GetRootFileID() (string, error) {
	req, err := http.NewRequest("PROPFIND", c.baseURL, strings.NewReader(propfindBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Depth", "0")
	req.Header.Set("Authorization", c.authHeader())
	req.Header.Set("Content-Type", "application/xml")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("PROPFIND request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMultiStatus {
		return "", fmt.Errorf("PROPFIND returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	var ms multistatus
	if err := xml.Unmarshal(body, &ms); err != nil {
		return "", fmt.Errorf("parse XML: %w", err)
	}

	if len(ms.Response) == 0 {
		return "", nil
	}
	return ms.Response[0].Propstat.Prop.FileID, nil
}

// PROPFIND response parsing types
type multistatus struct {
	XMLName  xml.Name   `xml:"multistatus"`
	Response []response `xml:"response"`
}

type response struct {
	Href     string `xml:"href"`
	Propstat struct {
		Prop struct {
			ContentLength string `xml:"getcontentlength"`
			ContentType   string `xml:"getcontenttype"`
			ResourceType  struct {
				Collection *struct{} `xml:"collection"`
			} `xml:"resourcetype"`
			Checksums struct {
				Checksum string `xml:"checksum"`
			} `xml:"checksums"`
			FileID string `xml:"fileid"` // oc:fileid, matched by local name
		} `xml:"prop"`
	} `xml:"propstat"`
}

// parsePROPFIND parses the WebDAV PROPFIND response XML.
// selfPath is the URL path of the directory being listed (e.g. "/remote.php/dav/public-files/token"
// for root, or "/remote.php/dav/public-files/token/docs" for a subpath).
// The self-entry (the directory itself) is excluded; its child collections are returned as IsDir:true.
func parsePROPFIND(data []byte, selfPath string) ([]FileInfo, error) {
	var ms multistatus
	if err := xml.Unmarshal(data, &ms); err != nil {
		return nil, fmt.Errorf("parse XML: %w", err)
	}

	var files []FileInfo

	for _, r := range ms.Response {
		href, _ := url.PathUnescape(r.Href)

		if r.Propstat.Prop.ResourceType.Collection != nil {
			// Skip the self-entry (the directory we're listing)
			if path.Clean(href) == path.Clean(selfPath) {
				continue
			}
			// Include child subdirectories as IsDir entries
			files = append(files, FileInfo{
				Name:  path.Base(href),
				IsDir: true,
			})
			continue
		}

		// Extract filename from href
		name := path.Base(href)
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
			IsDir:       false,
			SHA1:        extractSHA1(r.Propstat.Prop.Checksums.Checksum),
		})
	}

	return files, nil
}

func parseInt64(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

// extractSHA1 parses the first SHA1 value from an OpenCloud checksum string.
// Input format: "SHA1:<hex> MD5:<hex> ADLER32:<hex>" (space-separated, any order).
// Returns empty string if no SHA1 token is found.
func extractSHA1(checksumStr string) string {
	for _, token := range strings.Fields(checksumStr) {
		if strings.HasPrefix(token, "SHA1:") {
			return strings.ToLower(token[5:])
		}
	}
	return ""
}
