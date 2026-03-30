package transfer

import (
	"encoding/json"
	"io"
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

// Manager handles file transfer state, authentication, and backpressure.
type Manager struct {
	dc           DataChannel
	client       *opencloud.Client
	transfer     atomic.Bool // true if transfer in progress
	password     string      // empty = no password required
	maxDownloads int         // 0 = unlimited
	downloads    atomic.Int32 // completed download counter
	authFailures atomic.Int32 // consecutive password failures

	// Callbacks for external handling
	OnAuthFailed     func() // called when auth fails after 3 strikes
	OnSessionExpired func() // called when max downloads reached
}

// NewManager creates a transfer manager with optional password and download limit.
func NewManager(dc DataChannel, client *opencloud.Client, password string, maxDownloads int) *Manager {
	return &Manager{
		dc:           dc,
		client:       client,
		password:     password,
		maxDownloads: maxDownloads,
	}
}

// HandleOpen sends the hello message when DataChannel opens.
// Called by the DataChannel open handler.
func (m *Manager) HandleOpen() {
	hello := struct {
		Type             string `json:"type"`
		PasswordRequired bool   `json:"password_required"`
		MaxDownloads     int    `json:"max_downloads,omitempty"`
	}{
		Type:             "hello",
		PasswordRequired: m.password != "",
		MaxDownloads:     m.maxDownloads,
	}

	// Omit MaxDownloads if zero (unlimited)
	if m.maxDownloads == 0 {
		hello.MaxDownloads = 0
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

	// Parse JSON message
	var msg struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		m.sendError("invalid message format")
		return
	}

	switch msg.Type {
	case "list_request":
		m.handleListRequest(msg.Password)
	case "file_request":
		m.handleFileRequest(msg.Name)
	default:
		m.sendError("unknown message type: " + msg.Type)
	}
}

func (m *Manager) handleListRequest(password string) {
	// Check password if required
	if m.password != "" {
		if password != m.password {
			m.handleAuthFailure("invalid password")
			return
		}
		// Reset auth failures on successful password
		m.authFailures.Store(0)
	}

	// Check max downloads limit
	if m.maxDownloads > 0 && int(m.downloads.Load()) >= m.maxDownloads {
		m.sendError("max downloads reached")
		if m.OnSessionExpired != nil {
			m.OnSessionExpired()
		}
		return
	}

	if m.client == nil {
		m.sendError("share unavailable: client not initialized")
		return
	}

	files, err := m.client.ListFiles()
	if err != nil {
		m.sendError("share unavailable: " + err.Error())
		return
	}

	// Convert to JSON
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
		// Log error, connection may be closed
		return
	}
}

func (m *Manager) handleAuthFailure(reason string) {
	failures := m.authFailures.Add(1)

	if failures >= maxAuthFailures {
		m.sendError("authentication failed: too many attempts")
		if m.OnAuthFailed != nil {
			m.OnAuthFailed()
		}
		// Close the channel after 3 strikes
		m.dc.Close()
	} else {
		m.sendError("authentication failed: " + reason)
	}
}

func (m *Manager) handleFileRequest(name string) {
	// Check max downloads limit before starting transfer
	if m.maxDownloads > 0 && int(m.downloads.Load()) >= m.maxDownloads {
		m.sendError("max downloads reached")
		if m.OnSessionExpired != nil {
			m.OnSessionExpired()
		}
		return
	}

	if m.transfer.Load() {
		m.sendError("transfer in progress")
		return
	}
	if name == "" {
		m.sendError("file name required")
		return
	}

	// Get file info to send header
	files, err := m.client.ListFiles()
	if err != nil {
		m.sendError("share unavailable")
		return
	}

	var fileInfo *opencloud.FileInfo
	for _, f := range files {
		if f.Name == name {
			fileInfo = &f
			break
		}
	}
	if fileInfo == nil {
		m.sendError("file not found: " + name)
		return
	}

	// Send file header
	header := struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Size     int64  `json:"size"`
		MimeType string `json:"mimeType"`
	}{
		Type:     "file_header",
		Name:     fileInfo.Name,
		Size:     fileInfo.Size,
		MimeType: fileInfo.ContentType,
	}
	headerData, _ := json.Marshal(header)
	if err := m.dc.SendText(string(headerData)); err != nil {
		return
	}

	// Start transfer
	m.transfer.Store(true)
	go m.streamFile(name)
}

func (m *Manager) streamFile(name string) {
	defer func() { m.transfer.Store(false) }()

	// Create a pipe: WebDAV writes to writer, we read from reader
	pr, pw := io.Pipe()

	// Stream from WebDAV in background
	go func() {
		_, err := m.client.GetFile(name, pw)
		pw.CloseWithError(err)
	}()

	// Read chunks and send
	buf := make([]byte, chunkSize)
	for {
		n, err := pr.Read(buf)
		if n > 0 {
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
