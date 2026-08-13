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
	"sort"
	"strconv"
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
	maxAdvertisedGalleryItems = 65536
	maxThumbnailBatchSize     = 120
	defaultThumbnailGrace     = 5 * time.Second
)

type thumbnailMode uint8

const (
	thumbnailModeWaiting thumbnailMode = iota
	thumbnailModePull
	thumbnailModeLegacy
)

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

type AlbumDownloadBackend interface {
	GetAlbumDownload(ctx context.Context) (AlbumDownload, error)
	StreamAlbumArchive(ctx context.Context, assetIDs []string, w io.Writer) (int64, error)
}

type AlbumArchive struct {
	AssetIDs      []string
	EstimatedSize int64
}

type AlbumDownload struct {
	AlbumName string
	TotalSize int64
	Archives  []AlbumArchive
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

	mediaMu                 sync.Mutex
	mediaCancel             context.CancelFunc
	mediaID                 string
	mediaQuality            string
	operationSequence       atomic.Uint64
	mediaOperation          atomic.Uint64
	bulkOperation           atomic.Uint64
	bulkMu                  sync.Mutex
	currentGeneration       atomic.Uint32
	albumMu                 sync.Mutex
	albumBatch              *albumBatch
	albumAckTimeout         time.Duration
	galleryMu               sync.Mutex
	galleryItems            []GalleryItem
	galleryGeneration       uint64
	galleryContext          context.Context
	galleryCancel           context.CancelFunc
	thumbnailMode           thumbnailMode
	thumbnailBatchActive    bool
	thumbnailBatchRequestID string
	thumbnailSeenRequestIDs map[string]struct{}
	thumbnailFallback       *time.Timer
	thumbnailGrace          time.Duration

	OnSessionExpired   func()
	OnDownloadComplete func(bytesTransferred int64)
}

type albumBatch struct {
	requestID string
	ack       chan albumArchiveAck
}

type albumArchiveAck struct {
	partIndex   int
	operationID uint64
	ok          bool
}

// NewManager creates a transfer manager with an optional download limit.
func NewManager(channels multilane.ChannelSet, client StorageBackend, maxDownloads int) *Manager {
	m := &Manager{
		channels:        channels,
		client:          client,
		maxDownloads:    maxDownloads,
		albumAckTimeout: 60 * time.Second,
		thumbnailGrace:  defaultThumbnailGrace,
	}
	m.installChannels()
	return m
}

func NewGalleryManager(channels multilane.ChannelSet, backend GalleryBackend, maxDownloads int) *Manager {
	m := &Manager{
		channels:        channels,
		gallery:         backend,
		maxDownloads:    maxDownloads,
		albumAckTimeout: 60 * time.Second,
		thumbnailGrace:  defaultThumbnailGrace,
	}
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
	if lifecycle, ok := m.channels.(multilane.CloseListenerChannelSet); ok {
		lifecycle.AddOnClose(m.handleClose)
	}
}

func (m *Manager) handleClose() {
	m.galleryMu.Lock()
	if m.thumbnailFallback != nil {
		m.thumbnailFallback.Stop()
		m.thumbnailFallback = nil
	}
	if m.galleryCancel != nil {
		m.galleryCancel()
		m.galleryCancel = nil
	}
	m.galleryGeneration++
	m.thumbnailBatchActive = false
	m.thumbnailBatchRequestID = ""
	m.galleryMu.Unlock()
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
		OperationID string `json:"operation_id"`
		BatchID     string `json:"batch_id"`
		PartIndex   int    `json:"part_index"`
		Start       int    `json:"start"`
		Count       int    `json:"count"`
		OK          bool   `json:"ok"`
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
	case "album_download_request":
		m.handleAlbumDownloadRequest(msg.RequestID)
	case "album_archive_ack":
		m.handleAlbumArchiveAck(msg.BatchID, msg.PartIndex, msg.OperationID, msg.OK)
	case "thumbnail_batch_request":
		m.handleThumbnailBatchRequest(msg.Start, msg.Count, msg.RequestID)
	case "asset_preview_request":
		m.handleAssetPreviewRequest(msg.ID, msg.Quality, msg.Generation, msg.RequestID)
	case "asset_preview_seek":
		if msg.OperationID == "" {
			m.sendError("media", msg.RequestID, "asset_preview_seek: operation_id required")
			return
		}
		parentOperation, err := strconv.ParseUint(msg.OperationID, 10, 64)
		if err != nil || parentOperation == 0 {
			m.sendError("media", msg.RequestID, "asset_preview_seek: invalid operation_id")
			return
		}
		m.handleAssetPreviewSeek(msg.ID, msg.Quality, msg.StartOffset, msg.Generation, msg.RequestID, parentOperation)
	default:
		m.sendError("connection", msg.RequestID, "unknown message type: "+msg.Type)
	}
}

func (m *Manager) handleAlbumDownloadRequest(requestID string) {
	if m.maxDownloads > 0 && int(m.downloads.Load()) >= m.maxDownloads {
		m.sendError("bulk", requestID, "share has reached its download limit")
		if m.OnSessionExpired != nil {
			m.OnSessionExpired()
		}
		return
	}
	if requestID == "" {
		m.sendError("bulk", requestID, "album download request_id required")
		return
	}
	backend, ok := m.gallery.(AlbumDownloadBackend)
	if !ok {
		m.sendError("bulk", requestID, "album download unavailable")
		return
	}
	operationID, ok := m.acquireBulk(requestID)
	if !ok {
		return
	}
	batch := &albumBatch{requestID: requestID, ack: make(chan albumArchiveAck, 1)}
	m.albumMu.Lock()
	m.albumBatch = batch
	m.albumMu.Unlock()

	go func() {
		download, err := backend.GetAlbumDownload(context.Background())
		if err != nil {
			m.sendBulkErrorIfActive(operationID, requestID, "album download unavailable: "+err.Error())
			m.finishAlbumBatch(batch, operationID)
			return
		}
		m.streamAlbumDownload(context.Background(), backend, batch, operationID, download)
	}()
}

func (m *Manager) handleAlbumArchiveAck(batchID string, partIndex int, rawOperationID string, ok bool) {
	operationID, err := strconv.ParseUint(rawOperationID, 10, 64)
	if err != nil || operationID == 0 {
		return
	}
	m.albumMu.Lock()
	batch := m.albumBatch
	m.albumMu.Unlock()
	if batch == nil || batch.requestID != batchID {
		return
	}
	ack := albumArchiveAck{partIndex: partIndex, operationID: operationID, ok: ok}
	select {
	case batch.ack <- ack:
	default:
	}
}

func (m *Manager) streamAlbumDownload(ctx context.Context, backend AlbumDownloadBackend, batch *albumBatch, operationID uint64, download AlbumDownload) {
	currentOperation := operationID
	defer func() { m.finishAlbumBatch(batch, currentOperation) }()
	if len(download.Archives) == 0 {
		m.sendBulkErrorIfActive(currentOperation, batch.requestID, "album contains no downloadable assets")
		return
	}

	var totalBytes int64
	for index, archive := range download.Archives {
		if index > 0 {
			nextOperation, ok := m.advanceBulkOperation(currentOperation)
			if !ok {
				return
			}
			currentOperation = nextOperation
		}
		partIndex := index + 1
		header := struct {
			Type           string `json:"type"`
			Name           string `json:"name"`
			Size           int64  `json:"size"`
			EstimatedSize  int64  `json:"estimated_size"`
			MimeType       string `json:"mimeType"`
			BinaryEnvelope bool   `json:"binary_envelope"`
			Scope          string `json:"scope"`
			RequestID      string `json:"request_id"`
			OperationID    string `json:"operation_id"`
			BatchID        string `json:"batch_id"`
			PartIndex      int    `json:"part_index"`
			PartCount      int    `json:"part_count"`
		}{
			Type: "file_header", Name: albumArchiveName(download.AlbumName, partIndex, len(download.Archives), time.Now()),
			Size: 0, EstimatedSize: archive.EstimatedSize, MimeType: "application/zip", BinaryEnvelope: true,
			Scope: "bulk", RequestID: batch.requestID, OperationID: operationIDString(currentOperation),
			BatchID: batch.requestID, PartIndex: partIndex, PartCount: len(download.Archives),
		}
		headerData, _ := json.Marshal(header)
		if err := m.control.SendText(string(headerData)); err != nil {
			return
		}

		partBytes, err := m.streamAlbumArchive(ctx, backend, archive.AssetIDs, currentOperation)
		if err != nil {
			m.sendBulkErrorIfActive(currentOperation, batch.requestID, "album archive transfer failed: "+err.Error())
			return
		}
		totalBytes += partBytes
		end, _ := json.Marshal(struct {
			Type        string `json:"type"`
			Scope       string `json:"scope"`
			RequestID   string `json:"request_id"`
			OperationID string `json:"operation_id"`
			BytesSent   string `json:"bytes_sent"`
		}{"chunk_end", "bulk", batch.requestID, operationIDString(currentOperation), strconv.FormatInt(partBytes, 10)})
		if err := m.control.SendText(string(end)); err != nil {
			return
		}

		timer := time.NewTimer(m.albumAckTimeout)
		var ack albumArchiveAck
		select {
		case ack = <-batch.ack:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
			m.sendBulkErrorIfActive(currentOperation, batch.requestID, "album download acknowledgement timed out")
			return
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		}
		if !ack.ok || ack.partIndex != partIndex || ack.operationID != currentOperation {
			return
		}
	}

	m.downloads.Add(1)
	if m.OnDownloadComplete != nil {
		m.OnDownloadComplete(totalBytes)
	}
	complete, _ := json.Marshal(struct {
		Type      string `json:"type"`
		BatchID   string `json:"batch_id"`
		PartCount int    `json:"part_count"`
		BytesSent string `json:"bytes_sent"`
	}{"album_download_complete", batch.requestID, len(download.Archives), strconv.FormatInt(totalBytes, 10)})
	_ = m.control.SendText(string(complete))
}

func (m *Manager) streamAlbumArchive(ctx context.Context, backend AlbumDownloadBackend, assetIDs []string, operationID uint64) (int64, error) {
	pr, pw := io.Pipe()
	defer pr.Close()
	go func() {
		_, err := backend.StreamAlbumArchive(ctx, assetIDs, pw)
		_ = pw.CloseWithError(err)
	}()
	var totalBytes int64
	buf := make([]byte, chunkSize)
	for {
		n, err := pr.Read(buf)
		if n > 0 {
			totalBytes += int64(n)
			if sendErr := m.sendWithBackpressure(ctx, m.bulk, multilane.ClassBulk, encodeChunkFrame(operationID, 0, buf[:n])); sendErr != nil {
				_ = pr.CloseWithError(sendErr)
				return totalBytes, sendErr
			}
		}
		if err != nil {
			if err == io.EOF {
				return totalBytes, nil
			}
			return totalBytes, err
		}
	}
}

func albumArchiveName(albumName string, partIndex, partCount int, now time.Time) string {
	base := albumName + ".zip"
	suffix := ""
	if partCount > 1 {
		suffix = "+" + strconv.Itoa(partIndex)
	}
	return strings.Replace(base, ".zip", suffix+"-"+now.Format("20060102_150405")+".zip", 1)
}

func (m *Manager) sendGallery() {
	gallery, err := m.gallery.ListGallery(context.Background())
	if err != nil {
		m.sendError("media", "", "share unavailable: "+err.Error())
		return
	}
	if len(gallery.Items) > maxAdvertisedGalleryItems {
		log.Printf("transfer gallery: truncating gallery metadata from %d to %d items for uint16 thumbnail indices", len(gallery.Items), maxAdvertisedGalleryItems)
		gallery.Items = gallery.Items[:maxAdvertisedGalleryItems]
	}

	m.galleryMu.Lock()
	if m.thumbnailFallback != nil {
		m.thumbnailFallback.Stop()
	}
	if m.galleryCancel != nil {
		m.galleryCancel()
	}
	m.galleryGeneration++
	if m.galleryGeneration == 0 {
		m.galleryGeneration++
	}
	generation := m.galleryGeneration
	m.galleryContext, m.galleryCancel = context.WithCancel(context.Background())
	m.galleryItems = append(m.galleryItems[:0], gallery.Items...)
	m.thumbnailMode = thumbnailModeWaiting
	m.thumbnailBatchActive = false
	m.thumbnailBatchRequestID = ""
	m.thumbnailSeenRequestIDs = make(map[string]struct{})
	m.thumbnailFallback = nil
	m.galleryMu.Unlock()

	data, _ := json.Marshal(struct {
		Type          string `json:"type"`
		ThumbnailMode string `json:"thumbnailMode"`
		Gallery
	}{Type: "thumbnail_list", ThumbnailMode: "pull-v1", Gallery: gallery})
	log.Printf("transfer gallery: sending thumbnail_list items=%d bytes=%d", len(gallery.Items), len(data))
	if err := m.control.SendText(string(data)); err != nil {
		log.Printf("transfer gallery: send thumbnail_list failed: %v", err)
		return
	}
	log.Printf("transfer gallery: thumbnail_list sent items=%d", len(gallery.Items))

	m.galleryMu.Lock()
	if m.galleryGeneration == generation && m.thumbnailMode == thumbnailModeWaiting {
		m.thumbnailFallback = time.AfterFunc(m.thumbnailGrace, func() {
			m.startLegacyThumbnailFallback(generation)
		})
	}
	m.galleryMu.Unlock()
}

func (m *Manager) startLegacyThumbnailFallback(generation uint64) {
	m.galleryMu.Lock()
	if m.galleryGeneration != generation || m.thumbnailMode != thumbnailModeWaiting {
		m.galleryMu.Unlock()
		return
	}
	if m.thumbnailFallback != nil {
		m.thumbnailFallback.Stop()
	}
	m.thumbnailMode = thumbnailModeLegacy
	m.thumbnailFallback = nil
	items := append([]GalleryItem(nil), m.galleryItems...)
	ctx := m.galleryContext
	m.galleryMu.Unlock()

	sent, failed, failedIndices := m.sendGalleryThumbnails(ctx, generation, items, 0)
	log.Printf("transfer gallery: legacy thumbnail stream finished sent=%d failed=%d", sent, failed)
	data, _ := json.Marshal(struct {
		Type          string `json:"type"`
		Sent          int    `json:"sent"`
		Failed        int    `json:"failed"`
		FailedIndices []int  `json:"failed_indices,omitempty"`
	}{Type: "thumbnail_complete", Sent: sent, Failed: failed, FailedIndices: failedIndices})
	m.galleryMu.Lock()
	if m.galleryGeneration != generation || m.thumbnailMode != thumbnailModeLegacy || ctx.Err() != nil {
		m.galleryMu.Unlock()
		return
	}
	err := m.control.SendText(string(data))
	m.galleryMu.Unlock()
	if err != nil {
		log.Printf("transfer gallery: send thumbnail_complete failed: %v", err)
	}
}

func (m *Manager) handleThumbnailBatchRequest(start, count int, requestID string) {
	if requestID == "" {
		m.sendError("media", requestID, "thumbnail batch request_id required")
		return
	}
	if start < 0 || count < 1 || count > maxThumbnailBatchSize {
		m.sendError("media", requestID, "thumbnail batch range invalid")
		return
	}

	m.galleryMu.Lock()
	if start >= len(m.galleryItems) {
		m.galleryMu.Unlock()
		m.sendError("media", requestID, "thumbnail batch start outside gallery")
		return
	}
	if m.thumbnailMode == thumbnailModeLegacy {
		m.galleryMu.Unlock()
		m.sendError("media", requestID, "thumbnail batch unavailable after legacy stream started")
		return
	}
	if _, seen := m.thumbnailSeenRequestIDs[requestID]; seen {
		m.galleryMu.Unlock()
		m.sendError("media", requestID, "thumbnail batch request_id already used")
		return
	}
	if m.thumbnailBatchActive {
		m.galleryMu.Unlock()
		m.sendError("media", requestID, "thumbnail batch already active")
		return
	}
	if m.thumbnailMode == thumbnailModeWaiting {
		m.thumbnailMode = thumbnailModePull
		if m.thumbnailFallback != nil {
			m.thumbnailFallback.Stop()
			m.thumbnailFallback = nil
		}
	}
	end := min(start+count, len(m.galleryItems))
	items := append([]GalleryItem(nil), m.galleryItems[start:end]...)
	generation := m.galleryGeneration
	ctx := m.galleryContext
	m.thumbnailSeenRequestIDs[requestID] = struct{}{}
	m.thumbnailBatchActive = true
	m.thumbnailBatchRequestID = requestID
	m.galleryMu.Unlock()
	go m.sendThumbnailBatch(ctx, generation, start, items, requestID)
}

func (m *Manager) sendThumbnailBatch(ctx context.Context, generation uint64, start int, items []GalleryItem, requestID string) {
	sent, failed, failedIndices := m.sendGalleryThumbnails(ctx, generation, items, start)
	log.Printf("transfer gallery: thumbnail batch start=%d count=%d sent=%d failed=%d", start, len(items), sent, failed)
	data, _ := json.Marshal(struct {
		Type          string `json:"type"`
		RequestID     string `json:"request_id,omitempty"`
		Start         int    `json:"start"`
		Count         int    `json:"count"`
		Sent          int    `json:"sent"`
		Failed        int    `json:"failed"`
		FailedIndices []int  `json:"failed_indices,omitempty"`
	}{Type: "thumbnail_batch_complete", RequestID: requestID, Start: start, Count: len(items), Sent: sent, Failed: failed, FailedIndices: failedIndices})
	m.galleryMu.Lock()
	if m.galleryGeneration != generation || m.thumbnailMode != thumbnailModePull || m.thumbnailBatchRequestID != requestID || ctx.Err() != nil {
		m.galleryMu.Unlock()
		return
	}
	err := m.control.SendText(string(data))
	m.thumbnailBatchActive = false
	m.thumbnailBatchRequestID = ""
	m.galleryMu.Unlock()
	if err != nil {
		log.Printf("transfer gallery: send thumbnail_batch_complete failed: %v", err)
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

func (m *Manager) sendGalleryThumbnails(ctx context.Context, generation uint64, items []GalleryItem, indexOffset int) (int, int, []int) {
	total := len(items)
	if total == 0 {
		return 0, 0, nil
	}
	workCtx, cancelWork := context.WithCancel(ctx)
	defer cancelWork()

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
				if workCtx.Err() != nil {
					return
				}
				var buf bytes.Buffer
				_, err := m.gallery.GetThumbnail(workCtx, job.item.ID, &buf)
				result := thumbnailResult{
					index: job.index,
					id:    job.item.ID,
					data:  buf.Bytes(),
					err:   err,
				}
				select {
				case results <- result:
				case <-workCtx.Done():
					return
				}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for i := 0; i < total; i++ {
			select {
			case jobs <- thumbnailJob{index: indexOffset + i, item: items[i]}:
			case <-workCtx.Done():
				return
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	sentThumbs := 0
	failedThumbs := 0
	failedIndices := make([]int, 0)
	accounted := make(map[int]struct{}, total)
	for result := range results {
		accounted[result.index] = struct{}{}
		if result.err != nil {
			failedThumbs++
			failedIndices = append(failedIndices, result.index)
			log.Printf("transfer gallery: thumbnail fetch failed index=%d id=%s: %v", result.index, result.id, result.err)
			continue
		}
		if err := m.sendThumbnailWithBackpressure(workCtx, generation, encodeThumbnailFrame(uint16(result.index), result.data)); err != nil {
			log.Printf("transfer gallery: thumbnail send failed index=%d id=%s bytes=%d: %v", result.index, result.id, len(result.data), err)
			failedThumbs++
			failedIndices = append(failedIndices, result.index)
			cancelWork()
			continue
		}
		sentThumbs++
	}
	if ctx.Err() == nil {
		for index := indexOffset; index < indexOffset+total; index++ {
			if _, ok := accounted[index]; !ok {
				failedThumbs++
				failedIndices = append(failedIndices, index)
			}
		}
	}
	sort.Ints(failedIndices)
	return sentThumbs, failedThumbs, failedIndices
}

func (m *Manager) sendThumbnailWithBackpressure(ctx context.Context, generation uint64, data []byte) error {
	for m.media.BufferedAmount() > maxBuffer {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sleepInterval):
		}
	}

	m.galleryMu.Lock()
	defer m.galleryMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.galleryGeneration != generation {
		return context.Canceled
	}
	return m.media.SendBinaryClass(multilane.ClassThumbnail, data)
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
		m.sendBulkErrorIfActive(operationID, requestID, "share unavailable: client not initialized")
		m.releaseBulk(operationID)
		return
	}

	files, err := m.client.ListFiles(dir)
	if err != nil {
		m.sendBulkErrorIfActive(operationID, requestID, "share unavailable")
		m.releaseBulk(operationID)
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
		m.sendBulkErrorIfActive(operationID, requestID, "file not found: "+name)
		m.releaseBulk(operationID)
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
		OperationID    string `json:"operation_id"`
	}{
		Type:           "file_header",
		Name:           fileInfo.Name,
		Size:           fileInfo.Size,
		MimeType:       fileInfo.ContentType,
		SHA1:           sha1,
		BinaryEnvelope: true,
		Scope:          "bulk",
		RequestID:      requestID,
		OperationID:    operationIDString(operationID),
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
		m.sendBulkErrorIfActive(operationID, requestID, "share unavailable: client not initialized")
		m.releaseBulk(operationID)
		return
	}

	assetName, assetSize, assetMimeType, err := m.gallery.GetAssetInfo(context.Background(), id)
	if err != nil {
		m.sendBulkErrorIfActive(operationID, requestID, "asset info unavailable: "+err.Error())
		m.releaseBulk(operationID)
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
		OperationID    string `json:"operation_id"`
	}{
		Type:           "file_header",
		Name:           assetName,
		Size:           assetSize,
		MimeType:       assetMimeType,
		SHA1:           "",
		BinaryEnvelope: true,
		Scope:          "bulk",
		RequestID:      requestID,
		OperationID:    operationIDString(operationID),
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
			OperationID    string `json:"operation_id"`
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
			OperationID:    operationIDString(mediaOperation),
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
		OperationID    string `json:"operation_id"`
	}{
		Type:           "asset_preview_header",
		ID:             id,
		MimeType:       mimeType,
		Size:           previewSize,
		BinaryEnvelope: true,
		Scope:          "media",
		RequestID:      requestID,
		OperationID:    operationIDString(mediaOperation),
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
				m.sendBulkErrorIfActive(operationID, requestID, "transfer failed: "+err.Error())
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
		OperationID string `json:"operation_id"`
		BytesSent   string `json:"bytes_sent"`
	}{Type: "chunk_end", Scope: "bulk", RequestID: requestID, OperationID: operationIDString(operationID), BytesSent: strconv.FormatInt(totalBytes, 10)}
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
				m.sendBulkErrorIfActive(operationID, requestID, "transfer failed: "+err.Error())
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
		OperationID string `json:"operation_id"`
		BytesSent   string `json:"bytes_sent"`
	}{Type: "chunk_end", Scope: "bulk", RequestID: requestID, OperationID: operationIDString(operationID), BytesSent: strconv.FormatInt(totalBytes, 10)}
	endData, _ := json.Marshal(end)
	if err := m.control.SendText(string(endData)); err != nil {
		return
	}

	m.downloads.Add(1)
	if m.OnDownloadComplete != nil {
		m.OnDownloadComplete(totalBytes)
	}
}

func (m *Manager) handleAssetPreviewSeek(id string, quality string, startOffset int64, generation int, requestID string, parentOperation uint64) {
	log.Printf("transfer: asset_preview_seek id=%s start_offset=%d generation=%d", id, startOffset, generation)
	if id == "" || quality == "" {
		m.sendMediaErrorIfActive(parentOperation, requestID, "asset_preview_seek: id and quality required")
		return
	}
	if startOffset < 0 {
		m.sendMediaErrorIfActive(parentOperation, requestID, "asset_preview_seek: start_offset must be >= 0")
		return
	}
	normalizedQuality, ok := normalizePreviewQuality(quality)
	if !ok || normalizedQuality != "video" {
		m.sendMediaErrorIfActive(parentOperation, requestID, "asset_preview_seek: video quality required")
		return
	}
	if generation <= 0 || uint64(generation) > uint64(^uint32(0)) {
		m.sendMediaErrorIfActive(parentOperation, requestID, "asset_preview_seek: generation out of range")
		return
	}
	if m.gallery == nil {
		m.sendMediaErrorIfActive(parentOperation, requestID, "share unavailable: client not initialized")
		return
	}
	if !m.isCurrentMediaOperation(parentOperation, id, normalizedQuality) {
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
		m.sendMediaErrorIfActive(parentOperation, requestID, "preview seek failed: "+readErr.Error())
		return
	}

	// Validation succeeded. Atomically accept only a strictly newer generation;
	// a delayed older seek is stale and must not replace the active media stream.
	ctx, mediaOperation, accepted := m.beginMediaIfNewer(parentOperation, id, normalizedQuality, uint32(generation))
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
		OperationID    string `json:"operation_id"`
	}{
		Type:           "asset_preview_header",
		ID:             id,
		MimeType:       "video/mp4",
		BinaryEnvelope: true,
		Generation:     generation,
		ByteOffset:     startOffset,
		Scope:          "media",
		RequestID:      requestID,
		OperationID:    operationIDString(mediaOperation),
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
					m.sendMediaErrorIfActive(mediaOperation, requestID, "preview failed: "+err.Error())
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
			OperationID string `json:"operation_id"`
			BytesSent   string `json:"bytes_sent"`
		}{Type: "asset_preview_end", ID: id, Generation: int(generation), Scope: "media", RequestID: requestID, OperationID: operationIDString(mediaOperation), BytesSent: strconv.FormatInt(totalBytes, 10)}
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
	m.bulkMu.Lock()
	defer m.bulkMu.Unlock()
	if m.bulkTransfer.Load() {
		// This error belongs to the rejected request, not the active download.
		// Do not attach the active operation_id or the browser could abort it.
		m.sendError("bulk", requestID, "transfer in progress")
		return 0, false
	}
	operation := m.nextOperationID()
	m.bulkOperation.Store(operation)
	m.bulkTransfer.Store(true)
	return operation, true
}

func (m *Manager) releaseBulk(operation uint64) {
	m.bulkMu.Lock()
	if m.bulkOperation.Load() == operation {
		m.bulkOperation.Store(0)
		m.bulkTransfer.Store(false)
	}
	m.bulkMu.Unlock()
}

func (m *Manager) advanceBulkOperation(current uint64) (uint64, bool) {
	m.bulkMu.Lock()
	defer m.bulkMu.Unlock()
	if !m.bulkTransfer.Load() || m.bulkOperation.Load() != current {
		return 0, false
	}
	next := m.nextOperationID()
	m.bulkOperation.Store(next)
	return next, true
}

func (m *Manager) finishAlbumBatch(batch *albumBatch, operationID uint64) {
	m.albumMu.Lock()
	if m.albumBatch == batch {
		m.albumBatch = nil
	}
	m.albumMu.Unlock()
	m.releaseBulk(operationID)
}

func (m *Manager) beginMedia(id, quality string, generation uint32) (context.Context, uint64) {
	m.mediaMu.Lock()
	ctx, operation := m.beginMediaLocked(id, quality, generation)
	m.mediaMu.Unlock()
	return ctx, operation
}

func (m *Manager) beginMediaIfNewer(parentOperation uint64, id, quality string, generation uint32) (context.Context, uint64, bool) {
	m.mediaMu.Lock()
	defer m.mediaMu.Unlock()
	if m.mediaOperation.Load() != parentOperation || m.mediaID != id || m.mediaQuality != "video" || quality != "video" || generation <= m.currentGeneration.Load() {
		return nil, 0, false
	}
	ctx, operation := m.beginMediaLocked(id, quality, generation)
	return ctx, operation, true
}

func (m *Manager) isCurrentMediaOperation(operation uint64, id, quality string) bool {
	m.mediaMu.Lock()
	defer m.mediaMu.Unlock()
	return operation != 0 && m.mediaOperation.Load() == operation && m.mediaID == id && m.mediaQuality == quality
}

func (m *Manager) beginMediaLocked(id, quality string, generation uint32) (context.Context, uint64) {
	if m.mediaCancel != nil {
		m.mediaCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.mediaCancel = cancel
	operation := m.nextOperationID()
	m.mediaOperation.Store(operation)
	m.mediaID = id
	m.mediaQuality = quality
	m.currentGeneration.Store(generation)
	m.mediaTransfer.Store(true)
	return ctx, operation
}

func (m *Manager) nextOperationID() uint64 {
	for {
		if operation := m.operationSequence.Add(1); operation != 0 {
			return operation
		}
	}
}

func operationIDString(operation uint64) string {
	return strconv.FormatUint(operation, 10)
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

func (m *Manager) sendMediaErrorIfActive(operation uint64, requestID, message string) {
	m.mediaMu.Lock()
	defer m.mediaMu.Unlock()
	if m.mediaOperation.Load() != operation {
		return
	}
	m.sendOperationError("media", requestID, operation, message)
}

func (m *Manager) sendBulkErrorIfActive(operation uint64, requestID, message string) {
	m.bulkMu.Lock()
	defer m.bulkMu.Unlock()
	if m.bulkOperation.Load() != operation || !m.bulkTransfer.Load() {
		return
	}
	m.sendOperationError("bulk", requestID, operation, message)
}

func (m *Manager) sendOperationError(scope, requestID string, operation uint64, message string) {
	if operation == 0 {
		return
	}
	errMsg := struct {
		Type        string `json:"type"`
		Scope       string `json:"scope"`
		RequestID   string `json:"request_id,omitempty"`
		OperationID string `json:"operation_id"`
		Message     string `json:"message"`
	}{
		Type:        "error",
		Scope:       scope,
		RequestID:   requestID,
		OperationID: operationIDString(operation),
		Message:     message,
	}
	data, _ := json.Marshal(errMsg)
	_ = m.control.SendText(string(data))
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
