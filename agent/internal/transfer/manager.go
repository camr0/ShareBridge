package transfer

import (
	"encoding/json"
	"io"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"opencloudshare/agent/internal/opencloud"
)

const (
	chunkSize     = 64 * 1024  // 64KB
	maxBuffer     = 256 * 1024 // 256KB
	sleepInterval = 10 * time.Millisecond
	maxAuthFailures = 3 // 3 strikes policy for password auth
)

// DataChannel abstracts the DataChannel for sending.
type DataChannel interface {
	SendBinary(data []byte) error
	SendText(text string) error
	BufferedAmount() uint64
	Close() error
}

// openCloudClient abstracts the OpenCloud WebDAV client for testability.
// *opencloud.Client satisfies this interface.
type openCloudClient interface {
	ListFiles(subpath string) ([]opencloud.FileInfo, error)
	GetFile(filePath string, w io.Writer) (int64, error)
}

// Manager handles file transfer state, authentication, and backpressure.
type Manager struct {
	dc           DataChannel
	client       openCloudClient
	transfer     atomic.Bool  // true if transfer in progress
	password     string       // empty = no password required
	maxDownloads int          // 0 = unlimited
	downloads    atomic.Int32 // completed download counter
	authFailures atomic.Int32 // consecutive password failures
	authenticated atomic.Bool // true once password accepted (or no password required)

	// Callbacks for external handling
	OnAuthFailed       func()             // called when auth fails after 3 strikes
	OnSessionExpired   func()             // called when max downloads reached
	OnDownloadComplete func(bytesTransferred int64) // called after each successful download
}

// NewManager creates a transfer manager with optional password and download limit.
func NewManager(dc DataChannel, client openCloudClient, password string, maxDownloads int) *Manager {
	mgr := &Manager{
		dc:           dc,
		client:       client,
		password:     password,
		maxDownloads: maxDownloads,
	}
	if password == "" {
		mgr.authenticated.Store(true)
	}
	return mgr
}

// HandleOpen sends the hello message when DataChannel opens.
// Called by the DataChannel open handler.
func (m *Manager) HandleOpen() {
	hello := struct {
		Type             string `json:"type"`
		PasswordRequired bool   `json:"password_required"`
	}{
		Type:             "hello",
		PasswordRequired: m.password != "",
	}

	data, _ := json.Marshal(hello)
	m.dc.SendText(string(data))
}

// HandleMessage processes incoming DataChannel messages.
func (m *Manager) HandleMessage(data []byte) {
	// Check if channel is closed due to auth failures
	if m.authFailures.Load() >= maxAuthFailures {
		return // Ignore messages after lockout
	}

	var msg struct {
		Type     string `json:"type"`
		Password string `json:"password"`
		Path     string `json:"path"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		m.sendError("invalid message format")
		return
	}

	// Gate everything except list_request behind authentication.
	// Note: list_request is dual-purpose (auth + folder navigation) — a dedicated
	// auth message on hello would be cleaner but is deferred to the daemon slice.
	if msg.Type != "list_request" && !m.authenticated.Load() {
		m.sendError("authentication required")
		return
	}

	switch msg.Type {
	case "list_request":
		m.handleListRequest(msg.Password, msg.Path)
	case "file_request":
		m.handleFileRequest(msg.Path)
	default:
		m.sendError("unknown message type: " + msg.Type)
	}
}

func (m *Manager) handleListRequest(password, subpath string) {
	// Check password if required
	if m.password != "" {
		if password != m.password {
			m.handleAuthFailure()
			return
		}
		// Correct password — mark session as authenticated
		m.authFailures.Store(0)
		m.authenticated.Store(true)
	}

	// Check max downloads limit
	if m.maxDownloads > 0 && int(m.downloads.Load()) >= m.maxDownloads {
		m.sendError("share has reached its download limit")
		if m.OnSessionExpired != nil {
			m.OnSessionExpired()
		}
		return
	}

	if m.client == nil {
		m.sendError("share unavailable: client not initialized")
		return
	}

	files, err := m.client.ListFiles(subpath)
	if err != nil {
		m.sendError("share unavailable: " + err.Error())
		return
	}

	resp := struct {
		Type  string                `json:"type"`
		Files []opencloud.FileInfo `json:"files"`
	}{
		Type:  "file_list",
		Files: files,
	}

	data, err := json.Marshal(resp)
	if err != nil {
		m.sendError("internal error")
		return
	}

	if err := m.dc.SendText(string(data)); err != nil {
		return
	}
}

func (m *Manager) handleAuthFailure() {
	failures := m.authFailures.Add(1)

	// Notify server of every auth failure (for rate limiting in Slice 5)
	if m.OnAuthFailed != nil {
		m.OnAuthFailed()
	}

	m.sendError("incorrect password")

	if failures >= maxAuthFailures {
		m.dc.Close()
	}
}

func (m *Manager) handleFileRequest(filePath string) {
	// Check max downloads limit before starting transfer
	if m.maxDownloads > 0 && int(m.downloads.Load()) >= m.maxDownloads {
		m.sendError("share has reached its download limit")
		if m.OnSessionExpired != nil {
			m.OnSessionExpired()
		}
		return
	}

	if m.transfer.Load() {
		m.sendError("transfer in progress")
		return
	}

	// Reject path traversal (defense-in-depth; OpenCloud auth is the real protection)
	if strings.Contains(filePath, "..") {
		m.sendError("invalid path")
		return
	}

	if filePath == "" {
		m.sendError("file path required")
		return
	}

	// Split full path into directory and filename
	// path.Dir("README.txt") = "." → use "" for root
	// path.Dir("docs/file.txt") = "docs"
	dir := path.Dir(filePath)
	name := path.Base(filePath)
	if dir == "." {
		dir = ""
	}

	if m.client == nil {
		m.sendError("share unavailable: client not initialized")
		return
	}

	// Validate file exists by listing its parent directory
	files, err := m.client.ListFiles(dir)
	if err != nil {
		m.sendError("share unavailable")
		return
	}

	var fileInfo *opencloud.FileInfo
	for _, f := range files {
		if f.Name == name {
			fi := f
			fileInfo = &fi
			break
		}
	}
	if fileInfo == nil {
		m.sendError("file not found: " + name)
		return
	}

	// Send file header (name is basename — what the browser saves the download as)
	header := struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Size     int64  `json:"size"`
		MimeType string `json:"mimeType"`
		SHA1     string `json:"sha1,omitempty"`
	}{
		Type:     "file_header",
		Name:     fileInfo.Name,
		Size:     fileInfo.Size,
		MimeType: fileInfo.ContentType,
		SHA1:     fileInfo.SHA1,
	}
	headerData, _ := json.Marshal(header)
	if err := m.dc.SendText(string(headerData)); err != nil {
		return
	}

	// Start transfer with full path (e.g. "docs/reports/Q1.pdf")
	m.transfer.Store(true)
	go m.streamFile(filePath)
}

func (m *Manager) streamFile(filePath string) {
	defer func() { m.transfer.Store(false) }()

	// Create a pipe: WebDAV writes to writer, we read from reader
	pr, pw := io.Pipe()
	defer pr.Close() // unblocks the writer goroutine if we exit early

	// Stream from WebDAV in background
	go func() {
		_, err := m.client.GetFile(filePath, pw)
		pw.CloseWithError(err)
	}()

	// Read chunks and send
	var totalBytes int64
	buf := make([]byte, chunkSize)
	for {
		n, err := pr.Read(buf)
		if n > 0 {
			totalBytes += int64(n)
			// Send with backpressure
			if err := m.sendWithBackpressure(buf[:n]); err != nil {
				return // Connection closed
			}
		}
		if err != nil {
			if err != io.EOF {
				m.sendError("transfer failed: " + err.Error())
			}
			break
		}
	}

	// Send end marker
	end := struct {
		Type string `json:"type"`
	}{Type: "chunk_end"}
	endData, _ := json.Marshal(end)
	m.dc.SendText(string(endData))

	// Increment download counter on successful completion
	m.downloads.Add(1)
	if m.OnDownloadComplete != nil {
		m.OnDownloadComplete(totalBytes)
	}
}

func (m *Manager) sendWithBackpressure(data []byte) error {
	for m.dc.BufferedAmount() > maxBuffer {
		time.Sleep(sleepInterval)
	}
	return m.dc.SendBinary(data)
}

func (m *Manager) sendError(message string) {
	err := struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}{
		Type:    "error",
		Message: message,
	}
	data, _ := json.Marshal(err)
	m.dc.SendText(string(data))
}

// SetDownloadCount initializes the download counter from persisted state.
func (m *Manager) SetDownloadCount(n int) {
	m.downloads.Store(int32(n))
}
