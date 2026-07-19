package transfer

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"sharebridge/agent/internal/cloudwebdav"
	"sharebridge/agent/internal/multilane"
)

const (
	chunkSize     = 64 * 1024       // 64KB
	maxBuffer     = 5 * 1024 * 1024 // 5MB — deep pipeline for smooth streaming
	sleepInterval = 10 * time.Millisecond
)

const maxConcurrentGalleryThumbnails = 6

const (
	binaryFrameFileChunk = byte(0x10)
	binaryFrameThumbnail = byte(0x11)
	chunkEnvelopeVersion = byte(0x01)
	chunkEnvelopeSize    = 14 // type + version + uint64 operation + uint32 generation
)

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
	HeadVideoPlayback(ctx context.Context, id string) (int64, error)            // Content-Length of transcoded video (0 if unknown)
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
	channels      multilane.ChannelSet
	control       multilane.Endpoint
	media         multilane.Endpoint
	bulk          multilane.Endpoint
	client        StorageBackend
	gallery       GalleryBackend
	mediaTransfer atomic.Bool
	bulkTransfer  atomic.Bool
	maxDownloads  int
	downloads     atomic.Int32

	mediaMu           sync.Mutex
	mediaCancel       context.CancelFunc
	mediaID           string
	mediaQuality      string
	operationSequence atomic.Uint64
	mediaOperation    atomic.Uint64
	bulkOperation     atomic.Uint64
	currentGeneration atomic.Uint32

	OnSessionExpired   func()
	OnDownloadComplete func(bytesTransferred int64)
}

// NewManager creates a transfer manager with an optional download limit.
func NewManager(channels multilane.ChannelSet, client StorageBackend, maxDownloads int) *Manager {
	m := &Manager{
		channels:     channels,
		client:       client,
		maxDownloads: maxDownloads,
	}
	m.installChannels()
	return m
}

func NewGalleryManager(channels multilane.ChannelSet, backend GalleryBackend, maxDownloads int) *Manager {
	m := &Manager{channels: channels, gallery: backend, maxDownloads: maxDownloads}
	m.installChannels()
	return m
}

func (m *Manager) installChannels() {
	if m.channels == nil {
		return
	}
	m.control = m.channels.Endpoint(multilane.LaneControl)
	m.media = m.channels.Endpoint(multilane.LaneMedia)
	m.bulk = m.channels.Endpoint(multilane.LaneBulk)
	if m.control != nil {
		m.control.SetOnMessage(m.HandleMessage)
	}
}

// HandleOpen sends the hello message when DataChannel opens.
func (m *Manager) HandleOpen() {
	if m.gallery != nil {
		go m.sendGallery()
		return
	}
	data, _ := json.Marshal(map[string]string{"type": "hello"})
	_ = m.control.SendText(string(data))
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
		RequestID   string `json:"request_id"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		m.sendError("connection", "", "invalid message format")
		return
	}

	switch msg.Type {
	case "list_request":
		m.handleListRequest(msg.Path, msg.RequestID)
	case "file_request":
		m.handleFileRequest(msg.Path, msg.RequestID)
	case "asset_request":
		m.handleAssetRequest(msg.ID, msg.Quality, msg.RequestID)
	case "asset_preview_request":
		m.handleAssetPreviewRequest(msg.ID, msg.Quality, msg.Generation, msg.RequestID)
	case "asset_preview_seek":
		m.handleAssetPreviewSeek(msg.ID, msg.Quality, msg.StartOffset, msg.Generation, msg.RequestID)
	default:
		m.sendError("connection", msg.RequestID, "unknown message type: "+msg.Type)
	}
}

func (m *Manager) sendGallery() {
	gallery, err := m.gallery.ListGallery(context.Background())
	if err != nil {
		m.sendError("media", "", "share unavailable: "+err.Error())
		return
	}
	data, _ := json.Marshal(struct {
		Type string `json:"type"`
		Gallery
	}{Type: "thumbnail_list", Gallery: gallery})
	log.Printf("transfer gallery: sending thumbnail_list items=%d bytes=%d", len(gallery.Items), len(data))
	if err := m.control.SendText(string(data)); err != nil {
		log.Printf("transfer gallery: send thumbnail_list failed: %v", err)
		return
	}
	log.Printf("transfer gallery: thumbnail_list sent items=%d", len(gallery.Items))

	sentThumbs, failedThumbs := m.sendGalleryThumbnails(context.Background(), gallery.Items)
	log.Printf("transfer gallery: thumbnail stream finished sent=%d failed_fetch=%d", sentThumbs, failedThumbs)
	data, _ = json.Marshal(struct {
		Type   string `json:"type"`
		Sent   int    `json:"sent"`
		Failed int    `json:"failed"`
	}{Type: "thumbnail_complete", Sent: sentThumbs, Failed: failedThumbs})
	if err := m.control.SendText(string(data)); err != nil {
		log.Printf("transfer gallery: send thumbnail_complete failed: %v", err)
	}
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
		if err := m.sendWithBackpressure(context.Background(), m.media, multilane.ClassThumbnail, encodeThumbnailFrame(uint16(result.index), result.data)); err != nil {
			log.Printf("transfer gallery: thumbnail send failed index=%d id=%s bytes=%d: %v", result.index, result.id, len(result.data), err)
			sendFailed = true
			continue
		}
		sentThumbs++
	}
	return sentThumbs, failedThumbs
}

func (m *Manager) handleListRequest(subpath, requestID string) {
	if m.maxDownloads > 0 && int(m.downloads.Load()) >= m.maxDownloads {
		m.sendError("bulk", requestID, "share has reached its download limit")
		if m.OnSessionExpired != nil {
			m.OnSessionExpired()
		}
		return
	}

	if m.client == nil {
		m.sendError("connection", requestID, "share unavailable: client not initialized")
		return
	}

	files, err := m.client.ListFiles(subpath)
	if err != nil {
		m.sendError("connection", requestID, "share unavailable: "+err.Error())
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
		m.sendError("connection", requestID, "internal error")
		return
	}
	_ = m.control.SendText(string(data))
}

func (m *Manager) handleFileRequest(filePath, requestID string) {
	if m.maxDownloads > 0 && int(m.downloads.Load()) >= m.maxDownloads {
		m.sendError("bulk", requestID, "share has reached its download limit")
		if m.OnSessionExpired != nil {
			m.OnSessionExpired()
		}
		return
	}

	if strings.Contains(filePath, "..") {
		m.sendError("bulk", requestID, "invalid path")
		return
	}

	if filePath == "" {
		m.sendError("bulk", requestID, "file path required")
		return
	}

	operationID, ok := m.acquireBulk(requestID)
	if !ok {
		return
	}

	dir := path.Dir(filePath)
	name := path.Base(filePath)
	if dir == "." {
		dir = ""
	}

	if m.client == nil {
		m.releaseBulk(operationID)
		m.sendError("bulk", requestID, "share unavailable: client not initialized")
		return
	}

	files, err := m.client.ListFiles(dir)
	if err != nil {
		m.releaseBulk(operationID)
		m.sendError("bulk", requestID, "share unavailable")
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
		m.releaseBulk(operationID)
		m.sendError("bulk", requestID, "file not found: "+name)
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
		Scope          string `json:"scope"`
		RequestID      string `json:"request_id,omitempty"`
		OperationID    uint64 `json:"operation_id"`
	}{
		Type:           "file_header",
		Name:           fileInfo.Name,
		Size:           fileInfo.Size,
		MimeType:       fileInfo.ContentType,
		SHA1:           sha1,
		BinaryEnvelope: true,
		Scope:          "bulk",
		RequestID:      requestID,
		OperationID:    operationID,
	}
	headerData, _ := json.Marshal(header)
	if err := m.control.SendText(string(headerData)); err != nil {
		m.releaseBulk(operationID)
		return
	}

	go m.streamFile(resolveRequestPath(dir, fileInfo), requestID, operationID)
}

func (m *Manager) handleAssetRequest(id string, quality string, requestID string) {
	log.Printf("transfer: asset_request id=%s quality=%s", id, quality)
	if m.maxDownloads > 0 && int(m.downloads.Load()) >= m.maxDownloads {
		m.sendError("bulk", requestID, "share has reached its download limit")
		if m.OnSessionExpired != nil {
			m.OnSessionExpired()
		}
		return
	}

	if id == "" {
		m.sendError("bulk", requestID, "asset id required")
		return
	}

	quality, ok := normalizeAssetQuality(quality)
	if !ok {
		m.sendError("bulk", requestID, "unsupported asset quality: "+quality)
		return
	}

	operationID, ok := m.acquireBulk(requestID)
	if !ok {
		return
	}

	if m.gallery == nil {
		m.releaseBulk(operationID)
		m.sendError("bulk", requestID, "share unavailable: client not initialized")
		return
	}

	assetName, assetSize, assetMimeType, err := m.gallery.GetAssetInfo(context.Background(), id)
	if err != nil {
		m.releaseBulk(operationID)
		m.sendError("bulk", requestID, "asset info unavailable: "+err.Error())
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
		Scope          string `json:"scope"`
		RequestID      string `json:"request_id,omitempty"`
		OperationID    uint64 `json:"operation_id"`
	}{
		Type:           "file_header",
		Name:           assetName,
		Size:           assetSize,
		MimeType:       assetMimeType,
		SHA1:           "",
		BinaryEnvelope: true,
		Scope:          "bulk",
		RequestID:      requestID,
		OperationID:    operationID,
	}
	headerData, _ := json.Marshal(header)
	if err := m.control.SendText(string(headerData)); err != nil {
		log.Printf("transfer: file_header send failed: %v", err)
		m.releaseBulk(operationID)
		return
	}

	go func() { m.streamAsset(id, quality, requestID, operationID) }()
}

func (m *Manager) handleAssetPreviewRequest(id string, quality string, generation int, requestID string) {
	log.Printf("transfer: asset_preview_request id=%s quality=%s generation=%d", id, quality, generation)
	if id == "" {
		m.sendError("media", requestID, "asset id required")
		return
	}

	quality, ok := normalizePreviewQuality(quality)
	if !ok {
		m.sendError("media", requestID, "unsupported preview quality: "+quality)
		return
	}

	if m.gallery == nil {
		m.sendError("media", requestID, "share unavailable: client not initialized")
		return
	}
	if generation < 0 || uint64(generation) > uint64(^uint32(0)) {
		m.sendError("media", requestID, "preview generation out of range")
		return
	}

	ctx, mediaOperation := m.beginMedia(id, quality, uint32(generation))

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

		header := struct {
			Type           string `json:"type"`
			ID             string `json:"id"`
			MimeType       string `json:"mimeType"`
			Size           int64  `json:"size"`
			BinaryEnvelope bool   `json:"binary_envelope"`
			Generation     int    `json:"generation"`
			ByteOffset     int64  `json:"byte_offset"`
			Scope          string `json:"scope"`
			RequestID      string `json:"request_id,omitempty"`
			OperationID    uint64 `json:"operation_id"`
		}{
			Type:           "asset_preview_header",
			ID:             id,
			MimeType:       mimeType,
			Size:           previewSize,
			BinaryEnvelope: true,
			Generation:     generation,
			ByteOffset:     0,
			Scope:          "media",
			RequestID:      requestID,
			OperationID:    mediaOperation,
		}
		headerData, _ := json.Marshal(header)
		if err := m.sendMediaControlIfCurrent(mediaOperation, string(headerData)); err != nil {
			log.Printf("transfer: asset_preview_header send failed id=%s: %v", id, err)
			m.finishMedia(mediaOperation)
			return
		}
		log.Printf("transfer: asset_preview_header sent id=%s mime=%s size=%d generation=%d", id, mimeType, previewSize, generation)

		go m.streamAssetPreview(ctx, id, quality, uint32(generation), chunkSize, mediaOperation, requestID)
		return
	}

	// Non-video preview path (unchanged).
	header := struct {
		Type           string `json:"type"`
		ID             string `json:"id"`
		MimeType       string `json:"mimeType"`
		Size           int64  `json:"size"`
		BinaryEnvelope bool   `json:"binary_envelope"`
		Scope          string `json:"scope"`
		RequestID      string `json:"request_id,omitempty"`
		OperationID    uint64 `json:"operation_id"`
	}{
		Type:           "asset_preview_header",
		ID:             id,
		MimeType:       mimeType,
		Size:           previewSize,
		BinaryEnvelope: true,
		Scope:          "media",
		RequestID:      requestID,
		OperationID:    mediaOperation,
	}
	headerData, _ := json.Marshal(header)
	if err := m.sendMediaControlIfCurrent(mediaOperation, string(headerData)); err != nil {
		log.Printf("transfer: asset_preview_header send failed id=%s: %v", id, err)
		m.finishMedia(mediaOperation)
		return
	}
	log.Printf("transfer: asset_preview_header sent id=%s mime=%s size=%d", id, mimeType, previewSize)

	go m.streamAssetPreview(ctx, id, quality, 0, chunkSize, mediaOperation, requestID)
}

func (m *Manager) streamFile(filePath, requestID string, operationID uint64) {
	defer m.releaseBulk(operationID)

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
			if chunkErr := m.sendWithBackpressure(context.Background(), m.bulk, multilane.ClassBulk, encodeChunkFrame(operationID, 0, buf[:n])); chunkErr != nil {
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				transferErr = err
				m.sendError("bulk", requestID, "transfer failed: "+err.Error())
			}
			break
		}
	}

	if transferErr != nil {
		return
	}

	end := struct {
		Type        string `json:"type"`
		Scope       string `json:"scope"`
		RequestID   string `json:"request_id,omitempty"`
		OperationID uint64 `json:"operation_id"`
	}{Type: "chunk_end", Scope: "bulk", RequestID: requestID, OperationID: operationID}
	endData, _ := json.Marshal(end)
	if err := m.control.SendText(string(endData)); err != nil {
		return
	}

	m.downloads.Add(1)
	if m.OnDownloadComplete != nil {
		m.OnDownloadComplete(totalBytes)
	}
}

func (m *Manager) streamAsset(id string, quality string, requestID string, operationID uint64) {
	defer m.releaseBulk(operationID)

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
			if chunkErr := m.sendWithBackpressure(context.Background(), m.bulk, multilane.ClassBulk, encodeChunkFrame(operationID, 0, buf[:n])); chunkErr != nil {
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				transferErr = err
				m.sendError("bulk", requestID, "transfer failed: "+err.Error())
			}
			break
		}
	}

	if transferErr != nil {
		return
	}

	end := struct {
		Type        string `json:"type"`
		Scope       string `json:"scope"`
		RequestID   string `json:"request_id,omitempty"`
		OperationID uint64 `json:"operation_id"`
	}{Type: "chunk_end", Scope: "bulk", RequestID: requestID, OperationID: operationID}
	endData, _ := json.Marshal(end)
	if err := m.control.SendText(string(endData)); err != nil {
		return
	}

	m.downloads.Add(1)
	if m.OnDownloadComplete != nil {
		m.OnDownloadComplete(totalBytes)
	}
}

func (m *Manager) handleAssetPreviewSeek(id string, quality string, startOffset int64, generation int, requestID string) {
	log.Printf("transfer: asset_preview_seek id=%s start_offset=%d generation=%d", id, startOffset, generation)
	if id == "" || quality == "" {
		m.sendError("media", requestID, "asset_preview_seek: id and quality required")
		return
	}
	if startOffset < 0 {
		m.sendError("media", requestID, "asset_preview_seek: start_offset must be >= 0")
		return
	}
	normalizedQuality, ok := normalizePreviewQuality(quality)
	if !ok || normalizedQuality != "video" {
		m.sendError("media", requestID, "asset_preview_seek: video quality required")
		return
	}
	if generation <= 0 || uint64(generation) > uint64(^uint32(0)) {
		m.sendError("media", requestID, "asset_preview_seek: generation out of range")
		return
	}
	if m.gallery == nil {
		m.sendError("media", requestID, "share unavailable: client not initialized")
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
		m.sendError("media", requestID, "preview seek failed: "+readErr.Error())
		return
	}

	// Validation succeeded. Atomically accept only a strictly newer generation;
	// a delayed older seek is stale and must not replace the active media stream.
	ctx, mediaOperation, accepted := m.beginMediaIfNewer(id, normalizedQuality, uint32(generation))
	if !accepted {
		_ = pr.Close()
		return
	}

	// Send header with byte_offset so the page knows where this stream starts.
	header := struct {
		Type           string `json:"type"`
		ID             string `json:"id"`
		MimeType       string `json:"mimeType"`
		Size           int64  `json:"size"`
		BinaryEnvelope bool   `json:"binary_envelope"`
		Generation     int    `json:"generation"`
		ByteOffset     int64  `json:"byte_offset"`
		Scope          string `json:"scope"`
		RequestID      string `json:"request_id,omitempty"`
		OperationID    uint64 `json:"operation_id"`
	}{
		Type:           "asset_preview_header",
		ID:             id,
		MimeType:       "video/mp4",
		BinaryEnvelope: true,
		Generation:     generation,
		ByteOffset:     startOffset,
		Scope:          "media",
		RequestID:      requestID,
		OperationID:    mediaOperation,
	}
	headerData, _ := json.Marshal(header)
	if err := m.sendMediaControlIfCurrent(mediaOperation, string(headerData)); err != nil {
		log.Printf("transfer: seek header send failed id=%s: %v", id, err)
		pr.Close()
		m.finishMedia(mediaOperation)
		return
	}
	log.Printf("transfer: seek header sent id=%s byte_offset=%d generation=%d", id, startOffset, generation)

	// Prepend the already-read byte and stream from the pipe via a fresh goroutine.
	reader := io.MultiReader(bytes.NewReader(firstByte), pr)
	go m.streamAssetPreviewFromReader(ctx, id, reader, uint32(generation), chunkSize, mediaOperation, requestID)
}

func (m *Manager) streamAssetPreview(ctx context.Context, id string, quality string, generation uint32, bufSize int, mediaOperation uint64, requestID string) {
	pr, pw := io.Pipe()
	defer pr.Close()

	go func() {
		_, err := m.gallery.GetAsset(ctx, id, quality, pw)
		pw.CloseWithError(err)
	}()

	m.streamAssetPreviewFromReader(ctx, id, pr, generation, bufSize, mediaOperation, requestID)
}

// streamAssetPreviewFromReader streams chunks from reader, guarded by both the
// media operation and the video generation.
func (m *Manager) streamAssetPreviewFromReader(ctx context.Context, id string, reader io.Reader, generation uint32, bufSize int, mediaOperation uint64, requestID string) {
	defer m.finishMedia(mediaOperation)

	var totalBytes int64
	var transferErr error
	chunksSent := 0
	if bufSize <= 0 || bufSize > chunkSize {
		bufSize = chunkSize
	}
	buf := make([]byte, bufSize)
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
			if ctx.Err() != nil || m.mediaOperation.Load() != mediaOperation {
				log.Printf("transfer: discarding stale preview chunk operation=%d generation=%d size=%d", mediaOperation, generation, n)
				return
			}
			totalBytes += int64(n)
			chunksSent++
			if chunksSent == 1 {
				log.Printf("transfer: preview first chunk generation=%d size=%d", generation, n)
			}
			frame := encodeChunkFrame(mediaOperation, generation, buf[:n])
			if chunkSendErr := m.sendInteractiveMedia(ctx, mediaOperation, frame); chunkSendErr != nil {
				log.Printf("transfer: preview send failed generation=%d bytes=%d chunks=%d: %v", generation, totalBytes, chunksSent, chunkSendErr)
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				transferErr = err
				log.Printf("transfer: preview read failed generation=%d bytes=%d: %v", generation, totalBytes, err)
				if ctx.Err() == nil {
					m.sendError("media", requestID, "preview failed: "+err.Error())
				}
			}
			break
		}
	}

	log.Printf("transfer: preview finished generation=%d bytes=%d err=%v", generation, totalBytes, transferErr)
	if transferErr != nil {
		return
	}

	// Only send end if we're still the active generation.
	if ctx.Err() == nil && m.mediaOperation.Load() == mediaOperation && m.currentGeneration.Load() == generation {
		end := struct {
			Type        string `json:"type"`
			ID          string `json:"id"`
			Generation  int    `json:"generation"`
			Scope       string `json:"scope"`
			RequestID   string `json:"request_id,omitempty"`
			OperationID uint64 `json:"operation_id"`
		}{Type: "asset_preview_end", ID: id, Generation: int(generation), Scope: "media", RequestID: requestID, OperationID: mediaOperation}
		endData, _ := json.Marshal(end)
		_ = m.sendMediaControlIfCurrent(mediaOperation, string(endData))
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

// encodeChunkFrame creates the application envelope shared by bulk and
// interactive-media chunks. Physical lanes are independent, so operationID is
// required to correlate bytes with a control header even when bytes arrive
// first. generation is zero for bulk and still-image operations.
func encodeChunkFrame(operationID uint64, generation uint32, data []byte) []byte {
	out := make([]byte, chunkEnvelopeSize+len(data))
	out[0] = binaryFrameFileChunk
	out[1] = chunkEnvelopeVersion
	binary.BigEndian.PutUint64(out[2:10], operationID)
	binary.BigEndian.PutUint32(out[10:14], generation)
	copy(out[chunkEnvelopeSize:], data)
	return out
}

func decodeChunkFrame(frame []byte) (uint64, uint32, []byte, error) {
	if len(frame) < chunkEnvelopeSize {
		return 0, 0, nil, fmt.Errorf("chunk envelope too short: %d", len(frame))
	}
	if frame[0] != binaryFrameFileChunk {
		return 0, 0, nil, fmt.Errorf("unexpected chunk frame type: 0x%02x", frame[0])
	}
	if frame[1] != chunkEnvelopeVersion {
		return 0, 0, nil, fmt.Errorf("unsupported chunk envelope version: %d", frame[1])
	}
	operationID := binary.BigEndian.Uint64(frame[2:10])
	generation := binary.BigEndian.Uint32(frame[10:14])
	payload := append([]byte(nil), frame[chunkEnvelopeSize:]...)
	return operationID, generation, payload, nil
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

func (m *Manager) acquireBulk(requestID string) (uint64, bool) {
	if !m.bulkTransfer.CompareAndSwap(false, true) {
		m.sendError("bulk", requestID, "transfer in progress")
		return 0, false
	}
	operation := m.operationSequence.Add(1)
	m.bulkOperation.Store(operation)
	return operation, true
}

func (m *Manager) releaseBulk(operation uint64) {
	if m.bulkOperation.CompareAndSwap(operation, 0) {
		m.bulkTransfer.Store(false)
	}
}

func (m *Manager) beginMedia(id, quality string, generation uint32) (context.Context, uint64) {
	m.mediaMu.Lock()
	ctx, operation := m.beginMediaLocked(id, quality, generation)
	m.mediaMu.Unlock()
	return ctx, operation
}

func (m *Manager) beginMediaIfNewer(id, quality string, generation uint32) (context.Context, uint64, bool) {
	m.mediaMu.Lock()
	defer m.mediaMu.Unlock()
	if m.mediaID != id || m.mediaQuality != "video" || quality != "video" || generation <= m.currentGeneration.Load() {
		return nil, 0, false
	}
	ctx, operation := m.beginMediaLocked(id, quality, generation)
	return ctx, operation, true
}

func (m *Manager) beginMediaLocked(id, quality string, generation uint32) (context.Context, uint64) {
	if m.mediaCancel != nil {
		m.mediaCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.mediaCancel = cancel
	operation := m.operationSequence.Add(1)
	m.mediaOperation.Store(operation)
	m.mediaID = id
	m.mediaQuality = quality
	m.currentGeneration.Store(generation)
	m.mediaTransfer.Store(true)
	return ctx, operation
}

func (m *Manager) finishMedia(operation uint64) {
	m.mediaMu.Lock()
	if m.mediaOperation.Load() == operation {
		if m.mediaCancel != nil {
			m.mediaCancel()
		}
		m.mediaCancel = nil
		m.mediaTransfer.Store(false)
	}
	m.mediaMu.Unlock()
}

func (m *Manager) sendWithBackpressure(ctx context.Context, endpoint multilane.Endpoint, class multilane.TrafficClass, data []byte) error {
	for endpoint.BufferedAmount() > maxBuffer {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sleepInterval):
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return endpoint.SendBinaryClass(class, data)
}

func (m *Manager) sendInteractiveMedia(ctx context.Context, operation uint64, data []byte) error {
	for m.media.BufferedAmount() > maxBuffer {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sleepInterval):
		}
	}

	// Serialize the final active-operation check with beginMedia. A replaced
	// stream either finishes this frame before the replacement header or is
	// rejected here, so stale bytes cannot follow a replacement lifecycle.
	m.mediaMu.Lock()
	defer m.mediaMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.mediaOperation.Load() != operation {
		return context.Canceled
	}
	return m.media.SendBinaryClass(multilane.ClassInteractiveMedia, data)
}

func (m *Manager) sendMediaControlIfCurrent(operation uint64, text string) error {
	// Control and media use independent physical channels. Holding mediaMu makes
	// the active-operation check and its lifecycle send indivisible relative to
	// replacement; Task 8 can then buffer early binary frames by operation_id.
	m.mediaMu.Lock()
	defer m.mediaMu.Unlock()
	if m.mediaOperation.Load() != operation {
		return context.Canceled
	}
	return m.control.SendText(text)
}

func (m *Manager) sendError(scope, requestID, message string) {
	errMsg := struct {
		Type      string `json:"type"`
		Scope     string `json:"scope"`
		RequestID string `json:"request_id,omitempty"`
		Message   string `json:"message"`
	}{
		Type:      "error",
		Scope:     scope,
		RequestID: requestID,
		Message:   message,
	}
	data, _ := json.Marshal(errMsg)
	_ = m.control.SendText(string(data))
}

// SetDownloadCount initializes the download counter from persisted state.
func (m *Manager) SetDownloadCount(n int) {
	m.downloads.Store(int32(n))
}
