package transfer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"sharebridge/agent/internal/cloudwebdav"
)

const (
	chunkSize     = 64 * 1024  // 64KB
	maxBuffer     = 256 * 1024 // 256KB
	sleepInterval = 10 * time.Millisecond
)

const (
	binaryFrameFileChunk = byte(0x10)
	binaryFrameThumbnail = byte(0x11)
)

// DataChannel abstracts the DataChannel for sending.
type DataChannel interface {
	SendBinary(data []byte) error
	SendText(text string) error
	BufferedAmount() uint64
	Close() error
}

type StorageBackend interface {
	ListFiles(subpath string) ([]cloudwebdav.FileInfo, error)
	GetFile(filePath string, w io.Writer) (int64, error)
	GetSHA1(subpath string) string
}

type GalleryBackend interface {
	ListGallery(ctx context.Context) (Gallery, error)
	GetThumbnail(ctx context.Context, id string, w io.Writer) (int64, error)
	GetAsset(ctx context.Context, id string, quality string, w io.Writer) (int64, error)
}

type Gallery struct {
	AlbumName        string        `json:"albumName"`
	AlbumDescription string        `json:"albumDescription"`
	Items            []GalleryItem `json:"items"`
}

type GalleryItem struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	MimeType string   `json:"mimeType"`
	Width    int      `json:"width"`
	Height   int      `json:"height"`
	Size     int64    `json:"size"`
	Duration *float64 `json:"duration"`
	SHA1     string   `json:"sha1,omitempty"`
}

// Manager handles file transfer state and backpressure.
// Authentication is handled at the signaling layer (HMAC pre-challenge)
// before the WebRTC peer is created — no auth needed here.
type Manager struct {
	dc           DataChannel
	client       StorageBackend
	gallery      GalleryBackend
	transfer     atomic.Bool
	maxDownloads int
	downloads    atomic.Int32

	OnSessionExpired   func()
	OnDownloadComplete func(bytesTransferred int64)
}

// NewManager creates a transfer manager with an optional download limit.
func NewManager(dc DataChannel, client StorageBackend, maxDownloads int) *Manager {
	return &Manager{
		dc:           dc,
		client:       client,
		maxDownloads: maxDownloads,
	}
}

func NewGalleryManager(dc DataChannel, backend GalleryBackend, maxDownloads int) *Manager {
	return &Manager{dc: dc, gallery: backend, maxDownloads: maxDownloads}
}

// HandleOpen sends the hello message when DataChannel opens.
func (m *Manager) HandleOpen() {
	if m.gallery != nil {
		go m.sendGallery()
		return
	}
	data, _ := json.Marshal(map[string]string{"type": "hello"})
	m.dc.SendText(string(data))
}

// HandleMessage processes incoming DataChannel messages.
func (m *Manager) HandleMessage(data []byte) {
	var msg struct {
		Type    string `json:"type"`
		Path    string `json:"path"`
		ID      string `json:"id"`
		Quality string `json:"quality"`
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
	case "asset_request":
		m.handleAssetRequest(msg.ID, msg.Quality)
	default:
		m.sendError("unknown message type: " + msg.Type)
	}
}

func (m *Manager) sendGallery() {
	gallery, err := m.gallery.ListGallery(context.Background())
	if err != nil {
		m.sendError("share unavailable: " + err.Error())
		return
	}
	data, _ := json.Marshal(struct {
		Type string `json:"type"`
		Gallery
	}{Type: "thumbnail_list", Gallery: gallery})
	_ = m.dc.SendText(string(data))

	for i, item := range gallery.Items {
		if i > 65535 {
			break
		}
		var buf bytes.Buffer
		if _, err := m.gallery.GetThumbnail(context.Background(), item.ID, &buf); err != nil {
			continue
		}
		_ = m.sendWithBackpressure(encodeThumbnailFrame(uint16(i), buf.Bytes()))
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
		Files []cloudwebdav.FileInfo `json:"files"`
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

	if strings.Contains(filePath, "..") {
		m.sendError("invalid path")
		return
	}

	if filePath == "" {
		m.sendError("file path required")
		return
	}

	if !m.acquireTransfer() {
		return
	}

	dir := path.Dir(filePath)
	name := path.Base(filePath)
	if dir == "." {
		dir = ""
	}

	if m.client == nil {
		m.releaseTransfer()
		m.sendError("share unavailable: client not initialized")
		return
	}

	files, err := m.client.ListFiles(dir)
	if err != nil {
		m.releaseTransfer()
		m.sendError("share unavailable")
		return
	}

	var fileInfo *cloudwebdav.FileInfo
	for _, f := range files {
		if f.Name == name {
			fi := f
			fileInfo = &fi
			break
		}
	}
	if fileInfo == nil {
		m.releaseTransfer()
		m.sendError("file not found: " + name)
		return
	}

	sha1 := fileInfo.SHA1
	if sha1 == "" {
		// Nextcloud public shares don't expose oc:checksums in PROPFIND.
		// Fall back to the ShareBridge NC extension endpoint.
		sha1 = m.client.GetSHA1(filePath)
	}

	header := struct {
		Type           string `json:"type"`
		Name           string `json:"name"`
		Size           int64  `json:"size"`
		MimeType       string `json:"mimeType"`
		SHA1           string `json:"sha1,omitempty"`
		BinaryEnvelope bool   `json:"binary_envelope"`
	}{
		Type:           "file_header",
		Name:           fileInfo.Name,
		Size:           fileInfo.Size,
		MimeType:       fileInfo.ContentType,
		SHA1:           sha1,
		BinaryEnvelope: true,
	}
	headerData, _ := json.Marshal(header)
	if err := m.dc.SendText(string(headerData)); err != nil {
		m.releaseTransfer()
		return
	}

	go m.streamFile(resolveRequestPath(dir, fileInfo))
}

func (m *Manager) handleAssetRequest(id string, quality string) {
	if m.maxDownloads > 0 && int(m.downloads.Load()) >= m.maxDownloads {
		m.sendError("share has reached its download limit")
		if m.OnSessionExpired != nil {
			m.OnSessionExpired()
		}
		return
	}

	if id == "" {
		m.sendError("asset id required")
		return
	}

	quality, ok := normalizeAssetQuality(quality)
	if !ok {
		m.sendError("unsupported asset quality: " + quality)
		return
	}

	if !m.acquireTransfer() {
		return
	}

	if m.gallery == nil {
		m.releaseTransfer()
		m.sendError("share unavailable: client not initialized")
		return
	}

	gallery, err := m.gallery.ListGallery(context.Background())
	if err != nil {
		m.releaseTransfer()
		m.sendError("share unavailable: " + err.Error())
		return
	}

	var asset *GalleryItem
	for _, item := range gallery.Items {
		if item.ID == id {
			found := item
			asset = &found
			break
		}
	}
	if asset == nil {
		m.releaseTransfer()
		m.sendError("asset not found: " + id)
		return
	}

	header := struct {
		Type           string `json:"type"`
		Name           string `json:"name"`
		Size           int64  `json:"size"`
		MimeType       string `json:"mimeType"`
		SHA1           string `json:"sha1,omitempty"`
		BinaryEnvelope bool   `json:"binary_envelope"`
	}{
		Type:           "file_header",
		Name:           asset.Name,
		Size:           asset.Size,
		MimeType:       asset.MimeType,
		SHA1:           asset.SHA1,
		BinaryEnvelope: true,
	}
	headerData, _ := json.Marshal(header)
	if err := m.dc.SendText(string(headerData)); err != nil {
		m.releaseTransfer()
		return
	}

	go m.streamAsset(id, quality)
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
	var transferErr error
	buf := make([]byte, chunkSize)
	for {
		n, err := pr.Read(buf)
		if n > 0 {
			totalBytes += int64(n)
			if err := m.sendWithBackpressure(encodeFileChunkFrame(buf[:n])); err != nil {
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				transferErr = err
				m.sendError("transfer failed: " + err.Error())
			}
			break
		}
	}

	if transferErr != nil {
		return
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

func (m *Manager) streamAsset(id string, quality string) {
	defer func() { m.transfer.Store(false) }()

	pr, pw := io.Pipe()
	defer pr.Close()

	go func() {
		var err error
		if quality == "thumbnail" {
			_, err = m.gallery.GetThumbnail(context.Background(), id, pw)
		} else {
			_, err = m.gallery.GetAsset(context.Background(), id, quality, pw)
		}
		pw.CloseWithError(err)
	}()

	var totalBytes int64
	var transferErr error
	buf := make([]byte, chunkSize)
	for {
		n, err := pr.Read(buf)
		if n > 0 {
			totalBytes += int64(n)
			if err := m.sendWithBackpressure(encodeFileChunkFrame(buf[:n])); err != nil {
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				transferErr = err
				m.sendError("transfer failed: " + err.Error())
			}
			break
		}
	}

	if transferErr != nil {
		return
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

func resolveRequestPath(dir string, fileInfo *cloudwebdav.FileInfo) string {
	if fileInfo.RequestPath == "" {
		return ""
	}
	if dir == "" {
		return fileInfo.RequestPath
	}
	return path.Join(dir, fileInfo.RequestPath)
}

func encodeThumbnailFrame(index uint16, jpg []byte) []byte {
	out := make([]byte, 3+len(jpg))
	out[0] = binaryFrameThumbnail
	out[1] = byte(index >> 8)
	out[2] = byte(index)
	copy(out[3:], jpg)
	return out
}

func encodeFileChunkFrame(data []byte) []byte {
	out := make([]byte, 1+len(data))
	out[0] = binaryFrameFileChunk
	copy(out[1:], data)
	return out
}

func normalizeAssetQuality(quality string) (string, bool) {
	switch quality {
	case "", "original":
		return "original", true
	case "thumbnail":
		return "thumbnail", true
	default:
		return quality, false
	}
}

func (m *Manager) acquireTransfer() bool {
	if !m.transfer.CompareAndSwap(false, true) {
		m.sendError("transfer in progress")
		return false
	}
	return true
}

func (m *Manager) releaseTransfer() {
	m.transfer.Store(false)
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
