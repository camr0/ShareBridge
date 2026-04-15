package transfer

import (
	"encoding/json"
	"io"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"sharebridge/agent/internal/publicshare"
)

const (
	chunkSize     = 64 * 1024  // 64KB
	maxBuffer     = 256 * 1024 // 256KB
	sleepInterval = 10 * time.Millisecond
)

// DataChannel abstracts the DataChannel for sending.
type DataChannel interface {
	SendBinary(data []byte) error
	SendText(text string) error
	BufferedAmount() uint64
	Close() error
}

// openCloudClient abstracts the OpenCloud WebDAV client for testability.
type openCloudClient interface {
	ListFiles(subpath string) ([]publicshare.FileInfo, error)
	GetFile(filePath string, w io.Writer) (int64, error)
}

// Manager handles file transfer state and backpressure.
// Authentication is handled at the signaling layer (HMAC pre-challenge)
// before the WebRTC peer is created — no auth needed here.
type Manager struct {
	dc           DataChannel
	client       openCloudClient
	transfer     atomic.Bool
	maxDownloads int
	downloads    atomic.Int32

	OnSessionExpired   func()
	OnDownloadComplete func(bytesTransferred int64)
}

// NewManager creates a transfer manager with an optional download limit.
func NewManager(dc DataChannel, client openCloudClient, maxDownloads int) *Manager {
	return &Manager{
		dc:           dc,
		client:       client,
		maxDownloads: maxDownloads,
	}
}

// HandleOpen sends the hello message when DataChannel opens.
func (m *Manager) HandleOpen() {
	data, _ := json.Marshal(map[string]string{"type": "hello"})
	m.dc.SendText(string(data))
}

// HandleMessage processes incoming DataChannel messages.
func (m *Manager) HandleMessage(data []byte) {
	var msg struct {
		Type string `json:"type"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		m.sendError("invalid message format")
		return
	}

	switch msg.Type {
	case "list_request":
		m.handleListRequest(msg.Path)
	case "file_request":
		m.handleFileRequest(msg.Path)
	default:
		m.sendError("unknown message type: " + msg.Type)
	}
}

func (m *Manager) handleListRequest(subpath string) {
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
		Type  string                 `json:"type"`
		Files []publicshare.FileInfo `json:"files"`
	}{
		Type:  "file_list",
		Files: files,
	}

	data, err := json.Marshal(resp)
	if err != nil {
		m.sendError("internal error")
		return
	}
	m.dc.SendText(string(data))
}

func (m *Manager) handleFileRequest(filePath string) {
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

	if strings.Contains(filePath, "..") {
		m.sendError("invalid path")
		return
	}

	if filePath == "" {
		m.sendError("file path required")
		return
	}

	dir := path.Dir(filePath)
	name := path.Base(filePath)
	if dir == "." {
		dir = ""
	}

	if m.client == nil {
		m.sendError("share unavailable: client not initialized")
		return
	}

	files, err := m.client.ListFiles(dir)
	if err != nil {
		m.sendError("share unavailable")
		return
	}

	var fileInfo *publicshare.FileInfo
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

	m.transfer.Store(true)
	go m.streamFile(filePath)
}

func (m *Manager) streamFile(filePath string) {
	defer func() { m.transfer.Store(false) }()

	pr, pw := io.Pipe()
	defer pr.Close()

	go func() {
		_, err := m.client.GetFile(filePath, pw)
		pw.CloseWithError(err)
	}()

	var totalBytes int64
	buf := make([]byte, chunkSize)
	for {
		n, err := pr.Read(buf)
		if n > 0 {
			totalBytes += int64(n)
			if err := m.sendWithBackpressure(buf[:n]); err != nil {
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				m.sendError("transfer failed: " + err.Error())
			}
			break
		}
	}

	end := struct {
		Type string `json:"type"`
	}{Type: "chunk_end"}
	endData, _ := json.Marshal(end)
	m.dc.SendText(string(endData))

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
	errMsg := struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}{
		Type:    "error",
		Message: message,
	}
	data, _ := json.Marshal(errMsg)
	m.dc.SendText(string(data))
}

// SetDownloadCount initializes the download counter from persisted state.
func (m *Manager) SetDownloadCount(n int) {
	m.downloads.Store(int32(n))
}
