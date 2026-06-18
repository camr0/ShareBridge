package transfer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"sharebridge/agent/internal/cloudwebdav"
)

const (
	chunkSize     = 64 * 1024  // 64KB
	maxBuffer     = 5 * 1024 * 1024 // 5MB — deep pipeline for smooth streaming
	sleepInterval = 10 * time.Millisecond
)

const maxConcurrentGalleryThumbnails = 6

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
	GetAssetInfo(ctx context.Context, id string) (string, int64, string, error) // name, size, mimeType
	HeadVideoPlayback(ctx context.Context, id string) (int64, error)           // Content-Length of transcoded video (0 if unknown)
	GetAssetRange(ctx context.Context, id string, quality string, startOffset int64, w io.Writer) (int64, error)
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

	streamCancel      context.CancelFunc
	currentGeneration atomic.Uint32

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
		Type        string `json:"type"`
		Path        string `json:"path"`
		ID          string `json:"id"`
		Quality     string `json:"quality"`
		StartOffset int64  `json:"start_offset"`
		Generation  int    `json:"generation"`
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
	case "asset_preview_request":
		m.handleAssetPreviewRequest(msg.ID, msg.Quality, msg.Generation)
	case "asset_preview_seek":
		m.handleAssetPreviewSeek(msg.ID, msg.Quality, msg.StartOffset, msg.Generation)
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
	log.Printf("transfer gallery: sending thumbnail_list items=%d bytes=%d", len(gallery.Items), len(data))
	if err := m.dc.SendText(string(data)); err != nil {
		log.Printf("transfer gallery: send thumbnail_list failed: %v", err)
		return
	}
	log.Printf("transfer gallery: thumbnail_list sent items=%d", len(gallery.Items))

	sentThumbs, failedThumbs := m.sendGalleryThumbnails(context.Background(), gallery.Items)
	log.Printf("transfer gallery: thumbnail stream finished sent=%d failed_fetch=%d", sentThumbs, failedThumbs)
}

type thumbnailJob struct {
	index int
	item  GalleryItem
}

type thumbnailResult struct {
	index int
	id    string
	data  []byte
	err   error
}

func (m *Manager) sendGalleryThumbnails(ctx context.Context, items []GalleryItem) (int, int) {
	total := len(items)
	if total > 65536 {
		total = 65536
	}
	if total == 0 {
		return 0, 0
	}

	workers := maxConcurrentGalleryThumbnails
	if total < workers {
		workers = total
	}
	jobs := make(chan thumbnailJob)
	results := make(chan thumbnailResult, workers)
	var wg sync.WaitGroup

	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				var buf bytes.Buffer
				_, err := m.gallery.GetThumbnail(ctx, job.item.ID, &buf)
				results <- thumbnailResult{
					index: job.index,
					id:    job.item.ID,
					data:  buf.Bytes(),
					err:   err,
				}
			}
		}()
	}

	go func() {
		for i := 0; i < total; i++ {
			jobs <- thumbnailJob{index: i, item: items[i]}
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	sentThumbs := 0
	failedThumbs := 0
	sendFailed := false
	for result := range results {
		if result.err != nil {
			failedThumbs++
			log.Printf("transfer gallery: thumbnail fetch failed index=%d id=%s: %v", result.index, result.id, result.err)
			continue
		}
		if sendFailed {
			continue
		}
		if err := m.sendWithBackpressure(context.Background(), encodeThumbnailFrame(uint16(result.index), result.data)); err != nil {
			log.Printf("transfer gallery: thumbnail send failed index=%d id=%s bytes=%d: %v", result.index, result.id, len(result.data), err)
			sendFailed = true
			continue
		}
		sentThumbs++
	}
	return sentThumbs, failedThumbs
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
	log.Printf("transfer: asset_request id=%s quality=%s", id, quality)
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

	assetName, assetSize, assetMimeType, err := m.gallery.GetAssetInfo(context.Background(), id)
	if err != nil {
		m.releaseTransfer()
		m.sendError("asset info unavailable: " + err.Error())
		log.Printf("transfer: GetAssetInfo failed id=%s: %v", id, err)
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
		Name:           assetName,
		Size:           assetSize,
		MimeType:       assetMimeType,
		SHA1:           "",
		BinaryEnvelope: true,
	}
	headerData, _ := json.Marshal(header)
	if err := m.dc.SendText(string(headerData)); err != nil {
		log.Printf("transfer: file_header send failed: %v", err)
		m.releaseTransfer()
		return
	}

	go func() { m.streamAsset(id, quality) }()
}

func (m *Manager) handleAssetPreviewRequest(id string, quality string, generation int) {
	log.Printf("transfer: asset_preview_request id=%s quality=%s generation=%d", id, quality, generation)
	if id == "" {
		m.sendError("asset id required")
		return
	}

	quality, ok := normalizePreviewQuality(quality)
	if !ok {
		m.sendError("unsupported preview quality: " + quality)
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

	mimeType := "image/jpeg"
	if quality == "video" {
		mimeType = "video/mp4"
	}

	// Get file size for Content-Length. For video, prefer the transcoded
	// Content-Length from Immich's /video/playback endpoint via HEAD so
	// the browser can calculate accurate byte offsets for seeking.
	// Never forward the original asset size for video — it can differ
	// significantly from the transcoded stream and Chrome will mis-seek.
	_, previewSize, _, _ := m.gallery.GetAssetInfo(context.Background(), id)
	if quality == "video" {
		transcodedSize, err := m.gallery.HeadVideoPlayback(context.Background(), id)
		if err == nil && transcodedSize > 0 {
			log.Printf("transfer: video preview using transcoded size=%d (original=%d)", transcodedSize, previewSize)
			previewSize = transcodedSize
		} else {
			// No accurate size available — don't mislead Chrome with the
			// original file size.  The SW will serve without Content-Length
			// and Chrome can still seek via byte-range probes after parsing
			// the moov atom.
			log.Printf("transfer: video preview no transcoded size (head_err=%v), omitting Content-Length", err)
			previewSize = 0
		}

		// Create cancelable context for stream lifecycle. The seek handler
		// calls m.streamCancel() to stop the old stream before starting a new one.
		ctx, cancel := context.WithCancel(context.Background())
		m.streamCancel = cancel
		m.currentGeneration.Store(uint32(generation))

		header := struct {
			Type           string `json:"type"`
			ID             string `json:"id"`
			MimeType       string `json:"mimeType"`
			Size           int64  `json:"size"`
			BinaryEnvelope bool   `json:"binary_envelope"`
			Generation     int    `json:"generation"`
			ByteOffset     int64  `json:"byte_offset"`
		}{
			Type:           "asset_preview_header",
			ID:             id,
			MimeType:       mimeType,
			Size:           previewSize,
			BinaryEnvelope: true,
			Generation:     generation,
			ByteOffset:     0,
		}
		headerData, _ := json.Marshal(header)
		if err := m.dc.SendText(string(headerData)); err != nil {
			log.Printf("transfer: asset_preview_header send failed id=%s: %v", id, err)
			m.releaseTransfer()
			cancel()
			m.streamCancel = nil
			return
		}
		log.Printf("transfer: asset_preview_header sent id=%s mime=%s size=%d generation=%d", id, mimeType, previewSize, generation)

		go m.streamAssetPreview(ctx, id, quality, uint32(generation))
		return
	}

	// Non-video preview path (unchanged).
	header := struct {
		Type           string `json:"type"`
		ID             string `json:"id"`
		MimeType       string `json:"mimeType"`
		Size           int64  `json:"size"`
		BinaryEnvelope bool   `json:"binary_envelope"`
	}{
		Type:           "asset_preview_header",
		ID:             id,
		MimeType:       mimeType,
		Size:           previewSize,
		BinaryEnvelope: true,
	}
	headerData, _ := json.Marshal(header)
	if err := m.dc.SendText(string(headerData)); err != nil {
		log.Printf("transfer: asset_preview_header send failed id=%s: %v", id, err)
		m.releaseTransfer()
		return
	}
	log.Printf("transfer: asset_preview_header sent id=%s mime=%s size=%d", id, mimeType, previewSize)

	go m.streamAssetPreview(context.Background(), id, quality, 0)
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
			if chunkErr := m.sendWithBackpressure(context.Background(), encodeFileChunkFrameLegacy(buf[:n])); chunkErr != nil {
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
			if chunkErr := m.sendWithBackpressure(context.Background(), encodeFileChunkFrameLegacy(buf[:n])); chunkErr != nil {
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

func (m *Manager) handleAssetPreviewSeek(id string, quality string, startOffset int64, generation int) {
	log.Printf("transfer: asset_preview_seek id=%s start_offset=%d generation=%d", id, startOffset, generation)
	if id == "" || quality == "" {
		m.sendError("asset_preview_seek: id and quality required")
		return
	}
	if startOffset < 0 {
		m.sendError("asset_preview_seek: start_offset must be >= 0")
		return
	}
	if m.gallery == nil {
		m.sendError("share unavailable: client not initialized")
		return
	}

	// Single GET with first-byte validation — do NOT cancel the old stream yet.
	// We validate the Immich Range request succeeds before tearing anything down.
	pr, pw := io.Pipe()

	go func() {
		_, err := m.gallery.GetAssetRange(context.Background(), id, quality, startOffset, pw)
		pw.CloseWithError(err)
	}()

	// Read first byte to validate the Immich response arrived.
	firstByte := make([]byte, 1)
	_, readErr := io.ReadFull(pr, firstByte)
	if readErr != nil {
		pr.Close()
		log.Printf("transfer: seek validation failed id=%s start_offset=%d: %v", id, startOffset, readErr)
		m.sendError("preview seek failed: " + readErr.Error())
		return
	}

	// Validation succeeded. Store new generation FIRST so the old stream's
	// send-guard immediately fails, then cancel the old stream.
	m.currentGeneration.Store(uint32(generation))

	// Cancel old stream if one is running.
	if m.streamCancel != nil {
		m.streamCancel()
		m.streamCancel = nil
	}

	// Release transfer lock — the old stream's deferred unlock is
	// generation-guarded and will be a no-op since we stored a new generation.
	m.transfer.Store(false)

	// Acquire the lock for the new stream goroutine.
	if !m.acquireTransfer() {
		log.Printf("transfer: seek failed to acquire transfer lock id=%s", id)
		pr.Close()
		return
	}

	// Create new cancelable context for the new stream.
	ctx, cancel := context.WithCancel(context.Background())
	m.streamCancel = cancel

	// Send header with byte_offset so the page knows where this stream starts.
	header := struct {
		Type           string `json:"type"`
		ID             string `json:"id"`
		MimeType       string `json:"mimeType"`
		Size           int64  `json:"size"`
		BinaryEnvelope bool   `json:"binary_envelope"`
		Generation     int    `json:"generation"`
		ByteOffset     int64  `json:"byte_offset"`
	}{
		Type:           "asset_preview_header",
		ID:             id,
		MimeType:       "video/mp4",
		BinaryEnvelope: true,
		Generation:     generation,
		ByteOffset:     startOffset,
	}
	headerData, _ := json.Marshal(header)
	if err := m.dc.SendText(string(headerData)); err != nil {
		log.Printf("transfer: seek header send failed id=%s: %v", id, err)
		pr.Close()
		cancel()
		m.streamCancel = nil
		return
	}
	log.Printf("transfer: seek header sent id=%s byte_offset=%d generation=%d", id, startOffset, generation)

	// Prepend the already-read byte and stream from the pipe via a fresh goroutine.
	reader := io.MultiReader(bytes.NewReader(firstByte), pr)
	go m.streamAssetPreviewFromReader(ctx, id, reader, uint32(generation))
}

func (m *Manager) streamAssetPreview(ctx context.Context, id string, quality string, generation uint32) {
	defer func() {
		// Only release transfer lock if we're still the active generation.
		if m.currentGeneration.Load() == generation {
			m.transfer.Store(false)
		}
	}()

	pr, pw := io.Pipe()
	defer pr.Close()

	go func() {
		_, err := m.gallery.GetAsset(context.Background(), id, quality, pw)
		pw.CloseWithError(err)
	}()

	m.streamAssetPreviewFromReader(ctx, id, pr, generation)
}

// streamAssetPreviewFromReader streams chunks from reader, guarded by generation.
// The caller must hold the transfer lock (acquired via acquireTransfer()).
func (m *Manager) streamAssetPreviewFromReader(ctx context.Context, id string, reader io.Reader, generation uint32) {
	defer func() {
		if m.currentGeneration.Load() == generation {
			m.transfer.Store(false)
		}
	}()

	var totalBytes int64
	var transferErr error
	chunksSent := 0
	buf := make([]byte, chunkSize)
	for {
		// Check generation before every read. If stale, abort immediately.
		select {
		case <-ctx.Done():
			log.Printf("transfer: preview stream cancelled generation=%d bytes=%d", generation, totalBytes)
			return
		default:
		}
		if m.currentGeneration.Load() != generation {
			log.Printf("transfer: preview stale generation=%d (current=%d), aborting", generation, m.currentGeneration.Load())
			return
		}

		n, err := reader.Read(buf)
		if n > 0 {
			totalBytes += int64(n)
			chunksSent++
			if chunksSent == 1 {
				log.Printf("transfer: preview first chunk generation=%d size=%d", generation, n)
			}
			var frame []byte
			if generation > 0 {
				frame = encodeFileChunkFrame(generation, buf[:n])
			} else {
				frame = encodeFileChunkFrameLegacy(buf[:n])
			}
			if chunkSendErr := m.sendWithBackpressure(ctx, frame); chunkSendErr != nil {
				log.Printf("transfer: preview send failed generation=%d bytes=%d chunks=%d: %v", generation, totalBytes, chunksSent, chunkSendErr)
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				transferErr = err
				log.Printf("transfer: preview read failed generation=%d bytes=%d: %v", generation, totalBytes, err)
				m.sendError("preview failed: " + err.Error())
			}
			break
		}
	}

	log.Printf("transfer: preview finished generation=%d bytes=%d err=%v", generation, totalBytes, transferErr)
	if transferErr != nil {
		return
	}

	// Only send end if we're still the active generation.
	if m.currentGeneration.Load() == generation {
		end := struct {
			Type       string `json:"type"`
			ID         string `json:"id"`
			Generation int    `json:"generation"`
		}{Type: "asset_preview_end", ID: id, Generation: int(generation)}
		endData, _ := json.Marshal(end)
		m.dc.SendText(string(endData))
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

func encodeFileChunkFrame(generation uint32, data []byte) []byte {
	out := make([]byte, 5+len(data)) // 1 (type 0x10) + 4 (generation uint32 BE) + data
	out[0] = binaryFrameFileChunk
	out[1] = byte(generation >> 24)
	out[2] = byte(generation >> 16)
	out[3] = byte(generation >> 8)
	out[4] = byte(generation)
	copy(out[5:], data)
	return out
}

// encodeFileChunkFrameLegacy is used by non-preview file transfers that don't
// participate in the generation protocol.
func encodeFileChunkFrameLegacy(data []byte) []byte {
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
	case "video":
		return "video", true
	default:
		return quality, false
	}
}

func normalizePreviewQuality(quality string) (string, bool) {
	switch quality {
	case "", "preview":
		return "preview", true
	case "video":
		return "video", true
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

func (m *Manager) sendWithBackpressure(ctx context.Context, data []byte) error {
	for m.dc.BufferedAmount() > maxBuffer {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			time.Sleep(sleepInterval)
		}
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
