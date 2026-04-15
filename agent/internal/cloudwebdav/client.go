package cloudwebdav

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

// FileInfo describes a file or directory in a public WebDAV share.
type FileInfo struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	ContentType string `json:"mimeType"`
	IsDir       bool   `json:"isDir"`
	SHA1        string `json:"-"` // not sent in file_list; passed separately in file_header
}

type davEndpoint struct {
	baseURL  string
	selfPath string
}

// Client provides WebDAV access to public shares exposed by
// ownCloud-lineage servers such as OpenCloud and Nextcloud.
type Client struct {
	endpoints  []davEndpoint
	token      string
	password   string // OpenCloud share password (empty if share is unprotected)
	httpClient *http.Client
}

// New creates a WebDAV client for the given public share URL.
// password is the public share password; pass empty string for unprotected shares.
// allowedHosts lists permitted hostnames (SSRF protection); pass all configured cloud hosts.
func New(shareURL string, allowedHosts []string, password string) (*Client, error) {
	u, err := url.Parse(shareURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	allowed := false
	for _, host := range allowedHosts {
		if host != "" && u.Host == host {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, fmt.Errorf("host %q not in allowed list", u.Host)
	}

	// Extract token from path like /s/{token} or /remote.php/dav/public-files/{token}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	token := parts[len(parts)-1]
	if token == "" {
		return nil, fmt.Errorf("could not extract token from URL")
	}

	endpoints := []davEndpoint{
		{
			baseURL:  fmt.Sprintf("https://%s/remote.php/dav/public-files/%s", u.Host, token),
			selfPath: "/remote.php/dav/public-files/" + token,
		},
		{
			baseURL:  fmt.Sprintf("https://%s/public.php/dav/files/%s", u.Host, token),
			selfPath: "/public.php/dav/files/" + token,
		},
	}

	return &Client{
		endpoints: endpoints,
		token:     token,
		password:  password,
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
// Public WebDAV shares use the token as username, share password (or empty) as password.
func (c *Client) authHeader() string {
	auth := base64.StdEncoding.EncodeToString([]byte(c.token + ":" + c.password))
	return "Basic " + auth
}

// ListFiles returns file and directory info for the share at the given subpath.
// Pass "" for the share root. Pass "docs/reports" for a nested subfolder.
func (c *Client) ListFiles(subpath string) ([]FileInfo, error) {
	resp, endpoint, err := c.doRequestWithFallback(func(ep davEndpoint) (*http.Request, error) {
		requestURL := ep.baseURL
		if subpath != "" {
			requestURL += "/" + subpath
		}

		req, err := http.NewRequest("PROPFIND", requestURL, strings.NewReader(propfindBody))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Depth", "1")
		req.Header.Set("Authorization", c.authHeader())
		req.Header.Set("Content-Type", "application/xml")
		return req, nil
	}, http.StatusMultiStatus)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	selfPath := endpoint.selfPath
	if subpath != "" {
		selfPath += "/" + subpath
	}
	return parsePROPFIND(body, selfPath)
}

// GetFile streams the named file to the writer.
// Returns number of bytes written.
func (c *Client) GetFile(name string, w io.Writer) (int64, error) {
	resp, _, err := c.doRequestWithFallback(func(ep davEndpoint) (*http.Request, error) {
		req, err := http.NewRequest("GET", ep.baseURL+"/"+name, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", c.authHeader())
		return req, nil
	}, http.StatusOK)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	return io.Copy(w, resp.Body)
}

// GetRootFileID returns the oc:fileid of the shared resource (file or folder).
//
// ownCloud-lineage servers behave differently depending on share type:
//   - Folder share: Depth:0 root entry has oc:fileid directly.
//   - Single-file share: Depth:0 root is a virtual collection with no oc:fileid;
//     oc:fileid is only available on the child file at Depth:1.
//
// Algorithm: try Depth:0 first; if the root has a fileid it's a folder share and
// we return immediately. Otherwise fall back to Depth:1 and return the first
// non-collection child's fileid.
//
// Returns empty string without error if oc:fileid is absent (graceful degradation
// for older servers or non-ownCloud-lineage WebDAV servers).
func (c *Client) GetRootFileID() (string, error) {
	// Step 1: Depth:0 — covers folder shares.
	fileID, err := c.propfindFileID("0")
	if err != nil {
		return "", err
	}
	if fileID != "" {
		return fileID, nil
	}

	// Step 2: Depth:1 — covers single-file shares; skip root, read first child.
	return c.propfindFileID("1")
}

// propfindFileID issues a PROPFIND with the given Depth and returns the first
// non-empty oc:fileid found. For Depth:0 it checks the root entry; for Depth:1
// it skips hrefs ending in "/" and returns the first child's fileid.
func (c *Client) propfindFileID(depth string) (string, error) {
	resp, _, err := c.doRequestWithFallback(func(ep davEndpoint) (*http.Request, error) {
		req, err := http.NewRequest("PROPFIND", ep.baseURL, strings.NewReader(propfindBody))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Depth", depth)
		req.Header.Set("Authorization", c.authHeader())
		req.Header.Set("Content-Type", "application/xml")
		return req, nil
	}, http.StatusMultiStatus)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	var ms multistatus
	if err := xml.Unmarshal(body, &ms); err != nil {
		return "", fmt.Errorf("parse XML: %w", err)
	}

	for _, r := range ms.Response {
		href, _ := url.PathUnescape(r.Href)
		if depth == "1" && strings.HasSuffix(href, "/") {
			continue // skip collection root when scanning children
		}
		if fileID := r.Propstat.Prop.FileID; fileID != "" {
			return fileID, nil
		}
	}
	return "", nil
}

func (c *Client) doRequestWithFallback(buildReq func(davEndpoint) (*http.Request, error), wantStatus int) (*http.Response, davEndpoint, error) {
	var lastStatus int

	for _, endpoint := range c.endpoints {
		req, err := buildReq(endpoint)
		if err != nil {
			return nil, davEndpoint{}, err
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, davEndpoint{}, fmt.Errorf("%s request failed: %w", req.Method, err)
		}
		if resp.StatusCode == wantStatus {
			return resp, endpoint, nil
		}

		lastStatus = resp.StatusCode
		resp.Body.Close()
		if !shouldTryNextEndpoint(resp.StatusCode) {
			break
		}
	}

	return nil, davEndpoint{}, fmt.Errorf("%s returned %d", http.StatusText(wantStatus), lastStatus)
}

func shouldTryNextEndpoint(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusNotFound
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

// extractSHA1 parses the first SHA1 value from a public-share checksum string.
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
